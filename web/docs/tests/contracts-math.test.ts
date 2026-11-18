import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  MAX_FEE_BPS,
  MAX_FIXED_FEE_MICROS,
  bpsOf,
  checkSplit,
  computeFee,
  formatBps,
  formatUnits,
  parseShare,
  settle,
  split,
  type FeeResult,
  type Revert,
} from '../src/contracts/math';

// The vectors the Solidity, Go and Rust tests run (contracts/test/vectors/gen_splits.py).
interface Vectors {
  splits: { name: string; net: string; bps: number[]; shares: string[] }[];
  settlements: { name: string; gross: string; decimals: number; feeBps: number; fixedFeeMicros: number; bps: number[]; dust: boolean; fee: string; net: string; shares: string[] }[];
  invalidBps: { name: string; bps: number[]; error: 'count' | 'zero_share' | 'sum' }[];
  shareStrings: { share: string; bps?: number; error?: boolean }[];
}
const vectors = JSON.parse(readFileSync(join(process.cwd(), '../../contracts/test/vectors/splits.json'), 'utf8')) as Vectors;

const fee = (gross: bigint, decimals: number, bps: number, micros: bigint): FeeResult => {
  const r = computeFee(gross, decimals, bps, micros);
  if ('error' in r) throw new Error(`unexpected revert ${r.error}`);
  return r;
};

describe('shared split vectors', () => {
  it('has every category', () => {
    expect(vectors.splits.length).toBeGreaterThan(10);
    expect(vectors.settlements.length).toBeGreaterThan(10);
    expect(vectors.invalidBps.length).toBeGreaterThan(3);
    expect(vectors.shareStrings.length).toBeGreaterThan(10);
  });

  it('splits', () => {
    for (const v of vectors.splits) {
      expect(checkSplit(v.bps), v.name).toBeNull();
      expect(split(BigInt(v.net), v.bps).map(String), v.name).toEqual(v.shares);
    }
  });

  it('settlements', () => {
    for (const v of vectors.settlements) {
      const o = settle({ gross: BigInt(v.gross), decimals: v.decimals, feeBps: v.feeBps, fixedFeeMicros: BigInt(v.fixedFeeMicros), bps: v.bps });
      if (v.dust) {
        expect(o.kind, v.name).toBe('dust');
        const r = fee(BigInt(v.gross), v.decimals, v.feeBps, BigInt(v.fixedFeeMicros));
        expect([r.settles, r.fee, r.net], v.name).toEqual([false, 0n, 0n]);
        continue;
      }
      if (o.kind !== 'settled') throw new Error(`${v.name}: ${o.kind}`);
      expect(String(o.fee), v.name).toBe(v.fee);
      expect(String(o.net), v.name).toBe(v.net);
      expect(o.shares.map(String), v.name).toEqual(v.shares);
      expect(o.fee + o.shares.reduce((a, b) => a + b, 0n), v.name).toBe(o.gross);
    }
  });

  it('invalid shares', () => {
    const kinds: Record<string, Revert['error']> = { count: 'InvalidRecipientCount', zero_share: 'ZeroShare', sum: 'InvalidShareSum' };
    for (const v of vectors.invalidBps) expect(checkSplit(v.bps)?.error, v.name).toBe(kinds[v.error]);
  });

  it('share strings', () => {
    for (const v of vectors.shareStrings) expect(parseShare(v.share), JSON.stringify(v.share)).toBe(v.error ? null : v.bps);
  });
});

