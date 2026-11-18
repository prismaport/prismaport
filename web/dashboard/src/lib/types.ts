// Wire types of /v1/dashboard. Amounts are decimal strings, never numbers.

export type Mode = 'live' | 'test';

export interface User {
  id: string;
  email: string;
  totp_enabled: boolean;
  /** False until the emailed code was entered: only the confirmation screen opens. */
  email_verified?: boolean;
  /** Recovery codes still unused (of 10). Absent from an API older than recovery codes. */
  recovery_codes_left?: number;
  /** When a lost-app request removes 2FA (RFC 3339); null or absent when none is pending. */
  totp_removal_at?: string | null;
  /** While in the future, adding a live wallet and creating or rolling a live API key are refused. */
  recovery_locked_until?: string | null;
}

/** D64: a chain, or one asset on it, is paused for new payments. Already-sent money still lands. */
export interface ChainPause {
  chain: string;
  chain_name: string;
  /** Null when the whole chain is paused. */
  asset: string | null;
  livemode: boolean;
  paused_at: string;
}

export interface Me {
  user: User;
  /** live_enabled is false while the account's live mode is closed (older self-serve accounts): Live mode is read-only. */
  merchant: { id: string; name: string; live_enabled?: boolean; /** False until the merchant has an active live wallet. Absent from an API older than this page. */ has_live_wallet?: boolean; /** The checkout logo, relative to the API origin; null when none is set. */ logo_url?: string | null };
  csrf_token: string;
  /** The session's open confirmation window (D59): covered Live actions take no code until it ends. Null when closed; absent from an older API. */
  confirmation_window?: ConfirmationWindow | null;
  /** How long Unlock opens the window for (D61); 0 when the window is off. Absent from an older API. */
  confirmation_window_seconds?: number;
  /** Pauses that touch this merchant, both modes; absent from an older API. */
  chain_pauses?: ChainPause[];
}

export interface ConfirmationWindow {
  expires_at: string;
  /** Live wallets this window can still add without a code. */
  wallets_left: number;
}

/** How a product is priced; it cannot change after creation. `open` is "Variable price" in the UI. */
export type PriceType = 'fixed' | 'open';
/** Why a link opens no sessions, in the server's precedence order. */
export type UnavailableReason = 'product_inactive' | 'inactive' | 'expired' | 'exhausted' | 'reserved';

export interface AssetAmount {
  asset: string;
  amount: string;
}

export interface LinkStats {
  opened: number;
  paid: number;
  received: AssetAmount[];
  last_paid_at: string | null;
  /** Paid / opened as a fraction (0.5) or a decimal string ("0.5"). */
  conversion: number | string | null;
}

export interface ProductStats extends LinkStats {
  /** Sessions still open or partially paid: they keep the price and payout they opened with. */
  open: number;
}

/** The catalog item: what is sold, how it is priced and where the money goes. */
export interface Product {
  id: string;
  livemode?: boolean;
  name: string;
  /** Plain text, shown on the checkout under the name; null when there is none. */
  description: string | null;
  price_type: PriceType;
  amount: string | null;
  min_amount: string | null;
  max_amount: string | null;
  suggested_amounts: string[];
  currency: 'USD';
  /** Every network of the product's mode by chain id, in payout order; [] = not taken. */
  payout?: LinkPayout;
  /** One entry per network the product takes, in network order: all of the stablecoins on in Settings, or only the listed ones. */
  networks?: ProductNetwork[];
  /** What payers can pay with right now: Settings and the product's choice together. Can be empty. */
  payment_methods?: PaymentMethod[];
  /** Defaults for every session of the product; a link can override the return URL and add metadata. */
  success_url: string | null;
  metadata: Record<string, string>;
  active: boolean;
  /** Summed over every session of the product, from links and the API alike. */
  stats: ProductStats;
  links: { live: number; total: number };
  created_at: string;
  updated_at: string;
}

/** A network a product takes. `assets` is left out for `all`, and lists at least one stablecoin for `some`. */
export interface ProductNetwork {
  chain: string;
  chain_name: string;
  accept: 'all' | 'some';
  assets?: string[];
}

