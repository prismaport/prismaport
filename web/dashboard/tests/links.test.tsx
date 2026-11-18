import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { renderApp, useMockServer } from './helpers';

useMockServer();

const row = (id: string) => document.querySelector(`[data-link-id="${id}"]`) as HTMLElement;

describe('Payment links list', () => {
  it('is home after login when there are links, live links first', async () => {
    renderApp('/');
    const rows = await screen.findAllByTestId('link-row');
    expect(screen.getByRole('heading', { level: 1, name: 'Payment links' })).toBeInTheDocument();
    const pills = rows.map((r) => within(r).getByText(/^(Live|Inactive|Expired|Limit reached|Product inactive)$/).textContent);
    const firstNotLive = pills.findIndex((p) => p !== 'Live');
    expect(firstNotLive).toBeGreaterThan(0);
    expect(pills.slice(firstNotLive).every((p) => p !== 'Live')).toBe(true);
    expect(pills).toEqual(expect.arrayContaining(['Inactive', 'Expired', 'Limit reached', 'Product inactive']));
  });

  it('names each link by its label or its product, with the price the payer sees, stats and actions', async () => {
    const { mock } = renderApp('/links');
    await screen.findAllByTestId('link-row');
    const db = mock.db();
    const founder = db.products.find((p) => p.name === 'Founder plan')!;
    const plain = db.links.find((l) => l.product_id === founder.id && l.name === null)!;
    const insta = db.links.find((l) => l.name === 'Instagram bio')!;
    const invoice = db.links.find((l) => l.name === 'Invoice ACME #12')!;
    const r = within(row(plain.id));
    expect(r.getByRole('link', { name: 'Founder plan' })).toHaveAttribute('href', `/links/${plain.id}`);
    expect(r.getByTestId('link-price')).toHaveTextContent('25.00 USD');
    expect(r.getByLabelText(/\d+ opened, \d+ paid/)).toBeInTheDocument();
    expect(r.getAllByText(/^\d[\d,]*\.\d{2,6} USD[CT]$/).length).toBeGreaterThan(0);
    for (const name of ['Copy link for Founder plan', 'Show QR code for Founder plan', 'Edit Founder plan', 'Deactivate Founder plan']) {
      expect(r.getByRole('button', { name })).toBeInTheDocument();
    }
    // A labelled link names its product too.
    expect(within(row(insta.id)).getByTestId('link-product')).toHaveAttribute('href', `/products/${founder.id}`);
    expect(within(row(invoice.id)).getByTestId('link-price')).toHaveTextContent('120.00 USD');
    expect(within(row(invoice.id)).getByText('Set amount')).toBeInTheDocument();
    expect(within(row(db.links.find((l) => l.product_id === invoice.product_id && l.name === null)!.id)).getByTestId('link-price')).toHaveTextContent('5.00 to 500.00 USD');
    expect(within((await screen.findByRole('link', { name: 'Consulting hour' })).closest('li') as HTMLElement).getByRole('button', { name: 'Activate Consulting hour' })).toBeInTheDocument();
  });

  it('filters by status, product and a search over the label and the product name', async () => {
    const { user, mock } = renderApp('/links');
    await screen.findAllByTestId('link-row');
    const status = within(screen.getByRole('group', { name: 'Status' }));
    await user.click(status.getByRole('radio', { name: 'Live' }));
    await waitFor(() => expect(screen.queryByRole('link', { name: 'Consulting hour' })).not.toBeInTheDocument());
    expect(screen.getAllByTestId('link-row').every((r) => within(r).queryByText('Live'))).toBe(true);
    await user.click(status.getByRole('radio', { name: 'All' }));
    const founder = mock.db().products.find((p) => p.name === 'Founder plan')!;
    await user.selectOptions(screen.getByLabelText('Product'), founder.id);
    await waitFor(() => expect(screen.getAllByTestId('link-row')).toHaveLength(2));
    await user.selectOptions(screen.getByLabelText('Product'), '');
    await user.type(screen.getByLabelText('Search links'), 'instagram');
    await waitFor(() => expect(screen.getAllByTestId('link-row')).toHaveLength(1));
    expect(screen.getByRole('link', { name: 'Instagram bio' })).toBeInTheDocument();
    await user.clear(screen.getByLabelText('Search links'));
    await user.type(screen.getByLabelText('Search links'), 'workshop');
    await waitFor(() => expect(screen.getAllByTestId('link-row')).toHaveLength(1));
    await user.clear(screen.getByLabelText('Search links'));
    await user.type(screen.getByLabelText('Search links'), 'nothing like this');
    expect(await screen.findByText('No links match these filters.')).toBeInTheDocument();
  });

  it('explains links when there are none, and opens the link drawer', async () => {
    const { user } = renderApp('/links', { handlers: [http.get('/v1/dashboard/links', () => HttpResponse.json({ data: [], next_cursor: null }))] });
    expect(await screen.findByRole('heading', { name: 'Create your first payment link' })).toBeInTheDocument();
    expect(screen.getByText(/A link sells one of your products/)).toBeInTheDocument();
    await user.click(within(screen.getByRole('region', { name: 'Create your first payment link' })).getByRole('button', { name: 'New link' }));
    expect(await screen.findByRole('dialog', { name: 'Create payment link' })).toBeInTheDocument();
  });

  it('opens a QR code of the link URL with downloads', async () => {
    const { user } = renderApp('/links');
    await user.click(await screen.findByRole('button', { name: 'Show QR code for Instagram bio' }));
    const dialog = await screen.findByRole('dialog', { name: 'QR code for Instagram bio' });
    const img = within(dialog).getByRole('img', { name: /QR code of the payment link/ });
    expect(img.getAttribute('data-qr')).toMatch(/^https:\/\/prismaport\.xyz\/l\/plink_/);
    expect(within(dialog).getByText('Founder plan · 25.00 USD')).toBeInTheDocument();
    const open = within(dialog).getByRole('link', { name: 'Open the payment page' });
    expect(open).toHaveAttribute('href', img.getAttribute('data-qr'));
    expect(open).toHaveAttribute('target', '_blank');
    expect(open).toHaveAttribute('rel', 'noopener noreferrer');
    expect(within(dialog).getByRole('button', { name: 'Copy link' })).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: /Download SVG/ })).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: /Download PNG/ })).toBeInTheDocument();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('asks before deactivating and then shows the link as inactive', async () => {
    const { user } = renderApp('/links');
    await user.click(await screen.findByRole('button', { name: 'Deactivate Tip jar' }));
    const dialog = await screen.findByRole('dialog', { name: 'Deactivate Tip jar?' });
    expect(within(dialog).getByText(/The product and its other links are not changed/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Deactivate link' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    const r = (await screen.findByRole('link', { name: 'Tip jar' })).closest('li')!;
    await waitFor(() => expect(within(r as HTMLElement).getByText('Inactive')).toBeInTheDocument());
    await user.click(within(r as HTMLElement).getByRole('button', { name: 'Activate Tip jar' }));
    await waitFor(() => expect(within(screen.getByRole('link', { name: 'Tip jar' }).closest('li') as HTMLElement).getByText('Live')).toBeInTheDocument());
  });

  it('copies the full link URL', async () => {
    const { user } = renderApp('/links');
    await user.click(await screen.findByRole('button', { name: 'Copy link for Instagram bio' }));
    const text = await navigator.clipboard.readText();
    expect(text).toMatch(/^https:\/\/prismaport\.xyz\/l\/plink_01JB[0-9A-Z]{22}$/);
    expect(await screen.findByText('Link copied')).toBeInTheDocument();
  });

  it('explains a link whose product is inactive', async () => {
    const { mock, user } = renderApp('/links');
    await screen.findAllByTestId('link-row');
    const zine = mock.db().products.find((p) => p.name === 'Spring zine')!;
    const link = mock.db().links.find((l) => l.product_id === zine.id)!;
    expect(within(row(link.id)).getByText('Product inactive')).toBeInTheDocument();
    await user.click(within(row(link.id)).getByRole('link', { name: 'Spring zine' }));
    expect(await screen.findByText(/Its product, Spring zine, is inactive/)).toBeInTheDocument();
  });
});

