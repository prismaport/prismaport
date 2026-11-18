import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, CheckoutApi, type Session } from '../src/lib/api';
import { PAUSED, errorCopy, revealErrorCopy } from '../src/lib/copy';
import { mountPay } from '../src/pages/pay';
import { NETWORKS, NOW } from './fixtures';
import { unrevealedSession, withRevealed } from './select-fixtures';

// D64: a paused chain or asset on an open session.

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

async function mount(initial: Session, reveal: (id: string, chain: string) => Promise<Session> = async () => initial) {
  let current = initial;
  const api = new CheckoutApi('', vi.fn() as never);
  vi.spyOn(api, 'session').mockImplementation(async () => ({ data: current, serverTime: null }));
  vi.spyOn(api, 'networks').mockResolvedValue({ data: { data: NETWORKS }, serverTime: null });
  vi.spyOn(api, 'reveal').mockImplementation(async (id, chain) => {
    current = await reveal(id, chain);
    return { data: current, serverTime: null };
  });
  const root = document.createElement('main');
  document.body.replaceChildren(root);
  const handle = await mountPay(root, 'ses_01JBTEST', { api, now: () => NOW, pollMs: 1_000_000, discoverEvm: async () => [] });
  stops.push(handle.stop);
  await flush();
  return { root, api };
}

const pause = (s: Session, chain: string): Session => ({ ...s, pay_to: s.pay_to.map((p) => (p.chain === chain ? { ...p, paused: true, signed_payment: false } : p)) });

describe('paused network in the picker', () => {
  it('a paused network that was never generated is disabled with a Paused tag; the others stay selectable', async () => {
    const { root } = await mount(pause(unrevealedSession(), 'solana'));
    const sol = root.querySelector<HTMLInputElement>('[data-fid="net-solana"]')!;
    const base = root.querySelector<HTMLInputElement>('[data-fid="net-base"]')!;
    expect(sol.disabled).toBe(true);
    expect(base.disabled).toBe(false);
    expect(sol.closest('label')!.querySelector('[data-fid="paused-tag"]')!.textContent).toBe(PAUSED.tag);
    expect(base.closest('label')!.querySelector('[data-fid="paused-tag"]')).toBeNull();
  });

  it('a 409 chain_unavailable on reveal says the network is paused and stays in the picker', async () => {
    const { root } = await mount(unrevealedSession(), async () => {
      throw new ApiError(409, 'chain_unavailable', 'x');
    });
    const input = root.querySelector<HTMLInputElement>('[data-fid="net-base"]')!;
    input.checked = true;
    input.dispatchEvent(new Event('change'));
    root.querySelector<HTMLButtonElement>('[data-fid="generate"]')!.click();
    await flush();
    expect(root.querySelector('[data-fid="reveal-error"]')!.textContent).toBe(PAUSED.reveal);
    expect(root.querySelector('svg.qr')).toBeNull();
  });
});

describe('paused network already generated', () => {
  it('keeps the address and QR, shows the warning, hides the wallet and offers another network', async () => {
    const { root } = await mount(pause(withRevealed(unrevealedSession(), 'base'), 'base'));
    const panel = root.querySelector('[data-fid="paused-panel"]')!;
    expect(panel.getAttribute('role')).toBe('status');
    expect(panel.textContent).toContain(PAUSED.panel('Base'));
    expect(root.querySelector('svg.qr')).not.toBeNull();
    expect(root.querySelector('[data-fid="pay-wallet"]')).toBeNull();
    root.querySelector<HTMLButtonElement>('[data-fid="pay-other-network"]')!.click();
    await flush();
    expect(root.querySelector('[data-fid="select-step"]')).not.toBeNull();
  });

  it('every network paused: the warning stays without a way to another network', async () => {
    const s = pause(pause(withRevealed(unrevealedSession(), 'base'), 'base'), 'solana');
    const { root } = await mount(s);
    expect(root.querySelector('[data-fid="paused-panel"]')!.textContent).toContain(PAUSED.panelOnly('Base'));
    expect(root.querySelector('[data-fid="pay-other-network"]')).toBeNull();
  });

  it('nothing paused shows no panel', async () => {
    const { root } = await mount(withRevealed(unrevealedSession(), 'base'));
    expect(root.querySelector('[data-fid="paused-panel"]')).toBeNull();
  });
});

describe('chain_unavailable copy', () => {
  it('has fixed messages for a reveal and for any other call', () => {
    expect(revealErrorCopy('chain_unavailable')).toBe(PAUSED.reveal);
    expect(errorCopy('chain_unavailable')).toBe(PAUSED.unavailable);
  });
});
