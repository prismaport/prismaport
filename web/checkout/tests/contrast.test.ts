import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// Vitest stubs CSS imports, so the stylesheet is read from disk (tests run from the package root).
const css = readFileSync(resolve(process.cwd(), 'src/styles.css'), 'utf8');

function tokens(block: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const m of block.matchAll(/--([a-z-]+):\s*(#[0-9a-f]{6})/gi)) out[m[1]!] = m[2]!.toLowerCase();
  return out;
}

const light = tokens(css.slice(css.indexOf(':root {'), css.indexOf('}', css.indexOf(':root {'))));
const darkStart = css.indexOf('@media (prefers-color-scheme: dark)');
const dark = { ...light, ...tokens(css.slice(darkStart, css.indexOf('}\n}', darkStart))) };

function luminance(hex: string): number {
  const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255).map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
  return 0.2126 * r! + 0.7152 * g! + 0.0722 * b!;
}

function ratio(a: string, b: string): number {
  const [x, y] = [luminance(a), luminance(b)].sort((p, q) => q - p);
  return (x! + 0.05) / (y! + 0.05);
}

const TEXT: [string, string][] = [
  ['ink', 'surface'], ['ink', 'surface-raised'], ['ink-muted', 'surface'], ['ink-muted', 'surface-raised'],
  ['magenta', 'surface'], ['magenta', 'surface-raised'], ['magenta', 'magenta-soft'], ['on-magenta', 'magenta'],
  ['state-paid', 'surface-raised'], ['state-paid', 'state-paid-soft'], ['state-partial', 'surface-raised'], ['state-partial', 'state-partial-soft'],
  ['state-reversed', 'surface-raised'], ['ink', 'magenta-soft'],
  // The network and low-balance warning boxes: title in ink, text in ink-muted, on the partial tint.
  ['ink', 'state-partial-soft'], ['ink-muted', 'state-partial-soft'],
];
const NON_TEXT: [string, string][] = [['line-strong', 'surface'], ['line-strong', 'surface-raised'], ['magenta', 'surface-raised']];

describe.each([['light', light], ['dark', dark]] as const)('%s theme contrast (WCAG AA)', (_name, t) => {
  it.each(TEXT)('%s text on %s is at least 4.5:1', (fg, bg) => {
    expect(t[fg], fg).toBeDefined();
    expect(ratio(t[fg]!, t[bg]!)).toBeGreaterThanOrEqual(4.5);
  });
  it.each(NON_TEXT)('%s on %s is at least 3:1', (fg, bg) => {
    expect(ratio(t[fg]!, t[bg]!)).toBeGreaterThanOrEqual(3);
  });
});

it('the palette has no red', () => {
  for (const hex of [...Object.values(light), ...Object.values(dark)]) {
    const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16));
    expect(r! > 180 && g! < 80 && b! < 80, hex).toBe(false);
  }
});
