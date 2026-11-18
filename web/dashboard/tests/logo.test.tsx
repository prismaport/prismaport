import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { logoFileProblem, logoSizeProblem } from '../src/lib/logo';
import { renderApp, useMockServer } from './helpers';

useMockServer();

// 8 bytes of the PNG signature, enough for the mock server to accept it.
const PNG_B64 = btoa(String.fromCharCode(0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a));
const png = (name = 'logo.png') => new File([Uint8Array.from(atob(PNG_B64), (c) => c.charCodeAt(0))], name, { type: 'image/png' });
const file = (input: HTMLElement) => input as HTMLInputElement;

const sized = (width: number, height: number) =>
  vi.stubGlobal('createImageBitmap', vi.fn(async () => ({ width, height, close: () => {} })));

afterEach(() => vi.unstubAllGlobals());

describe('logoSizeProblem', () => {
  it('refuses a 4K image, a too-small one, and passes what the server takes', async () => {
    const f = new Blob(['x']);
    for (const [w, h] of [[3840, 2160], [4097, 32], [32, 4097], [2049, 2049]] as const) {
      sized(w, h);
      expect(await logoSizeProblem(f)).toMatch(new RegExp(`${w} × ${h} pixels, larger than we accept`));
    }
    sized(31, 200);
    expect(await logoSizeProblem(f)).toMatch(/at least 32 × 32/);
    for (const [w, h] of [[640, 160], [4096, 1024], [2048, 2048], [32, 32]] as const) {
      sized(w, h);
      expect(await logoSizeProblem(f)).toBeNull();
    }
  });

  it('leaves the decision to the server when the browser cannot read the image', async () => {
    vi.stubGlobal('createImageBitmap', vi.fn(async () => { throw new Error('bad image'); }));
    expect(await logoSizeProblem(new Blob(['x']))).toBeNull();
    vi.stubGlobal('createImageBitmap', undefined);
    expect(await logoSizeProblem(new Blob(['x']))).toBeNull();
  });
});

describe('logoFileProblem', () => {
  it('accepts png, jpeg and webp up to 512 KB and refuses the rest', () => {
    expect(logoFileProblem({ type: 'image/webp', size: 512 * 1024 })).toBeNull();
    expect(logoFileProblem({ type: 'image/gif', size: 10 })).toMatch(/PNG, JPEG or WebP/);
    expect(logoFileProblem({ type: 'image/png', size: 512 * 1024 + 1 })).toMatch(/512 KB/);
  });
});

describe('checkout logo setting', () => {
  it('shows the empty state, then the uploaded logo, then removes it', async () => {
    const { user, mock } = renderApp('/settings/account');
    expect(await screen.findByText(/^No logo\. Your checkout shows no business name or logo\./)).toBeInTheDocument();
    expect(screen.getByText(/PNG, JPEG or WebP, up to 512 KB\./)).toBeInTheDocument();
    await user.upload(file(screen.getByLabelText('Logo file')), png());
    const img = await screen.findByRole('img', { name: 'Your checkout logo' });
    expect(img.getAttribute('src')).toMatch(/^\/v1\/checkout\/logos\/[0-9a-f]{32}\.png$/);
    expect(mock.db().merchant.logo_url).toBe(img.getAttribute('src'));
    expect(screen.getByRole('button', { name: 'Replace logo' })).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Remove' }));
    const d = within(await screen.findByRole('dialog', { name: 'Remove your checkout logo' }));
    await user.click(d.getByRole('button', { name: 'Remove logo' }));
    await waitFor(() => expect(screen.queryByRole('img', { name: 'Your checkout logo' })).toBeNull());
    expect(screen.getByText(/^No logo\./)).toBeInTheDocument();
    expect(mock.db().merchant.logo_url).toBeNull();
  });

  it('shows the server message when it refuses the image', async () => {
    const { user } = renderApp('/settings/account', {
      handlers: [http.put('*/v1/dashboard/logo', () => HttpResponse.json({ error: { code: 'invalid_request', param: 'image', message: 'image must be at least 32 x 32 pixels' } }, { status: 400 }))],
    });
    await screen.findByText(/^No logo\./);
    await user.upload(file(screen.getByLabelText('Logo file')), png());
    expect(await screen.findByText('image must be at least 32 x 32 pixels')).toBeInTheDocument();
    expect(screen.queryByRole('img', { name: 'Your checkout logo' })).toBeNull();
  });

  it('refuses a wrong type or a too-big file without calling the API', async () => {
    let calls = 0;
    renderApp('/settings/account', {
      handlers: [http.put('*/v1/dashboard/logo', () => { calls++; return HttpResponse.json({ logo_url: '/x' }); })],
    });
    await screen.findByText(/^No logo\./);
    const input = file(screen.getByLabelText('Logo file'));
    // userEvent honours accept=, as a real picker does; a drop or a renamed file is what reaches the check.
    fireEvent.change(input, { target: { files: [new File(['GIF89a'], 'a.gif', { type: 'image/gif' })] } });
    expect(await screen.findByRole('alert')).toHaveTextContent('The logo must be a PNG, JPEG or WebP file.');
    fireEvent.change(input, { target: { files: [new File([new Uint8Array(512 * 1024 + 1)], 'big.png', { type: 'image/png' })] } });
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('larger than 512 KB'));
    sized(3840, 2160);
    fireEvent.change(input, { target: { files: [png('4k.png')] } });
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('3840 × 2160 pixels, larger than we accept'));
    expect(calls).toBe(0);
  });

  it('shows the logo in the payment page preview once it is set', async () => {
    const { user, mock } = renderApp('/links');
    mock.db().merchant.logo_url = '/v1/checkout/logos/0123456789abcdef0123456789abcdef.png';
    await user.click(await screen.findByRole('button', { name: /New link/ }));
    const img = await screen.findByTestId('preview-logo');
    expect(img).toHaveAttribute('alt', 'Acme Studio');
    expect(img.getAttribute('src')).toContain('/v1/checkout/logos/0123456789abcdef0123456789abcdef.png');
  });
});
