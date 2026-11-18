import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { ENABLE_NOTE, endpointPatch, endpointStatus, endpointUrlError, LIVE_URL_ERROR, PAUSE_NOTE, ROTATION_OVERLAP_NOTE, TEST_URL_ERROR, URL_EDIT_NOTE } from '../src/lib/webhooks';
import { INBOX_URL_HINT } from '../src/pages/Sandbox';
import { MOCK_CODE } from '../src/mocks/db';
import { emailCode, renderApp, useMockServer } from './helpers';

useMockServer();

describe('endpoint URL rules', () => {
  it('takes only https in Live mode', () => {
    expect(endpointUrlError('https://acme.studio/hooks', 'live')).toBeUndefined();
    expect(endpointUrlError('  https://acme.studio/hooks  ', 'live')).toBeUndefined();
    for (const bad of ['http://acme.studio/hooks', 'http://localhost:3000/hooks', 'acme.studio/hooks', 'ftp://acme.studio', 'https://user:pw@acme.studio', '']) {
      expect(endpointUrlError(bad, 'live')).toBe(LIVE_URL_ERROR);
    }
  });

  it('takes plain http in Test mode only for this machine', () => {
    for (const ok of ['https://acme.studio/hooks', 'http://localhost:3000/hooks', 'http://LOCALHOST/hooks', 'http://app.localhost/hooks', 'http://127.0.0.1:8080/x', 'http://[::1]:3000/x']) {
      expect(endpointUrlError(ok, 'test')).toBeUndefined();
    }
    for (const bad of ['http://acme.studio/hooks', 'http://10.0.0.5/hooks', 'http://localhost.evil.example/hooks', 'http://127.0.0.1.nip.io/x']) {
      expect(endpointUrlError(bad, 'test')).toBe(TEST_URL_ERROR);
    }
  });
});

describe('webhook endpoints', () => {
  it('adds a localhost endpoint over http in Test mode and refuses other http addresses', async () => {
    const { user } = renderApp('/developers/webhooks', { mode: 'test' });
    await screen.findAllByTestId('webhook-endpoint');
    await user.click(screen.getByRole('button', { name: 'Add endpoint' }));
    const d = within(await screen.findByRole('dialog', { name: 'Add webhook endpoint' }));
    expect(d.getByText('An https:// address, or http://localhost while you develop.')).toBeInTheDocument();
    await user.type(d.getByLabelText('Endpoint address'), 'http://staging.acme.studio/hooks');
    await user.click(d.getByRole('button', { name: 'Add endpoint' }));
    expect(d.getByText(TEST_URL_ERROR)).toBeInTheDocument();
    await user.clear(d.getByLabelText('Endpoint address'));
    await user.type(d.getByLabelText('Endpoint address'), 'http://localhost:3000/webhooks');
    await user.click(d.getByRole('button', { name: 'Add endpoint' }));
    expect(await screen.findByRole('dialog', { name: 'Webhook endpoint added' })).toBeInTheDocument();
  });

  it('refuses plain http to localhost in Live mode', async () => {
    const { user } = renderApp('/developers/webhooks');
    await screen.findAllByTestId('webhook-endpoint');
    await user.click(screen.getByRole('button', { name: 'Add endpoint' }));
    const d = within(await screen.findByRole('dialog', { name: 'Add webhook endpoint' }));
    await user.type(d.getByLabelText('Endpoint address'), 'http://localhost:3000/webhooks');
    await user.click(d.getByRole('button', { name: 'Add endpoint' }));
    expect(d.getByText(LIVE_URL_ERROR)).toBeInTheDocument();
  });

  it('says the old secret keeps signing for 24 hours after a rotation', async () => {
    const { user } = renderApp('/developers/webhooks');
    const row = (await screen.findAllByTestId('webhook-endpoint'))[0]!;
    expect(within(row).queryByTestId('rotation-window')).not.toBeInTheDocument();
    await user.click(within(row).getByRole('button', { name: /Rotate secret/ }));
    const confirm = within(await screen.findByRole('dialog', { name: 'Rotate the signing secret?' }));
    expect(confirm.getByText(ROTATION_OVERLAP_NOTE)).toBeInTheDocument();
    // A Live secret takes a confirmation code (D58): nothing is sent without it.
    await user.click(confirm.getByRole('button', { name: 'Rotate secret' }));
    expect(confirm.getByText('Ask for a code first, then enter it here.')).toBeInTheDocument();
    await emailCode(user, confirm);
    await user.click(confirm.getByRole('button', { name: 'Rotate secret' }));
    const done = within(await screen.findByRole('dialog', { name: 'New signing secret' }));
    expect(done.getByText(/^whsec_/)).toBeInTheDocument();
    expect(done.getByText(/Deliveries carry signatures with both secrets until/)).toBeInTheDocument();
    await user.click(done.getByRole('button', { name: 'I copied the secret' }));
    const after = (await screen.findAllByTestId('webhook-endpoint'))[0]!;
    expect(await within(after).findByTestId('rotation-window')).toHaveTextContent(/^The previous secret also signs deliveries until /);
  });

  it('warns next to the inbox address that it is a credential', async () => {
    const { mock } = renderApp('/developers/webhooks', { mode: 'test' });
    const section = within(await screen.findByTestId('sandbox-inbox'));
    expect(await section.findByText(mock.db().inbox!.url)).toBeInTheDocument();
    expect(section.getByTestId('inbox-url-hint')).toHaveTextContent(INBOX_URL_HINT);
    expect(INBOX_URL_HINT).toBe("Anyone with this address can send to your test address. Don't share it.");
  });
});

