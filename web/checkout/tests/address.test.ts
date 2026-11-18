import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, CheckoutApi, type PublicAddress } from '../src/lib/api';
import { COPY } from '../src/lib/copy';
import { mountAddress, startLabel } from '../src/pages/address';
import { BASE_USDC, EVM_ADDRESS, NETWORKS, SOL_ADDRESS, session } from './fixtures';
import { decodeSvg } from './qrdecode';

const flush = async () => {
  for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
};

const anyAmount: PublicAddress = {
  id: 'caddr_01JBTOPUP', livemode: true, status: 'active', replaced_by: null, live: true, unavailable_reason: null,
  product: { name: 'Wallet top-up', description: 'Add funds to your balance.' }, price_type: 'open', amount: null, open_amount: true,
  currency: 'USD',
  pay_to: [{ chain: 'base', asset: 'USDC', address: EVM_ADDRESS }, { chain: 'solana', asset: 'USDC', address: SOL_ADDRESS, token_account: 'JCAXHQhSPsgsDFUJN2RXVP5BGkCbAVFKfuHXTub1YHS5' }],
  session: null, merchant: null,
};
const fixed: PublicAddress = { ...anyAmount, product: { name: 'Monthly plan', description: null }, price_type: 'fixed', amount: '25.00', open_amount: false };
const rotated: PublicAddress = { ...anyAmount, status: 'rotated', replaced_by: 'caddr_01JBNEWER', live: false, unavailable_reason: 'rotated', pay_to: [] };

let stops: (() => void)[] = [];
afterEach(() => {
  stops.forEach((s) => s());
  stops = [];
  vi.restoreAllMocks();
});

async function mount(answers: (PublicAddress | ApiError)[], open: () => Promise<unknown> = async () => ({ data: session({ id: 'ses_01JBOPENED' }), serverTime: null })) {
  const api = new CheckoutApi('', vi.fn() as never);
  let i = 0;
  const get = vi.spyOn(api, 'address').mockImplementation(async () => {
    const a = answers[Math.min(i++, answers.length - 1)]!;
    if (a instanceof ApiError) throw a;
    return { data: a, serverTime: null };
  });
  vi.spyOn(api, 'networks').mockResolvedValue({ data: { data: NETWORKS }, serverTime: null });
  const post = vi.spyOn(api, 'openAddressSession').mockImplementation(open as never);
  const navigate = vi.fn();
  const root = document.createElement('main');
  document.body.replaceChildren(root);
  const handle = await mountAddress(root, 'caddr_01JBTOPUP', { api, navigate, pollMs: 1_000_000 });
  stops.push(handle.stop);
  await flush();
  return { root, get, post, navigate };
}

const text = (root: HTMLElement) => root.textContent ?? '';
const qr = (root: HTMLElement) => decodeSvg(root.querySelector('svg.qr')!);
const startButton = (root: HTMLElement) => root.querySelector<HTMLButtonElement>('[data-fid="start"]');

describe('startLabel', () => {
  it('resumes an open session, names a fixed price, otherwise starts', () => {
    expect(startLabel({ session: { id: 'ses_1', status: 'open', expires_at: '' }, open_amount: true, amount: null })).toBe('Continue to payment');
    expect(startLabel({ session: null, open_amount: false, amount: '25.00' })).toBe('Pay 25.00 USD');
    expect(startLabel({ session: null, open_amount: true, amount: null })).toBe('Start a payment');
  });
});

