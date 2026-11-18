import { http, HttpResponse } from 'msw';
import { setupServer } from 'msw/node';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, buildUrl, request, setCsrfToken, setUnauthenticatedHandler } from '../src/lib/api';
import { api } from '../src/lib/endpoints';
import { errorMessage } from '../src/lib/errors';
import { fetchMe } from '../src/lib/queries';

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
beforeEach(() => {
  setCsrfToken(null);
  setUnauthenticatedHandler(null);
});

const B = '/v1/dashboard';

describe('fetch wrapper', () => {
  it('builds URLs under /v1/dashboard and drops empty params', () => {
    expect(buildUrl('/links', { mode: 'test', status: 'live', q: '', kind: undefined, cursor: null })).toBe('/v1/dashboard/links?mode=test&status=live');
  });

  it('sends credentials, JSON and the CSRF token on non-GET only', async () => {
    const spy = vi.spyOn(globalThis, 'fetch');
    const seen: Record<string, string | null>[] = [];
    server.use(
      http.get(`${B}/links/:id`, ({ request: r }) => {
        seen.push({ csrf: r.headers.get('X-CSRF-Token') });
        return HttpResponse.json({ id: 'prod_1' });
      }),
      http.patch(`${B}/links/:id`, async ({ request: r }) => {
        seen.push({ csrf: r.headers.get('X-CSRF-Token'), type: r.headers.get('Content-Type'), body: await r.text() });
        return HttpResponse.json({ id: 'prod_1' });
      }),
    );
    setCsrfToken('tok-1');
    await api.link('live', 'prod_1');
    await api.updateLink('live', 'prod_1', { active: false });
    expect(seen[0]).toEqual({ csrf: null });
    expect(seen[1]).toEqual({ csrf: 'tok-1', type: 'application/json', body: '{"active":false}' });
    for (const call of spy.mock.calls) expect((call[1] as RequestInit).credentials).toBe('include');
    spy.mockRestore();
  });

  it('sends the Idempotency-Key and the mode on create', async () => {
    let key: string | null = null;
    let mode: string | null = null;
    server.use(
      http.post(`${B}/links`, ({ request: r }) => {
        key = r.headers.get('Idempotency-Key');
        mode = new URL(r.url).searchParams.get('mode');
        return HttpResponse.json({ id: 'plink_2' });
      }),
    );
    await api.createLink('test', { product_id: 'prod_1', name: 'x' }, 'key-123');
    expect(key).toBe('key-123');
    expect(mode).toBe('test');
  });

  it('parses the error shape into an ApiError, with the field it names', async () => {
    server.use(http.post(`${B}/links`, () => HttpResponse.json({ error: { code: 'invalid_request', message: 'amount must be a USD amount.', param: 'amount' } }, { status: 400 })));
    const e = await api.createLink('live', { product_id: 'prod_1', amount: 'x' }, 'k').catch((x) => x);
    expect(e).toBeInstanceOf(ApiError);
    expect(e).toMatchObject({ status: 400, code: 'invalid_request', message: 'amount must be a USD amount.', param: 'amount' });
    expect(errorMessage(e)).toBe('amount must be a USD amount.');
  });

  it('keeps a status-derived code when the body is not JSON', async () => {
    server.use(http.get(`${B}/overview`, () => new HttpResponse('<html>Bad gateway</html>', { status: 502 })));
    const e = await api.overview('live').catch((x) => x);
    expect(e).toMatchObject({ status: 502, code: 'server_error' });
    expect(errorMessage(e)).toBe('Something went wrong on our side. Try again in a moment.');
  });

  it('reads Retry-After on rate limits', async () => {
    server.use(http.post(`${B}/auth/login`, () => HttpResponse.json({ error: { code: 'rate_limited', message: 'slow down' } }, { status: 429, headers: { 'Retry-After': '30' } })));
    const e = await api.login({ email: 'a', password: 'b' }).catch((x) => x);
    expect(e.retryAfter).toBe(30);
    expect(errorMessage(e)).toBe('Too many attempts. Try again in 30 seconds.');
  });

  it('reports a network failure as network_error', async () => {
    server.use(http.get(`${B}/me`, () => HttpResponse.error()));
    const e = await request('/me').catch((x) => x);
    expect(e).toMatchObject({ status: 0, code: 'network_error' });
    expect(errorMessage(e)).toMatch(/Cannot reach Prismaport/);
  });

  it('refreshes a stale CSRF token from /me and retries once', async () => {
    const tokens: (string | null)[] = [];
    server.use(
      http.get(`${B}/me`, () => HttpResponse.json({ user: { id: 'u', email: 'e', totp_enabled: false }, merchant: { id: 'm', name: 'n' }, csrf_token: 'fresh' })),
      http.post(`${B}/api_keys/:id/revoke`, ({ request: r }) => {
        const t = r.headers.get('X-CSRF-Token');
        tokens.push(t);
        if (t !== 'fresh') return HttpResponse.json({ error: { code: 'csrf_failed', message: 'bad token' } }, { status: 403 });
        return HttpResponse.json({ id: 'key_1' });
      }),
    );
    setCsrfToken('stale');
    await expect(api.revokeApiKey('live', 'key_1')).resolves.toEqual({ id: 'key_1' });
    expect(tokens).toEqual(['stale', 'fresh']);
  });

  it('calls the unauthenticated handler on a 401, but not for the login form', async () => {
    const onAuth = vi.fn();
    setUnauthenticatedHandler(onAuth);
    server.use(
      http.get(`${B}/links`, () => HttpResponse.json({ error: { code: 'unauthenticated', message: 'no' } }, { status: 401 })),
      http.post(`${B}/auth/login`, () => HttpResponse.json({ error: { code: 'invalid_credentials', message: 'Email or password is incorrect.' } }, { status: 401 })),
    );
    await api.links('live', {}).catch(() => {});
    expect(onAuth).toHaveBeenCalledTimes(1);
    await api.login({ email: 'a', password: 'b' }).catch(() => {});
    expect(onAuth).toHaveBeenCalledTimes(1);
  });

  it('treats 204 as no content', async () => {
    setCsrfToken('t');
    server.use(http.delete(`${B}/webhook_endpoints/:id`, () => new HttpResponse(null, { status: 204 })));
    await expect(api.deleteWebhookEndpoint('live', 'we_1')).resolves.toBeUndefined();
  });
});

describe('the signed-in user', () => {
  it('stores the CSRF token from /me', async () => {
    server.use(
      http.get(`${B}/me`, () => HttpResponse.json({ user: { id: 'u', email: 'e', totp_enabled: false }, merchant: { id: 'm', name: 'Acme' }, csrf_token: 'csrf-from-me' })),
      http.post(`${B}/auth/logout`, ({ request: r }) => (r.headers.get('X-CSRF-Token') === 'csrf-from-me' ? new HttpResponse(null, { status: 204 }) : HttpResponse.json({ error: { code: 'csrf_failed', message: '' } }, { status: 403 }))),
    );
    const me = await fetchMe();
    expect(me?.merchant.name).toBe('Acme');
    await expect(api.logout()).resolves.toBeUndefined();
  });

  it('is null without a session', async () => {
    server.use(http.get(`${B}/me`, () => HttpResponse.json({ error: { code: 'unauthenticated', message: '' } }, { status: 401 })));
    await expect(fetchMe()).resolves.toBeNull();
  });
});
