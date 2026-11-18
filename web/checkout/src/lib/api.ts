export type SessionStatus =
  | 'open'
  | 'partially_paid'
  | 'paid'
  | 'overpaid'
  | 'expired'
  | 'late_payment'
  | 'under_review';

export type PaymentStatus = 'detected' | 'confirmed' | 'finalized' | 'reversed';

export interface PayTo {
  chain: string;
  asset: string;
  /** Absent once the session has closed (D45): the server stops serving deposit details at expiry. */
  address?: string;
  /** Absent once the session has closed (D45), like the address, and on an open-amount session (any amount). */
  amount?: string;
  signed_payment: boolean;
  /** Set when the backend serves server-built Solana transactions for this entry. */
  connected_wallet?: boolean;
  /** Solana: the deposit token account (the PDA's ATA for this mint), an alternative target. */
  token_account?: string;
  /** Solana: the session's reference key, for a Solana Pay transfer request. */
  reference?: string;
  /**
   * Whether the payer generated this network's deposit details (D48). Every entry of a network
   * shares it. Absent on an older API, where a served address means it was generated.
   */
  revealed?: boolean;
  /** D64: the chain or this asset is paused. The address still works, but new payments are refused. */
  paused?: boolean;
}

interface SolanaTransactionResponse {
  session_id: string;
  chain: string;
  asset: string;
  amount: string;
  fee_payer: string;
  reference: string;
  transaction: string;
  last_valid_block_height: number;
}

export interface Payment {
  chain: string;
  asset: string;
  amount: string;
  status: PaymentStatus;
  tx_hash: string;
  /** When it was confirmed; null while confirming. Absent from an API older than this page. */
  confirmed_at?: string | null;
  /** A detected payment's depth on a network that counts blocks, at most confirmations_required. */
  confirmations?: number;
  confirmations_required?: number;
}

export interface SignedPayment {
  id: string;
  chain: string;
  asset: string;
  status: 'pending' | 'submitted' | 'done' | 'failed';
  outcome: string | null;
  tx_hash: string | null;
}

export interface Merchant {
  name: string;
  logo_url: string;
}

export interface Session {
  id: string;
  livemode: boolean;
  status: SessionStatus;
  /** Null on an open-amount session: the payer sends any amount. */
  amount: string | null;
  /** True on an open-amount session (a recharge on a customer address). */
  open_amount?: boolean;
  currency: string;
  amount_received: string;
  /** Null on an open-amount session: whatever arrives counts. */
  amount_remaining: string | null;
  product: { name: string } | null;
  /** Set when the merchant uploaded a logo; `logo_url` is a path on the API origin. */
  merchant: Merchant | null;
  pay_to: PayTo[];
  payments: Payment[];
  signed_payments: SignedPayment[];
  /** Before the first "Generate payment" this is the selection deadline; after it, the pay deadline. */
  expires_at: string;
  /** When the payer first generated a payment; null until then. */
  payment_started_at?: string | null;
  /** How long the session stays open once the first payment is generated. */
  pay_window_seconds?: number;
  success_url: string | null;
  /** Seconds before expires_at at which the page starts warning (default 300). */
  expiry_warning_seconds?: number;
  /** The payment link this session came from, when it came from one. */
  link_id?: string | null;
  /** Whether that link still takes payments; when absent the page asks GET /links/{id}. */
  link_active?: boolean;
  /** The customer address the session was opened on; a closed page sends the payer back to /a/{id}. */
  customer_address_id?: string | null;
}

export interface NetworkAsset {
  symbol: string;
  token: string;
  decimals: number;
  eip3009: boolean;
  paused?: boolean;
}

export interface Network {
  id: string;
  family: 'evm' | 'solana';
  livemode: boolean;
  evm_chain_id: number | null;
  fee_bps: number;
  /** The fixed part of the fee in USD, charged on top of fee_bps on every settlement. */
  fixed_fee: string;
  assets: NetworkAsset[];
  paused?: boolean;
}

/**
 * A payment link as the payer meets it: the product's name and description at the link's
 * effective price (a link that sets an amount answers `fixed`). Never the label or the merchant.
 */
