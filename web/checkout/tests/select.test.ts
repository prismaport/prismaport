import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, CheckoutApi, type Session } from '../src/lib/api';
import { COPY, payWindowCopy } from '../src/lib/copy';
import { mountPay } from '../src/pages/pay';
import type { DiscoveredWallet } from '../src/wallet/evm';
import { EVM_ADDRESS, NETWORKS, NOW, SOL_ADDRESS, session } from './fixtures';
import { unrevealedSession, withRevealed } from './select-fixtures';

const flush = async () => {
  for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
};

let stops: (() => void)[] = [];
afterEach(() => {
  stops.forEach((s) => s());
  stops = [];
  sessionStorage.clear();
  vi.restoreAllMocks();
});

type Reveal = (id: string, chain: string) => Promise<Session>;

async function mount(initial: Session, reveal: Reveal = async () => initial, clock: () => number = () => NOW, wallets: DiscoveredWallet[] = []) {
  let current = initial;
  const api = new CheckoutApi('', vi.fn() as never);
  vi.spyOn(api, 'session').mockImplementation(async () => ({ data: current, serverTime: null }));
  vi.spyOn(api, 'networks').mockResolvedValue({ data: { data: NETWORKS }, serverTime: null });
  const revealSpy = vi.spyOn(api, 'reveal').mockImplementation(async (id, chain) => {
    current = await reveal(id, chain);
    return { data: current, serverTime: null };
  });
  const root = document.createElement('main');
  document.body.replaceChildren(root);
  const handle = await mountPay(root, 'ses_01JBTEST', { api, now: clock, pollMs: 1_000_000, discoverEvm: async () => wallets });
  stops.push(handle.stop);
  await flush();
  return { root, revealSpy };
}

const text = (root: HTMLElement) => root.textContent ?? '';
const generate = (root: HTMLElement) => root.querySelector<HTMLButtonElement>('[data-fid="generate"]')!;
const choose = (root: HTMLElement, chain: string) => {
  const input = root.querySelector<HTMLInputElement>(`[data-fid="net-${chain}"]`)!;
  input.checked = true;
  input.dispatchEvent(new Event('change'));
};

