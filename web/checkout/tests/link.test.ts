import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, CheckoutApi, type PublicLink } from '../src/lib/api';
import { COPY } from '../src/lib/copy';
import { mountLink, rangeHint, validateOpenAmount, type LinkDeps } from '../src/pages/link';
import { CLIENT_REFERENCE, clientReferenceFromSearch } from '../src/lib/reference';
import { session } from './fixtures';

const open: PublicLink = {
  id: 'plink_01JBCOFFEE', livemode: true, name: 'Coffee', price_type: 'open', amount: null, min_amount: '5.00', max_amount: '100.00',
  suggested_amounts: ['5.00', '10.00', '25.00'], currency: 'USD', active: true, merchant: null,
};
const fixed: PublicLink = { ...open, id: 'plink_01JBFOUNDER', name: 'Founder', price_type: 'fixed', amount: '25.00', min_amount: null, max_amount: null, suggested_amounts: [] };

function setup(product: PublicLink | ApiError, clientReference?: string) {
  const api = new CheckoutApi('', vi.fn() as never);
  vi.spyOn(api, 'link').mockImplementation(async () => {
    if (product instanceof ApiError) throw product;
    return { data: product, serverTime: null };
  });
  const openSession = vi.spyOn(api, 'openSession').mockResolvedValue({ data: session({ id: 'ses_01JBNEW' }), serverTime: null });
  const navigate = vi.fn();
  let n = 0;
  const newKey = () => `key-${++n}`;
  const root = document.createElement('main');
  document.body.replaceChildren(root);
  const linkId = product instanceof ApiError ? 'plink_x' : product.id;
  return { api, openSession, navigate, root, mount: (more: Partial<LinkDeps> = {}) => mountLink(root, linkId, { api, navigate, newKey, clientReference, retryMs: [], ...more }) };
}

const flush = () => new Promise((r) => setTimeout(r, 0));

const down = new ApiError(0, 'network_error', 'The connection failed.');

/** The accessible name of the card: the text of the element its aria-labelledby points at, exactly one. */
function cardName(root: HTMLElement): string | null {
  const id = root.querySelector('section.card')!.getAttribute('aria-labelledby');
  const named = id ? root.querySelectorAll(`[id="${id}"]`) : [];
  return named.length === 1 ? named[0]!.textContent : null;
}

describe('open amount validation', () => {
  it('accepts amounts within the bounds, inclusive', () => {
    expect(validateOpenAmount('5', open)).toEqual({ value: '5.00' });
    expect(validateOpenAmount('100.00', open)).toEqual({ value: '100.00' });
    expect(validateOpenAmount('12.5', open)).toEqual({ value: '12.50' });
  });

  it('explains amounts outside the bounds', () => {
    expect(validateOpenAmount('4.99', open)).toEqual({ error: 'Enter at least 5.00 USD.' });
    expect(validateOpenAmount('100.01', open)).toEqual({ error: 'Enter at most 100.00 USD.' });
    expect(validateOpenAmount('5.0001', open)).toEqual({ error: 'Use at most two decimals, such as 10.50.' });
    expect(validateOpenAmount('', open)).toEqual({ error: 'Enter an amount.' });
    expect(validateOpenAmount('ten', open)).toEqual({ error: 'Enter an amount in USD, such as 10.00.' });
  });

  it('accepts a comma decimal, never a mixed one or three digits after a lone separator', () => {
    expect(validateOpenAmount('10,50', open)).toEqual({ value: '10.50' });
    expect(validateOpenAmount('1.000,50', open)).toEqual({ error: 'Enter an amount in USD, such as 10.00.' });
    for (const typed of ['1,000', '10,505', '1.000', '10.505', '99.999', '.500']) {
      expect(validateOpenAmount(typed, open)).toEqual({ error: 'Enter an amount in USD, such as 10.00.' });
    }
    expect(validateOpenAmount('10,5055', open)).toEqual({ error: 'Use at most two decimals, such as 10.50.' });
  });

  it('describes the range', () => {
    expect(rangeHint(open)).toBe('Between 5.00 and 100.00 USD.');
    expect(rangeHint({ min_amount: '1.00', max_amount: null })).toBe('At least 1.00 USD.');
  });
});

