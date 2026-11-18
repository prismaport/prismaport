import { setupServer } from 'msw/node';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { ApiError, setCsrfToken } from '../src/lib/api';
import { api } from '../src/lib/endpoints';
import { createMockApi, TEST_CSRF } from '../src/mocks/handlers';

// The mock is the contract the dashboard is built against until the backend lands: products and
// payment links as the server defines them.
const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

let mock: ReturnType<typeof createMockApi>;
beforeEach(() => {
  mock = createMockApi();
  mock.signIn();
  setCsrfToken(TEST_CSRF);
  server.use(...mock.handlers);
});

let seq = 0;
const key = () => `catalog-${++seq}`;
const productNamed = (name: string) => mock.db().products.find((p) => p.name === name)!;
const linkOf = (name: string) => mock.db().links.find((l) => l.product_id === productNamed(name).id && l.name === null)!;

async function refusal(p: Promise<unknown>): Promise<ApiError> {
  try {
    await p;
  } catch (e) {
    if (e instanceof ApiError) return e;
    throw e;
  }
  throw new Error('expected a refusal');
}

const open = (id: string, body: Record<string, unknown> = {}) =>
  fetch(`/v1/checkout/links/${id}/sessions`, { method: 'POST', headers: { 'Idempotency-Key': key(), 'Content-Type': 'application/json' }, body: JSON.stringify(body) });

