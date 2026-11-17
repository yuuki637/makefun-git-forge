// In-memory store shared by REST, GraphQL, MCP and the git smart-HTTP server.
import {
  type GitObject,
  type ObjType,
  ZERO_OID,
  buildCommitData,
  buildTreeData,
  hashObject,
  parseCommit,
  parseTag,
  parseTree,
} from './git.ts';

export interface Label {
  name: string;
  color: string;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  createdAt: string;
  updatedAt: string;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  head: string;
  base: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface Comment {
  id: number;
  body: string;
}

export interface Repo {
  id: number;
  name: string;
  isPrivate: boolean;
  description: string | null;
  defaultBranch: string;
  objects: Map<string, GitObject>;
  refs: Map<string, string>;
  labels: Map<string, Label>;
  issues: Map<number, Issue>;
  pulls: Map<number, Pull>;
  comments: Map<number, Comment[]>;
  nextNumber: number;
  version: number; // bumped on any git mutation, for pack caching
  createdAt: string;
  updatedAt: string;
}

const EPOCH_MS = Date.UTC(2025, 0, 1, 0, 0, 0);

export class Store {
  repos: Repo[] = [];
  reposByName = new Map<string, Repo>();
  viewer: string | null = null;
  nextRepoId = 700001;
  nextIssueId = 900001;
  nextPullId = 950001;
  nextCommentId = 800001;
  tick = 0;

  nowIso(): string {
    this.tick += 1;
    return new Date(EPOCH_MS + this.tick * 1000).toISOString();
  }
  nowSec(): number {
    this.tick += 1;
    return Math.floor(EPOCH_MS / 1000) + this.tick;
  }

  viewerLogin(): string {
    return this.viewer ?? 'adaiversen';
  }
  learnViewer(owner: string): void {
    if (this.viewer === null && owner) this.viewer = owner;
  }

  createRepo(name: string, isPrivate: boolean, description: string | null): Repo {
    const ts = this.nowIso();
    const repo: Repo = {
      id: this.nextRepoId++,
      name,
      isPrivate,
      description,
      defaultBranch: 'main',
      objects: new Map(),
      refs: new Map(),
      labels: new Map(),
      issues: new Map(),
      pulls: new Map(),
      comments: new Map(),
      nextNumber: 1,
      version: 1,
      createdAt: ts,
      updatedAt: ts,
    };
    this.repos.push(repo);
    this.reposByName.set(name, repo);
    return repo;
  }

  repoById(id: number): Repo | undefined {
    return this.repos.find((r) => r.id === id);
  }

  // Resolve a repo for an (owner, name) request: foreign owner -> missing.
  resolveRepo(owner: string, name: string): Repo | null {
    const repo = this.reposByName.get(name);
    if (!repo) return null;
    if (owner !== this.viewerLogin()) return null;
    return repo;
  }

