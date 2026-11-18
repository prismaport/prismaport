import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import { formatUtc, lockedUntil, recoveryCodeValue, recoveryCodesFile, typedRecoveryCode } from '../src/lib/recovery';
import { MOCK_CANCEL_TOKEN, MOCK_LOST_TOKEN, MOCK_PASSWORD, MOCK_RECOVERY_CODES, MOCK_RESET_TOKEN, MOCK_TOTP } from '../src/mocks/db';
import { emailCode, renderApp, server, useMockServer } from './helpers';

useMockServer();

const SECURE = 'secure@acme.studio';
const OWNER = 'owner@acme.studio';
const NEW_PASSWORD = 'Brand-New Pass 42!';
const HOUR = 3_600_000;
const inHours = (h: number) => new Date(Date.now() + h * HOUR).toISOString();
const userOf = (mock: ReturnType<typeof renderApp>['mock'], email: string) => mock.db().users.find((u) => u.email === email)!;

afterEach(() => {
  sessionStorage.clear();
});

describe('recovery code helpers', () => {
  it('takes a code in any case, with or without the hyphen', () => {
    expect(recoveryCodeValue('k7m2q-9xqpa')).toBe('K7M2Q-9XQPA');
    expect(recoveryCodeValue(' k7m2q9xqpa ')).toBe('K7M2Q-9XQPA');
    expect(recoveryCodeValue('K7M2Q')).toBeNull();
    expect(recoveryCodeValue('K7M2Q-9XQP!')).toBeNull();
    expect(typedRecoveryCode('k7m2q 9xqpa!!')).toBe('K7M2Q9XQPA');
    expect(typedRecoveryCode('K7M2Q-9XQPA-EXTRA')).toBe('K7M2Q-9XQPA');
  });

  it('writes dates in UTC, whatever the browser zone', () => {
    expect(formatUtc('2026-11-12T14:32:00Z')).toBe('Nov 12, 2026 at 14:32 UTC');
    expect(formatUtc(null)).toBe('–');
  });

  it('knows a lock only while it is ahead', () => {
    const now = Date.parse('2026-11-12T00:00:00Z');
    expect(lockedUntil({ recovery_locked_until: '2026-11-13T00:00:00Z' }, now)).toBe('2026-11-13T00:00:00Z');
    expect(lockedUntil({ recovery_locked_until: '2026-11-11T00:00:00Z' }, now)).toBeNull();
    expect(lockedUntil({ recovery_locked_until: null }, now)).toBeNull();
  });

  it('puts one code per line in the downloaded file', () => {
    expect(recoveryCodesFile(['AAAAA-BBBBB', 'CCCCC-DDDDD'], 'a@b.co')).toBe('Prismaport recovery codes\nAccount: a@b.co\nEach code works once.\n\nAAAAA-BBBBB\nCCCCC-DDDDD\n');
  });
});

