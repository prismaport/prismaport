/**
 * The API's origin, from `<meta name="prismaport-api">` in index.html. Empty, as Vite builds it
 * and the api binary serves it, means this page's own origin. `prismaport pages build` writes the
 * API host's origin into it for the static Pages build (split mode). Anything
 * but an absolute http(s) URL counts as empty.
 */
export function apiOrigin(doc: Pick<Document, 'querySelector'> = document): string {
  const raw = doc.querySelector<HTMLMetaElement>('meta[name="prismaport-api"]')?.content.trim() ?? '';
  if (!raw) return '';
  try {
    const u = new URL(raw);
    return u.protocol === 'https:' || u.protocol === 'http:' ? u.origin : '';
  } catch {
    return '';
  }
}
