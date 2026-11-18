import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { MOCK_PASSWORD, MOCK_TOTP } from '../src/mocks/db';
import { renderApp, useMockServer } from './helpers';

useMockServer();

describe('sign in', () => {
  it('sends people without a session to the login page', async () => {
    renderApp('/payments', { signedIn: false });
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
  });

  it('waits on the session behind the brand mark, not a line of text', async () => {
    let answer!: () => void;
    const answered = new Promise<void>((r) => (answer = r));
    renderApp('/payments', {
      signedIn: false,
      handlers: [http.get('/v1/dashboard/me', async () => {
        await answered;
        return HttpResponse.json({ error: { code: 'unauthenticated', message: 'Sign in.' } }, { status: 401 });
      })],
    });
    const label = await screen.findByText('Loading your dashboard.');
    expect(label).toHaveClass('sr-only');
    const status = label.closest('[role=status]');
    expect(status).toHaveClass('boot-splash');
    expect(status?.querySelector('img')).not.toBeNull();
    answer();
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
  });

  it('says the same thing for any wrong email or password', async () => {
    const { user } = renderApp('/login', { signedIn: false });
    await user.type(await screen.findByLabelText('Email'), 'nobody@acme.studio');
    await user.type(screen.getByLabelText('Password'), 'wrong');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Email or password is incorrect.');
  });

  it('goes to the code step after the right password when 2FA is on, with no error', async () => {
    const { user } = renderApp('/login', { signedIn: false });
    await user.type(await screen.findByLabelText('Email'), 'secure@acme.studio');
    await user.type(screen.getByLabelText('Password'), MOCK_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    const code = await screen.findByLabelText('Code');
    expect(code).toHaveFocus();
    expect(screen.getByText('Two-step sign-in is on for secure@acme.studio. Enter the 6-digit code from your authenticator app.')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
    await user.type(code, '000000');
    await user.click(screen.getByRole('button', { name: 'Verify' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('That code is not valid. Enter the current code from your authenticator app.');
    await user.clear(code);
    await user.type(code, MOCK_TOTP);
    await user.click(screen.getByRole('button', { name: 'Verify' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Payment links' })).toBeInTheDocument();
  });

  it('stays on the password step with a wrong password on a 2FA account', async () => {
    const { user } = renderApp('/login', { signedIn: false });
    await user.type(await screen.findByLabelText('Email'), 'secure@acme.studio');
    await user.type(screen.getByLabelText('Password'), 'wrong password!');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Email or password is incorrect.');
    expect(screen.queryByLabelText('Code')).toBeNull();
    expect(screen.queryByRole('button', { name: /authenticator/ })).toBeNull();
  });

  it('a session that ends with a 401 never shows its data to the next account signing in', async () => {
    const { mock, client, user } = renderApp('/payments');
    expect((await screen.findAllByTestId('session-row')).length).toBeGreaterThan(0);
    mock.addSignup('Other Studio', 'other@other.studio', MOCK_PASSWORD, true);
    mock.db().session = null;

    await client.invalidateQueries();
    // The sign-in page offers the email of the session that ended; another account replaces it.
    await user.clear(await screen.findByLabelText('Email'));
    await user.type(screen.getByLabelText('Email'), 'other@other.studio');
    await user.type(screen.getByLabelText('Password'), MOCK_PASSWORD);
    let leaked = false;
    const watch = new MutationObserver(() => {
      if (document.querySelector('[data-testid=session-row]') || document.body.textContent?.includes('Founder plan')) leaked = true;
    });
    watch.observe(document.body, { childList: true, subtree: true });
    await user.click(screen.getByRole('button', { name: 'Sign in' }));

    // Another account starts where a sign-in does, not on the page of the session that ended.
    expect(await screen.findByRole('heading', { level: 1, name: 'Get started' })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('other@other.studio')).toBeInTheDocument());
    watch.disconnect();
    expect(leaked).toBe(false);
  });

  it('signs out', async () => {
    const { user } = renderApp('/overview');
    await screen.findByRole('heading', { level: 1, name: 'Overview' });
    await user.click(screen.getAllByRole('button', { name: 'Sign out' })[0]!);
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
  });

  it('stays signed in and says so when the server could not end the session', async () => {
    const { user, mock } = renderApp('/overview', {
      handlers: [http.post('/v1/dashboard/auth/logout', () => HttpResponse.json({ error: { code: 'server_error', message: '' } }, { status: 500 }))],
    });
    await screen.findByRole('heading', { level: 1, name: 'Overview' });
    await user.click(screen.getAllByRole('button', { name: 'Sign out' })[0]!);
    expect(await screen.findByText('We could not sign you out. Try again.')).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 1, name: 'Overview' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Sign in' })).toBeNull();
    expect(mock.db().session).not.toBeNull();
  });
});

describe('the API reference button', () => {
  it('opens /docs/ next to the dashboard by default', async () => {
    renderApp('/developers/api-keys');
    await screen.findByRole('heading', { level: 1, name: 'Developers' });
    expect(screen.getByRole('link', { name: /API reference/ })).toHaveAttribute('href', '/docs/');
  });

  it('follows the prismaport-docs meta tag', async () => {
    const m = document.createElement('meta');
    m.name = 'prismaport-docs';
    m.content = 'https://docs.prismaport.com/';
    document.head.append(m);
    try {
      renderApp('/developers/webhooks');
      await screen.findByRole('heading', { level: 1, name: 'Developers' });
      expect(screen.getByRole('link', { name: /API reference/ })).toHaveAttribute('href', 'https://docs.prismaport.com/');
    } finally {
      m.remove();
    }
  });
});

describe('a page that cannot render', () => {
  it('a malformed amount shows the error inside the shell, keeps the navigation and recovers on the next page', async () => {
    const quiet = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    try {
      const { mock, user } = renderApp('/payments');
      for (const ses of mock.db().sessions) ses.amount = '-25.00';

      expect(await screen.findByRole('heading', { level: 1, name: 'This page could not be shown' })).toBeInTheDocument();
      expect(screen.getByRole('alert')).toHaveTextContent('Something went wrong on our side. Try again in a moment.');
      expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
      expect(screen.queryByText('Unexpected Application Error!')).not.toBeInTheDocument();

      await user.click(screen.getAllByRole('link', { name: /Developers/ })[0]!);
      expect(await screen.findByRole('heading', { level: 1, name: 'Developers' })).toBeInTheDocument();
      expect(screen.queryByText('This page could not be shown')).not.toBeInTheDocument();
    } finally {
      quiet.mockRestore();
    }
  });
});

describe('phone navigation sheet', () => {
  async function openSheet() {
    const r = renderApp('/overview');
    await screen.findByRole('heading', { level: 1, name: 'Overview' });
    const menu = screen.getByRole('button', { name: 'Open navigation' });
    await r.user.click(menu);
    const sheet = screen.getByRole('dialog', { name: 'Navigation' });
    return { ...r, menu, sheet };
  }

  it('moves focus in and keeps Tab and Shift+Tab inside the sheet', async () => {
    const { user, sheet } = await openSheet();
    expect(sheet.contains(document.activeElement)).toBe(true);

    within(sheet).getByRole('button', { name: 'Sign out' }).focus();
    await user.tab();
    expect(sheet.contains(document.activeElement)).toBe(true);

    within(sheet).getByRole('button', { name: 'Close navigation' }).focus();
    await user.tab({ shift: true });
    expect(sheet.contains(document.activeElement)).toBe(true);
  });

  it('hands focus back to the menu button when closed by its button', async () => {
    const { user, sheet, menu } = await openSheet();
    await user.click(within(sheet).getByRole('button', { name: 'Close navigation' }));
    expect(screen.queryByRole('dialog', { name: 'Navigation' })).not.toBeInTheDocument();
    expect(menu).toHaveFocus();
  });

  it('hands focus back to the menu button after following a link', async () => {
    const { user, sheet, menu } = await openSheet();
    await user.click(within(sheet).getByRole('link', { name: /Payments/ }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Payments' })).toBeInTheDocument();
    expect(screen.queryByRole('dialog', { name: 'Navigation' })).not.toBeInTheDocument();
    expect(menu).toHaveFocus();
  });

  it('closes on Escape and hands focus back', async () => {
    const { user, menu } = await openSheet();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: 'Navigation' })).not.toBeInTheDocument();
    expect(menu).toHaveFocus();
  });
});

describe('mode switch', () => {
  it('is always visible and marks Test mode on every page', async () => {
    const { user } = renderApp('/links');
    await screen.findAllByTestId('link-row');
    expect(screen.queryByTestId('test-strip')).not.toBeInTheDocument();
    const mode = within(screen.getAllByRole('group', { name: 'Mode' })[0]!);
    await user.click(mode.getByRole('radio', { name: 'Test' }));
    expect(await screen.findByTestId('test-strip')).toHaveTextContent('Test mode');
    await waitFor(() => expect(screen.getByRole('link', { name: 'Test product' })).toBeInTheDocument());
    expect(screen.queryByRole('link', { name: 'Founder plan' })).not.toBeInTheDocument();
    await user.click(screen.getAllByRole('link', { name: /Payments/ }).find((a) => a.getAttribute('href') === '/payments')!);
    expect(await screen.findByRole('heading', { level: 1, name: 'Payments' })).toBeInTheDocument();
    expect(screen.getByTestId('test-strip')).toBeInTheDocument();
  });
});

describe('overview', () => {
  it('shows net, received and fee per stablecoin and network, and links to look at', async () => {
    renderApp('/overview');
    const totals = await screen.findAllByTestId('total');
    expect(totals.length).toBe(3);
    expect(within(totals[0]!).getByText('Net to your wallets')).toBeInTheDocument();
    expect(within(totals[0]!).getByText('Prismaport fee')).toBeInTheDocument();
    expect(screen.getByText('Stops accepting payments within 3 days')).toBeInTheDocument();
    expect(screen.getByText('Payment limit reached')).toBeInTheDocument();
  });

  it('names the link of each recent payment, or says it came from the API', async () => {
    const { mock } = renderApp('/overview');
    const rows = await screen.findAllByTestId('recent-payment');
    const db = mock.db();
    const names = rows.map((r) => {
      const sessionId = r.getAttribute('href')!.split('/').pop()!;
      const ses = db.sessions.find((s) => s.id === sessionId)!;
      const link = db.links.find((l) => l.id === ses.link_id);
      return { row: r, name: link ? link.name ?? db.products.find((p) => p.id === link.product_id)!.name : null };
    });
    expect(names.some((n) => n.name)).toBe(true);
    for (const n of names) expect(n.row).toHaveTextContent(n.name ?? 'Created with the API');
  });
});
