import type { DeliveryStatus, Link, PaymentStatus, Session, SessionStatus, UnavailableReason } from './types';

type Tone = 'paid' | 'partial' | 'reversed' | 'idle';

export interface Pill {
  label: string;
  tone: Tone;
}

/** Session states, written as the brand words with their state colour. */
export const SESSION_PILL: Record<SessionStatus, Pill> = {
  open: { label: 'Open', tone: 'idle' },
  partially_paid: { label: 'Partially paid', tone: 'partial' },
  paid: { label: 'Paid', tone: 'paid' },
  overpaid: { label: 'Overpaid', tone: 'paid' },
  expired: { label: 'Expired', tone: 'idle' },
  late_payment: { label: 'Late payment', tone: 'partial' },
  under_review: { label: 'Under review', tone: 'partial' },
};

export const PAYMENT_PILL: Record<PaymentStatus, Pill> = {
  detected: { label: 'Confirming', tone: 'idle' },
  confirmed: { label: 'Confirmed', tone: 'paid' },
  finalized: { label: 'Finalized', tone: 'paid' },
  reversed: { label: 'Reversed', tone: 'reversed' },
};

export const DELIVERY_PILL: Record<DeliveryStatus, Pill> = {
  pending: { label: 'Retrying', tone: 'partial' },
  succeeded: { label: 'Delivered', tone: 'paid' },
  dead: { label: 'Failed', tone: 'idle' },
};

/** Session states a payer can still pay into. */
const OPEN_SESSION_STATUSES: ReadonlySet<SessionStatus> = new Set(['open', 'partially_paid']);

/**
 * Whether a session can still change by itself: it is open to payments, or a transfer is still
 * confirming. Paid, overpaid, expired, late and under-review sessions whose transfers are all
 * confirmed, finalized or reversed only change when someone acts.
 */
export function sessionInProgress(s: Pick<Session, 'status' | 'payments'>): boolean {
  return OPEN_SESSION_STATUSES.has(s.status) || s.payments.some((p) => p.status === 'detected');
}

export function sessionPill(status: string): Pill {
  return SESSION_PILL[status as SessionStatus] ?? { label: status.replace(/_/g, ' ').replace(/^./, (c) => c.toUpperCase()), tone: 'idle' };
}

export const LINK_PILL: Record<'live' | UnavailableReason, Pill> = {
  live: { label: 'Live', tone: 'paid' },
  product_inactive: { label: 'Product inactive', tone: 'idle' },
  inactive: { label: 'Inactive', tone: 'idle' },
  expired: { label: 'Expired', tone: 'idle' },
  exhausted: { label: 'Limit reached', tone: 'partial' },
  reserved: { label: 'Limit reached', tone: 'partial' },
};

/**
 * Why a link is or is not opening sessions. The server's `live` and `unavailable_reason` win; the
 * fields are only derived locally when the server left the reason out.
 */
export function linkState(l: Pick<Link, 'live' | 'active' | 'unavailable_reason' | 'expires_at' | 'max_payments' | 'stats'> & { product?: { active: boolean } }, now = Date.now()): 'live' | UnavailableReason {
  if (l.live) return 'live';
  if (l.unavailable_reason && l.unavailable_reason in LINK_PILL) return l.unavailable_reason;
  if (l.product && !l.product.active) return 'product_inactive';
  if (!l.active) return 'inactive';
  if (l.expires_at && Date.parse(l.expires_at) <= now) return 'expired';
  if (l.max_payments !== null && l.stats.paid >= l.max_payments) return 'exhausted';
  return 'inactive';
}

export function linkPill(l: Parameters<typeof linkState>[0], now = Date.now()): Pill {
  return LINK_PILL[linkState(l, now)];
}

const PRODUCT_PILL: Record<'active' | 'inactive', Pill> = {
  active: { label: 'Active', tone: 'paid' },
  inactive: { label: 'Inactive', tone: 'idle' },
};

export const ATTENTION_COPY = {
  expiring: 'Stops accepting payments within 3 days',
  exhausted: 'Payment limit reached',
  inactive_with_open_sessions: 'Inactive, with sessions still open',
  product_inactive: 'Its product is inactive, with sessions still open',
} as const;

const EVENT_LABEL: Record<string, string> = {
  'session.created': 'Session opened',
  'session.payment_detected': 'Payment seen on the network',
  'session.partially_paid': 'Partially paid',
  'session.paid': 'Paid',
  'session.overpaid': 'Overpaid',
  'session.expired': 'Expired',
  'session.late_payment': 'Late payment',
  'session.under_review': 'Under review',
  'session.refunded': 'Refunded by the merchant',
  'payment.reversed': 'Payment reversed by the network',
};

/** Every event type a webhook endpoint can receive, in lifecycle order. */
export const WEBHOOK_EVENT_TYPES = [
  'session.payment_detected',
  'session.partially_paid',
  'session.paid',
  'session.overpaid',
  'session.expired',
  'session.late_payment',
  'session.under_review',
  'payment.reversed',
];

export function eventLabel(type: string): string {
  return EVENT_LABEL[type] ?? type;
}

export const productPill = (p: { active: boolean }): Pill => PRODUCT_PILL[p.active ? 'active' : 'inactive'];
