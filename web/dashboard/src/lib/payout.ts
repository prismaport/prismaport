// Where a product's money goes: per network (a chain id of the product's mode), 0 or 1 to 5 wallets of
// that network's family in fixed shares adding up to 100%, in payout order. The order is never
// changed here: it is committed on chain and the first wallet takes the rounding remainder.
import { networkName } from './format';
import { truncateMiddle } from './money';
import { chainFamily } from './logos';
import { bpsToShare, formatPercent, FULL, MAX_RECIPIENTS, parseShare, type Share } from './splits';
import type { AcceptedAsset, LinkPayout, PayoutInput, PayoutItem, Wallet } from './types';
import { activeWallets, defaultWallet, isActive } from './wallets';

export interface PayoutRow {
  /** Stable across edits, for React keys and focus. */
  key: string;
  wallet_id: string;
  /** As typed: "70", "33.33". */
  share: string;
}

/** Per chain id; an empty list means the product does not take payments on that network. */
export type PayoutForm = Record<string, PayoutRow[]>;

type Items = { wallet_id: string; share: string }[];

let rowSeq = 0;
const newKey = (): string => `payout-row-${++rowSeq}`;

const typed = (bps: number): string => formatPercent(bps).replace('%', '');

/**
 * The networks of the mode, in the order the API lists its stablecoins, then any other network a
 * stored payout names (an API that knows a network the stablecoin list does not).
 */
export function modeChains(accepted: AcceptedAsset[], ...payouts: (Record<string, unknown> | undefined)[]): string[] {
  const out = [...new Set(accepted.map((a) => a.chain))];
  for (const p of payouts) for (const k of Object.keys(p ?? {})) if (!out.includes(k)) out.push(k);
  return out;
}

/** One network taken at 100% by its family's default wallet, or not taken without one. */
export function takeChain(wallets: Wallet[], chain: string): PayoutRow[] {
  const first = defaultWallet(wallets, chainFamily(chain));
  return first ? [{ key: newKey(), wallet_id: first.id, share: '100' }] : [];
}

/** A new product: every network pays its family's default wallet. */
export function prefillPayout(wallets: Wallet[], chains: string[]): PayoutForm {
  return Object.fromEntries(chains.map((c) => [c, takeChain(wallets, c)]));
}

export function formFromPayout(p: LinkPayout | undefined, chains: string[] = []): PayoutForm {
  const rows = (items: PayoutItem[] | undefined) => (items ?? []).map((i) => ({ key: newKey(), wallet_id: i.wallet_id, share: typed(parseShare(i.share) ?? 0) }));
  const keys = [...new Set([...Object.keys(p ?? {}), ...chains])];
  return Object.fromEntries(keys.map((c) => [c, rows(p?.[c])]));
}

/** The networks the editor shows: those whose family has an active wallet, and any the product already pays. */
export function shownChains(wallets: Wallet[], form: PayoutForm, chains: string[]): string[] {
  const all = [...new Set([...chains, ...Object.keys(form)])];
  return all.filter((c) => activeWallets(wallets, chainFamily(c)).length > 0 || (form[c] ?? []).length > 0);
}

/** The other networks of the same family whose split this one can copy. */
export function copySources(form: PayoutForm, chain: string, shown: string[]): string[] {
  return shown.filter((c) => c !== chain && chainFamily(c) === chainFamily(chain) && (form[c] ?? []).length > 0);
}

/** A copy of another network's rows, as rows of their own. */
export const copyRows = (rows: PayoutRow[]): PayoutRow[] => rows.map((r) => ({ ...r, key: newKey() }));

/** The total of the shares typed so far; a row without a valid percentage counts as zero. */
export function totalBps(rows: PayoutRow[]): number {
  return rows.reduce((a, r) => a + (parseShare(r.share) ?? 0), 0);
}

export function sumMessage(total: number): string {
  return total === FULL ? 'Shares add up to 100%.' : `Shares must add up to 100%. Now ${formatPercent(total)}.`;
}

/** The wallets a row can pick: active ones of the network's family no other row has taken (and its own). */
export function pickable(rows: PayoutRow[], index: number, wallets: Wallet[], chain: string): Wallet[] {
  const taken = new Set(rows.filter((_, i) => i !== index).map((r) => r.wallet_id));
  const own = rows[index]?.wallet_id;
  const family = chainFamily(chain);
  return wallets.filter((w) => w.family === family && (isActive(w) || w.id === own) && !taken.has(w.id));
}