describe('endpoint status label', () => {
  it('reads the four states from the endpoint fields', () => {
    expect(endpointStatus({ status: 'enabled', disabled_reason: null, failing_since: null })).toMatchObject({ state: 'enabled', label: 'Enabled' });
    expect(endpointStatus({})).toMatchObject({ state: 'enabled' });
    expect(endpointStatus({ status: 'enabled', failing_since: '2026-11-01T10:00:00Z' })).toMatchObject({ state: 'failing', label: 'Failing since', since: '2026-11-01T10:00:00Z' });
    expect(endpointStatus({ status: 'disabled', disabled_reason: 'merchant', disabled_at: '2026-11-02T10:00:00Z', failing_since: null })).toMatchObject({ state: 'paused', label: 'Paused', since: '2026-11-02T10:00:00Z' });
    expect(endpointStatus({ status: 'disabled', disabled_reason: 'failing', disabled_at: '2026-11-02T10:00:00Z', failing_since: '2026-10-30T10:00:00Z' })).toMatchObject({ state: 'kept_failing', label: 'Disabled: kept failing', since: '2026-10-30T10:00:00Z' });
  });
});

describe('endpoint edit diff', () => {
  const orig = { url: 'https://a.example/h', enabled_events: ['session.paid', 'session.expired'], description: 'Orders' };
  it('sends nothing when nothing changed, whatever the event order', () => {
    expect(endpointPatch(orig, { url: ' https://a.example/h ', events: ['session.expired', 'session.paid'], description: 'Orders' })).toEqual({});
  });
  it('sends only the changed fields', () => {
    expect(endpointPatch(orig, { url: 'https://b.example/h', events: orig.enabled_events, description: 'Orders' })).toEqual({ url: 'https://b.example/h' });
    expect(endpointPatch(orig, { url: orig.url, events: [], description: 'Orders' })).toEqual({ enabled_events: [] });
    expect(endpointPatch(orig, { url: orig.url, events: orig.enabled_events, description: 'New' })).toEqual({ description: 'New' });
  });
  it('clears the description with null', () => {
    expect(endpointPatch(orig, { url: orig.url, events: orig.enabled_events, description: '  ' })).toEqual({ description: null });
    expect(endpointPatch({ ...orig, description: null }, { url: orig.url, events: orig.enabled_events, description: '' })).toEqual({});
  });
});

