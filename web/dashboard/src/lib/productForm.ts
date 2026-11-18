import {
  amount,
  changedFields,
  checkMetadata,
  checkSuccessUrl,
  FIXED_HINT,
  feeProblem,
  metaRows,
  NAME_MAX,
  rangeHint,
  typedAmount,
  type FeeTerms,
  type FieldErrors,
  type MetaRow,
  type PreviewModel,
} from './formFields';
import { compareAmounts, formatAmount } from './money';
import { networksFormOf, networksOfProduct, networksPatch, type NetworksForm } from './networkAssets';
import { formFromPayout, payoutPatch, type PayoutForm } from './payout';
import type { PriceType, Product, ProductInput, ProductPatch } from './types';

export interface ProductFormState {
  name: string;
  description: string;
  price_type: PriceType;
  amount: string;
  min_amount: string;
  max_amount: string;
  suggested: [string, string, string];
  success_url: string;
  metadata: MetaRow[];
  /** Where each network's payments go; checked by validatePayout, not validateProduct. */
  payout: PayoutForm;
  /** Which stablecoins each network takes; a network left out is Off. */
  networks: NetworksForm;
}

export const DESCRIPTION_MAX = 500;

export function emptyProductForm(priceType: PriceType = 'fixed'): ProductFormState {
  return { name: '', description: '', price_type: priceType, amount: '', min_amount: '', max_amount: '', suggested: ['', '', ''], success_url: '', metadata: [], payout: {}, networks: {} };
}

export function formFromProduct(p: Product): ProductFormState {
  const s = p.suggested_amounts ?? [];
  return {
    name: p.name,
    description: p.description ?? '',
    price_type: p.price_type,
    amount: p.amount ? formatAmount(p.amount) : '',
    min_amount: p.min_amount ? formatAmount(p.min_amount) : '',
    max_amount: p.max_amount ? formatAmount(p.max_amount) : '',
    suggested: [s[0] ? formatAmount(s[0]) : '', s[1] ? formatAmount(s[1]) : '', s[2] ? formatAmount(s[2]) : ''],
    success_url: p.success_url ?? '',
    metadata: metaRows(p.metadata),
    payout: formFromPayout(p.payout),
    networks: networksFormOf(networksOfProduct(p), Object.keys(p.payout ?? {})),
  };
}

export interface ValidateProductOptions {
  /** The fee of the accepted networks: a price must be large enough to settle after it. */
  fee?: FeeTerms | null;
}

export interface ValidatedProduct {
  errors: FieldErrors;
  input: ProductInput | null;
}

/** Checks the product details and builds the request body. Every message says what to do. */
export function validateProduct(f: ProductFormState, opts: ValidateProductOptions = {}): ValidatedProduct {
  const errors: FieldErrors = {};
  const name = f.name.trim();
  if (!name) errors.name = 'Give the product a name. Payers see it on the payment page.';
  else if (name.length > NAME_MAX) errors.name = `Use at most ${NAME_MAX} characters.`;

  const description = f.description.trim();
  if (description.length > DESCRIPTION_MAX) errors.description = `Use at most ${DESCRIPTION_MAX} characters. Now ${description.length.toLocaleString('en-US')}.`;

  const input: ProductInput = { name, description: description || null, price_type: f.price_type };
  const aboveFee = (value: string, field: string, what: string) => {
    const p = feeProblem(value, opts.fee, what);
    if (p) errors[field] = p;
  };

  if (f.price_type === 'fixed') {
    const a = amount(f.amount, 'Enter the price, such as 25.00.');
    if ('error' in a) errors.amount = a.error;
    else {
      input.amount = a.value;
      aboveFee(a.value, 'amount', 'The price');
    }
  } else {
    const min = amount(f.min_amount, 'Enter the minimum amount, such as 5.00.');
    let minValue: string | null = null;
    if ('error' in min) errors.min_amount = min.error;
    else {
      minValue = min.value;
      input.min_amount = min.value;
      aboveFee(min.value, 'min_amount', 'The minimum');
    }
    let maxValue: string | null = null;
    if (f.max_amount.trim()) {
      const max = amount(f.max_amount, '');
      if ('error' in max) errors.max_amount = max.error;
      else if (minValue && compareAmounts(max.value, minValue) < 0) errors.max_amount = `The maximum must be at least the minimum, ${formatAmount(minValue)} USD.`;
      else maxValue = max.value;
    }
    input.max_amount = maxValue;
    const suggested: string[] = [];
    f.suggested.forEach((raw, i) => {
      if (!raw.trim()) return;
      const s = amount(raw, '');
      const key = `suggested.${i}`;
      if ('error' in s) errors[key] = s.error;
      else if (minValue && compareAmounts(s.value, minValue) < 0) errors[key] = `Suggest at least the minimum, ${formatAmount(minValue)} USD.`;
      else if (maxValue && compareAmounts(s.value, maxValue) > 0) errors[key] = `Suggest at most the maximum, ${formatAmount(maxValue)} USD.`;
      else if (suggested.some((x) => compareAmounts(x, s.value) === 0)) errors[key] = 'Each suggested amount must be different.';
      else suggested.push(s.value);
    });
    input.suggested_amounts = suggested;
  }

  input.success_url = checkSuccessUrl(f.success_url, errors);
  input.metadata = checkMetadata(f.metadata, errors);
  return { errors, input: Object.keys(errors).length === 0 ? input : null };
}

