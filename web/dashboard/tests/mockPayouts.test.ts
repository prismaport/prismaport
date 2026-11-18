import { setupServer } from 'msw/node';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { ApiError, setCsrfToken } from '../src/lib/api';
import { api } from '../src/lib/endpoints';
import { payoutUnits } from '../src/lib/splits';
import type { ProductInput } from '../src/lib/types';
import { MOCK_RECOVERY_CODES, MOCK_TOTP, units } from '../src/mocks/db';
import { createMockApi, TEST_CSRF } from '../src/mocks/handlers';

// The mock API is what dev, the artifact preview and Playwright run against: it must answer
// wallets and product payouts the way the server defines them.
const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

let mock: ReturnType<typeof createMockApi>;
beforeEach(async () => {
  mock = createMockApi();
  mock.signIn();
  setCsrfToken(TEST_CSRF);
  server.use(...mock.handlers);
  // An emailed code works only for the action it was sent for, so both wallet actions get one.
  await api.sendConfirmation('live', 'wallet_create');
  await api.sendConfirmation('live', 'wallet_archive');
});

// The confirmation of a Live action: the code the mock emails.
const PW = { email_code: '246810' };
let seq = 0;
const product = (body: Partial<ProductInput> | Record<string, unknown>) =>
  api.createProduct('live', { name: 'Payout test', price_type: 'fixed', amount: '10.00', ...body } as ProductInput, `key-${++seq}`);
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

async function walletIds() {
  const { data } = await api.wallets('live');
  return {
    id: (label: string, family = 'evm') => data.find((w) => w.label === label && w.family === family && !w.archived_at)!.id,
    archived: data.find((w) => w.archived_at)!.id,
  };
}

