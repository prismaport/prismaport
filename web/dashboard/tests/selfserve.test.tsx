import { screen, waitFor, within } from '@testing-library/react';
import { delay, http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { MOCK_CODE, MOCK_PASSWORD, MOCK_RESET_TOKEN, MOCK_TOTP } from '../src/mocks/db';
import { renderApp, useMockServer } from './helpers';

useMockServer();

const NEW_PASSWORD = 'A long new password 1';

describe('sign-in page', () => {
  it('links to sign-up and to the password reset, without the pilot note', async () => {
    renderApp('/login', { signedIn: false });
    expect(await screen.findByRole('link', { name: 'Create an account' })).toHaveAttribute('href', '/signup');
    expect(screen.getByRole('link', { name: 'Forgot your password?' })).toHaveAttribute('href', '/forgot-password');
    expect(screen.queryByText(/created by Prismaport during the pilot/)).toBeNull();
  });
});

describe('sign-up', () => {
  it('checks every field before sending and focuses the first problem', async () => {
    const posts: unknown[] = [];
    const { user } = renderApp('/signup', {
      signedIn: false,
      handlers: [http.post('/v1/dashboard/auth/signup', async ({ request }) => void posts.push(await request.clone().json()))],
    });
    await user.click(await screen.findByRole('button', { name: 'Create account' }));
    expect(screen.getByLabelText('Business name')).toHaveFocus();
    expect(screen.getByLabelText('Business name')).toHaveAccessibleDescription(/Enter the name of your business or project\./);
    expect(screen.getByLabelText('Email')).toHaveAccessibleDescription(/Enter an email address such as you@example\.com\./);
    expect(screen.getByLabelText('Password')).toHaveAccessibleDescription(/Add 8 more characters, an uppercase letter, a lowercase letter, a number and a special character\./);
    await user.type(screen.getByLabelText('Business name'), 'Acme');
    await user.type(screen.getByLabelText('Email'), 'acme@example.com');
    await user.type(screen.getByLabelText('Password'), 'short');
    await user.click(screen.getByRole('button', { name: 'Create account' }));
    expect(screen.getByLabelText('Password')).toHaveFocus();
    expect(posts).toEqual([]);
    expect(screen.getByText('Live and Test mode both open once you confirm your email.')).toBeInTheDocument();
  });

  it('shows a password the server found in a breach on the field', async () => {
    const posts: unknown[] = [];
    const { user } = renderApp('/signup', {
      signedIn: false,
      handlers: [
        http.post('/v1/dashboard/auth/signup', () => {
          posts.push(1);
          return HttpResponse.json({ error: { code: 'breached_password', message: 'server text', param: 'password' } }, { status: 400 });
        }),
      ],
    });
    await user.type(await screen.findByLabelText('Business name'), 'Acme');
    await user.type(screen.getByLabelText('Email'), 'acme@example.com');
    await user.type(screen.getByLabelText('Password'), NEW_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Create account' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('This password appeared in a data breach. Choose one you have not used anywhere else.');
    expect(screen.getByLabelText('Password')).toHaveFocus();
    expect(posts).toHaveLength(1);
    expect(screen.getByRole('heading', { name: 'Create your account' })).toBeInTheDocument();
  });

  it('asks for another password when confirming refuses the one from sign-up', async () => {
    const { user } = renderApp('/signup', {
      signedIn: false,
      handlers: [http.post('/v1/dashboard/auth/verify-email', () => HttpResponse.json({ error: { code: 'breached_password', message: 'server text', param: 'password' } }, { status: 400 }))],
    });
    await user.type(await screen.findByLabelText('Business name'), 'Acme');
    await user.type(screen.getByLabelText('Email'), 'acme2@example.com');
    await user.type(screen.getByLabelText('Password'), NEW_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Create account' }));
    expect(await screen.findByRole('heading', { name: 'Confirm your email' })).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).toBeNull();
    await user.type(screen.getByRole('textbox', { name: 'Confirmation code' }), MOCK_CODE);
    expect(await screen.findByLabelText('Password')).toBeInTheDocument();
    expect(screen.getByText('This password appeared in a data breach. Choose one you have not used anywhere else.')).toBeInTheDocument();
  });

  it('keeps the password field hidden while the new account signs in after the code', async () => {
    // A slow sign-in spans a tick of the resend countdown, which renders the screen again.
    const { user } = renderApp('/signup', {
      signedIn: false,
      handlers: [
        http.post('/v1/dashboard/auth/login', async () => {
          await delay(3000);
        }),
      ],
    });
    await user.type(await screen.findByLabelText('Business name'), 'Slow');
    await user.type(screen.getByLabelText('Email'), 'slow@example.com');
    await user.type(screen.getByLabelText('Password'), NEW_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Create account' }));
    expect(await screen.findByRole('heading', { name: 'Confirm your email' })).toBeInTheDocument();
    await user.type(screen.getByRole('textbox', { name: 'Confirmation code' }), MOCK_CODE);
    const before = document.getElementById('resend-timer')?.textContent;
    await waitFor(() => expect(document.getElementById('resend-timer')?.textContent).not.toBe(before), { timeout: 2800 });
    expect(screen.queryByLabelText('Password')).toBeNull();
    expect(await screen.findByText('Email confirmed. Opening your dashboard.')).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).toBeNull();
  });

  it('creates the account, confirms the email and opens Get started in Live mode', async () => {
    const { user } = renderApp('/signup', { signedIn: false });
    await user.type(await screen.findByLabelText('Business name'), '  Nova   Coffee ');
    await user.type(screen.getByLabelText('Email'), 'Hello@Nova.Example');
    await user.type(screen.getByLabelText('Password'), NEW_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Create account' }));

    expect(await screen.findByRole('heading', { name: 'Confirm your email' })).toBeInTheDocument();
    expect(screen.getByText('hello@nova.example')).toBeInTheDocument();
    expect(screen.getByText(/It works for 15 minutes\./)).toBeInTheDocument();
    const resend = screen.getByRole('button', { name: 'Resend code' });
    expect(resend).toBeDisabled();
    expect(resend).toHaveAccessibleDescription(/^in (1:00|0:59)$/);
    expect(screen.getByText("Didn't get it? Check spam.")).toBeInTheDocument();
    const code = screen.getByRole('textbox', { name: 'Confirmation code' });
    await waitFor(() => expect(code).toHaveFocus());
    // Six digits send themselves: a wrong code is answered with the tries left, then cleared.
    await user.type(code, '111111');
    expect(await screen.findByText("That code isn't right. 4 tries left.")).toBeInTheDocument();
    expect(code).toHaveAttribute('aria-invalid', 'true');
    expect(code).toHaveAccessibleDescription("That code isn't right. 4 tries left.");
    await waitFor(() => expect(code).toHaveValue(''));
    await waitFor(() => expect(code).toHaveFocus());
    await user.type(code, MOCK_CODE);

    expect(await screen.findByText('Email confirmed. Opening your dashboard.')).toBeInTheDocument();
    expect(document.querySelector('.code-field')).toHaveAttribute('data-status', 'confirmed');
    // The sign-up's password is used up, but the pause before leaving asks for nothing.
    expect(screen.queryByLabelText('Password')).toBeNull();
    expect(await screen.findByRole('heading', { level: 1, name: 'Get started' })).toBeInTheDocument();
    expect(screen.queryByTestId('test-strip')).toBeNull();
    expect(screen.getAllByText('Nova Coffee')[0]).toBeInTheDocument();
    expect(screen.getByTestId('step-start')).toBeInTheDocument();
  });

  it('sends a signed-in user who has not confirmed to the confirmation screen, and can sign out', async () => {
    const { user, mock } = renderApp('/links', { signedIn: false });
    mock.addSignup('Late shop', 'late@example.com', NEW_PASSWORD);
    mock.signIn('late@example.com');
    expect(await screen.findByRole('heading', { name: 'Confirm your email' })).toBeInTheDocument();
    expect(screen.getByText(/Signed in as late@example\.com\./)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'Confirmation code' })).toHaveFocus());
    expect(screen.getByRole('button', { name: 'Resend code' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: 'Resend code' }));
    const sent = await screen.findByText('New code sent. Check your inbox.');
    expect(sent.closest('.code-message')).toHaveAttribute('data-tone', 'ok');
    expect(screen.getByRole('button', { name: 'Resend code' })).toBeDisabled();
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'Confirmation code' })).toHaveFocus());
    await user.click(screen.getByRole('button', { name: 'Sign out' }));
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
  });

  it('confirms from a fresh browser with the password, then opens the dashboard', async () => {
    const { user, mock } = renderApp('/verify-email', { signedIn: false });
    mock.addSignup('Other shop', 'other@example.com', NEW_PASSWORD);
    await user.type(await screen.findByLabelText('Email'), 'other@example.com');
    const code = screen.getByRole('textbox', { name: 'Confirmation code' });
    // Without a session or the sign-up's password the code is tied to the password typed here
    // (ADR 0008 C-1); nothing is sent without it.
    await user.type(code, MOCK_CODE);
    expect(await screen.findByText('Enter the password of this account.')).toBeInTheDocument();
    expect(screen.getByLabelText('Password')).toHaveFocus();
    await user.type(screen.getByLabelText('Password'), NEW_PASSWORD);
    await user.clear(code);
    // This page did not send the code, so it knows neither the tries left nor the code's age.
    await user.type(code, '135791');
    expect(await screen.findByText("That code isn't right, or it expired. Check the newest email, or send a new code.")).toBeInTheDocument();
    await waitFor(() => expect(code).toHaveValue(''));
    await user.click(code);
    await user.paste('246 810');
    expect(await screen.findByText('Email confirmed. Opening your dashboard.')).toBeInTheDocument();
    expect(await screen.findByRole('heading', { level: 1, name: 'Get started' })).toBeInTheDocument();
  });

  it('asks for every digit before sending, without calling the server', async () => {
    const posts: unknown[] = [];
    const { user, mock } = renderApp('/verify-email', {
      signedIn: false,
      handlers: [http.post('/v1/dashboard/auth/verify-email', async ({ request }) => void posts.push(await request.clone().json()))],
    });
    mock.addSignup('Short shop', 'short@example.com', NEW_PASSWORD);
    await user.type(await screen.findByLabelText('Email'), 'short@example.com');
    await user.type(screen.getByLabelText('Password'), NEW_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Confirm email' }));
    expect(screen.getByText('Enter the 6-digit code from the email.')).toBeInTheDocument();
    await user.type(screen.getByRole('textbox', { name: 'Confirmation code' }), '246');
    await user.click(screen.getByRole('button', { name: 'Confirm email' }));
    expect(screen.getByText('Type all 6 digits (3 of 6).')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'Confirmation code' })).toHaveFocus());
    expect(posts).toEqual([]);
  });

  it('five wrong codes lock the field until a new code is sent', async () => {
    const { user } = renderApp('/signup', { signedIn: false });
    await user.type(await screen.findByLabelText('Business name'), 'Locked shop');
    await user.type(screen.getByLabelText('Email'), 'locked@example.com');
    await user.type(screen.getByLabelText('Password'), NEW_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Create account' }));
    const code = await screen.findByRole('textbox', { name: 'Confirmation code' });
    for (const left of ['4 tries', '3 tries', '2 tries', '1 try']) {
      await user.type(code, '000000');
      expect(await screen.findByText(`That code isn't right. ${left} left.`)).toBeInTheDocument();
      await waitFor(() => expect(code).toHaveValue(''));
    }
    await user.type(code, '000000');
    expect(await screen.findByText('Too many wrong codes. Send a new one in a few minutes.')).toBeInTheDocument();
    expect(code).toBeDisabled();
    expect(document.querySelector('.code-field')).toHaveAttribute('data-status', 'locked');
    expect(screen.getByRole('button', { name: 'Confirm email' })).toBeDisabled();
  });

  it('a code older than 15 minutes is called expired', async () => {
    const { user } = renderApp('/signup', { signedIn: false });
    await user.type(await screen.findByLabelText('Business name'), 'Slow shop');
    await user.type(screen.getByLabelText('Email'), 'slow@example.com');
    await user.type(screen.getByLabelText('Password'), NEW_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Create account' }));
    const code = await screen.findByRole('textbox', { name: 'Confirmation code' });
    const now = vi.spyOn(Date, 'now').mockReturnValue(Date.now() + 16 * 60_000);
    try {
      await user.type(code, '999999');
      expect(await screen.findByText('This code expired. Send a new one.')).toBeInTheDocument();
      expect(document.querySelector('.code-field')).toHaveAttribute('data-status', 'expired');
      expect(code).toHaveValue('999999');
    } finally {
      now.mockRestore();
    }
  });

  it('too many tries in a minute lock the field', async () => {
    const { user, mock } = renderApp('/verify-email', {
      signedIn: false,
      handlers: [
        http.post('/v1/dashboard/auth/verify-email', () =>
          HttpResponse.json({ error: { code: 'rate_limited', message: 'Too many attempts. Wait a minute and try again.' } }, { status: 429, headers: { 'Retry-After': '40' } }),
        ),
      ],
    });
    mock.addSignup('Busy shop', 'busy@example.com', NEW_PASSWORD);
    await user.type(await screen.findByLabelText('Email'), 'busy@example.com');
    await user.type(screen.getByLabelText('Password'), NEW_PASSWORD);
    await user.type(screen.getByRole('textbox', { name: 'Confirmation code' }), MOCK_CODE);
    expect(await screen.findByText('Too many wrong codes. Send a new one in a few minutes.')).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Confirmation code' })).toBeDisabled();
  });
});