export interface PaymentMethod {
  chain: string;
  chain_name: string;
  asset: string;
}

/** What a product sends for each network: `none` drops it, `assets` goes only with `some`. */
export type NetworksInput = Record<string, { accept: 'all' | 'some' | 'none'; assets?: string[] | null }>;

export interface ProductInput {
  name: string;
  description?: string | null;
  price_type: PriceType;
  amount?: string | null;
  min_amount?: string | null;
  max_amount?: string | null;
  suggested_amounts?: string[];
  success_url?: string | null;
  metadata?: Record<string, string>;
  /** By chain id. On create a network left out gets its family's oldest active wallet at 100%; on PATCH it keeps its list. */
  payout?: PayoutInput;
  /** By chain id: which stablecoins the product takes on each network. On PATCH only the networks named change. */
  networks?: NetworksInput;
}

/** What a money edit does to the product's active links. */
export type LinksDecision = 'keep' | 'deactivate';

export type ProductPatch = Partial<Omit<ProductInput, 'price_type'>> & { active?: boolean; links?: LinksDecision };

/** The product a link sells, as the link embeds it. */
interface ProductSummary {
  id: string;
  name: string;
  price_type: PriceType;
  amount: string | null;
  min_amount: string | null;
  max_amount: string | null;
  active: boolean;
}

/** A payment link: a public URL that sells one product. */
export interface Link {
  id: string;
  livemode?: boolean;
  product_id: string;
  /** Internal label, never shown to the payer; null means the UI shows the product name. */
  name: string | null;
  /** Set only on a link of a variable product that fixes the amount for that link. */
  amount: string | null;
  active: boolean;
  live: boolean;
  unavailable_reason: UnavailableReason | null;
  url: string;
  /** Overrides the product's return URL; null uses the product's. */
  success_url: string | null;
  /** Merged over the product's metadata on every session, the link winning per key. */
  metadata: Record<string, string>;
  expires_at: string | null;
  max_payments: number | null;
  stats: LinkStats;
  product: ProductSummary;
  created_at: string;
  updated_at: string;
}

export interface LinkInput {
  product_id: string;
  name?: string | null;
  amount?: string | null;
  success_url?: string | null;
  metadata?: Record<string, string>;
  expires_at?: string | null;
  max_payments?: number | null;
}

/** `null` clears the label, amount, expiry, limit or return URL. */
export type LinkPatch = Partial<Omit<LinkInput, 'product_id'>> & { active?: boolean };

export interface Page<T> {
  data: T[];
  next_cursor: string | null;
}

export type SessionStatus = 'open' | 'partially_paid' | 'paid' | 'overpaid' | 'expired' | 'late_payment' | 'under_review';
export type PaymentStatus = 'detected' | 'confirmed' | 'finalized' | 'reversed';

export interface PayTo {
  chain: string;
  asset: string;
  /** Absent until the payer generates the payment on this network (D48). */
  address?: string;
  /** Null on an open-amount session: the payer decides how much to send. */
  amount: string | null;
  token_account?: string;
  /** Whether the deposit details of this network were generated. Absent on an older API: an address means yes. */
  revealed?: boolean;
  revealed_at?: string | null;
}

export interface Payment {
  id: string;
  chain: string;
  asset: string;
  amount: string;
  tx_hash: string;
  status: PaymentStatus;
  confirmed_at: string | null;
  explorer_url?: string;
  /** A synthetic test-mode payment made by the simulator; its tx_hash is not on any chain. */
  simulated?: boolean;
}

/** One recipient's share of a settlement, in the token's decimals. A recipient that is no wallet of the merchant any more has no id or label. */
export interface Payout {
  wallet_id: string | null;
  label: string | null;
  address: string;
  amount: string;
}

export interface Settlement {
  chain: string;
  asset: string;
  gross: string;
  fee: string;
  net: string;
  /** One row per wallet paid; fee + sum(amount) equals gross. Absent on settlements made before payout splits. */
  payouts?: Payout[];
  tx_hash: string;
  settled_at: string;
  explorer_url?: string;
  /** A test-mode settlement made by the simulator: its hash is on no chain. */
  simulated?: boolean;
}

