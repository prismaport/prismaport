import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import { createMockApi } from '../src/mocks/handlers';
import { renderApp, server, useMockServer } from './helpers';

useMockServer();
afterEach(() => server.events.removeAllListeners());

function captureLinkWrites() {
  const writes: { method: string; key: string | null; body: Record<string, unknown> }[] = [];
  server.events.on('request:start', async ({ request }) => {
    if ((request.method === 'POST' || request.method === 'PATCH') && /^\/v1\/dashboard\/links(\/[^/]+)?$/.test(new URL(request.url).pathname)) {
      writes.push({ method: request.method, key: request.headers.get('Idempotency-Key'), body: (await request.clone().json()) as Record<string, unknown> });
    }
  });
  return writes;
}

async function openNewLink(opts: Parameters<typeof renderApp>[1] = {}) {
  const app = renderApp('/links', opts);
  await app.user.click(await screen.findByRole('button', { name: 'New link' }));
  const dialog = await screen.findByRole('dialog', { name: 'Create payment link' });
  const d = within(dialog);
  await d.findByRole('radio', { name: /Founder plan/ });
  return { ...app, d, dialog };
}

const productId = (name: string) => createMockApi().db().products.find((p) => p.name === name)!.id;

describe('link drawer', () => {
  it('asks for a product first, from the active ones, searchable', async () => {
    const { user, d } = await openNewLink();
    const picker = within(d.getByTestId('product-picker'));
    const names = () => picker.getAllByRole('radio').map((r) => (r.closest('label') as HTMLElement).querySelector('.product-option-name')!.textContent);
    expect(names()).toContain('Founder plan');
    expect(names()).not.toContain('Spring zine');
    expect(screen.getByTestId('preview-name')).toHaveTextContent('Choose a product');
    await user.click(d.getByRole('button', { name: 'Create link' }));
    expect(d.getByText('Choose the product this link sells, or add a new one.')).toBeInTheDocument();
    await waitFor(() => expect(d.getByLabelText('Search products')).toHaveFocus());
    await user.type(d.getByLabelText('Search products'), 'tip');
    await waitFor(() => expect(names()).toEqual(['Tip jar']));
    await user.click(picker.getByRole('radio', { name: /Tip jar/ }));
    // The chosen one stays in view when another search hides it, apart from the matches.
    await user.clear(d.getByLabelText('Search products'));
    await user.type(d.getByLabelText('Search products'), 'founder');
    await waitFor(() => expect(names()).toEqual(['Founder plan', 'Tip jar']));
    expect(within(picker.getByRole('list', { name: 'Products' })).getAllByRole('radio')).toHaveLength(1);
    expect(within(d.getByTestId('product-picker-selected')).getByRole('radio', { name: /Tip jar/ })).toBeChecked();
    await user.clear(d.getByLabelText('Search products'));
    await waitFor(() => expect(d.queryByTestId('product-picker-selected')).toBeNull());
    await waitFor(() => expect(names()).toContain('Founder plan'));
    expect(names().filter((n) => n === 'Tip jar')).toHaveLength(1);
    expect(picker.getByRole('radio', { name: /Tip jar/ })).toBeChecked();
  });

  it('says a search found nothing even with a product chosen, and keeps the choice', async () => {
    const { user, d } = await openNewLink();
    const picker = within(d.getByTestId('product-picker'));
    await user.click(picker.getByRole('radio', { name: /Founder plan/ }));
    await user.type(d.getByLabelText('Search products'), 'abcdedf');
    expect(await picker.findByText('No active product matches this search.')).toBeInTheDocument();
    expect(picker.queryByRole('list', { name: 'Products' })).toBeNull();
    expect(within(d.getByTestId('product-picker-selected')).getByRole('radio', { name: /Founder plan/ })).toBeChecked();
    expect(screen.getByTestId('preview-name')).toHaveTextContent('Founder plan');
  });

  it('creates a link for a fixed product with only what was set, and shows its payout read-only', async () => {
    const writes = captureLinkWrites();
    const { user, d } = await openNewLink();
    await user.click(d.getByRole('radio', { name: /Founder plan/ }));
    expect(d.queryByRole('radio', { name: 'Set the amount' })).toBeNull();
    expect(await d.findByTestId('link-payout')).toHaveTextContent('Base: 70% Founder · 30% Partner · Solana: 80% Treasury · 20% Partner');
    expect(within(d.getByTestId('link-payout')).queryByRole('combobox')).toBeNull();
    expect(screen.getByTestId('preview-name')).toHaveTextContent('Founder plan');
    expect(screen.getByTestId('preview-description')).toHaveTextContent('Lifetime access to every Acme Studio template');
    expect(screen.getByTestId('preview-price')).toHaveTextContent('25.00 USD');
    await user.type(d.getByLabelText(/Label/), 'Newsletter');
    expect(d.getByLabelText(/Label/)).toHaveAccessibleDescription(/Only you see this/);
    await user.type(d.getByLabelText(/Payment limit/), '50');
    await user.click(d.getByRole('button', { name: 'Create link' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Newsletter' })).toBeInTheDocument();
    expect(writes).toHaveLength(1);
    expect(writes[0]!.key).toMatch(/^[0-9a-f-]{36}$/);
    expect(writes[0]!.body).toEqual({ product_id: productId('Founder plan'), name: 'Newsletter', max_payments: 50 });
    expect(screen.getByTestId('link-head-product')).toHaveTextContent('Founder plan');
  });

  it('on a variable product, lets the payer choose by default, or sets an amount inside the range', async () => {
    const writes = captureLinkWrites();
    const { user, d } = await openNewLink();
    await user.click(d.getByRole('radio', { name: /Pay Acme Studio/ }));
    const payer = await d.findByRole('radio', { name: /Let the payer choose/ });
    expect(payer).toBeChecked();
    expect(payer.closest('label')).toHaveTextContent('5.00 to 500.00 USD, with 10.00, 25.00 and 50.00 offered as buttons.');
    expect(screen.getByTestId('preview-hint')).toHaveTextContent('Between 5.00 and 500.00 USD.');
    expect(d.queryByLabelText('Amount for this link')).toBeNull();
    await user.click(d.getByRole('radio', { name: /Set the amount/ }));
    const amount = d.getByLabelText('Amount for this link');
    expect(amount).toHaveAccessibleDescription(/within the product's range: 5.00 to 500.00 USD/);
    await user.type(amount, '4');
    await user.click(d.getByRole('button', { name: 'Create link' }));
    expect(amount).toHaveAccessibleDescription(/Set at least 5.00 USD, the product's minimum./);
    await waitFor(() => expect(amount).toHaveFocus());
    expect(writes).toEqual([]);
    await user.clear(amount);
    await user.type(amount, '120');
    expect(screen.getByTestId('preview-price')).toHaveTextContent('120.00 USD');
    await user.type(d.getByLabelText(/Label/), 'Invoice ACME #13');
    await user.click(d.getByRole('button', { name: 'Create link' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Invoice ACME #13' })).toBeInTheDocument();
    expect(writes[0]!.body).toEqual({ product_id: productId('Pay Acme Studio'), name: 'Invoice ACME #13', amount: '120.00' });
    expect(screen.getByTestId('link-price')).toHaveTextContent('120.00 USD');
  });

  it('shows a range refusal from the server on the amount', async () => {
    const { user, d } = await openNewLink({
      handlers: [http.post('/v1/dashboard/links', () => HttpResponse.json({ error: { code: 'amount_above_maximum', message: 'The amount must be at most 500.00 USD.', param: 'amount' } }, { status: 400 }))],
    });
    await user.click(d.getByRole('radio', { name: /Pay Acme Studio/ }));
    await user.click(await d.findByRole('radio', { name: /Set the amount/ }));
    await user.type(d.getByLabelText('Amount for this link'), '100');
    await user.click(d.getByRole('button', { name: 'Create link' }));
    await waitFor(() => expect(d.getByLabelText('Amount for this link')).toHaveAccessibleDescription(/at most 500.00 USD/));
  });

  it('makes a new product on top and comes back with it chosen', async () => {
    const { user, d } = await openNewLink();
    await user.click(d.getByRole('button', { name: 'New product' }));
    const p = within(await screen.findByRole('dialog', { name: 'Add product' }));
    await user.type(p.getByLabelText('Name'), 'Sticker pack');
    await user.type(p.getByLabelText('Price'), '6');
    await waitFor(() => expect(p.getByRole('button', { name: 'Create product' })).toBeEnabled());
    expect(p.queryByRole('checkbox', { name: 'Also create a payment link' })).toBeNull();
    await user.click(p.getByRole('button', { name: 'Create product' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Add product' })).not.toBeInTheDocument());
    expect(await d.findByRole('radio', { name: /Sticker pack/ })).toBeChecked();
    expect(screen.getByTestId('preview-price')).toHaveTextContent('6.00 USD');
    await user.click(d.getByRole('button', { name: 'Create link' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Sticker pack' })).toBeInTheDocument();
  });

  it('opened from a product, sells that product and creates the link there', async () => {
    const writes = captureLinkWrites();
    const { user } = renderApp(`/products/${productId('Tip jar')}`);
    // The page head and the links section both offer it.
    await user.click((await screen.findAllByRole('button', { name: 'Create payment link' }))[0]!);
    const d = within(await screen.findByRole('dialog', { name: 'Create payment link' }));
    expect(await d.findByTestId('picked-product')).toHaveTextContent('Tip jar · Variable · from 1.00 USD');
    expect(d.queryByTestId('product-picker')).toBeNull();
    await user.click(d.getByRole('button', { name: 'Create link' }));
    expect(await screen.findByText('Tip jar link created')).toBeInTheDocument();
    expect(writes[0]!.body).toEqual({ product_id: productId('Tip jar') });
    await waitFor(() => expect(screen.getAllByTestId('link-row')).toHaveLength(2));
  });

  it('edits a link with only what changed, the product locked, and null to clear', async () => {
    const writes = captureLinkWrites();
    const { user } = renderApp('/links');
    await user.click(await screen.findByRole('button', { name: 'Edit Invoice ACME #12' }));
    const d = within(await screen.findByRole('dialog', { name: 'Edit payment link' }));
    expect(await d.findByTestId('picked-product')).toHaveTextContent('A link always sells the same product.');
    expect(await d.findByRole('radio', { name: /Set the amount/ })).toBeChecked();
    expect(d.getByLabelText('Amount for this link')).toHaveValue('120.00');
    await user.click(d.getByRole('radio', { name: /Let the payer choose/ }));
    await user.clear(d.getByLabelText(/Payment limit/));
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(writes.map((w) => [w.method, w.body])).toEqual([['PATCH', { amount: null, max_payments: null }]]);
  });

  it('edits the product from the link drawer, on top, on its payout step', async () => {
    const { user, d } = await openNewLink();
    await user.click(d.getByRole('radio', { name: /Black Friday bundle/ }));
    await user.click(await d.findByRole('button', { name: 'Edit product' }));
    const p = within(await screen.findByRole('dialog', { name: 'Edit product' }));
    expect(p.getByRole('button', { name: 'How you get paid' })).toHaveAttribute('aria-current', 'step');
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit product' })).not.toBeInTheDocument());
    expect(screen.getByRole('dialog', { name: 'Create payment link' })).toBeInTheDocument();
  });
});
