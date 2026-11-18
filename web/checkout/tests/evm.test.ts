import { describe, expect, it, vi } from 'vitest';
import { decodeFunctionData, erc20Abi, type EIP1193Provider } from 'viem';
import { ApiError, CheckoutApi, type TypedDataResponse } from '../src/lib/api';
import {
  EvmWallet,
  WalletError,
  checkTypedData,
  discoverWallets,
  isTooSmallToSettle,
  settles,
  paySigned,
  payTransfer,
  walletBalance,
  watchWallets,
  type PaymentTarget,
} from '../src/wallet/evm';
import { BASE_USDC, EVM_ADDRESS, GATEWAY, NETWORKS, PAYER, payTo, typedData } from './fixtures';

const SIG = '0x' + '11'.repeat(32) + '22'.repeat(32) + '1b';

interface Call {
  method: string;
  params?: unknown;
}

function mockProvider(opts: { chainId?: number; sign?: () => unknown; accounts?: string[]; switchFails?: boolean } = {}) {
  let chainId = opts.chainId ?? 8453;
  const calls: Call[] = [];
  const provider = {
    request: vi.fn(async ({ method, params }: { method: string; params?: unknown }) => {
      calls.push({ method, params });
      switch (method) {
        case 'eth_requestAccounts':
        case 'eth_accounts':
          return opts.accounts ?? [PAYER.toLowerCase()];
        case 'eth_chainId':
          return '0x' + chainId.toString(16);
        case 'wallet_switchEthereumChain': {
          if (opts.switchFails) throw Object.assign(new Error('rejected'), { code: 4001 });
          chainId = parseInt((params as [{ chainId: string }])[0].chainId, 16);
          return null;
        }
        case 'eth_signTypedData_v4':
          return opts.sign ? opts.sign() : SIG;
        case 'eth_sendTransaction':
          return '0x' + 'cd'.repeat(32);
        case 'eth_estimateGas':
          return '0x5208';
        default:
          throw new Error(`unexpected ${method}`);
      }
    }),
  } as unknown as EIP1193Provider;
  return { provider, calls };
}

function mockApi(td: TypedDataResponse | Error = typedData()) {
  const api = new CheckoutApi('', vi.fn() as never);
  const typed = vi.spyOn(api, 'typedData').mockImplementation(async () => {
    if (td instanceof Error) throw td;
    return { data: td, serverTime: null };
  });
  const submit = vi.spyOn(api, 'submit').mockResolvedValue({ data: { id: 'txj_01', session_id: 'ses_01JBTEST', status: 'pending' }, serverTime: null });
  return { api, typed, submit };
}

const target = (over: Partial<PaymentTarget> = {}): PaymentTarget => ({
  sessionId: 'ses_01JBTEST',
  payTo: payTo(),
  network: NETWORKS[0]!,
  due: '25.000000',
  expiresAt: '2026-10-23T19:00:00Z',
  ...over,
});

