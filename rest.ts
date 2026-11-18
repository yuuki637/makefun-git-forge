// REST v3 surface (/api/v3/*).
import {
  type Store,
  type Repo,
  type Issue,
  type Pull,
  commitFileChange,
  commitHistory,
  flattenCommitTree,
  mergePull,
  peelToCommit,
  pullChanges,
  resolveCommitish,
  storeObject,
} from './store.ts';
import { commitHeaderOf, hashObject, parseCommit, parseTree, ZERO_OID } from './git.ts';
import { type Ctx, type Reply, bodyObj, errorReply, jsonReply, paginate, str } from './web.ts';

// ------------------------------------------------------------- serializers
export function repoJson(store: Store, repo: Repo): Record<string, unknown> {
  const owner = store.viewerLogin();
  return {
    id: repo.id,
    node_id: `R_${repo.id}`,
    name: repo.name,
    full_name: `${owner}/${repo.name}`,
    owner: { login: owner, id: 1000, type: 'User' },
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
    html_url: `http://forge.local/${owner}/${repo.name}`,
    clone_url: `http://forge.local/${owner}/${repo.name}.git`,
    has_issues: true,
    open_issues_count: [...repo.issues.values()].filter((i) => i.state === 'open').length,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
  };
}

export function issueJson(store: Store, repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: `I_${issue.id}`,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const label = repo.labels.get(name);
      return { id: 1, name, color: label?.color ?? 'ededed' };
    }),
    user: { login: store.viewerLogin(), id: 1000 },
    comments: (repo.comments.get(issue.number) ?? []).length,
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
  };
}

export function pullJson(store: Store, repo: Repo, pull: Pull): Record<string, unknown> {
  const headSha = repo.refs.get(`refs/heads/${pull.head}`) ?? ZERO_OID;
  const baseSha = repo.refs.get(`refs/heads/${pull.base}`) ?? ZERO_OID;
  const owner = store.viewerLogin();
  return {
    id: pull.id,
    node_id: `PR_${pull.id}`,
    number: pull.number,
    state: pull.state,
    title: pull.title,
    body: pull.body,
    head: { ref: pull.head, sha: headSha, label: `${owner}:${pull.head}` },
    base: { ref: pull.base, sha: baseSha, label: `${owner}:${pull.base}` },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    user: { login: owner, id: 1000 },
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
  };
}

export function createIssue(store: Store, repo: Repo, input: { title: string; body: string; labels: string[] }): Issue {
  const ts = store.nowIso();
  const issue: Issue = {
    id: store.nextIssueId++,
    number: repo.nextNumber++,
    title: input.title,
    body: input.body,
    state: 'open',
    labels: [...input.labels],
    createdAt: ts,
    updatedAt: ts,
  };
  repo.issues.set(issue.number, issue);
  return issue;
}

export function createPull(
  store: Store,
  repo: Repo,
  input: { title: string; body: string; head: string; base: string },
): Pull {
  const ts = store.nowIso();
  const pull: Pull = {
    id: store.nextPullId++,
    number: repo.nextNumber++,
    title: input.title,
    body: input.body,
    head: input.head,
    base: input.base,
    state: 'open',
    merged: false,
    mergedAt: null,
    mergeCommitSha: null,
    createdAt: ts,
    updatedAt: ts,
  };
  repo.pulls.set(pull.number, pull);
  return pull;
}

function stripOwnerPrefix(ref: string): string {
  const idx = ref.indexOf(':');
  return idx === -1 ? ref : ref.slice(idx + 1);
}

