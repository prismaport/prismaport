import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { WALLET_DOWNLOADS, assetBadge, networkInfo, networkLogo, testnetTag, tokenInfo, tokenLogo, walletLogo } from '../src/lib/logos';

// jsdom gives import.meta.url an http scheme; vitest runs from the app root.
const logos = join(process.cwd(), 'src', 'logos');

describe('logo files', () => {
  it('ships every logo at 64 and 128 px, as small PNGs', () => {
    const names = readdirSync(logos).sort();
    for (const n of ['usdc', 'usdt', 'base', 'solana', 'ethereum', 'arbitrum', 'optimism', 'polygon', 'bsc', 'avalanche',
      'wallet-metamask', 'wallet-rabby', 'wallet-phantom']) {
      for (const s of [64, 128]) {
        const f = join(logos, `${n}-${s}.png`);
        expect(existsSync(f), `${n}-${s}.png`).toBe(true);
        const png = readFileSync(f);
        expect(png.subarray(1, 4).toString('latin1')).toBe('PNG');
        expect([png.readUInt32BE(16), png.readUInt32BE(20)]).toEqual([s, s]);
        expect(png.length).toBeLessThan(s === 64 ? 4096 : 9216);
      }
    }
    expect(names).toHaveLength(26);
  });
});

describe('walletLogo', () => {
  it('uses the bundled logo for a known wallet, by rdns or by name', () => {
    expect(walletLogo({ rdns: 'io.metamask', name: 'MetaMask' }).querySelector('img')!.getAttribute('src')).toMatch(/wallet-metamask-64\.png$/);
    expect(walletLogo({ rdns: 'io.metamask.flask', name: 'MetaMask Flask' }).dataset.logo).toBe('wallet-metamask');
    expect(walletLogo({ rdns: 'io.rabby', name: 'Rabby Wallet' }).dataset.logo).toBe('wallet-rabby');
    expect(walletLogo({ rdns: '', name: 'Phantom' }).dataset.logo).toBe('wallet-phantom');
    expect(walletLogo({ rdns: 'com.example', name: 'MetaMask' }).dataset.logo).toBe('none');
    const img = walletLogo({ rdns: 'app.phantom', name: 'Phantom' }, 28).querySelector('img')!;
    expect(img.getAttribute('srcset')).toMatch(/wallet-phantom-64\.png 1x, .*wallet-phantom-128\.png 2x$/);
    expect(img.alt).toBe('');
  });

  it('draws an outline for an unknown wallet and never the announced data: icon', () => {
    const tile = walletLogo({ rdns: 'com.example', name: 'Other' }, 20);
    expect(tile.dataset.logo).toBe('none');
    expect(tile.querySelector('img')).toBeNull();
    expect(tile.querySelector('svg.icon')).not.toBeNull();
    expect(tile.getAttribute('aria-hidden')).toBe('true');
  });

  it('offers only https download pages, each with a bundled logo', () => {
    for (const d of WALLET_DOWNLOADS) {
      expect(d.url).toMatch(/^https:\/\//);
      expect(walletLogo(d).dataset.logo).not.toBe('none');
    }
  });
});

describe('tokenInfo and networkInfo', () => {
  it('knows USDC and USDT, in any case', () => {
    expect(tokenInfo('USDC').logo?.src).toMatch(/usdc-64\.png$/);
    expect(tokenInfo('usdt').logo?.srcset).toMatch(/usdt-64\.png 1x, .*usdt-128\.png 2x$/);
    expect(tokenInfo('PYUSD').logo).toBeNull();
  });

  it('maps mainnets to their own logo and is not a testnet', () => {
    const base = networkInfo('base');
    expect(base).toMatchObject({ name: 'Base', mainnet: 'base', testnet: false, file: 'base' });
    expect(base.logo?.src).toMatch(/base-64\.png$/);
    expect(networkInfo('solana')).toMatchObject({ name: 'Solana', testnet: false, file: 'solana' });
  });

  it('testnets reuse the mainnet logo', () => {
    expect(networkInfo('base-sepolia')).toMatchObject({ name: 'Base Sepolia', mainnet: 'base', testnet: true, file: 'base' });
    expect(networkInfo('solana-devnet')).toMatchObject({ name: 'Solana devnet', mainnet: 'solana', testnet: true, file: 'solana' });
    expect(networkInfo('solana-devnet').logo).toEqual(networkInfo('solana').logo);
  });

  it('a network without a file has no logo', () => {
    expect(networkInfo('celo')).toMatchObject({ name: 'Celo', testnet: false, logo: null, file: null });
    expect(networkInfo('plasma').logo).toBeNull();
  });
});

describe('tokenLogo', () => {
  it('carries the asset name as alt text, with explicit size for no layout shift', () => {
    const el = tokenLogo('USDC', { size: 24 });
    const img = el.querySelector('img')!;
    expect(img.alt).toBe('USDC');
    expect(img.getAttribute('width')).toBe('24');
    expect(img.getAttribute('height')).toBe('24');
    expect(img.getAttribute('srcset')).toContain('2x');
    expect(el.dataset.size).toBe('24');
    expect(el.getAttribute('aria-hidden')).toBeNull();
    expect(el.hasAttribute('style')).toBe(false);
  });

  it('is decorative when the name is printed next to it', () => {
    const el = tokenLogo('USDT', { decorative: true });
    expect(el.querySelector('img')!.getAttribute('alt')).toBe('');
    expect(el.getAttribute('aria-hidden')).toBe('true');
    expect(el.dataset.size).toBe('20');
  });

  it('an unknown asset falls back to text, never a broken image', () => {
    const el = tokenLogo('PYUSD');
    expect(el.querySelector('img')).toBeNull();
    expect(el.textContent).toBe('P');
    expect(el.getAttribute('role')).toBe('img');
    expect(el.getAttribute('aria-label')).toBe('PYUSD');
    const quiet = tokenLogo('PYUSD', { decorative: true });
    expect(quiet.getAttribute('role')).toBeNull();
    expect(quiet.getAttribute('aria-hidden')).toBe('true');
  });

  it('marks the logos that need a ring or must not be clipped', () => {
    expect(tokenLogo('USDT').hasAttribute('data-free')).toBe(true);
    expect(tokenLogo('USDC').hasAttribute('data-free')).toBe(false);
    expect(networkLogo('solana').dataset.ring).toBe('dark');
    expect(networkLogo('base').dataset.ring).toBe('light');
  });
});

describe('networkLogo', () => {
  it('a mainnet is just the logo with the network name as alt', () => {
    const el = networkLogo('base');
    expect(el.classList.contains('logo')).toBe(true);
    expect(el.querySelector('img')!.alt).toBe('Base');
    expect(el.textContent).toBe('');
  });

  it('a testnet adds a visible Testnet tag, not colour alone', () => {
    const el = networkLogo('base-sepolia');
    expect(el.querySelector('img')!.alt).toBe('Base Sepolia');
    expect(el.querySelector('img')!.getAttribute('src')).toBe(networkInfo('base').logo!.src);
    expect(el.querySelector('.testnet-tag')!.textContent).toBe('Testnet');
    expect(networkLogo('solana-devnet', { decorative: true }).textContent).toBe('Testnet');
  });

  it('leaves the tag out only when asked, for callers that print it', () => {
    expect(networkLogo('base-sepolia', { tag: false }).querySelector('.testnet-tag')).toBeNull();
    expect(testnetTag().textContent).toBe('Testnet');
  });

  it('an unknown network falls back to its initial', () => {
    const el = networkLogo('plasma');
    expect(el.querySelector('img')).toBeNull();
    expect(el.textContent).toBe('P');
    expect(el.getAttribute('aria-label')).toBe('Plasma');
  });
});

describe('assetBadge', () => {
  it('stacks the network logo at the corner of the token logo, named as one image', () => {
    const el = assetBadge('USDC', 'base');
    expect(el.getAttribute('role')).toBe('img');
    expect(el.getAttribute('aria-label')).toBe('USDC on Base');
    const imgs = [...el.querySelectorAll('img')];
    expect(imgs.map((i) => i.alt)).toEqual(['', '']);
    expect(imgs.map((i) => i.getAttribute('width'))).toEqual(['24', '14']);
    expect(el.querySelector('.logo-corner img')!.getAttribute('src')).toMatch(/base-64\.png$/);
  });

  it('is hidden from assistive tech when decorative', () => {
    const el = assetBadge('USDT', 'solana', { size: 20, decorative: true });
    expect(el.getAttribute('aria-hidden')).toBe('true');
    expect(el.getAttribute('role')).toBeNull();
    expect(el.querySelectorAll('img')[1]!.getAttribute('width')).toBe('10');
  });

  it('on a testnet keeps the mainnet logo and adds the Testnet tag', () => {
    const el = assetBadge('USDC', 'base-sepolia');
    expect(el.querySelector('[role=img]')!.getAttribute('aria-label')).toBe('USDC on Base Sepolia');
    expect(el.querySelector('.testnet-tag')!.textContent).toBe('Testnet');
  });

  it('falls back to text for an unknown token or network', () => {
    const el = assetBadge('USDC', 'celo');
    expect(el.querySelectorAll('img')).toHaveLength(1);
    expect(el.querySelector('.logo-corner')!.textContent).toBe('C');
    expect(assetBadge('PYUSD', 'base').querySelector('.logo')!.textContent).toBe('P');
  });
});
