import { esc, highlightCode, highlightJSON, md, pathHtml, pretty } from './format';
import { DEPLOYMENTS, type Contract } from './contracts/addresses';
import { exampleKey, LANGS, snippet, type Lang } from './snippets';
import {
  apiBase,
  errorCodes,
  events,
  exampleFor,
  groups,
  operations,
  rateLimitText,
  resolve,
  schemaByName,
  toLive,
  typeLabel,
  type ErrorRow,
  type Operation,
  type Param,
  type Schema,
} from './spec';
import { HANDLERS, HOOK_LANGS, type HookLang } from './webhook-handlers';

export interface ViewState {
  live: boolean;
  lang: Lang;
  status: Record<string, string>;
  event: string;
  hook: HookLang;
}

export const DASHBOARD_KEYS = '/dashboard/developers/api-keys';
/** The create-endpoint example's secret, so the checker starts from values the reference shows. */
const SAMPLE_SECRET = (() => {
  const create = groups.flatMap((g) => g.ops).find((o) => o.id === 'createWebhookEndpoint');
  return String((create?.responses[0]?.example as { secret?: string } | undefined)?.secret ?? 'whsec_test');
})();
export const DASHBOARD_WEBHOOKS = '/dashboard/developers/webhooks';

/* ---------- shared pieces ---------- */

export function codeBox(title: string, text: string, json = false, attrs = ''): string {
  return `<div class="codebox"${attrs}><div class="codebox-head"><span class="title">${title}</span><button class="copy" type="button">Copy</button></div><pre>${json ? highlightJSON(text) : highlightCode(text)}</pre></div>`;
}

interface Row {
  name: string;
  type: string;
  description: string;
  loc?: string;
  required?: boolean;
  values?: unknown[];
}

function rows(list: Row[]): string {
  return (
    '<div class="params">' +
    list
      .map(
        (p) =>
          `<div class="param"><div class="param-head"><code>${esc(p.name)}</code><span class="type">${esc(p.type)}</span>` +
          (p.loc ? `<span class="loc">${p.loc}</span>` : '') +
          (p.required ? '<span class="req">Required</span>' : '') +
          `</div><p>${md(p.description)}</p>` +
          (p.values?.length ? `<div class="enum">${p.values.filter((v) => v !== null).map((v) => `<code class="inline-code">${esc(v)}</code>`).join('')}</div>` : '') +
          '</div>',
      )
      .join('') +
    '</div>'
  );
}

const paramRow = (p: Param): Row => ({ name: p.name, type: typeLabel(p.schema), description: p.description ?? '', loc: p.in, required: p.required, values: p.schema.enum });

function bodyRows(schema: Schema): Row[] {
  const s = resolve(schema);
  return Object.entries(s.properties ?? {}).map(([name, p]) => ({
    name,
    type: typeLabel(p),
    description: p.description ?? '',
    required: s.required?.includes(name),
    values: p.enum ?? p.items?.enum,
  }));
}

function errorTable(list: ErrorRow[]): string {
  return `<table class="errors"><tbody>${list.map((r) => `<tr><td>${r.status}</td><td><code>${esc(r.code)}</code></td><td>${md(r.description)}</td></tr>`).join('')}</tbody></table>`;
}

export const ICON_INFO =
  '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 8v5M12 16h.01"/></svg>';

/* ---------- endpoint pages ---------- */

export function langTabs(state: ViewState): string {
  return `<div class="ltabs" role="group" aria-label="Language">${LANGS.map(([k, n]) => `<button type="button" data-lang="${k}" aria-pressed="${state.lang === k}">${n}</button>`).join('')}</div>`;
}

export function requestBox(op: Operation, state: ViewState): string {
  return `<div class="codebox" data-box="req"><div class="codebox-head">${langTabs(state)}<button class="copy" type="button">Copy</button></div><pre>${highlightCode(snippet(op, state.lang, state.live))}</pre></div>`;
}

export function responseBox(op: Operation, state: ViewState): string {
  const shown = state.status[op.id] ?? op.responses[0]!.status;
  const res = op.responses.find((r) => r.status === shown) ?? op.responses[0]!;
  return (
    `<div class="codebox" data-box="res"><div class="codebox-head"><span class="title">Response</span><div class="status-row" role="group" aria-label="Status code">` +
    op.responses.map((r) => `<button type="button" data-status="${r.status}" aria-pressed="${r.status === res.status}">${r.status}</button>`).join('') +
    `</div><button class="copy" type="button">Copy</button></div><pre>${highlightJSON(pretty(exampleFor(op, res.example, state.live)))}</pre></div>`
  );
}

function tryBox(op: Operation, state: ViewState): string {
  const pathParams = op.params.filter((p) => p.in === 'path');
  const body = op.body?.example === undefined ? '' : pretty(exampleFor(op, op.body.example, state.live));
  return (
    '<section class="try" aria-label="Try it"><div class="try-head">' +
    '<svg class="icon-accent" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="m6 4 14 8-14 8z"/></svg>' +
    '<strong>Try it</strong><span class="note">Sample responses</span></div><div class="try-body">' +
    (op.isPublic ? '' : `<div class="field"><label for="try-key">Secret key</label><input id="try-key" value="${exampleKey(state.live)}" spellcheck="false" autocomplete="off"></div>`) +
    pathParams.map((p) => `<div class="field"><label for="try-${p.name}">${p.name}</label><input id="try-${p.name}" data-path="${p.name}" value="${esc(p.example ?? '')}" spellcheck="false"></div>`).join('') +
    (op.body ? `<div class="field"><label for="try-body">Body</label><textarea id="try-body" spellcheck="false">${esc(body)}</textarea></div>` : '') +
    (op.testOnly && state.live ? '<p class="warn-note">Live mode: this endpoint answers 403 test_mode_only.</p>' : '') +
    `<div class="try-actions"><button class="btn primary" id="try-send" type="button">Show response</button><span class="hint">Checks the key and the ids like the API does, then shows the documented answer. Nothing is sent.</span></div>` +
    '<p class="try-status" id="try-status" role="status" aria-live="polite"></p>' +
    '</div><div id="try-out" hidden></div></section>'
  );
}