describe('signed USDC payment (EIP-3009)', () => {
  it('fetches typed data, signs it as served with eth_signTypedData_v4, submits the signature', async () => {
    const { provider, calls } = mockProvider();
    const { api, typed, submit } = mockApi();
    const wallet = new EvmWallet(provider);
    const out = await paySigned(api, wallet, target());
    expect(out.jobId).toBe('txj_01');
    expect(typed).toHaveBeenCalledWith('ses_01JBTEST', { chain: 'base', asset: 'USDC', from: PAYER });
    const sign = calls.find((c) => c.method === 'eth_signTypedData_v4')!;
    const [from, json] = sign.params as [string, string];
    expect(from).toBe(PAYER);
    expect(JSON.parse(json)).toEqual(typedData().typed_data);
    expect(submit).toHaveBeenCalledWith('ses_01JBTEST', {
      chain: 'base', asset: 'USDC', from: PAYER, value: '25000000', valid_after: '0',
      valid_before: String(Date.parse('2026-10-23T19:00:00Z') / 1000), signature: SIG,
    });
  });

  it('switches the wallet to the session network first', async () => {
    const { provider, calls } = mockProvider({ chainId: 1 });
    const { api } = mockApi();
    await paySigned(api, new EvmWallet(provider), target());
    const methods = calls.map((c) => c.method);
    expect(methods.indexOf('wallet_switchEthereumChain')).toBeLessThan(methods.indexOf('eth_signTypedData_v4'));
    expect((calls.find((c) => c.method === 'wallet_switchEthereumChain')!.params as [{ chainId: string }])[0].chainId).toBe('0x2105');
  });

  it('stops when the payer refuses to switch networks', async () => {
    const { provider, calls } = mockProvider({ chainId: 1, switchFails: true });
    const { api, typed } = mockApi();
    await expect(paySigned(api, new EvmWallet(provider), target())).rejects.toMatchObject({ kind: 'rejected' });
    expect(typed).not.toHaveBeenCalled();
    expect(calls.some((c) => c.method === 'eth_signTypedData_v4')).toBe(false);
  });

  it('never asks for a signature for an amount too small to settle after the fee', async () => {
    for (const due of ['0.100502', '0.10', '0.05', '0.000001']) {
      const { provider, calls } = mockProvider();
      const { api, typed } = mockApi();
      expect(isTooSmallToSettle(target({ due }))).toBe(true);
      await expect(paySigned(api, new EvmWallet(provider), target({ due }))).rejects.toMatchObject({ kind: 'below_minimum' });
      expect(typed).not.toHaveBeenCalled();
      expect(calls).toHaveLength(0);
    }
    expect(isTooSmallToSettle(target({ due: '0.100503' }))).toBe(false);
  });

  it('settles only when the fixed fee plus the percentage is less than the amount', () => {
    const pilot = { fee_bps: 50, fixed_fee: '0.10' };
    expect(settles(25_000_000n, pilot, 6)).toBe(true);
    expect(settles(100_503n, pilot, 6)).toBe(true);
    expect(settles(100_502n, pilot, 6)).toBe(false);
    expect(settles(100_000n, pilot, 6)).toBe(false);
    expect(settles(0n, { fee_bps: 0, fixed_fee: '0' }, 6)).toBe(false);
    expect(settles(1n, { fee_bps: 50, fixed_fee: '0' }, 6)).toBe(true);
    expect(settles(100_001n, { fee_bps: 0, fixed_fee: '0.10' }, 6)).toBe(true);
    expect(settles(100_502_512_562_814_071n, pilot, 18)).toBe(true);
    expect(settles(100_502_512_562_814_070n, pilot, 18)).toBe(false);
  });

  it('refuses typed data whose value is too small to settle even if the server sent it', async () => {
    const { provider, calls } = mockProvider();
    const { api, submit } = mockApi(typedData({ value: '100000', amount: '0.100000' }));
    await expect(paySigned(api, new EvmWallet(provider), target())).rejects.toMatchObject({ kind: 'below_minimum' });
    expect(calls.some((c) => c.method === 'eth_signTypedData_v4')).toBe(false);
    expect(submit).not.toHaveBeenCalled();
  });

  it('refuses typed data that does not match the session before anything is signed', async () => {
    const bad: [string, TypedDataResponse][] = [
      ['more than due', typedData({ value: '26000000', amount: '26.000000' })],
      ['another payer', typedData({ from: EVM_ADDRESS })],
      ['recipient is not the gateway', typedData({ to: EVM_ADDRESS })],
      ['another chain', typedData({ chainId: 1 })],
      ['another token', typedData({ token: EVM_ADDRESS })],
      ['valid after expiry', typedData({ validBefore: String(Date.parse('2026-10-23T19:00:00Z') / 1000 + 1) })],
      ['value differs from amount', typedData({ value: '25000000', amount: '24.000000' })],
      ['fractional value', typedData({ value: '25000000.5' })],
    ];
    for (const [name, td] of bad) {
      const { provider, calls } = mockProvider();
      const { api, submit } = mockApi(td);
      await expect(paySigned(api, new EvmWallet(provider), target()), name).rejects.toBeInstanceOf(WalletError);
      expect(calls.some((c) => c.method === 'eth_signTypedData_v4'), name).toBe(false);
      expect(submit, name).not.toHaveBeenCalled();
    }
  });

  it('accepts a smaller value when a payment arrived meanwhile', () => {
    expect(() => checkTypedData(typedData({ value: '15000000', amount: '15.000000' }), target(), PAYER)).not.toThrow();
  });

  it('reports a declined signature and submits nothing', async () => {
    const { provider } = mockProvider({ sign: () => { throw Object.assign(new Error('User rejected'), { code: 4001 }); } });
    const { api, submit } = mockApi();
    await expect(paySigned(api, new EvmWallet(provider), target())).rejects.toMatchObject({ kind: 'rejected' });
    expect(submit).not.toHaveBeenCalled();
  });

  it('rejects a malformed signature', async () => {
    const { provider } = mockProvider({ sign: () => '0x1234' });
    const { api, submit } = mockApi();
    await expect(paySigned(api, new EvmWallet(provider), target())).rejects.toMatchObject({ kind: 'bad_signature' });
    expect(submit).not.toHaveBeenCalled();
  });

  it('passes API errors through for the page to explain', async () => {
    const { provider } = mockProvider();
    const { api } = mockApi(new ApiError(409, 'session_not_payable', 'not payable'));
    await expect(paySigned(api, new EvmWallet(provider), target())).rejects.toMatchObject({ code: 'session_not_payable' });
  });

  it('cannot pay an amount the token cannot represent', async () => {
    const { provider } = mockProvider();
    const { api, typed } = mockApi();
    await expect(paySigned(api, new EvmWallet(provider), target({ due: '0.1000001' }))).rejects.toMatchObject({ kind: 'not_exact' });
    expect(typed).not.toHaveBeenCalled();
  });
});

