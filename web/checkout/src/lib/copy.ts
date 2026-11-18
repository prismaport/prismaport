import type { PayTo, Payment, PaymentStatus, Session, SessionStatus, SignedPayment } from './api';
import { addAmounts, compareAmounts, formatAmount, subtractAmounts } from './format';
import { nativeSymbol, networkName } from './networks';

type Tone = 'paid' | 'partial' | 'reversed' | 'idle';

export interface Pill {
  label: string;
  tone: Tone;
}

export const STATUS_PILL: Record<SessionStatus, Pill> = {
  open: { label: 'Open', tone: 'idle' },
  partially_paid: { label: 'Partially paid', tone: 'partial' },
  paid: { label: 'Paid', tone: 'paid' },
  overpaid: { label: 'Overpaid', tone: 'paid' },
  expired: { label: 'Expired', tone: 'idle' },
  late_payment: { label: 'Late payment', tone: 'partial' },
  under_review: { label: 'Under review', tone: 'partial' },
};

export const PAYMENT_PILL: Record<PaymentStatus, Pill> = {
  detected: { label: 'Confirming', tone: 'idle' },
  confirmed: { label: 'Confirmed', tone: 'paid' },
  finalized: { label: 'Confirmed', tone: 'paid' },
  reversed: { label: 'Reversed', tone: 'reversed' },
};

/** A session or payment status this page does not know yet: the API can ship before the page. */
const PROCESSING_PILL: Pill = { label: 'Processing', tone: 'idle' };

/** The pill of a session status, or Processing for one this page does not know. */
export const statusPill = (status: string): Pill => STATUS_PILL[status as SessionStatus] ?? PROCESSING_PILL;

/** The pill of a payment status, or Processing for one this page does not know. */
export const paymentPill = (status: string): Pill => PAYMENT_PILL[status as PaymentStatus] ?? PROCESSING_PILL;

export const COPY = {
  waiting: 'Waiting for your payment. Keep this page open.',
  paid: 'Paid. You can close this page.',
  footer: 'Paid with Prismaport',
  late: 'This payment arrived after the session expired. It reached the merchant; contact them if you need help.',
  review: 'This payment is under review. Contact the merchant if you need help.',
  expired: 'This session expired. Ask the merchant for a new payment link.',
  closed: 'This payment session has closed. Do not send funds to the address shown before.',
  confirming: 'Payment seen on the network. Waiting for confirmations. This can take up to a minute. Keep this page open.',
  refund: 'Refunds come from the merchant. Prismaport never holds your payment.',
  complete: 'Payment complete',
  notFound: 'This payment link is not valid. Check the link or ask the merchant for a new one.',
  offline: 'Cannot reach Prismaport right now. Retrying.',
  reconnecting: 'Reconnecting to Prismaport',
  /** Under "Reconnecting to Prismaport" after "Try now" while a Retry-After runs, before when the page checks again. */
  retryWait: 'Prismaport asked us to wait.',
  /** A wallet payment that failed before it reached Prismaport: nothing retries it, the payer signs again. */
  notSent: 'We could not reach Prismaport, so this payment was not sent. Check your connection and sign again.',
  /**
   * The submit of a signed payment failed after it may have reached Prismaport (timeout, dropped
   * connection, unreadable answer): the payment may still go through, so the page checks first.
   */
  submitUnknown: 'We could not confirm that your payment reached Prismaport. Do not pay again yet; this page keeps checking.',
  /** The wallet signed and Prismaport is sending the payment (also the status of a pending signed payment). */
  signedSending: 'Payment signed. Sending it to the network. This can take up to a minute. Keep this page open.',
  updatingTitle: 'We are updating this payment',
  updating: 'This page refreshes by itself. There is nothing to do yet, and nothing you sent is lost.',
  renderFailed: 'This page could not be displayed. Reload the page. If it keeps happening, contact the merchant.',
  choose: 'Choose a network, then generate your payment.',
  switchWarning: 'If you already sent funds to the address shown, do not pay again. Payments to it are still detected until this session closes.',
  sendOnlyDetail: 'Other tokens and other networks do not count toward this payment and may be lost.',
  testMode: 'Test mode. Payments use test networks and have no value.',
  anyAmount: 'Any amount',
  anyAmountDetail: 'Send as much as you want. Everything that arrives before this payment closes counts.',
  expiredAnyAmount: 'This payment session expired. Start a new payment to send more.',
  addressNotFound: 'This deposit address is not valid. Check the link or ask the merchant for a new one.',
  linkLoadFailed: 'We could not load this payment link. Check your connection and try again. Nothing was charged.',
  addressReplaced: 'The merchant replaced this deposit address. Do not send funds to it: payments sent here from now on are not detected.',
  addressUnavailable: 'This deposit address is not taking payments right now. Contact the merchant if you need help.',
  addressStart: 'Start the payment before you send. Prismaport watches this address only while a payment is open.',
  addressStatic: 'This address is yours and never changes. You can save it, but start a payment here each time you send.',
  addressOpenSession: 'A payment on this address is still open.',
} as const;

