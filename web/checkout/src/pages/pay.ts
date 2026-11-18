import { ApiError, type CheckoutApi, type Network, type PayTo, type Payment, type Session } from '../lib/api';
import { COPY, PAYMENT_PILL, SENT_GRACE_MS, STATUS_PILL, describeSession, extraCopy, extraTitle, nextCheckCopy, payWindowCopy, paymentPill, PAUSED, revealErrorCopy, warningCopy, errorCopy, reversedNote, signedCopy, signedInFlight, type SessionView } from '../lib/copy';
import { h, icon } from '../lib/dom';
import { merchantLogos } from '../lib/merchant';
import { WALLET_DOWNLOADS, assetBadge, networkLogo, tokenLogo, walletLogo } from '../lib/logos';
import { formatAmount, formatCountdown, formatDateTime } from '../lib/format';
import { explorerTxUrl, findAsset, findNetwork, isSolana, nativeSymbol, networkName, payToKey } from '../lib/networks';
import {
  EvmWallet,
  WALLET_ERROR_COPY,
  WalletError,
  isTooSmallToSettle,
  dueUnits,
  paySigned,
  payTransfer,
  walletBalance,
  type DiscoveredWallet,
  type PaymentTarget,
  type WalletBalance,
  type WalletWatch,
} from '../wallet/evm';
import {
  SolanaUnavailableError,
  UnavailableSolanaApi,
  connectSolanaWallet,
  isWalletRejection,
  listSolanaWallets,
  payWithSolanaWallet,
  solanaWalletSupported,
  type SolanaTransactionApi,
} from '../wallet/solana';
import type { Wallet, WalletAccount } from '@wallet-standard/base';
import { brandHeader, checkedLine, copyButton, copyIconButton, footer, messageCard, offlineNotice, pill, replace, shortValue, tickChecked } from '../ui';
import { depositBlock as sharedDepositBlock, networkPicker, pausedTag, testnetTagFor } from './deposit';
import { createTestPanel } from './testpanel';

export interface PayDeps {
  api: CheckoutApi;
  now?: () => number;
  discoverEvm?: () => Promise<DiscoveredWallet[]>;
  /** Keeps finding wallets after the first look: extensions can inject their provider late. */
  watchEvm?: (onChange: (wallets: DiscoveredWallet[]) => void) => WalletWatch;
  solanaApi?: SolanaTransactionApi;
  listSolana?: (chain: string) => Wallet[];
  /** Poll interval while a payment is expected, in ms. */
  pollMs?: number;
  /** How long a wallet request may stay unanswered before the page abandons it, in ms. */
  promptTimeoutMs?: number;
}

interface WalletNote {
  text: string;
  tone?: 'paid' | 'partial';
}

/** A payment the wallet sent (or signed) for this session that the session does not list yet. */
interface Sent {
  key: string;
  /** The transaction hash or signature; empty on the signed path, where Prismaport sends it. */
  signature: string;
  /** The signed payment's job id (signed path only). */
  job?: string;
  /** When it was sent; 0 for a signed payment the page found on the session rather than sent. */
  at: number;
  /** Payments already listed for this option when it was sent. */
  seen: number;
}

/** What the payer is asked to approve while the wallet's request is open. */
interface Prompt {
  wallet: string;
  text: string;
  /** The wallet signed and Prismaport is being handed the payment: nothing is left to approve or cancel. */
  sending?: boolean;
}

/**
 * A signed payment whose submit failed after it may have reached Prismaport (a timeout, a dropped
 * connection, an answer that could not be read). Until the session lists a signed payment for it,
 * or SUBMIT_CHECK_MS and SUBMIT_CHECK_LOADS pass without one, the page offers no way to pay.
 */
interface Unknown {
  key: string;
  /** Payments already listed for this option when the payer signed. */
  seen: number;
  /** The signed payments the session listed when the payer signed. */
  known: string[];
  /** Local clock time before which the page does not conclude that nothing was sent. */
  until: number;
  /** Session loads answered since the submit failed. */
  loads: number;
  /** What the wallet card says when no signed payment shows up. */
  text: string;
}

/** What the page shows from the wallet's request to Paid, in place of the ways to pay. */
type Progress =
  | { phase: 'confirm'; prompt: Prompt }
  | { phase: 'unknown' }
  | { phase: 'held'; wallet: string }
  | { phase: 'sent'; sent: Sent; payTo: PayTo; tx: string; stale: boolean; grace: boolean }
  | { phase: 'seen'; payment: Payment };

interface State {
  session: Session | null;
  networks: Network[];
  selected: string | null;
  offset: number;
  offline: boolean;
  evmWallets: DiscoveredWallet[];
  /** The Solana wallet the payer connected on this page, and the account it shared. */
  solWallet: Wallet | null;
  solAccount: WalletAccount | null;
  wallet: EvmWallet | null;
  walletName: string;
  walletRdns: string;
  /** The connected account's balance of the selected asset, when the wallet can tell. */
  balance: WalletBalance | null;
  /** The "No gas needed" explanation is open. */
  explain: boolean;
  busy: boolean;
  note: WalletNote | null;
  jobId: string | null;
  fullAddress: boolean;
  announced: string;
  notFound: boolean;
  sent: Sent | null;
  /** The wallet's request is open: the page shows only what to approve. */
  prompt: Prompt | null;
  /** A signed payment's submit of unknown outcome: the page checks before it offers paying again. */
  unknown: Unknown | null;
  confirmSendAgain: boolean;
  /** A payment link that still takes payments, to offer "Start a new payment" (null: none or unknown yet). */
  restartLink: string | null;
  /** The network chosen in the choose step (null: none yet). */
  pick: string | null;
  /** The payer went back to the network list ("Pay another way") after generating a payment. */
  choosing: boolean;
  /** A "Generate payment" request is in flight. */
  revealing: boolean;
  revealError: string | null;
  /** The way to pay the payer opened; null until they choose, then it sticks across renders. */
  method: PayMethod | null;
  /** The way to pay that opened after the first look for wallets. */
  firstMethod: PayMethod | null;
}

export type PayMethod = 'wallet' | 'address';

/** Which way to pay starts open: the wallet when this browser has one, otherwise the address. */
export function defaultMethod(hasWallet: boolean): PayMethod {
  return hasWallet ? 'wallet' : 'address';
}

/** The blockhash of a server-built transaction expires after about a minute; offer a resend only after that. */
export const SEND_AGAIN_AFTER_MS = 120_000;

/** A wallet request the payer leaves unanswered this long is abandoned, as if they cancelled it. */
export const PROMPT_TIMEOUT_MS = 120_000;

/**
 * A server-built Solana transaction carries a recent blockhash, which expires after about 150
 * blocks (a minute or so). The build answer says its last valid block height, but the page has no
 * current height to compare it with, so it counts wall-clock time: this long after the wallet was
 * asked to send it, the transaction can no longer land.
 */
export const SOLANA_REQUEST_LIFETIME_MS = 150_000;

/** The longest wait a Retry-After can impose on the polling. */
export const MAX_RETRY_AFTER_MS = 300_000;

/**
 * After a submit of unknown outcome, how long (and how many session loads) the page waits for its
 * signed payment to show before it says the payment was not sent.
 */
export const SUBMIT_CHECK_MS = 30_000;
export const SUBMIT_CHECK_LOADS = 2;

/** The progress from the wallet's request to Paid. */
export const PROGRESS_COPY = {
  checking: 'Checking your payment',
  sentText: (net: string) => `Your wallet sent the transaction. We are waiting to see it on ${net}. This usually takes about a minute.`,
  signedText: (net: string) => `Prismaport is sending your signed payment on ${net} and pays the network fee. This usually takes about a minute.`,
  found: 'Payment found',
  foundText: (amount: string, net: string) => `We see ${amount} on ${net}. Waiting for confirmations.`,
  slow: 'Taking longer than usual',
  slowText: (net: string) => `We have not seen this transaction on ${net} yet. It may still be pending in your wallet. We keep checking while this page is open.`,
  onItsWay: 'Your payment is on its way. Do not send it again. Keep this page open.',
  notDetected: 'Check the activity in your wallet. Send again only if it shows that the transaction failed.',
  confirmSendAgain: 'Send again only if your wallet shows that the first transaction failed. If both arrive, you pay twice.',
  confirmHelp: (wallet: string) => `Don't see the request? Open ${wallet}. If you close it there, nothing is sent.`,
  cancel: 'Cancel this request',
  methodLocked: (wallet: string) => `To send from another wallet or an exchange, cancel the ${wallet} request first.`,
  sending: 'Sending your payment',
  otherRequest: (wallet: string) => `Reject the other request in ${wallet}. This payment is already on its way.`,
  heldTitle: (wallet: string) => `Your ${wallet} request is still open`,
  heldText: (wallet: string) =>
    `Reject it in ${wallet} before you start a new payment, or both could be paid. If you approve it, this page follows it for 10 minutes.`,
  graceTop: 'Time ran out. Still checking your payment.',
  graceText: (net: string) =>
    `Your wallet sent it before the time ran out. We are waiting to see it on ${net}. A payment that lands now still reaches the merchant.`,
  graceNote: 'Do not send it again. We keep checking for 10 minutes. Keep this page open.',
} as const;

export const SOLANA_COPY = {
  unavailable: 'Paying with a connected Solana wallet is not available yet. Use the address below.',
  unknown: 'Your wallet did not say whether it sent the payment. Check the activity in your wallet before you try again; a payment that went through shows on this page.',
} as const;

const TERMINAL = new Set(['paid', 'overpaid']);

/** An older API served the address of every entry; a newer one says so with `revealed`. */
export const isRevealed = (p: PayTo): boolean => p.revealed ?? p.address !== undefined;

function storageGet(key: string): string | null {
  try {
    return sessionStorage.getItem(key);
  } catch {
    return null;
  }
}

function storageSet(key: string, value: string): void {
  try {
    sessionStorage.setItem(key, value);
  } catch {
    /* per-viewer convenience only */
  }
}

function storageRemove(key: string): void {
  try {
    sessionStorage.removeItem(key);
  } catch {
    /* per-viewer convenience only */
  }
}

/** The wallet this browser last connected on any checkout, to list it first ("Last used"). */
const LAST_WALLET_KEY = 'pp:wallet:last';

/** What the picker shows for a wallet, EVM (EIP-6963 info) or Solana (Wallet Standard name). */
interface WalletInfo {
  uuid: string;
  name: string;
  rdns: string;
}

function walletKey(info: Pick<WalletInfo, 'rdns' | 'name'>): string {
  return info.rdns || info.name;
}

function lastWallet(): string | null {
  try {
    return localStorage.getItem(LAST_WALLET_KEY);
  } catch {
    return null;
  }
}

function rememberWallet(w: Pick<WalletInfo, 'rdns' | 'name'>): void {
  try {
    localStorage.setItem(LAST_WALLET_KEY, walletKey(w));
  } catch {
    /* per-viewer convenience only */
  }
}

