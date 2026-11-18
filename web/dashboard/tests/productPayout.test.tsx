import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import { createMockApi } from '../src/mocks/handlers';
import { renderApp, server, useMockServer } from './helpers';

useMockServer();
afterEach(() => server.events.removeAllListeners());

function captureProductWrites() {
  const bodies: Record<string, unknown>[] = [];
  server.events.on('request:start', async ({ request }) => {
    if ((request.method === 'POST' || request.method === 'PATCH') && /\/v1\/dashboard\/products(\/[^/]+)?$/.test(new URL(request.url).pathname)) {
      bodies.push((await request.clone().json()) as Record<string, unknown>);
    }
  });
  return bodies;
}

/** Opens the product drawer, fills the details and opens the split editor with "Change". */
async function openCreate(opts: Parameters<typeof renderApp>[1] = {}, details = { name: 'Payout test', price: '10' }) {
  const app = renderApp('/products', opts);
  await app.user.click(await screen.findByRole('button', { name: 'Add product' }));
  const dialog = await screen.findByRole('dialog', { name: 'Add product' });
  const d = within(dialog);
  await app.user.type(d.getByLabelText('Name'), details.name);
  await app.user.type(d.getByLabelText('Price'), details.price);
  await waitFor(() => expect(d.getByRole('button', { name: 'Change' })).toBeEnabled());
  await app.user.click(d.getByRole('button', { name: 'Change' }));
  await waitFor(() => expect(d.getByLabelText('Base wallet 1')).toBeEnabled());
  const walletId = (label: string, family = 'evm') => app.mock.db().wallets.find((w) => w.livemode && w.family === family && w.label === label && !w.archived_at)!.id;
  const base = within(d.getByTestId('payout-base'));
  const solana = within(d.getByTestId('payout-solana'));
  return { ...app, d, dialog, walletId, base, solana };
}

/** The All / Some / Off button of one network in the drawer. */
const choice = (d: ReturnType<typeof within>, network: string, name: 'All' | 'Some' | 'Off') => within(d.getByRole('group', { name: `Stablecoins on ${network}` })).getByRole('button', { name });

const legend = (el: HTMLElement) => within(el).getAllByTestId('split-legend-item').map((x) => x.textContent);

