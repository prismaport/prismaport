import { encode } from 'uqr';
import { svg } from './dom';

/** The QR modules of `text` as one SVG path ("M x y h1 v1 h-1 z" per dark module). */
export function qrPath(text: string): { size: number; d: string } {
  const qr = encode(text, { ecc: 'M', border: 4 });
  let d = '';
  qr.data.forEach((row, y) => {
    row.forEach((dark, x) => {
      if (dark) d += `M${x} ${y}h1v1h-1z`;
    });
  });
  return { size: qr.size, d };
}

/**
 * A QR code rendered locally as SVG. It stays dark on white in both themes: scanners need the
 * contrast and the quiet zone, whatever the page colours are.
 */
export function qrSvg(text: string, label: string): SVGElement {
  const { size, d } = qrPath(text);
  const title = svg('title', {});
  title.textContent = label;
  return svg('svg', { viewBox: `0 0 ${size} ${size}`, role: 'img', 'aria-label': label, class: 'qr', 'shape-rendering': 'crispEdges' },
    title, svg('rect', { width: size, height: size, fill: '#ffffff' }), svg('path', { d, fill: '#12141c' }));
}
