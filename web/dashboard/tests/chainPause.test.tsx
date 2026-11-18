import { screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { isPaused, pauseText } from '../src/lib/pauses';
import type { ChainPause } from '../src/lib/types';
import { renderApp, useMockServer } from './helpers';

useMockServer();

const base: ChainPause = { chain: 'base', chain_name: 'Base', asset: null, livemode: true, paused_at: '2026-11-12T14:05:00Z' };
const usdt: ChainPause = { chain: 'solana', chain_name: 'Solana', asset: 'USDT', livemode: true, paused_at: '2026-11-12T14:07:00Z' };

describe('pause text and matching (D64)', () => {
  it('words a whole chain, one asset and several, with no operator text', () => {
    expect(pauseText([base])).toBe("Payments on Base are paused. New checkouts won't offer Base for now; payments already sent there are still credited and paid out.");
    expect(pauseText([usdt])).toBe("USDT payments on Solana are paused. New checkouts won't offer USDT on Solana for now; payments already sent are still credited and paid out.");
    expect(pauseText([base, usdt])).toContain('Payments on some networks are paused: Base, USDT on Solana.');
  });

  it('a chain pause covers every asset, an asset pause only that asset', () => {
    expect(isPaused([base], 'base', 'USDC')).toBe(true);
    expect(isPaused([usdt], 'solana', 'USDT')).toBe(true);
    expect(isPaused([usdt], 'solana', 'USDC')).toBe(false);
    expect(isPaused([usdt], 'base', 'USDC')).toBe(false);
  });
});

describe('pause strip', () => {
  it('shows nothing by default', async () => {
    renderApp('/overview');
    await screen.findByRole('heading', { level: 1 });
    expect(screen.queryByTestId('pause-strip')).toBeNull();
  });

  it('shows the strip on a page for the current mode and tags that chain\'s totals', async () => {
    const { mock } = renderApp('/overview');
    mock.db().chain_pauses = [base];
    const strip = await screen.findByTestId('pause-strip');
    expect(strip).toHaveAttribute('role', 'status');
    expect(strip).toHaveTextContent('Payments on Base are paused.');
    const totals = await screen.findAllByTestId('total');
    const baseTotal = totals.find((t) => /Base/.test(t.textContent ?? ''));
    expect(baseTotal).toBeDefined();
    expect(within(baseTotal!).getByTestId('paused-tag')).toHaveTextContent('Paused');
  });

  it('ignores a pause of the other mode', async () => {
    const { mock } = renderApp('/overview');
    mock.db().chain_pauses = [{ ...base, livemode: false }];
    await screen.findByRole('heading', { level: 1 });
    expect(screen.queryByTestId('pause-strip')).toBeNull();
  });
});
