import './fonts.css';
import './styles.css';
import { bindContracts, renderContracts } from './contracts/page';
import { esc, highlightJSON, pretty } from './format';
import {
  eventPayloadBox,
  handlerBox,
  renderConventions,
  renderEndpoint,
  renderKeys,
  renderOnchain,
  renderOverview,
  renderSessionObject,
  renderWebhooks,
  requestBox,
  responseBox,
  sampleAnswer,
  type ViewState,
} from './pages';
import { quickstartBox, renderConcepts, renderHelp, renderLifecycle, renderNetworks, renderQuickstart } from './guides';
import { htmlText, lazyIndex, operationText } from './search';
import { computeSignature, verifySignature } from './signature';
import type { Lang } from './snippets';
import { events, groups, operations } from './spec';
import { versionLabel } from './version';
import { HOOK_LANGS, type HookLang } from './webhook-handlers';

// The running build, for whoever inspects the page (version.json has the same).
document.documentElement.dataset.version = versionLabel;

export const GUIDES: Record<string, [string, (s: ViewState) => string]> = {
  quickstart: ['First payment in 5 minutes', renderQuickstart],
  overview: ['Get started', renderOverview],
  keys: ['Authentication', renderKeys],
  conventions: ['Errors and conventions', renderConventions],
  lifecycle: ['Payment lifecycle', renderLifecycle],
  networks: ['Networks and assets', renderNetworks],
  concepts: ['Links, addresses and splits', renderConcepts],
  webhooks: ['Webhooks and events', renderWebhooks],
  'session-object': ['The session object', renderSessionObject],
  contracts: ['Smart contracts', renderContracts],
  onchain: ['On-chain contracts', renderOnchain],
  help: ['Help and glossary', renderHelp],
};
export const START = ['quickstart', 'overview', 'keys', 'conventions'];
export const GUIDE_PAGES = ['lifecycle', 'networks', 'concepts', 'webhooks', 'session-object', 'contracts', 'onchain', 'help'];

const STORE_KEY = 'prismaport-docs';
const state: ViewState = { live: false, lang: 'curl', status: {}, event: 'session.paid', hook: 'node' };
let theme: 'light' | 'dark' | null = null;
try {
  const saved = JSON.parse(localStorage.getItem(STORE_KEY) ?? '{}') as { live?: unknown; lang?: unknown; hook?: unknown; theme?: unknown };
  if (typeof saved.live === 'boolean') state.live = saved.live;
  if (saved.lang === 'curl' || saved.lang === 'node' || saved.lang === 'go' || saved.lang === 'python') state.lang = saved.lang;
  if (HOOK_LANGS.some(([k]) => k === saved.hook)) state.hook = saved.hook as HookLang;
  if (saved.theme === 'light' || saved.theme === 'dark') theme = saved.theme;
} catch {
  /* storage blocked: defaults */
}
const save = () => {
  try {
    localStorage.setItem(STORE_KEY, JSON.stringify({ live: state.live, lang: state.lang, hook: state.hook, theme }));
  } catch {
    /* storage blocked */
  }
};
if (theme) document.documentElement.dataset.theme = theme;

const IS_MAC = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
const MOD = IS_MAC ? '⌘' : 'Ctrl';

const $ = <T extends HTMLElement = HTMLElement>(sel: string): T | null => document.querySelector<T>(sel);
const page = $('#page')!;
const rail = $('#rail')!;
const railLinks = $('#rail-links')!;
const main = $('main')!;
const menuBtn = $<HTMLButtonElement>('#menu-btn')!;
const backdrop = $('#backdrop')!;
const searches = [$<HTMLInputElement>('#q')!, $<HTMLInputElement>('#q-rail')!];
$('#q-kbd')!.textContent = `${MOD} K`;

const current = () => window.location.hash.slice(1) || 'overview';
const findOp = (id: string) => operations.find((o) => o.id === id);

function navLink(id: string, label: string, method?: string, ev?: string): string {
  const badge = method ? `<span class="m"><span class="method ${method === 'EVT' ? 'GET' : method}">${method === 'DELETE' ? 'DEL' : method}</span></span>` : '';
  return `<a href="#${id}" data-id="${id}"${ev ? ` data-ev-link="${esc(ev)}"` : ''}>${badge}${esc(label)}</a>`;
}