describe('payment link page', () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.useRealTimers());

  it('fixed product: shows the price and opens a session without an amount', async () => {
    const t = setup(fixed);
    await t.mount();
    expect(t.root.querySelector('h1')!.textContent).toBe('Founder');
    expect(t.root.textContent).toContain('25.00 USD');
    expect(t.root.querySelector('input')).toBeNull();
    t.root.querySelector('form')!.requestSubmit();
    await flush();
    expect(t.openSession).toHaveBeenCalledWith('plink_01JBFOUNDER', 'key-1', undefined, undefined);
    expect(t.navigate).toHaveBeenCalledWith('/pay/ses_01JBNEW');
  });

  it('open product: suggested amounts fill the field, the session gets the exact amount', async () => {
    const t = setup(open);
    await t.mount();
    const chips = [...t.root.querySelectorAll<HTMLButtonElement>('.chip')];
    expect(chips.map((c) => c.textContent)).toEqual(['5.00 USD', '10.00 USD', '25.00 USD']);
    chips[1]!.click();
    expect(t.root.querySelector('input')!.value).toBe('10.00');
    expect(chips[1]!.getAttribute('aria-pressed')).toBe('true');
    t.root.querySelector('form')!.requestSubmit();
    await flush();
    expect(t.openSession).toHaveBeenCalledWith('plink_01JBCOFFEE', 'key-1', '10.00', undefined);
  });

  it('open product: an amount out of bounds is explained and nothing is opened', async () => {
    const t = setup(open);
    await t.mount();
    const input = t.root.querySelector('input')!;
    input.value = '2';
    t.root.querySelector('form')!.requestSubmit();
    await flush();
    expect(t.openSession).not.toHaveBeenCalled();
    const err = t.root.querySelector<HTMLElement>('#amount-error')!;
    expect(err.hidden).toBe(false);
    expect(err.textContent).toBe('Enter at least 5.00 USD.');
    expect(input.getAttribute('aria-invalid')).toBe('true');
  });

  it('open product: a comma decimal is accepted, the hint says what is paid and the session gets the dot form', async () => {
    const t = setup({ ...open, min_amount: null, max_amount: null });
    await t.mount();
    const input = t.root.querySelector('input')!;
    const hint = t.root.querySelector('#amount-hint')!;
    const type = (v: string) => {
      input.value = v;
      input.dispatchEvent(new Event('input'));
    };
    expect(hint.textContent).toBe('Any amount in USD.');
    type('10,50');
    expect(hint.textContent).toBe('Any amount in USD. You pay 10.50 USD.');
    expect(hint.querySelector('.mono')!.textContent).toBe('10.50');
    type('10.50');
    expect(hint.textContent).toBe('Any amount in USD.');
    type('1.000,50');
    expect(hint.textContent).toBe('Any amount in USD.');
    type('1,000');
    expect(hint.textContent).toBe('Any amount in USD.');
    type('10,505');
    expect(hint.textContent).toBe('Any amount in USD.');
    // A dot followed by three digits is no different: "1.000" is one thousand in pt-BR.
    for (const typed of ['1.000', '10.505', '999.999', '.500']) {
      type(typed);
      expect(hint.textContent).toBe('Any amount in USD.');
      t.root.querySelector('form')!.requestSubmit();
      await flush();
      const err = t.root.querySelector<HTMLElement>('#amount-error')!;
      expect(err.hidden).toBe(false);
      expect(err.textContent).toBe('Enter an amount in USD, such as 10.00.');
    }
    type('10,505');
    t.root.querySelector('form')!.requestSubmit();
    await flush();
    expect(t.root.querySelector('#amount-error')!.textContent).toBe('Enter an amount in USD, such as 10.00.');
    expect(t.openSession).not.toHaveBeenCalled();
    type('10,50');
    t.root.querySelector('form')!.requestSubmit();
    await flush();
    expect(t.openSession).toHaveBeenCalledWith('plink_01JBCOFFEE', 'key-1', '10.50', undefined);
    expect(input.value).toBe('10.50');
    expect(hint.textContent).toBe('Any amount in USD.');
  });

  it('open product: an amount out of bounds gets no "You pay" line', async () => {
    const t = setup(open);
    await t.mount();
    const input = t.root.querySelector('input')!;
    input.value = '2,50';
    input.dispatchEvent(new Event('input'));
    expect(t.root.querySelector('#amount-hint')!.textContent).toBe('Between 5.00 and 100.00 USD.');
  });

  it('a retry after a failure reuses the same idempotency key, a new amount gets a new one', async () => {
    const t = setup(open);
    t.openSession.mockRejectedValueOnce(new ApiError(0, 'network_error', 'down'));
    await t.mount();
    const input = t.root.querySelector('input')!;
    const form = t.root.querySelector('form')!;
    input.value = '12.50';
    form.requestSubmit();
    await flush();
    expect(t.root.querySelector('#amount-error')!.textContent).toBe('Cannot reach Prismaport right now. Check your connection and try again.');
    form.requestSubmit();
    await flush();
    input.value = '13';
    form.requestSubmit();
    await flush();
    expect(t.openSession.mock.calls.map((c) => [c[1], c[2]])).toEqual([['key-1', '12.50'], ['key-1', '12.50'], ['key-2', '13.00']]);
  });

  it('rate limited: says when to try again', async () => {
    const t = setup(fixed);
    t.openSession.mockRejectedValueOnce(new ApiError(429, 'rate_limited', 'slow down', 30));
    await t.mount();
    t.root.querySelector('form')!.requestSubmit();
    await flush();
    expect(t.root.textContent).toContain('Too many attempts from this connection. Try again in 30 seconds.');
    expect(t.navigate).not.toHaveBeenCalled();
  });

  it('inactive product: no longer available', async () => {
    const t = setup({ ...fixed, active: false, amount: null });
    await t.mount();
    expect(t.root.textContent).toContain('This link is no longer available.');
    expect(t.root.querySelector('form')).toBeNull();
  });

  it('a link that stops after the page loaded says it is no longer available', async () => {
    const t = setup(open);
    t.openSession.mockRejectedValueOnce(new ApiError(410, 'link_unavailable', 'this link is no longer available'));
    await t.mount();
    t.root.querySelector('input')!.value = '10';
    t.root.querySelector('form')!.requestSubmit();
    await flush();
    expect(t.root.querySelector('h1')!.textContent).toBe('Coffee');
    expect(t.root.textContent).toContain('This link is no longer available.');
    expect(t.root.textContent).not.toContain('Something went wrong');
    expect(t.root.querySelector('form')).toBeNull();
    expect(t.navigate).not.toHaveBeenCalled();
  });

  it('unknown link: not found', async () => {
    const t = setup(new ApiError(404, 'payment_link_not_found', 'no such payment link'));
    await t.mount();
    expect(t.root.querySelector('h1')!.textContent).toBe('Link not found');
    expect(t.root.textContent).toContain('This payment link is not valid.');
    expect(cardName(t.root)).toBe('Link not found');
  });

  it('a failed connection on the first load is retried silently, backing off, keeping the skeleton', async () => {
    vi.useFakeTimers();
    const t = setup(open);
    vi.mocked(t.api.link).mockRejectedValueOnce(down).mockRejectedValueOnce(down);
    // The page's own backoff: a retry after 1 s, the next 2 s later.
    const mounted = t.mount({ retryMs: undefined });
    await vi.advanceTimersByTimeAsync(999);
    expect(t.api.link).toHaveBeenCalledTimes(1);
    expect(t.root.querySelector('.skeleton')!.textContent).toBe('Loading the payment details.');
    expect(t.root.textContent).not.toContain(COPY.offline);
    await vi.advanceTimersByTimeAsync(1);
    expect(t.api.link).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(1999);
    expect(t.api.link).toHaveBeenCalledTimes(2);
    expect(t.root.querySelector('.skeleton')).not.toBeNull();
    await vi.advanceTimersByTimeAsync(1);
    await mounted;
    expect(t.api.link).toHaveBeenCalledTimes(3);
    expect(t.root.querySelector('h1')!.textContent).toBe('Coffee');
    expect(t.root.querySelector('input')).not.toBeNull();
  });

  it('a first load that still fails after three retries offers Try again, which loads again and focuses the title', async () => {
    vi.useFakeTimers();
    const t = setup(open);
    vi.mocked(t.api.link).mockRejectedValueOnce(down).mockRejectedValueOnce(down).mockRejectedValueOnce(down).mockRejectedValueOnce(down);
    const mounted = t.mount({ retryMs: undefined });
    await vi.advanceTimersByTimeAsync(6999);
    expect(t.api.link).toHaveBeenCalledTimes(3);
    expect(t.root.querySelector('.skeleton')).not.toBeNull();
    await vi.advanceTimersByTimeAsync(1);
    await mounted;
    expect(t.api.link).toHaveBeenCalledTimes(4);
    expect(cardName(t.root)).toBe('Payment link');
    expect(t.root.textContent).toContain('We could not load this payment link. Check your connection and try again. Nothing was charged.');
    expect(t.root.textContent).not.toContain('Retrying');
    const again = t.root.querySelector<HTMLButtonElement>('[data-fid="try-again"]')!;
    expect(again.textContent).toBe('Try again');
    expect(again.classList.contains('btn-primary')).toBe(true);
    expect(again.querySelector('svg.icon')).not.toBeNull();
    again.focus();
    again.click();
    expect(t.root.querySelector('.skeleton')!.textContent).toBe('Loading the payment details.');
    await vi.waitFor(() => expect(t.root.querySelector('h1')!.textContent).toBe('Coffee'));
    expect(t.api.link).toHaveBeenCalledTimes(5);
    expect(t.root.querySelector('[data-fid="try-again"]')).toBeNull();
    // The pressed button is gone: focus lands on the page's heading, not on the body.
    const title = t.root.querySelector<HTMLElement>('#title')!;
    expect(document.activeElement).toBe(title);
    expect(title.tabIndex).toBe(-1);
  });

  it('Try again is one attempt at once: when it fails too the card is back without waiting, its button focused', async () => {
    vi.useFakeTimers();
    const t = setup(down);
    const mounted = t.mount({ retryMs: undefined });
    await vi.advanceTimersByTimeAsync(7000);
    await mounted;
    expect(t.api.link).toHaveBeenCalledTimes(4);
    const again = t.root.querySelector<HTMLButtonElement>('[data-fid="try-again"]')!;
    again.focus();
    again.click();
    // No time passes: only the pending promises settle.
    await vi.advanceTimersByTimeAsync(0);
    expect(t.api.link).toHaveBeenCalledTimes(5);
    expect(t.root.querySelector('.skeleton')).toBeNull();
    expect(t.root.textContent).toContain(COPY.linkLoadFailed);
    expect(document.activeElement).toBe(t.root.querySelector('[data-fid="try-again"]'));
    // Nor is anything retried behind the card.
    await vi.advanceTimersByTimeAsync(10_000);
    expect(t.api.link).toHaveBeenCalledTimes(5);
  });

  it('a link that cannot be drawn after Try again says so instead of loading forever', async () => {
    const t = setup(open);
    // An amount that is not a decimal: drawing the price throws.
    vi.mocked(t.api.link).mockRejectedValueOnce(down).mockResolvedValueOnce({ data: { ...fixed, amount: 'twenty' }, serverTime: null });
    await t.mount();
    const again = t.root.querySelector<HTMLButtonElement>('[data-fid="try-again"]')!;
    again.focus();
    again.click();
    await vi.waitFor(() => expect(cardName(t.root)).toBe('Something went wrong'));
    expect(t.api.link).toHaveBeenCalledTimes(2);
    expect(t.root.textContent).toContain(COPY.renderFailed);
    expect(t.root.querySelector('.skeleton')).toBeNull();
    expect(document.activeElement).toBe(t.root.querySelector('#title'));
  });

  it('a server answer on the first load is not retried', async () => {
    const t = setup(new ApiError(503, 'service_unavailable', 'try later'));
    await t.mount({ retryMs: [0, 0, 0] });
    expect(t.api.link).toHaveBeenCalledTimes(1);
    expect(t.root.querySelector('[data-fid="try-again"]')).toBeNull();
  });

  it('an old product URL is no longer available, without asking the server', async () => {
    const t = setup(fixed);
    const root = t.root;
    await mountLink(root, 'prod_01JBOLD', { api: t.api, navigate: t.navigate });
    expect(t.api.link).not.toHaveBeenCalled();
    expect(root.textContent).toContain('This link is no longer available.');
    expect(root.querySelector('form')).toBeNull();
    expect(cardName(root)).toBe('Payment link');
  });

  it('shows the product description under the name', async () => {
    const t = setup({ ...fixed, description: 'Lifetime access to every template.' });
    await t.mount();
    const desc = t.root.querySelector('h1 + .description')!;
    expect(desc.textContent).toBe('Lifetime access to every template.');
    const plain = setup(fixed);
    await plain.mount();
    expect(plain.root.querySelector('.description')).toBeNull();
  });

  it('a link that sets an amount on a variable product is a fixed price for the payer', async () => {
    const t = setup({ ...open, price_type: 'fixed', amount: '120.00', min_amount: null, max_amount: null, suggested_amounts: [] });
    await t.mount();
    expect(t.root.textContent).toContain('120.00 USD');
    expect(t.root.querySelector('input')).toBeNull();
    t.root.querySelector('form')!.requestSubmit();
    await flush();
    expect(t.openSession).toHaveBeenCalledWith('plink_01JBCOFFEE', 'key-1', undefined, undefined);
  });

  it('a link that is not live, its product deactivated, is no longer available', async () => {
    const t = setup({ ...fixed, active: false, live: false, unavailable_reason: 'product_inactive' });
    await t.mount();
    expect(t.root.textContent).toContain('This link is no longer available.');
    expect(t.root.querySelector('form')).toBeNull();
  });

  it('calls the link endpoints', async () => {
    const fetchFn = vi.fn(async () => new Response(JSON.stringify(fixed), { status: 200 }));
    const api = new CheckoutApi('', fetchFn as never);
    await api.link('plink_01JBFOUNDER');
    await api.openSession('plink_01JBFOUNDER', 'key-1');
    expect((fetchFn.mock.calls as unknown as [string, RequestInit][]).map((c) => [c[1].method, c[0]])).toEqual([
      ['GET', '/v1/checkout/links/plink_01JBFOUNDER'],
      ['POST', '/v1/checkout/links/plink_01JBFOUNDER/sessions'],
    ]);
  });

  it('test mode is labelled', async () => {
    const t = setup({ ...fixed, livemode: false });
    await t.mount();
    expect(t.root.textContent).toContain('Test mode.');
  });
});

