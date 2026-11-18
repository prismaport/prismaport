import { getAddress } from 'viem';
import type { Network, PayTo, Session, TypedDataResponse } from '../src/lib/api';

export const EVM_ADDRESS = getAddress('0x7a3f9c2d4e5b6a7c8d9e0f1a2b3c4d5e6f7ae91c');
export const SOL_ADDRESS = '9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin';
export const PAYER = '0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed';
export const GATEWAY = getAddress('0x4a7f3c2d4e5b6a7c8d9e0f1a2b3c4d5e6f7a0001');
export const BASE_USDC = '0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913';

export const NETWORKS: Network[] = [
  { id: 'base', family: 'evm', livemode: true, evm_chain_id: 8453, fee_bps: 50, fixed_fee: '0.10',
    assets: [{ symbol: 'USDC', token: BASE_USDC, decimals: 6, eip3009: true }] },
  { id: 'arbitrum', family: 'evm', livemode: true, evm_chain_id: 42161, fee_bps: 50, fixed_fee: '0.10',
    assets: [{ symbol: 'USDT', token: '0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9', decimals: 6, eip3009: false }] },
  { id: 'solana', family: 'solana', livemode: true, evm_chain_id: null, fee_bps: 50, fixed_fee: '0.10',
    assets: [{ symbol: 'USDC', token: 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', decimals: 6, eip3009: false },
      { symbol: 'USDT', token: 'Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB', decimals: 6, eip3009: false }] },
];

export const NOW = Date.parse('2026-10-23T18:00:00Z');

export function payTo(over: Partial<PayTo> = {}): PayTo {
  return { chain: 'base', asset: 'USDC', address: EVM_ADDRESS, amount: '25.000000', signed_payment: true, ...over };
}

export function session(over: Partial<Session> = {}): Session {
  return {
    id: 'ses_01JBTEST',
    livemode: true,
    status: 'open',
    amount: '25.00',
    currency: 'USD',
    amount_received: '0.00',
    amount_remaining: '25.00',
    product: { name: 'Founder' },
    pay_to: [payTo(), payTo({ chain: 'solana', address: SOL_ADDRESS, signed_payment: false }), payTo({ chain: 'solana', asset: 'USDT', address: SOL_ADDRESS, signed_payment: false })],
    payments: [],
    signed_payments: [],
    expires_at: '2026-10-23T19:00:00Z',
    success_url: null,
    merchant: null,
    ...over,
  };
}

export function typedData(over: { value?: string; amount?: string; from?: string; to?: string; chainId?: number; token?: string; validBefore?: string } = {}): TypedDataResponse {
  const value = over.value ?? '25000000';
  return {
    session_id: 'ses_01JBTEST',
    chain: 'base',
    asset: 'USDC',
    amount: over.amount ?? '25.000000',
    gateway: GATEWAY,
    typed_data: {
      types: {
        EIP712Domain: [{ name: 'name', type: 'string' }, { name: 'version', type: 'string' }, { name: 'chainId', type: 'uint256' }, { name: 'verifyingContract', type: 'address' }],
        ReceiveWithAuthorization: [{ name: 'from', type: 'address' }, { name: 'to', type: 'address' }, { name: 'value', type: 'uint256' },
          { name: 'validAfter', type: 'uint256' }, { name: 'validBefore', type: 'uint256' }, { name: 'nonce', type: 'bytes32' }],
      },
      primaryType: 'ReceiveWithAuthorization',
      domain: { name: 'USD Coin', version: '2', chainId: over.chainId ?? 8453, verifyingContract: over.token ?? BASE_USDC },
      message: { from: over.from ?? PAYER, to: over.to ?? GATEWAY, value, validAfter: '0', validBefore: over.validBefore ?? String(Date.parse('2026-10-23T19:00:00Z') / 1000),
        nonce: '0x' + 'ab'.repeat(32) },
    },
  };
}
