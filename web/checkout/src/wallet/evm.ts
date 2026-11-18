import {
  createWalletClient,
  custom,
  encodeFunctionData,
  erc20Abi,
  formatUnits,
  getAddress,
  isAddress,
  isAddressEqual,
  type Address,
  type EIP1193Provider,
  type WalletClient,
} from 'viem';
import { base, baseSepolia } from 'viem/chains';
import { throwIfAborted, type CheckoutApi, type Network, type PayTo, type TypedDataResponse } from '../lib/api';
import { toUnits } from '../lib/format';

export interface ProviderInfo {
  uuid: string;
  name: string;
  icon: string;
  rdns: string;
}

export interface DiscoveredWallet {
  info: ProviderInfo;
  provider: EIP1193Provider;
}

type AnnounceEvent = CustomEvent<{ info: ProviderInfo; provider: EIP1193Provider }>;

/**
 * Finds injected EVM wallets: every EIP-6963 announcement within `waitMs`, or the legacy
 * `window.ethereum` when no wallet announces itself.
 */
export function discoverWallets(win: Window = window, waitMs = 250): Promise<DiscoveredWallet[]> {
  return new Promise((resolve) => {
    const found = new Map<string, DiscoveredWallet>();
    const onAnnounce = (e: Event) => {
      const d = (e as AnnounceEvent).detail;
      if (d?.info?.uuid && d.provider && typeof d.provider.request === 'function') found.set(d.info.uuid, { info: d.info, provider: d.provider });
    };
    win.addEventListener('eip6963:announceProvider', onAnnounce);
    win.dispatchEvent(new Event('eip6963:requestProvider'));
    setTimeout(() => {
      win.removeEventListener('eip6963:announceProvider', onAnnounce);
      const list = [...found.values()];
      const legacy = (win as unknown as { ethereum?: EIP1193Provider }).ethereum;
      if (list.length === 0 && legacy && typeof legacy.request === 'function') {
        list.push({ info: LEGACY_INFO, provider: legacy });
      }
      resolve(list);
    }, waitMs);
  });
}

const LEGACY_INFO: ProviderInfo = { uuid: 'injected', name: 'Browser wallet', icon: '', rdns: '' };

export interface WalletWatch {
  /** Asks every wallet to announce itself again, then falls back to window.ethereum. */
  rescan(): void;
  stop(): void;
}

/**
 * Listens for EIP-6963 announcements for the life of the page: an extension can inject its
 * provider after discoverWallets has stopped waiting. Calls `onChange` with every wallet found
 * so far each time the list changes; an announced wallet replaces the window.ethereum fallback.
 */
export function watchWallets(win: Window, onChange: (wallets: DiscoveredWallet[]) => void): WalletWatch {
  const found = new Map<string, DiscoveredWallet>();
  const emit = () => onChange([...found.values()]);
  const onAnnounce = (e: Event) => {
    const d = (e as AnnounceEvent).detail;
    if (!d?.info?.uuid || !d.provider || typeof d.provider.request !== 'function' || found.has(d.info.uuid)) return;
    found.delete(LEGACY_INFO.uuid);
    found.set(d.info.uuid, { info: d.info, provider: d.provider });
    emit();
  };
  const rescan = () => {
    win.dispatchEvent(new Event('eip6963:requestProvider'));
    const legacy = (win as unknown as { ethereum?: EIP1193Provider }).ethereum;
    if (found.size === 0 && legacy && typeof legacy.request === 'function') {
      found.set(LEGACY_INFO.uuid, { info: LEGACY_INFO, provider: legacy });
      emit();
    }
  };
  win.addEventListener('eip6963:announceProvider', onAnnounce);
  rescan();
  return { rescan, stop: () => win.removeEventListener('eip6963:announceProvider', onAnnounce) };
}

export type WalletErrorKind =
  | 'rejected'
  | 'wrong_chain'
  | 'below_minimum'
  | 'not_exact'
  | 'amount_changed'
  | 'typed_data_mismatch'
  | 'bad_signature'
  | 'wallet_error';

