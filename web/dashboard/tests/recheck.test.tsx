import { screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { api } from '../src/lib/endpoints';
import { ApiError } from '../src/lib/api';
import { canRecheck, recheckError, RECHECK_COPY, watchEnd } from '../src/lib/recheck';
import { createMockApi } from '../src/mocks/handlers';
import { renderApp, useMockServer } from './helpers';

useMockServer();

const H = 3600_000;
const seed = () => createMockApi().db();
const old = () => seed().sessions.find((s) => s.livemode && s.status === 'expired' && Date.now() > Date.parse(s.expires_at) + 23.5 * H)!;

describe('recheck helpers', () => {
  const s = { status: 'expired', expires_at: '2026-10-23T19:00:00Z' };
  it('watch window ends 23 h 30 min after expires_at, or at watch_until when sent', () => {
    expect(watchEnd(s)).toBe(Date.parse('2026-10-24T18:30:00Z'));
    expect(watchEnd({ ...s, watch_until: '2026-10-25T00:00:00Z' })).toBe(Date.parse('2026-10-25T00:00:00Z'));
  });
  it('only past the window and never for paid sessions', () => {
    expect(canRecheck(s, Date.parse('2026-10-24T18:29:59Z'))).toBe(false);
    expect(canRecheck(s, Date.parse('2026-10-24T18:30:00Z'))).toBe(true);
    expect(canRecheck({ ...s, status: 'paid' }, Date.parse('2026-12-01T00:00:00Z'))).toBe(false);
  });
  it('explains the 409 of a session nobody generated a payment for', () => {
    expect(recheckError(new ApiError(409, 'session_not_revealed', 'x'))).toBe(RECHECK_COPY.notRevealed);
  });
  it('follows recheck_available when the server sends it', () => {
    const now = Date.parse('2026-10-23T19:30:00Z');
    expect(canRecheck({ status: 'paid', expires_at: '2026-10-23T19:00:00Z', recheck_available: true }, now)).toBe(true);
    expect(canRecheck({ status: 'paid', expires_at: '2026-10-23T19:00:00Z', recheck_available: false }, now)).toBe(false);
    expect(canRecheck({ ...s, recheck_available: true }, now)).toBe(true);
    expect(canRecheck({ ...s, recheck_available: false }, Date.parse('2027-01-01T00:00:00Z'))).toBe(false);
  });
});

describe('Check again', () => {
  it('is idle for an old unpaid session, goes pending after the 202 and cannot be pressed twice', async () => {
    const ses = old();
    let calls = 0;
    const { user } = renderApp(`/payments/${ses.id}`, { handlers: [http.post('/v1/dashboard/sessions/:id/recheck', () => { calls++; return HttpResponse.json({}, { status: 202 }); })] });
    const btn = await screen.findByTestId('recheck-button');
    expect(btn).toBeEnabled();
    expect(screen.queryByTestId('recheck-pending')).toBeNull();
    await user.click(btn);
    expect(await screen.findByText('Checking the deposit address…')).toBeInTheDocument();
    expect(screen.getByTestId('recheck-button')).toBeDisabled();
    expect(calls).toBe(1);
  });

  it('does not show for a session still inside its watch window', async () => {
    const ses = seed().sessions.find((s) => s.livemode && Date.now() < Date.parse(s.expires_at) + 23.5 * H);
    if (!ses) return;
    renderApp(`/payments/${ses.id}`);
    await screen.findByTestId('payment-link-name').catch(() => undefined);
    await waitFor(() => expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument());
    expect(screen.queryByTestId('recheck-button')).toBeNull();
  });

  it('shows for a paid session whose payments are final (D47), with its own copy', async () => {
    const ses = seed().sessions.find((s) => s.livemode && s.status === 'paid')!;
    renderApp(`/payments/${ses.id}`);
    expect(await screen.findByTestId('recheck-button')).toBeEnabled();
    expect(screen.getByTestId('recheck')).toHaveTextContent('This payment is complete');
  });

  it('does not show for a paid session the server says is not final yet', async () => {
    const ses = seed().sessions.find((s) => s.livemode && s.status === 'paid')!;
    renderApp(`/payments/${ses.id}`, {
      handlers: [http.get(`/v1/dashboard/sessions/${ses.id}`, () => HttpResponse.json({ ...createMockApi().db().sessions.find((x) => x.id === ses.id), recheck_available: false }))],
    });
    await screen.findByTestId('from-product');
    expect(screen.queryByTestId('recheck-button')).toBeNull();
  });

  it('never shows for an overpaid session', async () => {
    const ses = seed().sessions.find((s) => s.livemode && s.status === 'overpaid');
    if (!ses) return;
    renderApp(`/payments/${ses.id}`);
    await screen.findByTestId('from-product');
    expect(screen.queryByTestId('recheck-button')).toBeNull();
  });

  it('409 says the session is still watched', async () => {
    const { user } = renderApp(`/payments/${old().id}`, {
      handlers: [http.post('/v1/dashboard/sessions/:id/recheck', () => HttpResponse.json({ error: { code: 'session_still_watched', message: 'x' } }, { status: 409 }))],
    });
    await user.click(await screen.findByTestId('recheck-button'));
    expect(await screen.findByTestId('recheck-error')).toHaveTextContent('still being watched automatically');
    expect(screen.getByTestId('recheck-button')).toBeEnabled();
  });

  it('429 says to try again in minutes', async () => {
    const { user } = renderApp(`/payments/${old().id}`, {
      handlers: [http.post('/v1/dashboard/sessions/:id/recheck', () => HttpResponse.json({ error: { code: 'rate_limited', message: 'x' } }, { status: 429, headers: { 'Retry-After': '540' } }))],
    });
    await user.click(await screen.findByTestId('recheck-button'));
    expect(await screen.findByTestId('recheck-error')).toHaveTextContent('Try again in 9 minutes.');
  });

  it('the default mock answers 202 then rate-limits a second check', async () => {
    const ses = old();
    const { mock } = renderApp(`/payments/${ses.id}`);
    await screen.findByTestId('recheck-button');
    void mock;
    await expect(api.recheckSession('live', ses.id)).resolves.toMatchObject({ status: 'queued' });
    await expect(api.recheckSession('live', ses.id)).rejects.toMatchObject({ status: 429, retryAfter: 600 });
  });
});
