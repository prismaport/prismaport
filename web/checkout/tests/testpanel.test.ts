import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, CheckoutApi, type Session, type TestPanelResult } from '../src/lib/api';
import { mountPay } from '../src/pages/pay';
import { PANEL_LABELS, panelState, resultCopy } from '../src/pages/testpanel';
import { NETWORKS, NOW, SOL_ADDRESS, payTo, session } from './fixtures';

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

const testSession = (over: Partial<Session> = {}) =>
  session({ livemode: false, pay_to: [payTo({ chain: 'base-sepolia' }), payTo({ chain: 'solana-devnet', address: SOL_ADDRESS, signed_payment: false })], ...over });

const paid = (over: Partial<Session> = {}) =>
  testSession({ status: 'paid', amount_received: '25.00', amount_remaining: '0.00',
    payments: [{ chain: 'base-sepolia', asset: 'USDC', amount: '25.000000', status: 'confirmed', tx_hash: '0x0000000000000000' + 'ab'.repeat(24) }], ...over });

async function mount(s: Session) {
  const api = new CheckoutApi('', vi.fn() as never);
  vi.spyOn(api, 'session').mockResolvedValue({ data: s, serverTime: null });
  vi.spyOn(api, 'networks').mockResolvedValue({ data: { data: NETWORKS }, serverTime: null });
  const simulate = vi.spyOn(api, 'simulate');
  const root = document.createElement('main');
  document.body.replaceChildren(root);
  const handle = await mountPay(root, s.id, { api, now: () => NOW, pollMs: 1_000_000, discoverEvm: async () => [] });
  stops.push(handle.stop);
  await flush();
  return { root, api, simulate };
}

const panelOf = (root: HTMLElement) => root.querySelector<HTMLElement>('[data-testid="test-panel"]')!;
const button = (root: HTMLElement, label: string) =>
  [...panelOf(root).querySelectorAll<HTMLButtonElement>('button')].find((b) => b.textContent === label)!;

describe('test panel state', () => {
  it('an open session can be paid, part paid, overpaid or expired, not settled', () => {
    const { buttons, hint } = panelState(testSession(), false);
    expect(buttons.map((b) => [b.label, b.enabled])).toEqual([
      ['Pay exact amount', true], ['Pay part', true], ['Overpay', true], ['Let it expire', true], ['Settle', false],
    ]);
    expect(hint).toBe('Settle works once the session is paid or has expired.');
  });

  it('a paid session can be settled once', () => {
    expect(panelState(paid(), false).buttons.map((b) => b.enabled)).toEqual([false, false, false, false, true]);
    const after = panelState(paid(), true);
    expect(after.buttons.every((b) => !b.enabled)).toBe(true);
    expect(after.hint).toBe('Settled. The dashboard shows the fee and each payout.');
  });

  it('an expired session without payments has nothing to settle; one under review takes no step', () => {
    const expired = panelState(testSession({ status: 'expired' }), false);
    expect(expired.buttons.every((b) => !b.enabled)).toBe(true);
    expect(expired.hint).toMatch(/^Nothing was paid, so there is nothing to settle\./);
    expect(panelState(paid({ status: 'under_review' }), false).buttons.every((b) => !b.enabled)).toBe(true);
  });

  it('says which events a step sent', () => {
    const res = { action: 'pay_exact', settled: false, session: paid(), events: [{ type: 'session.payment_detected', session_version: 2 }, { type: 'session.paid', session_version: 3 }] } as TestPanelResult;
    expect(resultCopy(res)).toBe('Simulated. Events sent: session.payment_detected, session.paid.');
    expect(resultCopy({ ...res, action: 'settle', events: [] })).toBe('Settled. The dashboard shows the fee and each payout.');
  });
});

