import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { render, screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { AssetBadge, AssetOnNetwork, FamilyLogos, NetworkLogo, TestnetTag, TokenAmount, TokenLogo } from '../src/components/Logos';
import { chainFamily, familyChains, isTestnet, networkInfo, tokenInfo } from '../src/lib/logos';
import { renderApp, useMockServer } from './helpers';

useMockServer();

// jsdom gives import.meta.url an http scheme; vitest runs from the app root.
const logos = join(process.cwd(), 'src', 'logos');
const fileOf = (img: Element) => img.getAttribute('src')!.replace(/^.*\/(\w+)-64\.png$/, '$1');

describe('logo files', () => {
  it('ships every logo at 64 and 128 px, as small PNGs', () => {
    expect(readdirSync(logos)).toHaveLength(20);
    for (const n of ['usdc', 'usdt', 'base', 'solana', 'ethereum', 'arbitrum', 'optimism', 'polygon', 'bsc', 'avalanche']) {
      for (const s of [64, 128]) {
        const png = readFileSync(join(logos, `${n}-${s}.png`));
        expect(png.subarray(1, 4).toString('latin1')).toBe('PNG');
        expect([png.readUInt32BE(16), png.readUInt32BE(20)]).toEqual([s, s]);
        expect(png.length).toBeLessThan(s === 64 ? 4096 : 9216);
      }
    }
  });
});

describe('logo helpers', () => {
  it('knows USDC and USDT, in any case, with 1x and 2x files', () => {
    expect(tokenInfo('USDC').logo?.src).toMatch(/usdc-64\.png$/);
    expect(tokenInfo('usdt').logo?.srcSet).toMatch(/usdt-64\.png 1x, .*usdt-128\.png 2x$/);
    expect(tokenInfo('USDT').free).toBe(true);
    expect(tokenInfo('PYUSD').logo).toBeNull();
  });

  it('testnets reuse the mainnet logo', () => {
    expect(networkInfo('base')).toMatchObject({ name: 'Base', mainnet: 'base', testnet: false, file: 'base', ring: 'light' });
    expect(networkInfo('base-sepolia')).toMatchObject({ name: 'Base Sepolia', mainnet: 'base', testnet: true, file: 'base' });
    expect(networkInfo('solana-devnet')).toMatchObject({ name: 'Solana devnet', mainnet: 'solana', testnet: true, ring: 'dark' });
    expect(networkInfo('solana-devnet').logo).toEqual(networkInfo('solana').logo);
    expect(isTestnet('base-sepolia')).toBe(true);
    expect(isTestnet('base')).toBe(false);
  });

  it('has no logo for a network without a file', () => {
    expect(networkInfo('celo')).toMatchObject({ name: 'Celo', testnet: false, logo: null, file: null });
  });

  it('maps destination families to the networks they pay out on', () => {
    expect(chainFamily('solana-devnet')).toBe('solana');
    expect(chainFamily('base-sepolia')).toBe('evm');
    const live = [{ chain: 'base', asset: 'USDC', enabled: true }, { chain: 'solana', asset: 'USDT', enabled: true }];
    expect(familyChains('evm', live)).toEqual(['base']);
    expect(familyChains('solana', live)).toEqual(['solana']);
    expect(familyChains('evm', [...live, { chain: 'arbitrum', asset: 'USDT', enabled: true }])).toEqual(['base', 'arbitrum']);
    expect(familyChains('evm', [{ chain: 'arbitrum', asset: 'USDT', enabled: false }, { chain: 'base', asset: 'USDC', enabled: true }])).toEqual(['base']);
    expect(familyChains('evm', [])).toEqual(['base']);
    expect(familyChains('solana', [{ chain: 'solana-devnet', asset: 'USDC', enabled: false }])).toEqual(['solana-devnet']);
  });
});

describe('logo components', () => {
  it('TokenLogo: the asset as alt text, explicit size, no inline style', () => {
    const { container } = render(<TokenLogo asset="USDC" size={24} />);
    const img = screen.getByRole('img', { name: 'USDC' });
    expect(img).toHaveAttribute('width', '24');
    expect(img).toHaveAttribute('height', '24');
    expect(img.getAttribute('srcset')).toContain('2x');
    expect(container.querySelector('[style]')).toBeNull();
  });

  it('decorative logos have empty alt text and are hidden from assistive tech', () => {
    const { container } = render(<TokenLogo asset="USDT" decorative />);
    expect(container.querySelector('img')).toHaveAttribute('alt', '');
    expect(container.firstElementChild).toHaveAttribute('aria-hidden', 'true');
    expect(screen.queryByRole('img')).toBeNull();
  });

  it('an unknown asset or network falls back to its initial, never a broken image', () => {
    const { container } = render(
      <>
        <TokenLogo asset="PYUSD" />
        <NetworkLogo chain="plasma" />
        <NetworkLogo chain="celo" decorative />
      </>,
    );
    expect(container.querySelector('img')).toBeNull();
    expect(screen.getByRole('img', { name: 'PYUSD' })).toHaveTextContent('P');
    expect(screen.getByRole('img', { name: 'Plasma' })).toHaveTextContent('P');
    expect(screen.queryByRole('img', { name: 'Celo' })).toBeNull();
  });

  it('NetworkLogo on a testnet keeps the mainnet logo and shows a Testnet tag', () => {
    const { container } = render(<NetworkLogo chain="base-sepolia" />);
    expect(screen.getByRole('img', { name: 'Base Sepolia' })).toHaveAttribute('src', networkInfo('base').logo!.src);
    expect(container).toHaveTextContent('Testnet');
    render(<NetworkLogo chain="base" />);
    expect(screen.getAllByText('Testnet')).toHaveLength(1);
  });

  it('the Testnet tag is left out only when the caller prints it', () => {
    const { container } = render(<NetworkLogo chain="solana-devnet" tag={false} />);
    expect(container).not.toHaveTextContent('Testnet');
    const tag = render(<TestnetTag chain="solana-devnet" />);
    expect(tag.container).toHaveTextContent('Testnet');
    expect(render(<TestnetTag chain="solana" />).container).toBeEmptyDOMElement();
  });

  it('AssetBadge: one image named "USDC on Base", with the network logo at the corner', () => {
    const { container } = render(<AssetBadge asset="USDC" chain="base" />);
    const badge = screen.getByRole('img', { name: 'USDC on Base' });
    const imgs = [...badge.querySelectorAll('img')];
    expect(imgs.map((i) => i.getAttribute('alt'))).toEqual(['', '']);
    expect(imgs.map(fileOf)).toEqual(['usdc', 'base']);
    expect(imgs.map((i) => i.getAttribute('width'))).toEqual(['24', '12']);
    expect(container.querySelector('.logo-corner')).not.toBeNull();
  });

  it('AssetOnNetwork prints the name, so the badge is decorative; testnets get the tag', () => {
    const { container } = render(<AssetOnNetwork asset="USDC" chain="solana-devnet" />);
    expect(container).toHaveTextContent('USDC on Solana devnetTestnet');
    expect(screen.queryByRole('img')).toBeNull();
    expect(container.querySelector('.asset-badge')).toHaveAttribute('aria-hidden', 'true');
  });

  it('TokenAmount puts the token logo before the amount', () => {
    const { container } = render(<TokenAmount asset="USDT">10.00 USDT</TokenAmount>);
    expect(container).toHaveTextContent(/^10\.00 USDT$/);
    expect(fileOf(container.querySelector('img')!)).toBe('usdt');
  });

  it('FamilyLogos: Base for EVM while Base is the only EVM network; one Testnet tag', () => {
    const live = render(<FamilyLogos family="evm" accepted={[{ chain: 'base', asset: 'USDC', enabled: true }]} />);
    expect([...live.container.querySelectorAll('img')].map(fileOf)).toEqual(['base']);
    const test = render(
      <FamilyLogos family="solana" accepted={[{ chain: 'solana-devnet', asset: 'USDC', enabled: true }, { chain: 'solana-devnet', asset: 'USDT', enabled: true }]} />,
    );
    expect([...test.container.querySelectorAll('img')].map(fileOf)).toEqual(['solana']);
    expect(test.container).toHaveTextContent(/^Testnet$/);
  });
});

describe('logos on the screens', () => {
  const files = (el: Element) => [...el.querySelectorAll('img')].map(fileOf);
  const allDecorative = (el: Element) => {
    for (const img of el.querySelectorAll('img')) {
      expect(img.getAttribute('alt')).toBe('');
      expect(img.closest('[aria-hidden="true"]')).not.toBeNull();
    }
  };

  it('overview: each total shows its token on network and the token by the net amount', async () => {
    renderApp('/overview');
    const totals = await screen.findAllByTestId('total');
    for (const t of totals) {
      const label = t.querySelector('.label')!;
      const [asset, , chain] = label.textContent!.split(' ');
      expect(files(label)).toEqual([asset!.toLowerCase(), chain!.toLowerCase()]);
      expect(files(t.querySelector('.amount-hero')!)).toEqual([asset!.toLowerCase()]);
      allDecorative(t);
    }
    const recent = await screen.findAllByTestId('recent-payment');
    expect(recent[0]!.querySelector('.asset-badge')).toHaveAttribute('aria-hidden', 'true');
  });

  it('payment links: received amounts carry the token logo, in the list and the detail', async () => {
    const { user } = renderApp('/links');
    const row = (await screen.findAllByRole('link', { name: 'Founder plan' })).find((a) => a.classList.contains('row-link'))!.closest('li')!;
    const received = row.querySelector('.c-received')!;
    expect(files(received).length).toBeGreaterThan(0);
    expect(files(received).every((f) => f === 'usdc' || f === 'usdt')).toBe(true);
    allDecorative(received);
    await user.click(within(row as HTMLElement).getByRole('link', { name: 'Founder plan' }));
    await screen.findByRole('heading', { level: 1, name: 'Founder plan' });
    const stats = screen.getByRole('region', { name: 'Link statistics' });
    expect(files(stats).length).toBeGreaterThan(0);
  });

  it('payments and payment detail: the badge per transfer, the network on the settlement, the badge per address', async () => {
    const { user } = renderApp('/payments?status=paid');
    const rows = await screen.findAllByTestId('session-row');
    expect(rows[0]!.querySelectorAll('.paid-with .asset-badge').length).toBeGreaterThan(0);
    await user.click(rows[0]!);
    await screen.findByRole('heading', { name: 'Timeline' });
    const settlement = screen.getByTestId('settlement');
    const heading = settlement.querySelector('h3')!;
    expect(heading.textContent).toMatch(/^Settled on (Base|Solana)$/);
    expect(files(heading)).toEqual([heading.textContent!.split(' ').pop()!.toLowerCase()]);
    // Received and fee, then the token logo on each payout and a network badge per payout.
    expect(settlement.querySelectorAll('dd .logo').length).toBe(2);
    for (const p of within(settlement).getAllByTestId('payout')) {
      const asset = p.querySelector('.payout-amount')!.textContent!.trim().split(' ').pop()!.toLowerCase();
      expect(files(p)).toEqual([heading.textContent!.split(' ').pop()!.toLowerCase(), asset]);
      expect(p.querySelector('.network-badge [data-logo]')!.getAttribute('aria-hidden')).toBe('true');
      expect(p.querySelector('.payout-amount .logo')).not.toBeNull();
    }
    for (const p of screen.getAllByTestId('payment')) expect(p.querySelector('.asset-badge')).not.toBeNull();
    const payTo = screen.getAllByTestId('pay-to');
    expect(payTo.map((p) => files(p))).toEqual([['usdc', 'base'], ['usdc', 'solana'], ['usdt', 'solana']]);
    allDecorative(screen.getByRole('main'));
  });

  it('settings: a badge per accepted stablecoin and the network logo per destination', async () => {
    renderApp('/settings/networks');
    await screen.findByTestId('network-solana');
    expect(screen.getByRole('checkbox', { name: 'USDT on Solana' })).toBeInTheDocument();
    const checks = [...document.querySelectorAll('[data-testid^="network-"] .asset-check')];
    expect(checks.map((c) => files(c))).toEqual([['usdc', 'base'], ['usdc', 'solana'], ['usdt', 'solana']]);
  });

  it('settings in Test mode: mainnet logos with a Testnet tag', async () => {
    renderApp('/settings/networks', { mode: 'test' });
    await screen.findByTestId('network-base-sepolia');
    expect(screen.getByRole('checkbox', { name: /^USDC on Base Sepolia ?Testnet$/ })).toBeInTheDocument();
  });

  it('link drawer preview: what the payer can choose on the next step', async () => {
    const { user } = renderApp('/products');
    await user.click(await screen.findByRole('button', { name: 'Add product' }));
    const preview = await screen.findByTestId('preview-assets');
    expect(preview).toHaveTextContent('USDC on BaseUSDC on SolanaUSDT on Solana');
    expect([...preview.querySelectorAll('li')].map((li) => files(li))).toEqual([['usdc', 'base'], ['usdc', 'solana'], ['usdt', 'solana']]);
  });
});