export function renderEndpoint(op: Operation, state: ViewState): string {
  const group = groups.find((g) => g.name === op.tag);
  const otherParams = op.params.map(paramRow);
  const body = op.body ? bodyRows(op.body.schema) : [];
  const tags =
    (op.isPublic ? '<span class="tag">Auth <b>none</b>, public</span>' : `<span class="tag">Scope <b class="mono">${esc(op.scope)}</b></span>`) +
    (op.idempotency ? '<span class="tag">Idempotency-Key <b>required</b></span>' : '') +
    `<span class="tag">Rate limit <b>${rateLimitText(op.rateLimit)}</b></span>` +
    (op.testOnly ? '<span class="tag">Test mode only</span>' : '');
  return (
    `<div class="crumb">${esc(op.tag)}</div><h1>${esc(op.summary)}</h1>` +
    `<div class="endpoint-line"><span class="method ${op.method}">${op.method}</span><span class="path">${pathHtml(op.path)}</span></div>` +
    `<div class="tags">${tags}</div>` +
    '<div class="split"><div class="prose">' +
    `<p class="lede">${md(op.description || group?.description || '')}</p>` +
    (otherParams.length ? `<h2>Parameters</h2>${rows(otherParams)}` : '') +
    (body.length ? `<h2>Body</h2>${rows(body)}` : '') +
    (op.errors.length ? `<h2>Errors</h2>${errorTable(op.errors)}` : '') +
    `<h2>Try it</h2>${tryBox(op, state)}` +
    `</div><div class="sticky">${requestBox(op, state)}${responseBox(op, state)}</div></div>`
  );
}

/* The merchant simulator refuses live mode before it reads the body (internal/api/sandbox.go). */
const TEST_MODE_BEFORE_BODY = new Set(['simulateSession']);

/* Fields the handlers read on purpose to refuse them with their own message (internal/service/payment_links.go). */
const FIELD_REFUSALS: Record<string, Record<string, string>> = {
  createPaymentLink: { active: 'a new link is always active; deactivate it afterwards' },
  updatePaymentLink: { product_id: 'product_id cannot be changed; create a link for the other product' },
};

export interface SampleAnswer {
  status: string;
  body: unknown;
  /** The mode the answer is in: the key's, which may differ from the page's. */
  live: boolean;
}

/**
 * What the API would answer for the inputs on the page, from the documented examples, with the
 * API's checks in its order: the key (merchant routes), the body, then the id and the test mode
 * refusal. Merchant sandbox tools refuse live mode before loading anything; the checkout test
 * panel loads the session first.
 */
export function sampleAnswer(op: Operation, state: ViewState, input: { key: string | null; path: Record<string, string>; body: string | null }): SampleAnswer {
  const err = (code: string, message: string) => ({ error: { code, message } });
  let live = state.live;
  if (input.key !== null) {
    if (!input.key) return { status: '401', body: err('invalid_api_key', 'missing Bearer API key'), live };
    if (!/^sk_(live|test)_\S+$/.test(input.key)) return { status: '401', body: err('invalid_api_key', 'invalid API key'), live };
    live = input.key.startsWith('sk_live_');
  }
  const testOnly = (): SampleAnswer | null => {
    if (!op.testOnly || !live) return null;
    const res = op.responses.find((r) => r.status === '403');
    return { status: '403', body: res?.example ?? err('test_mode_only', 'test mode only'), live };
  };
  if (TEST_MODE_BEFORE_BODY.has(op.id)) {
    const refused = testOnly();
    if (refused) return refused;
  }
  if (input.body !== null && input.body.trim() !== '') {
    let parsed: unknown;
    try {
      parsed = JSON.parse(input.body);
    } catch {
      parsed = undefined;
    }
    // Like Go's json.Unmarshal into a map, null is an empty body.
    if (parsed !== null && (typeof parsed !== 'object' || Array.isArray(parsed))) {
      return { status: '400', body: err('invalid_request', 'request body must be a JSON object'), live };
    }
    const allowed = op.body ? Object.keys(resolve(op.body.schema).properties ?? {}) : [];
    const unknown = Object.keys(parsed ?? {}).find((k) => !allowed.includes(k));
    if (unknown !== undefined) {
      const refusal = FIELD_REFUSALS[op.id]?.[unknown];
      return { status: '400', body: { error: refusal ? { code: 'invalid_request', message: refusal, param: unknown } : { code: 'invalid_request', message: `unknown field ${JSON.stringify(unknown)}` } }, live };
    }
  }
  const notFound = (): SampleAnswer | null => {
    for (const p of op.params.filter((x) => x.in === 'path')) {
      const prefix = typeof p.example === 'string' ? p.example.slice(0, p.example.indexOf('_') + 1) : '';
      if (op.notFound && prefix && !(input.path[p.name] ?? '').startsWith(prefix)) {
        return { status: '404', body: err(op.notFound.code, op.notFound.message), live };
      }
    }
    return null;
  };
  const refusal = op.isPublic ? notFound() ?? testOnly() : testOnly() ?? notFound();
  if (refusal) return refusal;
  const ok = op.responses[0]!;
  return { status: ok.status, body: exampleFor(op, ok.example, live), live };
}

