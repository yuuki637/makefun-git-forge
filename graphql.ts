// GraphQL surface (POST /api/graphql): a compact parser + executor covering
// the fixed document set the conformance suite sends (queries, mutations,
// aliases, inline fragments, variables, introspection for a few types).
import { type Store, type Repo, type Issue, type Pull, computeMerge, decodeGlobalId, globalId, mergePull, pullChanges } from './store.ts';
import { createIssue, createPull } from './rest.ts';
import { type Ctx, type Reply, errorReply, jsonReply } from './web.ts';

// ------------------------------------------------------------------ parsing
type Value =
  | { kind: 'var'; name: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'enum'; value: string }
  | { kind: 'null' }
  | { kind: 'list'; items: Value[] }
  | { kind: 'object'; fields: Record<string, Value> };

interface FieldSel {
  kind: 'field';
  name: string;
  alias?: string;
  args: Record<string, Value>;
  selectionSet?: Selection[];
}
type Selection =
  | FieldSel
  | { kind: 'spread'; name: string }
  | { kind: 'inline'; typeCondition?: string; selectionSet: Selection[] };

interface Operation {
  type: 'query' | 'mutation';
  name?: string;
  varDefaults: Record<string, Value>;
  selectionSet: Selection[];
}

interface FragmentDef {
  typeCondition: string;
  selectionSet: Selection[];
}

interface Token {
  kind: 'name' | 'string' | 'number' | 'punct' | 'spread' | 'eof';
  value: string;
}

function tokenize(input: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < input.length) {
    const ch = input[i];
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',' || ch === '﻿') {
      i++;
      continue;
    }
    if (ch === '#') {
      while (i < input.length && input[i] !== '\n') i++;
      continue;
    }
    if (ch === '.' && input.slice(i, i + 3) === '...') {
      tokens.push({ kind: 'spread', value: '...' });
      i += 3;
      continue;
    }
    if ('!$():=@[]{}|&'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch });
      i++;
      continue;
    }
    if (ch === '"') {
      if (input.slice(i, i + 3) === '"""') {
        const end = input.indexOf('"""', i + 3);
        tokens.push({ kind: 'string', value: input.slice(i + 3, end === -1 ? input.length : end) });
        i = end === -1 ? input.length : end + 3;
        continue;
      }
      let j = i + 1;
      let out = '';
      while (j < input.length && input[j] !== '"') {
        if (input[j] === '\\' && j + 1 < input.length) {
          const esc = input[j + 1];
          if (esc === 'n') out += '\n';
          else if (esc === 't') out += '\t';
          else if (esc === 'r') out += '\r';
          else if (esc === 'b') out += '\b';
          else if (esc === 'f') out += '\f';
          else if (esc === 'u') {
            out += String.fromCharCode(parseInt(input.slice(j + 2, j + 6), 16));
            j += 4;
          } else out += esc;
          j += 2;
        } else {
          out += input[j];
          j++;
        }
      }
      tokens.push({ kind: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(ch)) {
      let j = i;
      let isFloat = false;
      while (j < input.length && /[0-9.eE+-]/.test(input[j])) {
        if (input[j] === '.' || input[j] === 'e' || input[j] === 'E') isFloat = true;
        j++;
      }
      tokens.push({ kind: 'number', value: input.slice(i, j) });
      void isFloat;
      i = j;
      continue;
    }
    if (/[_A-Za-z]/.test(ch)) {
      let j = i;
      while (j < input.length && /[_0-9A-Za-z]/.test(input[j])) j++;
      tokens.push({ kind: 'name', value: input.slice(i, j) });
      i = j;
      continue;
    }
    throw new Error(`unexpected character ${JSON.stringify(ch)}`);
  }
  tokens.push({ kind: 'eof', value: '' });
  return tokens;
}

