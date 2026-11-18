import { describe, expect, it } from 'vitest';
import { displayAmount, explorerUrl, formatConversion, formatRelative, linkPrice, linkTitle, networkName, productPrice, receivedList, usd } from '../src/lib/format';
import { formatAmount, truncateMiddle } from '../src/lib/money';
import { feePercent } from '../src/pages/PaymentDetail';
import { feeLine } from '../src/pages/Settings';

describe('amounts', () => {
  it('show two decimals and never round a non-zero digit away', () => {
    expect(formatAmount('25')).toBe('25.00');
    expect(formatAmount('25.000000')).toBe('25.00');
    expect(formatAmount('14.999999')).toBe('14.999999');
    expect(displayAmount('1234567.5')).toBe('1,234,567.50');
    expect(displayAmount('999.990000')).toBe('999.99');
    expect(usd('0.1')).toBe('0.10 USD');
  });

  it('describes a product price by its type', () => {
    expect(productPrice({ price_type: 'fixed', amount: '25.00', min_amount: null, max_amount: null })).toBe('25.00 USD');
    expect(productPrice({ price_type: 'open', amount: null, min_amount: '5.00', max_amount: '1500.00' })).toBe('Variable · 5.00 to 1,500.00 USD');
    expect(productPrice({ price_type: 'open', amount: null, min_amount: '1.00', max_amount: null })).toBe('Variable · from 1.00 USD');
  });

  it('shows the price a link offers the payer, and names a link by its label or its product', () => {
    const fixed = { id: 'prod_1', name: 'Founder plan', price_type: 'fixed' as const, amount: '25.00', min_amount: null, max_amount: null, active: true };
    const open = { ...fixed, name: 'Pay Acme', price_type: 'open' as const, amount: null, min_amount: '5.00', max_amount: '500.00' };
    expect(linkPrice({ amount: null, product: fixed })).toBe('25.00 USD');
    expect(linkPrice({ amount: null, product: open })).toBe('5.00 to 500.00 USD');
    expect(linkPrice({ amount: '120.00', product: open })).toBe('120.00 USD');
    expect(linkPrice({ amount: null, product: { ...open, max_amount: null } })).toBe('From 5.00 USD');
    expect(linkTitle({ name: 'Instagram bio', product: fixed })).toBe('Instagram bio');
    expect(linkTitle({ name: null, product: fixed })).toBe('Founder plan');
  });

  it('list received amounts per asset, exact, skipping zero', () => {
    expect(receivedList([{ asset: 'USDC', amount: '850.000000' }, { asset: 'USDT', amount: '0.000000' }, { asset: 'USDT', amount: '75.100001' }])).toEqual(['850.00 USDC', '75.100001 USDT']);
  });

  it('compute the fee share on exact units', () => {
    expect(feePercent({ gross: '25.000000', fee: '0.250000' })).toBe('1.00%');
    expect(feePercent({ gross: '5.000000', fee: '0.100000' })).toBe('2.00%');
    expect(feePercent({ gross: '0', fee: '0' })).toBeNull();
    expect(feeLine({ fee_bps: 50, fixed_fee: '0.10' })).toBe('Prismaport fee 0.5% + 0.10 USD per payment, split on-chain in the same transaction.');
    expect(feeLine({ fee_bps: 125, fixed_fee: '0.1' })).toContain('1.25% + 0.10 USD');
    expect(feeLine({ fee_bps: 100, fixed_fee: '0' })).toContain('1% + 0.00 USD');
  });
});

describe('conversion', () => {
  it('is paid over opened from the counts', () => {
    expect(formatConversion(0.375, 8, 3)).toBe('37.5%');
    expect(formatConversion(null, 4, 4)).toBe('100%');
    expect(formatConversion('0.9', 10, 3)).toBe('30%');
  });

  it('is a dash before anyone opened the link', () => {
    expect(formatConversion(null, 0, 0)).toBe('–');
    expect(formatConversion(0, 0, 0)).toBe('–');
  });
});

describe('times and ids', () => {
  const now = Date.parse('2026-10-25T12:00:00Z');
  it('read relative times in words', () => {
    expect(formatRelative(null, now)).toBe('Never');
    expect(formatRelative('2026-10-25T11:59:40Z', now)).toBe('Just now');
    expect(formatRelative('2026-10-25T11:15:00Z', now)).toBe('45 min ago');
    expect(formatRelative('2026-10-25T07:00:00Z', now)).toBe('5 h ago');
    expect(formatRelative('2026-10-24T12:00:00Z', now)).toBe('1 day ago');
    expect(formatRelative('2026-10-20T12:00:00Z', now)).toBe('5 days ago');
  });

  it('shorten addresses and ids in the middle', () => {
    expect(truncateMiddle('0x7a3F5c2b9E81d4A6f0C3e2B7a915D8c4F06Be91c')).toBe('0x7a3F…e91c');
    expect(truncateMiddle('9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin')).toBe('9xQe…VFin');
    expect(truncateMiddle('short')).toBe('short');
  });

  it('name networks for people and link the right explorer', () => {
    expect(networkName('base')).toBe('Base');
    expect(networkName('solana-devnet')).toBe('Solana devnet');
    expect(explorerUrl('base', 'tx', '0xabc')).toBe('https://basescan.org/tx/0xabc');
    expect(explorerUrl('base-sepolia', 'tx', '0xabc')).toBe('https://sepolia.basescan.org/tx/0xabc');
    expect(explorerUrl('solana', 'tx', 'sig')).toBe('https://solscan.io/tx/sig');
    expect(explorerUrl('solana-devnet', 'address', 'acc')).toBe('https://solscan.io/account/acc?cluster=devnet');
    expect(explorerUrl('unknown', 'tx', 'x')).toBeNull();
  });
});
