import { screen, waitFor, within } from '@testing-library/react';
import { http } from 'msw';
import { describe, expect, it } from 'vitest';
import { CLIENT_REFERENCE, CLIENT_REFERENCE_HINT, referenceNote } from '../src/lib/reference';
import { createMockApi } from '../src/mocks/handlers';
import { renderApp, useMockServer } from './helpers';

useMockServer();

const seeded = () => createMockApi().db().sessions;

describe('client reference helpers', () => {
  it('checks a reference like the server', () => {
    expect(CLIENT_REFERENCE.test('order-42')).toBe(true);
    expect(CLIENT_REFERENCE.test('inv:2026-0042')).toBe(true);
    expect(CLIENT_REFERENCE.test('z'.repeat(128))).toBe(true);
    expect(CLIENT_REFERENCE.test('z'.repeat(129))).toBe(false);
    expect(CLIENT_REFERENCE.test('order 42')).toBe(false);
    expect(CLIENT_REFERENCE.test('')).toBe(false);
  });

  it('warns that a payer reference is only for matching', () => {
    expect(referenceNote('payer')).toMatch(/Anyone can change a payment link address/);
    expect(referenceNote('payer')).toMatch(/never to decide what the payer gets/);
    expect(referenceNote('merchant')).toBe('Set by your server when it created the payment, like metadata.');
  });
});

describe('payments with a client reference', () => {
  it('shows the reference in the list and says which came from the payer', async () => {
    renderApp('/payments');
    const rows = await screen.findAllByTestId('session-row');
    const refs = rows.map((r) => within(r).queryByTestId('session-ref')?.textContent ?? null).filter(Boolean) as string[];
    expect(refs.some((t) => /cart-\d+ \(from the payer\)$/.test(t))).toBe(true);
    expect(refs.some((t) => /inv:2026-\d{4}$/.test(t))).toBe(true);
  });

  it('filters by an exact client reference', async () => {
    const asked: string[] = [];
    const target = seeded().find((s) => s.livemode && s.client_reference_source === 'payer')!;
    const { user } = renderApp('/payments', {
      handlers: [
        http.get('/v1/dashboard/sessions', ({ request }) => {
          asked.push(new URL(request.url).searchParams.get('client_reference_id') ?? '');
        }),
      ],
    });
    await screen.findAllByTestId('session-row');
    const input = screen.getByRole('searchbox', { name: 'Client reference' });
    await user.type(input, 'not valid!');
    expect(await screen.findByText(CLIENT_REFERENCE_HINT)).toBeInTheDocument();
    expect(input).toHaveAttribute('aria-invalid', 'true');
    await user.clear(input);
    await user.type(input, target.client_reference_id!);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));
    expect(within(screen.getByTestId('session-row')).getByTestId('session-ref')).toHaveTextContent(`${target.client_reference_id} (from the payer)`);
    expect(asked).toContain(target.client_reference_id);
    expect(asked).not.toContain('not valid!');
    await user.clear(input);
    await user.type(input, 'cart-999999');
    expect(await screen.findByText('No payments match these filters.')).toBeInTheDocument();
  });

  it('marks a reference from the payer as untrusted on the payment', async () => {
    const target = seeded().find((s) => s.livemode && s.client_reference_source === 'payer')!;
    renderApp(`/payments/${target.id}`);
    const card = await screen.findByTestId('client-reference');
    expect(within(card).getByText('The payer, from the payment link address')).toBeInTheDocument();
    expect(within(card).getByText(referenceNote('payer'))).toBeInTheDocument();
    expect(card).toHaveTextContent(target.client_reference_id!);
  });

  it('marks a reference from the API as the merchant server', async () => {
    const target = seeded().find((s) => s.livemode && s.client_reference_source === 'merchant')!;
    renderApp(`/payments/${target.id}`);
    const card = await screen.findByTestId('client-reference');
    expect(within(card).getByText('Your server, through the API')).toBeInTheDocument();
    expect(within(card).getByText(referenceNote('merchant'))).toBeInTheDocument();
  });

  it('has no reference card when there is none', async () => {
    const target = seeded().find((s) => s.livemode && s.client_reference_id === null)!;
    renderApp(`/payments/${target.id}`);
    expect(await screen.findByRole('heading', { level: 1 })).toBeInTheDocument();
    expect(screen.queryByTestId('client-reference')).not.toBeInTheDocument();
  });
});
