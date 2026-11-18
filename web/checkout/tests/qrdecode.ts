import jsQR from 'jsqr';
import { qrPath } from '../src/lib/qr';

/** Rasterizes a QR SVG path back into pixels and decodes it with an independent QR reader. */
export function decodePath(size: number, d: string): string | undefined {
  const scale = 8;
  const px = size * scale;
  const data = new Uint8ClampedArray(px * px * 4).fill(255);
  for (const m of d.matchAll(/M(\d+) (\d+)h1v1h-1z/g)) {
    const x0 = Number(m[1]) * scale;
    const y0 = Number(m[2]) * scale;
    for (let y = y0; y < y0 + scale; y++) {
      for (let x = x0; x < x0 + scale; x++) {
        const i = (y * px + x) * 4;
        data[i] = data[i + 1] = data[i + 2] = 0;
      }
    }
  }
  return jsQR(data, px, px)?.data;
}

export function decode(text: string): string | undefined {
  const { size, d } = qrPath(text);
  return decodePath(size, d);
}

/** Decodes a QR as the page rendered it, from the SVG element. */
export function decodeSvg(el: Element): string | undefined {
  const size = Number(el.getAttribute('viewBox')!.split(' ')[2]);
  return decodePath(size, el.querySelector('path')!.getAttribute('d')!);
}
