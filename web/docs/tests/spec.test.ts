import { describe, expect, it } from 'vitest';
import { highlightCode, highlightJSON } from '../src/format';
import { sampleAnswer, type ViewState } from '../src/pages';
import { LANGS, pythonLiteral, shellQuote, snippet } from '../src/snippets';
import { apiBase, events, exampleFor, groups, operations, toLive, typeLabel } from '../src/spec';

const state = (live = false): ViewState => ({ live, lang: 'curl', status: {}, event: 'session.paid', hook: 'node' });
const op = (id: string) => {
  const o = operations.find((x) => x.id === id);
  if (!o) throw new Error(`no operation ${id}`);
  return o;
};
const TEST_PATTERN = /base-sepolia|solana-devnet|"livemode":false|sk_test_|84532|0x036CbD53842c5426634e7929541eC2318f3dCF7e|4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU/;

describe('the contract', () => {
  it('lists every operation once, grouped by tag', () => {
    expect(operations).toHaveLength(41);
    expect(new Set(operations.map((o) => o.id)).size).toBe(41);
    expect(groups.flatMap((g) => g.ops)).toHaveLength(41);
    expect(apiBase).toBe('https://api.prismaport.com');
  });

  it('marks merchant routes with a scope and public routes without one', () => {
    for (const o of operations) {
      expect(o.isPublic).toBe(o.scope === null);
      expect(o.responses.length).toBeGreaterThan(0);
    }
    expect(op('createSession').scope).toBe('sessions:write');
    expect(op('getCheckoutSession').isPublic).toBe(true);
  });

  it('has the reveal routes of the choose-then-generate flow (D48)', () => {
    expect(op('revealSession').scope).toBe('sessions:write');
    expect(op('revealSession').path).toBe('/v1/sessions/{id}/reveal');
    expect(op('revealCheckoutSession').isPublic).toBe(true);
    expect(op('revealCheckoutSession').path).toBe('/v1/checkout/sessions/{id}/reveal');
    for (const id of ['revealSession', 'revealCheckoutSession']) {
      const codes = op(id).responses.map((r) => r.status);
      for (const code of ['200', '400', '409', '410']) expect(codes).toContain(code);
      expect(op(id).method).toBe('POST');
    }
    const create = op('createSession').body!;
    const expiresIn = (create.schema as { properties: Record<string, { description?: string }> }).properties['expires_in']!;
    expect(expiresIn.description).toContain('after its first reveal');
  });

  it('reads wallets but never writes them: they are added and archived only in the dashboard', () => {
    const wallets = operations.filter((o) => o.path.startsWith('/v1/wallets'));
    expect(wallets.map((o) => `${o.method.toUpperCase()} ${o.path}`).sort()).toEqual(['GET /v1/wallets', 'GET /v1/wallets/{id}']);
    expect(wallets.every((o) => o.scope === 'wallets:read')).toBe(true);
    expect(operations.some((o) => o.scope === 'wallets:write')).toBe(false);
  });

  it('knows which routes need an Idempotency-Key', () => {
    expect(operations.filter((o) => o.idempotency).map((o) => o.id).sort()).toEqual(['createPaymentLink', 'createSession', 'openLinkSession']);
  });

  it('has an example for every webhook event', () => {
    expect(events.map((e) => e.type)).toContain('session.paid');
    for (const e of events) expect((e.example as { type: string }).type).toBe(e.type);
  });

  it('labels types for the parameter tables', () => {
    const create = op('createProduct').body!;
    const props = (create.schema as { properties: Record<string, never> }).properties;
    expect(typeLabel(props['amount']!)).toBe('decimal string');
    expect(typeLabel(op('listEvents').params.find((p) => p.name === 'after')!.schema)).toBe('integer | "latest"');
  });
});

