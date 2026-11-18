import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { MOCK_TOTP } from '../src/mocks/db';
import { emailCode, renderApp, useMockServer } from './helpers';

useMockServer();

const logos = (el: Element) => [...el.querySelectorAll('[data-logo]')].map((x) => x.getAttribute('data-logo'));

async function openAdd(opts: Parameters<typeof renderApp>[1] = {}, family = 'Base') {
  const app = renderApp('/settings/networks', opts);
  await app.user.click(await screen.findByRole('button', { name: `Add wallet on ${family}` }));
  const dialog = await screen.findByRole('dialog', { name: 'Add wallet' });
  return { ...app, d: within(dialog), dialog };
}

describe('settings wallets', () => {
  it('opens Settings on the Networks tab, which replaces Wallets and Stablecoins', async () => {
    renderApp('/settings');
    expect(await screen.findByRole('link', { name: 'Networks' })).toHaveAttribute('aria-current', 'page');
    expect(await screen.findByTestId('wallets-evm')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Wallets' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Stablecoins' })).toBeNull();
    expect(screen.getByRole('link', { name: 'Account' })).toBeInTheDocument();
  });

  it.each(['/settings/wallets', '/settings/stablecoins'])('%s still works: it lands on the Networks tab', async (path) => {
    renderApp(path);
    expect(await screen.findByRole('link', { name: 'Networks' })).toHaveAttribute('aria-current', 'page');
    expect(await screen.findByTestId('network-base')).toBeInTheDocument();
  });

  it('lists the wallets of each network with the address, the check and how many products pay them', async () => {
    renderApp('/settings/networks');
    const base = await screen.findByTestId('network-base');
    expect(within(base).getByRole('heading', { name: /^Base/ })).toHaveTextContent('Mainnet');
    expect(logos(base.querySelector('h2')!)).toEqual(['base']);
    expect(within(base).getByText(/^3 of 20 active wallets/)).toBeInTheDocument();
    // The stablecoins of the network sit in the same card.
    expect(within(base).getByRole('checkbox', { name: 'USDC on Base' })).toBeChecked();
    const rows = within(base).getAllByTestId('wallet');
    expect(rows.map((r) => r.querySelector('.wallet-name')!.textContent)).toEqual(['FounderDefault', 'Partner', 'Reserve']);
    const founder = within(rows[0]!);
    expect(founder.getByText('0x7a3F…e91c')).toBeInTheDocument();
    expect(founder.getByText('0x7a3F5c2b9E81d4A6f0C3e2B7a915D8c4F06Be91c')).toHaveClass('sr-only');
    expect(founder.getByRole('button', { name: 'Copy Founder address' })).toBeInTheDocument();
    expect(founder.getByText('Checked on the network')).toBeInTheDocument();
    expect(founder.getByTestId('wallet-usage')).toHaveTextContent('Used by 9 products');
    expect(within(rows[2]!).getByTestId('wallet-usage')).toHaveTextContent('Used by 1 product');
    const solana = screen.getByTestId('network-solana');
    expect(logos(solana.querySelector('h2')!)).toEqual(['solana']);
    expect(within(solana).getAllByTestId('wallet').map((r) => r.querySelector('.wallet-name')!.textContent)).toEqual(['TreasuryDefault', 'Partner']);
  });

  it('says when no product pays a wallet', async () => {
    const { mock } = renderApp('/settings/networks');
    mock.db().wallets.push({ id: 'wal_SPARE', label: 'Spare', family: 'evm', address: '0x2222222222222222222222222222222222222222', verified: true, archived_at: null, created_at: new Date().toISOString(), livemode: true });
    const row = (await screen.findByText('Spare')).closest('li')!;
    expect(within(row).getByTestId('wallet-usage')).toHaveTextContent('Not used by any product');
  });

  it('shows archived wallets only when asked', async () => {
    const { user } = renderApp('/settings/networks');
    const base = await screen.findByTestId('wallets-evm');
    expect(within(base).queryByText('Old hardware wallet')).toBeNull();
    await user.click(screen.getByRole('checkbox', { name: 'Show archived wallets (1)' }));
    const old = within(base).getByText('Old hardware wallet').closest('li')!;
    expect(within(old).getByText('Archived', { selector: '.tag' })).toBeInTheDocument();
    expect(within(old).queryByRole('button', { name: /Archive/ })).toBeNull();
  });

  it('Test mode: the mainnet logo with a Testnet tag, and a wallet not checked yet', async () => {
    renderApp('/settings/networks', { mode: 'test' });
    const base = await screen.findByTestId('network-base-sepolia');
    expect(logos(base.querySelector('h2')!)).toEqual(['base']);
    expect(base.querySelector('h2')).toHaveTextContent('Testnet');
    expect(within(base).getByText('Not checked on the network yet. Real test payouts may wait until it is.')).toBeInTheDocument();
  });

  it('Live mode: an unchecked wallet only says it was not checked', async () => {
    const w = { id: 'wal_1', label: 'Cold', family: 'evm', address: '0x7a3F5c2b9E81d4A6f0C3e2B7a915D8c4F06Be91c', verified: false, archived_at: null,
      created_at: '2026-10-01T00:00:00Z', livemode: true, used_by_products: 0 };
    renderApp('/settings/networks', { handlers: [http.get('/v1/dashboard/wallets', () => HttpResponse.json({ data: [w] }))] });
    const base = await screen.findByTestId('wallets-evm');
    expect(await within(base).findByTestId('wallet-check')).toHaveTextContent(/^Not checked on the network yet$/);
  });
});

