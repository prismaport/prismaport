import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { MOCK_TOTP } from '../src/mocks/db';
import { emailCode, renderApp, useMockServer } from './helpers';

useMockServer();

/** The sidebar's lock card (the phone bar shows a compact pill too). */
const lockStatus = async () => screen.findByTestId('lock-status');
const unlockButton = async () => within(await lockStatus()).getByRole('button', { name: 'Unlock Live changes' });

describe('Lock on Live changes', () => {
  it('unlocks with an emailed code, counts down, and locks again on Lock now', async () => {
    const bodies: Record<string, unknown>[] = [];
    const { user } = renderApp('/', {
      window: true,
      handlers: [
        http.post('/v1/dashboard/confirmation_window', async ({ request }) => {
          bodies.push((await request.clone().json()) as Record<string, unknown>);
          return undefined as unknown as HttpResponse<never>;
        }),
      ],
    });
    const locked = await lockStatus();
    expect(locked).toHaveTextContent('LockedWallets, keys and webhooks ask for a code.Unlock');
    expect(screen.getByTestId('lock-pill')).toHaveAccessibleName('Locked. Unlock Live changes');
    await user.click(await unlockButton());
    const d = within(await screen.findByRole('dialog', { name: 'Unlock Live changes for 10 minutes' }));
    expect(d.getByText('Add and archive wallets')).toBeInTheDocument();
    expect(d.getByText('Rolling an API key, rotating a webhook secret, and two-factor settings.')).toBeInTheDocument();
    await user.click(d.getByRole('button', { name: 'Unlock for 10 minutes' }));
    expect(d.getByText('Ask for a code first, then enter it here.')).toBeInTheDocument();
    expect(bodies).toEqual([]);
    await emailCode(user, d);
    await user.click(d.getByRole('button', { name: 'Unlock for 10 minutes' }));
    expect(await screen.findByText('Unlocked for 10 minutes')).toBeInTheDocument();
    expect(bodies).toEqual([{ email_code: '246810' }]);
    const unlocked = await lockStatus();
    await waitFor(() => expect(unlocked).toHaveAttribute('data-state', 'unlocked'));
    expect(unlocked).toHaveTextContent(/^Unlocked\d{1,2}:\d\duntil .+On this device\. It locks itself\.Lock now$/);
    expect(within(unlocked).getByRole('button', { name: 'Lock now' })).toHaveFocus();

    await user.click(within(unlocked).getByRole('button', { name: 'Lock now' }));
    expect(await screen.findByText('Locked. Live changes ask for a code.')).toBeInTheDocument();
    await waitFor(async () => expect(await lockStatus()).toHaveAttribute('data-state', 'locked'));
  });

  it('unlocks with the authenticator app when 2FA is on', async () => {
    const { user, mock } = renderApp('/', { window: true });
    mock.db().users[0]!.totp_enabled = true;
    await user.click(await unlockButton());
    const d = within(await screen.findByRole('dialog', { name: 'Unlock Live changes for 10 minutes' }));
    expect(d.getByRole('radio', { name: /Authenticator app/ })).toBeChecked();
    expect(d.getByRole('radio', { name: /Code by email/ })).not.toBeChecked();
    await user.type(d.getByLabelText('Code from your authenticator app'), '000000');
    await user.click(d.getByRole('button', { name: 'Unlock for 10 minutes' }));
    expect(await d.findByText(/That code is not valid/)).toBeInTheDocument();
    await user.clear(d.getByLabelText('Code from your authenticator app'));
    await user.type(d.getByLabelText('Code from your authenticator app'), MOCK_TOTP);
    await user.click(d.getByRole('button', { name: 'Unlock for 10 minutes' }));
    expect(await screen.findByText('Unlocked for 10 minutes')).toBeInTheDocument();
    expect(mock.db().session!.confirmed_until).toBeGreaterThan(Date.now());
  });

  it('says once when the window ends on its own', async () => {
    const { mock } = renderApp('/', { window: true });
    mock.db().session!.confirmed_until = Date.now() + 1500;
    await waitFor(async () => expect(await lockStatus()).toHaveAttribute('data-state', 'ending'));
    expect(await screen.findByText('Locked again. Live changes ask for a code.', {}, { timeout: 4000 })).toBeInTheDocument();
    await waitFor(async () => expect(await lockStatus()).toHaveAttribute('data-state', 'locked'));
  });

  it('is not shown in Test mode', async () => {
    renderApp('/', { mode: 'test' });
    await screen.findAllByRole('link', { name: /Prismaport dashboard home/ });
    expect(screen.queryByTestId('lock-status')).toBeNull();
    expect(screen.queryByTestId('lock-pill')).toBeNull();
  });
});

describe('Lock announcement', () => {
  it('has a polite live region that says Locked or Unlocked, without the countdown', async () => {
    const { user } = renderApp('/', { window: true });
    await lockStatus();
    const regions = screen.getAllByTestId('lock-announce');
    expect(regions.length).toBeGreaterThan(0);
    for (const r of regions) {
      expect(r).toHaveAttribute('role', 'status');
      expect(r).toHaveAttribute('aria-live', 'polite');
      expect(r).toHaveTextContent(/^Live changes locked$/);
    }
    await user.click(await unlockButton());
    const d = within(await screen.findByRole('dialog', { name: 'Unlock Live changes for 10 minutes' }));
    await emailCode(user, d);
    await user.click(d.getByRole('button', { name: 'Unlock for 10 minutes' }));
    await waitFor(() => expect(screen.getByTestId('lock-status')).toHaveAttribute('data-state', 'unlocked'));
    for (const r of screen.getAllByTestId('lock-announce')) expect(r).toHaveTextContent(/^Live changes unlocked$/);
  });
});
