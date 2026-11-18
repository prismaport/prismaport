/** What a client reference may hold; the server checks the same. */
export const CLIENT_REFERENCE = /^[A-Za-z0-9._:-]{1,128}$/;

/**
 * The client reference a payment link URL carries: `?ref=` or its alias
 * `?client_reference_id=`. Anyone can edit a URL, so the server stores it as the payer's and
 * the merchant uses it only to match payments. A value that is not valid, or two parameters
 * that disagree, give no reference: the payer can still pay.
 */
export function clientReferenceFromSearch(search: string): string | undefined {
  const params = new URLSearchParams(search);
  const values = [...params.getAll('ref'), ...params.getAll('client_reference_id')];
  if (values.length === 0) return undefined;
  const first = values[0]!;
  if (values.some((v) => v !== first)) return undefined;
  return CLIENT_REFERENCE.test(first) ? first : undefined;
}
