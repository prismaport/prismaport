// What the product and the link forms share: amounts, return addresses, expiry, metadata.
import { compareAmounts, formatAmount, fromUnits, parseUsdInput, toUnits, type UsdInputError } from './money';

export interface MetaRow {
  key: string;
  value: string;
}

export type FieldErrors = Record<string, string>;

export const NAME_MAX = 200;
export const META_MAX = 20;
const META_KEY_MAX = 40;
const META_VALUE_MAX = 500;
const MAX_PAYMENTS_MAX = 1_000_000;
const TEN_YEARS_MS = 3653 * 86_400_000;

const pad = (n: number) => String(n).padStart(2, '0');

/** ISO instant to the value a `datetime-local` input shows, in the browser's time zone. */
export function toLocalInput(iso: string | null): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function fromLocalInput(value: string): string | null {
  if (!value) return null;
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? null : d.toISOString().replace(/\.\d{3}Z$/, 'Z');
}

const AMOUNT_ERRORS: Record<UsdInputError, string> = {
  empty: 'Enter an amount, such as 25.00.',
  format: 'Enter an amount in USD, such as 25.00.',
  precision: 'Use at most two decimals, such as 25.50.',
  zero: 'Enter an amount above zero.',
};

export function amount(raw: string, emptyMessage: string): { value: string } | { error: string } {
  const p = parseUsdInput(raw);
  if ('error' in p) return { error: p.error === 'empty' ? emptyMessage : AMOUNT_ERRORS[p.error] };
  return { value: p.value };
}

/** The value of a typed amount when it is valid, for previews. */
export function typedAmount(raw: string): string | null {
  const p = parseUsdInput(raw);
  return 'error' in p ? null : p.value;
}

/** The Prismaport fee of the accepted networks: a percentage of each payment plus a fixed part in USD. */
export interface FeeTerms {
  fee_bps: number;
  fixed_fee: string;
}

/** "0.5%": basis points as a percentage without trailing zeros. */
export function bpsPercent(feeBps: number): string {
  return `${fromUnits(BigInt(feeBps), 2).replace(/(\.\d*?)0+$/, '$1').replace(/\.$/, '')}%`;
}

/** "0.5% + 0.10 USD", the fee as merchants read it. */
export function feeTermsText(fee: FeeTerms): string {
  return `${bpsPercent(fee.fee_bps)} + ${fromUnits(toUnits(fee.fixed_fee, 2), 2)} USD`;
}

/**
 * The smallest price that settles: c cents pay fixed + c * fee_bps micros of fee, which must be
 * less than the c * 10 000 micros paid, so c * (10 000 - fee_bps) > fixed.
 */
export function smallestPrice(fee: FeeTerms): string {
  const fixedMicros = toUnits(fee.fixed_fee, 6);
  return fromUnits(fixedMicros / BigInt(10_000 - fee.fee_bps) + 1n, 2);
}

/** "The price must be at least 0.11 USD, ...", or null when the price settles after the fee. */
export function feeProblem(value: string, fee: FeeTerms | null | undefined, what: string): string | null {
  if (!fee) return null;
  const least = smallestPrice(fee);
  return compareAmounts(value, least) < 0
    ? `${what} must be at least ${least} USD, the smallest amount that settles after the Prismaport fee of ${feeTermsText(fee)}.`
    : null;
}

function isValidSuccessUrl(raw: string): boolean {
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return false;
  }
  if (u.protocol === 'https:') return true;
  return u.protocol === 'http:' && (u.hostname === 'localhost' || u.hostname === '127.0.0.1');
}

const SUCCESS_URL_ERROR = 'Enter a full address that starts with https://, such as https://acme.com/thanks.';

/** The return address field: a valid address, or null when it is empty. */
export function checkSuccessUrl(raw: string, errors: FieldErrors): string | null {
  const url = raw.trim();
  if (url && !isValidSuccessUrl(url)) errors.success_url = SUCCESS_URL_ERROR;
  return url || null;
}

