type Child = Node | string | null | undefined | false;
type Attrs = Record<string, string | number | boolean | null | undefined | EventListener>;

/** Creates an element. `on*` attributes become listeners; text is always set as text, never HTML. */
export function h<K extends keyof HTMLElementTagNameMap>(tag: K, attrs: Attrs = {}, ...children: Child[]): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
    else if (k === 'class') el.className = String(v);
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  append(el, children);
  return el;
}

export function append(el: Element, children: Child[]): void {
  for (const c of children) {
    if (c === null || c === undefined || c === false) continue;
    el.append(typeof c === 'string' ? document.createTextNode(c) : c);
  }
}

const SVG_NS = 'http://www.w3.org/2000/svg';

export function svg(tag: string, attrs: Record<string, string | number>, ...children: Element[]): SVGElement {
  const el = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, String(v));
  el.append(...children);
  return el as SVGElement;
}

// Lucide outline icons (ISC license), 1.5px stroke, drawn in currentColor.
const ICONS: Record<string, string[]> = {
  copy: ['M10 8h10a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H10a2 2 0 0 1-2-2V10a2 2 0 0 1 2-2z', 'M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2'],
  check: ['M20 6 9 17l-5-5'],
  wallet: ['M19 7V4a1 1 0 0 0-1-1H5a2 2 0 0 0 0 4h15a1 1 0 0 1 1 1v4h-3a2 2 0 0 0 0 4h3a1 1 0 0 0 1-1v-2a1 1 0 0 0-1-1', 'M3 5v14a2 2 0 0 0 2 2h15a1 1 0 0 0 1-1v-4'],
  clock: ['M12 6v6l4 2', 'M22 12a10 10 0 1 1-20 0 10 10 0 0 1 20 0'],
  info: ['M22 12a10 10 0 1 1-20 0 10 10 0 0 1 20 0', 'M12 16v-4', 'M12 8h.01'],
  arrow: ['M5 12h14', 'm12 5 7 7-7 7'],
  close: ['M18 6 6 18', 'm6 6 12 12'],
  chevron: ['m9 18 6-6-6-6'],
  qr: ['M3 3h5v5H3z', 'M16 3h5v5h-5z', 'M3 16h5v5H3z', 'M21 16h-3a2 2 0 0 0-2 2v3', 'M21 21v.01', 'M12 7v3a2 2 0 0 1-2 2H7', 'M3 12h.01', 'M12 3h.01', 'M12 16v.01', 'M16 12h1', 'M21 12v.01', 'M12 21v-1'],
  external: ['M15 3h6v6', 'M10 14 21 3', 'M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6'],
  refresh: ['M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8', 'M21 3v5h-5', 'M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16', 'M8 16H3v5'],
  plug: ['M12 22v-5', 'M9 8V2', 'M15 8V2', 'M18 8v5a4 4 0 0 1-4 4h-4a4 4 0 0 1-4-4V8Z'],
  loader: ['M21 12a9 9 0 1 1-6.219-8.56'],
  alert: ['M12 9v4', 'M12 17h.01', 'M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z'],
  help: ['M22 12a10 10 0 1 1-20 0 10 10 0 0 1 20 0', 'M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3', 'M12 17h.01'],
  'wifi-off': ['M12 20h.01', 'M8.5 16.429a5 5 0 0 1 7 0', 'M5 12.859a10 10 0 0 1 5.17-2.69', 'M19 12.859a10 10 0 0 0-2.007-1.523',
    'M2 8.82a15 15 0 0 1 4.177-2.643', 'M22 8.82a15 15 0 0 0-11.288-3.764', 'm2 2 20 20'],
  lock: ['M5 11h14a2 2 0 0 1 2 2v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-7a2 2 0 0 1 2-2z', 'M7 11V7a5 5 0 0 1 10 0v4'],
};

export function icon(name: keyof typeof ICONS | string, size = 16): SVGElement {
  const paths = (ICONS[name] ?? []).map((d) => svg('path', { d }));
  return svg('svg', {
    width: size, height: size, viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor',
    'stroke-width': 1.5, 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true', focusable: 'false', class: 'icon',
  }, ...paths);
}
