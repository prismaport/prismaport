import { describe, expect, it, vi } from 'vitest';
import type { Wallet } from '@wallet-standard/base';
import {
  SIGN_AND_SEND,
  SolanaUnavailableError,
  UnavailableSolanaApi,
  backendSolanaApi,
  base58,
  connectSolanaWallet,
  isWalletRejection,
  payWithSolanaWallet,
  solanaWalletSupported,
  standardChain,
  usableWallets,
} from '../src/wallet/solana';
import { CheckoutApi } from '../src/lib/api';
import { SOL_ADDRESS, payTo } from './fixtures';

function wallet(name: string, opts: { chains?: string[]; features?: string[] } = {}) {
  const account = { address: SOL_ADDRESS, publicKey: new Uint8Array(32), chains: ['solana:mainnet'], features: [SIGN_AND_SEND] };
  const signAndSendTransaction = vi.fn(async () => [{ signature: new Uint8Array(64).fill(1) }]);
  const connect = vi.fn(async () => ({ accounts: [account] }));
  const all: Record<string, unknown> = {
    'standard:connect': { version: '1.0.0', connect },
    [SIGN_AND_SEND]: { version: '1.0.0', supportedTransactionVersions: ['legacy', 0], signAndSendTransaction },
  };
  const features = Object.fromEntries((opts.features ?? Object.keys(all)).map((f) => [f, all[f]]));
  const w = { version: '1.0.0', name, icon: 'data:image/svg+xml;base64,AA==', chains: opts.chains ?? ['solana:mainnet'], features, accounts: [] } as unknown as Wallet;
  return { w, signAndSendTransaction, connect };
}

