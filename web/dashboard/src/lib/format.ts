import { formatAmount } from './money';
import type { AssetAmount, Link, PriceType } from './types';

const NETWORKS: Record<string, string> = {
  base: 'Base',
  'base-sepolia': 'Base Sepolia',
  solana: 'Solana',
  'solana-devnet': 'Solana devnet',
};

/** The network name merchants read ("Base"), never the chain id. */
export function networkName(chain: string): string {
  return NETWORKS[chain] ?? chain.replace(/-/g, ' ').replace(/^./, (c) => c.toUpperCase());
}

/** An amount for people, grouped by thousands ("1,250.00"); never rounds away a non-zero digit. */
export function displayAmount(value: string): string {
  const [int = '0', frac = ''] = formatAmount(value).split('.');
  return `${int.replace(/\B(?=(\d{3})+(?!\d))/g, ',')}.${frac}`;
}

/** "25.00 USD" for people. */
export function usd(value: string): string {
  return `${displayAmount(value)} USD`;
}

/** A session's amount: "25.00 USD", or "Any amount" on an open-amount session (amount null), where the payer decides. */
export function sessionAmount(value: string | null): string {
  return value === null ? 'Any amount' : usd(value);
}

/** What one transfer to a deposit address must carry: "25.00 USDC", or "Any amount" on an open-amount session. */
export function tokenAmount(value: string | null, asset: string): string {
  return value === null ? 'Any amount' : `${value} ${asset}`;
}

type Priced = { price_type: PriceType; amount: string | null; min_amount: string | null; max_amount: string | null };

/** A variable price's range, as the payer reads it: "5.00 to 100.00 USD", "From 5.00 USD". */
export function rangeText(p: Pick<Priced, 'min_amount' | 'max_amount'>): string {
  if (p.min_amount && p.max_amount) return `${displayAmount(p.min_amount)} to ${displayAmount(p.max_amount)} USD`;
  if (p.min_amount) return `From ${displayAmount(p.min_amount)} USD`;
  return 'Any amount';
}

/** A product's price in the dashboard: "25.00 USD" or "Variable · 5.00 to 100.00 USD". */
export function productPrice(p: Priced): string {
  if (p.price_type === 'fixed') return p.amount ? usd(p.amount) : '';
  const range = rangeText(p);
  return `Variable · ${range.startsWith('From') ? range.replace('From', 'from') : range}`;
}

/** The price a link shows the payer: the product's fixed price, the amount the link sets, or the product's range. */
export function linkPrice(l: Pick<Link, 'amount' | 'product'>): string {
  if (l.product.price_type === 'fixed') return l.product.amount ? usd(l.product.amount) : '';
  if (l.amount) return usd(l.amount);
  return rangeText(l.product);
}

/** What the dashboard calls a link: its label, or the name of the product it sells. */
export function linkTitle(l: Pick<Link, 'name' | 'product'>): string {
  return l.name ?? l.product.name;
}

/** Exact received amounts, one per asset ("10.00 USDC"), skipping assets with nothing received. */
export function receivedList(items: AssetAmount[]): string[] {
  return items.filter((r) => /[1-9]/.test(r.amount)).map((r) => `${displayAmount(r.amount)} ${r.asset}`);
}

/**
 * Conversion (paid / opened) as a percentage with at most one decimal; "–" before anyone opened
 * the link. Computed from the counts so it always agrees with them; the server's `conversion`
 * is only used when the counts are missing, as a fraction or, above 1, as a percentage.
 */
export function formatConversion(conversion: number | string | null | undefined, opened: number, paid: number): string {
  let ratio: number | null = opened > 0 ? paid / opened : null;
  if (ratio === null && conversion !== null && conversion !== undefined && conversion !== '' && opened !== 0) {
    const c = Number(conversion);
    if (Number.isFinite(c)) ratio = c > 1 ? c / 100 : c;
  }
  if (ratio === null) return '–';
  const pct = Math.round(ratio * 1000) / 10;
  return `${Number.isInteger(pct) ? pct.toFixed(0) : pct.toFixed(1)}%`;
}

const DATE_TIME = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', year: 'numeric', hour: 'numeric', minute: '2-digit' });
const DATE = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', year: 'numeric' });

export function formatDateTime(iso: string | null | undefined): string {
  if (!iso) return '–';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '–' : DATE_TIME.format(d);
}

export function formatDate(iso: string | null | undefined): string {
  if (!iso) return '–';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '–' : DATE.format(d);
}

/** "just now", "5 min ago", "3 h ago", "2 days ago", then the date. */
export function formatRelative(iso: string | null | undefined, now = Date.now()): string {
  if (!iso) return 'Never';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return 'Never';
  const s = Math.round((now - t) / 1000);
  if (s < 0) return formatDate(iso);
  if (s < 60) return 'Just now';
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h} h ago`;
  const d = Math.floor(h / 24);
  if (d < 30) return `${d} ${d === 1 ? 'day' : 'days'} ago`;
  return formatDate(iso);
}

/** Explorer URL of a transaction or address, for the chains Prismaport serves. */
export function explorerUrl(chain: string, kind: 'tx' | 'address', ref: string): string | null {
  const r = encodeURIComponent(ref);
  switch (chain) {
    case 'base':
      return `https://basescan.org/${kind}/${r}`;
    case 'base-sepolia':
      return `https://sepolia.basescan.org/${kind}/${r}`;
    case 'solana':
      return `https://solscan.io/${kind === 'tx' ? 'tx' : 'account'}/${r}`;
    case 'solana-devnet':
      return `https://solscan.io/${kind === 'tx' ? 'tx' : 'account'}/${r}?cluster=devnet`;
    default:
      return null;
  }
}

export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n.toLocaleString('en-US')} ${n === 1 ? one : many}`;
}