  deleteRepo(repo: Repo): void {
    this.repos = this.repos.filter((r) => r !== repo);
    this.reposByName.delete(repo.name);
  }
}

// ------------------------------------------------------------ object helpers
export function storeObject(repo: Repo, type: ObjType, data: Buffer): string {
  const oid = hashObject(type, data);
  if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
  return oid;
}

export function peelToCommit(repo: Repo, oid: string): string | null {
  let current = oid;
  for (let i = 0; i < 16; i++) {
    const obj = repo.objects.get(current);
    if (!obj) return null;
    if (obj.type === 'commit') return current;
    if (obj.type === 'tag') {
      current = parseTag(obj.data).object;
      continue;
    }
    return null;
  }
  return null;
}

// Commit-ish resolution for a ref name, oid, or branch name.
export function resolveCommitish(repo: Repo, ref: string | undefined | null): string | null {
  if (!ref) {
    const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
    return tip ? peelToCommit(repo, tip) : null;
  }
  const asRef = repo.refs.get(ref.startsWith('refs/') ? ref : `refs/heads/${ref}`);
  if (asRef) return peelToCommit(repo, asRef);
  if (/^[0-9a-f]{40}$/.test(ref) && repo.objects.has(ref)) return peelToCommit(repo, ref);
  return null;
}

// Flatten a tree (recursively) into path -> {oid, mode}.
export function flattenTree(repo: Repo, treeOid: string, prefix = '', out = new Map<string, { oid: string; mode: string }>()): Map<string, { oid: string; mode: string }> {
  const obj = repo.objects.get(treeOid);
  if (!obj || obj.type !== 'tree') return out;
  for (const entry of parseTree(obj.data)) {
    const path = prefix ? `${prefix}/${entry.name}` : entry.name;
    if (entry.mode === '40000') flattenTree(repo, entry.oid, path, out);
    else out.set(path, { oid: entry.oid, mode: entry.mode });
  }
  return out;
}

export function treeOfCommit(repo: Repo, commitOid: string): string | null {
  const obj = repo.objects.get(commitOid);
  if (!obj || obj.type !== 'commit') return null;
  return parseCommit(obj.data).tree;
}

export function flattenCommitTree(repo: Repo, commitOid: string): Map<string, { oid: string; mode: string }> {
  const tree = treeOfCommit(repo, commitOid);
  if (!tree) return new Map();
  return flattenTree(repo, tree);
}

// Rebuild nested trees from a flat path map; stores tree objects; returns root oid.
export function buildTreeFromFlat(repo: Repo, flat: Map<string, { oid: string; mode: string }>): string {
  interface Node {
    files: Map<string, { oid: string; mode: string }>;
    dirs: Map<string, Node>;
  }
  const root: Node = { files: new Map(), dirs: new Map() };
  for (const [path, value] of flat) {
    const parts = path.split('/');
    let node = root;
    for (let i = 0; i < parts.length - 1; i++) {
      let next = node.dirs.get(parts[i]);
      if (!next) {
        next = { files: new Map(), dirs: new Map() };
        node.dirs.set(parts[i], next);
      }
      node = next;
    }
    node.files.set(parts[parts.length - 1], value);
  }
  const emit = (node: Node): string => {
    const entries: { mode: string; name: string; oid: string }[] = [];
    for (const [name, value] of node.files) entries.push({ mode: value.mode, name, oid: value.oid });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: emit(child) });
    return storeObject(repo, 'tree', buildTreeData(entries));
  };
  return emit(root);
}

export function makeCommit(
  store: Store,
  repo: Repo,
  input: { tree: string; parents: string[]; message: string },
): string {
  const login = store.viewerLogin();
  const data = buildCommitData({
    tree: input.tree,
    parents: input.parents,
    authorName: login,
    authorEmail: `${login}@example.invalid`,
    committerName: login,
    committerEmail: `${login}@example.invalid`,
    timestamp: store.nowSec(),
    message: input.message,
  });
  return storeObject(repo, 'commit', data);
}

// Apply a file change (set or delete a path) on a branch; returns the new commit oid.
export function commitFileChange(
  store: Store,
  repo: Repo,
  branch: string,
  path: string,
  blobOid: string | null,
  message: string,
): string {
  const ref = `refs/heads/${branch}`;
  const tip = repo.refs.get(ref);
  const tipCommit = tip ? peelToCommit(repo, tip) : null;
  const flat = tipCommit ? flattenCommitTree(repo, tipCommit) : new Map<string, { oid: string; mode: string }>();
  if (blobOid === null) flat.delete(path);
  else flat.set(path, { oid: blobOid, mode: '100644' });
  const tree = buildTreeFromFlat(repo, flat);
  const commit = makeCommit(store, repo, {
    tree,
    parents: tipCommit ? [tipCommit] : [],
    message,
  });
  repo.refs.set(ref, commit);
  repo.version += 1;
  repo.updatedAt = store.nowIso();
  return commit;
}

