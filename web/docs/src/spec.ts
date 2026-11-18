// The portal renders docs/openapi.json, the contract internal/api/openapi_test.go holds to the code.
import raw from '../../../docs/openapi.json';

export interface Schema {
  $ref?: string;
  type?: string | string[];
  description?: string;
  properties?: Record<string, Schema>;
  required?: string[];
  items?: Schema;
  enum?: unknown[];
  oneOf?: Schema[];
  additionalProperties?: boolean | Schema;
  pattern?: string;
  minimum?: number;
  maximum?: number;
  format?: string;
}

export interface Param {
  name: string;
  in: 'path' | 'query' | 'header';
  required: boolean;
  description?: string;
  schema: Schema;
  example?: unknown;
}

export interface ErrorRow {
  status: number;
  code: string;
  description: string;
}

export interface RateLimit {
  bucket: string;
  per_minute: number;
  burst: number;
  per: 'key' | 'ip' | 'merchant';
}

interface RawOp {
  operationId: string;
  tags: string[];
  summary: string;
  description?: string;
  security: Record<string, string[]>[];
  parameters?: Param[];
  requestBody?: { required: boolean; content: { 'application/json': { schema: Schema; example?: unknown } } };
  responses: Record<string, { description: string; content?: { 'application/json': { schema: Schema; example: unknown } } }>;
  'x-scope'?: string;
  'x-rate-limit'?: RateLimit;
  'x-test-only'?: boolean;
  'x-errors': ErrorRow[];
  'x-not-found'?: { code: string; message: string };
  'x-session-status'?: string;
}

interface RawDoc {
  info: { title: string; version: string; description: string };
  servers: { url: string }[];
  tags: { name: string; description: string }[];
  paths: Record<string, Record<string, RawOp>>;
  webhooks: Record<string, { post: RawOp }>;
  components: { schemas: Record<string, Schema> };
}

const doc = raw as unknown as RawDoc;

export interface Response {
  status: string;
  schema: Schema;
  example: unknown;
}

export interface Operation {
  id: string;
  method: string;
  path: string;
  tag: string;
  summary: string;
  description: string;
  isPublic: boolean;
  scope: string | null;
  params: Param[];
  body: { schema: Schema; example: unknown; required: boolean } | null;
  responses: Response[];
  errors: ErrorRow[];
  rateLimit: RateLimit | null;
  testOnly: boolean;
  idempotency: boolean;
  notFound: { code: string; message: string } | null;
}

const METHOD_ORDER = ['post', 'get', 'patch', 'delete'];

function toOperation(path: string, method: string, op: RawOp): Operation {
  const responses: Response[] = Object.entries(op.responses)
    .filter(([, r]) => r.content)
    .map(([status, r]) => ({ status, schema: r.content!['application/json'].schema, example: r.content!['application/json'].example }));
  const params = op.parameters ?? [];
  return {
    id: op.operationId,
    method: method.toUpperCase(),
    path,
    tag: op.tags[0] ?? '',
    summary: op.summary,
    description: op.description ?? '',
    isPublic: op.security.length === 0,
    scope: op['x-scope'] ?? null,
    params: params.filter((p) => p.in !== 'header'),
    body: op.requestBody
      ? { schema: op.requestBody.content['application/json'].schema, example: op.requestBody.content['application/json'].example, required: op.requestBody.required }
      : null,
    responses,
    errors: op['x-errors'],
    rateLimit: op['x-rate-limit'] ?? null,
    testOnly: op['x-test-only'] === true,
    // Required keys only: POST /v1/customer_addresses takes an optional one (it is idempotent by customer_id).
    idempotency: params.some((p) => p.in === 'header' && p.name === 'Idempotency-Key' && p.required),
    notFound: op['x-not-found'] ?? null,
  };
}

/** Every operation, grouped in the order of the document's tags. Within a group the order is the document's. */
export const operations: Operation[] = (() => {
  const all: Operation[] = [];
  for (const [path, methods] of Object.entries(doc.paths)) {
    for (const m of Object.keys(methods).sort((a, b) => METHOD_ORDER.indexOf(a) - METHOD_ORDER.indexOf(b))) {
      all.push(toOperation(path, m, methods[m]!));
    }
  }
  const tagOrder = doc.tags.map((t) => t.name);
  return all.sort((a, b) => tagOrder.indexOf(a.tag) - tagOrder.indexOf(b.tag));
})();

export interface ErrorCode {
  code: string;
  statuses: number[];
  /** The wording most operations use for it. */
  description: string;
  ops: Operation[];
}

