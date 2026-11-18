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

export interface CommitHeader {
  tree: string;
  parents: string[];
}

// Tree + parents of a commit, read straight from the raw bytes. Graph
// walks (reachability, history, fetch planning) need this per commit,
// and the full parseCommit — whole-buffer string conversion, line
// splits, regexes — dominated their cost.
export function parseCommitHeader(data: Buffer): CommitHeader {
  let tree = ZERO_OID;
  const parents: string[] = [];
  let pos = 0;
  while (pos < data.length) {
    const nl = data.indexOf(0x0a, pos);
    const lineEnd = nl === -1 ? data.length : nl;
    if (lineEnd === pos) break; // blank line ends the header
    if (lineEnd - pos >= 45 && data[pos] === 0x74 && data.toString('ascii', pos, pos + 5) === 'tree ') {
      tree = data.toString('ascii', pos + 5, pos + 45);
    } else if (lineEnd - pos >= 47 && data[pos] === 0x70 && data.toString('ascii', pos, pos + 7) === 'parent ') {
      parents.push(data.toString('ascii', pos + 7, pos + 47));
    }
    if (nl === -1) break;
    pos = nl + 1;
  }
  return { tree, parents };
}

// Commit objects are immutable once stored, so a parsed header can be
// cached on the object itself (WeakMap: no lifetime bookkeeping).
const commitHeaderCache = new WeakMap<GitObject, CommitHeader>();
export function commitHeaderOf(obj: GitObject): CommitHeader {
  let header = commitHeaderCache.get(obj);
  if (!header) {
    header = parseCommitHeader(obj.data);
    commitHeaderCache.set(obj, header);
  }
  return header;
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

// adler32 (RFC 1950) of a buffer: the checksum a zlib stream ends with.
function adler32(data: Buffer): number {
  const MOD = 65521;
  let a = 1;
  let b = 0;
  let i = 0;
  while (i < data.length) {
    const end = Math.min(i + 5552, data.length);
    for (; i < end; i += 1) {
      a += data[i];
      b += a;
    }
    a %= MOD;
    b %= MOD;
  }
  return ((b << 16) | a) >>> 0;
}

// The original boundary search, kept as a fallback for streams the fast
// path in inflateAt cannot size: binary-search the smallest slice length
// that inflates (inflateSync ignores trailing bytes past the stream end).
function inflateBinarySearch(buffer: Buffer, offset: number): { data: Buffer; consumed: number } {
  let best = -1;
  let loBound = 1;
  let hiBound = buffer.length - offset;
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
  const data = inflateSync(buffer.subarray(offset, offset + best));
  return { data, consumed: best };
}

// Inflates one object body starting at offset; returns bytes consumed.
// Fast path: inflate the whole remainder once (inflateSync stops at the
// end of the zlib stream and ignores what follows), check the result
// against the size the pack header declared, then locate the stream's
// end exactly — a zlib stream ends with the adler32 of its plain data,
// so scan for that trailer and confirm it with one exact-slice inflate
// compared byte for byte. That is two inflateSync calls per object; the
// binary search needed ~log2(n), and every call spins up a zlib stream
// object (and, on a miss, an Error with a stack trace), which made pack
// reading the dominant cost of receive-pack.
function inflateAt(buffer: Buffer, offset: number, expectedSize: number): { data: Buffer; consumed: number } {
  let data: Buffer | null = null;
  try {
    data = inflateSync(buffer.subarray(offset));
  } catch {
    data = null;
  }
  if (data !== null && data.length === expectedSize) {
    const trailer = Buffer.alloc(4);
    trailer.writeUInt32BE(adler32(data), 0);
    // The trailer follows the 2-byte zlib header and a deflate body of at
    // least 2 bytes, so it cannot start before offset + 4.
    let pos = buffer.indexOf(trailer, offset + 4);
    while (pos !== -1) {
      const end = pos + 4;
      try {
        if (inflateSync(buffer.subarray(offset, end)).equals(data)) {
          return { data, consumed: end - offset };
        }
      } catch {
        // Not the real stream end; keep scanning.
      }
      pos = buffer.indexOf(trailer, pos + 1);
    }
  }
  return inflateBinarySearch(buffer, offset);
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
    const { data, consumed } = inflateAt(buffer, offset, size);
    offset += consumed;
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