function freeWallets(rows: PayoutRow[], wallets: Wallet[], chain: string): Wallet[] {
  const taken = new Set(rows.map((r) => r.wallet_id));
  return activeWallets(wallets, chainFamily(chain)).filter((w) => !taken.has(w.id));
}

/** Why another wallet cannot be added, or null when it can. */
export function addRowBlocker(rows: PayoutRow[], wallets: Wallet[], chain: string): string | null {
  if (rows.length >= MAX_RECIPIENTS) return `A split pays at most ${MAX_RECIPIENTS} wallets.`;
  if (freeWallets(rows, wallets, chain).length === 0) return `Every wallet that can receive on ${networkName(chain)} is already in this split. Add a wallet in Settings to share with more.`;
  return null;
}

/** Appends the oldest wallet not yet in the split, with whatever share is still unassigned. */
export function addRow(rows: PayoutRow[], wallets: Wallet[], chain: string): PayoutRow[] {
  if (addRowBlocker(rows, wallets, chain)) return rows;
  const next = freeWallets(rows, wallets, chain)[0]!;
  const rest = FULL - totalBps(rows);
  return [...rows, { key: newKey(), wallet_id: next.id, share: rest > 0 ? typed(rest) : '' }];
}

export function removeRow(rows: PayoutRow[], index: number): PayoutRow[] {
  return rows.filter((_, i) => i !== index);
}

export const rowKey = (chain: string, i: number, part: 'wallet' | 'share') => `payout.${chain}.${i}.${part}`;
export const sumKey = (chain: string) => `payout.${chain}.sum`;
export const rowsKey = (chain: string) => `payout.${chain}.rows`;
export const NONE_KEY = 'payout';

export interface PayoutValidation {
  errors: Record<string, string>;
  /** Every network of the form, explicitly: what the merchant saw is what is stored. */
  input: Record<string, Items> | null;
  totals: Record<string, number>;
}

/**
 * Checks the editor and builds the request body. Without `wallets` only the shape is checked
 * (shares, duplicates, counts), not whether each wallet is an active one of the network's family.
 */
export function validatePayout(form: PayoutForm, wallets?: Wallet[]): PayoutValidation {
  const errors: Record<string, string> = {};
  const totals: Record<string, number> = {};
  for (const [chain, rows] of Object.entries(form)) {
    const name = networkName(chain);
    const family = chainFamily(chain);
    totals[chain] = totalBps(rows);
    if (rows.length > MAX_RECIPIENTS) errors[rowsKey(chain)] = `Use at most ${MAX_RECIPIENTS} wallets on ${name}.`;
    const seen = new Set<string>();
    let allShares = true;
    rows.forEach((r, i) => {
      const w = wallets?.find((x) => x.id === r.wallet_id);
      if (!r.wallet_id) errors[rowKey(chain, i, 'wallet')] = 'Choose a wallet.';
      else if (seen.has(r.wallet_id)) errors[rowKey(chain, i, 'wallet')] = 'This wallet is already in this split. Choose another one.';
      else if (wallets && (!w || w.family !== family)) errors[rowKey(chain, i, 'wallet')] = `Choose a wallet that can receive on ${name}.`;
      else if (w && !isActive(w)) errors[rowKey(chain, i, 'wallet')] = 'This wallet is archived. Choose another one.';
      if (r.wallet_id) seen.add(r.wallet_id);

      const bps = parseShare(r.share);
      if (!r.share.trim()) errors[rowKey(chain, i, 'share')] = 'Enter a share, such as 50.';
      else if (bps === null) errors[rowKey(chain, i, 'share')] = 'Enter a percentage with at most two decimals, such as 33.33.';
      else if (bps === 0) errors[rowKey(chain, i, 'share')] = 'Enter a share above 0%, or remove this wallet.';
      else if (bps > FULL) errors[rowKey(chain, i, 'share')] = 'Use at most 100%.';
      if (bps === null || bps === 0 || bps > FULL) allShares = false;
    });
    if (rows.length > 0 && allShares && totals[chain] !== FULL) errors[sumKey(chain)] = sumMessage(totals[chain]!);
  }
  if (Object.values(form).every((rows) => rows.length === 0)) {
    const hasWallet = wallets ? wallets.some(isActive) : true;
    errors[NONE_KEY] = hasWallet ? 'Take payments on at least one network: give it at least one wallet.' : 'Add a wallet first, so payments have somewhere to go.';
  }
  const ok = Object.keys(errors).length === 0;
  const body = (rows: PayoutRow[]) => rows.map((r) => ({ wallet_id: r.wallet_id, share: bpsToShare(parseShare(r.share)!) }));
  return { errors, totals, input: ok ? Object.fromEntries(Object.entries(form).map(([c, rows]) => [c, body(rows)])) : null };
}