describe('Solana adapter', () => {
  it('maps chains to the Wallet Standard', () => {
    expect(standardChain('solana')).toBe('solana:mainnet');
    expect(standardChain('solana-devnet')).toBe('solana:devnet');
    expect(standardChain('base')).toBeNull();
  });

  it('is only offered once the backend advertises it for the option', () => {
    expect(solanaWalletSupported(payTo({ chain: 'solana' }))).toBe(false);
    expect(solanaWalletSupported(payTo({ chain: 'solana', connected_wallet: true }))).toBe(true);
    expect(solanaWalletSupported(payTo({ chain: 'base', connected_wallet: true }))).toBe(false);
  });

  it('keeps wallets that can connect and sign-and-send on the chain', () => {
    const ok = wallet('Phantom').w;
    const devnetOnly = wallet('Dev', { chains: ['solana:devnet'] }).w;
    const noSend = wallet('Viewer', { features: ['standard:connect'] }).w;
    expect(usableWallets([ok, devnetOnly, noSend], 'solana').map((w) => w.name)).toEqual(['Phantom']);
    expect(usableWallets([ok, devnetOnly], 'solana-devnet').map((w) => w.name)).toEqual(['Dev']);
    expect(usableWallets([ok], 'base')).toEqual([]);
  });

  it('signs and sends the server-built transaction', async () => {
    const { w, signAndSendTransaction } = wallet('Phantom');
    const tx = new Uint8Array([1, 2, 3, 250]);
    const api = { buildTransaction: vi.fn(async () => ({ transaction: btoa(String.fromCharCode(...tx)) })) };
    const out = await payWithSolanaWallet(w, api, 'ses_01JBTEST', payTo({ chain: 'solana', asset: 'USDT', address: SOL_ADDRESS }));
    expect(api.buildTransaction).toHaveBeenCalledWith('ses_01JBTEST', { chain: 'solana', asset: 'USDT', account: SOL_ADDRESS });
    const input = (signAndSendTransaction.mock.calls[0] as unknown as [{ transaction: Uint8Array; chain: string }])[0];
    expect([...input.transaction]).toEqual([...tx]);
    expect(input.chain).toBe('solana:mainnet');
    expect(out.signature).toBe(base58(new Uint8Array(64).fill(1)));
    expect(out.account).toBe(SOL_ADDRESS);
  });

  it('pays from an account the page already connected, without asking the wallet to connect again', async () => {
    const { w, connect, signAndSendTransaction } = wallet('Phantom');
    const other = 'So11111111111111111111111111111111111111112';
    const connected = { address: other, publicKey: new Uint8Array(32), chains: ['solana:mainnet'], features: [SIGN_AND_SEND] } as never;
    const api = { buildTransaction: vi.fn(async () => ({ transaction: btoa('tx') })) };
    const out = await payWithSolanaWallet(w, api, 'ses_01JBTEST', payTo({ chain: 'solana', address: SOL_ADDRESS }), connected);
    expect(connect).not.toHaveBeenCalled();
    expect(api.buildTransaction).toHaveBeenCalledWith('ses_01JBTEST', { chain: 'solana', asset: 'USDC', account: other });
    expect((signAndSendTransaction.mock.calls[0] as unknown as [{ account: unknown }])[0].account).toBe(connected);
    expect(out.account).toBe(other);
  });

  it('connects and returns the account shared for the chain, or says why it cannot', async () => {
    const { w } = wallet('Phantom');
    const devnet = { address: 'Dev1111111111111111111111111111111111111111', publicKey: new Uint8Array(32), chains: ['solana:devnet'], features: [SIGN_AND_SEND] };
    const mainnet = { address: SOL_ADDRESS, publicKey: new Uint8Array(32), chains: ['solana:mainnet'], features: [SIGN_AND_SEND] };
    (w.features['standard:connect'] as { connect: unknown }).connect = vi.fn(async () => ({ accounts: [mainnet, devnet] }));
    expect((await connectSolanaWallet(w, 'solana-devnet')).address).toBe(devnet.address);
    expect((await connectSolanaWallet(w, 'solana')).address).toBe(SOL_ADDRESS);

    (w.features['standard:connect'] as { connect: unknown }).connect = vi.fn(async () => ({ accounts: [] }));
    await expect(connectSolanaWallet(w, 'solana')).rejects.toThrow('The wallet did not share an account.');
    await expect(connectSolanaWallet(w, 'base')).rejects.toBeInstanceOf(SolanaUnavailableError);
    await expect(connectSolanaWallet(wallet('NoSend', { features: ['standard:connect'] }).w, 'solana')).rejects.toBeInstanceOf(SolanaUnavailableError);
    await expect(connectSolanaWallet(wallet('NoConnect', { features: [SIGN_AND_SEND] }).w, 'solana')).rejects.toBeInstanceOf(SolanaUnavailableError);
  });

  it('fails clearly until the backend endpoint exists', async () => {
    const { w, signAndSendTransaction } = wallet('Phantom');
    await expect(payWithSolanaWallet(w, UnavailableSolanaApi, 'ses_1', payTo({ chain: 'solana' }))).rejects.toBeInstanceOf(SolanaUnavailableError);
    expect(signAndSendTransaction).not.toHaveBeenCalled();
  });

  it('encodes base58 like Solana', () => {
    expect(base58(new Uint8Array([0, 0, 1]))).toBe('112');
    expect(base58(new Uint8Array(32))).toBe('11111111111111111111111111111111');
    expect(base58(new TextEncoder().encode('hello world'))).toBe('StV1DL6CwTryKyV');
  });
});

