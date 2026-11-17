// MCP surface (POST /mcp): JSON-RPC 2.0 streamable HTTP, modern
// server/discover era with a legacy initialize fallback. Same store as REST.
import { Store, mergePull } from './store.ts';
import { createIssue, createPull, issueJson, pullJson, repoJson } from './rest.ts';
import { type Ctx, type Reply, errorReply, jsonReply } from './web.ts';

const SERVER_INFO = { name: 'git-forge', version: '1.0.0' };
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

const TOOLS = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'Repository name.' },
        description: { type: 'string', description: 'Repository description.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login.' },
        repo: { type: 'string', description: 'Repository name.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login.' },
        repo: { type: 'string', description: 'Repository name.' },
        title: { type: 'string', description: 'Issue title.' },
        body: { type: 'string', description: 'Issue body.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login.' },
        repo: { type: 'string', description: 'Repository name.' },
        state: { type: 'string', description: 'Filter by state: open, closed or all.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login.' },
        repo: { type: 'string', description: 'Repository name.' },
        title: { type: 'string', description: 'Pull request title.' },
        head: { type: 'string', description: 'Head branch name.' },
        base: { type: 'string', description: 'Base branch name.' },
        body: { type: 'string', description: 'Pull request body.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request (squash) into its base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login.' },
        repo: { type: 'string', description: 'Repository name.' },
        pull_number: { type: 'number', description: 'Pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

interface RpcOutcome {
  status: number;
  payload?: unknown;
  empty?: boolean;
}

function rpcResult(id: unknown, result: unknown): RpcOutcome {
  return { status: 200, payload: { jsonrpc: '2.0', id, result } };
}
function rpcError(id: unknown, code: number, message: string, status = 200): RpcOutcome {
  return { status, payload: { jsonrpc: '2.0', id, error: { code, message } } };
}

function toolOk(value: Record<string, unknown>): Record<string, unknown> {
  return {
    content: [{ type: 'text', text: JSON.stringify(value) }],
    structuredContent: value,
    isError: false,
  };
}
function toolErr(message: string): Record<string, unknown> {
  return {
    content: [{ type: 'text', text: message }],
    isError: true,
  };
}

function argStr(args: Record<string, unknown>, key: string): string | undefined {
  const value = args[key];
  return typeof value === 'string' && value !== '' ? value : undefined;
}

function callTool(store: Store, name: string, rawArgs: unknown): Record<string, unknown> {
  const args = (rawArgs && typeof rawArgs === 'object' ? rawArgs : {}) as Record<string, unknown>;
  const tool = TOOLS.find((t) => t.name === name);
  if (!tool) throw new RpcFailure(-32602, `Unknown tool: ${name}`);
  // Validate required arguments against the tool schema.
  for (const required of tool.inputSchema.required) {
    const value = args[required];
    if (value === undefined || value === null || value === '') {
      throw new RpcFailure(-32602, `Missing required argument: ${required}`);
    }
  }
  const owner = argStr(args, 'owner');
  const repoName = argStr(args, 'repo');
  const resolve = () => {
    if (!owner || !repoName) return null;
    return store.resolveRepo(owner, repoName);
  };

  switch (name) {
    case 'create_repo': {
      const repoNameArg = argStr(args, 'name') as string;
      const existing = store.reposByName.get(repoNameArg);
      if (existing && store.viewerLogin() === (owner ?? store.viewerLogin())) {
        return toolOk(repoJson(store, existing));
      }
      if (existing) return toolErr(`Repository ${repoNameArg} already exists`);
      const repo = store.createRepo(
        repoNameArg,
        args.private === true,
        typeof args.description === 'string' ? args.description : null,
      );
      return toolOk(repoJson(store, repo));
    }
    case 'get_repo': {
      const repo = resolve();
      if (!repo) return toolErr('Repository not found');
      return toolOk(repoJson(store, repo));
    }
    case 'create_issue': {
      const repo = resolve();
      if (!repo) return toolErr('Repository not found');
      const issue = createIssue(store, repo, {
        title: argStr(args, 'title') as string,
        body: argStr(args, 'body') ?? '',
        labels: [],
      });
      return toolOk(issueJson(store, repo, issue));
    }
    case 'list_issues': {
      const repo = resolve();
      if (!repo) return toolErr('Repository not found');
      const state = argStr(args, 'state') ?? 'all';
      let list = [...repo.issues.values()].sort((a, b) => a.number - b.number);
      if (state === 'open' || state === 'closed') list = list.filter((i) => i.state === state);
      return toolOk({ issues: list.map((i) => issueJson(store, repo, i)) });
    }
    case 'create_pull_request': {
      const repo = resolve();
      if (!repo) return toolErr('Repository not found');
      const head = (argStr(args, 'head') as string).split(':').pop() as string;
      const base = (argStr(args, 'base') as string).split(':').pop() as string;
      if (!repo.refs.has(`refs/heads/${head}`)) return toolErr(`Head branch "${head}" does not exist`);
      if (!repo.refs.has(`refs/heads/${base}`)) return toolErr(`Base branch "${base}" does not exist`);
      const existing = [...repo.pulls.values()].find((p) => p.state === 'open' && p.head === head && p.base === base);
      const pull =
        existing ??
        createPull(store, repo, {
          title: argStr(args, 'title') as string,
          body: argStr(args, 'body') ?? '',
          head,
          base,
        });
      return toolOk(pullJson(store, repo, pull));
    }
    case 'merge_pull_request': {
      const repo = resolve();
      if (!repo) return toolErr('Repository not found');
      const numberRaw = args.pull_number ?? args.number;
      const number = typeof numberRaw === 'number' ? numberRaw : Number(numberRaw);
      const pull = Number.isInteger(number) ? repo.pulls.get(number) : undefined;
      if (!pull) return toolErr('Pull request not found');
      if (pull.merged) return toolOk({ ...pullJson(store, repo, pull), merged: true });
      const result = mergePull(store, repo, pull, 'squash');
      if (result === 'conflict') return toolErr('Pull request is not mergeable: merge conflict');
      if (result === null) return toolErr('Pull request is not mergeable');
      return toolOk({ ...pullJson(store, repo, pull), merged: true, sha: result });
    }
  }
  throw new RpcFailure(-32602, `Unknown tool: ${name}`);
}

class RpcFailure extends Error {
  code: number;
  constructor(code: number, message: string) {
    super(message);
    this.code = code;
  }
}

function dispatch(store: Store, message: Record<string, unknown>): RpcOutcome {
  const method = typeof message.method === 'string' ? message.method : '';
  const hasId = 'id' in message && message.id !== undefined;
  const id = hasId ? message.id : null;
  const params = (message.params && typeof message.params === 'object' ? message.params : {}) as Record<string, unknown>;

  if (!hasId) {
    // Notification: never answered with a JSON-RPC body.
    return { status: 202, empty: true };
  }

  try {
    switch (method) {
      case 'server/discover':
        return rpcResult(id, {
          resultType: 'complete',
          supportedVersions: SUPPORTED_VERSIONS,
          capabilities: { tools: {} },
          serverInfo: SERVER_INFO,
          _meta: { 'io.modelcontextprotocol/serverInfo': SERVER_INFO },
        });
      case 'initialize': {
        const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
        const protocolVersion = SUPPORTED_VERSIONS.includes(requested) ? requested : '2025-11-25';
        return rpcResult(id, {
          protocolVersion,
          capabilities: { tools: {} },
          serverInfo: SERVER_INFO,
        });
      }
      case 'ping':
        return rpcResult(id, {});
      case 'tools/list':
        return rpcResult(id, { tools: TOOLS });
      case 'tools/call': {
        const toolName = typeof params.name === 'string' ? params.name : '';
        const result = callTool(store, toolName, params.arguments ?? {});
        return rpcResult(id, result);
      }
      default:
        return rpcError(id, -32601, `Method not found: ${method}`);
    }
  } catch (error) {
    if (error instanceof RpcFailure) return rpcError(id, error.code, error.message);
    return rpcError(id, -32603, error instanceof Error ? error.message : 'Internal error');
  }
}

export function handleMcp(ctx: Ctx): Reply {
  const { store } = ctx;
  if (ctx.method !== 'POST') return errorReply(405, 'Method Not Allowed');
  if (ctx.jsonError) {
    return jsonReply(400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
  }
  if (Array.isArray(ctx.json)) {
    const outcomes = (ctx.json as Record<string, unknown>[]).map((m) => dispatch(store, m));
    const payloads = outcomes.filter((o) => !o.empty).map((o) => o.payload);
    if (payloads.length === 0) return { status: 202, body: '' };
    return jsonReply(200, payloads);
  }
  if (!ctx.json || typeof ctx.json !== 'object') {
    return jsonReply(400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
  }
  const outcome = dispatch(store, ctx.json as Record<string, unknown>);
  if (outcome.empty) return { status: outcome.status, body: '' };
  return jsonReply(outcome.status, outcome.payload);
}
