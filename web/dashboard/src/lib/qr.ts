import { encode } from 'uqr';

export const QR_DARK = '#12141c';
export const QR_LIGHT = '#ffffff';

/** The QR modules of `text` as one SVG path ("M x y h1 v1 h-1 z" per dark module), made locally. */
export function qrPath(text: string): { size: number; d: string; modules: boolean[][] } {
  const qr = encode(text, { ecc: 'M', border: 4 });
  let d = '';
  qr.data.forEach((row, y) => {
    row.forEach((dark, x) => {
      if (dark) d += `M${x} ${y}h1v1h-1z`;
    });
  });
  return { size: qr.size, d, modules: qr.data };
}

/** A standalone SVG document: dark on white in every theme, since scanners need the contrast. */
export function qrSvgMarkup(text: string): string {
  const { size, d } = qrPath(text);
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${size} ${size}" width="${size * 8}" height="${size * 8}" shape-rendering="crispEdges"><rect width="${size}" height="${size}" fill="${QR_LIGHT}"/><path d="${d}" fill="${QR_DARK}"/></svg>`;
}

/** A PNG of the code drawn on a canvas, `scale` pixels per module. */
export async function qrPngBlob(text: string, scale = 12): Promise<Blob> {
  const { size, modules } = qrPath(text);
  const canvas = document.createElement('canvas');
  canvas.width = canvas.height = size * scale;
  const ctx = canvas.getContext('2d');
  if (!ctx) throw new Error('canvas unavailable');
  ctx.fillStyle = QR_LIGHT;
  ctx.fillRect(0, 0, canvas.width, canvas.height);
  ctx.fillStyle = QR_DARK;
  modules.forEach((row, y) => row.forEach((dark, x) => dark && ctx.fillRect(x * scale, y * scale, scale, scale)));
  return new Promise((resolve, reject) => canvas.toBlob((b) => (b ? resolve(b) : reject(new Error('png encoding failed'))), 'image/png'));
}

export function downloadBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

/** A file name from the link name: "founder-plan-qr.png". */
export function qrFilename(name: string, ext: 'svg' | 'png'): string {
  const slug = name.toLowerCase().normalize('NFKD').replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 60) || 'payment-link';
  return `${slug}-qr.${ext}`;
}
