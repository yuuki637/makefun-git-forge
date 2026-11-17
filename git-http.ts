// Git smart HTTP: /{owner}/{repo}.git/info/refs, git-upload-pack, git-receive-pack.
import {
  type GitObject,
  PKT_FLUSH,
  PKT_RESPONSE_END,
  ZERO_OID,
  decodePackets,
  parseCommit,
  parseTag,
  parseTree,
  pktLine,
  readPack,
  writePack,
} from './git.ts';
import {
  type Repo,
  ancestorDistances,
  collectClosure,
  isAncestor,
  peelToCommit,
  treeOfCommit,
} from './store.ts';
import { type Ctx, type Reply, errorReply, headerOf } from './web.ts';

const AGENT = 'forge/1.0';

// ------------------------------------------------------------------ helpers
function sortedRefs(repo: Repo): [string, string][] {
  return [...repo.refs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
}

function peeledOid(repo: Repo, oid: string): string | null {
  const obj = repo.objects.get(oid);
  if (!obj || obj.type !== 'tag') return null;
  const target = parseTag(obj.data).object;
  return repo.objects.has(target) ? target : null;
}

function sidebandPackets(pack: Buffer): Buffer[] {
  const out: Buffer[] = [];
  const CHUNK = 32768;
  for (let offset = 0; offset < pack.length; offset += CHUNK) {
    const chunk = pack.subarray(offset, offset + CHUNK);
    out.push(pktLine(Buffer.concat([Buffer.from([1]), chunk])));
  }
  if (out.length === 0) out.push(pktLine(Buffer.concat([Buffer.from([1])])));
  return out;
}

// Closure that includes trees/blobs fully but does not expand the parents of
// boundary commits (used to model what a shallow client already holds).
function boundedClosure(repo: Repo, roots: string[], boundary: Set<string>): Set<string> {
  const out = new Set<string>();
  const stack = [...roots];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (out.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (!obj) continue;
    out.add(oid);
    if (obj.type === 'commit') {
      const commit = parseCommit(obj.data);
      stack.push(commit.tree);
      if (!boundary.has(oid)) for (const parent of commit.parents) stack.push(parent);
    } else if (obj.type === 'tree') {
      for (const entry of parseTree(obj.data)) stack.push(entry.oid);
    } else if (obj.type === 'tag') {
      stack.push(parseTag(obj.data).object);
    }
  }
  return out;
}

// Adds a tree and everything under it (subtrees, blobs) to `out`.
function addTreeClosure(repo: Repo, treeOid: string, out: Set<string>): void {
  const stack = [treeOid];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (out.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (!obj) continue;
    out.add(oid);
    if (obj.type === 'tree') {
      for (const entry of parseTree(obj.data)) stack.push(entry.oid);
    }
  }
}

interface FetchPlan {
  objects: string[];
  shallow: string[];
  unshallow: string[];
}

function planFetch(
  repo: Repo,
  wants: string[],
  haves: string[],
  clientShallow: string[],
  depth: number | null,
): FetchPlan {
  const shallowSet = new Set(clientShallow);
  // What the client already holds: closure of haves, bounded by its shallow list.
  const heldCommits = new Set<string>();
  {
    const stack = haves.map((h) => peelToCommit(repo, h)).filter((x): x is string => x !== null);
    const seen = new Set<string>();
    while (stack.length > 0) {
      const current = stack.pop() as string;
      if (seen.has(current)) continue;
      seen.add(current);
      heldCommits.add(current);
      if (shallowSet.has(current)) continue;
      const obj = repo.objects.get(current);
      if (obj && obj.type === 'commit') {
        for (const parent of parseCommit(obj.data).parents) stack.push(parent);
      }
    }
  }
  const heldObjects = boundedClosure(repo, [...heldCommits], shallowSet);

  if (depth === null) {
    const wantClosure = collectClosure(repo, wants);
    const objects = [...wantClosure].filter((oid) => !heldObjects.has(oid));
    const included = new Set<string>([...heldCommits]);
    for (const oid of objects) {
      const obj = repo.objects.get(oid);
      if (obj && obj.type === 'commit') included.add(oid);
    }
    const unshallow: string[] = [];
    for (const oid of clientShallow) {
      if (!included.has(oid)) continue;
      const obj = repo.objects.get(oid);
      if (obj && obj.type === 'commit') {
        const parents = parseCommit(obj.data).parents;
        if (parents.every((p) => included.has(p))) unshallow.push(oid);
      }
    }
    return { objects, shallow: [], unshallow };
  }

  // Depth-limited: BFS by generation from the wants.
  const generations = new Map<string, number>();
  const queue: { oid: string; gen: number }[] = [];
  for (const want of wants) {
    const commit = peelToCommit(repo, want);
    if (commit && !generations.has(commit)) {
      generations.set(commit, 1);
      queue.push({ oid: commit, gen: 1 });
    }
  }
  while (queue.length > 0) {
    const { oid, gen } = queue.shift() as { oid: string; gen: number };
    if (gen >= depth) continue;
    const obj = repo.objects.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) {
      if (!generations.has(parent)) {
        generations.set(parent, gen + 1);
        queue.push({ oid: parent, gen: gen + 1 });
      }
    }
  }
  const sendCommits = new Set(generations.keys());
  // Objects to send: the tag chain objects for tag wants, each send commit,
  // and the full tree closure of each send commit — nothing beyond the depth
  // boundary (in particular not the trees of excluded ancestors).
  const sendSet = new Set<string>();
  for (const want of wants) {
    let current: string | null = want;
    while (current) {
      const obj = repo.objects.get(current);
      if (!obj || obj.type !== 'tag') break;
      sendSet.add(current);
      current = parseTag(obj.data).object;
    }
  }
  for (const commit of sendCommits) {
    sendSet.add(commit);
    const tree = treeOfCommit(repo, commit);
    if (tree) addTreeClosure(repo, tree, sendSet);
  }
  const objects = [...sendSet].filter((oid) => !heldObjects.has(oid));
  const included = new Set<string>([...heldCommits, ...sendCommits]);
  const boundary = new Set<string>();
  for (const [oid, gen] of generations) {
    if (gen !== depth) continue;
    const obj = repo.objects.get(oid);
    if (obj && obj.type === 'commit' && parseCommit(obj.data).parents.length > 0) boundary.add(oid);
  }
  const shallow = [...boundary].filter((oid) => !shallowSet.has(oid));
  const unshallow: string[] = [];
  for (const oid of clientShallow) {
    if (!included.has(oid) || boundary.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (obj && obj.type === 'commit') {
      const parents = parseCommit(obj.data).parents;
      if (parents.length === 0 || parents.every((p) => included.has(p))) unshallow.push(oid);
    }
  }
  return { objects, shallow, unshallow };
}

const packCache = new Map<string, Buffer>();
function buildPack(repo: Repo, objectOids: string[]): Buffer {
  const key = `${repo.id}:${repo.version}:${[...objectOids].sort().join(',')}`;
  const cached = packCache.get(key);
  if (cached) return cached;
  const entries = objectOids
    .map((oid) => {
      const obj = repo.objects.get(oid) as GitObject;
      return { oid, type: obj.type, data: obj.data };
    })
    .filter((e) => e !== undefined);
  const pack = writePack(entries);
  if (packCache.size > 128) packCache.clear();
  packCache.set(key, pack);
  return pack;
}

// ------------------------------------------------------------- advertisement
function refsAdvertisement(repo: Repo, capabilities: string): Buffer {
  const refs = sortedRefs(repo);
  const headRef = `refs/heads/${repo.defaultBranch}`;
  const headOid = repo.refs.get(headRef);
  const parts: Buffer[] = [];
  if (refs.length === 0) {
    parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${capabilities}\n`));
    parts.push(PKT_FLUSH);
    return Buffer.concat(parts);
  }
  let first = true;
  if (headOid) {
    parts.push(pktLine(`${headOid} HEAD\0${capabilities}\n`));
    first = false;
  }
  for (const [ref, oid] of refs) {
    parts.push(pktLine(first ? `${oid} ${ref}\0${capabilities}\n` : `${oid} ${ref}\n`));
    first = false;
    const peeled = peeledOid(repo, oid);
    if (peeled) parts.push(pktLine(`${peeled} ${ref}^{}\n`));
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

function advertisement(ctx: Ctx, repo: Repo, service: string): Reply {
  const isV2 = (headerOf(ctx, 'git-protocol') || '').includes('version=2');
  if (service === 'git-upload-pack' && isV2) {
    const body = Buffer.concat([
      pktLine('version 2\n'),
      pktLine(`agent=${AGENT}\n`),
      pktLine('ls-refs=unborn\n'),
      pktLine('fetch=shallow\n'),
      pktLine('object-format=sha1\n'),
      PKT_FLUSH,
    ]);
    return {
      status: 200,
      headers: { 'content-type': 'application/x-git-upload-pack-advertisement', 'cache-control': 'no-cache' },
      body,
    };
  }
  const capabilities =
    service === 'git-upload-pack'
      ? `multi_ack_detailed side-band-64k thin-pack ofs-delta shallow deepen-since deepen-not agent=${AGENT} symref=HEAD:refs/heads/${repo.defaultBranch}`
      : `report-status side-band-64k delete-refs ofs-delta agent=${AGENT}`;
  const body = Buffer.concat([
    pktLine(`# service=${service}\n`),
    PKT_FLUSH,
    refsAdvertisement(repo, capabilities),
  ]);
  return {
    status: 200,
    headers: { 'content-type': `application/x-git-${service === 'git-upload-pack' ? 'upload-pack' : 'receive-pack'}-advertisement`, 'cache-control': 'no-cache' },
    body,
  };
}