/* ---------- guides ---------- */

export function renderOverview(state: ViewState): string {
  const live = state.live;
  const create = groups.flatMap((g) => g.ops).find((o) => o.id === 'createSession');
  const opened = create ? (exampleFor(create, create.responses[0]!.example, live) as Record<string, unknown>) : {};
  const excerpt = { id: opened.id, version: opened.version, status: opened.status, amount: opened.amount, currency: opened.currency, checkout_url: opened.checkout_url, pay_to: opened.pay_to, expires_at: opened.expires_at };
  return (
    '<div class="crumb">Prismaport API v1</div><h1>Get paid in stablecoins from your own code</h1>' +
    '<p class="lede">Create a product, open a session for each order, and get a signed webhook when exactly the right amount is confirmed. Payments go to your own wallets, never to an account we hold, and the fee is split on-chain where anyone can read it.</p>' +
    '<div class="grid3">' +
    '<a class="card" href="#createProduct"><span class="step">Step 1</span><h3>Create a product</h3><p>Name, price, and which wallets get paid on each network.</p></a>' +
    '<a class="card" href="#createSession"><span class="step">Step 2</span><h3>Open a session per order</h3><p>Send the payer to <code class="inline-code">checkout_url</code>, or use a payment link.</p></a>' +
    '<a class="card" href="#webhooks"><span class="step">Step 3</span><h3>Handle session.paid</h3><p>Verify the signature, dedupe by event id, read the session, then fulfil.</p></a>' +
    '</div><div class="split"><div class="prose">' +
    `<h2>Base URL</h2><dl class="kv"><dt>API</dt><dd><code class="inline-code">${esc(apiBase)}</code></dd>` +
    '<dt>Format</dt><dd>JSON over HTTPS. Amounts are decimal strings, never floats; a USD amount you send is at most 10000000000000.00.</dd>' +
    `<dt>Networks</dt><dd>${live ? 'Base' : 'Base Sepolia (test mode)'}</dd>` +
    `<dt>Assets</dt><dd>${live ? 'USDC on Base' : 'USDC on Base Sepolia'}</dd>` +
    '<dt>Contract</dt><dd><a href="openapi.json" download>OpenAPI 3.1 (openapi.json)</a>, to generate a client.</dd></dl>' +
    '<h2>How money moves</h2><p>Sessions are priced in USD and paid 1:1 in stablecoins. Each session can get its own deposit address on every network it offers, and that address can only pay out to the wallets in your split and the fee, fixed when it was derived. Nobody can redirect a payment in flight.</p>' +
    '<ul><li><b>Deposit address.</b> Once the session closes (paid, overpaid, expired with funds or late payment) and its payments are confirmed, one transaction forwards the balance: the fee to Prismaport and each share to its wallet.</li>' +
    '<li><b>Signed payment.</b> On Base the payer can sign instead of sending. One transaction then pays the fee and every share directly; Prismaport pays the network fee.</li></ul>' +
    `<div class="callout">${ICON_INFO}<span><b>Exact amounts.</b> A 25.00 USD session on ${live ? 'Base' : 'Base Sepolia'} is paid when exactly 25.000000 USDC has been confirmed. Less is <code class="inline-code">partially_paid</code>, more is <code class="inline-code">overpaid</code>.</span></div>` +
    '<h2>Choose, then generate</h2><p>A new session has no deposit address yet. The payer opens <code class="inline-code">checkout_url</code>, sees the networks, assets and amounts the session offers, picks one and selects <b>Generate payment</b>. Only then does the deposit address of that network exist in the answer, and Prismaport starts watching it. Until then every <code class="inline-code">pay_to</code> entry has <code class="inline-code">revealed: false</code> and no <code class="inline-code">address</code>.</p>' +
    '<ul><li><b>Selection window.</b> A session nobody generates a payment for closes 1 hour after it was created. Nothing is watched during that time.</li>' +
    '<li><b>Pay window.</b> The first Generate payment sets <code class="inline-code">payment_started_at</code> and moves <code class="inline-code">expires_at</code> to that moment plus <code class="inline-code">pay_window_seconds</code>, the <code class="inline-code">expires_in</code> you sent (default 30 minutes). Generating another network later keeps every network already generated watched and does not move <code class="inline-code">expires_at</code>.</li>' +
    '<li><b>Your own payment page.</b> Call <a href="#revealSession">POST /v1/sessions/{id}/reveal</a> with <code class="inline-code">{"chain": "base"}</code> to get the details of one network. Asking again for a network already generated returns the same details. A wallet payment from the hosted page generates its network on its own.</li>' +
    '<li><b>Hosted checkout.</b> The payer can choose another network after generating one with <b>Pay another way</b>; the first address keeps being watched until the session closes. The public route <a href="#revealCheckoutSession">POST /v1/checkout/sessions/{id}/reveal</a> backs this.</li></ul>' +
    '<h2>Fee</h2><p>0.10 in the token paid plus 0.5% of each settlement, the percentage rounded down: <span class="mono">fee = fixed_fee + gross × fee_bps / 10 000</span>. For 25.00 USDC on Base you receive 24.775 USDC. A balance whose fee would be at or above the gross is too small to settle and stays at the deposit address.</p>' +
    '</div><div class="sticky">' +
    codeBox(
      'Quickstart · cURL',
      `curl -X POST ${apiBase}/v1/sessions \\\n  -H "Authorization: Bearer ${exampleKey(live)}" \\\n  -H "Idempotency-Key: order-42-attempt-1" \\\n  -H "Content-Type: application/json" \\\n  -d '{"product_id":"${String((opened as { product_id?: string }).product_id ?? '')}","client_reference_id":"order-42"}'`,
    ) +
    codeBox('Response · 200 (excerpt)', pretty(excerpt), true) +
    '</div></div>'
  );
}

