import type { ReactNode } from 'react';
import { familyChains, networkInfo, tokenInfo, type LogoInfo } from '../lib/logos';
import type { AcceptedAsset, Family } from '../lib/types';

export type LogoSize = 16 | 20 | 24;

interface LogoProps {
  size?: LogoSize;
  /** The name is printed next to the logo: hide the logo from assistive tech. */
  decorative?: boolean;
}

/** One logo on its circle, or the name's first letter when there is no file. Never a broken image. */
function Logo({ info, size, decorative, className }: { info: LogoInfo; size: number; decorative: boolean; className?: string }) {
  const common = {
    'data-logo': info.file ?? 'none',
    'data-ring': info.ring,
    'data-free': info.free ? '' : undefined,
    'data-size': size,
    'aria-hidden': decorative ? (true as const) : undefined,
  };
  if (!info.logo) {
    return (
      <span className={`logo logo-text ${className ?? ''}`} {...common} role={decorative ? undefined : 'img'} aria-label={decorative ? undefined : info.name}>
        {info.name.trim().charAt(0).toUpperCase()}
      </span>
    );
  }
  return (
    <span className={`logo ${className ?? ''}`} {...common}>
      <img src={info.logo.src} srcSet={info.logo.srcSet} alt={decorative ? '' : info.name} width={size} height={size} decoding="async" />
    </span>
  );
}

/** The visible "Testnet" tag next to a test network: never colour alone. */
export function TestnetTag({ chain }: { chain: string }) {
  return networkInfo(chain).testnet ? <span className="testnet-tag">Testnet</span> : null;
}

export function TokenLogo({ asset, size = 20, decorative = false }: LogoProps & { asset: string }) {
  return <Logo info={tokenInfo(asset)} size={size} decorative={decorative} />;
}

interface NetworkProps extends LogoProps {
  chain: string;
  /** Set false only where the caller prints <TestnetTag> itself. */
  tag?: boolean;
}

export function NetworkLogo({ chain, size = 20, decorative = false, tag = true }: NetworkProps) {
  const n = networkInfo(chain);
  const mark = <Logo info={n} size={size} decorative={decorative} />;
  if (!n.testnet || !tag) return mark;
  return (
    <span className="logo-group">
      {mark}
      <TestnetTag chain={chain} />
    </span>
  );
}

const CORNER: Record<LogoSize, number> = { 16: 8, 20: 10, 24: 12 };

/** The token logo with the network's logo at its lower right corner, for "USDC on Base". */
export function AssetBadge({ asset, chain, size = 24, decorative = false, tag = true }: NetworkProps & { asset: string }) {
  const n = networkInfo(chain);
  const badge = (
    <span
      className="asset-badge"
      data-size={size}
      role={decorative ? undefined : 'img'}
      aria-label={decorative ? undefined : `${asset} on ${n.name}`}
      aria-hidden={decorative ? true : undefined}
    >
      <Logo info={tokenInfo(asset)} size={size} decorative />
      <Logo info={n} size={CORNER[size]} decorative className="logo-corner" />
    </span>
  );
  if (!n.testnet || !tag) return badge;
  return (
    <span className="logo-group">
      {badge}
      <TestnetTag chain={chain} />
    </span>
  );
}

/** "USDC on Base" with its badge, and the Testnet tag on a test network. */
export function AssetOnNetwork({ asset, chain, size = 20 }: { asset: string; chain: string; size?: LogoSize }) {
  const n = networkInfo(chain);
  return (
    <span className="with-logo">
      <AssetBadge asset={asset} chain={chain} size={size} decorative tag={false} />
      <span>
        {asset} on {n.name}
      </span>
      <TestnetTag chain={chain} />
    </span>
  );
}

/** "10.00 USDC" with the token logo before the amount. */
export function TokenAmount({ asset, children, size = 16 }: { asset: string; children: ReactNode; size?: LogoSize }) {
  return (
    <span className="with-logo token-amount">
      <TokenLogo asset={asset} size={size} decorative />
      <span>{children}</span>
    </span>
  );
}

/** The logos of the networks a destination family pays out on (Base alone for EVM today). */
export function FamilyLogos({ family, accepted, size = 20 }: { family: Family; accepted: AcceptedAsset[]; size?: LogoSize }) {
  const chains = familyChains(family, accepted);
  const mainnets = [...new Map(chains.map((c) => [networkInfo(c).mainnet, c])).values()];
  return (
    <span className="logo-group" data-testid={`family-logos-${family}`}>
      {mainnets.map((c) => (
        <NetworkLogo key={c} chain={c} size={size} decorative tag={false} />
      ))}
      {chains.some((c) => networkInfo(c).testnet) ? <span className="testnet-tag">Testnet</span> : null}
    </span>
  );
}
