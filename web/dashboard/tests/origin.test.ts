import { afterEach, describe, expect, it, vi } from 'vitest';
import { buildUrl, request } from '../src/lib/api';
import { apiOrigin, docsUrl } from '../src/lib/origin';

function setMeta(content: string | null, name = 'prismaport-api') {
  document.head.querySelector(`meta[name="${name}"]`)?.remove();
  if (content === null) return;
  const m = document.createElement('meta');
  m.name = name;
  m.content = content;
  document.head.append(m);
}

afterEach(() => {
  setMeta(null);
  setMeta(null, 'prismaport-docs');
  vi.restoreAllMocks();
});

describe('API origin', () => {
  it('is this page (relative URLs) without the meta tag or with it empty, as the binary serves it', () => {
    expect(apiOrigin()).toBe('');
    setMeta('');
    expect(apiOrigin()).toBe('');
    expect(buildUrl('/me')).toBe('/v1/dashboard/me');
  });

  it('is the origin the Pages build writes, without any path', () => {
    setMeta('https://api.prismaport.com');
    expect(apiOrigin()).toBe('https://api.prismaport.com');
    expect(buildUrl('/links', { mode: 'test' })).toBe('https://api.prismaport.com/v1/dashboard/links?mode=test');
    setMeta(' http://127.0.0.1:58580/ignored/path ');
    expect(apiOrigin()).toBe('http://127.0.0.1:58580');
  });

  it('ignores anything but an absolute http(s) URL', () => {
    for (const bad of ['api.prismaport.com', '/v1', 'javascript:alert(1)', 'data:text/plain,x', 'ftp://api.prismaport.com']) {
      setMeta(bad);
      expect(apiOrigin(), bad).toBe('');
    }
  });

  it('sends the session cookie to the API origin on every call', async () => {
    setMeta('https://api.prismaport.com');
    const fetchFn = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response('{"ok":true}', { status: 200 }));
    await request('/auth/login', { method: 'POST', body: { email: 'a@b.c', password: 'x' } });
    expect(fetchFn).toHaveBeenCalledOnce();
    const [url, init] = fetchFn.mock.calls[0]!;
    expect(url).toBe('https://api.prismaport.com/v1/dashboard/auth/login');
    expect((init as RequestInit).credentials).toBe('include');
  });
});

describe('docs address', () => {
  it('is /docs/ on this origin when the meta tag is missing or empty, as the binary serves it', () => {
    expect(docsUrl()).toBe('/docs/');
    setMeta('', 'prismaport-docs');
    expect(docsUrl()).toBe('/docs/');
  });

  it("is the site's portal the Pages build writes", () => {
    setMeta('https://docs.prismaport.com/', 'prismaport-docs');
    expect(docsUrl()).toBe('https://docs.prismaport.com/');
  });

  it('ignores anything but an absolute http(s) URL', () => {
    for (const bad of ['prismaport.com/docs/', 'javascript:alert(1)', 'data:text/html,x']) {
      setMeta(bad, 'prismaport-docs');
      expect(docsUrl(), bad).toBe('/docs/');
    }
  });
});
