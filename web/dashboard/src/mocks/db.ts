// In-memory dashboard backend for dev and tests (never part of the production build). Amounts
// are exact: everything is computed on 6-decimal token units with bigint.
import type {
  AcceptedAsset,
  ApiKey,
  Family,
  InboxRequest,
  Link,
  LinkPayout,
  LinkStats,
  ProductStats,
  PriceType,
  Mode,
  Payment,
  PayTo,
  PaymentMethod,
  Product,
  ProductNetwork,
  Session,
  SessionEvent,
  SessionPayout,
  SessionStatus,
  Settlement,
  SettledWith,
  Wallet,
  WebhookDelivery,
  WebhookEndpoint,
} from '../lib/types';
import { networkName } from '../lib/format';
import { bpsToShare, payoutUnits } from '../lib/splits';
import { activeWallets } from '../lib/wallets';

export const MOCK_PASSWORD = 'correct horse battery';
/** A password the mock server answers as found in a data breach, in any case. */
export const MOCK_BREACHED_PASSWORD = 'Trustno1!';
export const MOCK_TOTP = '123456';
/** Every emailed code in the mocks (sign-up confirmation). */
export const MOCK_CODE = '246810';
/** The token of every password reset link in the mocks. */
export const MOCK_RESET_TOKEN = 'PreviewResetLinkToken_0123456789abcdefghijk';
/** The token of every lost-app link, and of the cancel link a removal request hands out, in the mocks. */
export const MOCK_LOST_TOKEN = 'PreviewLostLinkToken_0123456789abcdefghijklmn';
export const MOCK_CANCEL_TOKEN = 'PreviewCancelLinkToken_0123456789abcdefghijk';
/** The ten recovery codes of the seeded account that has 2FA on. */
export const MOCK_RECOVERY_CODES = ['K7M2Q-9XQPA', 'T4HVN-2JC8R', 'B6WZE-M3KDY', 'P9FAS-7LQ2G', 'R2NXC-5TVHJ', 'D8KMU-4PYBW', 'G3ZLQ-X6EFN', 'V5JRT-8CAMS', 'Y7BPD-H2WKE', 'N4QGU-9RLZX'];
/** How long a lost-app request waits, and how long the lock after the removal lasts. */
export const MOCK_REMOVAL_WAIT_MS = 72 * 3_600_000;
export const MOCK_LOCK_MS = 48 * 3_600_000;
export const CHECKOUT_BASE = 'https://prismaport.xyz';
export const FIXED_FEE = '0.10';
export const FEE_BPS = 50;

export interface MockUser {
  id: string;
  email: string;
  totp_enabled: boolean;
  totp_pending: boolean;
  /** False until the emailed code was entered (absent means confirmed). */
  email_verified?: boolean;
  /** Absent means MOCK_PASSWORD. */
  password?: string;
  /** A password reset link was sent and not used yet. */
  reset_link?: boolean;
  /** Wrong tries on the current emailed code; like the server, the fifth ends the code. */
  code_tries?: number;
  /** When the last confirmation code of each purpose was emailed (ms), and the wrong tries on it; the fifth ends the code. */
  confirm_sent_at?: Record<string, number>;
  confirm_tries?: number;
  /** The unused recovery codes (absent: none, as for an account that turned 2FA on before they existed). */
  recovery_codes?: string[];
  /** A lost-app link was sent and not used yet. */
  lost_link?: boolean;
  /** When a lost-app request removes 2FA (RFC 3339), while one is waiting. */
  totp_removal_at?: string | null;
  /** While in the future, a live wallet cannot be added and a live API key cannot be created or rolled. */
  recovery_locked_until?: string | null;
}

export interface StoredProduct {
  id: string;
  livemode: boolean;
  name: string;
  description: string | null;
  price_type: PriceType;
  amount: string | null;
  min_amount: string | null;
  max_amount: string | null;
  suggested_amounts: string[];
  active: boolean;
  success_url: string | null;
  metadata: Record<string, string>;
  payout: StoredPayout;
  /** Each taken network's stablecoin choice; a network it takes without an entry is All (a product seeded before D60). */
  networks?: StoredNetworks;
  created_at: string;
  updated_at: string;
}

/** A product's choice per network it takes: all of the stablecoins on in Settings, or only the listed ones. */
export type StoredNetworks = Record<string, { accept: 'all' | 'some'; assets?: string[] }>;

export interface StoredLink {
  id: string;
  livemode: boolean;
  product_id: string;
  /** The internal label; null shows the product name. */
  name: string | null;
  amount: string | null;
  active: boolean;
  success_url: string | null;
  metadata: Record<string, string>;
  expires_at: string | null;
  max_payments: number | null;
  created_at: string;
  updated_at: string;
}

export interface StoredWallet extends Wallet {
  livemode: boolean;
}

/** A link's payout as the server stores it: per chain id, 0 to 5 items in payout order. */
export type StoredPayout = Record<string, { wallet_id: string; bps: number }[]>;

export interface StoredSession extends Session {
  events: SessionEvent[];
  /** Covered flushes: another session's flush of the same customer address moved these funds. */
  settled_with?: SettledWith[];
}

/** A static customer address (B11) as the server stores it: its pay_to and payout snapshot never change. */
export interface StoredCustomerAddress {
  id: string;
  livemode: boolean;
  customer_id: string;
  product_id: string;
  status: 'active' | 'rotated';
  replaced_by: string | null;
  pay_to: { chain: string; asset: string; address: string; token_account?: string }[];
  payout: SessionPayout;
  metadata: Record<string, string>;
  created_at: string;
  rotated_at: string | null;
  /** The fee its forwarders commit to, in bps; FEE_BPS unless the fee changed since. */
  fee_bps: number;
  /** The latest session opened on it: it owns the address's watch. */
  current_session_id: string | null;
}

export interface StoredKey extends ApiKey {
  livemode: boolean;
}

export interface StoredEndpoint extends WebhookEndpoint {
  livemode: boolean;
}

/** The test-mode hosted inbox (one per merchant). */
interface StoredInbox {
  id: string;
  token: string;
  url: string;
  endpoint_id: string;
  created_at: string;
  secret: string;
}

export const INBOX_RETENTION_DAYS = 7;
export const INBOX_MAX_REQUESTS = 500;
export const INBOX_MAX_BODY = 64 * 1024;