describe('product drawer: where the money goes', () => {
  it('starts with the oldest wallet of each network at 100%, with a summary under the drawer', async () => {
    const { d, base, walletId } = await openCreate();
    expect(d.getByLabelText('Base wallet 1')).toHaveValue(walletId('Founder'));
    expect(d.getByLabelText('Share of Base wallet 1')).toHaveValue('100');
    expect(d.getByLabelText('Solana wallet 1')).toHaveValue(walletId('Treasury', 'solana'));
    expect(screen.getByTestId('payout-summary')).toHaveTextContent('Base: 100% Founder · Solana: 100% Treasury');
    expect(legend(base.getByTestId('payout-bar-base'))).toEqual(['Founder100%']);
    // One wallet at 100% needs no sum line.
    expect(base.queryByTestId('payout-sum-base')).toBeNull();
    // The archived wallet is never offered.
    expect([...(d.getByLabelText('Base wallet 1') as HTMLSelectElement).options].map((o) => o.textContent!.split(' · ')[0])).toEqual(['Founder', 'Partner', 'Reserve']);
  });

  it('adds wallets, keeps a live sum and the bar in order, and creates the product with both networks', async () => {
    const bodies = captureProductWrites();
    const { user, d, base, walletId } = await openCreate({}, { name: 'Shared workshop', price: '40' });
    await user.click(base.getByRole('button', { name: 'Add wallet to this split' }));
    expect(d.getByLabelText('Base wallet 2')).toHaveValue(walletId('Partner'));
    expect(d.getByLabelText('Share of Base wallet 2')).toHaveValue('');
    // The second row offers neither the wallet of the first nor the archived one.
    expect([...(d.getByLabelText('Base wallet 2') as HTMLSelectElement).options].map((o) => o.textContent!.split(' · ')[0])).toEqual(['Partner', 'Reserve']);
    await user.clear(d.getByLabelText('Share of Base wallet 1'));
    await user.type(d.getByLabelText('Share of Base wallet 1'), '70');
    await user.type(d.getByLabelText('Share of Base wallet 2'), '20');
    expect(base.getByTestId('payout-sum-base')).toHaveTextContent('Shares must add up to 100%. Now 90%.');
    expect(within(base.getByTestId('payout-bar-base')).getByTestId('split-legend-rest')).toHaveTextContent('Not assigned10%');
    await user.clear(d.getByLabelText('Share of Base wallet 2'));
    await user.type(d.getByLabelText('Share of Base wallet 2'), '30');
    expect(base.getByTestId('payout-sum-base')).toHaveTextContent('Shares add up to 100%.');
    expect(legend(base.getByTestId('payout-bar-base'))).toEqual(['Founder70%', 'Partner30%']);
    expect(screen.getByTestId('payout-summary')).toHaveTextContent('Base: 70% Founder · 30% Partner');
    await user.click(d.getByRole('button', { name: 'Create product' }));
    await screen.findByRole('heading', { level: 1, name: 'Shared workshop' });
    expect(bodies[0]).toMatchObject({
      name: 'Shared workshop',
      payout: { base: [{ wallet_id: walletId('Founder'), share: '70.00' }, { wallet_id: walletId('Partner'), share: '30.00' }], solana: [{ wallet_id: walletId('Treasury', 'solana'), share: '100.00' }] },
    });
    expect(await screen.findByTestId('product-payout-summary')).toHaveTextContent('Base: 70% Founder · 30% Partner · Solana: 100% Treasury');
  });

  it('refuses shares that do not add up to 100% before sending, and focuses the share', async () => {
    const bodies = captureProductWrites();
    const { user, d, base } = await openCreate({}, { name: 'Uneven', price: '10' });
    await user.click(base.getByRole('button', { name: 'Add wallet to this split' }));
    await user.clear(d.getByLabelText('Share of Base wallet 1'));
    await user.type(d.getByLabelText('Share of Base wallet 1'), '60');
    await user.type(d.getByLabelText('Share of Base wallet 2'), '30');
    await user.click(d.getByRole('button', { name: 'Create product' }));
    expect(base.getByText('Change the shares so they add up to exactly 100%.')).toBeInTheDocument();
    expect(d.getByLabelText('Share of Base wallet 1')).toHaveFocus();
    expect(bodies).toEqual([]);
  });

  it('stops adding when every wallet of the network is in the split, and removes a row keeping the order', async () => {
    const { user, d, base, walletId } = await openCreate();
    const add = base.getByRole('button', { name: 'Add wallet to this split' });
    await user.click(add);
    await user.click(add);
    expect(add).toBeDisabled();
    expect(base.getByText('Every wallet that can receive on Base is already in this split. Add a wallet in Settings to share with more.')).toBeInTheDocument();
    await user.click(base.getByRole('button', { name: 'Remove Base wallet 2' }));
    expect(base.getAllByTestId('payout-row')).toHaveLength(2);
    expect(d.getByLabelText('Base wallet 1')).toHaveValue(walletId('Founder'));
    expect(d.getByLabelText('Base wallet 2')).toHaveValue(walletId('Reserve'));
    expect(add).toBeEnabled();
  });

  it('can stop taking a network, and cannot be saved when it takes none', async () => {
    const bodies = captureProductWrites();
    const { user, d } = await openCreate({}, { name: 'Base only', price: '10' });
    await user.click(choice(d, 'Solana', 'Off'));
    expect(d.getByText("This product doesn't take payments on Solana. Payers won't see it.")).toBeInTheDocument();
    expect(d.queryByTestId('payout-solana')).toBeNull();
    expect(screen.getByTestId('payout-summary')).toHaveTextContent('Base: 100% Founder');
    await user.click(choice(d, 'Base', 'Off'));
    expect(d.getByText('No network left. Payers could not pay this product: take at least one network.')).toBeInTheDocument();
    await user.click(d.getByRole('button', { name: 'Create product' }));
    expect(d.getByText('Take payments on at least one network: give it at least one wallet.')).toBeInTheDocument();
    expect(bodies).toEqual([]);
    await user.click(choice(d, 'Base', 'All'));
    await user.click(d.getByRole('button', { name: 'Create product' }));
    await screen.findByRole('heading', { level: 1, name: 'Base only' });
    expect((bodies[0]!.payout as Record<string, unknown>).solana).toEqual([]);
    expect(bodies[0]!.networks).toEqual({ base: { accept: 'all' }, solana: { accept: 'none' } });
    expect(screen.getByTestId('product-payout-solana')).toHaveTextContent('This product does not take payments on Solana.');
  });

  it('keeps the payer preview free of wallets and shares', async () => {
    const { user, base } = await openCreate();
    await user.click(base.getByRole('button', { name: 'Add wallet to this split' }));
    const preview = screen.getByTestId('link-preview');
    for (const word of ['Founder', 'Partner', 'Reserve', 'Treasury', '%']) expect(preview.textContent).not.toContain(word);
  });

  it('offers the payer only the networks the link takes', async () => {
    const { user, d } = await openCreate();
    const assets = () => [...screen.getByTestId('preview-assets').querySelectorAll('li')].map((li) => li.textContent);
    expect(assets().some((t) => /Solana/.test(t!))).toBe(true);
    await user.click(choice(d, 'Solana', 'Off'));
    expect(assets().some((t) => /Solana/.test(t!))).toBe(false);
    expect(assets().some((t) => /Base/.test(t!))).toBe(true);
  });

  it('shows a server refusal on the row it is about', async () => {
    const { user, d, walletId } = await openCreate({
      handlers: [http.post('/v1/dashboard/products', () => HttpResponse.json({ error: { code: 'wallet_blacklisted', message: 'Founder is on the USDC blacklist and cannot receive payments. Choose another wallet.' } }, { status: 400 }))],
    });
    expect(walletId('Founder')).toBeTruthy();
    await user.click(d.getByRole('button', { name: 'Create product' }));
    const select = d.getByLabelText('Base wallet 1');
    await waitFor(() => expect(select).toHaveAttribute('aria-invalid', 'true'));
    expect(within(d.getByTestId('payout-base')).getByText('Founder is on the USDC blacklist and cannot receive payments. Choose another wallet.')).toBeInTheDocument();
    expect(select).toHaveFocus();
    // Changing the payout clears it.
    await user.selectOptions(select, walletId('Reserve'));
    expect(select).not.toHaveAttribute('aria-invalid');
  });

  it('puts a server total on its network', async () => {
    const { user, d } = await openCreate({
      handlers: [http.post('/v1/dashboard/products', () => HttpResponse.json({ error: { code: 'invalid_request', message: 'Shares must add up to 100% on Solana. Now 90%.' } }, { status: 400 }))],
    });
    await user.click(d.getByRole('button', { name: 'Create product' }));
    expect(await within(d.getByTestId('payout-solana')).findByText('Shares must add up to 100% on Solana. Now 90%.')).toBeInTheDocument();
    expect(d.getByLabelText('Share of Solana wallet 1')).toHaveFocus();
  });
});

