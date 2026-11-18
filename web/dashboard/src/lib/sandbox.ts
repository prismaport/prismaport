import type { Pill } from './status';
import type { InboxRequest, SessionStatus, SignatureFailure, SimAction } from './types';

/** A JSON body indented for reading; anything else exactly as received. */
export function prettyBody(body: string): { text: string; json: boolean } {
  if (!body.trim()) return { text: '', json: false };
  try {
    return { text: JSON.stringify(JSON.parse(body), null, 2), json: true };
  } catch {
    return { text: body, json: false };
  }
}

export const SIGNATURE_FAILURE: Record<SignatureFailure, string> = {
  missing: 'No Signature header was sent.',
  malformed: 'The Signature header is not in the t=…,v1=… form.',
  no_match: 'No v1 signature matches the endpoint secret: the body changed or another secret signed it.',
  timestamp_outside_tolerance: 'The signature matches, but its timestamp is more than 5 minutes from when it arrived.',
};

export function signaturePill(sig: InboxRequest['signature']): Pill {
  return sig.valid ? { label: 'Signature valid', tone: 'paid' } : { label: 'Signature not valid', tone: 'partial' };
}

/** "on time", "12 s late", "3 min early": arrival time minus the signed time. */
export function formatSkew(seconds: number | null): string | null {
  if (seconds === null) return null;
  if (seconds === 0) return 'on time';
  const abs = Math.abs(seconds);
  const amount = abs < 120 ? `${abs} s` : abs < 7200 ? `${Math.round(abs / 60)} min` : `${Math.round(abs / 3600)} h`;
  return seconds > 0 ? `${amount} after it was signed` : `${amount} before it was signed`;
}

export interface SimActionInfo {
  action: SimAction;
  label: string;
  description: string;
  /** Takes an optional amount: the synthetic payment, in the token. */
  amount: boolean;
  /** Session states the action can start from. */
  from: SessionStatus[];
  events: string[];
}

const BEFORE_EXPIRY: SessionStatus[] = ['open', 'partially_paid'];

export const SIM_ACTIONS: SimActionInfo[] = [
  { action: 'pay_exact', label: 'Pay the exact amount', description: 'Pays what is still due.', amount: false, from: BEFORE_EXPIRY, events: ['session.payment_detected', 'session.paid'] },
  { action: 'pay_partial', label: 'Pay part of it', description: 'Pays less than what is due. Leave the amount empty to pay half.', amount: true, from: BEFORE_EXPIRY, events: ['session.payment_detected', 'session.partially_paid'] },
  { action: 'overpay', label: 'Overpay', description: 'Pays more than what is due. Leave the amount empty to pay 1 more.', amount: true, from: BEFORE_EXPIRY, events: ['session.payment_detected', 'session.overpaid'] },
  { action: 'expire', label: 'Expire now', description: 'Closes the session as if its time ran out.', amount: false, from: BEFORE_EXPIRY, events: ['session.expired'] },
  { action: 'late_payment', label: 'Pay after expiry', description: 'Expires the session if needed, then pays. Leave the amount empty to pay what is due.', amount: true, from: [...BEFORE_EXPIRY, 'expired', 'late_payment'], events: ['session.payment_detected', 'session.late_payment'] },
  { action: 'reverse_last_payment', label: 'Reverse the last payment', description: 'Removes the newest simulated payment, as a chain reorganization would.', amount: false, from: ['open', 'partially_paid', 'paid', 'overpaid', 'expired', 'late_payment', 'under_review'], events: ['payment.reversed'] },
  { action: 'under_review', label: 'Flag for review', description: 'Holds the session as a screening hit would.', amount: false, from: ['open', 'partially_paid', 'paid', 'overpaid', 'expired', 'late_payment'], events: ['session.under_review'] },
];

export function simActionInfo(action: SimAction, openAmount = false): SimActionInfo {
  return simActionsFor(openAmount).find((a) => a.action === action)!;
}

/** The default synthetic payment on an open-amount session, where the payer decides the amount. */
export const OPEN_AMOUNT_DEFAULT = '10.00';

/**
 * The actions of a session. An open-amount session (amount null, on a customer address) is never
 * partially paid or overpaid, and paying it needs an amount.
 */
export function simActionsFor(openAmount: boolean): SimActionInfo[] {
  if (!openAmount) return SIM_ACTIONS;
  return SIM_ACTIONS.filter((a) => a.action !== 'pay_partial' && a.action !== 'overpay').map((a) => {
    if (a.action === 'pay_exact') return { ...a, label: 'Pay an amount', description: `Pays the amount you enter (${OPEN_AMOUNT_DEFAULT} when empty).`, amount: true };
    if (a.action === 'late_payment') return { ...a, description: `Expires the session if needed, then pays the amount you enter (${OPEN_AMOUNT_DEFAULT} when empty).` };
    return a;
  });
}

/** Whether the action can run now; the server has the last word. */
export function simAllowed(info: SimActionInfo, status: SessionStatus, hasReversible: boolean): boolean {
  if (!info.from.includes(status)) return false;
  return info.action !== 'reverse_last_payment' || hasReversible;
}

/** A synthetic payment amount: digits with up to six decimals, above zero. */
export function simAmountError(value: string): string | undefined {
  const v = value.trim();
  if (!v) return undefined;
  if (!/^\d+(\.\d{1,6})?$/.test(v)) return 'Use digits with at most six decimals, such as 10.50.';
  if (!/[1-9]/.test(v)) return 'Enter an amount above zero.';
  return undefined;
}