export interface MockDb {
  users: MockUser[];
  /** live_enabled false: live mode closed, as for a self-serve account from before sign-up opened it (absent means open). */
  merchant: { id: string; name: string; live_enabled?: boolean; logo_url?: string | null };
  /** D64: what GET /me lists as chain_pauses; empty unless a test sets it. */
  chain_pauses?: import('../lib/types').ChainPause[];
  /** confirmed_until and confirmed_wallets: the session's confirmation window (D59), as the server keeps it. */
  session: { userId: string; csrf: string; confirmed_until?: number; confirmed_wallets?: number } | null;
  products: StoredProduct[];
  links: StoredLink[];
  sessions: StoredSession[];
  /** Absent in a tab stored before customer addresses. */
  customerAddresses?: StoredCustomerAddress[];
  apiKeys: StoredKey[];
  endpoints: StoredEndpoint[];
  deliveries: WebhookDelivery[];
  wallets: StoredWallet[];
  accepted: Record<Mode, AcceptedAsset[]>;
  /** By mode, kind and key: what a create returned, so a replay answers the same object. */
  idempotency: Record<string, { body: string; id: string }>;
  seq: number;
  inbox: StoredInbox | null;
  inboxRequests: InboxRequest[];
}

// Deterministic data so screenshots and tests are stable.
function rng(seed: number) {
  let a = seed;
  return () => {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const CROCKFORD = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
const HEX = '0123456789abcdef';
const B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';

export function makeIds(seed: number) {
  const r = rng(seed);
  const pick = (alphabet: string, n: number) => Array.from({ length: n }, () => alphabet[Math.floor(r() * alphabet.length)]).join('');
  return {
    r,
    id: (prefix: string) => `${prefix}_01JB${pick(CROCKFORD, 22)}`,
    evmHash: () => `0x${pick(HEX, 64)}`,
    solSig: () => pick(B58, 88),
    hex: (n: number) => pick(HEX, n),
    b58: (n: number) => pick(B58, n),
  };
}

// 6-decimal token units.
const D = 6;
export function units(v: string): bigint {
  const [i = '0', f = ''] = v.split('.');
  return BigInt(i + f.padEnd(D, '0').slice(0, D));
}
export function token(u: bigint): string {
  const s = u.toString().padStart(D + 1, '0');
  return `${s.slice(0, -D)}.${s.slice(-D)}`;
}
/** USD with at least two places and every non-zero digit beyond (like the API). */
export function usdExact(u: bigint): string {
  const t = token(u);
  const [i, f = ''] = t.split('.');
  const trimmed = f.replace(/0+$/, '');
  return `${i}.${trimmed.length > 2 ? trimmed : trimmed.padEnd(2, '0')}`;
}

function splitFee(gross: bigint): { fee: bigint; net: bigint } {
  const fee = units(FIXED_FEE) + (gross * BigInt(FEE_BPS)) / 10_000n;
  return { fee, net: gross - fee };
}

const iso = (ms: number) => new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z');
const HOUR = 3_600_000;
const DAY = 24 * HOUR;

const EVM_DEST = '0x7a3F5c2b9E81d4A6f0C3e2B7a915D8c4F06Be91c';
const SOL_DEST = '9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin';

export const familyOf = (chain: string): Family => (chain.startsWith('solana') ? 'solana' : 'evm');

/** The networks of a mode: the chains its stablecoins are on, in the registry's order. */
export function chainsOf(db: MockDb, mode: Mode): string[] {
  return [...new Set(db.accepted[mode].map((a) => a.chain))];
}

/** The server's create rule: every network pays its family's oldest active wallet at 100%, or nothing without one. */
export function defaultPayout(db: MockDb, mode: Mode): StoredPayout {
  const mine = db.wallets.filter((w) => w.livemode === (mode === 'live'));
  return Object.fromEntries(
    chainsOf(db, mode).map((c) => {
      const first = activeWallets(mine, familyOf(c))[0];
      return [c, first ? [{ wallet_id: first.id, bps: 10_000 }] : []];
    }),
  );
}

/** The product's networks as the API renders them: only those it pays, in network order, `assets` only for Some. */
export function renderNetworks(db: MockDb, mode: Mode, p: Pick<StoredProduct, 'payout' | 'networks'>): ProductNetwork[] {
  return chainsOf(db, mode)
    .filter((c) => (p.payout[c] ?? []).length > 0)
    .map((c) => {
      const n = p.networks?.[c] ?? { accept: 'all' as const };
      return n.accept === 'some' ? { chain: c, chain_name: networkName(c), accept: 'some' as const, assets: n.assets ?? [] } : { chain: c, chain_name: networkName(c), accept: 'all' as const };
    });
}

/** What payers can choose: Settings ∩ the product's choice, on the networks it pays. */
function paymentMethodsOf(db: MockDb, mode: Mode, p: Pick<StoredProduct, 'payout' | 'networks'>): PaymentMethod[] {
  return renderNetworks(db, mode, p).flatMap((n) =>
    db.accepted[mode]
      .filter((a) => a.chain === n.chain && a.enabled && (n.accept === 'all' || (n.assets ?? []).includes(a.asset)))
      .map((a) => ({ chain: n.chain, chain_name: n.chain_name, asset: a.asset })),
  );
}

/** A link's payout as the API renders it: every network of the mode, each item with its wallet's label and address. */
function renderPayout(db: MockDb, mode: Mode, p: StoredPayout): LinkPayout {
  return Object.fromEntries(
    chainsOf(db, mode).map((c) => [
      c,
      (p[c] ?? []).map((i) => {
        const w = db.wallets.find((x) => x.id === i.wallet_id);
        return { wallet_id: i.wallet_id, label: w?.label ?? '', address: w?.address ?? '', share: bpsToShare(i.bps) };
      }),
    ]),
  );
}

/** The recipients a new session commits to: the product's payout, per network it takes. */
export function snapshotPayout(db: MockDb, payout: StoredPayout): SessionPayout {
  const out: SessionPayout = {};
  for (const [chain, items] of Object.entries(payout)) {
    if (items.length === 0) continue;
    out[chain] = items.map((i) => {
      const w = db.wallets.find((x) => x.id === i.wallet_id);
      return { wallet_id: i.wallet_id, label: w?.label ?? null, address: w?.address ?? '', share: bpsToShare(i.bps), bps: i.bps };
    });
  }
  return out;
}

/** A forwarded payment: fee to Prismaport, the rest split exactly by the session's recipients. */
export function settlementFor(db: MockDb, s: Pick<Session, 'payout'>, chain: string, asset: string, gross: bigint, txHash: string, settledAt: string): Settlement | null {
  const recipients = s.payout?.[chain] ?? [];
  if (recipients.length === 0) return null;
  const { fee, net } = splitFee(gross);
  const amounts = payoutUnits(net, recipients.map((r) => r.bps));
  // A recipient whose share rounds to zero receives nothing and is not listed.
  return {
    chain,
    asset,
    gross: token(gross),
    fee: token(fee),
    net: token(net),
    payouts: recipients
      .map((r, i) => ({ wallet_id: r.wallet_id, label: db.wallets.find((w) => w.id === r.wallet_id)?.label ?? r.label ?? null, address: r.address, amount: token(amounts[i]!) }))
      .filter((p) => p.amount !== token(0n)),
    tx_hash: txHash,
    settled_at: settledAt,
  };
}

export function payToFor(mode: Mode, amount: string, ids: ReturnType<typeof makeIds>): PayTo[] {
  const base = mode === 'live' ? 'base' : 'base-sepolia';
  const sol = mode === 'live' ? 'solana' : 'solana-devnet';
  const amt = token(units(amount));
  return [
    { chain: base, asset: 'USDC', address: `0x${ids.hex(40)}`, amount: amt },
    { chain: sol, asset: 'USDC', address: ids.b58(44), amount: amt, token_account: ids.b58(44) },
    { chain: sol, asset: 'USDT', address: ids.b58(44), amount: amt, token_account: ids.b58(44) },
  ];
}

/** A network nobody generated a payment for (D48): the entry keeps its amount and loses its address. */
export function unrevealed(p: PayTo): PayTo {
  return { chain: p.chain, asset: p.asset, amount: p.amount, revealed: false, revealed_at: null };
}

export function revealed(p: PayTo, at: string): PayTo {
  return { ...p, revealed: true, revealed_at: at };
}

interface ProductSeed {
  name: string;
  description?: string;
  price_type: PriceType;
  amount?: string;
  min?: string;
  max?: string;
  suggested?: string[];
  /** The product's own state; its first link carries `active` below. */
  productActive?: boolean;
  active?: boolean;
  expiresInDays?: number;
  maxPayments?: number;
  opened: number;
  paid: number;
  createdDaysAgo: number;
  metadata?: Record<string, string>;
  successUrl?: string;
  extra?: SessionStatus[];
  /** Wallet labels and basis points per family, set on every network of that family in the mode; unset = the oldest wallet at 100%. */
  payout?: Partial<Record<Family, [string, number][]>>;
  /** More links of the product, with no sessions. */
  links?: { name: string | null; amount?: string; maxPayments?: number; createdDaysAgo: number; metadata?: Record<string, string> }[];
}

const LIVE_PRODUCTS: ProductSeed[] = [
  {
    name: 'Founder plan', description: 'Lifetime access to every Acme Studio template, with updates.', price_type: 'fixed', amount: '25.00', opened: 24, paid: 9, createdDaysAgo: 40,
    metadata: { plan: 'founder', sku: 'FND-001' }, successUrl: 'https://acme.studio/thanks', extra: ['partially_paid', 'overpaid', 'late_payment'],
    payout: { evm: [['Founder', 7_000], ['Partner', 3_000]], solana: [['Treasury', 8_000], ['Partner', 2_000]] },
    links: [{ name: 'Instagram bio', createdDaysAgo: 12, metadata: { channel: 'instagram' } }],
  },
  {
    name: 'Pay Acme Studio', price_type: 'open', min: '5.00', max: '500.00', suggested: ['10.00', '25.00', '50.00'], opened: 15, paid: 9, createdDaysAgo: 60, extra: ['under_review'],
    payout: { evm: [['Founder', 5_000], ['Partner', 5_000]] },
    links: [{ name: 'Invoice ACME #12', amount: '120.00', maxPayments: 1, createdDaysAgo: 3 }],
  },
  { name: 'Black Friday bundle', price_type: 'fixed', amount: '149.00', expiresInDays: 2, opened: 6, paid: 2, createdDaysAgo: 5, payout: { evm: [['Reserve', 10_000]] } },
  { name: 'Tip jar', price_type: 'open', min: '1.00', suggested: ['2.00', '5.00'], opened: 6, paid: 2, createdDaysAgo: 30, payout: { solana: [['Treasury', 8_000], ['Partner', 2_000]] } },
  { name: 'Design workshop ticket', price_type: 'fixed', amount: '120.00', maxPayments: 12, opened: 15, paid: 12, createdDaysAgo: 25, payout: { evm: [['Founder', 5_000], ['Partner', 5_000]] } },
  { name: 'Summer sale poster', price_type: 'fixed', amount: '9.99', expiresInDays: -10, opened: 8, paid: 2, createdDaysAgo: 90 },
  { name: 'Consulting hour', price_type: 'fixed', amount: '80.00', active: false, opened: 5, paid: 3, createdDaysAgo: 120 },
];

const TEST_PRODUCTS: ProductSeed[] = [
  { name: 'Test product', price_type: 'fixed', amount: '1.00', opened: 3, paid: 1, createdDaysAgo: 3 },
  { name: 'Test donation', price_type: 'open', min: '0.50', opened: 1, paid: 0, createdDaysAgo: 1, payout: { solana: [] } },
];

interface WalletSeed {
  mode: Mode;
  family: Family;
  label: string;
  address: string;
  verified?: boolean;
  createdDaysAgo: number;
  archivedDaysAgo?: number;
}

const WALLETS: WalletSeed[] = [
  { mode: 'live', family: 'evm', label: 'Founder', address: EVM_DEST, createdDaysAgo: 45 },
  { mode: 'live', family: 'evm', label: 'Partner', address: '0x3C9e7B2a4F1d8E6c5A0b9D3f2E1c4B7a6D5e8F02', createdDaysAgo: 44 },
  { mode: 'live', family: 'evm', label: 'Reserve', address: '0x9B2d4E6f8A1c3E5b7D9f0A2c4E6b8D0f1A3c5E7b', createdDaysAgo: 44 },
  { mode: 'live', family: 'evm', label: 'Old hardware wallet', address: '0x6F1e3D5c7B9a0E2d4C6b8A0f2E4d6C8b0A2e4D6c', createdDaysAgo: 90, archivedDaysAgo: 46 },
  { mode: 'live', family: 'solana', label: 'Treasury', address: SOL_DEST, createdDaysAgo: 45 },
  { mode: 'live', family: 'solana', label: 'Partner', address: 'H3bTqVf6pK8dQ2mZ7rN4xW9cL1sJ5gY3aE8uB6oR2tFv', createdDaysAgo: 30 },
  { mode: 'test', family: 'evm', label: 'Founder', address: '0x51c0A1E7b3D2f4E8a9C6B0d1E2f3A4b5C6d7E8f9', verified: false, createdDaysAgo: 4 },
  { mode: 'test', family: 'solana', label: 'Founder', address: '7vYp3sKf2QmN8xR4tW6zB1cD9eH5jL3gA2uE7nP4qS8T', verified: false, createdDaysAgo: 4 },
];

/** A self-serve account right after sign-up: both modes open, nothing in them, email not confirmed. */
export function createSignupDb(name: string, email: string, password: string, id: (prefix: string) => string): MockDb {
  return {
    users: [{ id: id('usr'), email, totp_enabled: false, totp_pending: false, email_verified: false, password }],
    merchant: { id: id('mer'), name, live_enabled: true },
    session: null,
    products: [],
    links: [],
    sessions: [],
    apiKeys: [],
    endpoints: [],
    deliveries: [],
    wallets: [],
    accepted: {
      live: [
        { chain: 'base', asset: 'USDC', enabled: true },
        { chain: 'solana', asset: 'USDC', enabled: true },
        { chain: 'solana', asset: 'USDT', enabled: true },
      ],
      test: [
        { chain: 'base-sepolia', asset: 'USDC', enabled: true },
        { chain: 'solana-devnet', asset: 'USDC', enabled: true },
      ],
    },
    idempotency: {},
    seq: 1,
    inbox: null,
    inboxRequests: [],
  };
}

export function createDb(now = Date.now()): MockDb {
  const ids = makeIds(20261025);
  // Wallets, and the products and links that came with the catalog, draw their ids from their own
  // streams, so seeded sessions keep theirs.
  const wids = makeIds(20261026);
  const lids = makeIds(20261027);
  const r = ids.r;
  const db: MockDb = {
    users: [
      { id: 'usr_01JBOWNER', email: 'owner@acme.studio', totp_enabled: false, totp_pending: false },
      { id: 'usr_01JBSECURE', email: 'secure@acme.studio', totp_enabled: true, totp_pending: false, recovery_codes: [...MOCK_RECOVERY_CODES] },
    ],
    merchant: { id: 'mer_01JB8Q4ZQ2W7N3H5X9K0Y6T1R2', name: 'Acme Studio' },
    session: null,
    products: [],
    links: [],
    sessions: [],
    apiKeys: [],
    endpoints: [],
    deliveries: [],
    wallets: [],
    accepted: {
      live: [
        { chain: 'base', asset: 'USDC', enabled: true },
        { chain: 'solana', asset: 'USDC', enabled: true },
        { chain: 'solana', asset: 'USDT', enabled: true },
      ],
      test: [
        { chain: 'base-sepolia', asset: 'USDC', enabled: true },
        { chain: 'solana-devnet', asset: 'USDC', enabled: true },
      ],
    },
    idempotency: {},
    seq: 1,
    inbox: null,
    inboxRequests: [],
  };
  for (const w of WALLETS) {
    db.wallets.push({
      id: wids.id('wal'),
      label: w.label,
      family: w.family,
      address: w.address,
      verified: w.verified ?? true,
      archived_at: w.archivedDaysAgo === undefined ? null : iso(now - w.archivedDaysAgo * DAY),
      created_at: iso(now - w.createdDaysAgo * DAY),
      livemode: w.mode === 'live',
    });
  }
  const walletId = (mode: Mode, family: Family, label: string) => db.wallets.find((w) => w.livemode === (mode === 'live') && w.family === family && w.label === label && !w.archived_at)!.id;
  const seedPayout = (mode: Mode, seed: ProductSeed['payout']): StoredPayout => {
    const base = defaultPayout(db, mode);
    const items = (c: string) => seed?.[familyOf(c)]?.map(([label, bps]) => ({ wallet_id: walletId(mode, familyOf(c), label), bps })) ?? base[c]!;
    return Object.fromEntries(chainsOf(db, mode).map((c) => [c, items(c)]));
  };

  const addSession = (mode: Mode, product: StoredProduct, link: StoredLink | null, status: SessionStatus, amount: string, createdMs: number) => {
    const id = ids.id('ses');
    const payout = snapshotPayout(db, product.payout);
    const full = payToFor(mode, amount, ids);
    const pick = full[Math.floor(r() * full.length)]!;
    const offered = full.filter((p) => payout[p.chain]);
    // An open session, and every second expired one, was never generated; the others show the network the payer used.
    const generated = status !== 'open' && !(status === 'expired' && db.sessions.length % 2 === 0);
    const startedAt = iso(createdMs + 30_000);
    const payTo = offered.map((p) => (generated && p.chain === pick.chain ? revealed(p, startedAt) : unrevealed(p)));
    const anyRevealed = payTo.some((p) => p.revealed);
    const amountU = units(amount);
    const payments: Payment[] = [];
    const settlements: Settlement[] = [];
    const events: SessionEvent[] = [];
    let received = 0n;
    let paidAt: string | null = null;
    const ev = (type: string, ms: number) => events.push({ id: ids.id('evt'), type, created_at: iso(ms) });
    const pay = (u: bigint, ms: number, statusP: Payment['status'] = 'confirmed') => {
      const tx = pick.chain.startsWith('solana') ? ids.solSig() : ids.evmHash();
      payments.push({ id: ids.id('pay'), chain: pick.chain, asset: pick.asset, amount: token(u), tx_hash: tx, status: statusP, confirmed_at: statusP === 'detected' ? null : iso(ms + 90_000) });
      ev('session.payment_detected', ms);
      if (statusP === 'confirmed' || statusP === 'finalized') {
        received += u;
        const st = settlementFor(db, { payout }, pick.chain, pick.asset, u, tx, iso(ms + 120_000));
        if (st) settlements.push(st);
      }
    };
    const t1 = createdMs + Math.floor(r() * 20 * 60_000) + 60_000;
    switch (status) {
      case 'paid':
        pay(amountU, t1, 'finalized');
        ev('session.paid', t1 + 90_000);
        paidAt = iso(t1 + 90_000);
        break;
      case 'overpaid': {
        const over = amountU + units('2.50');
        pay(over, t1, 'finalized');
        ev('session.overpaid', t1 + 90_000);
        paidAt = iso(t1 + 90_000);
        break;
      }
      case 'partially_paid':
        pay(amountU / 2n, t1);
        ev('session.partially_paid', t1 + 90_000);
        break;
      case 'late_payment':
        pay(amountU, createdMs + 70 * 60_000);
        ev('session.expired', createdMs + 60 * 60_000);
        ev('session.late_payment', createdMs + 72 * 60_000);
        break;
      case 'under_review':
        pay(amountU - units('0.01'), t1);
        ev('session.under_review', t1 + 95_000);
        break;
      case 'expired':
        ev('session.expired', createdMs + 60 * 60_000);
        break;
      case 'open':
        break;
    }
    // One reversed transfer on a paid session shows how a reorg reads.
    if (status === 'paid' && db.sessions.length === 3) {
      payments.unshift({ id: ids.id('pay'), chain: pick.chain, asset: pick.asset, amount: token(amountU), tx_hash: ids.evmHash(), status: 'reversed', confirmed_at: null });
      ev('payment.reversed', createdMs + 30_000);
    }
    events.sort((a, b) => a.created_at.localeCompare(b.created_at));
    const s: StoredSession = {
      id,
      version: events.length + 1,
      livemode: mode === 'live',
      status,
      product_id: product.id,
      link_id: link?.id ?? null,
      link_name: link ? link.name ?? product.name : null,
      product_name: product.name,
      amount,
      currency: 'USD',
      amount_received: usdExact(received),
      amount_remaining: usdExact(received >= amountU ? 0n : amountU - received),
      checkout_url: `${CHECKOUT_BASE}/s/${id}`,
      pay_to: payTo,
      payments,
      settlement: settlements.length > 0 ? settlements[settlements.length - 1]! : null,
      ...(settlements.length > 1 ? { settlements } : {}),
      metadata: { ...product.metadata, ...(link?.metadata ?? {}), ...(link ? {} : { order_id: `ORD-${1000 + db.sessions.length}` }) },
      // API sessions carry the server's own reference; some link sessions one from the link URL.
      ...(link
        ? db.sessions.length % 3 === 1
          ? { client_reference_id: `cart-${2040 + db.sessions.length}`, client_reference_source: 'payer' as const }
          : { client_reference_id: null, client_reference_source: null }
        : { client_reference_id: `inv:2026-${String(100 + db.sessions.length).padStart(4, '0')}`, client_reference_source: 'merchant' as const }),
      expires_at: iso(createdMs + 60 * 60_000),
      payment_started_at: anyRevealed ? startedAt : null,
      created_at: iso(createdMs),
      paid_at: paidAt,
      payout,
      events,
    };
    db.sessions.push(s);
  };

  const seedProducts = (mode: Mode, seeds: ProductSeed[]) => {
    for (const sd of seeds) {
      const created = now - sd.createdDaysAgo * DAY;
      const product: StoredProduct = {
        id: ids.id('prod'),
        livemode: mode === 'live',
        name: sd.name,
        description: sd.description ?? null,
        price_type: sd.price_type,
        amount: sd.amount ?? null,
        min_amount: sd.min ?? null,
        max_amount: sd.max ?? null,
        suggested_amounts: sd.suggested ?? [],
        active: sd.productActive ?? true,
        success_url: sd.successUrl ?? null,
        metadata: sd.metadata ?? {},
        payout: seedPayout(mode, sd.payout),
        created_at: iso(created),
        updated_at: iso(created + DAY),
      };
      db.products.push(product);
      // As migration 00022 carries a product into its first link: the same ULID, the product's availability.
      const link: StoredLink = {
        id: `plink_${product.id.slice(5)}`,
        livemode: product.livemode,
        product_id: product.id,
        name: null,
        amount: null,
        active: sd.active ?? true,
        success_url: null,
        metadata: {},
        expires_at: sd.expiresInDays === undefined ? null : iso(now + sd.expiresInDays * DAY),
        max_payments: sd.maxPayments ?? null,
        created_at: product.created_at,
        updated_at: product.updated_at,
      };
      db.links.push(link);
      for (const extra of sd.links ?? []) {
        const at = iso(now - extra.createdDaysAgo * DAY);
        db.links.push({
          id: lids.id('plink'),
          livemode: product.livemode,
          product_id: product.id,
          name: extra.name,
          amount: extra.amount ?? null,
          active: true,
          success_url: null,
          metadata: extra.metadata ?? {},
          expires_at: null,
          max_payments: extra.maxPayments ?? null,
          created_at: at,
          updated_at: at,
        });
      }
      const end = sd.expiresInDays !== undefined && sd.expiresInDays < 0 ? now + sd.expiresInDays * DAY : sd.active === false || sd.productActive === false ? now - 20 * DAY : now - HOUR;
      const span = Math.max(end - created, HOUR);
      const statuses: SessionStatus[] = [...Array(sd.paid).fill('paid'), ...(sd.extra ?? [])];
      while (statuses.length < sd.opened) statuses.push(r() < 0.75 ? 'expired' : 'open');
      statuses.forEach((st, i) => {
        const createdMs = st === 'open' ? now - Math.floor(r() * 50 * 60_000) - 60_000 : created + Math.floor(((i + 1) / (statuses.length + 1)) * span);
        const amount = sd.price_type === 'fixed' ? sd.amount! : (sd.suggested ?? [sd.min!])[Math.floor(r() * (sd.suggested ?? [sd.min!]).length)]!;
        addSession(mode, product, link, st, amount, Math.min(createdMs, now - 60_000));
      });
    }
  };

  /** A product sold only through the merchant API: no link, each session with its own amount. */
  const apiProduct = (mode: Mode, name: string, min: string, active = true): StoredProduct => {
    const t = iso(now - 70 * DAY);
    const p: StoredProduct = {
      id: lids.id('prod'), livemode: mode === 'live', name, description: null, price_type: 'open', amount: null, min_amount: min, max_amount: null, suggested_amounts: [],
      active, success_url: null, metadata: {}, payout: defaultPayout(db, mode), created_at: t, updated_at: t,
    };
    db.products.push(p);
    return p;
  };

  seedProducts('live', LIVE_PRODUCTS);
  seedProducts('test', TEST_PRODUCTS);
  // Sessions created directly with the API, without a link.
  const orders = apiProduct('live', 'Custom order', '1.00');
  addSession('live', orders, null, 'paid', '312.40', now - 2 * DAY);
  addSession('live', orders, null, 'open', '48.00', now - 20 * 60_000);
  // An open Test mode payment to try the simulator on.
  addSession('test', apiProduct('test', 'Test order', '0.50'), null, 'open', '20.00', now - 10 * 60_000);
  // A product taken off sale: its link stays, unavailable.
  const zine = apiProduct('live', 'Spring zine', '1.00', false);
  Object.assign(zine, { price_type: 'fixed', amount: '12.00', min_amount: null, updated_at: iso(now - 15 * DAY) });
  db.links.push({ id: `plink_${zine.id.slice(5)}`, livemode: true, product_id: zine.id, name: null, amount: null, active: true, success_url: null, metadata: {}, expires_at: null, max_payments: null, created_at: zine.created_at, updated_at: zine.created_at });

  const FULL = ['sessions:read', 'sessions:write', 'products:read', 'products:write', 'payment_links:read', 'payment_links:write', 'customer_addresses:read', 'customer_addresses:write', 'wallets:read', 'events:read', 'webhooks:read', 'webhooks:write'];
  const key = (k: Partial<StoredKey> & Pick<StoredKey, 'name' | 'scopes' | 'livemode' | 'prefix' | 'created_at'>): StoredKey => ({
    id: ids.id('key'),
    mode: k.livemode ? 'live' : 'test',
    last_used_at: null,
    revoked_at: null,
    expires_at: null,
    replaced_by: null,
    status: 'active',
    ...k,
  });
  const ciNew = key({ name: 'CI runner', scopes: ['sessions:read', 'sessions:write', 'events:read'], livemode: false, prefix: 'sk_test_8NmR', created_at: iso(now - 4 * HOUR), last_used_at: iso(now - 20 * 60_000) });
  db.apiKeys.push(
    key({ name: 'Production server', scopes: ['sessions:read', 'sessions:write', 'products:read', 'payment_links:read', 'events:read'], livemode: true, prefix: 'sk_live_4fGh', created_at: iso(now - 50 * DAY), last_used_at: iso(now - 3 * HOUR) }),
    key({ name: 'Reporting script', scopes: ['sessions:read', 'events:read'], livemode: true, prefix: 'sk_live_7TyU', created_at: iso(now - 12 * DAY), last_used_at: iso(now - 26 * HOUR) }),
    key({ name: 'Old laptop script', scopes: ['sessions:read'], livemode: true, prefix: 'sk_live_9QwE', created_at: iso(now - 200 * DAY), last_used_at: iso(now - 150 * DAY), revoked_at: iso(now - 140 * DAY), status: 'revoked' }),
    key({ name: 'Staging', scopes: FULL, livemode: false, prefix: 'sk_test_2KpL', created_at: iso(now - 10 * DAY) }),
    ciNew,
    key({ name: 'CI runner', scopes: ['sessions:read', 'sessions:write', 'events:read'], livemode: false, prefix: 'sk_test_5JxB', created_at: iso(now - 30 * DAY), last_used_at: iso(now - 4 * HOUR), expires_at: iso(now + 20 * HOUR), replaced_by: ciNew.id, status: 'expiring' }),
  );

  const ep: StoredEndpoint = { id: ids.id('we'), url: 'https://acme.studio/api/prismaport/webhooks', enabled_events: [], livemode: true, created_at: iso(now - 45 * DAY), status: 'enabled', disabled_reason: null, disabled_at: null, failing_since: null, description: 'Order fulfilment' };
  db.endpoints.push(ep, { id: ids.id('we'), url: 'https://staging.acme.studio/hooks', enabled_events: ['session.paid', 'session.expired'], livemode: false, created_at: iso(now - 9 * DAY), status: 'enabled', disabled_reason: null, disabled_at: null, failing_since: null });
  db.endpoints.push(
    { id: ids.id('we'), url: 'https://billing.acme.studio/hooks', enabled_events: ['session.paid'], livemode: true, created_at: iso(now - 20 * DAY), status: 'enabled', disabled_reason: null, disabled_at: null, failing_since: iso(now - 2 * DAY), description: 'Billing sync' },
    { id: ids.id('we'), url: 'https://old.acme.studio/hooks', enabled_events: [], livemode: true, created_at: iso(now - 40 * DAY), status: 'disabled', disabled_reason: 'failing', disabled_at: iso(now - 3 * DAY), failing_since: iso(now - 10 * DAY) },
    { id: ids.id('we'), url: 'https://analytics.acme.studio/hooks', enabled_events: ['session.expired'], livemode: true, created_at: iso(now - 30 * DAY), status: 'disabled', disabled_reason: 'merchant', disabled_at: iso(now - 1 * DAY), failing_since: null },
  );
  const liveEvents = db.sessions.filter((s) => s.livemode).flatMap((s) => s.events).sort((a, b) => b.created_at.localeCompare(a.created_at)).slice(0, 26);
  liveEvents.forEach((e, i) => {
    const t = Date.parse(e.created_at);
    const status = i === 1 ? 'pending' : i === 4 ? 'dead' : 'succeeded';
    db.deliveries.push({
      id: ids.id('whd'),
      endpoint_id: ep.id,
      event_id: e.id,
      event_type: e.type,
      status,
      attempts: status === 'succeeded' ? 1 : status === 'pending' ? 3 : 12,
      last_status_code: status === 'succeeded' ? 200 : status === 'pending' ? 502 : null,
      last_error: status === 'dead' ? 'connection refused after 10 s' : status === 'pending' ? 'HTTP 502 Bad Gateway' : null,
      last_attempt_at: iso(t + 2_000),
      next_attempt_at: status === 'pending' ? iso(now + 20 * 60_000) : null,
      delivered_at: status === 'succeeded' ? iso(t + 2_000) : null,
      created_at: iso(t + 1_000),
    });
  });
  // Test mode: a hosted inbox that already received the test sessions' recent events, one of
  // them replayed with a stale signature.
  const inboxToken = ids.b58(44);
  const inboxEp: StoredEndpoint = { id: ids.id('we'), url: `${CHECKOUT_BASE}/v1/sandbox/inbox/${inboxToken}`, enabled_events: [], livemode: false, created_at: iso(now - 2 * DAY), status: 'enabled', disabled_reason: null, disabled_at: null, failing_since: null };
  db.endpoints.push(inboxEp);
  db.inbox = { id: ids.id('sbx'), token: inboxToken, url: inboxEp.url, endpoint_id: inboxEp.id, created_at: inboxEp.created_at, secret: `whsec_${ids.b58(44)}` };
  const testEvents = db.sessions
    .filter((s) => !s.livemode)
    .flatMap((s) => s.events.map((e) => ({ s, e })))
    .sort((a, b) => a.e.created_at.localeCompare(b.e.created_at))
    .slice(-4);
  testEvents.forEach(({ s, e }, i) => {
    const at = Date.parse(e.created_at) + 1_500;
    db.inboxRequests.unshift(inboxRequest(ids, envelope(s, e.id, e.type, e.created_at), e.id, e.type, at, i === 1 ? 'no_match' : null));
  });
  seedCustomerAddresses(db, now);
  return db;
}

/** The deposit addresses of a new customer address: one per accepted (chain, asset) its product pays. */
export function addressPayTo(db: MockDb, mode: Mode, payout: SessionPayout, ids: ReturnType<typeof makeIds>): StoredCustomerAddress['pay_to'] {
  return db.accepted[mode]
    .filter((a) => a.enabled && (payout[a.chain] ?? []).length > 0)
    .map((a) => (a.chain.startsWith('solana') ? { chain: a.chain, asset: a.asset, address: ids.b58(44), token_account: ids.b58(44) } : { chain: a.chain, asset: a.asset, address: `0x${ids.hex(40)}` }));
}

/**
 * Live mode customer addresses on an "Account top-up" product (open price): one up to date with
 * four sessions (two settled by one flush, an old expired one past its watch, an open one), one
 * that still pays an archived wallet, and a rotated one with its replacement. Their own id stream,
 * so the rest of the seed keeps its ids.
 */
function seedCustomerAddresses(db: MockDb, now: number): void {
  const ids = makeIds(20261028);
  const mode: Mode = 'live';
  const t = iso(now - 60 * DAY);
  const product: StoredProduct = {
    id: ids.id('prod'), livemode: true, name: 'Account top-up', description: 'Add balance to your account.', price_type: 'open', amount: null, min_amount: '1.00', max_amount: null,
    suggested_amounts: [], active: true, success_url: null, metadata: {}, payout: defaultPayout(db, mode), created_at: t, updated_at: t,
  };
  db.products.push(product);
  const addrs: StoredCustomerAddress[] = (db.customerAddresses = []);
  const make = (customer: string, createdMs: number, payout: SessionPayout = snapshotPayout(db, product.payout)): StoredCustomerAddress => {
    const ca: StoredCustomerAddress = {
      id: ids.id('caddr'), livemode: true, customer_id: customer, product_id: product.id, status: 'active', replaced_by: null, pay_to: addressPayTo(db, mode, payout, ids), payout,
      metadata: {}, created_at: iso(createdMs), rotated_at: null, fee_bps: FEE_BPS, current_session_id: null,
    };
    addrs.push(ca);
    return ca;
  };
  /** A session on the address: open-amount, paid with `paid` (or nothing), settled unless `settle` is false. */
  const session = (ca: StoredCustomerAddress, status: SessionStatus, createdMs: number, paid: string | null, settle: bigint | null = paid ? units(paid) : null): StoredSession => {
    const id = ids.id('ses');
    const pt = ca.pay_to[0]!;
    const events: SessionEvent[] = [];
    const payments: Payment[] = [];
    let settlement: Settlement | null = null;
    if (paid) {
      const tx = pt.chain.startsWith('solana') ? ids.solSig() : ids.evmHash();
      payments.push({ id: ids.id('pay'), chain: pt.chain, asset: pt.asset, amount: token(units(paid)), tx_hash: tx, status: 'finalized', confirmed_at: iso(createdMs + 5 * 60_000) });
      events.push({ id: ids.id('evt'), type: 'session.payment_detected', created_at: iso(createdMs + 4 * 60_000) }, { id: ids.id('evt'), type: 'session.paid', created_at: iso(createdMs + 5 * 60_000) });
      if (settle !== null) settlement = settlementFor(db, { payout: ca.payout }, pt.chain, pt.asset, settle, ids.evmHash(), iso(createdMs + 70 * 60_000));
    } else if (status === 'expired') events.push({ id: ids.id('evt'), type: 'session.expired', created_at: iso(createdMs + 60 * 60_000) });
    const s: StoredSession = {
      id, version: events.length + 1, livemode: true, status, product_id: product.id, link_id: null, link_name: null, product_name: product.name, customer_address_id: ca.id,
      amount: null, currency: 'USD', amount_received: paid ? usdExact(units(paid)) : '0.00', amount_remaining: '0.00', checkout_url: `${CHECKOUT_BASE}/s/${id}`,
      // Its customer already holds the address: every network is revealed from creation (D48).
      pay_to: ca.pay_to.map((p) => revealed({ ...p, amount: null }, iso(createdMs))), payments, settlement, metadata: {}, client_reference_id: null,
      client_reference_source: null, expires_at: iso(createdMs + 30 * 60_000), payment_started_at: iso(createdMs),
      created_at: iso(createdMs), paid_at: paid ? iso(createdMs + 5 * 60_000) : null, payout: ca.payout, events,
    };
    db.sessions.push(s);
    ca.current_session_id = id;
    return s;
  };

  // Up to date: two top-ups whose funds one flush moved, an expired one past its watch, an open one.
  const regular = make('user_1042', now - 30 * DAY);
  const first = session(regular, 'paid', now - 9 * DAY, '30.00', units('50.00'));
  const second = session(regular, 'paid', now - 9 * DAY + 30 * 60_000, '20.00', null);
  second.settled_with = [{ chain: first.settlement!.chain, asset: first.settlement!.asset, session_id: first.id, tx_hash: first.settlement!.tx_hash }];
  session(regular, 'expired', now - 2 * DAY, null);
  session(regular, 'open', now - 20 * 60_000, null);

  // Still pays the archived hardware wallet on Base.
  const archived = db.wallets.find((w) => w.livemode && w.archived_at && w.family === 'evm')!;
  const stale = make('user_0977', now - 50 * DAY, { ...snapshotPayout(db, product.payout), base: [{ wallet_id: archived.id, label: archived.label, address: archived.address, share: '100.00', bps: 10_000 }] });
  session(stale, 'paid', now - 12 * DAY, '75.00');

  // Rotated: the old address names the one that replaced it.
  const old = make('user_0810', now - 40 * DAY);
  session(old, 'paid', now - 20 * DAY, '15.00');
  const next = make('user_0810', now - 5 * DAY);
  Object.assign(old, { status: 'rotated', replaced_by: next.id, rotated_at: next.created_at });
}

/** The webhook envelope of a session event, as the API serializes it. */
export function envelope(s: StoredSession, eventId: string, type: string, createdAt: string): string {
  const { events: _e, ...session } = s;
  return JSON.stringify({ id: eventId, type, created_at: createdAt, data: { session } });
}

/** A request the hosted inbox received from Prismaport, with the signature check made on arrival. */
export function inboxRequest(ids: ReturnType<typeof makeIds>, body: string, eventId: string, type: string, atMs: number, failure: InboxRequest['signature']['error'] = null): InboxRequest {
  const t = Math.floor(atMs / 1000) - (failure === 'timestamp_outside_tolerance' ? 900 : 1);
  const sig = `t=${t},v1=${ids.hex(64)}`;
  return {
    id: ids.id('sbr'),
    received_at: new Date(atMs).toISOString(),
    method: 'POST',
    headers: [
      { name: 'Accept-Encoding', value: 'gzip' },
      { name: 'Content-Length', value: String(new TextEncoder().encode(body).length) },
      { name: 'Content-Type', value: 'application/json' },
      { name: 'Prismaport-Event-Id', value: eventId },
      { name: 'Signature', value: sig },
      { name: 'User-Agent', value: 'Prismaport-Webhooks/1' },
    ],
    body,
    size: new TextEncoder().encode(body).length,
    event_id: eventId,
    event_type: type,
    signature: { valid: failure === null, error: failure, timestamp: new Date(t * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z'), skew_seconds: Math.floor(atMs / 1000) - t },
  };
}

/** Stats over a set of sessions, as the API computes them for a link or a product. */
export function statsOf(sessions: Session[]): LinkStats {
  const paidSessions = sessions.filter((s) => s.status === 'paid' || s.status === 'overpaid');
  const received = new Map<string, bigint>();
  for (const s of sessions) {
    for (const p of s.payments) {
      if (p.status === 'confirmed' || p.status === 'finalized') received.set(p.asset, (received.get(p.asset) ?? 0n) + units(p.amount));
    }
  }
  const opened = sessions.length;
  return {
    opened,
    paid: paidSessions.length,
    received: [...received.entries()].sort().map(([asset, u]) => ({ asset, amount: token(u) })),
    last_paid_at: paidSessions.map((s) => s.paid_at!).sort().at(-1) ?? null,
    conversion: opened === 0 ? null : (Math.round((paidSessions.length / opened) * 10_000) / 10_000).toFixed(4),
  };
}

/** A product's stats also count its checkouts still waiting for money, which keep their old price. */
function productStatsOf(sessions: Session[]): ProductStats {
  return { ...statsOf(sessions), open: sessions.filter((s) => s.status === 'open' || s.status === 'partially_paid').length };
}

/** Whether a link opens sessions, and why not, in the server's precedence order. */
export function linkLiveness(db: MockDb, l: StoredLink, now = Date.now()): { live: boolean; reason: Link['unavailable_reason'] } {
  const product = db.products.find((p) => p.id === l.product_id);
  const sessions = db.sessions.filter((s) => s.link_id === l.id);
  const paid = sessions.filter((s) => s.status === 'paid' || s.status === 'overpaid').length;
  const expired = l.expires_at !== null && Date.parse(l.expires_at) <= now;
  const exhausted = l.max_payments !== null && paid >= l.max_payments;
  // Open sessions hold a slot until they expire unpaid.
  const holding = sessions.filter((s) => s.status === 'open' || s.status === 'partially_paid').length;
  const reserved = !exhausted && l.max_payments !== null && paid + holding >= l.max_payments;
  const reason = !product?.active ? 'product_inactive' : !l.active ? 'inactive' : expired ? 'expired' : exhausted ? 'exhausted' : reserved ? 'reserved' : null;
  return { live: reason === null, reason };
}

/** A product as the API renders it: stats over every session of the product, and its links' counts. */
export function renderProduct(db: MockDb, p: StoredProduct, now = Date.now()): Product {
  const links = db.links.filter((l) => l.product_id === p.id);
  return {
    id: p.id,
    livemode: p.livemode,
    name: p.name,
    description: p.description,
    price_type: p.price_type,
    amount: p.amount,
    min_amount: p.min_amount,
    max_amount: p.max_amount,
    suggested_amounts: p.suggested_amounts,
    currency: 'USD',
    payout: renderPayout(db, p.livemode ? 'live' : 'test', p.payout),
    networks: renderNetworks(db, p.livemode ? 'live' : 'test', p),
    payment_methods: paymentMethodsOf(db, p.livemode ? 'live' : 'test', p),
    success_url: p.success_url,
    metadata: p.metadata,
    active: p.active,
    stats: productStatsOf(db.sessions.filter((s) => s.product_id === p.id)),
    links: { live: links.filter((l) => linkLiveness(db, l, now).live).length, total: links.length },
    created_at: p.created_at,
    updated_at: p.updated_at,
  };
}

/** A link as the API renders it: stats and liveness from its own sessions, with its product embedded. */
export function renderLink(db: MockDb, l: StoredLink, now = Date.now()): Link {
  const p = db.products.find((x) => x.id === l.product_id)!;
  const { live, reason } = linkLiveness(db, l, now);
  return {
    id: l.id,
    livemode: l.livemode,
    product_id: l.product_id,
    name: l.name,
    amount: l.amount,
    active: l.active,
    live,
    unavailable_reason: reason,
    url: `${CHECKOUT_BASE}/l/${l.id}`,
    success_url: l.success_url,
    metadata: l.metadata,
    expires_at: l.expires_at,
    max_payments: l.max_payments,
    stats: statsOf(db.sessions.filter((s) => s.link_id === l.id)),
    product: { id: p.id, name: p.name, price_type: p.price_type, amount: p.amount, min_amount: p.min_amount, max_amount: p.max_amount, active: p.active },
    created_at: l.created_at,
    updated_at: l.updated_at,
  };
}
