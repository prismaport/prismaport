import type { ClientReferenceSource } from './types';

/** What a client reference may hold; the server checks the same. */
export const CLIENT_REFERENCE = /^[A-Za-z0-9._:-]{1,128}$/;

export const CLIENT_REFERENCE_HINT = 'A client reference has up to 128 letters, digits and . _ : - characters.';

/** What a merchant may do with a session's client reference, by who set it. */
export function referenceNote(source: ClientReferenceSource | null): string {
  return source === 'payer'
    ? 'Anyone can change a payment link address, so this reference may not be what you sent. Use it to find the payment, never to decide what the payer gets: check the amount and your own records.'
    : 'Set by your server when it created the payment, like metadata.';
}
