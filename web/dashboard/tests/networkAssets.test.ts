import { describe, expect, it } from 'vitest';
import {
  chooseNetwork,
  effectiveAssets,
  effectiveMethods,
  leftWithNothing,
  networksBody,
  networksErrors,
  networksPatch,
  prefillNetworks,
  tickedOn,
  toggleAsset,
  usedBy,
} from '../src/lib/networkAssets';
import type { AcceptedAsset, SettingsProductNetwork, Wallet } from '../src/lib/types';

const settings = (usdt: boolean, baseUsdc = true): AcceptedAsset[] => [
  { chain: 'base', asset: 'USDC', enabled: baseUsdc },
  { chain: 'solana', asset: 'USDC', enabled: true },
  { chain: 'solana', asset: 'USDT', enabled: usdt },
];

const wallet = (id: string, family: 'evm' | 'solana'): Wallet => ({ id, label: id, family, address: id, verified: true, archived_at: null, created_at: '2026-01-01T00:00:00Z' }) as Wallet;
const wallets = [wallet('w_evm', 'evm'), wallet('w_sol', 'solana')];

describe('effective stablecoins', () => {
  it('All is every stablecoin on in Settings, Some only the ticked ones that are on, Off nothing', () => {
    const s = settings(true);
    expect(effectiveAssets({ accept: 'all', assets: [] }, s, 'solana')).toEqual(['USDC', 'USDT']);
    expect(effectiveAssets({ accept: 'some', assets: ['USDT'] }, s, 'solana')).toEqual(['USDT']);
    expect(effectiveAssets({ accept: 'off', assets: ['USDT'] }, s, 'solana')).toEqual([]);
    expect(effectiveAssets(undefined, s, 'solana')).toEqual([]);
  });

  it('a stablecoin off in Settings is not offered, and comes back when it is turned on again', () => {
    const some = { accept: 'some' as const, assets: ['USDT'] };
    expect(effectiveAssets(some, settings(false), 'solana')).toEqual([]);
    expect(effectiveAssets(some, settings(true), 'solana')).toEqual(['USDT']);
    expect(effectiveAssets({ accept: 'all', assets: [] }, settings(false), 'solana')).toEqual(['USDC']);
  });

  it('lists the methods of every network in order', () => {
    const form = { base: { accept: 'all' as const, assets: [] }, solana: { accept: 'some' as const, assets: ['USDT'] } };
    expect(effectiveMethods(form, settings(true), ['base', 'solana']).map((m) => `${m.asset} on ${m.chain_name}`)).toEqual(['USDC on Base', 'USDT on Solana']);
    expect(effectiveMethods({}, settings(true), ['base', 'solana'])).toEqual([]);
  });
});

describe('Settings: used by and impact', () => {
  const nets: SettingsProductNetwork[] = [
    { product_id: 'p1', product_name: 'Pro plan', chain: 'solana', accept: 'some', assets: ['USDT'] },
    { product_id: 'p2', product_name: 'Donation', chain: 'solana', accept: 'some', assets: ['USDT'] },
    { product_id: 'p3', product_name: 'Starter', chain: 'solana', accept: 'all' },
    { product_id: 'p4', product_name: 'Mixed', chain: 'solana', accept: 'some', assets: ['USDC', 'USDT'] },
    { product_id: 'p1', product_name: 'Pro plan', chain: 'base', accept: 'all' },
  ];

  it('counts the products that take a stablecoin now: All on that network, or Some with it ticked', () => {
    expect(usedBy(nets, 'solana', 'USDT')).toBe(4);
    expect(usedBy(nets, 'solana', 'USDC')).toBe(2);
    expect(usedBy(nets, 'base', 'USDC')).toBe(1);
  });

  it('counts only the products that keep it ticked for a row that is off', () => {
    expect(tickedOn(nets, 'solana', 'USDT')).toBe(3);
    expect(tickedOn(nets, 'solana', 'USDC')).toBe(1);
    expect(tickedOn(nets, 'base', 'USDC')).toBe(0);
  });

  it('names the products left taking nothing on the network, exactly', () => {
    expect(leftWithNothing(nets, settings(true), settings(false), 'solana')).toEqual([
      { product_id: 'p1', product_name: 'Pro plan' },
      { product_id: 'p2', product_name: 'Donation' },
    ]);
    // Nothing changes for another network, nor when nothing is turned off.
    expect(leftWithNothing(nets, settings(true), settings(false), 'base')).toEqual([]);
    expect(leftWithNothing(nets, settings(true), settings(true), 'solana')).toEqual([]);
    // A product that already took nothing is not news.
    expect(leftWithNothing(nets, settings(false), settings(false), 'solana')).toEqual([]);
  });

  it('turning the whole network off leaves the All products with nothing too', () => {
    expect(leftWithNothing(nets, settings(true), settings(true, false), 'base')).toEqual([{ product_id: 'p1', product_name: 'Pro plan' }]);
  });
});

