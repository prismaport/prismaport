import { describe, expect, it } from 'vitest';
import { PayUriError, SOLANA_TRANSFER_URI_ENABLED, eip681, paymentUri, solanaPayTransfer } from '../src/lib/payuri';
import { BASE_USDC, EVM_ADDRESS, NETWORKS, SOL_ADDRESS, payTo } from './fixtures';
import { decode } from './qrdecode';

const BASE_SEPOLIA_USDC = '0x036CbD53842c5426634e7929541eC2318f3dCF7e';
const SOL_USDC = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
const REFERENCE = '7Ufqy2TBBgoaFZ8HqnYMk6Cz9PeZqo5pPnkbGXZdaiLd';

const base = (amount: string, over: Partial<Parameters<typeof eip681>[0]> = {}) =>
  eip681({ chainId: 8453, token: BASE_USDC, to: EVM_ADDRESS, amount, decimals: 6, ...over });

describe('eip681', () => {
  it('builds an ERC-20 transfer request in base units', () => {
    expect(base('25')).toBe(`ethereum:${BASE_USDC}@8453/transfer?address=${EVM_ADDRESS}&uint256=25000000`);
    expect(base('25.00')).toBe(`ethereum:${BASE_USDC}@8453/transfer?address=${EVM_ADDRESS}&uint256=25000000`);
    expect(base('0.01')).toBe(`ethereum:${BASE_USDC}@8453/transfer?address=${EVM_ADDRESS}&uint256=10000`);
    expect(base('1234.567891')).toBe(`ethereum:${BASE_USDC}@8453/transfer?address=${EVM_ADDRESS}&uint256=1234567891`);
  });

  it('accepts the 18-place amounts the page computes when the extra places are zeros', () => {
    expect(base('15.000000000000000000')).toMatch(/&uint256=15000000$/);
  });

  it('rejects more significant decimals than the token has, never rounding', () => {
    expect(() => base('1.0000001')).toThrow(PayUriError);
    expect(() => base('1234.5678915')).toThrow(PayUriError);
  });

  it('never emits scientific notation or a decimal point in uint256', () => {
    const uri = base('1000000000000.000001');
    expect(uri).toMatch(/&uint256=1000000000000000001$/);
    expect(base('0.000001')).toMatch(/&uint256=1$/);
    for (const bad of ['1e6', '2.5E3', '-1', '.5', '5.', '', ' 1', '0', '0.000000', '1,5']) {
      expect(() => base(bad), bad).toThrow(PayUriError);
    }
  });

  it('targets the chain from the networks list: Base and Base Sepolia', () => {
    expect(base('25', { chainId: 84532, token: BASE_SEPOLIA_USDC }))
      .toBe(`ethereum:${BASE_SEPOLIA_USDC}@84532/transfer?address=${EVM_ADDRESS}&uint256=25000000`);
    expect(() => base('25', { chainId: 0 })).toThrow(PayUriError);
    expect(() => base('25', { chainId: 1.5 })).toThrow(PayUriError);
  });

  it('keeps checksummed addresses exactly as given and rejects malformed ones', () => {
    const uri = base('25');
    expect(EVM_ADDRESS).not.toBe(EVM_ADDRESS.toLowerCase());
    expect(uri).toContain(`address=${EVM_ADDRESS}&`);
    expect(uri).toContain(`ethereum:${BASE_USDC}@`);
    expect(() => base('25', { to: '0x123' })).toThrow(PayUriError);
    expect(() => base('25', { token: SOL_USDC })).toThrow(PayUriError);
  });
});

const sol = (amount: string, over: Partial<Parameters<typeof solanaPayTransfer>[0]> = {}) =>
  solanaPayTransfer({ recipient: SOL_ADDRESS, amount, splToken: SOL_USDC, label: 'Prismaport', message: 'Founder', ...over });

