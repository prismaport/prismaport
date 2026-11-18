// @vitest-environment node
import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

// Vitest stubs CSS imports, so the stylesheets are read from disk.
const tokensCss = readFileSync(new URL('../src/styles/tokens.css', import.meta.url), 'utf8');
const appCss = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
const checkoutCss = readFileSync(new URL('../../checkout/src/styles.css', import.meta.url), 'utf8');

function tokens(block: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const m of block.matchAll(/--([a-z-]+):\s*(#[0-9a-f]{6})/gi)) out[m[1]!] = m[2]!.toLowerCase();
  return out;
}

function themes(css: string) {
  const light = tokens(css.slice(css.indexOf(':root {'), css.indexOf('}', css.indexOf(':root {'))));
  const darkStart = css.indexOf('@media (prefers-color-scheme: dark)');
  const dark = { ...light, ...tokens(css.slice(darkStart, css.indexOf('}\n}', darkStart))) };
  return { light, dark };
}

const { light, dark } = themes(tokensCss);

function luminance(hex: string): number {
  const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255).map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
  return 0.2126 * r! + 0.7152 * g! + 0.0722 * b!;
}

function ratio(a: string, b: string): number {
  const [x, y] = [luminance(a), luminance(b)].sort((p, q) => q - p);
  return (x! + 0.05) / (y! + 0.05);
}

const TEXT: [string, string][] = [
  ['ink', 'surface'],
  ['ink', 'surface-raised'],
  ['ink-muted', 'surface'],
  ['ink-muted', 'surface-raised'],
  ['magenta', 'surface'],
  ['magenta', 'surface-raised'],
  ['magenta', 'magenta-soft'],
  ['on-magenta', 'magenta'],
  ['state-paid', 'surface-raised'],
  ['state-paid', 'state-paid-soft'],
  ['state-partial', 'surface-raised'],
  ['state-partial', 'state-partial-soft'],
  ['state-reversed', 'surface-raised'],
  ['ink', 'magenta-soft'],
  // The Test mode strip, the selected Test segment and toasts: surface text on an ink ground.
  ['surface', 'ink'],
  ['surface-raised', 'ink'],
  ['ink-muted', 'magenta-soft'],
  // Code entry: digits in a wrong-code (amber) box.
  ['ink', 'state-partial-soft'],
];
const NON_TEXT: [string, string][] = [
  ['line-strong', 'surface'],
  ['line-strong', 'surface-raised'],
  ['magenta', 'surface-raised'],
  ['magenta', 'surface'],
  ['ink', 'surface-raised'],
  // Code entry box borders: amber (try again) and green (confirmed), on the card and on their own fill.
  ['state-partial', 'surface-raised'],
  ['state-partial', 'state-partial-soft'],
  ['state-paid', 'surface-raised'],
  ['state-paid', 'state-paid-soft'],
];

describe.each([
  ['light', light],
  ['dark', dark],
] as const)('%s theme contrast (WCAG AA)', (_name, t) => {
  it.each(TEXT)('%s text on %s is at least 4.5:1', (fg, bg) => {
    expect(t[fg], fg).toBeDefined();
    expect(ratio(t[fg]!, t[bg]!)).toBeGreaterThanOrEqual(4.5);
  });
  it.each(NON_TEXT)('%s on %s is at least 3:1', (fg, bg) => {
    expect(ratio(t[fg]!, t[bg]!)).toBeGreaterThanOrEqual(3);
  });
});

// Payout split segments are aliases of brand tokens, declared in the app stylesheet.
const SPLIT = [...appCss.matchAll(/--(split-\d):\s*var\(--([a-z-]+)\)/g)].map((m) => [m[1]!, m[2]!] as const);

describe('payout split segments', () => {
  it('has five segment tokens: magenta for the first recipient only, brand neutrals after it', () => {
    expect(SPLIT.map(([k]) => k)).toEqual(['split-1', 'split-2', 'split-3', 'split-4', 'split-5']);
    expect(SPLIT[0]![1]).toBe('magenta');
    for (const [, ref] of SPLIT.slice(1)) expect(['ink', 'ink-muted', 'line-strong']).toContain(ref);
    // Segments 2 to 4 are filled and must differ; the fifth is drawn as an outline.
    expect(new Set(SPLIT.slice(1, 4).map(([, ref]) => ref)).size).toBe(3);
    expect(appCss).toMatch(/\.split-seg\[data-seg='5'\][\s\S]*?box-shadow: inset 0 0 0 2px var\(--split-5\)/);
  });

  describe.each([
    ['light', light],
    ['dark', dark],
  ] as const)('%s theme', (_name, t) => {
    it.each(SPLIT)('%s (%s) is at least 3:1 on cards and on the page', (_k, ref) => {
      expect(ratio(t[ref]!, t['surface-raised']!)).toBeGreaterThanOrEqual(3);
      expect(ratio(t[ref]!, t.surface!)).toBeGreaterThanOrEqual(3);
    });
    it('the dashed rest of an unfinished split is at least 3:1', () => {
      expect(appCss).toMatch(/\.split-seg\[data-seg='rest'\][\s\S]*?border: 1px dashed var\(--line-strong\)/);
      expect(ratio(t['line-strong']!, t['surface-raised']!)).toBeGreaterThanOrEqual(3);
    });
  });
});

it('uses exactly the hosted checkout tokens', () => {
  const c = themes(checkoutCss);
  expect(light).toEqual(c.light);
  expect(dark).toEqual(c.dark);
});

it('has no red, and the app stylesheet adds no colours of its own', () => {
  for (const hex of [...Object.values(light), ...Object.values(dark)]) {
    const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16));
    expect(r! > 180 && g! < 80 && b! < 80, hex).toBe(false);
  }
  // Only the QR ground is a literal colour (scanners need white in both themes).
  const literals = [...appCss.matchAll(/#[0-9a-f]{3,8}\b/gi)].map((m) => m[0].toLowerCase());
  expect(new Set(literals)).toEqual(new Set(['#ffffff']));
  expect(appCss).not.toMatch(/gradient/);
});

it('draws focus as a 2px gap and a 2px magenta ring', () => {
  expect(appCss).toMatch(/:focus-visible \{\s*outline: 2px solid var\(--focus-ring\);\s*outline-offset: 2px;/);
  expect(tokensCss).toMatch(/--focus-ring: var\(--magenta\)/);
});

it('draws the code entry boxes from brand tokens only, with a focus ring around them', () => {
  expect(appCss).toMatch(/\.code-entry:has\(\.code-native:focus-visible\) \.code-boxes \{\s*outline: 2px solid var\(--focus-ring\);/);
  expect(appCss).toMatch(/\.code-entry:focus-within \.code-box\[data-active\] \{\s*border-color: var\(--magenta\);/);
  expect(appCss).toMatch(/\.code-field\[data-status='confirmed'\] \.code-box \{\s*border-color: var\(--state-paid\);/);
  expect(appCss).toMatch(/\.code-field\[data-status='locked'\] \.code-box \{\s*border-color: var\(--state-partial\);/);
});

it('leaves the hidden full value of a short address out of a copied selection', () => {
  expect(appCss).toMatch(/\.no-copy \{\s*-webkit-user-select: none;\s*user-select: none;\s*\}/);
});

it('respects reduced motion', () => {
  expect(tokensCss).toMatch(/@media \(prefers-reduced-motion: reduce\)[\s\S]*animation: none !important;[\s\S]*transition: none !important;/);
});