/** Every scope the operations name, in the document's order, with the operations it opens. */
function scopeTable(): [string, Operation[]][] {
  const by = new Map<string, Operation[]>();
  for (const o of operations) if (o.scope) by.set(o.scope, [...(by.get(o.scope) ?? []), o]);
  return [...by];
}

export function renderKeys(state: ViewState): string {
  const scopes = scopeTable();
  return (
    '<div class="crumb">Start here</div><h1>Authentication</h1><p class="lede">Send a secret key as a bearer token from your server. A key in a browser or a mobile app is a leaked key.</p>' +
    '<div class="split"><div class="prose">' +
    '<h2>Key format</h2><p><code>sk_test_</code> or <code>sk_live_</code> followed by base58 of 32 random bytes. The prefix sets the mode: a test key never sees live data. We store only a SHA-256 hash, so the full key shows once, when you create or roll it. Lists show the first 12 characters.</p>' +
    '<p>A missing, malformed, unknown, revoked or expired key answers <code>401 invalid_api_key</code>. A valid key asking for an object of the other mode gets the route\'s 404.</p>' +
    `<h2>Scopes</h2><p>A full-access key holds all ${scopes.length}. Restrict a key to what one service needs; any other route answers <code>403 insufficient_scope</code>. The public checkout routes take no key. Wallets have no write scope: add and archive them in the dashboard.</p>` +
    `<table class="errors" id="scopes"><tbody>${scopes.map(([scope, ops]) => `<tr><td></td><td><code>${esc(scope)}</code></td><td>${ops.map((o) => `<a href="#${o.id}">${esc(o.summary)}</a>`).join(', ')}</td></tr>`).join('')}</tbody></table>` +
    '<h2>Rotation</h2><p>Create, roll and revoke keys in the dashboard. When you roll a key, the old one keeps working for the overlap you pick (now, 1 hour, 24 hours or 7 days), so you can deploy the new one first. Creating or rolling a live key asks for your password, plus your 2FA code when 2FA is on.</p>' +
    '<h2>Dashboard step-up</h2><p>The same password, plus the 2FA code when 2FA is on, is asked on each request that creates or rolls a live key, adds or archives a wallet, or creates a live webhook endpoint, moves it to another URL or rotates its secret. It is checked per request: there is no elevated session to expire. Test keys and test endpoints ask for nothing. A 2FA recovery code never passes the step-up for a live key or a wallet.</p>' +
    '<p>For 48 hours after a lost authenticator is removed, creating or rolling a live key and adding a live wallet answer <code>403 recovery_locked</code>. API keys cannot manage keys or wallets. A key with <code>webhooks:write</code> manages webhook endpoints with no step-up, so give that scope only to the service that needs it.</p>' +
    '<p>Rate limits per key: <a href="#conventions">Errors and conventions</a>.</p>' +
    '</div><div class="sticky">' +
    `<section class="try"><div class="try-head"><strong>Your keys</strong></div><div class="try-body"><p class="aside-note">Keys are created and shown once in the dashboard, under Developers, API keys.</p><div class="try-actions"><a class="btn primary" href="${DASHBOARD_KEYS}">Open API keys</a></div></div></section>` +
    codeBox('Every request', `Authorization: Bearer ${exampleKey(state.live)}`) +
    '</div></div>'
  );
}

/* What a code means wherever it appears, for codes each operation words for its own case. */
const ERROR_MEANING: Record<string, string> = {
  invalid_request: 'A field, parameter or header is missing, malformed or unknown. Some add `param` with its name.',
  insufficient_scope: 'The key lacks the route scope.',
  test_mode_only: 'A sandbox tool called in live mode.',
  amount_above_maximum: 'Over the product range.',
  wallet_blacklisted: 'A payout wallet is blacklisted on a network the call would pay.',
  not_found: 'Unknown wallet or webhook endpoint, or one of the other mode. Unknown routes answer it too.',
  internal_error: 'Our fault. Retry; on a create that takes one, with the same Idempotency-Key.',
  service_unavailable: 'Something the call needs is not available on that network right now: deposit addresses, the blacklist check or a chain provider. Retry later.',
  chain_unavailable: 'Payments on that network, or of that stablecoin on it, are paused right now: use another network, or retry later. Funds already sent to a deposit address shown before the pause are still credited.',
};

/** Every code in the document's x-errors, with its statuses and the operations that answer it. */
function errorCatalog(): string {
  const where = (ops: Operation[]) =>
    ops.length === operations.length
      ? 'Every operation.'
      : ops.length <= 4
        ? `${ops.map((o) => `<a href="#${o.id}">${esc(o.summary)}</a>`).join(', ')}.`
        : `${ops.length} operations.`;
  return `<table class="errors" id="error-codes"><tbody>${errorCodes
    .map((e) => `<tr><td>${e.statuses.join(' ')}</td><td><code>${esc(e.code)}</code></td><td>${md(ERROR_MEANING[e.code] ?? e.description)}<span class="where">${where(e.ops)}</span></td></tr>`)
    .join('')}</tbody></table>`;
}