/** Every code in the operations' x-errors, by lowest status, then in the order the document first uses it. */
export const errorCodes: ErrorCode[] = (() => {
  const byCode = new Map<string, ErrorCode & { wordings: Map<string, number> }>();
  for (const op of operations) {
    for (const r of op.errors) {
      let e = byCode.get(r.code);
      if (!e) byCode.set(r.code, (e = { code: r.code, statuses: [], description: '', ops: [], wordings: new Map() }));
      if (!e.statuses.includes(r.status)) e.statuses.push(r.status);
      if (!e.ops.includes(op)) e.ops.push(op);
      e.wordings.set(r.description, (e.wordings.get(r.description) ?? 0) + 1);
    }
  }
  return [...byCode.values()]
    .map(({ wordings, ...e }) => ({
      ...e,
      statuses: e.statuses.sort((a, b) => a - b),
      description: [...wordings].reduce((best, w) => (w[1] > best[1] ? w : best))[0],
    }))
    .sort((a, b) => a.statuses[0]! - b.statuses[0]!);
})();

export const groups = doc.tags.map((t) => ({ name: t.name, description: t.description, ops: operations.filter((o) => o.tag === t.name) }));

export const apiBase = doc.servers[0]?.url ?? '';

export interface WebhookEvent {
  type: string;
  description: string;
  sessionStatus: string;
  example: unknown;
}

export const events: WebhookEvent[] = Object.entries(doc.webhooks).map(([type, w]) => ({
  type,
  description: w.post.description ?? '',
  sessionStatus: w.post['x-session-status'] ?? '',
  example: w.post.requestBody?.content['application/json'].example,
}));

export function schemaByName(name: string): Schema | undefined {
  return doc.components.schemas[name];
}

export function resolve(s: Schema): Schema {
  let cur = s;
  for (let i = 0; i < 10 && cur.$ref; i++) {
    const next = schemaByName(cur.$ref.replace('#/components/schemas/', ''));
    if (!next) break;
    cur = next;
  }
  return cur;
}

/** A short type label for a schema, for parameter tables: "string", "integer | null", "Session". */
export function typeLabel(s: Schema): string {
  if (s.$ref) return s.$ref.replace('#/components/schemas/', '');
  if (s.oneOf) return s.oneOf.map(typeLabel).join(' | ');
  if ((s as { const?: unknown }).const !== undefined) return JSON.stringify((s as { const?: unknown }).const);
  const types = Array.isArray(s.type) ? s.type : s.type ? [s.type] : [];
  return types
    .map((t) => {
      if (t === 'array' && s.items) return 'array of ' + typeLabel(s.items);
      if (t === 'string' && s.format === 'decimal') return 'decimal string';
      if (t === 'string' && s.format === 'date-time') return 'timestamp';
      if (t === 'string' && s.enum) return 'enum';
      return t;
    })
    .join(' | ');
}

/* Examples are written in test mode. Live mode shows the same objects on the live networks. */
const LIVE_STRINGS: [string, string][] = [
  ['base-sepolia', 'base'],
  ['solana-devnet', 'solana'],
  ['0x036CbD53842c5426634e7929541eC2318f3dCF7e', '0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913'],
  ['4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU', 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v'],
];
const BASE_SEPOLIA_ID = 84532;
const BASE_ID = 8453;

function liveString(s: string): string {
  for (const [test, live] of LIVE_STRINGS) if (s === test) return live;
  return s
    .replace(/\bsk_test_/g, 'sk_live_')
    .replace(/\bbase-sepolia\b/g, 'base')
    .replace(/\bsolana-devnet\b/g, 'solana');
}

/** The example as live mode would answer it: live networks, tokens and chain ids, livemode true. */
export function toLive(value: unknown, key = ''): unknown {
  if (Array.isArray(value)) return value.map((v) => toLive(v));
  if (value && typeof value === 'object') {
    const out: Record<string, unknown> = {};
    const obj = value as Record<string, unknown>;
    const isDomain = 'verifyingContract' in obj && 'chainId' in obj;
    for (const [k, v] of Object.entries(obj)) {
      let next = toLive(v, k);
      if (k === 'livemode' && v === false) next = true;
      if ((k === 'chainId' || k === 'evm_chain_id') && v === BASE_SEPOLIA_ID) next = BASE_ID;
      if (isDomain && k === 'name' && v === 'USDC') next = 'USD Coin';
      out[liveString(k)] = next;
    }
    return out;
  }
  if (typeof value === 'string') return key === 'url' || key === 'checkout_url' ? value : liveString(value);
  return value;
}

/** The example in the chosen mode. The network registry lists both modes already. */
export function exampleFor(op: Operation, value: unknown, live: boolean): unknown {
  if (!live || op.id === 'listNetworks') return value;
  return toLive(value);
}

export function rateLimitText(r: RateLimit | null): string {
  if (!r) return 'none';
  const per = r.per === 'key' ? 'per key' : r.per === 'ip' ? 'per IP' : 'per merchant';
  return `${r.per_minute} requests/min ${per}, bursts of ${r.burst}`;
}