describe('change password', () => {
  async function open(email = OWNER, handlers: Parameters<typeof renderApp>[1] extends infer O ? (O extends { handlers?: infer H } ? H : never) : never = []) {
    const app = renderApp('/settings/account', { email, handlers });
    await app.user.click(await screen.findByRole('button', { name: 'Change password' }));
    const dialog = await screen.findByRole('dialog', { name: 'Change password' });
    return { ...app, d: within(dialog) };
  }

  it('shows the password card and asks for the current and the new password', async () => {
    const { d } = await open();
    expect(screen.getByText('We ask for it when you sign in, and before you add a wallet or create a live API key.')).toBeInTheDocument();
    expect(d.getByText('Other devices are signed out. This one stays signed in.')).toBeInTheDocument();
    expect(d.getByLabelText('Current password')).toBeInTheDocument();
    expect(d.getByLabelText('New password')).toBeInTheDocument();
    expect(d.queryByLabelText('Authenticator code')).toBeNull();
    expect(d.getByRole('link', { name: 'Forgot your password?' })).toHaveAttribute('href', '/forgot-password');
  });

  it('keeps a wrong current password in the dialog and names the field', async () => {
    const { user, d, mock } = await open();
    await user.type(d.getByLabelText('Current password'), 'not my password');
    await user.type(d.getByLabelText('New password'), NEW_PASSWORD);
    await user.click(d.getByRole('button', { name: 'Change password' }));
    await waitFor(() => expect(d.getByLabelText('Current password')).toHaveAccessibleDescription('That password is not correct.'));
    expect(userOf(mock, OWNER).password).toBeUndefined();
  });

  it('checks the new password before sending anything', async () => {
    const { user, d } = await open();
    await user.type(d.getByLabelText('Current password'), MOCK_PASSWORD);
    await user.type(d.getByLabelText('New password'), 'short');
    await user.click(d.getByRole('button', { name: 'Change password' }));
    expect(d.getByLabelText('New password')).toHaveAccessibleDescription(/more characters/);
  });

  it('shows the server refusal of the same password on the new password field', async () => {
    const { user, d } = await open(OWNER, [
      http.post('/v1/dashboard/auth/password/change', () =>
        HttpResponse.json({ error: { code: 'invalid_request', message: 'Choose a password different from the current one.', param: 'new_password' } }, { status: 400 }),
      ),
    ]);
    await user.type(d.getByLabelText('Current password'), MOCK_PASSWORD);
    await user.type(d.getByLabelText('New password'), NEW_PASSWORD);
    await user.click(d.getByRole('button', { name: 'Change password' }));
    await waitFor(() => expect(d.getByLabelText('New password')).toHaveAccessibleDescription(/Choose a password different from the current one\./));
  });

  it('changes the password and says the other devices were signed out', async () => {
    const { user, d, mock } = await open();
    await user.type(d.getByLabelText('Current password'), MOCK_PASSWORD);
    await user.type(d.getByLabelText('New password'), NEW_PASSWORD);
    await user.click(d.getByRole('button', { name: 'Change password' }));
    expect(await screen.findByText('Password changed. Other devices were signed out.')).toBeInTheDocument();
    expect(screen.queryByRole('dialog', { name: 'Change password' })).toBeNull();
    expect(userOf(mock, OWNER).password).toBe(NEW_PASSWORD);
  });

  it('asks for the authenticator code when 2FA is on, and shows a wrong one in its field', async () => {
    const { user, d, mock } = await open(SECURE);
    expect(d.getByText('Two-factor authentication is on for this account.')).toBeInTheDocument();
    await user.type(d.getByLabelText('Current password'), MOCK_PASSWORD);
    await user.type(d.getByLabelText('New password'), NEW_PASSWORD);
    await user.click(d.getByRole('button', { name: 'Change password' }));
    expect(d.getByLabelText('Authenticator code')).toHaveAccessibleDescription(/Enter the 6-digit code/);
    await user.type(d.getByLabelText('Authenticator code'), '000000');
    await user.click(d.getByRole('button', { name: 'Change password' }));
    await waitFor(() => expect(d.getByLabelText('Authenticator code')).toHaveAccessibleDescription(/That code is not valid\./));
    await user.clear(d.getByLabelText('Authenticator code'));
    await user.type(d.getByLabelText('Authenticator code'), MOCK_TOTP);
    await user.click(d.getByRole('button', { name: 'Change password' }));
    expect(await screen.findByText('Password changed. Other devices were signed out.')).toBeInTheDocument();
    expect(userOf(mock, SECURE).password).toBe(NEW_PASSWORD);
  });
});

