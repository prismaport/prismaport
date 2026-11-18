import type { User } from './types';

/** How long a lost-app request waits, and how long adding a wallet stays locked afterwards. Dates come from the API. */
export const REMOVAL_WAIT_HOURS = 72;
export const RECOVERY_LOCK_HOURS = 48;

/** A recovery code is XXXXX-XXXXX; the hyphen and the case do not matter when typing one. */
const BODY = /^[A-Z0-9]{10}$/;

/** What a person typed, kept to letters, digits and the hyphen, upper case, at most 11 characters. */
export function typedRecoveryCode(v: string): string {
  return v.toUpperCase().replace(/[^A-Z0-9-]/g, '').slice(0, 11);
}

/** The code as the API takes it (XXXXX-XXXXX), or null when it cannot be one. */
export function recoveryCodeValue(v: string): string | null {
  const plain = v.toUpperCase().replace(/[\s-]/g, '');
  return BODY.test(plain) ? `${plain.slice(0, 5)}-${plain.slice(5)}` : null;
}

const UTC = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', year: 'numeric', hour: '2-digit', minute: '2-digit', hourCycle: 'h23', timeZone: 'UTC' });

/** "Nov 12, 2026 at 14:32 UTC": the time of a removal or a lock, the same for everyone who reads the email. */
export function formatUtc(iso: string | null | undefined): string {
  const d = iso ? new Date(iso) : null;
  if (!d || Number.isNaN(d.getTime())) return '–';
  const p = Object.fromEntries(UTC.formatToParts(d).map((x) => [x.type, x.value]));
  return `${p.month} ${p.day}, ${p.year} at ${p.hour}:${p.minute} UTC`;
}

/** The day of a date alone: "Nov 12". */
export function formatUtcDay(iso: string | null | undefined): string {
  const d = iso ? new Date(iso) : null;
  if (!d || Number.isNaN(d.getTime())) return '–';
  return new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC' }).format(d);
}

/** When the recovery lock ends, while it is still ahead; null otherwise. */
export function lockedUntil(user: Pick<User, 'recovery_locked_until'> | undefined, now = Date.now()): string | null {
  const at = user?.recovery_locked_until;
  return at && Date.parse(at) > now ? at : null;
}

// A recovery sign-in is told once, on the two-factor card, in the tab it happened in.
const RECOVERY_SIGN_IN_KEY = 'prismaport.recovery-sign-in';

export function noteRecoverySignIn(used: boolean): void {
  try {
    if (used) sessionStorage.setItem(RECOVERY_SIGN_IN_KEY, '1');
    else sessionStorage.removeItem(RECOVERY_SIGN_IN_KEY);
  } catch {
    // Storage blocked: the card simply does not mention it.
  }
}

export function recoverySignInNoted(): boolean {
  try {
    return sessionStorage.getItem(RECOVERY_SIGN_IN_KEY) === '1';
  } catch {
    return false;
  }
}

/** The text of the downloaded file: one code per line, with a line saying what they are. */
export function recoveryCodesFile(codes: string[], email?: string): string {
  return ['Prismaport recovery codes', email ? `Account: ${email}` : null, 'Each code works once.', '', ...codes, ''].filter((l) => l !== null).join('\n');
}
