// Regenerates src/logos/ (64 px and 128 px PNGs) from the original logo files:
//
//   node scripts/logos.mjs <source-dir>
//
// The source directory holds the logos as collected, never redrawn or recoloured:
//   tokens   usdc.png, usdt.png (250 x 250, the issuer logos as published on CoinGecko)
//   networks base.png, solana.png, ethereum.png, arbitrum.png, optimism.png, polygon.png,
//            bsc.png, avalanche.png (collected from a sibling project's frontend)
// They were collected into the session scratchpad (…/scratchpad/logos/) for the first run; only
// the outputs are committed. Networks without a file (Celo, Plasma) render a text fallback.
//
// The outputs are quantized to a 256-colour palette at full quality (premultiplied PSNR above
// 48 dB on every file, invisible at 16 to 24 px), a sixth smaller than truecolour PNG. The
// pinned sharp version makes the output byte-stable.
import { mkdirSync, readdirSync, rmSync, statSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import sharp from 'sharp';

const LOGOS = ['usdc', 'usdt', 'base', 'solana', 'ethereum', 'arbitrum', 'optimism', 'polygon', 'bsc', 'avalanche'];
const SIZES = [64, 128];

const source = process.argv[2];
if (!source) {
  console.error('usage: node scripts/logos.mjs <source-dir>');
  process.exit(2);
}
const out = fileURLToPath(new URL('../src/logos/', import.meta.url));
rmSync(out, { recursive: true, force: true });
mkdirSync(out, { recursive: true });

let total = 0;
for (const name of LOGOS) {
  const input = resolve(source, `${name}.png`);
  for (const size of SIZES) {
    const file = join(out, `${name}-${size}.png`);
    await sharp(input)
      .resize(size, size, { fit: 'contain', kernel: 'lanczos3', background: { r: 0, g: 0, b: 0, alpha: 0 } })
      .png({ palette: true, quality: 100, dither: 1, effort: 10, compressionLevel: 9 })
      .toFile(file);
    total += statSync(file).size;
  }
}
console.log(`${readdirSync(out).length} files, ${total} bytes in ${out}`);
