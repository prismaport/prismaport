import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { beforeAll, describe, expect, it } from 'vitest';
import { EVENT_GUIDE, GLOSSARY, MIN_SETTLING, QUICKSTART_OPS, STATUS_GUIDE, WATCH_AFTER_EXPIRY } from '../src/guides';
import { FEE_BPS, FIXED_FEE } from '../src/networks';
import { events, operations, schemaByName } from '../src/spec';

const shell = readFileSync(join(process.cwd(), 'index.html'), 'utf8');
const body = shell.slice(shell.indexOf('<body'), shell.indexOf('</body>')).replace(/<body[^>]*>/, '').replace(/<script[\s\S]*?<\/script>/g, '');
const go = async (hash: string) => {
  window.location.hash = hash;
  window.dispatchEvent(new HashChangeEvent('hashchange'));
  await Promise.resolve();
};
const page = () => document.getElementById('page')!;
const click = (el: Element | null) => (el as HTMLElement).click();
const goSrc = (p: string) => readFileSync(join(process.cwd(), '../..', p), 'utf8');

let main: typeof import('../src/main');
beforeAll(async () => {
  window.scrollTo = () => {};
  document.body.innerHTML = body;
  main = await import('../src/main');
});

const pageIds = () => [...main.START, ...main.GUIDE_PAGES, ...operations.map((o) => o.id)];

describe('sidebar', () => {
  it('has an entry for every guide, and every entry opens its own page', async () => {
    expect([...main.START, ...main.GUIDE_PAGES].sort()).toEqual(Object.keys(main.GUIDES).sort());
    const links = [...document.querySelectorAll<HTMLAnchorElement>('#rail a')];
    for (const a of links) {
      const id = a.getAttribute('href')!.slice(1);
      await go(id);
      const label = Object.hasOwn(main.GUIDES, id) ? main.GUIDES[id]![0] : operations.find((o) => o.id === id)?.summary;
      expect(label, id).toBeDefined();
      expect(document.title, id).toBe(`${label} · Prismaport API`);
      expect(page().querySelector('h1'), id).not.toBeNull();
    }
  });
});

