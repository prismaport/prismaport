import type { Network, NetworkAsset, PayTo } from './api';
import { AmountError, toUnits } from './format';

// The deposit address is an off-curve program-derived address: wallets that derive the
// recipient's token account without allowing an off-curve owner fail, so verify on real wallets first.
export const SOLANA_TRANSFER_URI_ENABLED = false;

export class PayUriError extends Error {}

const EVM_ADDRESS = /^0x[0-9a-fA-F]{40}$/;
const BASE58_KEY = /^[1-9A-HJ-NP-Za-km-z]{32,44}$/;
const DECIMAL = /^\d+(?:\.\d+)?$/;

function atomic(amount: string, decimals: number): bigint {
  if (!Number.isInteger(decimals) || decimals < 0) throw new PayUriError(`invalid decimals: ${decimals}`);
  if (!DECIMAL.test(amount)) throw new PayUriError(`not a decimal amount: ${amount}`);
  let units: bigint;
  try {
    units = toUnits(amount, decimals);
  } catch (e) {
    throw new PayUriError(e instanceof AmountError ? e.message : `invalid amount: ${amount}`);
  }
  if (units <= 0n) throw new PayUriError(`amount must be positive: ${amount}`);
  return units;
}

export interface Eip681Transfer {
  chainId: number;
  token: string;
  to: string;
  /**
   * Decimal string in token units ("25.00"); trailing zeros beyond `decimals` are allowed. Null
   * leaves the amount out (an open-amount session: the wallet asks the payer how much).
   */
  amount: string | null;
  decimals: number;
}

/**
 * An EIP-681 ERC-20 `transfer` request: `ethereum:<token>@<chainId>/transfer?address=<to>&uint256=<base units>`,
 * without `uint256` when there is no amount.
 */
export function eip681({ chainId, token, to, amount, decimals }: Eip681Transfer): string {
  if (!Number.isSafeInteger(chainId) || chainId <= 0) throw new PayUriError(`invalid chain id: ${chainId}`);
  if (!EVM_ADDRESS.test(token)) throw new PayUriError(`invalid token address: ${token}`);
  if (!EVM_ADDRESS.test(to)) throw new PayUriError(`invalid recipient: ${to}`);
  const base = `ethereum:${token}@${chainId}/transfer?address=${to}`;
  return amount === null ? base : `${base}&uint256=${atomic(amount, decimals).toString()}`;
}

export interface SolanaPayTransfer {
  recipient: string;
  /** Decimal string in token units; sent without trailing zeros ("25.000000" is "25"). Null leaves it out. */
  amount: string | null;
  splToken: string;
  reference?: string;
  label: string;
  message: string;
  /** When set, an amount with more significant decimals than the mint has is rejected. */
  decimals?: number;
}

function uiAmount(amount: string, decimals?: number): string {
  const m = /^(\d+)(?:\.(\d+))?$/.exec(amount);
  if (!m) throw new PayUriError(`not a decimal amount: ${amount}`);
  const f = (m[2] ?? '').replace(/0+$/, '');
  if (decimals !== undefined && f.length > decimals) throw new PayUriError(`${amount} has more than ${decimals} decimals`);
  const i = (m[1] ?? '0').replace(/^0+(?=\d)/, '');
  if (/^0*$/.test(i + f)) throw new PayUriError(`amount must be positive: ${amount}`);
  return f ? `${i}.${f}` : i;
}

/** A Solana Pay transfer request (https://docs.solanapay.com/spec), parameters in the spec's order. */
export function solanaPayTransfer({ recipient, amount, splToken, reference, label, message, decimals }: SolanaPayTransfer): string {
  if (!BASE58_KEY.test(recipient)) throw new PayUriError(`invalid recipient: ${recipient}`);
  if (!BASE58_KEY.test(splToken)) throw new PayUriError(`invalid mint: ${splToken}`);
  if (reference !== undefined && !BASE58_KEY.test(reference)) throw new PayUriError(`invalid reference: ${reference}`);
  const params = amount === null ? [`spl-token=${splToken}`] : [`amount=${uiAmount(amount, decimals)}`, `spl-token=${splToken}`];
  if (reference !== undefined) params.push(`reference=${reference}`);
  params.push(`label=${encodeURIComponent(label)}`, `message=${encodeURIComponent(message)}`);
  return `solana:${recipient}?${params.join('&')}`;
}

export interface QrRequest {
  payTo: Pick<PayTo, 'chain' | 'asset' | 'address' | 'reference'>;
  network: Network | undefined;
  asset: NetworkAsset | undefined;
  /** What is still due, the amount the page shows; null when the payer sends any amount. */
  due: string | null;
  label: string;
  message: string;
}

/**
 * The payment URI for the deposit QR, or null when the page must fall back to the plain address
 * (network list not loaded, unknown token, an amount not exact in the token's decimals, or a
 * Solana request without its reference).
 */
export function paymentUri(req: QrRequest, solanaEnabled = SOLANA_TRANSFER_URI_ENABLED): string | null {
  const { payTo, network, asset, due } = req;
  if (!network || !asset) return null;
  try {
    if (network.family === 'evm') {
      if (network.evm_chain_id === null) return null;
      return eip681({ chainId: network.evm_chain_id, token: asset.token, to: payTo.address ?? '', amount: due, decimals: asset.decimals });
    }
    if (!solanaEnabled || !payTo.reference) return null;
    return solanaPayTransfer({ recipient: payTo.address ?? '', amount: due, splToken: asset.token, reference: payTo.reference,
      label: req.label, message: req.message, decimals: asset.decimals });
  } catch (e) {
    if (e instanceof PayUriError) return null;
    throw e;
  }
}
