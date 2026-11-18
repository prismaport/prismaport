import { describe, expect, it } from 'vitest';
import { ApiError } from '../src/lib/api';
import { activeLinksIn, asksAboutLinks, changesMoney, decisionCopy, isLinksDecisionRequired, linksDialogTitle, openCheckoutsNote, ownAmountNote } from '../src/lib/productLinks';
import type { Link, Product } from '../src/lib/types';

const product = (live: number): Product => ({
  id: 'prod_1', name: 'Pay Acme Studio', description: null, price_type: 'open', amount: null, min_amount: '5.00', max_amount: '500.00', suggested_amounts: [], currency: 'USD',
  success_url: null, metadata: {}, active: true, stats: { opened: 0, paid: 0, open: 0, received: [], last_paid_at: null, conversion: null }, links: { live, total: live },
  created_at: '', updated_at: '',
});

const link = (name: string | null, amount: string | null, active = true): Link => ({
  id: `plink_${name ?? 'none'}`, product_id: 'prod_1', name, amount, active, live: active, unavailable_reason: active ? null : 'inactive', url: '', success_url: null, metadata: {},
  expires_at: null, max_payments: null, stats: { opened: 0, paid: 0, received: [], last_paid_at: null, conversion: null },
  product: { id: 'prod_1', name: 'Pay Acme Studio', price_type: 'open', amount: null, min_amount: '5.00', max_amount: '500.00', active: true },
  created_at: '', updated_at: '',
});

describe('links decision on a product edit', () => {
  it('asks only when the patch changes money and the product has live links', () => {
    expect(changesMoney({ amount: '30.00' })).toBe(true);
    expect(changesMoney({ min_amount: '6.00' })).toBe(true);
    expect(changesMoney({ max_amount: null })).toBe(true);
    expect(changesMoney({ payout: { base: [] } })).toBe(true);
    expect(changesMoney({ name: 'X', description: null, suggested_amounts: ['10.00'], success_url: null, metadata: {}, active: false })).toBe(false);
    expect(asksAboutLinks(product(2), { min_amount: '6.00' })).toBe(true);
    expect(asksAboutLinks(product(0), { min_amount: '6.00' })).toBe(false);
    expect(asksAboutLinks(product(2), { name: 'Renamed' })).toBe(false);
  });

  it("recognizes the server's question and the count it names", () => {
    const three = new ApiError(409, 'links_decision_required', 'This product has 3 active links. Choose whether to keep them active or deactivate them.', undefined, 'links');
    const one = new ApiError(409, 'links_decision_required', 'This product has 1 active link. Choose whether to keep it active or deactivate it.', undefined, 'links');
    expect(isLinksDecisionRequired(three)).toBe(true);
    expect(isLinksDecisionRequired(new ApiError(409, 'idempotency_conflict', 'x'))).toBe(false);
    expect(isLinksDecisionRequired(new Error('x'))).toBe(false);
    expect([activeLinksIn(three), activeLinksIn(one), activeLinksIn(new ApiError(409, 'links_decision_required', 'Choose.'))]).toEqual([3, 1, null]);
  });

  it('words the title and the choices for one link or many', () => {
    expect(linksDialogTitle(3)).toBe('What happens to the 3 active links?');
    expect(linksDialogTitle(1)).toBe('What happens to the active link?');
    expect(linksDialogTitle(null)).toBe('What happens to the active links?');
    expect(decisionCopy('keep', 3)).toEqual({ title: 'Keep them active', description: 'New checkouts use the new price.', confirm: 'Save and keep links' });
    expect(decisionCopy('deactivate', 3)).toEqual({
      title: 'Deactivate them',
      description: 'Nobody can open a new checkout from them until you turn each one back on.',
      confirm: 'Save and deactivate links',
    });
    expect(decisionCopy('deactivate', 1).confirm).toBe('Save and deactivate link');
    expect(decisionCopy('keep', 1).title).toBe('Keep it active');
  });

  it('names up to three active links that set their own amount', () => {
    expect(ownAmountNote([link(null, null), link('Off', '9.00', false)], 'Pay Acme Studio')).toBeNull();
    expect(ownAmountNote([link('Invoice ACME #12', '120'), link(null, null)], 'Pay Acme Studio')).toBe('A link that sets its own amount keeps it: Invoice ACME #12 (120.00 USD).');
    expect(ownAmountNote([link('A', '1.00'), link(null, '2.50')], 'Pay Acme Studio')).toBe('Links that set their own amount keep it: A (1.00 USD) and Pay Acme Studio (2.50 USD).');
    expect(ownAmountNote([link('A', '1.00'), link('B', '2.00'), link('C', '3.00')], 'P')).toBe('Links that set their own amount keep it: A (1.00 USD), B (2.00 USD) and C (3.00 USD).');
    expect(ownAmountNote(['A', 'B', 'C', 'D', 'E'].map((n) => link(n, '1.00')), 'P')).toBe('Links that set their own amount keep it: A (1.00 USD), B (1.00 USD), C (1.00 USD) and 2 more.');
  });

  it('says how many checkouts already open keep the old price', () => {
    expect(openCheckoutsNote(0)).toBeNull();
    expect(openCheckoutsNote(1)).toBe('1 checkout already open keeps the old price and payout either way. Its payment address is tied to it until it expires.');
    expect(openCheckoutsNote(2)).toBe('2 checkouts already open keep the old price and payout either way. Their payment address is tied to them until they expire.');
    for (const n of [1, 2]) expect(openCheckoutsNote(n)).not.toMatch(/—/);
  });
});