describe('plain transfer (USDT on EVM)', () => {
  const usdt = () => target({ payTo: payTo({ chain: 'arbitrum', asset: 'USDT', signed_payment: false }), network: NETWORKS[1]!, due: '15.000000' });

  it('sends exactly what is due to the session address', async () => {
    const { provider, calls } = mockProvider({ chainId: 42161 });
    const out = await payTransfer(new EvmWallet(provider), usdt());
    expect(out.txHash).toBe('0x' + 'cd'.repeat(32));
    const tx = (calls.find((c) => c.method === 'eth_sendTransaction')!.params as [{ to: string; data: `0x${string}`; from: string }])[0];
    expect(tx.to.toLowerCase()).toBe('0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9');
    expect(tx.from.toLowerCase()).toBe(PAYER.toLowerCase());
    const decoded = decodeFunctionData({ abi: erc20Abi, data: tx.data });
    expect(decoded.functionName).toBe('transfer');
    expect(decoded.args).toEqual([EVM_ADDRESS, 15_000_000n]);
  });

  it('switches network before sending', async () => {
    const { provider, calls } = mockProvider({ chainId: 8453 });
    await payTransfer(new EvmWallet(provider), usdt());
    const methods = calls.map((c) => c.method);
    expect(methods.indexOf('wallet_switchEthereumChain')).toBeGreaterThan(-1);
    expect(methods.indexOf('wallet_switchEthereumChain')).toBeLessThan(methods.indexOf('eth_sendTransaction'));
  });

  it('reports a declined transaction', async () => {
    const { provider } = mockProvider({ chainId: 42161 });
    (provider.request as ReturnType<typeof vi.fn>).mockImplementation(async ({ method }: { method: string }) => {
      if (method === 'eth_sendTransaction') throw Object.assign(new Error('denied'), { code: 4001 });
      if (method === 'eth_chainId') return '0xa4b1';
      return [PAYER];
    });
    await expect(payTransfer(new EvmWallet(provider), usdt())).rejects.toMatchObject({ kind: 'rejected' });
  });
});

describe('wallet discovery', () => {
  it('collects EIP-6963 announcements', async () => {
    const { provider } = mockProvider();
    const onRequest = () => {
      window.dispatchEvent(new CustomEvent('eip6963:announceProvider', { detail: { info: { uuid: 'a', name: 'Wallet A', icon: '', rdns: 'a' }, provider } }));
      window.dispatchEvent(new CustomEvent('eip6963:announceProvider', { detail: { info: { uuid: 'b', name: 'Wallet B', icon: '', rdns: 'b' }, provider } }));
      window.dispatchEvent(new CustomEvent('eip6963:announceProvider', { detail: { info: { uuid: 'a', name: 'Wallet A', icon: '', rdns: 'a' }, provider } }));
    };
    window.addEventListener('eip6963:requestProvider', onRequest);
    const found = await discoverWallets(window, 10);
    window.removeEventListener('eip6963:requestProvider', onRequest);
    expect(found.map((w) => w.info.name)).toEqual(['Wallet A', 'Wallet B']);
  });

  it('falls back to window.ethereum', async () => {
    const { provider } = mockProvider();
    (window as unknown as { ethereum?: unknown }).ethereum = provider;
    const found = await discoverWallets(window, 10);
    delete (window as unknown as { ethereum?: unknown }).ethereum;
    expect(found).toHaveLength(1);
    expect(found[0]!.provider).toBe(provider);
  });

  it('finds nothing without a wallet', async () => {
    expect(await discoverWallets(window, 10)).toEqual([]);
  });
});

