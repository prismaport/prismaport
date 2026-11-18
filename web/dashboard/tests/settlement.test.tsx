import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { payoutUnits } from '../src/lib/splits';
import { units } from '../src/mocks/db';
import { createMockApi } from '../src/mocks/handlers';
import { settlementCheck } from '../src/pages/PaymentDetail';
import { renderApp, useMockServer } from './helpers';

useMockServer();

const seed = () => createMockApi().db();

/** A paid Founder plan payment on Base: 70% Founder, 30% Partner. */
function founderPlanOnBase() {
  const db = seed();
  const product = db.products.find((p) => p.name === 'Founder plan')!;
  return db.sessions.find((s) => s.product_id === product.id && s.status === 'paid' && s.settlement?.chain === 'base')!;
}

describe('settlement check', () => {
  it('adds the fee and the payouts in exact units and compares them with the gross', () => {
    expect(settlementCheck({ gross: '25.000000', fee: '0.250000', payouts: [{ wallet_id: 'a', label: 'A', address: 'x', amount: '17.325000' }, { wallet_id: 'b', label: 'B', address: 'y', amount: '7.425000' }] })).toEqual({ ok: true, paid: '24.750000', total: '25.000000' });
    expect(settlementCheck({ gross: '25.000000', fee: '0.250000', payouts: [{ wallet_id: 'a', label: 'A', address: 'x', amount: '24.749999' }] })).toEqual({ ok: false, paid: '24.749999', total: '24.999999' });
    expect(settlementCheck({ gross: '25.000000', fee: '0.250000' })).toBeNull();
    expect(settlementCheck({ gross: '25.000000', fee: '0.250000', payouts: [] })).toBeNull();
  });
});

describe('payment detail settlement', () => {
  it('shows the fee, then one row per wallet with its network, address and exact amount, then the check', async () => {
    const s = founderPlanOnBase();
    const st = s.settlement!;
    renderApp(`/payments/${s.id}`);
    const card = await screen.findByTestId('settlement');
    expect(within(card).getByText('Prismaport fee (0.90%)')).toBeInTheDocument();
    expect(within(card).getByText(`${st.fee} USDC`)).toBeInTheDocument();
    expect(within(card).queryByText('Net to your wallet')).toBeNull();
    const rows = within(card).getAllByTestId('payout');
    expect(rows).toHaveLength(2);
    const [founder, partner] = rows.map((r) => within(r));
    expect(founder!.getByText('Founder')).toBeInTheDocument();
    expect(founder!.getByText('Base', { selector: '.network-badge span' })).toBeInTheDocument();
    expect(founder!.getByText('0x7a3F…e91c')).toBeInTheDocument();
    expect(founder!.getByRole('button', { name: 'Copy Founder address' })).toBeInTheDocument();
    expect(partner!.getByText('Partner')).toBeInTheDocument();
    expect(partner!.getByText('0x3C9e…8F02')).toBeInTheDocument();
    const [f, p] = payoutUnits(units(st.net), [7000, 3000]);
    expect(rows[0]).toHaveTextContent(`${(Number(f) / 1e6).toFixed(6)} USDC`);
    expect(rows[1]).toHaveTextContent(`${(Number(p) / 1e6).toFixed(6)} USDC`);
    expect(within(card).getByTestId('settlement-check')).toHaveTextContent(`Fee ${st.fee} + payouts ${st.net} = ${st.gross} USDC received.`);
    expect(within(card).getByTestId('settlement-check')).toHaveAttribute('data-ok', 'true');
  });

  it('says so when fee and payouts do not add up to what was received', async () => {
    const s = founderPlanOnBase();
    renderApp(`/payments/${s.id}`, {
      handlers: [
        http.get('/v1/dashboard/sessions/:id', () => {
          const st = { ...s.settlement!, payouts: s.settlement!.payouts!.map((x, i) => (i === 0 ? { ...x, amount: '1.000000' } : x)) };
          return HttpResponse.json({ ...s, settlement: st, events: [], explorer_links: [] });
        }),
      ],
    });
    const check = await screen.findByTestId('settlement-check');
    expect(check).not.toHaveAttribute('data-ok');
    expect(check).toHaveTextContent(/^Fee and payouts add up to \d+\.\d{6} USDC, not the 25\.000000 USDC received\. Contact us with this session ID\.$/);
  });

  it('keeps the net line for a settlement from before payout splits', async () => {
    const s = founderPlanOnBase();
    renderApp(`/payments/${s.id}`, {
      handlers: [
        http.get('/v1/dashboard/sessions/:id', () => {
          const { payouts: _p, ...legacy } = s.settlement!;
          return HttpResponse.json({ ...s, settlement: legacy, events: [], explorer_links: [] });
        }),
      ],
    });
    const card = await screen.findByTestId('settlement');
    expect(within(card).getByText('Net to your wallet')).toBeInTheDocument();
    expect(within(card).queryByTestId('payout')).toBeNull();
    expect(within(card).queryByTestId('settlement-check')).toBeNull();
    expect(within(card).getByTestId('settlement-no-payouts')).toHaveTextContent('No per-wallet payout breakdown is recorded for this payment.');
  });

  it('a simulated settlement is tagged and never links to an explorer', async () => {
    const open = seed().sessions.find((s) => !s.livemode && s.status === 'open')!;
    const { user } = renderApp(`/payments/${open.id}`, { mode: 'test' });
    await user.click(await screen.findByRole('button', { name: 'Simulate' }));
    const d = within(await screen.findByRole('dialog', { name: 'Simulate' }));
    await user.click(d.getByRole('radio', { name: /Pay the exact amount/ }));
    await user.click(d.getByRole('button', { name: 'Run simulation' }));
    const card = await screen.findByTestId('settlement');
    expect(within(card).getByText('Simulated')).toBeInTheDocument();
    expect(within(card).queryByRole('link', { name: 'View transaction' })).toBeNull();
    expect(within(card).getAllByTestId('payout').map((r) => r.querySelector('.subheading')!.textContent)).toEqual(['Founder']);
  });
});