export class WalletError extends Error {
  constructor(
    readonly kind: WalletErrorKind,
    message: string,
  ) {
    super(message);
  }
}

export const WALLET_ERROR_COPY: Record<WalletErrorKind, string> = {
  rejected: 'You declined in your wallet. Nothing was sent.',
  wrong_chain: 'Your wallet is on another network. Switch it to the network shown and try again.',
  below_minimum: 'The amount left is too small to pay by signature. Send it to the address below instead.',
  not_exact: 'This amount cannot be paid exactly from a wallet. Send it to the address below instead.',
  amount_changed: 'The amount due changed. Check the page and try again.',
  typed_data_mismatch: 'The payment details did not match this session, so nothing was signed. Reload the page and try again.',
  bad_signature: 'Your wallet returned a signature we cannot use. Nothing was sent. Try again.',
  wallet_error: 'Your wallet could not complete this. Nothing was sent. Try again.',
};

export function isUserRejection(err: unknown): boolean {
  for (let e: unknown = err, i = 0; e && i < 8; e = (e as { cause?: unknown }).cause, i++) {
    const x = e as { code?: unknown; name?: unknown };
    if (x.code === 4001 || x.name === 'UserRejectedRequestError') return true;
  }
  return false;
}

function toWalletError(err: unknown): WalletError {
  if (err instanceof WalletError) return err;
  if (isUserRejection(err)) return new WalletError('rejected', WALLET_ERROR_COPY.rejected);
  return new WalletError('wallet_error', WALLET_ERROR_COPY.wallet_error);
}

const KNOWN_CHAINS = { [base.id]: base, [baseSepolia.id]: baseSepolia } as const;

/** One connected EVM wallet. */
export class EvmWallet {
  readonly client: WalletClient;
  account: Address | null = null;

  constructor(readonly provider: EIP1193Provider) {
    this.client = createWalletClient({ transport: custom(provider) });
  }

  async connect(): Promise<Address> {
    try {
      const [first] = await this.client.requestAddresses();
      if (!first) throw new WalletError('wallet_error', WALLET_ERROR_COPY.wallet_error);
      this.account = getAddress(first);
      return this.account;
    } catch (e) {
      throw toWalletError(e);
    }
  }

  /**
   * Follows the wallet when the payer switches account or chain in it: keeps `account` current
   * (null once the site is disconnected) and calls `onChange`. Returns the unsubscribe.
   */
  watch(onChange: () => void): () => void {
    const p = this.provider as Partial<Pick<EIP1193Provider, 'on' | 'removeListener'>>;
    if (typeof p.on !== 'function') return () => {};
    const onAccounts = (accounts: readonly string[]) => {
      const [first] = accounts;
      this.account = first && isAddress(first, { strict: false }) ? getAddress(first) : null;
      onChange();
    };
    const onChain = () => onChange();
    p.on('accountsChanged', onAccounts);
    p.on('chainChanged', onChain);
    return () => {
      if (typeof p.removeListener !== 'function') return;
      p.removeListener('accountsChanged', onAccounts);
      p.removeListener('chainChanged', onChain);
    };
  }

  async ensureChain(chainId: number): Promise<void> {
    try {
      if ((await this.client.getChainId()) === chainId) return;
      try {
        await this.client.switchChain({ id: chainId });
      } catch (e) {
        const known = KNOWN_CHAINS[chainId as keyof typeof KNOWN_CHAINS];
        if (isUserRejection(e) || !known) throw e;
        await this.client.addChain({ chain: known });
      }
      if ((await this.client.getChainId()) !== chainId) throw new WalletError('wrong_chain', WALLET_ERROR_COPY.wrong_chain);
    } catch (e) {
      if (e instanceof WalletError) throw e;
      if (isUserRejection(e)) throw new WalletError('rejected', WALLET_ERROR_COPY.rejected);
      throw new WalletError('wrong_chain', WALLET_ERROR_COPY.wrong_chain);
    }
  }
}

