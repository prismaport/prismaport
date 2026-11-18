// The networks and assets the gateway accepts, in one place for the portal. Sources:
// internal/chain/chain.go (the chain registry: ids, modes, tokens, decimals, platform fee),
// internal/chainwatch/chainwatch.go (DefaultConfirmations), internal/config/solana.go (credit at
// confirmed), internal/fee/fee.go (MinSettlingCents). tests/networks.test.ts holds this module to
// those files and to the GET /v1/checkout/networks example.

interface NetworkAsset {
  symbol: 'USDC' | 'USDT';
  token: string;
  decimals: number;
  /** Payable by an EIP-3009 signature, so Prismaport sends the transaction. */
  signed: boolean;
}

export interface Network {
  id: string;
  name: string;
  family: 'evm' | 'solana';
  live: boolean;
  /** Not open yet: kept in the registry, but the copy says "coming later" and no example or faucet uses it. */
  comingLater?: boolean;
  evmChainId: number | null;
  assets: NetworkAsset[];
  /** When a payment counts towards the session. */
  credit: string;
  /** When it can no longer be reversed. */
  final: string;
  explorer: (address: string) => string;
}

export const FEE_BPS = 50;
export const FIXED_FEE = '0.10';
/** The smallest price a product or session can have: below it the fee would take the whole payment. */
export const MIN_AMOUNT = '0.11';
/** Blocks a Base payment waits for, by default, before it counts. */
export const EVM_CONFIRMATIONS = 10;

const EVM_CREDIT = `${EVM_CONFIRMATIONS} blocks`;
const EVM_FINAL = 'its block at or below the finalized head, hash unchanged';
const SOL_CREDIT = 'commitment confirmed';
const SOL_FINAL = 'its slot finalized, hash unchanged';

export const NETWORKS: Network[] = [
  {
    id: 'base',
    name: 'Base',
    family: 'evm',
    live: true,
    evmChainId: 8453,
    assets: [{ symbol: 'USDC', token: '0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913', decimals: 6, signed: true }],
    credit: EVM_CREDIT,
    final: EVM_FINAL,
    explorer: (a) => `https://basescan.org/address/${a}`,
  },
  {
    id: 'solana',
    name: 'Solana',
    family: 'solana',
    live: true,
    comingLater: true,
    evmChainId: null,
    assets: [
      { symbol: 'USDC', token: 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', decimals: 6, signed: false },
      { symbol: 'USDT', token: 'Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB', decimals: 6, signed: false },
    ],
    credit: SOL_CREDIT,
    final: SOL_FINAL,
    explorer: (a) => `https://explorer.solana.com/address/${a}`,
  },
  {
    id: 'base-sepolia',
    name: 'Base Sepolia',
    family: 'evm',
    live: false,
    evmChainId: 84532,
    assets: [{ symbol: 'USDC', token: '0x036CbD53842c5426634e7929541eC2318f3dCF7e', decimals: 6, signed: true }],
    credit: EVM_CREDIT,
    final: EVM_FINAL,
    explorer: (a) => `https://sepolia.basescan.org/address/${a}`,
  },
  {
    id: 'solana-devnet',
    name: 'Solana devnet',
    family: 'solana',
    live: false,
    comingLater: true,
    evmChainId: null,
    assets: [{ symbol: 'USDC', token: '4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU', decimals: 6, signed: false }],
    credit: SOL_CREDIT,
    final: SOL_FINAL,
    explorer: (a) => `https://explorer.solana.com/address/${a}?cluster=devnet`,
  },
];

/** The networks that are open today, by mode. Public copy lists these and mentions the rest as coming later. */
export const openNetworks = (live: boolean): Network[] => NETWORKS.filter((n) => n.live === live && !n.comingLater);
export const COMING_LATER = NETWORKS.filter((n) => n.comingLater && n.live);

/** Test tokens and gas for the test networks. */
export const FAUCETS: [label: string, href: string, what: string][] = [
  ['Circle faucet', 'https://faucet.circle.com/', 'test USDC on Base Sepolia'],
  ['Base Sepolia faucets', 'https://docs.base.org/base-chain/tools/network-faucets', 'Sepolia ETH for the network fee of a transfer you send yourself'],
];
