import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { ApiError } from '../src/lib/api';
import { attentionLine, canRotate, rotationAdvised } from '../src/lib/customers';
import { sessionAmount, tokenAmount } from '../src/lib/format';
import { recheckError, RECHECK_COPY, supersededBy } from '../src/lib/recheck';
import { simActionsFor } from '../src/lib/sandbox';
import { createMockApi } from '../src/mocks/handlers';
import { renderApp, useMockServer } from './helpers';

useMockServer();

const seed = () => createMockApi().db();
const byCustomer = (customer: string, status: 'active' | 'rotated' = 'active') => seed().customerAddresses!.find((a) => a.customer_id === customer && a.status === status)!;
const row = (id: string) => document.querySelector(`[data-address-id="${id}"]`) as HTMLElement;

describe('customer helpers', () => {
  it('formats open amounts', () => {
    expect(sessionAmount(null)).toBe('Any amount');
    expect(sessionAmount('1250.5')).toBe('1,250.50 USD');
    expect(tokenAmount(null, 'USDC')).toBe('Any amount');
    expect(tokenAmount('25.000000', 'USDC')).toBe('25.000000 USDC');
  });

  it('says what a rotation would change', () => {
    expect(attentionLine({ reason: 'archived_wallet', wallets: [{ id: 'wal_1', label: 'Old hardware wallet' }] })).toBe('It still pays Old hardware wallet, which you archived.');
    expect(attentionLine({ reason: 'payout_changed', chains: ['base', 'solana'] })).toMatch(/^The product pays different wallets or shares on Base and Solana now\./);
    expect(attentionLine({ reason: 'fee_changed', chains: ['base-sepolia'] })).toMatch(/fee on Base Sepolia changed/);
    expect(attentionLine({ reason: 'new_networks', assets: [{ chain: 'solana', asset: 'USDT' }] })).toBe('It does not offer USDT on Solana, which you accept now.');
    expect(attentionLine({ reason: 'no_networks' })).toMatch(/no new payment opens/);
    expect(attentionLine({ reason: 'product_inactive' })).toMatch(/rotated only once the product is active/);
  });

  it('advises a rotation only on an active address it would fix', () => {
    const product = { id: 'prod_1', name: 'P', price_type: 'open' as const, active: true };
    expect(rotationAdvised({ status: 'active', attention: [{ reason: 'archived_wallet' }] })).toBe(true);
    expect(rotationAdvised({ status: 'active', attention: [{ reason: 'product_inactive' }] })).toBe(false);
    expect(rotationAdvised({ status: 'rotated', attention: [{ reason: 'fee_changed' }] })).toBe(false);
    expect(canRotate({ status: 'active', product })).toBe(true);
    expect(canRotate({ status: 'active', product: { ...product, active: false } })).toBe(false);
    expect(canRotate({ status: 'rotated', product })).toBe(false);
  });

  it('knows a superseded session and its 409', () => {
    expect(supersededBy({ superseded_by: 'ses_2', recheck_available: false })).toBe('ses_2');
    expect(supersededBy({ superseded_by: null, recheck_available: false })).toBeNull();
    expect(recheckError(new ApiError(409, 'session_superseded', 'x'))).toBe(RECHECK_COPY.superseded);
  });

  it('simulates an open-amount session with an amount, never partly or over', () => {
    const actions = simActionsFor(true);
    expect(actions.map((a) => a.action)).toEqual(['pay_exact', 'expire', 'late_payment', 'reverse_last_payment', 'under_review']);
    expect(actions[0]).toMatchObject({ label: 'Pay an amount', amount: true });
    expect(simActionsFor(false).map((a) => a.action)).toContain('pay_partial');
  });
});

describe('Customers list', () => {
  it('lists each address with its product, status, last payment and total received', async () => {
    renderApp('/customers');
    const rows = await screen.findAllByTestId('customer-row');
    expect(screen.getByRole('heading', { level: 1, name: 'Customers' })).toBeInTheDocument();
    expect(rows).toHaveLength(4);
    const regular = within(row(byCustomer('user_1042').id));
    expect(regular.getByTestId('customer-product')).toHaveTextContent('Account top-up');
    expect(regular.getByText('Active')).toBeInTheDocument();
    expect(regular.getByText('Open')).toBeInTheDocument();
    expect(regular.getByText(/50\.00 USDC/)).toBeInTheDocument();
    expect(regular.queryByTestId('customer-attention')).toBeNull();
    const stale = within(row(byCustomer('user_0977').id));
    expect(stale.getByTestId('customer-attention')).toHaveTextContent('Rotate advised');
    expect(stale.getByRole('button', { name: 'Rotate the address of user_0977' })).toBeInTheDocument();
    expect(within(row(byCustomer('user_0810', 'rotated').id)).getByText('Rotated')).toBeInTheDocument();
  });

  it('finds a customer by exact id and says when none matches', async () => {
    const { user } = renderApp('/customers');
    await screen.findAllByTestId('customer-row');
    const search = screen.getByRole('searchbox', { name: 'Find a customer by ID' });
    await user.type(search, 'user_0810');
    await waitFor(() => expect(screen.getAllByTestId('customer-row')).toHaveLength(2));
    await user.clear(search);
    await user.type(search, 'bad id');
    expect(await screen.findByRole('alert')).toHaveTextContent('Customer IDs use letters');
    await user.clear(search);
    await user.type(search, 'nobody');
    expect(await screen.findByText('No customer with this ID.')).toBeInTheDocument();
  });

  it('is in the navigation', async () => {
    renderApp('/overview');
    const nav = (await screen.findAllByRole('navigation', { name: 'Main' }))[0]!;
    expect(within(nav).getByRole('link', { name: 'Customers' })).toHaveAttribute('href', '/customers');
  });
});

