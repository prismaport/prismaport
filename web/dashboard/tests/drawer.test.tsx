import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import { renderApp, server, useMockServer } from './helpers';

useMockServer();
afterEach(() => server.events.removeAllListeners());

/** Every create and edit of a product or a link, with its Idempotency-Key. */
function captureWrites() {
  const writes: { method: string; path: string; key: string | null; body: unknown }[] = [];
  server.events.on('request:start', async ({ request }) => {
    const path = new URL(request.url).pathname;
    if ((request.method === 'POST' || request.method === 'PATCH') && /^\/v1\/dashboard\/(products|links)(\/[^/]+)?$/.test(path)) {
      writes.push({ method: request.method, path, key: request.headers.get('Idempotency-Key'), body: await request.clone().json() });
    }
  });
  return writes;
}

async function openProductCreate(opts: Parameters<typeof renderApp>[1] = {}) {
  const app = renderApp('/products', opts);
  await app.user.click(await screen.findByRole('button', { name: 'Add product' }));
  const dialog = await screen.findByRole('dialog', { name: 'Add product' });
  return { ...app, d: within(dialog), dialog };
}

describe('product drawer', () => {
  it('shows a live preview of what the payer sees, description included', async () => {
    const { user, d } = await openProductCreate();
    const preview = () => within(screen.getByTestId('link-preview'));
    expect(preview().getByText('Your product name')).toBeInTheDocument();
    await user.type(d.getByLabelText('Name'), 'Founder plan');
    await user.type(d.getByLabelText(/Description/), 'Lifetime access.');
    await user.type(d.getByLabelText('Price'), '25');
    expect(preview().getByTestId('preview-name')).toHaveTextContent('Founder plan');
    expect(preview().getByTestId('preview-description')).toHaveTextContent('Lifetime access.');
    expect(preview().getByTestId('preview-price')).toHaveTextContent('25.00 USD');
    await user.click(d.getByRole('radio', { name: /Variable price/ }));
    expect(d.getByText('The amount is decided per link, by the payer, or by your server.')).toBeInTheDocument();
    await user.type(d.getByLabelText('Minimum'), '5');
    await user.type(d.getByLabelText(/Maximum/), '250');
    await user.type(d.getByLabelText('Suggested amount 1'), '10');
    await user.type(d.getByLabelText('Suggested amount 2'), '25');
    expect(preview().getByTestId('preview-hint')).toHaveTextContent('Between 5.00 and 250.00 USD.');
    expect(preview().getByTestId('preview-chips')).toHaveTextContent('10.00 USD25.00 USD');
  });

  it('shows clear messages, marks the fields and focuses the first one', async () => {
    const { user, d } = await openProductCreate();
    await user.type(d.getByLabelText('Price'), '0.10');
    await user.click(d.getByRole('button', { name: 'Create product' }));
    expect(d.getByRole('alert')).toHaveTextContent('Fix the 2 highlighted fields to continue.');
    // A new product is one form: no steps, and the payout stays a recap.
    expect(d.queryByRole('list', { name: 'Steps' })).toBeNull();
    expect(d.queryByTestId('payout-section')).toBeNull();
    const name = d.getByLabelText('Name');
    await waitFor(() => expect(name).toHaveFocus());
    expect(name).toHaveAccessibleDescription(/Give the product a name/);
    await waitFor(() => expect(d.getByLabelText('Price')).toHaveAccessibleDescription(/must be at least 0\.11 USD, the smallest amount that settles after the Prismaport fee of 0\.5% \+ 0\.10 USD/));
  });

  it('creates the product and, with the box checked, a link with no options, then offers to copy it', async () => {
    const writes = captureWrites();
    const { user, d, mock } = await openProductCreate();
    await user.type(d.getByLabelText('Name'), 'Pay Acme');
    await user.click(d.getByRole('radio', { name: /Variable price/ }));
    await user.type(d.getByLabelText('Minimum'), '5');
    await user.click(d.getByRole('button', { name: /More options/ }));
    await user.click(d.getByRole('button', { name: 'Add metadata' }));
    await user.type(d.getByLabelText('Key 1'), 'campaign');
    await user.type(d.getByLabelText('Value 1'), 'fall');
    // One step: the payout is a recap of the default wallets, not a second step.
    expect(await d.findByTestId('payout-recap')).toHaveTextContent('Where the money goes');
    expect(d.queryByRole('button', { name: 'Continue' })).toBeNull();
    const box = d.getByRole('checkbox', { name: 'Also create a payment link' });
    expect(box).toBeChecked();
    await waitFor(() => expect(d.getByRole('button', { name: 'Create product' })).toBeEnabled());
    await user.click(d.getByRole('button', { name: 'Create product' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Pay Acme' })).toBeInTheDocument();
    const oldest = (family: string) => mock.db().wallets.find((w) => w.livemode && w.family === family && !w.archived_at)!.id;
    expect(writes.map((w) => [w.method, w.path])).toEqual([
      ['POST', '/v1/dashboard/products'],
      ['POST', '/v1/dashboard/links'],
    ]);
    expect(writes[0]!.body).toEqual({
      name: 'Pay Acme', price_type: 'open', min_amount: '5.00', metadata: { campaign: 'fall' },
      payout: { base: [{ wallet_id: oldest('evm'), share: '100.00' }], solana: [{ wallet_id: oldest('solana'), share: '100.00' }] },
      networks: { base: { accept: 'all' }, solana: { accept: 'all' } },
    });
    const product = mock.db().products.find((p) => p.name === 'Pay Acme')!;
    expect(writes[1]!.body).toEqual({ product_id: product.id });
    expect(writes[0]!.key).toMatch(/^[0-9a-f-]{36}$/);
    expect(writes[1]!.key).toMatch(/^[0-9a-f-]{36}$/);
    expect(writes[1]!.key).not.toBe(writes[0]!.key);
    expect(await screen.findByText('Product and link created')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Copy link' }));
    const link = mock.db().links.find((l) => l.product_id === product.id)!;
    expect(await navigator.clipboard.readText()).toBe(`https://prismaport.xyz/l/${link.id}`);
    expect(await screen.findByText('Link copied')).toBeInTheDocument();
    expect(await screen.findByTestId('link-row')).toHaveAttribute('data-link-id', link.id);
  });

  it('creates only the product when the box is unchecked', async () => {
    const writes = captureWrites();
    const { user, d } = await openProductCreate();
    await user.type(d.getByLabelText('Name'), 'Catalog only');
    await user.type(d.getByLabelText('Price'), '9');
    await waitFor(() => expect(d.getByRole('button', { name: 'Change' })).toBeEnabled());
    await user.click(d.getByRole('checkbox', { name: 'Also create a payment link' }));
    await user.click(d.getByRole('button', { name: 'Create product' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Catalog only' })).toBeInTheDocument();
    expect(writes.map((w) => w.path)).toEqual(['/v1/dashboard/products']);
    expect(await screen.findByText('No payment links yet. Create one to start selling this product.')).toBeInTheDocument();
  });

  it('keeps the product when its link fails, and says so', async () => {
    const { user, d } = await openProductCreate({
      handlers: [http.post('/v1/dashboard/links', () => HttpResponse.json({ error: { code: 'invalid_request', message: 'Links are paused.' } }, { status: 400 }))],
    });
    await user.type(d.getByLabelText('Name'), 'Half made');
    await user.type(d.getByLabelText('Price'), '9');
    await waitFor(() => expect(d.getByRole('button', { name: 'Create product' })).toBeEnabled());
    await user.click(d.getByRole('button', { name: 'Create product' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Half made' })).toBeInTheDocument();
    expect(await screen.findByText('Half made created, but its link was not: Links are paused. Create it from the product page.')).toBeInTheDocument();
  });

  it('reuses the same key when the same create is retried', async () => {
    const keys: string[] = [];
    let calls = 0;
    const { user, d } = await openProductCreate({
      handlers: [
        http.post('/v1/dashboard/products', ({ request }) => {
          keys.push(request.headers.get('Idempotency-Key') ?? '');
          calls++;
          return calls === 1 ? HttpResponse.error() : HttpResponse.json({ error: { code: 'invalid_request', message: 'name is taken.' } }, { status: 400 });
        }),
      ],
    });
    await user.type(d.getByLabelText('Name'), 'Retry me');
    await user.type(d.getByLabelText('Price'), '9');
    await waitFor(() => expect(d.getByRole('button', { name: 'Create product' })).toBeEnabled());
    await user.click(d.getByRole('button', { name: 'Create product' }));
    expect(await d.findByText(/Cannot reach Prismaport/)).toBeInTheDocument();
    await user.click(d.getByRole('button', { name: 'Create product' }));
    expect(await d.findByText('name is taken.')).toBeInTheDocument();
    expect(keys).toHaveLength(2);
    expect(keys[0]).toBe(keys[1]);
  });

  it('edits a product, sending only what changed, never its price type', async () => {
    const writes = captureWrites();
    const { user } = renderApp('/products');
    await user.click(await screen.findByRole('button', { name: 'Edit Founder plan' }));
    const d = within(await screen.findByRole('dialog', { name: 'Edit product' }));
    expect(d.getByText(/Price changes apply to new payments only/)).toBeInTheDocument();
    expect(d.getByRole('radio', { name: /Variable price/ })).toBeDisabled();
    expect(d.getByText(/price type of a product cannot change/)).toBeInTheDocument();
    expect(d.queryByRole('checkbox', { name: 'Also create a payment link' })).toBeNull();
    expect(d.getByLabelText('Price')).toHaveValue('25.00');
    await user.clear(d.getByLabelText('Price'));
    await user.type(d.getByLabelText('Price'), '30');
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    // Founder plan has live links: the price change asks first, keeping them by default.
    const ask = within(await screen.findByRole('dialog', { name: 'What happens to the 2 active links?' }));
    expect(ask.getByRole('radio', { name: /Keep them active/ })).toBeChecked();
    await user.click(ask.getByRole('button', { name: 'Save and keep links' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(writes.map((w) => w.body)).toEqual([{ amount: '30.00', links: 'keep' }]);
    const row = screen.getByRole('link', { name: 'Founder plan' }).closest('li') as HTMLElement;
    await waitFor(() => expect(within(row).getByTestId('product-price')).toHaveTextContent('30.00 USD'));
  });

  it('asks what happens to the live links before a price change, and deactivates them', async () => {
    const writes = captureWrites();
    const { user, mock } = renderApp('/products');
    await user.click(await screen.findByRole('button', { name: 'Edit Pay Acme Studio' }));
    const d = within(await screen.findByRole('dialog', { name: 'Edit product' }));
    for (const [label, value] of [['Minimum', '130'], ['Suggested amount 1', '150'], ['Suggested amount 2', ''], ['Suggested amount 3', '']] as const) {
      await user.clear(d.getByLabelText(label));
      if (value) await user.type(d.getByLabelText(label), value);
    }
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    const ask = within(await screen.findByRole('dialog', { name: 'What happens to the 2 active links?' }));
    expect(writes).toEqual([]);
    expect(await ask.findByTestId('own-amount-links')).toHaveTextContent('A link that sets its own amount keeps it: Invoice ACME #12 (120.00 USD).');
    const acme = mock.db().products.find((p) => p.name === 'Pay Acme Studio')!;
    const open = mock.db().sessions.filter((x) => x.product_id === acme.id && (x.status === 'open' || x.status === 'partially_paid')).length;
    // No checkout of this product is open, so there is nothing to say about them.
    expect(open).toBe(0);
    expect(ask.queryByTestId('open-checkouts')).toBeNull();
    await user.click(ask.getByRole('radio', { name: /Deactivate them/ }));
    expect(ask.getByText('Nobody can open a new checkout from them until you turn each one back on.')).toBeInTheDocument();
    await user.click(ask.getByRole('button', { name: 'Save and deactivate links' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(writes.map((w) => w.body)).toEqual([{ min_amount: '130.00', suggested_amounts: ['150.00'], links: 'deactivate' }]);
    expect(mock.db().links.filter((l) => l.product_id === acme.id).map((l) => l.active)).toEqual([false, false]);
  });

  it('sends nothing when the question is cancelled, and never asks about a change that is not money', async () => {
    const writes = captureWrites();
    const { user, mock } = renderApp('/products');
    await user.click(await screen.findByRole('button', { name: 'Edit Founder plan' }));
    const d = within(await screen.findByRole('dialog', { name: 'Edit product' }));
    await user.clear(d.getByLabelText('Price'));
    await user.type(d.getByLabelText('Price'), '31');
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    const ask = within(await screen.findByRole('dialog', { name: 'What happens to the 2 active links?' }));
    // A fixed price: no link sets its own amount. Its partially paid checkout keeps the old price.
    expect(ask.queryByTestId('own-amount-links')).toBeNull();
    const founder = mock.db().products.find((p) => p.name === 'Founder plan')!;
    const open = mock.db().sessions.filter((x) => x.product_id === founder.id && (x.status === 'open' || x.status === 'partially_paid')).length;
    expect(open).toBeGreaterThan(0);
    expect(ask.getByTestId('open-checkouts')).toHaveTextContent(
      `${open === 1 ? '1 checkout already open keeps' : `${open} checkouts already open keep`} the old price and payout either way.`,
    );
    await user.click(ask.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog', { name: /What happens/ })).toBeNull();
    expect(writes).toEqual([]);
    await user.clear(d.getByLabelText('Price'));
    await user.type(d.getByLabelText('Price'), '25');
    await user.clear(d.getByLabelText('Name'));
    await user.type(d.getByLabelText('Name'), 'Founder pass');
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(writes.map((w) => w.body)).toEqual([{ name: 'Founder pass' }]);
  });

  it('opens the same question when the server asks for it', async () => {
    const writes = captureWrites();
    // A link turned on elsewhere since the page loaded: the server knows of active links the page does not.
    const asks = http.patch(
      '/v1/dashboard/products/:id',
      () => HttpResponse.json({ error: { code: 'links_decision_required', message: 'This product has 3 active links. Choose whether to keep them active or deactivate them.', param: 'links' } }, { status: 409 }),
      { once: true },
    );
    const { user } = renderApp('/products', { handlers: [asks] });
    await user.click(await screen.findByRole('button', { name: 'Edit Custom order' }));
    const d = within(await screen.findByRole('dialog', { name: 'Edit product' }));
    await user.clear(d.getByLabelText('Minimum'));
    await user.type(d.getByLabelText('Minimum'), '2');
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    const ask = within(await screen.findByRole('dialog', { name: 'What happens to the 3 active links?' }));
    expect(d.queryByRole('alert')).toBeNull();
    await user.click(ask.getByRole('button', { name: 'Save and keep links' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(writes.map((w) => w.body)).toEqual([{ min_amount: '2.00' }, { min_amount: '2.00', links: 'keep' }]);
  });

  it('opens More options on its own error, and keeps everything typed', async () => {
    const { user, d } = await openProductCreate();
    await user.type(d.getByLabelText('Name'), 'Workshop');
    await user.type(d.getByLabelText('Price'), '12');
    await user.click(d.getByRole('button', { name: /More options/ }));
    await user.type(d.getByLabelText(/Return address/), 'ftp://acme.com');
    await user.click(d.getByRole('button', { name: /More options/ }));
    expect(d.queryByLabelText(/Return address/)).toBeNull();
    await user.click(d.getByRole('button', { name: 'Create product' }));
    const url = await d.findByLabelText(/Return address/);
    await waitFor(() => expect(url).toHaveFocus());
    expect(url).toHaveAccessibleDescription(/starts with https:\/\//);
    expect(d.getByLabelText('Name')).toHaveValue('Workshop');
    expect(d.getByLabelText('Price')).toHaveValue('12');
  });

  it('recaps the default wallet of each network at 100%, and Change opens the split editor in place', async () => {
    const { user, d } = await openProductCreate();
    const recap = await d.findByTestId('payout-recap');
    await waitFor(() => expect(within(recap).getAllByTestId('payout-recap-line').map((l) => l.textContent)).toEqual(['Base: 100% Founder', 'Solana: 100% Treasury']));
    expect(d.queryByTestId('payout-section')).toBeNull();
    await user.type(d.getByLabelText('Name'), 'Workshop');
    await user.click(within(recap).getByRole('button', { name: 'Change' }));
    expect(d.queryByTestId('payout-recap')).toBeNull();
    expect(await d.findByTestId('payout-section')).toBeInTheDocument();
    expect(d.getByLabelText('Base wallet 1')).toBeEnabled();
    // The details are still there, nothing was a step away.
    expect(d.getByLabelText('Name')).toHaveValue('Workshop');
    await user.click(d.getByRole('button', { name: 'Done' }));
    expect(await d.findByTestId('payout-recap')).toBeInTheDocument();
  });

  it('the recap and the created product follow the default wallet', async () => {
    const writes = captureWrites();
    const { user, mock } = renderApp('/products');
    for (const w of mock.db().wallets) if (w.livemode && w.family === 'evm') w.default = w.label === 'Partner';
    await user.click(await screen.findByRole('button', { name: 'Add product' }));
    const d = within(await screen.findByRole('dialog', { name: 'Add product' }));
    await user.type(d.getByLabelText('Name'), 'Workshop');
    await user.type(d.getByLabelText('Price'), '12');
    await waitFor(() => expect(d.getAllByTestId('payout-recap-line')[0]).toHaveTextContent('Base: 100% Partner'));
    await user.click(d.getByRole('button', { name: 'Create product' }));
    await screen.findByRole('heading', { level: 1, name: 'Workshop' });
    const partner = mock.db().wallets.find((w) => w.livemode && w.family === 'evm' && w.label === 'Partner')!;
    expect((writes[0]!.body as { payout: { base: unknown } }).payout.base).toEqual([{ wallet_id: partner.id, share: '100.00' }]);
  });

  it('opens the split editor by itself when the server refuses the split', async () => {
    const { user, d } = await openProductCreate({
      handlers: [http.post('/v1/dashboard/products', () => HttpResponse.json({ error: { code: 'invalid_request', message: 'Base shares must add up to 100%.', param: 'payout.base' } }, { status: 400 }))],
    });
    await user.type(d.getByLabelText('Name'), 'Workshop');
    await user.type(d.getByLabelText('Price'), '12');
    await waitFor(() => expect(d.getByRole('button', { name: 'Create product' })).toBeEnabled());
    await user.click(d.getByRole('button', { name: 'Create product' }));
    expect(await d.findByTestId('payout-section')).toBeInTheDocument();
    expect(d.getByRole('button', { name: 'Done' })).toBeInTheDocument();
  });

  it('on an edit, sends the merchant to the step of the first error', async () => {
    const { user } = renderApp('/products');
    await user.click(await screen.findByRole('button', { name: 'Edit Founder plan' }));
    const d = within(await screen.findByRole('dialog', { name: 'Edit product' }));
    await user.clear(d.getByLabelText('Name'));
    await user.click(d.getByRole('button', { name: 'How you get paid' }));
    await waitFor(() => expect(d.getByLabelText('Base wallet 1')).toBeEnabled());
    await user.click(d.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(d.getByLabelText('Name')).toHaveFocus());
    expect(d.getByRole('alert')).toHaveTextContent('Fix the highlighted field to continue.');
  });

  it('keeps Save disabled while the wallets load, then saves', async () => {
    const writes = captureWrites();
    let release!: () => void;
    const loaded = new Promise<void>((resolve) => (release = resolve));
    // Falls through to the mock's wallets once released.
    const { user } = renderApp('/products', { handlers: [http.get('/v1/dashboard/wallets', async () => void (await loaded))] });
    await user.click(await screen.findByRole('button', { name: 'Edit Founder plan' }));
    const d = within(await screen.findByRole('dialog', { name: 'Edit product' }));
    await user.clear(d.getByLabelText('Name'));
    await user.type(d.getByLabelText('Name'), 'Founder plan 2');
    const save = d.getByRole('button', { name: 'Save changes' });
    expect(save).toBeDisabled();
    expect(save).toHaveAccessibleDescription('Loading your wallets…');
    await user.click(save);
    expect(writes).toEqual([]);
    release();
    await waitFor(() => expect(save).toBeEnabled());
    expect(save).not.toHaveAttribute('aria-describedby');
    await user.click(save);
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(writes.map((w) => w.body)).toEqual([{ name: 'Founder plan 2' }]);
  });

  it('says so when the wallets cannot be loaded, and never saves', async () => {
    const writes = captureWrites();
    const { user } = renderApp('/products', {
      handlers: [http.get('/v1/dashboard/wallets', () => HttpResponse.json({ error: { code: 'internal_error', message: 'Something went wrong.' } }, { status: 500 }))],
    });
    await user.click(await screen.findByRole('button', { name: 'Edit Founder plan' }));
    const d = within(await screen.findByRole('dialog', { name: 'Edit product' }));
    expect(await d.findByRole('alert')).toHaveTextContent('Your wallets and payout settings could not be loaded. Close this and try again.');
    const save = d.getByRole('button', { name: 'Save changes' });
    expect(save).toBeDisabled();
    await user.click(save);
    expect(writes).toEqual([]);
  });

  it('says so when the payout settings cannot be loaded', async () => {
    const { user } = renderApp('/products', {
      handlers: [http.get('/v1/dashboard/settings', () => HttpResponse.json({ error: { code: 'internal_error', message: 'Something went wrong.' } }, { status: 500 }))],
    });
    await user.click(await screen.findByRole('button', { name: 'Edit Founder plan' }));
    const d = within(await screen.findByRole('dialog', { name: 'Edit product' }));
    expect(d.getByRole('button', { name: 'Save changes' })).toBeDisabled();
    expect(await d.findByRole('alert')).toHaveTextContent('Your wallets and payout settings could not be loaded. Close this and try again.');
  });

  it('on a create, keeps Create product disabled while the wallets load', async () => {
    const writes = captureWrites();
    let release!: () => void;
    const loaded = new Promise<void>((resolve) => (release = resolve));
    const { user, d } = await openProductCreate({ handlers: [http.get('/v1/dashboard/wallets', async () => void (await loaded))] });
    await user.type(d.getByLabelText('Name'), 'Workshop');
    await user.type(d.getByLabelText('Price'), '12');
    expect(await d.findByRole('button', { name: 'Create product' })).toBeInTheDocument();
    const create = d.getByRole('button', { name: 'Create product' });
    expect(create).toBeDisabled();
    expect(create).toHaveAccessibleDescription('Loading your wallets…');
    await user.click(create);
    expect(writes).toEqual([]);
    release();
    await waitFor(() => expect(create).toBeEnabled());
  });

  it('traps focus and closes on Escape, returning focus to the opener', async () => {
    const { user } = await openProductCreate();
    expect(screen.getByLabelText('Name')).toHaveFocus();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'Add product' })).toHaveFocus();
  });
});