// ------------------------------------------------------------------ handler
export function handleRest(ctx: Ctx): Reply {
  const { store } = ctx;
  const seg = ctx.segments.slice(2); // after api/v3

  if ((ctx.method === 'POST' || ctx.method === 'PUT' || ctx.method === 'PATCH' || ctx.method === 'DELETE') && ctx.jsonError) {
    return errorReply(400, 'Problems parsing JSON');
  }

  // Root + meta
  if (seg.length === 0) {
    if (ctx.method !== 'GET') return errorReply(404, 'Not Found');
    return jsonReply(
      200,
      {
        current_user_url: '/api/v3/user',
        repository_url: '/api/v3/repos/{owner}/{repo}',
        issue_search_url: '/api/v3/search/issues',
      },
      { 'x-oauth-scopes': 'repo, read:org' },
    );
  }
  if (seg.length === 1 && seg[0] === 'meta') {
    if (ctx.method !== 'GET') return errorReply(404, 'Not Found');
    return jsonReply(200, { installed_version: '1.0.0-forge', verifiable_password_authentication: false });
  }

  // Current user
  if (seg.length === 1 && seg[0] === 'user') {
    if (ctx.method !== 'GET') return errorReply(404, 'Not Found');
    const login = store.viewerLogin();
    return jsonReply(200, { login, id: 1000, name: login, email: `${login}@example.invalid`, type: 'User' });
  }

  // POST /user/repos, GET /user/repos
  if (seg.length === 2 && seg[0] === 'user' && seg[1] === 'repos') {
    if (ctx.method === 'POST') {
      const body = bodyObj(ctx);
      const name = str(body.name);
      if (!name) return errorReply(422, 'Validation Failed: name is required');
      if (store.reposByName.has(name)) return errorReply(422, 'Validation Failed: name already exists on this account');
      const repo = store.createRepo(name, body.private === true, str(body.description) ?? null);
      return jsonReply(201, repoJson(store, repo));
    }
    if (ctx.method === 'GET') {
      const { page, link } = paginate(ctx, '/api/v3/user/repos', store.repos);
      return jsonReply(200, page.map((r) => repoJson(store, r)), link ? { link } : {});
    }
    return errorReply(404, 'Not Found');
  }

  // GET /users/{owner}/repos
  if (seg.length === 3 && seg[0] === 'users' && seg[2] === 'repos' && ctx.method === 'GET') {
    if (seg[1] !== store.viewerLogin()) return errorReply(404, 'Not Found');
    const { page, link } = paginate(ctx, `/api/v3/users/${seg[1]}/repos`, store.repos);
    return jsonReply(200, page.map((r) => repoJson(store, r)), link ? { link } : {});
  }

  if (seg.length >= 3 && seg[0] === 'repos') {
    const owner = seg[1];
    const name = seg[2];
    store.learnViewer(owner);
    const repo = store.resolveRepo(owner, name);
    if (!repo) return errorReply(404, 'Not Found');
    const rest = seg.slice(3);
    return handleRepo(ctx, repo, rest);
  }

  return errorReply(404, 'Not Found');
}

