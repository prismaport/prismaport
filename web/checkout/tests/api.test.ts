import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, CheckoutApi, REQUEST_TIMEOUT_MS, SUBMIT_TIMEOUT_MS, type SubmitRequest } from '../src/lib/api';
import { session } from './fixtures';

const submitReq: SubmitRequest = { chain: 'base', asset: 'USDC', from: '0x1', value: '1', valid_after: '0', valid_before: '1', signature: '0x2' };

afterEach(() => {
  vi.restoreAllMocks();
});

describe('API client', () => {
  it('gives every request a timeout, and a submit a longer one', async () => {
    const timeout = vi.spyOn(AbortSignal, 'timeout');
    const fetchFn = vi.fn(async () => new Response(JSON.stringify(session()), { status: 200 }));
    const api = new CheckoutApi('', fetchFn as never);
    await api.session('ses_1');
    await api.submit('ses_1', submitReq);
    expect(REQUEST_TIMEOUT_MS).toBe(10_000);
    expect(timeout.mock.calls.map((c) => c[0])).toEqual([REQUEST_TIMEOUT_MS, SUBMIT_TIMEOUT_MS]);
    expect((fetchFn.mock.calls[0] as unknown as [string, RequestInit])[1].signal).toBeInstanceOf(AbortSignal);
  });

  it('a request that hangs past its timeout fails as a network error, so the next poll can run', async () => {
    const ctl = new AbortController();
    vi.spyOn(AbortSignal, 'timeout').mockReturnValue(ctl.signal);
    const fetchFn = vi.fn((_url: string, init: RequestInit) => new Promise<Response>((_, reject) => {
      init.signal!.addEventListener('abort', () => reject(new DOMException('The operation timed out.', 'TimeoutError')));
    }));
    const pending = new CheckoutApi('', fetchFn as never).session('ses_1');
    ctl.abort();
    await expect(pending).rejects.toMatchObject({ status: 0, code: 'network_error' });
  });

  it('a body cut off mid-stream is a network error, not a wallet error', async () => {
    const res = { ok: true, status: 202, headers: new Headers(), text: () => Promise.reject(new TypeError('network error')) };
    const err = await new CheckoutApi('', vi.fn(async () => res) as never).submit('ses_1', submitReq).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ code: 'network_error' });
  });
});
