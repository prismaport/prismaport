import { setupServer } from 'msw/node';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { ApiError, setCsrfToken } from '../src/lib/api';
import { api } from '../src/lib/endpoints';
import { createMockApi, TEST_CSRF } from '../src/mocks/handlers';

// GET /onboarding as the server answers it: the mock answers the
// contract field for field, since the dashboard decides what to show from it.
const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

function start(opts: { sendFails?: boolean } = {}) {
  const mock = createMockApi(opts);
  mock.addSignup('Starter', 'starter@example.com', 'A long new password 1', true);
  mock.signIn('starter@example.com');
  setCsrfToken(TEST_CSRF);
  server.use(...mock.handlers);
}
beforeEach(() => start());

describe('mock GET /onboarding', () => {
  it('answers a brand-new account with the first-run steps at choose', async () => {
    expect(await api.onboarding('live')).toEqual({
      visible: true,
      first_run: true,
      has_wallet: false,
      has_product: false,
      networks: [
        { chain: 'base', family: 'evm', has_wallet: false },
        { chain: 'solana', family: 'solana', has_wallet: false },
      ],
      steps: ['choose', 'wallets', 'product'],
      step: 'choose',
      done: 0,
    });
  });

  it('moves to product once a wallet exists in the mode, and is no longer a first choose with a wallet in the other mode', async () => {
    await api.sampleWallet('test', { family: 'evm' });
    expect(await api.onboarding('test')).toMatchObject({ has_wallet: true, step: 'product', done: 2, networks: [
      { chain: 'base-sepolia', family: 'evm', has_wallet: true },
      { chain: 'solana-devnet', family: 'solana', has_wallet: false },
    ] });
    expect(await api.onboarding('live')).toMatchObject({ has_wallet: false, step: 'wallets', first_run: true, done: 1 });
  });

  it('is done and not visible once the mode has a product, and the other mode then lists two steps', async () => {
    const w = await api.sampleWallet('test', { family: 'evm' });
    await api.createProduct('test', { name: 'Seed', price_type: 'fixed', amount: '25.00', payout: { 'base-sepolia': [{ wallet_id: w.id, share: '100.00' }] } } as never, 'seed');
    expect(await api.onboarding('test')).toMatchObject({ visible: false, has_product: true, first_run: false, steps: ['wallets', 'product'], step: 'done', done: 2 });
    expect(await api.onboarding('live')).toMatchObject({ visible: true, has_product: false, first_run: false, steps: ['wallets', 'product'], step: 'wallets', done: 0 });
  });

  it('needs a session', async () => {
    setCsrfToken(null);
    const mock = createMockApi();
    server.use(...mock.handlers);
    const e = await api.onboarding('live').catch((x: unknown) => x);
    expect(e).toBeInstanceOf(ApiError);
    expect((e as ApiError).status).toBe(401);
  });
});

describe('mock POST /confirmations with a mail server that refuses', () => {
  it('answers 503 email_send_failed and leaves the next try free', async () => {
    start({ sendFails: true });
    for (let i = 0; i < 2; i++) {
      const e = await api.sendConfirmation('live', 'wallet_create').catch((x: unknown) => x);
      expect(e).toMatchObject({ status: 503, code: 'email_send_failed' });
    }
  });
});