describe('add wallet', () => {
  it('checks the name, the address and the last 6 characters before sending', async () => {
    const posts: unknown[] = [];
    const { user, d } = await openAdd({ handlers: [http.post('/v1/dashboard/wallets', async ({ request }) => void posts.push(await request.clone().json()))] });
    expect(d.getByRole('radio', { name: /Base/ })).toBeChecked();
    expect(d.getByLabelText('Name')).toHaveFocus();
    await user.click(d.getByRole('button', { name: 'Add wallet' }));
    expect(d.getByLabelText('Name')).toHaveAccessibleDescription(/Give the wallet a name/);
    expect(d.getByLabelText('Base wallet address')).toHaveAccessibleDescription(/Paste your wallet address\./);
    expect(d.getByLabelText('Name')).toHaveFocus();
    await user.type(d.getByLabelText('Name'), 'Ops');
    await user.type(d.getByLabelText('Base wallet address'), '0x1234');
    await user.click(d.getByRole('button', { name: 'Add wallet' }));
    expect(d.getByLabelText('Base wallet address')).toHaveAccessibleDescription(/This is not a Base wallet address/);
    await user.clear(d.getByLabelText('Base wallet address'));
    await user.type(d.getByLabelText('Base wallet address'), '0x1111111111111111111111111111111111aBcDeF');
    await user.type(d.getByLabelText('Confirm the last 6 characters'), 'abcdee');
    await user.click(d.getByRole('button', { name: 'Add wallet' }));
    expect(d.getByLabelText('Confirm the last 6 characters')).toHaveAccessibleDescription(/These are not the last 6 characters of the address/);
    expect(d.getByLabelText('Confirm the last 6 characters')).toHaveFocus();
    expect(posts).toEqual([]);
  });

  it('shows the server refusal for a token contract and keeps a wrong code in the drawer', async () => {
    const { user, d } = await openAdd();
    await user.type(d.getByLabelText('Name'), 'Oops');
    await user.type(d.getByLabelText('Base wallet address'), '0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913');
    await user.type(d.getByLabelText('Confirm the last 6 characters'), 'a02913');
    await emailCode(user, d, '000000');
    await user.click(d.getByRole('button', { name: 'Add wallet' }));
    expect(await d.findByRole('alert')).toHaveTextContent('This is the USDC contract, not a wallet. Paste your wallet address instead.');
    await user.clear(d.getByLabelText('Base wallet address'));
    await user.type(d.getByLabelText('Base wallet address'), '0x1111111111111111111111111111111111aBcDeF');
    await user.clear(d.getByLabelText('Confirm the last 6 characters'));
    await user.type(d.getByLabelText('Confirm the last 6 characters'), 'ABCDEF');
    await user.click(d.getByRole('button', { name: 'Add wallet' }));
    await waitFor(() => expect(d.getByLabelText('Code from your email')).toHaveAccessibleDescription(/That code is not valid or has expired/));
    expect(screen.getByRole('dialog', { name: 'Add wallet' })).toBeInTheDocument();
  });

  it('adds a Solana wallet with the authenticator code when 2FA is on, and lists it', async () => {
    const { user, d, mock } = await openAdd({ email: 'secure@acme.studio' }, 'Solana');
    expect(d.getByRole('radio', { name: /Solana/ })).toBeChecked();
    expect(d.getByLabelText('Code from your authenticator app')).toBeInTheDocument();
    expect(d.getByRole('radio', { name: /Code by email/ })).toBeInTheDocument();
    const address = '5yNmRkS3ZzK7h8QyJ1v2Xc4Bn6Mq9Wd3Tf5Gp8Lr2Ae1';
    await user.type(d.getByLabelText('Name'), 'Payroll');
    await user.type(d.getByLabelText('Solana wallet address'), address);
    await user.type(d.getByLabelText('Confirm the last 6 characters'), 'p8lr2ae1'.slice(2));
    await user.type(d.getByLabelText('Code from your authenticator app'), MOCK_TOTP);
    await user.click(d.getByRole('button', { name: 'Add wallet' }));
    // Base58 is case-sensitive: "lr2ae1" is not "Lr2Ae1".
    expect(d.getByLabelText('Confirm the last 6 characters')).toHaveAccessibleDescription(/not the last 6 characters/);
    await user.clear(d.getByLabelText('Confirm the last 6 characters'));
    await user.type(d.getByLabelText('Confirm the last 6 characters'), 'Lr2Ae1');
    await user.click(d.getByRole('button', { name: 'Add wallet' }));
    expect(await screen.findByText('Payroll added')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Add wallet' })).toBeNull());
    const solana = screen.getByTestId('wallets-solana');
    await within(solana).findByText('Payroll');
    expect(mock.db().wallets.find((w) => w.label === 'Payroll')).toMatchObject({ family: 'solana', address, livemode: true });
  });
});

describe('confirm the last 6 characters', () => {
  const OWNER = '0xD544632988655aa58d0C15b925970526e93658A5';
  const confirmField = (d: ReturnType<typeof within>) => d.getByRole('textbox', { name: 'Confirm the last 6 characters' });
  const status = () => document.querySelector('.code-field')!.getAttribute('data-status');

  it('is one textbox drawn as six boxes, with the case rule of the network in its help', async () => {
    const { user, d } = await openAdd();
    const field = confirmField(d);
    expect(d.getAllByRole('textbox').filter((el) => el.closest('.code-field'))).toHaveLength(1);
    expect(document.querySelectorAll('.code-box')).toHaveLength(6);
    expect(field).toHaveAttribute('autocomplete', 'one-time-code');
    expect(field).toHaveAccessibleDescription(/Read them from your wallet app, not from what you pasted, so a wrong paste is caught\. Upper or lower case, either works\./);
    await user.click(d.getByRole('radio', { name: /Solana/ }));
    expect(confirmField(d)).toHaveAccessibleDescription(/Solana addresses are case-sensitive, so match each letter exactly\./);
  });

  it('five characters are not a mismatch: it says one is missing, and the full tail then matches in any case', async () => {
    const posts: unknown[] = [];
    const { user, d } = await openAdd({ handlers: [http.post('/v1/dashboard/wallets', async ({ request }) => void posts.push(await request.clone().json()))] });
    await user.type(d.getByLabelText('Name'), 'Acme Store Wallet');
    await user.type(d.getByLabelText('Base wallet address'), OWNER);
    await user.type(confirmField(d), '658A5');
    expect(status()).toBe('idle');
    expect(d.queryByText(/not the last 6 characters/)).toBeNull();
    expect(confirmField(d)).not.toHaveAttribute('aria-invalid');
    await user.click(d.getByRole('button', { name: 'Add wallet' }));
    expect(confirmField(d)).toHaveFocus();
    expect(confirmField(d)).toHaveAccessibleDescription(/Type all 6 characters \(5 of 6\)\./);
    expect(confirmField(d)).toHaveAttribute('aria-invalid', 'true');
    expect(d.queryByText(/not the last 6 characters/)).toBeNull();
    expect(status()).toBe('idle');
    expect(posts).toEqual([]);

    await user.clear(confirmField(d));
    await user.type(confirmField(d), '3658a5');
    expect(status()).toBe('confirmed');
    expect(d.getByText('These match the last 6 characters of the address.')).toBeInTheDocument();
    expect(confirmField(d)).not.toHaveAttribute('aria-invalid');
  });

  it('a wrong full value is reported at once, kept for checking, and blocks the save', async () => {
    const posts: unknown[] = [];
    const { user, d } = await openAdd({ handlers: [http.post('/v1/dashboard/wallets', async ({ request }) => void posts.push(await request.clone().json()))] });
    await user.type(d.getByLabelText('Name'), 'Ops');
    await user.type(d.getByLabelText('Base wallet address'), OWNER);
    await user.type(confirmField(d), '3658A6');
    expect(status()).toBe('invalid');
    expect(confirmField(d)).toHaveAccessibleDescription(/These are not the last 6 characters of the address you pasted\. Check the address\./);
    await new Promise((r) => setTimeout(r, 800));
    expect(confirmField(d)).toHaveValue('3658A6');
    await user.click(d.getByRole('button', { name: 'Add wallet' }));
    expect(confirmField(d)).toHaveFocus();
    expect(posts).toEqual([]);
  });

  it('Solana compares the exact case', async () => {
    const { user, d } = await openAdd({}, 'Solana');
    await user.type(d.getByLabelText('Solana wallet address'), '9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin');
    await user.type(confirmField(d), 'usvfin');
    expect(status()).toBe('invalid');
    await user.clear(confirmField(d));
    await user.type(confirmField(d), 'usVFin');
    expect(status()).toBe('confirmed');
  });

  it('pasted characters drop spaces and dashes', async () => {
    const { user, d } = await openAdd();
    await user.type(d.getByLabelText('Base wallet address'), OWNER);
    await user.click(confirmField(d));
    await user.paste('36-58 A5');
    expect(confirmField(d)).toHaveValue('3658A5');
    expect(status()).toBe('confirmed');
  });
});

describe('archive wallet', () => {
  it('explains that products still pay the wallet, without asking for the password, and goes to the products', async () => {
    const { user } = renderApp('/settings/networks');
    await user.click(await screen.findByRole('button', { name: 'Archive Founder' }));
    const d = within(await screen.findByRole('dialog', { name: 'Founder still receives payments' }));
    expect(d.getByTestId('archive-blocked')).toHaveTextContent('This wallet still receives payments from 9 products. Change those products first.');
    expect(d.queryByLabelText('Password')).toBeNull();
    await user.click(d.getByRole('link', { name: 'Go to products' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Products' })).toBeInTheDocument();
  });

  it('says one product in the singular', async () => {
    const { user } = renderApp('/settings/networks');
    await user.click(await screen.findByRole('button', { name: 'Archive Reserve' }));
    const d = within(await screen.findByRole('dialog', { name: 'Reserve still receives payments' }));
    expect(d.getByTestId('archive-blocked')).toHaveTextContent('This wallet still receives payments from 1 product. Change that product first.');
  });

  it('turns a refusal the list did not foresee into the same explanation', async () => {
    const { user, mock } = renderApp('/settings/networks', {
      handlers: [http.post('/v1/dashboard/wallets/:id/archive', () => HttpResponse.json({ error: { code: 'wallet_in_use', message: 'This wallet still receives payments from 1 product. Change that product first.' } }, { status: 409 }))],
    });
    mock.db().wallets.push({ id: 'wal_SPARE', label: 'Spare', family: 'evm', address: '0x2222222222222222222222222222222222222222', verified: true, archived_at: null, created_at: new Date().toISOString(), livemode: true });
    await user.click(await screen.findByRole('button', { name: 'Archive Spare' }));
    const d = within(await screen.findByRole('dialog', { name: 'Archive Spare?' }));
    await emailCode(user, d);
    await user.click(d.getByRole('button', { name: 'Archive wallet' }));
    const blocked = within(await screen.findByRole('dialog', { name: 'Spare still receives payments' }));
    expect(blocked.getByTestId('archive-blocked')).toHaveTextContent('This wallet still receives payments from 1 product. Change that product first.');
  });

  it('archives a wallet no product pays, after an emailed code', async () => {
    const { user, mock } = renderApp('/settings/networks');
    // The wallets are asked for after the session check, so this is in place before they load.
    mock.db().wallets.push({ id: 'wal_SPARE', label: 'Spare', family: 'evm', address: '0x2222222222222222222222222222222222222222', verified: true, archived_at: null, created_at: new Date().toISOString(), livemode: true });
    await user.click(await screen.findByRole('button', { name: 'Archive Spare' }));
    const d = within(await screen.findByRole('dialog', { name: 'Archive Spare?' }));
    await user.click(d.getByRole('button', { name: 'Archive wallet' }));
    expect(d.getByText('Ask for a code first, then enter it here.')).toBeInTheDocument();
    await emailCode(user, d);
    await user.click(d.getByRole('button', { name: 'Archive wallet' }));
    expect(await screen.findByText('Spare archived')).toBeInTheDocument();
    expect(mock.db().wallets.find((w) => w.id === 'wal_SPARE')!.archived_at).not.toBeNull();
  });
});

describe('confirmation methods', () => {
  it('on a server without email, falls back to the authenticator the refusal lists', async () => {
    const { user, mock } = renderApp('/settings/networks', { email: 'secure@acme.studio', noEmail: true });
    mock.db().wallets.push({ id: 'wal_SPARE', label: 'Spare', family: 'evm', address: '0x2222222222222222222222222222222222222222', verified: true, archived_at: null, created_at: new Date().toISOString(), livemode: true });
    await user.click(await screen.findByRole('button', { name: 'Archive Spare' }));
    const d = within(await screen.findByRole('dialog', { name: 'Archive Spare?' }));
    await user.click(d.getByRole('radio', { name: /Code by email/ }));
    await user.click(d.getByRole('button', { name: 'Email me a code' }));
    expect(await d.findByText(/Codes by email are not set up on this server/)).toBeInTheDocument();
    expect(d.queryByRole('radio', { name: /Code by email/ })).toBeNull();
    await user.type(d.getByLabelText('Code from your authenticator app'), MOCK_TOTP);
    await user.click(d.getByRole('button', { name: 'Archive wallet' }));
    expect(await screen.findByText('Spare archived')).toBeInTheDocument();
  });
});

describe('settings networks', () => {
  const rowOf = (card: HTMLElement, label: string) => within(card).getByText(label, { selector: '.wallet-name > span' }).closest('li')!;

  it('marks the default wallet of each network and offers Make default on the others', async () => {
    renderApp('/settings/networks');
    const base = await screen.findByTestId('network-base');
    const founder = within(rowOf(base, 'Founder'));
    expect(founder.getByText('Default')).toBeInTheDocument();
    expect(founder.getByText('New products pay here')).toBeInTheDocument();
    expect(founder.queryByRole('button', { name: /Make Founder the default/ })).toBeNull();
    const partner = within(rowOf(base, 'Partner'));
    expect(partner.queryByText('Default')).toBeNull();
    expect(partner.getByRole('button', { name: 'Make Partner the default' })).toBeInTheDocument();
    // The Solana default is its own: one per network.
    expect(within(rowOf(screen.getByTestId('network-solana'), 'Treasury')).getByText('Default')).toBeInTheDocument();
  });

  it('makes another wallet the default with no confirmation field', async () => {
    const posts: string[] = [];
    const { user } = renderApp('/settings/networks', {
      handlers: [http.post('/v1/dashboard/wallets/:id/default', async ({ request }) => void posts.push(`${new URL(request.url).pathname}?${new URL(request.url).searchParams.get('mode')}`))],
    });
    const base = await screen.findByTestId('network-base');
    await user.click(within(base).getByRole('button', { name: 'Make Partner the default' }));
    expect(posts).toHaveLength(1);
    expect(posts[0]).toMatch(/^\/v1\/dashboard\/wallets\/wal_[^/]+\/default\?live$/);
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('moves the Default tag, and new products then pay that wallet', async () => {
    const { user, mock } = renderApp('/settings/networks');
    const base = await screen.findByTestId('network-base');
    await user.click(within(base).getByRole('button', { name: 'Make Partner the default' }));
    expect(await screen.findByText('Partner is now the default')).toBeInTheDocument();
    await waitFor(() => expect(within(rowOf(screen.getByTestId('network-base'), 'Partner')).getByText('Default')).toBeInTheDocument());
    expect(within(rowOf(screen.getByTestId('network-base'), 'Founder')).queryByText('Default')).toBeNull();
    const partner = mock.db().wallets.find((w) => w.livemode && w.family === 'evm' && w.label === 'Partner')!;
    expect(partner.default).toBe(true);
    // The Solana default did not move.
    expect(within(rowOf(screen.getByTestId('network-solana'), 'Treasury')).getByText('Default')).toBeInTheDocument();
  });

  it('opens the add-wallet drawer for the family of the card it is on', async () => {
    const { user } = renderApp('/settings/networks');
    const solana = await screen.findByTestId('network-solana');
    await user.click(within(solana).getByRole('button', { name: 'Add wallet on Solana' }));
    const d = within(await screen.findByRole('dialog', { name: 'Add wallet' }));
    expect(d.getByLabelText(/^Solana wallet address/)).toBeInTheDocument();
  });

  it('offers both networks in the add-wallet drawer when both have a chain', async () => {
    const both = await openAdd();
    expect(both.d.getByRole('radio', { name: 'Base' })).toBeChecked();
    expect(both.d.getByRole('radio', { name: 'Solana' })).toBeInTheDocument();
  });

  it('hides the network choice when only Base is open (Solana off)', async () => {
    const settings = { accepted_assets: [{ chain: 'base', asset: 'USDC', enabled: true }], fee: { fee_bps: 50, fixed_fee: '0.10' } };
    const { d } = await openAdd({ handlers: [http.get('/v1/dashboard/settings', () => HttpResponse.json(settings))] });
    expect(d.queryByRole('radio')).toBeNull();
    expect(d.getByLabelText(/^Base wallet address/)).toBeInTheDocument();
  });

  it('shows a network the mode does not offer as disabled, "Coming soon"', async () => {
    const settings = { accepted_assets: [{ chain: 'base', asset: 'USDC', enabled: true }, { chain: 'base', asset: 'USDT', enabled: true }], fee: { fee_bps: 50, fixed_fee: '0.10' } };
    renderApp('/settings/networks', { handlers: [http.get('/v1/dashboard/settings', () => HttpResponse.json(settings))] });
    const soon = await screen.findByTestId('network-soon-solana');
    expect(soon).toHaveAttribute('aria-disabled', 'true');
    expect(within(soon).getByText('Coming soon')).toBeInTheDocument();
    expect(within(soon).queryByRole('button', { name: /Add wallet/ })).toBeNull();
    expect(within(soon).queryByRole('checkbox')).toBeNull();
    // Wallets already on that network stay listed, so they can still be archived.
    expect(within(within(soon).getByTestId('wallets-solana')).getAllByRole('button', { name: /Archive/ }).length).toBeGreaterThan(0);
    expect(screen.queryByTestId('network-solana')).toBeNull();
    expect(screen.getByTestId('network-base')).toBeInTheDocument();
  });

  it('archives the default wallet of a network: the oldest active wallet takes over', async () => {
    const { user, mock } = renderApp('/settings/networks');
    mock.db().wallets.push({ id: 'wal_SPARE', label: 'Spare', family: 'evm', address: '0x2222222222222222222222222222222222222222', verified: true, archived_at: null, created_at: new Date().toISOString(), livemode: true, default: true });
    for (const w of mock.db().wallets) if (w.livemode && w.family === 'evm' && w.id !== 'wal_SPARE') w.default = false;
    const base = await screen.findByTestId('network-base');
    expect(within(rowOf(base, 'Spare')).getByText('Default')).toBeInTheDocument();
    await user.click(within(base).getByRole('button', { name: 'Archive Spare' }));
    const d = within(await screen.findByRole('dialog', { name: 'Archive Spare?' }));
    await emailCode(user, d);
    await user.click(d.getByRole('button', { name: 'Archive wallet' }));
    expect(await screen.findByText('Spare archived')).toBeInTheDocument();
    await waitFor(() => expect(within(rowOf(screen.getByTestId('network-base'), 'Founder')).getByText('Default')).toBeInTheDocument());
  });

  it('saves the stablecoins of one network and leaves the others as they are', async () => {
    const { user, mock } = renderApp('/settings/networks');
    const solana = await screen.findByTestId('network-solana');
    await user.click(within(solana).getByRole('checkbox', { name: 'USDT on Solana' }));
    await user.click(within(solana).getByRole('button', { name: 'Save Solana stablecoins' }));
    await waitFor(() => expect(mock.db().accepted.live.find((a) => a.asset === 'USDT')!.enabled).toBe(false));
    expect(mock.db().accepted.live.find((a) => a.chain === 'base')!.enabled).toBe(true);
  });
});