// ------------------------------------------------------------- reachability
// Ancestor map (oid -> shortest distance) following commit parents.
export function ancestorDistances(repo: Repo, start: string, limit = 100000): Map<string, number> {
  const dist = new Map<string, number>();
  const startCommit = peelToCommit(repo, start);
  if (!startCommit) return dist;
  const queue: string[] = [startCommit];
  dist.set(startCommit, 0);
  while (queue.length > 0 && dist.size < limit) {
    const current = queue.shift() as string;
    const d = dist.get(current) as number;
    const obj = repo.objects.get(current);
    if (!obj || obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) {
      if (!dist.has(parent)) {
        dist.set(parent, d + 1);
        queue.push(parent);
      }
    }
  }
  return dist;
}

export function isAncestor(repo: Repo, ancestor: string, descendant: string): boolean {
  const a = peelToCommit(repo, ancestor);
  const d = peelToCommit(repo, descendant);
  if (!a || !d) return false;
  if (a === d) return true;
  return ancestorDistances(repo, d).has(a);
}

export function mergeBase(repo: Repo, a: string, b: string): string | null {
  const ca = peelToCommit(repo, a);
  const cb = peelToCommit(repo, b);
  if (!ca || !cb) return null;
  const ancestorsA = ancestorDistances(repo, ca);
  // BFS from b; first commit also in A's ancestors is a merge base.
  const seen = new Set<string>([cb]);
  const queue: string[] = [cb];
  while (queue.length > 0) {
    const current = queue.shift() as string;
    if (ancestorsA.has(current)) return current;
    const obj = repo.objects.get(current);
    if (!obj || obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) {
      if (!seen.has(parent)) {
        seen.add(parent);
        queue.push(parent);
      }
    }
  }
  return null;
}

// Ordered history (newest first) from a commit tip.
export function commitHistory(repo: Repo, tip: string, limit = 500): string[] {
  const start = peelToCommit(repo, tip);
  if (!start) return [];
  const out: string[] = [];
  const seen = new Set<string>();
  const stack: string[] = [start];
  while (stack.length > 0 && out.length < limit) {
    const current = stack.pop() as string;
    if (seen.has(current)) continue;
    seen.add(current);
    const obj = repo.objects.get(current);
    if (!obj || obj.type !== 'commit') continue;
    out.push(current);
    const parents = parseCommit(obj.data).parents;
    for (let i = parents.length - 1; i >= 0; i--) stack.push(parents[i]);
  }
  return out;
}

// Collect all objects reachable from the given root oids (commits' full
// closure incl. trees/blobs; tags peeled into the set).
export function collectClosure(repo: Repo, roots: string[]): Set<string> {
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
      for (const parent of commit.parents) stack.push(parent);
    } else if (obj.type === 'tree') {
      for (const entry of parseTree(obj.data)) stack.push(entry.oid);
    } else if (obj.type === 'tag') {
      stack.push(parseTag(obj.data).object);
    }
  }
  return out;
}

// --------------------------------------------------------------- pull merges
export interface FileChange {
  filename: string;
  status: 'added' | 'removed' | 'modified';
  additions: number;
  deletions: number;
  changes: number;
}

function lineCount(data: Buffer): number {
  if (data.length === 0) return 0;
  return data.toString('utf8').split('\n').length;
}

// Diff merge-base tree -> head tree (three-dot, like GitHub).
export function pullChanges(repo: Repo, pull: Pull): FileChange[] {
  const baseTip = repo.refs.get(`refs/heads/${pull.base}`);
  const headTip = repo.refs.get(`refs/heads/${pull.head}`);
  if (!baseTip || !headTip) return [];
  const mb = mergeBase(repo, baseTip, headTip) ?? baseTip;
  const fromFlat = flattenCommitTree(repo, mb);
  const toFlat = flattenCommitTree(repo, headTip);
  const paths = new Set<string>([...fromFlat.keys(), ...toFlat.keys()]);
  const changes: FileChange[] = [];
  for (const path of [...paths].sort()) {
    const from = fromFlat.get(path);
    const to = toFlat.get(path);
    if (from && to && from.oid === to.oid) continue;
    const fromObj = from ? repo.objects.get(from.oid) : undefined;
    const toObj = to ? repo.objects.get(to.oid) : undefined;
    const additions = toObj ? lineCount(toObj.data) : 0;
    const deletions = fromObj ? lineCount(fromObj.data) : 0;
    changes.push({
      filename: path,
      status: !from ? 'added' : !to ? 'removed' : 'modified',
      additions,
      deletions,
      changes: additions + deletions,
    });
  }
  return changes;
}