/** Only what changed. The price type cannot change after creation, so it is never sent. */
export function productPatch(original: Product, next: ProductInput): ProductPatch {
  const { payout, networks, price_type: _t, ...fields } = next;
  const before = {
    name: original.name,
    description: original.description,
    amount: original.amount,
    min_amount: original.min_amount,
    max_amount: original.max_amount,
    suggested_amounts: original.suggested_amounts ?? [],
    success_url: original.success_url,
    metadata: original.metadata ?? {},
  };
  const out = changedFields(before, fields) as ProductPatch;
  if (payout) {
    const changed = payoutPatch(original.payout, payout);
    if (changed) out.payout = changed;
  }
  if (networks) {
    const asked = networksPatch(networksOfProduct(original), networks);
    if (asked) out.networks = asked;
  }
  return out;
}

/** The create body without empty optional fields. */
export function createProductBody(input: ProductInput): ProductInput {
  const out: ProductInput = { name: input.name, price_type: input.price_type };
  if (input.description) out.description = input.description;
  if (input.price_type === 'fixed') out.amount = input.amount ?? null;
  else {
    out.min_amount = input.min_amount ?? null;
    if (input.max_amount) out.max_amount = input.max_amount;
    if (input.suggested_amounts && input.suggested_amounts.length > 0) out.suggested_amounts = input.suggested_amounts;
  }
  if (input.success_url) out.success_url = input.success_url;
  if (input.metadata && Object.keys(input.metadata).length > 0) out.metadata = input.metadata;
  // Without a payout the API gives each network its family's oldest active wallet at 100%.
  if (input.payout) out.payout = input.payout;
  if (input.networks) out.networks = input.networks;
  return out;
}

/** The drawer asks for the product first, then for where its payments go. */
export type ProductStep = 'details' | 'payout';

export const stepOfField = (key: string): ProductStep => (key === 'payout' || key.startsWith('payout.') || key.startsWith('networks.') ? 'payout' : 'details');

/** The networks the form pays on, which are the only ones the payer is offered. */
export const takenChains = (payout: PayoutForm) => Object.keys(payout).filter((c) => (payout[c]?.length ?? 0) > 0);

/** What a payer sees on a link of this product that sets no amount of its own. */
export function productPreview(f: ProductFormState): PreviewModel {
  const min = typedAmount(f.min_amount);
  const max = typedAmount(f.max_amount);
  const price = f.price_type === 'fixed' ? typedAmount(f.amount) : null;
  return {
    name: f.name.trim() || 'Your product name',
    description: f.description.trim() || null,
    price_type: f.price_type,
    price: price ? `${formatAmount(price)} USD` : null,
    hint: f.price_type === 'fixed' ? FIXED_HINT : rangeHint(min, max),
    suggested: f.price_type === 'open' ? f.suggested.map(typedAmount).filter((x): x is string => x !== null).map((x) => formatAmount(x)) : [],
    placeholder: min ? formatAmount(min) : '0.00',
  };
}

/** One line for the top of the payout step: "Founder plan · 25.00 USD". */
export function recapLine(m: PreviewModel): string {
  const price = m.price_type === 'fixed' ? m.price : m.hint.replace(/\.$/, '');
  return price ? `${m.name} · ${price}` : m.name;
}