describe('recovery codes', () => {
  it('shows ten codes after turning 2FA on and does not let the dialog close until they are saved', async () => {
    const { user, mock } = renderApp('/settings/account', { email: OWNER });
    await user.click(await screen.findByRole('button', { name: 'Turn on' }));
    const enroll = within(await screen.findByRole('dialog', { name: 'Turn on two-factor authentication' }));
    await emailCode(user, enroll);
    await user.click(enroll.getByRole('button', { name: 'Continue' }));
    await user.type(await enroll.findByLabelText('Code'), MOCK_TOTP);
    await user.click(enroll.getByRole('button', { name: 'Turn on' }));

    const dialog = await screen.findByRole('dialog', { name: 'Save your recovery codes' });
    const d = within(dialog);
    const codes = d.getAllByRole('listitem').map((x) => x.textContent);
    expect(codes).toHaveLength(10);
    expect(codes.every((c) => /^[A-Z0-9]{5}-[A-Z0-9]{5}$/.test(c ?? ''))).toBe(true);
    expect(codes).toEqual(userOf(mock, OWNER).recovery_codes);
    expect(d.getByRole('button', { name: 'Done' })).toBeDisabled();
    expect(d.getByRole('button', { name: 'Close' })).toBeDisabled();
    await user.keyboard('{Escape}');
    expect(screen.getByRole('dialog', { name: 'Save your recovery codes' })).toBeInTheDocument();
    await user.click(d.getByRole('checkbox', { name: 'I saved these codes' }));
    await user.click(d.getByRole('button', { name: 'Done' }));
    expect(screen.queryByRole('dialog', { name: 'Save your recovery codes' })).toBeNull();
    expect(await screen.findByText('10 of 10 left. Each one works once, if you lose your authenticator app.')).toBeInTheDocument();
  });

  it('shows no codes after replacing the app: the ones in hand stay valid', async () => {
    const { user, mock } = renderApp('/settings/account', { email: SECURE });
    await user.click(await screen.findByRole('button', { name: 'Replace app' }));
    const d = within(await screen.findByRole('dialog', { name: 'Replace your authenticator app' }));
    await user.type(d.getByLabelText('Current code'), MOCK_TOTP);
    await user.click(d.getByRole('button', { name: 'Continue' }));
    await user.type(await d.findByLabelText('Code'), MOCK_TOTP);
    await user.click(d.getByRole('button', { name: 'Use this app' }));
    expect(await screen.findByText('Authenticator app replaced')).toBeInTheDocument();
    expect(screen.queryByRole('dialog', { name: 'Save your recovery codes' })).toBeNull();
    expect(userOf(mock, SECURE).recovery_codes).toEqual(MOCK_RECOVERY_CODES);
  });

  it('replaces the current code with a recovery code when the app is lost', async () => {
    const { user, mock } = renderApp('/settings/account', { email: SECURE });
    await user.click(await screen.findByRole('button', { name: 'Replace app' }));
    const d = within(await screen.findByRole('dialog', { name: 'Replace your authenticator app' }));
    await user.click(d.getByRole('button', { name: 'Lost your app? Use a recovery code' }));
    expect(d.queryByLabelText('Current code')).toBeNull();
    await user.type(d.getByLabelText('Recovery code'), 't4hvn2jc8r');
    await user.click(d.getByRole('button', { name: 'Continue' }));
    expect(await d.findByRole('img', { name: /authenticator app/ })).toBeInTheDocument();
    expect(userOf(mock, SECURE).recovery_codes).toHaveLength(9);
  });

  it('counts the codes left and creates new ones after a real authenticator code', async () => {
    const { user, mock } = renderApp('/settings/account', { email: SECURE });
    expect(await screen.findByText('10 of 10 left. Each one works once, if you lose your authenticator app.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Create new codes' }));
    const d = within(await screen.findByRole('dialog', { name: 'Create new recovery codes' }));
    await user.click(d.getByRole('button', { name: 'Create new codes' }));
    expect(await d.findByText('Enter the 6-digit code from your authenticator app.')).toBeInTheDocument();
    expect(d.queryByLabelText('Password')).toBeNull();
    await user.type(d.getByLabelText('Code'), '000000');
    await user.click(d.getByRole('button', { name: 'Create new codes' }));
    await waitFor(() => expect(d.getByLabelText('Code')).toHaveAccessibleDescription(/That code is not valid\./));
    await user.clear(d.getByLabelText('Code'));
    await user.type(d.getByLabelText('Code'), MOCK_TOTP);
    await user.click(d.getByRole('button', { name: 'Create new codes' }));
    const shown = within(await screen.findByRole('dialog', { name: 'Save your recovery codes' }));
    expect(shown.getAllByRole('listitem')).toHaveLength(10);
    expect(userOf(mock, SECURE).recovery_codes).not.toContain(MOCK_RECOVERY_CODES[0]);
  });

  it('says there are no recovery codes on an account that turned 2FA on before they existed', async () => {
    const { user, mock } = renderApp('/settings/account', { email: SECURE });
    userOf(mock, SECURE).recovery_codes = [];
    await screen.findByText(/^On\. We ask for a code/);
    const callout = await screen.findByTestId('no-recovery-codes');
    expect(callout).toHaveTextContent('You have no recovery codes. If you lose your authenticator app, getting back in takes 72 hours.');
    await user.click(within(callout).getByRole('button', { name: 'Create recovery codes' }));
    expect(await screen.findByRole('dialog', { name: 'Create recovery codes' })).toBeInTheDocument();
  });

  it('focuses the code boxes when the turn-off dialog opens and gives focus back on cancel', async () => {
    const { user } = renderApp('/settings/account', { email: SECURE });
    const opener = await screen.findByRole('button', { name: 'Turn off' });
    await user.click(opener);
    const d = within(await screen.findByRole('dialog', { name: 'Turn off two-factor authentication' }));
    expect(d.getByLabelText('Code')).toHaveFocus();
    await user.click(d.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(opener).toHaveFocus();
  });

  it('turns 2FA off with a recovery code, and says what changes', async () => {
    const { user, mock } = renderApp('/settings/account', { email: SECURE });
    await user.click(await screen.findByRole('button', { name: 'Turn off' }));
    const d = within(await screen.findByRole('dialog', { name: 'Turn off two-factor authentication' }));
    expect(d.getByText(/Your recovery codes stop working\. We email you to confirm\./)).toBeInTheDocument();
    expect(d.queryByLabelText('Password')).toBeNull();
    await user.click(d.getByRole('button', { name: 'Lost your app? Use a recovery code' }));
    expect(d.getByRole('button', { name: 'Use an authenticator code' })).toHaveFocus();
    await user.type(d.getByLabelText('Recovery code'), '00000-00000');
    await user.click(d.getByRole('button', { name: 'Turn off' }));
    await waitFor(() => expect(d.getByLabelText('Recovery code')).toHaveAccessibleDescription(/not valid/));
    await user.clear(d.getByLabelText('Recovery code'));
    await user.type(d.getByLabelText('Recovery code'), MOCK_RECOVERY_CODES[2]!);
    await user.click(d.getByRole('button', { name: 'Turn off' }));
    expect(await screen.findByText('Two-factor authentication is off')).toBeInTheDocument();
    expect(userOf(mock, SECURE).totp_enabled).toBe(false);
    expect(userOf(mock, SECURE).recovery_codes).toBeUndefined();
  });
});

