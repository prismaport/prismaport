import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { renderApp, useMockServer } from './helpers';

useMockServer();

const DISCARD_TEXT = 'If you close now, your changes are not saved.';

async function openProductCreate(opts: Parameters<typeof renderApp>[1] = {}) {
  const app = renderApp('/products', opts);
  await app.user.click(await screen.findByRole('button', { name: 'Add product' }));
  const dialog = await screen.findByRole('dialog', { name: 'Add product' });
  return { ...app, d: within(dialog), dialog };
}

const closed = () => waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());

describe('a drawer with unsaved changes', () => {
  it('closes at once when nothing was changed, the prefilled payout included', async () => {
    const { user, d } = await openProductCreate();
    await user.click(d.getByRole('button', { name: 'Cancel' }));
    await closed();
    expect(screen.queryByRole('alertdialog')).toBeNull();
  });

  it('closes at once when a change was put back', async () => {
    const { user, d } = await openProductCreate();
    await user.type(d.getByLabelText('Name'), 'x');
    await user.clear(d.getByLabelText('Name'));
    await user.keyboard('{Escape}');
    await closed();
  });

  it('asks before Escape loses a new product, and Keep editing keeps what was typed', async () => {
    const { user, d } = await openProductCreate();
    await user.type(d.getByLabelText('Name'), 'Founder plan');
    await user.keyboard('{Escape}');
    const ask = await screen.findByRole('alertdialog', { name: 'Discard this product?' });
    expect(ask).toHaveAccessibleDescription(DISCARD_TEXT);
    const keep = within(ask).getByRole('button', { name: 'Keep editing' });
    expect(keep).toHaveFocus();
    expect(keep).toHaveClass('btn-primary');
    expect(within(ask).getByRole('button', { name: 'Discard' })).not.toHaveClass('btn-primary');
    await user.click(keep);
    expect(screen.queryByRole('alertdialog')).toBeNull();
    expect(screen.getByRole('dialog', { name: 'Add product' })).toBeInTheDocument();
    expect(d.getByLabelText('Name')).toHaveValue('Founder plan');
  });

  it('asks on the X and on the backdrop too; Escape on the question goes back to editing', async () => {
    const { user, d, dialog } = await openProductCreate();
    await user.type(d.getByLabelText('Name'), 'Founder plan');
    await user.click(d.getByRole('button', { name: 'Close' }));
    expect(await screen.findByRole('alertdialog', { name: 'Discard this product?' })).toBeInTheDocument();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull());
    expect(d.getByLabelText('Name')).toHaveValue('Founder plan');

    fireEvent.mouseDown(dialog.parentElement!);
    expect(await screen.findByRole('alertdialog', { name: 'Discard this product?' })).toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'Add product' })).toBeInTheDocument();
  });

  it('Discard closes the drawer and gives the page its scroll back', async () => {
    const { user, d } = await openProductCreate();
    await user.type(d.getByLabelText('Name'), 'Founder plan');
    await user.click(d.getByRole('button', { name: 'Cancel' }));
    await user.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Discard' }));
    await closed();
    expect(screen.queryByRole('alertdialog')).toBeNull();
    expect(document.body.style.overflow).toBe('');
    expect(screen.getByRole('button', { name: 'Add product' })).toHaveFocus();
  });

  it('asks about the changes of a saved product', async () => {
    const { user } = renderApp('/products');
    await user.click(await screen.findByRole('button', { name: 'Edit Founder plan' }));
    const d = within(await screen.findByRole('dialog', { name: 'Edit product' }));
    await user.type(d.getByLabelText('Name'), ' 2');
    await user.keyboard('{Escape}');
    expect(await screen.findByRole('alertdialog', { name: 'Discard your changes?' })).toHaveAccessibleDescription(DISCARD_TEXT);
  });

  it('asks about a new link and about a changed one', async () => {
    const { user } = renderApp('/links');
    await user.click(await screen.findByRole('button', { name: 'New link' }));
    let d = within(await screen.findByRole('dialog', { name: 'Create payment link' }));
    await user.click(await d.findByRole('radio', { name: /Founder plan/ }));
    await user.click(d.getByRole('button', { name: 'Cancel' }));
    await user.click(within(await screen.findByRole('alertdialog', { name: 'Discard this link?' })).getByRole('button', { name: 'Discard' }));
    await closed();

    await user.click(await screen.findByRole('button', { name: 'Edit Invoice ACME #12' }));
    d = within(await screen.findByRole('dialog', { name: 'Edit payment link' }));
    await user.click(await d.findByRole('radio', { name: /Let the payer choose/ }));
    await user.keyboard('{Escape}');
    expect(await screen.findByRole('alertdialog', { name: 'Discard your changes?' })).toBeInTheDocument();
  });

  it('asks before a link in the drawer leaves the page', async () => {
    const { user, d } = await openProductCreate({ handlers: [http.get('/v1/dashboard/wallets', () => HttpResponse.json({ data: [] }))] });
    await user.type(d.getByLabelText('Name'), 'Founder plan, shared');
    await user.type(d.getByLabelText('Price'), '25');
    await user.click(await d.findByRole('link', { name: 'Add a wallet' }));
    const ask = await screen.findByRole('alertdialog', { name: 'Discard this product?' });
    await user.click(within(ask).getByRole('button', { name: 'Keep editing' }));
    expect(screen.getByRole('heading', { level: 1, name: 'Products' })).toBeInTheDocument();
    expect(d.getByLabelText('Name')).toHaveValue('Founder plan, shared');

    await user.click(d.getByRole('link', { name: 'Add a wallet' }));
    await user.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Discard' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Settings' })).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).toBeNull();
    // The router navigates in a transition, so React runs the drawer's effect cleanup (which gives
    // the page its scroll back) in a task after the commit that shows the new page, not within it.
    await waitFor(() => expect(document.body.style.overflow).toBe(''));
  });

  it('a saved drawer moves on without asking', async () => {
    const { user, d } = await openProductCreate();
    await user.type(d.getByLabelText('Name'), 'Founder plan, shared');
    await user.type(d.getByLabelText('Price'), '25');
    await waitFor(() => expect(d.getByRole('button', { name: 'Create product' })).toBeEnabled());
    await user.click(d.getByRole('button', { name: 'Create product' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Founder plan, shared' })).toBeInTheDocument();
    expect(screen.queryByRole('alertdialog')).toBeNull();
  });
});