/** The recipients a session committed to when it opened, per chain id it offers. */
export type SessionPayout = Record<string, { wallet_id: string | null; label?: string | null; address: string; share?: string; bps: number }[]>;

/** The merchant API session shape (internal/view.Session). */
export interface Session {
  id: string;
  version: number;
  livemode: boolean;
  status: SessionStatus;
  product_id: string | null;
  /** The link the session was opened from; null for sessions created with the merchant API. */
  link_id?: string | null;
  /** The customer address the session was opened on (B11); null otherwise. */
  customer_address_id?: string | null;
  /** Null on an open-amount session (a recharge on a customer address): the payer decides how much to send. */
  amount: string | null;
  currency: 'USD';
  amount_received: string;
  amount_remaining: string;
  checkout_url: string;
  pay_to: PayTo[];
  payments: Payment[];
  settlement: Settlement | null;
  settlements?: Settlement[];
  metadata: Record<string, string>;
  /** Correlation value: set by the merchant's server (trusted) or taken from a link URL (the payer's, untrusted). */
  client_reference_id: string | null;
  client_reference_source: ClientReferenceSource | null;
  /** Before the first generated payment this is the selection deadline; after it, the pay deadline. */
  expires_at: string;
  /** When the payer first generated a payment; null until then. */
  payment_started_at?: string | null;
  /** End of the automatic watch window; when absent the page uses expires_at + 23 h 30 min. */
  watch_until?: string | null;
  /** Whether "Check again" is accepted now (D47: also for a paid session whose payments are final). */
  recheck_available?: boolean;
  created_at: string;
  paid_at: string | null;
  /** The link's current label, or its product's name; null for API-created sessions. */
  link_name: string | null;
  /** The product's current name. */
  product_name?: string | null;
  payout?: SessionPayout;
}

export type ClientReferenceSource = 'merchant' | 'payer';

export interface SessionEvent {
  id: string;
  type: string;
  created_at: string;
  session_version?: number;
  seq?: number | null;
}

/** A block explorer link for one transaction (payment or settlement) or deposit address. */
export interface ExplorerLink {
  type: 'payment' | 'settlement' | 'address';
  chain: string;
  asset: string;
  payment_id?: string;
  /** The transaction hash or address the link points at. */
  value: string;
  url: string;
}

export interface SessionDetail extends Session {
  events: SessionEvent[];
  explorer_links?: ExplorerLink[];
  /** "Check again" can run now (past the watch window, and not superseded on its customer address). */
  recheck_available?: boolean;
  recheck_requested_at?: string | null;
  /** On a session of a customer address: the newer session that watches the address now. */
  superseded_by?: string | null;
  /** Flushes whose funds another session's flush of the same customer address moved (one settlement, on that session). */
  settled_with?: SettledWith[];
}

export interface SettledWith {
  chain: string;
  asset: string;
  session_id: string;
  tx_hash: string;
}

type CustomerAddressStatus = 'active' | 'rotated';

type AttentionReasonCode = 'product_inactive' | 'archived_wallet' | 'payout_changed' | 'fee_changed' | 'new_networks' | 'no_networks';

/** One thing a rotation would change on an active customer address. */
export interface AddressAttention {
  reason: AttentionReasonCode;
  chains?: string[];
  wallets?: { id: string; label: string }[];
  assets?: { chain: string; asset: string }[];
}

/** A static deposit address of one customer (B11): the merchant API's object plus its product, stats and attention. */
export interface CustomerAddress {
  id: string;
  livemode: boolean;
  customer_id: string;
  product_id: string;
  status: CustomerAddressStatus;
  replaced_by: string | null;
  pay_to: { chain: string; asset: string; address: string; token_account?: string }[];
  payout: SessionPayout;
  metadata: Record<string, string>;
  /** The public address page, /a/{id}. */
  url: string;
  created_at: string;
  rotated_at: string | null;
  product: { id: string; name: string; price_type: PriceType; active: boolean } | null;
  stats: {
    sessions: number;
    paid: number;
    received: AssetAmount[];
    last_paid_at: string | null;
    last_session: { id: string; status: SessionStatus; created_at: string } | null;
  };
  attention: AddressAttention[];
}