describe('signing in with a recovery code', () => {
  async function toCodeStep() {
    const app = renderApp('/login', { signedIn: false });
    await app.user.type(await screen.findByLabelText('Email'), SECURE);
    await app.user.type(screen.getByLabelText('Password'), MOCK_PASSWORD);
    await app.user.click(screen.getByRole('button', { name: 'Sign in' }));
    await screen.findByLabelText('Code');
    return app;
  }

  it('keeps the password step as it was: no recovery link, no foot link to the lost-app page', async () => {
    renderApp('/login', { signedIn: false });
    await screen.findByLabelText('Email');
    expect(screen.queryByRole('button', { name: 'Use a recovery code' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Get back into your account' })).toBeNull();
  });

  it('swaps the code for a recovery code and back, and links to the lost-app page', async () => {
    const { user } = await toCodeStep();
    expect(await screen.findByRole('heading', { name: 'Two-factor code' })).toBeInTheDocument();
    expect(screen.getByText(`Two-step sign-in is on for ${SECURE}. Enter the 6-digit code from your authenticator app.`)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Get back into your account' })).toHaveAttribute('href', '/lost-authenticator');
    await user.click(screen.getByRole('button', { name: 'Use a recovery code' }));
    expect(screen.getByRole('heading', { name: 'Recovery code' })).toBeInTheDocument();
    expect(screen.getByText(`Enter one of the recovery codes you saved for ${SECURE}. Each code works once.`)).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Recovery code' })).toHaveAttribute('placeholder', 'XXXXX-XXXXX');
    expect(screen.getByRole('link', { name: 'Get back into your account' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Use an authenticator code' }));
    expect(screen.getByRole('heading', { name: 'Two-factor code' })).toBeInTheDocument();
  });

  it('says a recovery code is wrong, then signs in with a right one and tells the card once', async () => {
    const { user, mock } = await toCodeStep();
    await user.click(screen.getByRole('button', { name: 'Use a recovery code' }));
    await user.type(screen.getByRole('textbox', { name: 'Recovery code' }), 'AAAAA-AAAAA');
    await user.click(screen.getByRole('button', { name: 'Verify' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('That recovery code is not valid or was already used.');
    expect(userOf(mock, SECURE).recovery_codes).toHaveLength(10);
    await user.clear(screen.getByRole('textbox', { name: 'Recovery code' }));
    await user.type(screen.getByRole('textbox', { name: 'Recovery code' }), 'k7m2q9xqpa');
    await user.click(screen.getByRole('button', { name: 'Verify' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Payment links' })).toBeInTheDocument();
    expect(userOf(mock, SECURE).recovery_codes).toHaveLength(9);
    expect(sessionStorage.getItem('prismaport.recovery-sign-in')).toBe('1');
  });

  it('after too many wrong codes, says to wait or use a recovery code', async () => {
    const { user } = await toCodeStep();
    server.use(
      http.post('/v1/dashboard/auth/login', () =>
        HttpResponse.json(
          { error: { code: 'rate_limited', message: 'Too many wrong codes. Wait 2 minutes before entering another code.' } },
          { status: 429, headers: { 'Retry-After': '90' } },
        ),
      ),
    );
    await user.type(screen.getByLabelText('Code'), '000000');
    await user.click(screen.getByRole('button', { name: 'Verify' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Too many wrong codes. Try again in 90 seconds, or use a recovery code.');
    expect(screen.getByRole('button', { name: 'Use a recovery code' })).toBeInTheDocument();
  });

  it('tells the two-factor card that a recovery code was used, and puts Replace app first', async () => {
    sessionStorage.setItem('prismaport.recovery-sign-in', '1');
    renderApp('/settings/account', { email: SECURE });
    const callout = await screen.findByTestId('recovery-sign-in');
    expect(callout).toHaveTextContent('You signed in with a recovery code. 10 of 10 left. If you lost your authenticator app, replace it now.');
    expect(screen.getByRole('button', { name: 'Replace app' })).toHaveClass('btn-primary');
  });

  it('a pending removal is cancelled by signing in with a recovery code', async () => {
    const { user, mock } = renderApp('/login', { signedIn: false });
    userOf(mock, SECURE).totp_removal_at = inHours(60);
    await user.type(await screen.findByLabelText('Email'), SECURE);
    await user.type(screen.getByLabelText('Password'), MOCK_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    await user.click(await screen.findByRole('button', { name: 'Use a recovery code' }));
    await user.type(screen.getByRole('textbox', { name: 'Recovery code' }), MOCK_RECOVERY_CODES[0]!);
    await user.click(screen.getByRole('button', { name: 'Verify' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Payment links' })).toBeInTheDocument();
    expect(userOf(mock, SECURE).totp_removal_at).toBeNull();
  });

  it('a reset link takes a recovery code in place of the authenticator code', async () => {
    const { user, mock } = renderApp(`/reset-password#token=${MOCK_RESET_TOKEN}`, { signedIn: false });
    const u = userOf(mock, SECURE);
    u.reset_link = true;
    await user.type(await screen.findByLabelText('New password'), NEW_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Lost your app? Use a recovery code' }));
    await user.type(screen.getByLabelText('Recovery code'), MOCK_RECOVERY_CODES[1]!);
    await user.click(screen.getByRole('button', { name: 'Save new password' }));
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(u.password).toBe(NEW_PASSWORD);
    expect(u.recovery_codes).toHaveLength(9);
  });
});

describe('lost authenticator app', () => {
  it('answers the same for every email', async () => {
    const { user, mock } = renderApp('/lost-authenticator', { signedIn: false });
    await user.click(await screen.findByRole('button', { name: 'Send link' }));
    expect(await screen.findByText('Enter an email address such as you@example.com.')).toBeInTheDocument();
    await user.type(screen.getByLabelText('Email'), 'nobody@example.com');
    await user.click(screen.getByRole('button', { name: 'Send link' }));
    expect(await screen.findByRole('heading', { name: 'Check your email' })).toHaveFocus();
    expect(screen.getByText('If that account has two-factor authentication on, a link is on its way. It works for 30 minutes.')).toBeInTheDocument();
    expect(mock.db().users.some((u) => u.lost_link)).toBe(false);
    expect(screen.getByRole('link', { name: 'Back to sign in' })).toHaveAttribute('href', '/login');
  });

  it('sends a link only for an account with 2FA on', async () => {
    const { user, mock } = renderApp('/lost-authenticator', { signedIn: false });
    await user.type(await screen.findByLabelText('Email'), SECURE);
    await user.click(screen.getByRole('button', { name: 'Send link' }));
    expect(await screen.findByRole('heading', { name: 'Check your email' })).toBeInTheDocument();
    expect(userOf(mock, SECURE).lost_link).toBe(true);
  });

  it('is reached from the code step of the sign-in page with the email filled in', async () => {
    const { user } = renderApp('/login', { signedIn: false });
    await user.type(await screen.findByLabelText('Email'), SECURE);
    await user.type(screen.getByLabelText('Password'), MOCK_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    await user.click(await screen.findByRole('link', { name: 'Get back into your account' }));
    expect(await screen.findByRole('heading', { name: 'Lost your authenticator app?' })).toBeInTheDocument();
    expect(screen.getByLabelText('Email')).toHaveValue(SECURE);
    expect(screen.getByText(/Saved your recovery codes\?/)).toBeInTheDocument();
  });

  const LINK = `/lost-authenticator/confirm#token=${MOCK_LOST_TOKEN}`;

  it('states what the wait means, needs the password, then schedules the removal and can cancel it', async () => {
    const { user, mock } = renderApp(LINK, { signedIn: false });
    const u = userOf(mock, SECURE);
    u.lost_link = true;
    expect(await screen.findByRole('heading', { name: 'Remove two-factor authentication' })).toBeInTheDocument();
    expect(screen.getByText(/For the account/)).toHaveTextContent(`For the account ${SECURE}.`);
    for (const line of [
      'It is removed after 72 hours. You can cancel until then.',
      'We email you now, 24 hours before it is removed, and when it is removed.',
      'Payments keep going to your wallet while you wait.',
      'Afterwards, adding a wallet and creating live API keys stay locked for 48 more hours.',
    ]) {
      expect(screen.getByText(line)).toBeInTheDocument();
    }
    await user.click(screen.getByRole('button', { name: 'Start the 72-hour wait' }));
    expect(await screen.findByText('Enter your password to confirm.')).toBeInTheDocument();
    await user.type(screen.getByLabelText('Password'), 'wrong password');
    await user.click(screen.getByRole('button', { name: 'Start the 72-hour wait' }));
    await waitFor(() => expect(screen.getByLabelText('Password')).toHaveAccessibleDescription('That password is not correct.'));
    expect(u.totp_removal_at).toBeFalsy();
    // The link was not spent by the wrong password.
    await user.clear(screen.getByLabelText('Password'));
    await user.type(screen.getByLabelText('Password'), MOCK_PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Start the 72-hour wait' }));
    expect(await screen.findByRole('heading', { name: 'Removal scheduled' })).toBeInTheDocument();
    expect(screen.getByText(/is removed on/)).toHaveTextContent(`Two-factor authentication on ${SECURE} is removed on ${formatUtc(u.totp_removal_at)}. After that, your password signs you in.`);
    expect(screen.getByText('Until then your authenticator app and recovery codes keep working. Signing in with either one cancels this request.')).toBeInTheDocument();
    expect(Date.parse(u.totp_removal_at!) - Date.now()).toBeGreaterThan(71 * HOUR);
    await user.click(screen.getByRole('button', { name: 'Cancel removal' }));
    expect(await screen.findByRole('heading', { name: 'Removal cancelled' })).toBeInTheDocument();
    expect(u.totp_removal_at).toBeNull();
  });

  it('a link that does not work offers a new one', async () => {
    const { user } = renderApp('/lost-authenticator/confirm#token=not-the-link', { signedIn: false });
    expect(await screen.findByRole('heading', { name: 'This link no longer works' })).toBeInTheDocument();
    await user.click(screen.getByRole('link', { name: 'Send a new link' }));
    expect(await screen.findByRole('heading', { name: 'Lost your authenticator app?' })).toBeInTheDocument();
  });

  it('takes the token out of the address bar', async () => {
    const { mock } = renderApp(LINK, { signedIn: false });
    userOf(mock, SECURE).lost_link = true;
    await screen.findByRole('heading', { name: 'Remove two-factor authentication' });
    expect(window.location.hash).toBe('');
  });
});

describe('cancel link from the email', () => {
  const LINK = `/cancel-2fa-removal#token=${MOCK_CANCEL_TOKEN}`;

  it('opening the page changes nothing: only the button cancels', async () => {
    const { user, mock } = renderApp(LINK, { signedIn: false });
    const u = userOf(mock, SECURE);
    u.totp_removal_at = inHours(70);
    expect(await screen.findByRole('heading', { name: 'Cancel the removal?' })).toBeInTheDocument();
    expect(u.totp_removal_at).not.toBeNull();
    await user.click(screen.getByRole('button', { name: 'Cancel removal' }));
    expect(await screen.findByRole('heading', { name: 'Removal cancelled' })).toBeInTheDocument();
    expect(u.totp_removal_at).toBeNull();
  });

  it('says so when the removal is gone already', async () => {
    const { user } = renderApp(LINK, { signedIn: false });
    await user.click(await screen.findByRole('button', { name: 'Cancel removal' }));
    expect(await screen.findByRole('heading', { name: 'This link no longer works' })).toBeInTheDocument();
  });

  it('a page without a token has nothing to cancel', async () => {
    renderApp('/cancel-2fa-removal', { signedIn: false });
    expect(await screen.findByRole('heading', { name: 'This link no longer works' })).toBeInTheDocument();
  });
});

describe('while a removal waits', () => {
  it('shows the strip on every page and cancels from it', async () => {
    const { user, mock } = renderApp('/payments', { email: SECURE });
    userOf(mock, SECURE).totp_removal_at = inHours(60);
    const strip = await screen.findByTestId('removal-strip');
    expect(strip).toHaveTextContent(/^Two-factor authentication will be removed on .+ UTC, after a recovery request\./);
    await user.click(within(strip).getByRole('button', { name: 'Cancel removal' }));
    await waitFor(() => expect(screen.queryByTestId('removal-strip')).toBeNull());
    expect(userOf(mock, SECURE).totp_removal_at).toBeNull();
    expect(await screen.findByText('Removal cancelled. Two-factor authentication stays on.')).toBeInTheDocument();
  });

  it('shows no strip when nothing is pending', async () => {
    renderApp('/payments', { email: SECURE });
    await screen.findByRole('heading', { level: 1, name: 'Payments' });
    expect(screen.queryByTestId('removal-strip')).toBeNull();
  });

  it('changes the card: the date, the warning and one button', async () => {
    const { user, mock } = renderApp('/settings/account', { email: SECURE });
    const u = userOf(mock, SECURE);
    u.totp_removal_at = inHours(60);
    expect(await screen.findByText(`On until ${formatUtc(u.totp_removal_at)}. A recovery request removes it then.`)).toBeInTheDocument();
    expect(screen.getByText('Did you not ask for this? Cancel the removal and change your password. Someone may know your password and have access to your email.')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Replace app' })).toBeNull();
    const card = screen.getByRole('region', { name: 'Two-factor authentication' });
    await user.click(within(card).getByRole('button', { name: 'Cancel removal' }));
    expect(await screen.findByRole('button', { name: 'Replace app' })).toBeInTheDocument();
  });
});

describe('after a removal: the lock', () => {
  /** An account whose 2FA was removed by a recovery request a day ago. */
  async function locked() {
    const app = renderApp('/settings/networks', { email: OWNER });
    userOf(app.mock, OWNER).recovery_locked_until = inHours(24);
    return app;
  }

  it('applies a removal whose time came: 2FA is off and the lock starts', async () => {
    const { mock } = renderApp('/settings/account', { email: SECURE });
    const u = userOf(mock, SECURE);
    u.totp_removal_at = new Date(Date.now() - HOUR).toISOString();
    expect(await screen.findByText(/^Off\. Turn it on/)).toBeInTheDocument();
    expect(Date.parse(u.recovery_locked_until!) - Date.now()).toBeGreaterThan(46 * HOUR);
  });

  it('says so before a live wallet is submitted, and sends nothing', async () => {
    const { user } = await locked();
    const posts: unknown[] = [];
    server.use(http.post('/v1/dashboard/wallets', () => {
      posts.push(1);
      return HttpResponse.json({}, { status: 201 });
    }));
    await user.click(await screen.findByRole('button', { name: 'Add wallet on Base' }));
    await screen.findByRole('dialog', { name: 'Add wallet' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Add wallet' })).toBeEnabled());
    // /me carries the lock once it has loaded.
    await waitFor(async () => {
      await user.click(screen.getByRole('button', { name: 'Add wallet' }));
      expect(screen.getByRole('dialog', { name: 'Adding a wallet is locked' })).toBeInTheDocument();
    });
    const d = within(screen.getByRole('dialog', { name: 'Adding a wallet is locked' }));
    expect(d.getByText(/^Until .+ UTC\.$/)).toBeInTheDocument();
    expect(d.getByText(/For 48 hours you cannot add a wallet or create a live API key\. This protects where your payments go\./)).toBeInTheDocument();
    expect(d.getByText('Your current wallets keep receiving payments. Turn on two-factor authentication now, so you are protected when the lock ends.')).toBeInTheDocument();
    expect(posts).toEqual([]);
    await user.click(d.getAllByRole('button', { name: 'Close' }).pop()!);
    expect(screen.queryByRole('dialog', { name: 'Adding a wallet is locked' })).toBeNull();
  });

  it('opens the same dialog when the server refuses with recovery_locked', async () => {
    const until = inHours(30);
    const { user } = renderApp('/settings/networks', {
      email: OWNER,
      handlers: [
        http.post('/v1/dashboard/wallets', () =>
          HttpResponse.json({ error: { code: 'recovery_locked', message: 'Locked.', details: { locked_until: until } } }, { status: 403, headers: { 'Retry-After': '108000' } }),
        ),
      ],
    });
    await user.click(await screen.findByRole('button', { name: 'Add wallet on Base' }));
    const d = within(await screen.findByRole('dialog', { name: 'Add wallet' }));
    await user.type(d.getByLabelText('Name'), 'Payroll');
    await user.type(d.getByLabelText('Base wallet address'), '0x1111111111111111111111111111111111aBcDeF');
    await user.type(d.getByLabelText('Confirm the last 6 characters'), 'ABCDEF');
    await emailCode(user, d);
    await user.click(d.getByRole('button', { name: 'Add wallet' }));
    expect(await screen.findByRole('dialog', { name: 'Adding a wallet is locked' })).toBeInTheDocument();
    expect(screen.queryByText('Locked.')).toBeNull();
  });

  it('answers recovery_locked in the mock before it checks the code', async () => {
    const { user, mock } = renderApp('/developers/api-keys', { email: OWNER });
    userOf(mock, OWNER).recovery_locked_until = inHours(24);
    await user.click(await screen.findByRole('button', { name: 'Create API key' }));
    const d = within(await screen.findByRole('dialog', { name: 'Create API key' }));
    await user.type(d.getByLabelText('Name'), 'Server');
    await emailCode(user, d, '000000');
    await waitFor(async () => {
      await user.click(d.getByRole('button', { name: 'Create key' }));
      expect(screen.getByRole('dialog', { name: 'Adding a wallet is locked' })).toBeInTheDocument();
    });
    expect(d.getByLabelText('Code from your email')).not.toHaveAccessibleDescription(/not valid/);
  });

  it('leaves Test mode alone', async () => {
    const { user, mock } = renderApp('/developers/api-keys', { email: OWNER, mode: 'test' });
    userOf(mock, OWNER).recovery_locked_until = inHours(24);
    await user.click(await screen.findByRole('button', { name: 'Create API key' }));
    const d = within(await screen.findByRole('dialog', { name: 'Create API key' }));
    await user.type(d.getByLabelText('Name'), 'Server');
    await user.click(d.getByRole('button', { name: 'Create key' }));
    expect(await screen.findByRole('dialog', { name: 'Your new API key' })).toBeInTheDocument();
  });
});