/* Search: guide pages by their rendered text, operations by fields and error codes, events by type and description. */
const guideIndex = lazyIndex(() => {
  const view: ViewState = { ...state, status: {} };
  return Object.fromEntries(Object.entries(GUIDES).map(([id, [label, render]]) => [id, `${label.toLowerCase()} ${htmlText(render(view))}`]));
});
const opIndex = lazyIndex(() => Object.fromEntries(operations.map((o) => [o.id, operationText(o)])));

function renderNav(filter = ''): void {
  const q = filter.trim().toLowerCase();
  const pageHit = (id: string) => !q || guideIndex()[id]!.includes(q);
  let html = '';
  const start = START.filter(pageHit);
  if (start.length) html += '<h4>Start here</h4>' + start.map((id) => navLink(id, GUIDES[id]![0])).join('');
  const guides = GUIDE_PAGES.filter(pageHit);
  const evHits = q ? events.filter((e) => `${e.type} ${e.description}`.toLowerCase().includes(q)) : [];
  if (guides.length || evHits.length) {
    html += '<h4>Guides</h4>' + guides.map((id) => navLink(id, GUIDES[id]![0])).join('') + evHits.map((e) => navLink('webhooks', e.type, 'EVT', e.type)).join('');
  }
  for (const g of groups) {
    const ops = g.ops.filter((o) => !q || opIndex()[o.id]!.includes(q));
    if (ops.length) html += `<h4>${esc(g.name)}</h4>` + ops.map((o) => navLink(o.id, o.summary, o.method)).join('');
  }
  railLinks.innerHTML = html || `<p class="empty">Nothing matches "${esc(filter)}".</p>`;
  markCurrent();
}

function markCurrent(): void {
  const id = current();
  railLinks.querySelectorAll<HTMLAnchorElement>('a').forEach((a) => {
    if (a.dataset.id === id && !a.dataset.evLink) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  });
}

function route(focusHeading = false): void {
  const id = current();
  const op = findOp(id);
  if (op) {
    page.innerHTML = renderEndpoint(op, state);
    document.title = `${op.summary} · Prismaport API`;
    bindTry(op.id);
  } else {
    const guide = Object.hasOwn(GUIDES, id) ? GUIDES[id]! : GUIDES.overview!;
    page.innerHTML = guide[1](state);
    document.title = `${guide[0]} · Prismaport API`;
    if (id === 'webhooks') bindWebhooks();
    if (id === 'contracts') bindContracts(page);
  }
  markCurrent();
  if (focusHeading) {
    const h1 = page.querySelector<HTMLElement>('h1');
    h1?.setAttribute('tabindex', '-1');
    h1?.focus();
  }
}

function bindTry(opId: string): void {
  const op = findOp(opId)!;
  const send = $<HTMLButtonElement>('#try-send');
  const out = $('#try-out');
  if (!send || !out) return;
  send.addEventListener('click', () => {
    const path: Record<string, string> = {};
    page.querySelectorAll<HTMLInputElement>('[data-path]').forEach((i) => (path[i.dataset.path!] = i.value.trim()));
    const answer = sampleAnswer(op, state, {
      key: $<HTMLInputElement>('#try-key')?.value.trim() ?? null,
      path,
      body: $<HTMLTextAreaElement>('#try-body')?.value ?? null,
    });
    let url = op.path;
    for (const [k, v] of Object.entries(path)) url = url.replace(`{${k}}`, v);
    const code = (answer.body as { error?: { code?: string } }).error?.code;
    const modeNote = answer.live !== state.live ? ` In ${answer.live ? 'live' : 'test'} mode, the mode of this key.` : '';
    $('#try-status')!.textContent = `${answer.status}${code ? ` ${code}` : ''}.${modeNote}`;
    out.hidden = false;
    out.innerHTML = `<div class="codebox flush"><div class="result-meta"><span class="st${Number(answer.status) >= 400 ? ' err' : ''}">${answer.status}</span><span>${op.method} ${esc(url)}</span></div><pre>${highlightJSON(pretty(answer.body))}</pre></div>`;
  });
}