const sig = (items: Items | undefined) => (items ?? []).map((i) => `${i.wallet_id}:${parseShare(i.share)}`).join(',');

/** Only the networks whose recipients or shares changed; undefined when none did. */
export function payoutPatch(original: LinkPayout | undefined, next: Record<string, Items>): PayoutInput | undefined {
  const out: PayoutInput = {};
  for (const [c, items] of Object.entries(next)) if (sig(original?.[c]) !== sig(items)) out[c] = items;
  return Object.keys(out).length > 0 ? out : undefined;
}

function walletName(walletId: string, wallets: Wallet[], fallback?: string): string {
  return wallets.find((w) => w.id === walletId)?.label ?? fallback ?? 'Unknown wallet';
}

/** For the bar: stored items carry their label; the wallet list fills in any missing one. */
export function sharesOfItems(items: PayoutItem[], wallets: Wallet[]): Share[] {
  return items.map((i) => ({ walletId: i.wallet_id, label: i.label || walletName(i.wallet_id, wallets), bps: parseShare(i.share) ?? 0 }));
}

export function sharesOfRows(rows: PayoutRow[], wallets: Wallet[]): Share[] {
  return rows.map((r) => ({ walletId: r.wallet_id, label: r.wallet_id ? walletName(r.wallet_id, wallets) : 'No wallet chosen', bps: parseShare(r.share) ?? 0 }));
}

/** "Base: 50% Founder · 50% Partner", or "" for a network with nothing assigned. */
export function payoutLine(chain: string, shares: Share[]): string {
  const parts = shares.filter((s) => s.bps > 0).map((s) => `${formatPercent(s.bps)} ${s.label}`);
  return parts.length > 0 ? `${networkName(chain)}: ${parts.join(' · ')}` : '';
}

/** "Base: 50% A · 50% B · Solana: 100% A": the networks taken, in the order given. */
function joinLines(lines: string[]): string {
  return lines.filter(Boolean).join(' · ');
}

/** The summary of a stored product: only the networks it takes. */
export function linkPayoutSummary(payout: LinkPayout | undefined, wallets: Wallet[] = []): string {
  return joinLines(Object.entries(payout ?? {}).map(([c, items]) => payoutLine(c, sharesOfItems(items, wallets))));
}

/** The summary of the form, as it stands. */
export function formSummary(form: PayoutForm, chains: string[], wallets: Wallet[]): string {
  return joinLines(chains.map((c) => payoutLine(c, sharesOfRows(form[c] ?? [], wallets))));
}

/** The network a message names, by chain id or by name, longest name first ("Base Sepolia" before "Base"). */
function chainNamed(m: string, chains: string[]): string | null {
  const byLength = [...chains].sort((a, b) => networkName(b).length - networkName(a).length);
  return byLength.find((c) => new RegExp(`\\b${c.replace(/[-]/g, '\\-')}\\b`).test(m)) ?? byLength.find((c) => m.includes(networkName(c))) ?? null;
}

const PAYOUT_PARAM = /^payout(?:\.([a-z0-9-]+)(?:\[(\d+)\]\.(wallet_id|share))?)?$/;

/**
 * The field of a refusal whose `param` is a payout path: "payout" (pays no network),
 * "payout.<chain>" (its count or its total) or "payout.<chain>[i].wallet_id|share" (one row).
 * Undefined when the param is not a payout path.
 */
