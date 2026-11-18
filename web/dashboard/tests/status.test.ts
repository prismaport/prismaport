import { describe, expect, it } from 'vitest';
import { DELIVERY_PILL, LINK_PILL, linkPill, linkState, PAYMENT_PILL, SESSION_PILL, sessionPill } from '../src/lib/status';
import type { Link } from '../src/lib/types';

const base = { live: true, active: true, unavailable_reason: null, expires_at: null, max_payments: null, stats: { opened: 0, paid: 0, received: [], last_paid_at: null, conversion: null } } as Pick<
  Link,
  'live' | 'active' | 'unavailable_reason' | 'expires_at' | 'max_payments' | 'stats'
>;
const now = Date.parse('2026-10-25T12:00:00Z');

describe('link status pill', () => {
  it('says Live, Inactive, Expired or Limit reached, always as a word', () => {
    expect(linkPill(base, now)).toEqual({ label: 'Live', tone: 'paid' });
    expect(linkPill({ ...base, live: false, unavailable_reason: 'inactive', active: false }, now)).toEqual({ label: 'Inactive', tone: 'idle' });
    expect(linkPill({ ...base, live: false, unavailable_reason: 'expired' }, now)).toEqual({ label: 'Expired', tone: 'idle' });
    expect(linkPill({ ...base, live: false, unavailable_reason: 'exhausted' }, now)).toEqual({ label: 'Limit reached', tone: 'partial' });
    expect(linkPill({ ...base, live: false, unavailable_reason: 'reserved' }, now)).toEqual({ label: 'Limit reached', tone: 'partial' });
  });

  it('trusts the server reason over its own derivation', () => {
    expect(linkState({ ...base, live: false, unavailable_reason: 'exhausted', expires_at: '2020-01-01T00:00:00Z' }, now)).toBe('exhausted');
    expect(linkState({ ...base, live: true, active: false }, now)).toBe('live');
  });

  it('derives the reason when the server left it out', () => {
    expect(linkState({ ...base, live: false, active: false }, now)).toBe('inactive');
    expect(linkState({ ...base, live: false, expires_at: '2026-10-25T11:00:00Z' }, now)).toBe('expired');
    expect(linkState({ ...base, live: false, max_payments: 3, stats: { ...base.stats, paid: 3 } }, now)).toBe('exhausted');
  });
});

describe('state pills follow the brand', () => {
  it('write every session state with its brand word', () => {
    expect(Object.values(SESSION_PILL).map((p) => p.label)).toEqual(['Open', 'Partially paid', 'Paid', 'Overpaid', 'Expired', 'Late payment', 'Under review']);
    expect(SESSION_PILL.paid.tone).toBe('paid');
    expect(SESSION_PILL.overpaid.tone).toBe('paid');
    expect(SESSION_PILL.partially_paid.tone).toBe('partial');
    expect(SESSION_PILL.late_payment.tone).toBe('partial');
    expect(SESSION_PILL.under_review.tone).toBe('partial');
    expect(SESSION_PILL.open.tone).toBe('idle');
    expect(SESSION_PILL.expired.tone).toBe('idle');
    expect(PAYMENT_PILL.reversed).toEqual({ label: 'Reversed', tone: 'reversed' });
  });

  it('never use a colour without a word, and never a brand or red tone', () => {
    for (const p of [...Object.values(SESSION_PILL), ...Object.values(PAYMENT_PILL), ...Object.values(LINK_PILL), ...Object.values(DELIVERY_PILL)]) {
      expect(p.label.trim().length).toBeGreaterThan(0);
      expect(['paid', 'partial', 'reversed', 'idle']).toContain(p.tone);
      expect(p.label).toBe(p.label.charAt(0).toUpperCase() + p.label.slice(1).toLowerCase());
    }
  });

  it('humanizes an unknown status instead of hiding it', () => {
    expect(sessionPill('refund_pending')).toEqual({ label: 'Refund pending', tone: 'idle' });
  });
});