describe('solanaPayTransfer', () => {
  it('builds a transfer request with the parameters in the spec order', () => {
    expect(sol('25.000000', { reference: REFERENCE }))
      .toBe(`solana:${SOL_ADDRESS}?amount=25&spl-token=${SOL_USDC}&reference=${REFERENCE}&label=Prismaport&message=Founder`);
  });

  it('writes the amount in token units without trailing zeros or scientific notation', () => {
    expect(sol('25')).toContain('?amount=25&');
    expect(sol('25.00')).toContain('?amount=25&');
    expect(sol('0.010000')).toContain('?amount=0.01&');
    expect(sol('1234.567891')).toContain('?amount=1234.567891&');
    expect(sol('007.50')).toContain('?amount=7.5&');
    for (const bad of ['1e6', '-1', '.5', '', '0', '0.00']) expect(() => sol(bad), bad).toThrow(PayUriError);
  });

  it('rejects more decimals than the mint has when told the decimals', () => {
    expect(() => sol('1.0000001', { decimals: 6 })).toThrow(PayUriError);
    expect(sol('1.0000000', { decimals: 6 })).toContain('?amount=1&');
  });

  it('URL-encodes label and message', () => {
    const uri = sol('1', { label: 'Café & Co', message: 'Order #42: 2/3 done?' });
    expect(uri).toContain('&label=Caf%C3%A9%20%26%20Co&message=Order%20%2342%3A%202%2F3%20done%3F');
    const url = new URL(uri);
    expect(url.searchParams.get('label')).toBe('Café & Co');
    expect(url.searchParams.get('message')).toBe('Order #42: 2/3 done?');
  });

  it('omits the reference when there is none', () => {
    expect(sol('1')).toBe(`solana:${SOL_ADDRESS}?amount=1&spl-token=${SOL_USDC}&label=Prismaport&message=Founder`);
    expect(() => sol('1', { reference: 'not-base58!' })).toThrow(PayUriError);
  });
});

describe('paymentUri', () => {
  const req = (over: Partial<Parameters<typeof paymentUri>[0]> = {}) => {
    const p = over.payTo ?? payTo();
    const network = NETWORKS.find((n) => n.id === p.chain);
    return { payTo: p, network, asset: network?.assets.find((a) => a.symbol === p.asset), due: '25.000000000000000000', label: 'Prismaport', message: 'Founder', ...over };
  };

  it('EVM: an EIP-681 request for what is due', () => {
    expect(paymentUri(req())).toBe(`ethereum:${BASE_USDC}@8453/transfer?address=${EVM_ADDRESS}&uint256=25000000`);
    expect(paymentUri(req({ due: '15.00' }))).toMatch(/&uint256=15000000$/);
  });

  it('falls back to the address when the request cannot be exact', () => {
    expect(paymentUri(req({ network: undefined }))).toBeNull();
    expect(paymentUri(req({ asset: undefined }))).toBeNull();
    expect(paymentUri(req({ network: { ...NETWORKS[0]!, evm_chain_id: null } }))).toBeNull();
    expect(paymentUri(req({ due: '1.0000001' }))).toBeNull();
    expect(paymentUri(req({ due: '0.000000' }))).toBeNull();
  });

  it('Solana: off by default; when enabled it needs the reference', () => {
    expect(SOLANA_TRANSFER_URI_ENABLED).toBe(false);
    const solPayTo = payTo({ chain: 'solana', address: SOL_ADDRESS, signed_payment: false, reference: REFERENCE });
    expect(paymentUri(req({ payTo: solPayTo }))).toBeNull();
    expect(paymentUri(req({ payTo: solPayTo }), true))
      .toBe(`solana:${SOL_ADDRESS}?amount=25&spl-token=${SOL_USDC}&reference=${REFERENCE}&label=Prismaport&message=Founder`);
    expect(paymentUri(req({ payTo: { ...solPayTo, reference: undefined } }), true)).toBeNull();
  });
});

describe('payment URIs in a QR code', () => {
  it('decode back to the exact URI', () => {
    const evm = base('1234.567891');
    expect(decode(evm)).toBe(evm);
    const testnet = base('0.01', { chainId: 84532, token: BASE_SEPOLIA_USDC });
    expect(decode(testnet)).toBe(testnet);
    const solana = sol('25.000000', { reference: REFERENCE, label: 'Café & Co', message: 'Order #42' });
    expect(decode(solana)).toBe(solana);
  });
});

describe('amountless payment requests (open-amount sessions)', () => {
  it('EIP-681 leaves uint256 out, Solana Pay leaves amount out', () => {
    expect(eip681({ chainId: 8453, token: BASE_USDC, to: EVM_ADDRESS, amount: null, decimals: 6 }))
      .toBe(`ethereum:${BASE_USDC}@8453/transfer?address=${EVM_ADDRESS}`);
    expect(solanaPayTransfer({ recipient: SOL_ADDRESS, amount: null, splToken: SOL_USDC, label: 'Prismaport', message: 'Top-up' }))
      .toBe(`solana:${SOL_ADDRESS}?spl-token=${SOL_USDC}&label=Prismaport&message=Top-up`);
  });

  it('paymentUri with no due builds the amountless EVM request', () => {
    expect(paymentUri({ payTo: payTo(), network: NETWORKS[0], asset: NETWORKS[0]!.assets[0], due: null, label: 'Prismaport', message: 'x' }))
      .toBe(`ethereum:${BASE_USDC}@8453/transfer?address=${EVM_ADDRESS}`);
  });
});