function payoutParamKey(e: { code: string; message: string; param?: string }, form: PayoutForm): string | null | undefined {
  const p = e.param ? PAYOUT_PARAM.exec(e.param) : null;
  if (!p) return undefined;
  const [, chain, index, part] = p;
  if (!chain) return NONE_KEY;
  const rows = form[chain];
  if (!rows) return null;
  if (index !== undefined) return Number(index) < rows.length ? rowKey(chain, Number(index), part === 'wallet_id' ? 'wallet' : 'share') : rowsKey(chain);
  if (e.code === 'unknown_chain') return rowsKey(chain);
  // The network's total and its recipient count share one param; only the total talks about 100%.
  return /add up to|100\s?%/i.test(e.message) ? sumKey(chain) : rowsKey(chain);
}

/**
 * The field a server refusal of the payout belongs to, or null to show it above the form. The
 * API names the place in `param` ("payout.base[1].share"), which wins; the message (a path, a
 * wallet id or name, a network name, "add up to 100%") is only read when there is none.
 */
export function payoutErrorKey(e: { code: string; message: string; param?: string }, form: PayoutForm, wallets: Wallet[]): string | null {
  const m = e.message;
  if (!['invalid_request', 'wallet_blacklisted', 'unknown_chain'].includes(e.code)) return null;
  const byParam = payoutParamKey(e, form);
  if (byParam !== undefined) return byParam;
  const chains = Object.keys(form);
  const path = /payout\.([a-z0-9-]+)(?:\[(\d+)\])?(?:\.(wallet_id|share))?/.exec(m);
  let chain: string | null = path && chains.includes(path[1]!) ? path[1]! : chainNamed(m, chains);
  const index = path?.[2] !== undefined ? Number(path[2]) : null;

  if (/at least one network|no network|pays? (no|neither)/i.test(m)) return NONE_KEY;
  if (e.code === 'unknown_chain') return chain ? rowsKey(chain) : null;

  // A wallet named by id or address (or by name when the issuer blacklisted it): its row.
  for (const c of chain ? [chain] : chains) {
    const i = (form[c] ?? []).findIndex((r) => {
      const w = wallets.find((x) => x.id === r.wallet_id);
      return Boolean(r.wallet_id) && (m.includes(r.wallet_id) || (w && (m.includes(w.address) || (e.code === 'wallet_blacklisted' && m.includes(w.label)))));
    });
    if (i >= 0) return rowKey(c, i, 'wallet');
  }

  if (chain && index !== null && index < (form[chain] ?? []).length) {
    const onWallet = path?.[3] === 'wallet_id' || (path?.[3] === undefined && /wallet/i.test(m) && !/share/i.test(m));
    return rowKey(chain, index, onWallet ? 'wallet' : 'share');
  }
  if (/add up to|100\s?%/i.test(m)) {
    chain ??= chains.find((c) => form[c]!.length > 0 && totalBps(form[c]!) !== FULL) ?? chains.find((c) => form[c]!.length > 0) ?? null;
    return chain ? sumKey(chain) : null;
  }
  if (/1 to 5|at most 5|only once|twice|more than once/i.test(m)) {
    chain ??= chains.find((c) => form[c]!.length > 0) ?? null;
    return chain ? rowsKey(chain) : null;
  }
  return chain && /payout/i.test(m) ? rowsKey(chain) : null;
}

export interface PayoutChange {
  chain: string;
  before: string[];
  after: string[];
}

const destination = (i: { wallet_id: string; share: string }, wallets: Wallet[], label?: string): string => {
  const w = wallets.find((x) => x.id === i.wallet_id);
  const share = formatPercent(parseShare(i.share) ?? 0);
  const address = w?.address ?? (i as PayoutItem).address;
  return `${share} ${w?.label || label || 'Unknown wallet'}${address ? ` (${truncateMiddle(address, 6, 4)})` : ''}`;
};

/**
 * Where the money goes now against where it will go, for the networks whose recipients or shares
 * change: one line per wallet with its share and a shortened address. Empty when nothing changes.
 */
export function payoutChanges(original: LinkPayout | undefined, next: Record<string, Items>, wallets: Wallet[]): PayoutChange[] {
  const patch = payoutPatch(original, next);
  if (!patch) return [];
  return Object.keys(patch).map((chain) => ({
    chain,
    before: (original?.[chain] ?? []).map((i) => destination(i, wallets, i.label)),
    after: (patch[chain] ?? []).map((i) => destination(i, wallets)),
  }));
}