describe('client reference from the link URL', () => {
  beforeEach(() => vi.restoreAllMocks());

  it('reads ?ref= and its alias, and drops values the server would refuse', () => {
    expect(clientReferenceFromSearch('?ref=order-42')).toBe('order-42');
    expect(clientReferenceFromSearch('?client_reference_id=user:f36361bc')).toBe('user:f36361bc');
    expect(clientReferenceFromSearch('?ref=A.b_c:d-9&utm_source=x')).toBe('A.b_c:d-9');
    expect(clientReferenceFromSearch('?ref=order-42&client_reference_id=order-42')).toBe('order-42');
    expect(clientReferenceFromSearch(`?ref=${'z'.repeat(128)}`)).toBe('z'.repeat(128));
    expect(clientReferenceFromSearch('')).toBeUndefined();
    expect(clientReferenceFromSearch('?ref=')).toBeUndefined();
    expect(clientReferenceFromSearch(`?ref=${'z'.repeat(129)}`)).toBeUndefined();
    expect(clientReferenceFromSearch('?ref=order%2042')).toBeUndefined();
    expect(clientReferenceFromSearch('?ref=a/b')).toBeUndefined();
    expect(clientReferenceFromSearch('?ref=%3Cscript%3E')).toBeUndefined();
    expect(clientReferenceFromSearch('?ref=pedido-%C3%A9')).toBeUndefined();
    expect(clientReferenceFromSearch('?ref=a&ref=b')).toBeUndefined();
    expect(clientReferenceFromSearch('?ref=a&client_reference_id=b')).toBeUndefined();
    expect(CLIENT_REFERENCE.test('order-42')).toBe(true);
  });

  it('sends the reference with the session the payer opens', async () => {
    const t = setup(fixed, 'order-42');
    await t.mount();
    t.root.querySelector('form')!.requestSubmit();
    await flush();
    expect(t.openSession).toHaveBeenCalledWith('plink_01JBFOUNDER', 'key-1', undefined, 'order-42');
    expect(t.navigate).toHaveBeenCalledWith('/pay/ses_01JBNEW');
  });

  it('puts the reference in the request body next to the amount', async () => {
    const fetchFn = vi.fn(async () => new Response(JSON.stringify(session({ id: 'ses_01JBNEW' })), { status: 200 }));
    const api = new CheckoutApi('', fetchFn as never);
    await api.openSession('plink_01JBCOFFEE', 'key-1', '12.50', 'cart:7');
    await api.openSession('plink_01JBCOFFEE', 'key-2');
    const calls = fetchFn.mock.calls as unknown as [string, RequestInit][];
    expect(JSON.parse(calls[0]![1].body as string)).toEqual({ amount: '12.50', client_reference_id: 'cart:7' });
    expect(JSON.parse(calls[1]![1].body as string)).toEqual({});
    expect(new Headers(calls[0]![1].headers).get('Idempotency-Key')).toBe('key-1');
  });
});
