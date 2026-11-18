// Renders the share cards (1200 x 630): the landing's site/og.png from og/og.svg and the API
// portal's ../docs/public/og.png from ../docs/og/og.svg:
//
//   npm run og
//
// Each SVG names its app's own fonts and logos by relative path; they are inlined as data
// URIs so Chromium draws the card with the brand faces and no network. The screenshot is then
// quantized to a 256-colour palette, which keeps the text edges and is a fraction of the size.
// It then records the hash of the source it drew next to it (og.svg.sha256), so a test can tell
// when the SVG has changed and the card has not been rendered again.
import { createHash } from 'node:crypto';
import { readFileSync, statSync, writeFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from '@playwright/test';
import sharp from 'sharp';

const WIDTH = 1200;
const HEIGHT = 630;
const TYPES = { '.woff2': 'font/woff2', '.png': 'image/png' };

const root = fileURLToPath(new URL('../', import.meta.url));
const CARDS = [
  ['og/og.svg', 'site/og.png'],
  ['../docs/og/og.svg', '../docs/public/og.png'],
];

const browser = await chromium.launch();
try {
  for (const [src, dst] of CARDS) {
    const source = resolve(root, src);
    const out = resolve(root, dst);
    const inline = (rel) => {
      const file = resolve(dirname(source), rel);
      const type = TYPES[file.slice(file.lastIndexOf('.'))];
      if (!type) throw new Error(`${src} names ${rel}, which is neither a font nor a PNG`);
      return `data:${type};base64,${readFileSync(file).toString('base64')}`;
    };
    const svg = readFileSync(source, 'utf8')
      .replace(/url\('(\.\.\/[^']+)'\)/g, (_, rel) => `url('${inline(rel)}')`)
      .replace(/href="(\.\.\/[^"]+)"/g, (_, rel) => `href="${inline(rel)}"`);

    const page = await browser.newPage({ viewport: { width: WIDTH, height: HEIGHT }, deviceScaleFactor: 1 });
    await page.setContent(`<!doctype html><html><body style="margin:0">${svg}</body></html>`);
    const faces = await page.evaluate(async () => {
      await document.fonts.ready;
      return [...document.fonts].filter((f) => f.status === 'loaded').length;
    });
    if (faces < 4) throw new Error(`${src}: only ${faces} of the 4 font faces loaded`);
    const shot = await page.screenshot({ clip: { x: 0, y: 0, width: WIDTH, height: HEIGHT } });
    await page.close();
    await sharp(shot).png({ palette: true, quality: 100, dither: 1, effort: 10, compressionLevel: 9 }).toFile(out);
    writeFileSync(`${source}.sha256`, `${createHash('sha256').update(readFileSync(source)).digest('hex')}
`);
    console.log(`${out}: ${statSync(out).size} bytes`);
  }
} finally {
  await browser.close();
}