describe('watchWallets', () => {
  const announce = (uuid: string, provider: unknown) =>
    window.dispatchEvent(new CustomEvent('eip6963:announceProvider', { detail: { info: { uuid, name: uuid, icon: '', rdns: uuid }, provider } }));

  it('keeps listening after the first look and reports each new wallet once', () => {
    const { provider } = mockProvider();
    const seen: string[][] = [];
    const watch = watchWallets(window, (list) => seen.push(list.map((w) => w.info.uuid)));
    expect(seen).toEqual([]);
    announce('late', provider);
    announce('late', provider);
    announce('later', provider);
    watch.stop();
    announce('after-stop', provider);
    expect(seen).toEqual([['late'], ['late', 'later']]);
  });

  it('rescan asks wallets again and falls back to window.ethereum; an announcement replaces the fallback', () => {
    const { provider } = mockProvider();
    const requests = vi.fn();
    window.addEventListener('eip6963:requestProvider', requests);
    const seen: string[][] = [];
    const watch = watchWallets(window, (list) => seen.push(list.map((w) => w.info.uuid)));
    (window as unknown as { ethereum?: unknown }).ethereum = provider;
    watch.rescan();
    delete (window as unknown as { ethereum?: unknown }).ethereum;
    announce('real', provider);
    watch.stop();
    window.removeEventListener('eip6963:requestProvider', requests);
    expect(requests).toHaveBeenCalledTimes(2);
    expect(seen).toEqual([['injected'], ['real']]);
  });
});

describe('EvmWallet.watch', () => {
  it('follows account switches and disconnects, and unsubscribes', async () => {
    const listeners: Record<string, ((x: never) => void)[]> = {};
    const { provider } = mockProvider();
    Object.assign(provider, {
      on: (ev: string, fn: (x: never) => void) => { (listeners[ev] ??= []).push(fn); },
      removeListener: (ev: string, fn: (x: never) => void) => { listeners[ev] = (listeners[ev] ?? []).filter((f) => f !== fn); },
    });
    const w = new EvmWallet(provider);
    await w.connect();
    const changed = vi.fn();
    const stop = w.watch(changed);
    const other = '0x468f86b3311786e7fd02ae754ae0a4a79a33d05e';
    listeners.accountsChanged![0]!([other] as never);
    expect(w.account).toBe('0x468F86B3311786E7FD02ae754aE0a4A79A33d05E');
    listeners.chainChanged![0]!('0x1' as never);
    listeners.accountsChanged![0]!([] as never);
    expect(w.account).toBeNull();
    expect(changed).toHaveBeenCalledTimes(3);
    stop();
    expect(listeners.accountsChanged).toHaveLength(0);
    expect(listeners.chainChanged).toHaveLength(0);
  });

  it('is a no-op for a provider without events', async () => {
    const { provider } = mockProvider();
    const w = new EvmWallet(provider);
    expect(() => w.watch(() => {})()).not.toThrow();
  });
});