export function renderConventions(): string {
  const tr = (r: [string, string, string]) => `<tr><td>${r[0]}</td><td><code>${esc(r[1])}</code></td><td>${md(r[2])}</td></tr>`;
  const pages: [string, string, string][] = [
    ['', 'GET /v1/products', '`{data, has_more}`. Next page: `starting_after` = the last id. `limit` 1 to 100, default 20.'],
    ['', 'GET /v1/payment_links', '`{data, next_cursor}`. Next page: `cursor` = `next_cursor`, `null` on the last page. `limit` 1 to 100, default 20.'],
    ['', 'GET /v1/customer_addresses', '`{data, next_cursor}`, newest first. Next page: `cursor` = `next_cursor`, `null` on the last page. `limit` 1 to 100, default 20.'],
    ['', 'GET /v1/customer_addresses/{id}/sessions', '`{data, next_cursor}`, newest first. Next page: `cursor` = `next_cursor`. `limit` 1 to 100, default 20.'],
    ['', 'GET /v1/events', '`{data, has_more, next_cursor}`. Next page: `after` = `next_cursor`, a seq that is never null; `after=latest` starts at the newest event. `limit` 1 to 200, default 50.'],
    ['', 'GET /v1/wallets', '`{data}`, one page. Filter with `family` and `include_archived`.'],
    ['', 'GET /v1/webhook_endpoints', '`{data, has_more}`, one page (at most 16 endpoints per mode); `has_more` is always false.'],
  ];
  const ids: [string, string][] = [
    ['prod_', 'Product'],
    ['plink_', 'Payment link'],
    ['ses_', 'Session'],
    ['pay_', 'Payment'],
    ['evt_', 'Event'],
    ['wal_', 'Wallet'],
    ['we_', 'Webhook endpoint'],
    ['whd_', 'Webhook delivery'],
    ['txj_', 'Signed payment job'],
    ['key_', 'API key (in logs and the dashboard)'],
    ['whsec_', 'Webhook signing secret'],
  ];
  return (
    '<div class="crumb">Start here</div><h1>Errors and conventions</h1><p class="lede">One error shape everywhere, idempotent session and link creates, cursor pagination, rate limits, exact decimal amounts and versions.</p>' +
    '<div class="split"><div class="prose">' +
    '<h2>Errors</h2><p>Every error has a stable <code>code</code> to branch on and a <code>message</code> written for people. Some validation errors add <code>param</code> with the field name. Every code an operation answers, from its error table:</p>' +
    errorCatalog() +
    '<h2>Idempotency</h2><p><code>Idempotency-Key</code> (at most 255 characters) is required on <a href="#createSession">POST /v1/sessions</a>, <a href="#createPaymentLink">POST /v1/payment_links</a> and the public <a href="#openLinkSession">link open</a>. The same key with the same body returns the same object again with <code>Idempotent-Replayed: true</code>; the same key with a different body or on another endpoint is <code>409 idempotency_conflict</code>. <a href="#createCustomerAddress">POST /v1/customer_addresses</a> takes it optionally (the call already answers the customer\'s existing address); other creates ignore the header.</p>' +
    "<p>Only a successful answer is kept, so a call that failed can be retried with the same key. Two calls with one key at once do not both run: the second waits and gets the first one's answer. Sessions and payment links share one key space, so use different keys for each.</p>" +
    `<h2>Pagination</h2><table class="errors"><tbody>${pages.map(tr).join('')}</tbody></table>` +
    '<h2>Rate limits</h2><p>Per key, reads (GET) and writes (everything else) have separate buckets: 600 reads and 120 writes per minute, with bursts of 200 and 60. The public checkout routes are limited per IP, each in its own bucket; every endpoint page shows its limit. Over the limit answers <code>429 rate_limited</code> with <code>Retry-After</code>, in whole seconds, at least 1. There are no <code>X-RateLimit-*</code> headers: wait for <code>Retry-After</code>, then retry.</p>' +
    '<h2>Amounts</h2><p>Strings, so nothing rounds through a float. <code>amount</code> is USD with two decimals (<code>"25.00"</code>). <code>amount_received</code> and <code>amount_remaining</code> keep any digit past the cent (<code>"25.000001"</code> after an overpayment of one unit). Token amounts use the token decimals (<code>"25.000000"</code> USDC).</p>' +
    `<h2>Ids</h2><p>A prefix plus a 26-character ULID, so ids sort by creation time.</p><dl class="kv">${ids.map(([p, d]) => `<dt><code class="inline-code">${p}</code></dt><dd>${d}</dd>`).join('')}</dl>` +
    '<h2>Versions</h2><p>Routes carry the API version in the path, <code>/v1</code>. New fields, events and error codes can appear in any answer: ignore fields you do not know, and branch on <code>code</code>, never on <code>message</code>. <code>GET /version</code> answers the running release, its commit and build time.</p>' +
    '</div><div class="sticky">' +
    codeBox('400 · error', pretty({ error: { code: 'invalid_request', message: 'amount applies only to fixed prices; use min_amount' } }), true) +
    codeBox('401 · error', pretty({ error: { code: 'invalid_api_key', message: 'invalid API key' } }), true) +
    codeBox(
      'Replay · the same key and body',
      `curl -i -X POST ${apiBase}/v1/sessions \\\n  -H "Authorization: Bearer ${exampleKey(false)}" \\\n  -H "Idempotency-Key: order-42-attempt-1" \\\n  -H "Content-Type: application/json" \\\n  -d '{"product_id":"prod_01M5MXJAAGCXAAZ5HJM9T5X1E2","client_reference_id":"order-42"}'\n\nHTTP/2 200\nidempotent-replayed: true\n\n{"id": "ses_01M5MYPW80FMXFE81YHNB8MPR8", …}  # the session of the first call`,
    ) +
    codeBox('Next page · events', `GET /v1/events?after=1042&limit=50\n\n${pretty({ data: ['…'], has_more: true, next_cursor: '1092' })}`) +
    codeBox('429 · error', pretty({ error: { code: 'rate_limited', message: 'too many requests with this API key; retry after the Retry-After delay' } }), true) +
    codeBox(
      'Back off on 429 · Node',
      "async function withRetry(call, tries = 5) {\n  for (let i = 1; ; i++) {\n    const res = await call();\n    if (res.status !== 429 || i === tries) return res;\n    const wait = Number(res.headers.get('Retry-After')) || 1; // whole seconds\n    await new Promise((r) => setTimeout(r, wait * 1000));\n  }\n}",
    ) +
    '</div></div>'
  );
}