describe('mock wallets', () => {
  it('seeds three Base wallets and two Solana wallets, and products that pay them in order', async () => {
    const { data: wallets } = await api.wallets('live');
    const active = wallets.filter((w) => !w.archived_at);
    expect(active.filter((w) => w.family === 'evm').map((w) => w.label)).toEqual(['Founder', 'Partner', 'Reserve']);
    expect(active.filter((w) => w.family === 'solana').map((w) => w.label)).toEqual(['Treasury', 'Partner']);
    expect(wallets.filter((w) => w.archived_at).map((w) => w.label)).toEqual(['Old hardware wallet']);
    const founderPlan = (await api.products('live', { q: 'Founder plan' })).data[0]!;
    expect(Object.keys(founderPlan.payout!)).toEqual(['base', 'solana']);
    expect(founderPlan.payout!.base!.map((i) => [i.label, i.share])).toEqual([['Founder', '70.00'], ['Partner', '30.00']]);
    expect(founderPlan.payout!.solana!.map((i) => [i.label, i.share])).toEqual([['Treasury', '80.00'], ['Partner', '20.00']]);
    expect(founderPlan.payout!.base![0]!.address).toBe(wallets.find((w) => w.label === 'Founder')!.address);
    // Test mode products are keyed by the test networks, every one of them, [] when not taken.
    const donation = (await api.products('test', { q: 'Test donation' })).data[0]!;
    expect(donation.payout).toEqual({ 'base-sepolia': [expect.objectContaining({ label: 'Founder', share: '100.00' })], 'solana-devnet': [] });
  });

  it('creates a wallet only after the address checks and the step-up', async () => {
    const usdc = await refusal(api.createWallet('live', { label: 'Oops', family: 'evm', address: '0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913', email_code: '000000' }));
    expect(usdc.message).toBe('This is the USDC contract, not a wallet. Paste your wallet address instead.');
    const dup = await refusal(api.createWallet('live', { label: 'Again', family: 'evm', address: '0x7a3f5c2b9e81d4a6f0c3e2b7a915d8c4f06be91c', ...PW }));
    expect(dup.message).toBe('This address is already your wallet Founder.');
    const pw = await refusal(api.createWallet('live', { label: 'Ops', family: 'evm', address: '0x1111111111111111111111111111111111111111', email_code: '000000' }));
    expect([pw.status, pw.code]).toEqual([403, 'invalid_code']);
    const none = await refusal(api.createWallet('live', { label: 'Ops', family: 'evm', address: '0x1111111111111111111111111111111111111111' }));
    expect([none.status, none.code]).toEqual([403, 'confirmation_required']);
    const w = await api.createWallet('live', { label: 'Ops', family: 'evm', address: '0x1111111111111111111111111111111111111111', ...PW });
    expect(w).toMatchObject({ label: 'Ops', family: 'evm', archived_at: null, verified: true, used_by_products: 0 });
    expect(w.id).toMatch(/^wal_/);
  });

  it('asks for the code when 2FA is on', async () => {
    mock.signIn('secure@acme.studio');
    const body = { label: 'Ops', family: 'solana' as const, address: '5yNmRkS3ZzK7h8QyJ1v2Xc4Bn6Mq9Wd3Tf5Gp8Lr2Ae1' };
    const e = await refusal(api.createWallet('live', body));
    expect([e.status, e.code]).toEqual([403, 'confirmation_required']);
    const wrong = await refusal(api.createWallet('live', { ...body, totp: '999999' }));
    expect([wrong.status, wrong.code]).toEqual([403, 'invalid_code']);
    await expect(api.createWallet('live', { ...body, totp: '123456' })).resolves.toMatchObject({ label: 'Ops' });
  });

  it('asks Test mode for no confirmation, the sample wallet included', async () => {
    await expect(api.createWallet('test', { label: 'Ops', family: 'evm', address: '0x1111111111111111111111111111111111111111' })).resolves.toMatchObject({ label: 'Ops' });
    await expect(api.sampleWallet('test', { family: 'evm' })).resolves.toMatchObject({ family: 'evm' });
  });

  it('emails a code at most once a minute, and only for Live actions', async () => {
    const sent = await api.sendConfirmation('live', 'api_key');
    expect(sent).toEqual({ sent_to: 'o•••@acme.studio', expires_in: 900, resend_after: 60 });
    const again = await refusal(api.sendConfirmation('live', 'api_key'));
    expect([again.status, again.code]).toEqual([429, 'rate_limited']);
    expect(again.retryAfter).toBeGreaterThan(0);
    await expect(api.sendConfirmation('test', 'webhook_endpoint')).rejects.toMatchObject({ status: 400 });
  });

  it('marks the oldest wallet of a network as the default until another one is made default', async () => {
    const list = async () => (await api.wallets('live')).data.filter((w) => w.family === 'evm' && !w.archived_at);
    const before = await list();
    expect(before.filter((w) => w.default)).toHaveLength(1);
    const other = before.find((w) => !w.default)!;
    await api.makeDefaultWallet('live', other.id);
    const after = await list();
    expect(after.filter((w) => w.default).map((w) => w.id)).toEqual([other.id]);
  });

  it('adding a wallet changes no product', async () => {
    const before = mock.db().products.map((p) => JSON.stringify(p.payout));
    await api.createWallet('live', { label: 'Spare', family: 'evm', address: '0x2222222222222222222222222222222222222222', ...PW });
    expect(mock.db().products.map((p) => JSON.stringify(p.payout))).toEqual(before);
  });

  it('refuses to archive a wallet a product pays, saying how many, and archives one no product pays', async () => {
    const { id } = await walletIds();
    const e = await refusal(api.archiveWallet('live', id('Partner', 'solana'), PW));
    expect([e.status, e.code]).toEqual([409, 'wallet_in_use']);
    expect(e.message).toBe('This wallet still receives payments from 2 products. Change those products first.');
    // An inactive product still counts: it can be turned back on.
    productNamed('Black Friday bundle').active = false;
    expect((await refusal(api.archiveWallet('live', id('Reserve'), PW))).message).toBe('This wallet still receives payments from 1 product. Change that product first.');
    const fresh = await api.createWallet('live', { label: 'Spare', family: 'evm', address: '0x2222222222222222222222222222222222222222', ...PW });
    const archived = await api.archiveWallet('live', fresh.id, PW);
    expect(archived.archived_at).not.toBeNull();
  });

  it('lists archived wallets only when asked, with how many products pay each', async () => {
    const res = await fetch('/v1/dashboard/wallets?mode=live');
    const active = ((await res.json()) as { data: { archived_at: string | null }[] }).data;
    expect(active.some((w) => w.archived_at)).toBe(false);
    const all = (await api.wallets('live')).data;
    expect(all.find((w) => w.label === 'Old hardware wallet')!.archived_at).not.toBeNull();
    expect(all.find((w) => w.label === 'Founder' && w.family === 'evm')!.used_by_products).toBe(9);
    expect(all.find((w) => w.label === 'Partner' && w.family === 'evm')!.used_by_products).toBe(3);
    expect(all.find((w) => w.label === 'Old hardware wallet')!.used_by_products).toBe(0);
  });

  it('reports no destinations or default splits in the settings', async () => {
    const settings = (await api.settings('live')) as unknown as Record<string, unknown>;
    expect(Object.keys(settings).sort()).toEqual(['accepted_assets', 'fee', 'product_networks']);
  });
});

