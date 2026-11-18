import { afterEach, describe, expect, it, vi } from 'vitest';
import { CheckoutApi, type Merchant, type PublicLink } from '../src/lib/api';
import { mountLink } from '../src/pages/link';
import { mountPay } from '../src/pages/pay';
import { NETWORKS, NOW, session } from './fixtures';

const LOGO: Merchant = { name: 'Acme Coffee', logo_url: '/v1/checkout/logos/0123456789abcdef0123456789abcdef.png' };
const stops: Array<() => void> = [];

afterEach(() => {
  while (stops.length) stops.pop()!();
  vi.restoreAllMocks();
});

const flush = () => new Promise((r) => setTimeout(r, 0));

async function pay(merchant: Merchant | null, base = '') {
  const api = new CheckoutApi(base, vi.fn() as never);
  vi.spyOn(api, 'session').mockResolvedValue({ data: session({ merchant }), serverTime: null });
  vi.spyOn(api, 'networks').mockResolvedValue({ data: { data: NETWORKS }, serverTime: null });
  const root = document.createElement('main');
  document.body.replaceChildren(root);
  const handle = await mountPay(root, 'ses_01JBTEST', { api, now: () => NOW, pollMs: 1_000_000, discoverEvm: async () => [] });
  stops.push(handle.stop);
  await flush();
  return { root, api };
}

async function link(merchant: Merchant | null, base = '') {
  const product: PublicLink = {
    id: 'plink_01JBFOUNDER', livemode: true, name: 'Founder', price_type: 'fixed', amount: '25.00', min_amount: null, max_amount: null,
    suggested_amounts: [], currency: 'USD', active: true, merchant,
  };
  const api = new CheckoutApi(base, vi.fn() as never);
  vi.spyOn(api, 'link').mockResolvedValue({ data: product, serverTime: null });
  const root = document.createElement('main');
  document.body.replaceChildren(root);
  await mountLink(root, product.id, { api, navigate: vi.fn(), newKey: () => 'k' });
  await flush();
  return root;
}

describe('merchant logo on the pay page', () => {
  it('same origin: src is the API path, with alt, size attributes and no inline style', async () => {
    const { root } = await pay(LOGO);
    const img = root.querySelector<HTMLImageElement>('.merchant-logo img')!;
    expect(img.getAttribute('src')).toBe(LOGO.logo_url);
    expect(img.alt).toBe('Acme Coffee');
    expect(img.getAttribute('width')).toBe('160');
    expect(img.getAttribute('height')).toBe('40');
    expect(root.querySelector('[style]')).toBeNull();
  });

  it('split mode: src is on the API origin', async () => {
    const { root } = await pay(LOGO, 'https://api.prismaport.com');
    expect(root.querySelector('.merchant-logo img')!.getAttribute('src')).toBe(`https://api.prismaport.com${LOGO.logo_url}`);
  });

  it('renders nothing for a null merchant', async () => {
    const { root } = await pay(null);
    expect(root.querySelector('.merchant-logo')).toBeNull();
    expect(root.querySelector('img[alt="Acme Coffee"]')).toBeNull();
  });

  it('ignores a logo_url outside /v1/checkout/logos/', async () => {
    for (const logo_url of ['https://evil.example/x.png', '/v1/other/x.png', '//evil.example/v1/checkout/logos/a.png', 'javascript:alert(1)', '/v1/checkout/logos/../../v1/dashboard/me', '/v1/checkout/logos/ABC.png']) {
      const { root } = await pay({ name: 'Evil', logo_url });
      expect(root.querySelector('.merchant-logo')).toBeNull();
    }
  });

  it('removes the logo when it fails to load', async () => {
    const { root } = await pay(LOGO);
    root.querySelector('.merchant-logo img')!.dispatchEvent(new Event('error'));
    expect(root.querySelector('.merchant-logo')).toBeNull();
  });
});

describe('merchant logo on the link page', () => {
  it('shows the logo above the product name', async () => {
    const root = await link(LOGO, 'https://api.prismaport.com');
    const img = root.querySelector<HTMLImageElement>('.merchant-logo img')!;
    expect(img.getAttribute('src')).toBe(`https://api.prismaport.com${LOGO.logo_url}`);
    expect(img.alt).toBe('Acme Coffee');
    expect(root.querySelector('.merchant-logo')!.compareDocumentPosition(root.querySelector('h1')!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(root.querySelector('[style]')).toBeNull();
  });

  it('shows nothing for a null merchant or a foreign url', async () => {
    expect((await link(null)).querySelector('.merchant-logo')).toBeNull();
    expect((await link({ name: 'X', logo_url: 'https://evil.example/a.png' })).querySelector('.merchant-logo')).toBeNull();
  });
});

describe('merchant logo across polls', () => {
  it('keeps the same img element when the page re-renders', async () => {
    const api = new CheckoutApi('', vi.fn() as never);
    vi.spyOn(api, 'session').mockResolvedValue({ data: session({ merchant: LOGO }), serverTime: null });
    vi.spyOn(api, 'networks').mockResolvedValue({ data: { data: NETWORKS }, serverTime: null });
    const root = document.createElement('main');
    document.body.replaceChildren(root);
    vi.useFakeTimers();
    try {
      const handle = await mountPay(root, 'ses_01JBTEST', { api, now: () => NOW, pollMs: 1000, discoverEvm: async () => [] });
      stops.push(handle.stop);
      await vi.advanceTimersByTimeAsync(10);
      const first = root.querySelector('.merchant-logo img');
      await vi.advanceTimersByTimeAsync(3500);
      expect(api.session).toHaveBeenCalledTimes(4);
      expect(root.querySelector('.merchant-logo img')).toBe(first);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('Prismaport header', () => {
  const header = (root: Element) => {
    const el = root.querySelector('header.brand-header')!;
    expect(el).not.toBeNull();
    expect(el.textContent).toBe('Prismaport');
    expect(el.querySelector('img')!.getAttribute('alt')).toBe('');
    expect(el.querySelector('[style]')).toBeNull();
    return el;
  };

  it('opens the pay page, above the card, with or without a merchant logo', async () => {
    for (const merchant of [null, LOGO]) {
      const { root } = await pay(merchant);
      expect(header(root).nextElementSibling!.classList.contains('card')).toBe(true);
    }
  });

  it('opens the link page, above the card', async () => {
    const root = await link(LOGO);
    expect(header(root).nextElementSibling!.classList.contains('card')).toBe(true);
  });
});