export interface OverviewTotal {
  chain: string;
  asset: string;
  received: string;
  fees: string;
  net: string;
}

type AttentionReason = 'expiring' | 'exhausted' | 'inactive_with_open_sessions' | 'product_inactive';

/** One transfer that arrived, for the overview. */
export interface RecentPayment {
  id: string;
  session_id: string;
  link_id: string | null;
  link_name: string | null;
  chain: string;
  asset: string;
  amount: string;
  status: PaymentStatus;
  late: boolean;
  tx_hash: string;
  explorer_url: string | null;
  detected_at: string;
  confirmed_at: string | null;
  simulated?: boolean;
}

/** What was paid out to one wallet, per network and stablecoin. */
export interface WalletTotal {
  wallet_id: string | null;
  label: string | null;
  family?: Family;
  address: string;
  chain: string;
  asset: string;
  amount: string;
}

export interface Overview {
  totals: OverviewTotal[];
  totals_by_wallet?: WalletTotal[];
  recent_payments: RecentPayment[];
  links_attention: { id: string; name: string; reason: AttentionReason }[];
}

type ApiKeyStatus = 'active' | 'expiring' | 'expired' | 'revoked';

export interface ApiKey {
  id: string;
  /** Keys made with the CLI may have no name. */
  name: string | null;
  scopes: string[];
  mode?: Mode;
  livemode?: boolean;
  prefix: string;
  created_at: string;
  /** Precise to a minute; null until the key is first used. */
  last_used_at: string | null;
  revoked_at: string | null;
  /** When a rolled key stops working. */
  expires_at: string | null;
  /** The key a roll issued in place of this one. */
  replaced_by: string | null;
  status: ApiKeyStatus;
}

export interface CreatedApiKey extends ApiKey {
  secret: string;
}

export type RollOverlap = 'now' | '1h' | '24h' | '7d';

export interface RolledApiKey {
  api_key: CreatedApiKey;
  previous: ApiKey;
}

export interface WebhookEndpoint {
  id: string;
  url: string;
  description?: string | null;
  enabled_events: string[];
  livemode?: boolean;
  created_at: string;
  /** `disabled` while paused by the merchant or switched off after repeated failures. */
  status?: 'enabled' | 'disabled';
  disabled_reason?: 'merchant' | 'failing' | null;
  disabled_at?: string | null;
  /** Start of the open failure streak; kept while the endpoint is disabled, cleared by enable. */
  failing_since?: string | null;
  /** Set while a rotated-out secret still signs deliveries (24 hours after a rotation). */
  previous_secret_expires_at?: string | null;
}

/** PATCH /webhook_endpoints/{id}: only the fields that change. `description: null` clears it. */
export interface WebhookEndpointPatch {
  url?: string;
  enabled_events?: string[];
  description?: string | null;
  status?: 'enabled' | 'disabled';
}

export interface WebhookEndpointWithSecret extends WebhookEndpoint {
  secret: string;
}

export type DeliveryStatus = 'pending' | 'succeeded' | 'dead';

export interface WebhookDelivery {
  id: string;
  endpoint_id: string;
  event_id: string;
  event_type: string;
  session_id?: string | null;
  status: DeliveryStatus;
  attempts: number;
  last_status_code: number | null;
  last_error: string | null;
  last_attempt_at: string | null;
  next_attempt_at: string | null;
  delivered_at: string | null;
  created_at: string;
  /** A "Send test event" delivery: sent once, never retried or resent. */
  test?: boolean;
}

export interface HeaderPair {
  name: string;
  value: string;
}