describe('choose a network (D48)', () => {
  it('first step: amount, networks with assets, a disabled button, the pay window note, no timer and no address', async () => {
    const { root, revealSpy } = await mount(unrevealedSession());
    expect(root.querySelector('.amount-hero')!.textContent).toBe('25.00 USD');
    const options = [...root.querySelectorAll('.option')];
    expect(options.map((o) => o.querySelector('.asset')!.textContent)).toEqual(['Base', 'Solana']);
    expect(options[1]!.textContent).toContain('25.00 USDC');
    expect(options[1]!.textContent).toContain('25.00 USDT');
    expect(options[1]!.querySelectorAll('img').length).toBeGreaterThanOrEqual(3);
    expect(generate(root).disabled).toBe(true);
    expect(text(root)).toContain('This payment stays open for 30 minutes once generated.');
    expect(root.querySelector('[data-timer]')).toBeNull();
    expect(root.querySelector('svg.qr')).toBeNull();
    expect(text(root)).not.toContain(EVM_ADDRESS.slice(0, 8));
    expect(revealSpy).not.toHaveBeenCalled();
  });

  it('a single-network product shows the same step with one option', async () => {
    const s = unrevealedSession();
    const { root, revealSpy } = await mount({ ...s, pay_to: [s.pay_to[0]!] });
    expect(root.querySelectorAll('.option')).toHaveLength(1);
    expect(root.querySelector('svg.qr')).toBeNull();
    expect(generate(root).disabled).toBe(true);
    expect(revealSpy).not.toHaveBeenCalled();
  });

  it('choose, generate: one reveal request, then the payment view with address, QR and the countdown to the pay deadline', async () => {
    const { root, revealSpy } = await mount(unrevealedSession(), async () => withRevealed(unrevealedSession(), 'solana'));
    choose(root, 'solana');
    expect(generate(root).disabled).toBe(false);
    generate(root).click();
    await flush();
    expect(revealSpy).toHaveBeenCalledTimes(1);
    expect(revealSpy).toHaveBeenCalledWith('ses_01JBTEST', 'solana');
    expect(root.querySelector('svg.qr')).not.toBeNull();
    expect(root.querySelector('.field .address')!.textContent).toContain(SOL_ADDRESS.slice(0, 4));
    expect(root.querySelector('[data-timer]')!.textContent).toBe('Expires in 30:00');
    expect(root.querySelector('[data-fid="generate"]')).toBeNull();
    expect(document.activeElement?.id).toBe('title');
  });

  it('pay another way: the warning, the generated network marked, reopening it without a new request, generating another one', async () => {
    const first = withRevealed(unrevealedSession(), 'solana');
    const { root, revealSpy } = await mount(first, async () => withRevealed(first, 'base'));
    expect(root.querySelector('svg.qr')).not.toBeNull();
    root.querySelector<HTMLButtonElement>('[data-fid="pay-another"]')!.click();
    expect(text(root)).toContain(COPY.switchWarning);
    expect(root.querySelector('svg.qr')).toBeNull();
    expect(root.querySelector('[data-timer]')).not.toBeNull();
    const labels = [...root.querySelectorAll('.option')].map((o) => o.querySelector('.generated')?.textContent ?? null);
    expect(labels).toEqual([null, 'Generated']);

    choose(root, 'solana');
    expect(generate(root).textContent).toBe('Show payment');
    generate(root).click();
    await flush();
    expect(revealSpy).not.toHaveBeenCalled();
    expect(root.querySelector('svg.qr')).not.toBeNull();

    root.querySelector<HTMLButtonElement>('[data-fid="pay-another"]')!.click();
    choose(root, 'base');
    generate(root).click();
    await flush();
    expect(revealSpy).toHaveBeenCalledWith('ses_01JBTEST', 'base');
    expect(text(root)).toContain('Send exactly 25.00 USDC on Base');
  });

  it('"Back to the payment" leaves the list without a request', async () => {
    const { root, revealSpy } = await mount(withRevealed(unrevealedSession(), 'base'));
    root.querySelector<HTMLButtonElement>('[data-fid="pay-another"]')!.click();
    root.querySelector<HTMLButtonElement>('[data-fid="back-to-payment"]')!.click();
    expect(root.querySelector('svg.qr')).not.toBeNull();
    expect(revealSpy).not.toHaveBeenCalled();
  });

  it('reload with a generated network goes straight to its payment view', async () => {
    const { root } = await mount(withRevealed(unrevealedSession(), 'solana'));
    expect(root.querySelector('[data-fid="generate"]')).toBeNull();
    expect(text(root)).toContain('Send exactly 25.00 USDC on Solana');
    expect(root.querySelector('.field .address')!.textContent).toContain(SOL_ADDRESS.slice(0, 4));
  });

  it('reload prefers the network that has a payment', async () => {
    const s = withRevealed(withRevealed(unrevealedSession(), 'solana'), 'base');
    const { root } = await mount({
      ...s, status: 'partially_paid', amount_received: '5.00', amount_remaining: '20.00',
      payments: [{ chain: 'solana', asset: 'USDT', amount: '5.000000', status: 'confirmed', tx_hash: '5'.repeat(64) }],
    });
    expect(text(root)).toContain('USDT on Solana');
  });

  it('the selection window ended before any generation: the lock panel, no network list, no address', async () => {
    const { root } = await mount(unrevealedSession({ expires_at: '2026-10-23T17:59:00Z' }));
    expect(root.querySelector('[data-fid="lock-panel"]')).not.toBeNull();
    expect(root.querySelector('[data-fid="generate"]')).toBeNull();
    expect(root.querySelectorAll('.option')).toHaveLength(0);
    expect(root.querySelector('svg.qr')).toBeNull();
  });

  it('the selection window runs out while the list is open: the page locks', async () => {
    let t = NOW;
    const { root } = await mount(unrevealedSession({ expires_at: new Date(NOW + 500).toISOString() }), undefined, () => t);
    expect(generate(root)).not.toBeNull();
    t = NOW + 5000;
    await new Promise((r) => setTimeout(r, 1200));
    await flush();
    expect(root.querySelector('[data-fid="lock-panel"]')).not.toBeNull();
    expect(root.querySelector('[data-fid="generate"]')).toBeNull();
  });

  it('no expiry warning or timer in the choose step, however close the selection deadline is', async () => {
    const { root } = await mount(unrevealedSession({ expires_at: new Date(NOW + 60_000).toISOString() }));
    expect(root.querySelector('[data-fid="expiry-warning"]')).toBeNull();
    expect(root.querySelector('[data-timer]')).toBeNull();
  });

  it('words the pay window in the note', () => {
    expect(payWindowCopy(1800)).toBe('This payment stays open for 30 minutes once generated.');
    expect(payWindowCopy(3600)).toBe('This payment stays open for 1 hour once generated.');
    expect(payWindowCopy(60)).toBe('This payment stays open for 1 minute once generated.');
    expect(payWindowCopy(undefined)).toContain('limited time');
  });
});

