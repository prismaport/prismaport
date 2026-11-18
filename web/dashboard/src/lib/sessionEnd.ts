// Who was signed in when the session ended by itself (a 401 from the API), kept in memory for the
// sign-in page: it says why, offers the same email and goes back to the page. Nothing is noted on
// a first visit or after a sign-out, when no one is signed in. Landing on any auth page clears it,
// and a 401 while one is open (an unconfirmed account on /verify-email) is not noted: no dashboard
// page was left, so a later redirect to sign-in would name the wrong one.
let ended: { email: string } | null = null;
// Auth pages open: a count, as the next one can mount before the last one unmounts.
let authPages = 0;

const PAGES: [RegExp, string][] = [
  [/^\/(get-started)?$/, 'Get started'],
  [/^\/overview$/, 'Overview'],
  [/^\/products$/, 'Products'],
  [/^\/products\/[^/]+$/, 'the product'],
  [/^\/links$/, 'Payment links'],
  [/^\/links\/[^/]+$/, 'the payment link'],
  [/^\/payments$/, 'Payments'],
  [/^\/payments\/[^/]+$/, 'the payment'],
  [/^\/customers$/, 'Customers'],
  [/^\/customers\/[^/]+$/, 'the customer'],
  [/^\/developers\/webhooks\/[^/]+$/, 'the webhook deliveries'],
  [/^\/developers(\/.*)?$/, 'Developers'],
  [/^\/settings(\/.*)?$/, 'Settings'],
];

/** What the page at this path is called in the notice, or null for a path it does not know. */
export function pageName(pathname: string): string | null {
  return PAGES.find(([re]) => re.test(pathname))?.[1] ?? null;
}

export function noteSessionEnded(email: string): void {
  if (authPages === 0) ended = { email };
}

export function endedSession(): { email: string } | null {
  return ended;
}

/** For the auth pages (sign-in, sign-up, email and password): forgets the note; returns the cleanup for when they close. */
export function openAuthPages(): () => void {
  authPages += 1;
  ended = null;
  return () => {
    authPages -= 1;
  };
}

/** Whether the account that signed in is the one whose session ended: the same email, trimmed, in any case. */
export function sameEmail(a: string | undefined, b: string | undefined): boolean {
  return a !== undefined && b !== undefined && a.trim().toLowerCase() === b.trim().toLowerCase();
}

export function sessionEndedNotice(page: string | null): string {
  return `Your session ended, so we signed you out. ${page ? `Sign in again to go back to ${page}.` : 'Sign in again to continue.'}`;
}