// ------------------------------------------------------------------ ls-refs
function lsRefs(repo: Repo, args: string[]): Reply {
  const peel = args.includes('peel');
  const symrefs = args.includes('symrefs');
  const unborn = args.includes('unborn');
  const prefixes = args.filter((a) => a.startsWith('ref-prefix ')).map((a) => a.slice('ref-prefix '.length));
  const matches = (name: string): boolean => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));
  const parts: Buffer[] = [];
  const headRef = `refs/heads/${repo.defaultBranch}`;
  const headOid = repo.refs.get(headRef);
  // HEAD is a ref like any other for prefix purposes: include it when no
  // prefixes were given, or when a prefix matches the name "HEAD" (real git
  // sends ref-prefix HEAD; the suite's prefix checks use refs/... prefixes).
  const headMatches = prefixes.length === 0 || prefixes.some((p) => 'HEAD'.startsWith(p));
  if (headMatches) {
    if (headOid) {
      parts.push(pktLine(symrefs ? `${headOid} HEAD symref-target:${headRef}\n` : `${headOid} HEAD\n`));
    } else if (unborn) {
      parts.push(pktLine(`unborn HEAD symref-target:${headRef}\n`));
    } else {
      parts.push(pktLine(`${ZERO_OID} HEAD symref-target:${headRef}\n`));
    }
  }
  for (const [ref, oid] of sortedRefs(repo)) {
    if (!matches(ref)) continue;
    let line = `${oid} ${ref}`;
    if (peel) {
      const peeled = peeledOid(repo, oid);
      if (peeled) line += ` peeled:${peeled}`;
    }
    parts.push(pktLine(`${line}\n`));
  }
  parts.push(PKT_FLUSH);
  return {
    status: 200,
    headers: { 'content-type': 'application/x-git-upload-pack-result' },
    body: Buffer.concat(parts),
  };
}