export const WALLET_COPY = {
  signedHint: 'You sign in your wallet; Prismaport sends the payment and pays the network fee.',
  transferHint: 'Connect a browser wallet to send the payment from it.',
  signedNote: 'No ETH needed. Your wallet shows a signature request, not a transaction.',
  transferNote: 'Your wallet pays the network fee.',
  noWalletTitle: 'No wallet found in this browser',
  noWalletText: 'Some wallets take a moment to load. Try again, or pay from any wallet or exchange with the deposit address.',
  stillNone: 'Still no wallet found. Check that the extension is on and can access this site.',
  phone: "On a phone? Open this page in your wallet app's browser, or scan the QR code.",
  /** A transfer the payer cancelled on the page is still open in the wallet: a second one would pay twice. */
  heldRequest: (wallet: string) => `Reject the earlier request in ${wallet} first, or wait for it to finish.`,
  /** The same after a reload: the page cannot see that request any more, so the payer says when it is gone. */
  restoredRequest: (wallet: string) => `A ${wallet} request from before may still be open. Reject it in ${wallet} first, or wait for it to finish.`,
  rejectedIt: 'I rejected it',
  lowBalanceTitle: (asset: string) => `Not enough ${asset} in this account`,
  lowBalance: (held: string, due: string, asset: string, wallet: string) =>
    `It holds ${held}; this payment needs ${due}. Switch to an account with enough ${asset} in ${wallet}, or send from another wallet to the address below.`,
} as const;

function readSent(raw: string | null): Sent | null {
  if (!raw) return null;
  try {
    const v = JSON.parse(raw) as Partial<Sent>;
    if (typeof v.key === 'string' && typeof v.signature === 'string' && typeof v.at === 'number' && typeof v.seen === 'number') {
      return { key: v.key, signature: v.signature, at: v.at, seen: v.seen, ...(typeof v.job === 'string' ? { job: v.job } : {}) };
    }
  } catch {
    /* ignore a corrupt entry */
  }
  return null;
}

/**
 * A wallet request the payer abandoned that the wallet can still send, kept per session (pp:held)
 * so that a reload does not offer a second wallet payment next to it.
 */
interface HoldMarker {
  wallet: string;
  /** The payment option it pays (payToKey). */
  key: string;
  /** When the wallet was asked (local clock). */
  at: number;
}

function readHold(raw: string | null): HoldMarker | null {
  if (!raw) return null;
  try {
    const v = JSON.parse(raw) as Partial<HoldMarker>;
    if (typeof v.wallet === 'string' && typeof v.key === 'string' && typeof v.at === 'number') return { wallet: v.wallet, key: v.key, at: v.at };
  } catch {
    /* ignore a corrupt entry */
  }
  return null;
}

const paymentsFor = (s: Session, key: string): number => s.payments.filter((p) => payToKey(p) === key).length;

/** An API error of a wallet payment. One that never reached Prismaport sent nothing, and nothing retries it. */
function payApiErrorText(e: ApiError): string {
  return e.code === 'network_error' ? COPY.notSent : errorCopy(e.code, e.retryAfter, e.status);
}

/**
 * A submit that failed without an answer from Prismaport (or behind a proxy's bare 5xx) may still
 * have reached it, and then the signed payment goes through: its outcome is unknown, not "not sent".
 */
function submitOutcomeUnknown(e: unknown): e is ApiError {
  return e instanceof ApiError && (e.code === 'network_error' || (e.code === 'http_error' && e.status >= 500));
}

function evmErrorText(e: unknown): string {
  if (e instanceof WalletError) return e.message;
  if (e instanceof ApiError) return payApiErrorText(e);
  return WALLET_ERROR_COPY.wallet_error;
}

function solanaErrorText(e: unknown): string {
  if (e instanceof SolanaUnavailableError) return SOLANA_COPY.unavailable;
  if (e instanceof ApiError) return payApiErrorText(e);
  if (isWalletRejection(e)) return WALLET_ERROR_COPY.rejected;
  return SOLANA_COPY.unknown;
}

/** A success URL is shown only when it is https (http in test mode): defence in depth, the API checks it too. */
export function safeSuccessUrl(url: string | null | undefined, livemode: boolean): string | null {
  if (!url) return null;
  try {
    const p = new URL(url).protocol;
    return p === 'https:' || (!livemode && p === 'http:') ? url : null;
  } catch {
    return null;
  }
}