describe('links', () => {
  it('every #link on every page, in both modes, opens a page that exists', async () => {
    const known = new Set(pageIds());
    for (const live of [false, true]) {
      click(document.getElementById(live ? 'mode-live' : 'mode-test'));
      for (const id of pageIds()) {
        await go(id);
        for (const a of page().querySelectorAll<HTMLAnchorElement>('a[href^="#"]')) {
          expect(known.has(a.getAttribute('href')!.slice(1)), `${id} -> ${a.getAttribute('href')}`).toBe(true);
        }
        for (const a of page().querySelectorAll<HTMLAnchorElement>('a[href]')) {
          const href = a.getAttribute('href')!;
          expect(/^(#|https:\/\/|\/dashboard\/|openapi\.json$)/.test(href), `${id} -> ${href}`).toBe(true);
        }
      }
    }
    click(document.getElementById('mode-test'));
  });
});

describe('quickstart', () => {
  it('shows a request for each step from the spec, and one tab click switches every box', async () => {
    await go('quickstart');
    expect(page().querySelector('h1')?.textContent).toBe('First payment in 5 minutes');
    const boxes = () => [...page().querySelectorAll('[data-box="qs"] pre')].map((p) => p.textContent!);
    expect(boxes()).toHaveLength(QUICKSTART_OPS.length);
    click(page().querySelector('[data-step="3"] [data-lang="python"]'));
    for (const text of boxes()) expect(text).toContain('requests.');
    expect(document.activeElement?.closest('[data-step]')?.getAttribute('data-step')).toBe('3');
    click(page().querySelector('[data-step="0"] [data-lang="curl"]'));
    const curl = boxes();
    expect(curl[0]).toContain('/v1/wallets');
    expect(curl[3]).toContain('Idempotency-Key');
    expect(curl[4]).toContain('/v1/test_helpers/sessions/');
    expect(curl.every((t) => !t.includes('sk_live_'))).toBe(true);
    expect(page().querySelectorAll('a[href^="https://faucet."]').length).toBe(1);
  });
});

describe('payment lifecycle', () => {
  it('covers every session status and every webhook event', async () => {
    const statuses = schemaByName('Session')!.properties!.status!.enum as string[];
    expect(Object.keys(STATUS_GUIDE).sort()).toEqual([...statuses].sort());
    expect(Object.keys(EVENT_GUIDE).sort()).toEqual(events.map((e) => e.type).sort());
    await go('lifecycle');
    expect(page().querySelector('.diagram svg')).not.toBeNull();
    const text = page().textContent!;
    expect(text).toContain('The gateway never refunds.');
    expect(text).not.toMatch(/frozen/i);
  });

  it('tells support which funds a deposit address can never move (ADR 0008 I-3, I-4)', async () => {
    await go('lifecycle');
    expect(page().querySelector('#unrecoverable')?.textContent).toBe('Funds that cannot be recovered');
    const items = [...page().querySelectorAll('#unrecoverable-list li')].map((li) => li.textContent!);
    expect(items).toHaveLength(5);
    for (const want of ['ETH', 'SOL', 'USDbC', 'Token-2022', '36 decimals', 'wrong network']) {
      expect(items.some((i) => i.includes(want)), want).toBe(true);
    }
  });

  it('states the watch window and the settling minimum as the code computes them', () => {
    const d = /DefaultWatchAfterExpiry\s*=\s*(\d+)\s*\*\s*time\.Hour\s*\+\s*(\d+)\s*\*\s*time\.Minute/.exec(goSrc('internal/tuning/tuning.go'))!;
    expect(WATCH_AFTER_EXPIRY).toBe(`${d[1]} h ${d[2]} min`);
    // The least gross (in base units of a 6-decimal token) whose fee is below it.
    const fixed = Number(FIXED_FEE) * 1e6;
    let g = fixed;
    while (fixed + Math.floor((g * FEE_BPS) / 10_000) >= g) g++;
    expect((g / 1e6).toFixed(6)).toBe(MIN_SETTLING);
  });
});

describe('networks page', () => {
  it('lists the open networks and one coming-later row for Solana', async () => {
    await go('networks');
    const rows = [...page().querySelectorAll('#networks-table tbody tr')].map((tr) => tr.textContent!);
    // Base and Base Sepolia (USDC), then one muted row for Solana, which opens later.
    expect(rows).toHaveLength(3);
    expect(rows.filter((r) => r.includes('Coming later'))).toHaveLength(1);
    expect(rows.filter((r) => r.includes('Solana') && r.includes('USDC, USDT') && r.includes('Listed when Solana opens'))).toHaveLength(1);
    expect(page().querySelector('#networks-table tbody')!.textContent).not.toContain('devnet');
    expect(page().innerHTML).not.toContain('faucet.solana.com');
    expect(page().querySelector('.lede')!.textContent).toBe('Stablecoins only, priced 1:1 in USD. Live mode pays on Base, test mode on Base Sepolia. Solana is coming later. The same fee on every network.');
  });
});

describe('Base-only launch copy', () => {
  it('quickstart says test mode runs on Base Sepolia and going live is on Base only', async () => {
    click(document.getElementById('mode-test'));
    await go('quickstart');
    expect(page().querySelector('.lede')!.textContent).toContain('Test mode runs on Base Sepolia, and test tokens have no value.');
    const callout = page().querySelector('.callout')!.textContent!;
    expect(callout).toContain('key, on Base. Live mode is open as soon as you confirm your email.');
    expect(callout).not.toContain('Solana');
  });

  it('overview lists Base in live mode and Base Sepolia in test mode', async () => {
    for (const [live, networks, assets] of [[true, 'Base', 'USDC on Base'], [false, 'Base Sepolia (test mode)', 'USDC on Base Sepolia']] as const) {
      click(document.getElementById(live ? 'mode-live' : 'mode-test'));
      await go('overview');
      const dd = (term: string) => [...page().querySelectorAll('dt')].find((t) => t.textContent === term)!.nextElementSibling!.textContent;
      expect(dd('Networks')).toBe(networks);
      expect(dd('Assets')).toBe(assets);
    }
    click(document.getElementById('mode-test'));
  });
});

describe('help', () => {
  it('defines each glossary term once and shows no support address', async () => {
    const terms = GLOSSARY.map(([t]) => t.toLowerCase());
    expect(new Set(terms).size).toBe(terms.length);
    for (const t of ['session', 'pay_to', 'deposit address', 'forwarder', 'flush', 'settlement', 'finalization', 'customer address', 'payment link', 'dead letter']) expect(terms).toContain(t);
    await go('help');
    expect(page().innerHTML).not.toMatch(/mailto:|@prismaport\.com/);
  });
});

describe('search', () => {
  const search = (q: string) => {
    const input = document.getElementById('q') as HTMLInputElement;
    input.value = q;
    input.dispatchEvent(new Event('input'));
    const hrefs = [...document.querySelectorAll('#rail a')].map((a) => a.getAttribute('href'));
    input.value = '';
    input.dispatchEvent(new Event('input'));
    return hrefs;
  };

  it('finds guides by their text, operations by fields and error codes', () => {
    expect(search('dead letter')).toContain('#help');
    expect(search('starting_after')).toContain('#conventions');
    expect(search('idempotency_conflict')).toEqual(expect.arrayContaining(['#createSession', '#conventions']));
    expect(search('client_reference_id')).toEqual(expect.arrayContaining(['#createSession', '#session-object']));
    expect(search('address_rotated')).toContain('#createSession');
  });
});