// -------------------------------------------------------------------- fetch
function fetchReply(repo: Repo, wants: string[], haves: string[], clientShallow: string[], depth: number | null, v2: boolean, done = true, deepenRelative = false): Reply {
  // deepen-relative: the requested depth extends the client's current
  // shallow depth (distance from the wants to its shallow boundary).
  if (depth !== null && deepenRelative && clientShallow.length > 0) {
    let current = 0;
    for (const want of wants) {
      const distances = ancestorDistances(repo, want);
      for (const boundary of clientShallow) {
        const commit = peelToCommit(repo, boundary);
        if (commit && distances.has(commit)) current = Math.max(current, (distances.get(commit) as number) + 1);
      }
    }
    if (current > 0) depth = depth + current;
  }
  const plan = planFetch(repo, wants, haves, clientShallow, depth);
  const pack = buildPack(repo, plan.objects);
  const band = sidebandPackets(pack);
  const parts: Buffer[] = [];
  if (v2) {
    if (!done) {
      // Negotiation round: acknowledgments only. The HTTP body simply ends;
      // git's transport synthesizes the response-end at EOF (a literal one
      // in the body is rejected by remote-curl).
      parts.push(pktLine('acknowledgments\n'));
      const present = haves.filter((h) => repo.objects.has(h));
      if (present.length === 0) parts.push(pktLine('NAK\n'));
      else for (const have of present) parts.push(pktLine(`ACK ${have}\n`));
      parts.push(PKT_FLUSH);
      return {
        status: 200,
        headers: { 'content-type': 'application/x-git-upload-pack-result' },
        body: Buffer.concat(parts),
      };
    }
    // With 'done' the response carries no acknowledgments section: real git
    // dies with "expected 'packfile', received 'acknowledgments'" if it does.
    if (depth !== null || plan.shallow.length > 0 || plan.unshallow.length > 0) {
      parts.push(pktLine('shallow-info\n'));
      for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
      for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
      parts.push(PKT_FLUSH);
    }
    parts.push(pktLine('packfile\n'));
    parts.push(...band);
    parts.push(PKT_FLUSH);
  } else {
    for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
    const present = haves.filter((h) => repo.objects.has(h));
    if (present.length === 0) parts.push(pktLine('NAK\n'));
    else parts.push(pktLine(`ACK ${present[present.length - 1]}\n`));
    parts.push(...band);
    parts.push(PKT_FLUSH);
  }
  return {
    status: 200,
    headers: { 'content-type': 'application/x-git-upload-pack-result' },
    body: Buffer.concat(parts),
  };
}

