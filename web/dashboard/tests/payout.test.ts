import { describe, expect, it } from 'vitest';
import { createProductBody, emptyProductForm, formFromProduct, productPatch, validateProduct } from '../src/lib/productForm';
import {
  addRow,
  addRowBlocker,
  copyRows,
  copySources,
  formFromPayout,
  formSummary,
  linkPayoutSummary,
  modeChains,
  NONE_KEY,
  payoutErrorKey,
  payoutLine,
  payoutPatch,
  pickable,
  prefillPayout,
  removeRow,
  rowKey,
  rowsKey,
  sharesOfRows,
  shownChains,
  sumKey,
  sumMessage,
  takeChain,
  totalBps,
  validatePayout,
  type PayoutForm,
  type PayoutRow,
} from '../src/lib/payout';
import type { LinkPayout, Product, Wallet } from '../src/lib/types';

const wallet = (id: string, label: string, family: Wallet['family'] = 'evm', created = '2026-09-01T00:00:00Z', archived = false): Wallet => ({
  id,
  label,
  family,
  address: family === 'evm' ? `0x${id.replace('wal_', '').padEnd(40, '0')}` : `${id.replace('wal_', '')}${'1'.repeat(40)}`,
  verified: true,
  archived_at: archived ? '2026-10-01T00:00:00Z' : null,
  created_at: created,
});

// Listed out of age order on purpose: the oldest active wallet is wal_a, not the first listed.
const W = [
  wallet('wal_b', 'Partner', 'evm', '2026-09-02T00:00:00Z'),
  wallet('wal_a', 'Founder', 'evm', '2026-09-01T00:00:00Z'),
  wallet('wal_c', 'Reserve', 'evm', '2026-09-03T00:00:00Z'),
  wallet('wal_d', 'Old', 'evm', '2026-08-01T00:00:00Z', true),
  wallet('wal_e', 'Fifth', 'evm', '2026-09-04T00:00:00Z'),
  wallet('wal_f', 'Sixth', 'evm', '2026-09-05T00:00:00Z'),
  wallet('wal_s', 'Treasury', 'solana'),
];

// Any chain ids the API sends; polygon stands for a second EVM network.
const CHAINS = ['base', 'polygon', 'solana'];

const rows = (list: [string, string][]): PayoutRow[] => list.map(([wallet_id, share], i) => ({ key: `k${i}`, wallet_id, share }));
const form = (base: [string, string][], solana: [string, string][] = [], polygon: [string, string][] = []): PayoutForm => ({ base: rows(base), polygon: rows(polygon), solana: rows(solana) });
const pairs = (r: PayoutRow[] | undefined) => (r ?? []).map((x) => [x.wallet_id, x.share]);

describe('networks and prefill', () => {
  it('lists the networks of the mode from the stablecoins, then any a stored payout names', () => {
    const accepted = [
      { chain: 'base', asset: 'USDC', enabled: true },
      { chain: 'solana', asset: 'USDC', enabled: true },
      { chain: 'solana', asset: 'USDT', enabled: false },
    ];
    expect(modeChains(accepted)).toEqual(['base', 'solana']);
    expect(modeChains(accepted, { base: [], polygon: [] })).toEqual(['base', 'solana', 'polygon']);
  });

  it('starts a new link with every network paying its family oldest active wallet at 100%', () => {
    const f = prefillPayout(W, CHAINS);
    expect(pairs(f.base)).toEqual([['wal_a', '100']]);
    expect(pairs(f.polygon)).toEqual([['wal_a', '100']]);
    expect(pairs(f.solana)).toEqual([['wal_s', '100']]);
  });

  it('leaves a network without an active wallet of its family empty, and shows only networks with wallets or rows', () => {
    const evmOnly = W.filter((w) => w.family === 'evm');
    expect(prefillPayout(evmOnly, CHAINS).solana).toEqual([]);
    expect(takeChain([wallet('wal_x', 'Gone', 'solana', undefined, true)], 'solana')).toEqual([]);
    expect(shownChains(evmOnly, prefillPayout(evmOnly, CHAINS), CHAINS)).toEqual(['base', 'polygon']);
    expect(shownChains([], { solana: rows([['wal_s', '100']]) }, CHAINS)).toEqual(['solana']);
    expect(shownChains([], {}, CHAINS)).toEqual([]);
  });

  it('reads a stored payout back per network, in its order, shares as typed', () => {
    const p: LinkPayout = { base: [{ wallet_id: 'wal_c', share: '12.50' }, { wallet_id: 'wal_a', share: '87.50' }], solana: [] };
    const f = formFromPayout(p, CHAINS);
    expect(pairs(f.base)).toEqual([['wal_c', '12.5'], ['wal_a', '87.5']]);
    expect(f.solana).toEqual([]);
    expect(f.polygon).toEqual([]);
    expect(formFromPayout(undefined)).toEqual({});
  });
});

