import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { ALL_SCOPES, canRoll, describeScopes, isFullAccess, keyPill, lastUsedLabel, NO_ACCESS, ROLL_OVERLAPS, SCOPE_GROUPS, scopesFromAccess } from '../src/lib/apiKeys';
import { MOCK_CODE } from '../src/mocks/db';
import { emailCode, renderApp, useMockServer } from './helpers';

useMockServer();

describe('API key helpers', () => {
  it('turns the picker into scopes, write always with read', () => {
    expect(scopesFromAccess(NO_ACCESS)).toEqual([]);
    expect(scopesFromAccess({ ...NO_ACCESS, sessions: 'read', events: 'read' })).toEqual(['sessions:read', 'events:read']);
    expect(scopesFromAccess({ ...NO_ACCESS, sessions: 'write', webhooks: 'write' })).toEqual(['sessions:read', 'sessions:write', 'webhooks:read', 'webhooks:write']);
    expect(scopesFromAccess({ sessions: 'write', products: 'write', payment_links: 'write', customer_addresses: 'write', wallets: 'read', events: 'read', webhooks: 'write' }).sort()).toEqual([...ALL_SCOPES].sort());
  });

  it('describes a key by what it can do', () => {
    expect(describeScopes([...ALL_SCOPES])).toBe('Full access');
    expect(isFullAccess(['sessions:read'])).toBe(false);
    expect(describeScopes(['sessions:read', 'events:read'])).toBe('Payments: read · Event feed: read');
    expect(describeScopes(['events:read', 'sessions:write', 'sessions:read'])).toBe('Payments: read and create · Event feed: read');
    expect(describeScopes(['webhooks:write'])).toBe('Webhook endpoints: manage');
    expect(describeScopes(['products:read', 'products:write'])).toBe('Products: read and change');
    expect(describeScopes(['payment_links:read', 'payment_links:write'])).toBe('Payment links: read and change');
    expect(describeScopes(['wallets:read'])).toBe('Wallets: read');
    expect(isFullAccess(ALL_SCOPES.filter((s) => s !== 'wallets:read'))).toBe(false);
  });

  it('says how a rolled key ends, and only rolls a key that works and was not rolled', () => {
    expect(keyPill({ status: 'active', expires_at: null })).toBeNull();
    expect(keyPill({ status: 'expiring', expires_at: '2026-10-26T14:00:00Z' })?.label).toMatch(/^Works until /);
    expect(keyPill({ status: 'expired', expires_at: '2026-10-26T14:00:00Z' })).toEqual({ label: 'Expired', tone: 'idle' });
    expect(keyPill({ status: 'revoked', expires_at: null })).toBeNull();
    expect(canRoll({ status: 'active' })).toBe(true);
    expect(canRoll({ status: 'expiring' })).toBe(false);
    expect(ROLL_OVERLAPS.map((o) => o.value)).toEqual(['now', '1h', '24h', '7d']);
  });

  it('says when a key was last used, keeping dates capitalized', () => {
    const now = Date.now();
    const iso = (ms: number) => new Date(ms).toISOString();
    expect(lastUsedLabel(null)).toBe('Never used');
    expect(lastUsedLabel(iso(now - 10_000))).toBe('Last used just now');
    expect(lastUsedLabel(iso(now - 3 * 3_600_000))).toBe('Last used 3 h ago');
    expect(lastUsedLabel(iso(now - 150 * 86_400_000))).toMatch(/^Last used on [A-Z]/);
  });
});