/** Under "Reconnecting to Prismaport": how old the page's last answer from the API is. */
export const offlineDetail = (seconds: number) => `Last checked ${seconds} s ago. Your payment still counts.`;

/** After COPY.retryWait: when the page checks again, as m:ss. */
export const nextCheckCopy = (left: string) => `Next check in ${left}.`;

/** Under a status the page does not know: how old the page's last answer from the API is. */
export const checkedCopy = (seconds: number) => `Checked ${seconds} s ago`;

/** The title of the one network warning above the deposit QR, the same on /pay and /a. */
export const sendOnlyTitle = (asset: string, chain: string) => `Send only ${asset} on ${networkName(chain)}`;

/** The instruction of an open-amount session: there is no amount to match. */
const anyAmountInstruction = (asset: string, chain: string) => `Send any amount of ${asset} on ${networkName(chain)}`;

export const instruction = (amount: string, asset: string, chain: string) =>
  `Send exactly ${formatAmount(amount)} ${asset} on ${networkName(chain)}`;

const partialCopy = (received: string, amount: string, asset: string, remaining: string) =>
  `Received ${formatAmount(received)} of ${formatAmount(amount)} ${asset}. Send the remaining ${formatAmount(remaining)} to the same address.`;

/** Under the address and amount: whoever sends the transaction pays its network fee. */
export function selfSendFeeCopy(chain: string, solana: boolean): string {
  const native = nativeSymbol(chain);
  const fee = native === null ? `a little of the ${networkName(chain)} native token`
    : solana ? `a little ${native}` : `a little ${native} on ${networkName(chain)}`;
  return `You send this transaction yourself, so your wallet needs ${fee} for the network fee.`;
}

export const exchangeWarning = (amount: string, asset: string) =>
  `Sending from an exchange? Add its withdrawal fee so ${formatAmount(amount)} ${asset} arrives.`;

export interface SessionView {
  pill: Pill;
  /** The main instruction, only while the payer should send something. */
  instruction: string | null;
  /** The status line, announced to screen readers when it changes. */
  message: string;
  detail: string | null;
  payable: boolean;
  /** What is still due once pending payments confirm, in the asset's units. */
  due: string | null;
  exchangeWarning: string | null;
  /** The session closed (D45): no deposit details, no polling. */
  locked: boolean;
  /**
   * Past expiry with nothing listed, but a payment this page sent may still land: the page keeps
   * checking instead of locking, for SENT_GRACE_MS after expiry.
   */
  grace: boolean;
  /** A status this page does not know: nothing to pay, the page keeps polling until it does. */
  updating: boolean;
  /** Milliseconds left when inside the warning window of a payable session. */
  warnMs: number | null;
  /** A fixed-amount session paid above its amount: what was asked, sent and the difference. */
  overpaid: { amount: string; received: string; extra: string; unit: string } | null;
}

/** "Acme Store received your payment…", or "The merchant…" when the session names none. */
const receivedByCopy = (merchant: string | null) =>
  `${merchant ?? 'The merchant'} received your payment. You can close this page.`;

export const extraTitle = (extra: string, unit: string) => `About the extra ${formatAmount(extra)} ${unit}`;

