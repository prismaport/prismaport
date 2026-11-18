import { describe, expect, it } from 'vitest';
import { fromLocalInput, toLocalInput, type FeeTerms } from '../src/lib/formFields';
import { createLinkBody, emptyLinkForm, formFromLink, linkPatch, linkPreview, rangeProblem, validateLink, type LinkFormState, type LinkProduct } from '../src/lib/linkForm';
import type { Link } from '../src/lib/types';

const now = Date.parse('2026-10-25T12:00:00Z');
const fixed: LinkProduct = { id: 'prod_f', name: 'Founder plan', price_type: 'fixed', amount: '25.00', min_amount: null, max_amount: null, active: true, description: 'Lifetime access.', metadata: { plan: 'founder' } };
const variable: LinkProduct = {
  id: 'prod_v', name: 'Pay Acme', price_type: 'open', amount: null, min_amount: '5.00', max_amount: '500.00', active: true, suggested_amounts: ['10.00', '25.00'], description: null, metadata: {},
};
const form = (patch: Partial<LinkFormState>, productId = variable.id): LinkFormState => ({ ...emptyLinkForm(productId), ...patch });

const link: Link = {
  id: 'plink_1',
  product_id: variable.id,
  name: 'Invoice ACME #12',
  amount: '120.00',
  active: true,
  live: true,
  unavailable_reason: null,
  url: 'https://prismaport.xyz/l/plink_1',
  success_url: null,
  metadata: { invoice: '12' },
  expires_at: null,
  max_payments: 1,
  stats: { opened: 0, paid: 0, received: [], last_paid_at: null, conversion: null },
  product: { id: variable.id, name: 'Pay Acme', price_type: 'open', amount: null, min_amount: '5.00', max_amount: '500.00', active: true },
  created_at: '2026-10-01T00:00:00Z',
  updated_at: '2026-10-01T00:00:00Z',
};