describe('editing, pausing and enabling', () => {
  const LIVE_URL = 'https://acme.studio/api/prismaport/webhooks';
  const rowOf = async (url: string) => (await screen.findByText(url)).closest('li') as HTMLElement;

  it('shows each endpoint status', async () => {
    renderApp('/developers/webhooks');
    await screen.findAllByTestId('webhook-endpoint');
    const status = async (url: string) => within(await rowOf(url)).getByTestId('endpoint-status');
    expect(await status(LIVE_URL)).toHaveTextContent('Enabled');
    expect(await status('https://billing.acme.studio/hooks')).toHaveTextContent(/^Failing since/);
    expect(await status('https://analytics.acme.studio/hooks')).toHaveTextContent(/^Paused/);
    const kept = await status('https://old.acme.studio/hooks');
    expect(kept).toHaveTextContent('Disabled: kept failing');
    expect(kept).toHaveTextContent('Failing since');
  });

  it('edits only what changed and says the secret stays', async () => {
    const bodies: unknown[] = [];
    const { user } = renderApp('/developers/webhooks', {
      handlers: [
        http.patch('/v1/dashboard/webhook_endpoints/:id', async ({ request }) => {
          bodies.push(await request.clone().json());
          return undefined;
        }),
      ],
    });
    const row = await rowOf(LIVE_URL);
    await user.click(within(row).getByRole('button', { name: /^Edit / }));
    const d = within(await screen.findByRole('dialog', { name: 'Edit webhook endpoint' }));
    expect(d.queryByTestId('url-edit-note')).not.toBeInTheDocument();
    const url = d.getByLabelText('Endpoint address');
    await user.clear(url);
    await user.type(url, 'https://acme.studio/api/v2/hooks');
    expect(d.getByTestId('url-edit-note')).toHaveTextContent(URL_EDIT_NOTE);
    // Moving a Live endpoint takes a confirmation code (D58).
    await emailCode(user, d);
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    expect(await screen.findByText('https://acme.studio/api/v2/hooks')).toBeInTheDocument();
    expect(bodies).toEqual([{ url: 'https://acme.studio/api/v2/hooks', email_code: MOCK_CODE }]);
  });

  it('shows a refusal from the server inside the dialog', async () => {
    const { user } = renderApp('/developers/webhooks', {
      handlers: [http.patch('/v1/dashboard/webhook_endpoints/:id', () => HttpResponse.json({ error: { code: 'invalid_request', message: 'url resolves to a private address' } }, { status: 400 }))],
    });
    const row = await rowOf(LIVE_URL);
    await user.click(within(row).getByRole('button', { name: /^Edit / }));
    const d = within(await screen.findByRole('dialog', { name: 'Edit webhook endpoint' }));
    await user.type(d.getByLabelText(/Description/), ' x');
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    expect(await d.findByText(/private address/)).toBeInTheDocument();
  });

  it('pauses and enables with the consequences spelled out', async () => {
    const { user } = renderApp('/developers/webhooks');
    let row = await rowOf(LIVE_URL);
    await user.click(within(row).getByRole('button', { name: /^Pause / }));
    const pause = within(await screen.findByRole('dialog', { name: 'Pause this endpoint?' }));
    expect(pause.getByText(PAUSE_NOTE)).toBeInTheDocument();
    await user.click(pause.getByRole('button', { name: 'Pause endpoint' }));
    await waitFor(async () => expect(within(await rowOf(LIVE_URL)).getByTestId('endpoint-status')).toHaveTextContent(/^Paused/));
    row = await rowOf(LIVE_URL);
    await user.click(within(row).getByRole('button', { name: /^Enable / }));
    const enable = within(await screen.findByRole('dialog', { name: 'Enable this endpoint?' }));
    expect(enable.getByText(ENABLE_NOTE)).toBeInTheDocument();
    await user.click(enable.getByRole('button', { name: 'Enable endpoint' }));
    await waitFor(async () => expect(within(await rowOf(LIVE_URL)).getByTestId('endpoint-status')).toHaveTextContent('Enabled'));
  });

  it('offers no Edit or Pause on the test webhook events row', async () => {
    const { mock } = renderApp('/developers/webhooks', { mode: 'test' });
    await screen.findAllByTestId('webhook-endpoint');
    const inbox = (await screen.findAllByTestId('webhook-endpoint')).find((r) => within(r).queryByText(mock.db().inbox!.url))!;
    expect(within(inbox).queryByRole('button', { name: /^Edit / })).not.toBeInTheDocument();
    expect(within(inbox).queryByRole('button', { name: /^Pause / })).not.toBeInTheDocument();
  });

  it('adds the optional description on create', async () => {
    const bodies: unknown[] = [];
    const { user } = renderApp('/developers/webhooks', {
      handlers: [
        http.post('/v1/dashboard/webhook_endpoints', async ({ request }) => {
          bodies.push(await request.clone().json());
          return undefined;
        }),
      ],
    });
    await screen.findAllByTestId('webhook-endpoint');
    await user.click(screen.getByRole('button', { name: 'Add endpoint' }));
    const d = within(await screen.findByRole('dialog', { name: 'Add webhook endpoint' }));
    await user.type(d.getByLabelText('Endpoint address'), 'https://acme.studio/hooks/d');
    await user.type(d.getByLabelText(/Description/), 'Fulfilment');
    await user.click(d.getByRole('button', { name: 'Add endpoint' }));
    // A Live endpoint takes a confirmation code first (D58).
    expect(bodies).toEqual([]);
    await emailCode(user, d);
    await user.click(d.getByRole('button', { name: 'Add endpoint' }));
    await screen.findByRole('dialog', { name: 'Webhook endpoint added' });
    expect(bodies).toEqual([{ url: 'https://acme.studio/hooks/d', enabled_events: [], description: 'Fulfilment', email_code: MOCK_CODE }]);
  });
});

