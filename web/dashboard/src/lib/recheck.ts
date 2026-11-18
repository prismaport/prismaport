import { ApiError } from './api';

/** Sessions are watched until 23 h 30 min after they expire, 24 h after creation by default (D44, D46). */
const WATCH_AFTER_EXPIRY_MS = 23.5 * 60 * 60 * 1000;

/** End of the automatic watch window: the server's field when it sends one. */
export function watchEnd(s: { expires_at: string; watch_until?: string | null }): number {
  if (s.watch_until) return Date.parse(s.watch_until);
  return Date.parse(s.expires_at) + WATCH_AFTER_EXPIRY_MS;
}

/**
 * "Check again" follows the server's `recheck_available` (D47: also for a paid session once its payments
 * are final). Without the field, it is for sessions no longer watched that did not finish as paid.
 */
export function canRecheck(
  s: { status: string; expires_at: string; watch_until?: string | null; recheck_available?: boolean },
  nowMs: number,
): boolean {
  if (typeof s.recheck_available === 'boolean') return s.recheck_available;
  if (s.status === 'paid' || s.status === 'overpaid') return false;
  return nowMs >= watchEnd(s);
}

/**
 * A session of a customer address that a newer session on the address superseded: the newer one
 * watches the address and gets what arrives, so "Check again" is not available here.
 */
export function supersededBy(s: { superseded_by?: string | null; recheck_available?: boolean }): string | null {
  return s.superseded_by && s.recheck_available !== true ? s.superseded_by : null;
}

export const RECHECK_COPY = {
  pending: 'Checking the deposit address…',
  paid: 'This payment is complete, so Prismaport no longer looks for more funds on its own. If someone sent the amount again to its deposit address, check again to find it.',
  stillWatched: 'This payment is still being watched automatically. Check again once its watch window has ended.',
  superseded: 'A newer payment on this customer address watches it now, so funds that arrive are credited there.',
  notRevealed: 'No payment was generated for this session, so no deposit address was shown and there is nothing to check.',
} as const;

export function recheckError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.code === 'session_superseded') return RECHECK_COPY.superseded;
    if (e.code === 'session_not_revealed') return RECHECK_COPY.notRevealed;
    if (e.code === 'session_still_watched' || e.status === 409) return RECHECK_COPY.stillWatched;
    if (e.status === 429) {
      const mins = e.retryAfter ? Math.max(1, Math.ceil(e.retryAfter / 60)) : null;
      return `You checked this payment a moment ago. ${mins ? `Try again in ${mins} ${mins === 1 ? 'minute' : 'minutes'}.` : 'Try again in a few minutes.'}`;
    }
  }
  return 'Could not start the check. Try again in a moment.';
}