/* Session states as the state machine derives them (internal/session). */
export function stateDiagram(): string {
  type Node = [x: number, y: number, label: string, tone: 'paid' | 'partial' | 'idle'];
  const nodes: Record<string, Node> = {
    open: [80, 70, 'Open', 'idle'],
    pp: [330, 70, 'Partially paid', 'partial'],
    paid: [600, 70, 'Paid', 'paid'],
    over: [600, 180, 'Overpaid', 'paid'],
    exp: [80, 280, 'Expired', 'idle'],
    late: [330, 280, 'Late payment', 'partial'],
    rev: [620, 280, 'Under review', 'partial'],
  };
  const width = (k: string) => nodes[k]![2].length * 7.6 + 28;
  const left = (k: string) => nodes[k]![0] - width(k) / 2;
  const right = (k: string) => nodes[k]![0] + width(k) / 2;
  const node = (k: string) => {
    const [x, y, label, tone] = nodes[k]!;
    const w = width(k);
    return `<g class="dg-node dg-${tone}"><rect x="${x - w / 2}" y="${y - 15}" width="${w}" height="30" rx="15"/><text x="${x}" y="${y + 4.5}" text-anchor="middle">${label.toUpperCase()}</text></g>`;
  };
  const line = (x1: number, y1: number, x2: number, y2: number) => `<line class="dg-edge" x1="${x1}" y1="${y1}" x2="${x2}" y2="${y2}" marker-end="url(#dg-arrow)"/>`;
  const label = (x: number, y: number, t: string, anchor = 'middle') => `<text class="dg-label" x="${x}" y="${y}" text-anchor="${anchor}">${t}</text>`;
  return (
    '<div class="diagram" role="img" aria-label="Session states. After each confirmed payment the counted sum decides the status: below the amount is Partially paid, equal is Paid, above is Overpaid. Open and Partially paid become Expired when the expiry passes; a payment confirmed after that makes Late payment. Under review can be set from any state. A reorg reverses a detected or confirmed payment and the status is recomputed.">' +
    '<svg viewBox="0 0 760 356"><defs><marker id="dg-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path class="dg-arrow" d="M0 0 10 5 0 10z"/></marker></defs>' +
    line(right('open'), 70, left('pp') - 2, 70) + label((right('open') + left('pp')) / 2, 62, 'sum &lt; amount') +
    line(right('pp'), 70, left('paid') - 2, 70) + label((right('pp') + left('paid')) / 2, 62, 'sum = amount') +
    '<path class="dg-edge" d="M80 55 C 200 0, 480 0, 600 53" fill="none" marker-end="url(#dg-arrow)"/>' + label(340, 34, 'sum = amount') +
    line(600, 85, 600, 163) + label(608, 128, 'sum &gt; amount', 'start') +
    line(370, 85, left('over') + 4, 172) + label(478, 120, 'sum &gt; amount', 'start') +
    line(110, 85, left('over') - 2, 184) + label(420, 176, 'sum &gt; amount') +
    line(80, 85, 80, 263) + label(88, 200, 'expiry', 'start') +
    line(300, 85, 108, 266) + label(200, 200, 'expiry', 'start') +
    line(right('exp'), 280, left('late') - 2, 280) + label((right('exp') + left('late')) / 2, 272, 'confirmed') +
    Object.keys(nodes).map(node).join('') +
    label(620, 312, 'from any state') +
    '<text class="dg-note" x="380" y="330" text-anchor="middle">A payment counts after 10 blocks on Base.</text>' +
    '<text class="dg-note" x="380" y="346" text-anchor="middle">A reorg reverses a detected or confirmed payment and the status is recomputed.</text>' +
    '</svg></div>'
  );
}

const STATUS_PILL: Record<string, [string, string]> = {
  partially_paid: ['partial', 'Partially paid'],
  paid: ['paid', 'Paid'],
  overpaid: ['paid', 'Overpaid'],
  expired: ['idle', 'Expired'],
  late_payment: ['partial', 'Late payment'],
  under_review: ['partial', 'Under review'],
};

export function eventPayloadBox(state: ViewState): string {
  const ev = events.find((e) => e.type === state.event) ?? events[0]!;
  const example = state.live ? toLive(ev.example) : ev.example;
  return codeBox(`POST your endpoint · ${esc(ev.type)}`, pretty(example), true, ' id="ev-payload"');
}

