import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, CheckoutApi, type PublicLink, type Session } from '../src/lib/api';
import { COPY, SENT_GRACE_MS } from '../src/lib/copy';
import { MAX_RETRY_AFTER_MS, PROGRESS_COPY, mountPay } from '../src/pages/pay';
import { NETWORKS, NOW, payTo, session } from './fixtures';

/** Moves fake time by `ms`: the polling, the 1 s ticker and the answers they wait for. */
const wait = (ms: number) => vi.advanceTimersByTimeAsync(ms);
const EXPIRES = Date.parse('2026-10-23T19:00:00Z');
const closedPayTo = () => payTo({ address: undefined, signed_payment: false });
const closed = (over: Partial<Session> = {}) => session({ status: 'expired', pay_to: [closedPayTo()], ...over });
const detected = { chain: 'base', asset: 'USDC', amount: '25.000000', status: 'detected' as const, tx_hash: '0x' + 'ab'.repeat(32) };

let stops: (() => void)[] = [];
// Every test runs on fake time: a real wait made the polling counts depend on the machine's speed.
beforeEach(() => {
  vi.useFakeTimers();
});
afterEach(() => {
  stops.forEach((s) => s());
  stops = [];
  sessionStorage.clear();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

async function mount(s: Session | (() => Session), opts: { now?: () => number; pollMs?: number; link?: Partial<PublicLink> } = {}) {
  const api = new CheckoutApi('', vi.fn() as never);
  const get = vi.spyOn(api, 'session').mockImplementation(async () => ({ data: typeof s === 'function' ? s() : s, serverTime: null }));
  vi.spyOn(api, 'networks').mockResolvedValue({ data: { data: NETWORKS }, serverTime: null });
  const link = vi.spyOn(api, 'link').mockResolvedValue({ data: { id: 'plink_1', active: true, ...opts.link } as PublicLink, serverTime: null });
  const root = document.createElement('main');
  document.body.replaceChildren(root);
  const handle = await mountPay(root, 'ses_01JBTEST', { api, now: opts.now ?? (() => NOW), pollMs: opts.pollMs ?? 20, discoverEvm: async () => [] });
  stops.push(handle.stop);
  await wait(10);
  return { root, api, get, link };
}

const text = (r: HTMLElement) => r.textContent ?? '';
const DEPOSIT = ['svg.qr', '[data-fid="copy-address"]', '[data-fid="copy-amount"]', '[data-fid="open-wallet"]', '[data-fid="toggle-address"]', '[data-fid="connect"]', '[data-fid="pay"]', '.deposit', '#pay-address', '#pay-wallet', '[data-fid^="method-"]'];

describe('expiry lock (D45)', () => {
  it('reload of an expired session shows the lock from the first render, with no deposit DOM', async () => {
    const { root } = await mount(closed());
    expect(text(root)).toContain(COPY.closed);
    expect(root.querySelector('[data-fid="lock-panel"]')).not.toBeNull();
    for (const sel of DEPOSIT) expect(root.querySelector(sel), sel).toBeNull();
  });

  it('renders the server\'s expired shape (no pay_to amount) and shows the session amount as the price', async () => {
    const s = closed({ pay_to: [payTo({ address: undefined, amount: undefined, signed_payment: false })] });
    const { root } = await mount(s);
    expect(root.querySelector('[data-fid="lock-panel"]')).not.toBeNull();
    expect(root.querySelector('.amount-hero')?.textContent).toContain(`${s.amount!.split('.')[0]}.`);
    expect(text(root)).not.toContain('undefined');
  });

  it('client clock past expires_at removes the payment block even if the server still says open', async () => {
    const { root } = await mount(session(), { now: () => EXPIRES + 1000 });
    expect(text(root)).toContain(COPY.closed);
    for (const sel of DEPOSIT) expect(root.querySelector(sel), sel).toBeNull();
    expect(text(root)).not.toContain('Send exactly');
  });

  it('locks live when the clock crosses expires_at, and the address leaves the DOM', async () => {
    let t = EXPIRES - 1500;
    const { root } = await mount(session(), { now: () => t });
    expect(root.querySelector('.deposit')).not.toBeNull();
    t = EXPIRES + 1;
    await wait(1000);
    expect(root.querySelector('[data-fid="lock-panel"]')).not.toBeNull();
    expect(root.querySelector('.deposit')).toBeNull();
  });

  it('a payment being confirmed keeps the confirming copy, no lock, and polling continues', async () => {
    const s = session({ status: 'partially_paid', amount_received: '0.00', amount_remaining: '25.00', payments: [detected] });
    const { root, get } = await mount(s, { now: () => EXPIRES + 1000 });
    expect(text(root)).toContain(PROGRESS_COPY.found);
    expect(root.querySelector('[data-fid="progress"]')!.getAttribute('data-phase')).toBe('seen');
    expect(root.querySelector('[data-fid="lock-panel"]')).toBeNull();
    const n = get.mock.calls.length;
    await wait(100);
    expect(get.mock.calls.length).toBeGreaterThan(n);
  });

  it('polling stops at the lock', async () => {
    const { get } = await mount(closed());
    const n = get.mock.calls.length;
    await wait(120);
    expect(get.mock.calls.length).toBe(n);
  });

  it('keeps polling while open', async () => {
    const { get } = await mount(session());
    const n = get.mock.calls.length;
    await wait(100);
    expect(get.mock.calls.length).toBeGreaterThan(n);
  });

  it('a visibility change re-checks once, and does not restart polling at the lock', async () => {
    const { get } = await mount(closed());
    const n = get.mock.calls.length;
    document.dispatchEvent(new Event('visibilitychange'));
    await wait(120);
    expect(get.mock.calls.length).toBe(n + 1);
  });

  it('a visibility re-check that finds a new expires_at unlocks the page', async () => {
    let s: Session = closed();
    const { root } = await mount(() => s);
    expect(root.querySelector('.deposit')).toBeNull();
    s = session({ expires_at: '2026-10-23T20:00:00Z' });
    document.dispatchEvent(new Event('visibilitychange'));
    await wait(30);
    expect(root.querySelector('.deposit')).not.toBeNull();
  });

  it('a paid session is not locked and stops polling', async () => {
    const { root, get } = await mount(session({ status: 'paid', amount_received: '25.00', amount_remaining: '0.00' }), { now: () => EXPIRES + 1000 });
    expect(root.querySelector('[data-fid="lock-panel"]')).toBeNull();
    const n = get.mock.calls.length;
    await wait(80);
    expect(get.mock.calls.length).toBe(n);
  });
});

describe('expiry warning', () => {
  it('shows in the last 300 s by default with the countdown, not before', async () => {
    const before = await mount(session(), { now: () => EXPIRES - 301_000 });
    expect(before.root.querySelector('[data-fid="expiry-warning"]')).toBeNull();
    const inside = await mount(session(), { now: () => EXPIRES - 299_000 });
    expect(text(inside.root)).toContain('This payment closes in 4:59. Do not send funds after it closes.');
    expect(inside.root.querySelector('.deposit')).not.toBeNull();
  });

  it('honours expiry_warning_seconds', async () => {
    const { root } = await mount(session({ expiry_warning_seconds: 60 }), { now: () => EXPIRES - 120_000 });
    expect(root.querySelector('[data-fid="expiry-warning"]')).toBeNull();
    const b = await mount(session({ expiry_warning_seconds: 60 }), { now: () => EXPIRES - 59_000 });
    expect(text(b.root)).toContain('closes in 0:59');
  });
});

describe('start a new payment', () => {
  it('shows when the session came from a link that is still active (link looked up)', async () => {
    const { root, link } = await mount(closed({ link_id: 'plink_1' }));
    await wait(10);
    expect(link).toHaveBeenCalledWith('plink_1');
    expect(root.querySelector<HTMLAnchorElement>('[data-fid="start-new"]')!.getAttribute('href')).toBe('/l/plink_1');
  });

  it('uses link_active from the payload without a lookup', async () => {
    const { root, link } = await mount(closed({ link_id: 'plink_1', link_active: true }));
    expect(root.querySelector('[data-fid="start-new"]')).not.toBeNull();
    expect(link).not.toHaveBeenCalled();
  });

  it('is hidden when the link was deactivated', async () => {
    const a = await mount(closed({ link_id: 'plink_1', link_active: false }));
    expect(a.root.querySelector('[data-fid="start-new"]')).toBeNull();
    const b = await mount(closed({ link_id: 'plink_1' }), { link: { active: false } });
    await wait(10);
    expect(b.root.querySelector('[data-fid="start-new"]')).toBeNull();
  });

  it('is hidden without a link id and while the session is payable', async () => {
    const a = await mount(closed());
    expect(a.root.querySelector('[data-fid="start-new"]')).toBeNull();
    const b = await mount(session({ link_id: 'plink_1', link_active: true }));
    expect(b.root.querySelector('[data-fid="start-new"]')).toBeNull();
  });
});

describe('payer presence', () => {
  const setVisibility = (v: 'visible' | 'hidden') => Object.defineProperty(document, 'visibilityState', { value: v, configurable: true });
  afterEach(() => setVisibility('visible'));

  it('the API adds ?active=1 only when asked', async () => {
    const fetchFn = vi.fn(async () => new Response(JSON.stringify(session()), { status: 200 }));
    const api = new CheckoutApi('', fetchFn as never);
    await api.session('ses_1');
    await api.session('ses_1', true);
    expect(fetchFn.mock.calls.map((c) => (c as unknown as [string])[0])).toEqual(['/v1/checkout/sessions/ses_1', '/v1/checkout/sessions/ses_1?active=1']);
  });

  it('the page sends active=true while visible and false while hidden', async () => {
    setVisibility('visible');
    const a = await mount(session());
    expect(a.get.mock.calls[0]).toEqual(['ses_01JBTEST', true]);
    setVisibility('hidden');
    const b = await mount(session());
    expect(b.get.mock.calls[0]).toEqual(['ses_01JBTEST', false]);
  });
});

describe('a payment this page sent, past expiry', () => {
  const TX = '0x' + 'cd'.repeat(32);
  const sent = () => sessionStorage.setItem('pp:sent:ses_01JBTEST', JSON.stringify({ key: 'base:USDC', signature: TX, at: EXPIRES - 30_000, seen: 0 }));
  const submitted = { id: 'txj_1', chain: 'base', asset: 'USDC', status: 'submitted' as const, outcome: null, tx_hash: null };
  afterEach(() => sessionStorage.clear());

  it('keeps checking instead of locking: no Expired pill, no new payment, polling goes on', async () => {
    sent();
    const { root, get } = await mount(session({ link_id: 'plink_1', link_active: true }), { now: () => EXPIRES + 1000 });
    expect(root.querySelector('[data-fid="lock-panel"]')).toBeNull();
    expect(root.querySelector('[data-fid="start-new"]')).toBeNull();
    expect(text(root)).not.toContain(COPY.closed);
    for (const sel of DEPOSIT) expect(root.querySelector(sel), sel).toBeNull();
    expect(root.querySelector('[data-fid="grace-clock"]')!.textContent).toBe('Time ran out. Still checking your payment.');
    expect(root.querySelector('.pill')).toBeNull();
    const progress = root.querySelector('[data-fid="progress"]')!;
    expect(progress.getAttribute('data-phase')).toBe('grace');
    expect(progress.querySelector('h2')!.textContent).toBe('Checking your payment');
    expect(text(progress as HTMLElement)).toContain(
      'Your wallet sent it before the time ran out. We are waiting to see it on Base. A payment that lands now still reaches the merchant.');
    expect(progress.querySelector('.note[role="status"]')!.textContent).toBe('Do not send it again. We keep checking for 10 minutes. Keep this page open.');
    expect(root.querySelector('[data-fid="send-again"]')).toBeNull();
    const n = get.mock.calls.length;
    await wait(100);
    expect(get.mock.calls.length).toBeGreaterThan(n);
  });

  it('also when the server already says expired, and for a signed payment Prismaport is still sending', async () => {
    const { root } = await mount(closed({ link_id: 'plink_1', link_active: true, signed_payments: [submitted] }), { now: () => EXPIRES + 60_000 });
    expect(root.querySelector('[data-fid="lock-panel"]')).toBeNull();
    expect(root.querySelector('[data-fid="start-new"]')).toBeNull();
    expect(root.querySelector('[data-fid="progress"]')!.getAttribute('data-phase')).toBe('grace');
    // Found on the session, not sent from this tab: the page does not know when it was signed.
    expect(root.querySelector('[data-elapsed]')).toBeNull();
  });

  it('a signed payment that did not go through holds nothing: the lock as before', async () => {
    const { root } = await mount(closed({ signed_payments: [{ ...submitted, status: 'failed', outcome: 'rejected' }] }), { now: () => EXPIRES + 60_000 });
    expect(root.querySelector('[data-fid="lock-panel"]')).not.toBeNull();
  });

  it('ten minutes after expiry with nothing seen: the lock as before, and polling stops', async () => {
    expect(SENT_GRACE_MS).toBe(600_000);
    sent();
    const { root, get } = await mount(closed({ link_id: 'plink_1', link_active: true }), { now: () => EXPIRES + SENT_GRACE_MS + 1 });
    expect(text(root)).toContain(COPY.closed);
    expect(root.querySelector('[data-fid="start-new"]')).not.toBeNull();
    const n = get.mock.calls.length;
    await wait(80);
    expect(get.mock.calls.length).toBe(n);
  });

  it('locks live when the ten minutes run out', async () => {
    sent();
    let t = EXPIRES + SENT_GRACE_MS - 500;
    const { root } = await mount(session(), { now: () => t });
    expect(root.querySelector('[data-fid="progress"]')!.getAttribute('data-phase')).toBe('grace');
    t = EXPIRES + SENT_GRACE_MS + 1;
    await wait(1000);
    expect(root.querySelector('[data-fid="lock-panel"]')).not.toBeNull();
    expect(root.querySelector('[data-fid="progress"]')).toBeNull();
  });

  it('the payment shows up: the normal flow takes over', async () => {
    sent();
    let s = session();
    const { root } = await mount(() => s, { now: () => EXPIRES + 1000 });
    expect(root.querySelector('[data-fid="progress"]')!.getAttribute('data-phase')).toBe('grace');
    s = session({ payments: [{ ...detected, tx_hash: TX }] });
    await wait(60);
    expect(root.querySelector('[data-fid="progress"]')!.getAttribute('data-phase')).toBe('seen');
    expect(sessionStorage.getItem('pp:sent:ses_01JBTEST')).toBeNull();
  });
});

describe('polling', () => {
  it('goes on through failed requests', async () => {
    let fail = true;
    const { root, get } = await mount(() => {
      if (fail) throw new ApiError(0, 'network_error', 'The connection failed.');
      return session();
    });
    await wait(200);
    expect(get.mock.calls.length).toBeGreaterThanOrEqual(2);
    fail = false;
    await wait(400);
    expect(root.querySelector('.deposit')).not.toBeNull();
  });

  it('a refusal waits out its Retry-After, also on "Try now" or back online, then polls again', async () => {
    const { get } = await mount(() => {
      throw new ApiError(429, 'rate_limited', 'Too many requests.', 1);
    });
    const n = get.mock.calls.length;
    await wait(500);
    window.dispatchEvent(new Event('online'));
    document.dispatchEvent(new Event('visibilitychange'));
    await wait(400);
    expect(get.mock.calls.length).toBe(n);
    // The second is up (from the refusal, not from the events): polling resumes.
    await wait(200);
    expect(get.mock.calls.length).toBe(n + 1);
    await wait(1000);
    expect(get.mock.calls.length).toBe(n + 2);
  });

  it('"Try now" during a Retry-After says when the next check is, counting down, and the answer clears it', async () => {
    let t = NOW;
    /** Moves the page's clock with fake time. */
    const step = async (ms: number) => {
      t += ms;
      await wait(ms);
    };
    let answer: 'session' | 'refuse' | 'fail' = 'session';
    const { root, get } = await mount(() => {
      if (answer === 'refuse') throw new ApiError(429, 'rate_limited', 'Too many requests.', 90);
      if (answer === 'fail') throw new ApiError(0, 'network_error', 'The connection failed.');
      return session();
    }, { now: () => t, pollMs: 3000 });
    answer = 'refuse';
    await step(3000);
    const retry = () => root.querySelector<HTMLButtonElement>('[data-fid="offline-retry"]')!;
    expect(root.querySelector('[data-fid="offline"]')).not.toBeNull();
    expect(root.querySelector('[data-fid="retry-wait"]')).toBeNull();

    // The press loads nothing (it would only be refused again), but says so.
    const n = get.mock.calls.length;
    retry().click();
    await wait(0);
    expect(get.mock.calls.length).toBe(n);
    const line = root.querySelector('[data-fid="retry-wait"]')!;
    expect(line.textContent).toBe('Prismaport asked us to wait. Next check in 1:30.');
    expect(line.closest('[role="status"]')).not.toBeNull();
    // Inside the status region only the count is not live: it would be read out every second.
    expect(line.querySelector('[data-next-check]')!.getAttribute('aria-live')).toBe('off');
    await step(1000);
    expect(root.querySelector('[data-next-check]')!.textContent).toBe('Next check in 1:29.');
    await step(60_000);
    expect(root.querySelector('[data-next-check]')!.textContent).toBe('Next check in 0:29.');

    // The wait is over and the check answers: the notice goes.
    answer = 'session';
    await step(29_000);
    expect(get.mock.calls.length).toBe(n + 1);
    expect(root.querySelector('[data-fid="offline"]')).toBeNull();

    // Without a Retry-After, "Try now" loads at once and says nothing about waiting.
    answer = 'fail';
    await step(3000);
    retry().click();
    await wait(0);
    expect(get.mock.calls.length).toBe(n + 3);
    expect(root.querySelector('[data-fid="retry-wait"]')).toBeNull();
  });

  it('"Next check in" counts to the load that really runs: a failure backoff later than the Retry-After wins', async () => {
    let t = NOW;
    const step = async (ms: number) => {
      t += ms;
      await wait(ms);
    };
    let refuse = false;
    // A slow poll: after a failure the backoff (30 s) outlasts the 5 s Retry-After.
    const { root, get } = await mount(() => {
      if (refuse) throw new ApiError(429, 'rate_limited', 'Too many requests.', 5);
      return session();
    }, { now: () => t, pollMs: 20_000 });
    refuse = true;
    await step(20_000);
    const n = get.mock.calls.length;
    root.querySelector<HTMLButtonElement>('[data-fid="offline-retry"]')!.click();
    await wait(0);
    expect(root.querySelector('[data-next-check]')!.textContent).toBe('Next check in 0:30.');
    // Past the Retry-After the line stays: the next load is still to come.
    await step(6000);
    expect(root.querySelector('[data-next-check]')!.textContent).toBe('Next check in 0:24.');
    await step(23_000);
    expect(root.querySelector('[data-next-check]')!.textContent).toBe('Next check in 0:01.');
    expect(get.mock.calls.length).toBe(n);
    refuse = false;
    await step(1000);
    expect(get.mock.calls.length).toBe(n + 1);
    expect(root.querySelector('[data-fid="offline"]')).toBeNull();
  });

  it('at the lock no poll is scheduled, so "Try now" during a Retry-After promises no next check', async () => {
    let refuse = false;
    const { root, get } = await mount(() => {
      if (refuse) throw new ApiError(429, 'rate_limited', 'Too many requests.', 60);
      return closed();
    });
    expect(root.querySelector('[data-fid="lock-panel"]')).not.toBeNull();
    refuse = true;
    document.dispatchEvent(new Event('visibilitychange'));
    await wait(0);
    expect(root.querySelector('[data-fid="offline"]')).not.toBeNull();
    const n = get.mock.calls.length;
    root.querySelector<HTMLButtonElement>('[data-fid="offline-retry"]')!.click();
    await wait(1000);
    expect(get.mock.calls.length).toBe(n);
    expect(root.querySelector('[data-fid="retry-wait"]')).toBeNull();
  });

  it('a Retry-After longer than five minutes waits the five minutes, not more', async () => {
    expect(MAX_RETRY_AFTER_MS).toBe(300_000);
    const { get } = await mount(() => {
      throw new ApiError(429, 'rate_limited', 'Too many requests.', 3600);
    });
    const n = get.mock.calls.length;
    await wait(MAX_RETRY_AFTER_MS - 1000);
    expect(get.mock.calls.length).toBe(n);
    await wait(2000);
    expect(get.mock.calls.length).toBe(n + 1);
  });
});
