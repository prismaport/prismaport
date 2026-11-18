import { screen, waitFor, within } from '@testing-library/react';
import { http, type RequestHandler } from 'msw';
import { beforeEach, describe, expect, it } from 'vitest';
import { MOCK_CODE, MOCK_TOTP } from '../src/mocks/db';
import type { Mode } from '../src/lib/types';
import { emailCode, renderApp, useMockServer } from './helpers';

useMockServer();

// The tab's hint that step 1 was passed lives in sessionStorage, which outlives a test.
beforeEach(() => sessionStorage.clear());

const PASSWORD = 'A long new password 1';
const FOUNDER = '0xD544632988655aa58d0C15b925970526e93658A5';
const SPARE = '0x1111111111111111111111111111111111aBcDeF';

/** A brand-new account, signed in, on the given page and mode: nothing in either mode. */
function newAccount(path: string, mode: Mode, opts: { twofa?: boolean; handlers?: RequestHandler[]; window?: boolean } = {}) {
  const app = renderApp(path, { signedIn: false, mode, handlers: opts.handlers, window: opts.window });
  const acc = app.mock.addSignup('Starter', 'starter@example.com', PASSWORD, true);
  if (opts.twofa) acc.users[0]!.totp_enabled = true;
  app.mock.signIn('starter@example.com');
  return app;
}

const csrf = { 'Content-Type': 'application/json', 'X-CSRF-Token': 'csrf-test-token' };

/** A product in a mode, through the API: a sample wallet in Test mode, so the product has somewhere to pay. */
async function seedTestProduct() {
  const w = (await (await fetch('/v1/dashboard/wallets/sample?mode=test', { method: 'POST', headers: csrf, body: JSON.stringify({ family: 'evm' }) })).json()) as { id: string };
  const res = await fetch('/v1/dashboard/products?mode=test', {
    method: 'POST',
    headers: { ...csrf, 'Idempotency-Key': 'seed-test-product' },
    body: JSON.stringify({ name: 'Seed', price_type: 'fixed', amount: '25.00', payout: { 'base-sepolia': [{ wallet_id: w.id, share: '100.00' }] } }),
  });
  expect(res.ok).toBe(true);
}

/** Step 1: picks the mode to start in and continues. */
async function startIn(user: ReturnType<typeof renderApp>['user'], mode: Mode) {
  const start = within(await screen.findByTestId('step-start'));
  await user.click(start.getByRole('radio', { name: mode === 'live' ? /Live/ : /Test/ }));
  await user.click(start.getByRole('button', { name: mode === 'live' ? 'Continue in Live mode' : 'Continue in Test mode' }));
}

const nav = () => within(screen.getByRole('navigation', { name: 'Main' }));

async function addBaseWallet(user: ReturnType<typeof renderApp>['user'], name: string, address: string, opts: { confirm?: (scope: ReturnType<typeof within>) => Promise<void> } = {}) {
  const card = within(await screen.findByTestId('network-evm'));
  await user.type(card.getByLabelText('Name'), name);
  await user.type(card.getByLabelText('Base wallet address'), address);
  await user.type(card.getByRole('textbox', { name: 'Confirm the last 6 characters' }), address.slice(-6));
  if (opts.confirm) await opts.confirm(card);
  await user.click(card.getByRole('button', { name: 'Add wallet' }));
}