export function handlerBox(state: ViewState): string {
  const tabs = HOOK_LANGS.map(([k, n]) => `<button type="button" data-hook="${k}" aria-pressed="${state.hook === k}">${n}</button>`).join('');
  return `<div class="codebox" data-box="hook"><div class="codebox-head"><div class="ltabs" role="group" aria-label="Handler language">${tabs}</div><button class="copy" type="button">Copy</button></div><pre>${highlightCode(HANDLERS[state.hook])}</pre></div>`;
}

export function renderWebhooks(state: ViewState): string {
  const paidEvent = (events.find((e) => e.type === 'session.paid')?.example ?? {}) as object;
  const list = events
    .map((e) => {
      const pill = STATUS_PILL[e.sessionStatus];
      const status = pill ? `<span class="pill ${pill[0]}">${pill[1]}</span>` : `<span class="ev-note">status ${e.sessionStatus === 'unchanged' ? 'unchanged' : 'recomputed'}</span>`;
      return `<button type="button" class="ev" data-ev="${esc(e.type)}" aria-pressed="${e.type === state.event}"><code>${esc(e.type)}</code>${status}<span class="d">${md(e.description)}</span></button>`;
    })
    .join('');
  return (
    '<div class="crumb">Guides</div><h1>Webhooks and events</h1>' +
    '<p class="lede">We POST a signed event to your endpoint when a session changes status or a payment is detected or reversed. Finalization and settlement change the session (its <code class="inline-code">version</code> grows, <code class="inline-code">settlement</code> fills) but send no event: read the session when you need them. Deliveries are at least once and may arrive out of order, so dedupe by <code class="inline-code">id</code> and apply only a higher <code class="inline-code">data.session.version</code>. What each event means for an order, with the state diagram: <a href="#lifecycle">Payment lifecycle</a>.</p>' +
    '<div class="split"><div class="prose">' +
    `<h2>Event types</h2><div class="events-list" role="group" aria-label="Event types">${list}</div>` +
    '<h2>Delivery</h2><dl class="kv">' +
    '<dt>Headers</dt><dd><code class="inline-code">Signature</code> <code class="inline-code">Prismaport-Event-Id</code> <code class="inline-code">User-Agent: Prismaport-Webhooks/1</code></dd>' +
    '<dt>Success</dt><dd>Any 2xx within 10 seconds. Answer first, then do the work. Redirects count as failures.</dd>' +
    '<dt>Retries</dt><dd>12 attempts, 30 s doubling up to 6 h with 10% jitter, about 14.5 hours in total. Then the dead letter, where you can resend from the dashboard.</dd>' +
    '<dt>Rotation</dt><dd>For 24 hours after a rotation both secrets sign, as two <code class="inline-code">v1</code> values.</dd>' +
    `<dt>Endpoints</dt><dd>Add them with <a href="#createWebhookEndpoint">POST /v1/webhook_endpoints</a> or in the dashboard under <a href="${DASHBOARD_WEBHOOKS}">Developers, Webhooks</a>, which also has a sandbox inbox. In live mode the dashboard asks for your password to create an endpoint, move it to another URL or rotate its secret.</dd>` +
    '<dt>Safety net</dt><dd><a href="#listEvents">GET /v1/events</a> serves the same bytes in one global order.</dd></dl>' +
    '<h2>Check a signature</h2><p>Paste a secret and a raw body to see the header we would send, or paste a header to check it with the same rules as the handlers on this page. Runs in your browser; nothing is sent.</p>' +
    '<div class="verify">' +
    `<div class="field"><label for="v-secret">Signing secret</label><input id="v-secret" value="${SAMPLE_SECRET}" spellcheck="false" autocomplete="off"></div>` +
    `<div class="field"><label for="v-t">Timestamp (t)</label><input id="v-t" value="${Math.floor(Date.now() / 1000)}" spellcheck="false" inputmode="numeric"></div>` +
    `<div class="field"><label for="v-body">Raw body</label><textarea id="v-body" spellcheck="false">${esc(JSON.stringify({ id: (paidEvent as { id?: string }).id, type: 'session.paid', created_at: (paidEvent as { created_at?: string }).created_at }))}</textarea></div>` +
    '<div class="field"><label for="v-header">Signature header to check (optional)</label><input id="v-header" placeholder="t=…,v1=…" spellcheck="false"></div>' +
    '<div class="try-actions"><button class="btn primary" id="v-run" type="button">Compute and check</button></div>' +
    '<div class="verify-out" id="v-out" role="status" aria-live="polite">signed_payload = t + "." + raw_body; v1 = hex(HMAC-SHA256(secret, signed_payload))</div>' +
    '</div></div><div class="sticky">' +
    eventPayloadBox(state) +
    handlerBox(state) +
    '<p class="aside-note">No Prismaport package: each handler checks the signature with the standard library, and Express or Flask only route the request. Check the raw body bytes, before any JSON parsing, and accept the request when any <code class="inline-code">v1</code> matches.</p>' +
    '</div></div>'
  );
}