export interface PaymentTarget {
  sessionId: string;
  payTo: PayTo;
  network: Network;
  /** What is still due, as a decimal in the asset's units (from describeSession). */
  due: string;
  expiresAt: string;
}

function tokenOf(t: PaymentTarget) {
  const asset = t.network.assets.find((a) => a.symbol === t.payTo.asset);
  if (!asset || t.network.family !== 'evm' || t.network.evm_chain_id === null) {
    throw new WalletError('typed_data_mismatch', WALLET_ERROR_COPY.typed_data_mismatch);
  }
  return { asset, chainId: t.network.evm_chain_id };
}

export interface WalletBalance {
  /** Decimal in the asset's units ("20.5"). */
  amount: string;
  /** Base units, to compare with what is due. */
  units: bigint;
}

/**
 * The connected account's balance of the asset, read through the wallet. Null when
 * the wallet is on another chain (asking it to switch only to show a balance would be a surprise)
 * or does not answer.
 */
export async function walletBalance(wallet: EvmWallet, t: PaymentTarget): Promise<WalletBalance | null> {
  try {
    const { asset, chainId } = tokenOf(t);
    if (!wallet.account || !isAddress(asset.token) || (await wallet.client.getChainId()) !== chainId) return null;
    const data = encodeFunctionData({ abi: erc20Abi, functionName: 'balanceOf', args: [wallet.account] });
    const out = await wallet.provider.request({ method: 'eth_call', params: [{ to: getAddress(asset.token), data }, 'latest'] } as never);
    if (typeof out !== 'string' || !/^0x[0-9a-fA-F]{1,64}$/.test(out)) return null;
    const units = BigInt(out);
    return { amount: formatUnits(units, asset.decimals), units };
  } catch {
    return null;
  }
}

/** The due amount in base units; an amount the token cannot represent is never rounded. */
export function dueUnits(t: PaymentTarget): bigint {
  const { asset } = tokenOf(t);
  try {
    return toUnits(t.due, asset.decimals);
  } catch {
    throw new WalletError('not_exact', WALLET_ERROR_COPY.not_exact);
  }
}

/** True when a payment of `units` settles: the fee, a fixed part plus a percentage, is less than it. */
export function settles(units: bigint, network: Pick<Network, 'fee_bps' | 'fixed_fee'>, decimals: number): boolean {
  const fee = toUnits(network.fixed_fee, decimals) + (units * BigInt(network.fee_bps)) / 10_000n;
  return fee < units;
}

/** True when what is due is too small to settle after the fee: a signature could never settle. */
export function isTooSmallToSettle(t: PaymentTarget): boolean {
  const { asset } = tokenOf(t);
  return !settles(dueUnits(t), t.network, asset.decimals);
}

const UINT = /^\d{1,78}$/;

/**
 * Checks the typed data the server returned against what this page knows, before the payer is
 * asked to sign: the right token and chain, the payer's own address, the Gateway as recipient,
 * a value no larger than what is due and large enough to settle after the fee, valid only until
 * expiry.
 */
export function checkTypedData(td: TypedDataResponse, t: PaymentTarget, from: Address): void {
  const { asset, chainId } = tokenOf(t);
  const m = td.typed_data?.message;
  const d = td.typed_data?.domain;
  const fail = () => {
    throw new WalletError('typed_data_mismatch', WALLET_ERROR_COPY.typed_data_mismatch);
  };
  if (!m || !d || td.chain !== t.payTo.chain || td.asset !== t.payTo.asset || td.session_id !== t.sessionId) fail();
  if (td.typed_data.primaryType !== 'ReceiveWithAuthorization' || !td.typed_data.types?.ReceiveWithAuthorization) fail();
  if (Number(d.chainId) !== chainId || !isAddress(d.verifyingContract) || !isAddressEqual(d.verifyingContract, asset.token as Address)) fail();
  if (!isAddress(m.from) || !isAddressEqual(m.from, from)) fail();
  if (!isAddress(td.gateway) || !isAddress(m.to) || !isAddressEqual(m.to, td.gateway)) fail();
  if (![m.value, m.validAfter, m.validBefore].every((x) => UINT.test(String(x)))) fail();
  const value = BigInt(m.value);
  if (value !== toUnits(td.amount, asset.decimals)) fail();
  if (!settles(value, t.network, asset.decimals)) throw new WalletError('below_minimum', WALLET_ERROR_COPY.below_minimum);
  if (value > dueUnits(t)) throw new WalletError('amount_changed', WALLET_ERROR_COPY.amount_changed);
  if (BigInt(m.validBefore) > BigInt(Math.floor(Date.parse(t.expiresAt) / 1000))) fail();
}

