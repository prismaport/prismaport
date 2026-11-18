import { describe, expect, it } from 'vitest';
import { bpsToShare, evenShares, formatPercent, parseShare, payoutUnits } from '../src/lib/splits';
import { confirmCaseNote, confirmTailError, tailMatches, walletAddressError, walletLabelError } from '../src/lib/wallets';

describe('shares', () => {
  it('parses percentages with up to two decimals into basis points', () => {
    expect(parseShare('70')).toBe(7000);
    expect(parseShare('70.00')).toBe(7000);
    expect(parseShare('12.5')).toBe(1250);
    expect(parseShare('33.33')).toBe(3333);
    expect(parseShare(' 0.01 % ')).toBe(1);
    expect(parseShare('100')).toBe(10_000);
    for (const bad of ['', 'abc', '-5', '33.333', '1e2', '1,5', '1000']) expect(parseShare(bad), bad).toBeNull();
  });

  it('writes the wire form and the human form', () => {
    expect(bpsToShare(7000)).toBe('70.00');
    expect(bpsToShare(1)).toBe('0.01');
    expect(bpsToShare(10_000)).toBe('100.00');
    expect(formatPercent(7000)).toBe('70%');
    expect(formatPercent(1250)).toBe('12.5%');
    expect(formatPercent(3333)).toBe('33.33%');
    expect(formatPercent(5)).toBe('0.05%');
  });

  it('splits evenly with the remainder on the first share', () => {
    expect(evenShares(1)).toEqual(['100.00']);
    expect(evenShares(2)).toEqual(['50.00', '50.00']);
    expect(evenShares(3)).toEqual(['33.34', '33.33', '33.33']);
    expect(evenShares(5)).toEqual(['20.00', '20.00', '20.00', '20.00', '20.00']);
    for (let n = 1; n <= 5; n++) expect(evenShares(n).reduce((a, s) => a + parseShare(s)!, 0)).toBe(10_000);
  });

  it('pays out by the on-chain rule: floors for the others, remainder to the first, nothing lost', () => {
    expect(payoutUnits(24_750_000n, [7000, 3000])).toEqual([17_325_000n, 7_425_000n]);
    expect(payoutUnits(1_000_000n, [3334, 3333, 3333])).toEqual([333_400n, 333_300n, 333_300n]);
    expect(payoutUnits(1n, [5000, 5000])).toEqual([1n, 0n]);
    expect(payoutUnits(99n, [2000, 2000, 2000, 2000, 2000])).toEqual([23n, 19n, 19n, 19n, 19n]);
    for (const net of [0n, 1n, 7n, 999_999n, 123_456_789n]) {
      const parts = payoutUnits(net, [3333, 3333, 3334]);
      expect(parts.reduce((a, b) => a + b, 0n)).toBe(net);
    }
  });
});

describe('wallet form checks', () => {
  it('checks the address shape per family', () => {
    expect(walletAddressError('evm', '')).toBe('Paste your wallet address.');
    expect(walletAddressError('evm', '0x123')).toMatch(/not a Base wallet address/);
    expect(walletAddressError('evm', `0x${'0'.repeat(40)}`)).toMatch(/zero address/);
    expect(walletAddressError('evm', '0x7a3F5c2b9E81d4A6f0C3e2B7a915D8c4F06Be91c')).toBeNull();
    expect(walletAddressError('solana', '0x7a3F5c2b9E81d4A6f0C3e2B7a915D8c4F06Be91c')).toMatch(/not a Solana wallet address/);
    expect(walletAddressError('solana', '9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin')).toBeNull();
  });

  it('asks for the last 6 characters: any case for EVM, exact for Solana', () => {
    expect(confirmTailError('evm', '0x7a3F5c2b9E81d4A6f0C3e2B7a915D8c4F06Be91c', '')).toBe('Type the last 6 characters of the address to confirm it.');
    expect(confirmTailError('evm', '0x7a3F5c2b9E81d4A6f0C3e2B7a915D8c4F06Be91c', '06be91c')).toMatch(/not the last 6 characters/);
    expect(confirmTailError('evm', '0x7a3F5c2b9E81d4A6f0C3e2B7a915D8c4F06Be91c', '6be91c')).toBeNull();
    expect(confirmTailError('solana', '9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin', 'usvfin')).toMatch(/not the last 6 characters/);
    expect(confirmTailError('solana', '9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin', 'PusVFin'.slice(1))).toBeNull();
  });

  it('says how many characters are missing before it ever reports a mismatch', () => {
    const owner = '0xD544632988655aa58d0C15b925970526e93658A5';
    // The owner typed five characters: not a mismatch, one is missing.
    expect(confirmTailError('evm', owner, '658A5')).toBe('Type all 6 characters (5 of 6).');
    expect(confirmTailError('evm', owner, '3')).toBe('Type all 6 characters (1 of 6).');
    expect(confirmTailError('evm', owner, '3658A5')).toBeNull();
    expect(confirmTailError('evm', owner, '3658a5')).toBeNull();
    expect(confirmTailError('evm', owner, '3658A6')).toBe('These are not the last 6 characters of the address you pasted. Check the address.');
    expect(tailMatches('solana', '9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin', 'usVFin')).toBe(true);
    expect(tailMatches('solana', '9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin', 'usvfin')).toBe(false);
    expect(confirmCaseNote('evm')).toBe('Upper or lower case, either works.');
    expect(confirmCaseNote('solana')).toMatch(/case-sensitive/);
  });

  it('needs a label of at most 60 characters', () => {
    expect(walletLabelError(' ')).toMatch(/Give the wallet a name/);
    expect(walletLabelError('x'.repeat(61))).toBe('Use at most 60 characters.');
    expect(walletLabelError('Founder')).toBeNull();
  });
});
