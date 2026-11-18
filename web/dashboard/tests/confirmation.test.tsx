import { QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { ConfirmFields, useConfirmation } from '../src/components/Confirmation';
import { makeQueryClient } from '../src/lib/queries';
import { createMockApi } from '../src/mocks/handlers';
import { server, useMockServer } from './helpers';

useMockServer();

function Harness() {
  const s = useConfirmation('wallet_create', 'live');
  return <ConfirmFields s={s} idPrefix="t" />;
}

function setup(opts: { twofa?: boolean; send: 'limited' | 'unavailable' | 'failed-once' }) {
  const mock = createMockApi({ email: true, confirmWindow: false });
  mock.signIn();
  if (opts.twofa) mock.db().users[0]!.totp_enabled = true;
  let calls = 0;
  const send =
    opts.send === 'failed-once'
      ? http.post('/v1/dashboard/confirmations', () =>
          ++calls === 1
            ? HttpResponse.json({ error: { code: 'email_send_failed', message: "We couldn't send the code. Try again in a moment." } }, { status: 503 })
            : HttpResponse.json({ sent_to: 'o***@acme.studio', expires_in: 900, resend_after: 60 }),
        )
      : opts.send === 'limited'
      ? http.post('/v1/dashboard/confirmations', () => HttpResponse.json({ error: { code: 'rate_limited', message: 'A code was just sent. Wait 42 seconds.' } }, { status: 429, headers: { 'Retry-After': '42' } }))
      : http.post('/v1/dashboard/confirmations', () => HttpResponse.json({ error: { code: 'service_unavailable', message: 'Codes by email are not set up.' } }, { status: 503 }));
  server.use(...mock.handlers);
  server.use(send);
  const client = makeQueryClient();
  client.setDefaultOptions({ queries: { retry: false }, mutations: { retry: false } });
  const user = userEvent.setup();
  render(
    <QueryClientProvider client={client}>
      <Harness />
    </QueryClientProvider>,
  );
  return user;
}

describe('Confirmation', () => {
  it('after a 429 counts down from Retry-After and still takes a code', async () => {
    const user = setup({ send: 'limited' });
    await user.click(await screen.findByRole('button', { name: 'Email me a code' }));
    expect(await screen.findByLabelText('Code from your email')).toBeInTheDocument();
    // A busy runner may take a few seconds to get here: the wait only has to start near Retry-After.
    expect(screen.getByRole('button', { name: /Send a new code in (3\d|4[0-2]) s/ })).toBeDisabled();
    expect(screen.getByText(/Try again in 42 seconds/)).toBeInTheDocument();
    await user.type(screen.getByLabelText('Code from your email'), '246810');
    expect(screen.getByLabelText('Code from your email')).toHaveValue('246810');
  });

  it('after a failed send shows the message, keeps the choice and lets another send through', async () => {
    const user = setup({ twofa: true, send: 'failed-once' });
    await user.click(await screen.findByRole('radio', { name: /Code by email/ }));
    await user.click(await screen.findByRole('button', { name: 'Email me a code' }));
    expect(await screen.findByText(/We couldn't send the code/)).toBeInTheDocument();
    // Momentary, not "no email here": both methods and the button stay.
    expect(screen.getAllByRole('radio')).toHaveLength(2);
    const again = screen.getByRole('button', { name: 'Email me a code' });
    expect(again).toBeEnabled();
    await user.click(again);
    expect(await screen.findByLabelText('Code from your email')).toBeInTheDocument();
  });

  it('when email is unavailable and 2FA is on, falls back to the authenticator', async () => {
    const user = setup({ twofa: true, send: 'unavailable' });
    await user.click(await screen.findByRole('radio', { name: /Code by email/ }));
    await user.click(await screen.findByRole('button', { name: 'Email me a code' }));
    expect(await screen.findByLabelText('Code from your authenticator app')).toBeInTheDocument();
    expect(screen.queryByRole('radio')).toBeNull();
    expect(screen.queryByRole('button', { name: /code/i })).toBeNull();
  });

  it('when email is unavailable and 2FA is off, says it cannot confirm now', async () => {
    const user = setup({ send: 'unavailable' });
    await user.click(await screen.findByRole('button', { name: 'Email me a code' }));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/can.t confirm this right now/));
    expect(screen.queryByRole('button', { name: 'Email me a code' })).toBeNull();
    expect(screen.queryByLabelText('Code from your email')).toBeNull();
  });
});
