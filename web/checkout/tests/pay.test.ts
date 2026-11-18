import { afterEach, describe, expect, it, vi } from 'vitest';
import type { EIP1193Provider } from 'viem';
import { ApiError, CheckoutApi, type Session } from '../src/lib/api';
import { COPY, SENT_GRACE_MS } from '../src/lib/copy';
import { formatDateTime } from '../src/lib/format';
import { SOLANA_EXCHANGE_NOTE } from '../src/lib/networks';
import { PROGRESS_COPY, PROMPT_TIMEOUT_MS, SEND_AGAIN_AFTER_MS, SOLANA_COPY, SOLANA_REQUEST_LIFETIME_MS, SUBMIT_CHECK_MS, WALLET_COPY, mountPay, safeSuccessUrl } from '../src/pages/pay';
import { WALLET_ERROR_COPY, type DiscoveredWallet } from '../src/wallet/evm';
import { SIGN_AND_SEND, base58, listSolanaWallets, type SolanaTransactionApi } from '../src/wallet/solana';
import type { Wallet } from '@wallet-standard/base';
import { BASE_USDC, EVM_ADDRESS, NETWORKS, NOW, PAYER, SOL_ADDRESS, payTo, session, typedData } from './fixtures';
import { decodeSvg } from './qrdecode';

const TX = '0x' + 'cd'.repeat(32);

/** The progress steps as a screen reader hears them. */
const steps = (root: HTMLElement) => [...root.querySelectorAll('.step-title')].map((s) => s.textContent);

const poll = async () => {
  document.dispatchEvent(new Event('visibilitychange'));
  await flush();
};

/** Lets pending promises and zero-delay timers run; with fake timers it advances them by 0 ms. */
const flush = async () => {
  for (let i = 0; i < 5; i++) {
    if (vi.isFakeTimers()) await vi.advanceTimersByTimeAsync(0);
    else await new Promise((r) => setTimeout(r, 0));
  }
};