describe('Products list', () => {
  it('shows each product with its price, networks, links, received and status, active first', async () => {
    renderApp('/products');
    const rows = await screen.findAllByTestId('product-row');
    expect(screen.getByRole('heading', { level: 1, name: 'Products' })).toBeInTheDocument();
    const byName = (n: string) => rows.find((r) => within(r).queryByRole('link', { name: n }))!;
    expect(within(byName('Founder plan')).getByTestId('product-price')).toHaveTextContent('25.00 USD');
    expect(within(byName('Founder plan')).getByTestId('product-links')).toHaveTextContent('2 live of 2');
    expect(within(byName('Pay Acme Studio')).getByTestId('product-price')).toHaveTextContent('Variable · 5.00 to 500.00 USD');
    expect(within(byName('Tip jar')).getByTestId('product-price')).toHaveTextContent('Variable · from 1.00 USD');
    expect(within(byName('Custom order')).getByTestId('product-links')).toHaveTextContent('No links yet');
    expect(within(rows.at(-1)!).getByText('Inactive')).toBeInTheDocument();
    expect(within(rows.at(-1)!).getByRole('button', { name: 'Create link for Spring zine' })).toBeDisabled();
    for (const name of ['Create link for Founder plan', 'Edit Founder plan', 'Deactivate Founder plan']) expect(within(byName('Founder plan')).getByRole('button', { name })).toBeInTheDocument();
  });

  it('says what a product is when there are none', async () => {
    const { user } = renderApp('/products', { handlers: [http.get('/v1/dashboard/products', () => HttpResponse.json({ data: [], next_cursor: null }))] });
    expect(await screen.findByRole('heading', { name: 'Add your first product' })).toBeInTheDocument();
    await user.click(within(screen.getByRole('region', { name: 'Add your first product' })).getByRole('button', { name: 'Add product' }));
    expect(await screen.findByRole('dialog', { name: 'Add product' })).toBeInTheDocument();
  });

  it('deactivating a product says its links stop, and its links show it', async () => {
    const { user } = renderApp('/products');
    await user.click(await screen.findByRole('button', { name: 'Deactivate Founder plan' }));
    const dialog = await screen.findByRole('dialog', { name: 'Deactivate Founder plan?' });
    expect(within(dialog).getByText(/Its 2 payment links stop opening new payments/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Deactivate product' }));
    expect(await screen.findByText('Founder plan deactivated')).toBeInTheDocument();
    const r = (await screen.findAllByTestId('product-row')).find((x) => within(x).queryByRole('link', { name: 'Founder plan' }))!;
    await waitFor(() => expect(within(r).getByText('Inactive')).toBeInTheDocument());
    await user.click(within(r).getByRole('link', { name: 'Founder plan' }));
    expect(await screen.findByText(/This product is inactive/)).toBeInTheDocument();
    const links = await screen.findAllByTestId('link-row');
    expect(links.map((l) => within(l).getByText('Product inactive'))).toHaveLength(2);
  });
});
