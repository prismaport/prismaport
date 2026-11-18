// Shares are percentages with two decimals on the wire ("70.00") and basis points everywhere
// else (7000), so sums are exact integers, never floats.

export const MAX_RECIPIENTS = 5;
export const FULL = 10_000;

const SHARE = /^(\d{1,3})(?:\.(\d{0,2}))?$/;

/** Basis points of a typed or wire percentage ("70", "70.5", "33.33", "70.00%"), or null. */
export function parseShare(raw: string): number | null {
  const v = raw.trim().replace(/\s*%$/, '');
  const m = SHARE.exec(v);
  if (!m) return null;
  return Number(m[1]) * 100 + Number((m[2] ?? '').padEnd(2, '0'));
}

/** The wire form: "70.00". */
export function bpsToShare(bps: number): string {
  return `${Math.floor(bps / 100)}.${String(bps % 100).padStart(2, '0')}`;
}

/** For people: "70%", "12.5%", "33.33%". */
export function formatPercent(bps: number): string {
  const frac = bps % 100;
  if (frac === 0) return `${Math.floor(bps / 100)}%`;
  return `${Math.floor(bps / 100)}.${String(frac).padStart(2, '0').replace(/0$/, '')}%`;
}

/** n equal shares; the first takes what does not divide evenly (3 → 33.34, 33.33, 33.33). */
export function evenShares(n: number): string[] {
  if (n < 1) return [];
  const each = Math.floor(FULL / n);
  return Array.from({ length: n }, (_, i) => bpsToShare(i === 0 ? each + (FULL - each * n) : each));
}

/**
 * The on-chain rule: every recipient after the first gets floor(net * bps / 10 000) base units,
 * and the first gets the rest, so the shares always add up to net exactly.
 */
export function payoutUnits(net: bigint, bps: number[]): bigint[] {
  const out = bps.map((b, i) => (i === 0 ? 0n : (net * BigInt(b)) / BigInt(FULL)));
  if (out.length > 0) out[0] = net - out.slice(1).reduce((a, b) => a + b, 0n);
  return out;
}

export interface Share {
  walletId: string;
  label: string;
  bps: number;
}