export interface PublicLink {
  id: string;
  livemode: boolean;
  name: string;
  description?: string | null;
  price_type: 'fixed' | 'open';
  amount: string | null;
  min_amount: string | null;
  max_amount: string | null;
  suggested_amounts: string[];
  currency: string;
  active: boolean;
  live?: boolean;
  unavailable_reason?: string | null;
  merchant: Merchant | null;
}

/** The customer's deposit address on one network: always the same. */
interface AddressPayTo {
  chain: string;
  asset: string;
  address: string;
  token_account?: string;
  paused?: boolean;
}

/**
 * A static customer address as the payer meets it on /a/{id}: the product, the price rule of the
 * sessions its button opens, the deposit addresses (only while it is live) and the session the
 * button resumes. Never the merchant's customer id, metadata or payout.
 */
export interface PublicAddress {
  id: string;
  livemode: boolean;
  status: 'active' | 'rotated';
  replaced_by: string | null;
  live: boolean;
  unavailable_reason: 'rotated' | 'product_inactive' | 'no_networks' | null;
  product: { name: string; description: string | null };
  price_type: 'fixed' | 'open';
  amount: string | null;
  open_amount: boolean;
  currency: string;
  pay_to: AddressPayTo[];
  session: { id: string; status: 'open' | 'partially_paid'; expires_at: string } | null;
  merchant: Merchant | null;
}

export interface TypedDataResponse {
  session_id: string;
  chain: string;
  asset: string;
  amount: string;
  gateway: string;
  typed_data: {
    types: Record<string, { name: string; type: string }[]>;
    primaryType: string;
    domain: { name: string; version: string; chainId: number; verifyingContract: string };
    message: {
      from: string;
      to: string;
      value: string;
      validAfter: string;
      validBefore: string;
      nonce: string;
    };
  };
}

/** The steps the hosted checkout's Simulate panel can run on a test-mode session. */
export type TestPanelAction = 'pay_exact' | 'pay_partial' | 'overpay' | 'expire' | 'settle';

export interface TestPanelResult {
  action: TestPanelAction;
  events: { type: string; session_version: number }[];
  settled: boolean;
  session: Session;
}

export interface SubmitRequest {
  chain: string;
  asset: string;
  from: string;
  value: string;
  valid_after: string;
  valid_before: string;
  signature: string;
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly retryAfter?: number,
  ) {
    super(message);
  }
}

export type Fetch = typeof fetch;

/** How long one request may take before it counts as a failed connection: a hung request must not stall the polling. */
export const REQUEST_TIMEOUT_MS = 10_000;

/**
 * A submit runs an RPC call before it answers, and once it reached the server the payment may go
 * through: it gets more time before the page gives up on it.
 */
export const SUBMIT_TIMEOUT_MS = 60_000;

/**
 * Throws an AbortError once `signal` aborted. AbortSignal.throwIfAborted is missing before Chrome
 * 100, Safari 15.4 and Firefox 97, where calling it would fail every wallet payment.
 */
export function throwIfAborted(signal?: AbortSignal): void {
  if (signal?.aborted) throw new DOMException('Aborted', 'AbortError');
}

/** An AbortSignal that fires after `ms`; none on a browser without AbortSignal.timeout. */
function timeoutSignal(ms: number): AbortSignal | undefined {
  return typeof AbortSignal !== 'undefined' && typeof AbortSignal.timeout === 'function' ? AbortSignal.timeout(ms) : undefined;
}

export interface ApiResult<T> {
  data: T;
  /** Server time from the Date header, to correct the countdown for a wrong local clock. */
  serverTime: number | null;
}

export class CheckoutApi {
  constructor(
    private readonly base = '',
    private readonly fetchFn: Fetch = (...args) => fetch(...args),
  ) {}

  /** A path on the API as a URL on the API origin (for `<img src>`; the fetches add the same base). */
  url(path: string): string {
    return this.base + path;
  }

