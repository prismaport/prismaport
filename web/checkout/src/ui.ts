import { COPY, checkedCopy, nextCheckCopy, offlineDetail, type Pill } from './lib/copy';
import { h, icon } from './lib/dom';
import { formatCountdown, truncateMiddle } from './lib/format';
import { versionLabel } from './lib/version';

const BRAND = `${import.meta.env.BASE_URL}brand/`;

export function pill(p: Pill): HTMLElement {
  return h('span', { class: 'pill', 'data-tone': p.tone }, p.label);
}

let announcer: HTMLElement | null = null;

/** Announces a short confirmation ("Address copied") to screen readers. */
export function announce(text: string): void {
  if (!announcer || !announcer.isConnected) {
    announcer = h('div', { class: 'sr-only', role: 'status', 'aria-live': 'polite' });
    document.body.append(announcer);
  }
  announcer.textContent = '';
  const el = announcer;
  setTimeout(() => (el.textContent = text), 20);
}

async function writeClipboard(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    const ta = h('textarea', { class: 'sr-only', readonly: true, 'aria-hidden': 'true' });
    ta.value = text;
    document.body.append(ta);
    ta.select();
    let ok = false;
    try {
      ok = document.execCommand('copy');
    } catch {
      ok = false;
    }
    ta.remove();
    return ok;
  }
}

/** A copy button that always copies the full value, whatever is displayed next to it. */
export function copyButton(value: string, what: string, fid: string): HTMLButtonElement {
  const label = h('span', {}, 'Copy');
  const btn = h('button', { type: 'button', class: 'btn btn-small', 'aria-label': `Copy ${what}`, 'data-fid': fid }, icon('copy'), label);
  btn.addEventListener('click', async () => {
    const ok = await writeClipboard(value);
    label.textContent = ok ? 'Copied' : 'Copy failed';
    announce(ok ? `${what[0]!.toUpperCase()}${what.slice(1)} copied` : 'Copy failed. Select the text and copy it yourself.');
    setTimeout(() => (label.textContent = 'Copy'), 1500);
  });
  return btn;
}

/** A square copy button with no label, for a row with no room for one; it shows a check once copied. */
export function copyIconButton(value: string, what: string, fid: string): HTMLButtonElement {
  const btn = h('button', { type: 'button', class: 'btn btn-small btn-icon', 'aria-label': `Copy ${what}`, title: `Copy ${what}`, 'data-fid': fid }, icon('copy'));
  btn.addEventListener('click', async () => {
    const ok = await writeClipboard(value);
    if (ok) btn.replaceChildren(icon('check'));
    announce(ok ? `${what[0]!.toUpperCase()}${what.slice(1)} copied` : 'Copy failed. Select the text and copy it yourself.');
    setTimeout(() => btn.replaceChildren(icon('copy')), 1500);
  });
  return btn;
}

/** A middle-truncated address or hash, with the full value for assistive tech and on hover. */
export function shortValue(value: string, cls = 'address'): HTMLElement {
  return h('span', { class: `${cls} value`, title: value },
    h('span', { 'aria-hidden': 'true' }, truncateMiddle(value)),
    h('span', { class: 'sr-only' }, value));
}

/** The Prismaport mark, white on a dark scheme; decorative, the name is always printed next to it. */
function brandMark(size: number): HTMLPictureElement {
  const pic = document.createElement('picture');
  pic.append(
    h('source', { srcset: `${BRAND}prismaport-mark-white.svg`, media: '(prefers-color-scheme: dark)' }),
    h('img', { src: `${BRAND}prismaport-mark-ink.svg`, alt: '', width: size, height: size }),
  );
  return pic;
}

/** The page's own header, above the card: the page is Prismaport's, the card is the merchant's sale. */
export function brandHeader(): HTMLElement {
  return h('header', { class: 'brand-header' }, brandMark(24), h('span', { class: 'brand-name' }, 'Prismaport'));
}

export function footer(): HTMLElement {
  return h('footer', { class: 'footer', title: `Running build ${versionLabel}` }, brandMark(20), h('span', {}, COPY.footer));
}

/** A card with a heading and one line; with `titleId` the heading takes that id and names the card. */
export function messageCard(title: string, text: string, titleId?: string): HTMLElement {
  return h('section', { class: 'card stack', 'aria-labelledby': titleId }, h('h1', { class: 'display', id: titleId }, title), h('p', {}, text));
}

/** The last-resort card of a page that could not be drawn. */
export const renderFailedCard = (): HTMLElement => messageCard('Something went wrong', COPY.renderFailed, 'title');

/** Whole seconds from `at` to `nowMs`, never negative (a corrected clock can step back). */
const secondsSince = (at: number, nowMs: number): number => Math.max(0, Math.floor((nowMs - at) / 1000));

/**
 * The API stopped answering after a first answer: the page keeps trying by itself, says how old
 * what it shows is, and lets the payer try at once. `lastOk` is when the API last answered.
 * `nextCheckMs`: "Try now" came while Prismaport asked the page to wait (Retry-After), so the
 * notice says when it checks again; the page keeps `[data-next-check]` current.
 */
export function offlineNotice(lastOk: number, nowMs: number, onRetry: () => void, nextCheckMs: number | null = null): HTMLElement {
  return h('div', { class: 'offline-notice', role: 'status', 'data-fid': 'offline' },
    icon('wifi-off', 20),
    h('div', { class: 'offline-text' },
      h('p', { class: 'offline-title' }, COPY.reconnecting),
      checkedLine(lastOk, nowMs, 'offline'),
      // The countdown is not live: inside the status region it would be read out every second.
      nextCheckMs === null ? null : h('p', { 'data-fid': 'retry-wait' }, `${COPY.retryWait} `,
        h('span', { 'aria-live': 'off', 'data-next-check': '' }, nextCheckCopy(formatCountdown(nextCheckMs))))),
    h('button', { type: 'button', class: 'btn btn-small', 'data-fid': 'offline-retry', onclick: onRetry }, 'Try now'));
}

/**
 * "Checked 12 s ago" (or the offline notice's "Last checked…"), kept current by `tickChecked`.
 * Not live: inside a status region a ticking count would be read out every second.
 */
export function checkedLine(lastOk: number, nowMs: number, kind: 'offline' | 'checked', cls?: string): HTMLElement {
  return h('p', { class: cls, 'aria-live': 'off', 'data-checked': kind }, checkedText(kind, secondsSince(lastOk, nowMs)));
}

const checkedText = (kind: string | undefined, n: number) => (kind === 'offline' ? offlineDetail(n) : checkedCopy(n));

/** Moves every checked line under `root` to `nowMs`; `lastOk` is when the API last answered. */
export function tickChecked(root: ParentNode, lastOk: number, nowMs: number): void {
  const n = secondsSince(lastOk, nowMs);
  for (const el of root.querySelectorAll<HTMLElement>('[data-checked]')) el.textContent = checkedText(el.dataset.checked, n);
}

/** Re-renders root with node, keeping keyboard focus on the element with the same data-fid. */
export function replace(root: HTMLElement, ...nodes: Node[]): void {
  const active = document.activeElement as HTMLElement | null;
  const fid = active && root.contains(active) ? active.getAttribute('data-fid') : null;
  root.replaceChildren(...nodes);
  if (fid) {
    const next = root.querySelector<HTMLElement>(`[data-fid="${CSS.escape(fid)}"]`);
    next?.focus({ preventScroll: true });
  }
}
