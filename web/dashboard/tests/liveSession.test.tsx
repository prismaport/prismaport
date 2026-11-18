import { act, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SESSION_POLL_MS } from '../src/lib/queries';
import { sessionInProgress } from '../src/lib/status';
import type { Payment, PaymentStatus, SessionStatus } from '../src/lib/types';
import { createMockApi } from '../src/mocks/handlers';
import { renderApp, server, useMockServer } from './helpers';

useMockServer();
afterEach(() => {
  server.events.removeAllListeners();
  vi.useRealTimers();
});

const pay = (status: PaymentStatus) => ({ status }) as Payment;
const sessions = () => createMockApi().db().sessions;

/** GETs of one session, counted as they start. */
function countReads(id: string) {
  const reads = { n: 0 };
  server.events.on('request:start', ({ request }) => {
    if (request.method === 'GET' && new URL(request.url).pathname === `/v1/dashboard/sessions/${id}`) reads.n++;
  });
  return reads;
}

describe('a payment that can still change', () => {
  it('is open, partly paid or has a transfer confirming; a final one with final transfers is not', () => {
    for (const status of ['open', 'partially_paid'] as SessionStatus[]) expect(sessionInProgress({ status, payments: [] })).toBe(true);
    for (const status of ['paid', 'overpaid', 'expired', 'late_payment', 'under_review'] as SessionStatus[]) {
      expect(sessionInProgress({ status, payments: [pay('confirmed'), pay('finalized'), pay('reversed')] })).toBe(false);
      expect(sessionInProgress({ status, payments: [pay('finalized'), pay('detected')] })).toBe(true);
    }
  });

  it('says it updates live, then picks up the change by itself and stops checking', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    const ses = sessions().find((s) => s.livemode && s.status === 'open' && s.payments.length === 0)!;
    const reads = countReads(ses.id);
    const { mock } = renderApp(`/payments/${ses.id}`);
    const live = await screen.findByTestId('updating-live');
    expect(live).toHaveAttribute('role', 'status');
    expect(live).toHaveTextContent(/^Updating live · checked \d+ s ago$/);
    expect(screen.getByRole('heading', { level: 1 }).closest('.page-head')).toContainElement(live);
    expect(reads.n).toBe(1);

    mock.db().sessions.find((s) => s.id === ses.id)!.status = 'paid';
    act(() => vi.advanceTimersByTime(SESSION_POLL_MS));
    await waitFor(() => expect(reads.n).toBe(2));
    expect(await screen.findByText('Paid', { selector: '.pill' })).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByTestId('updating-live')).toBeNull());

    act(() => vi.advanceTimersByTime(SESSION_POLL_MS * 3));
    await new Promise((r) => setTimeout(r, 50));
    expect(reads.n).toBe(2);
  });

  it('keeps showing the payment when a background check fails', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    const ses = sessions().find((s) => s.livemode && s.status === 'open' && s.payments.length === 0)!;
    const reads = countReads(ses.id);
    renderApp(`/payments/${ses.id}`);
    await screen.findByTestId('updating-live');
    server.use(http.get('*/v1/dashboard/sessions/:id', () => HttpResponse.json({ error: { code: 'internal_error', message: 'down' } }, { status: 503 })));
    act(() => vi.advanceTimersByTime(SESSION_POLL_MS));
    await waitFor(() => expect(reads.n).toBe(2));
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument();
    expect(screen.getByTestId('updating-live')).toBeInTheDocument();
  });

  it('a paid payment whose transfers are final is not checked again', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    const ses = sessions().find((s) => s.livemode && s.status === 'paid' && s.payments.every((p) => p.status !== 'detected'))!;
    const reads = countReads(ses.id);
    renderApp(`/payments/${ses.id}`);
    expect(await screen.findByTestId('settlement')).toBeInTheDocument();
    expect(screen.queryByTestId('updating-live')).toBeNull();
    act(() => vi.advanceTimersByTime(SESSION_POLL_MS * 3));
    await new Promise((r) => setTimeout(r, 50));
    expect(reads.n).toBe(1);
  });
});

describe('the Live marker', () => {
  it('shows under the lockup and in the phone bar in Live mode only', async () => {
    renderApp('/payments');
    await screen.findAllByTestId('session-row');
    expect(screen.getAllByTestId('live-badge').map((b) => b.textContent)).toEqual(['Live', 'Live · real payments']);
    expect(screen.queryByTestId('test-strip')).toBeNull();
  });

  it('gives way to the Test strip in Test mode', async () => {
    renderApp('/payments', { mode: 'test' });
    expect(await screen.findByTestId('test-strip')).toHaveTextContent('Test mode');
    expect(screen.queryByTestId('live-badge')).toBeNull();
  });

  it('tags a live payment Live and a test one Test', async () => {
    const live = sessions().find((s) => s.livemode)!;
    const { unmount } = renderApp(`/payments/${live.id}`);
    const tag = await screen.findByTestId('mode-tag');
    expect(tag).toHaveTextContent('Live');
    expect(tag).toHaveClass('tag', 'tag-live');
    unmount();

    const test = sessions().find((s) => !s.livemode)!;
    renderApp(`/payments/${test.id}`, { mode: 'test' });
    const testTag = await screen.findByTestId('mode-tag');
    expect(testTag).toHaveTextContent('Test');
    expect(testTag).not.toHaveClass('tag-live');
  });

  it('tags a live customer address Live', async () => {
    const ca = createMockApi().db().customerAddresses!.find((c) => c.livemode)!;
    renderApp(`/customers/${ca.id}`);
    const head = (await screen.findByRole('heading', { level: 1, name: ca.customer_id })).closest('.page-head') as HTMLElement;
    expect(within(head).getByTestId('mode-tag')).toHaveTextContent('Live');
  });
});
