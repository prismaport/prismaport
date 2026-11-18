import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { createMockApi } from '../src/mocks/handlers';
import { renderApp, useMockServer } from './helpers';

useMockServer();

const seed = () => createMockApi().db();

describe('product detail', () => {
  it('shows the price, description, stats, its links, where the money goes and its payments', async () => {
    const db = seed();
    const founder = db.products.find((p) => p.name === 'Founder plan')!;
    renderApp(`/products/${founder.id}`);
    expect(await screen.findByRole('heading', { level: 1, name: 'Founder plan' })).toBeInTheDocument();
    expect(screen.getByTestId('product-description')).toHaveTextContent('Lifetime access to every Acme Studio template, with updates.');
    expect(screen.getAllByTestId('product-price')[0]).toHaveTextContent('25.00 USD');
    expect(screen.getByTestId('stat-opened')).toHaveTextContent(String(db.sessions.filter((s) => s.product_id === founder.id).length));
    const links = await screen.findAllByTestId('link-row');
    expect(links.map((l) => l.getAttribute('data-link-id')).sort()).toEqual(db.links.filter((l) => l.product_id === founder.id).map((l) => l.id).sort());
    // On its own product's page a link without a label says so instead of repeating the product.
    const plain = links.find((l) => l.textContent!.includes('No label'))!;
    expect(within(plain).getByRole('button', { name: 'Copy link for Founder plan' })).toBeInTheDocument();
    expect(within(plain).getByRole('button', { name: 'Show QR code for Founder plan' })).toBeInTheDocument();
    expect(screen.getByTestId('product-payout-summary')).toHaveTextContent('Base: 70% Founder · 30% Partner');
    const sessions = await screen.findAllByTestId('session-row');
    expect(sessions.length).toBeGreaterThan(0);
    expect(screen.getByRole('link', { name: 'Open in Payments' })).toHaveAttribute('href', `/payments?product_id=${founder.id}`);
  });

  it('marks payments from the API on a product sold only with the API', async () => {
    const orders = seed().products.find((p) => p.name === 'Custom order')!;
    renderApp(`/products/${orders.id}`);
    expect(await screen.findByText('No payment links yet. Create one to start selling this product.')).toBeInTheDocument();
    const rows = await screen.findAllByTestId('session-row');
    expect(rows.every((r) => r.textContent!.includes('API'))).toBe(true);
  });
});

describe('payments filters', () => {
  it('filters by product, then by one of its links, and names each row by product and label', async () => {
    const db = seed();
    const founder = db.products.find((p) => p.name === 'Founder plan')!;
    const plain = db.links.find((l) => l.product_id === founder.id && l.name === null)!;
    const { user } = renderApp('/payments');
    await screen.findAllByTestId('session-row');
    await user.selectOptions(screen.getByLabelText('Product'), founder.id);
    await waitFor(() => expect(screen.getAllByTestId('session-row').every((r) => r.textContent!.includes('Founder plan'))).toBe(true));
    const linkSelect = screen.getByLabelText('Payment link') as HTMLSelectElement;
    await waitFor(() => expect([...linkSelect.options].map((o) => o.textContent)).toEqual(['Every link', 'Founder plan', 'Instagram bio']));
    await user.selectOptions(linkSelect, plain.id);
    const opened = db.sessions.filter((s) => s.link_id === plain.id).length;
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(Math.min(opened, 20)));
    // Another product clears a link that would match nothing.
    await user.selectOptions(screen.getByLabelText('Product'), db.products.find((p) => p.name === 'Tip jar')!.id);
    await waitFor(() => expect((screen.getByLabelText('Payment link') as HTMLSelectElement).value).toBe(''));
  });

  it('opens filtered on a link from its page', async () => {
    const db = seed();
    const insta = db.links.find((l) => l.name === 'Instagram bio')!;
    renderApp(`/payments?link_id=${insta.id}`);
    expect(await screen.findByText('No payments match these filters.')).toBeInTheDocument();
    expect(screen.getByLabelText('Payment link')).toHaveValue(insta.id);
  });
});

describe('navigation', () => {
  it('lists Products between Overview and Payment links, and Customers after Payments', async () => {
    renderApp('/overview');
    const nav = (await screen.findAllByRole('navigation', { name: 'Main' }))[0]!;
    expect(within(nav).getAllByRole('link').map((a) => a.textContent)).toEqual(['Overview', 'Products', 'Payment links', 'Payments', 'Customers', 'Developers', 'Settings']);
  });
});