describe('API keys page', () => {
  it('links to the API reference on both developer tabs', async () => {
    for (const path of ['/developers/api-keys', '/developers/webhooks']) {
      const { unmount } = renderApp(path);
      const link = await screen.findByRole('link', { name: 'API reference' });
      expect(link).toHaveAttribute('href', '/docs/');
      expect(link).toHaveAttribute('target', '_blank');
      expect(link).toHaveAttribute('rel', expect.stringContaining('noopener'));
      unmount();
    }
  });

  it('shows when each key was last used, its access and its scopes', async () => {
    renderApp('/developers/api-keys');
    const prod = (await screen.findByText('Production server')).closest('li') as HTMLElement;
    expect(within(prod).getByTestId('last-used')).toHaveTextContent('Last used 3 h ago');
    expect(within(prod).getByText('Restricted')).toBeInTheDocument();
    expect(within(prod).getByText('Payments: read and create · Products: read · Payment links: read · Event feed: read')).toBeInTheDocument();
    expect(within(within(prod).getByLabelText('Scopes')).getAllByText(/:/).map((x) => x.textContent)).toEqual(['sessions:read', 'sessions:write', 'products:read', 'payment_links:read', 'events:read']);
  });

  it('says a key was never used', async () => {
    renderApp('/developers/api-keys', { mode: 'test' });
    const staging = (await screen.findByText('Staging')).closest('li') as HTMLElement;
    expect(within(staging).getByTestId('last-used')).toHaveTextContent('Never used');
    expect(within(staging).getByText('Full access')).toBeInTheDocument();
  });

  it('creates a restricted key from the scope picker', async () => {
    const bodies: Record<string, unknown>[] = [];
    const { user } = renderApp('/developers/api-keys', {
      handlers: [
        http.post('/v1/dashboard/api_keys', async ({ request }) => {
          bodies.push((await request.clone().json()) as Record<string, unknown>);
          return undefined as unknown as HttpResponse<never>;
        }),
      ],
    });
    await user.click(await screen.findByRole('button', { name: 'Create API key' }));
    const d = within(await screen.findByRole('dialog', { name: 'Create API key' }));
    expect(d.queryByTestId('scope-picker')).not.toBeInTheDocument();
    await user.type(d.getByLabelText('Name'), 'Reports');
    await user.click(d.getByRole('radio', { name: 'Restricted' }));
    await user.click(d.getByRole('button', { name: 'Create key' }));
    expect(d.getByText('Give the key access to at least one area.')).toBeInTheDocument();
    await user.selectOptions(d.getByLabelText('Payments'), 'read');
    await user.selectOptions(d.getByLabelText('Event feed'), 'read');
    await emailCode(user, d);
    await user.click(d.getByRole('button', { name: 'Create key' }));
    const done = within(await screen.findByRole('dialog', { name: 'Your new API key' }));
    expect(done.getByText(/Payments: read · Event feed: read/)).toBeInTheDocument();
    expect(bodies.at(-1)).toEqual({ name: 'Reports', scopes: ['sessions:read', 'events:read'], mode: 'live', email_code: '246810' });
    await user.click(done.getByRole('button', { name: 'I copied the key' }));
    const row = (await screen.findByText('Reports')).closest('li') as HTMLElement;
    expect(within(row).getByText('Restricted')).toBeInTheDocument();
    expect(within(row).getByTestId('last-used')).toHaveTextContent('Never used');
  });

  it('gives a new key full access unless restricted', async () => {
    const bodies: Record<string, unknown>[] = [];
    const { user } = renderApp('/developers/api-keys', {
      mode: 'test',
      handlers: [
        http.post('/v1/dashboard/api_keys', async ({ request }) => {
          bodies.push((await request.clone().json()) as Record<string, unknown>);
          return undefined as unknown as HttpResponse<never>;
        }),
      ],
    });
    await user.click(await screen.findByRole('button', { name: 'Create API key' }));
    const d = within(await screen.findByRole('dialog', { name: 'Create API key' }));
    expect(d.queryByTestId('step-up')).not.toBeInTheDocument();
    await user.type(d.getByLabelText('Name'), 'Everything');
    await user.click(d.getByRole('button', { name: 'Create key' }));
    await screen.findByRole('dialog', { name: 'Your new API key' });
    expect((bodies.at(-1)!.scopes as string[]).sort()).toEqual([...ALL_SCOPES].sort());
    expect(bodies.at(-1)).not.toHaveProperty('email_code');
  });

  it('never offers a scope that writes wallets: they are added and archived only in the dashboard', async () => {
    // Arrange
    const bodies: Record<string, unknown>[] = [];
    const { user } = renderApp('/developers/api-keys', {
      mode: 'test',
      handlers: [
        http.post('/v1/dashboard/api_keys', async ({ request }) => {
          bodies.push((await request.clone().json()) as Record<string, unknown>);
          return undefined as unknown as HttpResponse<never>;
        }),
      ],
    });
    await user.click(await screen.findByRole('button', { name: 'Create API key' }));
    const d = within(await screen.findByRole('dialog', { name: 'Create API key' }));
    await user.type(d.getByLabelText('Name'), 'Widest restricted');
    await user.click(d.getByRole('radio', { name: 'Restricted' }));
    const picker = within(d.getByTestId('scope-picker'));
    const walletOptions = Array.from((picker.getByLabelText('Wallets') as HTMLSelectElement).options).map((o) => o.value);

    // Act
    for (const g of SCOPE_GROUPS) await user.selectOptions(picker.getByLabelText(g.label), g.write ? 'write' : 'read');
    await user.click(d.getByRole('button', { name: 'Create key' }));
    await screen.findByRole('dialog', { name: 'Your new API key' });

    // Assert
    expect(ALL_SCOPES).not.toContain('wallets:write');
    expect(SCOPE_GROUPS.flatMap((g) => [g.read, g.write])).not.toContain('wallets:write');
    expect(walletOptions).toEqual(['none', 'read']);
    expect(bodies.at(-1)!.scopes as string[]).not.toContain('wallets:write');
    expect((bodies.at(-1)!.scopes as string[]).sort()).toEqual([...ALL_SCOPES].sort());
  });

  it('rolls a key: the new secret once, the old one works until the chosen time', async () => {
    const bodies: Record<string, unknown>[] = [];
    const { user } = renderApp('/developers/api-keys', {
      handlers: [
        http.post('/v1/dashboard/api_keys/:id/roll', async ({ request }) => {
          bodies.push((await request.clone().json()) as Record<string, unknown>);
          return undefined as unknown as HttpResponse<never>;
        }),
      ],
    });
    const row = (await screen.findByText('Production server')).closest('li') as HTMLElement;
    await user.click(within(row).getByRole('button', { name: 'Roll Production server' }));
    const d = within(await screen.findByRole('dialog', { name: 'Roll Production server?' }));
    expect(d.getByRole('radio', { name: '24 hours' })).toBeChecked();
    await user.click(d.getByRole('radio', { name: '1 hour' }));
    expect(d.getByText('The old secret keeps working for 1 hour.')).toBeInTheDocument();
    await emailCode(user, d);
    await user.click(d.getByRole('button', { name: 'Roll key' }));
    const done = within(await screen.findByRole('dialog', { name: 'New secret for your key' }));
    expect(bodies).toEqual([{ overlap: '1h', email_code: '246810' }]);
    const secret = done.getByText(/^sk_live_/).textContent!;
    expect(done.getByTestId('roll-result')).toHaveTextContent(/The old secret keeps working until .+\. Put the new one on your servers before then\./);
    await user.click(done.getByRole('button', { name: 'I copied the key' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.queryByText(secret)).not.toBeInTheDocument();
    const rows = (await screen.findAllByText('Production server')).map((x) => x.closest('li') as HTMLElement);
    expect(rows).toHaveLength(2);
    const old = rows.find((r) => within(r).queryByText(/^Works until /))!;
    expect(old).toBeDefined();
    expect(within(old).queryByRole('button', { name: /^Roll/ })).not.toBeInTheDocument();
    expect(within(old).getByRole('button', { name: 'Revoke' })).toBeInTheDocument();
  });

  it('rolls a key now: the old secret stops at once', async () => {
    const { user } = renderApp('/developers/api-keys', { mode: 'test' });
    const row = (await screen.findByText('Staging')).closest('li') as HTMLElement;
    await user.click(within(row).getByRole('button', { name: 'Roll Staging' }));
    const d = within(await screen.findByRole('dialog', { name: 'Roll Staging?' }));
    await user.click(d.getByRole('radio', { name: 'Now' }));
    await user.click(d.getByRole('button', { name: 'Roll key' }));
    const done = within(await screen.findByRole('dialog', { name: 'New secret for your key' }));
    expect(done.getByTestId('roll-result')).toHaveTextContent('The old secret no longer works.');
    await user.click(done.getByRole('button', { name: 'I copied the key' }));
    await waitFor(() => expect(screen.getAllByText('Staging')).toHaveLength(2));
    const old = screen.getAllByText('Staging').map((x) => x.closest('li') as HTMLElement).find((r) => within(r).queryByText('Expired'))!;
    expect(old).toBeDefined();
    expect(within(old).queryByRole('button')).not.toBeInTheDocument();
  });

  it('asks for the authenticator code before rolling a live key when 2FA is on, or an emailed one if chosen', async () => {
    const { user } = renderApp('/developers/api-keys', { email: 'secure@acme.studio' });
    const row = (await screen.findByText('Reporting script')).closest('li') as HTMLElement;
    await user.click(within(row).getByRole('button', { name: 'Roll Reporting script' }));
    const d = within(await screen.findByRole('dialog', { name: 'Roll Reporting script?' }));
    expect(d.getByRole('radio', { name: /Authenticator app/ })).toBeChecked();
    expect(d.getByRole('radio', { name: /Code by email/ })).toBeInTheDocument();
    expect(d.queryByLabelText('Password')).not.toBeInTheDocument();
    await user.click(d.getByRole('button', { name: 'Roll key' }));
    expect(d.getByText('Enter the 6-digit code from your authenticator app.')).toBeInTheDocument();
    await user.type(d.getByLabelText('Code from your authenticator app'), '999999');
    await user.click(d.getByRole('button', { name: 'Roll key' }));
    expect(await d.findByText('That code is not valid. Enter the current code from your authenticator app.')).toBeInTheDocument();
    await user.clear(d.getByLabelText('Code from your authenticator app'));
    await user.type(d.getByLabelText('Code from your authenticator app'), '123456');
    await user.click(d.getByRole('button', { name: 'Roll key' }));
    expect(await screen.findByRole('dialog', { name: 'New secret for your key' })).toBeInTheDocument();
  });

  it('asks for an emailed code before creating a live key, and shows a wrong one in its field', async () => {
    // Arrange
    const bodies: Record<string, unknown>[] = [];
    const { user } = renderApp('/developers/api-keys', {
      handlers: [
        http.post('/v1/dashboard/api_keys', async ({ request }) => {
          bodies.push((await request.clone().json()) as Record<string, unknown>);
          return undefined as unknown as HttpResponse<never>;
        }),
      ],
    });
    await user.click(await screen.findByRole('button', { name: 'Create API key' }));
    const d = within(await screen.findByRole('dialog', { name: 'Create API key' }));
    await user.type(d.getByLabelText('Name'), 'Server');

    // Act
    await user.click(d.getByRole('button', { name: 'Create key' }));
    const sentWithout = bodies.length;
    await emailCode(user, d, '000000');
    await user.click(d.getByRole('button', { name: 'Create key' }));

    // Assert
    expect(sentWithout).toBe(0);
    expect(d.getByTestId('step-up')).toBeInTheDocument();
    expect(d.queryByLabelText('Password')).not.toBeInTheDocument();
    await waitFor(() => expect(d.getByLabelText('Code from your email')).toHaveAccessibleDescription(/That code is not valid or has expired/));
    expect(d.queryByRole('alert')).not.toBeInTheDocument();
    expect(bodies).toEqual([{ name: 'Server', scopes: [...ALL_SCOPES], mode: 'live', email_code: '000000' }]);
    await user.clear(d.getByLabelText('Code from your email'));
    await user.type(d.getByLabelText('Code from your email'), MOCK_CODE);
    await user.click(d.getByRole('button', { name: 'Create key' }));
    expect(await screen.findByRole('dialog', { name: 'Your new API key' })).toBeInTheDocument();
  });

  it('says where the code was sent and asks for the code before sending', async () => {
    const { user } = renderApp('/developers/api-keys');
    await user.click(await screen.findByRole('button', { name: 'Create API key' }));
    const d = within(await screen.findByRole('dialog', { name: 'Create API key' }));
    await user.type(d.getByLabelText('Name'), 'Server');
    expect(d.getByText(/6-digit code we email to o•••@acme.studio/)).toBeInTheDocument();
    await user.click(d.getByRole('button', { name: 'Create key' }));
    expect(d.getByText('Ask for a code first, then enter it here.')).toBeInTheDocument();
    await user.click(d.getByRole('button', { name: 'Email me a code' }));
    expect(await d.findByLabelText('Code from your email')).toHaveAccessibleDescription(/Sent to o•••@acme.studio/);
    expect(d.getByRole('button', { name: /Send a new code in \d+ s/ })).toBeDisabled();
  });

  it('keeps the key name out of autofill', async () => {
    const { user } = renderApp('/developers/api-keys');
    await user.click(await screen.findByRole('button', { name: 'Create API key' }));
    const d = within(await screen.findByRole('dialog', { name: 'Create API key' }));
    expect(d.getByLabelText('Name')).toHaveAttribute('autocomplete', 'off');
  });

  it('rolls a test key without a step-up', async () => {
    const bodies: Record<string, unknown>[] = [];
    const { user } = renderApp('/developers/api-keys', {
      mode: 'test',
      handlers: [
        http.post('/v1/dashboard/api_keys/:id/roll', async ({ request }) => {
          bodies.push((await request.clone().json()) as Record<string, unknown>);
          return undefined as unknown as HttpResponse<never>;
        }),
      ],
    });
    const row = (await screen.findByText('Staging')).closest('li') as HTMLElement;
    await user.click(within(row).getByRole('button', { name: 'Roll Staging' }));
    const d = within(await screen.findByRole('dialog', { name: 'Roll Staging?' }));
    expect(d.queryByTestId('step-up')).not.toBeInTheDocument();
    await user.click(d.getByRole('button', { name: 'Roll key' }));
    await screen.findByRole('dialog', { name: 'New secret for your key' });
    expect(bodies).toEqual([{ overlap: '24h' }]);
  });

  it('shows a rolled key that still works until its deadline, with revoke only', async () => {
    renderApp('/developers/api-keys', { mode: 'test' });
    const rows = (await screen.findAllByText('CI runner')).map((x) => x.closest('li') as HTMLElement);
    const old = rows.find((r) => within(r).queryByText(/^Works until /))!;
    expect(old).toBeDefined();
    expect(within(old).queryByRole('button', { name: /^Roll/ })).not.toBeInTheDocument();
    const fresh = rows.find((r) => r !== old)!;
    expect(within(fresh).getByRole('button', { name: 'Roll CI runner' })).toBeInTheDocument();
  });
});

describe('Create API key with the confirmation window off', () => {
  it('shows the missing-code error together with the other field errors', async () => {
    const { user } = renderApp('/developers/api-keys');
    await user.click(await screen.findByRole('button', { name: 'Create API key' }));
    const d = within(await screen.findByRole('dialog', { name: 'Create API key' }));
    await user.click(d.getByRole('button', { name: 'Create key' }));
    expect(await d.findByText('Give the key a name, such as Production server.')).toBeInTheDocument();
    expect(d.getByText('Ask for a code first, then enter it here.')).toBeInTheDocument();
  });
});
