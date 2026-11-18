import { screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { createMockApi } from '../src/mocks/handlers';
import { renderApp, server, useMockServer } from './helpers';

useMockServer();
afterEach(() => server.events.removeAllListeners());

type Mock = ReturnType<typeof renderApp>['mock'];

function captureProductWrites() {
  const bodies: Record<string, unknown>[] = [];
  server.events.on('request:start', async ({ request }) => {
    if ((request.method === 'POST' || request.method === 'PATCH') && /\/v1\/dashboard\/products(\/[^/]+)?$/.test(new URL(request.url).pathname)) {
      bodies.push((await request.clone().json()) as Record<string, unknown>);
    }
  });
  return bodies;
}

const product = (mock: Mock, name: string) => mock.db().products.find((p) => p.livemode && p.name === name)!;
const setAsset = (mock: Mock, chain: string, asset: string, enabled: boolean) => {
  mock.db().accepted.live.find((a) => a.chain === chain && a.asset === asset)!.enabled = enabled;
};
const group = (d: ReturnType<typeof within>, network: string) => within(d.getByRole('group', { name: `Stablecoins on ${network}` }));

/** Opens the drawer of a product on its "How you get paid" step. */
async function openEdit(mock: Mock, user: ReturnType<typeof renderApp>['user'], name: string) {
  await user.click(await screen.findByRole('button', { name: `Edit ${name}` }));
  const d = within(await screen.findByRole('dialog', { name: 'Edit product' }));
  await user.click(d.getByRole('button', { name: 'How you get paid' }));
  await d.findByTestId('networks-solana');
  return d;
}

describe('Settings › Networks: stablecoins and the products that use them', () => {
  it('shows how many products take each stablecoin, and how many keep an off one ticked', async () => {
    const { mock, user } = renderApp('/settings/networks');
    const some = ['Founder plan', 'Tip jar'].map((n) => product(mock, n));
    for (const p of some) p.networks = { ...p.networks, solana: { accept: 'some', assets: ['USDT'] } };
    const active = mock.db().products.filter((p) => p.livemode && p.active);
    const takes = (chain: string, asset: string) => active.filter((p) => (p.payout[chain] ?? []).length > 0 && (p.networks?.[chain]?.accept !== 'some' || p.networks[chain]!.assets!.includes(asset))).length;
    const solana = await screen.findByTestId('network-solana');
    const plural = (n: number) => (n === 1 ? '1 product' : `${n} products`);
    expect(await within(solana).findByTestId('used-solana-USDT')).toHaveTextContent(`Used by ${plural(takes('solana', 'USDT'))}`);
    expect(within(solana).getByTestId('used-solana-USDC')).toHaveTextContent(`Used by ${plural(takes('solana', 'USDC'))}`);
    expect(takes('solana', 'USDC')).toBeLessThan(takes('solana', 'USDT'));
    await user.click(within(solana).getByRole('checkbox', { name: 'USDT on Solana' }));
    const ticked = active.filter((p) => p.networks?.solana?.accept === 'some').length;
    expect(within(solana).getByTestId('used-solana-USDT')).toHaveTextContent(`Off · ticked on ${plural(ticked)}`);
    // Used by never reads as something else when a row has no product on it.
    expect(within(screen.getByTestId('network-base')).getByTestId('used-base-USDC')).toHaveTextContent(/^Used by \d+ products?$/);
  });

  it('warns which products would be left taking nothing before USDT is turned off, and still lets it be saved', async () => {
    const { mock, user } = renderApp('/settings/networks');
    for (const n of ['Founder plan', 'Tip jar']) product(mock, n).networks = { solana: { accept: 'some', assets: ['USDT'] } };
    const solana = await screen.findByTestId('network-solana');
    await within(solana).findByTestId('used-solana-USDT');
    expect(within(solana).queryByTestId('impact-solana')).toBeNull();
    await user.click(within(solana).getByRole('checkbox', { name: 'USDT on Solana' }));
    const box = within(await within(solana).findByTestId('impact-solana'));
    expect(box.getByText('If you save, USDT stops on Solana for every product')).toBeInTheDocument();
    expect(box.getByText('2 products take only USDT on Solana, so they stop taking Solana:')).toBeInTheDocument();
    expect(box.getByRole('link', { name: 'Founder plan' })).toHaveAttribute('href', `/products/${product(mock, 'Founder plan').id}`);
    expect(box.getByRole('link', { name: 'Tip jar' })).toBeInTheDocument();
    expect(box.getByText(/They keep USDT ticked: turn it back on here and they take it again\. Payments already opened are not affected\./)).toBeInTheDocument();
    // A warning, not a block.
    expect(within(solana).getByRole('button', { name: 'Save Solana stablecoins' })).toBeEnabled();
    await user.click(within(solana).getByRole('checkbox', { name: 'USDT on Solana' }));
    expect(within(solana).queryByTestId('impact-solana')).toBeNull();
  });

  it('says nothing when every product keeps another stablecoin on that network', async () => {
    const { mock, user } = renderApp('/settings/networks');
    const solana = await screen.findByTestId('network-solana');
    await within(solana).findByTestId('used-solana-USDT');
    expect(product(mock, 'Founder plan').networks?.solana ?? { accept: 'all' }).toEqual({ accept: 'all' });
    await user.click(within(solana).getByRole('checkbox', { name: 'USDT on Solana' }));
    expect(within(solana).queryByTestId('impact-solana')).toBeNull();
  });

  it('turns every stablecoin of a network on or off with its switch, and keeps one on overall', async () => {
    const { user, mock } = renderApp('/settings/networks');
    const solana = within(await screen.findByTestId('network-solana'));
    const base = within(await screen.findByTestId('network-base'));
    const sw = solana.getByRole('switch', { name: 'Accept payments on Solana' });
    expect(sw).toBeChecked();
    await user.click(sw);
    expect(sw).not.toBeChecked();
    expect(solana.getByRole('checkbox', { name: 'USDC on Solana' })).not.toBeChecked();
    expect(solana.getByRole('checkbox', { name: 'USDT on Solana' })).not.toBeChecked();
    expect(solana.getByTestId('network-off-solana')).toHaveTextContent('Solana is off. No product takes payments on Solana until you turn it back on. Each product keeps its choice for Solana.');
    expect(solana.getByRole('button', { name: 'Save Solana stablecoins' })).toBeEnabled();
    await user.click(solana.getByRole('switch', { name: 'Accept payments on Solana' }));
    expect(solana.getByRole('checkbox', { name: 'USDT on Solana' })).toBeChecked();
    // Ticking one stablecoin turns the network on.
    await user.click(solana.getByRole('switch', { name: 'Accept payments on Solana' }));
    await user.click(solana.getByRole('checkbox', { name: 'USDC on Solana' }));
    expect(solana.getByRole('switch', { name: 'Accept payments on Solana' })).toBeChecked();
    // The last stablecoin on the account cannot be turned off.
    await user.click(solana.getByRole('switch', { name: 'Accept payments on Solana' }));
    await user.click(solana.getByRole('button', { name: 'Save Solana stablecoins' }));
    await waitFor(() => expect(mock.db().accepted.live.filter((x) => x.chain === 'solana').every((x) => !x.enabled)).toBe(true));
    await user.click(base.getByRole('switch', { name: 'Accept payments on Base' }));
    expect(base.getByText('Keep at least one stablecoin. Payers need one to pay.')).toBeInTheDocument();
    expect(base.getByRole('button', { name: 'Save Base stablecoins' })).toBeDisabled();
  });

  it('says what a stablecoin added later does', async () => {
    renderApp('/settings/networks');
    expect(await screen.findByText('A stablecoin Prismaport adds later starts off on your account. Turn it on here to offer it; products set to All on that network take it at once.')).toBeInTheDocument();
  });
});

describe('product drawer: All, Some and Off per network', () => {
  it('shows All with the stablecoins on in Settings, and Off with its reason and no payout editor', async () => {
    const { mock, user } = renderApp('/products');
    setAsset(mock, 'solana', 'USDT', false);
    const d = await openEdit(mock, user, 'Founder plan');
    const solana = group(d, 'Solana');
    expect(solana.getByRole('button', { name: 'All' })).toHaveAttribute('aria-pressed', 'true');
    expect(solana.getByRole('button', { name: 'Some' })).toHaveAttribute('aria-pressed', 'false');
    const section = within(d.getByTestId('networks-solana'));
    expect(section.getByText('Every stablecoin on in Settings for Solana, including ones you turn on later.')).toBeInTheDocument();
    expect(section.getByRole('list', { name: 'Stablecoins taken on Solana' })).toHaveTextContent('USDC');
    expect(section.getByRole('list', { name: 'Stablecoins taken on Solana' })).not.toHaveTextContent('USDT');
    expect(d.getByTestId('payout-solana')).toBeInTheDocument();
    await user.click(solana.getByRole('button', { name: 'Off' }));
    expect(solana.getByRole('button', { name: 'Off' })).toHaveAttribute('aria-pressed', 'true');
    expect(section.getByText("This product doesn't take payments on Solana. Payers won't see it.")).toBeInTheDocument();
    expect(d.queryByTestId('payout-solana')).toBeNull();
    expect(d.queryByRole('button', { name: "Don't take payments on this network" })).toBeNull();
  });

  it('saves Some as the ticked stablecoins only, without touching the payout', async () => {
    const bodies = captureProductWrites();
    const { mock, user } = renderApp('/products');
    const d = await openEdit(mock, user, 'Founder plan');
    const solana = group(d, 'Solana');
    await user.click(solana.getByRole('button', { name: 'Some' }));
    const boxes = within(d.getByTestId('networks-solana'));
    expect(boxes.getByRole('checkbox', { name: 'USDC' })).toBeChecked();
    expect(boxes.getByRole('checkbox', { name: 'USDT' })).toBeChecked();
    expect(boxes.getByText('Only the ones ticked. A stablecoin you turn on later in Settings does not join this product.')).toBeInTheDocument();
    await user.click(boxes.getByRole('checkbox', { name: 'USDC' }));
    expect(screen.getByTestId('payers-see')).toHaveTextContent('USDC on Base');
    expect(screen.getByTestId('payers-see')).toHaveTextContent('USDT on Solana');
    expect(screen.getByTestId('payers-see')).not.toHaveTextContent('USDC on Solana');
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ networks: { solana: { accept: 'some', assets: ['USDT'] } } });
    expect(product(mock, 'Founder plan').networks?.solana).toEqual({ accept: 'some', assets: ['USDT'] });
  });

  it('blocks the save while Some has nothing ticked, and says how to fix it', async () => {
    const bodies = captureProductWrites();
    const { mock, user } = renderApp('/products');
    const d = await openEdit(mock, user, 'Founder plan');
    await user.click(group(d, 'Solana').getByRole('button', { name: 'Some' }));
    const boxes = within(d.getByTestId('networks-solana'));
    await user.click(boxes.getByRole('checkbox', { name: 'USDC' }));
    await user.click(boxes.getByRole('checkbox', { name: 'USDT' }));
    expect(boxes.getByText('Tick at least one stablecoin, or set Solana to Off.')).toBeInTheDocument();
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    expect(bodies).toEqual([]);
    await user.click(group(d, 'Solana').getByRole('button', { name: 'Off' }));
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    await screen.findByRole('dialog', { name: 'Change where this product pays?' });
  });

  it('shows a stored stablecoin that is off in Settings ticked and disabled, with a way out when nothing is left', async () => {
    const { mock, user } = renderApp('/products');
    product(mock, 'Founder plan').networks = { base: { accept: 'all' }, solana: { accept: 'some', assets: ['USDT'] } };
    setAsset(mock, 'solana', 'USDT', false);
    const d = await openEdit(mock, user, 'Founder plan');
    const boxes = within(d.getByTestId('networks-solana'));
    const usdt = boxes.getByRole('checkbox', { name: /USDT/ });
    expect(usdt).toBeChecked();
    expect(usdt).toBeDisabled();
    expect(boxes.getByText('Off in Settings')).toBeInTheDocument();
    expect(boxes.getByText('This product takes nothing on Solana right now')).toBeInTheDocument();
    expect(boxes.getByRole('link', { name: 'Turn USDT on in Settings' })).toHaveAttribute('href', '/settings/networks');
    expect(boxes.getByRole('checkbox', { name: 'USDC' })).not.toBeChecked();
    // Base still gives payers something, so the product can be saved.
    expect(screen.getByTestId('payers-see')).toHaveTextContent('USDC on Base');
  });

  it('blocks the save when no network is left for payers', async () => {
    const bodies = captureProductWrites();
    const { mock, user } = renderApp('/products');
    product(mock, 'Tip jar').networks = { solana: { accept: 'some', assets: ['USDT'] } };
    product(mock, 'Tip jar').payout.base = [];
    setAsset(mock, 'solana', 'USDT', false);
    const d = await openEdit(mock, user, 'Tip jar');
    expect(screen.getByTestId('payers-see')).toHaveTextContent('No network left. Payers could not pay this product: take at least one network.');
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    expect(bodies).toEqual([]);
    await user.click(group(d, 'Solana').getByRole('button', { name: 'All' }));
    expect(screen.getByTestId('payers-see')).toHaveTextContent('USDC on Solana');
  });

  it('creates a product with the choice of each network, Off as none', async () => {
    const bodies = captureProductWrites();
    const { user } = renderApp('/products');
    await user.click(await screen.findByRole('button', { name: 'Add product' }));
    const d = within(await screen.findByRole('dialog', { name: 'Add product' }));
    await user.type(d.getByLabelText('Name'), 'Mixed choices');
    await user.type(d.getByLabelText('Price'), '10');
    await waitFor(() => expect(group(d, 'Solana').getByRole('button', { name: 'Some' })).toBeEnabled());
    await user.click(group(d, 'Solana').getByRole('button', { name: 'Some' }));
    await user.click(within(d.getByTestId('networks-solana')).getByRole('checkbox', { name: 'USDC' }));
    await user.click(d.getByRole('button', { name: 'Create product' }));
    await screen.findByRole('heading', { level: 1, name: 'Mixed choices' });
    expect(bodies[0]!.networks).toEqual({ base: { accept: 'all' }, solana: { accept: 'some', assets: ['USDT'] } });
    expect(await screen.findByTestId('product-pay-with')).toHaveTextContent('Some');
  });

  it('shows a refusal of the server on the network it is about', async () => {
    const { mock, user } = renderApp('/products');
    const d = await openEdit(mock, user, 'Founder plan');
    await user.click(group(d, 'Solana').getByRole('button', { name: 'Some' }));
    // The server no longer offers USDT on Solana by the time the form is sent.
    mock.db().accepted.live = mock.db().accepted.live.filter((a) => a.asset !== 'USDT');
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    expect(await within(d.getByTestId('networks-solana')).findByText('networks.solana.assets: USDT is not offered on solana in live mode; use USDC')).toBeInTheDocument();
  });
});