describe('Get started in the menu', () => {
  it('shows while the mode on screen has no product, with its own steps for the other mode', async () => {
    const { user, client } = newAccount('/', 'live');
    await screen.findByRole('heading', { level: 1, name: 'Get started' });
    const item = nav().getByRole('link', { name: /Get started/ });
    expect(within(item).getByTestId('get-started-count')).toHaveTextContent('0/3');
    expect(screen.getByTestId('gs-steps')).toHaveTextContent('Choose where to start');

    // A product in Test mode ends Get started there; Live, still empty, keeps it with steps 2 and 3 only.
    await seedTestProduct();
    await client.invalidateQueries();
    await user.click(screen.getAllByRole('radio', { name: 'Test' })[0]!);
    await waitFor(() => expect(nav().queryByRole('link', { name: /Get started/ })).toBeNull());
    await user.click(screen.getAllByRole('radio', { name: 'Live' })[0]!);
    expect(await nav().findByRole('link', { name: /Get started/ })).toBeInTheDocument();
    await user.click(nav().getByRole('link', { name: /Get started/ }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Set up Live mode' })).toBeInTheDocument();
    expect(screen.getByTestId('gs-steps')).not.toHaveTextContent('Choose where to start');
    expect(within(nav().getByRole('link', { name: /Get started/ })).getByTestId('get-started-count')).toHaveTextContent('0/2');
  });

  it('is not shown, and its page goes home, once the mode has a product', async () => {
    renderApp('/get-started');
    await screen.findByRole('heading', { level: 1 });
    expect(nav().queryByRole('link', { name: /Get started/ })).toBeNull();
    await waitFor(() => expect(screen.queryByRole('heading', { level: 1, name: 'Get started' })).toBeNull());
  });
});

describe('resuming Get started', () => {
  it('opens at the step the server names after leaving and coming back, and counts from the server', async () => {
    const { user } = newAccount('/get-started', 'live');
    await startIn(user, 'test');
    await user.click(within(await screen.findByTestId('network-evm')).getByRole('button', { name: 'Use a sample test wallet' }));
    await screen.findByTestId('added-wallets');
    await waitFor(() => expect(within(nav().getByRole('link', { name: /Get started/ })).getByTestId('get-started-count')).toHaveTextContent('2/3'));

    // Away to another page and back: the wallet is there, so it opens on the product step.
    await user.click(nav().getByRole('link', { name: 'Overview' }));
    await screen.findByRole('heading', { level: 1, name: 'Overview' });
    await user.click(nav().getByRole('link', { name: /Get started/ }));
    expect(await screen.findByTestId('step-product')).toBeInTheDocument();
  });

  it('comes back to the networks step after Continue with no wallet yet', async () => {
    const { user } = newAccount('/get-started', 'live');
    await startIn(user, 'test');
    await screen.findByTestId('network-evm');
    await user.click(nav().getByRole('link', { name: 'Overview' }));
    await screen.findByRole('heading', { level: 1, name: 'Overview' });
    await user.click(nav().getByRole('link', { name: /Get started/ }));
    expect(await screen.findByTestId('network-evm')).toBeInTheDocument();
    expect(within(nav().getByRole('link', { name: /Get started/ })).getByTestId('get-started-count')).toHaveTextContent('1/3');
  });

  it('stays on the networks step it resumed at when a wallet is added', async () => {
    const { user } = newAccount('/get-started', 'live');
    await startIn(user, 'test');
    await screen.findByTestId('network-evm');
    await user.click(nav().getByRole('link', { name: 'Overview' }));
    await screen.findByRole('heading', { level: 1, name: 'Overview' });
    await user.click(nav().getByRole('link', { name: /Get started/ }));
    await user.click(within(await screen.findByTestId('network-evm')).getByRole('button', { name: 'Use a sample test wallet' }));
    await screen.findByTestId('added-wallets');
    // The server now says "product"; the page moves on only when Continue is pressed.
    await waitFor(() => expect(within(nav().getByRole('link', { name: /Get started/ })).getByTestId('get-started-count')).toHaveTextContent('2/3'));
    expect(screen.getByTestId('step-networks')).toBeInTheDocument();
    expect(screen.queryByTestId('step-product')).toBeNull();
  });

  it('opens on step 1 when the server says choose and nothing was started in this tab', async () => {
    newAccount('/get-started', 'live');
    expect(await screen.findByTestId('step-start')).toBeInTheDocument();
  });

  it('opens on step 1 for another account signing in on a tab where someone else pressed Continue', async () => {
    sessionStorage.setItem('prismaport.dashboard.started', 'usr_SOMEONE_ELSE');
    newAccount('/get-started', 'live');
    expect(await screen.findByTestId('step-start')).toBeInTheDocument();
  });

  it('is gone in a mode once its product exists', async () => {
    const { user } = newAccount('/get-started', 'test');
    await screen.findByTestId('step-start');
    await seedTestProduct();
    await user.click(screen.getAllByRole('radio', { name: 'Live' })[0]!);
    await user.click(screen.getAllByRole('radio', { name: 'Test' })[0]!);
    await waitFor(() => expect(nav().queryByRole('link', { name: /Get started/ })).toBeNull());
    await waitFor(() => expect(screen.queryByTestId('step-start')).toBeNull());
  });
});

describe('step 1: where to start', () => {
  it('starts on Live by default and Test is optional; the switch at the top stays free', async () => {
    const { user } = newAccount('/get-started', 'live');
    const start = within(await screen.findByTestId('step-start'));
    expect(start.getByRole('radio', { name: /Live/ })).toBeChecked();
    expect(start.getByRole('radio', { name: /Test/ })).not.toBeChecked();
    await user.click(start.getByRole('radio', { name: /Test/ }));
    await user.click(start.getByRole('button', { name: 'Continue in Test mode' }));
    expect(await screen.findByTestId('test-strip')).toBeInTheDocument();
    expect(within(await screen.findByTestId('network-evm')).getByRole('heading', { name: /Base/ })).toBeInTheDocument();
    expect(screen.getByTestId('gs-steps')).toHaveTextContent('Networks and wallet');
  });
});

describe('step 2: networks and wallets in Live mode', () => {
  it('adds several wallets on a network, the first is the default and any can be made the default', async () => {
    const bodies: Record<string, unknown>[] = [];
    const { user, mock } = newAccount('/get-started', 'live', {
      window: true,
      handlers: [
        http.post('/v1/dashboard/wallets', async ({ request }) => {
          bodies.push((await request.clone().json()) as Record<string, unknown>);
          return undefined;
        }),
      ],
    });
    await user.click(await screen.findByRole('button', { name: 'Continue in Live mode' }));
    const base = await screen.findByTestId('network-evm');
    expect(within(base).getByRole('checkbox', { name: /USDC on Base/ })).toBeChecked();
    // Solana is enabled in the mock, so it offers its own wallet; it is not open until asked for.
    expect(within(screen.getByTestId('network-solana')).getByRole('button', { name: 'Add a Solana wallet' })).toBeInTheDocument();

    // Locked: Add wallet opens Unlock (D61); once unlocked, the wallet goes without a code of its own.
    await addBaseWallet(user, 'Founder', FOUNDER, { confirm: async (card) => expect(card.queryByTestId('step-up')).toBeNull() });
    const unlock = within(await screen.findByRole('dialog', { name: 'Unlock Live changes for 10 minutes' }));
    expect(bodies).toEqual([]);
    await emailCode(user, unlock);
    await user.click(unlock.getByRole('button', { name: 'Unlock for 10 minutes' }));
    expect(await screen.findByText('Founder added')).toBeInTheDocument();
    expect(bodies).toEqual([{ label: 'Founder', family: 'evm', address: FOUNDER }]);
    const list = within(await screen.findByTestId('added-wallets'));
    expect(list.getAllByTestId('added-wallet')).toHaveLength(1);
    expect(list.getByText('Default')).toBeInTheDocument();
    expect(list.getByText('New products pay here')).toBeInTheDocument();

    // A second one while unlocked: nothing asked.
    await user.click(within(screen.getByTestId('network-evm')).getByRole('button', { name: 'Add another Base wallet' }));
    await addBaseWallet(user, 'Spare', SPARE);
    expect(screen.queryByRole('dialog', { name: 'Unlock Live changes for 10 minutes' })).toBeNull();
    await waitFor(() => expect(within(screen.getByTestId('added-wallets')).getAllByTestId('added-wallet')).toHaveLength(2));
    expect(bodies[1]).toEqual({ label: 'Spare', family: 'evm', address: SPARE });
    const spare = within(screen.getByTestId('added-wallets')).getAllByTestId('added-wallet')[1]!;
    expect(within(spare).queryByText('Default')).toBeNull();
    await user.click(within(spare).getByRole('button', { name: 'Make Spare the default wallet' }));
    // The default comes first in the list, as everywhere wallets are listed.
    await waitFor(() => expect(within(within(screen.getByTestId('added-wallets')).getAllByTestId('added-wallet')[0]!).getByText('Spare')).toBeInTheDocument());
    expect(within(within(screen.getByTestId('added-wallets')).getAllByTestId('added-wallet')[0]!).getByText('Default')).toBeInTheDocument();
    const stored = mock.db().wallets.filter((w) => w.livemode && w.family === 'evm');
    expect(stored.find((w) => w.label === 'Spare')!.default).toBe(true);
    expect(stored.find((w) => w.label === 'Founder')!.default).toBeFalsy();
  });

  it('asks for the emailed code before the wallet is sent, and shows a wrong one in its field', async () => {
    const { user } = newAccount('/get-started', 'live');
    await user.click(await screen.findByRole('button', { name: 'Continue in Live mode' }));
    const base = within(await screen.findByTestId('network-evm'));
    expect(base.getByText(/6-digit code we email to s•••@example\.com/)).toBeInTheDocument();
    expect(base.queryByLabelText('Password')).toBeNull();
    await addBaseWallet(user, 'Founder', FOUNDER);
    expect(await base.findByText('Ask for a code first, then enter it here.')).toBeInTheDocument();
    await emailCode(user, base, '000000');
    await user.click(base.getByRole('button', { name: 'Add wallet' }));
    await waitFor(() => expect(base.getByLabelText('Code from your email')).toHaveAccessibleDescription(/That code is not valid or has expired/));
    await user.clear(base.getByLabelText('Code from your email'));
    await user.type(base.getByLabelText('Code from your email'), MOCK_CODE);
    await user.click(base.getByRole('button', { name: 'Add wallet' }));
    expect(await screen.findByText('Founder added')).toBeInTheDocument();
  });

  it('lets an account with 2FA choose the authenticator app or an emailed code', async () => {
    const { user } = newAccount('/get-started', 'live', { twofa: true });
    await user.click(await screen.findByRole('button', { name: 'Continue in Live mode' }));
    const base = within(await screen.findByTestId('network-evm'));
    expect(base.getByRole('radio', { name: /Authenticator app/ })).toBeChecked();
    expect(base.getByRole('radio', { name: /Code by email/ })).not.toBeChecked();
    expect(base.queryByRole('button', { name: 'Email me a code' })).toBeNull();
    await user.click(base.getByRole('radio', { name: /Code by email/ }));
    expect(base.getByRole('button', { name: 'Email me a code' })).toBeInTheDocument();
    await user.click(base.getByRole('radio', { name: /Authenticator app/ }));
    await addBaseWallet(user, 'Founder', FOUNDER, { confirm: async (card) => void (await user.type(card.getByLabelText('Code from your authenticator app'), MOCK_TOTP)) });
    expect(await screen.findByText('Founder added')).toBeInTheDocument();
  });
});

describe('Test mode', () => {
  it('asks for no confirmation: the sample wallet and a wallet of your own', async () => {
    const { user, mock } = newAccount('/get-started', 'live');
    await startIn(user, 'test');
    const base = within(await screen.findByTestId('network-evm'));
    expect(base.queryByTestId('step-up')).toBeNull();
    expect(base.queryByRole('button', { name: 'Email me a code' })).toBeNull();
    await user.click(base.getByRole('button', { name: 'Use a sample test wallet' }));
    expect(await screen.findByText(/Sample test wallet/)).toBeInTheDocument();
    await user.click(await base.findByRole('button', { name: 'Add another Base wallet' }));
    await addBaseWallet(user, 'Mine', FOUNDER);
    await waitFor(() => expect(within(screen.getByTestId('added-wallets')).getAllByTestId('added-wallet')).toHaveLength(2));
    expect(mock.db().wallets.filter((w) => !w.livemode)).toHaveLength(2);
  });
});

describe('step 3 and You are ready', () => {
  it('creates the first product with its link, paying the default wallet, then says it is ready and leaves the menu', async () => {
    const { user, mock } = newAccount('/get-started', 'live');
    await startIn(user, 'test');
    await user.click(within(await screen.findByTestId('network-evm')).getByRole('button', { name: 'Use a sample test wallet' }));
    await screen.findByTestId('added-wallets');
    await user.click(screen.getByRole('button', { name: 'Continue' }));

    const step = within(await screen.findByTestId('step-product'));
    expect(step.getByRole('heading', { name: 'Create your first product' })).toBeInTheDocument();
    // The same recap as Products, Add product: the default wallet of each network at 100%.
    const recap = within(await step.findByTestId('payout-recap'));
    await waitFor(() => expect(recap.getAllByTestId('payout-recap-line').map((l) => l.textContent)).toEqual(['Base Sepolia: 100% Sample test wallet (demo, nobody holds its key)']));
    expect(screen.getByTestId('link-preview')).toBeInTheDocument();

    await user.click(step.getByRole('button', { name: 'Create product' }));
    expect(step.getByLabelText('Name')).toHaveAccessibleDescription(/Give the product a name/);
    await user.type(step.getByLabelText('Name'), 'Starter kit');
    await user.type(step.getByLabelText('Price'), '25');
    expect(within(screen.getByTestId('link-preview')).getByText('Starter kit')).toBeInTheDocument();
    expect(step.getByRole('checkbox', { name: 'Also create a payment link' })).toBeChecked();
    await user.click(step.getByRole('button', { name: 'Create product' }));

    const ready = within(await screen.findByTestId('step-ready'));
    expect(ready.getByRole('heading', { name: "You're ready to test" })).toBeInTheDocument();
    expect(ready.getByText(/Starter kit · 25\.00 USD is ready in Test mode\./)).toBeInTheDocument();
    expect(ready.getByRole('link', { name: 'Open the payment page' }).getAttribute('href')).toMatch(/\/l\/plink_/);
    expect(mock.db().products[0]!.payout).toEqual({ 'base-sepolia': [{ wallet_id: mock.db().wallets[0]!.id, bps: 10_000 }], 'solana-devnet': [] });
    expect(mock.db().links).toHaveLength(1);
    await waitFor(() => expect(nav().queryByRole('link', { name: /Get started/ })).toBeNull());
    // The ready screen stays until the person moves on.
    expect(screen.getByTestId('step-ready')).toBeInTheDocument();
  });

  it('says You are ready for Live with the link to share', async () => {
    const { user } = newAccount('/get-started', 'live');
    await user.click(await screen.findByRole('button', { name: 'Continue in Live mode' }));
    await addBaseWallet(user, 'Founder', FOUNDER, { confirm: (card) => emailCode(user, card) });
    await screen.findByTestId('added-wallets');
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    const step = within(await screen.findByTestId('step-product'));
    await user.type(await step.findByLabelText('Name'), 'Founder plan');
    await user.type(step.getByLabelText('Price'), '25');
    await user.click(step.getByRole('button', { name: 'Create product' }));
    const ready = within(await screen.findByTestId('step-ready'));
    expect(ready.getByRole('heading', { name: "You're ready" })).toBeInTheDocument();
    expect(ready.getByText(/Share this link with whoever pays you/)).toBeInTheDocument();
    expect(ready.getByText(/Want to try a payment without real money\? Switch to Test at the top\./)).toBeInTheDocument();
  });

  it('keeps Continue on step 2 until a wallet exists', async () => {
    const { user } = newAccount('/get-started', 'live');
    await startIn(user, 'test');
    await screen.findByTestId('network-evm');
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    expect(await screen.findByText('Add a wallet first, so payments have somewhere to go.')).toBeInTheDocument();
  });
});

describe('step 2: a family with no chain (D62)', () => {
  it('shows Coming soon, with no wallet button, for a family the server serves no stablecoin on', async () => {
    const { user, mock } = newAccount('/get-started', 'test');
    mock.db().accepted.test = mock.db().accepted.test.filter((a) => !a.chain.startsWith('solana'));
    await startIn(user, 'test');
    const card = within(await screen.findByTestId('network-solana-off'));
    expect(card.getByText('Coming soon')).toBeInTheDocument();
    expect(card.getByText(/USDC and USDT on Solana are on the way/)).toBeInTheDocument();
    expect(card.queryByRole('button')).toBeNull();
    expect(screen.queryByTestId('network-solana')).toBeNull();
    expect(within(await screen.findByTestId('network-evm')).getByRole('heading', { name: /Base/ })).toBeInTheDocument();
  });
});