describe('overview by wallet', () => {
  it('groups what each wallet was paid, and remembers the choice in the address', async () => {
    const { user } = renderApp('/overview');
    await screen.findAllByTestId('total');
    await user.click(screen.getByRole('radio', { name: 'By wallet' }));
    const cards = await screen.findAllByTestId('wallet-total');
    expect(cards.map((c) => c.querySelector('.subheading')!.textContent)).toEqual(['Founder', 'Partner', 'Reserve', 'Old hardware wallet', 'Treasury', 'Partner']);
    const founder = within(cards[0]!);
    expect(founder.getByText('0x7a3F…e91c')).toBeInTheDocument();
    expect(founder.getByText('Paid out on Base')).toBeInTheDocument();
    expect(cards[4]).toHaveTextContent('Paid out on Solana');
    expect(screen.queryAllByTestId('total')).toHaveLength(0);
    await user.click(screen.getByRole('radio', { name: 'By network' }));
    await waitFor(() => expect(screen.getAllByTestId('total').length).toBe(3));
  });

  it('opens on the wallet view from the address', async () => {
    renderApp('/overview?group=wallet');
    expect((await screen.findAllByTestId('wallet-total')).length).toBe(6);
  });

  it('shows the network view only when the API sends no wallet totals', async () => {
    renderApp('/overview?group=wallet', {
      handlers: [http.get('/v1/dashboard/overview', () => HttpResponse.json({ totals: [{ chain: 'base', asset: 'USDC', received: '10.000000', fees: '0.100000', net: '9.900000' }], recent_payments: [], links_attention: [] }))],
    });
    expect(await screen.findAllByTestId('total')).toHaveLength(1);
    expect(screen.queryByRole('radio', { name: 'By wallet' })).toBeNull();
  });
});
