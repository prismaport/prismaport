// Webhook signatures, with the same rules as internal/webhooksig: Signature: t=<unix seconds>,v1=<hex
// HMAC-SHA256 of t + "." + raw body>, keyed with the whole whsec_ secret.

const TOLERANCE_SECONDS = 300;

export interface ParsedSignature {
  t: number;
  signatures: string[];
}

/** Splits a Signature header. Null when it is malformed: exactly one non-negative integer t and at least one v1 are required. */
export function parseSignatureHeader(header: string): ParsedSignature | null {
  let t = -1;
  const signatures: string[] = [];
  for (const part of header.split(',')) {
    const p = part.trim();
    const i = p.indexOf('=');
    if (i < 0) return null;
    const key = p.slice(0, i);
    const value = p.slice(i + 1);
    if (key === 't') {
      if (!/^[+-]?\d+$/.test(value) || t !== -1) return null;
      const n = Number(value);
      if (!Number.isSafeInteger(n) || n < 0) return null;
      t = n;
    } else if (key === 'v1') {
      signatures.push(value);
    }
  }
  return t < 0 || signatures.length === 0 ? null : { t, signatures };
}

export async function computeSignature(secret: string, t: number | string, body: string): Promise<string> {
  const enc = new TextEncoder();
  const key = await crypto.subtle.importKey('raw', enc.encode(secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
  const mac = await crypto.subtle.sign('HMAC', key, enc.encode(`${t}.${body}`));
  return [...new Uint8Array(mac)].map((b) => b.toString(16).padStart(2, '0')).join('');
}

export type VerifyResult = { ok: true; t: number } | { ok: false; reason: 'malformed' | 'timestamp' | 'mismatch'; expected?: string };

export async function verifySignature(body: string, header: string, secret: string, nowSeconds: number, toleranceSeconds = TOLERANCE_SECONDS): Promise<VerifyResult> {
  const parsed = parseSignatureHeader(header);
  if (!parsed) return { ok: false, reason: 'malformed' };
  if (Math.abs(nowSeconds - parsed.t) > toleranceSeconds) return { ok: false, reason: 'timestamp' };
  const expected = await computeSignature(secret, parsed.t, body);
  return parsed.signatures.includes(expected) ? { ok: true, t: parsed.t } : { ok: false, reason: 'mismatch', expected };
}