function handleRepo(ctx: Ctx, repo: Repo, seg: string[]): Reply {
  const { store } = ctx;
  const basePath = `/api/v3/repos/${store.viewerLogin()}/${repo.name}`;

  // The repo itself
  if (seg.length === 0) {
    if (ctx.method === 'GET') return jsonReply(200, repoJson(store, repo));
    if (ctx.method === 'PATCH') {
      const body = bodyObj(ctx);
      if (typeof body.description === 'string' || body.description === null) {
        repo.description = body.description as string | null;
      }
      if (typeof body.private === 'boolean') repo.isPrivate = body.private;
      if (typeof body.name === 'string' && body.name !== repo.name) {
        store.reposByName.delete(repo.name);
        repo.name = body.name;
        store.reposByName.set(repo.name, repo);
      }
      repo.updatedAt = store.nowIso();
      return jsonReply(200, repoJson(store, repo));
    }
    if (ctx.method === 'DELETE') {
      store.deleteRepo(repo);
      return { status: 204 };
    }
    return errorReply(404, 'Not Found');
  }

  // Branches
  if (seg[0] === 'branches') {
    if (seg.length === 1 && ctx.method === 'GET') {
      const branches = [...repo.refs.entries()]
        .filter(([ref]) => ref.startsWith('refs/heads/'))
        .map(([ref, oid]) => ({ name: ref.slice('refs/heads/'.length), commit: { sha: oid }, protected: false }));
      return jsonReply(200, branches);
    }
    if (seg.length >= 2 && ctx.method === 'GET') {
      const branch = seg.slice(1).join('/');
      const oid = repo.refs.get(`refs/heads/${branch}`);
      if (!oid) return errorReply(404, 'Branch not found');
      return jsonReply(200, { name: branch, commit: { sha: oid }, protected: false });
    }
    return errorReply(404, 'Not Found');
  }

  // Commits
  if (seg[0] === 'commits') {
    if (seg.length === 1 && ctx.method === 'GET') {
      const ref = ctx.query.get('sha') ?? undefined;
      const tip = resolveCommitish(repo, ref ?? null);
      if (!tip) return jsonReply(200, []);
      const { page } = paginate(ctx, `${basePath}/commits`, commitHistory(repo, tip));
      const list = page.map((oid) => {
        const obj = repo.objects.get(oid);
        const commit = obj && obj.type === 'commit' ? parseCommit(obj.data) : null;
        return {
          sha: oid,
          commit: {
            message: commit?.message ?? '',
            author: { name: commit?.authorName ?? '', email: commit?.authorEmail ?? '', date: repo.createdAt },
            committer: { name: commit?.committerName ?? '', email: commit?.committerEmail ?? '', date: repo.createdAt },
            tree: { sha: commit?.tree ?? ZERO_OID },
          },
          parents: (commit?.parents ?? []).map((sha) => ({ sha })),
        };
      });
      return jsonReply(200, list);
    }
    if (seg.length === 2 && ctx.method === 'GET') {
      const oid = seg[1];
      const obj = repo.objects.get(oid);
      if (!obj || obj.type !== 'commit') return errorReply(404, 'Not Found');
      const commit = parseCommit(obj.data);
      return jsonReply(200, {
        sha: oid,
        commit: {
          message: commit.message,
          author: { name: commit.authorName, email: commit.authorEmail, date: repo.createdAt },
          committer: { name: commit.committerName, email: commit.committerEmail, date: repo.createdAt },
          tree: { sha: commit.tree },
        },
        parents: commit.parents.map((sha) => ({ sha })),
      });
    }
    return errorReply(404, 'Not Found');
  }

  // git refs
  if (seg[0] === 'git' && seg[1] === 'refs') {
    if (seg.length === 2 && ctx.method === 'POST') {
      const body = bodyObj(ctx);
      const ref = str(body.ref);
      const sha = str(body.sha);
      if (!ref || !sha) return errorReply(422, 'Validation Failed: ref and sha are required');
      const obj = repo.objects.get(sha);
      if (!obj) return errorReply(422, 'Validation Failed: sha does not exist');
      if (repo.refs.has(ref)) return errorReply(422, 'Reference already exists');
      repo.refs.set(ref, sha);
      repo.version += 1;
      return jsonReply(201, { ref, object: { sha, type: obj.type } });
    }
    if (seg.length >= 3 && ctx.method === 'GET') {
      const ref = `refs/${seg.slice(2).join('/')}`;
      const oid = repo.refs.get(ref);
      if (!oid) return errorReply(404, 'Reference not found');
      const obj = repo.objects.get(oid);
      return jsonReply(200, { ref, object: { sha: oid, type: obj?.type ?? 'commit' } });
    }
    if (seg.length === 2 && ctx.method === 'GET') {
      const list = [...repo.refs.entries()].map(([ref, oid]) => ({
        ref,
        object: { sha: oid, type: repo.objects.get(oid)?.type ?? 'commit' },
      }));
      return jsonReply(200, list);
    }
    return errorReply(404, 'Not Found');
  }

  // Contents
  if (seg[0] === 'contents') {
    const path = seg.slice(1).join('/');
    return handleContents(ctx, repo, path);
  }

  // Labels
  if (seg[0] === 'labels') {
    if (seg.length === 1 && ctx.method === 'POST') {
      const body = bodyObj(ctx);
      const name = str(body.name);
      if (!name) return errorReply(422, 'Validation Failed: name is required');
      const color = (str(body.color) ?? 'ededed').replace(/^#/, '');
      if (!repo.labels.has(name)) repo.labels.set(name, { name, color });
      const label = repo.labels.get(name) as { name: string; color: string };
      return jsonReply(201, { id: 1, name: label.name, color: label.color });
    }
    if (seg.length === 1 && ctx.method === 'GET') {
      return jsonReply(200, [...repo.labels.values()].map((l) => ({ id: 1, name: l.name, color: l.color })));
    }
    return errorReply(404, 'Not Found');
  }

  // Issues + comments
  if (seg[0] === 'issues') {
    return handleIssues(ctx, repo, seg.slice(1), basePath);
  }

  // Pulls
  if (seg[0] === 'pulls') {
    return handlePulls(ctx, repo, seg.slice(1), basePath);
  }

  return errorReply(404, 'Not Found');
}

function handleContents(ctx: Ctx, repo: Repo, path: string): Reply {
  const { store } = ctx;
  const body = bodyObj(ctx);

  if (ctx.method === 'PUT') {
    const contentB64 = str(body.content);
    const message = str(body.message) ?? 'Update file';
    const branch = str(body.branch) ?? repo.defaultBranch;
    if (contentB64 === undefined) return errorReply(422, 'Validation Failed: content is required');
    const bytes = Buffer.from(contentB64.replace(/\s+/g, ''), 'base64');
    const blobOid = storeObject(repo, 'blob', bytes);
    const ref = `refs/heads/${branch}`;
    const tip = repo.refs.get(ref);
    const tipCommit = tip ? peelToCommit(repo, tip) : null;
    const existing = tipCommit ? flattenCommitTree(repo, tipCommit).get(path) : undefined;
    const givenSha = str(body.sha);
    if (existing && givenSha !== existing.oid) {
      return errorReply(409, 'Conflict: sha does not match the current file sha');
    }
    const commit = commitFileChange(store, repo, branch, path, blobOid, message);
    const created = !existing;
    return jsonReply(created ? 201 : 200, {
      content: {
        type: 'file',
        name: path.split('/').pop() ?? path,
        path,
        sha: blobOid,
        size: bytes.length,
        encoding: 'base64',
        content: bytes.toString('base64'),
      },
      commit: { sha: commit, message },
    });
  }

  if (ctx.method === 'GET') {
    const ref = ctx.query.get('ref') ?? undefined;
    const tip = resolveCommitish(repo, ref ?? null);
    if (!tip) return errorReply(404, 'Not Found');
    const treeOid = (() => {
      const obj = repo.objects.get(tip);
      return obj && obj.type === 'commit' ? commitHeaderOf(obj).tree : null;
    })();
    if (!treeOid) return errorReply(404, 'Not Found');
    // Walk to the target path.
    if (path === '') {
      const entries = parseTree(repo.objects.get(treeOid)?.data ?? Buffer.alloc(0)).map((e) => ({
        type: e.mode === '40000' ? 'dir' : 'file',
        name: e.name,
        path: e.name,
        sha: e.oid,
      }));
      return jsonReply(200, entries);
    }
    const parts = path.split('/');
    let currentTree: string | null = treeOid;
    for (let i = 0; i < parts.length; i++) {
      if (!currentTree) return errorReply(404, 'Not Found');
      const treeObj = repo.objects.get(currentTree);
      if (!treeObj || treeObj.type !== 'tree') return errorReply(404, 'Not Found');
      const entry = parseTree(treeObj.data).find((e) => e.name === parts[i]);
      if (!entry) return errorReply(404, 'Not Found');
      if (i === parts.length - 1) {
        if (entry.mode === '40000') {
          const dirObj = repo.objects.get(entry.oid);
          const entries = parseTree(dirObj?.data ?? Buffer.alloc(0)).map((e) => ({
            type: e.mode === '40000' ? 'dir' : 'file',
            name: e.name,
            path: `${path}/${e.name}`,
            sha: e.oid,
          }));
          return jsonReply(200, entries);
        }
        const blob = repo.objects.get(entry.oid);
        if (!blob) return errorReply(404, 'Not Found');
        return jsonReply(200, {
          type: 'file',
          name: entry.name,
          path,
          sha: entry.oid,
          size: blob.data.length,
          encoding: 'base64',
          content: blob.data.toString('base64'),
        });
      }
      if (entry.mode !== '40000') return errorReply(404, 'Not Found');
      currentTree = entry.oid;
    }
    return errorReply(404, 'Not Found');
  }

  if (ctx.method === 'DELETE') {
    const message = str(body.message) ?? 'Delete file';
    const branch = str(body.branch) ?? repo.defaultBranch;
    const ref = `refs/heads/${branch}`;
    const tip = repo.refs.get(ref);
    const tipCommit = tip ? peelToCommit(repo, tip) : null;
    const existing = tipCommit ? flattenCommitTree(repo, tipCommit).get(path) : undefined;
    if (!existing) return errorReply(404, 'Not Found');
    const givenSha = str(body.sha);
    if (givenSha !== undefined && givenSha !== existing.oid) {
      return errorReply(409, 'Conflict: sha does not match the current file sha');
    }
    const commit = commitFileChange(store, repo, branch, path, null, message);
    return jsonReply(200, { content: null, commit: { sha: commit, message } });
  }

  return errorReply(404, 'Not Found');
}

function handleIssues(ctx: Ctx, repo: Repo, seg: string[], basePath: string): Reply {
  const { store } = ctx;

  if (seg.length === 0) {
    if (ctx.method === 'POST') {
      const body = bodyObj(ctx);
      const title = str(body.title);
      if (!title) return errorReply(422, 'Validation Failed: title is required');
      const rawLabels = Array.isArray(body.labels) ? body.labels : [];
      const labelNames: string[] = [];
      for (const raw of rawLabels) {
        const name = typeof raw === 'string' ? raw : str((raw as Record<string, unknown>)?.name);
        if (!name) continue;
        if (!repo.labels.has(name)) return errorReply(422, `Validation Failed: label "${name}" does not exist`);
        labelNames.push(name);
      }
      const issue = createIssue(store, repo, {
        title,
        body: str(body.body) ?? '',
        labels: labelNames,
      });
      return jsonReply(201, issueJson(store, repo, issue));
    }
    if (ctx.method === 'GET') {
      const state = ctx.query.get('state') ?? 'open';
      let list = [...repo.issues.values()];
      if (state === 'open') list = list.filter((i) => i.state === 'open');
      else if (state === 'closed') list = list.filter((i) => i.state === 'closed');
      const direction = ctx.query.get('direction') ?? 'desc';
      list.sort((a, b) => a.number - b.number);
      if (direction === 'desc') list.reverse();
      const { page, link } = paginate(ctx, `${basePath}/issues`, list);
      return jsonReply(200, page.map((i) => issueJson(store, repo, i)), link ? { link } : {});
    }
    return errorReply(404, 'Not Found');
  }

  const number = Number(seg[0]);
  const issue = Number.isInteger(number) ? repo.issues.get(number) : undefined;

  if (seg.length === 1) {
    if (!issue) return errorReply(404, 'Not Found');
    if (ctx.method === 'GET') return jsonReply(200, issueJson(store, repo, issue));
    if (ctx.method === 'PATCH') {
      const body = bodyObj(ctx);
      if (body.state === 'closed' || body.state === 'open') issue.state = body.state;
      if (typeof body.title === 'string') issue.title = body.title;
      if (typeof body.body === 'string') issue.body = body.body;
      issue.updatedAt = store.nowIso();
      return jsonReply(200, issueJson(store, repo, issue));
    }
    return errorReply(404, 'Not Found');
  }

  if (seg.length === 2 && seg[1] === 'comments') {
    // Comments work on issues (and PR numbers, GitHub-style).
    if (!issue && !repo.pulls.has(number)) return errorReply(404, 'Not Found');
    if (ctx.method === 'POST') {
      const body = bodyObj(ctx);
      const text = str(body.body);
      if (text === undefined) return errorReply(422, 'Validation Failed: body is required');
      const comment = { id: store.nextCommentId++, body: text };
      const list = repo.comments.get(number) ?? [];
      list.push(comment);
      repo.comments.set(number, list);
      return jsonReply(201, {
        id: comment.id,
        body: comment.body,
        user: { login: store.viewerLogin(), id: 1000 },
        created_at: store.nowIso(),
        updated_at: store.nowIso(),
      });
    }
    if (ctx.method === 'GET') {
      const list = repo.comments.get(number) ?? [];
      return jsonReply(
        200,
        list.map((c) => ({
          id: c.id,
          body: c.body,
          user: { login: store.viewerLogin(), id: 1000 },
          created_at: repo.createdAt,
          updated_at: repo.updatedAt,
        })),
      );
    }
    return errorReply(404, 'Not Found');
  }

  return errorReply(404, 'Not Found');
}

function handlePulls(ctx: Ctx, repo: Repo, seg: string[], basePath: string): Reply {
  const { store } = ctx;

  if (seg.length === 0) {
    if (ctx.method === 'POST') {
      const body = bodyObj(ctx);
      const headRaw = str(body.head);
      const baseRaw = str(body.base);
      const title = str(body.title);
      if (!headRaw || !baseRaw || !title) return errorReply(422, 'Validation Failed: head, base and title are required');
      const head = stripOwnerPrefix(headRaw);
      const base = stripOwnerPrefix(baseRaw);
      if (!repo.refs.has(`refs/heads/${head}`)) return errorReply(422, `Validation Failed: head branch "${head}" does not exist`);
      if (!repo.refs.has(`refs/heads/${base}`)) return errorReply(422, `Validation Failed: base branch "${base}" does not exist`);
      const duplicate = [...repo.pulls.values()].find((p) => p.state === 'open' && p.head === head && p.base === base);
      if (duplicate) return errorReply(422, 'Validation Failed: A pull request already exists for this head and base');
      const pull = createPull(store, repo, { title, body: str(body.body) ?? '', head, base });
      return jsonReply(201, pullJson(store, repo, pull));
    }
    if (ctx.method === 'GET') {
      const state = ctx.query.get('state') ?? 'open';
      let list = [...repo.pulls.values()].sort((a, b) => a.number - b.number);
      if (state === 'open') list = list.filter((p) => p.state === 'open');
      else if (state === 'closed') list = list.filter((p) => p.state === 'closed');
      const { page, link } = paginate(ctx, `${basePath}/pulls`, list);
      return jsonReply(200, page.map((p) => pullJson(store, repo, p)), link ? { link } : {});
    }
    return errorReply(404, 'Not Found');
  }

  const number = Number(seg[0]);
  const pull = Number.isInteger(number) ? repo.pulls.get(number) : undefined;

  if (seg.length === 1 && ctx.method === 'GET') {
    if (!pull) return errorReply(404, 'Not Found');
    return jsonReply(200, pullJson(store, repo, pull));
  }

  if (seg.length === 2 && seg[1] === 'files' && ctx.method === 'GET') {
    if (!pull) return errorReply(404, 'Not Found');
    const changes = pullChanges(repo, pull);
    return jsonReply(
      200,
      changes.map((c) => ({
        filename: c.filename,
        status: c.status,
        additions: c.additions,
        deletions: c.deletions,
        changes: c.changes,
        sha: '',
      })),
    );
  }

  if (seg.length === 2 && seg[1] === 'merge' && ctx.method === 'PUT') {
    if (!pull) return errorReply(404, 'Not Found');
    if (pull.merged) return errorReply(405, 'Pull Request is not mergeable');
    const body = bodyObj(ctx);
    const method = str(body.merge_method) === 'merge' ? 'merge' : 'squash';
    const result = mergePull(store, repo, pull, method);
    if (result === 'conflict') return errorReply(405, 'Pull Request is not mergeable: merge conflict');
    if (result === null) return errorReply(405, 'Pull Request is not mergeable');
    return jsonReply(200, { sha: result, merged: true, message: 'Pull Request successfully merged' });
  }

  return errorReply(404, 'Not Found');
}

export { hashObject };