class Parser {
  tokens: Token[];
  pos = 0;
  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }
  peek(): Token {
    return this.tokens[this.pos];
  }
  next(): Token {
    return this.tokens[this.pos++];
  }
  expectPunct(value: string): void {
    const tok = this.next();
    if (tok.kind !== 'punct' || tok.value !== value) throw new Error(`expected ${value}, got ${tok.value}`);
  }
  skipPunct(value: string): boolean {
    const tok = this.peek();
    if (tok.kind === 'punct' && tok.value === value) {
      this.pos++;
      return true;
    }
    return false;
  }
  expectName(): string {
    const tok = this.next();
    if (tok.kind !== 'name') throw new Error(`expected name, got ${tok.value}`);
    return tok.value;
  }

  parseValue(): Value {
    const tok = this.next();
    if (tok.kind === 'punct' && tok.value === '$') return { kind: 'var', name: this.expectName() };
    if (tok.kind === 'string') return { kind: 'string', value: tok.value };
    if (tok.kind === 'number') {
      return tok.value.includes('.') || tok.value.includes('e') || tok.value.includes('E')
        ? { kind: 'float', value: Number(tok.value) }
        : { kind: 'int', value: Number(tok.value) };
    }
    if (tok.kind === 'name') {
      if (tok.value === 'true') return { kind: 'boolean', value: true };
      if (tok.value === 'false') return { kind: 'boolean', value: false };
      if (tok.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: tok.value };
    }
    if (tok.kind === 'punct' && tok.value === '[') {
      const items: Value[] = [];
      while (!this.skipPunct(']')) items.push(this.parseValue());
      return { kind: 'list', items };
    }
    if (tok.kind === 'punct' && tok.value === '{') {
      const fields: Record<string, Value> = {};
      while (!this.skipPunct('}')) {
        const name = this.expectName();
        this.expectPunct(':');
        fields[name] = this.parseValue();
      }
      return { kind: 'object', fields };
    }
    throw new Error(`unexpected value token ${tok.value}`);
  }

  parseSelectionSet(): Selection[] {
    this.expectPunct('{');
    const selections: Selection[] = [];
    while (!this.skipPunct('}')) selections.push(this.parseSelection());
    return selections;
  }

  parseSelection(): Selection {
    const tok = this.peek();
    if (tok.kind === 'spread') {
      this.next();
      const nameTok = this.peek();
      if (nameTok.kind === 'name' && nameTok.value === 'on') {
        this.next();
        const typeCondition = this.expectName();
        // skip directives
        this.skipDirectives();
        const selectionSet = this.parseSelectionSet();
        return { kind: 'inline', typeCondition, selectionSet };
      }
      const name = this.expectName();
      this.skipDirectives();
      return { kind: 'spread', name };
    }
    let name = this.expectName();
    let alias: string | undefined;
    if (this.skipPunct(':')) {
      alias = name;
      name = this.expectName();
    }
    const args: Record<string, Value> = {};
    if (this.skipPunct('(')) {
      while (!this.skipPunct(')')) {
        const argName = this.expectName();
        this.expectPunct(':');
        args[argName] = this.parseValue();
      }
    }
    this.skipDirectives();
    let selectionSet: Selection[] | undefined;
    if (this.peek().kind === 'punct' && this.peek().value === '{') selectionSet = this.parseSelectionSet();
    return { kind: 'field', name, alias, args, selectionSet };
  }

  skipDirectives(): void {
    while (this.peek().kind === 'punct' && this.peek().value === '@') {
      this.next();
      this.expectName();
      if (this.skipPunct('(')) {
        let depth = 1;
        while (depth > 0) {
          const tok = this.next();
          if (tok.kind === 'eof') break;
          if (tok.kind === 'punct' && tok.value === '(') depth++;
          if (tok.kind === 'punct' && tok.value === ')') depth--;
        }
      }
    }
  }

  parseDocument(): { operations: Operation[]; fragments: Map<string, FragmentDef> } {
    const operations: Operation[] = [];
    const fragments = new Map<string, FragmentDef>();
    while (this.peek().kind !== 'eof') {
      const tok = this.peek();
      if (tok.kind === 'punct' && tok.value === '{') {
        operations.push({ type: 'query', varDefaults: {}, selectionSet: this.parseSelectionSet() });
        continue;
      }
      const keyword = this.expectName();
      if (keyword === 'query' || keyword === 'mutation' || keyword === 'subscription') {
        let name: string | undefined;
        if (this.peek().kind === 'name') name = this.expectName();
        const varDefaults: Record<string, Value> = {};
        if (this.skipPunct('(')) {
          while (!this.skipPunct(')')) {
            this.expectPunct('$');
            const varName = this.expectName();
            this.expectPunct(':');
            this.parseType();
            if (this.skipPunct('=')) varDefaults[varName] = this.parseValue();
            this.skipDirectives();
          }
        }
        this.skipDirectives();
        const selectionSet = this.parseSelectionSet();
        operations.push({ type: keyword === 'mutation' ? 'mutation' : 'query', name, varDefaults, selectionSet });
        continue;
      }
      if (keyword === 'fragment') {
        const fragName = this.expectName();
        const onTok = this.expectName();
        if (onTok !== 'on') throw new Error('expected on in fragment definition');
        const typeCondition = this.expectName();
        this.skipDirectives();
        const selectionSet = this.parseSelectionSet();
        fragments.set(fragName, { typeCondition, selectionSet });
        continue;
      }
      throw new Error(`unexpected definition ${keyword}`);
    }
    return { operations, fragments };
  }

  parseType(): void {
    if (this.skipPunct('[')) {
      this.parseType();
      this.expectPunct(']');
    } else {
      this.expectName();
    }
    this.skipPunct('!');
  }
}

function evalValue(value: Value, vars: Record<string, unknown>, defaults: Record<string, Value>): unknown {
  switch (value.kind) {
    case 'var': {
      if (value.name in vars) return vars[value.name];
      if (value.name in defaults) return evalValue(defaults[value.name], vars, defaults);
      return null;
    }
    case 'string':
    case 'int':
    case 'float':
    case 'boolean':
    case 'enum':
      return value.value;
    case 'null':
      return null;
    case 'list':
      return value.items.map((item) => evalValue(item, vars, defaults));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const [key, v] of Object.entries(value.fields)) out[key] = evalValue(v, vars, defaults);
      return out;
    }
  }
}

