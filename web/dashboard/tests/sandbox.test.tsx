import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { formatSkew, prettyBody, SIM_ACTIONS, simActionInfo, simAllowed, simAmountError } from '../src/lib/sandbox';
import { WEBHOOK_EVENT_TYPES } from '../src/lib/status';
import { renderApp, useMockServer } from './helpers';

useMockServer();

describe('sandbox helpers', () => {
  it('indents JSON bodies and keeps anything else as received', () => {
    expect(prettyBody('{"a":1,"b":[2]}')).toEqual({ text: '{\n  "a": 1,\n  "b": [\n    2\n  ]\n}', json: true });
    expect(prettyBody('ok')).toEqual({ text: 'ok', json: false });
    expect(prettyBody('  ')).toEqual({ text: '', json: false });
  });

  it('says how far from its signature a request arrived', () => {
    expect(formatSkew(null)).toBeNull();
    expect(formatSkew(0)).toBe('on time');
    expect(formatSkew(12)).toBe('12 s after it was signed');
    expect(formatSkew(600)).toBe('10 min after it was signed');
    expect(formatSkew(-360)).toBe('6 min before it was signed');
    expect(formatSkew(4 * 3600)).toBe('4 h after it was signed');
  });

  it('offers only the actions the session state allows', () => {
    expect(SIM_ACTIONS.map((a) => a.action)).toEqual(['pay_exact', 'pay_partial', 'overpay', 'expire', 'late_payment', 'reverse_last_payment', 'under_review']);
    const allowed = (status: Parameters<typeof simAllowed>[1], reversible = false) => SIM_ACTIONS.filter((a) => simAllowed(a, status, reversible)).map((a) => a.action);
    expect(allowed('open')).toEqual(['pay_exact', 'pay_partial', 'overpay', 'expire', 'late_payment', 'under_review']);
    expect(allowed('paid', true)).toEqual(['reverse_last_payment', 'under_review']);
    expect(allowed('expired')).toEqual(['late_payment', 'under_review']);
    expect(allowed('under_review', true)).toEqual(['reverse_last_payment']);
    expect(simActionInfo('late_payment').amount).toBe(true);
    expect(simActionInfo('expire').events).toEqual(['session.expired']);
  });

  it('checks a synthetic amount like the server', () => {
    expect(simAmountError('')).toBeUndefined();
    expect(simAmountError('10.50')).toBeUndefined();
    expect(simAmountError('0.000001')).toBeUndefined();
    expect(simAmountError('1.0000001')).toMatch(/six decimals/);
    expect(simAmountError('ten')).toMatch(/six decimals/);
    expect(simAmountError('0.00')).toMatch(/above zero/);
  });
});

