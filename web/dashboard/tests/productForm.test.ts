import { describe, expect, it } from 'vitest';
import { bpsPercent, feeTermsText, fromLocalInput, hasMoreOptions, isMoreOption, smallestPrice, toLocalInput } from '../src/lib/formFields';
import {
  createProductBody,
  emptyProductForm,
  formFromProduct,
  productPatch,
  productPreview,
  recapLine,
  stepOfField,
  takenChains,
  validateProduct,
  type ProductFormState,
} from '../src/lib/productForm';
import type { Product } from '../src/lib/types';

const form = (patch: Partial<ProductFormState>): ProductFormState => ({ ...emptyProductForm(), ...patch });

const product: Product = {
  id: 'prod_1',
  name: 'Founder plan',
  description: 'Lifetime access.',
  price_type: 'fixed',
  amount: '25.00',
  min_amount: null,
  max_amount: null,
  suggested_amounts: [],
  currency: 'USD',
  active: true,
  success_url: null,
  metadata: { plan: 'founder' },
  stats: { opened: 0, paid: 0, open: 0, received: [], last_paid_at: null, conversion: null },
  links: { live: 1, total: 1 },
  created_at: '2026-10-01T00:00:00Z',
  updated_at: '2026-10-01T00:00:00Z',
};

describe('product form validation', () => {
  it('builds a fixed-price body with a description', () => {
    const v = validateProduct(form({ name: ' Founder plan ', description: ' Lifetime access. ', amount: '25' }));
    expect(v.errors).toEqual({});
    expect(createProductBody(v.input!)).toEqual({ name: 'Founder plan', description: 'Lifetime access.', price_type: 'fixed', amount: '25.00' });
  });

  it('asks for a name and a price with clear messages', () => {
    const v = validateProduct(form({}));
    expect(v.input).toBeNull();
    expect(v.errors.name).toBe('Give the product a name. Payers see it on the payment page.');
    expect(v.errors.amount).toBe('Enter the price, such as 25.00.');
  });

  it('keeps the description optional and within 500 characters', () => {
    expect(createProductBody(validateProduct(form({ name: 'x', amount: '5', description: '   ' })).input!)).not.toHaveProperty('description');
    expect(validateProduct(form({ name: 'x', amount: '5', description: 'a'.repeat(500) })).errors.description).toBeUndefined();
    expect(validateProduct(form({ name: 'x', amount: '5', description: 'a'.repeat(501) })).errors.description).toBe('Use at most 500 characters. Now 501.');
  });

  it('rejects malformed prices and more than two decimals', () => {
    expect(validateProduct(form({ name: 'x', amount: '2,500.00' })).errors.amount).toBe('Enter an amount in USD, such as 25.00.');
    // A lone separator before exactly three digits is ambiguous ("1.000" is a thousand in pt-BR).
    expect(validateProduct(form({ name: 'x', amount: '25.000' })).errors.amount).toBe('Enter an amount in USD, such as 25.00.');
    expect(validateProduct(form({ name: 'x', amount: '25.001' })).errors.amount).toBe('Enter an amount in USD, such as 25.00.');
    expect(validateProduct(form({ name: 'x', amount: '25.0001' })).errors.amount).toBe('Use at most two decimals, such as 25.50.');
    expect(validateProduct(form({ name: 'x', amount: '0' })).errors.amount).toBe('Enter an amount above zero.');
    expect(validateProduct(form({ name: 'x', amount: '25,50' })).errors.amount).toBeUndefined();
    expect(validateProduct(form({ name: 'x', amount: '25.0000' })).errors.amount).toBeUndefined();
  });

  it('keeps the price at or above the smallest amount that settles after the fee', () => {
    const fee = { fee_bps: 50, fixed_fee: '0.10' };
    expect(validateProduct(form({ name: 'x', amount: '0.10' }), { fee }).errors.amount).toBe(
      'The price must be at least 0.11 USD, the smallest amount that settles after the Prismaport fee of 0.5% + 0.10 USD.',
    );
    expect(validateProduct(form({ name: 'x', amount: '0.11' }), { fee }).errors.amount).toBeUndefined();
    expect(validateProduct(form({ name: 'x', price_type: 'open', min_amount: '0.05' }), { fee }).errors.min_amount).toMatch(/^The minimum must be at least 0\.11 USD/);
  });

  it('finds the smallest price that settles from the fee terms', () => {
    expect(smallestPrice({ fee_bps: 50, fixed_fee: '0.10' })).toBe('0.11');
    expect(smallestPrice({ fee_bps: 100, fixed_fee: '0.10' })).toBe('0.11');
    expect(smallestPrice({ fee_bps: 50, fixed_fee: '0.099499' })).toBe('0.10');
    expect(smallestPrice({ fee_bps: 50, fixed_fee: '0.0995' })).toBe('0.11');
    expect(smallestPrice({ fee_bps: 300, fixed_fee: '5.00' })).toBe('5.16');
    expect(smallestPrice({ fee_bps: 0, fixed_fee: '0' })).toBe('0.01');
    expect(feeTermsText({ fee_bps: 50, fixed_fee: '0.10' })).toBe('0.5% + 0.10 USD');
    expect(bpsPercent(0)).toBe('0%');
    expect(bpsPercent(300)).toBe('3%');
    expect(bpsPercent(5)).toBe('0.05%');
  });

  it('checks a variable price range and its suggested amounts', () => {
    const bad = validateProduct(form({ name: 'Tip jar', price_type: 'open', min_amount: '10', max_amount: '5', suggested: ['1', '20', ''] }));
    expect(bad.errors.max_amount).toBe('The maximum must be at least the minimum, 10.00 USD.');
    expect(bad.errors['suggested.0']).toBe('Suggest at least the minimum, 10.00 USD.');
    const dup = validateProduct(form({ name: 'Tip jar', price_type: 'open', min_amount: '5', max_amount: '100', suggested: ['10', '10.00', '200'] }));
    expect(dup.errors['suggested.1']).toBe('Each suggested amount must be different.');
    expect(dup.errors['suggested.2']).toBe('Suggest at most the maximum, 100.00 USD.');
    const ok = validateProduct(form({ name: 'Pay Acme', price_type: 'open', min_amount: '5', suggested: ['', '25', '10'] }));
    expect(ok.errors).toEqual({});
    expect(createProductBody(ok.input!)).toEqual({ name: 'Pay Acme', price_type: 'open', min_amount: '5.00', suggested_amounts: ['25.00', '10.00'] });
  });

  it('requires an https return address and checks metadata keys', () => {
    expect(validateProduct(form({ name: 'x', amount: '5', success_url: 'acme.com/thanks' })).errors.success_url).toMatch(/starts with https:\/\//);
    expect(validateProduct(form({ name: 'x', amount: '5', success_url: 'http://localhost:3000/ok' })).errors.success_url).toBeUndefined();
    const v = validateProduct(form({ name: 'x', amount: '5', metadata: [{ key: 'sku', value: 'A' }, { key: 'sku', value: 'B' }, { key: '', value: 'orphan' }, { key: '', value: '' }] }));
    expect(v.errors['metadata.1.key']).toBe('Each key must be different.');
    expect(v.errors['metadata.2.key']).toBe('Add a key for this value.');
    expect(v.errors['metadata.3.key']).toBeUndefined();
  });
});

describe('editing a product', () => {
  it('round-trips a product through the form', () => {
    const f = formFromProduct(product);
    expect(f.amount).toBe('25.00');
    expect(f.description).toBe('Lifetime access.');
    expect(f.metadata).toEqual([{ key: 'plan', value: 'founder' }]);
    expect(productPatch(product, validateProduct(f).input!)).toEqual({});
  });

  it('sends only what changed, and null to clear the description', () => {
    expect(productPatch(product, validateProduct({ ...formFromProduct(product), amount: '30' }).input!)).toEqual({ amount: '30.00' });
    expect(productPatch(product, validateProduct({ ...formFromProduct(product), description: '' }).input!)).toEqual({ description: null });
  });

  it('never sends the price type, which cannot change after creation', () => {
    const v = validateProduct({ ...formFromProduct(product), price_type: 'open', min_amount: '5' });
    expect(productPatch(product, v.input!)).not.toHaveProperty('price_type');
  });
});

describe('payer preview', () => {
  it('shows the name, the description and the fixed price as the payer reads them', () => {
    expect(productPreview(form({ name: 'Founder plan', description: 'Lifetime access.', amount: '25' }))).toMatchObject({ name: 'Founder plan', description: 'Lifetime access.', price: '25.00 USD', price_type: 'fixed' });
  });

  it('shows the range and the suggested amounts of a variable price', () => {
    const m = productPreview(form({ name: '', price_type: 'open', min_amount: '5', max_amount: '250', suggested: ['10', 'oops', '25'] }));
    expect(m.name).toBe('Your product name');
    expect(m.description).toBeNull();
    expect(m.hint).toBe('Between 5.00 and 250.00 USD.');
    expect(m.suggested).toEqual(['10.00', '25.00']);
    expect(m.placeholder).toBe('5.00');
    expect(productPreview(form({ price_type: 'open', min_amount: '1' })).hint).toBe('At least 1.00 USD.');
  });
});

describe('product drawer steps', () => {
  it('puts every payout key on the second step and every other field on the first', () => {
    for (const k of ['payout', 'payout.base.sum', 'payout.solana.0.wallet', 'payout.base.rows']) expect(stepOfField(k)).toBe('payout');
    for (const k of ['name', 'description', 'amount', 'suggested.1', 'success_url', 'metadata', 'metadata.0.key']) expect(stepOfField(k)).toBe('details');
  });

  it('keeps the return address and metadata under More options, open when they hold something', () => {
    expect(['success_url', 'metadata', 'metadata.2.value'].every(isMoreOption)).toBe(true);
    expect(['name', 'description', 'expires_at', 'payout'].some(isMoreOption)).toBe(false);
    expect(hasMoreOptions(form({}))).toBe(false);
    expect(hasMoreOptions(form({ metadata: [{ key: ' ', value: '' }] }))).toBe(false);
    expect(hasMoreOptions(form({ success_url: 'https://acme.com' }))).toBe(true);
  });

  it('offers the payer only the networks with at least one wallet', () => {
    const row = { key: 'r1', wallet_id: 'wal_1', share: '100' };
    expect(takenChains({ base: [row], solana: [], ethereum: [row] })).toEqual(['base', 'ethereum']);
    expect(takenChains({})).toEqual([]);
  });

  it('recaps the product on the payout step', () => {
    expect(recapLine(productPreview(form({ name: 'Founder plan', amount: '25' })))).toBe('Founder plan · 25.00 USD');
    expect(recapLine(productPreview(form({ name: 'Pay Acme', price_type: 'open', min_amount: '5', max_amount: '50' })))).toBe('Pay Acme · Between 5.00 and 50.00 USD');
  });

  it('converts expiry times between the input and the API', () => {
    expect(fromLocalInput(toLocalInput('2026-11-01T10:00:00Z'))).toBe('2026-11-01T10:00:00Z');
    expect(fromLocalInput('')).toBeNull();
  });
});
