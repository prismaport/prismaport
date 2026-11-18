import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { beforeAll, describe, expect, it } from 'vitest';
import { operations } from '../src/spec';

// Loads the real page shell and app, then walks every page in both modes.
const shell = readFileSync(join(process.cwd(), 'index.html'), 'utf8');
const body = shell.slice(shell.indexOf('<body'), shell.indexOf('</body>')).replace(/<body[^>]*>/, '').replace(/<script[\s\S]*?<\/script>/g, '');

const go = async (hash: string) => {
  window.location.hash = hash;
  window.dispatchEvent(new HashChangeEvent('hashchange'));
  await Promise.resolve();
};
const page = () => document.getElementById('page')!;
const click = (el: Element | null) => (el as HTMLElement).click();

beforeAll(async () => {
  window.scrollTo = () => {};
  document.body.innerHTML = body;
  await import('../src/main');
});

const GUIDES = ['quickstart', 'overview', 'keys', 'conventions', 'lifecycle', 'networks', 'concepts', 'webhooks', 'session-object', 'contracts', 'onchain', 'help'];

describe('every page renders', () => {
  for (const live of [false, true]) {
    it(`in ${live ? 'live' : 'test'} mode`, async () => {
      click(document.getElementById(live ? 'mode-live' : 'mode-test'));
      for (const id of [...GUIDES, ...operations.map((o) => o.id)]) {
        await go(id);
        expect(page().querySelector('h1'), id).not.toBeNull();
        expect(page().textContent, id).not.toMatch(/undefined|\[object Object\]|NaN/);
        expect(page().innerHTML, id).not.toContain(' style="');
      }
    });
  }
});

describe('authentication', () => {
  it('lists every scope of internal/apikey with its operations, wallets read only', async () => {
    const src = readFileSync(join(process.cwd(), '../../internal/apikey/apikey.go'), 'utf8');
    const goScopes = [...src.matchAll(/Scope = "([a-z_]+:(?:read|write))"/g)].map((m) => m[1]);
    await go('keys');
    const text = page().textContent!;
    expect(text).toContain(`A full-access key holds all ${goScopes.length}.`);
    const rows = [...page().querySelectorAll('#scopes tr')];
    expect(rows.map((r) => r.querySelector('code')!.textContent).sort()).toEqual([...goScopes].sort());
    for (const r of rows) {
      const scope = r.querySelector('code')!.textContent;
      const hrefs = [...r.querySelectorAll('a')].map((a) => a.getAttribute('href'));
      expect(hrefs).toEqual(operations.filter((o) => o.scope === scope).map((o) => `#${o.id}`));
    }
    expect(text).not.toContain('wallets:write');
  });

  it('says a live key takes the password, plus the 2FA code when it is on', async () => {
    await go('keys');
    const text = page().textContent!;
    expect(text).toContain('Creating or rolling a live key asks for your password, plus your 2FA code when 2FA is on.');
    expect(text).not.toContain('Rolling a live key asks for your 2FA code.');
  });
});

describe('navigation', () => {
  it('lists every operation and marks the current page', async () => {
    const links = [...document.querySelectorAll<HTMLAnchorElement>('#rail a')];
    for (const o of operations) expect(links.some((a) => a.getAttribute('href') === `#${o.id}`)).toBe(true);
    await go('createSession');
    expect(document.querySelector('#rail a[aria-current="page"]')?.getAttribute('href')).toBe('#createSession');
  });

  it('lists the guides in order, Smart contracts before On-chain contracts', () => {
    const guides = [...document.querySelectorAll<HTMLAnchorElement>('#rail a')].filter((a) => !a.querySelector('.m')).map((a) => a.textContent);
    expect(guides).toEqual([
      'First payment in 5 minutes',
      'Get started',
      'Authentication',
      'Errors and conventions',
      'Payment lifecycle',
      'Networks and assets',
      'Links, addresses and splits',
      'Webhooks and events',
      'The session object',
      'Smart contracts',
      'On-chain contracts',
      'Help and glossary',
    ]);
  });

  it('opens the Smart contracts page with its controls bound', async () => {
    await go('contracts');
    expect(document.title).toBe('Smart contracts · Prismaport API');
    expect(document.querySelector('#rail a[aria-current="page"]')?.getAttribute('href')).toBe('#contracts');
    click(page().querySelector('[data-attempt="replay"]'));
    expect(document.getElementById('cx-result')?.textContent).toContain('Use the same signature twice');
  });

  it('filters by the search box', () => {
    const q = document.getElementById('q') as HTMLInputElement;
    q.value = 'webhook_endpoints';
    q.dispatchEvent(new Event('input'));
    const hrefs = [...document.querySelectorAll('#rail a .m')].map((m) => m.parentElement!.getAttribute('href'));
    expect(hrefs.length).toBeGreaterThan(0);
    const paths = hrefs.map((h) => operations.find((o) => `#${o.id}` === h)?.path ?? '');
    expect(paths.every((p) => p.includes('/v1/webhook_endpoints'))).toBe(true);
    q.value = '';
    q.dispatchEvent(new Event('input'));
  });
});

describe('endpoint page', () => {
  it('switches language and status without leaving the page', async () => {
    click(document.getElementById('mode-test'));
    await go('createSession');
    click(page().querySelector('[data-lang="python"]'));
    expect(page().querySelector('[data-box="req"] pre')?.textContent).toContain('requests.post(');
    click(page().querySelector('[data-status="409"]'));
    expect(page().querySelector('[data-box="res"] pre')?.textContent).toContain('idempotency_conflict');
  });

  it('shows the sample answer for the inputs', async () => {
    await go('getWallet');
    (document.getElementById('try-id') as HTMLInputElement).value = 'xyz';
    click(document.getElementById('try-send'));
    expect(document.getElementById('try-out')?.textContent).toContain('no such wallet');
    (document.getElementById('try-key') as HTMLInputElement).value = 'nope';
    click(document.getElementById('try-send'));
    expect(document.getElementById('try-out')?.textContent).toContain('invalid_api_key');
  });
});