describe('mock product payouts', () => {
  it('validates like the server: distinct active wallets of the network family, 1 to 5, shares summing to 100.00, known networks, at least one', async () => {
    const { id, archived } = await walletIds();
    const bad = async (payout: unknown) => (await refusal(product({ payout }))).message;
    expect(await bad({ base: [{ wallet_id: id('Founder'), share: '60.00' }, { wallet_id: id('Partner'), share: '30.00' }] })).toBe('Shares must add up to 100% on Base. Now 90%.');
    expect(await bad({ base: [{ wallet_id: id('Founder'), share: '50.00' }, { wallet_id: id('Founder'), share: '50.00' }] })).toBe('Each wallet can appear only once in a split. Founder appears twice on Base.');
    expect(await bad({ base: [{ wallet_id: archived, share: '100.00' }] })).toBe(`Wallet ${archived} is not an active EVM wallet of this account, so it cannot receive on Base.`);
    expect(await bad({ solana: [{ wallet_id: id('Founder'), share: '100.00' }] })).toBe(`Wallet ${id('Founder')} is not an active Solana wallet of this account, so it cannot receive on Solana.`);
    expect(await bad({ base: [{ wallet_id: id('Treasury', 'solana'), share: '100.00' }] })).toBe(`Wallet ${id('Treasury', 'solana')} is not an active EVM wallet of this account, so it cannot receive on Base.`);
    expect(await bad({ base: [{ wallet_id: id('Founder'), share: '0' }] })).toMatch(/^payout\.base\[0\]\.share must be a percentage/);
    expect(await bad({ base: Array.from({ length: 6 }, () => ({ wallet_id: id('Founder'), share: '10.00' })) })).toBe('A split pays 1 to 5 wallets. Base has 6.');
    expect(await bad({ base: [], solana: [] })).toBe('A product must take payments on at least one network. Give one of them at least one wallet.');
    const unknown = await refusal(product({ payout: { 'base-sepolia': [] } }));
    expect([unknown.status, unknown.code, unknown.message]).toEqual([400, 'unknown_chain', 'payout.base-sepolia is not a network of this mode. Use base or solana.']);
  });

  it('creates with the oldest wallet of the family on a network left out, keeps the order given, and patches network by network without a step-up', async () => {
    const { id } = await walletIds();
    const made = await product({ payout: { base: [{ wallet_id: id('Reserve'), share: '33.34' }, { wallet_id: id('Founder'), share: '66.66' }] } });
    expect(made.payout!.base!.map((i) => [i.label, i.share])).toEqual([['Reserve', '33.34'], ['Founder', '66.66']]);
    expect(made.payout!.solana!.map((i) => [i.label, i.share])).toEqual([['Treasury', '100.00']]);
    expect((await product({})).payout).toMatchObject({ base: [{ label: 'Founder', share: '100.00' }], solana: [{ label: 'Treasury', share: '100.00' }] });
    const baseOnly = await api.updateProduct('live', made.id, { payout: { solana: [] } });
    expect(baseOnly.payout!.solana).toEqual([]);
    expect(baseOnly.payout!.base!.map((i) => i.label)).toEqual(['Reserve', 'Founder']);
    expect((await refusal(api.updateProduct('live', made.id, { payout: { base: [] } }))).message).toBe('A product must take payments on at least one network. Give one of them at least one wallet.');
    expect((await api.wallets('live')).data.find((w) => w.id === id('Reserve'))!.used_by_products).toBe(2);
  });

  it('settles seeded payments to several wallets, and fee plus payouts is always the gross', async () => {
    const sessions = mock.db().sessions.filter((s) => s.livemode);
    const settled = sessions.flatMap((s) => s.settlements ?? (s.settlement ? [s.settlement] : []));
    expect(settled.length).toBeGreaterThan(10);
    expect(settled.some((x) => (x.payouts ?? []).length > 1)).toBe(true);
    for (const x of settled) {
      const sum = (x.payouts ?? []).reduce((a, p) => a + units(p.amount), 0n);
      expect(units(x.fee) + sum, x.tx_hash).toBe(units(x.gross));
      expect(sum).toBe(units(x.net));
    }
    const s = sessions.find((x) => x.product_id === productNamed('Founder plan').id && x.settlement?.chain === 'base')!;
    expect(s.settlement!.payouts!.map((p) => p.label)).toEqual(['Founder', 'Partner']);
    expect(s.settlement!.payouts!.map((p) => units(p.amount))).toEqual(payoutUnits(units(s.settlement!.net), [7000, 3000]));
  });

  it('groups what each wallet was paid in the overview', async () => {
    const o = await api.overview('live');
    expect(o.totals_by_wallet!.length).toBeGreaterThan(3);
    const labels = new Set(o.totals_by_wallet!.map((t) => t.label));
    for (const l of ['Founder', 'Partner', 'Reserve', 'Treasury']) expect(labels).toContain(l);
    const net = o.totals.reduce((a, t) => a + units(t.net), 0n);
    expect(o.totals_by_wallet!.reduce((a, t) => a + units(t.amount), 0n)).toBe(net);
  });

  it("opens a payment from a link with the recipients of its product's payout, and a simulated payment settles to them", async () => {
    const p = productNamed('Test product');
    const res = await fetch(`/v1/checkout/links/${linkOf('Test product').id}/sessions`, { method: 'POST', headers: { 'Idempotency-Key': 'pay-1', 'Content-Type': 'application/json' }, body: '{}' });
    const opened = (await res.json()) as { id: string };
    const stored = mock.db().sessions.find((x) => x.id === opened.id)!;
    expect(stored.payout!['base-sepolia']!.map((r) => [r.wallet_id, r.bps])).toEqual(p.payout['base-sepolia']!.map((i) => [i.wallet_id, 10_000]));
    await api.simulate('test', opened.id, { action: 'pay_exact' });
    const detail = await api.session('test', opened.id);
    expect(detail.settlement).toMatchObject({ gross: '1.000000', fee: '0.105000', net: '0.895000', simulated: true, payouts: [{ label: 'Founder', amount: '0.895000' }] });
    expect(detail.explorer_links!.filter((x) => x.type === 'settlement')).toEqual([]);
  });

  it("offers payers only the networks a link's product pays", async () => {
    const res = await fetch(`/v1/checkout/links/${linkOf('Test donation').id}/sessions`, { method: 'POST', headers: { 'Idempotency-Key': 'pay-2', 'Content-Type': 'application/json' }, body: JSON.stringify({ amount: '5.00' }) });
    const opened = (await res.json()) as { pay_to: { chain: string }[] };
    expect(opened.pay_to.map((p) => p.chain)).toEqual(['base-sepolia']);
  });
});