function bindWebhooks(): void {
  page.querySelectorAll<HTMLButtonElement>('[data-ev]').forEach((b) =>
    b.addEventListener('click', () => {
      state.event = b.dataset.ev!;
      page.querySelectorAll('[data-ev]').forEach((x) => x.setAttribute('aria-pressed', String(x === b)));
      $('#ev-payload')!.outerHTML = eventPayloadBox(state);
    }),
  );
  $('#v-run')!.addEventListener('click', () => void runVerify());
}

async function runVerify(): Promise<void> {
  const out = $('#v-out')!;
  const set = (ok: boolean, text: string) => {
    out.className = `verify-out ${ok ? 'ok' : 'bad'}`;
    out.textContent = text;
  };
  const secret = $<HTMLInputElement>('#v-secret')!.value;
  const tIn = $<HTMLInputElement>('#v-t')!.value.trim();
  const body = $<HTMLTextAreaElement>('#v-body')!.value;
  const header = $<HTMLInputElement>('#v-header')!.value.trim();
  if (!secret) return set(false, 'Enter the signing secret, the whole whsec_ value.');
  if (!/^\d+$/.test(tIn) || !Number.isSafeInteger(Number(tIn))) return set(false, 'The timestamp must be whole Unix seconds.');
  const t = String(Number(tIn));
  if (!globalThis.crypto?.subtle) return set(false, 'This browser has no Web Crypto, so the signature cannot be computed here.');
  if (!header) return set(true, `Signature: t=${t},v1=${await computeSignature(secret, t, body)}`);
  const r = await verifySignature(body, header, secret, Math.floor(Date.now() / 1000));
  if (r.ok) return set(true, `valid: a v1 matches the signature of this body at t=${r.t}.`);
  if (r.reason === 'malformed') return set(false, 'malformed: need exactly one t (a non-negative integer) and at least one v1.');
  if (r.reason === 'timestamp') return set(false, 'timestamp: t is more than 5 minutes from now. Reject it to bound replays.');
  set(false, `mismatch: no v1 matches. Expected v1=${r.expected}`);
}

function copy(button: HTMLElement): void {
  const pre = button.closest('.codebox')?.querySelector('pre');
  if (!pre) return;
  const fallback = () => {
    const range = document.createRange();
    range.selectNodeContents(pre);
    const sel = window.getSelection();
    sel?.removeAllRanges();
    sel?.addRange(range);
    toast(`Selected. Press ${MOD} C to copy`);
  };
  try {
    navigator.clipboard.writeText(pre.textContent ?? '').then(() => toast('Copied'), fallback);
  } catch {
    fallback();
  }
}

let toastTimer: number | undefined;
function toast(msg: string): void {
  const t = $('#toast')!;
  t.textContent = msg;
  window.clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => (t.textContent = ''), 1600);
}

function setDrawer(open: boolean, restoreFocus = false): void {
  const was = rail.classList.contains('open');
  rail.classList.toggle('open', open);
  backdrop.hidden = !open;
  main.inert = open;
  menuBtn.setAttribute('aria-expanded', String(open));
  if (open) {
    const q = $<HTMLElement>('#q-rail');
    (q && q.offsetParent !== null ? q : railLinks.querySelector<HTMLElement>('a'))?.focus();
  }
  else if (was && restoreFocus) menuBtn.focus();
}

function setMode(live: boolean): void {
  state.live = live;
  save();
  $('#mode-test')!.setAttribute('aria-pressed', String(!live));
  $('#mode-live')!.setAttribute('aria-pressed', String(live));
  route();
}

function openLink(a: HTMLAnchorElement): void {
  if (a.dataset.evLink) state.event = a.dataset.evLink;
  if (a.getAttribute('href') === `#${current()}` || (a.getAttribute('href') === '#overview' && !window.location.hash)) route(true);
}

