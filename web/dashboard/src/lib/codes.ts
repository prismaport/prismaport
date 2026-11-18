import type { CodeStatus } from '../components/CodeInput';
import { ApiError } from './api';

/** Emailed codes: 6 digits, 5 tries each, 15 minutes (the server's MaxCodeAttempts and EmailCodeLifetime). */
export const CODE_LENGTH = 6;
const CODE_TRIES = 5;
export const CODE_LIFETIME_MS = 15 * 60_000;
/** How long the green confirmed state shows before the page moves on. */
export const CONFIRMED_PAUSE_MS = 700;

export const CODE_MESSAGES = {
  expired: 'This code expired. Send a new one.',
  locked: 'Too many wrong codes. Send a new one in a few minutes.',
  wrongUnknown: "That code isn't right, or it expired. Check the newest email, or send a new code.",
  sent: 'New code sent. Check your inbox.',
  empty: 'Enter the 6-digit code from the email.',
} as const;

export const wrongCode = (left: number) => `That code isn't right. ${left} ${left === 1 ? 'try' : 'tries'} left.`;

export const incompleteCode = (typed: number) => (typed === 0 ? CODE_MESSAGES.empty : `Type all ${CODE_LENGTH} digits (${typed} of ${CODE_LENGTH}).`);

/** What this page knows about the code in the user's inbox. */
export interface CodeTries {
  /** Wrong codes entered here since the code was sent, this one included. */
  failures: number;
  /** When this page asked for the code; null when it was sent before the page opened. */
  sentAt: number | null;
}

/**
 * The state and message for a failed check. The API answers every failure alike (`invalid_code`:
 * wrong, expired or out of tries), so the page tells them apart from what it saw itself; when it
 * did not send the code it cannot count the tries left and says so. `rate_limited` means many
 * tries in a minute. Any other error is not about the code: null.
 */
export function codeFailure(err: unknown, tries: CodeTries, now: number): { status: CodeStatus; message: string } | null {
  if (!(err instanceof ApiError)) return null;
  if (err.code === 'rate_limited') return { status: 'locked', message: CODE_MESSAGES.locked };
  if (err.code !== 'invalid_code') return null;
  if (tries.sentAt !== null && now - tries.sentAt >= CODE_LIFETIME_MS) return { status: 'expired', message: CODE_MESSAGES.expired };
  if (tries.failures >= CODE_TRIES) return { status: 'locked', message: CODE_MESSAGES.locked };
  if (tries.sentAt === null) return { status: 'invalid', message: CODE_MESSAGES.wrongUnknown };
  return { status: 'invalid', message: wrongCode(CODE_TRIES - tries.failures) };
}
