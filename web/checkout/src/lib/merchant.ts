import { h } from './dom';
import type { Merchant } from './api';

/** The only logo paths the API answers: a content id and `.png`. */
const LOGO_PATH = /^\/v1\/checkout\/logos\/[0-9a-f]{32}\.png$/;

/** The logo box, in CSS pixels. The PNG is at most 256x256 and any aspect ratio, so it is contained, never cropped. */
const WIDTH = 160;
const HEIGHT = 40;

/**
 * The merchant's logo for the top of the card, or null when there is none to show. The page
 * re-renders on every poll: one `<img>` per URL is kept and handed back, so it neither flickers
 * nor is fetched again. A logo that fails to load is dropped for good (never a broken image icon).
 * `resolve` turns the API path into a URL on the API origin, the way the fetches do.
 */
export function merchantLogos(resolve: (path: string) => string): (merchant: Merchant | null | undefined) => HTMLElement | null {
  const made = new Map<string, HTMLElement>();
  const failed = new Set<string>();
  return (merchant) => {
    const path = merchant?.logo_url;
    if (!merchant || typeof path !== 'string' || !LOGO_PATH.test(path) || failed.has(path)) return null;
    let box = made.get(path);
    if (!box) {
      const img = h('img', { src: resolve(path), alt: merchant.name, width: WIDTH, height: HEIGHT, decoding: 'async' });
      box = h('div', { class: 'merchant-logo' }, img);
      const el = box;
      img.addEventListener('error', () => {
        failed.add(path);
        made.delete(path);
        el.remove();
      });
      made.set(path, box);
    }
    return box;
  };
}