describe('products list: Payers can pay with', () => {
  it('shows the network logo, the stablecoins payers can choose and All or Some', async () => {
    const { mock } = renderApp('/products');
    product(mock, 'Tip jar').networks = { solana: { accept: 'some', assets: ['USDT'] } };
    const rows = await screen.findAllByTestId('product-row');
    const row = (name: string) => rows.find((r) => r.textContent!.includes(name))!;
    const founder = within(row('Founder plan'));
    expect(founder.getByTestId('pay-with-base')).toHaveTextContent('All');
    expect(founder.getByTestId('pay-with-solana')).toHaveTextContent('All');
    expect(within(founder.getByTestId('pay-with-solana')).getAllByRole('img')).toHaveLength(3);
    expect(screen.getByText('Payers can pay with', { selector: '.rows-head span' })).toBeInTheDocument();
  });

  it('warns when a product has nothing to pay with, and why', async () => {
    const { mock } = renderApp('/products');
    const tip = product(mock, 'Tip jar');
    tip.networks = { solana: { accept: 'some', assets: ['USDT'] } };
    tip.payout.base = [];
    setAsset(mock, 'solana', 'USDT', false);
    const rows = await screen.findAllByTestId('product-row');
    const row = within(rows.find((r) => r.textContent!.includes('Tip jar'))!);
    expect(await row.findByText('Nothing to pay with: USDT on Solana is off in Settings')).toBeInTheDocument();
  });

  it('shows the same summary on the product page', async () => {
    const id = createMockApi().db().products.find((p) => p.livemode && p.name === 'Founder plan')!.id;
    renderApp(`/products/${id}`);
    expect(await screen.findByTestId('product-pay-with')).toHaveTextContent('All');
  });
});