// ------------------------------------------------------------------- schema
const TYPE_FIELDS: Record<string, string[]> = {
  Query: ['viewer', 'repository', 'node', 'user', 'organization', 'search', 'rateLimit', '__type', '__schema'],
  Mutation: [
    'createIssue', 'closeIssue', 'reopenIssue', 'updateIssue', 'createPullRequest', 'mergePullRequest',
    'addComment', 'addLabelsToLabelable', 'removeLabelsFromLabelable', 'updatePullRequest', 'closePullRequest',
  ],
  Repository: [
    'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission',
    'hasIssuesEnabled', 'issue', 'issues', 'pullRequest', 'pullRequests', 'description', 'isPrivate',
    'url', 'createdAt', 'updatedAt', 'stargazerCount', 'forkCount', 'isArchived', 'isFork', 'parent',
    'primaryLanguage', 'licenseInfo', 'defaultBranch', 'refs', 'ref', 'object', 'viewerCanAdminister',
  ],
  Issue: [
    'id', 'number', 'title', 'state', 'body', 'author', 'labels', 'comments', 'createdAt', 'updatedAt',
    'url', 'repository', 'closed', 'stateReason', 'viewerCanUpdate', 'assignees', 'milestone',
  ],
  PullRequest: [
    'id', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName',
    'files', 'author', 'createdAt', 'updatedAt', 'closed', 'mergedAt', 'mergeCommit', 'baseRef',
    'headRef', 'additions', 'deletions', 'changedFiles', 'url', 'repository', 'headRefOid', 'baseRefOid',
  ],
  User: ['id', 'login', 'name', 'email', 'url', 'avatarUrl', 'databaseId'],
  Ref: ['id', 'name', 'prefix', 'target', 'repository'],
  Commit: ['id', 'oid', 'message', 'tree'],
  Tree: ['id', 'oid', 'entries'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'],
  IssueConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  IssueEdge: ['node', 'cursor'],
  PullRequestConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestEdge: ['node', 'cursor'],
  PullRequestChangedFileConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestChangedFile: ['path', 'additions', 'deletions', 'changeType'],
  Label: ['id', 'name', 'color', 'description'],
  LabelConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  IssueComment: ['id', 'body', 'author', 'createdAt', 'updatedAt', 'url'],
  IssueCommentConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  CreateIssuePayload: ['issue', 'clientMutationId'],
  CloseIssuePayload: ['issue', 'clientMutationId'],
  CreatePullRequestPayload: ['pullRequest', 'clientMutationId'],
  MergePullRequestPayload: ['pullRequest', 'clientMutationId'],
  AddCommentPayload: ['commentEdge', 'subject', 'clientMutationId'],
  CommentEdge: ['node', 'cursor'],
  __Type: ['name', 'kind', 'description', 'fields', 'enumValues', 'possibleTypes', 'inputFields', 'ofType', 'specifiedByURL'],
  __Field: ['name', 'description', 'args', 'type', 'isDeprecated', 'deprecationReason'],
  __EnumValue: ['name', 'description', 'isDeprecated', 'deprecationReason'],
  __Schema: ['types', 'queryType', 'mutationType', 'subscriptionType', 'directives'],
  Node: ['id'],
};

const CHILD_TYPE: Record<string, Record<string, string>> = {
  Query: { viewer: 'User', repository: 'Repository', node: 'Node', user: 'User', organization: 'User', __type: '__Type', __schema: '__Schema' },
  Mutation: {
    createIssue: 'CreateIssuePayload', closeIssue: 'CloseIssuePayload', reopenIssue: 'CloseIssuePayload',
    updateIssue: 'CreateIssuePayload', createPullRequest: 'CreatePullRequestPayload',
    mergePullRequest: 'MergePullRequestPayload', addComment: 'AddCommentPayload',
    updatePullRequest: 'CreatePullRequestPayload', closePullRequest: 'MergePullRequestPayload',
    addLabelsToLabelable: 'CreateIssuePayload', removeLabelsFromLabelable: 'CreateIssuePayload',
  },
  Repository: {
    owner: 'User', defaultBranchRef: 'Ref', issue: 'Issue', issues: 'IssueConnection',
    pullRequest: 'PullRequest', pullRequests: 'PullRequestConnection', parent: 'Repository',
  },
  Issue: { author: 'User', labels: 'LabelConnection', comments: 'IssueCommentConnection', repository: 'Repository' },
  PullRequest: {
    files: 'PullRequestChangedFileConnection', author: 'User', mergeCommit: 'Commit',
    baseRef: 'Ref', headRef: 'Ref', repository: 'Repository',
  },
  Ref: { target: 'Commit', repository: 'Repository' },
  IssueConnection: { nodes: 'Issue', edges: 'IssueEdge', pageInfo: 'PageInfo' },
  IssueEdge: { node: 'Issue' },
  PullRequestConnection: { nodes: 'PullRequest', edges: 'PullRequestEdge', pageInfo: 'PageInfo' },
  PullRequestEdge: { node: 'PullRequest' },
  PullRequestChangedFileConnection: { nodes: 'PullRequestChangedFile', pageInfo: 'PageInfo' },
  LabelConnection: { nodes: 'Label', pageInfo: 'PageInfo' },
  IssueCommentConnection: { nodes: 'IssueComment', pageInfo: 'PageInfo' },
  CreateIssuePayload: { issue: 'Issue' },
  CloseIssuePayload: { issue: 'Issue' },
  CreatePullRequestPayload: { pullRequest: 'PullRequest' },
  MergePullRequestPayload: { pullRequest: 'PullRequest' },
  AddCommentPayload: { commentEdge: 'CommentEdge', subject: 'Issue' },
  CommentEdge: { node: 'IssueComment' },
  __Type: { fields: '__Field', enumValues: '__EnumValue', possibleTypes: '__Type', inputFields: '__Field', ofType: '__Type' },
  __Schema: { types: '__Type', queryType: '__Type', mutationType: '__Type', subscriptionType: '__Type' },
};

const SEARCH_TYPE_VALUES = ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'];

// -------------------------------------------------------------------- nodes
interface Node {
  kind: string;
  repo?: Repo;
  issue?: Issue;
  pull?: Pull;
  login?: string;
  name?: string;
  items?: Node[];
  hasNextPage?: boolean;
  endCursor?: string | null;
  totalCount?: number;
  payloadIssue?: Node;
  payloadPull?: Node;
  commentNode?: Node;
  commentId?: number;
  body?: string;
  path?: string;
}

function repoNode(repo: Repo): Node {
  return { kind: 'Repository', repo };
}
function issueNode(repo: Repo, issue: Issue): Node {
  return { kind: 'Issue', repo, issue };
}
function pullNode(repo: Repo, pull: Pull): Node {
  return { kind: 'PullRequest', repo, pull };
}

function encodeCursor(index: number): string {
  return Buffer.from(`cursor:${index}`, 'utf8').toString('base64');
}
function decodeCursor(cursor: string): number {
  try {
    const text = Buffer.from(cursor, 'base64').toString('utf8');
    const n = Number(text.replace('cursor:', ''));
    return Number.isFinite(n) ? n : -1;
  } catch {
    return -1;
  }
}

class GqlError extends Error {}

function issuesConnection(repo: Repo, args: Record<string, unknown>): Node {
  let list = [...repo.issues.values()];
  const states = args.states;
  if (Array.isArray(states) && states.length > 0) {
    const wanted = states.map((s) => String(s).toLowerCase());
    list = list.filter((i) => wanted.includes(i.state));
  }
  const orderBy = args.orderBy as { field?: string; direction?: string } | undefined;
  list.sort((a, b) => a.number - b.number);
  if (orderBy && String(orderBy.direction) === 'DESC') list.reverse();
  const first = typeof args.first === 'number' ? (args.first as number) : list.length;
  let start = 0;
  if (typeof args.after === 'string') start = decodeCursor(args.after) + 1;
  const slice = list.slice(start, start + Math.max(0, first));
  const hasNextPage = start + slice.length < list.length;
  const endCursor = slice.length > 0 ? encodeCursor(start + slice.length - 1) : typeof args.after === 'string' ? args.after : null;
  return {
    kind: 'IssueConnection',
    repo,
    items: slice.map((i) => issueNode(repo, i)),
    hasNextPage,
    endCursor,
    totalCount: list.length,
  };
}

function resolveField(store: Store, node: Node, field: string, args: Record<string, unknown>): unknown {
  const repo = node.repo as Repo;
  switch (node.kind) {
    case 'Query': {
      if (field === 'viewer') return { kind: 'User', login: store.viewerLogin() } as Node;
      if (field === 'repository') {
        const found = store.resolveRepo(String(args.owner ?? ''), String(args.name ?? ''));
        return found ? repoNode(found) : null;
      }
      if (field === 'node') return resolveNode(store, String(args.id ?? ''));
      if (field === '__type') {
        const name = String(args.name ?? '');
        if (TYPE_FIELDS[name] || name === 'SearchType' || ['String', 'Int', 'Boolean', 'ID', 'Float'].includes(name)) {
          return { kind: '__Type', name } as Node;
        }
        return null;
      }
      if (field === '__schema') return { kind: '__Schema' } as Node;
      if (field === 'user') return { kind: 'User', login: String(args.login ?? store.viewerLogin()) } as Node;
      return null;
    }
    case 'Mutation':
      return resolveMutation(store, field, args);
    case 'User':
      if (field === 'login') return node.login;
      if (field === 'id') return globalId('User', 1000);
      if (field === 'databaseId') return 1000;
      if (field === 'name') return node.login;
      if (field === 'email') return `${node.login}@example.invalid`;
      if (field === 'url' || field === 'avatarUrl') return '';
      return null;
    case 'Repository': {
      if (field === 'id') return globalId('Repository', repo.id);
      if (field === 'databaseId') return repo.id;
      if (field === 'name') return repo.name;
      if (field === 'nameWithOwner') return `${store.viewerLogin()}/${repo.name}`;
      if (field === 'owner') return { kind: 'User', login: store.viewerLogin() } as Node;
      if (field === 'defaultBranchRef') {
        return repo.refs.has(`refs/heads/${repo.defaultBranch}`) ? ({ kind: 'Ref', repo, name: repo.defaultBranch } as Node) : null;
      }
      if (field === 'viewerPermission') return 'ADMIN';
      if (field === 'hasIssuesEnabled') return true;
      if (field === 'description') return repo.description;
      if (field === 'isPrivate') return repo.isPrivate;
      if (field === 'issue') {
        const issue = repo.issues.get(Number(args.number));
        return issue ? issueNode(repo, issue) : null;
      }
      if (field === 'issues') return issuesConnection(repo, args);
      if (field === 'pullRequest') {
        const pull = repo.pulls.get(Number(args.number));
        return pull ? pullNode(repo, pull) : null;
      }
      if (field === 'pullRequests') {
        let list = [...repo.pulls.values()].sort((a, b) => a.number - b.number);
        const states = args.states;
        if (Array.isArray(states) && states.length > 0) {
          const wanted = states.map((s) => String(s));
          list = list.filter((p) => wanted.includes(p.merged ? 'MERGED' : p.state.toUpperCase()));
        }
        const first = typeof args.first === 'number' ? (args.first as number) : list.length;
        return {
          kind: 'PullRequestConnection',
          repo,
          items: list.slice(0, Math.max(0, first)).map((p) => pullNode(repo, p)),
          totalCount: list.length,
          hasNextPage: false,
          endCursor: null,
        } as Node;
      }
      if (field === 'createdAt') return repo.createdAt;
      if (field === 'updatedAt') return repo.updatedAt;
      if (field === 'stargazerCount' || field === 'forkCount') return 0;
      if (field === 'url') return '';
      return null;
    }
    case 'Issue': {
      const issue = node.issue as Issue;
      if (field === 'id') return globalId('Issue', repo.id, issue.number);
      if (field === 'number') return issue.number;
      if (field === 'title') return issue.title;
      if (field === 'body') return issue.body;
      if (field === 'state') return issue.state.toUpperCase();
      if (field === 'author') return { kind: 'User', login: store.viewerLogin() } as Node;
      if (field === 'repository') return repoNode(repo);
      if (field === 'createdAt') return issue.createdAt;
      if (field === 'updatedAt') return issue.updatedAt;
      if (field === 'closed') return issue.state === 'closed';
      if (field === 'url') return '';
      if (field === 'labels') {
        return {
          kind: 'LabelConnection',
          items: issue.labels.map((name) => ({ kind: 'Label', name, body: repo.labels.get(name)?.color } as Node)),
          totalCount: issue.labels.length,
        } as Node;
      }
      if (field === 'comments') {
        const list = repo.comments.get(issue.number) ?? [];
        return {
          kind: 'IssueCommentConnection',
          items: list.map((c) => ({ kind: 'IssueComment', repo, commentId: c.id, body: c.body } as Node)),
          totalCount: list.length,
        } as Node;
      }
      return null;
    }
    case 'PullRequest': {
      const pull = node.pull as Pull;
      if (field === 'id') return globalId('PullRequest', repo.id, pull.number);
      if (field === 'number') return pull.number;
      if (field === 'title') return pull.title;
      if (field === 'body') return pull.body;
      if (field === 'state') return pull.merged ? 'MERGED' : pull.state.toUpperCase();
      if (field === 'merged') return pull.merged;
      if (field === 'headRefName') return pull.head;
      if (field === 'baseRefName') return pull.base;
      if (field === 'mergeable') {
        if (pull.merged) return 'UNKNOWN';
        const computed = computeMerge(repo, pull);
        if (!computed) return 'UNKNOWN';
        return computed.conflict ? 'CONFLICTING' : 'MERGEABLE';
      }
      if (field === 'files') {
        const changes = pullChanges(repo, pull);
        const first = typeof args.first === 'number' ? (args.first as number) : changes.length;
        return {
          kind: 'PullRequestChangedFileConnection',
          items: changes.slice(0, Math.max(0, first)).map((c) => ({ kind: 'PullRequestChangedFile', path: c.filename } as Node)),
          totalCount: changes.length,
        } as Node;
      }
      if (field === 'author') return { kind: 'User', login: store.viewerLogin() } as Node;
      if (field === 'repository') return repoNode(repo);
      if (field === 'createdAt') return pull.createdAt;
      if (field === 'updatedAt') return pull.updatedAt;
      if (field === 'closed') return pull.state === 'closed';
      if (field === 'mergedAt') return pull.mergedAt;
      if (field === 'additions' || field === 'deletions' || field === 'changedFiles') {
        const changes = pullChanges(repo, pull);
        if (field === 'changedFiles') return changes.length;
        return changes.reduce((sum, c) => sum + (field === 'additions' ? c.additions : c.deletions), 0);
      }
      if (field === 'url') return '';
      return null;
    }
    case 'Ref':
      if (field === 'name') return node.name;
      if (field === 'id') return globalId('Ref', repo?.id ?? 0, node.name ?? '');
      if (field === 'prefix') return 'refs/heads/';
      if (field === 'repository' && repo) return repoNode(repo);
      if (field === 'target') {
        const oid = repo?.refs.get(`refs/heads/${node.name}`);
        return oid ? ({ kind: 'Commit', name: oid } as Node) : null;
      }
      return null;
    case 'Commit':
      if (field === 'oid' || field === 'id') return node.name;
      return null;
    case 'IssueConnection':
    case 'PullRequestConnection':
    case 'PullRequestChangedFileConnection':
    case 'LabelConnection':
    case 'IssueCommentConnection': {
      if (field === 'nodes') return node.items ?? [];
      if (field === 'totalCount') return node.totalCount ?? (node.items ?? []).length;
      if (field === 'pageInfo') {
        return { kind: 'PageInfo', hasNextPage: node.hasNextPage ?? false, endCursor: node.endCursor ?? null } as Node;
      }
      if (field === 'edges') {
        return (node.items ?? []).map((item) => ({ kind: node.kind.replace('Connection', 'Edge'), payloadIssue: item, name: node.endCursor ?? undefined } as unknown as Node));
      }
      return null;
    }
    case 'PageInfo':
      if (field === 'hasNextPage') return node.hasNextPage ?? false;
      if (field === 'hasPreviousPage') return false;
      if (field === 'endCursor') return node.endCursor ?? null;
      if (field === 'startCursor') return node.endCursor ?? null;
      return null;
    case 'Label':
      if (field === 'name') return node.name;
      if (field === 'color') return node.body ?? 'ededed';
      if (field === 'id') return globalId('Label', node.name ?? '');
      return null;
    case 'PullRequestChangedFile':
      if (field === 'path') return node.path;
      return null;
    case 'IssueComment':
      if (field === 'id') return globalId('IssueComment', node.commentId ?? 0);
      if (field === 'body') return node.body;
      if (field === 'author') return { kind: 'User', login: store.viewerLogin() } as Node;
      return null;
    case 'CreateIssuePayload':
    case 'CloseIssuePayload':
      if (field === 'issue') return node.payloadIssue ?? null;
      if (field === 'clientMutationId') return null;
      return null;
    case 'CreatePullRequestPayload':
    case 'MergePullRequestPayload':
      if (field === 'pullRequest') return node.payloadPull ?? null;
      if (field === 'clientMutationId') return null;
      return null;
    case 'AddCommentPayload':
      if (field === 'commentEdge') return { kind: 'CommentEdge', commentNode: node.commentNode } as Node;
      if (field === 'subject') return node.payloadIssue ?? null;
      if (field === 'clientMutationId') return null;
      return null;
    case 'CommentEdge':
      if (field === 'node') return node.commentNode ?? null;
      if (field === 'cursor') return encodeCursor(0);
      return null;
    case '__Type': {
      if (field === 'name') return node.name;
      if (field === 'kind') return node.name === 'SearchType' ? 'ENUM' : TYPE_FIELDS[node.name ?? ''] ? 'OBJECT' : 'SCALAR';
      if (field === 'fields') {
        const fields = TYPE_FIELDS[node.name ?? ''];
        if (!fields) return null;
        return fields.map((name) => ({ kind: '__Field', name } as Node));
      }
      if (field === 'enumValues') {
        if (node.name !== 'SearchType') return null;
        return SEARCH_TYPE_VALUES.map((name) => ({ kind: '__EnumValue', name } as Node));
      }
      return null;
    }
    case '__Field':
      if (field === 'name') return node.name;
      if (field === 'description') return null;
      if (field === 'isDeprecated') return false;
      return null;
    case '__EnumValue':
      if (field === 'name') return node.name;
      if (field === 'description') return null;
      if (field === 'isDeprecated') return false;
      return null;
    case '__Schema':
      if (field === 'queryType') return { kind: '__Type', name: 'Query' } as Node;
      if (field === 'mutationType') return { kind: '__Type', name: 'Mutation' } as Node;
      if (field === 'types') return Object.keys(TYPE_FIELDS).map((name) => ({ kind: '__Type', name } as Node));
      return null;
  }
  return null;
}

function resolveNode(store: Store, id: string): Node | null {
  const decoded = decodeGlobalId(id);
  if (!decoded) return null;
  if (decoded.type === 'Repository') {
    const repo = store.repoById(Number(decoded.parts[0]));
    return repo ? repoNode(repo) : null;
  }
  if (decoded.type === 'Issue') {
    const repo = store.repoById(Number(decoded.parts[0]));
    const issue = repo?.issues.get(Number(decoded.parts[1]));
    return repo && issue ? issueNode(repo, issue) : null;
  }
  if (decoded.type === 'PullRequest') {
    const repo = store.repoById(Number(decoded.parts[0]));
    const pull = repo?.pulls.get(Number(decoded.parts[1]));
    return repo && pull ? pullNode(repo, pull) : null;
  }
  if (decoded.type === 'User') return { kind: 'User', login: store.viewerLogin() } as Node;
  return null;
}

function resolveMutation(store: Store, field: string, args: Record<string, unknown>): unknown {
  const input = (args.input ?? {}) as Record<string, unknown>;
  if (field === 'createIssue') {
    const decoded = decodeGlobalId(String(input.repositoryId ?? ''));
    const repo = decoded && decoded.type === 'Repository' ? store.repoById(Number(decoded.parts[0])) : undefined;
    if (!repo) throw new GqlError('Repository not found');
    const title = String(input.title ?? '');
    if (!title) throw new GqlError('Title is required');
    const issue = createIssue(store, repo, { title, body: String(input.body ?? ''), labels: [] });
    return { kind: 'CreateIssuePayload', payloadIssue: issueNode(repo, issue) } as Node;
  }
  if (field === 'closeIssue' || field === 'reopenIssue') {
    const decoded = decodeGlobalId(String(input.issueId ?? ''));
    const repo = decoded && decoded.type === 'Issue' ? store.repoById(Number(decoded.parts[0])) : undefined;
    const issue = repo?.issues.get(Number(decoded?.parts[1]));
    if (!repo || !issue) throw new GqlError('Issue not found');
    issue.state = field === 'closeIssue' ? 'closed' : 'open';
    issue.updatedAt = store.nowIso();
    return { kind: 'CloseIssuePayload', payloadIssue: issueNode(repo, issue) } as Node;
  }
  if (field === 'createPullRequest') {
    const decoded = decodeGlobalId(String(input.repositoryId ?? ''));
    const repo = decoded && decoded.type === 'Repository' ? store.repoById(Number(decoded.parts[0])) : undefined;
    if (!repo) throw new GqlError('Repository not found');
    const head = String(input.headRefName ?? '');
    const base = String(input.baseRefName ?? '');
    if (!repo.refs.has(`refs/heads/${head}`)) throw new GqlError(`Head branch "${head}" does not exist`);
    if (!repo.refs.has(`refs/heads/${base}`)) throw new GqlError(`Base branch "${base}" does not exist`);
    const existing = [...repo.pulls.values()].find((p) => p.state === 'open' && p.head === head && p.base === base);
    const pull =
      existing ??
      createPull(store, repo, {
        title: String(input.title ?? ''),
        body: String(input.body ?? ''),
        head,
        base,
      });
    return { kind: 'CreatePullRequestPayload', payloadPull: pullNode(repo, pull) } as Node;
  }
  if (field === 'mergePullRequest') {
    const decoded = decodeGlobalId(String(input.pullRequestId ?? ''));
    const repo = decoded && decoded.type === 'PullRequest' ? store.repoById(Number(decoded.parts[0])) : undefined;
    const pull = repo?.pulls.get(Number(decoded?.parts[1]));
    if (!repo || !pull) throw new GqlError('Pull request not found');
    if (!pull.merged) {
      const method = String(input.mergeMethod ?? 'SQUASH').toUpperCase() === 'MERGE' ? 'merge' : 'squash';
      const result = mergePull(store, repo, pull, method as 'squash' | 'merge');
      if (result === 'conflict') throw new GqlError('Pull request is not mergeable');
      if (result === null) throw new GqlError('Pull request is not mergeable');
    }
    return { kind: 'MergePullRequestPayload', payloadPull: pullNode(repo, pull) } as Node;
  }
  if (field === 'addComment') {
    const decoded = decodeGlobalId(String(input.subjectId ?? ''));
    const repo = decoded && decoded.type === 'Issue' ? store.repoById(Number(decoded.parts[0])) : undefined;
    const issue = repo?.issues.get(Number(decoded?.parts[1]));
    if (!repo || !issue) throw new GqlError('Subject not found');
    const comment = { id: store.nextCommentId++, body: String(input.body ?? '') };
    const list = repo.comments.get(issue.number) ?? [];
    list.push(comment);
    repo.comments.set(issue.number, list);
    return {
      kind: 'AddCommentPayload',
      commentNode: { kind: 'IssueComment', repo, commentId: comment.id, body: comment.body } as Node,
      payloadIssue: issueNode(repo, issue),
    } as Node;
  }
  throw new GqlError(`Mutation ${field} is not supported`);
}

// ---------------------------------------------------------------- execution
interface ExecCtx {
  store: Store;
  vars: Record<string, unknown>;
  defaults: Record<string, Value>;
  fragments: Map<string, FragmentDef>;
}

function expandSelections(selections: Selection[], nodeKind: string, ctx: ExecCtx, out: FieldSel[], depth = 0): void {
  if (depth > 16) return;
  for (const sel of selections) {
    if (sel.kind === 'field') out.push(sel);
    else if (sel.kind === 'inline') {
      if (!sel.typeCondition || sel.typeCondition === nodeKind || sel.typeCondition === 'Node') {
        expandSelections(sel.selectionSet, nodeKind, ctx, out, depth + 1);
      }
    } else if (sel.kind === 'spread') {
      const frag = ctx.fragments.get(sel.name);
      if (frag && (!frag.typeCondition || frag.typeCondition === nodeKind || frag.typeCondition === 'Node')) {
        expandSelections(frag.selectionSet, nodeKind, ctx, out, depth + 1);
      }
    }
  }
}

function validateSelections(selections: Selection[], typeName: string, ctx: ExecCtx, errors: string[], depth = 0): void {
  if (depth > 24) return;
  const fieldSet = new Set<string>([...(TYPE_FIELDS[typeName] ?? []), ...(typeName === 'Node' ? TYPE_FIELDS.Repository : [])]);
  for (const sel of selections) {
    if (sel.kind === 'inline') {
      validateSelections(sel.selectionSet, sel.typeCondition ?? typeName, ctx, errors, depth + 1);
      continue;
    }
    if (sel.kind === 'spread') {
      const frag = ctx.fragments.get(sel.name);
      if (frag) validateSelections(frag.selectionSet, frag.typeCondition, ctx, errors, depth + 1);
      continue;
    }
    if (sel.name === '__typename') continue;
    if (!fieldSet.has(sel.name)) {
      errors.push(`Field '${sel.name}' doesn't exist on type '${typeName}'`);
      continue;
    }
    if (sel.selectionSet) {
      const child = (CHILD_TYPE[typeName] ?? {})[sel.name];
      if (child) validateSelections(sel.selectionSet, child, ctx, errors, depth + 1);
    }
  }
}

function completeValue(ctx: ExecCtx, node: Node, selections: Selection[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const fields: FieldSel[] = [];
  expandSelections(selections, node.kind, ctx, fields);
  for (const field of fields) {
    const key = field.alias ?? field.name;
    if (field.name === '__typename') {
      out[key] = node.kind;
      continue;
    }
    const args: Record<string, unknown> = {};
    for (const [argName, argValue] of Object.entries(field.args)) {
      args[argName] = evalValue(argValue, ctx.vars, ctx.defaults);
    }
    const value = resolveField(ctx.store, node, field.name, args);
    out[key] = completeAny(ctx, value, field.selectionSet);
  }
  return out;
}

function completeAny(ctx: ExecCtx, value: unknown, selectionSet?: Selection[]): unknown {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) return value.map((item) => completeAny(ctx, item, selectionSet));
  if (typeof value === 'object' && selectionSet) {
    return completeValue(ctx, value as Node, selectionSet);
  }
  return value;
}

function executeOperation(store: Store, op: Operation, vars: Record<string, unknown>, fragments: Map<string, FragmentDef>): Record<string, unknown> {
  const ctx: ExecCtx = { store, vars, defaults: op.varDefaults, fragments };
  const rootKind = op.type === 'mutation' ? 'Mutation' : 'Query';
  const errors: string[] = [];
  validateSelections(op.selectionSet, rootKind, ctx, errors);
  if (errors.length > 0) {
    return { data: null, errors: errors.map((message) => ({ message })) };
  }
  const root: Node = { kind: rootKind };
  try {
    const data = completeValue(ctx, root, op.selectionSet);
    return { data };
  } catch (error) {
    if (error instanceof GqlError) return { data: null, errors: [{ message: error.message }] };
    throw error;
  }
}

export function handleGraphql(ctx: Ctx): Reply {
  const { store } = ctx;
  if (ctx.method !== 'POST') return errorReply(404, 'Not Found');
  if (ctx.jsonError) return jsonReply(400, { errors: [{ message: 'Invalid JSON body' }] });
  const isBatch = Array.isArray(ctx.json);
  const items = (isBatch ? (ctx.json as unknown[]) : [ctx.json]) as Record<string, unknown>[];
  const results: Record<string, unknown>[] = [];
  for (const item of items) {
    const query = item && typeof item === 'object' ? (item as Record<string, unknown>).query : undefined;
    if (typeof query !== 'string') {
      results.push({ errors: [{ message: 'Must provide a query string' }] });
      continue;
    }
    const variables = ((item as Record<string, unknown>).variables ?? {}) as Record<string, unknown>;
    try {
      const doc = new Parser(tokenize(query)).parseDocument();
      const op = doc.operations[0];
      if (!op) {
        results.push({ errors: [{ message: 'No operation found' }] });
        continue;
      }
      results.push(executeOperation(store, op, variables, doc.fragments));
    } catch (error) {
      results.push({ errors: [{ message: error instanceof Error ? error.message : 'Syntax error' }] });
    }
  }
  return jsonReply(200, isBatch ? results : results[0]);
}