/** Expiry and payment limit, as a link's Availability asks for them. */
export function checkAvailability(
  f: { expires_at: string; max_payments: string },
  errors: FieldErrors,
  opts: { now: number; originalExpiresAt?: string | null },
): { expires_at: string | null; max_payments: number | null } {
  let expires: string | null = null;
  if (f.expires_at) {
    const iso = fromLocalInput(f.expires_at);
    if (!iso) errors.expires_at = 'Enter a date and time.';
    else if (Date.parse(iso) <= opts.now && iso !== opts.originalExpiresAt) errors.expires_at = 'Choose a time in the future. After it the link stops opening payments.';
    else if (Date.parse(iso) - opts.now > TEN_YEARS_MS) errors.expires_at = 'Choose a time within the next 10 years, or leave it empty.';
    else expires = iso;
  }
  let max: number | null = null;
  const mp = f.max_payments.trim();
  if (mp) {
    if (!/^\d+$/.test(mp) || Number(mp) < 1 || !Number.isSafeInteger(Number(mp))) errors.max_payments = 'Enter a whole number of payments, 1 or more.';
    else if (Number(mp) > MAX_PAYMENTS_MAX) errors.max_payments = 'Use at most 1,000,000 payments, or leave it empty for no limit.';
    else max = Number(mp);
  }
  return { expires_at: expires, max_payments: max };
}

/** Metadata rows to an object; empty rows are skipped and every problem lands on its row. */
export function checkMetadata(rows: MetaRow[], errors: FieldErrors): Record<string, string> {
  const metadata: Record<string, string> = {};
  const filled = rows.filter((r) => r.key.trim() || r.value.trim());
  if (filled.length > META_MAX) errors.metadata = `Use at most ${META_MAX} metadata entries.`;
  rows.forEach((r, i) => {
    const key = r.key.trim();
    if (!key && !r.value.trim()) return;
    if (!key) errors[`metadata.${i}.key`] = 'Add a key for this value.';
    else if (key.length > META_KEY_MAX) errors[`metadata.${i}.key`] = `Keys can have at most ${META_KEY_MAX} characters.`;
    else if (key in metadata) errors[`metadata.${i}.key`] = 'Each key must be different.';
    if (r.value.length > META_VALUE_MAX) errors[`metadata.${i}.value`] = `Values can have at most ${META_VALUE_MAX} characters.`;
    if (key && !errors[`metadata.${i}.key`]) metadata[key] = r.value;
  });
  return metadata;
}

export const metaRows = (m: Record<string, string> | null | undefined): MetaRow[] => Object.entries(m ?? {}).map(([key, value]) => ({ key, value }));

/** Fields kept under "More options". */
export const isMoreOption = (key: string) => key === 'success_url' || key === 'metadata' || key.startsWith('metadata.');

export const hasMoreOptions = (f: { success_url: string; metadata: MetaRow[] }) => f.success_url.trim() !== '' || f.metadata.some((r) => r.key.trim() || r.value.trim());

const PRICE_FIELDS = ['amount', 'min_amount', 'max_amount', 'suggested_amounts'];

/** Only what changed between a stored object and the form's body, so an edit never rewrites fields the merchant did not touch. */
export function changedFields(before: Record<string, unknown>, next: Record<string, unknown>): Record<string, unknown> {
  const norm = (k: string, v: unknown) => {
    if (v === undefined) return null;
    if (PRICE_FIELDS.includes(k) && typeof v === 'string') return formatAmount(v);
    if (Array.isArray(v)) return v.map((x) => (typeof x === 'string' ? formatAmount(x) : x));
    return v;
  };
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(next)) {
    if (JSON.stringify(norm(k, v)) !== JSON.stringify(norm(k, before[k]))) out[k] = v;
  }
  return out;
}

/** What the payer sees on the link page, from whatever is typed so far. */
export interface PreviewModel {
  name: string;
  description: string | null;
  /** The price the payer meets: `fixed` once a product or link sets it. */
  price_type: 'fixed' | 'open';
  /** "25.00 USD" for a fixed price once a valid one is known. */
  price: string | null;
  hint: string;
  suggested: string[];
  placeholder: string;
}

export const FIXED_HINT = 'You choose the network and stablecoin on the next step. You send exactly this amount.';

export function rangeHint(min: string | null, max: string | null): string {
  if (min && max) return `Between ${formatAmount(min)} and ${formatAmount(max)} USD.`;
  if (min) return `At least ${formatAmount(min)} USD.`;
  return 'Any amount in USD.';
}