let stops: (() => void)[] = [];
afterEach(() => {
  stops.forEach((s) => s());
  stops = [];
  sessionStorage.clear();
  localStorage.clear();
  document.querySelectorAll('dialog').forEach((d) => d.remove());
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

interface SolanaOpts {
  wallets: Wallet[];
  api?: SolanaTransactionApi;
}

interface MountOpts {
  provider?: EIP1193Provider;
  wallets?: DiscoveredWallet[];
  watch?: { emit?: (w: DiscoveredWallet[]) => void; rescan: () => void };
  discover?: () => Promise<DiscoveredWallet[]>;
  now?: number | (() => number);
  solana?: SolanaOpts;
  /** How long the page waits on a wallet request before abandoning it. */
  promptMs?: number;
  /** The poll interval (default: so long that only the tests trigger loads). */
  pollMs?: number;
}

async function mount(s: Session | ApiError, opts: MountOpts = {}) {
  const api = new CheckoutApi('', vi.fn() as never);
  const get = vi.spyOn(api, 'session').mockImplementation(async () => {
    if (s instanceof ApiError) throw s;
    return { data: s, serverTime: null };
  });
  vi.spyOn(api, 'networks').mockResolvedValue({ data: { data: NETWORKS }, serverTime: null });
  const root = document.createElement('main');
  document.body.replaceChildren(root);
  const wallets = opts.wallets ?? (opts.provider ? [walletOf('w', 'Test wallet', 'test', opts.provider)] : []);
  const watch = opts.watch;
  const now = opts.now;
  const clock = typeof now === 'function' ? now : () => now ?? NOW;
  const solana = opts.solana;
  const handle = await mountPay(root, 'ses_01JBTEST', {
    api, now: clock, pollMs: opts.pollMs ?? 1_000_000, discoverEvm: opts.discover ?? (async () => wallets),
    ...(watch ? { watchEvm: (cb: (w: DiscoveredWallet[]) => void) => { watch.emit = cb; return { rescan: watch.rescan, stop: () => {} }; } } : {}),
    ...(solana ? { listSolana: (chain: string) => listSolanaWallets(chain, () => solana.wallets), solanaApi: solana.api } : {}),
    ...(opts.promptMs ? { promptTimeoutMs: opts.promptMs } : {}),
  });
  stops.push(handle.stop);
  await flush();
  return { root, api, get };
}

const text = (root: HTMLElement) => root.textContent ?? '';

function walletOf(uuid: string, name: string, rdns: string, provider: EIP1193Provider): DiscoveredWallet {
  return { info: { uuid, name, icon: '', rdns }, provider };
}

/** A wallet on `chainId` holding `balance` base units of every token. */
function provider(chainId = '0x2105', balance?: bigint): EIP1193Provider {
  return {
    request: vi.fn(async ({ method }: { method: string }) => {
      if (method === 'eth_requestAccounts') return [PAYER];
      if (method === 'eth_chainId') return chainId;
      if (method === 'eth_call' && balance !== undefined) return '0x' + balance.toString(16).padStart(64, '0');
      throw new Error(method);
    }),
  } as unknown as EIP1193Provider;
}

describe('pay page states', () => {
  it('open: product, amount, instruction, countdown, deposit address with QR and full copy', async () => {
    const { root } = await mount(session());
    expect(root.querySelector('h1')!.textContent).toBe('Founder');
    expect(text(root)).toContain('25.00 USDC');
    expect(root.querySelector('h2.heading')!.textContent).toBe('Send exactly 25.00 USDC on Base');
    expect(text(root)).toContain(COPY.waiting);
    expect(root.querySelector('.pill')!.textContent).toBe('Open');
    expect(root.querySelector('[data-timer]')!.textContent).toBe('Expires in 1:00:00');
    expect(root.querySelector('svg.qr')).not.toBeNull();
    expect(root.querySelector('.field .address [aria-hidden]')!.textContent).toBe(`${EVM_ADDRESS.slice(0, 6)}…${EVM_ADDRESS.slice(-4)}`);
    expect(root.querySelector('[aria-label="Copy address"]')).not.toBeNull();
    expect(text(root)).toContain('Sending from an exchange? Add its withdrawal fee so 25.00 USDC arrives.');
    expect(text(root)).toContain('Exact on-chain amount 25.000000 USDC');
    expect(text(root)).toContain(COPY.footer);
    expect(text(root)).not.toContain('Bybit');
    expect(text(root)).not.toContain(SOLANA_EXCHANGE_NOTE);
  });

  it('logos: picker, hero, deposit header, amount and payments, all decorative next to the printed names', async () => {
    const { root } = await mount(session({
      status: 'partially_paid', amount_received: '10.00', amount_remaining: '15.00',
      payments: [{ chain: 'solana', asset: 'USDT', amount: '10.000000', status: 'confirmed', tx_hash: '5'.repeat(64) }],
    }));
    const options = [...root.querySelectorAll('.option')];
    expect(options.map((o) => o.querySelector('.asset-badge')!.getAttribute('aria-hidden'))).toEqual(['true', 'true', 'true']);
    expect(options.map((o) => [...o.querySelectorAll('img')].map((i) => i.getAttribute('src')!.replace(/^.*\/(\w+)-64\.png$/, '$1')))).toEqual([
      ['usdc', 'base'], ['usdc', 'solana'], ['usdt', 'solana'],
    ]);
    expect(root.querySelector('.amount-hero img')!.getAttribute('alt')).toBe('');
    expect(root.querySelector('.amount-hero')!.textContent).toBe('25.00 USDT');
    expect(root.querySelector('.deposit-head')!.textContent).toBe('Deposit addressUSDT on Solana');
    expect(root.querySelector('.deposit-head .asset-badge')!.getAttribute('aria-hidden')).toBe('true');
    expect(root.querySelector('.deposit .amount.with-logo img')!.getAttribute('alt')).toBe('');
    const pay = root.querySelector('.payments li')!;
    expect([...pay.querySelectorAll('img')].map((i) => i.getAttribute('src')!.replace(/^.*\/(\w+)-64\.png$/, '$1'))).toEqual(['usdt', 'solana']);
    expect(pay.textContent).toContain('10.00 USDTon Solana');
    for (const img of root.querySelectorAll('img:not(.footer img):not(.brand-header img)')) {
      expect(img.getAttribute('width')).toBe(img.getAttribute('height'));
      expect(img.closest('[aria-hidden="true"]')).not.toBeNull();
    }
    expect(root.querySelector('.testnet-tag')).toBeNull();
  });

  it('logos on a test network: the mainnet logo plus a Testnet tag wherever the network shows', async () => {
    const { root } = await mount(session({
      livemode: false,
      pay_to: [payTo({ chain: 'base-sepolia' }), payTo({ chain: 'solana-devnet', address: SOL_ADDRESS, signed_payment: false })],
    }));
    const options = [...root.querySelectorAll('.option')];
    expect(options.map((o) => o.querySelector('.net')!.textContent)).toEqual(['Base SepoliaTestnet', 'Solana devnetTestnet']);
    expect(options[0]!.querySelector('.logo-corner img')!.getAttribute('src')).toMatch(/base-64\.png$/);
    expect(root.querySelector('.deposit-head .testnet-tag')!.textContent).toBe('Testnet');
  });

  it('an unknown network or token shows its initial, never a broken image', async () => {
    const { root } = await mount(session({ pay_to: [payTo({ chain: 'celo', asset: 'USDC' }), payTo({ chain: 'plasma', asset: 'USDT0' })] }));
    const corners = [...root.querySelectorAll('.option .logo-corner')];
    expect(corners.map((c) => [c.textContent, c.querySelector('img')])).toEqual([['C', null], ['P', null]]);
    expect(root.querySelectorAll('.option')[1]!.querySelector('.logo')!.textContent).toBe('U');
    expect(root.querySelector('.deposit-head .logo-corner')!.textContent).toBe('C');
    expect(root.querySelector('.deposit-head > div')!.textContent).toBe('Deposit addressUSDC on Celo');
  });

  it('copies the full address, not the truncated one', async () => {
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    const { root } = await mount(session());
    root.querySelector<HTMLButtonElement>('[aria-label="Copy address"]')!.click();
    await flush();
    expect(writeText).toHaveBeenCalledWith(EVM_ADDRESS);
  });

  it('copy amount copies the exact on-chain string, not the trimmed one (ADR-0008 G-1)', async () => {
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    const { root } = await mount(session());
    root.querySelector<HTMLButtonElement>('[data-fid="copy-amount"]')!.click();
    await flush();
    expect(writeText).toHaveBeenCalledWith('25.000000');
  });

  it('makes the network unmissable: one warning, directly above the QR (ADR-0008 I-4)', async () => {
    const { root } = await mount(session());
    const warnings = root.querySelectorAll('[data-fid="network-warning"]');
    expect(warnings).toHaveLength(1);
    expect(warnings[0]!.getAttribute('role')).toBe('note');
    expect(warnings[0]!.querySelector('.network-warning-title')!.textContent).toBe('Send only USDC on Base');
    expect(warnings[0]!.textContent).toContain(COPY.sendOnlyDetail);
    expect(warnings[0]!.nextElementSibling!.className).toBe('qr-wrap');
    expect(root.textContent).not.toContain('cannot be recovered');
    expect(root.textContent).not.toContain('to this address');
  });

  it('a render error shows the fallback card and is not reported as offline (ADR-0008 D-1, D-2)', async () => {
    const { root } = await mount(session({ pay_to: undefined as never }));
    expect(root.textContent).toContain(COPY.renderFailed);
    expect(root.textContent).not.toContain(COPY.offline);
    expect(root.querySelector('[aria-busy]')).toBeNull();
  });

  it('shows the return link only for https (http in test mode) (ADR-0008 D-3)', async () => {
    expect(safeSuccessUrl('https://m.example/ok', true)).toBe('https://m.example/ok');
    expect(safeSuccessUrl('http://m.example/ok', true)).toBeNull();
    expect(safeSuccessUrl('http://m.example/ok', false)).toBe('http://m.example/ok');
    expect(safeSuccessUrl('javascript:alert(1)', false)).toBeNull();
    expect(safeSuccessUrl('not a url', false)).toBeNull();
    expect(safeSuccessUrl(null, true)).toBeNull();
    const { root } = await mount(session({ status: 'paid', amount_received: '25.00', amount_remaining: '0.00', success_url: 'javascript:alert(1)' }));
    expect(root.querySelector('[data-fid=success]')).toBeNull();
  });

  it('the network picker only lists the session pay_to and switches the address', async () => {
    const { root } = await mount(session());
    const radios = [...root.querySelectorAll<HTMLInputElement>('input[type=radio]')];
    expect(radios.map((r) => r.value)).toEqual(['base:USDC', 'solana:USDC', 'solana:USDT']);
    radios[2]!.click();
    await flush();
    expect(root.querySelector('h2.heading')!.textContent).toBe('Send exactly 25.00 USDT on Solana');
    expect(root.querySelector('.field .address')!.getAttribute('title')).toBe(SOL_ADDRESS);
    expect(text(root)).toContain(SOLANA_EXCHANGE_NOTE);
    // No exchange is named to the payer.
    expect(text(root)).not.toMatch(/Bybit|Bitget/);
    expect(root.querySelector<HTMLInputElement>('input[value="solana:USDT"]')!.checked).toBe(true);
  });

  it('shows the Solana deposit token account as an alternative address', async () => {
    const ata = '7UeZhudGACwJb3TSY8hdo4bJTpHiCTFv2wsmLGVP9kQz';
    const base = session();
    const { root } = await mount(session({ pay_to: base.pay_to.map((p) => (p.chain === 'solana' ? { ...p, token_account: ata } : p)) }));
    expect(root.querySelector('[aria-label="Copy token account"]')).toBeNull();
    root.querySelectorAll<HTMLInputElement>('input[type=radio]')[1]!.click();
    await flush();
    expect(text(root)).toContain('Or send to the token account');
    expect(root.querySelector('[aria-label="Copy token account"]')).not.toBeNull();
  });

  it('partial: remaining amount and the payment row', async () => {
    const { root } = await mount(session({
      status: 'partially_paid', amount_received: '10.00', amount_remaining: '15.00',
      payments: [{ chain: 'base', asset: 'USDC', amount: '10.000000', status: 'confirmed', tx_hash: '0x' + 'ab'.repeat(32) }],
    }));
    expect(root.querySelector('.pill')!.textContent).toBe('Partially paid');
    expect(text(root)).toContain('Received 10.00 of 25.00 USDC. Send the remaining 15.00 to the same address.');
    expect(root.querySelector('h2.heading')!.textContent).toBe('Send exactly 15.00 USDC on Base');
    expect(root.querySelector('.payments')!.textContent).toContain('10.00 USDC');
    expect(root.querySelector('.payments .pill')!.textContent).toBe('Confirmed');
  });

  it('paid: no address, the paid line, the return link', async () => {
    const { root } = await mount(session({ status: 'paid', amount_received: '25.00', amount_remaining: '0.00', success_url: 'https://merchant.example/thanks' }));
    expect(root.querySelector('.success .status-line')!.textContent).toBe(COPY.paid);
    // The success block says it once: no state pill on top.
    expect(root.querySelector('.pill')).toBeNull();
    expect(root.querySelector('svg.qr')).toBeNull();
    expect(root.querySelector('[aria-label="Copy address"]')).toBeNull();
    expect(root.querySelector<HTMLAnchorElement>('a.btn')!.href).toBe('https://merchant.example/thanks');
    expect(root.querySelector('[data-fid=success]')!.textContent).toBe('Return to the merchant');
  });

  it('the return link names the merchant when the session does', async () => {
    const { root } = await mount(session({ status: 'paid', amount_received: '25.00', amount_remaining: '0.00', success_url: 'https://merchant.example/thanks',
      merchant: { name: 'Acme Store', logo_url: '' } }));
    expect(root.querySelector('[data-fid=success]')!.textContent).toBe('Return to Acme Store');
  });

  it('overpaid with a named merchant: the copy, the return link, and the reference copied whole', async () => {
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    const { root } = await mount(session({ status: 'overpaid', amount_received: '26.00', amount_remaining: '0.00', success_url: 'https://merchant.example/thanks',
      merchant: { name: 'Acme Store', logo_url: '' },
      payments: [{ chain: 'base', asset: 'USDC', amount: '26.000000', status: 'confirmed', tx_hash: '0x' + 'ab'.repeat(32) }] }));
    expect(root.querySelector('.success .small')!.textContent).toBe('Acme Store received your payment. You can close this page.');
    expect(root.querySelector('[data-fid=overpaid-note]')!.textContent).toContain('It went to Acme Store with your payment.');
    expect(root.querySelector('[data-fid=overpaid-note] h2')!.textContent).toBe('About the extra 1.00 USDC');
    expect(root.querySelector('[data-fid=success]')!.textContent).toBe('Return to Acme Store');
    root.querySelector<HTMLButtonElement>('[data-fid=copy-reference]')!.click();
    await flush();
    expect(writeText).toHaveBeenCalledWith('ses_01JBTEST');
  });

  it('paid: one payment, linked to the explorer, without a second Confirmed pill', async () => {
    const tx = '0x' + 'ab'.repeat(32);
    const { root } = await mount(session({ status: 'paid', amount_received: '25.00', amount_remaining: '0.00',
      payments: [{ chain: 'base', asset: 'USDC', amount: '25.000000', status: 'confirmed', tx_hash: tx, confirmed_at: '2026-10-23T18:42:10Z' }] }));
    expect(root.querySelector('[data-fid="confirmed-at-0"]')!.textContent).toBe(`Confirmed ${formatDateTime('2026-10-23T18:42:10Z')}`);
    expect(root.querySelector('.payments')!.previousElementSibling!.textContent).toBe('Payment');
    expect(root.querySelector('.payments .pill')).toBeNull();
    const link = root.querySelector<HTMLAnchorElement>('.payments a.tx-link')!;
    expect(link.href).toBe(`https://basescan.org/tx/${tx}`);
    expect(link.target).toBe('_blank');
    expect(link.rel).toBe('noopener noreferrer');
    expect(root.querySelector('.payments [aria-label="Copy transaction hash"]')!.textContent).toBe('');
  });

  it('overpaid: the success block carries what was sent; a chain with no explorer shows the bare hash', async () => {
    const { root } = await mount(session({ status: 'overpaid', amount_received: '30.00', amount_remaining: '0.00',
      payments: [
        { chain: 'polygon', asset: 'USDC', amount: '25.000000', status: 'confirmed', tx_hash: '0x' + 'cd'.repeat(32) },
        { chain: 'base', asset: 'USDC', amount: '5.000000', status: 'reversed', tx_hash: '0x' + 'ef'.repeat(32) },
      ] }));
    expect(root.querySelector('.success .status-line')!.textContent).toBe(COPY.complete);
    expect(root.querySelector('.success .small')!.textContent).toBe('The merchant received your payment. You can close this page.');
    expect([...root.querySelectorAll('[data-fid=overpaid-pills] .pill')].map((p) => p.textContent)).toEqual(['Paid', '5.00 USDC extra']);
    expect([...root.querySelectorAll('[data-fid=overpaid-breakdown] dd')].map((d) => d.textContent)).toEqual(['25.00 USDC', '30.00 USDC', '5.00 USDC']);
    expect(root.querySelector('[data-fid=overpaid-note] h2')!.textContent).toBe('About the extra 5.00 USDC');
    expect(root.querySelector('[data-fid=overpaid-note]')!.textContent).toContain('only the merchant can send it back');
    expect(root.querySelector('[data-fid=overpaid-note] .reference')!.textContent).toBe('ses_01JBTEST');
    expect(root.querySelector('[data-fid=copy-reference]')).not.toBeNull();
    expect(root.querySelector('.payments')!.previousElementSibling!.textContent).toBe('Payments');
    const [unknown, reversed] = [...root.querySelectorAll('.payments li')];
    expect(unknown!.querySelector('a.tx-link')).toBeNull();
    expect(unknown!.querySelector('.address')).not.toBeNull();
    // An API without confirmed_at: no time line rather than a wrong one.
    expect(unknown!.querySelector('[data-fid^="confirmed-at"]')).toBeNull();
    // Only Confirmed goes quiet once paid: a reversed payment keeps its word.
    expect(reversed!.querySelector('.pill')!.textContent).toBe('Reversed');
  });

  it('late payment: the brand copy and no address', async () => {
    const { root } = await mount(session({ status: 'late_payment', amount_received: '25.00', amount_remaining: '0.00' }));
    expect(text(root)).toContain(COPY.late);
    expect(root.querySelector('.pill')!.textContent).toBe('Late payment');
    expect(root.querySelector('svg.qr')).toBeNull();
  });

  it('expired: no address', async () => {
    const { root } = await mount(session({ status: 'expired' }));
    expect(text(root)).toContain(COPY.expired);
    expect(root.querySelector('svg.qr')).toBeNull();
  });

  it('reversed payment: shown with its word', async () => {
    const { root } = await mount(session({ payments: [{ chain: 'base', asset: 'USDC', amount: '10.000000', status: 'reversed', tx_hash: '0x' + 'cd'.repeat(32) }] }));
    expect(root.querySelector('.payments .pill')!.textContent).toBe('Reversed');
    expect(text(root)).toContain('A payment of 10.00 USDC was reversed by the network and no longer counts.');
  });

  it('test mode is labelled', async () => {
    const { root } = await mount(session({ livemode: false }));
    expect(text(root)).toContain(COPY.testMode);
  });

  it('unknown session: not found', async () => {
    const { root } = await mount(new ApiError(404, 'session_not_found', 'no such session'));
    expect(text(root)).toContain(COPY.notFound);
  });

  it('unknown session: no wallet watcher is started', async () => {
    const watch: { emit?: (w: DiscoveredWallet[]) => void; rescan: () => void } = { rescan: vi.fn() };
    const { root } = await mount(new ApiError(404, 'session_not_found', 'no such session'), { watch });
    expect(text(root)).toContain(COPY.notFound);
    expect(watch.emit).toBeUndefined();
  });

  it('announces status changes through one live region', async () => {
    const { root } = await mount(session());
    const live = root.querySelector('[role=status][aria-live=polite]')!;
    expect(live.textContent).toBe(COPY.waiting);
  });
});

describe('pay page payment request QR', () => {
  const evmUri = (units: string) => `ethereum:${BASE_USDC}@8453/transfer?address=${EVM_ADDRESS}&uint256=${units}`;
  const qr = (root: HTMLElement) => decodeSvg(root.querySelector('svg.qr')!);
  const pointer = (coarse: boolean) => vi.stubGlobal('matchMedia', vi.fn((q: string) => ({ matches: coarse && q === '(pointer: coarse)', media: q })));
  const FEE_BASE = 'You send this transaction yourself, so your wallet needs a little ETH on Base for the network fee.';
  const FEE_SOL = 'You send this transaction yourself, so your wallet needs a little SOL for the network fee.';

  it('EVM: the QR is an EIP-681 transfer of what is due; address, amount and fee copy stay', async () => {
    const { root } = await mount(session());
    expect(qr(root)).toBe(evmUri('25000000'));
    expect(root.querySelector('svg.qr')!.getAttribute('aria-label')).toBe('QR code of the Base payment request');
    expect(root.querySelector('[aria-label="Copy address"]')).not.toBeNull();
    expect(root.querySelector('[aria-label="Copy amount"]')).not.toBeNull();
    expect(root.querySelector('[data-fid="fee-note"]')!.textContent).toBe(FEE_BASE);
  });

  it('follows the remaining amount after a partial payment', async () => {
    const { root, get } = await mount(session());
    expect(qr(root)).toBe(evmUri('25000000'));
    get.mockResolvedValue({ data: session({
      status: 'partially_paid', amount_received: '10.00', amount_remaining: '15.00',
      payments: [{ chain: 'base', asset: 'USDC', amount: '10.000000', status: 'confirmed', tx_hash: '0x' + 'ab'.repeat(32) }],
    }), serverTime: null });
    document.dispatchEvent(new Event('visibilitychange'));
    await flush();
    expect(qr(root)).toBe(evmUri('15000000'));
    expect(root.querySelector('.deposit .amount')!.textContent).toBe('15.00 USDC');
  });

  it('asks only for what is due once a pending payment confirms, like the amount field', async () => {
    const { root } = await mount(session({ payments: [{ chain: 'base', asset: 'USDC', amount: '10.000000', status: 'detected', tx_hash: '0x' + 'cd'.repeat(32) }] }));
    expect(qr(root)).toBe(evmUri('15000000'));
    expect(root.querySelector('.deposit .amount')!.textContent).toBe('15.00 USDC');
  });

  it('no QR, link or fee copy once the session stops taking payments', async () => {
    pointer(true);
    for (const s of [session({ status: 'expired' }), session({ status: 'paid', amount_received: '25.00', amount_remaining: '0.00' })]) {
      const { root } = await mount(s);
      expect(root.querySelector('svg.qr')).toBeNull();
      expect(root.querySelector('[data-fid="open-wallet"]')).toBeNull();
      expect(root.querySelector('[data-fid="fee-note"]')).toBeNull();
    }
    const { root } = await mount(session(), { now: Date.parse('2026-10-23T19:00:01Z') });
    expect(root.querySelector('svg.qr')).toBeNull();
    expect(root.querySelector('[data-fid="open-wallet"]')).toBeNull();
  });

  it('"Open in wallet app" only on a touch screen, with the same URI', async () => {
    pointer(false);
    let { root } = await mount(session());
    expect(root.querySelector('[data-fid="open-wallet"]')).toBeNull();
    pointer(true);
    ({ root } = await mount(session()));
    const link = root.querySelector<HTMLAnchorElement>('a[data-fid="open-wallet"]')!;
    expect(link.textContent).toBe('Open in wallet app');
    expect(link.getAttribute('href')).toBe(evmUri('25000000'));
    expect(link.closest('.deposit')).not.toBeNull();
  });

  it('Solana: the QR stays on the plain address, even with a reference, and no wallet link', async () => {
    pointer(true);
    const s = session();
    s.pay_to[1] = { ...s.pay_to[1]!, reference: '7Ufqy2TBBgoaFZ8HqnYMk6Cz9PeZqo5pPnkbGXZdaiLd' };
    const { root } = await mount(s);
    const opt = root.querySelector<HTMLInputElement>('[data-fid="opt-solana:USDC"]')!;
    opt.checked = true;
    opt.dispatchEvent(new Event('change'));
    expect(qr(root)).toBe(SOL_ADDRESS);
    expect(root.querySelector('svg.qr')!.getAttribute('aria-label')).toBe('QR code of the Solana deposit address');
    expect(root.querySelector('[data-fid="open-wallet"]')).toBeNull();
    expect(root.querySelector('[data-fid="fee-note"]')!.textContent).toBe(FEE_SOL);
  });

  it('a due amount with more significant decimals than the token has falls back to the plain address', async () => {
    pointer(true);
    const { root } = await mount(session({ payments: [{ chain: 'base', asset: 'USDC', amount: '10.000000000000000001', status: 'detected', tx_hash: '0x' + 'ef'.repeat(32) }] }));
    expect(qr(root)).toBe(EVM_ADDRESS);
    expect(root.querySelector('svg.qr')!.getAttribute('aria-label')).toBe('QR code of the Base deposit address');
    expect(root.querySelector('[data-fid="open-wallet"]')).toBeNull();
  });

  it('without the network list the QR falls back to the plain address', async () => {
    const api = new CheckoutApi('', vi.fn() as never);
    vi.spyOn(api, 'session').mockResolvedValue({ data: session(), serverTime: null });
    vi.spyOn(api, 'networks').mockRejectedValue(new ApiError(503, 'network_error', 'offline'));
    const root = document.createElement('main');
    document.body.replaceChildren(root);
    const h = await mountPay(root, 'ses_01JBTEST', { api, now: () => NOW, pollMs: 1_000_000 });
    stops.push(h.stop);
    await flush();
    expect(qr(root)).toBe(EVM_ADDRESS);
    expect(root.querySelector('svg.qr')!.getAttribute('aria-label')).toBe('QR code of the Base deposit address');
  });
});

describe('pay page ways to pay', () => {
  const shown = (root: HTMLElement, id: string) => !root.querySelector<HTMLElement>(`#${id}`)!.hidden;

  it('no wallet in the browser: the address is open and the wallet folds to one row', async () => {
    const { root } = await mount(session());
    expect(shown(root, 'pay-address')).toBe(true);
    expect(shown(root, 'pay-wallet')).toBe(false);
    expect(root.querySelector('[data-fid=method-address]')).toBeNull();
    root.querySelector<HTMLButtonElement>('[data-fid=method-wallet]')!.click();
    expect(shown(root, 'pay-wallet')).toBe(true);
    expect(shown(root, 'pay-address')).toBe(false);
    expect(root.querySelector('[data-fid=method-address]')!.textContent).toBe('Or send from any wallet or exchange');
  });

  it('a wallet in the browser: the wallet is open, the address folds, and the choice sticks', async () => {
    const { root } = await mount(session(), { provider: provider() });
    expect(shown(root, 'pay-wallet')).toBe(true);
    expect(shown(root, 'pay-address')).toBe(false);
    root.querySelector<HTMLButtonElement>('[data-fid=method-address]')!.click();
    expect(shown(root, 'pay-address')).toBe(true);
    expect(shown(root, 'pay-wallet')).toBe(false);
    await flush();
    expect(shown(root, 'pay-address')).toBe(true);
  });

  it('keyboard focus moves into the way to pay that opened', async () => {
    const { root } = await mount(session(), { provider: provider() });
    const toAddress = root.querySelector<HTMLButtonElement>('[data-fid=method-address]')!;
    toAddress.focus();
    toAddress.click();
    expect(document.activeElement).toBe(root.querySelector('#pay-address'));
    root.querySelector<HTMLButtonElement>('[data-fid=method-wallet]')!.click();
    expect(document.activeElement).toBe(root.querySelector('#pay-wallet'));
  });

  it('"Use the address" in the no-wallet dialog opens the address and keeps focus there', async () => {
    const { root } = await mount(session());
    root.querySelector<HTMLButtonElement>('[data-fid=method-wallet]')!.click();
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    document.querySelector<HTMLButtonElement>('dialog [data-fid=use-address]')!.click();
    await flush();
    expect(shown(root, 'pay-address')).toBe(true);
    expect(document.activeElement).toBe(root.querySelector('#pay-address'));
  });

  it('a wallet announced after the first look does not fold the address the payer is reading', async () => {
    const watch: MountOpts['watch'] = { rescan: vi.fn() };
    const { root } = await mount(session(), { watch });
    expect(shown(root, 'pay-address')).toBe(true);
    watch!.emit!([walletOf('mm', 'MetaMask', 'io.metamask', provider())]);
    await flush();
    expect(shown(root, 'pay-address')).toBe(true);
    expect(shown(root, 'pay-wallet')).toBe(false);
  });

  it('a wallet that cannot pay what is left: the address opens', async () => {
    const tiny = session({ status: 'partially_paid', amount_received: '24.999999', amount_remaining: '0.000001', pay_to: [payTo({ amount: '0.000001' })] });
    const { root } = await mount(tiny, { provider: provider() });
    expect(root.querySelector('#pay-wallet')!.hasAttribute('data-short')).toBe(true);
    expect(shown(root, 'pay-address')).toBe(true);
    expect(shown(root, 'pay-wallet')).toBe(false);
  });
});

describe('pay page wallet flow', () => {
  it('connect, sign the typed data, submit, then show progress', async () => {
    const calls: string[] = [];
    const provider = {
      request: vi.fn(async ({ method }: { method: string }) => {
        calls.push(method);
        if (method === 'eth_requestAccounts') return [PAYER];
        if (method === 'eth_chainId') return '0x2105';
        if (method === 'eth_signTypedData_v4') return '0x' + '11'.repeat(64) + '1b';
        throw new Error(method);
      }),
    } as unknown as EIP1193Provider;
    const { root, api, get } = await mount(session(), { provider });
    const typed = vi.spyOn(api, 'typedData').mockResolvedValue({ data: typedData(), serverTime: null });
    const submit = vi.spyOn(api, 'submit').mockResolvedValue({ data: { id: 'txj_01', session_id: 'ses_01JBTEST', status: 'pending' }, serverTime: null });

    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(root.querySelector('.wallet-name')!.textContent).toBe('Test wallet');
    const pay = root.querySelector<HTMLButtonElement>('[data-fid=pay]')!;
    expect(pay.textContent).toBe('Sign to pay 25.00 USDC');
    expect(text(root)).toContain(WALLET_COPY.signedNote);

    get.mockResolvedValue({ data: session({ signed_payments: [{ id: 'txj_01', chain: 'base', asset: 'USDC', status: 'submitted', outcome: null, tx_hash: null }] }), serverTime: null });
    pay.click();
    await flush();
    expect(typed).toHaveBeenCalledTimes(1);
    expect(calls).toContain('eth_signTypedData_v4');
    expect(submit).toHaveBeenCalledTimes(1);
    const progress = root.querySelector('[data-fid=progress]')!;
    expect(progress.getAttribute('data-phase')).toBe('sent');
    expect(text(root)).toContain(PROGRESS_COPY.signedText('Base'));
    expect(steps(root)).toEqual(['Signed in your wallet (in progress)', 'Seen on the network (next)', 'Confirmations (next)', 'Paid (next)']);
    expect(text(root)).toContain(PROGRESS_COPY.onItsWay);
    expect(root.querySelector('[data-fid=pay]')).toBeNull();
    expect(root.querySelector('svg.qr')).toBeNull();
    expect(JSON.parse(sessionStorage.getItem('pp:sent:ses_01JBTEST')!)).toMatchObject({ key: 'base:USDC', signature: '', job: 'txj_01' });

    // Prismaport sent it: the transaction shows, and the page looks for it on the network.
    get.mockResolvedValue({ data: session({ signed_payments: [{ id: 'txj_01', chain: 'base', asset: 'USDC', status: 'done', outcome: 'settled', tx_hash: TX }] }), serverTime: null });
    await poll();
    expect(steps(root)).toEqual(['Sent by Prismaport (done)', 'Seen on the network (in progress)', 'Confirmations (next)', 'Paid (next)']);
    expect(root.querySelector('[data-fid=progress] .tx-link')!.getAttribute('href')).toContain(TX);
  });

  it('a signed payment that did not go through goes back to the button and says so', async () => {
    const provider = {
      request: vi.fn(async ({ method }: { method: string }) => {
        if (method === 'eth_requestAccounts') return [PAYER];
        if (method === 'eth_chainId') return '0x2105';
        if (method === 'eth_signTypedData_v4') return '0x' + '11'.repeat(64) + '1b';
        throw new Error(method);
      }),
    } as unknown as EIP1193Provider;
    const { root, api, get } = await mount(session(), { provider });
    vi.spyOn(api, 'typedData').mockResolvedValue({ data: typedData(), serverTime: null });
    vi.spyOn(api, 'submit').mockResolvedValue({ data: { id: 'txj_01', session_id: 'ses_01JBTEST', status: 'pending' }, serverTime: null });
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    get.mockResolvedValue({ data: session({ signed_payments: [{ id: 'txj_01', chain: 'base', asset: 'USDC', status: 'failed', outcome: 'rejected', tx_hash: null }] }), serverTime: null });
    root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.click();
    await flush();
    expect(root.querySelector('[data-fid=progress]')).toBeNull();
    expect(text(root)).toContain('This signed payment did not go through. You can sign again.');
    expect(root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(false);
    expect(sessionStorage.getItem('pp:sent:ses_01JBTEST')).toBeNull();
  });

  it('a transfer: confirm in the wallet, then sent, found with its confirmations, and paid', async () => {
    let release!: () => void;
    const sending = new Promise<void>((r) => { release = r; });
    const provider = {
      request: vi.fn(async ({ method }: { method: string }) => {
        if (method === 'eth_requestAccounts' || method === 'eth_accounts') return [PAYER];
        if (method === 'eth_chainId') return '0x2105';
        if (method === 'eth_estimateGas') return '0x5208';
        if (method === 'eth_sendTransaction') {
          await sending;
          return TX;
        }
        throw new Error(method);
      }),
    } as unknown as EIP1193Provider;
    const transfer = session({ pay_to: [payTo({ signed_payment: false })] });
    const { root, get } = await mount(transfer, { provider });
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(root.querySelector('[data-fid=pay]')!.textContent).toBe('Send 25.00 USDC');
    root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.click();
    await flush();

    // While the wallet asks: only what to approve, no address to pay twice with.
    const confirm = root.querySelector('[data-fid=progress]')!;
    expect(confirm.getAttribute('data-phase')).toBe('confirm');
    expect(confirm.querySelector('h2')!.textContent).toBe('Confirm in Test wallet');
    expect(text(root)).toContain('Approve the transfer of 25.00 USDC on Base. Your wallet pays the network fee.');
    expect(root.querySelector('svg.qr')).toBeNull();
    expect(root.querySelector('[data-timer]')).toBeNull();

    release();
    await flush();
    const sent = root.querySelector('[data-fid=progress]')!;
    expect(sent.getAttribute('data-phase')).toBe('sent');
    expect(sent.querySelector('h2')!.textContent).toBe(PROGRESS_COPY.checking);
    expect(steps(root)).toEqual(['Sent from your wallet (done)', 'Seen on the network (in progress)', 'Confirmations (next)', 'Paid (next)']);
    expect(sent.querySelector('.tx-link')!.getAttribute('href')).toContain(TX);
    expect(root.querySelector('[data-elapsed]')!.textContent).toBe('0:00 elapsed');
    expect(root.querySelector('[data-fid=pay]')).toBeNull();
    expect(root.querySelector('svg.qr')).toBeNull();

    // Still not listed: no way to pay comes back on a poll.
    await poll();
    expect(root.querySelector('[data-fid=pay]')).toBeNull();

    const detected = { chain: 'base', asset: 'USDC', amount: '25.000000', status: 'detected' as const, tx_hash: TX, confirmed_at: null, confirmations: 6, confirmations_required: 10 };
    get.mockResolvedValue({ data: session({ ...transfer, payments: [detected] }), serverTime: null });
    await poll();
    const found = root.querySelector('[data-fid=progress]')!;
    expect(found.getAttribute('data-phase')).toBe('seen');
    expect(found.querySelector('h2')!.textContent).toBe(PROGRESS_COPY.found);
    expect(root.querySelector('.pill')!.textContent).toBe('Confirming');
    expect(steps(root)).toEqual(['Sent (done)', 'Seen on the network (done)', 'Confirmations (in progress)', 'Paid (next)']);
    expect(root.querySelector('[data-fid=confirmations]')!.textContent).toBe('6 of 10');
    expect(root.querySelector('[role=progressbar]')!.getAttribute('aria-valuenow')).toBe('6');
    expect(root.querySelector('.payments')).toBeNull();
    expect(sessionStorage.getItem('pp:sent:ses_01JBTEST')).toBeNull();

    const { confirmations: _c, confirmations_required: _r, ...rest } = detected;
    get.mockResolvedValue({ data: session({ ...transfer, status: 'paid', amount_received: '25.00', amount_remaining: '0.00',
      payments: [{ ...rest, status: 'confirmed', confirmed_at: '2026-10-23T18:42:10Z' }] }), serverTime: null });
    await poll();
    expect(root.querySelector('[data-fid=progress]')).toBeNull();
    expect(root.querySelector('[data-fid=paid]')).not.toBeNull();
  });

  it('a detected payment without a count still shows it is confirming', async () => {
    const { root } = await mount(session({ payments: [{ chain: 'base', asset: 'USDC', amount: '25.000000', status: 'detected', tx_hash: TX, confirmed_at: null }] }));
    expect(root.querySelector('[data-fid=progress]')!.getAttribute('data-phase')).toBe('seen');
    expect(text(root)).toContain('Waiting for the network to confirm it.');
    expect(root.querySelector('[role=progressbar]')).toBeNull();
  });

  it('too small to settle: no pay button, only the address', async () => {
    const provider = { request: vi.fn(async () => [PAYER]) } as unknown as EIP1193Provider;
    const { root } = await mount(session({ status: 'partially_paid', amount_received: '24.95', amount_remaining: '0.05' }), { provider });
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(root.querySelector('[data-fid=pay]')).toBeNull();
    expect(text(root)).toContain('The amount left is too small to pay by signature. Send it to the address below instead.');
    expect(root.querySelector('svg.qr')).not.toBeNull();
  });

  it('no wallet found: Connect still shows and opens a dialog to retry, pay by address or get a wallet', async () => {
    const rescan = vi.fn();
    const { root } = await mount(session(), { watch: { rescan } });
    expect(text(root)).toContain(WALLET_COPY.signedHint);
    expect(root.querySelector('svg.qr')).not.toBeNull();
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    const dialog = document.querySelector('dialog')!;
    expect(dialog.textContent).toContain(WALLET_COPY.noWalletTitle);
    const downloads = [...dialog.querySelectorAll<HTMLAnchorElement>('a.download')];
    expect(downloads.map((a) => a.href)).toEqual(['https://metamask.io/download/', 'https://rabby.io/', 'https://phantom.com/download']);
    expect(downloads.every((a) => a.target === '_blank' && a.rel === 'noopener noreferrer' && a.querySelector('img') !== null)).toBe(true);
    // Connect asked once more before giving up; Try again asks again.
    expect(rescan).toHaveBeenCalledTimes(1);
    dialog.querySelector<HTMLButtonElement>('[data-fid=retry]')!.click();
    await flush();
    expect(rescan).toHaveBeenCalledTimes(2);
    expect(dialog.querySelector<HTMLElement>('[data-fid=still-none]')!.hidden).toBe(false);
    expect(dialog.textContent).toContain(WALLET_COPY.stillNone);
    dialog.querySelector<HTMLButtonElement>('[data-fid=use-address]')!.click();
    await flush();
    expect(document.querySelector('dialog')).toBeNull();
    expect(root.querySelector('.wallet-name')).toBeNull();
  });

  it('Try again finds a wallet that loaded late and connects it', async () => {
    const wallet = walletOf('mm', 'MetaMask', 'io.metamask', provider());
    // Nothing on the first look nor on Connect's second look; Try again finds it.
    const discover = vi.fn<() => Promise<DiscoveredWallet[]>>().mockResolvedValueOnce([]).mockResolvedValueOnce([]).mockResolvedValue([wallet]);
    const { root } = await mount(session(), { discover });
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    document.querySelector<HTMLButtonElement>('dialog [data-fid=retry]')!.click();
    await flush();
    expect(document.querySelector('dialog')).toBeNull();
    expect(root.querySelector('.wallet-name')!.textContent).toBe('MetaMask');
    expect(root.querySelector('.wallet-account img')!.getAttribute('src')).toMatch(/wallet-metamask-64\.png$/);
  });

  it('Connect clicked before the first look for wallets ends waits for it instead of saying none', async () => {
    let release!: (w: DiscoveredWallet[]) => void;
    const pending = new Promise<DiscoveredWallet[]>((r) => { release = r; });
    const api = new CheckoutApi('', vi.fn() as never);
    vi.spyOn(api, 'session').mockResolvedValue({ data: session(), serverTime: null });
    vi.spyOn(api, 'networks').mockResolvedValue({ data: { data: NETWORKS }, serverTime: null });
    const root = document.createElement('main');
    document.body.replaceChildren(root);
    const mounted = mountPay(root, 'ses_01JBTEST', { api, now: () => NOW, pollMs: 1_000_000, discoverEvm: () => pending });
    await flush();
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(document.querySelector('dialog')).toBeNull();
    release([walletOf('mm', 'MetaMask', 'io.metamask', provider())]);
    stops.push((await mounted).stop);
    await flush();
    expect(document.querySelector('dialog')).toBeNull();
    expect(root.querySelector('.wallet-name')!.textContent).toBe('MetaMask');
  });

  it('a wallet that announces itself late shows up without a reload', async () => {
    const watch: MountOpts['watch'] = { rescan: vi.fn() };
    const { root } = await mount(session(), { watch });
    watch!.emit!([walletOf('mm', 'MetaMask', 'io.metamask', provider())]);
    await flush();
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(document.querySelector('dialog')).toBeNull();
    expect(root.querySelector('.wallet-name')!.textContent).toBe('MetaMask');
  });

  it('several wallets: a picker with logos, the last used first', async () => {
    localStorage.setItem('pp:wallet:last', 'io.rabby');
    const p = provider();
    const wallets = [walletOf('mm', 'MetaMask', 'io.metamask', p), walletOf('rb', 'Rabby Wallet', 'io.rabby', p), walletOf('x', 'Other', 'com.example', p)];
    const { root } = await mount(session(), { wallets });
    // The page does not list the wallets before Connect.
    expect(text(root)).not.toContain('MetaMask');
    expect(text(root)).toContain(WALLET_COPY.signedHint);
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    const options = [...document.querySelectorAll<HTMLButtonElement>('dialog .wallet-option')];
    expect(options.map((o) => o.querySelector('.wallet-option-name')!.textContent)).toEqual(['Rabby Wallet', 'MetaMask', 'Other']);
    expect(options[0]!.textContent).toContain('Last used');
    expect(options[1]!.textContent).not.toContain('Last used');
    expect(options[0]!.querySelector('img')).not.toBeNull();
    expect(options[2]!.querySelector('img')).toBeNull();
    options[1]!.click();
    await flush();
    expect(document.querySelector('dialog')).toBeNull();
    expect(root.querySelector('.wallet-name')!.textContent).toBe('MetaMask');
    expect(localStorage.getItem('pp:wallet:last')).toBe('io.metamask');
  });

  it('an announced wallet replaces the window.ethereum fallback of the first look', async () => {
    const watch: MountOpts['watch'] = { rescan: vi.fn() };
    const p = provider();
    const { root } = await mount(session(), { wallets: [walletOf('injected', 'Browser wallet', '', p)], watch });
    watch!.emit!([walletOf('mm', 'MetaMask', 'io.metamask', p)]);
    await flush();
    watch!.emit!([]);
    await flush();
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(root.querySelector('.wallet-name')!.textContent).toBe('MetaMask');
  });

  it('a double click on Connect opens one dialog', async () => {
    const { root } = await mount(session());
    const connect = root.querySelector<HTMLButtonElement>('[data-fid=connect]')!;
    connect.click();
    connect.click();
    await flush();
    expect(document.querySelectorAll('dialog')).toHaveLength(1);
  });

  it('one wallet: Disconnect instead of Change; several: Change opens the picker again', async () => {
    const one = await mount(session(), { provider: provider() });
    one.root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(one.root.querySelector('[data-fid=change]')).toBeNull();
    one.root.querySelector<HTMLButtonElement>('[data-fid=disconnect]')!.click();
    await flush();
    expect(one.root.querySelector('.wallet-name')).toBeNull();
    expect(one.root.querySelector('[data-fid=connect]')).not.toBeNull();
    stops.forEach((s) => s());
    stops = [];
    const p = provider();
    const two = await mount(session(), { wallets: [walletOf('a', 'A', 'a', p), walletOf('b', 'B', 'b', p)] });
    two.root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    document.querySelector<HTMLButtonElement>('dialog [data-fid=wallet-a]')!.click();
    await flush();
    two.root.querySelector<HTMLButtonElement>('[data-fid=change]')!.click();
    await flush();
    expect(two.root.querySelector('.wallet-name')).toBeNull();
    expect(document.querySelectorAll('dialog .wallet-option')).toHaveLength(2);
  });

  it('closing the picker connects nothing', async () => {
    const p = provider();
    const { root } = await mount(session(), { wallets: [walletOf('a', 'A', 'a', p), walletOf('b', 'B', 'b', p)] });
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    document.querySelector<HTMLButtonElement>('dialog [data-fid=dialog-close]')!.click();
    await flush();
    expect(document.querySelector('dialog')).toBeNull();
    expect(root.querySelector('[data-fid=connect]')).not.toBeNull();
  });

  it('shows the balance when the wallet is on the session network, and none on another chain', async () => {
    const { root } = await mount(session(), { provider: provider('0x2105', 20_500_000n) });
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(root.querySelector('[data-fid=balance]')!.textContent).toBe('Balance20.50 USDC');
    stops.forEach((s) => s());
    stops = [];
    const other = await mount(session(), { provider: provider('0x1', 20_500_000n) });
    other.root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(other.root.querySelector('.wallet-name')).not.toBeNull();
    expect(other.root.querySelector('[data-fid=balance]')).toBeNull();
  });

  it('warns and offers no signature when the account holds less than is due', async () => {
    const { root } = await mount(session(), { provider: provider('0x2105', 20_000_000n) });
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(root.querySelector('[data-fid=balance]')!.textContent).toBe('Balance20.00 USDC');
    expect(root.querySelector('.balance-value')!.hasAttribute('data-low')).toBe(true);
    const pay = root.querySelector<HTMLButtonElement>('[data-fid=pay]')!;
    expect(pay.disabled).toBe(true);
    expect(pay.textContent).toBe('Sign to pay 25.00 USDC');
    expect(root.querySelector('[data-fid=low-balance]')!.textContent)
      .toBe(WALLET_COPY.lowBalanceTitle('USDC') + WALLET_COPY.lowBalance('20.00 USDC', '25.00 USDC', 'USDC', 'Test wallet'));
    expect(root.querySelector('svg.qr')).not.toBeNull();
  });

  it('enough balance, or a balance the wallet cannot tell: the signature is offered', async () => {
    const { root } = await mount(session(), { provider: provider('0x2105', 25_000_000n) });
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(root.querySelector('[data-fid=low-balance]')).toBeNull();
    expect(root.querySelector('.balance-value')!.hasAttribute('data-low')).toBe(false);
    expect(root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(false);
    expect(root.querySelector('[data-fid=pay]')!.textContent).toBe('Sign to pay 25.00 USDC');
    stops.forEach((s) => s());
    stops = [];
    // On another chain the wallet's balance says nothing about this payment: no line, no warning.
    const other = await mount(session(), { provider: provider('0x1', 1n) });
    other.root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(other.root.querySelector('[data-fid=balance]')).toBeNull();
    expect(other.root.querySelector('[data-fid=low-balance]')).toBeNull();
    expect(other.root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(false);
  });

  it('a chain switch in the wallet re-reads the balance; a late answer for an older read is dropped', async () => {
    const listeners: Record<string, ((x: unknown) => void)[]> = {};
    let chain = '0x1';
    const pending: ((v: string) => void)[] = [];
    const p = {
      request: vi.fn(async ({ method }: { method: string }) => {
        if (method === 'eth_requestAccounts') return [PAYER];
        if (method === 'eth_chainId') return chain;
        if (method === 'eth_call') return new Promise<string>((r) => pending.push(r));
        throw new Error(method);
      }),
      on: (ev: string, fn: (x: unknown) => void) => { (listeners[ev] ??= []).push(fn); },
      removeListener: () => {},
    } as unknown as EIP1193Provider;
    const word = (n: bigint) => '0x' + n.toString(16).padStart(64, '0');
    const { root } = await mount(session(), { provider: p });
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(root.querySelector('[data-fid=balance]')).toBeNull();
    chain = '0x2105';
    listeners.chainChanged!.forEach((f) => f('0x2105'));
    await flush();
    listeners.chainChanged!.forEach((f) => f('0x2105'));
    await flush();
    expect(pending).toHaveLength(2);
    pending[1]!(word(30_000_000n));
    await flush();
    expect(root.querySelector('[data-fid=balance]')!.textContent).toBe('Balance30.00 USDC');
    pending[0]!(word(1_000_000n));
    await flush();
    expect(root.querySelector('[data-fid=balance]')!.textContent).toBe('Balance30.00 USDC');
    expect(root.querySelector('[data-fid=low-balance]')).toBeNull();
  });

  it('follows an account switch in the wallet: new account, new balance; no account left disconnects', async () => {
    const OTHER = '0x468F86B3311786E7FD02ae754aE0a4A79A33d05E';
    const listeners: Record<string, ((x: unknown) => void)[]> = {};
    let current = PAYER;
    const p = {
      request: vi.fn(async ({ method, params }: { method: string; params?: [{ data: string }] }) => {
        if (method === 'eth_requestAccounts') return [current];
        if (method === 'eth_chainId') return '0x2105';
        if (method === 'eth_call') {
          const balance = params![0].data.toLowerCase().endsWith(OTHER.slice(2).toLowerCase()) ? 40_000_000n : 20_000_000n;
          return '0x' + balance.toString(16).padStart(64, '0');
        }
        throw new Error(method);
      }),
      on: (ev: string, fn: (x: unknown) => void) => { (listeners[ev] ??= []).push(fn); },
      removeListener: (ev: string, fn: (x: unknown) => void) => { listeners[ev] = (listeners[ev] ?? []).filter((f) => f !== fn); },
    } as unknown as EIP1193Provider;
    const { root } = await mount(session(), { provider: p });
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(root.querySelector('[data-fid=low-balance]')).not.toBeNull();
    current = OTHER;
    listeners.accountsChanged!.forEach((f) => f([OTHER.toLowerCase()]));
    await flush();
    expect(root.querySelector('.wallet-account')!.textContent).toContain('0x468F');
    expect(root.querySelector('[data-fid=balance]')!.textContent).toBe('Balance40.00 USDC');
    expect(root.querySelector('[data-fid=low-balance]')).toBeNull();
    expect(root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(false);
    listeners.accountsChanged!.forEach((f) => f([]));
    await flush();
    expect(root.querySelector('.wallet-account')).toBeNull();
    expect(root.querySelector('[data-fid=connect]')).not.toBeNull();
    expect(listeners.accountsChanged).toHaveLength(0);
  });

  it('"No gas needed" explains the signed payment in three steps', async () => {
    const { root } = await mount(session());
    const badge = root.querySelector<HTMLButtonElement>('[data-fid=gas-help]')!;
    expect(badge.getAttribute('aria-expanded')).toBe('false');
    badge.click();
    await flush();
    expect(root.querySelectorAll('[data-fid=gas-steps] li')).toHaveLength(3);
    expect(root.querySelector('[data-fid=gas-help]')!.getAttribute('aria-expanded')).toBe('true');
  });

  it('networks that fail to load are fetched again on the next session poll, bringing back the exact amount', async () => {
    const api = new CheckoutApi('', vi.fn() as never);
    vi.spyOn(api, 'session').mockResolvedValue({ data: session(), serverTime: null });
    const nets = vi.spyOn(api, 'networks')
      .mockRejectedValueOnce(new ApiError(503, 'network_error', 'offline'))
      .mockResolvedValue({ data: { data: NETWORKS }, serverTime: null });
    const root = document.createElement('main');
    document.body.replaceChildren(root);
    const h = await mountPay(root, 'ses_01JBTEST', { api, now: () => NOW, pollMs: 1_000_000 });
    stops.push(h.stop);
    await flush();
    expect(text(root)).not.toContain('Exact on-chain amount');

    document.dispatchEvent(new Event('visibilitychange'));
    await flush();
    expect(nets).toHaveBeenCalledTimes(2);
    expect(text(root)).toContain('Exact on-chain amount 25.000000 USDC');

    document.dispatchEvent(new Event('visibilitychange'));
    await flush();
    expect(nets).toHaveBeenCalledTimes(2);
  });

  it('countdown corrected by the server clock', async () => {
    const api = new CheckoutApi('', vi.fn() as never);
    vi.spyOn(api, 'session').mockResolvedValue({ data: session(), serverTime: NOW + 30 * 60_000 });
    vi.spyOn(api, 'networks').mockResolvedValue({ data: { data: NETWORKS }, serverTime: null });
    const root = document.createElement('main');
    document.body.replaceChildren(root);
    const h = await mountPay(root, 'ses_01JBTEST', { api, now: () => NOW, pollMs: 1_000_000 });
    stops.push(h.stop);
    await flush();
    expect(root.querySelector('[data-timer]')!.textContent).toBe('Expires in 30:00');
  });
});

const SIGNATURE = new Uint8Array(64).fill(7);
const SIG58 = base58(SIGNATURE);

function solWallet(name: string, send: () => Promise<readonly { signature: Uint8Array }[]>): Wallet {
  const account = { address: SOL_ADDRESS, publicKey: new Uint8Array(32), chains: ['solana:mainnet'], features: [SIGN_AND_SEND] };
  return {
    version: '1.0.0', name, icon: 'data:image/svg+xml;base64,AA==', chains: ['solana:mainnet'], accounts: [],
    features: {
      'standard:connect': { version: '1.0.0', connect: async () => ({ accounts: [account] }) },
      [SIGN_AND_SEND]: { version: '1.0.0', supportedTransactionVersions: ['legacy', 0], signAndSendTransaction: send },
    },
  } as unknown as Wallet;
}

const solApi = () => ({ buildTransaction: vi.fn(async () => ({ transaction: btoa('tx') })) });
const signs = () => solWallet('Phantom', async () => [{ signature: SIGNATURE }]);

const solSession = (over: Partial<Session> = {}) => session({
  pay_to: [
    payTo({ chain: 'solana', address: SOL_ADDRESS, signed_payment: false, connected_wallet: true }),
    payTo({ chain: 'solana', asset: 'USDT', address: SOL_ADDRESS, signed_payment: false }),
    payTo(),
  ],
  ...over,
});

const solConnect = (root: HTMLElement) => root.querySelector<HTMLButtonElement>('[data-fid="sol-connect"]');
const solButton = (root: HTMLElement) => root.querySelector<HTMLButtonElement>('[data-fid="sol-pay"]');

/** Connect (when not yet connected), then Pay: the two steps of the Solana card. */
async function paySol(root: HTMLElement) {
  if (!solButton(root)) {
    solConnect(root)!.click();
    await flush();
  }
  solButton(root)!.click();
  await flush();
}

describe('pay page Solana connected wallet', () => {
  it('offers the wallet only on a connected_wallet option', async () => {
    const { root } = await mount(solSession(), { solana: { wallets: [signs()], api: solApi() } });
    expect(solConnect(root)!.textContent).toBe('Connect wallet');
    expect(root.querySelector('.wallet-card .gas-badge')!.textContent).toBe('No SOL needed');

    root.querySelector<HTMLInputElement>('input[value="solana:USDT"]')!.click();
    await flush();
    expect(solConnect(root)).toBeNull();

    root.querySelector<HTMLInputElement>('input[value="base:USDC"]')!.click();
    await flush();
    expect(solConnect(root)).toBeNull();
  });

  it('one wallet: Connect shows the account, then Pay; Disconnect goes back', async () => {
    const { root } = await mount(solSession(), { solana: { wallets: [signs()], api: solApi() } });
    expect(solButton(root)).toBeNull();
    solConnect(root)!.click();
    await flush();
    expect(root.querySelector('.wallet-card .wallet-name')!.textContent).toBe('Phantom');
    expect(root.querySelector('.wallet-card .wallet-account img')!.getAttribute('src')).toMatch(/wallet-phantom-64\.png$/);
    expect(root.querySelector('.wallet-card .wallet-account .net-dot')!.textContent).toBe('Solana');
    expect(solButton(root)!.textContent).toBe('Pay 25.00 USDC');
    expect(text(root)).toContain('No SOL needed. Phantom asks you to approve; Prismaport pays the network fee.');
    expect(root.querySelector('[data-fid=sol-change]')).toBeNull();
    root.querySelector<HTMLButtonElement>('[data-fid=sol-disconnect]')!.click();
    expect(solButton(root)).toBeNull();
    expect(solConnect(root)).not.toBeNull();
  });

  it('Pay reuses the connected account, and connects again when the wallet no longer lists it', async () => {
    const wallet = signs();
    const feature = wallet.features['standard:connect'] as { connect: () => Promise<{ accounts: unknown[] }> };
    const connectOnce = feature.connect;
    const connect = vi.fn(connectOnce);
    feature.connect = connect;
    const api = solApi();
    const { root } = await mount(solSession(), { solana: { wallets: [wallet], api } });
    solConnect(root)!.click();
    await flush();
    expect(connect).toHaveBeenCalledTimes(1);
    // The wallet lists the account it shared: Pay builds for it without a second connect.
    (wallet as unknown as { accounts: unknown[] }).accounts = (await connectOnce()).accounts;
    solButton(root)!.click();
    await flush();
    expect(connect).toHaveBeenCalledTimes(1);
    expect(api.buildTransaction).toHaveBeenCalledTimes(1);
  });

  it('an account switched in the wallet after Connect: Pay connects again first', async () => {
    const wallet = signs();
    const feature = wallet.features['standard:connect'] as { connect: () => Promise<{ accounts: unknown[] }> };
    const connect = vi.fn(feature.connect);
    feature.connect = connect;
    const { root } = await mount(solSession(), { solana: { wallets: [wallet], api: solApi() } });
    solConnect(root)!.click();
    await flush();
    (wallet as unknown as { accounts: unknown[] }).accounts = [];
    solButton(root)!.click();
    await flush();
    expect(connect).toHaveBeenCalledTimes(2);
  });

  it('several wallets: a picker with logos and the last used first; Change opens it again', async () => {
    localStorage.setItem('pp:wallet:last', 'MetaMask');
    const { root } = await mount(solSession(), { solana: { wallets: [signs(), solWallet('MetaMask', async () => [{ signature: SIGNATURE }])], api: solApi() } });
    solConnect(root)!.click();
    await flush();
    const options = [...document.querySelectorAll<HTMLButtonElement>('dialog .wallet-option')];
    expect(options.map((o) => o.querySelector('.wallet-option-name')!.textContent)).toEqual(['MetaMask', 'Phantom']);
    expect(options[0]!.textContent).toContain('Last used');
    expect(options.every((o) => o.querySelector('img') !== null)).toBe(true);
    expect(document.querySelector('dialog')!.textContent).toContain('Solana wallets found in this browser');
    options[1]!.click();
    await flush();
    expect(root.querySelector('.wallet-card .wallet-name')!.textContent).toBe('Phantom');
    expect(localStorage.getItem('pp:wallet:last')).toBe('Phantom');
    root.querySelector<HTMLButtonElement>('[data-fid=sol-change]')!.click();
    await flush();
    expect(document.querySelectorAll('dialog .wallet-option')).toHaveLength(2);
  });

  it('"No SOL needed" explains the payment in three steps', async () => {
    const { root } = await mount(solSession(), { solana: { wallets: [signs()], api: solApi() } });
    root.querySelector<HTMLButtonElement>('.wallet-card [data-fid=gas-help]')!.click();
    await flush();
    expect([...root.querySelectorAll('[data-fid=gas-steps] li')].map((l) => l.textContent)).toEqual([
      'You approve one transaction in your wallet.',
      'Prismaport pays the network fee on Solana.',
      'The payment reaches the merchant in that same transaction.',
    ]);
  });

  it('a wallet that refuses to connect says so and keeps Connect', async () => {
    const wallet = signs();
    (wallet.features['standard:connect'] as { connect: () => Promise<never> }).connect = async () => { throw Object.assign(new Error('User rejected the request.'), { code: 4001 }); };
    const { root } = await mount(solSession(), { solana: { wallets: [wallet], api: solApi() } });
    solConnect(root)!.click();
    await flush();
    expect(text(root)).toContain(WALLET_ERROR_COPY.rejected);
    expect(solConnect(root)!.disabled).toBe(false);
  });

  it('busy: the wallet request replaces the ways to pay until it is answered or cancelled', async () => {
    const api = solApi();
    const { root } = await mount(solSession(), { solana: { wallets: [solWallet('Phantom', () => new Promise(() => undefined))], api } });
    await paySol(root);
    expect(root.querySelector('[data-fid=progress] h2')!.textContent).toBe('Confirm in Phantom');
    expect(solButton(root)).toBeNull();
    expect(root.querySelector('[data-fid=method-address]')).toBeNull();
    expect(text(root)).toContain(PROGRESS_COPY.methodLocked('Phantom'));
    expect(api.buildTransaction).toHaveBeenCalledTimes(1);

    // Cancel frees the page, but Phantom was asked to send: Pay waits until it lets go of the request.
    root.querySelector<HTMLButtonElement>('[data-fid=prompt-cancel]')!.click();
    const again = solButton(root)!;
    expect(again.disabled).toBe(true);
    expect(root.querySelector('[data-fid=held-request]')!.textContent).toBe(WALLET_COPY.heldRequest('Phantom'));
    expect(root.querySelector<HTMLButtonElement>('[data-fid=sol-disconnect]')!.disabled).toBe(false);
    expect(root.querySelector('[data-fid=method-address]')).not.toBeNull();
    again.click();
    await flush();
    expect(api.buildTransaction).toHaveBeenCalledTimes(1);
  });

  it('sent: shows the signature, reloads, and keeps the buttons hidden until the payment is detected', async () => {
    const { root, get } = await mount(solSession(), { solana: { wallets: [signs()], api: solApi() } });
    const loadsBefore = get.mock.calls.length;

    await paySol(root);
    expect(solButton(root)).toBeNull();
    expect(text(root)).toContain(PROGRESS_COPY.sentText('Solana'));
    expect(root.querySelector('[data-fid=progress] [aria-label="Copy transaction hash"]')).not.toBeNull();
    expect(root.querySelector('[data-fid=progress] .address')!.getAttribute('title')).toBe(SIG58);
    expect(get.mock.calls.length).toBe(loadsBefore + 1);

    await poll();
    expect(solButton(root)).toBeNull();

    get.mockResolvedValue({ data: solSession({
      status: 'partially_paid', amount_received: '10.00', amount_remaining: '15.00',
      payments: [{ chain: 'solana', asset: 'USDC', amount: '10.000000', status: 'detected', tx_hash: SIG58 }],
    }), serverTime: null });
    await poll();
    expect(root.querySelector('[data-fid=progress]')).toBeNull();
    expect(solButton(root)!.textContent).toBe('Pay 5.00 USDC');
  });

  it('sent survives a reload of the page in the same tab', async () => {
    const first = await mount(solSession(), { solana: { wallets: [signs()], api: solApi() } });
    await paySol(first.root);
    stops.forEach((s) => s());
    stops = [];

    const { root } = await mount(solSession(), { solana: { wallets: [signs()], api: solApi() } });
    expect(solButton(root)).toBeNull();
    expect(text(root)).toContain(PROGRESS_COPY.checking);
  });

  it('after the timeout, sending again needs an explicit confirmation', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    try {
      let t = NOW;
      const { root } = await mount(solSession(), { now: () => t, solana: { wallets: [signs()], api: solApi() } });
      await paySol(root);
      expect(root.querySelector('[data-fid=send-again]')).toBeNull();
      t = NOW + 41_000;
      vi.advanceTimersByTime(1000);
      expect(root.querySelector('[data-elapsed]')!.textContent).toBe('0:41 elapsed');

      t = NOW + SEND_AGAIN_AFTER_MS;
      vi.advanceTimersByTime(1000);
      expect(root.querySelector('[data-fid=progress] h2')!.textContent).toBe(PROGRESS_COPY.slow);
      expect(text(root)).toContain(PROGRESS_COPY.notDetected);
      root.querySelector<HTMLButtonElement>('[data-fid=send-again]')!.click();
      expect(text(root)).toContain(PROGRESS_COPY.confirmSendAgain);
      expect(solButton(root)).toBeNull();

      root.querySelector<HTMLButtonElement>('[data-fid=send-again-cancel]')!.click();
      expect(text(root)).not.toContain(PROGRESS_COPY.confirmSendAgain);
      expect(solButton(root)).toBeNull();

      root.querySelector<HTMLButtonElement>('[data-fid=send-again]')!.click();
      root.querySelector<HTMLButtonElement>('[data-fid=send-again-confirm]')!.click();
      expect(solButton(root)!.textContent).toBe('Pay 25.00 USDC');
      expect(root.querySelector('[data-fid=progress]')).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it('declined in the wallet: says so and offers the button again', async () => {
    const reject = async (): Promise<never> => { throw Object.assign(new Error('User rejected the request.'), { code: 4001 }); };
    const { root } = await mount(solSession(), { solana: { wallets: [solWallet('Phantom', reject)], api: solApi() } });
    await paySol(root);
    expect(text(root)).toContain(WALLET_ERROR_COPY.rejected);
    expect(solButton(root)!.disabled).toBe(false);
  });

  it('a wallet error of unknown outcome never claims nothing was sent', async () => {
    const fail = async (): Promise<never> => { throw new Error('Request timed out'); };
    const { root } = await mount(solSession(), { solana: { wallets: [solWallet('Phantom', fail)], api: solApi() } });
    await paySol(root);
    expect(text(root)).toContain(SOLANA_COPY.unknown);
    expect(text(root)).not.toContain('Nothing was sent');
  });

  it('no transaction service: points to the address', async () => {
    const { root } = await mount(solSession(), { solana: { wallets: [signs()] } });
    await paySol(root);
    expect(text(root)).toContain(SOLANA_COPY.unavailable);
    expect(solButton(root)).not.toBeNull();
  });

  it('a refusal from the API shows its copy', async () => {
    const api = { buildTransaction: vi.fn(async (): Promise<never> => { throw new ApiError(429, 'rate_limited', 'slow down', 12); }) };
    const { root } = await mount(solSession(), { solana: { wallets: [signs()], api } });
    await paySol(root);
    expect(text(root)).toContain('Too many attempts from this connection. Try again in 12 seconds.');
    expect(solButton(root)).not.toBeNull();
  });
});

describe('pay page open-amount session (customer address)', () => {
  const anyAmount = (over: Partial<Session> = {}) => session({
    amount: null, open_amount: true, amount_remaining: null, customer_address_id: 'caddr_01JBTOPUP', product: { name: 'Wallet top-up' },
    pay_to: [payTo({ amount: undefined, signed_payment: false }), payTo({ chain: 'solana', address: SOL_ADDRESS, amount: undefined, signed_payment: false })],
    ...over,
  });

  it('asks for any amount: no amount to match, no wallet buttons, an amountless payment request', async () => {
    const provider = { request: vi.fn(async () => [PAYER]) } as unknown as EIP1193Provider;
    const { root } = await mount(anyAmount(), { provider });
    expect(root.querySelector('.amount-hero')!.textContent).toBe('Any amount of USDC');
    expect(root.querySelector('h2.heading')!.textContent).toBe('Send any amount of USDC on Base');
    expect(text(root)).toContain(COPY.anyAmountDetail);
    expect(root.querySelector('[data-fid="connect"]')).toBeNull();
    expect(root.querySelector('[data-fid="pay"]')).toBeNull();
    expect(root.querySelector('[data-fid="copy-amount"]')).toBeNull();
    expect(root.querySelector('[data-fid="any-amount"]')!.textContent).toBe('Any amount of USDC');
    expect(text(root)).not.toContain('Exact on-chain amount');
    expect(text(root)).not.toMatch(/0\.00 USDC|null|undefined/);
    expect(decodeSvg(root.querySelector('svg.qr')!)).toBe(`ethereum:${BASE_USDC}@8453/transfer?address=${EVM_ADDRESS}`);
  });

  it('paid: says how much arrived', async () => {
    const { root } = await mount(anyAmount({ status: 'paid', amount_received: '40.00',
      payments: [{ chain: 'base', asset: 'USDC', amount: '40.000000', status: 'confirmed', tx_hash: '0xabc' }] }));
    expect(text(root)).toContain(COPY.paid);
    expect(text(root)).toContain('You sent 40.00 USDC.');
    expect(root.querySelector('svg.qr')).toBeNull();
  });

  it('closed: the lock panel sends the payer back to the address page for a new payment', async () => {
    const { root } = await mount(anyAmount({ expires_at: '2026-10-23T17:00:00Z', pay_to: [{ chain: 'base', asset: 'USDC', signed_payment: false }] }));
    expect(root.querySelector('[data-fid="lock-panel"]')).not.toBeNull();
    expect(text(root)).toContain(COPY.expiredAnyAmount);
    expect(root.querySelector('[data-fid="start-new"]')!.getAttribute('href')).toBe('/a/caddr_01JBTOPUP');
    expect(root.querySelector('svg.qr')).toBeNull();
  });
});

describe('pay page wallet request: cancel and late answers', () => {
  const SIG = '0x' + '11'.repeat(64) + '1b';
  const transfer = (over: Partial<Session> = {}) => session({ pay_to: [payTo({ signed_payment: false })], ...over });

  /** A wallet that holds `method` until `release` (it answers `answer`) or `reject` (the payer declines it in the wallet). */
  function holding(method: 'eth_sendTransaction' | 'eth_signTypedData_v4', answer: string) {
    let release!: () => void;
    let reject!: (e: Error) => void;
    const held = new Promise<void>((res, rej) => { release = res; reject = rej; });
    held.catch(() => undefined);
    const provider = {
      request: vi.fn(async ({ method: m }: { method: string }) => {
        if (m === 'eth_requestAccounts' || m === 'eth_accounts') return [PAYER];
        if (m === 'eth_chainId') return '0x2105';
        if (m === 'eth_estimateGas') return '0x5208';
        if (m === method) {
          await held;
          return answer;
        }
        throw new Error(m);
      }),
    } as unknown as EIP1193Provider;
    return { provider, release: () => release(), reject: () => reject(Object.assign(new Error('User rejected the request.'), { code: 4001 })) };
  }

  async function connectAndPay(root: HTMLElement) {
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.click();
    await flush();
  }

  const cancel = (root: HTMLElement) => root.querySelector<HTMLButtonElement>('[data-fid=prompt-cancel]')!.click();
  /** The wallet was asked `method`: the request is open in it, not still being prepared. */
  const asked = (provider: EIP1193Provider, method: string) => times(provider, method) > 0;
  /** How many times the wallet was asked `method`. */
  const times = (provider: EIP1193Provider, method: string) =>
    (provider.request as unknown as { mock: { calls: [{ method: string }][] } }).mock.calls.filter(([c]) => c.method === method).length;
  const storedSent = () => JSON.parse(sessionStorage.getItem('pp:sent:ses_01JBTEST') ?? 'null');
  const storedHold = () => JSON.parse(sessionStorage.getItem('pp:held:ses_01JBTEST') ?? 'null');
  const holdFromBefore = (hold: { wallet: string; key: string; at: number }) => sessionStorage.setItem('pp:held:ses_01JBTEST', JSON.stringify(hold));
  const phase = (root: HTMLElement) => root.querySelector('[data-fid=progress]')?.getAttribute('data-phase') ?? null;
  /** A wallet that signs at once. */
  const signer = () => ({
    request: vi.fn(async ({ method }: { method: string }) => {
      if (method === 'eth_requestAccounts') return [PAYER];
      if (method === 'eth_chainId') return '0x2105';
      if (method === 'eth_signTypedData_v4') return SIG;
      throw new Error(method);
    }),
  } as unknown as EIP1193Provider);
  const EXPIRES = Date.parse('2026-10-23T19:00:00Z');

  it('while the wallet asks: what to approve, Cancel, and the address out of reach', async () => {
    const { provider } = holding('eth_sendTransaction', TX);
    const { root } = await mount(transfer(), { provider });
    await connectAndPay(root);
    const confirm = root.querySelector<HTMLElement>('[data-fid=progress]')!;
    expect(confirm.getAttribute('data-phase')).toBe('confirm');
    expect(text(confirm)).toContain("Don't see the request? Open Test wallet. If you close it there, nothing is sent.");
    expect(root.querySelector('[data-fid=prompt-cancel]')!.textContent).toBe('Cancel this request');
    expect(root.querySelector('[data-fid=prompt-back]')).toBeNull();
    expect(root.querySelector('[data-fid=method-locked]')!.textContent).toBe('To send from another wallet or an exchange, cancel the Test wallet request first.');
    expect(root.querySelector('[data-fid=method-address]')).toBeNull();
    expect(root.querySelector('.deposit')).toBeNull();
  });

  it('Cancel abandons the request: Disconnect and the address come back, Pay waits for the wallet to let go of the transfer', async () => {
    const { provider, reject } = holding('eth_sendTransaction', TX);
    const { root } = await mount(transfer(), { provider });
    await connectAndPay(root);
    expect(asked(provider, 'eth_sendTransaction')).toBe(true);
    cancel(root);
    expect(root.querySelector('[data-fid=progress]')).toBeNull();
    expect(root.querySelector<HTMLButtonElement>('[data-fid=disconnect]')!.disabled).toBe(false);
    expect(root.querySelector('[data-fid=method-address]')).not.toBeNull();
    // The wallet still holds the first transfer: a second request could have both approved.
    const pay = root.querySelector<HTMLButtonElement>('[data-fid=pay]')!;
    expect(pay.textContent).toBe('Send 25.00 USDC');
    expect(pay.disabled).toBe(true);
    expect(root.querySelector('[data-fid=held-request]')!.textContent).toBe('Reject the earlier request in Test wallet first, or wait for it to finish.');
    pay.click();
    await flush();
    expect(times(provider, 'eth_sendTransaction')).toBe(1);
    expect(storedSent()).toBeNull();

    // Rejected in the wallet: nothing was sent, and paying is offered again without an error.
    reject();
    await flush();
    expect(root.querySelector('[data-fid=held-request]')).toBeNull();
    expect(root.querySelector('.wallet-card .note')).toBeNull();
    expect(root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(false);
    expect(storedSent()).toBeNull();
  });

  it('a cancelled signature request holds nothing: a late signature is never submitted, so Sign stays available', async () => {
    const { provider } = holding('eth_signTypedData_v4', SIG);
    const { root, api } = await mount(session(), { provider });
    vi.spyOn(api, 'typedData').mockResolvedValue({ data: typedData(), serverTime: null });
    await connectAndPay(root);
    expect(storedHold()).toBeNull();
    cancel(root);
    expect(root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(false);
    expect(root.querySelector('[data-fid=held-request]')).toBeNull();
    expect(storedHold()).toBeNull();
  });

  it('a transfer the wallet still sends after Cancel moved the money: recorded and followed like any other', async () => {
    const { provider, release } = holding('eth_sendTransaction', TX);
    const { root } = await mount(transfer(), { provider });
    await connectAndPay(root);
    expect(asked(provider, 'eth_sendTransaction')).toBe(true);
    cancel(root);
    release();
    await flush();
    const sent = root.querySelector('[data-fid=progress]')!;
    expect(sent.getAttribute('data-phase')).toBe('sent');
    expect(sent.querySelector('.tx-link')!.getAttribute('href')).toContain(TX);
    expect(root.querySelector('[data-fid=pay]')).toBeNull();
    expect(root.querySelector('svg.qr')).toBeNull();
    expect(storedSent()).toMatchObject({ key: 'base:USDC', signature: TX });
  });

  it('a signature that comes back after Cancel is dropped: never submitted, nothing recorded', async () => {
    const { provider, release } = holding('eth_signTypedData_v4', SIG);
    const { root, api } = await mount(session(), { provider });
    vi.spyOn(api, 'typedData').mockResolvedValue({ data: typedData(), serverTime: null });
    const submit = vi.spyOn(api, 'submit').mockResolvedValue({ data: { id: 'txj_01', session_id: 'ses_01JBTEST', status: 'pending' }, serverTime: null });
    await connectAndPay(root);
    expect(root.querySelector('[data-fid=progress]')!.getAttribute('data-phase')).toBe('confirm');
    expect(asked(provider, 'eth_signTypedData_v4')).toBe(true);
    cancel(root);
    release();
    await flush();
    expect(submit).not.toHaveBeenCalled();
    expect(root.querySelector('[data-fid=progress]')).toBeNull();
    expect(root.querySelector('.wallet-card .note')).toBeNull();
    expect(root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(false);
    expect(storedSent()).toBeNull();
  });

  it('once the wallet signed, the submit runs: no approval asked, no Cancel, no timeout, then followed', async () => {
    vi.useFakeTimers();
    try {
      const { root, api } = await mount(session(), { provider: signer() });
      vi.spyOn(api, 'typedData').mockResolvedValue({ data: typedData(), serverTime: null });
      let accept!: () => void;
      const accepted = new Promise<void>((r) => { accept = r; });
      vi.spyOn(api, 'submit').mockImplementation(async () => {
        await accepted;
        return { data: { id: 'txj_01', session_id: 'ses_01JBTEST', status: 'pending' }, serverTime: null };
      });
      await connectAndPay(root);
      const panel = root.querySelector<HTMLElement>('[data-fid=progress]')!;
      expect(panel.getAttribute('data-phase')).toBe('submitting');
      expect(panel.querySelector('h2')!.textContent).toBe(PROGRESS_COPY.sending);
      expect(root.querySelector('[data-fid=submitting]')!.textContent).toBe(COPY.signedSending);
      expect(text(panel)).not.toContain('Approve the signature request');
      expect(root.querySelector('[data-fid=prompt-cancel]')).toBeNull();
      expect(root.querySelector('.deposit')).toBeNull();

      // The wallet answered: the prompt timeout no longer abandons anything.
      await vi.advanceTimersByTimeAsync(PROMPT_TIMEOUT_MS + 1000);
      expect(phase(root)).toBe('submitting');

      accept();
      await flush();
      expect(phase(root)).toBe('sent');
      expect(storedSent()).toMatchObject({ key: 'base:USDC', signature: '', job: 'txj_01' });
    } finally {
      vi.useRealTimers();
    }
  });

  it('Solana: a transaction the wallet still sends after Cancel is recorded and followed', async () => {
    let release!: () => void;
    const held = new Promise<void>((r) => { release = r; });
    const wallet = solWallet('Phantom', async () => {
      await held;
      return [{ signature: SIGNATURE }];
    });
    const { root } = await mount(solSession(), { solana: { wallets: [wallet], api: solApi() } });
    await paySol(root);
    cancel(root);
    expect(solButton(root)!.disabled).toBe(true);
    release();
    await flush();
    expect(root.querySelector('[data-fid=progress]')!.getAttribute('data-phase')).toBe('sent');
    expect(solButton(root)).toBeNull();
    expect(storedSent()).toMatchObject({ key: 'solana:USDC', signature: base58(SIGNATURE) });
  });

  it('a request left unanswered is abandoned after the timeout, as with Cancel', async () => {
    expect(PROMPT_TIMEOUT_MS).toBe(120_000);
    vi.useFakeTimers();
    try {
      const { provider } = holding('eth_signTypedData_v4', SIG);
      const { root, api } = await mount(session(), { provider });
      vi.spyOn(api, 'typedData').mockResolvedValue({ data: typedData(), serverTime: null });
      await connectAndPay(root);
      await vi.advanceTimersByTimeAsync(PROMPT_TIMEOUT_MS - 10);
      expect(phase(root)).toBe('confirm');
      await vi.advanceTimersByTimeAsync(10);
      expect(root.querySelector('[data-fid=progress]')).toBeNull();
      expect(root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a transfer left unanswered is abandoned after the timeout too, and held until the wallet lets go of it', async () => {
    vi.useFakeTimers();
    try {
      const { provider } = holding('eth_sendTransaction', TX);
      const { root } = await mount(transfer(), { provider });
      await connectAndPay(root);
      expect(asked(provider, 'eth_sendTransaction')).toBe(true);
      await vi.advanceTimersByTimeAsync(PROMPT_TIMEOUT_MS);
      expect(root.querySelector('[data-fid=progress]')).toBeNull();
      expect(root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(true);
      expect(root.querySelector('[data-fid=held-request]')).not.toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it('an abandoned request that sends while a newer one is open: the payment on its way wins and the newer one is dropped', async () => {
    vi.useFakeTimers();
    let t = NOW;
    const answers: { send: () => void; refuse: () => void }[] = [];
    const wallet = solWallet('Phantom', () => new Promise((res, rej) => {
      answers.push({ send: () => res([{ signature: SIGNATURE }]), refuse: () => rej(Object.assign(new Error('User rejected the request.'), { code: 4001 })) });
    }));
    const { root } = await mount(solSession(), { now: () => t, solana: { wallets: [wallet], api: solApi() } });
    await paySol(root);
    cancel(root);
    // Held until its transaction can no longer land; then a new request can start.
    t += SOLANA_REQUEST_LIFETIME_MS;
    await vi.advanceTimersByTimeAsync(1000);
    solButton(root)!.click();
    await flush();
    expect(answers).toHaveLength(2);
    expect(phase(root)).toBe('confirm');

    // The wallet still answers the first request with a signature: the page follows it, and the
    // newer request must not be approved as well.
    answers[0]!.send();
    await flush();
    expect(phase(root)).toBe('sent');
    expect(root.querySelector('[data-fid=prompt-cancel]')).toBeNull();
    expect(root.querySelector('[data-fid=other-request]')!.textContent).toBe('Reject the other request in Phantom. This payment is already on its way.');
    expect(solButton(root)).toBeNull();
    expect(storedSent()).toMatchObject({ key: 'solana:USDC', signature: base58(SIGNATURE) });

    // The payer rejects the other request: the note goes, the payment is still followed.
    answers[1]!.refuse();
    await flush();
    expect(root.querySelector('[data-fid=other-request]')).toBeNull();
    expect(phase(root)).toBe('sent');
    expect(root.querySelector('.wallet-card .note')).toBeNull();
    expect(storedHold()).toBeNull();
  });

  it('a payment that never reached Prismaport says it was not sent, and the button stays (no "Retrying")', async () => {
    const provider = {
      request: vi.fn(async ({ method }: { method: string }) => {
        if (method === 'eth_requestAccounts') return [PAYER];
        if (method === 'eth_chainId') return '0x2105';
        throw new Error(method);
      }),
    } as unknown as EIP1193Provider;
    const { root, api } = await mount(session(), { provider });
    vi.spyOn(api, 'typedData').mockRejectedValue(new ApiError(0, 'network_error', 'The connection failed.'));
    await connectAndPay(root);
    expect(text(root)).toContain('We could not reach Prismaport, so this payment was not sent. Check your connection and sign again.');
    expect(text(root)).not.toContain(COPY.offline);
    expect(root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(false);
  });

  const WAYS_TO_PAY = ['.deposit', 'svg.qr', '[data-fid=pay]', '[data-fid=connect]', '[data-fid^="method-"]', '[data-fid=pay-another]'];
  const pendingJob = { id: 'txj_01', chain: 'base', asset: 'USDC', status: 'pending' as const, outcome: null, tx_hash: null };
  const timedOut = () => new ApiError(0, 'network_error', 'The connection failed.');

  it('a submit that timed out after reaching Prismaport: its signed payment is followed, with no address and no second signature', async () => {
    const { root, api, get } = await mount(session(), { provider: signer() });
    vi.spyOn(api, 'typedData').mockResolvedValue({ data: typedData(), serverTime: null });
    vi.spyOn(api, 'submit').mockRejectedValue(timedOut());
    get.mockResolvedValue({ data: session({ signed_payments: [pendingJob] }), serverTime: null });
    await connectAndPay(root);
    expect(phase(root)).toBe('sent');
    expect(text(root)).toContain(PROGRESS_COPY.signedText('Base'));
    expect(text(root)).not.toContain(COPY.notSent);
    for (const sel of WAYS_TO_PAY) expect(root.querySelector(sel), sel).toBeNull();
    expect(storedSent()).toMatchObject({ key: 'base:USDC', signature: '', job: 'txj_01' });
  });

  it('a submit that timed out with no signed payment showing: checking first, "not sent" only after the window', async () => {
    let t = NOW;
    const { root, api, get } = await mount(session(), { provider: signer(), now: () => t });
    vi.spyOn(api, 'typedData').mockResolvedValue({ data: typedData(), serverTime: null });
    const submit = vi.spyOn(api, 'submit').mockRejectedValue(timedOut());
    await connectAndPay(root);
    expect(submit).toHaveBeenCalledTimes(1);
    expect(phase(root)).toBe('unknown');
    const line = root.querySelector('[data-fid=submit-unknown]')!;
    expect(line.getAttribute('role')).toBe('status');
    expect(line.textContent).toBe('We could not confirm that your payment reached Prismaport. Do not pay again yet; this page keeps checking.');
    expect(text(root)).not.toContain(COPY.notSent);
    for (const sel of WAYS_TO_PAY) expect(root.querySelector(sel), sel).toBeNull();

    // Answers inside the window, and a failed load, conclude nothing.
    const n = get.mock.calls.length;
    t = NOW + 10_000;
    await poll();
    get.mockRejectedValueOnce(timedOut());
    t = NOW + SUBMIT_CHECK_MS;
    await poll();
    expect(get.mock.calls.length).toBe(n + 2);
    expect(phase(root)).toBe('unknown');

    // Past the window, the session answers again with no signed payment: not sent, pay again.
    await poll();
    expect(root.querySelector('[data-fid=progress]')).toBeNull();
    expect(text(root)).toContain(COPY.notSent);
    expect(root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(false);
    expect(storedSent()).toBeNull();
  });

  it('a reload with a signed payment Prismaport is still sending, before expiry: followed, no way to pay next to it', async () => {
    const { root } = await mount(session({ signed_payments: [pendingJob] }), { provider: signer() });
    expect(phase(root)).toBe('sent');
    expect(text(root)).toContain(PROGRESS_COPY.signedText('Base'));
    for (const sel of WAYS_TO_PAY) expect(root.querySelector(sel), sel).toBeNull();
  });

  it('a wallet request open at expiry holds off the lock; approved late, the page follows it and keeps polling', async () => {
    vi.useFakeTimers();
    try {
      let t = EXPIRES - 5000;
      const { provider, release } = holding('eth_sendTransaction', TX);
      const { root, get } = await mount(transfer({ link_id: 'plink_1', link_active: true }), { provider, now: () => t, pollMs: 3000 });
      await connectAndPay(root);
      expect(phase(root)).toBe('confirm');
      t = EXPIRES + 1000;
      await vi.advanceTimersByTimeAsync(1000);
      expect(root.querySelector('[data-fid=lock-panel]')).toBeNull();
      expect(root.querySelector('[data-fid=start-new]')).toBeNull();
      expect(phase(root)).toBe('confirm');
      let n = get.mock.calls.length;
      await vi.advanceTimersByTimeAsync(3000);
      expect(get.mock.calls.length).toBeGreaterThan(n);

      release();
      await flush();
      expect(phase(root)).toBe('grace');
      expect(root.querySelector('[data-fid=lock-panel]')).toBeNull();
      n = get.mock.calls.length;
      await vi.advanceTimersByTimeAsync(3000);
      expect(get.mock.calls.length).toBeGreaterThan(n);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a cancelled transfer the wallet still holds at expiry, sent late: the page follows it and keeps polling', async () => {
    vi.useFakeTimers();
    try {
      let t = EXPIRES - 5000;
      const { provider, release } = holding('eth_sendTransaction', TX);
      const { root, get } = await mount(transfer(), { provider, now: () => t, pollMs: 3000 });
      await connectAndPay(root);
      cancel(root);
      t = EXPIRES + 1000;
      await vi.advanceTimersByTimeAsync(1000);
      expect(phase(root)).toBe('held');
      expect(root.querySelector('[data-fid=lock-panel]')).toBeNull();
      let n = get.mock.calls.length;
      await vi.advanceTimersByTimeAsync(3000);
      expect(get.mock.calls.length).toBeGreaterThan(n);

      release();
      await flush();
      expect(phase(root)).toBe('grace');
      expect(root.querySelector('[data-fid=lock-panel]')).toBeNull();
      expect(storedSent()).toMatchObject({ key: 'base:USDC', signature: TX });
      n = get.mock.calls.length;
      await vi.advanceTimersByTimeAsync(3000);
      expect(get.mock.calls.length).toBeGreaterThan(n);
    } finally {
      vi.useRealTimers();
    }
  });

  it('while a cancelled transfer is held at expiry: no lock and no "Start a new payment"; rejected, the lock comes back', async () => {
    vi.useFakeTimers();
    try {
      let t = EXPIRES - 5000;
      const { provider, reject } = holding('eth_sendTransaction', TX);
      const { root, get } = await mount(transfer({ link_id: 'plink_1', link_active: true }), { provider, now: () => t, pollMs: 3000 });
      await connectAndPay(root);
      cancel(root);
      t = EXPIRES + 1000;
      await vi.advanceTimersByTimeAsync(1000);
      expect(root.querySelector('[data-fid=lock-panel]')).toBeNull();
      expect(root.querySelector('[data-fid=start-new]')).toBeNull();
      expect(text(root)).not.toContain(COPY.closed);
      for (const sel of WAYS_TO_PAY) expect(root.querySelector(sel), sel).toBeNull();
      expect(root.querySelector('[data-fid=grace-clock]')).not.toBeNull();
      expect(root.querySelector('[data-fid=progress] h2')!.textContent).toBe('Your Test wallet request is still open');
      expect(root.querySelector('[data-fid=held-at-expiry]')!.textContent).toBe(
        'Reject it in Test wallet before you start a new payment, or both could be paid. If you approve it, this page follows it for 10 minutes.');

      // Rejected in the wallet: nothing can be sent any more, so the session locks as usual.
      reject();
      await flush();
      expect(root.querySelector('[data-fid=lock-panel]')).not.toBeNull();
      expect(root.querySelector('[data-fid=start-new]')).not.toBeNull();
      const n = get.mock.calls.length;
      await vi.advanceTimersByTimeAsync(10_000);
      expect(get.mock.calls.length).toBe(n);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a held transfer is kept across a reload: Pay stays blocked until the payer says they rejected it', async () => {
    const first = holding('eth_sendTransaction', TX);
    const { root } = await mount(transfer(), { provider: first.provider });
    await connectAndPay(root);
    cancel(root);
    expect(storedHold()).toEqual({ wallet: 'Test wallet', key: 'base:USDC', at: NOW });

    // The reload: the page that saw the request is gone, the wallet may still hold it.
    stops.pop()!();
    const { root: again } = await mount(transfer(), { provider: provider() });
    again.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    const pay = again.querySelector<HTMLButtonElement>('[data-fid=pay]')!;
    expect(pay.disabled).toBe(true);
    expect(again.querySelector('[data-fid=held-request]')!.textContent).toBe(
      'A Test wallet request from before may still be open. Reject it in Test wallet first, or wait for it to finish.');
    const release = again.querySelector<HTMLButtonElement>('[data-fid=held-release]')!;
    expect(release.textContent).toBe('I rejected it');
    expect(release.classList.contains('btn-small')).toBe(true);
    expect(release.classList.contains('btn-primary')).toBe(false);

    // "I rejected it" lets go of it: Pay is back, with focus, and the marker is gone.
    release.click();
    const payAgain = again.querySelector<HTMLButtonElement>('[data-fid=pay]')!;
    expect(payAgain.disabled).toBe(false);
    expect(document.activeElement).toBe(payAgain);
    expect(again.querySelector('[data-fid=held-request]')).toBeNull();
    expect(again.querySelector('[data-fid=held-release]')).toBeNull();
    expect(storedHold()).toBeNull();
  });

  it('the held marker goes when the wallet lets go of the request: rejected, or sent and followed', async () => {
    const rejected = holding('eth_sendTransaction', TX);
    const { root } = await mount(transfer(), { provider: rejected.provider });
    await connectAndPay(root);
    cancel(root);
    expect(storedHold()).not.toBeNull();
    rejected.reject();
    await flush();
    expect(storedHold()).toBeNull();

    stops.pop()!();
    const sent = holding('eth_sendTransaction', TX);
    const { root: again } = await mount(transfer(), { provider: sent.provider });
    await connectAndPay(again);
    cancel(again);
    expect(storedHold()).not.toBeNull();
    sent.release();
    await flush();
    expect(phase(again)).toBe('sent');
    expect(storedSent()).toMatchObject({ key: 'base:USDC', signature: TX });
    expect(storedHold()).toBeNull();

    // Answered while still open on the page: the marker written at the request goes with it.
    stops.pop()!();
    sessionStorage.clear();
    const open = holding('eth_sendTransaction', TX);
    const { root: third } = await mount(transfer(), { provider: open.provider });
    await connectAndPay(third);
    expect(phase(third)).toBe('confirm');
    expect(storedHold()).toEqual({ wallet: 'Test wallet', key: 'base:USDC', at: NOW });
    open.release();
    await flush();
    expect(phase(third)).toBe('sent');
    expect(storedHold()).toBeNull();
  });

  it('a reload while the transfer is still open in the wallet: the hold is restored, and "I rejected it" releases it', async () => {
    const first = holding('eth_sendTransaction', TX);
    const { root } = await mount(transfer(), { provider: first.provider });
    await connectAndPay(root);
    expect(asked(first.provider, 'eth_sendTransaction')).toBe(true);
    expect(phase(root)).toBe('confirm');

    // No Cancel and no timeout: the payer reloads while the wallet still shows the request.
    stops.pop()!();
    const { root: again } = await mount(transfer(), { provider: provider() });
    again.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(again.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(true);
    expect(again.querySelector('[data-fid=held-request]')!.textContent).toBe(WALLET_COPY.restoredRequest('Test wallet'));
    again.querySelector<HTMLButtonElement>('[data-fid=held-release]')!.click();
    expect(again.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(false);
    expect(storedHold()).toBeNull();
  });

  it('a reload after Phantom was asked to send: an EVM payment on another option stays blocked', async () => {
    const wallet = solWallet('Phantom', () => new Promise(() => undefined));
    const { root } = await mount(solSession(), { solana: { wallets: [wallet], api: solApi() } });
    await paySol(root);
    expect(phase(root)).toBe('confirm');
    expect(storedHold()).toEqual({ wallet: 'Phantom', key: 'solana:USDC', at: NOW });

    stops.pop()!();
    const { root: again } = await mount(solSession(), { provider: provider(), solana: { wallets: [signs()], api: solApi() } });
    again.querySelector<HTMLInputElement>('input[value="base:USDC"]')!.click();
    await flush();
    again.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(again.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(true);
    expect(again.querySelector('[data-fid=held-request]')!.textContent).toBe(
      'A Phantom request from before may still be open. Reject it in Phantom first, or wait for it to finish.');
    again.querySelector<HTMLButtonElement>('[data-fid=held-release]')!.click();
    expect(again.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(false);
    expect(storedHold()).toBeNull();
  });

  it('"I rejected it" on a request this page abandoned frees Pay; a transfer the wallet still sends is recorded', async () => {
    const { provider: wallet, release } = holding('eth_sendTransaction', TX);
    const { root } = await mount(transfer(), { provider: wallet });
    await connectAndPay(root);
    cancel(root);
    expect(root.querySelector('[data-fid=held-request]')!.textContent).toBe(WALLET_COPY.heldRequest('Test wallet'));
    const button = root.querySelector<HTMLButtonElement>('[data-fid=held-release]')!;
    expect(button.textContent).toBe('I rejected it');
    button.click();
    expect(root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(false);
    expect(root.querySelector('[data-fid=held-request]')).toBeNull();
    expect(storedHold()).toBeNull();

    // The wallet sends it after all: the payment is followed, nothing offers a second one.
    release();
    await flush();
    expect(phase(root)).toBe('sent');
    expect(storedSent()).toMatchObject({ key: 'base:USDC', signature: TX });
    expect(root.querySelector('[data-fid=pay]')).toBeNull();
  });

  it('a held marker past expiry and the grace window is dropped on load, not restored', async () => {
    holdFromBefore({ wallet: 'Test wallet', key: 'base:USDC', at: EXPIRES - 60_000 });
    const { root } = await mount(transfer({ link_id: 'plink_1', link_active: true }), { provider: provider(), now: EXPIRES + SENT_GRACE_MS + 1 });
    expect(root.querySelector('[data-fid=lock-panel]')).not.toBeNull();
    expect(root.querySelector('[data-fid=start-new]')).not.toBeNull();
    expect(storedHold()).toBeNull();
  });

  it('a held marker restored at expiry counts as in flight; it goes when the grace window runs out', async () => {
    vi.useFakeTimers();
    holdFromBefore({ wallet: 'Test wallet', key: 'base:USDC', at: EXPIRES - 60_000 });
    let t = EXPIRES + 1000;
    const { root, get } = await mount(transfer({ link_id: 'plink_1', link_active: true }), { provider: provider(), now: () => t, pollMs: 3000 });
    expect(phase(root)).toBe('held');
    expect(root.querySelector('[data-fid=lock-panel]')).toBeNull();
    expect(root.querySelector('[data-fid=start-new]')).toBeNull();
    for (const sel of WAYS_TO_PAY) expect(root.querySelector(sel), sel).toBeNull();
    expect(root.querySelector('[data-fid=progress] h2')!.textContent).toBe('Your Test wallet request is still open');
    expect(root.querySelector('[data-fid=held-at-expiry]')!.textContent).toBe(PROGRESS_COPY.heldText('Test wallet'));
    expect(root.querySelector('[data-fid=progress] [data-fid=held-release]')).not.toBeNull();
    const n = get.mock.calls.length;
    await vi.advanceTimersByTimeAsync(3000);
    expect(get.mock.calls.length).toBeGreaterThan(n);

    t = EXPIRES + SENT_GRACE_MS + 1;
    await vi.advanceTimersByTimeAsync(1000);
    expect(root.querySelector('[data-fid=lock-panel]')).not.toBeNull();
    expect(phase(root)).toBeNull();
    expect(storedHold()).toBeNull();
  });

  it('"I rejected it" on a held marker at expiry: the lock and "Start a new payment" come back', async () => {
    holdFromBefore({ wallet: 'Test wallet', key: 'base:USDC', at: EXPIRES - 60_000 });
    const { root } = await mount(transfer({ link_id: 'plink_1', link_active: true }), { provider: provider(), now: EXPIRES + 1000 });
    expect(phase(root)).toBe('held');
    root.querySelector<HTMLButtonElement>('[data-fid=held-release]')!.click();
    expect(root.querySelector('[data-fid=lock-panel]')).not.toBeNull();
    expect(root.querySelector('[data-fid=start-new]')).not.toBeNull();
    expect(storedHold()).toBeNull();
  });

  it('an abandoned Solana request holds every wallet payment, an EVM one on another option too, until the wallet settles it', async () => {
    expect(SOLANA_REQUEST_LIFETIME_MS).toBe(150_000);
    let refuse!: () => void;
    const wallet = solWallet('Phantom', () => new Promise((_, rej) => {
      refuse = () => rej(Object.assign(new Error('User rejected the request.'), { code: 4001 }));
    }));
    const { root } = await mount(solSession(), { provider: provider(), solana: { wallets: [wallet], api: solApi() } });
    await paySol(root);
    cancel(root);
    expect(solButton(root)!.disabled).toBe(true);
    expect(root.querySelector('[data-fid=held-request]')!.textContent).toBe('Reject the earlier request in Phantom first, or wait for it to finish.');
    expect(storedHold()).toEqual({ wallet: 'Phantom', key: 'solana:USDC', at: NOW });

    // Another option and another wallet: the Phantom request and this payment could both land.
    root.querySelector<HTMLInputElement>('input[value="base:USDC"]')!.click();
    await flush();
    root.querySelector<HTMLButtonElement>('[data-fid=connect]')!.click();
    await flush();
    expect(root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(true);
    expect(root.querySelector('[data-fid=held-request]')!.textContent).toBe('Reject the earlier request in Phantom first, or wait for it to finish.');

    // Rejected in Phantom: nothing can land any more, so paying is offered again.
    refuse();
    await flush();
    expect(root.querySelector<HTMLButtonElement>('[data-fid=pay]')!.disabled).toBe(false);
    expect(root.querySelector('[data-fid=held-request]')).toBeNull();
    expect(root.querySelector('.wallet-card .note')).toBeNull();
    expect(storedHold()).toBeNull();
  });

  it('an abandoned Solana request the wallet never answers is held only until its transaction can no longer land', async () => {
    vi.useFakeTimers();
    let t = NOW;
    const api = solApi();
    const { root } = await mount(solSession(), { now: () => t, solana: { wallets: [solWallet('Phantom', () => new Promise(() => undefined))], api } });
    await paySol(root);
    // Asked when Pay was pressed; abandoned later, the blockhash still counts from the ask.
    t += 30_000;
    cancel(root);
    t = NOW + SOLANA_REQUEST_LIFETIME_MS - 1000;
    await vi.advanceTimersByTimeAsync(1000);
    expect(solButton(root)!.disabled).toBe(true);
    expect(storedHold()).toEqual({ wallet: 'Phantom', key: 'solana:USDC', at: NOW });
    t = NOW + SOLANA_REQUEST_LIFETIME_MS;
    await vi.advanceTimersByTimeAsync(1000);
    expect(solButton(root)!.disabled).toBe(false);
    expect(root.querySelector('[data-fid=held-request]')).toBeNull();
    expect(storedHold()).toBeNull();
    solButton(root)!.click();
    await flush();
    expect(api.buildTransaction).toHaveBeenCalledTimes(2);
  });

  it('a Solana request abandoned before the wallet was asked to send holds nothing', async () => {
    const api = { buildTransaction: vi.fn(() => new Promise<{ transaction: string }>(() => undefined)) };
    const send = vi.fn(async () => [{ signature: SIGNATURE }]);
    const { root } = await mount(solSession(), { solana: { wallets: [solWallet('Phantom', send)], api } });
    await paySol(root);
    expect(phase(root)).toBe('confirm');
    expect(storedHold()).toBeNull();
    cancel(root);
    expect(solButton(root)!.disabled).toBe(false);
    expect(root.querySelector('[data-fid=held-request]')).toBeNull();
    expect(storedHold()).toBeNull();
    expect(send).not.toHaveBeenCalled();
  });

  it('a held Solana marker from before a reload holds until its transaction can no longer land', async () => {
    vi.useFakeTimers();
    let t = NOW;
    holdFromBefore({ wallet: 'Phantom', key: 'solana:USDC', at: NOW - 100_000 });
    const { root } = await mount(solSession(), { now: () => t, solana: { wallets: [signs()], api: solApi() } });
    solConnect(root)!.click();
    await flush();
    expect(solButton(root)!.disabled).toBe(true);
    expect(root.querySelector('[data-fid=held-request]')!.textContent).toBe(WALLET_COPY.restoredRequest('Phantom'));
    expect(root.querySelector('[data-fid=held-release]')).not.toBeNull();
    t = NOW + 50_000;
    await vi.advanceTimersByTimeAsync(1000);
    expect(solButton(root)!.disabled).toBe(false);
    expect(root.querySelector('[data-fid=held-release]')).toBeNull();
    expect(storedHold()).toBeNull();
  });

  it('a submit answered 200 with a body it cannot read: an unknown outcome, never "nothing was sent"', async () => {
    const { root, api } = await mount(session(), { provider: signer() });
    vi.spyOn(api, 'typedData').mockResolvedValue({ data: typedData(), serverTime: null });
    const garbled = new CheckoutApi('', (async () => new Response('<html>upstream</html>', { status: 200 })) as never);
    vi.spyOn(api, 'submit').mockImplementation((id, req) => garbled.submit(id, req));
    await connectAndPay(root);
    expect(phase(root)).toBe('unknown');
    expect(root.querySelector('[data-fid=submit-unknown]')!.textContent).toBe(COPY.submitUnknown);
    expect(text(root)).not.toContain(WALLET_ERROR_COPY.wallet_error);
    expect(text(root)).not.toContain(COPY.notSent);
    for (const sel of WAYS_TO_PAY) expect(root.querySelector(sel), sel).toBeNull();
  });
});

describe('pay page connection', () => {
  const offline = () => new ApiError(0, 'network_error', 'The connection failed.');

  it('lost connection: a status box that says how old the page is, and that clears on the next answer', async () => {
    // The 1 s ticker runs on fake time; loads still settle on real (zero-delay) timers.
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    let t = NOW;
    const { root, get } = await mount(session(), { now: () => t });
    get.mockRejectedValue(offline());
    t += 5000;
    await poll();
    const box = root.querySelector<HTMLElement>('[data-fid=offline]')!;
    expect(box.getAttribute('role')).toBe('status');
    expect(box.querySelector('.offline-title')!.textContent).toBe('Reconnecting to Prismaport');
    expect(text(box)).toContain('Last checked 5 s ago. Your payment still counts.');
    expect(text(root)).not.toContain(COPY.offline);
    expect(root.querySelector('.deposit')).not.toBeNull();

    // The count moves between polls, without a live region reading it out.
    t += 3000;
    vi.advanceTimersByTime(1000);
    const line = root.querySelector('[data-fid=offline] [data-checked]')!;
    expect(line.textContent).toBe('Last checked 8 s ago. Your payment still counts.');
    expect(line.getAttribute('aria-live')).toBe('off');

    // The same session answers again: the box goes although nothing else changed.
    get.mockResolvedValue({ data: session(), serverTime: null });
    await poll();
    expect(root.querySelector('[data-fid=offline]')).toBeNull();
  });

  it('"Try now" and coming back online load at once', async () => {
    const { root, get } = await mount(session());
    get.mockRejectedValue(offline());
    await poll();
    const n = get.mock.calls.length;
    root.querySelector<HTMLButtonElement>('[data-fid=offline-retry]')!.click();
    await flush();
    expect(get.mock.calls.length).toBe(n + 1);
    get.mockResolvedValue({ data: session(), serverTime: null });
    window.dispatchEvent(new Event('online'));
    await flush();
    expect(get.mock.calls.length).toBe(n + 2);
    expect(root.querySelector('[data-fid=offline]')).toBeNull();
  });

  it('an answer older than the last one shown is dropped, success or failure', async () => {
    const { root, get } = await mount(session());
    const paid = session({ status: 'paid', amount_received: '25.00', amount_remaining: '0.00' });
    type Answer = Awaited<ReturnType<typeof get>>;
    let answerOld!: (v: Answer) => void;
    get.mockImplementationOnce(() => new Promise<Answer>((r) => { answerOld = r; }));
    await poll();
    get.mockResolvedValueOnce({ data: paid, serverTime: null });
    await poll();
    expect(root.querySelector('[data-fid=paid]')).not.toBeNull();
    // The first request answers last, with the session as it was: the page stays on Paid.
    answerOld({ data: session(), serverTime: null });
    await flush();
    expect(root.querySelector('[data-fid=paid]')).not.toBeNull();
    expect(root.querySelector('.deposit')).toBeNull();

    let failOld!: (e: Error) => void;
    get.mockImplementationOnce(() => new Promise<Answer>((_, rej) => { failOld = rej; }));
    await poll();
    get.mockResolvedValueOnce({ data: paid, serverTime: null });
    await poll();
    failOld(offline());
    await flush();
    expect(root.querySelector('[data-fid=offline]')).toBeNull();
  });
});

describe('pay page statuses it does not know', () => {
  it('a session status: Processing, a status block that keeps checking, the payments, and no way to pay', async () => {
    const { root } = await mount(session({ status: 'refund_pending' as never,
      payments: [{ chain: 'base', asset: 'USDC', amount: '10.000000', status: 'settling' as never, tx_hash: TX }] }));
    expect(text(root)).not.toContain(COPY.renderFailed);
    expect(root.querySelector('.pill')!.textContent).toBe('Processing');
    const block = root.querySelector<HTMLElement>('[data-fid=updating]')!;
    expect(block.querySelector('[role=status] h2')!.textContent).toBe('We are updating this payment');
    expect(text(block)).toContain('This page refreshes by itself. There is nothing to do yet, and nothing you sent is lost.');
    expect(block.querySelector('[data-checked]')!.textContent).toBe('Checked 0 s ago');
    expect(root.querySelector('.payments .pill')!.textContent).toBe('Processing');
    for (const sel of ['.deposit', 'svg.qr', '[data-fid=connect]', '[data-fid^="method-"]']) expect(root.querySelector(sel), sel).toBeNull();
  });

  it('a signed payment state it does not know: no crash, and no second signature', async () => {
    sessionStorage.setItem('pp:sent:ses_01JBTEST', JSON.stringify({ key: 'base:USDC', signature: '', job: 'txj_01', at: NOW, seen: 0 }));
    const { root } = await mount(session({ signed_payments: [{ id: 'txj_01', chain: 'base', asset: 'USDC', status: 'queued' as never, outcome: null, tx_hash: null }] }));
    expect(text(root)).not.toContain(COPY.renderFailed);
    expect(root.querySelector('[data-fid=progress]')!.getAttribute('data-phase')).toBe('sent');
    expect(root.querySelector('[data-fid=pay]')).toBeNull();
  });

  it('a render error keeps polling: the next session that renders replaces the fallback card', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    const { root, get } = await mount(session({ pay_to: undefined as never }));
    expect(text(root)).toContain(COPY.renderFailed);
    // The 1 s ticker leaves the fallback card alone rather than throwing on it.
    vi.advanceTimersByTime(1000);
    expect(text(root)).toContain(COPY.renderFailed);
    get.mockResolvedValue({ data: session(), serverTime: null });
    await poll();
    expect(text(root)).not.toContain(COPY.renderFailed);
    expect(root.querySelector('.deposit')).not.toBeNull();
  });

  it('the next status it knows renders the page again', async () => {
    const { root, get } = await mount(session({ status: 'refund_pending' as never }));
    get.mockResolvedValue({ data: session(), serverTime: null });
    await poll();
    expect(root.querySelector('[data-fid=updating]')).toBeNull();
    expect(root.querySelector('.pill')!.textContent).toBe('Open');
    expect(root.querySelector('.deposit')).not.toBeNull();
  });
});