describe('link form validation', () => {
  it('needs a product, and a link with nothing else set is just the product', () => {
    expect(validateLink(emptyLinkForm(), { now }).errors.product_id).toBe('Choose the product this link sells, or add a new one.');
    const v = validateLink(form({}, fixed.id), { product: fixed, now });
    expect(v.errors).toEqual({});
    expect(createLinkBody(v.input!)).toEqual({ product_id: 'prod_f' });
  });

  it('lets the payer choose by default on a variable product, sending no amount', () => {
    const v = validateLink(form({ amount: '42' }), { product: variable, now });
    expect(v.errors).toEqual({});
    expect(v.input!.amount).toBeNull();
    expect(createLinkBody(v.input!)).toEqual({ product_id: 'prod_v' });
  });

  it('checks a set amount against the product range and the fee floor', () => {
    const at = (amount: string, fee?: FeeTerms) => validateLink(form({ amount_choice: 'set', amount }), { product: variable, now, fee });
    expect(at('').errors.amount).toBe('Enter the amount for this link, such as 120.00.');
    expect(at('4.99').errors.amount).toBe("Set at least 5.00 USD, the product's minimum.");
    expect(at('500.01').errors.amount).toBe("Set at most 500.00 USD, the product's maximum.");
    expect(at('12.3456').errors.amount).toBe('Use at most two decimals, such as 25.50.');
    expect(createLinkBody(at('120').input!)).toEqual({ product_id: 'prod_v', amount: '120.00' });
    expect(rangeProblem('1000000.00', { min_amount: '5.00', max_amount: null })).toBeNull();
  });

  it('never sends an amount for a fixed price product', () => {
    const v = validateLink(form({ amount_choice: 'set', amount: '10' }, fixed.id), { product: fixed, now });
    expect(v.errors).toEqual({});
    expect(v.input!.amount).toBeNull();
  });

  it('keeps the label optional, internal and within 200 characters', () => {
    expect(createLinkBody(validateLink(form({ name: '  Instagram bio ' }), { product: variable, now }).input!)).toEqual({ product_id: 'prod_v', name: 'Instagram bio' });
    expect(validateLink(form({ name: 'a'.repeat(201) }), { product: variable, now }).errors.name).toBe('Use at most 200 characters.');
  });

  it('requires an https return address, a future expiry and a whole limit', () => {
    const v = validateLink(form({ success_url: 'acme.com/thanks', expires_at: '2026-10-24T10:00', max_payments: '2.5' }), { product: variable, now });
    expect(v.errors.success_url).toMatch(/starts with https:\/\//);
    expect(v.errors.expires_at).toMatch(/future/);
    expect(v.errors.max_payments).toBe('Enter a whole number of payments, 1 or more.');
    const ok = validateLink(form({ success_url: 'https://acme.com/thanks', expires_at: '2026-11-01T10:00', max_payments: '30' }), { product: variable, now });
    expect(ok.errors).toEqual({});
    expect(ok.input).toMatchObject({ max_payments: 30, expires_at: fromLocalInput('2026-11-01T10:00'), success_url: 'https://acme.com/thanks' });
    expect(validateLink(form({ max_payments: '1000001' }), { product: variable, now }).errors.max_payments).toMatch(/at most 1,000,000/);
    expect(validateLink(form({ expires_at: '2037-01-01T00:00' }), { product: variable, now }).errors.expires_at).toMatch(/10 years/);
  });

  it("counts the product's metadata in the 20-key limit of every payment", () => {
    const many = Array.from({ length: 20 }, (_, i) => ({ key: `k${i}`, value: 'v' }));
    expect(validateLink(form({ metadata: many }, fixed.id), { product: fixed, now }).errors.metadata).toBe("With the product's 1 entry, a payment can carry at most 20. Remove some here or on the product.");
    // A key the link shares with the product replaces it, so it does not count twice.
    expect(validateLink(form({ metadata: [...many.slice(0, 19), { key: 'plan', value: 'pro' }] }, fixed.id), { product: fixed, now }).errors.metadata).toBeUndefined();
  });
});

describe('editing a link', () => {
  it('round-trips a link through the form', () => {
    const f = formFromLink(link);
    expect(f).toMatchObject({ product_id: 'prod_v', amount_choice: 'set', amount: '120.00', name: 'Invoice ACME #12', max_payments: '1', metadata: [{ key: 'invoice', value: '12' }] });
    expect(linkPatch(link, validateLink(f, { product: variable, now }).input!)).toEqual({});
  });

  it('sends only what changed, with null to clear, and never the product', () => {
    const f = formFromLink(link);
    const patch = linkPatch(link, validateLink({ ...f, amount_choice: 'payer', name: '', max_payments: '' }, { product: variable, now }).input!);
    expect(patch).toEqual({ amount: null, name: null, max_payments: null });
    expect(linkPatch(link, validateLink({ ...f, amount: '150' }, { product: variable, now }).input!)).toEqual({ amount: '150.00' });
    expect(linkPatch(link, validateLink({ ...f, product_id: 'prod_other' }, { product: variable, now }).input!)).not.toHaveProperty('product_id');
  });

  it('lets an edit keep an expiry already in the past', () => {
    const past = '2026-10-01T10:00:00Z';
    expect(validateLink(form({ expires_at: toLocalInput(past) }), { product: variable, now, originalExpiresAt: past }).errors.expires_at).toBeUndefined();
  });
});

describe('link preview', () => {
  it("shows the product's name, description and fixed price", () => {
    expect(linkPreview(fixed, form({}, fixed.id))).toMatchObject({ name: 'Founder plan', description: 'Lifetime access.', price_type: 'fixed', price: '25.00 USD' });
  });

  it('shows a set amount as a fixed price, and the range with suggestions when the payer chooses', () => {
    expect(linkPreview(variable, form({ amount_choice: 'set', amount: '120' }))).toMatchObject({ price_type: 'fixed', price: '120.00 USD' });
    expect(linkPreview(variable, form({}))).toMatchObject({ price_type: 'open', hint: 'Between 5.00 and 500.00 USD.', suggested: ['10.00', '25.00'], placeholder: '5.00' });
  });

  it('asks for a product before there is one', () => {
    expect(linkPreview(null, emptyLinkForm())).toMatchObject({ name: 'Choose a product', price: null });
  });
});