describe('payout rows', () => {
  it('adds the oldest wallet not yet in the split with the share still unassigned, never re-sorting', () => {
    let r = rows([['wal_c', '70']]);
    r = addRow(r, W, 'base');
    expect(pairs(r)).toEqual([['wal_c', '70'], ['wal_a', '30']]);
    r = addRow(r, W, 'base');
    expect(pairs(r)).toEqual([['wal_c', '70'], ['wal_a', '30'], ['wal_b', '']]);
  });

  it('offers each row only active wallets of the network family that no other row took', () => {
    const r = rows([['wal_a', '50'], ['wal_b', '50']]);
    expect(pickable(r, 0, W, 'base').map((w) => w.id)).toEqual(['wal_a', 'wal_c', 'wal_e', 'wal_f']);
    expect(pickable(r, 0, W, 'polygon').map((w) => w.id)).toEqual(['wal_a', 'wal_c', 'wal_e', 'wal_f']);
    expect(pickable(r, 1, W, 'base').map((w) => w.id)).not.toContain('wal_a');
    expect(pickable([], 0, W, 'solana').map((w) => w.id)).toEqual(['wal_s']);
  });

  it('stops at five wallets, or when every wallet of the family is already in the split', () => {
    let r = rows([['wal_a', '20']]);
    for (let i = 0; i < 6; i++) r = addRow(r, W, 'base');
    expect(r).toHaveLength(5);
    expect(addRowBlocker(r, W, 'base')).toBe('A split pays at most 5 wallets.');
    expect(addRowBlocker(rows([['wal_s', '100']]), W, 'solana')).toBe('Every wallet that can receive on Solana is already in this split. Add a wallet in Settings to share with more.');
    expect(addRowBlocker(rows([['wal_a', '100']]), W, 'base')).toBeNull();
  });

  it('removes a row and keeps the others in order', () => {
    expect(removeRow(rows([['wal_a', '50'], ['wal_b', '30'], ['wal_c', '20']]), 1).map((x) => x.wallet_id)).toEqual(['wal_a', 'wal_c']);
  });

  it('copies another network of the same family, never one of the other family', () => {
    const f = form([['wal_a', '50'], ['wal_b', '50']], [['wal_s', '100']]);
    expect(copySources(f, 'polygon', CHAINS)).toEqual(['base']);
    expect(copySources(f, 'base', CHAINS)).toEqual([]);
    expect(copySources(f, 'solana', CHAINS)).toEqual([]);
    const copied = copyRows(f.base!);
    expect(pairs(copied)).toEqual(pairs(f.base));
    expect(copied.map((r) => r.key)).not.toEqual(f.base!.map((r) => r.key));
  });
});