describe('test panel on the pay page', () => {
  it('never shows on a live session', async () => {
    const { root, simulate } = await mount(session());
    expect(panelOf(root).childElementCount).toBe(0);
    expect(root.textContent).not.toContain('Not a real payment');
    expect(root.textContent).not.toContain(PANEL_LABELS.pay_exact);
    expect(simulate).not.toHaveBeenCalled();
  });

  it('is clearly marked on a test session and pays through the simulator', async () => {
    const { root, simulate } = await mount(testSession());
    const panel = panelOf(root);
    expect(panel.querySelector('h2')!.textContent).toBe('SimulateNot a real payment');
    // Collapsed until the tester opens it.
    const toggle = panel.querySelector<HTMLButtonElement>('[data-fid="test-toggle"]')!;
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(panel.querySelector<HTMLElement>('#test-panel-body')!.hidden).toBe(true);
    toggle.focus();
    toggle.click();
    expect(panel.querySelector('[data-fid="test-toggle"]')!.getAttribute('aria-expanded')).toBe('true');
    expect(document.activeElement).toBe(panel.querySelector('[data-fid="test-toggle"]'));
    expect(panel.querySelector<HTMLElement>('#test-panel-body')!.hidden).toBe(false);
    expect([...panel.querySelectorAll('.test-actions button')].map((b) => b.textContent)).toEqual(Object.values(PANEL_LABELS));
    simulate.mockResolvedValueOnce({ data: { action: 'pay_exact', settled: false, session: paid(),
      events: [{ type: 'session.payment_detected', session_version: 2 }, { type: 'session.paid', session_version: 3 }] }, serverTime: null });
    button(root, 'Pay exact amount').click();
    await flush();
    expect(simulate).toHaveBeenCalledWith('ses_01JBTEST', { action: 'pay_exact', chain: 'base-sepolia', asset: 'USDC' });
    expect(root.querySelector('.success .status-line')!.textContent).toBe('Paid. You can close this page.');
    expect(panel.querySelector('[role=status]')!.textContent).toBe('Simulated. Events sent: session.payment_detected, session.paid.');
    // Paid, only the step left shows, and the panel stays open.
    expect(panel.querySelector<HTMLElement>('#test-panel-body')!.hidden).toBe(false);
    expect([...panel.querySelectorAll('.test-actions button')].map((b) => b.textContent)).toEqual(['Settle']);
    expect(button(root, 'Settle').disabled).toBe(false);

    simulate.mockResolvedValueOnce({ data: { action: 'settle', settled: true, session: paid(), events: [] }, serverTime: null });
    button(root, 'Settle').click();
    await flush();
    expect(simulate).toHaveBeenLastCalledWith('ses_01JBTEST', { action: 'settle' });
    expect(panel.querySelector('[role=status]')!.textContent).toBe('Settled. The dashboard shows the fee and each payout.');
    expect(panel.querySelector('.test-actions')).toBeNull();
  });

  it('an expired session with nothing paid shows no step, only why', async () => {
    const { root } = await mount(testSession({ status: 'expired' }));
    const panel = panelOf(root);
    expect(panel.querySelector('.test-actions')).toBeNull();
    expect(panel.querySelector('[data-testid="test-panel-hint"]')!.textContent).toMatch(/^Nothing was paid/);
  });

  it('pays on the network the payer picked', async () => {
    const { root, simulate } = await mount(testSession());
    root.querySelectorAll<HTMLInputElement>('input[name=payto]')[1]!.click();
    await flush();
    simulate.mockResolvedValueOnce({ data: { action: 'pay_partial', settled: false, session: testSession({ status: 'partially_paid' }), events: [] }, serverTime: null });
    button(root, 'Pay part').click();
    await flush();
    expect(simulate).toHaveBeenCalledWith('ses_01JBTEST', { action: 'pay_partial', chain: 'solana-devnet', asset: 'USDC' });
  });

  it('shows a refusal as a sentence and keeps the buttons usable', async () => {
    const { root, simulate } = await mount(testSession());
    simulate.mockRejectedValueOnce(new ApiError(409, 'invalid_state', 'a detected payment is still pending confirmation'));
    button(root, 'Let it expire').click();
    await flush();
    const note = panelOf(root).querySelector('[role=status]')!;
    expect(note.textContent).toBe('A detected payment is still pending confirmation.');
    expect(note.hasAttribute('data-emphasis')).toBe(true);
    expect(button(root, 'Let it expire').disabled).toBe(false);
    simulate.mockRejectedValueOnce(new ApiError(429, 'rate_limited', 'slow down', 20));
    button(root, 'Overpay').click();
    await flush();
    expect(panelOf(root).querySelector('[role=status]')!.textContent).toBe('Too many attempts from this connection. Try again in 20 seconds.');
  });
});

describe('test panel on an open-amount session', () => {
  const anyAmount = (over: Partial<Session> = {}) => testSession({ amount: null, open_amount: true, amount_remaining: null,
    pay_to: [payTo({ chain: 'base-sepolia', amount: undefined, signed_payment: false })], ...over });

  it('offers to pay a typed amount, never a part or an overpayment', () => {
    expect(panelState(anyAmount(), false).buttons.map((b) => [b.label, b.enabled])).toEqual([
      ['Pay this amount', true], ['Let it expire', true], ['Settle', false],
    ]);
    // A paid open-amount session keeps crediting until it expires.
    expect(panelState(anyAmount({ status: 'paid' }), false).buttons[0]!.enabled).toBe(true);
  });

  it('sends the typed amount', async () => {
    const { root, simulate } = await mount(anyAmount());
    const input = panelOf(root).querySelector<HTMLInputElement>('[data-fid="test-amount"]')!;
    input.value = '12.5';
    simulate.mockResolvedValueOnce({ data: { action: 'pay_exact', settled: false, session: anyAmount({ status: 'paid' }), events: [] }, serverTime: null });
    button(root, 'Pay this amount').click();
    await flush();
    expect(simulate).toHaveBeenCalledWith('ses_01JBTEST', { action: 'pay_exact', chain: 'base-sepolia', asset: 'USDC', amount: '12.5' });
  });
});