describe('send test event', () => {
  it('sends a signed sample to a test endpoint and shows the request and the response', async () => {
    const sent: { url: string; body: unknown }[] = [];
    const { mock, user } = renderApp('/developers/webhooks', {
      mode: 'test',
      handlers: [
        http.post('*/v1/dashboard/webhook_endpoints/:id/test', async ({ request }) => {
          sent.push({ url: request.url, body: await request.clone().json() });
          return undefined;
        }),
      ],
    });
    const staging = mock.db().endpoints.find((e) => e.url === 'https://staging.acme.studio/hooks')!;
    await user.click(await screen.findByRole('button', { name: `Send test event to ${staging.url}` }));
    const d = within(await screen.findByRole('dialog', { name: 'Send test event' }));
    const select = d.getByLabelText('Event');
    expect(within(select).getAllByRole('option')).toHaveLength(WEBHOOK_EVENT_TYPES.length);
    await user.selectOptions(select, 'session.overpaid');
    await user.click(d.getByRole('button', { name: 'Send test event' }));
    expect(await d.findByRole('heading', { name: 'Response' })).toBeInTheDocument();
    expect(sent).toHaveLength(1);
    expect(new URL(sent[0]!.url).pathname).toBe(`/v1/dashboard/webhook_endpoints/${staging.id}/test`);
    expect(new URL(sent[0]!.url).searchParams.get('mode')).toBe('test');
    expect(sent[0]!.body).toEqual({ type: 'session.overpaid' });
    const headers = within(d.getByLabelText('Request headers'));
    expect(headers.getByText('Signature')).toBeInTheDocument();
    expect(headers.getByText(/^t=\d+,v1=[0-9a-f]{64}$/)).toBeInTheDocument();
    expect(d.getByLabelText('Request body').textContent).toContain('"type": "session.overpaid"');
    expect(d.getByText('Delivered')).toBeInTheDocument();
    expect(d.getByText(/HTTP 200 in \d+ ms/)).toBeInTheDocument();
    expect(mock.db().deliveries.filter((x) => x.test)).toHaveLength(1);
    await user.click(d.getByRole('button', { name: 'Send another' }));
    expect(d.getByLabelText('Event')).toBeInTheDocument();
  });

  it('shows why a send failed when the endpoint did not answer', async () => {
    const { mock, user } = renderApp('/developers/webhooks', {
      mode: 'test',
      handlers: [
        http.post('*/v1/dashboard/webhook_endpoints/:id/test', ({ params }) =>
          HttpResponse.json({
            event: { id: 'evt_1', type: 'session.paid' },
            delivery: { id: 'whd_1', endpoint_id: params.id, event_id: 'evt_1', event_type: 'session.paid', status: 'dead', attempts: 1, last_status_code: null, last_error: 'connection refused', last_attempt_at: null, next_attempt_at: null, delivered_at: null, created_at: '2026-10-25T12:00:00Z', test: true },
            request: { method: 'POST', url: 'https://staging.acme.studio/hooks', headers: [{ name: 'Signature', value: 't=1,v1=ab' }], body: '{"id":"evt_1"}' },
            response: null,
            error: 'dial tcp: connection refused',
          }),
        ),
      ],
    });
    const staging = mock.db().endpoints.find((e) => e.url === 'https://staging.acme.studio/hooks')!;
    await user.click(await screen.findByRole('button', { name: `Send test event to ${staging.url}` }));
    const d = within(await screen.findByRole('dialog', { name: 'Send test event' }));
    await user.click(d.getByRole('button', { name: 'Send test event' }));
    expect(await d.findByText('dial tcp: connection refused')).toBeInTheDocument();
    expect(d.getByText('Failed')).toBeInTheDocument();
    expect(d.getByText('No HTTP response came back.')).toBeInTheDocument();
  });

  it('is not offered in Live mode, which points to Test mode instead', async () => {
    const { user } = renderApp('/developers/webhooks', { mode: 'live' });
    await screen.findAllByTestId('webhook-endpoint');
    expect(screen.queryByRole('button', { name: /^Send test event/ })).not.toBeInTheDocument();
    expect(screen.queryByTestId('sandbox-inbox')).not.toBeInTheDocument();
    await user.click(within(screen.getByTestId('sandbox-live-note')).getByRole('button', { name: 'Switch to Test mode' }));
    expect(await screen.findByTestId('sandbox-inbox')).toBeInTheDocument();
    expect(await screen.findByTestId('test-strip')).toBeInTheDocument();
  });

  it('marks test deliveries in the log and never offers to resend them', async () => {
    const { mock, user } = renderApp('/developers/webhooks', { mode: 'test' });
    const staging = mock.db().endpoints.find((e) => e.url === 'https://staging.acme.studio/hooks')!;
    mock.db().deliveries.push({ id: 'whd_01JBTEST', endpoint_id: staging.id, event_id: 'evt_01JBTEST', event_type: 'session.paid', status: 'succeeded', attempts: 1, last_status_code: 200, last_error: null, last_attempt_at: '2026-10-25T12:00:00Z', next_attempt_at: null, delivered_at: '2026-10-25T12:00:00Z', created_at: '2099-01-01T00:00:00Z', test: true });
    await screen.findAllByTestId('webhook-endpoint');
    await user.click(screen.getAllByRole('link', { name: 'Deliveries' }).find((a) => a.getAttribute('href')?.endsWith(staging.id))!);
    const rows = await screen.findAllByTestId('delivery');
    const test = rows.find((r) => within(r).queryByText('Test event'))!;
    expect(test).toBeDefined();
    expect(within(test).queryByRole('button', { name: 'Resend' })).not.toBeInTheDocument();
    expect(within(test).getByText('Sent once, not resent')).toBeInTheDocument();
  });
});