describe('fee rule', () => {
  it('computes the page example: 30.00 USDC, 0.10 + 0.5%, 70/30', () => {
    const o = settle({ gross: 30_000_000n, decimals: 6, feeBps: 50, fixedFeeMicros: 100_000n, bps: [7000, 3000] });
    if (o.kind !== 'settled') throw new Error(o.kind);
    expect([o.fixed, o.pct, o.fee, o.net]).toEqual([100_000n, 150_000n, 250_000n, 29_750_000n]);
    expect(o.shares).toEqual([20_825_000n, 8_925_000n]);
    expect(formatUnits(o.fee, 6)).toBe('0.25');
    expect(formatUnits(o.shares[0]!, 6)).toBe('20.825');
  });

  it('is dust exactly when the fee reaches the gross', () => {
    // 0.10 + 0.5%: the fee of g is 100_000 + floor(g * 50 / 10_000).
    const boundary = (g: bigint) => fee(g, 6, 50, 100_000n);
    expect(boundary(100_000n).settles).toBe(false);
    expect(boundary(100_502n).settles).toBe(false);
    expect(boundary(100_503n)).toMatchObject({ settles: true, fee: 100_502n, net: 1n });
    expect(fee(1n, 6, 0, 0n)).toMatchObject({ settles: true, fee: 0n, net: 1n });
    expect(fee(0n, 6, 0, 0n).settles).toBe(false);
    const dust = settle({ gross: 50_000n, decimals: 6, feeBps: 50, fixedFeeMicros: 100_000n, bps: [10000] });
    expect(dust).toMatchObject({ kind: 'dust', wouldBeFee: 100_250n });
  });

  it('refuses parameters above the caps, and accepts the caps', () => {
    expect(computeFee(1_000_000n, 6, MAX_FEE_BPS + 1, 0n)).toEqual({ error: 'FeeBpsTooHigh', args: ['301'] });
    expect(computeFee(1_000_000n, 6, 0, MAX_FIXED_FEE_MICROS + 1n)).toEqual({ error: 'FixedFeeTooHigh', args: ['5000001'] });
    expect(fee(1_000_000_000n, 6, MAX_FEE_BPS, MAX_FIXED_FEE_MICROS)).toMatchObject({ settles: true, fee: 35_000_000n });
    expect(computeFee(1n, 5, 0, 0n)).toEqual({ error: 'UnsupportedDecimals', args: ['5'] });
    expect(computeFee(1n, 37, 0, 0n)).toEqual({ error: 'UnsupportedDecimals', args: ['37'] });
  });

  it('checks the fee before the split, as Factory.encodeArgs does', () => {
    const o = settle({ gross: 1n, decimals: 6, feeBps: 1000, fixedFeeMicros: 0n, bps: [5000] });
    expect(o).toEqual({ kind: 'revert', revert: { error: 'FeeBpsTooHigh', args: ['1000'] } });
    expect(settle({ gross: 1n, decimals: 6, feeBps: 0, fixedFeeMicros: 0n, bps: [0, 10000] })).toEqual({ kind: 'revert', revert: { error: 'ZeroShare', args: ['0'] } });
    expect(settle({ gross: 1n, decimals: 6, feeBps: 0, fixedFeeMicros: 0n, bps: [5000, 4000] })).toEqual({ kind: 'revert', revert: { error: 'InvalidShareSum', args: ['9000'] } });
  });

  it('scales the fixed part for an 18-decimal token', () => {
    const one = 10n ** 18n;
    const r = fee(30n * one, 18, 50, 100_000n);
    expect(r.fixed).toBe(one / 10n);
    expect(r.pct).toBe((30n * one * 50n) / 10_000n);
    expect(r.net).toBe(30n * one - one / 10n - (30n * one * 50n) / 10_000n);
    expect(formatUnits(r.fee, 18)).toBe('0.25');
  });

  it('bpsOf is floor(amount * bps / 10_000) for amounts past 2^256', () => {
    for (const a of [0n, 1n, 9_999n, 10_001n, 123_456_789n, 2n ** 255n + 12_345n, 2n ** 300n - 1n]) {
      for (const b of [0n, 1n, 50n, 300n, 3333n, 9999n, 10_000n]) expect(bpsOf(a, b)).toBe((a * b) / 10_000n);
    }
  });
});

describe('split rule', () => {
  it('gives the first recipient at most n - 1 base units above its exact share', () => {
    let seed = 7;
    const rnd = (n: number) => ((seed = (seed * 1_103_515_245 + 12_345) % 2_147_483_648), seed % n);
    for (let k = 0; k < 2000; k++) {
      const n = 1 + rnd(5);
      const bps: number[] = [];
      let left = 10_000;
      for (let i = 0; i < n - 1; i++) {
        const v = 1 + rnd(left - (n - 1 - i));
        bps.push(v);
        left -= v;
      }
      bps.push(left);
      const net = BigInt(rnd(2_000_000_000)) * BigInt(1 + rnd(1000));
      const shares = split(net, bps);
      expect(shares.reduce((a, b) => a + b, 0n)).toBe(net);
      const extra = shares[0]! - bpsOf(net, BigInt(bps[0]!));
      expect(extra >= 0n && extra <= BigInt(n - 1), `${net} ${bps}`).toBe(true);
    }
    const o = settle({ gross: 10n, decimals: 6, feeBps: 0, fixedFeeMicros: 0n, bps: [3334, 3333, 3333] });
    expect(o).toMatchObject({ kind: 'settled', shares: [4n, 3n, 3n], remainder: 1n });
  });

  it('allows 1 to 5 recipients', () => {
    expect(checkSplit([10000])).toBeNull();
    expect(checkSplit([2000, 2000, 2000, 2000, 2000])).toBeNull();
    expect(checkSplit([])?.error).toBe('InvalidRecipientCount');
    expect(checkSplit([2000, 2000, 2000, 2000, 1000, 1000])).toEqual({ error: 'InvalidRecipientCount', args: ['6'] });
  });
});

describe('formatting', () => {
  it('shows at least two decimals and no needless zeros', () => {
    expect(formatUnits(30_000_000n, 6)).toBe('30.00');
    expect(formatUnits(8_925_000n, 6)).toBe('8.925');
    expect(formatUnits(1n, 6)).toBe('0.000001');
    expect(formatUnits(1_234_567_000_000n, 6)).toBe('1,234,567.00');
    expect(formatBps(50)).toBe('0.50%');
    expect(formatBps(10000)).toBe('100.00%');
  });
});