export function renderSessionObject(state: ViewState): string {
  const s = schemaByName('Session');
  const fields = s ? bodyRows({ $ref: '#/components/schemas/Session' }).map((r) => ({ ...r, required: false })) : [];
  const get = groups.flatMap((g) => g.ops).find((o) => o.id === 'getSession');
  const paid = (get ? structuredClone(get.responses[0]!.example) : {}) as Record<string, unknown>;
  const payments = (paid.payments as Record<string, unknown>[] | undefined) ?? [];
  type Recipient = { wallet_id: string; label: string; address: string };
  const split = ((paid.payout as Record<string, Recipient[]> | undefined)?.['base-sepolia'] ?? []).slice(0, 2);
  const settled = {
    ...paid,
    version: 5,
    payments: payments.map((p) => ({ ...p, status: 'finalized' })),
    settlement: {
      chain: 'base-sepolia',
      asset: 'USDC',
      gross: '25.000000',
      fee: '0.225000',
      net: '24.775000',
      payouts: split.map((w, i) => ({ wallet_id: w.wallet_id, label: w.label, address: w.address, amount: i === 0 ? '17.342500' : '7.432500' })),
      tx_hash: '0x9b2e7c4a1f8d3e6b5a0c9f2e7d4b1a8c3f6e9d2b5a8c1f4e7d0a3b6c9f2e5d8a',
      settled_at: '2026-10-23T18:20:41Z',
    },
  };
  return (
    '<div class="crumb">Guides</div><h1>The session object</h1><p class="lede">The same shape in <a href="#getSession">GET /v1/sessions/{id}</a>, in every webhook and in the event feed. Fields are only ever added.</p>' +
    `<div class="split"><div class="prose"><h2>Fields</h2>${rows(fields)}</div>` +
    `<div class="sticky">${codeBox(`Paid and settled · 70/30 split of 25.00 USDC on ${state.live ? 'Base' : 'Base Sepolia'}`, pretty(state.live ? toLive(settled) : settled), true)}</div></div>`
  );
}

function addresses(contract: Contract): string {
  const rows = DEPLOYMENTS[contract].map(({ network, address, explorer }) => {
    const value = address && explorer ? `<a href="${explorer}" rel="noopener"><code>${address}</code></a>` : 'not deployed yet';
    return `<dt>${network}</dt><dd>${value}</dd>`;
  });
  return `<dl class="kv addr-note">${rows.join('')}</dl>`;
}

export function renderOnchain(): string {
  const contracts: [Contract, string, string][] = [
    ['Factory', 'EVM', 'Deploys one deposit address per session with CREATE2, as an ERC-1167 clone of ForwarderImpl. `predict` gives the address before anything is deployed. Same Factory address on every EVM network.'],
    ['ForwarderImpl', 'EVM', 'The code behind every deposit address. Its fee, recipients and accepted tokens are immutable arguments. `flush(token)` is permissionless for those tokens: it pays the fee, then each share, in one transaction. `recover(token)` is the one restricted call: only the treasury can make it, never for a bound token, and it forwards a stray token or the native coin to the same recipients after the percentage fee, with no fixed fee.'],
    ['Gateway', 'EVM', 'Settles EIP-3009 signed payments: receives the payer authorization, then pays the fee and every share in the same transaction. The nonce binds recipients, shares and fee.'],
    ['Forwarder program', 'Solana', 'Coming later with Solana. PDA forwarders seeded by `["fwd", salt, sha256(params)]`. Deployed final, with no upgrade authority.'],
  ];
  const evs: [string, string][] = [
    ['Payout(address indexed token, address indexed recipient, uint256 amount)', 'Forwarder flush and Gateway: one per non-zero share, in payout order.'],
    ['Settled(address indexed token, uint256 gross, uint256 fee, uint256 net, address treasury)', 'Forwarder flush, after its payouts. On Solana (coming later) the program emits Settled before the payouts.'],
    ['PaymentReceived(bytes32 indexed nonce, address indexed token, address indexed from, uint256 gross, uint256 fee, uint256 net)', 'Gateway, after a signed payment pays out.'],
    ['Recovered(address indexed token, uint256 gross, uint256 fee, uint256 net)', 'Forwarder recover, after its payouts. The zero address is the native coin. Not a settlement: it never pays a session.'],
  ];
  return (
    '<div class="crumb">Guides</div><h1>On-chain contracts</h1><p class="lede">You never call these to get paid; the API does. They are here so you, or anyone, can check where a payment went: addresses, events and the fee rule. How the contracts work, with a calculator and the code that guarantees each rule: <a href="#contracts">Smart contracts</a>.</p>' +
    '<div class="split"><div class="prose"><h2>Contracts</h2><div class="params">' +
    contracts.map(([n, f, d]) => `<div class="param"><div class="param-head"><code>${n}</code><span class="type">${f}</span></div><p>${md(d)}</p>${addresses(n)}</div>`).join('') +
    '</div><h2>Events to index</h2><div class="params">' +
    evs.map(([sig, d]) => `<div class="param"><div class="param-head"><code class="wrap">${esc(sig)}</code></div><p>${esc(d)}</p></div>`).join('') +
    '</div><h2>Fee rule</h2><p><span class="mono">fee = fixedFee + gross × feeBps / 10 000</span>, the percentage rounded down, with <span class="mono">fixedFee = fixedFeeMicros × 10^(decimals − 6)</span>. For a deposit address the fee parameters are fixed when it is derived; for a signed payment they are bound in the nonce. The contracts refuse fee parameters above 300 bps or a fixed fee above 5.00 USD. The first recipient gets the rounding remainder, at most a few base units.</p>' +
    '</div><div class="sticky">' +
    codeBox('Read a settlement with cast', 'cast logs --rpc-url $BASE_RPC \\\n  --from-block <block the session opened> \\\n  --address <deposit address from pay_to> \\\n  "Settled(address,uint256,uint256,uint256,address)"') +
    codeBox('Predict a deposit address', '// One clone per session (ERC-1167, immutable args)\nFactory.predict(salt, Params(treasury, feeBps, fixedFeeMicros, recipients, bps, tokens))\n// Same address on every EVM network') +
    '</div></div>'
  );
}