// The confirmation contract of the server (D58), as the mock answers it.
describe('mock confirmations', () => {
  const WALLET = { label: 'Ops', family: 'evm' as const, address: '0x1111111111111111111111111111111111111111' };

  it('lists the methods in error.details.methods when no code is sent', async () => {
    const e = await refusal(api.createWallet('live', WALLET));
    expect([e.status, e.code, e.details]).toEqual([403, 'confirmation_required', { methods: ['email'] }]);
    mock.signIn('secure@acme.studio');
    const both = await refusal(api.createWallet('live', WALLET));
    expect(both.details).toEqual({ methods: ['email', 'totp'] });
  });

  it('refuses an emailed code sent for another action, and both codes at once', async () => {
    const other = await refusal(api.createApiKey({ name: 'Server', scopes: ['sessions:read'], mode: 'live', ...PW }));
    expect([other.status, other.code]).toEqual([403, 'invalid_code']);
    mock.signIn('secure@acme.studio');
    await api.sendConfirmation('live', 'wallet_create');
    const twice = await refusal(api.createWallet('live', { ...WALLET, ...PW, totp: MOCK_TOTP }));
    expect([twice.status, twice.code]).toEqual([400, 'invalid_request']);
  });

  it('without email on the server: 503 for a code, and the authenticator only', async () => {
    const noEmail = createMockApi({ email: false });
    noEmail.signIn('secure@acme.studio');
    server.use(...noEmail.handlers);
    const e = await refusal(api.sendConfirmation('live', 'wallet_create'));
    expect([e.status, e.code]).toEqual([503, 'service_unavailable']);
    expect((await refusal(api.createWallet('live', WALLET))).details).toEqual({ methods: ['totp'] });
    await expect(api.createWallet('live', { ...WALLET, totp: MOCK_TOTP })).resolves.toMatchObject({ label: 'Ops' });
  });

  it('2FA: turning on takes the emailed code; replace, off and new codes take a code or a recovery code, not both', async () => {
    const on = await refusal(api.totpEnroll({}));
    expect([on.code, on.details]).toEqual(['confirmation_required', { methods: ['email'] }]);
    await api.sendConfirmation('live', 'totp_enroll');
    await expect(api.totpEnroll({ email_code: PW.email_code })).resolves.toHaveProperty('secret');

    mock.signIn('secure@acme.studio');
    expect((await refusal(api.sendConfirmation('live', 'totp_enroll'))).status).toBe(400);
    expect((await refusal(api.totpEnroll({ email_code: PW.email_code }))).code).toBe('totp_required');
    expect((await refusal(api.totpRecoveryCodes({}))).code).toBe('totp_required');
    expect((await refusal(api.totpRecoveryCodes({ code: MOCK_TOTP, recovery_code: MOCK_RECOVERY_CODES[0]! }))).status).toBe(400);
    await expect(api.totpRecoveryCodes({ recovery_code: MOCK_RECOVERY_CODES[0]! })).resolves.toHaveProperty('recovery_codes');
    expect((await refusal(api.totpDisable({ code: '000000' }))).code).toBe('totp_required');
    await expect(api.totpDisable({ code: MOCK_TOTP })).resolves.toBeTruthy();
  });
});