export async function mountPay(root: HTMLElement, sessionId: string, deps: PayDeps): Promise<{ stop: () => void }> {
  const now = deps.now ?? (() => Date.now());
  const pollMs = deps.pollMs ?? 3000;
  const promptTimeoutMs = deps.promptTimeoutMs ?? PROMPT_TIMEOUT_MS;
  const solanaApi = deps.solanaApi ?? UnavailableSolanaApi;
  const listSolana = deps.listSolana ?? ((chain: string) => listSolanaWallets(chain));
  const sentKey = `pp:sent:${sessionId}`;
  const heldKey = `pp:held:${sessionId}`;
  let watcher: WalletWatch | undefined;
  const discover = (): Promise<DiscoveredWallet[]> =>
    (deps.discoverEvm ?? (() => Promise.resolve([] as DiscoveredWallet[])))().catch(() => [] as DiscoveredWallet[]);
  /** Settles once the first look for wallets is done (Connect shows before it ends). */
  let firstLook: Promise<unknown> = Promise.resolve();
  let looked = false;
  const sent = readSent(storageGet(sentKey));
  const st: State = {
    session: null, networks: [], selected: storageGet(`pp:sel:${sessionId}`), offset: 0, offline: false,
    evmWallets: [], solWallet: null, solAccount: null, wallet: null, walletName: '', walletRdns: '', balance: null, explain: false, busy: false, note: null, jobId: sent?.job ?? null, fullAddress: false, announced: '', notFound: false,
    sent, prompt: null, unknown: null, confirmSendAgain: false, restartLink: null, method: null, firstMethod: null,
    pick: null, choosing: false, revealing: false, revealError: null,
  };
  const card = h('section', { class: 'card stack', 'aria-labelledby': 'title' });
  const status = h('div', { class: 'sr-only', role: 'status', 'aria-live': 'polite' });
  const panel = createTestPanel({
    api: deps.api,
    session: () => st.session,
    selected: () => selectedPayTo(),
    onSession: (s) => {
      st.session = s;
      lastJson = JSON.stringify(s);
      render();
      schedule();
    },
    announce: (text) => announce(text),
  });
  root.replaceChildren(brandHeader(), card, panel.el, footer(), status);
  root.removeAttribute('aria-busy');

  let stopped = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let ticker: ReturnType<typeof setInterval> | undefined;
  let failures = 0;
  let lastJson = '';
  let wasPastExpiry = false;
  let wasSelecting = false;
  let focusTitle = false;
  let renderedStale = false;
  let networksReady = false;
  let networksPending: Promise<void> | null = null;
  let linkChecked = '';
  /** When the API last answered a session load (local clock), for "Last checked 12 s ago". */
  let lastOk = 0;
  /** The last render threw: the fallback card shows until a render succeeds. */
  let renderFailed = false;
  let wasGrace = false;
  /** Until when (local clock) a refusal asked the polling to wait (Retry-After); 0 when none. */
  let retryUntil = 0;
  /** When (local clock) the scheduled load runs: the later of the failure backoff and the Retry-After. */
  let nextLoadAt = 0;
  /** "Try now" came during a Retry-After: the reconnecting notice says when the next check is. */
  let retryNoticed = false;
  /** Session loads started, and the newest one whose answer the page applied: an older answer is dropped. */
  let loadSeq = 0;
  let appliedSeq = 0;

  /**
   * The payment whose wallet request is open. The payer can abandon it (Cancel, or no answer within
   * promptTimeoutMs), which frees the page, but the wallet can still answer later: a transaction
   * it sent is then recorded like any other, and a signature is dropped without being submitted.
   */
  interface Attempt extends HoldMarker {
    ctl: AbortController;
    timer: ReturnType<typeof setTimeout>;
    /**
     * The wallet can send it after the payer abandons it, and nothing on the server stops a second
     * payment next to it: a plain EVM transfer, or a Solana transaction once the wallet was asked
     * (the server only stops a second Solana one). A late signature is never submitted: not held.
     */
    holds: boolean;
    /** The wallet signed and the submit runs: past the point where the request can be abandoned. */
    answered: boolean;
  }
  let attempt: Attempt | null = null;
  /**
   * Requests the payer abandoned while the wallet still holds them: the wallet can still send each
   * one, so no new wallet payment starts until they settle (rejected, failed or sent), on Solana
   * can no longer land, or the payer says they rejected them. pp:held keeps the newest of these,
   * or the open request once it reached the wallet, so a reload holds it too (`restoredHold`).
   */
  const heldRequests = new Set<Attempt>();
  /**
   * A held request from before a reload: this page cannot see when the wallet lets go of it, so it
   * holds until it can no longer pay (expiry and the grace window, or the Solana blockhash) or the
   * payer says they rejected it.
   */
  let restoredHold = readHold(storageGet(heldKey));
  /** What pp:held holds, so it is written only when it changes. */
  let savedHold = restoredHold ? JSON.stringify(restoredHold) : null;
  /** A newer request the page dropped because an older one sent the payment: the payer must reject it. */
  let otherRequest: Attempt | null = null;

  const clock = () => now() + st.offset;

  /**
   * The session as the page shows it. A payment this page sent, a wallet request still open (also
   * a cancelled transfer the wallet still holds), or a submit of unknown outcome keeps it checking
   * past expiry instead of locking: "Start a new payment" next to it could pay twice.
   */
  const describe = (s: Session, sel: PayTo): SessionView =>
    describeSession(s, sel, clock(), st.sent !== null || attempt !== null || heldBy() !== null || st.unknown !== null);

  const selectedPayTo = (): PayTo | undefined => {
    const s = st.session;
    if (!s || s.pay_to.length === 0) return undefined;
    const revealed = s.pay_to.filter(isRevealed);
    if (revealed.length === 0) return s.pay_to[0];
    // The payer's last choice (kept per tab), else the network that has a payment, else the first
    // generated one: the public session does not say which was generated last.
    return revealed.find((p) => payToKey(p) === st.selected)
      ?? revealed.find((p) => s.payments.some((x) => payToKey(x) === payToKey(p)))
      ?? revealed[0];
  };

  // Without the network list the page cannot offer the wallet path or the exact on-chain amount,
  // so a failed fetch is tried again after the next session load that succeeds.
  function loadNetworks(): Promise<void> {
    networksPending ??= deps.api.networks()
      .then((r) => {
        st.networks = r.data.data;
        networksReady = true;
      })
      .catch(() => undefined)
      .finally(() => {
        networksPending = null;
      });
    return networksPending;
  }

  /**
   * Renders; an exception shows the fallback card instead of leaving the page on "Loading" (or
   * reading as "offline"). Polling goes on: the next answer that renders replaces the card.
   */
  function render(): void {
    try {
      renderCard();
      renderFailed = false;
    } catch {
      renderFailed = true;
      replace(card, ...messageCard('Something went wrong', COPY.renderFailed, 'title').childNodes);
      card.removeAttribute('aria-busy');
      root.removeAttribute('aria-busy');
    }
  }

  async function load(): Promise<void> {
    // Loads overlap ("Try now", back online, visible again, the ticker, the timer): an answer older
    // than the last one applied, success or failure, would show a stale session or state.
    const seq = ++loadSeq;
    try {
      const r = await deps.api.session(sessionId, typeof document !== 'undefined' && document.visibilityState === 'visible');
      if (seq < appliedSeq) return;
      appliedSeq = seq;
      if (r.serverTime !== null) st.offset = r.serverTime - now();
      failures = 0;
      retryUntil = 0;
      retryNoticed = false;
      lastOk = now();
      const wasOffline = st.offline;
      st.offline = false;
      const json = JSON.stringify(r.data);
      st.session = r.data;
      const settled = settleUnknown(r.data);
      // render() never throws (it shows the fallback card), so a render error is not counted as a
      // failed fetch and does not back off polling.
      void checkLink(r.data);
      // Back online, or showing the fallback card: render even when the session did not change.
      if (json !== lastJson || wasOffline || renderFailed || settled) {
        lastJson = json;
        render();
      }
      tickChecked(card, lastOk, now());
      if (!networksReady && !networksPending) {
        void loadNetworks().then(() => {
          if (networksReady && !stopped) render();
        });
      }
    } catch (e) {
      if (seq < appliedSeq) return;
      appliedSeq = seq;
      if (e instanceof ApiError && e.code === 'session_not_found') {
        stopped = true;
        st.notFound = true;
        render();
        return;
      }
      failures++;
      // A refusal that says when to come back (429): polling sooner would only be refused again.
      retryUntil = e instanceof ApiError && e.retryAfter ? now() + Math.min(e.retryAfter * 1000, MAX_RETRY_AFTER_MS) : 0;
      // The wait "Try now" asked about is over: a new one shows only on the next "Try now".
      retryNoticed = false;
      if (!st.offline) {
        st.offline = true;
        render();
      }
    }
  }

  /** The session's own link must still take payments for "Start a new payment" to show. */
  async function checkLink(s: Session): Promise<void> {
    const id = s.link_id;
    if (!id || linkChecked === id) return;
    if (s.link_active !== undefined) {
      linkChecked = id;
      st.restartLink = s.link_active ? id : null;
      return;
    }
    if (!isLocked(s)) return;
    linkChecked = id;
    try {
      const r = await deps.api.link(id);
      st.restartLink = r.data.active && r.data.live !== false ? id : null;
    } catch {
      st.restartLink = null;
    }
    if (!stopped) render();
  }

  /** Closed to payments: past expiry (or expired on the server) with nothing confirming or on its way. */
  function isLocked(s: Session): boolean {
    try {
      const sel = selectedPayTo();
      return sel ? describe(s, sel).locked : false;
    } catch {
      // A session this page cannot read (the fallback card shows): keep polling for one it can.
      return false;
    }
  }

  function schedule(): void {
    clearTimeout(timer);
    timer = undefined;
    if (stopped) return;
    const s = st.session;
    let wait = pollMs;
    if (s && TERMINAL.has(s.status)) return;
    if (s && isLocked(s)) return;
    if (s && s.status !== 'open' && s.status !== 'partially_paid') wait = Math.max(pollMs, 15000);
    if (typeof document !== 'undefined' && document.hidden) wait *= 3;
    if (failures > 0) wait = Math.min(30000, pollMs * 2 ** Math.min(failures, 4));
    const backoff = retryUntil - now();
    if (backoff > 0) wait = Math.max(wait, backoff);
    nextLoadAt = now() + wait;
    timer = setTimeout(async () => {
      timer = undefined;
      try {
        await load();
      } finally {
        // Re-armed whatever the round did: one failure must not end the polling.
        schedule();
      }
    }, wait);
  }

  /**
   * Loads now instead of at the next poll (page visible again, back online, "Try now", a wallet
   * answer); polling goes on from there. A Retry-After still running is waited out instead.
   */
  function loadNow(): void {
    if (stopped) return;
    if (now() < retryUntil) {
      if (timer === undefined) schedule();
      return;
    }
    clearTimeout(timer);
    timer = undefined;
    void load().finally(schedule);
  }

  /** "Try now": loads at once; during a Retry-After it says when the next check is instead of doing nothing visible. */
  function tryNow(): void {
    const waiting = now() < retryUntil;
    loadNow();
    if (waiting && !stopped) {
      retryNoticed = true;
      render();
    }
  }

  /**
   * How long until the scheduled load (the later of the failure backoff and the Retry-After), after
   * "Try now" met a Retry-After; null otherwise. With no poll scheduled (at the lock) there is no
   * next check to promise.
   */
  function nextCheckIn(): number | null {
    if (!retryNoticed || timer === undefined) return null;
    return Math.max(0, nextLoadAt - now());
  }

  function announce(text: string): void {
    if (text && text !== st.announced) {
      st.announced = text;
      status.textContent = text;
    }
  }

  function target(s: Session, p: PayTo, view: SessionView): PaymentTarget | null {
    const network = findNetwork(st.networks, p.chain);
    if (!network || view.due === null) return null;
    return { sessionId: s.id, payTo: p, network, due: view.due, expiresAt: s.expires_at };
  }

  // A signed payment's job says how it went, so only a transaction the wallet sent offers a resend.
  const sentStale = (): boolean => st.sent !== null && !st.sent.job && now() - st.sent.at >= SEND_AGAIN_AFTER_MS;

  function remember(sent: Sent): void {
    st.sent = sent;
    st.confirmSendAgain = false;
    storageSet(sentKey, JSON.stringify(sent));
    // Defensive: a hold from before a reload blocks every wallet payment, so none gets here while
    // one stands. The request's own marker goes with endAttempt, right after this.
    restoredHold = null;
    saveHold();
  }

  function clearSent(): void {
    st.sent = null;
    st.confirmSendAgain = false;
    storageRemove(sentKey);
  }

  /**
   * Forgets the sent payment once the session lists it, or a new payment for that option, or stops
   * taking payments, or its signed payment did not go through (the wallet block then says so).
   */
  function settleSent(s: Session): void {
    const sent = st.sent;
    if (!sent) return;
    const listed = (sent.signature !== '' && s.payments.some((p) => p.tx_hash === sent.signature)) || paymentsFor(s, sent.key) > sent.seen;
    const job = sent.job ? s.signed_payments.find((j) => j.id === sent.job) : undefined;
    if (listed || TERMINAL.has(s.status) || (job && signedCopy(job).failed)) clearSent();
  }

  /**
   * Settles a submit of unknown outcome against a session load: a signed payment for its option
   * that is in flight (or new since the payer signed) is followed like an answered submit; with
   * none after SUBMIT_CHECK_MS and SUBMIT_CHECK_LOADS answers, the wallet card says it was not
   * sent and paying is offered again. Returns whether the state changed.
   */
  function settleUnknown(s: Session): boolean {
    const u = st.unknown;
    if (!u) return false;
    u.loads++;
    const job = s.signed_payments.find((j) => payToKey(j) === u.key && (signedInFlight(j) || !u.known.includes(j.id)));
    if (job) {
      st.unknown = null;
      st.jobId = job.id;
      remember({ key: u.key, signature: '', job: job.id, at: now(), seen: u.seen });
      return true;
    }
    if (TERMINAL.has(s.status)) {
      st.unknown = null;
      return true;
    }
    if (u.loads >= SUBMIT_CHECK_LOADS && now() >= u.until) {
      st.unknown = null;
      st.note = { text: u.text, tone: 'partial' };
      return true;
    }
    return false;
  }

  function progressOf(s: Session, sel: PayTo, view: SessionView): Progress | null {
    if (view.locked || TERMINAL.has(s.status)) return null;
    // A payment the wallet already sent wins over a newer request still open: approving it would pay twice.
    if (st.prompt && !st.sent) return { phase: 'confirm', prompt: st.prompt };
    if (st.unknown) return { phase: 'unknown' };
    if (view.updating) return null;
    // A signed payment Prismaport is still sending is followed even when this tab did not sign it
    // (another tab, a reload, a submit whose answer was lost): it pays what is due, so offering the
    // address or a new signature next to it would invite a second payment.
    const found = !st.sent ? s.signed_payments.find(signedInFlight) : undefined;
    const sent = st.sent ?? (found ? { key: payToKey(found), signature: '', job: found.id, at: 0, seen: 0 } : null);
    if (sent) {
      const job = sent.job ? s.signed_payments.find((j) => j.id === sent.job) : undefined;
      // Past expiry there is no second payment to offer: "Taking longer than usual" would invite one.
      return { phase: 'sent', sent, payTo: s.pay_to.find((p) => payToKey(p) === sent.key) ?? sel, tx: sent.signature || job?.tx_hash || '',
        stale: !view.grace && sentStale(), grace: view.grace };
    }
    // Past expiry, an abandoned request the wallet still holds (or held from before a reload) can still be sent.
    const held = view.grace ? heldBy() : null;
    if (held !== null) return { phase: 'held', wallet: held };
    // Detected payments that cover what is due: nothing is left to pay, only to confirm.
    const confirming = s.payments.filter((p) => p.status === 'detected');
    if ((s.status === 'open' || s.status === 'partially_paid') && !view.payable && confirming.length > 0) {
      return { phase: 'seen', payment: confirming[confirming.length - 1]! };
    }
    return null;
  }

  function timerText(s: Session): string {
    const left = Date.parse(s.expires_at) - clock();
    return left > 0 ? `Expires in ${formatCountdown(left)}` : 'Expired';
  }

  const logoFor = merchantLogos((path) => deps.api.url(path));

  function renderCard(): void {
    if (st.notFound) {
      replace(card, h('h1', { class: 'display', id: 'title' }, 'Payment not found'), h('p', {}, COPY.notFound));
      announce(COPY.notFound);
      return;
    }
    const s = st.session;
    if (!s) {
      replace(card, h('p', { class: 'skeleton' }, st.offline ? COPY.offline : 'Loading the payment details.'));
      return;
    }
    settleSent(s);
    expireHolds();
    renderedStale = sentStale();
    document.title = `${s.product?.name ?? 'Payment'} · Prismaport`;
    const sel = selectedPayTo();
    if (!sel) {
      replace(card, h('h1', { class: 'display', id: 'title' }, s.product?.name ?? 'Payment'),
        h('p', {}, 'This session has no payment methods. Contact the merchant.'));
      return;
    }
    const view = describe(s, sel);
    wasPastExpiry = Date.parse(s.expires_at) <= clock();
    wasGrace = view.grace;
    const started = s.pay_to.some(isRevealed);
    const selecting = view.payable && (!started || st.choosing);
    wasSelecting = selecting;
    const progress = selecting ? null : progressOf(s, sel, view);
    const progressEl = progress ? progressPanel(progress) : null;
    announce(selecting ? COPY.choose : progressEl?.dataset.announce ?? view.message);

    // Paid: the success block below says so once, so the state pill stays out of the way. While a
    // payment is on its way the clock no longer matters to the payer.
    const paid = view.pill.tone === 'paid';
    // No countdown before a payment is generated: the selection window is not the pay window.
    const over = view.overpaid;
    const top = paid ? (over ? h('div', { class: 'row between', 'data-fid': 'overpaid-pills' }, pill(STATUS_PILL.paid),
      pill({ label: `${formatAmount(over.extra)} ${over.unit} extra`, tone: 'partial' })) : null)
      // Past expiry with a sent payment still on its way: an Expired pill would read as "too late".
      : view.grace ? h('div', { class: 'row' }, h('span', { class: 'timer', 'data-fid': 'grace-clock' }, icon('clock'), h('span', {}, PROGRESS_COPY.graceTop)))
      : h('div', { class: 'row between' }, pill(progress?.phase === 'seen' ? PAYMENT_PILL.detected : view.pill),
      view.payable && started && !progress ? h('span', { class: 'timer', role: 'timer', 'aria-live': 'off', 'data-timer': '' }, icon('clock'), h('span', {}, timerText(s))) : null);
    const title = h('div', { class: 'hero' },
      logoFor(s.merchant),
      h('h1', { class: 'display', id: 'title', tabindex: '-1' }, s.product?.name ?? 'Payment'),
      selecting
        ? h('p', { class: 'amount-hero' }, s.open_amount || s.amount === null ? COPY.anyAmount : `${formatAmount(s.amount)} ${s.currency}`)
        : h('p', { class: 'amount-hero with-logo' }, tokenLogo(sel.asset, { size: 24, decorative: true }),
          h('span', {}, s.open_amount || s.amount === null ? `${COPY.anyAmount} of ${sel.asset}` : `${formatAmount(sel.amount ?? s.amount)} ${sel.asset}`)));

    const parts: (Node | null)[] = [top];
    if (!s.livemode) parts.push(h('p', { class: 'note' }, icon('info'), h('span', {}, COPY.testMode)));
    parts.push(title);
    if (st.offline) parts.push(offlineNotice(lastOk, now(), tryNow, nextCheckIn()));
    const shown = s.pay_to.filter(isRevealed);
    if (view.payable && !selecting && !progress && shown.length > 1) parts.push(picker(shown, sel));
    const statusBlock = selecting
      ? h('div', { class: 'stack-tight' }, h('h2', { class: 'heading' }, 'Choose how to pay'),
        h('p', { class: 'status-line' }, COPY.choose))
      : paid
        ? h('div', { class: 'success', 'data-fid': 'paid' },
          h('span', { class: 'success-mark' }, icon('check', 24)),
          h('p', { class: 'status-line', 'data-tone': 'paid' }, view.message),
          view.detail ? h('p', { class: 'small muted' }, view.detail) : null)
        : h('div', { class: 'stack-tight' },
          view.instruction ? h('h2', { class: 'heading' }, view.instruction) : null,
          h('p', { class: 'status-line', 'data-tone': '' }, view.message),
          view.detail ? h('p', { class: 'small muted' }, view.detail) : null);
    parts.push(progressEl ?? (view.updating ? updatingBlock() : statusBlock));
    if (paid && over) parts.push(...overpaidBlocks(s, over));

    if (view.locked) {
      parts.push(lockPanel());
    } else if (selecting) {
      parts.push(selectBlock(s));
    } else if (view.payable && !progress) {
      if (view.warnMs !== null) parts.push(h('p', { class: 'note', 'data-emphasis': true, 'data-fid': 'expiry-warning', role: 'alert' }, icon('info'), h('span', {}, warningCopy(formatCountdown(view.warnMs)))));
      // Only a generated network gets here: the wallet and the address need its revealed deposit address.
      if (sel.paused) parts.push(pausedPanel(s, sel));
      parts.push(...payMethods(walletBlock(s, sel, view), depositBlock(s, sel, view), sel));
      if (new Set(s.pay_to.map((p) => p.chain)).size > 1) {
        parts.push(h('button', { type: 'button', class: 'btn-link', 'data-fid': 'pay-another', onclick: () => payAnotherWay() }, 'Pay another way'));
      }
    }
    const successUrl = safeSuccessUrl(s.success_url, s.livemode);
    if (successUrl && (s.status === 'paid' || s.status === 'overpaid')) {
      parts.push(h('a', { class: 'btn btn-primary', href: successUrl, rel: 'noopener', 'data-fid': 'success' },
        s.merchant?.name ? `Return to ${s.merchant.name}` : 'Return to the merchant', icon('arrow')));
    }
    // The steps already show the one payment being confirmed.
    if (s.payments.length > (progress?.phase === 'seen' ? 1 : 0)) parts.push(paymentsBlock(s.payments, paid));
    replace(card, ...parts.filter((x): x is Node => x !== null));
    panel.render();
    if (focusTitle) {
      focusTitle = false;
      card.querySelector<HTMLElement>('#title')?.focus({ preventScroll: true });
    }
  }

  /** D64: the payer is on a network that was paused after its address was shown. The address keeps working. */
  function pausedPanel(s: Session, sel: PayTo): HTMLElement {
    const net = networkName(sel.chain);
    const elsewhere = s.pay_to.some((p) => p.chain !== sel.chain && !p.paused);
    return h('div', { class: 'paused-panel', 'data-fid': 'paused-panel', role: 'status' },
      h('p', { class: 'note', 'data-emphasis': true }, icon('info'), h('span', {}, elsewhere ? PAUSED.panel(net) : PAUSED.panelOnly(net))),
      elsewhere ? h('button', { type: 'button', class: 'btn', 'data-fid': 'pay-other-network', onclick: () => payAnotherWay() }, PAUSED.action) : null);
  }

  /** A status this page does not know yet: nothing to do, and the page keeps polling until it knows it. */
  function updatingBlock(): HTMLElement {
    return h('div', { class: 'progress', 'data-fid': 'updating' },
      h('div', { class: 'progress-head', role: 'status' },
        h('span', { class: 'progress-mark', 'data-spin': '' }, icon('loader', 28)),
        h('h2', { class: 'heading' }, COPY.updatingTitle),
        h('p', { class: 'muted' }, COPY.updating),
        checkedLine(lastOk, now(), 'checked', 'progress-elapsed')));
  }

  function payAnotherWay(): void {
    st.choosing = true;
    st.pick = null;
    st.revealError = null;
    focusTitle = true;
    render();
  }

  /** Opens the payment view of a generated network. */
  function openNetwork(s: Session, chain: string): void {
    const entry = s.pay_to.filter((p) => p.chain === chain && isRevealed(p));
    const key = entry.find((p) => payToKey(p) === st.selected) ?? entry[0];
    if (!key) return;
    const changed = payToKey(key) !== payToKey(selectedPayTo() ?? key);
    st.selected = payToKey(key);
    storageSet(`pp:sel:${sessionId}`, st.selected);
    if (changed) {
      // Another network may have a wallet where this one had none: it picks its own way to pay.
      st.method = null;
      st.firstMethod = null;
      st.balance = null;
      void refreshBalance();
    }
    st.choosing = false;
    st.pick = null;
    st.note = null;
    st.fullAddress = false;
    st.revealError = null;
    focusTitle = true;
  }

  async function generate(): Promise<void> {
    const chain = st.pick;
    const s = st.session;
    if (!chain || !s || st.revealing) return;
    if (s.pay_to.some((p) => p.chain === chain && isRevealed(p))) {
      openNetwork(s, chain);
      render();
      return;
    }
    st.revealing = true;
    st.revealError = null;
    render();
    try {
      const r = await deps.api.reveal(sessionId, chain);
      if (r.serverTime !== null) st.offset = r.serverTime - now();
      st.session = r.data;
      lastJson = JSON.stringify(r.data);
      openNetwork(r.data, chain);
    } catch (e) {
      st.revealError = e instanceof ApiError ? revealErrorCopy(e.code, e.retryAfter) : revealErrorCopy('network_error');
      // A closed or finished session is shown by the next load, not by this form.
      if (e instanceof ApiError && (e.code === 'session_expired' || e.code === 'session_not_payable' || e.code === 'chain_unavailable')) {
        if (e.code === 'chain_unavailable') st.pick = null;
        loadNow();
      }
    } finally {
      st.revealing = false;
      render();
    }
  }

  /** The choose step: networks with their assets, then "Generate payment". Nothing here is an address. */
  function selectBlock(s: Session): HTMLElement {
    const chains = [...new Set(s.pay_to.map((p) => p.chain))];
    const fs = h('fieldset', { class: 'picker' }, h('legend', { class: 'label' }, 'Network'));
    const opts = h('div', { class: 'options options-list' });
    for (const chain of chains) {
      const entries = s.pay_to.filter((p) => p.chain === chain);
      const generated = entries.some(isRevealed);
      // D64: a network not generated yet whose every entry is paused cannot be started (409 chain_unavailable).
      const paused = !generated && entries.every((e) => e.paused);
      const input = h('input', { type: 'radio', name: 'network', value: chain, 'data-fid': `net-${chain}` });
      input.checked = st.pick === chain;
      input.disabled = st.revealing || paused;
      input.addEventListener('change', () => {
        st.pick = chain;
        st.revealError = null;
        render();
      });
      const assets = h('span', { class: 'option-assets' }, ...entries.map((p) =>
        h('span', {}, tokenLogo(p.asset, { size: 16, decorative: true }), p.amount ?? s.amount ? `${formatAmount((p.amount ?? s.amount) as string)} ${p.asset}` : `${COPY.anyAmount} of ${p.asset}`)));
      opts.append(h('label', { class: 'option' }, input, networkLogo(chain, { size: 24, decorative: true, tag: false }),
        h('span', { class: 'option-text' }, h('span', { class: 'asset net-line' }, networkName(chain), testnetTagFor(chain), paused ? pausedTag() : null), assets),
        generated ? h('span', { class: 'generated' }, 'Generated') : null));
    }
    fs.append(opts);
    const pickedGenerated = st.pick !== null && s.pay_to.some((p) => p.chain === st.pick && isRevealed(p));
    const out = h('div', { class: 'stack-tight', 'data-fid': 'select-step' });
    if (st.choosing) out.append(h('p', { class: 'note', 'data-emphasis': true, 'data-fid': 'switch-warning' }, icon('info'), h('span', {}, COPY.switchWarning)));
    out.append(fs);
    if (st.revealError) out.append(h('p', { class: 'note', 'data-emphasis': true, role: 'alert', 'data-fid': 'reveal-error' }, icon('info'), h('span', {}, st.revealError)));
    out.append(
      h('button', { type: 'button', class: 'btn btn-primary', 'data-fid': 'generate', disabled: st.pick === null || st.revealing, 'aria-busy': String(st.revealing),
        onclick: () => void generate() }, st.revealing ? 'Generating' : pickedGenerated ? 'Show payment' : 'Generate payment'),
      h('p', { class: 'small muted' }, payWindowCopy(s.pay_window_seconds)));
    if (st.choosing) {
      out.append(h('button', { type: 'button', class: 'btn-link', 'data-fid': 'back-to-payment', onclick: () => { st.choosing = false; st.pick = null; st.revealError = null; focusTitle = true; render(); } },
        'Back to the payment'));
    }
    return out;
  }

  /**
   * The wallet and the deposit address as two options of which one is open at a time: the closed one
   * shrinks to a single row that opens it. Both stay in the page so a pending wallet step survives.
   */
  function payMethods(wallet: HTMLElement | null, deposit: HTMLElement, sel: PayTo): HTMLElement[] {
    if (!wallet) return [deposit];
    const hasWallet = isSolana(sel.chain, st.networks) || st.wallet !== null || st.evmWallets.length > 0;
    // Fixed once the first look for wallets is done, so a wallet announced later does not fold the address.
    if (looked) st.firstMethod ??= defaultMethod(hasWallet);
    // A wallet that cannot send this amount points to the address, so the address opens instead.
    const open = st.method ?? (wallet.hasAttribute('data-short') ? 'address' : st.firstMethod ?? defaultMethod(hasWallet));
    wallet.id = 'pay-wallet';
    wallet.tabIndex = -1;
    wallet.dataset.fid = 'pay-wallet';
    const depositWrap = h('div', { id: 'pay-address', tabindex: '-1', 'data-fid': 'pay-address' }, deposit);
    const toggle = (to: PayMethod, controls: string, label: string, iconName: string) =>
      h('button', { type: 'button', class: 'method-toggle', 'aria-expanded': 'false', 'aria-controls': controls, 'data-fid': `method-${to}`,
        onclick: () => openMethod(to) },
      icon(iconName, 20), h('span', {}, label), icon('chevron', 16));
    if (open === 'wallet') {
      depositWrap.hidden = true;
      return [wallet, toggle('address', 'pay-address', 'Or send from any wallet or exchange', 'qr'), depositWrap];
    }
    wallet.hidden = true;
    return [toggle('wallet', 'pay-wallet', 'Or pay with your wallet', 'wallet'), wallet, depositWrap];
  }

  /** What replaces the payment block at expiry. No address, QR, amount to send or wallet control. */
  function lockPanel(): HTMLElement {
    return h('div', { class: 'stack-tight', 'data-fid': 'lock-panel' },
      h('p', { class: 'note', 'data-emphasis': true, role: 'alert' }, icon('info'), h('span', {}, COPY.closed)),
      st.restartLink ? h('a', { class: 'btn btn-primary', href: `/l/${encodeURIComponent(st.restartLink)}`, 'data-fid': 'start-new' }, 'Start a new payment', icon('arrow')) : null,
      // A session on a customer address: its page opens the next one (or says the address changed).
      !st.restartLink && st.session?.customer_address_id
        ? h('a', { class: 'btn btn-primary', href: `/a/${encodeURIComponent(st.session.customer_address_id)}`, 'data-fid': 'start-new' }, 'Start a new payment', icon('arrow')) : null);
  }

  function picker(options: PayTo[], sel: PayTo): HTMLElement {
    return networkPicker(options, sel, (key) => {
      st.selected = key;
      // Another network may have a wallet where this one had none: it picks its own way to pay.
      st.method = null;
      st.firstMethod = null;
      st.note = null;
      st.fullAddress = false;
      st.balance = null;
      storageSet(`pp:sel:${sessionId}`, key);
      render();
      void refreshBalance();
    });
  }

  function walletBlock(s: Session, sel: PayTo, view: SessionView): HTMLElement | null {
    // D48: no wallet control before the network is generated (a transfer needs its deposit address).
    if (!isRevealed(sel) || sel.paused) return null;
    if (isSolana(sel.chain, st.networks)) return solanaBlock(s, sel);
    const t = target(s, sel, view);
    if (!t) return null;
    const signed = sel.signed_payment && (findAsset(st.networks, sel)?.eip3009 ?? false);
    let exact = true;
    let tooSmall = false;
    try {
      dueUnits(t);
      tooSmall = signed && isTooSmallToSettle(t);
    } catch {
      exact = false;
    }
    const latestSigned = st.jobId ? s.signed_payments.find((j) => j.id === st.jobId) : undefined;
    const sc = latestSigned ? signedCopy(latestSigned) : null;
    const net = networkName(sel.chain);

    const head = h('div', { class: 'wallet-card-head' },
      h('h3', { class: 'wallet-card-title' }, icon('wallet', 20), h('span', {}, 'Pay with your wallet'), h('span', { class: 'net-chip', 'data-fid': 'wallet-network' }, `on ${net}`)),
      signed ? h('button', {
        type: 'button', class: 'gas-badge', 'data-fid': 'gas-help', 'aria-expanded': st.explain ? 'true' : 'false',
        onclick: () => { st.explain = !st.explain; render(); },
      }, 'No gas needed', icon('help', 14)) : null);
    const block = h('section', { class: 'wallet-card', 'aria-label': `Pay with your wallet on ${net}`, 'data-short': !exact || tooSmall || undefined }, head);
    if (signed && st.explain) {
      block.append(h('ol', { class: 'gas-steps small', 'data-fid': 'gas-steps' },
        h('li', {}, 'You sign a message in your wallet. It authorizes exactly this payment.'),
        h('li', {}, `Prismaport sends the transaction on ${net} and pays the network fee.`),
        h('li', {}, `The ${sel.asset} contract checks your signature and moves the payment.`)));
    }

    if (!st.wallet) {
      // Which wallets this browser has shows on Connect, not before it.
      block.append(h('p', { class: 'small muted' }, signed ? WALLET_COPY.signedHint : WALLET_COPY.transferHint));
      block.append(h('button', { type: 'button', class: 'btn btn-primary', 'data-fid': 'connect', disabled: st.busy, onclick: () => void connectEvm() },
        st.busy ? 'Check your wallet' : 'Connect wallet'));
    } else {
      block.append(h('div', { class: 'wallet-account' },
        walletLogo({ rdns: st.walletRdns, name: st.walletName }, 36),
        h('div', { class: 'wallet-account-text' },
          h('p', { class: 'wallet-name' }, st.walletName),
          h('p', { class: 'small muted row' }, st.wallet.account ? shortValue(st.wallet.account) : null,
            h('span', { class: 'net-dot' }, net))),
        st.evmWallets.length > 1
          ? h('button', {
            type: 'button', class: 'btn-link small', 'data-fid': 'change', disabled: st.busy,
            onclick: () => { disconnect(); void connectEvm(); },
          }, 'Change')
          : h('button', { type: 'button', class: 'btn-link small', 'data-fid': 'disconnect', disabled: st.busy, onclick: disconnect }, 'Disconnect')));
      const low = st.balance !== null && exact && !tooSmall && st.balance.units < dueUnits(t);
      if (st.balance !== null) {
        block.append(h('p', { class: 'row between small muted', 'data-fid': 'balance' }, h('span', {}, 'Balance'),
          h('span', { class: 'mono balance-value', 'data-low': low }, `${formatAmount(st.balance.amount)} ${sel.asset}`)));
      }
      if (!exact) {
        block.append(h('p', { class: 'note' }, icon('info'), h('span', {}, WALLET_ERROR_COPY.not_exact)));
      } else if (tooSmall) {
        block.append(h('p', { class: 'note' }, icon('info'), h('span', {}, WALLET_ERROR_COPY.below_minimum)));
      } else if (low && !st.busy && !(sc && !sc.failed)) {
        const due = `${formatAmount(view.due!)} ${sel.asset}`;
        block.append(
          h('div', { class: 'low-balance', role: 'status', 'data-fid': 'low-balance' }, icon('alert', 18),
            h('div', {},
              h('p', { class: 'low-balance-title' }, WALLET_COPY.lowBalanceTitle(sel.asset)),
              h('p', {}, WALLET_COPY.lowBalance(`${formatAmount(st.balance!.amount)} ${sel.asset}`, due, sel.asset, st.walletName)))),
          h('button', { type: 'button', class: 'btn btn-primary', 'data-fid': 'pay', disabled: true }, signed ? `Sign to pay ${due}` : `Send ${due}`));
      } else if (!(sc && !sc.failed)) {
        const due = `${formatAmount(view.due!)} ${sel.asset}`;
        const label = signed ? `Sign to pay ${due}` : `Send ${due}`;
        const held = heldBy();
        const hint = st.busy
          ? [h('p', { class: 'small muted center' }, signed
            ? `Approve the signature request in ${st.walletName}. It authorizes ${due} to the Prismaport Gateway on ${net}.`
            : `Approve the transaction in ${st.walletName}.`)]
          : held !== null ? heldHint(held)
            : [h('p', { class: 'small muted center' }, signed ? WALLET_COPY.signedNote : WALLET_COPY.transferNote)];
        block.append(
          h('button', { type: 'button', class: 'btn btn-primary', 'data-fid': 'pay', disabled: st.busy || held !== null, onclick: () => void payEvm(signed) },
            st.busy ? icon('loader', 18) : null, st.busy ? `Confirm in ${st.walletName}` : label),
          ...hint,
        );
      }
    }
    const note = sc ? { text: sc.text, tone: sc.failed ? ('partial' as const) : undefined } : st.note;
    if (note) {
      block.append(h('p', { class: 'note', 'data-emphasis': note.tone === 'partial', role: 'status' }, icon('info'), h('span', {}, note.text)));
    }
    return block;
  }

  /** Paid above the amount: the arithmetic, then where the extra went and how to ask for it back. */
  function overpaidBlocks(s: Session, over: NonNullable<SessionView['overpaid']>): HTMLElement[] {
    const row = (label: string, value: string, extra = false) => [
      h('dt', { 'data-extra': extra }, label),
      h('dd', { class: 'mono', 'data-extra': extra }, `${formatAmount(value)} ${over.unit}`),
    ];
    const merchant = s.merchant?.name || null;
    return [
      h('dl', { class: 'breakdown', 'data-fid': 'overpaid-breakdown' },
        ...row('Price', over.amount), ...row('You sent', over.received), ...row('Extra', over.extra, true)),
      h('section', { class: 'extra-note', 'aria-labelledby': 'extra-title', 'data-fid': 'overpaid-note' },
        h('h2', { id: 'extra-title' }, icon('info', 18), h('span', {}, extraTitle(over.extra, over.unit))),
        h('p', {}, extraCopy(merchant)),
        h('div', { class: 'stack-tight' },
          h('p', { class: 'label' }, 'Payment reference'),
          h('div', { class: 'field' }, h('span', { class: 'value mono reference' }, s.id), copyButton(s.id, 'payment reference', 'copy-reference')))),
    ];
  }

  /** The same card as on EVM: wallets found, Connect (a picker when there are several), then Pay. */
  function solanaBlock(s: Session, sel: PayTo): HTMLElement | null {
    if (!solanaWalletSupported(sel)) return null;
    const wallets = listSolana(sel.chain);
    if (wallets.length === 0) return null;
    const net = networkName(sel.chain);
    const native = nativeSymbol(sel.chain) ?? 'SOL';
    const block = h('section', { class: 'wallet-card', 'aria-label': `Pay with your wallet on ${net}` },
      h('div', { class: 'wallet-card-head' },
        h('h3', { class: 'wallet-card-title' }, icon('wallet', 20), h('span', {}, 'Pay with your wallet'), h('span', { class: 'net-chip', 'data-fid': 'wallet-network' }, `on ${net}`)),
        h('button', {
          type: 'button', class: 'gas-badge', 'data-fid': 'gas-help', 'aria-expanded': st.explain ? 'true' : 'false',
          onclick: () => { st.explain = !st.explain; render(); },
        }, `No ${native} needed`, icon('help', 14))));
    if (st.explain) {
      block.append(h('ol', { class: 'gas-steps small', 'data-fid': 'gas-steps' },
        h('li', {}, 'You approve one transaction in your wallet.'),
        h('li', {}, `Prismaport pays the network fee on ${net}.`),
        h('li', {}, 'The payment reaches the merchant in that same transaction.')));
    }

    const wallet = st.solWallet && st.solAccount && wallets.includes(st.solWallet) ? st.solWallet : null;
    if (!wallet || !st.solAccount) {
      // Which wallets this browser has shows on Connect, as on EVM.
      block.append(
        h('p', { class: 'small muted' }, `You approve in your wallet; Prismaport pays the ${net} network fee.`),
        h('button', { type: 'button', class: 'btn btn-primary', 'data-fid': 'sol-connect', disabled: st.busy, onclick: () => void connectSolana(wallets, sel) },
          st.busy ? 'Check your wallet' : 'Connect wallet'));
    } else {
      const view = describe(s, sel);
      const due = view.due !== null ? `${formatAmount(view.due)} ${sel.asset}` : sel.asset;
      const held = heldBy();
      block.append(
        h('div', { class: 'wallet-account' },
          walletLogo({ rdns: '', name: wallet.name }, 36),
          h('div', { class: 'wallet-account-text' },
            h('p', { class: 'wallet-name' }, wallet.name),
            h('p', { class: 'small muted row' }, shortValue(st.solAccount.address), h('span', { class: 'net-dot' }, net))),
          wallets.length > 1
            ? h('button', { type: 'button', class: 'btn-link small', 'data-fid': 'sol-change', disabled: st.busy,
              onclick: () => { disconnectSolana(); void connectSolana(wallets, sel); } }, 'Change')
            : h('button', { type: 'button', class: 'btn-link small', 'data-fid': 'sol-disconnect', disabled: st.busy, onclick: disconnectSolana }, 'Disconnect')),
        h('button', { type: 'button', class: 'btn btn-primary', 'data-fid': 'sol-pay', disabled: st.busy || held !== null, onclick: () => void paySolana(wallet, s, sel) },
          st.busy ? 'Check your wallet' : `Pay ${due}`),
        ...(held !== null && !st.busy ? heldHint(held)
          : [h('p', { class: 'small muted center' }, `No ${native} needed. ${wallet.name} asks you to approve; Prismaport pays the network fee.`)]));
    }
    if (st.note) block.append(h('p', { class: 'note', 'data-emphasis': st.note.tone === 'partial', role: 'status' }, icon('info'), h('span', {}, st.note.text)));
    return block;
  }

  /**
   * From the wallet's request to Paid, in place of the ways to pay. While a sent payment is not
   * listed yet no way to pay shows: the server asks for what is due minus detected payments only,
   * so paying again would pay the full amount twice. `data-announce` is what the page announces.
   */
  function progressPanel(p: Progress): HTMLElement {
    // The wallet signed, or a submit's outcome is unknown: nothing to approve or cancel, only to wait.
    if (p.phase === 'unknown' || (p.phase === 'confirm' && p.prompt.sending)) {
      const unknown = p.phase === 'unknown';
      const heading = unknown ? PROGRESS_COPY.checking : PROGRESS_COPY.sending;
      const line = unknown ? COPY.submitUnknown : COPY.signedSending;
      return h('div', { class: 'progress', 'data-fid': 'progress', 'data-phase': unknown ? 'unknown' : 'submitting', 'data-announce': `${heading}. ${line}` },
        h('div', { class: 'progress-head' },
          h('span', { class: 'progress-mark', 'data-spin': '' }, icon('loader', 28)),
          h('h2', { class: 'heading' }, heading),
          h('p', { class: 'muted', role: 'status', 'data-fid': unknown ? 'submit-unknown' : 'submitting' }, line)));
    }
    if (p.phase === 'held') {
      const heading = PROGRESS_COPY.heldTitle(p.wallet);
      const line = PROGRESS_COPY.heldText(p.wallet);
      return h('div', { class: 'progress', 'data-fid': 'progress', 'data-phase': 'held', 'data-announce': `${heading}. ${line}` },
        h('div', { class: 'progress-head' },
          h('span', { class: 'progress-mark', 'data-pulse': '' }, icon('wallet', 28)),
          h('h2', { class: 'heading' }, heading),
          h('p', { class: 'muted', role: 'status', 'data-fid': 'held-at-expiry' }, line)),
        // A wallet that never settles the request (or one from before a reload) would hold off
        // the lock for the whole grace window: the payer can say they rejected it.
        releaseButton());
    }
    if (p.phase === 'confirm') {
      const heading = `Confirm in ${p.prompt.wallet}`;
      return h('div', { class: 'progress', 'data-fid': 'progress', 'data-phase': 'confirm', 'data-announce': heading },
        h('div', { class: 'progress-head' },
          h('span', { class: 'progress-mark', 'data-pulse': '' }, icon('wallet', 28)),
          h('h2', { class: 'heading' }, heading),
          h('p', { class: 'muted' }, p.prompt.text)),
        h('p', { class: 'small muted center' }, PROGRESS_COPY.confirmHelp(p.prompt.wallet)),
        h('button', { type: 'button', class: 'btn btn-block', 'data-fid': 'prompt-cancel', onclick: () => abandonAttempt() }, PROGRESS_COPY.cancel),
        // The address stays out of reach while the wallet holds the request: both could be paid.
        h('p', { class: 'method-locked', 'data-fid': 'method-locked' }, icon('lock'), h('span', {}, PROGRESS_COPY.methodLocked(p.prompt.wallet))));
    }
    const seen = p.phase === 'seen';
    const chain = seen ? p.payment.chain : p.payTo.chain;
    const net = networkName(chain);
    const tx = seen ? p.payment.tx_hash : p.tx;
    const signed = !seen && p.sent.job !== undefined;
    const stale = !seen && p.stale;
    const grace = !seen && p.grace;
    const amount = seen ? `${formatAmount(p.payment.amount)} ${p.payment.asset}` : '';
    const count = seen && p.payment.confirmations !== undefined && p.payment.confirmations_required
      ? { n: p.payment.confirmations, of: p.payment.confirmations_required } : null;

    const heading = stale ? PROGRESS_COPY.slow : seen ? PROGRESS_COPY.found : PROGRESS_COPY.checking;
    const text = stale ? PROGRESS_COPY.slowText(net) : seen ? PROGRESS_COPY.foundText(amount, net) : grace ? PROGRESS_COPY.graceText(net)
      : signed && !tx ? PROGRESS_COPY.signedText(net) : PROGRESS_COPY.sentText(net);
    const head = h('div', { class: 'progress-head' },
      stale ? h('span', { class: 'progress-mark', 'data-tone': 'partial' }, icon('clock', 28))
        : h('span', { class: 'progress-mark', 'data-spin': '' }, icon('loader', 28)),
      h('h2', { class: 'heading' }, heading),
      h('p', { class: 'muted' }, text),
      seen || !p.sent.at ? null : h('p', { class: 'progress-elapsed', 'data-elapsed': String(p.sent.at) }, elapsedText(p.sent.at)));

    // Prismaport sends a signed payment, so its first step runs until the job has a transaction.
    const sending = signed && !tx;
    const steps = h('ol', { class: 'steps', 'aria-label': 'Payment progress' },
      step(sending ? 'active' : 'done', seen ? 'Sent' : signed ? (tx ? 'Sent by Prismaport' : 'Signed in your wallet') : 'Sent from your wallet',
        tx ? h('span', { class: 'row' }, txLink(chain, tx), copyIconButton(tx, 'transaction hash', 'copy-progress-tx'))
          : sending ? h('span', {}, 'Prismaport sends it and pays the network fee.') : null),
      step(seen ? 'done' : sending ? 'todo' : 'active', 'Seen on the network',
        seen ? h('span', { class: 'mono' }, amount) : sending ? null : h('span', {}, stale ? `Not seen on ${net} yet.` : `Looking for it on ${net}.`)),
      step(seen ? 'active' : 'todo', 'Confirmations', seen ? confirmationsDetail(count) : null),
      step('todo', 'Paid', null));

    const out = h('div', { class: 'progress', 'data-fid': 'progress', 'data-phase': stale ? 'slow' : grace ? 'grace' : p.phase, 'data-announce': `${heading}. ${text}` }, head, steps);
    // A newer wallet request the page dropped once this payment went out: approving it pays twice.
    if (otherRequest) {
      out.append(h('p', { class: 'note', 'data-emphasis': true, role: 'alert', 'data-fid': 'other-request' }, icon('info'),
        h('span', {}, PROGRESS_COPY.otherRequest(otherRequest.wallet))));
    }
    if (!stale) {
      out.append(h('p', { class: 'note', role: 'status' }, icon('info'), h('span', {}, grace ? PROGRESS_COPY.graceNote : PROGRESS_COPY.onItsWay)));
    } else if (!st.confirmSendAgain) {
      out.append(h('p', { class: 'small muted' }, PROGRESS_COPY.notDetected),
        h('button', { type: 'button', class: 'btn', 'data-fid': 'send-again', onclick: () => { st.confirmSendAgain = true; render(); } }, 'It failed, send again'));
    } else {
      out.append(h('p', { class: 'note', 'data-emphasis': true, role: 'alert' }, icon('info'), h('span', {}, PROGRESS_COPY.confirmSendAgain)),
        h('div', { class: 'row' },
          h('button', { type: 'button', class: 'btn btn-small', 'data-fid': 'send-again-confirm', onclick: () => { clearSent(); st.note = null; render(); } }, 'Yes, send again'),
          h('button', { type: 'button', class: 'btn btn-small', 'data-fid': 'send-again-cancel', onclick: () => { st.confirmSendAgain = false; render(); } }, 'Cancel')));
    }
    return out;
  }

  function step(state: 'done' | 'active' | 'todo', title: string, detail: HTMLElement | null): HTMLElement {
    const label = state === 'done' ? 'done' : state === 'active' ? 'in progress' : 'next';
    return h('li', { class: 'step', 'data-state': state },
      h('span', { class: 'step-dot', 'aria-hidden': 'true' }, state === 'done' ? icon('check', 14) : null),
      h('div', { class: 'step-body' },
        h('p', { class: 'step-title' }, title, h('span', { class: 'sr-only' }, ` (${label})`)),
        detail ? h('div', { class: 'small muted step-detail' }, detail) : null));
  }

  /** "6 of 10" with a meter when the API counts blocks; otherwise just that it is waiting. */
  function confirmationsDetail(count: { n: number; of: number } | null): HTMLElement {
    if (!count) return h('span', {}, 'Waiting for the network to confirm it.');
    const meter = h('span', { class: 'meter', role: 'progressbar', 'aria-label': 'Confirmations', 'aria-valuemin': '0',
      'aria-valuemax': String(count.of), 'aria-valuenow': String(count.n) }, h('span', {}));
    // CSSOM, not a style attribute: the page's CSP has no unsafe-inline for styles.
    (meter.firstElementChild as HTMLElement).style.width = `${Math.min(100, (count.n / count.of) * 100)}%`;
    return h('span', { class: 'confirmations', 'data-fid': 'confirmations' }, h('span', { class: 'mono' }, `${count.n} of ${count.of}`), meter);
  }

  function elapsedText(at: number): string {
    return `${formatCountdown(now() - at)} elapsed`;
  }

  function depositBlock(s: Session, sel: PayTo, view: SessionView): HTMLElement {
    return sharedDepositBlock({
      payTo: sel, networks: st.networks,
      // An open-amount session has no amount: the block says "any amount" and the QR carries none.
      due: s.open_amount ? null : view.due ?? sel.amount ?? s.amount,
      message: s.product?.name ?? 'Payment',
      fullAddress: st.fullAddress,
      onToggleAddress: () => { st.fullAddress = !st.fullAddress; render(); },
      exchangeWarning: view.exchangeWarning,
    });
  }

  /** Each payment with its state and transaction. A paid session drops the Confirmed pill: the success block said it. */
  function paymentsBlock(payments: Payment[], paid: boolean): HTMLElement {
    const list = h('ul', { class: 'payments' });
    payments.forEach((p, i) => {
      const confirmed = p.status === 'confirmed' || p.status === 'finalized';
      const when = confirmed && p.confirmed_at ? formatDateTime(p.confirmed_at) : null;
      list.append(h('li', {},
        h('div', { class: 'payment-what' }, assetBadge(p.asset, p.chain, { size: 24, decorative: true, tag: false }),
          h('div', {},
            h('p', { class: 'amount' }, `${formatAmount(p.amount)} ${p.asset}`),
            h('p', { class: 'small muted net-line' }, `on ${networkName(p.chain)}`, testnetTagFor(p.chain)),
            p.status === 'reversed' ? h('p', { class: 'small muted' }, reversedNote(p)) : null)),
        h('div', { class: 'tx-meta' },
          h('div', { class: 'row' }, paid && confirmed ? null : pill(paymentPill(p.status)), p.tx_hash ? txLink(p.chain, p.tx_hash) : null,
            p.tx_hash ? copyIconButton(p.tx_hash, 'transaction hash', `copy-pay-${i}`) : null),
          when ? h('p', { class: 'small muted', 'data-fid': `confirmed-at-${i}` }, `Confirmed ${when}`) : null)));
    });
    return h('div', {}, h('h2', { class: 'label' }, payments.length === 1 ? 'Payment' : 'Payments'), list);
  }

  /** The short hash, linked to the network's explorer when the page knows it. */
  function txLink(chain: string, tx: string): HTMLElement {
    const url = explorerTxUrl(chain, tx);
    if (!url) return shortValue(tx);
    return h('a', { class: 'tx-link', href: url, target: '_blank', rel: 'noopener noreferrer', title: `View on the ${networkName(chain)} explorer`, 'data-fid': 'tx-link' },
      shortValue(tx), icon('external', 12));
  }

  /** The wallets found so far; when none, waits for the first look and asks once more. */
  async function findWallets(): Promise<DiscoveredWallet[]> {
    await firstLook;
    if (st.evmWallets.length > 0) return st.evmWallets;
    watcher?.rescan();
    const again = await discover();
    if (again.length > 0 && st.evmWallets.length === 0) {
      st.evmWallets = again;
      render();
    }
    return st.evmWallets;
  }

  /** Stops following the connected wallet's account and chain changes. */
  let unwatchWallet: (() => void) | null = null;

  function disconnect(): void {
    unwatchWallet?.();
    unwatchWallet = null;
    st.wallet = null;
    st.balance = null;
    st.note = null;
    render();
  }

  /** Set from the click until a wallet is chosen, so a double click opens one dialog. */
  let choosing = false;

  async function connectEvm(): Promise<void> {
    if (st.busy || choosing) return;
    choosing = true;
    let picked: DiscoveredWallet | undefined;
    try {
      if (st.evmWallets.length === 0) await findWallets();
      picked = st.evmWallets.length === 0 ? await noWalletDialog()
        : st.evmWallets.length === 1 ? st.evmWallets[0] : await pickWallet(st.evmWallets);
    } finally {
      choosing = false;
    }
    if (!picked) return;
    const w = new EvmWallet(picked.provider);
    st.busy = true;
    st.note = null;
    render();
    try {
      await w.connect();
      if (stopped) return;
      unwatchWallet?.();
      st.wallet = w;
      st.walletName = picked.info.name;
      st.walletRdns = picked.info.rdns;
      rememberWallet(picked.info);
      unwatchWallet = w.watch(() => {
        if (st.wallet !== w || stopped) return;
        if (!w.account) {
          disconnect();
          return;
        }
        st.balance = null;
        // A wallet error is about the old account; a sent transaction's note and hash still hold.
        if (st.note?.tone === 'partial') st.note = null;
        render();
        void refreshBalance();
      });
    } catch (e) {
      st.note = { text: e instanceof WalletError ? e.message : WALLET_ERROR_COPY.wallet_error, tone: 'partial' };
    } finally {
      st.busy = false;
      render();
    }
    void refreshBalance();
  }

  /** Counts balance reads: only the latest one may land (account, chain or option may have changed). */
  let balanceRead = 0;

  async function refreshBalance(): Promise<void> {
    const s = st.session;
    const sel = selectedPayTo();
    const wallet = st.wallet;
    if (!s || !sel || !wallet) return;
    const read = ++balanceRead;
    const t = target(s, sel, describe(s, sel));
    const balance = t ? await walletBalance(wallet, t) : null;
    if (read !== balanceRead || st.wallet !== wallet || stopped) return;
    st.balance = balance;
    render();
  }

  /**
   * The wallet of an abandoned request still open in it (or held from before a reload), which
   * blocks a new wallet payment; null when none.
   */
  function heldBy(): string | null {
    for (const a of heldRequests) return a.wallet;
    return restoredHold?.wallet ?? null;
  }

  /** The hold comes from before a reload: no request of this page is behind it. */
  const heldFromBefore = (): boolean => heldRequests.size === 0 && restoredHold !== null;

  /** When a held request can no longer land (local clock): a Solana blockhash expires, an EVM transfer does not. */
  const holdEnds = (h: HoldMarker): number =>
    (isSolana(h.key.split(':', 1)[0]!, st.networks) ? h.at + SOLANA_REQUEST_LIFETIME_MS : Infinity);

  /** A held request can still pay this session: it can still land, and the session is not past expiry and the grace window. */
  function holdLive(h: HoldMarker): boolean {
    const s = st.session;
    return now() < holdEnds(h) && (!s || clock() < Date.parse(s.expires_at) + SENT_GRACE_MS);
  }

  /**
   * Keeps pp:held on the newest hold that can still pay, or removes it when none is left. The open
   * request counts once it reached the wallet: a reload while the wallet shows it must not offer a
   * second payment next to it.
   */
  function saveHold(): void {
    let h = restoredHold && holdLive(restoredHold) ? restoredHold : null;
    for (const a of heldRequests) if (holdLive(a)) h = a;
    if (attempt && attempt.holds && !attempt.answered && holdLive(attempt)) h = attempt;
    const json = h ? JSON.stringify({ wallet: h.wallet, key: h.key, at: h.at }) : null;
    if (json === savedHold) return;
    savedHold = json;
    if (json) storageSet(heldKey, json);
    else storageRemove(heldKey);
  }

  /**
   * Lets go of holds that can no longer pay: a Solana transaction past its blockhash, and a hold
   * from before a reload past expiry and the grace window. Returns whether a hold ended.
   */
  function expireHolds(): boolean {
    let ended = false;
    for (const a of heldRequests) {
      if (now() >= holdEnds(a)) {
        heldRequests.delete(a);
        ended = true;
      }
    }
    if (restoredHold && !holdLive(restoredHold)) {
      restoredHold = null;
      ended = true;
    }
    saveHold();
    return ended;
  }

  /**
   * "I rejected it": the payer says the wallet let go of the held request (one from before a
   * reload, or one this page abandoned that the wallet never settles). A transaction the wallet
   * still sends is recorded and followed like any late answer.
   */
  function releaseHolds(): void {
    heldRequests.clear();
    restoredHold = null;
    saveHold();
    render();
    // The button is gone: focus moves to the way to pay it freed.
    (card.querySelector<HTMLElement>('[data-fid=pay]:not(:disabled), [data-fid=sol-pay]:not(:disabled)') ?? card.querySelector<HTMLElement>('#title'))
      ?.focus({ preventScroll: true });
  }

  /** The line under a Pay button a held request blocks, then "I rejected it". */
  function heldHint(wallet: string): HTMLElement[] {
    return [
      h('p', { class: 'small muted center', 'data-fid': 'held-request' }, heldFromBefore() ? WALLET_COPY.restoredRequest(wallet) : WALLET_COPY.heldRequest(wallet)),
      releaseButton(),
    ];
  }

  const releaseButton = (): HTMLElement =>
    h('button', { type: 'button', class: 'btn btn-small held-release', 'data-fid': 'held-release', onclick: releaseHolds }, WALLET_COPY.rejectedIt);

  function startAttempt(prompt: Prompt, key: string, holds: boolean): Attempt {
    const ctl = new AbortController();
    const a: Attempt = { ctl, wallet: prompt.wallet, key, at: now(), holds, answered: false,
      timer: setTimeout(() => { if (attempt === a) abandonAttempt(); }, promptTimeoutMs) };
    attempt = a;
    st.busy = true;
    st.note = null;
    st.prompt = prompt;
    // An EVM transfer is kept from the start: the wallet may show it before the page hears back.
    if (holds) saveHold();
    return a;
  }

  /**
   * The wallet signed `a`'s payment and the submit starts: nothing is left to approve, and the
   * request can no longer be cancelled or time out (the payment may go through from here).
   */
  function answerAttempt(a: Attempt): void {
    if (attempt !== a) return;
    a.answered = true;
    clearTimeout(a.timer);
    if (st.prompt) st.prompt = { ...st.prompt, sending: true };
    render();
  }

  /**
   * The wallet was asked to send `a`'s Solana transaction: from here it can land even if the payer
   * abandons the request, until its blockhash expires.
   */
  function askedWallet(a: Attempt): void {
    a.holds = true;
    a.at = now();
    saveHold();
  }

  /**
   * `a`'s request settled (the wallet answered, refused or failed): frees the page unless the payer
   * already abandoned it, and its marker goes (a payment it sent is followed from pp:sent).
   */
  function endAttempt(a: Attempt): void {
    heldRequests.delete(a);
    if (otherRequest === a) otherRequest = null;
    if (attempt === a) {
      clearTimeout(a.timer);
      attempt = null;
      st.busy = false;
      st.prompt = null;
    }
    saveHold();
  }

  /**
   * Drops the open request, unless the wallet already signed it; a request the wallet can still
   * send stays held until it settles (or, on Solana, can no longer land).
   */
  function dropAttempt(): Attempt | null {
    const a = attempt;
    if (!a || a.answered) return null;
    clearTimeout(a.timer);
    a.ctl.abort();
    if (a.holds) {
      heldRequests.add(a);
      saveHold();
    }
    attempt = null;
    st.busy = false;
    st.prompt = null;
    return a;
  }

  function abandonAttempt(): void {
    if (dropAttempt()) render();
  }

  /**
   * `a` moved money after the payer abandoned it. A newer request still open in a wallet would pay
   * a second time: the page drops it, shows the payment on its way, and asks the payer to reject it.
   */
  function supersede(a: Attempt): void {
    if (attempt === null || attempt === a) return;
    otherRequest = dropAttempt() ?? otherRequest;
  }

  async function payEvm(signed: boolean): Promise<void> {
    const s = st.session;
    const sel = selectedPayTo();
    const wallet = st.wallet;
    if (!s || !sel || !wallet || st.busy || heldBy() !== null) return;
    const view = describe(s, sel);
    const t = target(s, sel, view);
    if (!t || !view.payable) return;
    const due = `${formatAmount(t.due)} ${sel.asset}`;
    const net = networkName(sel.chain);
    const key = payToKey(sel);
    const seen = paymentsFor(s, key);
    const known = s.signed_payments.map((j) => j.id);
    st.jobId = null;
    const a = startAttempt({
      wallet: st.walletName,
      text: signed ? `Approve the signature request. It authorizes ${due} to the Prismaport Gateway on ${net}. You pay no network fee.`
        : `Approve the transfer of ${due} on ${net}. Your wallet pays the network fee.`,
    }, key, !signed);
    render();
    try {
      // Whatever comes back moved money and is recorded, even after the payer cancelled: a submitted
      // signature, or a transfer the wallet broadcast itself. A signature that comes back after the
      // cancel is never submitted (paySigned drops it).
      if (signed) {
        const { jobId } = await paySigned(deps.api, wallet, t, a.ctl.signal, () => answerAttempt(a));
        st.jobId = jobId;
        remember({ key, signature: '', job: jobId, at: now(), seen });
      } else {
        const { txHash } = await payTransfer(wallet, t, a.ctl.signal);
        remember({ key, signature: txHash, at: now(), seen });
        supersede(a);
      }
    } catch (e) {
      // After the signature only a definite refusal from Prismaport proves nothing was sent; a lost
      // or unreadable answer may hide a submit that went through, and "not sent" could pay twice.
      if (a.answered && (!(e instanceof ApiError) || submitOutcomeUnknown(e))) {
        const text = e instanceof ApiError ? payApiErrorText(e) : COPY.notSent;
        st.unknown = { key, seen, known, until: now() + SUBMIT_CHECK_MS, loads: 0, text };
      } else if (attempt === a) {
        // An abandoned request says nothing more: the payer moved on.
        st.note = { text: evmErrorText(e), tone: 'partial' };
      }
    } finally {
      endAttempt(a);
      render();
      // Polling may have stopped at the lock while the wallet held the request.
      loadNow();
      void refreshBalance();
    }
  }

  async function connectSolana(wallets: Wallet[], sel: PayTo): Promise<void> {
    if (st.busy || choosing) return;
    choosing = true;
    let picked: Wallet | undefined;
    try {
      picked = wallets.length === 1 ? wallets[0]
        : await pickFrom(wallets, (w) => ({ uuid: w.name, name: w.name, rdns: '' }), `${networkName(sel.chain)} wallets found in this browser`);
    } finally {
      choosing = false;
    }
    if (!picked) return;
    st.busy = true;
    st.note = null;
    render();
    try {
      const account = await connectSolanaWallet(picked, sel.chain);
      if (stopped) return;
      st.solAccount = account;
      st.solWallet = picked;
      rememberWallet({ rdns: '', name: picked.name });
    } catch (e) {
      st.note = { text: solanaErrorText(e), tone: 'partial' };
    } finally {
      st.busy = false;
      render();
    }
  }

  function disconnectSolana(): void {
    st.solWallet = null;
    st.solAccount = null;
    st.note = null;
    render();
  }

  async function paySolana(w: Wallet, s: Session, sel: PayTo): Promise<void> {
    if (st.busy || st.sent || heldBy() !== null) return;
    // Held once the wallet is asked to send: before that, an abandoned request never reaches it.
    const a = startAttempt({ wallet: w.name, text: `Approve the payment on ${networkName(sel.chain)}. Prismaport pays the network fee.` }, payToKey(sel), false);
    render();
    let sent = false;
    try {
      // The payer may have switched account in the wallet since Connect: then connect again.
      const account = st.solAccount && w.accounts.some((x) => x.address === st.solAccount!.address) ? st.solAccount : undefined;
      const { signature, account: paid } = await payWithSolanaWallet(w, solanaApi, s.id, sel, account, a.ctl.signal, () => askedWallet(a));
      // The wallet sent the transaction itself: recorded even when the payer cancelled meanwhile.
      if (!account) st.solAccount = w.accounts.find((x) => x.address === paid) ?? st.solAccount;
      const key = payToKey(sel);
      remember({ key, signature, at: now(), seen: paymentsFor(st.session ?? s, key) });
      supersede(a);
      sent = true;
    } catch (e) {
      if (attempt === a) st.note = { text: solanaErrorText(e), tone: 'partial' };
    } finally {
      endAttempt(a);
      render();
      // Polling may have stopped at the lock while the wallet held the request.
      if (sent) loadNow();
    }
  }

  /** A modal dialog with a title and a close button; `done` closes it and settles the promise. */
  function modal<T>(title: string, build: (done: (v?: T) => void, dialog: HTMLDialogElement) => Node[]): Promise<T | undefined> {
    return new Promise((resolve) => {
      const dialog = h('dialog', { class: 'dialog', 'aria-labelledby': 'wallet-title' });
      let open = true;
      const done = (v?: T) => {
        if (!open) return;
        open = false;
        if (typeof dialog.close === 'function') dialog.close();
        dialog.remove();
        resolve(v);
      };
      dialog.append(
        h('div', { class: 'dialog-head' },
          h('h2', { class: 'heading', id: 'wallet-title' }, title),
          h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Close', 'data-fid': 'dialog-close', onclick: () => done() }, icon('close', 20))),
        ...build(done, dialog));
      dialog.addEventListener('cancel', (e) => {
        e.preventDefault();
        done();
      });
      document.body.append(dialog);
      if (typeof dialog.showModal === 'function') dialog.showModal();
      else dialog.setAttribute('open', '');
    });
  }

  /** Opens one way to pay and moves focus into it: the row that opened it is gone after the render. */
  function openMethod(to: PayMethod): void {
    st.method = to;
    render();
    card.querySelector<HTMLElement>(to === 'wallet' ? '#pay-wallet' : '#pay-address')?.focus({ preventScroll: true });
  }

  function showDepositAddress(): void {
    openMethod('address');
    const deposit = card.querySelector('.deposit');
    if (deposit && typeof deposit.scrollIntoView === 'function') deposit.scrollIntoView({ block: 'start', behavior: 'smooth' });
  }

  function pickWallet(list: DiscoveredWallet[]): Promise<DiscoveredWallet | undefined> {
    return pickFrom(list, (w) => w.info, 'Found in this browser');
  }

  /** The wallet picker, for either family: `info` names each item, the last used one comes first. */
  function pickFrom<T>(list: T[], info: (t: T) => WalletInfo, subtitle: string): Promise<T | undefined> {
    const last = lastWallet();
    const ordered = [...list].sort((a, b) => Number(walletKey(info(b)) === last) - Number(walletKey(info(a)) === last));
    return modal<T>('Connect a wallet', (done) => [
      h('p', { class: 'small muted' }, subtitle),
      h('ul', { class: 'wallet-list' }, ...ordered.map((w) => h('li', {},
        h('button', { type: 'button', class: 'wallet-option', 'data-fid': `wallet-${info(w).uuid}`, onclick: () => done(w) },
          walletLogo(info(w), 36),
          h('span', { class: 'wallet-option-name' }, info(w).name),
          walletKey(info(w)) === last ? h('span', { class: 'tag' }, 'Last used') : null,
          icon('chevron', 18))))),
      h('p', { class: 'small muted dialog-foot' }, 'Wallet not listed? ',
        h('button', { type: 'button', class: 'btn-link', 'data-fid': 'use-address', onclick: () => { done(); showDepositAddress(); } }, 'Use the deposit address'),
        ' from any wallet or exchange.'),
    ]);
  }

  /** No wallet found: try again (extensions can load late), pay by address, or get a wallet. */
  async function noWalletDialog(): Promise<DiscoveredWallet | undefined> {
    const found = await modal<DiscoveredWallet[]>('Connect a wallet', (done, dialog) => {
      const status = h('p', { class: 'note', role: 'status', 'data-fid': 'still-none' });
      status.hidden = true;
      const retry = async () => {
        watcher?.rescan();
        const again = await discover();
        const list = again.length > 0 ? again : st.evmWallets;
        if (list.length > 0) {
          st.evmWallets = list;
          done(list);
          return;
        }
        replace(status, icon('info'), h('span', {}, WALLET_COPY.stillNone));
        status.hidden = false;
        dialog.querySelector<HTMLButtonElement>('[data-fid=retry]')?.focus();
      };
      return [
        h('div', { class: 'wallet-empty' },
          h('span', { class: 'wallet-empty-icon' }, icon('plug', 26)),
          h('h3', { class: 'wallet-empty-title' }, WALLET_COPY.noWalletTitle),
          h('p', { class: 'small muted' }, WALLET_COPY.noWalletText)),
        status,
        h('div', { class: 'dialog-actions' },
          h('button', { type: 'button', class: 'btn', 'data-fid': 'retry', onclick: () => void retry() }, 'Try again'),
          h('button', { type: 'button', class: 'btn btn-primary', 'data-fid': 'use-address', onclick: () => { done(); showDepositAddress(); } }, 'Use the address')),
        h('p', { class: 'label' }, 'Get a wallet'),
        h('ul', { class: 'download-list' }, ...WALLET_DOWNLOADS.map((d) => h('li', {},
          h('a', { class: 'download', href: d.url, target: '_blank', rel: 'noopener noreferrer' },
            walletLogo(d, 28), h('span', {}, d.name), h('span', { class: 'sr-only' }, '(opens in a new tab)'), icon('external', 16))))),
        h('p', { class: 'small muted' }, WALLET_COPY.phone),
      ];
    });
    render();
    if (!found) return undefined;
    return found.length === 1 ? found[0] : pickWallet(found);
  }

  render();
  const looking = discover();
  firstLook = looking;
  const [, , wallets] = await Promise.all([loadNetworks(), load(), looking]);
  if (st.evmWallets.length === 0) st.evmWallets = wallets;
  looked = true;
  render();
  // The first load may already have ended the page (an unknown session): start no watcher, timer
  // or listener that nothing would stop.
  if (stopped) return { stop: () => {} };
  schedule();
  watcher = deps.watchEvm?.((list) => {
    const ids = (l: DiscoveredWallet[]) => l.map((w) => w.info.uuid).sort().join(' ');
    // Never shrink the list (the first look may hold a wallet the watcher has not heard yet), but
    // take a same-size list that differs: an announced wallet replacing the window.ethereum fallback.
    if (stopped || list.length < st.evmWallets.length || ids(list) === ids(st.evmWallets)) return;
    st.evmWallets = list;
    render();
  });
  ticker = setInterval(() => {
    const s = st.session;
    // Nothing ticks on a stopped page or on the fallback card: the next load renders again.
    if (!s || stopped || renderFailed) return;
    tickChecked(card, lastOk, now());
    // A held request that can no longer land frees wallet payments (or, past expiry, the lock).
    const released = expireHolds();
    const past = Date.parse(s.expires_at) <= clock();
    const sel = selectedPayTo();
    const view = sel ? describe(s, sel) : null;
    if (released || past !== wasPastExpiry || (view?.grace ?? false) !== wasGrace || sentStale() !== renderedStale) {
      render();
      // Re-validate against the server once at the lock; the next poll decides whether to continue.
      if (past) loadNow();
      return;
    }
    const warn = view && !wasSelecting ? view.warnMs : null;
    const w = card.querySelector('[data-fid="expiry-warning"] span:last-child');
    if (warn !== null && !w) {
      render();
      return;
    }
    if (w && warn !== null) w.textContent = warningCopy(formatCountdown(warn));
    const el = card.querySelector('[data-timer] span');
    if (el) el.textContent = timerText(s);
    const elapsed = card.querySelector<HTMLElement>('[data-elapsed]');
    if (elapsed) elapsed.textContent = elapsedText(Number(elapsed.dataset.elapsed));
    const next = card.querySelector('[data-next-check]');
    if (next) {
      const left = nextCheckIn();
      if (left === null) render();
      else next.textContent = nextCheckCopy(formatCountdown(left));
    }
  }, 1000);
  const onVisible = () => {
    if (!document.hidden) loadNow();
  };
  // Back online: check at once instead of waiting out the backoff.
  const onOnline = () => loadNow();
  document.addEventListener('visibilitychange', onVisible);
  window.addEventListener('online', onOnline);
  return {
    stop: () => {
      stopped = true;
      watcher?.stop();
      unwatchWallet?.();
      clearTimeout(timer);
      clearInterval(ticker);
      if (attempt) clearTimeout(attempt.timer);
      document.removeEventListener('visibilitychange', onVisible);
      window.removeEventListener('online', onOnline);
    },
  };
}
