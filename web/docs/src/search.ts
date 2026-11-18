// The search box index: what each page and operation can be found by, beyond its nav label.
// Guide pages are indexed by their rendered text, operations by their fields and error codes.
// Small on purpose: plain substring matching over lowercased text, no dependency.
import { resolve, type Operation, type Schema } from './spec';

const ENTITIES: Record<string, string> = { amp: '&', lt: '<', gt: '>', quot: '"', '#39': "'", nbsp: ' ' };

/** The visible text of an HTML fragment, lowercased, with markup and inline SVG removed. */
export function htmlText(html: string): string {
  return html
    .replace(/<svg[\s\S]*?<\/svg>/g, ' ')
    .replace(/<[^>]+>/g, ' ')
    .replace(/&(amp|lt|gt|quot|#39|nbsp);/g, (_, e: string) => ENTITIES[e] ?? '')
    .replace(/\s+/g, ' ')
    .trim()
    .toLowerCase();
}

/** Property names of a schema, one level into objects and arrays of objects, each name once. */
function fieldNames(schema: Schema | undefined, depth = 2, out = new Set<string>()): Set<string> {
  if (!schema || depth < 0) return out;
  const s = resolve(schema);
  for (const [name, p] of Object.entries(s.properties ?? {})) {
    out.add(name);
    fieldNames(p, depth - 1, out);
  }
  if (s.items) fieldNames(s.items, depth, out);
  for (const o of s.oneOf ?? []) fieldNames(o, depth, out);
  return out;
}

/** What an operation can be found by: its label and path, scope, parameters, body and response fields, and error codes. */
export function operationText(op: Operation): string {
  const fields = fieldNames(op.body?.schema);
  for (const r of op.responses) if (r.status.startsWith('2')) fieldNames(r.schema, 2, fields);
  return [op.summary, op.method, op.path, op.scope ?? '', ...op.params.map((p) => p.name), ...fields, ...op.errors.map((e) => e.code)].join(' ').toLowerCase();
}

/** A lazily filled map of page id to searchable text, so pages are rendered for the index only once someone searches. */
export function lazyIndex(build: () => Record<string, string>): () => Record<string, string> {
  let index: Record<string, string> | null = null;
  return () => (index ??= build());
}
