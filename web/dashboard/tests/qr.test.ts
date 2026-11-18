import jsQR from 'jsqr';
import { describe, expect, it } from 'vitest';
import { qrFilename, qrPath, qrSvgMarkup } from '../src/lib/qr';

/** Rasterizes the SVG path back into pixels and decodes it with an independent QR reader. */
function decode(text: string): string | undefined {
  const { size, d } = qrPath(text);
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

describe('QR codes', () => {
  it('encode the full link URL, locally', () => {
    const url = 'https://prismaport.xyz/l/prod_01JB8Q4ZQ2W7N3H5X9K0Y6T1R2';
    expect(decode(url)).toBe(url);
    const otp = 'otpauth://totp/Prismaport:owner%40acme.studio?secret=JBSWY3DPEHPK3PXP&issuer=Prismaport';
    expect(decode(otp)).toBe(otp);
  });

  it('download as a standalone SVG, dark on white', () => {
    const svg = qrSvgMarkup('https://prismaport.xyz/l/prod_1');
    expect(svg).toMatch(/^<svg xmlns="http:\/\/www.w3.org\/2000\/svg"/);
    expect(svg).toContain('fill="#ffffff"');
    expect(svg).toContain('fill="#12141c"');
  });

  it('name downloads after the link', () => {
    expect(qrFilename('Founder plan', 'png')).toBe('founder-plan-qr.png');
    expect(qrFilename('Café & Co.', 'svg')).toBe('cafe-co-qr.svg');
    expect(qrFilename('***', 'svg')).toBe('payment-link-qr.svg');
  });
});