describe('payout validation', () => {
  it('accepts the owner example and sends every network, shares with two decimals, in the order typed', () => {
    const v = validatePayout(form([['wal_b', '50'], ['wal_a', '50']], [], [['wal_a', '100']]), W);
    expect(v.errors).toEqual({});
    expect(v.input).toEqual({
      base: [{ wallet_id: 'wal_b', share: '50.00' }, { wallet_id: 'wal_a', share: '50.00' }],
      polygon: [{ wallet_id: 'wal_a', share: '100.00' }],
      solana: [],
    });
  });

  it('says what the shares add up to while they are not 100%', () => {
    const v = validatePayout(form([['wal_a', '60'], ['wal_b', '30']]), W);
    expect(v.errors[sumKey('base')]).toBe('Shares must add up to 100%. Now 90%.');
    expect(v.input).toBeNull();
    expect(sumMessage(totalBps(rows([['wal_a', '60.5'], ['wal_b', '40']])))).toBe('Shares must add up to 100%. Now 100.5%.');
    expect(sumMessage(10_000)).toBe('Shares add up to 100%.');
  });

  it('checks every share', () => {
    const v = validatePayout(form([['wal_a', '0'], ['wal_b', '33.333'], ['wal_c', '']]), W);
    expect(v.errors[rowKey('base', 0, 'share')]).toBe('Enter a share above 0%, or remove this wallet.');
    expect(v.errors[rowKey('base', 1, 'share')]).toBe('Enter a percentage with at most two decimals, such as 33.33.');
    expect(v.errors[rowKey('base', 2, 'share')]).toBe('Enter a share, such as 50.');
    expect(v.errors[sumKey('base')]).toBeUndefined();
    expect(validatePayout(form([['wal_a', '101']]), W).errors[rowKey('base', 0, 'share')]).toBe('Use at most 100%.');
  });

  it('refuses a wallet twice, a missing, archived or other-family wallet', () => {
    expect(validatePayout(form([['wal_a', '50'], ['wal_a', '50']]), W).errors[rowKey('base', 1, 'wallet')]).toBe('This wallet is already in this split. Choose another one.');
    expect(validatePayout(form([['', '100']]), W).errors[rowKey('base', 0, 'wallet')]).toBe('Choose a wallet.');
    expect(validatePayout(form([['wal_d', '100']]), W).errors[rowKey('base', 0, 'wallet')]).toBe('This wallet is archived. Choose another one.');
    expect(validatePayout(form([['wal_s', '100']]), W).errors[rowKey('base', 0, 'wallet')]).toBe('Choose a wallet that can receive on Base.');
    expect(validatePayout(form([], [['wal_a', '100']]), W).errors[rowKey('solana', 0, 'wallet')]).toBe('Choose a wallet that can receive on Solana.');
    // Without the wallet list only the shape is checked.
    expect(validatePayout(form([['wal_zz', '100']])).errors).toEqual({});
  });

  it('allows one to five wallets per network', () => {
    const six = form([['wal_a', '20'], ['wal_b', '20'], ['wal_c', '20'], ['wal_e', '20'], ['wal_f', '10'], ['wal_s', '10']]);
    expect(validatePayout(six).errors[rowsKey('base')]).toBe('Use at most 5 wallets on Base.');
    expect(validatePayout(form([['wal_a', '20'], ['wal_b', '20'], ['wal_c', '20'], ['wal_e', '20'], ['wal_f', '20']]), W).input!.base).toHaveLength(5);
  });

  it('lets a network be left out, but the link must take at least one', () => {
    const solanaOnly = validatePayout(form([], [['wal_s', '100']]), W);
    expect(solanaOnly.errors).toEqual({});
    expect(solanaOnly.input).toEqual({ base: [], polygon: [], solana: [{ wallet_id: 'wal_s', share: '100.00' }] });
    expect(validatePayout(form([]), W).errors[NONE_KEY]).toBe('Take payments on at least one network: give it at least one wallet.');
    expect(validatePayout(form([]), []).errors[NONE_KEY]).toBe('Add a wallet first, so payments have somewhere to go.');
  });
});