describe('password reset', () => {
  const LINK = `/reset-password#token=${MOCK_RESET_TOKEN}`;
  /** Opens the reset link of a link sent to email. */
  const openLink = (email = 'owner@acme.studio', totp = false) => {
    const app = renderApp(LINK, { signedIn: false });
    const u = app.mock.db().users.find((x) => x.email === email)!;
    u.reset_link = true;
    u.totp_enabled = totp;
    return app;
  };

  it('says the link was sent, whatever the email', async () => {
    const { user } = renderApp('/login', { signedIn: false });
    await user.type(await screen.findByLabelText('Email'), 'nobody@example.com');
    await user.click(screen.getByRole('link', { name: 'Forgot your password?' }));
    expect(await screen.findByRole('heading', { name: 'Reset your password' })).toBeInTheDocument();
    expect(screen.getByLabelText('Email')).toHaveValue('nobody@example.com');
    await user.click(screen.getByRole('button', { name: 'Send reset link' }));

    expect(await screen.findByRole('heading', { name: 'Check your email' })).toHaveFocus();
    expect(screen.getByText(/We sent a link to/)).toHaveTextContent(
      'We sent a link to nobody@example.com. Open it to choose a new password. It works for 30 minutes, once.',
    );
    expect(screen.getByRole('button', { name: 'Send again' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: 'Use another email' }));
    expect(await screen.findByLabelText('Email')).toHaveValue('nobody@example.com');
  });

  it('the link sets a new password', async () => {
    const { user } = openLink();
    expect(await screen.findByRole('heading', { name: 'Choose a new password' })).toBeInTheDocument();
    expect(screen.getByText(/For the account/)).toHaveTextContent('For the account owner@acme.studio.');
    expect(screen.queryByLabelText('Authenticator code')).toBeNull();
    await user.type(screen.getByLabelText('New password'), 'short');
    await user.click(screen.getByRole('button', { name: 'Save new password' }));
    expect(screen.getByLabelText('New password')).toHaveFocus();
    expect(screen.getByLabelText('New password')).toHaveAccessibleDescription(/Add 3 more characters, an uppercase letter, a number and a special character\./);
    await user.clear(screen.getByLabelText('New password'));
    await user.type(screen.getByLabelText('New password'), NEW_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Save new password' }));

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.getByText(/Your password was changed and every session was signed out\./).closest('[role=status]')).not.toBeNull();
    expect(screen.getByLabelText('Email')).toHaveValue('owner@acme.studio');
    await user.type(screen.getByLabelText('Password'), MOCK_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Email or password is incorrect.');
    await user.clear(screen.getByLabelText('Password'));
    await user.type(screen.getByLabelText('Password'), NEW_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Payment links' })).toBeInTheDocument();
  });

  it('asks for the authenticator code when 2FA is on', async () => {
    const { user } = openLink('owner@acme.studio', true);
    await user.type(await screen.findByLabelText('New password'), NEW_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Save new password' }));
    const code = screen.getByLabelText('Authenticator code');
    await waitFor(() => expect(code).toHaveFocus());
    expect(code).toHaveAccessibleDescription(/Enter the 6-digit code from your authenticator app\./);
    await user.type(code, '000000');
    await user.click(screen.getByRole('button', { name: 'Save new password' }));
    expect(await screen.findByText('That code is not valid. Enter the current code from your authenticator app.')).toBeInTheDocument();
    expect(code).toHaveValue('');
    await user.type(code, MOCK_TOTP);
    await user.click(screen.getByRole('button', { name: 'Save new password' }));
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
  });

  it('a link that does not work offers a new one', async () => {
    const { user } = renderApp('/reset-password#token=not-the-link', { signedIn: false });
    expect(await screen.findByRole('heading', { name: 'This link no longer works' })).toBeInTheDocument();
    await user.click(screen.getByRole('link', { name: 'Send a new link' }));
    expect(await screen.findByRole('heading', { name: 'Reset your password' })).toBeInTheDocument();
  });

  it('a link spent meanwhile says so when saving', async () => {
    const { user, mock } = openLink();
    await user.type(await screen.findByLabelText('New password'), NEW_PASSWORD);
    mock.db().users.find((x) => x.email === 'owner@acme.studio')!.reset_link = false;
    await user.click(screen.getByRole('button', { name: 'Save new password' }));
    expect(await screen.findByRole('heading', { name: 'This link no longer works' })).toBeInTheDocument();
  });
});

describe('a new account', () => {
  it('opens Live mode at once', async () => {
    const { mock } = renderApp('/overview', { signedIn: false, mode: 'live' });
    mock.addSignup('Live shop', 'live@example.com', NEW_PASSWORD, true);
    mock.signIn('live@example.com');
    await screen.findByRole('heading', { level: 1, name: 'Overview' });
    expect(screen.queryByTestId('review-strip')).toBeNull();
  });

  it('opens a new account on Live mode, Get started, with the item in the menu', async () => {
    const { mock } = renderApp('/', { signedIn: false });
    mock.addSignup('New shop', 'new@example.com', NEW_PASSWORD, true);
    mock.signIn('new@example.com');
    await screen.findByRole('heading', { level: 1, name: 'Get started' });
    expect(screen.queryByTestId('test-strip')).toBeNull();
    expect(within(screen.getByRole('navigation', { name: 'Main' })).getByRole('link', { name: /Get started/ })).toBeInTheDocument();
    expect(localStorage.getItem('prismaport.dashboard.mode')).not.toBe('test');
  });

  it('leaves an account that has products on Live mode, without Get started', async () => {
    renderApp('/');
    await screen.findByRole('heading', { level: 1 });
    expect(screen.queryByRole('heading', { level: 1, name: 'Get started' })).toBeNull();
    expect(within(screen.getByRole('navigation', { name: 'Main' })).queryByRole('link', { name: /Get started/ })).toBeNull();
    expect(screen.queryByTestId('test-strip')).toBeNull();
    expect(localStorage.getItem('prismaport.dashboard.mode')).toBeNull();
  });

  it('keeps a browser that chose Live mode there with no live wallet, on Get started', async () => {
    const { mock } = renderApp('/', { signedIn: false, mode: 'live' });
    mock.addSignup('Chosen', 'chosen@example.com', NEW_PASSWORD, true);
    mock.signIn('chosen@example.com');
    await screen.findByRole('heading', { level: 1, name: 'Get started' });
    expect(screen.queryByTestId('test-strip')).toBeNull();
  });

  it('an older account with Live mode closed says so', async () => {
    const { user, mock } = renderApp('/overview', { signedIn: false, mode: 'live' });
    mock.addSignup('Review shop', 'review@example.com', NEW_PASSWORD, true).merchant.live_enabled = false;
    mock.signIn('review@example.com');
    const strip = await screen.findByTestId('review-strip');
    expect(strip).toHaveTextContent('Live payments open after a review. Until then, use Test mode.');
    await user.click(within(strip).getByRole('button', { name: 'Switch to Test mode' }));
    expect(await screen.findByTestId('test-strip')).toBeInTheDocument();
    expect(screen.queryByTestId('review-strip')).toBeNull();
  });

  it('a live change while Live mode is closed shows why it was refused', async () => {
    const { mock } = renderApp('/settings/networks', { signedIn: false, mode: 'live' });
    mock.addSignup('Early', 'early@example.com', NEW_PASSWORD, true).merchant.live_enabled = false;
    mock.signIn('early@example.com');
    const res = await fetch('/v1/dashboard/products?mode=live', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': 'csrf-test-token' },
      body: JSON.stringify({ name: 'x', price_type: 'fixed', amount: '25.00' }) });
    expect(res.status).toBe(403);
    expect(((await res.json()) as { error: { code: string } }).error.code).toBe('live_review_pending');
  });
});