  private async call<T>(method: string, path: string, body?: unknown, headers: Record<string, string> = {}, timeoutMs = REQUEST_TIMEOUT_MS): Promise<ApiResult<T>> {
    let res: Response;
    let text: string;
    try {
      res = await this.fetchFn(this.base + path, {
        method,
        headers: body === undefined ? headers : { 'Content-Type': 'application/json', ...headers },
        body: body === undefined ? undefined : JSON.stringify(body),
        cache: 'no-store',
        credentials: 'omit',
        signal: timeoutSignal(timeoutMs),
      });
      // A body cut off mid-stream (or past the timeout) is a failed connection too, not a wallet error.
      text = await res.text();
    } catch {
      throw new ApiError(0, 'network_error', 'The connection failed.');
    }
    let json: unknown = null;
    try {
      json = text ? JSON.parse(text) : null;
    } catch {
      json = null;
    }
    if (!res.ok) {
      const err = (json as { error?: { code?: string; message?: string } } | null)?.error;
      const retry = Number(res.headers.get('Retry-After'));
      throw new ApiError(res.status, err?.code ?? 'http_error', err?.message ?? `HTTP ${res.status}`, Number.isFinite(retry) && retry > 0 ? retry : undefined);
    }
    const date = Date.parse(res.headers.get('Date') ?? '');
    return { data: json as T, serverTime: Number.isNaN(date) ? null : date };
  }

  /** `active` marks the request as coming from a visible page (payer presence). */
  session(id: string, active = false) {
    return this.call<Session>('GET', `/v1/checkout/sessions/${encodeURIComponent(id)}${active ? '?active=1' : ''}`);
  }

  networks() {
    return this.call<{ data: Network[] }>('GET', '/v1/checkout/networks');
  }

  link(id: string) {
    return this.call<PublicLink>('GET', `/v1/checkout/links/${encodeURIComponent(id)}`);
  }

  openSession(linkId: string, idempotencyKey: string, amount?: string, clientReference?: string) {
    const body: { amount?: string; client_reference_id?: string } = {};
    if (amount !== undefined) body.amount = amount;
    if (clientReference !== undefined) body.client_reference_id = clientReference;
    return this.call<Session>('POST', `/v1/checkout/links/${encodeURIComponent(linkId)}/sessions`, body, { 'Idempotency-Key': idempotencyKey });
  }

  /** `active` marks the request as coming from a visible page (payer presence on the session it resumes). */
  address(id: string, active = false) {
    return this.call<PublicAddress>('GET', `/v1/checkout/addresses/${encodeURIComponent(id)}${active ? '?active=1' : ''}`);
  }

  /** Opens a session on the address, or answers the one it has open. */
  openAddressSession(id: string) {
    return this.call<Session>('POST', `/v1/checkout/addresses/${encodeURIComponent(id)}/sessions`, {});
  }

  typedData(id: string, req: { chain: string; asset: string; from: string }) {
    return this.call<TypedDataResponse>('POST', `/v1/checkout/sessions/${encodeURIComponent(id)}/eip3009/typed_data`, req);
  }

  /** Generates the deposit details of one network (D48). Repeating it for a generated network returns the same details. */
  reveal(id: string, chain: string) {
    return this.call<Session>('POST', `/v1/checkout/sessions/${encodeURIComponent(id)}/reveal`, { chain });
  }

  solanaTransaction(id: string, req: { chain: string; asset: string; account: string }) {
    return this.call<SolanaTransactionResponse>('POST', `/v1/checkout/sessions/${encodeURIComponent(id)}/solana/transaction`, req);
  }

  /** Test mode only: the server refuses live sessions. */
  simulate(id: string, req: { action: TestPanelAction; chain?: string; asset?: string; amount?: string }) {
    return this.call<TestPanelResult>('POST', `/v1/checkout/sessions/${encodeURIComponent(id)}/test_helpers/simulate`, req);
  }

  submit(id: string, req: SubmitRequest) {
    return this.call<{ id: string; session_id: string; status: string }>('POST', `/v1/checkout/sessions/${encodeURIComponent(id)}/eip3009/submit`, req, {}, SUBMIT_TIMEOUT_MS);
  }
}