describe('payout on the link body', () => {
  const product: Product = {
    id: 'prod_1', name: 'Founder plan', description: null, price_type: 'fixed', amount: '25.00', min_amount: null, max_amount: null, suggested_amounts: [], currency: 'USD', active: true, success_url: null, metadata: {},
    payout: { base: [{ wallet_id: 'wal_a', share: '70.00', label: 'Founder' }, { wallet_id: 'wal_b', share: '30.00', label: 'Partner' }], solana: [{ wallet_id: 'wal_s', share: '100.00', label: 'Treasury' }] },
    stats: { opened: 0, paid: 0, open: 0, received: [], last_paid_at: null, conversion: null }, links: { live: 0, total: 0 }, created_at: '', updated_at: '',
  };

  it('creates with every network as chosen', () => {
    const f = { ...emptyProductForm('fixed'), name: 'Founder plan', amount: '25', payout: prefillPayout(W, ['base', 'solana']) };
    const payout = validatePayout(f.payout, W).input!;
    expect(createProductBody({ ...validateProduct(f).input!, payout }).payout).toEqual({ base: [{ wallet_id: 'wal_a', share: '100.00' }], solana: [{ wallet_id: 'wal_s', share: '100.00' }] });
  });

  it('patches only the networks that changed, and nothing when the payout is the same', () => {
    const f = formFromProduct(product);
    const same = validatePayout(f.payout, W).input!;
    expect(productPatch(product, { ...validateProduct(f).input!, payout: same })).toEqual({});
    const swapped = validatePayout({ ...f.payout, base: [f.payout.base![1]!, f.payout.base![0]!].map((r) => ({ ...r, share: '50' })) }, W).input!;
    expect(productPatch(product, { ...validateProduct(f).input!, payout: swapped })).toEqual({ payout: { base: [{ wallet_id: 'wal_b', share: '50.00' }, { wallet_id: 'wal_a', share: '50.00' }] } });
    expect(payoutPatch(product.payout, { base: same.base!, solana: [] })).toEqual({ solana: [] });
  });
});

describe('payout summaries', () => {
  it('reads "Base: 50% A · 50% B · Polygon: 100% A", leaving out networks not taken', () => {
    expect(payoutLine('base', sharesOfRows(rows([['wal_a', '70'], ['wal_b', '30']]), W))).toBe('Base: 70% Founder · 30% Partner');
    expect(payoutLine('solana', [])).toBe('');
    const f = form([['wal_a', '50'], ['wal_b', '50']], [], [['wal_a', '100']]);
    expect(formSummary(f, CHAINS, W)).toBe('Base: 50% Founder · 50% Partner · Polygon: 100% Founder');
    expect(linkPayoutSummary({ base: [{ wallet_id: 'wal_a', share: '50.00', label: 'Founder' }, { wallet_id: 'wal_b', share: '50.00' }], solana: [] }, W)).toBe('Base: 50% Founder · 50% Partner');
    expect(linkPayoutSummary(undefined)).toBe('');
  });
});

