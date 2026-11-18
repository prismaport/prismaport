/**
 * The API's origin, from `<meta name="prismaport-api">` in index.html. Empty, as Vite builds it
 * and the api binary serves it, means this page's own origin. `prismaport pages build` writes the
 * API host's origin into it for the static Pages build (split mode). Anything
 * but an absolute http(s) URL counts as empty.
 */
export function apiOrigin(doc: Pick<Document, 'querySelector'> = document): string {
  return absoluteMeta(doc, 'prismaport-api')?.origin ?? '';
}

/**
 * The developer portal's address, from `<meta name="prismaport-docs">`: `/docs/` on this origin
 * when it is empty (the binary serves the portal next to the dashboard); the docs host's root in the
 * Pages build, where the dashboard has a host of its own.
 */
export function docsUrl(doc: Pick<Document, 'querySelector'> = document): string {
  return absoluteMeta(doc, 'prismaport-docs')?.href ?? '/docs/';
}

function absoluteMeta(doc: Pick<Document, 'querySelector'>, name: string): URL | null {
  const raw = doc.querySelector<HTMLMetaElement>(`meta[name="${name}"]`)?.content.trim() ?? '';
  if (!raw) return null;
  try {
    const u = new URL(raw);
    return u.protocol === 'https:' || u.protocol === 'http:' ? u : null;
  } catch {
    return null;
  }
}