describe('reveal errors', () => {
  const cases: [string, number, RegExp][] = [
    ['session_expired', 410, /session has closed/],
    ['session_not_payable', 409, /no longer accepts payments/],
    ['wallet_blacklisted', 409, /cannot receive payments on this network/],
    ['invalid_request', 400, /not available for this payment/],
    ['service_unavailable', 503, /Cannot generate this payment right now/],
    ['network_error', 0, /Cannot reach Prismaport/],
  ];
  for (const [code, status, re] of cases) {
    it(`${code}: clear copy, the choice kept, nothing shown for the network`, async () => {
      const { root } = await mount(unrevealedSession(), async () => {
        throw new ApiError(status, code, 'x');
      });
      choose(root, 'base');
      generate(root).click();
      await flush();
      const alert = root.querySelector('[data-fid="reveal-error"]')!;
      expect(alert.textContent).toMatch(re);
      expect(alert.getAttribute('role')).toBe('alert');
      expect(root.querySelector('svg.qr')).toBeNull();
      expect(root.querySelector<HTMLInputElement>('[data-fid="net-base"]')!.checked).toBe(true);
      expect(generate(root).disabled).toBe(false);
    });
  }

  it('rate limited says when to try again', async () => {
    const { root } = await mount(unrevealedSession(), async () => {
      throw new ApiError(429, 'rate_limited', 'slow', 30);
    });
    choose(root, 'base');
    generate(root).click();
    await flush();
    expect(text(root)).toContain('Try again in 30 seconds.');
  });

  it('the button is disabled while the request is in flight', async () => {
    let release: (s: Session) => void = () => undefined;
    const { root } = await mount(unrevealedSession(), () => new Promise<Session>((r) => { release = r; }));
    choose(root, 'base');
    generate(root).click();
    await flush();
    expect(generate(root).disabled).toBe(true);
    expect(generate(root).textContent).toBe('Generating');
    release(withRevealed(unrevealedSession(), 'base'));
    await flush();
    expect(root.querySelector('svg.qr')).not.toBeNull();
  });
});

describe('older API', () => {
  it('a session without revealed flags that serves addresses opens on the payment view', async () => {
    const { root } = await mount(session());
    expect(root.querySelector('[data-fid="generate"]')).toBeNull();
    expect(root.querySelector('svg.qr')).not.toBeNull();
  });
});

describe('wallet and paid state with the choose step', () => {
  const metamask: DiscoveredWallet = { info: { uuid: 'mm', name: 'MetaMask', icon: '', rdns: 'io.metamask' }, provider: { request: async () => [] } as never };

  it('a browser wallet shows nothing in the choose step; the wallet card opens with the generated network', async () => {
    const { root } = await mount(unrevealedSession(), async () => withRevealed(unrevealedSession(), 'base'), () => NOW, [metamask]);
    for (const fid of ['connect', 'pay-wallet', 'pay-address', 'method-wallet', 'method-address', 'gas-help', 'pay-another', 'paid']) {
      expect(root.querySelector(`[data-fid="${fid}"]`), fid).toBeNull();
    }
    expect(root.querySelector('.wallet-card')).toBeNull();
    choose(root, 'base');
    generate(root).click();
    await flush();
    expect(root.querySelector('[data-fid="select-step"]')).toBeNull();
    const card = root.querySelector<HTMLElement>('[data-fid="pay-wallet"]')!;
    expect(card.hidden).toBe(false);
    expect(card.querySelector('[data-fid="connect"]')).not.toBeNull();
    expect(root.querySelector('[data-fid="method-address"]')).not.toBeNull();
    expect(root.querySelector('[data-fid="pay-another"]')).not.toBeNull();
  });

  it('pay another way hides the wallet card and the address until a network is shown again', async () => {
    const { root } = await mount(withRevealed(unrevealedSession(), 'base'), undefined, () => NOW, [metamask]);
    expect(root.querySelector('[data-fid="connect"]')).not.toBeNull();
    root.querySelector<HTMLButtonElement>('[data-fid="pay-another"]')!.click();
    expect(root.querySelector('[data-fid="select-step"]')).not.toBeNull();
    expect(root.querySelector('[data-fid="connect"]')).toBeNull();
    expect(root.querySelector('[data-fid="pay-wallet"]')).toBeNull();
    expect(root.querySelector('svg.qr')).toBeNull();
  });

  it('paid: the success block, no pill, no choose step and no wallet', async () => {
    const paid = withRevealed(unrevealedSession(), 'base');
    const { root } = await mount({
      ...paid, status: 'paid', amount_received: '25.00', amount_remaining: '0.00',
      payments: [{ chain: 'base', asset: 'USDC', amount: '25.000000', status: 'confirmed', tx_hash: '0x' + 'ab'.repeat(32), confirmed_at: '2026-10-23T18:10:00Z' }],
    }, undefined, () => NOW, [metamask]);
    expect(root.querySelector('[data-fid="paid"]')).not.toBeNull();
    expect(root.querySelector('.card .pill')).toBeNull();
    expect(root.querySelector('[data-fid="select-step"]')).toBeNull();
    expect(root.querySelector('[data-fid="connect"]')).toBeNull();
  });
});
