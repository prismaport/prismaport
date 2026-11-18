import { afterEach, describe, expect, it, vi } from 'vitest';
import { CheckoutApi } from '../src/lib/api';
import { apiOrigin } from '../src/lib/origin';

function setMeta(content: string | null) {
  document.head.querySelector('meta[name="prismaport-api"]')?.remove();
  if (content === null) return;
  const m = document.createElement('meta');
  m.name = 'prismaport-api';
  m.content = content;
  document.head.append(m);
}

afterEach(() => setMeta(null));

describe('API origin', () => {
  it('is this page without the meta tag or with it empty, as the binary serves it', () => {
    expect(apiOrigin()).toBe('');
    setMeta('');
    expect(apiOrigin()).toBe('');
  });

  it('is the origin the Pages build writes', () => {
    setMeta('https://api.prismaport.com');
    expect(apiOrigin()).toBe('https://api.prismaport.com');
    setMeta('http://127.0.0.1:58580/x');
    expect(apiOrigin()).toBe('http://127.0.0.1:58580');
  });

  it('ignores anything but an absolute http(s) URL', () => {
    for (const bad of ['api.prismaport.com', '/v1', 'javascript:alert(1)', 'ftp://api.prismaport.com']) {
      setMeta(bad);
      expect(apiOrigin(), bad).toBe('');
    }
  });

  it('calls the API origin without credentials', async () => {
    setMeta('https://api.prismaport.com');
    const fetchFn = vi.fn().mockResolvedValue(new Response('{"data":[]}', { status: 200 }));
    await new CheckoutApi(apiOrigin(), fetchFn).networks();
    const [url, init] = fetchFn.mock.calls[0]!;
    expect(url).toBe('https://api.prismaport.com/v1/checkout/networks');
    expect((init as RequestInit).credentials).toBe('omit');
  });
});
