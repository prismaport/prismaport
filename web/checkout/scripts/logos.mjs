// Regenerates src/logos/ (64 px and 128 px PNGs) from the original logo files:
//
//   node scripts/logos.mjs <source-dir>
//
// The source directory holds the logos as collected, never redrawn or recoloured:
//   tokens   usdc.png, usdt.png (250 x 250, the issuer logos as published on CoinGecko)
//   networks base.png, solana.png, ethereum.png, arbitrum.png, optimism.png, polygon.png,
//            bsc.png, avalanche.png (collected from a sibling project's frontend)
//   wallets  wallet-metamask.svg, wallet-rabby.svg, wallet-phantom.svg (from the same
//            project's public/wallets/, renamed with the wallet- prefix)
// They were collected into the session scratchpad for each run; only the outputs are committed.
// A logo whose source is missing from the directory keeps its committed outputs, so one group can
// be added without the others' sources. Networks without a file (Celo, Plasma) render a text
// fallback. The wallet SVGs are rasterized here rather than shipped: MetaMask's styles its paths
// with an internal <style>, which a page's CSP may refuse.
//
// The outputs are quantized to a 256-colour palette at full quality (premultiplied PSNR above
// 48 dB on every file, invisible at 16 to 24 px), a sixth smaller than truecolour PNG. The
// pinned sharp version makes the output byte-stable.
import { existsSync, mkdirSync, readdirSync, statSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import sharp from 'sharp';

const LOGOS = [
  'usdc', 'usdt', 'base', 'solana', 'ethereum', 'arbitrum', 'optimism', 'polygon', 'bsc', 'avalanche',
  'wallet-metamask', 'wallet-rabby', 'wallet-phantom',
];
const SIZES = [64, 128];

const source = process.argv[2];
if (!source) {
  console.error('usage: node scripts/logos.mjs <source-dir>');
  process.exit(2);
}
const out = fileURLToPath(new URL('../src/logos/', import.meta.url));
mkdirSync(out, { recursive: true });

let total = 0;
for (const name of LOGOS) {
  const input = ['png', 'svg'].map((ext) => resolve(source, `${name}.${ext}`)).find((f) => existsSync(f));
  if (!input) {
    if (SIZES.every((size) => existsSync(join(out, `${name}-${size}.png`)))) continue;
    console.error(`missing ${name}.png or ${name}.svg in ${source}`);
    process.exit(1);
  }
  for (const size of SIZES) {
    const file = join(out, `${name}-${size}.png`);
    await sharp(input, { density: 600 })
      .resize(size, size, { fit: 'contain', kernel: 'lanczos3', background: { r: 0, g: 0, b: 0, alpha: 0 } })
      .png({ palette: true, quality: 100, dither: 1, effort: 10, compressionLevel: 9 })
      .toFile(file);
    total += statSync(file).size;
  }
}
console.log(`${readdirSync(out).length} files, ${total} bytes in ${out}`);
