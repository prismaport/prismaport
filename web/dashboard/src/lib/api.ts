import { apiOrigin } from './origin';
import type { ApiErrorBody } from './types';

const API_BASE = '/v1/dashboard';

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly retryAfter?: number,
    /** The request field the error is about, when the server names one. */
    readonly param?: string,
    /** The error's extra fields (`error.details`), such as the `methods` of `confirmation_required`. */
    readonly details?: Record<string, unknown>,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

export type Query = Record<string, string | number | boolean | null | undefined>;

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE';
  body?: unknown;
  query?: Query;
  idempotencyKey?: string;
  signal?: AbortSignal;
}

/** Routes that need no session: a 401 there is about the request, not a session that ended. */
const PUBLIC_PATHS = new Set(['/auth/login', '/auth/signup', '/auth/verify-email', '/auth/resend-code', '/auth/password/forgot', '/auth/password/reset/check', '/auth/password/reset', '/auth/totp/lost', '/auth/totp/lost/check', '/auth/totp/lost/confirm', '/auth/totp/removal/cancel_link']);

let csrfToken: string | null = null;
let onUnauthenticated: (() => void) | null = null;

/** The session's CSRF token, from GET /me. Every non-GET request sends it. */
export function setCsrfToken(token: string | null): void {
  csrfToken = token;
}

/** Called once per 401, so the app can drop its cached user and show the login page. */
export function setUnauthenticatedHandler(fn: (() => void) | null): void {
  onUnauthenticated = fn;
}

export function buildUrl(path: string, query?: Query): string {
  const params = new URLSearchParams();
  for (const [k, v] of Object.entries(query ?? {})) {
    if (v === undefined || v === null || v === '') continue;
    params.set(k, String(v));
  }
  const qs = params.toString();
  return `${apiOrigin()}${API_BASE}${path}${qs ? `?${qs}` : ''}`;
}

async function readError(res: Response): Promise<ApiError> {
  let code = res.status === 401 ? 'unauthenticated' : res.status === 429 ? 'rate_limited' : 'server_error';
  let message = res.statusText || 'Request failed';
  let param: string | undefined;
  let details: Record<string, unknown> | undefined;
  try {
    const body = (await res.json()) as Partial<ApiErrorBody>;
    if (body.error?.code) code = body.error.code;
    if (body.error?.message) message = body.error.message;
    if (body.error?.param) param = body.error.param;
    if (body.error?.details && typeof body.error.details === 'object') details = body.error.details;
  } catch {
    // Not JSON (a proxy error page): keep the status-derived code.
  }
  const ra = Number(res.headers.get('Retry-After'));
  return new ApiError(res.status, code, message, Number.isFinite(ra) && ra > 0 ? ra : undefined, param, details);
}

async function send(path: string, opts: RequestOptions): Promise<Response> {
  const method = opts.method ?? 'GET';
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET' && csrfToken) headers['X-CSRF-Token'] = csrfToken;
  if (opts.idempotencyKey) headers['Idempotency-Key'] = opts.idempotencyKey;
  try {
    return await fetch(buildUrl(path, opts.query), {
      method,
      headers,
      credentials: 'include',
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
      signal: opts.signal,
    });
  } catch (e) {
    if (e instanceof DOMException && e.name === 'AbortError') throw e;
    throw new ApiError(0, 'network_error', 'Cannot reach Prismaport.');
  }
}

async function refreshCsrf(): Promise<boolean> {
  const res = await send('/me', {});
  if (!res.ok) return false;
  const me = (await res.json()) as { csrf_token?: string };
  if (!me.csrf_token) return false;
  csrfToken = me.csrf_token;
  return true;
}

/**
 * One JSON call to the dashboard API. Cookies ride along (`credentials: 'include'`); a stale
 * CSRF token is refreshed from /me and the call retried once.
 */
export async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  let res = await send(path, opts);
  if (!res.ok) {
    let err = await readError(res);
    if (err.code === 'csrf_failed' && (await refreshCsrf())) {
      res = await send(path, opts);
      if (res.ok) return parse<T>(res);
      err = await readError(res);
    }
    if (err.status === 401 && !PUBLIC_PATHS.has(path)) onUnauthenticated?.();
    throw err;
  }
  return parse<T>(res);
}

async function parse<T>(res: Response): Promise<T> {
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

export function newIdempotencyKey(): string {
  return crypto.randomUUID();
}