describe('webhooks page', () => {
  it('shows each event with the session in the state it announces', async () => {
    await go('webhooks');
    click(page().querySelector('[data-ev="session.expired"]'));
    const payload = JSON.parse(document.querySelector('#ev-payload pre')!.textContent!);
    expect(payload.type).toBe('session.expired');
    expect(payload.data.session.status).toBe('expired');
    expect(page().querySelector('[data-ev="session.expired"]')?.getAttribute('aria-pressed')).toBe('true');
  });

  it('shows handlers that need no Prismaport package, one tab per language', async () => {
    await go('webhooks');
    expect(page().innerHTML).not.toContain('@prismaport/');
    for (const [lang, needle] of [['python', 'hmac.compare_digest'], ['go', 'hmac.Equal'], ['php', 'hash_equals'], ['node', 'timingSafeEqual']]) {
      click(page().querySelector(`[data-hook="${lang}"]`));
      expect(page().querySelector('[data-box="hook"] pre')?.textContent, lang).toContain(needle);
      expect(page().querySelector(`[data-hook="${lang}"]`)?.getAttribute('aria-pressed')).toBe('true');
    }
  });
});

describe('conventions page', () => {
  it('lists every error code the operations answer, with its statuses', async () => {
    await go('conventions');
    const rows = new Map([...page().querySelectorAll('#error-codes tr')].map((tr) => [tr.children[1]!.textContent!, tr.children[0]!.textContent!]));
    for (const op of operations) {
      for (const e of op.errors) expect(rows.get(e.code)?.split(' '), `${op.id} ${e.code}`).toContain(String(e.status));
    }
    expect(rows.get('internal_error')).toBe('500');
  });
});

describe('on-chain contracts page', () => {
  it('links each deployment to its explorer and says Solana mainnet is not deployed', async () => {
    await go('onchain');
    const links = [...page().querySelectorAll<HTMLAnchorElement>('.addr-note a')].map((a) => a.href);
    expect(links).toHaveLength(7);
    expect(links.filter((h) => h.startsWith('https://basescan.org/address/0x'))).toHaveLength(3);
    expect(links.filter((h) => h.startsWith('https://sepolia.basescan.org/address/0x'))).toHaveLength(3);
    expect(links.filter((h) => h.startsWith('https://explorer.solana.com/address/') && h.endsWith('?cluster=devnet'))).toHaveLength(1);
    expect([...page().querySelectorAll('.addr-note dd')].filter((dd) => dd.textContent === 'not deployed yet')).toHaveLength(1);
    expect(page().textContent).not.toContain('published when the contracts are deployed');
  });
});

describe('focus and keyboard', () => {
  it('moves focus to the new page heading after navigating', async () => {
    await go('listEvents');
    expect(document.activeElement?.tagName).toBe('H1');
  });

  it('survives fragments named like object members', async () => {
    for (const id of ['constructor', 'toString', '__proto__']) {
      await go(id);
      expect(page().querySelector('h1')?.textContent, id).toBe('Get paid in stablecoins from your own code');
    }
  });

  it('focuses the search with the shortcut and opens the first hit on Enter', async () => {
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', ctrlKey: true }));
    const q = document.getElementById('q') as HTMLInputElement;
    expect(document.activeElement).toBe(q);
    q.value = 'Retrieve a wallet';
    q.dispatchEvent(new Event('input'));
    q.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter' }));
    await Promise.resolve();
    expect(window.location.hash).toBe('#getWallet');
    q.value = '';
    q.dispatchEvent(new Event('input'));
  });

  it('makes the page inert while the drawer is open and returns focus to the menu on Escape', () => {
    const menu = document.getElementById('menu-btn')!;
    click(menu);
    expect(document.getElementById('rail')!.classList.contains('open')).toBe(true);
    expect((document.querySelector('main') as HTMLElement).inert).toBe(true);
    expect(menu.getAttribute('aria-expanded')).toBe('true');
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    expect(document.getElementById('rail')!.classList.contains('open')).toBe(false);
    expect((document.querySelector('main') as HTMLElement).inert).toBe(false);
    expect(document.activeElement).toBe(menu);
  });

  it('remembers the chosen theme', () => {
    click(document.getElementById('theme-btn'));
    const theme = document.documentElement.dataset.theme;
    expect(JSON.parse(localStorage.getItem('prismaport-docs') ?? '{}').theme).toBe(theme);
  });
});

describe('signature checker', () => {
  it('signs with the timestamp as the Go helper formats it', async () => {
    await go('webhooks');
    await new Promise((r) => setTimeout(r, 20));
    (document.getElementById('v-secret') as HTMLInputElement).value = 'whsec_test';
    (document.getElementById('v-t') as HTMLInputElement).value = '01761240000';
    (document.getElementById('v-body') as HTMLTextAreaElement).value = '{"id":"evt_1"}';
    (document.getElementById('v-header') as HTMLInputElement).value = '';
    click(document.getElementById('v-run'));
    const outEl = document.getElementById('v-out')!;
    for (let i = 0; i < 100 && outEl.textContent!.startsWith('signed_payload'); i++) await new Promise((r) => setTimeout(r, 20));
    const out = outEl.textContent!;
    expect(out).toBe('Signature: t=1761240000,v1=27b7b99a1b97b1b82cfe33170c926269604cc5e36bbf29169553b84bf6c9d06b');
  });
});