describe('mock products', () => {
  it('lists products with their price, description, links count and stats, active first', async () => {
    const { data } = await api.products('live', { status: 'all' });
    expect(data.at(-1)!.active).toBe(false);
    const founder = data.find((p) => p.name === 'Founder plan')!;
    expect(founder).toMatchObject({ price_type: 'fixed', amount: '25.00', description: 'Lifetime access to every Acme Studio template, with updates.', links: { live: 2, total: 2 } });
    expect(founder.id).toMatch(/^prod_/);
    // Product stats sum every session of the product, links and API alike.
    const orders = data.find((p) => p.name === 'Custom order')!;
    expect(orders.links).toEqual({ live: 0, total: 0 });
    expect(orders.stats.opened).toBe(2);
    expect((await api.products('live', { status: 'inactive' })).data.map((p) => p.name)).toEqual(['Spring zine']);
  });

  it('creates a product without a URL or availability, and refuses a price type change', async () => {
    const made = await api.createProduct('live', { name: 'Zine', description: '  A small zine.  ', price_type: 'open', min_amount: '5.00', max_amount: '50.00' }, key());
    expect(made).toMatchObject({ name: 'Zine', description: 'A small zine.', price_type: 'open', active: true, links: { live: 0, total: 0 } });
    expect(made).not.toHaveProperty('url');
    expect(made).not.toHaveProperty('expires_at');
    const e = await refusal(api.updateProduct('live', made.id, { price_type: 'fixed' } as never));
    expect([e.status, e.param]).toEqual([400, 'price_type']);
    const long = await refusal(api.createProduct('live', { name: 'x', price_type: 'fixed', amount: '5.00', description: 'a'.repeat(501) }, key()));
    expect(long.param).toBe('description');
  });

  it('asks what happens to the active links on a money edit, and only then', async () => {
    const acme = productNamed('Pay Acme Studio');
    const active = mock.db().links.filter((l) => l.product_id === acme.id && l.active);
    expect(active).toHaveLength(2);
    const e = await refusal(api.updateProduct('live', acme.id, { min_amount: '130.00', suggested_amounts: ['150.00'] }));
    expect([e.status, e.code, e.param, e.message]).toEqual([409, 'links_decision_required', 'links', 'This product has 2 active links. Choose whether to keep them active or deactivate them.']);
    expect(productNamed('Pay Acme Studio').min_amount).toBe('5.00');
    // Not money, or the same values again: no question.
    await expect(api.updateProduct('live', acme.id, { name: 'Pay Acme Studio', suggested_amounts: ['20.00'], metadata: { a: 'b' } })).resolves.toMatchObject({ suggested_amounts: ['20.00'] });
    await expect(api.updateProduct('live', acme.id, { min_amount: '5.00', max_amount: '500.00' })).resolves.toMatchObject({ min_amount: '5.00' });
    const bad = await refusal(api.updateProduct('live', acme.id, { name: 'X', links: 'maybe' as never }));
    expect([bad.status, bad.param]).toEqual([400, 'links']);
  });

  it('keeps the links, a set amount included even outside the new range', async () => {
    const acme = productNamed('Pay Acme Studio');
    const invoice = mock.db().links.find((l) => l.name === 'Invoice ACME #12')!;
    await expect(api.updateProduct('live', acme.id, { min_amount: '130.00', suggested_amounts: ['150.00'], links: 'keep' })).resolves.toMatchObject({ min_amount: '130.00', links: { live: 2 } });
    expect(await api.link('live', invoice.id)).toMatchObject({ live: true, amount: '120.00' });
    // Renaming the kept link is not a new amount: the range is not checked again.
    await expect(api.updateLink('live', invoice.id, { name: 'Invoice ACME #12b' })).resolves.toMatchObject({ amount: '120.00' });
    const opened = (await (await open(invoice.id)).json()) as { amount: string };
    expect(opened.amount).toBe('120.00');
    expect((await refusal(api.updateLink('live', invoice.id, { amount: '120.00' }))).code).toBe('amount_below_minimum');
  });

  it('deactivates every active link with the edit, and counts the checkouts still open', async () => {
    const acme = productNamed('Pay Acme Studio');
    const before = await api.product('live', acme.id);
    const open = mock.db().sessions.filter((x) => x.product_id === acme.id && (x.status === 'open' || x.status === 'partially_paid')).length;
    expect(before.stats.open).toBe(open);
    const after = await api.updateProduct('live', acme.id, { max_amount: '400.00', links: 'deactivate' });
    expect(after).toMatchObject({ max_amount: '400.00', links: { live: 0, total: 2 } });
    expect(after.stats.open).toBe(open);
    expect((await api.productLinks('live', acme.id)).data.map((l) => [l.active, l.unavailable_reason])).toEqual([
      [false, 'inactive'],
      [false, 'inactive'],
    ]);
    // With no active link left, nothing asks.
    await expect(api.updateProduct('live', acme.id, { max_amount: '450.00' })).resolves.toMatchObject({ max_amount: '450.00' });
  });

  it('keeps active links within the metadata limits, and checks them again when one is turned on', async () => {
    const founder = productNamed('Founder plan');
    const keys = (n: number) => Object.fromEntries(Array.from({ length: n }, (_, i) => [`k${i}`, 'v']));
    // The Instagram link adds one key of its own.
    const e = await refusal(api.updateProduct('live', founder.id, { metadata: keys(20) }));
    expect([e.status, e.param]).toEqual([400, 'metadata']);
    const insta = mock.db().links.find((l) => l.name === 'Instagram bio')!;
    await api.updateLink('live', insta.id, { active: false });
    await api.updateLink('live', linkOf('Founder plan').id, { active: false });
    await expect(api.updateProduct('live', founder.id, { metadata: keys(20) })).resolves.toMatchObject({ links: { live: 0 } });
    const back = await refusal(api.updateLink('live', insta.id, { active: true }));
    expect([back.status, back.param]).toEqual([400, 'metadata']);
    await expect(api.updateLink('live', linkOf('Founder plan').id, { active: true })).resolves.toMatchObject({ live: true });
  });

  it('deactivating a product stops its links, and activating it brings them back', async () => {
    const founder = productNamed('Founder plan');
    await api.updateProduct('live', founder.id, { active: false });
    const links = (await api.productLinks('live', founder.id)).data;
    expect(links.map((l) => [l.live, l.unavailable_reason])).toEqual([
      [false, 'product_inactive'],
      [false, 'product_inactive'],
    ]);
    expect((await open(linkOf('Founder plan').id)).status).toBe(410);
    await api.updateProduct('live', founder.id, { active: true });
    expect((await api.link('live', linkOf('Founder plan').id)).live).toBe(true);
  });
});

