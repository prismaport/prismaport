// @vitest-environment node
import { existsSync, readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

const root = new URL('../', import.meta.url);
const read = (p: string) => readFileSync(new URL(p, root), 'utf8');

describe('self-hosted brand fonts', () => {
  it('never loads fonts or styles from another origin', () => {
    expect(read('index.html')).not.toMatch(/googleapis|gstatic|<link[^>]+stylesheet[^>]+https?:/);
    for (const f of ['src/styles/app.css', 'src/styles/tokens.css', 'src/styles/fonts.css']) expect(read(f)).not.toMatch(/@import|https?:\/\//);
  });

  it('declares every brand weight with font-display: swap and a vendored woff2', () => {
    const faces = read('src/styles/fonts.css').split('@font-face').slice(1);
    const seen = new Set<string>();
    for (const face of faces) {
      const family = /font-family: '([^']+)'/.exec(face)?.[1];
      const weight = /font-weight: (\d+)/.exec(face)?.[1];
      const file = /url\('\.\.\/(fonts\/[^']+\.woff2)'\)/.exec(face)?.[1];
      expect(face).toContain('font-display: swap;');
      expect(file, `${family} ${weight}`).toBeDefined();
      expect(existsSync(new URL(`src/${file}`, root)), file).toBe(true);
      expect(readFileSync(new URL(`src/${file}`, root)).subarray(0, 4).toString('latin1')).toBe('wOF2');
      seen.add(`${family} ${weight}`);
    }
    expect([...seen].sort()).toEqual(['Familjen Grotesk 700', 'Geist Mono 400', 'Geist Mono 500', 'Onest 400', 'Onest 600']);
  });

  it('ships the SIL OFL licenses and the same brand marks as the checkout', () => {
    for (const f of ['OFL-FamiljenGrotesk.txt', 'OFL-Onest.txt', 'OFL-GeistMono.txt']) expect(read(`public/fonts/${f}`)).toContain('SIL OPEN FONT LICENSE Version 1.1');
    for (const f of ['prismaport-mark-ink.svg', 'prismaport-mark-white.svg', 'prismaport-mark-magenta.svg']) expect(read(`public/brand/${f}`)).toBe(read(`../checkout/public/brand/${f}`));
  });

  it('draws the favicon with the same prism as every brand mark, in both apps', () => {
    const shape = (svg: string) => [...svg.matchAll(/<path d="([^"]+)"[^>]*stroke-width="([^"]+)"/g)].map((m) => `${m[1]} @${m[2]}`);
    const mark = shape(read('public/brand/prismaport-mark-white.svg'));
    expect(mark).toHaveLength(3);
    for (const f of ['ink', 'magenta']) expect(shape(read(`public/brand/prismaport-mark-${f}.svg`))).toEqual(mark);
    expect(shape(read('public/favicon.svg'))).toEqual(mark);
    expect(read('public/favicon.svg')).toBe(read('../checkout/public/favicon.svg'));
  });
});