// Compute the merged tree + conflict flag for merging `pull` into its base.
export function computeMerge(repo: Repo, pull: Pull): { conflict: boolean; tree: string; baseTip: string; headTip: string } | null {
  const baseTip = repo.refs.get(`refs/heads/${pull.base}`);
  const headTip = repo.refs.get(`refs/heads/${pull.head}`);
  if (!baseTip || !headTip) return null;
  const mb = mergeBase(repo, baseTip, headTip);
  if (!mb) return null;
  const mbFlat = flattenCommitTree(repo, mb);
  const baseFlat = flattenCommitTree(repo, baseTip);
  const headFlat = flattenCommitTree(repo, headTip);
  const resultFlat = new Map(baseFlat);
  let conflict = false;
  const paths = new Set<string>([...mbFlat.keys(), ...headFlat.keys()]);
  for (const path of paths) {
    const mbEntry = mbFlat.get(path);
    const headEntry = headFlat.get(path);
    if (mbEntry && headEntry && mbEntry.oid === headEntry.oid) continue; // unchanged on head
    if (!mbEntry && !headEntry) continue;
    // head changed this path relative to the merge base
    const baseEntry = baseFlat.get(path);
    const baseChanged = (baseEntry?.oid ?? null) !== (mbEntry?.oid ?? null);
    const sameAsHead = (baseEntry?.oid ?? null) === (headEntry?.oid ?? null);
    if (baseChanged && !sameAsHead) {
      conflict = true;
      continue;
    }
    if (headEntry) resultFlat.set(path, headEntry);
    else resultFlat.delete(path);
  }
  const tree = buildTreeFromFlat(repo, resultFlat);
  return { conflict, tree, baseTip, headTip };
}

// Squash (or merge) `pull` into its base branch. Returns the new commit oid,
// or 'conflict', or null when the pull cannot be merged at all.
export function mergePull(store: Store, repo: Repo, pull: Pull, method: 'squash' | 'merge'): string | 'conflict' | null {
  if (pull.merged || pull.state === 'closed') return null;
  const computed = computeMerge(repo, pull);
  if (!computed) return null;
  if (computed.conflict) return 'conflict';
  const baseCommit = peelToCommit(repo, computed.baseTip);
  const headCommit = peelToCommit(repo, computed.headTip);
  if (!baseCommit || !headCommit) return null;
  let commit: string;
  if (method === 'merge') {
    commit = makeCommit(store, repo, {
      tree: computed.tree,
      parents: [baseCommit, headCommit],
      message: `Merge pull request #${pull.number} from ${pull.head}\n\n${pull.title}`,
    });
  } else {
    const body = pull.body ? `\n\n${pull.body}` : '';
    commit = makeCommit(store, repo, {
      tree: computed.tree,
      parents: [baseCommit],
      message: `${pull.title} (#${pull.number})${body}`,
    });
  }
  repo.refs.set(`refs/heads/${pull.base}`, commit);
  repo.version += 1;
  repo.updatedAt = store.nowIso();
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedAt = store.nowIso();
  pull.mergeCommitSha = commit;
  pull.updatedAt = store.nowIso();
  return commit;
}

// -------------------------------------------------------------- global ids
export function globalId(type: string, ...parts: (string | number)[]): string {
  return Buffer.from(`${type}:${parts.join(':')}`, 'utf8').toString('base64');
}

export function decodeGlobalId(id: string): { type: string; parts: string[] } | null {
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    const split = text.split(':');
    if (split.length < 2) return null;
    return { type: split[0], parts: split.slice(1) };
  } catch {
    return null;
  }
}

export { ZERO_OID };