describe('walletBalance', () => {
  it('reads the token balance through the wallet when it is on the right chain', async () => {
    const { provider, calls } = mockProvider();
    (provider.request as ReturnType<typeof vi.fn>).mockImplementation(async ({ method, params }: { method: string; params?: unknown }) => {
      calls.push({ method, params });
      if (method === 'eth_chainId') return '0x2105';
      if (method === 'eth_call') return '0x' + (12_345_678n).toString(16).padStart(64, '0');
      return [PAYER.toLowerCase()];
    });
    const w = new EvmWallet(provider);
    await w.connect();
    expect(await walletBalance(w, target())).toEqual({ amount: '12.345678', units: 12_345_678n });
    const call = calls.find((c) => c.method === 'eth_call')!.params as [{ to: string; data: `0x${string}` }, string];
    expect(call[0].to.toLowerCase()).toBe(BASE_USDC.toLowerCase());
    expect(decodeFunctionData({ abi: erc20Abi, data: call[0].data })).toMatchObject({ functionName: 'balanceOf', args: [PAYER] });
  });

  it('is null on another chain, without asking the wallet to switch', async () => {
    const { provider, calls } = mockProvider({ chainId: 1 });
    const w = new EvmWallet(provider);
    await w.connect();
    expect(await walletBalance(w, target())).toBeNull();
    expect(calls.map((c) => c.method)).not.toContain('wallet_switchEthereumChain');
  });

  it('is null when not connected or when the wallet fails', async () => {
    const { provider } = mockProvider();
    const w = new EvmWallet(provider);
    expect(await walletBalance(w, target())).toBeNull();
    await w.connect();
    expect(await walletBalance(w, target())).toBeNull();
  });
});

it('the fixture token is the Base USDC contract', () => {
  expect(NETWORKS[0]!.assets[0]!.token).toBe(BASE_USDC);
  expect(GATEWAY).toMatch(/^0x[0-9a-fA-F]{40}$/);
});

describe('cancelled payments on an older browser', () => {
  /** An aborted signal without AbortSignal.throwIfAborted, as on Chrome < 100, Safari < 15.4 or Firefox < 97. */
  const oldAborted = () => ({ aborted: true }) as AbortSignal;
  const usdt = () => target({ payTo: payTo({ chain: 'arbitrum', asset: 'USDT', signed_payment: false }), network: NETWORKS[1]!, due: '15.000000' });

  it('a signed payment stops with an AbortError, not a TypeError, and nothing is signed or submitted', async () => {
    const { provider, calls } = mockProvider();
    const { api, submit } = mockApi();
    await expect(paySigned(api, new EvmWallet(provider), target(), oldAborted())).rejects.toMatchObject({ name: 'AbortError' });
    expect(calls.some((c) => c.method === 'eth_signTypedData_v4')).toBe(false);
    expect(submit).not.toHaveBeenCalled();
  });

  it('a transfer stops with an AbortError before the wallet is asked to send', async () => {
    const { provider, calls } = mockProvider({ chainId: 42161 });
    await expect(payTransfer(new EvmWallet(provider), usdt(), oldAborted())).rejects.toMatchObject({ name: 'AbortError' });
    expect(calls.some((c) => c.method === 'eth_sendTransaction')).toBe(false);
  });
});

describe('signed payment: the point of no return', () => {
  it('onSigned runs once the wallet signed, right before the submit', async () => {
    const { provider, calls } = mockProvider();
    const { api, submit } = mockApi();
    const order: string[] = [];
    submit.mockImplementation(async () => {
      order.push('submit');
      return { data: { id: 'txj_01', session_id: 'ses_01JBTEST', status: 'pending' }, serverTime: null };
    });
    await paySigned(api, new EvmWallet(provider), target(), undefined, () => {
      order.push(calls.some((c) => c.method === 'eth_signTypedData_v4') ? 'signed' : 'not signed');
    });
    expect(order).toEqual(['signed', 'submit']);
  });

  it('a submit answer without a job id is an error, not a payment with no job', async () => {
    const { provider } = mockProvider();
    const { api, submit } = mockApi();
    submit.mockResolvedValue({ data: {} as never, serverTime: null });
    const onSigned = vi.fn();
    const err = await paySigned(api, new EvmWallet(provider), target(), undefined, onSigned).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(Error);
    expect(err).not.toBeInstanceOf(ApiError);
    expect(onSigned).toHaveBeenCalledTimes(1);
  });

  it('a declined signature never calls onSigned', async () => {
    const { provider } = mockProvider({ sign: () => { throw Object.assign(new Error('denied'), { code: 4001 }); } });
    const { api } = mockApi();
    const onSigned = vi.fn();
    await expect(paySigned(api, new EvmWallet(provider), target(), undefined, onSigned)).rejects.toMatchObject({ kind: 'rejected' });
    expect(onSigned).not.toHaveBeenCalled();
  });
});
