import common from './commonPasswords.json';

// Mirrors the password policy in internal/dashauth/password.go for live feedback. The server is
// the authority: it also checks known data breaches, which only it can do.
export const PASSWORD_MIN_CHARS = 8;
const PASSWORD_MAX_BYTES = 256;
const MAX_REPEATED_DISTINCT = 3;

const COMMON = new Set(common.map((p) => p.toLowerCase()));

export const COMMON_PASSWORD_ERROR = 'This password is too easy to guess. Choose one that is harder to predict.';
export const BREACHED_PASSWORD_ERROR = 'This password appeared in a data breach. Choose one you have not used anywhere else.';
const LONG_PASSWORD_ERROR = `Use a password with at most ${PASSWORD_MAX_BYTES} characters.`;

export interface PasswordChecks {
  length: number;
  upper: boolean;
  lower: boolean;
  number: boolean;
  special: boolean;
}

export function passwordChecks(pw: string): PasswordChecks {
  return {
    length: [...pw].length,
    upper: /\p{Lu}/u.test(pw),
    lower: /\p{Ll}/u.test(pw),
    number: /\p{N}/u.test(pw),
    special: /[^\p{L}\p{N}]/u.test(pw),
  };
}

/** What the password still needs, as phrases for {@link missingMessage}. */
export function missingParts(c: PasswordChecks): string[] {
  const parts: string[] = [];
  if (c.length < PASSWORD_MIN_CHARS) {
    const n = PASSWORD_MIN_CHARS - c.length;
    parts.push(`${n} more character${n === 1 ? '' : 's'}`);
  }
  if (!c.upper) parts.push('an uppercase letter');
  if (!c.lower) parts.push('a lowercase letter');
  if (!c.number) parts.push('a number');
  if (!c.special) parts.push('a special character');
  return parts;
}

export function missingMessage(parts: string[]): string {
  const list = parts.length === 1 ? parts[0] : `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}`;
  return `Add ${list}.`;
}

export function commonOrRepeated(pw: string): boolean {
  const folded = pw.toLowerCase();
  return COMMON.has(folded) || new Set(folded).size <= MAX_REPEATED_DISTINCT;
}

/** Counted in bytes, like the server's upper bound. */
export function passwordTooLong(pw: string): boolean {
  return new TextEncoder().encode(pw).length > PASSWORD_MAX_BYTES;
}

/** Why this password cannot be sent, or null when the server should decide. */
export function passwordProblem(pw: string): string | null {
  const parts = missingParts(passwordChecks(pw));
  if (parts.length > 0) return missingMessage(parts);
  if (passwordTooLong(pw)) return LONG_PASSWORD_ERROR;
  return commonOrRepeated(pw) ? COMMON_PASSWORD_ERROR : null;
}
