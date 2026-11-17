// Entry point: builds the HTTP server for the forge. All state lives in one
// in-memory Store shared by the REST, GraphQL, MCP and git surfaces.
import { createServer, IncomingMessage, Server, ServerResponse } from 'node:http';
import { Store } from './store.ts';
import { type Ctx, type Reply, errorReply, jsonReply } from './web.ts';
import { handleRest } from './rest.ts';
import { handleGit } from './git-http.ts';
import { handleGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';

const MAX_BODY_BYTES = 96 * 1024 * 1024;

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > MAX_BODY_BYTES) {
        reject(new Error('request body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function decodeSegment(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

export function createApp(): Server {
  const store = new Store();
  const expectedToken = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  let requestCounter = 0;

  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      requestCounter += 1;
      const requestId = `req-${requestCounter.toString(36)}`;
      try {
        const rawBody = await readBody(req);
        const url = new URL(req.url ?? '/', 'http://forge.internal');
        const segments = url.pathname.split('/').filter((s) => s !== '').map(decodeSegment);
        const path = `/${segments.join('/')}`;

        let json: unknown;
        let jsonError = false;
        if (rawBody.length > 0) {
          try {
            json = JSON.parse(rawBody.toString('utf8'));
          } catch {
            jsonError = true;
          }
        }

        const authHeader = req.headers.authorization ?? '';
        const authed = expectedToken === '' || authHeader === `Bearer ${expectedToken}`;

        const ctx: Ctx = {
          store,
          method: (req.method ?? 'GET').toUpperCase(),
          path,
          segments,
          query: url.searchParams,
          headers: req.headers,
          rawBody,
          json,
          jsonError,
          host: req.headers.host ?? '127.0.0.1',
          requestId,
          authed,
        };

        const isGitPath = segments.length >= 2 && segments[1].endsWith('.git');
        const needsAuth = path !== '/healthz' && (segments[0] === 'api' || path === '/mcp' || isGitPath);

        let reply: Reply;
        if (path === '/healthz') {
          reply = jsonReply(200, { ok: true });
        } else if (needsAuth && !authed) {
          reply = errorReply(401, 'Requires authentication');
        } else if (segments[0] === 'api' && segments[1] === 'v3') {
          reply = handleRest(ctx);
        } else if (path === '/api/graphql') {
          reply = handleGraphql(ctx);
        } else if (path === '/mcp') {
          reply = handleMcp(ctx);
        } else if (isGitPath) {
          reply = handleGit(ctx);
        } else {
          reply = errorReply(404, 'Not Found');
        }

        const headers: Record<string, string> = { ...(reply.headers ?? {}) };
        let body: Buffer;
        if (reply.json !== undefined) {
          if (!headers['content-type']) headers['content-type'] = 'application/json; charset=utf-8';
          body = Buffer.from(JSON.stringify(reply.json), 'utf8');
        } else if (reply.body !== undefined) {
          body = typeof reply.body === 'string' ? Buffer.from(reply.body, 'utf8') : reply.body;
          if (!headers['content-type'] && body.length > 0) headers['content-type'] = 'application/json; charset=utf-8';
        } else {
          body = Buffer.alloc(0);
        }
        headers['x-github-request-id'] = requestId;
        if (reply.status !== 204) headers['content-length'] = String(body.length);
        res.writeHead(reply.status, headers);
        res.end(reply.status === 204 ? undefined : body);
      } catch (error) {
        console.error('[forge] request failed', error);
        if (!res.headersSent) {
          const payload = Buffer.from(JSON.stringify({ message: 'Internal error' }), 'utf8');
          res.writeHead(500, {
            'content-type': 'application/json; charset=utf-8',
            'x-github-request-id': requestId,
            'content-length': String(payload.length),
          });
          res.end(payload);
        } else {
          res.end();
        }
      }
    })();
  });
  return server;
}