describe('customer address page', () => {
  it('any amount: the product, "Any amount", the fixed address with an amountless payment request', async () => {
    const { root } = await mount([anyAmount]);
    expect(root.querySelector('h1')!.textContent).toBe('Wallet top-up');
    expect(root.querySelector('[data-fid="price"]')!.textContent).toBe(COPY.anyAmount);
    expect(text(root)).toContain(COPY.anyAmountDetail);
    expect(text(root)).toContain(COPY.addressStart);
    expect(text(root)).toContain(COPY.addressStatic);
    expect(startButton(root)!.textContent).toBe('Start a payment');
    expect(root.querySelector('[data-fid="any-amount"]')!.textContent).toBe('Any amount of USDC');
    expect(root.querySelector('[data-fid="copy-amount"]')).toBeNull();
    expect(qr(root)).toBe(`ethereum:${BASE_USDC}@8453/transfer?address=${EVM_ADDRESS}`);
    expect(document.title).toBe('Wallet top-up · Prismaport');
  });

  it('switches networks with the picker, Solana showing its token account', async () => {
    const { root } = await mount([anyAmount]);
    const solana = root.querySelector<HTMLInputElement>('[data-fid="opt-solana:USDC"]')!;
    solana.checked = true;
    solana.dispatchEvent(new Event('change'));
    expect(text(root)).toContain('USDC on Solana');
    expect(root.querySelector('[data-fid="copy-token-account"]')).not.toBeNull();
    // Solana transfer requests are off: the QR is the plain address.
    expect(qr(root)).toBe(SOL_ADDRESS);
  });

  it('one network warning above the QR, the same as on the session page, following the picker', async () => {
    const { root } = await mount([anyAmount]);
    const warning = () => root.querySelectorAll<HTMLElement>('[data-fid="network-warning"]');
    expect(warning()).toHaveLength(1);
    expect(warning()[0]!.getAttribute('role')).toBe('note');
    expect(warning()[0]!.querySelector('.network-warning-title')!.textContent).toBe('Send only USDC on Base');
    expect(warning()[0]!.textContent).toContain(COPY.sendOnlyDetail);
    expect(warning()[0]!.nextElementSibling!.className).toBe('qr-wrap');
    expect(text(root)).not.toContain('are not detected');
    const solana = root.querySelector<HTMLInputElement>('[data-fid="opt-solana:USDC"]')!;
    solana.checked = true;
    solana.dispatchEvent(new Event('change'));
    expect(warning()[0]!.querySelector('.network-warning-title')!.textContent).toBe('Send only USDC on Solana');
  });

  it('fixed price: names the amount on the button and in the payment request', async () => {
    const { root } = await mount([fixed]);
    expect(root.querySelector('[data-fid="price"]')!.textContent).toBe('25.00 USD');
    expect(startButton(root)!.textContent).toBe('Pay 25.00 USD');
    expect(qr(root)).toBe(`ethereum:${BASE_USDC}@8453/transfer?address=${EVM_ADDRESS}&uint256=25000000`);
  });

  it('the button opens the session and moves to its page', async () => {
    const { root, post, navigate } = await mount([anyAmount]);
    startButton(root)!.click();
    await flush();
    expect(post).toHaveBeenCalledWith('caddr_01JBTOPUP');
    expect(navigate).toHaveBeenCalledWith('/pay/ses_01JBOPENED');
  });

  it('an open session: the button resumes it', async () => {
    const { root } = await mount([{ ...anyAmount, session: { id: 'ses_01JBOPEN', status: 'open', expires_at: '2026-10-23T19:00:00Z' } }]);
    expect(startButton(root)!.textContent).toBe('Continue to payment');
    expect(text(root)).toContain(COPY.addressOpenSession);
  });

  it('rotated: says so, links the new address and shows no deposit address', async () => {
    const { root } = await mount([{ ...rotated, session: { id: 'ses_01JBSTILL', status: 'open', expires_at: '2026-10-23T19:00:00Z' } }]);
    expect(root.querySelector('h2')!.textContent).toBe('This address was replaced');
    expect(text(root)).toContain(COPY.addressReplaced);
    expect(root.querySelector('[data-fid="new-address"]')!.getAttribute('href')).toBe('/a/caddr_01JBNEWER');
    expect(root.querySelector('[data-fid="continue"]')!.getAttribute('href')).toBe('/pay/ses_01JBSTILL');
    expect(root.querySelector('svg.qr')).toBeNull();
    expect(text(root)).not.toContain(EVM_ADDRESS.slice(0, 8));
    expect(startButton(root)).toBeNull();
  });

  it('not live (product deactivated): no address, no button', async () => {
    const { root } = await mount([{ ...anyAmount, live: false, unavailable_reason: 'product_inactive', pay_to: [] }]);
    expect(text(root)).toContain(COPY.addressUnavailable);
    expect(root.querySelector('svg.qr')).toBeNull();
    expect(startButton(root)).toBeNull();
  });

  it('unknown address: not found', async () => {
    const { root } = await mount([new ApiError(404, 'customer_address_not_found', 'no such customer address')]);
    expect(root.querySelector('h1')!.textContent).toBe('Address not found');
    expect(text(root)).toContain(COPY.addressNotFound);
  });

  it('rotated while the page was open: the refusal reloads the address and shows the new state', async () => {
    const { root, navigate } = await mount([anyAmount, rotated], async () => {
      throw new ApiError(409, 'address_rotated', 'this customer address was rotated');
    });
    startButton(root)!.click();
    await flush();
    expect(navigate).not.toHaveBeenCalled();
    expect(root.querySelector('h2')!.textContent).toBe('This address was replaced');
  });

  it('rate limited: the error is announced and the button comes back', async () => {
    const { root } = await mount([anyAmount], async () => {
      throw new ApiError(429, 'rate_limited', 'too many', 12);
    });
    startButton(root)!.click();
    await flush();
    expect(root.querySelector('[data-fid="start-error"]')!.textContent).toContain('Try again in 12 seconds');
    expect(startButton(root)!.disabled).toBe(false);
  });

  it('lost connection after the first load: the reconnecting box, and Try now clears it', async () => {
    const { root, get } = await mount([anyAmount, new ApiError(0, 'network_error', 'The connection failed.'), anyAmount]);
    // Coming back online loads at once; here the connection is still down.
    window.dispatchEvent(new Event('online'));
    await flush();
    const box = root.querySelector<HTMLElement>('[data-fid="offline"]')!;
    expect(box.getAttribute('role')).toBe('status');
    expect(text(box)).toMatch(/^Reconnecting to PrismaportLast checked \d+ s ago\. Your payment still counts\.Try now$/);
    expect(root.querySelector('.deposit')).not.toBeNull();
    box.querySelector<HTMLButtonElement>('[data-fid="offline-retry"]')!.click();
    await flush();
    expect(get).toHaveBeenCalledTimes(3);
    expect(root.querySelector('[data-fid="offline"]')).toBeNull();
  });

  it('records presence only while the page is visible', async () => {
    Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true });
    const visible = await mount([anyAmount]);
    expect(visible.get).toHaveBeenCalledWith('caddr_01JBTOPUP', true);
    Object.defineProperty(document, 'visibilityState', { value: 'hidden', configurable: true });
    const hidden = await mount([anyAmount]);
    expect(hidden.get).toHaveBeenCalledWith('caddr_01JBTOPUP', false);
  });
});

describe('paused network on the address page (D64)', () => {
  it('keeps the address and warns on the paused row only', async () => {
    const paused: PublicAddress = { ...anyAmount, pay_to: anyAmount.pay_to.map((p) => (p.chain === 'base' ? { ...p, paused: true } : p)) };
    const { root } = await mount([paused]);
    expect(root.querySelector('[data-fid="paused-note"]')!.textContent).toContain('Payments on Base are paused');
    expect(root.querySelector('svg.qr')).not.toBeNull();
    const sol = root.querySelector<HTMLInputElement>('[data-fid="opt-solana:USDC"]') ?? root.querySelector<HTMLInputElement>('input[value^="solana"]')!;
    sol.checked = true;
    sol.dispatchEvent(new Event('change'));
    await flush();
    expect(root.querySelector('[data-fid="paused-note"]')).toBeNull();
  });
});