describe('several networks of one family', () => {
  // Whatever networks the mode has get a section; Ethereum stands in for a second EVM network.
  const withEthereum = http.get('/v1/dashboard/settings', () =>
    HttpResponse.json({
      accepted_assets: [
        { chain: 'base', asset: 'USDC', enabled: true },
        { chain: 'ethereum', asset: 'USDC', enabled: true },
        { chain: 'solana', asset: 'USDC', enabled: true },
      ],
      fee: { fee_bps: 50, fixed_fee: '0.10' },
    }),
  );

  it('sets each network on its own, copies one EVM network to another, and a network the server does not know lands on its section', async () => {
    const bodies = captureProductWrites();
    const { user, d, base, walletId } = await openCreate({ handlers: [withEthereum] }, { name: 'Two EVM networks', price: '10' });
    const eth = within(d.getByTestId('payout-ethereum'));
    expect(d.getByLabelText('Ethereum wallet 1')).toHaveValue(walletId('Founder'));
    expect(screen.getByTestId('payout-summary')).toHaveTextContent('Base: 100% Founder · Ethereum: 100% Founder · Solana: 100% Treasury');
    // Base 50/50 across two wallets, Ethereum 100% to one: the owner example.
    await user.click(base.getByRole('button', { name: 'Add wallet to this split' }));
    await user.clear(d.getByLabelText('Share of Base wallet 1'));
    await user.type(d.getByLabelText('Share of Base wallet 1'), '50');
    await user.type(d.getByLabelText('Share of Base wallet 2'), '50');
    expect(screen.getByTestId('payout-summary')).toHaveTextContent('Base: 50% Founder · 50% Partner · Ethereum: 100% Founder · Solana: 100% Treasury');
    // Solana is the other family: no copy between them.
    expect(within(d.getByTestId('payout-solana')).queryByRole('button', { name: /^Same as/ })).toBeNull();
    await user.click(eth.getByRole('button', { name: 'Same as Base' }));
    expect(d.getByLabelText('Ethereum wallet 2')).toHaveValue(walletId('Partner'));
    expect(legend(eth.getByTestId('payout-bar-ethereum'))).toEqual(['Founder50%', 'Partner50%']);
    // A copy is its own: changing it leaves Base as it was.
    await user.click(eth.getByRole('button', { name: 'Remove Ethereum wallet 2' }));
    await user.clear(d.getByLabelText('Share of Ethereum wallet 1'));
    await user.type(d.getByLabelText('Share of Ethereum wallet 1'), '100');
    expect(base.getAllByTestId('payout-row')).toHaveLength(2);
    await user.click(d.getByRole('button', { name: 'Create product' }));
    expect(await within(d.getByTestId('networks-ethereum')).findByText('networks has an unknown network "ethereum": use one of base, solana')).toBeInTheDocument();
    expect(Object.keys(bodies[0]!.payout as object)).toEqual(['base', 'ethereum', 'solana']);
  });

  it('offers the copy on a network that takes no payments yet', async () => {
    const { user, d } = await openCreate({ handlers: [withEthereum] });
    await user.click(choice(d, 'Ethereum', 'Off'));
    expect(d.queryByTestId('payout-ethereum')).toBeNull();
    await user.click(choice(d, 'Ethereum', 'All'));
    const eth = within(d.getByTestId('payout-ethereum'));
    await user.click(eth.getByRole('button', { name: 'Same as Base' }));
    expect(d.getByLabelText('Ethereum wallet 1')).toHaveValue((d.getByLabelText('Base wallet 1') as HTMLSelectElement).value);
    expect(d.getByLabelText('Share of Ethereum wallet 1')).toHaveValue('100');
  });
});

