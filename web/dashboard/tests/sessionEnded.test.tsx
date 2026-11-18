import { act, render, screen, waitFor } from '@testing-library/react';
import { createMemoryRouter, RouterProvider } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import { Providers, routes } from '../src/App';
import { sessionAmount } from '../src/lib/format';
import { makeQueryClient } from '../src/lib/queries';
import { endedSession, pageName, sameEmail, sessionEndedNotice } from '../src/lib/sessionEnd';
import { MOCK_PASSWORD, MOCK_TOTP } from '../src/mocks/db';
import { createMockApi } from '../src/mocks/handlers';
import { returnPath } from '../src/pages/Login';
import { renderApp, server, useMockServer } from './helpers';

useMockServer();

const ENDED_ON_PAYMENTS = 'Your session ended, so we signed you out. Sign in again to go back to Payments.';

/** Signs in on `path`, then the server forgets the session: the next request answers 401. */
async function endSessionOn(path: string, heading: string, email?: string) {
  const app = renderApp(path, { email });
  await screen.findByRole('heading', { level: 1, name: heading });
  app.mock.db().session = null;
  await app.client.invalidateQueries();
  await screen.findByRole('heading', { name: 'Sign in' });
  return app;
}

describe('a session that ends by itself', () => {
  it('says so on the sign-in page, with the email filled in, and goes back to the page', async () => {
    const { user } = await endSessionOn('/payments', 'Payments');
    const notice = screen.getByText(ENDED_ON_PAYMENTS).closest('p') as HTMLElement;
    expect(notice).toHaveAttribute('role', 'status');
    expect(notice).toHaveClass('callout', 'notice');
    expect(screen.getByLabelText('Email')).toHaveValue('owner@acme.studio');
    await user.type(screen.getByLabelText('Password'), MOCK_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Payments' })).toBeInTheDocument();
    expect(screen.queryByText(ENDED_ON_PAYMENTS)).toBeNull();
  });

  it('goes back to the page after the two-factor step too', async () => {
    const { user } = await endSessionOn('/customers', 'Customers', 'secure@acme.studio');
    expect(screen.getByText('Your session ended, so we signed you out. Sign in again to go back to Customers.')).toBeInTheDocument();
    expect(screen.getByLabelText('Email')).toHaveValue('secure@acme.studio');
    await user.type(screen.getByLabelText('Password'), MOCK_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    await user.type(await screen.findByLabelText('Code'), MOCK_TOTP);
    await user.click(screen.getByRole('button', { name: 'Verify' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Customers' })).toBeInTheDocument();
  });

  it('sends another account to where a sign-in starts, not to the page of the one that ended', async () => {
    const ses = createMockApi().db().sessions.find((s) => s.livemode)!;
    const { user, mock } = await endSessionOn(`/payments/${ses.id}`, sessionAmount(ses.amount));
    expect(screen.getByText('Your session ended, so we signed you out. Sign in again to go back to the payment.')).toBeInTheDocument();
    mock.addSignup('Other Studio', 'other@other.studio', MOCK_PASSWORD, true).merchant.live_enabled = true;
    await user.clear(screen.getByLabelText('Email'));
    await user.type(screen.getByLabelText('Email'), 'other@other.studio');
    await user.type(screen.getByLabelText('Password'), MOCK_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Get started' })).toBeInTheDocument();
    expect(screen.queryByText('Page not found')).toBeNull();
    expect(endedSession()).toBeNull();

    // A later sign-in in this tab is a plain one: no notice, no email, home after it.
    await user.click(screen.getAllByRole('button', { name: 'Sign out' })[0]!);
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.queryByText(/Your session ended/)).toBeNull();
    expect(screen.getByLabelText('Email')).toHaveValue('');
    await user.type(screen.getByLabelText('Email'), 'owner@acme.studio');
    await user.type(screen.getByLabelText('Password'), MOCK_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Payment links' })).toBeInTheDocument();
  });

  it('sends another account to where a sign-in starts after the two-factor step too', async () => {
    const { user } = await endSessionOn('/customers', 'Customers');
    expect(screen.getByLabelText('Email')).toHaveValue('owner@acme.studio');
    await user.clear(screen.getByLabelText('Email'));
    await user.type(screen.getByLabelText('Email'), 'secure@acme.studio');
    await user.type(screen.getByLabelText('Password'), MOCK_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    await user.type(await screen.findByLabelText('Code'), MOCK_TOTP);
    await user.click(screen.getByRole('button', { name: 'Verify' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Payment links' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { level: 1, name: 'Customers' })).toBeNull();
  });

  it('forgets a session that ends on /verify-email, so a later redirect to sign-in names no page', async () => {
    // On a router the test can move, as the back button would, to a page that needs a session.
    const mock = createMockApi();
    mock.addSignup('Late shop', 'late@example.com', MOCK_PASSWORD);
    mock.signIn('late@example.com');
    server.use(...mock.handlers);
    const client = makeQueryClient();
    client.setDefaultOptions({ queries: { retry: false, staleTime: 0 }, mutations: { retry: false } });
    const router = createMemoryRouter(routes, { initialEntries: ['/verify-email'] });
    render(
      <Providers client={client} mode="live">
        <RouterProvider router={router} />
      </Providers>,
    );
    expect(await screen.findByText(/Signed in as late@example\.com\./)).toBeInTheDocument();

    mock.db().session = null;
    await client.invalidateQueries();
    await waitFor(() => expect(screen.queryByText(/Signed in as/)).toBeNull());
    expect(endedSession()).toBeNull();

    await act(() => router.navigate('/payments'));
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.queryByText(/Your session ended/)).toBeNull();
    expect(screen.getByLabelText('Email')).toHaveValue('');
  });

  it('says nothing after a sign-out', async () => {
    const { user } = renderApp('/payments');
    await screen.findByRole('heading', { level: 1, name: 'Payments' });
    await user.click(screen.getAllByRole('button', { name: 'Sign out' })[0]!);
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.queryByText(/Your session ended/)).toBeNull();
    expect(screen.getByLabelText('Email')).toHaveValue('');
  });

  it('says nothing on a first visit', async () => {
    renderApp('/payments', { signedIn: false });
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.queryByText(/Your session ended/)).toBeNull();
    expect(screen.getByLabelText('Email')).toHaveValue('');
  });

  it('still drops what the old session cached', async () => {
    const { client } = await endSessionOn('/payments', 'Payments');
    await waitFor(() => expect(client.getQueryCache().getAll().map((q) => q.queryKey[0])).toEqual(['me']));
  });
});

describe('the notice and the way back', () => {
  it('names the page from where it was, or just asks to sign in again', () => {
    expect(pageName('/payments')).toBe('Payments');
    expect(pageName('/payments/ses_123')).toBe('the payment');
    expect(pageName('/links')).toBe('Payment links');
    expect(pageName('/settings/wallets')).toBe('Settings');
    expect(pageName('/developers/webhooks/we_1')).toBe('the webhook deliveries');
    expect(pageName('/nowhere')).toBeNull();
    expect(sessionEndedNotice(null)).toBe('Your session ended, so we signed you out. Sign in again to continue.');
  });

  it('knows the same account by its email, trimmed and in any case', () => {
    expect(sameEmail(' Owner@Acme.Studio ', 'owner@acme.studio')).toBe(true);
    expect(sameEmail('other@other.studio', 'owner@acme.studio')).toBe(false);
    expect(sameEmail('owner@acme.studio', undefined)).toBe(false);
    expect(sameEmail(undefined, undefined)).toBe(false);
  });

  it('only goes back to a path of this app', () => {
    expect(returnPath('/payments?status=open')).toBe('/payments?status=open');
    expect(returnPath('/links/plink_1')).toBe('/links/plink_1');
    for (const bad of [undefined, null, 42, '', 'payments', 'https://evil.example', '//evil.example', '/\\evil.example', '/\t/evil.example', '/login', '/login?x=1', '/signup'])
      expect(returnPath(bad)).toBe('/');
  });
});
