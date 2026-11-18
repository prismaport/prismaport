// The fee and split rule of contracts/src/FeeLib.sol and SplitLib.sol, in integers: every
// division floors, exactly as the EVM does on uint256.

const BPS_DENOMINATOR = 10_000n;
export const MAX_FEE_BPS = 300;
export const MAX_FIXED_FEE_MICROS = 5_000_000n;
const MIN_DECIMALS = 6;
const MAX_DECIMALS = 36;
export const MAX_RECIPIENTS = 5;
export const BPS_TOTAL = 10_000;

/** A custom error the contracts revert with, and its arguments as they would be logged. */
export interface Revert {
  error: 'FeeBpsTooHigh' | 'FixedFeeTooHigh' | 'UnsupportedDecimals' | 'InvalidRecipientCount' | 'ZeroShare' | 'InvalidShareSum';
  args: string[];
}

export const revertText = (r: Revert): string => `${r.error}(${r.args.join(', ')})`;

/** FeeLib.bpsOf: floor(amount * bps / 10_000) in the form that cannot overflow on-chain. */
export const bpsOf = (amount: bigint, bps: bigint): bigint =>
  (amount / BPS_DENOMINATOR) * bps + ((amount % BPS_DENOMINATOR) * bps) / BPS_DENOMINATOR;

/** FeeLib.checkParams. */
function checkFeeParams(feeBps: number, fixedFeeMicros: bigint): Revert | null {
  if (feeBps > MAX_FEE_BPS) return { error: 'FeeBpsTooHigh', args: [String(feeBps)] };
  if (fixedFeeMicros > MAX_FIXED_FEE_MICROS) return { error: 'FixedFeeTooHigh', args: [String(fixedFeeMicros)] };
  return null;
}

/** FeeLib.fixedFee: the fixed part in the token's base units. */
export function fixedFee(fixedFeeMicros: bigint, decimals: number): bigint | Revert {
  if (decimals < MIN_DECIMALS || decimals > MAX_DECIMALS) return { error: 'UnsupportedDecimals', args: [String(decimals)] };
  return fixedFeeMicros * 10n ** BigInt(decimals - MIN_DECIMALS);
}

export interface FeeResult {
  settles: boolean;
  /** Zero when the payment is dust, as FeeLib.compute returns. */
  fee: bigint;
  net: bigint;
  /** The parts of the fee, also when it is dust, so a page can say what the fee would have been. */
  fixed: bigint;
  pct: bigint;
}

/** FeeLib.compute. */
export function computeFee(gross: bigint, decimals: number, feeBps: number, fixedFeeMicros: bigint): FeeResult | Revert {
  const bad = checkFeeParams(feeBps, fixedFeeMicros);
  if (bad) return bad;
  const fixed = fixedFee(fixedFeeMicros, decimals);
  if (typeof fixed !== 'bigint') return fixed;
  const pct = bpsOf(gross, BigInt(feeBps));
  const fee = fixed + pct;
  if (fee >= gross) return { settles: false, fee: 0n, net: 0n, fixed, pct };
  return { settles: true, fee, net: gross - fee, fixed, pct };
}

/**
 * SplitLib.check for the shares, in its order (count, then each share left to right, then the sum).
 * Addresses are not modelled: the page has no recipient addresses.
 */
export function checkSplit(bps: readonly number[]): Revert | null {
  const n = bps.length;
  if (n === 0 || n > MAX_RECIPIENTS) return { error: 'InvalidRecipientCount', args: [String(n)] };
  let sum = 0;
  for (let i = 0; i < n; i++) {
    if (bps[i] === 0) return { error: 'ZeroShare', args: [String(i)] };
    sum += bps[i]!;
  }
  if (sum !== BPS_TOTAL) return { error: 'InvalidShareSum', args: [String(sum)] };
  return null;
}

