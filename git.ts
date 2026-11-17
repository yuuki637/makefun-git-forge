// Git object model, packfile read/write, pkt-line framing. Stdlib only.
import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export type ObjType = 'commit' | 'tree' | 'blob' | 'tag';
export interface GitObject {
  type: ObjType;
  data: Buffer;
}

export const ZERO_OID = '0000000000000000000000000000000000000000';
export const TYPE_IDS: Record<string, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
export const TYPE_NAMES: Record<number, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

export function hashObject(type: ObjType, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return sha1Hex(Buffer.concat([header, data]));
}

// ---------------------------------------------------------------- pkt-lines
export function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const total = body.length + 4;
  if (total > 65520) throw new Error('pkt-line too large');
  const head = Buffer.from(total.toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([head, body]);
}
export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');
export const PKT_RESPONSE_END = Buffer.from('0002', 'ascii');

export interface Packet {
  kind: 'data' | 'flush' | 'delim' | 'response-end';
  data: Buffer;
}

// Stream decoder over a complete buffer: returns packets and the unconsumed rest.
export function decodePackets(buffer: Buffer, start = 0): { packets: Packet[]; rest: Buffer } {
  const packets: Packet[] = [];
  let offset = start;
  while (offset < buffer.length) {
    if (buffer.length - offset < 4) break;
    const hex = buffer.toString('ascii', offset, offset + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(hex)) break;
    const length = parseInt(hex, 16);
    if (length === 0) {
      packets.push({ kind: 'flush', data: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (length === 1) {
      packets.push({ kind: 'delim', data: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (length === 2) {
      packets.push({ kind: 'response-end', data: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (length < 4 || offset + length > buffer.length) break;
    packets.push({ kind: 'data', data: buffer.subarray(offset + 4, offset + length) });
    offset += length;
  }
  return { packets, rest: buffer.subarray(offset) };
}

// ------------------------------------------------------------------- objects
export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    const mode = data.toString('ascii', offset, space);
    const nul = data.indexOf(0x00, space + 1);
    const name = data.toString('utf8', space + 1, nul);
    const oid = data.toString('hex', nul + 1, nul + 21);
    entries.push({ mode, name, oid });
    offset = nul + 21;
  }
  return entries;
}

export function treeSortKey(entry: TreeEntry): string {
  return entry.mode === '40000' ? `${entry.name}/` : entry.name;
}

export function buildTreeData(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const ka = treeSortKey(a);
    const kb = treeSortKey(b);
    return ka < kb ? -1 : ka > kb ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface CommitData {
  tree: string;
  parents: string[];
  authorLine: string;
  committerLine: string;
  authorName: string;
  authorEmail: string;
  committerName: string;
  committerEmail: string;
  message: string;
}

export function parseCommit(data: Buffer): CommitData {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const header = split === -1 ? text : text.slice(0, split);
  const message = split === -1 ? '' : text.slice(split + 2);
  const result: CommitData = {
    tree: ZERO_OID,
    parents: [],
    authorLine: '',
    committerLine: '',
    authorName: '',
    authorEmail: '',
    committerName: '',
    committerEmail: '',
    message,
  };
  for (const line of header.split('\n')) {
    if (line.startsWith('tree ')) result.tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) result.parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) {
      result.authorLine = line.slice(7);
      const m = /^(.*) <(.*)>/.exec(result.authorLine);
      if (m) {
        result.authorName = m[1];
        result.authorEmail = m[2];
      }
    } else if (line.startsWith('committer ')) {
      result.committerLine = line.slice(10);
      const m = /^(.*) <(.*)>/.exec(result.committerLine);
      if (m) {
        result.committerName = m[1];
        result.committerEmail = m[2];
      }
    }
  }
  return result;
}

export function buildCommitData(input: {
  tree: string;
  parents: string[];
  authorName: string;
  authorEmail: string;
  committerName: string;
  committerEmail: string;
  timestamp: number;
  message: string;
}): Buffer {
  const author = `${input.authorName} <${input.authorEmail}> ${input.timestamp} +0000`;
  const committer = `${input.committerName} <${input.committerEmail}> ${input.timestamp} +0000`;
  let text = `tree ${input.tree}\n`;
  for (const parent of input.parents) text += `parent ${parent}\n`;
  text += `author ${author}\ncommitter ${committer}\n\n${input.message}`;
  return Buffer.from(text, 'utf8');
}

export interface TagData {
  object: string;
  type: string;
  tag: string;
  message: string;
}

export function parseTag(data: Buffer): TagData {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const header = split === -1 ? text : text.slice(0, split);
  const result: TagData = { object: ZERO_OID, type: '', tag: '', message: split === -1 ? '' : text.slice(split + 2) };
  for (const line of header.split('\n')) {
    if (line.startsWith('object ')) result.object = line.slice(7).trim();
    else if (line.startsWith('type ')) result.type = line.slice(5).trim();
    else if (line.startsWith('tag ')) result.tag = line.slice(4).trim();
  }
  return result;
}

// ---------------------------------------------------------------- pack write
export function writePack(entries: { oid: string; type: ObjType; data: Buffer }[]): Buffer {
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(entries.length, 8);
  const parts: Buffer[] = [header];
  for (const entry of entries) {
    const size = entry.data.length;
    const typeId = TYPE_IDS[entry.type];
    const bytes: number[] = [];
    let first = (typeId << 4) | (size & 0x0f);
    let rest = size >>> 4;
    if (rest > 0) first |= 0x80;
    bytes.push(first);
    while (rest > 0) {
      let byte = rest & 0x7f;
      rest >>>= 7;
      if (rest > 0) byte |= 0x80;
      bytes.push(byte);
    }
    parts.push(Buffer.from(bytes));
    parts.push(deflateSync(entry.data));
  }
  const body = Buffer.concat(parts);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

// ----------------------------------------------------------------- pack read
function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[offset++];
      value |= (byte & 0x7f) << shift;
      shift += 7;
      if ((byte & 0x80) === 0) break;
    }
    return value;
  };
  const sourceSize = readVarint();
  if (sourceSize !== base.length) throw new Error('delta source size mismatch');
  const targetSize = readVarint();
  const chunks: Buffer[] = [];
  let produced = 0;
  while (offset < delta.length) {
    const opcode = delta[offset++];
    if (opcode & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      if (opcode & 0x01) copyOffset = delta[offset++];
      if (opcode & 0x02) copyOffset |= delta[offset++] << 8;
      if (opcode & 0x04) copyOffset |= delta[offset++] << 16;
      if (opcode & 0x08) copyOffset |= delta[offset++] << 24;
      if (opcode & 0x10) copySize = delta[offset++];
      if (opcode & 0x20) copySize |= delta[offset++] << 8;
      if (opcode & 0x40) copySize |= delta[offset++] << 16;
      if (copySize === 0) copySize = 0x10000;
      chunks.push(base.subarray(copyOffset, copyOffset + copySize));
      produced += copySize;
    } else if (opcode > 0) {
      chunks.push(delta.subarray(offset, offset + opcode));
      offset += opcode;
      produced += opcode;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  const out = Buffer.concat(chunks);
  if (out.length !== targetSize || produced !== targetSize) throw new Error('delta target size mismatch');
  return out;
}

// Inflates one object body starting at offset; returns bytes consumed.
function inflateAt(buffer: Buffer, offset: number): { data: Buffer; consumed: number } {
  // zlib stream: find its end by inflating with progressive slices is costly;
  // instead use the fact that inflateSync on a subarray throws on trailing
  // garbage only when strict. Node's inflateSync ignores trailing bytes, so
  // determine the compressed length by trying the sync API via a stream-like
  // approach: use `inflateSync` with a manually located end. We instead use
  // zlib's inflate through createInflate in a synchronous accumulation with a
  // chunked feed is not available; fall back to binary search on stream end:
  // the deflate stream ends when inflate stops consuming. Node exposes this
  // via `zlib.inflateSync(buffer, { finishFlush: ... })` not directly.
  // Practical approach: use the async-free trick of scanning for the point at
  // which inflateSync succeeds and produces stable output AND the remaining
  // bytes parse as the next object — too fragile. Use createInflate with
  // `process` streaming synchronously through a small manual loop is not
  // possible in pure sync code.
  // Reliable alternative: incremental window inflation.
  let lo = 1;
  let hi = buffer.length - offset;
  // First try whole remainder (works when this is the last object before trailer handled by caller slicing).
  // Binary search smallest length that inflates successfully.
  let best = -1;
  let loBound = 1;
  let hiBound = hi;
  while (loBound <= hiBound) {
    const mid = (loBound + hiBound) >> 1;
    try {
      inflateSync(buffer.subarray(offset, offset + mid));
      best = mid;
      hiBound = mid - 1;
    } catch {
      loBound = mid + 1;
    }
  }
  if (best === -1) throw new Error('cannot inflate pack object');
  lo = best;
  const data = inflateSync(buffer.subarray(offset, offset + lo));
  return { data, consumed: lo };
}

// Parses a pack buffer. resolveBase supplies objects not in the pack (thin packs).
export function readPack(
  buffer: Buffer,
  resolveBase: (oid: string) => GitObject | undefined,
): Map<string, GitObject> {
  if (buffer.length < 12 + 20) throw new Error('pack too small');
  if (buffer.toString('ascii', 0, 4) !== 'PACK') throw new Error('bad pack signature');
  const version = buffer.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error('bad pack version');
  const count = buffer.readUInt32BE(8);
  const trailer = buffer.subarray(buffer.length - 20);
  const checksum = createHash('sha1').update(buffer.subarray(0, buffer.length - 20)).digest();
  if (!checksum.equals(trailer)) throw new Error('pack checksum mismatch');

  interface Raw {
    typeId: number;
    data?: Buffer;
    baseOid?: string;
    baseOffset?: number;
    startOffset: number;
  }
  const raws: Raw[] = [];
  let offset = 12;
  for (let i = 0; i < count; i++) {
    const startOffset = offset;
    let byte = buffer[offset++];
    const typeId = (byte >> 4) & 0x07;
    let size = byte & 0x0f;
    let shift = 4;
    while (byte & 0x80) {
      byte = buffer[offset++];
      size |= (byte & 0x7f) << shift;
      shift += 7;
    }
    let baseOid: string | undefined;
    let baseOffset: number | undefined;
    if (typeId === 6) {
      // OFS_DELTA
      let b = buffer[offset++];
      let off = b & 0x7f;
      while (b & 0x80) {
        b = buffer[offset++];
        off = ((off + 1) << 7) | (b & 0x7f);
      }
      baseOffset = startOffset - off;
    } else if (typeId === 7) {
      // REF_DELTA
      baseOid = buffer.toString('hex', offset, offset + 20);
      offset += 20;
    }
    const { data, consumed } = inflateAt(buffer, offset);
    offset += consumed;
    void size;
    raws.push({ typeId, data: typeId === 6 || typeId === 7 ? undefined : data, baseOid, baseOffset, startOffset });
    if (typeId === 6 || typeId === 7) {
      raws[raws.length - 1].data = data; // delta payload (still store raw delta bytes here temporarily)
    }
  }

  const byOffset = new Map<number, number>();
  raws.forEach((raw, idx) => byOffset.set(raw.startOffset, idx));
  const resolved = new Map<number, GitObject>();
  const result = new Map<string, GitObject>();

  const resolveIndex = (idx: number): GitObject => {
    const cached = resolved.get(idx);
    if (cached) return cached;
    const raw = raws[idx];
    let obj: GitObject;
    if (raw.typeId >= 1 && raw.typeId <= 4) {
      obj = { type: TYPE_NAMES[raw.typeId], data: raw.data as Buffer };
    } else if (raw.typeId === 6 || raw.typeId === 7) {
      let base: GitObject | undefined;
      if (raw.typeId === 6) {
        const baseIdx = byOffset.get(raw.baseOffset as number);
        if (baseIdx !== undefined) base = resolveIndex(baseIdx);
      } else {
        const baseOid = raw.baseOid as string;
        const inPack = result.get(baseOid);
        if (inPack) base = inPack;
        else base = resolveBase(baseOid);
      }
      if (!base) throw new Error('delta base not found');
      obj = { type: base.type, data: applyDelta(base.data, raw.data as Buffer) };
    } else {
      throw new Error(`unknown pack object type ${raw.typeId}`);
    }
    resolved.set(idx, obj);
    result.set(hashObject(obj.type, obj.data), obj);
    return obj;
  };
  for (let i = 0; i < raws.length; i++) resolveIndex(i);
  return result;
}