const SIGNATURE = /^0x[0-9a-fA-F]{130}$/;

/**
 * Pays USDC by EIP-3009 signature: fetch the typed data, check it, sign it with
 * eth_signTypedData_v4 exactly as served, then hand the signature to the relayer. No signature
 * is ever requested for an amount too small to settle after the fee. Once `signal` aborts, the
 * wallet is not asked and a signature that still comes back is dropped, never submitted: nothing
 * moves without the submit. `onSigned` runs once the wallet signed, right before the submit: from
 * then on the payment may go through, so an error is no longer proof that nothing was sent.
 */
export async function paySigned(api: CheckoutApi, wallet: EvmWallet, t: PaymentTarget, signal?: AbortSignal,
  onSigned?: () => void): Promise<{ jobId: string }> {
  const { chainId } = tokenOf(t);
  if (isTooSmallToSettle(t)) throw new WalletError('below_minimum', WALLET_ERROR_COPY.below_minimum);
  const from = wallet.account ?? (await wallet.connect());
  await wallet.ensureChain(chainId);
  const { data: td } = await api.typedData(t.sessionId, { chain: t.payTo.chain, asset: t.payTo.asset, from });
  checkTypedData(td, t, from);
  throwIfAborted(signal);
  let signature: string;
  try {
    signature = (await wallet.client.request({
      method: 'eth_signTypedData_v4',
      params: [from, JSON.stringify(td.typed_data)],
    } as never)) as string;
  } catch (e) {
    throw toWalletError(e);
  }
  if (typeof signature !== 'string' || !SIGNATURE.test(signature)) throw new WalletError('bad_signature', WALLET_ERROR_COPY.bad_signature);
  throwIfAborted(signal);
  onSigned?.();
  const m = td.typed_data.message;
  const { data } = await api.submit(t.sessionId, {
    chain: t.payTo.chain,
    asset: t.payTo.asset,
    from,
    value: String(m.value),
    valid_after: String(m.validAfter),
    valid_before: String(m.validBefore),
    signature,
  });
  // An answer without a job id says nothing about whether the payment went through.
  if (typeof data?.id !== 'string' || data.id === '') throw new Error('The submit answer has no job id.');
  return { jobId: data.id };
}

/**
 * Pays with a plain ERC-20 transfer of exactly what is due to the session address (USDT on EVM
 * has no signed path). The payer's wallet pays the network fee. Once `signal` aborts the wallet
 * is not asked; a hash it returns after that is still returned, since the wallet sent it.
 */
export async function payTransfer(wallet: EvmWallet, t: PaymentTarget, signal?: AbortSignal): Promise<{ txHash: string }> {
  const { asset, chainId } = tokenOf(t);
  const amount = dueUnits(t);
  if (amount <= 0n) throw new WalletError('amount_changed', WALLET_ERROR_COPY.amount_changed);
  if (!isAddress(t.payTo.address ?? '') || !isAddress(asset.token)) throw new WalletError('typed_data_mismatch', WALLET_ERROR_COPY.typed_data_mismatch);
  const from = wallet.account ?? (await wallet.connect());
  await wallet.ensureChain(chainId);
  throwIfAborted(signal);
  try {
    const hash = await wallet.client.writeContract({
      account: from,
      chain: null,
      address: getAddress(asset.token),
      abi: erc20Abi,
      functionName: 'transfer',
      args: [getAddress(t.payTo.address ?? ''), amount],
    });
    return { txHash: hash };
  } catch (e) {
    throw toWalletError(e);
  }
}