describe('Customer address detail', () => {
  it('shows the addresses, the page link, the sessions with open amounts, and no warning when up to date', async () => {
    const ca = byCustomer('user_1042');
    renderApp(`/customers/${ca.id}`);
    expect(await screen.findByRole('heading', { level: 1, name: 'user_1042' })).toBeInTheDocument();
    expect(screen.getAllByTestId('address-pay-to')).toHaveLength(ca.pay_to.length);
    expect(screen.getByRole('link', { name: /Open the address page/ })).toHaveAttribute('href', `https://prismaport.xyz/a/${ca.id}`);
    expect(screen.queryByTestId('address-attention')).toBeNull();
    expect(screen.getByTestId('stat-sessions')).toHaveTextContent('4');
    const sessions = await screen.findAllByTestId('session-row');
    expect(sessions).toHaveLength(4);
    expect(sessions[0]).toHaveTextContent('Any amount');
  });

  it('warns about the archived wallet and the payout, and rotates after the confirm', async () => {
    const ca = byCustomer('user_0977');
    const { user, mock } = renderApp(`/customers/${ca.id}`);
    const warning = await screen.findByTestId('address-attention');
    expect(warning).toHaveTextContent('This address is out of date');
    expect(warning).toHaveTextContent('It still pays Old hardware wallet, which you archived.');
    expect(warning).toHaveTextContent('The product pays different wallets or shares on Base now.');
    await user.click(screen.getByTestId('rotate-button'));
    const d = within(await screen.findByRole('dialog', { name: 'Rotate the address of user_0977?' }));
    expect(d.getByTestId('rotate-body')).toHaveTextContent('The old address stops being watched. A payment already open on it finishes as usual');
    await user.click(d.getByRole('button', { name: 'Rotate address' }));
    await waitFor(() => expect(mock.db().customerAddresses!.find((a) => a.id === ca.id)!.status).toBe('rotated'));
    const next = mock.db().customerAddresses!.find((a) => a.customer_id === 'user_0977' && a.status === 'active')!;
    expect(await screen.findByText('user_0977 has a new address')).toBeInTheDocument();
    // The page moves to the new address: active, up to date.
    await waitFor(() => expect(screen.getByRole('link', { name: /Open the address page/ })).toHaveAttribute('href', `https://prismaport.xyz/a/${next.id}`));
    expect(screen.queryByTestId('address-attention')).toBeNull();
    expect(screen.getByTestId('rotate-button')).toBeEnabled();
  });

  it('shows a rotated address with the one that replaced it, and no Rotate', async () => {
    const old = byCustomer('user_0810', 'rotated');
    renderApp(`/customers/${old.id}`);
    const note = await screen.findByTestId('address-rotated');
    expect(within(note).getByTestId('replaced-by')).toHaveAttribute('href', `/customers/${old.replaced_by}`);
    expect(screen.queryByTestId('rotate-button')).toBeNull();
  });
});

describe('payments on a customer address', () => {
  const sessions = () => {
    const db = seed();
    const ca = db.customerAddresses!.find((a) => a.customer_id === 'user_1042')!;
    return db.sessions.filter((s) => s.customer_address_id === ca.id).sort((a, b) => a.created_at.localeCompare(b.created_at));
  };

  it('links the address, shows an open amount and says Check again is not available once superseded', async () => {
    const expired = sessions().find((s) => s.status === 'expired')!;
    renderApp(`/payments/${expired.id}`);
    expect(await screen.findByRole('heading', { level: 1, name: 'Any amount' })).toBeInTheDocument();
    expect(screen.getByTestId('from-customer-address')).toHaveAttribute('href', `/customers/${expired.customer_address_id}`);
    const panel = await screen.findByTestId('recheck-superseded');
    expect(panel).toHaveTextContent('Check again is not available here.');
    expect(screen.queryByTestId('recheck-button')).toBeNull();
    expect(within(panel).getByTestId('recheck-newer')).toHaveAttribute('href', `/payments/${sessions().at(-1)!.id}`);
  });

  it('says a session was settled together with another one', async () => {
    const [first, second] = sessions();
    renderApp(`/payments/${second!.id}`);
    const note = await screen.findByTestId('settled-with');
    expect(note).toHaveTextContent('was settled together with');
    expect(within(note).getByRole('link')).toHaveAttribute('href', `/payments/${first!.id}`);
    expect(screen.queryByText(/Nothing settled yet/)).toBeNull();
  });
});
