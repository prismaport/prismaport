// The credentials of a sign-up waiting for its email code, kept in memory only (never in storage
// or history), so the confirmation screen can sign the new user in right away. A reload loses
// them; the user then signs in by hand.
let pending: { email: string; password: string } | null = null;

export function setPendingSignup(email: string, password: string): void {
  pending = { email, password };
}

export function pendingSignup(email: string): { email: string; password: string } | null {
  return pending && pending.email === email ? pending : null;
}

export function clearPendingSignup(): void {
  pending = null;
}

/** Seconds before another code may be asked for (the server allows a few, then one a minute). */
export const RESEND_SECONDS = 60;
