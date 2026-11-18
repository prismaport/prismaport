// @vitest-environment node
import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { computeSignature, parseSignatureHeader, verifySignature } from '../src/signature';

interface Vectors {
  compute: { name: string; secret: string; timestamp: number; body: string; signature: string }[];
  verify: { name: string; body: string; header: string; secret: string; now: number; tolerance_seconds?: number; result: string }[];
}

// The vectors the Go and Node helpers share, so the portal's checker gives the same answers.
const vectors = JSON.parse(readFileSync(new URL('../../../internal/webhooksig/testdata/vectors.json', import.meta.url), 'utf8')) as Vectors;

describe('webhook signatures', () => {
  it.each(vectors.compute)('computes $name', async (v) => {
    expect(await computeSignature(v.secret, v.timestamp, v.body)).toBe(v.signature);
  });

  it.each(vectors.verify)('verifies $name', async (v) => {
    const r = await verifySignature(v.body, v.header, v.secret, v.now, v.tolerance_seconds);
    expect(r.ok ? 'ok' : r.reason).toBe(v.result);
  });

  it('refuses a duplicate or signed t', () => {
    expect(parseSignatureHeader('t=1,t=2,v1=aa')).toBeNull();
    expect(parseSignatureHeader('t=-1,v1=aa')).toBeNull();
    expect(parseSignatureHeader('t= 1,v1=aa')).toBeNull();
    expect(parseSignatureHeader('t=1')).toBeNull();
    expect(parseSignatureHeader('t=1, v1=aa ,x=y')).toEqual({ t: 1, signatures: ['aa'] });
  });
});