describe('deliveries page of a disabled endpoint', () => {
  it('honours ?mode= from the warning email, shows the status and the 409 on resend', async () => {
    const dead = { id: 'whd_01JBDEAD', endpoint_id: 'we_01JBOFF', event_id: 'evt_01JBDEAD', event_type: 'session.paid', status: 'dead', attempts: 12, last_status_code: 500, last_error: 'HTTP 500', last_attempt_at: '2026-10-30T12:00:00Z', next_attempt_at: null, delivered_at: null, created_at: '2026-10-30T12:00:00Z' };
    const off = { id: 'we_01JBOFF', url: 'https://off.acme.studio/hooks', enabled_events: [], livemode: false, created_at: '2026-10-01T10:00:00Z', status: 'disabled', disabled_reason: 'failing', disabled_at: '2026-11-01T10:00:00Z', failing_since: '2026-10-30T10:00:00Z' };
    const seen: string[] = [];
    // Browser last used Live; the email link names Test.
    const { user } = renderApp('/developers/webhooks/we_01JBOFF?mode=test', {
      mode: 'live',
      handlers: [
        http.get('/v1/dashboard/webhook_endpoints', ({ request }) => {
          const m = new URL(request.url).searchParams.get('mode');
          seen.push(m ?? '');
          return HttpResponse.json({ data: m === 'test' ? [off] : [] });
        }),
        http.get('/v1/dashboard/webhook_endpoints/:id/deliveries', () => HttpResponse.json({ data: [dead], has_more: false, next_cursor: null })),
        http.post('/v1/dashboard/webhook_deliveries/:id/resend', () => HttpResponse.json({ error: { code: 'endpoint_disabled', message: 'the endpoint is disabled; enable it before resending' } }, { status: 409 })),
      ],
    });
    expect(await screen.findByTestId('endpoint-status')).toHaveTextContent('Disabled: kept failing');
    expect(seen).toContain('test');
    expect(screen.getByTestId('endpoint-disabled-note')).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: /Resend/ }));
    expect(await screen.findByText(/enable it before resending/i)).toBeInTheDocument();
  });
});
