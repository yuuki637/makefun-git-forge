// Shared request/response plumbing between app.ts and the surface handlers.
import type { Store } from './store.ts';

export interface Ctx {
  store: Store;
  method: string;
  path: string;
  segments: string[];
  query: URLSearchParams;
  headers: Record<string, string | string[] | undefined>;
  rawBody: Buffer;
  json: unknown;
  jsonError: boolean;
  host: string;
  requestId: string;
  authed: boolean;
}

export interface Reply {
  status: number;
  headers?: Record<string, string>;
  body?: string | Buffer;
  json?: unknown;
}

export function jsonReply(status: number, value: unknown, headers: Record<string, string> = {}): Reply {
  return { status, json: value, headers };
}

export function errorReply(status: number, message: string): Reply {
  return { status, json: { message, documentation_url: 'https://docs.github.com/rest' } };
}

export function headerOf(ctx: Ctx, name: string): string {
  const value = ctx.headers[name.toLowerCase()];
  if (Array.isArray(value)) return value[0] ?? '';
  return value ?? '';
}

// Reads a property from a parsed JSON body object ({} when not an object).
export function bodyObj(ctx: Ctx): Record<string, unknown> {
  if (ctx.json && typeof ctx.json === 'object' && !Array.isArray(ctx.json)) {
    return ctx.json as Record<string, unknown>;
  }
  return {};
}

export function str(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

export function parsePageParams(ctx: Ctx): { perPage: number; page: number } {
  const perRaw = Number(ctx.query.get('per_page'));
  const pageRaw = Number(ctx.query.get('page'));
  const perPage = Number.isFinite(perRaw) && perRaw > 0 ? Math.min(Math.floor(perRaw), 100) : 30;
  const page = Number.isFinite(pageRaw) && pageRaw > 0 ? Math.floor(pageRaw) : 1;
  return { perPage, page };
}

// Slices a list for the page and builds a Link header value when more remain.
export function paginate<T>(ctx: Ctx, basePath: string, items: T[]): { page: T[]; link?: string } {
  const { perPage, page } = parsePageParams(ctx);
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  let link: string | undefined;
  const origin = `http://${ctx.host}`;
  const parts: string[] = [];
  const hasNext = start + perPage < items.length;
  const lastPage = Math.max(1, Math.ceil(items.length / perPage));
  if (hasNext) parts.push(`<${origin}${basePath}?per_page=${perPage}&page=${page + 1}>; rel="next"`);
  if (hasNext) parts.push(`<${origin}${basePath}?per_page=${perPage}&page=${lastPage}>; rel="last"`);
  if (page > 1) {
    parts.push(`<${origin}${basePath}?per_page=${perPage}&page=1>; rel="first"`);
    parts.push(`<${origin}${basePath}?per_page=${perPage}&page=${page - 1}>; rel="prev"`);
  }
  if (parts.length > 0) link = parts.join(', ');
  return { page: slice, link };
}
