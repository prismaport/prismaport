import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { MOCK_TOTP } from '../src/mocks/db';
import { emailCode, renderApp, useMockServer } from './helpers';

useMockServer();

const SPARE = { id: 'wal_SPARE', label: 'Spare', family: 'evm' as const, address: '0x2222222222222222222222222222222222222222', verified: true, archived_at: null, created_at: new Date().toISOString(), livemode: true };
const UNLOCK = 'Unlock Live changes for 10 minutes';

/** The session's confirmation window (D59), open for ten more minutes. */
function openWindow(mock: ReturnType<typeof renderApp>['mock']) {
  mock.db().session!.confirmed_until = Date.now() + 10 * 60_000;
}

async function archiveSpare(user: ReturnType<typeof renderApp>['user']) {
  await user.click(await screen.findByRole('button', { name: 'Archive Spare' }));
  const d = within(await screen.findByRole('dialog', { name: 'Archive Spare?' }));
  await user.click(d.getByRole('button', { name: 'Archive wallet' }));
  return d;
}

describe('Unlock on a Live change (D61)', () => {
  it('archives a Live wallet at once while unlocked, and the form says nothing about the lock', async () => {
    const { user, mock } = renderApp('/settings/networks', { window: true });
    openWindow(mock);
    mock.db().wallets.push({ ...SPARE });
    await user.click(await screen.findByRole('button', { name: 'Archive Spare' }));
    const d = within(await screen.findByRole('dialog', { name: 'Archive Spare?' }));
    expect(d.queryByTestId('step-up')).toBeNull();
    expect(d.queryByText(/Locked|Unlocked/)).toBeNull();
    await user.click(d.getByRole('button', { name: 'Archive wallet' }));
    expect(await screen.findByText('Spare archived')).toBeInTheDocument();
    expect(screen.queryByRole('dialog', { name: UNLOCK })).toBeNull();
  });

  it('opens Unlock when locked, then archives on its own', async () => {
    const bodies: unknown[] = [];
    const { user, mock } = renderApp('/settings/networks', {
      window: true,
      handlers: [
        http.post('/v1/dashboard/wallets/:id/archive', async ({ request }) => {
          bodies.push(await request.clone().json().catch(() => null));
          return undefined as unknown as HttpResponse<never>;
        }),
      ],
    });
    mock.db().wallets.push({ ...SPARE });
    const d = await archiveSpare(user);
    expect(d.queryByTestId('step-up')).toBeNull();
    const unlock = within(await screen.findByRole('dialog', { name: UNLOCK }));
    expect(bodies).toEqual([]);
    await emailCode(user, unlock);
    await user.click(unlock.getByRole('button', { name: 'Unlock for 10 minutes' }));
    expect(await screen.findByText('Spare archived')).toBeInTheDocument();
    expect(bodies).toHaveLength(1);
    expect(bodies[0] ?? {}).not.toHaveProperty('email_code');
    expect(mock.db().wallets.find((w) => w.id === 'wal_SPARE')!.archived_at).not.toBeNull();
  });

  it('opens Unlock again when the window ended on the way, then carries on', async () => {
    const { user, mock } = renderApp('/settings/networks', { window: true });
    openWindow(mock);
    mock.db().wallets.push({ ...SPARE });
    await user.click(await screen.findByRole('button', { name: 'Archive Spare' }));
    const d = within(await screen.findByRole('dialog', { name: 'Archive Spare?' }));
    mock.db().session!.confirmed_until = Date.now() - 1;
    await user.click(d.getByRole('button', { name: 'Archive wallet' }));
    const unlock = within(await screen.findByRole('dialog', { name: UNLOCK }));
    await emailCode(user, unlock);
    await user.click(unlock.getByRole('button', { name: 'Unlock for 10 minutes' }));
    expect(await screen.findByText('Spare archived')).toBeInTheDocument();
  });

  it('sends nothing when Unlock is cancelled, and asks again on the next try', async () => {
    const { user, mock } = renderApp('/settings/networks', { window: true });
    mock.db().wallets.push({ ...SPARE });
    const d = await archiveSpare(user);
    const unlock = within(await screen.findByRole('dialog', { name: UNLOCK }));
    await user.click(unlock.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: UNLOCK })).toBeNull());
    expect(mock.db().wallets.find((w) => w.id === 'wal_SPARE')!.archived_at).toBeNull();
    await user.click(d.getByRole('button', { name: 'Archive wallet' }));
    expect(await screen.findByRole('dialog', { name: UNLOCK })).toBeInTheDocument();
  });

  it('creates a Live API key after Unlock', async () => {
    const { user } = renderApp('/developers/api-keys', { window: true });
    await screen.findAllByTestId('api-key');
    await user.click(screen.getByRole('button', { name: 'Create API key' }));
    const d = within(await screen.findByRole('dialog', { name: 'Create API key' }));
    expect(d.queryByTestId('step-up')).toBeNull();
    await user.type(d.getByLabelText('Name'), 'Checkout server');
    await user.click(d.getByRole('button', { name: 'Create key' }));
    const unlock = within(await screen.findByRole('dialog', { name: UNLOCK }));
    await emailCode(user, unlock);
    await user.click(unlock.getByRole('button', { name: 'Unlock for 10 minutes' }));
    expect(await screen.findByRole('dialog', { name: 'Your new API key' })).toBeInTheDocument();
  });

  it('still asks for a code to roll a key, in its own form', async () => {
    const { user, mock } = renderApp('/developers/api-keys', { window: true });
    openWindow(mock);
    const row = (await screen.findByText('Production server')).closest('li') as HTMLElement;
    await user.click(within(row).getByRole('button', { name: 'Roll Production server' }));
    const d = within(await screen.findByRole('dialog', { name: 'Roll Production server?' }));
    expect(d.getByRole('button', { name: 'Email me a code' })).toBeInTheDocument();
  });
  it('adds a Live webhook endpoint after Unlock, with no code in its request', async () => {
    const bodies: unknown[] = [];
    const { user } = renderApp('/developers/webhooks', {
      window: true,
      handlers: [
        http.post('/v1/dashboard/webhook_endpoints', async ({ request }) => {
          bodies.push(await request.clone().json());
          return undefined as unknown as HttpResponse<never>;
        }),
      ],
    });
    await screen.findAllByTestId('webhook-endpoint');
    await user.click(screen.getByRole('button', { name: 'Add endpoint' }));
    const d = within(await screen.findByRole('dialog', { name: 'Add webhook endpoint' }));
    expect(d.queryByTestId('step-up')).toBeNull();
    await user.type(d.getByLabelText('Endpoint address'), 'https://acme.studio/hooks/unlock');
    await user.click(d.getByRole('button', { name: 'Add endpoint' }));
    const unlock = within(await screen.findByRole('dialog', { name: UNLOCK }));
    expect(bodies).toEqual([]);
    await emailCode(user, unlock);
    await user.click(unlock.getByRole('button', { name: 'Unlock for 10 minutes' }));
    expect(await screen.findByRole('dialog', { name: 'Webhook endpoint added' })).toBeInTheDocument();
    expect(bodies).toEqual([{ url: 'https://acme.studio/hooks/unlock', enabled_events: [] }]);
  });

  it('adds a Live wallet from Settings after Unlock', async () => {
    const { user } = renderApp('/settings/networks', { window: true });
    await user.click(await screen.findByRole('button', { name: 'Add wallet on Base' }));
    const d = within(await screen.findByRole('dialog', { name: 'Add wallet' }));
    expect(d.queryByTestId('step-up')).toBeNull();
    await user.type(d.getByLabelText('Name'), 'Payroll');
    await user.type(d.getByLabelText('Base wallet address'), '0x3333333333333333333333333333333333333333');
    await user.type(d.getByRole('textbox', { name: 'Confirm the last 6 characters' }), '333333');
    await user.click(d.getByRole('button', { name: 'Add wallet' }));
    const unlock = within(await screen.findByRole('dialog', { name: UNLOCK }));
    await emailCode(user, unlock);
    await user.click(unlock.getByRole('button', { name: 'Unlock for 10 minutes' }));
    expect(await screen.findByText('Payroll added')).toBeInTheDocument();
  });

  it('unlocks with the authenticator app from an action when 2FA is on', async () => {
    const { user, mock } = renderApp('/settings/networks', { window: true });
    mock.db().users[0]!.totp_enabled = true;
    mock.db().wallets.push({ ...SPARE });
    await archiveSpare(user);
    const unlock = within(await screen.findByRole('dialog', { name: UNLOCK }));
    expect(unlock.getByRole('radio', { name: /Authenticator app/ })).toBeChecked();
    await user.type(unlock.getByLabelText('Code from your authenticator app'), MOCK_TOTP);
    await user.click(unlock.getByRole('button', { name: 'Unlock for 10 minutes' }));
    expect(await screen.findByText('Spare archived')).toBeInTheDocument();
  });

  it('never asks to unlock in Test mode', async () => {
    const { user } = renderApp('/settings/networks', { window: true, mode: 'test' });
    await user.click(await screen.findByRole('button', { name: /^Add wallet on Base/ }));
    const d = within(await screen.findByRole('dialog', { name: 'Add wallet' }));
    await user.type(d.getByLabelText('Name'), 'Sandbox');
    await user.type(d.getByLabelText('Base wallet address'), '0x4444444444444444444444444444444444444444');
    await user.type(d.getByRole('textbox', { name: 'Confirm the last 6 characters' }), '444444');
    await user.click(d.getByRole('button', { name: 'Add wallet' }));
    expect(await screen.findByText('Sandbox added')).toBeInTheDocument();
    expect(screen.queryByRole('dialog', { name: UNLOCK })).toBeNull();
  });
});
