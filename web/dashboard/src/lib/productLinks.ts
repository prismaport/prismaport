import { ApiError } from './api';
import { formatAmount } from './money';
import type { Link, LinksDecision, Product, ProductPatch } from './types';

const MONEY_FIELDS = ['amount', 'min_amount', 'max_amount', 'payout'] as const;

/** Whether a patch changes what the product's checkouts charge or where their money goes. */
export const changesMoney = (patch: ProductPatch): boolean => MONEY_FIELDS.some((k) => patch[k] !== undefined);

/**
 * Whether saving the patch asks first what happens to the product's links. The server counts every
 * link that is switched on; the dashboard asks when it knows of live ones and opens the same
 * question on a links_decision_required answer for the rest.
 */
export const asksAboutLinks = (product: Product, patch: ProductPatch): boolean => changesMoney(patch) && product.links.live > 0;

export const isLinksDecisionRequired = (e: unknown): e is ApiError => e instanceof ApiError && e.status === 409 && e.code === 'links_decision_required';

/** The number of active links a links_decision_required answer names, when it names one. */
export function activeLinksIn(e: ApiError): number | null {
  const m = /has (\d+) active links?\b/.exec(e.message);
  return m ? Number(m[1]) : null;
}

export function linksDialogTitle(n: number | null): string {
  if (n === 1) return 'What happens to the active link?';
  return n && n > 1 ? `What happens to the ${n} active links?` : 'What happens to the active links?';
}

export interface DecisionCopy {
  title: string;
  description: string;
  confirm: string;
}

export function decisionCopy(choice: LinksDecision, n: number | null): DecisionCopy {
  const one = n === 1;
  if (choice === 'keep') {
    return { title: one ? 'Keep it active' : 'Keep them active', description: 'New checkouts use the new price.', confirm: one ? 'Save and keep link' : 'Save and keep links' };
  }
  return {
    title: one ? 'Deactivate it' : 'Deactivate them',
    description: one ? 'Nobody can open a new checkout from it until you turn it back on.' : 'Nobody can open a new checkout from them until you turn each one back on.',
    confirm: one ? 'Save and deactivate link' : 'Save and deactivate links',
  };
}

const SHOWN = 3;

function listOf(items: string[], more: number): string {
  if (more > 0) return `${items.join(', ')} and ${more} more`;
  if (items.length <= 1) return items.join('');
  return `${items.slice(0, -1).join(', ')} and ${items.at(-1)}`;
}

/** Which kept links stay at an amount of their own, up to three by name: null when none does. */
export function ownAmountNote(links: Link[], productName: string): string | null {
  const own = links.filter((l) => l.active && l.amount !== null);
  if (own.length === 0) return null;
  const names = own.slice(0, SHOWN).map((l) => `${l.name ?? productName} (${formatAmount(l.amount!)} USD)`);
  const head = own.length === 1 ? 'A link that sets its own amount keeps it' : 'Links that set their own amount keep it';
  return `${head}: ${listOf(names, own.length - SHOWN)}.`;
}

/** Checkouts already open keep what they opened with whatever the choice; null when there are none. */
export function openCheckoutsNote(open: number): string | null {
  if (open <= 0) return null;
  if (open === 1) return '1 checkout already open keeps the old price and payout either way. Its payment address is tied to it until it expires.';
  return `${open.toLocaleString('en-US')} checkouts already open keep the old price and payout either way. Their payment address is tied to them until they expire.`;
}
