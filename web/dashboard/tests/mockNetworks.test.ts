import { setupServer } from 'msw/node';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { ApiError, setCsrfToken } from '../src/lib/api';
import { api } from '../src/lib/endpoints';
import type { ProductInput } from '../src/lib/types';
import { createMockApi, TEST_CSRF } from '../src/mocks/handlers';

// The mock mirrors the server of D60: effective = Settings ∩ the product's choice, and the same refusals.
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
const create = (body: Record<string, unknown>) => api.createProduct('live', { name: 'Choice test', price_type: 'fixed', amount: '10.00', ...body } as ProductInput, `net-${++seq}`);
const named = (name: string) => mock.db().products.find((p) => p.livemode && p.name === name)!;

async function refusal(p: Promise<unknown>): Promise<ApiError> {
  try {
    await p;
  } catch (e) {
    if (e instanceof ApiError) return e;
    throw e;
  }
  throw new Error('expected a refusal');
}

describe('mock products carry networks and payment methods', () => {
  it('a seeded product takes every network it pays with All', async () => {
    const p = await api.product('live', named('Founder plan').id);
    expect(p.networks).toEqual([
      { chain: 'base', chain_name: 'Base', accept: 'all' },
      { chain: 'solana', chain_name: 'Solana', accept: 'all' },
    ]);
    expect(p.payment_methods!.map((m) => `${m.asset}/${m.chain}`)).toEqual(['USDC/base', 'USDC/solana', 'USDT/solana']);
  });

  it('Some offers only the ticked ones that are on in Settings, and a stablecoin turned off comes back when turned on', async () => {
    const p = await create({ networks: { base: { accept: 'all' }, solana: { accept: 'some', assets: ['USDT'] } } });
    expect(p.networks![1]).toEqual({ chain: 'solana', chain_name: 'Solana', accept: 'some', assets: ['USDT'] });
    expect(p.payment_methods!.map((m) => `${m.asset}/${m.chain}`)).toEqual(['USDC/base', 'USDT/solana']);
    mock.db().accepted.live.find((a) => a.asset === 'USDT')!.enabled = false;
    expect((await api.product('live', p.id)).payment_methods!.map((m) => m.asset)).toEqual(['USDC']);
    expect((await api.product('live', p.id)).networks![1]!.assets).toEqual(['USDT']);
    mock.db().accepted.live.find((a) => a.asset === 'USDT')!.enabled = true;
    expect((await api.product('live', p.id)).payment_methods!.map((m) => m.asset)).toEqual(['USDC', 'USDT']);
  });

  it('none drops the network and its payout, a stablecoin-only change never asks about links, dropping a network does', async () => {
    const founder = named('Founder plan');
    const some = await api.updateProduct('live', founder.id, { networks: { solana: { accept: 'some', assets: ['USDC'] } } });
    expect(some.networks![1]!.assets).toEqual(['USDC']);
    const none = await api.updateProduct('live', founder.id, { networks: { solana: { accept: 'none' } }, links: 'keep' });
    expect(none.networks!.map((n) => n.chain)).toEqual(['base']);
    expect(none.payout!.solana).toEqual([]);
    // Taking it again, with no payout sent, uses the family's default wallet.
    const back = await api.updateProduct('live', founder.id, { networks: { solana: { accept: 'all' } }, links: 'keep' });
    expect(back.payout!.solana).toHaveLength(1);
  });

  it('lists the networks of active products in the settings', async () => {
    const s = await api.settings('live');
    const mine = s.product_networks!.filter((n) => n.product_id === named('Founder plan').id);
    expect(mine.map((n) => [n.chain, n.accept])).toEqual([['base', 'all'], ['solana', 'all']]);
    expect(s.product_networks!.every((n) => mock.db().products.find((p) => p.id === n.product_id)!.active)).toBe(true);
  });
});

describe('mock refusals follow the hand-off', () => {
  const cases: [string, Record<string, unknown>, string, string][] = [
    ['assets without some', { networks: { base: { accept: 'all', assets: ['USDC'] } } }, 'networks.base.assets', 'networks.base.assets goes only with accept some'],
    ['some without assets', { networks: { solana: { accept: 'some' } } }, 'networks.solana.assets', 'networks.solana.assets must list at least one stablecoin with accept some'],
    ['a duplicate', { networks: { solana: { accept: 'some', assets: ['USDT', 'USDT'] } } }, 'networks.solana.assets', 'networks.solana.assets lists USDT twice'],
    ['an asset the chain does not offer', { networks: { base: { accept: 'some', assets: ['USDT'] } } }, 'networks.base.assets', 'networks.base.assets: USDT is not offered on base in live mode; use USDC'],
    ['a bad accept', { networks: { base: { accept: 'most' } } }, 'networks.base.accept', 'networks.base.accept must be all, some or none'],
    ['an unknown field', { networks: { base: { accept: 'all', x: 1 } } }, 'networks.base.x', 'networks.base has an unknown field "x"'],
    ['none with a payout', { networks: { solana: { accept: 'none' } }, payout: { solana: [{ wallet_id: 'x', share: '100.00' }] } }, 'payout.solana', 'payout.solana names wallets but networks.solana.accept is none: send [] or leave payout.solana out'],
    ['all with an empty payout', { networks: { solana: { accept: 'all' } }, payout: { solana: [] } }, 'networks.solana.accept', 'networks.solana.accept is all but payout.solana is []: a network the product takes needs a payout'],
  ];
  it.each(cases)('refuses %s', async (_n, body, param, message) => {
    const e = await refusal(create(body));
    expect([e.status, e.param, e.message]).toEqual([400, param, message]);
  });

  it('refuses a network with no wallet of its family', async () => {
    for (const w of mock.db().wallets) if (w.livemode && w.family === 'solana') w.archived_at = '2026-01-01T00:00:00Z';
    const e = await refusal(create({ networks: { solana: { accept: 'all' } } }));
    expect([e.param, e.message]).toEqual(['networks.solana', 'Add a Solana wallet before taking payments on Solana, or send payout.solana.']);
  });

  it('refuses a product left with no network', async () => {
    const e = await refusal(create({ networks: { base: { accept: 'none' }, solana: { accept: 'none' } } }));
    expect(e.param).toBe('payout');
  });
});