export const extraCopy = (merchant: string | null) => {
  const who = merchant ?? 'the merchant';
  return `It went to ${who} with your payment. Prismaport never holds funds, so only ${who} can send it back. To ask for a refund, contact them and include this reference.`;
};

const DEFAULT_WARNING_SECONDS = 300;

/** How long after expiry the page keeps checking for a payment it sent before it locks. */
export const SENT_GRACE_MS = 10 * 60_000;

/** A signed payment Prismaport has not finished sending: it can still land after expiry. */
export const signedInFlight = (sp: SignedPayment): boolean => sp.status === 'pending' || sp.status === 'submitted';

export const warningCopy = (left: string) => `This payment closes in ${left}. Do not send funds after it closes.`;

const ZERO = '0';

function pendingAmount(payments: Payment[]): string {
  return addAmounts(ZERO, ...payments.filter((p) => p.status === 'detected').map((p) => p.amount));
}

/** The unit shown with session-level sums: the asset of the payments when they share one. */
function sumUnit(s: Session, sel: PayTo): string {
  const assets = new Set(s.payments.filter((p) => p.status !== 'reversed').map((p) => p.asset));
  return assets.size === 1 ? [...assets][0]! : sel.asset;
}

/**
 * Maps a session and the selected payment option to everything the page says. `sent`: this page
 * sent a payment that the session does not list yet, or has a wallet request open (or a submit of
 * unknown outcome) that may still send one: past expiry it keeps the page checking instead of locking.
 */
export function describeSession(s: Session, sel: PayTo, nowMs: number, sent = false): SessionView {
  const asset = sel.asset;
  const unit = sumUnit(s, sel);
  const base: SessionView = {
    pill: statusPill(s.status),
    instruction: null,
    message: COPY.waiting,
    detail: null,
    payable: false,
    due: null,
    exchangeWarning: null,
    locked: false,
    grace: false,
    updating: false,
    warnMs: null,
    overpaid: null,
  };
  // Closed to payments, unless a payment is confirming. A payment this page sent (or one Prismaport
  // is still sending) can land after expiry: locking then would invite a second one, so the page
  // keeps checking for a while first.
  const grace = (sent || s.signed_payments.some(signedInFlight)) && nowMs < Date.parse(s.expires_at) + SENT_GRACE_MS;
  const closed = (pending: boolean) => ({ locked: !pending && !grace, grace: !pending && grace });
  const received = s.amount_received;
  const hasReceived = compareAmounts(received, ZERO) > 0;
  if (s.open_amount || s.amount === null || s.amount_remaining === null) return describeOpenAmount(s, sel, nowMs, base, unit, hasReceived, closed);
  const amount = s.amount;

  switch (s.status) {
    case 'paid':
      return { ...base, message: COPY.paid };
    case 'overpaid':
      return {
        ...base,
        message: COPY.complete,
        detail: receivedByCopy(s.merchant?.name || null),
        overpaid: { amount, received, extra: formatAmount(subtractAmounts(received, amount)), unit },
      };
    case 'late_payment':
      return { ...base, message: COPY.late, ...closed(hasPendingPayment(s)) };
    case 'under_review':
      return { ...base, message: COPY.review };
    case 'expired':
      return { ...base, message: expiredCopy(s, amount, unit, hasReceived), ...closed(hasPendingPayment(s)) };
    case 'open':
    case 'partially_paid':
      break;
    default:
      // The API and the page do not deploy in lockstep: never guess that an unknown status is payable.
      return { ...base, message: COPY.updating, updating: true };
  }

  const pending = pendingAmount(s.payments);
  const hasPending = compareAmounts(pending, ZERO) > 0;
  const due = subtractAmounts(s.amount_remaining, pending);
  if (compareAmounts(due, ZERO) === 0) {
    return { ...base, message: COPY.confirming };
  }
  if (nowMs >= Date.parse(s.expires_at)) {
    if (hasPending) return { ...base, message: COPY.confirming };
    return { ...base, pill: STATUS_PILL.expired, message: expiredCopy(s, amount, unit, hasReceived), ...closed(false) };
  }
  const started = hasReceived || hasPending || s.status === 'partially_paid';
  let message: string = COPY.waiting;
  if (hasPending) {
    message = `A payment of ${formatAmount(pending)} ${unit} is confirming. Send the remaining ${formatAmount(due)} to the same address.`;
  } else if (started) {
    message = partialCopy(received, amount, asset, due);
  }
  return {
    ...base,
    instruction: instruction(started ? due : sel.amount ?? amount, asset, sel.chain),
    message,
    payable: true,
    due,
    warnMs: warnLeft(s, nowMs),
    exchangeWarning: exchangeWarning(due, asset),
  };
}