describe('test webhook events', () => {
  it('lists received requests with the signature check, headers and the body as JSON', async () => {
    const { mock, user } = renderApp('/developers/webhooks', { mode: 'test' });
    const inbox = mock.db().inbox!;
    expect(await screen.findByText(inbox.url)).toBeInTheDocument();
    const endpointRow = screen.getAllByTestId('webhook-endpoint').find((r) => within(r).queryByText(inbox.url))!;
    expect(within(endpointRow).getByText('Test webhook events')).toBeInTheDocument();
    const rows = await screen.findAllByTestId('inbox-request');
    expect(rows).toHaveLength(mock.db().inboxRequests.length);
    const bad = rows.find((r) => within(r).queryByText('Signature not valid'))!;
    await user.click(within(bad).getByText('Signature not valid'));
    expect(within(bad).getByTestId('signature-check')).toHaveTextContent('No v1 signature matches the endpoint secret');
    const good = rows.find((r) => within(r).queryByText('Signature valid'))!;
    await user.click(within(good).getByText('Signature valid'));
    expect(within(good).getByTestId('signature-check')).toHaveTextContent('within the 5-minute tolerance');
    expect(within(good).getByTestId('signature-check')).toHaveTextContent('arrived 1 s after it was signed');
    expect(within(within(good).getByLabelText('Received headers')).getByText('Prismaport-Event-Id')).toBeInTheDocument();
    expect(within(good).getByLabelText('Received body').textContent).toMatch(/^\{\n {2}"id": "evt_/);
  });

  it('deletes the inbox, creates a new one showing its secret once, and receives a test event', async () => {
    const { mock, user } = renderApp('/developers/webhooks', { mode: 'test' });
    const old = mock.db().inbox!;
    const section = within(await screen.findByTestId('sandbox-inbox'));
    await screen.findAllByTestId('inbox-request');
    await user.click(section.getByRole('button', { name: 'Delete test address' }));
    await user.click(within(await screen.findByRole('dialog', { name: 'Delete the test address?' })).getByRole('button', { name: 'Delete test address' }));
    await user.click(await section.findByRole('button', { name: 'Create test address' }));
    const fresh = await waitFor(() => {
      const i = mock.db().inbox;
      if (!i || i.id === old.id) throw new Error('not created yet');
      return i;
    });
    expect(await section.findByText(fresh.url)).toBeInTheDocument();
    expect(section.getByText(fresh.secret)).toBeInTheDocument();
    expect(await section.findByText(/Nothing received yet/)).toBeInTheDocument();
    expect(screen.queryByText(old.url)).not.toBeInTheDocument();

    await user.click(section.getByRole('button', { name: 'Send test event' }));
    const d = within(await screen.findByRole('dialog', { name: 'Send test event' }));
    await user.click(d.getByRole('button', { name: 'Send test event' }));
    expect(await d.findByText(/"received": true/)).toBeInTheDocument();
    await user.click(d.getByRole('button', { name: 'Done' }));
    const rows = await section.findAllByTestId('inbox-request');
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveTextContent('Paid');
    expect(rows[0]).toHaveTextContent('Signature valid');
  });

  it('offers to create an inbox when there is none', async () => {
    const { mock, user } = renderApp('/developers/webhooks', {
      mode: 'test',
      handlers: [http.get('*/v1/dashboard/sandbox/inbox', () => HttpResponse.json({ inbox: null }))],
    });
    const section = within(await screen.findByTestId('sandbox-inbox'));
    expect(await section.findByText(/Prismaport sends every Test mode event to it/)).toBeInTheDocument();
    expect(section.queryByTestId('inbox-request')).not.toBeInTheDocument();
    expect(mock.db().inbox).not.toBeNull();
    expect(section.getByRole('button', { name: 'Create test address' })).toBeEnabled();
    await user.click(section.getByRole('button', { name: 'Create test address' }));
    // The mock already has one, so the create answers it without a secret.
    await waitFor(() => expect(section.queryByText(mock.db().inbox!.secret)).not.toBeInTheDocument());
  });
});

describe('simulate', () => {
  it('runs a step on a Test mode payment and shows the new state and events', async () => {
    const posted: unknown[] = [];
    const { mock, user } = renderApp('/payments', {
      mode: 'test',
      handlers: [
        http.post('*/v1/dashboard/sessions/:id/simulate', async ({ request }) => {
          posted.push(await request.clone().json());
          return undefined;
        }),
      ],
    });
    const open = mock.db().sessions.find((s) => !s.livemode && s.status === 'open')!;
    const before = open.events.length;
    await screen.findAllByTestId('session-row');
    await user.click(screen.getAllByTestId('session-row').find((r) => r.getAttribute('href')?.endsWith(open.id))!);
    await user.click(await screen.findByRole('button', { name: 'Simulate' }));
    const d = within(await screen.findByRole('dialog', { name: 'Simulate' }));
    expect(d.getByRole('radio', { name: /Reverse the last payment/ })).toBeDisabled();
    await user.click(d.getByRole('radio', { name: /Pay the exact amount/ }));
    await user.click(d.getByRole('button', { name: 'Run simulation' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Simulate' })).not.toBeInTheDocument());
    expect(posted).toEqual([{ action: 'pay_exact' }]);
    expect(await screen.findByText(/Simulated: Payment seen on the network, Paid/)).toBeInTheDocument();
    const payment = await screen.findByTestId('payment');
    expect(within(payment).getByText('Simulated')).toBeInTheDocument();
    expect(within(payment).queryByRole('link', { name: /View transaction/ })).not.toBeInTheDocument();
    expect(mock.db().sessions.find((s) => s.id === open.id)!.events.length).toBe(before + 2);
    // Every Test mode event reached the inbox too.
    expect(mock.db().inboxRequests[0]!.event_type).toBe('session.paid');
  });

  it('checks the amount before sending, and shows a refusal in the dialog', async () => {
    const posted: unknown[] = [];
    const { mock, user } = renderApp('/payments', {
      mode: 'test',
      handlers: [
        http.post('*/v1/dashboard/sessions/:id/simulate', async ({ request }) => {
          posted.push(await request.clone().json());
          return HttpResponse.json({ error: { code: 'invalid_state', message: 'the session cannot do that in its current state' } }, { status: 409 });
        }),
      ],
    });
    const open = mock.db().sessions.find((s) => !s.livemode && s.status === 'open')!;
    await screen.findAllByTestId('session-row');
    await user.click(screen.getAllByTestId('session-row').find((r) => r.getAttribute('href')?.endsWith(open.id))!);
    await user.click(await screen.findByRole('button', { name: 'Simulate' }));
    const d = within(await screen.findByRole('dialog', { name: 'Simulate' }));
    await user.click(d.getByRole('radio', { name: /Pay part of it/ }));
    await user.type(d.getByLabelText(/Amount in USDC/), '1.1234567');
    await user.click(d.getByRole('button', { name: 'Run simulation' }));
    expect(d.getByText('Use digits with at most six decimals, such as 10.50.')).toBeInTheDocument();
    expect(posted).toHaveLength(0);
    await user.clear(d.getByLabelText(/Amount in USDC/));
    await user.type(d.getByLabelText(/Amount in USDC/), '0.40');
    await user.click(d.getByRole('button', { name: 'Run simulation' }));
    expect(await d.findByText('the session cannot do that in its current state')).toBeInTheDocument();
    expect(posted).toEqual([{ action: 'pay_partial', amount: '0.40' }]);
  });

  it('is only offered on Test mode payments', async () => {
    const { mock, user } = renderApp('/payments', { mode: 'live' });
    const rows = await screen.findAllByTestId('session-row');
    const live = mock.db().sessions.find((s) => s.livemode && rows[0]!.getAttribute('href')?.endsWith(s.id))!;
    expect(live).toBeDefined();
    await user.click(rows[0]!);
    expect(await screen.findByRole('heading', { name: 'Timeline' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Simulate' })).not.toBeInTheDocument();
  });
});
