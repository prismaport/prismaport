import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { MOCK_CODE, MOCK_TOTP, units } from '../src/mocks/db';
import { feePercent } from '../src/pages/PaymentDetail';
import { SEARCH_HINT } from '../src/pages/Payments';
import { createMockApi } from '../src/mocks/handlers';
import { emailCode, renderApp, useMockServer } from './helpers';

useMockServer();

/** The first link of a seeded product: the one migration 00022 made, with no label. */
const firstLink = (db: ReturnType<ReturnType<typeof createMockApi>['db']>, product: string) => {
  const p = db.products.find((x) => x.name === product)!;
  return db.links.find((l) => l.product_id === p.id && l.name === null)!;
};

describe('link detail', () => {
  it('shows stats, the share URL with its QR, the product, and the sessions from the link', async () => {
    const { mock, user } = renderApp('/links');
    const founder = firstLink(mock.db(), 'Founder plan');
    await screen.findAllByTestId('link-row');
    const row = document.querySelector(`[data-link-id="${founder.id}"]`) as HTMLElement;
    await user.click(within(row).getByRole('link', { name: 'Founder plan' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Founder plan' })).toBeInTheDocument();
    expect(founder.id).toMatch(/^plink_/);
    const opened = mock.db().sessions.filter((s) => s.link_id === founder.id).length;
    expect(within(screen.getByTestId('link-product-card')).getByTestId('link-product')).toHaveAttribute('href', `/products/${founder.product_id}`);
    expect(screen.getByTestId('stat-opened')).toHaveTextContent(String(opened));
    expect(screen.getByText(`https://prismaport.xyz/l/${founder.id}`)).toBeInTheDocument();
    expect(screen.getByRole('img', { name: 'QR code of the payment link for Founder plan' })).toHaveAttribute('data-qr', `https://prismaport.xyz/l/${founder.id}`);
    const rows = await screen.findAllByTestId('session-row');
    expect(rows.length).toBe(Math.min(opened, 20));
    expect(screen.getByRole('button', { name: 'Show more payments' })).toBeInTheDocument();
  });

  it('explains why a link is not live', async () => {
    const { mock, user } = renderApp('/links');
    await screen.findAllByTestId('link-row');
    await user.click(screen.getByRole('link', { name: 'Design workshop ticket' }));
    expect(await screen.findByText(/reached its limit of 12 paid payments/)).toBeInTheDocument();
    expect(screen.getByText('Limit reached', { selector: '.pill' })).toBeInTheDocument();
  });
});

describe('payment detail', () => {
  it('shows the timeline, the fee and net of the settlement, and explorer links', async () => {
    const { mock, user } = renderApp('/payments?status=paid');
    const rows = await screen.findAllByTestId('session-row');
    const first = mock.db().sessions.filter((s) => s.livemode && s.status === 'paid').sort((a, b) => b.created_at.localeCompare(a.created_at))[0]!;
    await user.click(rows[0]!);
    expect(await screen.findByRole('heading', { name: 'Timeline' })).toBeInTheDocument();
    const timeline = within(screen.getByTestId('timeline'));
    expect(timeline.getByText('Paid')).toBeInTheDocument();
    const settlement = within(screen.getByTestId('settlement'));
    const st = first.settlement!;
    expect(units(st.fee) + units(st.net)).toBe(units(st.gross));
    expect(settlement.getByText(`${st.fee} ${st.asset}`)).toBeInTheDocument();
    expect(settlement.getByText(`${st.net} ${st.asset}`)).toBeInTheDocument();
    expect(units(st.fee)).toBe(100_000n + (units(st.gross) * 50n) / 10_000n);
    expect(settlement.getByText(`Prismaport fee (${feePercent(st)})`)).toBeInTheDocument();
    const link = settlement.getByRole('link', { name: 'View transaction' });
    expect(link.getAttribute('href')).toMatch(/^https:\/\/(basescan\.org|solscan\.io)\/tx\//);
    expect(link).toHaveAttribute('rel', 'noreferrer');
    expect(screen.getAllByTestId('pay-to')).toHaveLength(3);
  });

  it('names the product and link of each payment in the list, and says which came from the API', async () => {
    const { mock } = renderApp('/payments');
    const rows = await screen.findAllByTestId('session-row');
    const shown = mock
      .db()
      .sessions.filter((s) => s.livemode)
      .sort((a, b) => b.created_at.localeCompare(a.created_at))
      .slice(0, rows.length);
    const fromLink = shown.findIndex((s) => s.link_id);
    const fromApi = shown.findIndex((s) => !s.link_id);
    expect(fromLink).toBeGreaterThanOrEqual(0);
    expect(fromApi).toBeGreaterThanOrEqual(0);
    const product = mock.db().products.find((p) => p.id === shown[fromLink]!.product_id)!;
    expect(rows[fromLink]).toHaveTextContent(product.name);
    expect(rows[fromLink]).not.toHaveTextContent(shown[fromLink]!.id.slice(0, 8));
    expect(rows[fromApi]).toHaveTextContent('Custom order · API');
    expect(rows[fromApi]).not.toHaveTextContent(shown[fromApi]!.id.slice(0, 8));
  });

  it('shows the product and link of a payment from the session itself', async () => {
    const seed = createMockApi().db();
    const ses = seed.sessions.find((s) => s.livemode && s.link_id)!;
    const link = seed.links.find((l) => l.id === ses.link_id)!;
    const product = seed.products.find((p) => p.id === ses.product_id)!;
    const reads: string[] = [];
    renderApp(`/payments/${ses.id}`, {
      handlers: [http.get('/v1/dashboard/links/:id', ({ request }) => void reads.push(request.url)), http.get('/v1/dashboard/products/:id', ({ request }) => void reads.push(request.url))],
    });
    expect(await screen.findByTestId('from-product')).toHaveTextContent(product.name);
    expect(screen.getByTestId('from-product')).toHaveAttribute('href', `/products/${product.id}`);
    expect(screen.getByTestId('from-link')).toHaveAttribute('href', `/links/${link.id}`);
    expect(screen.getByTestId('payment-link-name')).toHaveTextContent(product.name);
    expect(reads).toEqual([]);
  });

  it('names an API payment by its product and says it came from the API', async () => {
    const seed = createMockApi().db();
    const ses = seed.sessions.find((s) => s.livemode && !s.link_id)!;
    renderApp(`/payments/${ses.id}`);
    expect(await screen.findByTestId('from-product')).toHaveTextContent('Custom order');
    expect(screen.getByText('Created with the API')).toBeInTheDocument();
    expect(screen.getByTestId('payment-link-name')).toHaveTextContent('Custom order · API');
  });

  it('explains what the search finds', async () => {
    renderApp('/payments');
    const input = await screen.findByRole('searchbox', { name: 'Search payments' });
    expect(input).toHaveAttribute('placeholder', 'Session ID, transaction hash, address or amount');
    expect(input).toHaveAccessibleDescription(SEARCH_HINT);
    expect(SEARCH_HINT).toMatch(/start of a session ID or of a transaction hash \(8 characters or more\)/);
    expect(SEARCH_HINT).toMatch(/exact amount such as 25\.00/);
  });

  it('finds payments by the start of a session ID, in any case, and by an exact amount', async () => {
    const { mock, user } = renderApp('/payments');
    await screen.findAllByTestId('session-row');
    const live = mock.db().sessions.filter((s) => s.livemode);
    // The last one with an amount of its own (open-amount sessions on customer addresses have none).
    const target = live.filter((s) => s.amount !== null).at(-1)! as (typeof live)[number] & { amount: string };
    const prefix = target.id.slice(0, 14);
    const search = screen.getByRole('searchbox', { name: 'Search payments' });
    await user.type(search, prefix.toLowerCase());
    await waitFor(() => {
      const rows = screen.getAllByTestId('session-row');
      expect(rows).toHaveLength(live.filter((s) => s.id.startsWith(prefix)).length);
    });
    await user.clear(search);
    await user.type(search, target.amount);
    const want = live.filter((s) => s.amount === target.amount || s.payments.some((p) => Number(p.amount) === Number(target.amount))).length;
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(Math.min(want, 20)));
    await user.clear(search);
    await user.type(search, 'ses_%');
    expect(await screen.findByText('No payments match these filters.')).toBeInTheDocument();
  });

  it('filters payments by status', async () => {
    const { user } = renderApp('/payments');
    await screen.findAllByTestId('session-row');
    await user.selectOptions(screen.getByLabelText('Status'), 'expired');
    await waitFor(() => expect(screen.getAllByTestId('session-row').every((r) => within(r).queryByText('Expired'))).toBe(true));
  });
});

describe('developers', () => {
  it('creates an API key and shows its secret once', async () => {
    const { user } = renderApp('/developers/api-keys');
    await screen.findAllByTestId('api-key');
    await user.click(screen.getByRole('button', { name: 'Create API key' }));
    const d = within(await screen.findByRole('dialog', { name: 'Create API key' }));
    await user.click(d.getByRole('button', { name: 'Create key' }));
    expect(d.getByText('Give the key a name, such as Production server.')).toBeInTheDocument();
    await user.type(d.getByLabelText('Name'), 'Checkout server');
    await emailCode(user, d);
    await user.click(d.getByRole('button', { name: 'Create key' }));
    const done = within(await screen.findByRole('dialog', { name: 'Your new API key' }));
    expect(done.getByText(/You will not see it again/)).toBeInTheDocument();
    const secret = done.getByText(/^sk_live_/).textContent!;
    await user.click(done.getByRole('button', { name: 'Copy API key' }));
    expect(await navigator.clipboard.readText()).toBe(secret);
    await user.click(done.getByRole('button', { name: 'I copied the key' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.queryByText(secret)).not.toBeInTheDocument();
    expect(await screen.findByText('Checkout server')).toBeInTheDocument();
  });

  it('confirms a live key with the authenticator code when 2FA is on', async () => {
    const { user } = renderApp('/developers/api-keys', { email: 'secure@acme.studio' });
    await user.click(await screen.findByRole('button', { name: 'Create API key' }));
    const d = within(await screen.findByRole('dialog', { name: 'Create API key' }));
    await user.type(d.getByLabelText('Name'), 'Live key');
    await user.type(d.getByLabelText('Code from your authenticator app'), '123456');
    await user.click(d.getByRole('button', { name: 'Create key' }));
    expect(await screen.findByRole('dialog', { name: 'Your new API key' })).toBeInTheDocument();
  });

  it('says so when the two-factor code for a live key is wrong', async () => {
    const { user } = renderApp('/developers/api-keys', { email: 'secure@acme.studio' });
    await user.click(await screen.findByRole('button', { name: 'Create API key' }));
    const d = within(await screen.findByRole('dialog', { name: 'Create API key' }));
    await user.type(d.getByLabelText('Name'), 'Live key');
    await user.type(d.getByLabelText('Code from your authenticator app'), '999999');
    await user.click(d.getByRole('button', { name: 'Create key' }));
    expect(await d.findByText('That code is not valid. Enter the current code from your authenticator app.')).toBeInTheDocument();
  });

  it('revokes a key after confirming', async () => {
    const { user } = renderApp('/developers/api-keys');
    const row = (await screen.findByText('Production server')).closest('li') as HTMLElement;
    await user.click(within(row).getByRole('button', { name: 'Revoke' }));
    const d = within(await screen.findByRole('dialog', { name: 'Revoke Production server?' }));
    await user.click(d.getByRole('button', { name: 'Revoke key' }));
    await waitFor(() => expect(within(screen.getByText('Production server').closest('li') as HTMLElement).getByText(/^Revoked/)).toBeInTheDocument());
  });

  it('adds a webhook endpoint, rotates its secret and resends a delivery', async () => {
    const { user, mock } = renderApp('/developers/webhooks');
    await screen.findAllByTestId('webhook-endpoint');
    await user.click(screen.getByRole('button', { name: 'Add endpoint' }));
    let d = within(await screen.findByRole('dialog', { name: 'Add webhook endpoint' }));
    await user.type(d.getByLabelText('Endpoint address'), 'http://insecure.example');
    await user.click(d.getByRole('button', { name: 'Add endpoint' }));
    expect(d.getByText('Enter a full address that starts with https://.')).toBeInTheDocument();
    await user.clear(d.getByLabelText('Endpoint address'));
    await user.type(d.getByLabelText('Endpoint address'), 'https://acme.studio/hooks/new');
    await emailCode(user, d);
    await user.click(d.getByRole('button', { name: 'Add endpoint' }));
    d = within(await screen.findByRole('dialog', { name: 'Webhook endpoint added' }));
    expect(d.getByText(/^whsec_/)).toBeInTheDocument();
    await user.click(d.getByRole('button', { name: 'I copied the secret' }));
    const row = (await screen.findByText('https://acme.studio/hooks/new')).closest('li') as HTMLElement;
    await user.click(within(row).getByRole('button', { name: /Rotate secret/ }));
    const rotate = within(await screen.findByRole('dialog', { name: 'Rotate the signing secret?' }));
    // A new code can be asked for once a minute.
    for (const u of mock.db().users) u.confirm_sent_at = {};
    await emailCode(user, rotate, '246810');
    await user.click(rotate.getByRole('button', { name: 'Rotate secret' }));
    expect(await screen.findByRole('dialog', { name: 'New signing secret' })).toBeInTheDocument();
    await user.keyboard('{Escape}');
    const first = (await screen.findAllByTestId('webhook-endpoint'))[0]!;
    await user.click(within(first).getByRole('link', { name: 'Deliveries' }));
    const deliveries = await screen.findAllByTestId('delivery');
    const failed = deliveries.find((x) => within(x).queryByText('Failed'))!;
    await user.click(within(failed).getByRole('button', { name: /Resend/ }));
    expect(await screen.findByText('Delivery queued again')).toBeInTheDocument();
  });
});

describe('settings', () => {
  it('turns two-factor authentication on after an emailed code, with a local QR code', async () => {
    const enrolls: unknown[] = [];
    const { user } = renderApp('/settings/account', {
      handlers: [
        http.post('/v1/dashboard/auth/totp/enroll', async ({ request }) => {
          enrolls.push(await request.clone().json());
          return undefined;
        }),
      ],
    });
    await user.click(await screen.findByRole('button', { name: 'Turn on' }));
    const d = within(await screen.findByRole('dialog', { name: 'Turn on two-factor authentication' }));
    expect(d.queryByRole('img', { name: /authenticator app/ })).toBeNull();
    expect(d.queryByLabelText('Current code')).toBeNull();
    await user.click(d.getByRole('button', { name: 'Continue' }));
    expect(await d.findByText('Ask for a code first, then enter it here.')).toBeInTheDocument();
    expect(enrolls).toEqual([]);
    expect(d.queryByLabelText('Password')).toBeNull();
    await emailCode(user, d);
    await user.click(d.getByRole('button', { name: 'Continue' }));
    expect(await d.findByRole('img', { name: /authenticator app/ })).toHaveAttribute('data-qr', expect.stringMatching(/^otpauth:\/\/totp\//));
    expect(enrolls).toEqual([{ email_code: MOCK_CODE }]);
    expect(d.getByLabelText('Code')).toHaveFocus();
    await user.type(d.getByLabelText('Code'), MOCK_TOTP);
    await user.click(d.getByRole('button', { name: 'Turn on' }));
    expect(await screen.findByText(/^On\. We ask for a code/)).toBeInTheDocument();
  });

  it('keeps a wrong emailed code in the same dialog and shows no setup code', async () => {
    const { user, mock } = renderApp('/settings/account');
    await user.click(await screen.findByRole('button', { name: 'Turn on' }));
    const d = within(await screen.findByRole('dialog', { name: 'Turn on two-factor authentication' }));
    await emailCode(user, d, '000000');
    await user.click(d.getByRole('button', { name: 'Continue' }));
    expect(await d.findByText(/That code is not valid or has expired/)).toBeInTheDocument();
    expect(d.queryByRole('img', { name: /authenticator app/ })).toBeNull();
    expect(mock.db().users.find((u) => u.email === 'owner@acme.studio')!.totp_pending).toBe(false);
  });

  it('replaces the authenticator app with a current code, never the password or an emailed code', async () => {
    const { user } = renderApp('/settings/account', { email: 'secure@acme.studio' });
    await user.click(await screen.findByRole('button', { name: 'Replace app' }));
    const d = within(await screen.findByRole('dialog', { name: 'Replace your authenticator app' }));
    expect(d.queryByLabelText('Password')).toBeNull();
    expect(d.queryByRole('button', { name: 'Email me a code' })).toBeNull();
    await user.click(d.getByRole('button', { name: 'Continue' }));
    expect(await d.findByText('Enter the 6-digit code from the app you use now.')).toBeInTheDocument();
    await user.type(d.getByLabelText('Current code'), '999999');
    await user.click(d.getByRole('button', { name: 'Continue' }));
    expect(await d.findByText('That code is not valid. Enter the current code from your authenticator app.')).toBeInTheDocument();
    await user.clear(d.getByLabelText('Current code'));
    await user.type(d.getByLabelText('Current code'), MOCK_TOTP);
    await user.click(d.getByRole('button', { name: 'Continue' }));
    expect(await d.findByRole('img', { name: /authenticator app/ })).toHaveAttribute('data-qr', expect.stringContaining('secret=KRSXG5CTMVRXEZLU'));
    await user.type(d.getByLabelText('Code'), MOCK_TOTP);
    await user.click(d.getByRole('button', { name: 'Use this app' }));
    expect(await screen.findByText('Authenticator app replaced')).toBeInTheDocument();
    expect(screen.queryByRole('dialog', { name: 'Replace your authenticator app' })).toBeNull();
  });

  it('offers to start again when the setup expired', async () => {
    const { user } = renderApp('/settings/account', {
      handlers: [http.post('/v1/dashboard/auth/totp/verify', () => HttpResponse.json({ error: { code: 'invalid_request', message: 'start the two-factor setup again' } }, { status: 400 }))],
    });
    await user.click(await screen.findByRole('button', { name: 'Turn on' }));
    const d = within(await screen.findByRole('dialog', { name: 'Turn on two-factor authentication' }));
    await emailCode(user, d);
    await user.click(d.getByRole('button', { name: 'Continue' }));
    await user.type(await d.findByLabelText('Code'), MOCK_TOTP);
    await user.click(d.getByRole('button', { name: 'Turn on' }));
    expect(await d.findByText('start the two-factor setup again')).toBeInTheDocument();
    await user.click(d.getByRole('button', { name: 'Start again' }));
    expect(d.getByLabelText('Code from your email')).toHaveValue('');
    expect(d.queryByRole('img', { name: /authenticator app/ })).toBeNull();
  });

  it('saves accepted stablecoins', async () => {
    const { user, mock } = renderApp('/settings/networks');
    const box = await screen.findByRole('checkbox', { name: 'USDT on Solana' });
    expect(box).toBeChecked();
    await user.click(box);
    await user.click(screen.getByRole('button', { name: 'Save Solana stablecoins' }));
    await waitFor(() => expect(mock.db().accepted.live.find((a) => a.asset === 'USDT')!.enabled).toBe(false));
  });
});