function hasPendingPayment(s: Session): boolean {
  return compareAmounts(pendingAmount(s.payments), ZERO) > 0;
}

function warnLeft(s: Session, nowMs: number): number | null {
  const left = Date.parse(s.expires_at) - nowMs;
  const window = (s.expiry_warning_seconds ?? DEFAULT_WARNING_SECONDS) * 1000;
  return left > 0 && left <= window ? left : null;
}

function expiredCopy(s: Session, amount: string, unit: string, hasReceived: boolean): string {
  if (!hasReceived) return COPY.expired;
  return `This session expired after receiving ${formatAmount(s.amount_received)} of ${formatAmount(amount)} ${unit}. Contact the merchant about this payment.`;
}

/**
 * An open-amount session (a recharge on a customer address): there is no amount to match, so
 * nothing is "due". It is open until a payment counts, then paid; it is never partially paid.
 * There is no wallet path (both need an amount): only the address and its QR.
 */
function describeOpenAmount(s: Session, sel: PayTo, nowMs: number, base: SessionView, unit: string, hasReceived: boolean,
  closed: (pending: boolean) => Pick<SessionView, 'locked' | 'grace'>): SessionView {
  const received = formatAmount(s.amount_received);
  const pending = pendingAmount(s.payments);
  const hasPending = compareAmounts(pending, ZERO) > 0;
  const expired = hasReceived
    ? `This payment session expired after receiving ${received} ${unit}, too little to count after the Prismaport fee. Contact the merchant about this payment.`
    : COPY.expiredAnyAmount;
  switch (s.status) {
    case 'paid':
    case 'overpaid':
      return { ...base, message: COPY.paid, detail: `You sent ${received} ${unit}.` };
    case 'late_payment':
      return { ...base, message: COPY.late, ...closed(hasPending) };
    case 'under_review':
      return { ...base, message: COPY.review };
    case 'expired':
      return { ...base, message: expired, ...closed(hasPending) };
    case 'open':
    case 'partially_paid':
      break;
    default:
      return { ...base, message: COPY.updating, updating: true };
  }
  if (nowMs >= Date.parse(s.expires_at)) {
    if (hasPending) return { ...base, message: COPY.confirming };
    return { ...base, pill: STATUS_PILL.expired, message: expired, ...closed(false) };
  }
  return {
    ...base,
    instruction: anyAmountInstruction(sel.asset, sel.chain),
    message: hasPending ? `A payment of ${formatAmount(pending)} ${unit} is confirming. Keep this page open.` : COPY.waiting,
    detail: COPY.anyAmountDetail,
    payable: true,
    due: null,
    warnMs: warnLeft(s, nowMs),
  };
}

export function reversedNote(p: Payment): string {
  return `A payment of ${formatAmount(p.amount)} ${p.asset} was reversed by the network and no longer counts.`;
}

/** The latest signed payment for this option decides what the wallet panel says. */
export function signedCopy(sp: SignedPayment): { text: string; done: boolean; failed: boolean } {
  switch (sp.status) {
    case 'pending':
    case 'submitted':
      return { text: COPY.signedSending, done: false, failed: false };
    case 'done':
      if (sp.outcome === 'settled') return { text: 'Payment sent. Waiting for confirmations.', done: true, failed: false };
      return { text: 'This signed payment did not go through. You can sign again.', done: false, failed: true };
    case 'failed':
      return { text: 'This signed payment did not go through. You can sign again.', done: false, failed: true };
    default:
      // A state this page does not know: claim no outcome and offer no second signature.
      return { text: 'Payment signed. We are checking how it went. Keep this page open.', done: false, failed: false };
  }
}

