import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { ALL_EXCERPTS, EIP3009, X, type Excerpt } from '../src/contracts/excerpts';
import { ATTEMPTS, renderContracts } from '../src/contracts/page';

const ELISION = /^[ \t]*\/\/ \.\.\.[ \t]*$/m;
const source = (file: string) => readFileSync(join(process.cwd(), '../../contracts/src', file), 'utf8');

/** Whitespace runs become one space, and spaces next to punctuation go, so only formatting can differ. */
const normalize = (s: string) =>
  s
    .replace(/\s+/g, ' ')
    .replace(/ ?([^\w ]) ?/g, '$1')
    .trim();

/** The excerpt's fragments (split at `// ...` lines) appear in the file, in order. */
function findIn(file: string, x: Excerpt): string | null {
  const text = normalize(file);
  let from = 0;
  for (const fragment of x.code.split(ELISION).map(normalize).filter(Boolean)) {
    const at = text.indexOf(fragment, from);
    if (at < 0) return fragment;
    from = at + fragment.length;
  }
  return null;
}

describe('code excerpts', () => {
  it('every excerpt from contracts/src is still in its file', () => {
    const fromContracts = ALL_EXCERPTS.filter((x) => x.source !== EIP3009);
    expect(fromContracts.length).toBeGreaterThan(20);
    for (const x of fromContracts) expect(findIn(source(x.source), x), `${x.source}:\n${x.code}`).toBeNull();
  });

  it('catches an edited excerpt and a reordered elision', () => {
    const file = source('FeeLib.sol');
    expect(findIn(file, { source: 'FeeLib.sol', code: 'uint16 internal constant MAX_FEE_BPS = 400;' })).not.toBeNull();
    expect(findIn(file, { source: 'FeeLib.sol', code: 'net = gross - fee;\n// ...\nfee = fixedFee(fixedFeeMicros, decimals) + pctFee(gross, feeBps);' })).not.toBeNull();
    expect(findIn(file, X.feeCompute)).toBeNull();
  });

  it('labels the token-side rules as EIP-3009 and keeps them out of the contracts', () => {
    const usdc = ALL_EXCERPTS.filter((x) => x.source === EIP3009);
    expect(usdc.map((x) => x.code)).toEqual([X.usdcTypedData.code, X.usdcPayee.code, X.usdcOnce.code]);
    for (const file of ['Gateway.sol', 'FeeLib.sol', 'SplitLib.sol', 'Factory.sol', 'ForwarderImpl.sol']) {
      expect(source(file)).not.toContain('authorizationState');
    }
  });

  it('the page shows only excerpts from the module, with their source', () => {
    const known = new Set(ALL_EXCERPTS);
    for (const a of ATTEMPTS) for (const x of a.code) expect(known.has(x), a.id).toBe(true);
    const html = renderContracts();
    expect(html).toContain('<span class="title">Factory.sol</span>');
    expect(html).not.toMatch(/\d+ lines/);
  });
});