describe('what a product sends', () => {
  it('sends every network of a new product, Off as none', () => {
    expect(networksBody({ base: { accept: 'all', assets: [] }, solana: { accept: 'some', assets: ['USDT'] }, ethereum: { accept: 'off', assets: ['USDC'] } })).toEqual({
      base: { accept: 'all' },
      solana: { accept: 'some', assets: ['USDT'] },
      ethereum: { accept: 'none' },
    });
  });

  it('patches only the networks whose choice changed', () => {
    const was = [
      { chain: 'base', chain_name: 'Base', accept: 'all' as const },
      { chain: 'solana', chain_name: 'Solana', accept: 'some' as const, assets: ['USDC', 'USDT'] },
    ];
    expect(networksPatch(was, { base: { accept: 'all' }, solana: { accept: 'some', assets: ['USDT', 'USDC'] } })).toBeUndefined();
    expect(networksPatch(was, { base: { accept: 'all' }, solana: { accept: 'some', assets: ['USDT'] } })).toEqual({ solana: { accept: 'some', assets: ['USDT'] } });
    expect(networksPatch(was, { base: { accept: 'none' }, solana: { accept: 'all' } })).toEqual({ base: { accept: 'none' }, solana: { accept: 'all' } });
    // A network the product never took, left Off, is not news.
    expect(networksPatch([was[0]!], { base: { accept: 'all' }, solana: { accept: 'none' } })).toBeUndefined();
    expect(networksPatch([was[0]!], { solana: { accept: 'all' } })).toEqual({ solana: { accept: 'all' } });
  });

  it('refuses Some with nothing ticked', () => {
    expect(networksErrors({ solana: { accept: 'some', assets: [] }, base: { accept: 'all', assets: [] } })).toEqual({ 'networks.solana': 'Tick at least one stablecoin, or set Solana to Off.' });
  });
});

describe('choosing All, Some or Off', () => {
  const payout = () => ({ base: [{ key: 'k', wallet_id: 'w_evm', share: '100' }], solana: [{ key: 'k2', wallet_id: 'w_sol', share: '100' }] });
  const start = () => ({ networks: prefillNetworks(payout(), ['base', 'solana']), payout: payout() });

  it('a new product takes every network it can pay with All', () => {
    expect(start().networks).toEqual({ base: { accept: 'all', assets: [] }, solana: { accept: 'all', assets: [] } });
  });

  it('Off empties the payout, and taking the network again fills the default wallet', () => {
    const off = chooseNetwork(start(), 'solana', 'off', wallets, settings(true));
    expect(off.payout.solana).toEqual([]);
    expect(off.networks.solana!.accept).toBe('off');
    const back = chooseNetwork(off, 'solana', 'all', wallets, settings(true));
    expect(back.payout.solana).toHaveLength(1);
    expect(back.payout.solana![0]!.wallet_id).toBe('w_sol');
  });

  it('Some starts with the stablecoins on in Settings ticked, and ticking toggles one', () => {
    const some = chooseNetwork(start(), 'solana', 'some', wallets, settings(true));
    expect(some.networks.solana).toEqual({ accept: 'some', assets: ['USDC', 'USDT'] });
    expect(toggleAsset(some.networks, 'solana', 'USDC').solana!.assets).toEqual(['USDT']);
  });

  it('a network with no wallet stays Off', () => {
    const next = chooseNetwork({ networks: {}, payout: { solana: [] } }, 'solana', 'all', [wallet('w_evm', 'evm')], settings(true));
    expect(next.networks.solana!.accept).toBe('off');
    expect(next.payout.solana).toEqual([]);
  });
});