/** POST /webhook_endpoints/{id}/test: the exact request sent and what came back. */
export interface TestEventResult {
  event: { id: string; type: string };
  delivery: WebhookDelivery;
  request: { method: string; url: string; headers: HeaderPair[]; body: string };
  response: { status: number; latency_ms: number; body: string; body_truncated: boolean } | null;
  error: string | null;
}

export interface SandboxInbox {
  id: string;
  url: string;
  endpoint_id: string;
  created_at: string;
  retention_days: number;
  max_requests: number;
  max_body_bytes: number;
  /** The endpoint's signing secret, only in the response that created the inbox. */
  secret?: string;
}

export type SignatureFailure = 'missing' | 'malformed' | 'no_match' | 'timestamp_outside_tolerance';

export interface InboxRequest {
  id: string;
  received_at: string;
  method: string;
  headers: HeaderPair[];
  body: string;
  size: number;
  event_id: string | null;
  event_type: string | null;
  signature: { valid: boolean; error: SignatureFailure | null; timestamp: string | null; skew_seconds: number | null };
}

export type SimAction = 'pay_exact' | 'pay_partial' | 'overpay' | 'expire' | 'late_payment' | 'reverse_last_payment' | 'under_review';

export interface SimulateResult {
  action: SimAction;
  events: { id: string; type: string; session_version: number }[];
  session: Session;
}

export type Family = 'evm' | 'solana';

export interface Wallet {
  id: string;
  label: string;
  family: Family;
  address: string;
  verified: boolean;
  archived_at: string | null;
  created_at: string;
  livemode?: boolean;
  /** Where new products pay on this wallet's network; one active wallet per family. An older API omits it: the oldest active wallet is the default then. */
  default?: boolean;
  /** How many products (active or not) pay this wallet; it cannot be archived while any does. */
  used_by_products?: number;
}

/** One recipient of a product's payout: a share with two decimals on the wire ("70.00"). */
export interface PayoutItem {
  wallet_id: string;
  share: string;
  label?: string;
  address?: string;
}

/** Per network (chain id), 0 or 1 to 5 items in payout order (the first takes the rounding remainder). */
export type LinkPayout = Record<string, PayoutItem[]>;

export type PayoutInput = Record<string, { wallet_id: string; share: string }[]>;

/** What a sensitive Live action is confirmed with: a code emailed to the owner, or the authenticator code when 2FA is on. */
export interface Confirmation {
  email_code?: string;
  totp?: string;
}

export type ConfirmationPurpose = 'wallet_create' | 'wallet_archive' | 'api_key' | 'webhook_endpoint' | 'unlock' | 'totp_enroll';

export interface ConfirmationSent {
  sent_to: string;
  expires_in: number;
  resend_after: number;
}

export interface AcceptedAsset {
  chain: string;
  asset: string;
  enabled: boolean;
}

/** A network an active product takes, for "Used by N products" in Settings. */
export interface SettingsProductNetwork {
  product_id: string;
  product_name: string;
  chain: string;
  accept: 'all' | 'some';
  assets?: string[];
}

export interface Settings {
  accepted_assets: AcceptedAsset[];
  /** Active products of the mode and the networks they take; an older API omits it. */
  product_networks?: SettingsProductNetwork[];
  fee: { fee_bps: number; fixed_fee: string };
}

/** The steps of Get started, in the order the merchant takes them. */
type OnboardingStep = 'choose' | 'wallets' | 'product';

/** Where the merchant is in Get started for one mode (GET /onboarding). */
export interface Onboarding {
  /** Get started stays in the menu while the mode has no product. */
  visible: boolean;
  /** No product in either mode: the very first time, which also asks where to start. */
  first_run: boolean;
  has_wallet: boolean;
  has_product: boolean;
  networks: { chain: string; family: Family; has_wallet: boolean }[];
  steps: OnboardingStep[];
  /** The step to open at; `done` once the mode has a product. */
  step: OnboardingStep | 'done';
  /** How many of `steps` come before `step`. */
  done: number;
}

export interface ApiErrorBody {
  /** `details` carries extra fields of some errors, such as `methods` of `confirmation_required`. */
  error: { code: string; message: string; param?: string; details?: Record<string, unknown> };
}
