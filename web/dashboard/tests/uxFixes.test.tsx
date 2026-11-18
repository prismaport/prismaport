import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import { payoutChanges } from '../src/lib/payout';
import { TEST_EVENTS } from '../src/lib/copy';
import { createMockApi } from '../src/mocks/handlers';
import { renderApp, server, useMockServer } from './helpers';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

useMockServer();
afterEach(() => server.events.removeAllListeners());

describe('payout change confirmation (ADR 0008 G-2)', () => {
  const wallets = [
    { id: 'w1', label: 'Founder', family: 'evm', address: '0x1111111111111111111111111111111111111111', verified: true, archived_at: null, created_at: '' },
    { id: 'w2', label: 'Partner', family: 'evm', address: '0x2222222222222222222222222222222222222222', verified: true, archived_at: null, created_at: '' },
  ] as Parameters<typeof payoutChanges>[2];

  it('lists only the networks that change, with shortened addresses', () => {
    const original = { base: [{ wallet_id: 'w1', share: '100.00' }], solana: [{ wallet_id: 's1', share: '100.00', label: 'Sol', address: 'So1anaAddress1111111111111111111111111111' }] };
    const next = { base: [{ wallet_id: 'w2', share: '100.00' }], solana: original.solana };
    const changes = payoutChanges(original, next, wallets);
    expect(changes).toEqual([{ chain: 'base', before: ['100% Founder (0x111111…1111)'], after: ['100% Partner (0x222222…2222)'] }]);
    expect(payoutChanges(original, original, wallets)).toEqual([]);
  });

  it('asks with the old and the new destination before saving, and saves only after confirming', async () => {
    const writes: unknown[] = [];
    server.events.on('request:start', async ({ request }) => {
      if (request.method === 'PATCH' && /\/products\//.test(request.url)) writes.push(await request.clone().json());
    });
    const { user } = renderApp('/products');
    // Founder plan has live links, so the links question follows the payout one.
    await user.click(await screen.findByRole('button', { name: 'Edit Founder plan' }));
    const d = within(await screen.findByRole('dialog', { name: 'Edit product' }));
    await user.click(d.getByRole('button', { name: 'How you get paid' }));
    await waitFor(() => expect(d.getByLabelText('Base wallet 1')).toBeEnabled());
    await user.click(within(d.getByRole('group', { name: 'Stablecoins on Base' })).getByRole('button', { name: 'Off' }));
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    const ask = within(await screen.findByRole('dialog', { name: 'Change where this product pays?' }));
    expect(ask.getByTestId('payout-confirm')).toHaveTextContent('Now:');
    expect(ask.getByTestId('payout-confirm')).toHaveTextContent('New: Takes no payments');
    expect(writes).toEqual([]);
    await user.click(ask.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog', { name: 'Change where this product pays?' })).toBeNull();
    expect(writes).toEqual([]);
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    await user.click(within(await screen.findByRole('dialog', { name: 'Change where this product pays?' })).getByRole('button', { name: 'Change payout' }));
    // Past the payout question, the links one (this product has live links) still comes.
    expect(await screen.findByRole('dialog', { name: /What happens to the/ })).toBeInTheDocument();
  });

  it('does not ask when the payout is untouched', async () => {
    const { user } = renderApp('/products');
    await user.click(await screen.findByRole('button', { name: 'Edit Founder plan' }));
    const d = within(await screen.findByRole('dialog', { name: 'Edit product' }));
    await user.click(d.getByRole('button', { name: 'How you get paid' }));
    await waitFor(() => expect(d.getByLabelText('Base wallet 1')).toBeEnabled());
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    expect(screen.queryByRole('dialog', { name: 'Change where this product pays?' })).toBeNull();
  });
});

describe('test webhook events wording (ADR 0008 G-4)', () => {
  it('defines the name once', () => {
    expect(TEST_EVENTS).toBe('Test webhook events');
  });
  it('no merchant-facing file says sandbox inbox any more', () => {
    for (const f of ['GetStarted', 'Sandbox', 'Developers']) {
      const src = readFileSync(join(process.cwd(), 'src/pages', `${f}.tsx`), 'utf8');
      expect(src).not.toMatch(/[Ss]andbox inbox/);
    }
  });
});

describe('no silent empty sections (ADR 0008 G-6)', () => {
  it('says so when a product pays no network', async () => {
    const db = createMockApi().db();
    const link = db.links[0]!;
    const product = db.products.find((p) => p.id === link.product_id)!;
    renderApp(`/links/${link.id}`, {
      handlers: [http.get(`/v1/dashboard/products/${product.id}`, () => HttpResponse.json({ ...product, payout: {} }))],
    });
    expect(await screen.findByTestId('product-payout-lines')).toHaveTextContent('No network takes payments yet.');
  });
});

describe('verify email loading (ADR 0008 G-7)', () => {
  it('shows Loading while the account is fetched, not a blank page', async () => {
    renderApp('/verify-email', { signedIn: false, handlers: [http.get('/v1/dashboard/me', () => new Promise(() => {}))] });
    expect(await screen.findByText('Loading your account.')).toBeInTheDocument();
  });
});

describe('touch targets (ADR 0008 G-5)', () => {
  it('raises buttons and inputs to 44px on coarse pointers', () => {
    const css = readFileSync(join(process.cwd(), 'src/styles/app.css'), 'utf8');
    const block = css.slice(css.indexOf('@media (pointer: coarse)'));
    expect(block).toMatch(/\.btn-small[\s\S]*min-height: 44px/);
    for (const sel of ['.nav-link', '.btn', '.input', '.select']) expect(block).toContain(sel);
  });
});