document.addEventListener('click', (e) => {
  const target = e.target as HTMLElement;
  const lang = target.closest<HTMLElement>('[data-lang]');
  if (lang) {
    state.lang = lang.dataset.lang as Lang;
    save();
    const op = findOp(current());
    if (op) {
      $('[data-box="req"]')!.outerHTML = requestBox(op, state);
      $<HTMLElement>(`[data-lang="${state.lang}"]`)?.focus();
    } else {
      // Quickstart: every step's box follows the language; focus stays in the box that was clicked.
      const step = lang.closest<HTMLElement>('[data-step]')?.dataset.step;
      page.querySelectorAll<HTMLElement>('[data-box="qs"]').forEach((box) => (box.outerHTML = quickstartBox(Number(box.dataset.step), state)));
      $<HTMLElement>(`[data-step="${step}"] [data-lang="${state.lang}"]`)?.focus();
    }
    return;
  }
  const hook = target.closest<HTMLElement>('[data-hook]');
  if (hook) {
    state.hook = hook.dataset.hook as HookLang;
    save();
    $('[data-box="hook"]')!.outerHTML = handlerBox(state);
    $<HTMLElement>(`[data-hook="${state.hook}"]`)?.focus();
    return;
  }
  const status = target.closest<HTMLElement>('[data-status]');
  if (status) {
    const op = findOp(current())!;
    state.status[op.id] = status.dataset.status!;
    $('[data-box="res"]')!.outerHTML = responseBox(op, state);
    $<HTMLElement>(`[data-status="${status.dataset.status}"]`)?.focus();
    return;
  }
  const copyBtn = target.closest<HTMLElement>('.copy');
  if (copyBtn) return copy(copyBtn);
  const navA = target.closest<HTMLAnchorElement>('.rail a');
  if (navA) {
    openLink(navA);
    setDrawer(false);
  }
});

$('#mode-test')!.addEventListener('click', () => setMode(false));
$('#mode-live')!.addEventListener('click', () => setMode(true));

const themeBtn = $('#theme-btn')!;
const isDark = () => {
  const forced = document.documentElement.dataset.theme;
  return forced ? forced === 'dark' : window.matchMedia?.('(prefers-color-scheme: dark)').matches === true;
};
themeBtn.setAttribute('aria-pressed', String(isDark()));
themeBtn.addEventListener('click', () => {
  theme = isDark() ? 'light' : 'dark';
  document.documentElement.dataset.theme = theme;
  themeBtn.setAttribute('aria-pressed', String(isDark()));
  save();
});

menuBtn.addEventListener('click', () => setDrawer(!rail.classList.contains('open'), true));
backdrop.addEventListener('click', () => setDrawer(false, true));
for (const search of searches) {
  search.addEventListener('input', () => {
    for (const other of searches) if (other !== search) other.value = search.value;
    renderNav(search.value);
  });
  search.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      // A hit on its label beats one found only in the page text.
      const q = search.value.trim().toLowerCase();
      const hits = [...railLinks.querySelectorAll<HTMLAnchorElement>('a')];
      const a = hits.find((h) => q && h.textContent!.toLowerCase().includes(q)) ?? hits[0];
      if (!a) return;
      setDrawer(false);
      if (a.getAttribute('href') === `#${current()}`) openLink(a);
      else {
        if (a.dataset.evLink) state.event = a.dataset.evLink;
        window.location.hash = a.getAttribute('href')!;
      }
    }
    if (e.key === 'Escape' && search.value) {
      e.stopPropagation();
      for (const s of searches) s.value = '';
      renderNav();
    }
  });
}
document.addEventListener('keydown', (e) => {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') {
    e.preventDefault();
    if (window.matchMedia?.('(max-width: 560px)').matches) setDrawer(true);
    else searches[0]!.focus();
  }
  if (e.key === 'Escape' && rail.classList.contains('open')) setDrawer(false, true);
});
window.addEventListener('hashchange', () => {
  route(true);
  window.scrollTo(0, 0);
});

$('#mode-test')!.setAttribute('aria-pressed', String(!state.live));
$('#mode-live')!.setAttribute('aria-pressed', String(state.live));
renderNav();
route();
document.body.removeAttribute('aria-busy');