/** "This payment stays open for 30 minutes once generated." from the session's pay window. */
export function payWindowCopy(seconds: number | undefined): string {
  if (!seconds || seconds <= 0) return 'This payment stays open for a limited time once generated.';
  let span: string;
  if (seconds < 60) span = `${seconds} second${seconds === 1 ? '' : 's'}`;
  else if (seconds % 3600 === 0) span = `${seconds / 3600} hour${seconds === 3600 ? '' : 's'}`;
  else {
    const m = Math.round(seconds / 60);
    span = `${m} minute${m === 1 ? '' : 's'}`;
  }
  return `This payment stays open for ${span} once generated.`;
}

/** Errors of "Generate payment": the generic copy talks about "the address below", which does not exist yet. */
/** D64: a paused chain or asset. Fixed text; the address still works for money already sent. */
export const PAUSED = {
  tag: 'Paused',
  title: (network: string): string => `${network} is paused`,
  panel: (network: string): string => `Payments on ${network} are paused right now. If you haven't sent yet, pay on another network. Funds already sent to this address will still be credited.`,
  panelOnly: (network: string): string => `Payments on ${network} are paused right now. Funds already sent to this address will still be credited.`,
  action: 'Pay on another network',
  reveal: 'This network is paused right now. Choose another network.',
  unavailable: 'Payments are paused right now. Try again later.',
  addressRow: (network: string): string => `Payments on ${network} are paused right now. Funds already sent to this address will still be credited.`,
};

export function revealErrorCopy(code: string, retryAfter?: number): string {
  switch (code) {
    case 'session_expired':
      return 'This payment session has closed. Ask the merchant for a new payment link.';
    case 'invalid_request':
      return 'This network is not available for this payment. Choose another one.';
    case 'wallet_blacklisted':
      return 'This merchant cannot receive payments on this network right now. Choose another network or contact the merchant.';
    case 'chain_unavailable':
      return PAUSED.reveal;
    case 'service_unavailable':
      return 'Cannot generate this payment right now. Try again in a moment.';
    default:
      return errorCopy(code, retryAfter);
  }
}

/** Plain-language messages for checkout API errors. */
export function errorCopy(code: string, retryAfter?: number, status?: number): string {
  switch (code) {
    case 'session_not_found':
    case 'payment_link_not_found':
    case 'product_not_found':
      return COPY.notFound;
    case 'customer_address_not_found':
      return COPY.addressNotFound;
    case 'address_rotated':
      return COPY.addressReplaced;
    case 'product_inactive':
    case 'link_unavailable':
      return 'This link is no longer available. Contact the merchant if you need help.';
    case 'chain_unavailable':
      return PAUSED.unavailable;
    case 'session_not_payable':
      return 'This session no longer accepts payments. Reload the page to see its status.';
    case 'amount_below_minimum':
      return 'The amount is too small to pay this way. Send it to the address below instead.';
    case 'amount_above_maximum':
      return 'The amount is above the maximum for this link.';
    case 'invalid_signature':
      return 'The signature did not match this payment. Nothing was sent. Try again.';
    case 'authorization_rejected':
      return 'The network rejected this payment, often for a low balance. Nothing was sent. Check your balance and try again.';
    case 'service_unavailable':
      return 'This payment method is not available right now. Use the address below instead.';
    case 'destination_not_ready':
      return 'This merchant cannot receive this token from a connected wallet yet. Send it to the address below instead.';
    case 'payment_in_progress':
      return 'A payment for this checkout is already in progress. If you cancelled it, wait a minute and try again.';
    case 'rate_limited':
      return `Too many attempts from this connection. Try again in ${retryAfter ?? 60} seconds.`;
    case 'network_error':
      return COPY.offline;
    case 'not_found':
      return COPY.notFound;
    case 'session_expired':
    case 'expired':
      return COPY.expired;
    case 'invalid_request':
      return 'Some of the details were not accepted. Check what you entered and try again.';
    case 'invalid_state':
      return 'This payment cannot be changed in its current state. Reload the page to see its status.';
    default:
      // The API sends no request id, so the code (and HTTP status) is the reference to quote to support.
      return `Something went wrong on our side. Try again in a moment. Reference: ${status ? `${status} ` : ''}${code}.`;
  }
}