describe('live examples', () => {
  it('moves test networks, tokens and chain ids to live', () => {
    const live = toLive({
      livemode: false,
      chain: 'base-sepolia',
      payout: { 'base-sepolia': [], 'solana-devnet': [] },
      domain: { name: 'USDC', chainId: 84532, verifyingContract: '0x036CbD53842c5426634e7929541eC2318f3dCF7e' },
      key: 'sk_test_abc',
      message: 'deposit addresses are not available on base-sepolia yet',
      checkout_url: 'https://pay.prismaport.com/s/ses_x',
    });
    expect(live).toEqual({
      livemode: true,
      chain: 'base',
      payout: { base: [], solana: [] },
      domain: { name: 'USD Coin', chainId: 8453, verifyingContract: '0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913' },
      key: 'sk_live_abc',
      message: 'deposit addresses are not available on base yet',
      checkout_url: 'https://pay.prismaport.com/s/ses_x',
    });
  });

  it('leaves the network registry as it is, since it lists both modes', () => {
    const nets = op('listNetworks');
    expect(exampleFor(nets, nets.responses[0]!.example, true)).toBe(nets.responses[0]!.example);
  });

  it('shows nothing of test mode in any live response, request, parameter or event', () => {
    for (const o of operations.filter((x) => x.id !== 'listNetworks')) {
      for (const r of o.responses) expect(JSON.stringify(exampleFor(o, r.example, true)), `${o.id} ${r.status}`).not.toMatch(TEST_PATTERN);
      if (o.body) expect(JSON.stringify(exampleFor(o, o.body.example, true)), `${o.id} request`).not.toMatch(TEST_PATTERN);
      for (const p of o.params) expect(JSON.stringify(exampleFor(o, p.example ?? null, true))).not.toMatch(TEST_PATTERN);
      for (const lang of LANGS) expect(snippet(o, lang[0], true), `${o.id} ${lang[0]}`).not.toMatch(TEST_PATTERN);
    }
    for (const e of events) expect(JSON.stringify(toLive(e.example)), e.type).not.toMatch(TEST_PATTERN);
  });
});

describe('request snippets', () => {
  it.each(LANGS.map(([l]) => l))('sends the headers each endpoint needs in %s', (lang) => {
    const s = snippet(op('createSession'), lang, false);
    expect(s).toContain(`${apiBase}/v1/sessions`);
    expect(s).toContain('Idempotency-Key');
    expect(s).toMatch(/Authorization|PRISMAPORT_SECRET_KEY/);
    const pub = snippet(op('getCheckoutSession'), lang, false);
    expect(pub).not.toMatch(/Authorization|PRISMAPORT_SECRET_KEY/);
    expect(pub).toContain('/v1/checkout/sessions/ses_');
  });

  it('quotes every cURL URL and body, so ? and & stay literal in zsh', () => {
    for (const o of operations) {
      const first = snippet(o, 'curl', false).split('\n')[0]!;
      expect(first, o.id).toMatch(/^curl (-X [A-Z]+ )?'https:\/\/[^']+' ?\\?$/);
    }
    expect(snippet(op('listEvents'), 'curl', false)).toContain(`'${apiBase}/v1/events?after=latest'`);
    expect(shellQuote("it's")).toBe(`'it'\\''s'`);
  });

  it('writes Python literals for booleans and null, leaving strings alone', () => {
    expect(pythonLiteral('{"a":true,"b":null,"c":"true","d":false}')).toBe('{"a":True,"b":None,"c":"true","d":False}');
    expect(snippet(op('createWebhookEndpoint'), 'python', false)).not.toMatch(/\b(true|false|null)\b/);
  });

  it('uses the live key and networks in live mode', () => {
    const s = snippet(op('buildTypedData'), 'curl', true);
    expect(s).toContain('"chain":"base"');
    expect(snippet(op('createProduct'), 'curl', true)).toContain('sk_live_');
  });
});