describe('backend Solana transaction API', () => {
  it('posts chain, asset and account to the session endpoint and returns the transaction', async () => {
    const fetchFn = vi.fn(async () => new Response(JSON.stringify({ session_id: 'ses_01JBTEST', chain: 'solana', asset: 'USDC', amount: '25.000000',
      fee_payer: SOL_ADDRESS, reference: SOL_ADDRESS, transaction: 'AQID', last_valid_block_height: 10 }), { status: 200 }));
    const api = backendSolanaApi(new CheckoutApi('https://api.test', fetchFn as never));
    await expect(api.buildTransaction('ses_01JBTEST', { chain: 'solana', asset: 'USDC', account: SOL_ADDRESS })).resolves.toEqual({ transaction: 'AQID' });
    const [url, init] = fetchFn.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe('https://api.test/v1/checkout/sessions/ses_01JBTEST/solana/transaction');
    expect(init.method).toBe('POST');
    expect(JSON.parse(init.body as string)).toEqual({ chain: 'solana', asset: 'USDC', account: SOL_ADDRESS });
  });

  it('surfaces backend errors', async () => {
    const fetchFn = vi.fn(async () => new Response(JSON.stringify({ error: { code: 'transaction_rejected', message: 'insufficient funds' } }), { status: 400 }));
    const api = backendSolanaApi(new CheckoutApi('', fetchFn as never));
    await expect(api.buildTransaction('ses_01JBTEST', { chain: 'solana', asset: 'USDC', account: SOL_ADDRESS })).rejects.toMatchObject({ code: 'transaction_rejected' });
  });
});

describe('wallet rejection', () => {
  it('recognises a declined request by code or by the Wallet Standard message, also when wrapped', () => {
    expect(isWalletRejection(Object.assign(new Error('nope'), { code: 4001 }))).toBe(true);
    expect(isWalletRejection(new Error('User rejected the request.'))).toBe(true);
    expect(isWalletRejection(new Error('outer', { cause: new Error('Request rejected') }))).toBe(true);
  });

  it('does not treat other failures as a decline', () => {
    expect(isWalletRejection(new Error('Request timed out'))).toBe(false);
    expect(isWalletRejection(Object.assign(new Error('internal'), { code: -32603 }))).toBe(false);
    expect(isWalletRejection(undefined)).toBe(false);
  });
});

describe('a cancelled Solana payment on an older browser', () => {
  it('stops with an AbortError, not a TypeError, before the wallet is asked to send', async () => {
    const { w, signAndSendTransaction } = wallet('Phantom');
    const api = { buildTransaction: vi.fn(async () => ({ transaction: btoa('tx') })) };
    // No AbortSignal.throwIfAborted, as on Chrome < 100, Safari < 15.4 or Firefox < 97.
    const signal = { aborted: true } as AbortSignal;
    await expect(payWithSolanaWallet(w, api, 'ses_01JBTEST', payTo({ chain: 'solana', address: SOL_ADDRESS }), undefined, signal))
      .rejects.toMatchObject({ name: 'AbortError' });
    expect(signAndSendTransaction).not.toHaveBeenCalled();
  });
});

describe('the ask to send', () => {
  it('onAsk runs just before the wallet is asked to send, and never once the payment was cancelled', async () => {
    const { w, signAndSendTransaction } = wallet('Phantom');
    const api = { buildTransaction: vi.fn(async () => ({ transaction: btoa('tx') })) };
    const asked = vi.fn(() => expect(signAndSendTransaction).not.toHaveBeenCalled());
    await payWithSolanaWallet(w, api, 'ses_01JBTEST', payTo({ chain: 'solana', address: SOL_ADDRESS }), undefined, undefined, asked);
    expect(asked).toHaveBeenCalledTimes(1);
    expect(signAndSendTransaction).toHaveBeenCalledTimes(1);

    const ctl = new AbortController();
    const late = vi.fn();
    const build = vi.fn(async () => {
      ctl.abort();
      return { transaction: btoa('tx') };
    });
    await expect(payWithSolanaWallet(w, { buildTransaction: build }, 'ses_01JBTEST', payTo({ chain: 'solana', address: SOL_ADDRESS }), undefined, ctl.signal, late))
      .rejects.toMatchObject({ name: 'AbortError' });
    expect(late).not.toHaveBeenCalled();
    expect(signAndSendTransaction).toHaveBeenCalledTimes(1);
  });
});
