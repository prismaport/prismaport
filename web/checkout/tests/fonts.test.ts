// @vitest-environment node
import { existsSync, readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

const root = new URL('../', import.meta.url);
const read = (p: string) => readFileSync(new URL(p, root), 'utf8');

describe('self-hosted fonts', () => {
  it('never loads fonts from another origin', () => {
    const html = read('index.html');
    expect(html).not.toMatch(/googleapis|gstatic|<link[^>]+stylesheet[^>]+https?:/);
    expect(read('src/styles.css')).not.toMatch(/@import|https?:\/\//);
    expect(read('src/fonts.css')).not.toMatch(/https?:\/\//);
  });

  it('declares every brand weight with font-display: swap and a vendored woff2', () => {
    const css = read('src/fonts.css');
    const faces = css.split('@font-face').slice(1);
    const seen = new Set<string>();
    for (const face of faces) {
      const family = /font-family: '([^']+)'/.exec(face)?.[1];
      const weight = /font-weight: (\d+)/.exec(face)?.[1];
      const file = /url\('\.\/(fonts\/[^']+\.woff2)'\)/.exec(face)?.[1];
      expect(face).toContain('font-display: swap;');
      expect(file, `${family} ${weight}`).toBeDefined();
      expect(existsSync(new URL(`src/${file}`, root)), file).toBe(true);
      expect(readFileSync(new URL(`src/${file}`, root)).subarray(0, 4).toString('latin1')).toBe('wOF2');
      seen.add(`${family} ${weight}`);
    }
    expect([...seen].sort()).toEqual(['Familjen Grotesk 700', 'Geist Mono 400', 'Geist Mono 500', 'Onest 400', 'Onest 600']);
  });

  it('ships the SIL OFL license of each family', () => {
    for (const f of ['OFL-FamiljenGrotesk.txt', 'OFL-Onest.txt', 'OFL-GeistMono.txt']) {
      expect(read(`public/fonts/${f}`)).toContain('SIL OPEN FONT LICENSE Version 1.1');
    }
  });
});