describe('sample answers', () => {
  const ask = (id: string, input: { key?: string | null; path?: Record<string, string>; body?: string | null }, live = false) =>
    sampleAnswer(op(id), state(live), { key: input.key === undefined ? 'sk_test_x' : input.key, path: input.path ?? {}, body: input.body ?? null });
  const code = (r: { body: unknown }) => (r.body as { error?: { code: string; message: string } }).error;

  it('answers 401 invalid_api_key for a missing or malformed key', () => {
    expect(ask('getSession', { key: '', path: { id: 'ses_x' } })).toMatchObject({ status: '401', body: { error: { code: 'invalid_api_key', message: 'missing Bearer API key' } } });
    expect(ask('getSession', { key: 'abc', path: { id: 'ses_x' } })).toMatchObject({ status: '401', body: { error: { code: 'invalid_api_key', message: 'invalid API key' } } });
  });

  it('answers in the mode of the key, as the API does', () => {
    const r = ask('getSession', { key: 'sk_live_x', path: { id: 'ses_x' } });
    expect(r.status).toBe('200');
    expect(r.live).toBe(true);
    expect((r.body as { livemode: boolean }).livemode).toBe(true);
  });

  it('accepts a blank body and refuses what is not a JSON object with the API message', () => {
    expect(ask('openLinkSession', { key: null, path: { id: 'plink_x' }, body: '' }).status).toBe('200');
    expect(ask('updateProduct', { path: { id: 'prod_x' }, body: '   ' }).status).toBe('200');
    expect(ask('updateProduct', { path: { id: 'prod_x' }, body: 'null' }).status).toBe('200');
    for (const body of ['{', '[]', '1', '"x"']) {
      const r = ask('createProduct', { body });
      expect(r.status, body).toBe('400');
      expect(code(r)).toEqual({ code: 'invalid_request', message: 'request body must be a JSON object' });
    }
  });

  it('refuses a field the request schema does not have, with the handler message where it has one', () => {
    const r = ask('createSession', { body: '{"product_id":"prod_x","currency":"USD"}' });
    expect(code(r)).toEqual({ code: 'invalid_request', message: 'unknown field "currency"' });
    expect(code(ask('createPaymentLink', { body: '{"product_id":"prod_x","active":true}' }))).toMatchObject({ message: 'a new link is always active; deactivate it afterwards', param: 'active' });
  });

  it('answers the documented not-found for an id of the wrong kind', () => {
    expect(ask('getWallet', { path: { id: 'ses_1' } })).toMatchObject({ status: '404', body: { error: { code: 'not_found', message: 'no such wallet' } } });
  });

  it('checks in the order of each route', () => {
    expect(ask('sendTestEvent', { key: 'sk_live_x', path: { id: 'we_x' }, body: '{' }).status).toBe('400');
    expect(ask('sendTestEvent', { key: 'sk_live_x', path: { id: 'zzz' }, body: '{"type":"session.paid"}' }).status).toBe('403');
    expect(ask('simulateSession', { key: 'sk_live_x', path: { id: 'zzz' }, body: '{"action":"pay_exact"}' }).status).toBe('403');
    expect(ask('simulateSession', { key: 'sk_live_x', path: { id: 'ses_x' }, body: '{' }).status).toBe('403');
    expect(ask('simulateCheckoutSession', { key: null, path: { id: 'zzz' }, body: '{"action":"pay_exact"}' }, true).status).toBe('404');
    expect(ask('simulateCheckoutSession', { key: null, path: { id: 'ses_x' }, body: '{"action":"pay_exact"}' }, true).status).toBe('403');
  });

  it('answers the success example otherwise', () => {
    expect(ask('submitSignedPayment', { key: null, path: { id: 'ses_x' }, body: '{}' }).status).toBe('202');
  });
});

describe('highlighting', () => {
  it('escapes markup in JSON and code', () => {
    expect(highlightJSON('{"a":"<b>"}')).not.toContain('<b>');
    expect(highlightCode('x = "<script>"')).not.toContain('<script>');
  });

  it('keeps a URL out of the comment colour and marks real comments', () => {
    expect(highlightCode('curl https://api.prismaport.com/v1/sessions')).not.toContain('tk-com');
    expect(highlightCode('res.sendStatus(204); // done')).toContain('<span class="tk-com">// done</span>');
    expect(highlightCode('# Idempotency-Key: one per order')).toContain('tk-com');
  });
});
