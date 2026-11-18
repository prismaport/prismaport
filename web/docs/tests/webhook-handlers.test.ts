// @vitest-environment node
import { createHmac, timingSafeEqual } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { HANDLERS, NODE_VERIFY } from '../src/webhook-handlers';

interface Vector {
  name: string;
  body: string;
  header: string;
  secret: string;
  now: number;
  tolerance_seconds?: number;
  result: string;
}

// The vectors internal/webhooksig holds to; the Node handler on the webhooks page must give the same answers.
const vectors = (JSON.parse(readFileSync(new URL('../../../internal/webhooksig/testdata/vectors.json', import.meta.url), 'utf8')) as { verify: Vector[] }).verify;

type Verify = (rawBody: string | Buffer, header: string | undefined, secret: string, now?: number) => boolean;
const nodeVerify = (tolerance = 300): Verify => {
  const code = NODE_VERIFY.replace('const TOLERANCE = 300;', `const TOLERANCE = ${tolerance};`);
  return new Function('createHmac', 'timingSafeEqual', 'Buffer', `${code}\nreturn verify;`)(createHmac, timingSafeEqual, Buffer) as Verify;
};

describe('webhook handler samples', () => {
  it('shows the tested verify in the Node handler', () => {
    expect(HANDLERS.node).toContain(NODE_VERIFY);
    expect(NODE_VERIFY).toContain('const TOLERANCE = 300;');
  });

  it.each(vectors)('Node verify: $name', (v) => {
    const verify = nodeVerify(v.tolerance_seconds);
    for (const body of [v.body, Buffer.from(v.body)]) expect(verify(body, v.header, v.secret, v.now)).toBe(v.result === 'ok');
  });

  it('refuses a missing header', () => {
    expect(nodeVerify()('{}', undefined, 'whsec_test', 0)).toBe(false);
  });

  it('imports nothing from Prismaport', () => {
    for (const code of Object.values(HANDLERS)) expect(code).not.toMatch(/prismaport\/|webhooksig/);
  });
});