function handleUploadPack(ctx: Ctx, repo: Repo): Reply {
  const isV2 = (headerOf(ctx, 'git-protocol') || '').includes('version=2');
  const { packets } = decodePackets(ctx.rawBody);
  // Split packets into sections on delim/flush boundaries.
  const sections: string[][] = [[]];
  for (const packet of packets) {
    if (packet.kind === 'data') {
      sections[sections.length - 1].push(packet.data.toString('utf8').replace(/\n$/, ''));
    } else if (packet.kind === 'delim' || packet.kind === 'flush') {
      sections.push([]);
    }
  }
  if (isV2) {
    const head = sections[0] ?? [];
    const command = head.find((l) => l.startsWith('command='));
    if (!command) return { status: 400, headers: { 'content-type': 'text/plain' }, body: 'invalid upload-pack request' };
    const args = sections[1] ?? [];
    if (command === 'command=ls-refs') return lsRefs(repo, args);
    if (command === 'command=fetch') {
      const wants: string[] = [];
      const haves: string[] = [];
      const shallow: string[] = [];
      let depth: number | null = null;
      let done = false;
      let deepenRelative = false;
      for (const line of args) {
        if (line.startsWith('want ')) wants.push(line.slice(5).trim());
        else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
        else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
        else if (line.startsWith('deepen ')) depth = Number(line.slice(7).trim());
        else if (line === 'deepen-relative') deepenRelative = true;
        else if (line === 'done') done = true;
      }
      return fetchReply(repo, wants, haves, shallow, depth, true, done, deepenRelative);
    }
    return { status: 400, headers: { 'content-type': 'text/plain' }, body: 'unknown command' };
  }
  // v0 fetch
  const wantSection = sections[0] ?? [];
  const haveSection = sections[1] ?? [];
  const wants: string[] = [];
  const shallow: string[] = [];
  let depth: number | null = null;
  for (const line of wantSection) {
    if (line.startsWith('want ')) wants.push(line.slice(5).trim().split(' ')[0]);
    else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) depth = Number(line.slice(7).trim());
  }
  const haves: string[] = [];
  for (const line of haveSection) {
    if (line.startsWith('have ')) haves.push(line.slice(5).trim());
  }
  if (wants.length === 0) return { status: 400, headers: { 'content-type': 'text/plain' }, body: 'invalid upload-pack request' };
  return fetchReply(repo, wants, haves, shallow, depth, false);
}

// -------------------------------------------------------------- receive-pack
interface RefCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