describe('server refusals land on their field', () => {
  const f = form([['wal_a', '60'], ['wal_b', '30']], [['wal_s', '100']]);
  const at = (message: string, code = 'invalid_request') => payoutErrorKey({ code, message }, f, W);

  it('reads a field path with a chain id', () => {
    expect(at('payout.base[1].share must be a percentage above 0 and at most 100 with up to two decimals, such as "70.00"')).toBe(rowKey('base', 1, 'share'));
    expect(at('payout.solana[0].wallet_id is required')).toBe(rowKey('solana', 0, 'wallet'));
  });

  it('finds the row of a wallet named by id, or by name when the issuer blacklisted it', () => {
    expect(at('Wallet wal_b is not an active EVM wallet of this account, so it cannot receive on Base.')).toBe(rowKey('base', 1, 'wallet'));
    expect(at('Partner is on the USDC blacklist and cannot receive payments.', 'wallet_blacklisted')).toBe(rowKey('base', 1, 'wallet'));
  });

  it('puts a sum, a count or a duplicate on its network, and a link that pays nothing above the networks', () => {
    expect(at('Shares must add up to 100%. Now 90%.')).toBe(sumKey('base'));
    expect(at('Shares must add up to 100% on Solana. Now 90%.')).toBe(sumKey('solana'));
    expect(at('A split pays 1 to 5 wallets.')).toBe(rowsKey('base'));
    expect(at('Each wallet can appear only once in a split.')).toBe(rowsKey('base'));
    expect(at('A link must take payments on at least one network.')).toBe(NONE_KEY);
  });

  it('tells a longer network name from a shorter one', () => {
    const t: PayoutForm = { 'base-sepolia': rows([['wal_a', '60']]), 'solana-devnet': rows([['wal_s', '100']]) };
    expect(payoutErrorKey({ code: 'invalid_request', message: 'Shares must add up to 100% on Base Sepolia. Now 60%.' }, t, W)).toBe(sumKey('base-sepolia'));
    expect(payoutErrorKey({ code: 'unknown_chain', message: 'payout.base-sepolia is not a network of this mode.' }, t, W)).toBe(rowsKey('base-sepolia'));
    expect(payoutErrorKey({ code: 'unknown_chain', message: 'payout.tron is not a network of this mode.' }, t, W)).toBeNull();
  });

  describe('by param first, whatever the message says', () => {
    const withParam = (param: string, message: string, code = 'invalid_request') => payoutErrorKey({ code, message, param }, f, W);

    it('"payout": the product pays no network', () => {
      expect(withParam('payout', 'A product must pay at least one network. Add a wallet first, or give one network of payout a wallet.')).toBe(NONE_KEY);
      expect(withParam('payout', 'Reworded refusal.')).toBe(NONE_KEY);
    });

    it('"payout.<chain>": the total of that network, even when the message names none', () => {
      expect(withParam('payout.solana', 'Shares must add up to 100%. Now 90%.')).toBe(sumKey('solana'));
    });

    it('"payout.<chain>": the recipient count of that network', () => {
      expect(withParam('payout.solana', 'A split pays 1 to 5 wallets.')).toBe(rowsKey('solana'));
      expect(withParam('payout.solana', 'payout has an unknown network', 'unknown_chain')).toBe(rowsKey('solana'));
    });

    it('"payout.<chain>[i].wallet_id": that row\'s wallet', () => {
      expect(withParam('payout.base[1].wallet_id', 'Each wallet can appear only once in a split.')).toBe(rowKey('base', 1, 'wallet'));
      expect(withParam('payout.base[0].wallet_id', 'The USDC issuer has blacklisted the wallet Partner (0xabc), so payments to it can never settle.', 'wallet_blacklisted'))
        .toBe(rowKey('base', 0, 'wallet'));
    });

    it('"payout.<chain>[i].share": that row\'s share', () => {
      expect(withParam('payout.solana[0].share', 'Reworded refusal.')).toBe(rowKey('solana', 0, 'share'));
    });

    it('a network or row the form does not show goes to its list or above the form', () => {
      expect(withParam('payout.base[7].share', 'Reworded refusal.')).toBe(rowsKey('base'));
      expect(withParam('payout.tron', 'payout has an unknown network "tron"', 'unknown_chain')).toBeNull();
    });

    it('a param that is not a payout path falls back to the message', () => {
      expect(withParam('name', 'Shares must add up to 100% on Solana. Now 90%.')).toBe(sumKey('solana'));
    });
  });

  it('leaves anything else to the message above the form', () => {
    expect(at('name must be 1 to 200 characters.')).toBeNull();
    expect(payoutErrorKey({ code: 'rate_limited', message: 'Shares must add up to 100%.' }, f, W)).toBeNull();
  });
});