describe('mock payment links', () => {
  it('creates a link with no options from just the product, with its product embedded', async () => {
    const tip = productNamed('Tip jar');
    const l = await api.createLink('live', { product_id: tip.id }, key());
    expect(l.id).toMatch(/^plink_/);
    expect(l).toMatchObject({ product_id: tip.id, name: null, amount: null, live: true, url: `https://prismaport.xyz/l/${l.id}`, product: { id: tip.id, name: 'Tip jar', price_type: 'open', min_amount: '1.00', active: true } });
    expect(l).not.toHaveProperty('payout');
    expect(l).not.toHaveProperty('kind');
    expect((await api.product('live', tip.id)).links).toEqual({ live: 2, total: 2 });
  });

  it('refuses an amount on a fixed product, outside the range, or on an unknown product', async () => {
    const fixed = await refusal(api.createLink('live', { product_id: productNamed('Founder plan').id, amount: '10.00' }, key()));
    expect([fixed.status, fixed.code, fixed.param]).toEqual([400, 'invalid_request', 'amount']);
    const acme = productNamed('Pay Acme Studio').id;
    expect((await refusal(api.createLink('live', { product_id: acme, amount: '1.00' }, key()))).code).toBe('amount_below_minimum');
    expect((await refusal(api.createLink('live', { product_id: acme, amount: '900.00' }, key()))).code).toBe('amount_above_maximum');
    const other = await refusal(api.createLink('live', { product_id: productNamed('Test product').id }, key()));
    expect([other.status, other.code]).toEqual([404, 'product_not_found']);
    const crowded = Object.fromEntries(Array.from({ length: 19 }, (_, i) => [`k${i}`, 'v']));
    expect((await refusal(api.createLink('live', { product_id: productNamed('Founder plan').id, metadata: crowded }, key()))).param).toBe('metadata');
  });

  it('edits a link with nulls that clear, never its product', async () => {
    const invoice = mock.db().links.find((l) => l.name === 'Invoice ACME #12')!;
    const cleared = await api.updateLink('live', invoice.id, { name: null, amount: null, max_payments: null });
    expect(cleared).toMatchObject({ name: null, amount: null, max_payments: null });
    const e = await refusal(api.updateLink('live', invoice.id, { product_id: productNamed('Tip jar').id } as never));
    expect([e.status, e.param]).toEqual([400, 'product_id']);
  });

  it('searches links by label, product name or exact id, and filters by product', async () => {
    expect((await api.links('live', { q: 'instagram' })).data.map((l) => l.name)).toEqual(['Instagram bio']);
    expect((await api.links('live', { q: 'founder plan' })).data).toHaveLength(2);
    const id = linkOf('Tip jar').id;
    expect((await api.links('live', { q: id })).data.map((l) => l.id)).toEqual([id]);
    const acme = productNamed('Pay Acme Studio').id;
    expect((await api.links('live', { product_id: acme })).data.every((l) => l.product_id === acme)).toBe(true);
  });

  it('answers the public link at its effective price and opens sessions that snapshot it', async () => {
    const invoice = mock.db().links.find((l) => l.name === 'Invoice ACME #12')!;
    const pub = (await (await fetch(`/v1/checkout/links/${invoice.id}`)).json()) as Record<string, unknown>;
    expect(pub).toMatchObject({ name: 'Pay Acme Studio', price_type: 'fixed', amount: '120.00', min_amount: null, live: true });
    expect(pub).not.toHaveProperty('metadata');
    expect(JSON.stringify(pub)).not.toContain('Invoice ACME');
    expect((await open(invoice.id, { amount: '50.00' })).status).toBe(400);
    const opened = (await (await open(invoice.id)).json()) as { id: string; amount: string };
    expect(opened.amount).toBe('120.00');
    // The limit of 1 counts this link's own sessions only.
    expect((await api.link('live', invoice.id)).unavailable_reason).toBe('reserved');
    expect((await api.link('live', linkOf('Pay Acme Studio').id)).live).toBe(true);
    const insta = mock.db().links.find((l) => l.name === 'Instagram bio')!;
    const s = (await (await open(insta.id)).json()) as { id: string };
    const stored = mock.db().sessions.find((x) => x.id === s.id)!;
    expect(stored).toMatchObject({ link_id: insta.id, product_id: insta.product_id, metadata: { plan: 'founder', sku: 'FND-001', channel: 'instagram' } });
    const listed = (await api.sessions('live', { link_id: insta.id })).data;
    expect(listed.map((x) => [x.link_name, x.product_name])).toEqual([['Instagram bio', 'Founder plan']]);
    expect((await fetch(`/v1/checkout/links/${productNamed('Founder plan').id}`)).status).toBe(404);
  });

  it('flags a link whose product was deactivated while sessions are still open', async () => {
    const tip = productNamed('Tip jar');
    await open(linkOf('Tip jar').id, { amount: '2.00' });
    await api.updateProduct('live', tip.id, { active: false });
    const { links_attention } = await api.overview('live');
    expect(links_attention).toContainEqual({ id: linkOf('Tip jar').id, name: 'Tip jar', reason: 'product_inactive' });
  });
});
