import type { Network, NetworkAsset, PayTo } from './api';

const NAMES: Record<string, string> = {
  base: 'Base',
  'base-sepolia': 'Base Sepolia',
  solana: 'Solana',
  'solana-devnet': 'Solana devnet',
  arbitrum: 'Arbitrum',
  polygon: 'Polygon',
  optimism: 'Optimism',
};

/** The payer's view of a transaction on the network's explorer, for the chains Prismaport serves. */
export function explorerTxUrl(chain: string, tx: string): string | null {
  const r = encodeURIComponent(tx);
  switch (chain) {
    case 'base':
      return `https://basescan.org/tx/${r}`;
    case 'base-sepolia':
      return `https://sepolia.basescan.org/tx/${r}`;
    case 'solana':
      return `https://solscan.io/tx/${r}`;
    case 'solana-devnet':
      return `https://solscan.io/tx/${r}?cluster=devnet`;
    default:
      return null;
  }
}

/** The network name payers read ("Base"), never the chain id. */
export function networkName(chain: string): string {
  return NAMES[chain] ?? chain.replace(/-/g, ' ').replace(/^./, (c) => c.toUpperCase());
}

const NATIVE: Record<string, string> = {
  ethereum: 'ETH',
  base: 'ETH',
  'base-sepolia': 'ETH',
  arbitrum: 'ETH',
  optimism: 'ETH',
  polygon: 'POL',
  bsc: 'BNB',
  avalanche: 'AVAX',
  solana: 'SOL',
  'solana-devnet': 'SOL',
};

/** The token a chain charges its network fee in ("ETH" on Base), or null when not known. */
export function nativeSymbol(chain: string): string | null {
  return NATIVE[chain] ?? null;
}

export function isSolana(chain: string, networks?: Network[]): boolean {
  const n = networks?.find((x) => x.id === chain);
  return n ? n.family === 'solana' : chain.startsWith('solana');
}

export function findNetwork(networks: Network[], chain: string): Network | undefined {
  return networks.find((n) => n.id === chain);
}

export function findAsset(networks: Network[], payTo: Pick<PayTo, 'chain' | 'asset'>): NetworkAsset | undefined {
  return findNetwork(networks, payTo.chain)?.assets.find((a) => a.symbol === payTo.asset);
}

export const payToKey = (p: Pick<PayTo, 'chain' | 'asset'>): string => `${p.chain}:${p.asset}`;

/**
 * Under a Solana deposit address: some exchanges refuse withdrawals to an off-curve
 * (program-derived) owner. The payer is told so without naming exchanges: which ones refuse is
 * third-party data we have not verified and that changes over time.
 */
export const SOLANA_EXCHANGE_NOTE = "Some exchanges can't withdraw to this type of Solana address. If yours refuses it, send from a wallet or another exchange.";