/** SplitLib.split: every share after the first is floored, the first takes the rest. */
export function split(net: bigint, bps: readonly number[]): bigint[] {
  const shares = bps.map(() => 0n);
  let rest = net;
  for (let i = 1; i < bps.length; i++) {
    const s = bpsOf(net, BigInt(bps[i]!));
    shares[i] = s;
    rest -= s;
  }
  shares[0] = rest;
  return shares;
}

export interface SettleInput {
  gross: bigint;
  decimals: number;
  feeBps: number;
  fixedFeeMicros: bigint;
  bps: readonly number[];
}

export type Outcome =
  | { kind: 'revert'; revert: Revert }
  | { kind: 'dust'; gross: bigint; fixed: bigint; pct: bigint; wouldBeFee: bigint }
  | {
      kind: 'settled';
      gross: bigint;
      fixed: bigint;
      pct: bigint;
      fee: bigint;
      net: bigint;
      shares: bigint[];
      /** What rounding gave the first recipient above its exact share: at most n - 1. */
      remainder: bigint;
    };

/**
 * One settlement, with the checks in Factory.encodeArgs order (fee parameters, then the split)
 * and the fee rule of FeeLib.compute.
 */
export function settle(input: SettleInput): Outcome {
  const params = checkFeeParams(input.feeBps, input.fixedFeeMicros);
  if (params) return { kind: 'revert', revert: params };
  const shape = checkSplit(input.bps);
  if (shape) return { kind: 'revert', revert: shape };
  const r = computeFee(input.gross, input.decimals, input.feeBps, input.fixedFeeMicros);
  if ('error' in r) return { kind: 'revert', revert: r };
  if (!r.settles) return { kind: 'dust', gross: input.gross, fixed: r.fixed, pct: r.pct, wouldBeFee: r.fixed + r.pct };
  const shares = split(r.net, input.bps);
  return {
    kind: 'settled',
    gross: input.gross,
    fixed: r.fixed,
    pct: r.pct,
    fee: r.fee,
    net: r.net,
    shares,
    remainder: shares[0]! - bpsOf(r.net, BigInt(input.bps[0]!)),
  };
}

/**
 * A decimal string to base units, with the syntax of internal/money.ParseDecimal: digits, no
 * leading zero, an optional point followed by 1 to `decimals` digits. Null when it does not parse.
 */
export function parseDecimal(s: string, decimals: number): bigint | null {
  const m = /^(0|[1-9]\d*)(?:\.(\d+))?$/.exec(s);
  if (!m) return null;
  const frac = m[2] ?? '';
  if (frac.length > decimals) return null;
  return BigInt(m[1]! + frac.padEnd(decimals, '0'));
}

/** A share on the wire ("70.00" is 7000 bps): above zero and at most 100. internal/payout.ParseShare. */
export function parseShare(s: string): number | null {
  const u = parseDecimal(s, 2);
  if (u === null || u === 0n || u > BigInt(BPS_TOTAL)) return null;
  return Number(u);
}

const group = (digits: string): string => digits.replace(/\B(?=(\d{3})+(?!\d))/g, ',');

/** Base units as a decimal with at least two fraction digits and no trailing zeros beyond them. */
export function formatUnits(u: bigint, decimals: number): string {
  const neg = u < 0n;
  const abs = neg ? -u : u;
  const scale = 10n ** BigInt(decimals);
  const frac = (abs % scale).toString().padStart(decimals, '0').replace(/0+$/, '').padEnd(2, '0');
  return `${neg ? '-' : ''}${group((abs / scale).toString())}.${frac}`;
}

/** Base units as the contract sees them, grouped for reading. */
export const formatBaseUnits = (u: bigint): string => (u < 0n ? '-' : '') + group((u < 0n ? -u : u).toString());

/** Basis points as a percentage with two decimals: 50 is "0.50%". */
export const formatBps = (bps: number): string => `${Math.floor(bps / 100)}.${String(bps % 100).padStart(2, '0')}%`;
