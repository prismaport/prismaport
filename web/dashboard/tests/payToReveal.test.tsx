import { screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { createMockApi } from '../src/mocks/handlers';
import { renderApp, useMockServer } from './helpers';

useMockServer();

const sessions = () => createMockApi().db().sessions.filter((s) => s.livemode);

describe('payment addresses (D48)', () => {
  it('an open session was never generated: every network says Not generated and no address shows', async () => {
    const ses = sessions().find((s) => s.status === 'open')!;
    expect(ses.pay_to.every((p) => p.address === undefined && p.revealed === false)).toBe(true);
    renderApp(`/payments/${ses.id}`);
    const rows = await screen.findAllByTestId('pay-to');
    expect(rows).toHaveLength(ses.pay_to.length);
    for (const row of rows) expect(within(row).getByTestId('not-generated')).toHaveTextContent('Not generated');
    expect(screen.getByTestId('payment-started')).toHaveTextContent('Not yet');
    expect(screen.getByText('Selection ends')).toBeInTheDocument();
  });

  it('a paid session shows the generated network with its address and time, the others as Not generated', async () => {
    const ses = sessions().find((s) => s.status === 'paid')!;
    renderApp(`/payments/${ses.id}`);
    const rows = await screen.findAllByTestId('pay-to');
    const generated = rows.filter((r) => within(r).queryByTestId('revealed-at'));
    const bare = rows.filter((r) => within(r).queryByTestId('not-generated'));
    expect(generated.length).toBeGreaterThan(0);
    expect(generated.length + bare.length).toBe(rows.length);
    expect(generated[0]!).toHaveTextContent('Generated');
    expect(screen.getByTestId('payment-started')).not.toHaveTextContent('Not yet');
    expect(screen.getByText('Expires')).toBeInTheDocument();
  });

  it('the seed has open, generated and paid examples, and no address for an unrevealed network', () => {
    const all = sessions();
    expect(all.some((s) => s.payment_started_at === null)).toBe(true);
    expect(all.some((s) => s.payment_started_at && s.pay_to.some((p) => p.revealed) && s.pay_to.some((p) => !p.revealed))).toBe(true);
    for (const s of all) for (const p of s.pay_to) expect(p.revealed ? p.address : p.address ?? null).toBe(p.revealed ? p.address : null);
  });
});
