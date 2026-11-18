import type { Pill } from './status';
import type { ApiKey, RollOverlap } from './types';
import { formatDateTime, formatRelative } from './format';

/** Every scope a key can hold; a key with all of them has full access. */
export const ALL_SCOPES = ['products:read', 'products:write', 'payment_links:read', 'payment_links:write', 'customer_addresses:read', 'customer_addresses:write', 'wallets:read', 'sessions:read', 'sessions:write', 'events:read', 'webhooks:read', 'webhooks:write'] as const;

export type Access = 'none' | 'read' | 'write';

export interface ScopeGroup {
  resource: string;
  label: string;
  read: string;
  write?: string;
  /** Picker option labels. */
  readLabel: string;
  writeLabel?: string;
  /** Summary words in the key list. */
  writeSummary?: string;
  writeOnlySummary?: string;
}

/** The scope picker: one row per resource, none / read / read and write. */
export const SCOPE_GROUPS: ScopeGroup[] = [
  { resource: 'sessions', label: 'Payments', read: 'sessions:read', write: 'sessions:write', readLabel: 'Read', writeLabel: 'Read and create', writeSummary: 'read and create', writeOnlySummary: 'create' },
  { resource: 'products', label: 'Products', read: 'products:read', write: 'products:write', readLabel: 'Read', writeLabel: 'Read and change', writeSummary: 'read and change', writeOnlySummary: 'change' },
  { resource: 'payment_links', label: 'Payment links', read: 'payment_links:read', write: 'payment_links:write', readLabel: 'Read', writeLabel: 'Read and change', writeSummary: 'read and change', writeOnlySummary: 'change' },
  { resource: 'customer_addresses', label: 'Customer addresses', read: 'customer_addresses:read', write: 'customer_addresses:write', readLabel: 'Read', writeLabel: 'Read and manage', writeSummary: 'read and manage', writeOnlySummary: 'manage' },
  { resource: 'wallets', label: 'Wallets', read: 'wallets:read', readLabel: 'Read' },
  { resource: 'events', label: 'Event feed', read: 'events:read', readLabel: 'Read' },
  { resource: 'webhooks', label: 'Webhook endpoints', read: 'webhooks:read', write: 'webhooks:write', readLabel: 'Read', writeLabel: 'Read and manage', writeSummary: 'read and manage', writeOnlySummary: 'manage' },
];

export type AccessMap = Record<string, Access>;

export const NO_ACCESS: AccessMap = Object.fromEntries(SCOPE_GROUPS.map((g) => [g.resource, 'none'])) as AccessMap;

/** The scopes an access choice grants. Write always comes with read. */
export function scopesFromAccess(access: AccessMap): string[] {
  const out: string[] = [];
  for (const g of SCOPE_GROUPS) {
    const a = access[g.resource] ?? 'none';
    if (a === 'none') continue;
    out.push(g.read);
    if (a === 'write' && g.write) out.push(g.write);
  }
  return out;
}

export function isFullAccess(scopes: readonly string[]): boolean {
  return ALL_SCOPES.every((s) => scopes.includes(s));
}

/** "Payments: read and create · Event feed: read", in picker order; "Full access" for every scope. */
export function describeScopes(scopes: readonly string[]): string {
  if (isFullAccess(scopes)) return 'Full access';
  const parts: string[] = [];
  for (const g of SCOPE_GROUPS) {
    const r = scopes.includes(g.read);
    const w = g.write !== undefined && scopes.includes(g.write);
    if (r && w) parts.push(`${g.label}: ${g.writeSummary}`);
    else if (w) parts.push(`${g.label}: ${g.writeOnlySummary}`);
    else if (r) parts.push(`${g.label}: read`);
  }
  return parts.join(' · ');
}

export const ROLL_OVERLAPS: { value: RollOverlap; label: string; hint: string }[] = [
  { value: 'now', label: 'Now', hint: 'The old secret stops working at once.' },
  { value: '1h', label: '1 hour', hint: 'The old secret keeps working for 1 hour.' },
  { value: '24h', label: '24 hours', hint: 'The old secret keeps working for 24 hours.' },
  { value: '7d', label: '7 days', hint: 'The old secret keeps working for 7 days.' },
];

/** What a key's state looks like in the list; null for a key that simply works (and a revoked one, which says when). */
export function keyPill(k: Pick<ApiKey, 'status' | 'expires_at'>): Pill | null {
  switch (k.status) {
    case 'expiring':
      return { label: `Works until ${formatDateTime(k.expires_at)}`, tone: 'partial' };
    case 'expired':
      return { label: 'Expired', tone: 'idle' };
    default:
      return null;
  }
}

/** "Last used 3 h ago", "Last used on May 28, 2026", "Never used". */
export function lastUsedLabel(iso: string | null): string {
  if (!iso) return 'Never used';
  const r = formatRelative(iso);
  if (r === 'Just now') return 'Last used just now';
  return r.endsWith(' ago') ? `Last used ${r}` : `Last used on ${r}`;
}

/** Only a key that still works and was not rolled yet can be rolled or revoked from the list. */
export function canRoll(k: Pick<ApiKey, 'status'>): boolean {
  return k.status === 'active';
}