function handleReceivePack(ctx: Ctx, repo: Repo): Reply {
  const { packets, rest } = decodePackets(ctx.rawBody);
  const commands: RefCommand[] = [];
  let capabilities = '';
  let sawFlush = false;
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      sawFlush = true;
      break;
    }
    if (packet.kind !== 'data') continue;
    let line = packet.data.toString('utf8').replace(/\n$/, '');
    const nul = line.indexOf('\0');
    if (nul !== -1) {
      capabilities = line.slice(nul + 1);
      line = line.slice(0, nul);
    }
    const match = /^([0-9a-f]{40}) ([0-9a-f]{40}) (.+)$/.exec(line);
    if (!match) return { status: 400, headers: { 'content-type': 'text/plain' }, body: 'invalid receive-pack request' };
    commands.push({ oldOid: match[1], newOid: match[2], ref: match[3] });
  }
  if (!sawFlush && commands.length === 0) {
    return { status: 400, headers: { 'content-type': 'text/plain' }, body: 'invalid receive-pack request' };
  }

  let unpackLine = 'unpack ok';
  if (rest.length > 0) {
    try {
      const objects = readPack(rest, (oid) => repo.objects.get(oid));
      for (const [oid, obj] of objects) {
        if (!repo.objects.has(oid)) repo.objects.set(oid, obj);
      }
    } catch (error) {
      unpackLine = `unpack ${error instanceof Error ? error.message : 'error'}`;
    }
  }

  const results: { ref: string; ok: boolean; reason?: string }[] = [];
  if (!unpackLine.startsWith('unpack ok')) {
    for (const command of commands) results.push({ ref: command.ref, ok: false, reason: 'unpack failed' });
  } else {
    for (const command of commands) {
      const current = repo.refs.get(command.ref) ?? ZERO_OID;
      if (command.newOid === ZERO_OID) {
        // delete
        if (!repo.refs.has(command.ref)) {
          results.push({ ref: command.ref, ok: false, reason: 'reference not found' });
        } else if (command.oldOid !== current) {
          results.push({ ref: command.ref, ok: false, reason: 'stale info' });
        } else {
          repo.refs.delete(command.ref);
          repo.version += 1;
          results.push({ ref: command.ref, ok: true });
        }
        continue;
      }
      const newObj = repo.objects.get(command.newOid);
      if (!newObj) {
        results.push({ ref: command.ref, ok: false, reason: 'missing object' });
        continue;
      }
      if (command.oldOid !== ZERO_OID && command.oldOid !== current) {
        results.push({ ref: command.ref, ok: false, reason: 'stale info' });
        continue;
      }
      if (command.oldOid !== ZERO_OID && current === command.oldOid && command.oldOid !== command.newOid) {
        if (!isAncestor(repo, command.oldOid, command.newOid)) {
          results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
          continue;
        }
      }
      repo.refs.set(command.ref, command.newOid);
      repo.version += 1;
      results.push({ ref: command.ref, ok: true });
    }
  }

  const reportParts: Buffer[] = [pktLine(`${unpackLine}\n`)];
  for (const result of results) {
    reportParts.push(pktLine(result.ok ? `ok ${result.ref}\n` : `ng ${result.ref} ${result.reason ?? 'failed'}\n`));
  }
  reportParts.push(PKT_FLUSH);
  const report = Buffer.concat(reportParts);

  let body: Buffer;
  if (capabilities.includes('side-band-64k')) {
    const wrapped: Buffer[] = [];
    const CHUNK = 32768;
    for (let offset = 0; offset < report.length; offset += CHUNK) {
      wrapped.push(pktLine(Buffer.concat([Buffer.from([1]), report.subarray(offset, offset + CHUNK)])));
    }
    wrapped.push(PKT_FLUSH);
    body = Buffer.concat(wrapped);
  } else {
    body = report;
  }
  return {
    status: 200,
    headers: { 'content-type': 'application/x-git-receive-pack-result' },
    body,
  };
}

// ------------------------------------------------------------------- router
export function handleGit(ctx: Ctx): Reply {
  const { store } = ctx;
  const [owner, repoGit, ...rest] = ctx.segments;
  const name = repoGit.endsWith('.git') ? repoGit.slice(0, -4) : repoGit;
  store.learnViewer(owner);
  const repo = store.resolveRepo(owner, name);
  if (!repo) return errorReply(404, 'Not Found');

  if (rest.length === 2 && rest[0] === 'info' && rest[1] === 'refs' && ctx.method === 'GET') {
    const service = ctx.query.get('service');
    if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
      return { status: 403, headers: { 'content-type': 'text/plain' }, body: 'unsupported service' };
    }
    return advertisement(ctx, repo, service);
  }
  if (rest.length === 1 && rest[0] === 'git-upload-pack' && ctx.method === 'POST') {
    return handleUploadPack(ctx, repo);
  }
  if (rest.length === 1 && rest[0] === 'git-receive-pack' && ctx.method === 'POST') {
    return handleReceivePack(ctx, repo);
  }
  return errorReply(404, 'Not Found');
}
