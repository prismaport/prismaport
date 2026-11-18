import {
  amount,
  changedFields,
  checkAvailability,
  checkMetadata,
  checkSuccessUrl,
  FIXED_HINT,
  feeProblem,
  META_MAX,
  metaRows,
  NAME_MAX,
  rangeHint,
  toLocalInput,
  typedAmount,
  type FeeTerms,
  type FieldErrors,
  type MetaRow,
  type PreviewModel,
} from './formFields';
import { compareAmounts, formatAmount } from './money';
import type { Link, LinkInput, LinkPatch, Product } from './types';

/** On a variable product: the payer types the amount, or the link sets one for every payer. */
export type AmountChoice = 'payer' | 'set';

export interface LinkFormState {
  product_id: string;
  amount_choice: AmountChoice;
  amount: string;
  /** The internal label; empty shows the product name. */
  name: string;
  /** A `datetime-local` value in the browser's time zone, or ''. */
  expires_at: string;
  max_payments: string;
  success_url: string;
  metadata: MetaRow[];
}

/** What the link form needs to know about the product it sells. */
export type LinkProduct = Pick<Product, 'id' | 'name' | 'price_type' | 'amount' | 'min_amount' | 'max_amount' | 'active'> &
  Partial<Pick<Product, 'description' | 'suggested_amounts' | 'metadata' | 'success_url'>>;

export function emptyLinkForm(productId = ''): LinkFormState {
  return { product_id: productId, amount_choice: 'payer', amount: '', name: '', expires_at: '', max_payments: '', success_url: '', metadata: [] };
}

export function formFromLink(l: Link): LinkFormState {
  return {
    product_id: l.product_id,
    amount_choice: l.amount ? 'set' : 'payer',
    amount: l.amount ? formatAmount(l.amount) : '',
    name: l.name ?? '',
    expires_at: toLocalInput(l.expires_at),
    max_payments: l.max_payments === null ? '' : String(l.max_payments),
    success_url: l.success_url ?? '',
    metadata: metaRows(l.metadata),
  };
}

export interface ValidateLinkOptions {
  /** The product the link sells, once known. */
  product?: LinkProduct | null;
  fee?: FeeTerms | null;
  now?: number;
  /** When editing, an expiry already in the past may stay as it is. */
  originalExpiresAt?: string | null;
}

export interface ValidatedLink {
  errors: FieldErrors;
  input: LinkInput | null;
}

/** Why an amount does not fit the product's range, or null when it does. */
export function rangeProblem(value: string, p: Pick<Product, 'min_amount' | 'max_amount'>): string | null {
  if (p.min_amount && compareAmounts(value, p.min_amount) < 0) return `Set at least ${formatAmount(p.min_amount)} USD, the product's minimum.`;
  if (p.max_amount && compareAmounts(value, p.max_amount) > 0) return `Set at most ${formatAmount(p.max_amount)} USD, the product's maximum.`;
  return null;
}

/** Checks the link drawer and builds the request body. */
export function validateLink(f: LinkFormState, opts: ValidateLinkOptions = {}): ValidatedLink {
  const errors: FieldErrors = {};
  const now = opts.now ?? Date.now();
  const product = opts.product ?? null;
  if (!f.product_id) errors.product_id = 'Choose the product this link sells, or add a new one.';
  const input: LinkInput = { product_id: f.product_id };

  if (product?.price_type === 'open' && f.amount_choice === 'set') {
    const a = amount(f.amount, 'Enter the amount for this link, such as 120.00.');
    if ('error' in a) errors.amount = a.error;
    else {
      const p = rangeProblem(a.value, product) ?? feeProblem(a.value, opts.fee, 'The amount');
      if (p) errors.amount = p;
      else input.amount = a.value;
    }
  }
  input.amount ??= null;

  const name = f.name.trim();
  if (name.length > NAME_MAX) errors.name = `Use at most ${NAME_MAX} characters.`;
  input.name = name || null;

  const availability = checkAvailability(f, errors, { now, originalExpiresAt: opts.originalExpiresAt });
  input.expires_at = availability.expires_at;
  input.max_payments = availability.max_payments;

  input.success_url = checkSuccessUrl(f.success_url, errors);
  const metadata = checkMetadata(f.metadata, errors);
  // Every payment carries the product's metadata with the link's merged over it.
  const merged = new Set([...Object.keys(product?.metadata ?? {}), ...Object.keys(metadata)]);
  if (!errors.metadata && merged.size > META_MAX) {
    const own = Object.keys(product?.metadata ?? {}).length;
    errors.metadata = `With the product's ${own} ${own === 1 ? 'entry' : 'entries'}, a payment can carry at most ${META_MAX}. Remove some here or on the product.`;
  }
  input.metadata = metadata;

  return { errors, input: Object.keys(errors).length === 0 ? input : null };
}

/** Only what changed; `null` clears a field. The product cannot change after creation. */
export function linkPatch(original: Link, next: LinkInput): LinkPatch {
  const { product_id: _p, ...fields } = next;
  const before = {
    name: original.name,
    amount: original.amount,
    success_url: original.success_url,
    metadata: original.metadata ?? {},
    expires_at: original.expires_at,
    max_payments: original.max_payments,
  };
  return changedFields(before, fields) as LinkPatch;
}

/** The create body without empty optional fields: a link with no options is `{product_id}`. */
export function createLinkBody(input: LinkInput): LinkInput {
  const out: LinkInput = { product_id: input.product_id };
  if (input.name) out.name = input.name;
  if (input.amount) out.amount = input.amount;
  if (input.expires_at) out.expires_at = input.expires_at;
  if (input.max_payments !== null && input.max_payments !== undefined) out.max_payments = input.max_payments;
  if (input.success_url) out.success_url = input.success_url;
  if (input.metadata && Object.keys(input.metadata).length > 0) out.metadata = input.metadata;
  return out;
}

/** The order the drawer checks and focuses its fields in. */
export const LINK_FIELD_ORDER = ['product_id', 'amount', 'name', 'expires_at', 'max_payments', 'success_url'];

/** What the payer sees on the link page: the product's name and description, at the link's effective price. */
export function linkPreview(product: LinkProduct | null, f: LinkFormState): PreviewModel {
  const base = { name: product?.name ?? 'Choose a product', description: product?.description ?? null, suggested: [] as string[] };
  if (!product || product.price_type === 'fixed') {
    return { ...base, price_type: 'fixed', price: product?.amount ? `${formatAmount(product.amount)} USD` : null, hint: FIXED_HINT, placeholder: '0.00' };
  }
  if (f.amount_choice === 'set') {
    const v = typedAmount(f.amount);
    return { ...base, price_type: 'fixed', price: v ? `${formatAmount(v)} USD` : null, hint: FIXED_HINT, placeholder: '0.00' };
  }
  return {
    ...base,
    price_type: 'open',
    price: null,
    hint: rangeHint(product.min_amount, product.max_amount),
    suggested: (product.suggested_amounts ?? []).map((s) => formatAmount(s)),
    placeholder: product.min_amount ? formatAmount(product.min_amount) : '0.00',
  };
}

export { hasMoreOptions, isMoreOption } from './formFields';
