// Solana connected-wallet payments, isolated from the rest of the page.
//
// The flow follows the Solana Pay "transaction request" shape: the backend builds one
// transaction (net and fee transfers plus the session reference, with the gateway as fee payer
// and its signature already attached), the wallet adds the payer's signature and sends it; the
// backend credits it through the session reference. The page only offers this method for a
// pay_to entry that advertises `connected_wallet: true`.

import { getWallets } from '@wallet-standard/app';
import type { Wallet, WalletAccount } from '@wallet-standard/base';
import { throwIfAborted, type CheckoutApi, type PayTo } from '../lib/api';

export const SIGN_AND_SEND = 'solana:signAndSendTransaction';
const CONNECT = 'standard:connect';

export interface SolanaTransactionApi {
  /** Returns the server-built transaction, base64 encoded, for this payer account. */
  buildTransaction(sessionId: string, req: { chain: string; asset: string; account: string }): Promise<{ transaction: string }>;
}

export class SolanaUnavailableError extends Error {
  constructor() {
    super('Paying with a connected Solana wallet is not available yet.');
  }
}

export const UnavailableSolanaApi: SolanaTransactionApi = {
  buildTransaction: () => Promise.reject(new SolanaUnavailableError()),
};

/** The backend's POST /v1/checkout/sessions/{id}/solana/transaction. */
export function backendSolanaApi(api: Pick<CheckoutApi, 'solanaTransaction'>): SolanaTransactionApi {
  return {
    async buildTransaction(sessionId, req) {
      const { data } = await api.solanaTransaction(sessionId, req);
      return { transaction: data.transaction };
    },
  };
}

/** The Wallet Standard chain of a Prismaport chain id. */
export function standardChain(chain: string): `solana:${string}` | null {
  if (chain === 'solana') return 'solana:mainnet';
  if (chain === 'solana-devnet') return 'solana:devnet';
  return null;
}

/** Whether the page offers the connected-wallet method for this option. */
export function solanaWalletSupported(payTo: PayTo): boolean {
  return payTo.connected_wallet === true && standardChain(payTo.chain) !== null;
}

export function usableWallets(wallets: readonly Wallet[], chain: string): Wallet[] {
  const sc = standardChain(chain);
  if (!sc) return [];
  return wallets.filter((w) => CONNECT in w.features && SIGN_AND_SEND in w.features && w.chains.includes(sc));
}

export function listSolanaWallets(chain: string, source: () => readonly Wallet[] = () => getWallets().get()): Wallet[] {
  try {
    return usableWallets(source(), chain);
  } catch {
    return [];
  }
}

/** A payer declining in the wallet: EIP-1193 style code 4001, or a Wallet Standard "rejected" error. */
export function isWalletRejection(err: unknown): boolean {
  for (let e: unknown = err, i = 0; e && i < 8; e = (e as { cause?: unknown }).cause, i++) {
    const x = e as { code?: unknown; message?: unknown };
    if (x.code === 4001) return true;
    if (typeof x.message === 'string' && /user rejected|rejected by (the )?user|rejected the request|request rejected/i.test(x.message)) return true;
  }
  return false;
}

const B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';

export function base58(bytes: Uint8Array): string {
  let n = 0n;
  for (const b of bytes) n = (n << 8n) | BigInt(b);
  let out = '';
  while (n > 0n) {
    out = B58[Number(n % 58n)] + out;
    n /= 58n;
  }
  for (const b of bytes) {
    if (b !== 0) break;
    out = '1' + out;
  }
  return out;
}

function fromBase64(s: string): Uint8Array {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

type ConnectFeature = { connect: (input?: { silent?: boolean }) => Promise<{ accounts: readonly WalletAccount[] }> };
type SignAndSendFeature = {
  signAndSendTransaction: (
    ...inputs: { account: WalletAccount; transaction: Uint8Array; chain: string }[]
  ) => Promise<readonly { signature: Uint8Array }[]>;
};

/** Connects the wallet and returns the account it shares for this chain. */
export async function connectSolanaWallet(wallet: Wallet, payChain: string): Promise<WalletAccount> {
  const chain = standardChain(payChain);
  if (!chain) throw new SolanaUnavailableError();
  const connect = wallet.features[CONNECT] as ConnectFeature | undefined;
  if (!connect || !wallet.features[SIGN_AND_SEND]) throw new SolanaUnavailableError();
  const { accounts } = await connect.connect();
  const account = accounts.find((a) => a.chains.includes(chain)) ?? accounts[0];
  if (!account) throw new Error('The wallet did not share an account.');
  return account;
}

/**
 * Signs the server-built transaction and sends it, connecting first unless the page already did
 * (`connected`). Returns the signature. Once `signal` aborts the wallet is not asked to send; a
 * signature it returns after that is still returned, since the wallet sent the transaction itself.
 * `onAsk` runs as the wallet is asked to send: from then on it can send the transaction even if the
 * page lets go of the request.
 */
export async function payWithSolanaWallet(
  wallet: Wallet,
  api: SolanaTransactionApi,
  sessionId: string,
  payTo: PayTo,
  connected?: WalletAccount,
  signal?: AbortSignal,
  onAsk?: () => void,
): Promise<{ signature: string; account: string }> {
  const chain = standardChain(payTo.chain);
  if (!chain) throw new SolanaUnavailableError();
  const send = wallet.features[SIGN_AND_SEND] as SignAndSendFeature | undefined;
  if (!send) throw new SolanaUnavailableError();
  const account = connected ?? (await connectSolanaWallet(wallet, payTo.chain));
  const { transaction } = await api.buildTransaction(sessionId, { chain: payTo.chain, asset: payTo.asset, account: account.address });
  throwIfAborted(signal);
  onAsk?.();
  const [result] = await send.signAndSendTransaction({ account, transaction: fromBase64(transaction), chain });
  if (!result) throw new Error('The wallet did not return a signature.');
  return { signature: base58(result.signature), account: account.address };
}