describe('product list, detail and links', () => {
  it('shows the networks each product pays on every row of the list', async () => {
    renderApp('/products');
    const row = (await screen.findAllByTestId('product-row')).find((r) => r.textContent!.includes('Tip jar'))!;
    expect(within(row).getByTestId('pay-with-base')).toBeInTheDocument();
    expect(within(row).getByTestId('pay-with-solana')).toBeInTheDocument();
    const bundle = (await screen.findAllByTestId('product-row')).find((r) => r.textContent!.includes('Black Friday bundle'))!;
    expect(within(bundle).getByTestId('pay-with-base')).toHaveTextContent('All');
  });

  it("shows a link's payout read-only, from its product", async () => {
    const db = createMockApi().db();
    const founder = db.products.find((p) => p.name === 'Founder plan')!;
    const link = db.links.find((l) => l.product_id === founder.id && l.name === null)!;
    renderApp(`/links/${link.id}`);
    const card = await screen.findByTestId('link-product-card');
    expect(await within(card).findByTestId('product-payout-lines')).toHaveTextContent('Base: 70% Founder · 30% Partner · Solana: 80% Treasury · 20% Partner');
    expect(within(card).queryByRole('button', { name: /Add wallet/ })).toBeNull();
  });

  it("a link's payout that fails to load says so and retries, instead of loading forever", async () => {
    const db = createMockApi().db();
    const founder = db.products.find((p) => p.name === 'Founder plan')!;
    const link = db.links.find((l) => l.product_id === founder.id && l.name === null)!;
    let failing = true;
    const { user } = renderApp(`/links/${link.id}`, {
      handlers: [http.get('/v1/dashboard/products/:id', ({ params }) => (failing && params.id === founder.id ? HttpResponse.json({ error: { code: 'server_error', message: 'boom' } }, { status: 500 }) : undefined))],
    });
    const card = await screen.findByTestId('link-product-card');
    expect(await within(card).findByRole('alert')).toBeInTheDocument();
    expect(within(card).queryByText('Loading.')).toBeNull();

    failing = false;
    await user.click(within(card).getByRole('button', { name: 'Try again' }));
    expect(await within(card).findByTestId('product-payout-lines')).toHaveTextContent('Base: 70% Founder · 30% Partner');
    expect(within(card).queryByRole('alert')).toBeNull();
  });

  it('edits a product payout in its order, with no password, and sends only the network that changed', async () => {
    const founder = createMockApi().db().products.find((p) => p.name === 'Founder plan')!;
    const bodies = captureProductWrites();
    const { user, mock } = renderApp(`/products/${founder.id}`);
    const card = await screen.findByTestId('product-payout');
    expect(within(card).getByTestId('product-payout-summary')).toHaveTextContent('Base: 70% Founder · 30% Partner · Solana: 80% Treasury · 20% Partner');
    expect(legend(within(card).getByTestId('product-payout-base'))).toEqual(['Founder70%', 'Partner30%']);
    await user.click(within(card).getByRole('button', { name: 'Edit where the money goes' }));
    const dialog = await screen.findByRole('dialog', { name: 'Edit product' });
    const d = within(dialog);
    // The payout card opens the drawer on its step; the details are one click away.
    expect(d.getByRole('button', { name: 'How you get paid' })).toHaveAttribute('aria-current', 'step');
    await waitFor(() => expect(d.getByLabelText('Base wallet 1')).toBeEnabled());
    expect(d.getByText(/Payout changes apply to new payments only/)).toBeInTheDocument();
    const id = (label: string) => mock.db().wallets.find((w) => w.livemode && w.family === 'evm' && w.label === label)!.id;
    expect(d.getByLabelText('Base wallet 1')).toHaveValue(id('Founder'));
    expect(d.getByLabelText('Share of Base wallet 1')).toHaveValue('70');
    expect(d.getByLabelText('Base wallet 2')).toHaveValue(id('Partner'));
    expect(d.queryByLabelText('Password')).toBeNull();
    await user.click(d.getByRole('button', { name: 'Remove Base wallet 2' }));
    await user.clear(d.getByLabelText('Share of Base wallet 1'));
    await user.type(d.getByLabelText('Share of Base wallet 1'), '100');
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    // Changing where the money goes asks first, then (live links) what happens to them.
    await user.click(within(await screen.findByRole('dialog', { name: 'Change where this product pays?' })).getByRole('button', { name: 'Change payout' }));
    const ask = within(await screen.findByRole('dialog', { name: 'What happens to the 2 active links?' }));
    await user.click(ask.getByRole('button', { name: 'Save and keep links' }));
    await screen.findByText('Changes saved');
    expect(bodies).toEqual([{ payout: { base: [{ wallet_id: id('Founder'), share: '100.00' }] }, links: 'keep' }]);
    await waitFor(() => expect(within(screen.getByTestId('product-payout')).getByTestId('product-payout-summary')).toHaveTextContent('Base: 100% Founder · Solana: 80% Treasury · 20% Partner'));
  });
});
