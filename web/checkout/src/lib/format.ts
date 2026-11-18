// Amounts are decimal strings end to end. Arithmetic happens on bigint base units; a number is
// never converted to a float.

const DECIMAL = /^(\d+)(?:\.(\d+))?$/;

export class AmountError extends Error {}

function split(value: string): [string, string] {
  const m = DECIMAL.exec(value.trim());
  if (!m) throw new AmountError(`not a decimal amount: ${value}`);
  return [m[1] ?? '0', m[2] ?? ''];
}

/** Base units of a decimal string. Digits beyond `decimals` must be zeros, never rounded away. */
export function toUnits(value: string, decimals: number): bigint {
  const [int, frac] = split(value);
  const extra = frac.slice(decimals);
  if (/[1-9]/.test(extra)) throw new AmountError(`${value} has more than ${decimals} decimals`);
  const f = frac.slice(0, decimals).padEnd(decimals, '0');
  return BigInt(int + f);
}

/** The decimal string of base units, with exactly `decimals` places. */
export function fromUnits(units: bigint, decimals: number): string {
  if (units < 0n) throw new AmountError('negative amount');
  const s = units.toString().padStart(decimals + 1, '0');
  if (decimals === 0) return s;
  return `${s.slice(0, -decimals)}.${s.slice(-decimals)}`;
}

/**
 * An amount for people: two decimals, plus every non-zero digit beyond them, so 14.999999 is
 * never shown as 15.00.
 */
export function formatAmount(value: string): string {
  const [int, frac] = split(value);
  const trimmed = frac.replace(/0+$/, '');
  const places = trimmed.length > 2 ? trimmed : trimmed.padEnd(2, '0');
  return `${BigInt(int).toString()}.${places}`;
}

/** The exact on-chain amount, in the token's full decimals ("25.000000"). */
export function formatExact(value: string, decimals: number): string {
  return fromUnits(toUnits(value, decimals), decimals);
}

/** Common scale for comparing amounts with different decimals. */
export const SCALE = 18;

export const scaled = (value: string): bigint => toUnits(value, SCALE);

export function compareAmounts(a: string, b: string): number {
  const x = scaled(a);
  const y = scaled(b);
  return x === y ? 0 : x < y ? -1 : 1;
}

export function subtractAmounts(a: string, b: string): string {
  const d = scaled(a) - scaled(b);
  return fromUnits(d < 0n ? 0n : d, SCALE);
}

export function addAmounts(...xs: string[]): string {
  return fromUnits(xs.reduce((acc, x) => acc + scaled(x), 0n), SCALE);
}

/**
 * Shortens an address or hash in the middle: "0x7a3F…e91c" for EVM, "9xQe…k2Pq" otherwise.
 * The full value is always offered with a copy button next to it.
 */
export function truncateMiddle(value: string, head = 4, tail = 4): string {
  const prefix = value.startsWith('0x') ? '0x' : '';
  const body = value.slice(prefix.length);
  if (body.length <= head + tail + 1) return value;
  return `${prefix}${body.slice(0, head)}…${body.slice(-tail)}`;
}

/** A timestamp as the payer reads dates ("Nov 3, 2026, 2:32 PM"), in their locale and time zone. */
export function formatDateTime(iso: string): string | null {
  const t = Date.parse(iso);
  return Number.isNaN(t) ? null : new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(t);
}

/** "59:12" under an hour, "1:02:03" above, "0:00" once due. Seconds are rounded up. */
export function formatCountdown(ms: number): string {
  const total = Math.max(0, Math.ceil(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const ss = String(s).padStart(2, '0');
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${ss}` : `${m}:${ss}`;
}

export type UsdInputError = 'empty' | 'format' | 'precision' | 'zero';

/**
 * Parses what a payer typed as a USD amount (at most two decimals) into cents. A comma is the
 * decimal separator when it is the only separator ("10,50": comma-locale keypads show no dot);
 * text with both "." and "," or with several separators is refused, never guessed. So is a lone
 * separator, dot or comma, followed by exactly three digits: "1,000" is one thousand in en-US and
 * one in pt-BR, "1.000" the other way round. Only the payer's typing comes here; amounts from the
 * API are compared as they are.
 */
export function parseUsdInput(raw: string): { cents: bigint; value: string } | { error: UsdInputError } {
  let v = raw.trim().replace(/^\$/, '');
  if (v === '') return { error: 'empty' };
  if (/^[^.,]*[.,]\d{3}$/.test(v)) return { error: 'format' };
  if (/^[^.,]*,[^.,]*$/.test(v)) v = v.replace(',', '.');
  if (!/^\d+(\.\d*)?$|^\.\d+$/.test(v)) return { error: 'format' };
  const normalized = v.startsWith('.') ? `0${v}` : v.endsWith('.') ? v.slice(0, -1) : v;
  const frac = normalized.split('.')[1] ?? '';
  if (frac.length > 2 && /[1-9]/.test(frac.slice(2))) return { error: 'precision' };
  const cents = toUnits(normalized, 2);
  if (cents === 0n) return { error: 'zero' };
  return { cents, value: fromUnits(cents, 2) };
}
