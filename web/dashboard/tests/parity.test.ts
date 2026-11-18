// The dashboard and the hosted checkout each carry their own copy of the money and naming helpers.
// The merchant and the payer must read the same numbers and names for one payment, so both
// copies run the same vectors here.
import { describe, expect, it } from 'vitest';
import { networkName as merchantNetworkName } from '../src/lib/format';
import * as merchant from '../src/lib/money';
import { CLIENT_REFERENCE as MERCHANT_REFERENCE } from '../src/lib/reference';

// Loaded through a glob, not a static import, so type-checking the dashboard alone (its Docker
// build stage has no checkout sources) does not need the checkout tree.
const lib = import.meta.glob('../../checkout/src/lib/{format,networks,reference}.ts', { eager: true });
const payer = lib['../../checkout/src/lib/format.ts'] as typeof merchant;
const { networkName: payerNetworkName } = lib['../../checkout/src/lib/networks.ts'] as { networkName: typeof merchantNetworkName };
const { CLIENT_REFERENCE: PAYER_REFERENCE } = lib['../../checkout/src/lib/reference.ts'] as { CLIENT_REFERENCE: RegExp };

/** The result of a call, or the message it threw, so a refusal is compared like a value. */
function outcome(fn: () => unknown): unknown {
  try {
    return { ok: fn() };
  } catch (e) {
    return { threw: e instanceof Error ? `${e.constructor.name}: ${e.message}` : String(e) };
  }
}

const AMOUNTS = ['0', '0.00', '1', '25.00', '14.999999', '0.1', '1000000.5', '007.10', ' 3.50 ', '1.0000001', '-1', '1e3', '', 'abc', '1.', '.5', '12.345678901234567890'];
const DECIMALS = [0, 2, 6, 18];

describe('money helpers agree between the dashboard and the checkout', () => {
  it('finds the checkout copies', () => {
    expect(Object.keys(lib).sort()).toEqual(['../../checkout/src/lib/format.ts', '../../checkout/src/lib/networks.ts', '../../checkout/src/lib/reference.ts']);
  });

  it('share one scale', () => {
    expect(merchant.SCALE).toBe(payer.SCALE);
  });

  it.each(AMOUNTS)('formatAmount(%j)', (v) => {
    expect(outcome(() => merchant.formatAmount(v))).toEqual(outcome(() => payer.formatAmount(v)));
  });

  it.each(AMOUNTS)('toUnits, formatExact and scaled of %j', (v) => {
    for (const d of DECIMALS) {
      expect(outcome(() => merchant.toUnits(v, d))).toEqual(outcome(() => payer.toUnits(v, d)));
      expect(outcome(() => merchant.formatExact(v, d))).toEqual(outcome(() => payer.formatExact(v, d)));
    }
    expect(outcome(() => merchant.scaled(v))).toEqual(outcome(() => payer.scaled(v)));
  });

  it('fromUnits', () => {
    for (const u of [0n, 1n, 25_000_000n, 10n ** 30n, -1n]) {
      for (const d of DECIMALS) expect(outcome(() => merchant.fromUnits(u, d))).toEqual(outcome(() => payer.fromUnits(u, d)));
    }
  });

  it('compare, add and subtract', () => {
    const pairs: [string, string][] = [['25.00', '24.999999'], ['10', '10.000000'], ['0.1', '0.2'], ['15.00', '25.00'], ['1', 'x']];
    for (const [a, b] of pairs) {
      expect(outcome(() => merchant.compareAmounts(a, b))).toEqual(outcome(() => payer.compareAmounts(a, b)));
      expect(outcome(() => merchant.subtractAmounts(a, b))).toEqual(outcome(() => payer.subtractAmounts(a, b)));
      expect(outcome(() => merchant.addAmounts(a, b))).toEqual(outcome(() => payer.addAmounts(a, b)));
    }
  });

  it('truncateMiddle', () => {
    for (const v of ['0x7a3f9c2d4e5b6a7c8d9e0f1a2b3c4d5e6f7ae91c', '9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin', 'short', '0x123456789']) {
      expect(merchant.truncateMiddle(v)).toBe(payer.truncateMiddle(v));
      expect(merchant.truncateMiddle(v, 6, 4)).toBe(payer.truncateMiddle(v, 6, 4));
    }
  });

  it('parseUsdInput', () => {
    for (const raw of ['10', '$10.50', '10.505', '10.500', '.5', '5.', '0', '0.00', '', 'ten', ' 7 ', '1,000', '1.000', '10,50', '10,5', ',5', '10,', '0,01', '1.000,50', '1,000.50', '10,505', '5.0000', '5.0001', '$7,25']) {
      expect(merchant.parseUsdInput(raw)).toEqual(payer.parseUsdInput(raw));
    }
  });
});

describe('names and references agree between the dashboard and the checkout', () => {
  it('names every network the same way', () => {
    for (const chain of ['base', 'base-sepolia', 'solana', 'solana-devnet', 'arbitrum', 'polygon', 'optimism', 'celo', 'bnb-chain']) {
      expect(merchantNetworkName(chain), chain).toBe(payerNetworkName(chain));
    }
  });

  it('accepts the same client references', () => {
    expect(MERCHANT_REFERENCE.source).toBe(PAYER_REFERENCE.source);
    expect(MERCHANT_REFERENCE.flags).toBe(PAYER_REFERENCE.flags);
  });
});
