import { delay, http, HttpResponse, type HttpResponseResolver } from 'msw';
import type { AcceptedAsset, AddressAttention, ApiKey, CustomerAddress, ExplorerLink, Family, LinkInput, Mode, Overview, Payment, PriceType, ProductInput, RecentPayment, SessionDetail, SessionStatus, SimAction, TestEventResult, WalletTotal, WebhookDelivery } from '../lib/types';
import { networkName } from '../lib/format';
import { BREACHED_PASSWORD_ERROR, COMMON_PASSWORD_ERROR, commonOrRepeated, missingParts, passwordChecks, passwordTooLong } from '../lib/password';
import { feeProblem } from '../lib/formFields';
import { CLIENT_REFERENCE } from '../lib/reference';
import { formatPercent, MAX_RECIPIENTS, parseShare } from '../lib/splits';
import { activeWallets } from '../lib/wallets';
import { endpointUrlError } from '../lib/webhooks';
import {
  CHECKOUT_BASE,
  createDb,
  type StoredNetworks,
  chainsOf,
  createSignupDb,
  defaultPayout,
  envelope,
  familyOf,
  INBOX_MAX_BODY,
  INBOX_MAX_REQUESTS,
  INBOX_RETENTION_DAYS,
  inboxRequest,
  linkLiveness,
  FEE_BPS,
  FIXED_FEE,
  makeIds,
  renderNetworks,
  addressPayTo,
  MOCK_CANCEL_TOKEN,
  MOCK_CODE,
  MOCK_LOCK_MS,
  MOCK_LOST_TOKEN,
  MOCK_REMOVAL_WAIT_MS,
  MOCK_RESET_TOKEN,
  MOCK_BREACHED_PASSWORD,
  MOCK_PASSWORD,
  MOCK_TOTP,
  payToFor,
  renderLink,
  renderProduct,
  revealed,
  settlementFor,
  snapshotPayout,
  statsOf,
  token,
  unrevealed,
  units,
  usdExact,
  type MockDb,
  type MockUser,
  type StoredCustomerAddress,
  type StoredEndpoint,
  type StoredKey,
  type StoredLink,
  type StoredProduct,
  type StoredSession,
  type StoredPayout,
  type StoredWallet,
} from './db';

const BASE = '/v1/dashboard';
const KEY_SCOPES = ['products:read', 'products:write', 'payment_links:read', 'payment_links:write', 'customer_addresses:read', 'customer_addresses:write', 'wallets:read', 'sessions:read', 'sessions:write', 'events:read', 'webhooks:read', 'webhooks:write'];
const PAGE = 20;

export interface MockOptions {
  /** Keeps state across reloads (dev and Playwright); off in unit tests. */
  persist?: boolean;
  latency?: number;
  /** false: a server with no email set up, so confirmations take the authenticator only and POST /confirmations answers 503. */
  email?: boolean;
  /** true: the mail server refuses to send, so POST /confirmations answers 503 email_send_failed (and spends nothing). */
  sendFails?: boolean;
  /** false: the confirmation window is off on this server (PRISMAPORT_CONFIRM_WINDOW=0), so every covered Live change takes its own code. */
  confirmWindow?: boolean;
}

// Versioned: a tab holding the shape from before products and links were apart starts fresh.
const STORAGE_KEY = 'prismaport.mock.db.v3';

type Json = Record<string, unknown>;

const rechecks = new Map<string, number>();
const err = (status: number, code: string, message: string, param?: string) => HttpResponse.json({ error: { code, message, ...(param ? { param } : {}) } }, { status });

/** The server's answer to a password being set, or null when it is accepted. */
const passwordRefusal = (pw: string, param: string) => {
  const parts = missingParts(passwordChecks(pw));
  if (parts.length > 0 || passwordTooLong(pw)) return err(400, 'weak_password', 'Use 8 to 256 characters with an uppercase letter, a lowercase letter, a number and a special character.', param);
  if (commonOrRepeated(pw)) return err(400, 'common_password', COMMON_PASSWORD_ERROR, param);
  if (pw.toLowerCase() === MOCK_BREACHED_PASSWORD.toLowerCase()) return err(400, 'breached_password', BREACHED_PASSWORD_ERROR, param);
  return null;
};

function page<T>(items: T[], cursor: string | null): { data: T[]; next_cursor: string | null } {
  const start = cursor ? Number(cursor) || 0 : 0;
  const data = items.slice(start, start + PAGE);
  return { data, next_cursor: start + PAGE < items.length ? String(start + PAGE) : null };
}

const nowIso = () => new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');

function explorer(chain: string, kind: 'tx' | 'address', ref: string): string {
  switch (chain) {
    case 'base':
      return `https://basescan.org/${kind}/${ref}`;
    case 'base-sepolia':
      return `https://sepolia.basescan.org/${kind}/${ref}`;
    case 'solana':
      return `https://solscan.io/${kind === 'tx' ? 'tx' : 'account'}/${ref}`;
    default:
      return `https://solscan.io/${kind === 'tx' ? 'tx' : 'account'}/${ref}?cluster=devnet`;
  }
}

const AMOUNT = /^\d+(\.\d{1,2})?$/;

type Problem = { message: string; param: string } | null;
const META_MAX = 20;

/** A whole product as it would be stored, checked as the server checks a create or the result of a PATCH. */
function checkProductFields(b: Partial<ProductInput>): Problem {
  if (typeof b.name !== 'string' || !b.name.trim() || b.name.trim().length > 200) return { message: 'name must be 1 to 200 characters.', param: 'name' };
  if (b.description !== undefined && b.description !== null && (typeof b.description !== 'string' || !b.description.trim() || b.description.trim().length > 500)) {
    return { message: 'description must be 1 to 500 characters of plain text, or null.', param: 'description' };
  }
  if (b.price_type !== 'fixed' && b.price_type !== 'open') return { message: 'price_type must be fixed or open.', param: 'price_type' };
  const fee = { fee_bps: FEE_BPS, fixed_fee: FIXED_FEE };
  if (b.price_type === 'fixed') {
    if (typeof b.amount !== 'string' || !AMOUNT.test(b.amount)) return { message: 'amount must be a USD amount with at most two decimals.', param: 'amount' };
    const low = feeProblem(b.amount, fee, 'The price');
    if (low) return { message: low, param: 'amount' };
    if (b.min_amount || b.max_amount || (b.suggested_amounts ?? []).length > 0) return { message: 'A fixed price product has no minimum, maximum or suggested amounts.', param: 'min_amount' };
  } else {
    if (b.amount) return { message: 'A variable price product has no amount: set min_amount, and optionally max_amount.', param: 'amount' };
    if (typeof b.min_amount !== 'string' || !AMOUNT.test(b.min_amount)) return { message: 'min_amount must be a USD amount with at most two decimals.', param: 'min_amount' };
    const low = feeProblem(b.min_amount, fee, 'The minimum');
    if (low) return { message: low, param: 'min_amount' };
    if (b.max_amount && units(b.max_amount) < units(b.min_amount)) return { message: 'max_amount must be at least min_amount.', param: 'max_amount' };
    const sug = b.suggested_amounts ?? [];
    if (sug.length > 3) return { message: 'At most 3 suggested amounts.', param: 'suggested_amounts' };
    for (const [i, x] of sug.entries()) {
      if (!AMOUNT.test(x) || units(x) < units(b.min_amount) || (b.max_amount && units(x) > units(b.max_amount))) return { message: 'Each suggested amount must be within the minimum and maximum.', param: `suggested_amounts[${i}]` };
    }
  }
  return checkCommon(b);
}

function checkCommon(b: { success_url?: string | null; metadata?: Record<string, string> }): Problem {
  if (b.success_url && !/^https:\/\//.test(b.success_url) && !/^http:\/\/(localhost|127\.0\.0\.1)/.test(b.success_url)) return { message: 'success_url must be an https URL.', param: 'success_url' };
  if (Object.keys(b.metadata ?? {}).length > META_MAX) return { message: `metadata can have at most ${META_MAX} keys.`, param: 'metadata' };
  return null;
}

/**
 * A whole link as it would be stored, against the product it sells. A stored amount is checked only
 * when it is set (`amount`), since a kept link may lie outside a range its product moved to; the
 * merged metadata only when the link's metadata changes or the link is turned on (`merged`).
 */
function checkLinkFields(b: Partial<LinkInput>, product: StoredProduct, check: { amount: boolean; merged: boolean } = { amount: true, merged: true }): Problem | { status: 400; code: string; message: string; param: string } {
  if (b.name !== undefined && b.name !== null && (typeof b.name !== 'string' || !b.name.trim() || b.name.trim().length > 200)) return { message: 'name must be 1 to 200 characters, or null.', param: 'name' };
  if (check.amount && b.amount !== undefined && b.amount !== null) {
    if (product.price_type === 'fixed') return { message: 'This product has a fixed price: a link cannot set its amount.', param: 'amount' };
    if (typeof b.amount !== 'string' || !AMOUNT.test(b.amount)) return { message: 'amount must be a USD amount with at most two decimals.', param: 'amount' };
    if (units(b.amount) < units(product.min_amount!)) return { status: 400, code: 'amount_below_minimum', message: `The amount must be at least ${product.min_amount} USD, the product's minimum.`, param: 'amount' };
    if (product.max_amount && units(b.amount) > units(product.max_amount)) return { status: 400, code: 'amount_above_maximum', message: `The amount must be at most ${product.max_amount} USD, the product's maximum.`, param: 'amount' };
  }
  if (b.max_payments !== undefined && b.max_payments !== null && (!Number.isInteger(b.max_payments) || b.max_payments < 1 || b.max_payments > 1_000_000)) return { message: 'max_payments must be 1 to 1,000,000, or null.', param: 'max_payments' };
  if (b.expires_at) {
    const t = Date.parse(b.expires_at);
    if (Number.isNaN(t) || t > Date.now() + 3653 * 86_400_000) return { message: 'expires_at must be an RFC 3339 time within 10 years.', param: 'expires_at' };
  }
  const merged = { ...product.metadata, ...(b.metadata ?? {}) };
  if (check.merged && Object.keys(merged).length > META_MAX) return { message: `With the product's metadata, a payment would carry more than ${META_MAX} keys.`, param: 'metadata' };
  return checkCommon(b);
}

const sameAmount = (a: string | null | undefined, b: string | null | undefined) => ((a ?? null) === null || (b ?? null) === null ? (a ?? null) === (b ?? null) : units(a!) === units(b!));

function samePayout(a: StoredPayout, b: StoredPayout): boolean {
  const chains = new Set([...Object.keys(a), ...Object.keys(b)]);
  return [...chains].every((c) => JSON.stringify((a[c] ?? []).map((x) => [x.wallet_id, x.bps])) === JSON.stringify((b[c] ?? []).map((x) => [x.wallet_id, x.bps])));
}

/** Whether an edit changes what the product's checkouts charge or where their money goes. */
export function changesMoney(before: StoredProduct, after: StoredProduct): boolean {
  return !sameAmount(before.amount, after.amount) || !sameAmount(before.min_amount, after.min_amount) || !sameAmount(before.max_amount, after.max_amount) || !samePayout(before.payout, after.payout);
}

/** Every account the mock knows (the seeded one, then each sign-up); db is the active one. */
interface MockState {
  accounts: MockDb[];
  active: number;
}

export function createMockApi(opts: MockOptions = {}) {
  let state: MockState = load();
  let db: MockDb = state.accounts[state.active]!;
  /** Uploaded checkout logos by id (base64), so the mock can serve them back; lost on reload. */
  const logos = new Map<string, string>();

  function load(): MockState {
    if (opts.persist) {
      try {
        if (new URLSearchParams(globalThis.location?.search ?? '').has('mock-reset')) sessionStorage.removeItem(STORAGE_KEY);
        const raw = sessionStorage.getItem(STORAGE_KEY);
        if (raw) {
          const parsed = JSON.parse(raw) as MockState | MockDb;
          return 'accounts' in parsed ? parsed : { accounts: [parsed], active: 0 };
        }
      } catch {
        // Fall back to a fresh database.
      }
    }
    return { accounts: [createDb()], active: 0 };
  }

  const save = () => {
    if (!opts.persist) return;
    try {
      sessionStorage.setItem(STORAGE_KEY, JSON.stringify(state));
    } catch {
      // Storage full or unavailable: keep the in-memory state.
    }
  };

  /** The account holding a user with this email, made the active one. */
  const accountOf = (email: string): MockUser | null => {
    const e = email.trim().toLowerCase();
    for (let i = 0; i < state.accounts.length; i++) {
      const u = state.accounts[i]!.users.find((x) => x.email === e);
      if (u) {
        state.active = i;
        db = state.accounts[i]!;
        return u;
      }
    }
    return null;
  };
  const findUser = (email: string): MockUser | null => {
    const e = email.trim().toLowerCase();
    for (const a of state.accounts) {
      const u = a.users.find((x) => x.email === e);
      if (u) return u;
    }
    return null;
  };
  const accountWith = (u: MockUser) => state.accounts.find((a) => a.users.includes(u))!;
  /** What the worker's sweep does: a removal whose time came takes 2FA off and starts the lock. The mock runs it when the user is read. */
  const settle = (u: MockUser) => {
    if (!u.totp_removal_at || Date.parse(u.totp_removal_at) > Date.now()) return;
    u.recovery_locked_until = new Date(Date.parse(u.totp_removal_at) + MOCK_LOCK_MS).toISOString();
    u.totp_removal_at = null;
    u.totp_enabled = false;
    u.totp_pending = false;
    u.recovery_codes = undefined;
  };
  const userView = (u: MockUser) => {
    settle(u);
    return {
      id: u.id,
      email: u.email,
      totp_enabled: u.totp_enabled,
      email_verified: u.email_verified !== false,
      recovery_codes_left: u.totp_enabled ? (u.recovery_codes ?? []).length : 0,
      totp_removal_at: u.totp_removal_at ?? null,
      recovery_locked_until: u.recovery_locked_until ?? null,
    };
  };
  /** Spends a recovery code: case and hyphen do not matter, and a code works once. */
  const takeRecoveryCode = (u: MockUser, given: unknown): boolean => {
    const plain = String(given ?? '').toUpperCase().replace(/[\s-]/g, '');
    const i = (u.recovery_codes ?? []).findIndex((c) => c.replace('-', '') === plain);
    if (i < 0) return false;
    u.recovery_codes!.splice(i, 1);
    return true;
  };
  const newRecoveryCodes = (): string[] => {
    const alphabet = 'ABCDEFGHJKMNPQRSTVWXYZ23456789';
    const part = () => Array.from({ length: 5 }, () => alphabet[Math.floor(Math.random() * alphabet.length)]).join('');
    return Array.from({ length: 10 }, () => `${part()}-${part()}`);
  };
  /** The lock after a lost-app removal, checked before the step-up so no code is spent. */
  const recoveryLock = (): Response | null => {
    const until = user()?.recovery_locked_until;
    if (!until || Date.parse(until) <= Date.now()) return null;
    const secs = Math.ceil((Date.parse(until) - Date.now()) / 1000);
    return HttpResponse.json(
      { error: { code: 'recovery_locked', message: 'Two-factor authentication was removed by a recovery request. Adding a wallet and creating live API keys are locked for a while.', details: { locked_until: until } } },
      { status: 403, headers: { 'Retry-After': String(secs) } },
    );
  };
  const noLostLink = () => err(400, 'invalid_link', 'This link no longer works. A link works for 30 minutes and only once, and a newer one replaces it. Ask for a new link.');
  /** The user whose lost-app link carries this token, if the link is still live. */
  const lostLinkUser = (token: unknown): MockUser | null => {
    if (token !== MOCK_LOST_TOKEN) return null;
    for (const a of state.accounts) for (const u of a.users) if (u.lost_link) return u;
    return null;
  };
  const wrongCode = () => err(403, 'totp_required', 'That code is not valid. Enter the current code from your authenticator app.');
  /**
   * Replacing the app, turning 2FA off and new recovery codes: a current authenticator code or a
   * recovery code, one of them (both is 400; none or a wrong one is 403 totp_required). Never an
   * emailed code or the password.
   */
  const secondFactor = (u: MockUser, body: Json): Response | null => {
    const code = typeof body.code === 'string' && body.code !== '' ? body.code : null;
    const recovery = typeof body.recovery_code === 'string' && body.recovery_code !== '' ? body.recovery_code : null;
    if (code && recovery) return err(400, 'invalid_request', 'Send a code or a recovery code, not both.', 'recovery_code');
    if (recovery) return takeRecoveryCode(u, recovery) ? null : err(403, 'totp_required', 'That recovery code is not valid or was already used.');
    if (!code) return err(403, 'totp_required', 'Enter the 6-digit code from your authenticator app, or a recovery code.');
    return code === MOCK_TOTP ? null : wrongCode();
  };
  const invalidCode = () => err(400, 'invalid_code', 'That code is not valid or has expired. Check the newest email from Prismaport, or send a new code.');
  const invalidLink = () =>
    err(400, 'invalid_link', 'This link no longer works. A reset link works for 30 minutes and only once, and a newer one replaces it. Ask for a new link.');
  /** The user whose reset link carries this token, if the link is still live. */
  const resetLinkUser = (token: unknown): MockUser | null => {
    if (token !== MOCK_RESET_TOKEN) return null;
    for (const a of state.accounts) for (const u of a.users) if (u.reset_link) return u;
    return null;
  };
  /** The server's check: five wrong tries end the code, the right one included after that. */
  const codeMatches = (u: MockUser, code: unknown): boolean => {
    if ((u.code_tries ?? 0) >= 5) return false;
    if (String(code ?? '').replace(/[\s-]/g, '') === MOCK_CODE) return true;
    u.code_tries = (u.code_tries ?? 0) + 1;
    save();
    return false;
  };
  const EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
  const publicBody = async (request: Request): Promise<Json> => {
    if (opts.latency) await delay(opts.latency);
    return (await request.json().catch(() => ({}))) as Json;
  };

  const ids = makeIds(Date.now() & 0xffffff);

  const user = () => (db.session ? db.users.find((u) => u.id === db.session!.userId) ?? null : null);

  /** A key's status at read time, as the server computes it. */
  const refreshKey = (k: StoredKey) => {
    const expired = k.expires_at !== null && Date.parse(k.expires_at) <= Date.now();
    k.status = expired ? 'expired' : k.revoked_at ? 'revoked' : k.expires_at ? 'expiring' : 'active';
  };

  const modeOf = (req: Request): Mode => (new URL(req.url).searchParams.get('mode') === 'test' ? 'test' : 'live');
  const live = (m: Mode) => m === 'live';

  /** How long an emailed confirmation code works, as on the server. */
  const CONFIRM_TTL_MS = 15 * 60_000;
  const CONFIRM_WINDOW_MS = 10 * 60_000;
  const CONFIRM_WINDOW_WALLETS = 20;
  const emailOn = opts.email !== false;
  /**
   * A Live action's confirmation: a code emailed to the owner for this purpose, or the
   * authenticator code when 2FA is on; never both, and never the password. Without a code the
   * refusal lists the methods in error.details.methods, as the server does. As on the
   * server, an emailed code is spent once it confirms an action: the next one needs a new code.
   */
  const confirmation = (body: Json, purpose: string): Response | null => {
    const u = user()!;
    const emailCode = typeof body.email_code === 'string' && body.email_code !== '' ? body.email_code : null;
    const totp = typeof body.totp === 'string' && body.totp !== '' ? body.totp : null;
    if (emailCode && totp) return err(400, 'invalid_request', 'Send the code from your email or from your authenticator app, not both.', 'email_code');
    const invalid = () => err(403, 'invalid_code', 'That code is not valid or has expired. Check the newest email from Prismaport, or send a new code.');
    if (emailCode && emailOn) {
      const sent = u.confirm_sent_at?.[purpose];
      // A code works for the action it was sent for, for 15 minutes; five wrong tries end it.
      if (!sent || Date.now() - sent > CONFIRM_TTL_MS || (u.confirm_tries ?? 0) >= 5) return invalid();
      if (emailCode.replace(/[\s-]/g, '') !== MOCK_CODE) {
        u.confirm_tries = (u.confirm_tries ?? 0) + 1;
        save();
        return invalid();
      }
      if (u.confirm_sent_at) delete u.confirm_sent_at[purpose];
      u.confirm_tries = 0;
      save();
      return null;
    }
    if (totp && u.totp_enabled) {
      return totp === MOCK_TOTP ? null : err(403, 'invalid_code', 'That code is not valid. Enter the current code from your authenticator app.');
    }
    const methods = [...(emailOn ? ['email'] : []), ...(u.totp_enabled ? ['totp'] : [])];
    const message =
      methods.length === 2
        ? "Confirm it's you with the code from your authenticator app or a code we email you."
        : methods[0] === 'totp'
          ? "Confirm it's you with the code from your authenticator app."
          : "Confirm it's you with a 6-digit code we email you.";
    return HttpResponse.json({ error: { code: 'confirmation_required', message, details: { methods } } }, { status: 403 });
  };
  /** Issuing a live credential or a live wallet: the confirmation; Test mode asks for nothing. */
  const liveStepUp = (mode: Mode, body: Json, purpose: string): Response | null => (live(mode) ? recoveryLock() ?? confirmation(body, purpose) : null);
  /**
   * A Live action the confirmation window covers (D59): inside the session's open window it takes
   * no code (a wallet only while the window's cap is not reached); a right code opens a new window.
   * Unlike the server, the mock opens and counts the window before the action's own checks.
   */
  const windowStepUp = (mode: Mode, body: Json, purpose: string): Response | null => {
    if (opts.confirmWindow === false) return liveStepUp(mode, body, purpose);
    if (!live(mode)) return null;
    const locked = recoveryLock();
    if (locked) return locked;
    const s = db.session!;
    const wallet = purpose === 'wallet_create';
    const coded = (typeof body.email_code === 'string' && body.email_code !== '') || (typeof body.totp === 'string' && body.totp !== '');
    if (!coded && (s.confirmed_until ?? 0) > Date.now() && (!wallet || (s.confirmed_wallets ?? 0) < CONFIRM_WINDOW_WALLETS)) {
      if (wallet) s.confirmed_wallets = (s.confirmed_wallets ?? 0) + 1;
      save();
      return null;
    }
    const refused = confirmation(body, purpose);
    if (refused) return refused;
    s.confirmed_until = Date.now() + CONFIRM_WINDOW_MS;
    s.confirmed_wallets = wallet ? 1 : 0;
    save();
    return null;
  };
  /** The session's open confirmation window, as GET /me shows it. */
  const confirmationWindow = () => {
    const s = db.session;
    if (!s?.confirmed_until || s.confirmed_until <= Date.now()) return null;
    return { expires_at: new Date(s.confirmed_until).toISOString(), wallets_left: Math.max(CONFIRM_WINDOW_WALLETS - (s.confirmed_wallets ?? 0), 0) };
  };
  /** A wallet as the server shows it: default is the wallet marked so, else the family's oldest active one (the activeWallets order). */
  const walletView = (mode: Mode, w: StoredWallet) => ({
    ...w,
    default: !w.archived_at && activeWallets(walletsOf(mode), w.family)[0]?.id === w.id,
    used_by_products: productsPaying(mode, w.id).length,
  });
  const familyName = (f: Family) => (f === 'evm' ? 'Base' : 'Solana');
  const walletsOf = (mode: Mode) => db.wallets.filter((w) => w.livemode === live(mode));
  const sameAddr = (family: Family, a: string, b: string) => (family === 'evm' ? a.toLowerCase() === b.toLowerCase() : a === b);

  /** The server's address checks, in the order it runs them (shape, then what the address is). */
  const addressProblem = (family: Family, address: string): string | null => {
    if (family === 'evm') {
      if (!/^0x[0-9a-fA-F]{40}$/.test(address)) return 'This is not a valid EVM address.';
      if (/^0x0{40}$/.test(address)) return 'This is the zero address, which nobody controls. Paste your wallet address instead.';
      if (address.toLowerCase() === '0x833589fcd6edb6e08f4c7c32d4f71b54bda02913') return 'This is the USDC contract, not a wallet. Paste your wallet address instead.';
      return null;
    }
    if (!/^[1-9A-HJ-NP-Za-km-z]{32,44}$/.test(address)) return 'This is not a valid Solana address.';
    if (address === 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v') return 'This is the USDC mint, not a wallet. Paste your wallet address instead.';
    return null;
  };

  /** One network of a link's payout: 0 or 1 to 5 distinct active wallets of the network's family, shares above zero with two decimals, summing to 100.00. */
  const checkChainItems = (mode: Mode, chain: string, items: unknown): StoredPayout[string] | string => {
    const name = networkName(chain);
    const family = familyOf(chain);
    if (!Array.isArray(items)) return `payout.${chain} must be a list of 0 to ${MAX_RECIPIENTS} wallets.`;
    if (items.length > MAX_RECIPIENTS) return `A split pays 1 to ${MAX_RECIPIENTS} wallets. ${name} has ${items.length}.`;
    const out: StoredPayout[string] = [];
    for (const [i, it] of (items as { wallet_id?: unknown; share?: unknown }[]).entries()) {
      if (typeof it?.wallet_id !== 'string' || !it.wallet_id) return `payout.${chain}[${i}].wallet_id is required`;
      const w = walletsOf(mode).find((x) => x.id === it.wallet_id);
      if (!w || w.family !== family || w.archived_at) return `Wallet ${it.wallet_id} is not an active ${family === 'evm' ? 'EVM' : 'Solana'} wallet of this account, so it cannot receive on ${name}.`;
      if (out.some((x) => x.wallet_id === w.id)) return `Each wallet can appear only once in a split. ${w.label} appears twice on ${name}.`;
      const bps = typeof it.share === 'string' && /^\d{1,3}(\.\d{1,2})?$/.test(it.share) ? parseShare(it.share) : null;
      if (bps === null || bps <= 0 || bps > 10_000) return `payout.${chain}[${i}].share must be a percentage above 0 and at most 100 with up to two decimals, such as "70.00"`;
      out.push({ wallet_id: w.id, bps });
    }
    const total = out.reduce((a, x) => a + x.bps, 0);
    if (out.length > 0 && total !== 10_000) return `Shares must add up to 100% on ${name}. Now ${formatPercent(total)}.`;
    return out;
  };

  /**
   * A link's payout, as the server applies it, keyed by the mode's chain ids. Create: a network
   * left out gets its family's oldest active wallet at 100% (none without one). Patch: a network
   * present replaces its list, absent keep theirs. Either way the link must still take payments
   * on at least one network. A refusal comes back as the error response.
   */
  const checkPayout = (mode: Mode, payout: unknown, current: StoredPayout | null): StoredPayout | Response => {
    if (payout !== undefined && (payout === null || typeof payout !== 'object' || Array.isArray(payout))) return err(400, 'invalid_request', 'payout must be an object with a list per network.');
    const given = (payout ?? {}) as Record<string, unknown>;
    const chains = chainsOf(db, mode);
    for (const k of Object.keys(given)) if (!chains.includes(k)) return err(400, 'unknown_chain', `payout.${k} is not a network of this mode. Use ${chains.join(' or ')}.`);
    const base = current ?? defaultPayout(db, mode);
    const out: StoredPayout = Object.fromEntries(chains.map((c) => [c, base[c] ?? []]));
    for (const c of chains) {
      if (given[c] === undefined) continue;
      const items = checkChainItems(mode, c, given[c]);
      if (typeof items === 'string') return err(400, 'invalid_request', items);
      out[c] = items;
    }
    if (Object.values(out).every((x) => x.length === 0)) return err(400, 'invalid_request', 'A product must take payments on at least one network. Give one of them at least one wallet.', 'payout');
    return out;
  };

  /**
   * The `networks` of a create or patch together with `payout`, as the server applies them (D60):
   * none drops a network, all and some take it (the family's default wallet when it has no payout
   * yet), and the choice is kept for the networks the product still takes. A refusal is the error response.
   */
  const checkNetworks = (mode: Mode, networks: unknown, payout: unknown, current: StoredProduct | null): { payout: StoredPayout; networks: StoredNetworks } | Response => {
    const chains = chainsOf(db, mode);
    const asked: Record<string, { accept: 'all' | 'some' | 'none'; assets?: string[] }> = {};
    if (networks !== undefined) {
      if (networks === null || typeof networks !== 'object' || Array.isArray(networks)) return err(400, 'invalid_request', 'networks must be an object keyed by network such as {"base": {"accept": "all"}, "solana": {"accept": "some", "assets": ["USDC"]}}', 'networks');
      for (const [c, raw] of Object.entries(networks as Record<string, unknown>)) {
        if (!chains.includes(c)) return err(400, 'unknown_chain', `networks has an unknown network "${c}": use one of ${chains.join(', ')}`, `networks.${c}`);
        if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) return err(400, 'invalid_request', `networks.${c} must be {"accept": "all" | "some" | "none", "assets": [...]}`, `networks.${c}`);
        const n = raw as Record<string, unknown>;
        for (const f of Object.keys(n)) if (f !== 'accept' && f !== 'assets') return err(400, 'invalid_request', `networks.${c} has an unknown field "${f}"`, `networks.${c}.${f}`);
        if (n.accept !== 'all' && n.accept !== 'some' && n.accept !== 'none') return err(400, 'invalid_request', `networks.${c}.accept must be all, some or none`, `networks.${c}.accept`);
        const assets = n.assets ?? undefined;
        if (assets !== undefined && (!Array.isArray(assets) || assets.some((a) => typeof a !== 'string'))) return err(400, 'invalid_request', `networks.${c}.assets must be a list of stablecoin symbols such as ["USDC"]`, `networks.${c}.assets`);
        if (n.accept !== 'some' && assets !== undefined) return err(400, 'invalid_request', `networks.${c}.assets goes only with accept some`, `networks.${c}.assets`);
        if (n.accept === 'some') {
          const list = (assets ?? []) as string[];
          if (list.length === 0) return err(400, 'invalid_request', `networks.${c}.assets must list at least one stablecoin with accept some`, `networks.${c}.assets`);
          const offered = db.accepted[mode].filter((a) => a.chain === c).map((a) => a.asset);
          for (const [i, a] of list.entries()) {
            if (list.indexOf(a) !== i) return err(400, 'invalid_request', `networks.${c}.assets lists ${a} twice`, `networks.${c}.assets`);
            if (!offered.includes(a)) return err(400, 'invalid_request', `networks.${c}.assets: ${a} is not offered on ${c} in ${mode} mode; use ${offered.join(' or ')}`, `networks.${c}.assets`);
          }
          asked[c] = { accept: 'some', assets: offered.filter((a) => list.includes(a)) };
        } else asked[c] = { accept: n.accept };
      }
    }
    const given: Record<string, unknown> = payout !== null && typeof payout === 'object' && !Array.isArray(payout) ? { ...(payout as Record<string, unknown>) } : {};
    for (const [c, n] of Object.entries(asked)) {
      const pg = given[c];
      const taken = (current?.payout[c] ?? []).length > 0;
      const family = familyOf(c);
      if (n.accept === 'none') {
        if (Array.isArray(pg) && pg.length > 0) return err(400, 'invalid_request', `payout.${c} names wallets but networks.${c}.accept is none: send [] or leave payout.${c} out`, `payout.${c}`);
        given[c] = [];
      } else if (Array.isArray(pg) && pg.length === 0) {
        return err(400, 'invalid_request', `networks.${c}.accept is ${n.accept} but payout.${c} is []: a network the product takes needs a payout`, `networks.${c}.accept`);
      } else if (pg === undefined && !taken) {
        const first = activeWallets(walletsOf(mode), family)[0];
        if (!first) return err(400, 'invalid_request', `Add a ${family === 'evm' ? 'EVM' : 'Solana'} wallet before taking payments on ${networkName(c)}, or send payout.${c}.`, `networks.${c}`);
        given[c] = [{ wallet_id: first.id, share: '100.00' }];
      }
    }
    const badShape = payout !== undefined && (payout === null || typeof payout !== 'object' || Array.isArray(payout));
    const next = checkPayout(mode, badShape ? payout : payout === undefined && Object.keys(given).length === 0 ? undefined : given, current?.payout ?? null);
    if (next instanceof Response) return next;
    const out: StoredNetworks = {};
    for (const c of chains) {
      if ((next[c] ?? []).length === 0) continue;
      const a = asked[c];
      out[c] = a && a.accept !== 'none' ? { ...a, accept: a.accept } : current?.networks?.[c] ?? { accept: 'all' };
    }
    return { payout: next, networks: out };
  };

  /** Products of the mode, active or not, that pay a wallet on any network. */
  const productsPaying = (mode: Mode, walletId: string) => db.products.filter((p) => p.livemode === live(mode) && Object.values(p.payout).some((items) => items.some((i) => i.wallet_id === walletId)));

  /** Auth and CSRF exactly as the spec asks: a session cookie, and the token on every non-GET. */
  const authed =
    (fn: (args: { request: Request; params: Record<string, string>; body: Json }) => Response | Promise<Response>): HttpResponseResolver =>
    async ({ request, params }) => {
      if (opts.latency) await delay(opts.latency);
      if (!user()) return err(401, 'unauthenticated', 'Sign in to continue.');
      const path = new URL(request.url).pathname;
      if (user()!.email_verified === false && !/\/(me|auth\/logout)$/.test(path)) {
        return err(403, 'email_unverified', 'Confirm your email to continue. Enter the code we sent you.');
      }
      if (!/\/(me|logo|confirmations|confirmation_window|auth\/.*)$/.test(path) && !['live', 'test'].includes(new URL(request.url).searchParams.get('mode') ?? '')) {
        return err(400, 'invalid_request', 'mode must be live or test');
      }
      if (request.method !== 'GET' && request.headers.get('X-CSRF-Token') !== db.session!.csrf) return err(403, 'csrf_failed', 'The CSRF token is missing or wrong.');
      let body: Json = {};
      if (request.method !== 'GET' && request.method !== 'DELETE') {
        const text = await request.text();
        if (text) {
          try {
            body = JSON.parse(text) as Json;
          } catch {
            return err(400, 'invalid_request', 'The body is not valid JSON.');
          }
        }
      }
      const liveChange = request.method !== 'GET' && (new URL(request.url).searchParams.get('mode') === 'live' || body.mode === 'live');
      if (liveChange && db.merchant.live_enabled === false) return err(403, 'live_review_pending', 'Live payments open after a review. Use Test mode until then.');
      const res = await fn({ request, params: params as Record<string, string>, body });
      save();
      return res;
    };

  const findLink = (id: string, mode?: Mode) => db.links.find((l) => l.id === id && (mode === undefined || l.livemode === live(mode)));
  const findProduct = (id: string, mode?: Mode) => db.products.find((p) => p.id === id && (mode === undefined || p.livemode === live(mode)));
  const productOf = (l: StoredLink) => db.products.find((p) => p.id === l.product_id)!;

  const linkSort = (now: number) => (a: StoredLink, b: StoredLink) => {
    const ra = renderLink(db, a, now);
    const rb = renderLink(db, b, now);
    if (ra.live !== rb.live) return ra.live ? -1 : 1;
    const pa = ra.stats.last_paid_at ?? '';
    const pb = rb.stats.last_paid_at ?? '';
    if (pa !== pb) return pb.localeCompare(pa);
    return b.created_at.localeCompare(a.created_at);
  };
  const productSort = (a: StoredProduct, b: StoredProduct) => {
    if (a.active !== b.active) return a.active ? -1 : 1;
    const pa = renderProduct(db, a).stats.last_paid_at ?? '';
    const pb = renderProduct(db, b).stats.last_paid_at ?? '';
    if (pa !== pb) return pb.localeCompare(pa);
    return b.created_at.localeCompare(a.created_at);
  };
  const linksOfMode = (mode: Mode, f: { productId?: string | null; status?: string | null; q?: string }) => {
    const now = Date.now();
    const q = (f.q ?? '').trim();
    const lower = q.toLowerCase();
    return db.links
      .filter((l) => l.livemode === live(mode))
      .filter((l) => !f.productId || l.product_id === f.productId)
      .filter((l) => !q || l.id === q || (l.name ?? '').toLowerCase().includes(lower) || productOf(l).name.toLowerCase().includes(lower))
      .filter((l) => {
        if (!f.status || f.status === 'all') return true;
        const isLive = linkLiveness(db, l, now).live;
        return f.status === 'live' ? isLive : !isLive;
      })
      .sort(linkSort(now))
      .map((l) => renderLink(db, l, now));
  };
  /** A create replayed with the same Idempotency-Key and body answers the object it made. */
  const replay = (mode: Mode, kind: string, key: string, sig: string): Response | 'new' => {
    const seen = db.idempotency[`${mode}:${kind}:${key}`];
    if (!seen) return 'new';
    if (seen.body !== sig) return err(409, 'idempotency_conflict', 'This Idempotency-Key was used with a different body.');
    const out = kind === 'product' ? renderProduct(db, findProduct(seen.id)!) : renderLink(db, findLink(seen.id)!);
    return HttpResponse.json(out, { headers: { 'Idempotent-Replayed': 'true' } });
  };
  const problem = (p: { message: string; param: string; status?: number; code?: string }) => err(p.status ?? 400, p.code ?? 'invalid_request', p.message, p.param);

  // The server's search: the start of a session id (any case) or of a tx hash (8 characters or
  // more after any 0x; EVM in any case), an id or deposit address exactly, an amount exactly.
  const sameAmount = (a: string, b: string) => {
    const norm = (v: string) => {
      const [i = '', f = ''] = v.split('.');
      return `${i.replace(/^0+(?=\d)/, '')}.${f.replace(/0+$/, '')}`;
    };
    return norm(a) === norm(b);
  };
  const matchesSearch = (s: StoredSession, q: string): boolean => {
    const lower = q.toLowerCase();
    if (s.id === q || s.pay_to.some((p) => p.address !== undefined && (p.address === q || p.address.toLowerCase() === lower))) return true;
    if (/^[0-9]{1,15}(\.[0-9]{1,18})?$/.test(q) && ((s.amount !== null && sameAmount(s.amount, q)) || s.payments.some((p) => sameAmount(p.amount, q)))) return true;
    if (!/^[0-9A-Za-z_]+$/.test(q)) return false;
    if (lower.startsWith('ses_')) return /^[0-9A-Z]*$/.test(q.slice(4).toUpperCase()) && s.id.startsWith(`ses_${q.slice(4).toUpperCase()}`);
    if (lower.startsWith('0x')) return q.length >= 10 && s.payments.some((p) => p.tx_hash.toLowerCase().startsWith(lower));
    return q.length >= 8 && s.payments.some((p) => p.tx_hash.startsWith(q));
  };

  // Sessions show the link's current label (or its product's name) and the product's current name, as the server does.
  const productName = (s: { product_id: string | null }): string | null => (s.product_id ? findProduct(s.product_id)?.name ?? null : null);
  const linkName = (s: { link_id?: string | null }): string | null => {
    const l = s.link_id ? findLink(s.link_id) : null;
    return l ? l.name ?? productOf(l).name : null;
  };
  // D47: a paid session whose payments are final leaves the automatic checks and may be checked again by hand.
  const settledAt = (s: StoredSession): string | null =>
    s.status === 'paid' && s.payments.length > 0 && s.payments.every((p) => p.status === 'finalized' || p.status === 'reversed') ? s.paid_at : null;
  const recheckAvailable = (s: StoredSession): boolean => {
    if (s.status === 'paid') return settledAt(s) !== null;
    if (s.status === 'overpaid') return false;
    return Date.now() >= Date.parse(s.expires_at) + 23.5 * 3600_000;
  };
  /** The newer session of the customer address that owns the watch of s, or null. */
  const supersededOf = (s: StoredSession): string | null => {
    const ca = s.customer_address_id ? addrs().find((x) => x.id === s.customer_address_id) : undefined;
    return ca && ca.current_session_id && ca.current_session_id !== s.id ? ca.current_session_id : null;
  };
  const withWatch = (s: StoredSession) => ({ ...s, ...(settledAt(s) ? { watch_until: settledAt(s) } : {}), recheck_available: recheckAvailable(s) });
  const listItem = ({ events: _e, ...s }: StoredSession) => ({ ...withWatch(s as StoredSession), link_name: linkName(s), product_name: productName(s) });

  const sessionDetail = (id: string): SessionDetail | null => {
    const s = db.sessions.find((x) => x.id === id);
    if (!s) return null;
    const links: ExplorerLink[] = [
      ...s.payments.filter((p) => !p.simulated).map((p) => ({ type: 'payment' as const, chain: p.chain, asset: p.asset, payment_id: p.id, value: p.tx_hash, url: explorer(p.chain, 'tx', p.tx_hash) })),
      ...(s.settlements ?? (s.settlement ? [s.settlement] : []))
        .filter((x) => !s.payments.some((p) => p.simulated && p.tx_hash === x.tx_hash))
        .map((x) => ({ type: 'settlement' as const, chain: x.chain, asset: x.asset, value: x.tx_hash, url: explorer(x.chain, 'tx', x.tx_hash) })),
      ...s.pay_to.flatMap((p) => (p.address ? [{ type: 'address' as const, chain: p.chain, asset: p.asset, value: p.address, url: explorer(p.chain, 'address', p.address) }] : [])),
    ];
    // Like the server: a session of a customer address that a newer one superseded takes no "Check again".
    const supersededBy = supersededOf(s);
    const w = withWatch(s);
    const watchUntil = w.watch_until ?? new Date(Date.parse(s.expires_at) + 23.5 * 3600_000).toISOString().replace(/\.\d{3}Z$/, 'Z');
    return {
      ...w,
      link_name: linkName(s),
      product_name: productName(s),
      explorer_links: links,
      watch_until: watchUntil,
      recheck_available: w.recheck_available && !supersededBy,
      recheck_requested_at: null,
      superseded_by: supersededBy,
      settled_with: s.settled_with ?? [],
    };
  };

  // Customer addresses (B11): the server computes the stats and what a rotation would change.
  const addrs = (): StoredCustomerAddress[] => (db.customerAddresses ??= []);
  const addressSessions = (id: string) => db.sessions.filter((s) => s.customer_address_id === id).sort((a, b) => b.created_at.localeCompare(a.created_at));
  const addressAttention = (ca: StoredCustomerAddress, mode: Mode): AddressAttention[] => {
    const out: AddressAttention[] = [];
    const p = findProduct(ca.product_id);
    if (!p || !p.active) out.push({ reason: 'product_inactive' });
    const archived = new Map<string, { id: string; label: string }>();
    for (const items of Object.values(ca.payout)) {
      for (const i of items) {
        const w = i.wallet_id ? db.wallets.find((x) => x.id === i.wallet_id) : undefined;
        if (w?.archived_at) archived.set(w.id, { id: w.id, label: w.label });
      }
    }
    if (archived.size > 0) out.push({ reason: 'archived_wallet', wallets: [...archived.values()] });
    if (p) {
      const changed = Object.keys(ca.payout)
        .sort()
        .filter((c) => JSON.stringify(ca.payout[c]!.map((i) => [i.wallet_id, i.bps])) !== JSON.stringify((p.payout[c] ?? []).map((i) => [i.wallet_id, i.bps])));
      if (changed.length > 0) out.push({ reason: 'payout_changed', chains: changed });
    }
    if (ca.fee_bps !== FEE_BPS) out.push({ reason: 'fee_changed', chains: [...new Set(ca.pay_to.map((x) => x.chain))].sort() });
    const accepted = db.accepted[mode].filter((a) => a.enabled);
    const has = (c: string, a: string) => ca.pay_to.some((x) => x.chain === c && x.asset === a);
    if (p) {
      const added = accepted.filter((a) => (p.payout[a.chain] ?? []).length > 0 && !has(a.chain, a.asset)).map((a) => ({ chain: a.chain, asset: a.asset }));
      if (added.length > 0) out.push({ reason: 'new_networks', assets: added });
    }
    if (!ca.pay_to.some((x) => accepted.some((a) => a.chain === x.chain && a.asset === x.asset))) out.push({ reason: 'no_networks' });
    return out;
  };
  const renderAddress = (ca: StoredCustomerAddress): CustomerAddress => {
    const sessions = addressSessions(ca.id);
    const stats = statsOf(sessions);
    const p = findProduct(ca.product_id);
    const last = sessions.find((s) => s.id === ca.current_session_id);
    const { fee_bps: _f, current_session_id: _c, ...rest } = ca;
    return {
      ...rest,
      url: `${CHECKOUT_BASE}/a/${ca.id}`,
      product: p ? { id: p.id, name: p.name, price_type: p.price_type, active: p.active } : null,
      stats: {
        sessions: sessions.length,
        paid: stats.paid,
        received: stats.received,
        last_paid_at: stats.last_paid_at,
        last_session: last ? { id: last.id, status: last.status, created_at: last.created_at } : null,
      },
      attention: ca.status === 'active' ? addressAttention(ca, ca.livemode ? 'live' : 'test') : [],
    };
  };
  const findAddress = (id: string, mode: Mode) => addrs().find((x) => x.id === id && x.livemode === live(mode));
  const addressNotFound = () => err(404, 'customer_address_not_found', 'no such customer address');

  // Sandbox (test mode only, as on the server).
  const testOnly = () => err(403, 'test_mode_only', 'sandbox tools work in test mode only: use an sk_test_ key or the Test mode of the dashboard');
  const EVENT_TYPES = ['session.payment_detected', 'session.partially_paid', 'session.paid', 'session.overpaid', 'session.expired', 'session.late_payment', 'session.under_review', 'payment.reversed'];
  const inboxView = (withSecret = false) =>
    db.inbox
      ? {
          id: db.inbox.id,
          url: db.inbox.url,
          endpoint_id: db.inbox.endpoint_id,
          created_at: db.inbox.created_at,
          retention_days: INBOX_RETENTION_DAYS,
          max_requests: INBOX_MAX_REQUESTS,
          max_body_bytes: INBOX_MAX_BODY,
          ...(withSecret ? { secret: db.inbox.secret } : {}),
        }
      : null;
  const sigHeader = () => `t=${Math.floor(Date.now() / 1000)},v1=${ids.hex(64)}`;
  /** Hands an event to the test endpoints: the inbox keeps it, every one logs a delivery. */
  const deliverTest = (body: string, eventId: string, type: string, onlyEndpoint?: string, test = false): WebhookDelivery[] => {
    const out: WebhookDelivery[] = [];
    for (const e of db.endpoints.filter((x) => !x.livemode && (x.status !== 'disabled' || test) && (onlyEndpoint ? x.id === onlyEndpoint : true))) {
      if (!onlyEndpoint && e.enabled_events.length > 0 && !e.enabled_events.includes(type)) continue;
      if (db.inbox && e.id === db.inbox.endpoint_id) {
        db.inboxRequests.unshift(inboxRequest(ids, body, eventId, type, Date.now()));
        db.inboxRequests = db.inboxRequests.slice(0, INBOX_MAX_REQUESTS);
      }
      const t = nowIso();
      const d: WebhookDelivery = {
        id: ids.id('whd'),
        endpoint_id: e.id,
        event_id: eventId,
        event_type: type,
        status: 'succeeded',
        attempts: 1,
        last_status_code: 200,
        last_error: null,
        last_attempt_at: t,
        next_attempt_at: null,
        delivered_at: t,
        created_at: t,
        ...(test ? { test: true } : {}),
      };
      db.deliveries.push(d);
      out.push(d);
    }
    return out;
  };
  const synthHash = (chain: string) => (chain.startsWith('solana') ? `11111111${ids.b58(80)}` : `0x0000000000000000${ids.hex(48)}`);
  const counted = (s: StoredSession) => s.payments.filter((p) => p.status === 'confirmed' || p.status === 'finalized').reduce((a, p) => a + units(p.amount), 0n);
  /** The status the payments imply, as the server's state machine computes it. */
  const derived = (s: StoredSession, expired: boolean, late: boolean): SessionStatus => {
    const recv = counted(s);
    if (expired) return late && recv > 0n ? 'late_payment' : 'expired';
    if (recv === 0n) return 'open';
    // An open-amount session: what was sent is what was paid.
    if (s.amount === null) return 'paid';
    const amt = units(s.amount);
    return recv < amt ? 'partially_paid' : recv === amt ? 'paid' : 'overpaid';
  };
  const STATUS_EVENT: Partial<Record<SessionStatus, string>> = {
    partially_paid: 'session.partially_paid',
    paid: 'session.paid',
    overpaid: 'session.overpaid',
    late_payment: 'session.late_payment',
  };
  /** Records one state change: version, event, and its deliveries. */
  const emit = (s: StoredSession, type: string, out: { id: string; type: string; session_version: number }[]) => {
    s.version += 1;
    const e = { id: ids.id('evt'), type, created_at: nowIso(), session_version: s.version };
    s.events.push(e);
    out.push({ id: e.id, type, session_version: s.version });
    deliverTest(envelope(s, e.id, type, e.created_at), e.id, type);
  };
  const recompute = (s: StoredSession, expired: boolean, late: boolean) => {
    if (s.status !== 'under_review') s.status = derived(s, expired, late);
    const recv = counted(s);
    const amt = s.amount === null ? recv : units(s.amount);
    s.amount_received = usdExact(recv);
    s.amount_remaining = usdExact(recv >= amt ? 0n : amt - recv);
    s.paid_at = s.status === 'paid' || s.status === 'overpaid' ? s.paid_at ?? nowIso() : null;
  };
  const SIM_AMOUNT = /^\d+(\.\d{1,6})?$/;
  type SimEvent = { id: string; type: string; session_version: number };
  const simulateSession = (s: StoredSession, action: SimAction, amountArg: string | undefined): Response | SimEvent[] => {
    const out: SimEvent[] = [];
    // A simulated payment on a network nobody generated reveals it, as a wallet payment would.
    if (!s.pay_to[0]!.revealed) {
      const at = nowIso();
      const sol = s.pay_to[0]!.chain.startsWith('solana');
      const address = sol ? ids.b58(44) : `0x${ids.hex(40)}`;
      s.pay_to = s.pay_to.map((p) => (p.chain === s.pay_to[0]!.chain ? { ...revealed(p, at), address, ...(sol ? { token_account: ids.b58(44) } : {}) } : p));
      s.payment_started_at ??= at;
    }
    const pt = s.pay_to[0]!;
    let expired = s.status === 'expired' || s.status === 'late_payment';
    const openAmount = s.amount === null;
    const due = () => {
      const r = units(s.amount ?? '0') - counted(s);
      return r > 0n ? r : 0n;
    };
    if (amountArg !== undefined && !['pay_partial', 'overpay', 'late_payment'].includes(action) && !(openAmount && action === 'pay_exact')) {
      return err(400, 'invalid_request', 'amount applies only to pay_partial, overpay and late_payment, and to pay_exact on an open-amount session');
    }
    if (openAmount && (action === 'pay_partial' || action === 'overpay')) return err(409, 'invalid_state', 'an open-amount session is never partially paid or overpaid; use pay_exact with an amount');
    if (openAmount && (action === 'pay_exact' || action === 'late_payment') && amountArg === undefined) return err(400, 'invalid_request', 'amount is required on an open-amount session: the payer decides how much to send');
    let amount: bigint | null = null;
    if (amountArg !== undefined) {
      if (!SIM_AMOUNT.test(amountArg)) return err(400, 'invalid_request', `amount must be a positive decimal with at most 6 places, in ${pt.asset}`);
      amount = units(amountArg);
      if (amount === 0n) return err(400, 'invalid_request', 'amount must be greater than zero');
    }
    const beforeExpiry = s.status === 'open' || s.status === 'partially_paid';
    const notPayable = () => err(409, 'invalid_state', `the session is ${s.status}: only open and partially paid sessions take payments before expiry`);
    const pay = (u: bigint) => {
      const p: Payment = { id: ids.id('pay'), chain: pt.chain, asset: pt.asset, amount: token(u), tx_hash: synthHash(pt.chain), status: 'detected', confirmed_at: null, simulated: true };
      s.payments.push(p);
      emit(s, 'session.payment_detected', out);
      p.status = 'confirmed';
      p.confirmed_at = nowIso();
      // Forwarded at once: fee to Prismaport, the rest by the recipients the session committed to.
      const st = settlementFor(db, s, p.chain, p.asset, u, p.tx_hash, nowIso());
      if (st) {
        st.simulated = true;
        const all = [...(s.settlements ?? (s.settlement ? [s.settlement] : [])), st];
        s.settlement = st;
        if (all.length > 1) s.settlements = all;
      }
      recompute(s, expired, expired);
      const ev = STATUS_EVENT[s.status];
      if (ev) emit(s, ev, out);
    };
    const expire = () => {
      s.expires_at = nowIso();
      expired = true;
      recompute(s, true, false);
      emit(s, 'session.expired', out);
    };
    switch (action) {
      case 'pay_exact':
        if (!beforeExpiry) return notPayable();
        if (openAmount) {
          pay(amount!);
          break;
        }
        if (due() === 0n) return err(409, 'invalid_state', 'nothing is left to pay on this session');
        pay(due());
        break;
      case 'pay_partial':
        if (!beforeExpiry) return notPayable();
        if (amount !== null && amount >= due()) return err(400, 'invalid_request', `a partial payment must be below the ${token(due())} ${pt.asset} still due`);
        pay(amount ?? due() / 2n);
        break;
      case 'overpay':
        if (!beforeExpiry) return notPayable();
        if (amount !== null && amount <= due()) return err(400, 'invalid_request', `an overpayment must be above the ${token(due())} ${pt.asset} still due`);
        pay(amount ?? due() + 1_000_000n);
        break;
      case 'expire':
        if (!beforeExpiry) return err(409, 'invalid_state', `only open and partially paid sessions expire; this one is ${s.status}`);
        expire();
        break;
      case 'late_payment':
        if (!beforeExpiry && !expired) return err(409, 'invalid_state', `a late payment needs an open, partially paid or expired session; this one is ${s.status}`);
        if (beforeExpiry) expire();
        pay(amount ?? (due() > 0n ? due() : units(s.amount ?? '0')));
        break;
      case 'reverse_last_payment': {
        const p = [...s.payments].reverse().find((x) => x.simulated && (x.status === 'detected' || x.status === 'confirmed'));
        if (!p) return err(409, 'invalid_state', 'the session has no simulated payment that can be reversed');
        p.status = 'reversed';
        const kept = (s.settlements ?? (s.settlement ? [s.settlement] : [])).filter((x) => x.tx_hash !== p.tx_hash);
        s.settlement = kept.at(-1) ?? null;
        if (kept.length > 1) s.settlements = kept;
        else delete s.settlements;
        p.confirmed_at = null;
        recompute(s, expired, s.status === 'late_payment');
        emit(s, 'payment.reversed', out);
        break;
      }
      case 'under_review':
        if (s.status === 'under_review') return err(409, 'invalid_state', 'the session is already under review');
        s.status = 'under_review';
        emit(s, 'session.under_review', out);
        break;
      default:
        return err(400, 'invalid_request', `unknown action "${String(action)}"`);
    }
    return out;
  };

  const handlers = [
    http.post(`${BASE}/auth/signup`, async ({ request }) => {
      const b = await publicBody(request);
      const name = typeof b.business_name === 'string' ? b.business_name.trim().replace(/\s+/g, ' ') : '';
      const email = typeof b.email === 'string' ? b.email.trim().toLowerCase() : '';
      const password = typeof b.password === 'string' ? b.password : '';
      if (!name || name.length > 200) return err(400, 'invalid_request', 'Enter your business name, up to 200 characters.');
      if (!EMAIL.test(email)) return err(400, 'invalid_request', 'email must be a plain address such as owner@example.com');
      const refusedSignup = passwordRefusal(password, 'password');
      if (refusedSignup) return refusedSignup;
      const existing = findUser(email);
      if (!existing) {
        state.accounts.push(createSignupDb(name, email, password, ids.id));
      } else if (existing.email_verified === false) {
        // A re-submit only sends a fresh code: the password is set by whoever enters it.
        existing.code_tries = 0;
      }
      save();
      return HttpResponse.json({ ok: true });
    }),

    http.post(`${BASE}/auth/verify-email`, async ({ request }) => {
      const b = await publicBody(request);
      const u = findUser(String(b.email ?? ''));
      // A browser signed in as this user proved the password; any other one sends it.
      const acc = u ? accountWith(u) : null;
      const signedIn = Boolean(u && acc?.session?.userId === u.id);
      const password = typeof b.password === 'string' ? b.password : '';
      if (!signedIn) {
        if (!password) return err(400, 'invalid_request', 'Enter the password for this account, or sign in first.', 'password');
        const refused = passwordRefusal(password, 'password');
        if (refused) return refused;
      }
      if (!u || u.email_verified !== false || !codeMatches(u, b.code)) return invalidCode();
      u.email_verified = true;
      u.code_tries = 0;
      if (!signedIn) {
        u.password = password;
        acc!.session = null;
      }
      save();
      return HttpResponse.json({ verified: true });
    }),

    http.post(`${BASE}/auth/resend-code`, async ({ request }) => {
      const b = await publicBody(request);
      const u = findUser(String(b.email ?? ''));
      if (u && u.email_verified === false) u.code_tries = 0;
      save();
      return HttpResponse.json({ ok: true });
    }),

    http.post(`${BASE}/auth/password/forgot`, async ({ request }) => {
      const b = await publicBody(request);
      const u = findUser(String(b.email ?? ''));
      // A newer link replaces the one before: one account holds the link at a time.
      for (const a of state.accounts) for (const x of a.users) x.reset_link = false;
      if (u) u.reset_link = true;
      save();
      return HttpResponse.json({ ok: true });
    }),

    http.post(`${BASE}/auth/password/reset/check`, async ({ request }) => {
      const b = await publicBody(request);
      const u = resetLinkUser(b.token);
      if (!u) return invalidLink();
      return HttpResponse.json({ email: u.email, totp_required: u.totp_enabled });
    }),

    http.post(`${BASE}/auth/password/reset`, async ({ request }) => {
      const b = await publicBody(request);
      const u = resetLinkUser(b.token);
      if (!u) return invalidLink();
      settle(u);
      const pw = typeof b.new_password === 'string' ? b.new_password : '';
      const refused = passwordRefusal(pw, 'new_password');
      if (refused) return refused;
      if (u.totp_enabled) {
        if (!b.totp && !b.recovery_code) return err(401, 'totp_required', 'Enter the 6-digit code from your authenticator app.');
        if (b.recovery_code ? !takeRecoveryCode(u, b.recovery_code) : b.totp !== MOCK_TOTP) return err(401, 'totp_required', 'That code is not valid. Enter the current code from your authenticator app.');
      }
      u.password = pw;
      u.reset_link = false;
      u.email_verified = true;
      accountWith(u).session = null;
      save();
      return HttpResponse.json({ ok: true });
    }),

    http.post(`${BASE}/auth/login`, async ({ request }) => {
      if (opts.latency) await delay(opts.latency);
      const b = (await request.json().catch(() => ({}))) as { email?: string; password?: string; totp?: string; recovery_code?: string };
      const u = accountOf(b.email ?? '');
      // Like the server: a wrong email or password is unauthenticated; the right password with
      // 2FA on asks for the code by name.
      if (!u || b.password !== (u.password ?? MOCK_PASSWORD)) return err(401, 'unauthenticated', 'Email or password is incorrect.');
      settle(u);
      let usedRecovery = false;
      if (u.totp_enabled) {
        if (b.totp && b.recovery_code) return err(400, 'invalid_request', 'Send the authenticator code or a recovery code, not both.');
        if (b.recovery_code) {
          if (!takeRecoveryCode(u, b.recovery_code)) return err(401, 'totp_required', 'That recovery code is not valid or was already used.');
          usedRecovery = true;
        } else if (!b.totp) return err(401, 'totp_required', 'Enter the 6-digit code from your authenticator app.');
        else if (b.totp !== MOCK_TOTP) return err(401, 'totp_required', 'That code is not valid. Enter the current code from your authenticator app.');
        // Signing in with the app or a recovery code is the owner: it cancels a pending removal.
        u.totp_removal_at = null;
      }
      db.session = { userId: u.id, csrf: ids.hex(32) };
      save();
      return HttpResponse.json({ user: userView(u), csrf_token: db.session.csrf, ...(usedRecovery ? { used_recovery_code: true } : {}) });
    }),

    http.post(`${BASE}/auth/logout`, authed(() => {
      db.session = null;
      return HttpResponse.json({ logged_out: true });
    })),

    // Unlock and Lock now (D61): the confirmation window without an action.
    http.post(`${BASE}/confirmation_window`, authed(({ request, body }) => {
      if (new URL(request.url).searchParams.get('mode') !== 'live') return err(400, 'invalid_request', 'Test mode needs no code: there is nothing to unlock.');
      if (opts.confirmWindow === false) return err(409, 'invalid_state', 'Unlocking is off on this server: each Live change asks for its own code.');
      const refused = confirmation(body, 'unlock');
      if (refused) return refused;
      db.session!.confirmed_until = Date.now() + CONFIRM_WINDOW_MS;
      db.session!.confirmed_wallets = 0;
      save();
      return HttpResponse.json({ confirmation_window: confirmationWindow() });
    })),
    http.delete(`${BASE}/confirmation_window`, authed(() => {
      db.session!.confirmed_until = undefined;
      db.session!.confirmed_wallets = 0;
      save();
      return HttpResponse.json({ confirmation_window: null });
    })),
    http.get(`${BASE}/me`, authed(() => {
      const u = user()!;
      return HttpResponse.json({ user: userView(u), merchant: { ...db.merchant, live_enabled: db.merchant.live_enabled !== false, has_live_wallet: db.wallets.some((w) => w.livemode && !w.archived_at), logo_url: db.merchant.logo_url ?? null }, csrf_token: db.session!.csrf, confirmation_window: confirmationWindow(), confirmation_window_seconds: opts.confirmWindow === false ? 0 : CONFIRM_WINDOW_MS / 1000, chain_pauses: db.chain_pauses ?? [] });
    })),

    // The checkout logo belongs to the merchant, not to a mode: these routes take no ?mode=.
    http.put(`${BASE}/logo`, authed(({ body }) => {
      const image = typeof body.image === 'string' ? body.image : '';
      const bytes = (() => {
        try {
          return Uint8Array.from(atob(image), (c) => c.charCodeAt(0));
        } catch {
          return null;
        }
      })();
      const at = (i: number[], off = 0) => !!bytes && i.every((b, k) => bytes[off + k] === b);
      const known = at([0x89, 0x50, 0x4e, 0x47]) || at([0xff, 0xd8, 0xff]) || (at([0x52, 0x49, 0x46, 0x46]) && at([0x57, 0x45, 0x42, 0x50], 8));
      if (!bytes || !known) return err(400, 'invalid_request', 'image must be a PNG, JPEG or WebP file', 'image');
      if (bytes.length > 512 * 1024) return err(400, 'invalid_request', 'image is larger than 512 KB', 'image');
      const id = Array.from({ length: 32 }, () => Math.floor(Math.random() * 16).toString(16)).join('');
      logos.set(id, image);
      db.merchant.logo_url = `/v1/checkout/logos/${id}.png`;
      save();
      return HttpResponse.json({ logo_url: db.merchant.logo_url });
    })),

    http.delete(`${BASE}/logo`, authed(() => {
      db.merchant.logo_url = null;
      save();
      return HttpResponse.json({ logo_url: null });
    })),

    http.get('*/v1/checkout/logos/:file', ({ params }) => {
      const image = logos.get(String(params.file).replace(/\.png$/, ''));
      if (!image) return new HttpResponse(null, { status: 404 });
      return new HttpResponse(Uint8Array.from(atob(image), (c) => c.charCodeAt(0)), { headers: { 'Content-Type': 'image/png' } });
    }),

    http.post(`${BASE}/confirmations`, authed(({ request, body }) => {
      const u = user()!;
      const purposes = ['wallet_create', 'wallet_archive', 'api_key', 'webhook_endpoint', 'unlock', 'totp_enroll'];
      if (!purposes.includes(String(body.purpose))) return err(400, 'invalid_request', 'purpose is not one of ' + purposes.join(', ') + '.');
      if (body.purpose !== 'totp_enroll' && new URL(request.url).searchParams.get('mode') !== 'live') return err(400, 'invalid_request', 'Test mode needs no confirmation: send the request without a code.');
      const purpose = String(body.purpose);
      if (purpose === 'totp_enroll' && u.totp_enabled) return err(400, 'invalid_request', 'Two-factor authentication is on: confirm with your authenticator app or a recovery code.');
      if (!emailOn) return HttpResponse.json({ error: { code: 'service_unavailable', message: 'Codes by email are not set up on this server. Use your authenticator app.' } }, { status: 503 });
      // A failed send deletes the code just issued, so the resend gap does not apply to the retry.
      if (opts.sendFails) return err(503, 'email_send_failed', "We couldn't send the code. Try again in a moment.");
      const wait = Math.ceil(((u.confirm_sent_at?.[purpose] ?? 0) + 60_000 - Date.now()) / 1000);
      if (wait > 0) return HttpResponse.json({ error: { code: 'rate_limited', message: `A code was just sent. Wait ${wait} seconds before asking for a new one.` } }, { status: 429, headers: { 'Retry-After': String(wait) } });
      u.confirm_sent_at = { ...u.confirm_sent_at, [purpose]: Date.now() };
      u.confirm_tries = 0;
      const at = u.email.indexOf('@');
      return HttpResponse.json({ sent_to: u.email[0] + '•••' + u.email.slice(at), expires_in: 900, resend_after: 60 });
    })),

    http.post(`${BASE}/auth/totp/enroll`, authed(({ body }) => {
      const u = user()!;
      if (u.totp_enabled) {
        const refused = secondFactor(u, body);
        if (refused) return refused;
      } else {
        // Turning it on is confirmed with an emailed code, never the password.
        const refused = confirmation({ email_code: body.email_code }, 'totp_enroll');
        if (refused) return refused;
      }
      u.totp_pending = true;
      const secret = u.totp_enabled ? 'KRSXG5CTMVRXEZLU' : 'JBSWY3DPEHPK3PXP';
      return HttpResponse.json({ secret, otpauth_url: `otpauth://totp/Prismaport:${encodeURIComponent(u.email)}?secret=${secret}&issuer=Prismaport` });
    })),

    http.post(`${BASE}/auth/totp/verify`, authed(({ body }) => {
      const u = user()!;
      if (!u.totp_pending) return err(400, 'invalid_request', 'start the two-factor setup again');
      if (body.code !== MOCK_TOTP) return err(400, 'totp_required', 'That code is not valid. Enter the current code from your authenticator app.');
      // The codes come with the first turn-on only; a replace keeps the ones in hand and ends a pending removal.
      const first = !u.totp_enabled;
      u.totp_enabled = true;
      u.totp_pending = false;
      u.totp_removal_at = null;
      if (first) u.recovery_codes = newRecoveryCodes();
      return HttpResponse.json({ user: userView(u), ...(first ? { recovery_codes: [...u.recovery_codes!] } : {}) });
    })),

    http.post(`${BASE}/auth/totp/disable`, authed(({ body }) => {
      const u = user()!;
      if (body.code == null && body.recovery_code == null) return err(400, 'invalid_request', 'code or recovery_code is required');
      if (!u.totp_enabled) return err(400, 'invalid_request', 'two-factor authentication is off');
      const refused = secondFactor(u, body);
      if (refused) return refused;
      u.totp_enabled = false;
      u.totp_pending = false;
      u.recovery_codes = undefined;
      u.totp_removal_at = null;
      return HttpResponse.json({ user: userView(u) });
    })),

    http.post(`${BASE}/auth/totp/recovery_codes`, authed(({ body }) => {
      const u = user()!;
      if (!u.totp_enabled) return err(400, 'invalid_request', 'two-factor authentication is off');
      const refused = secondFactor(u, body);
      if (refused) return refused;
      u.recovery_codes = newRecoveryCodes();
      return HttpResponse.json({ recovery_codes: [...u.recovery_codes] });
    })),

    http.post(`${BASE}/auth/totp/removal/cancel`, authed(() => {
      const u = user()!;
      u.totp_removal_at = null;
      return HttpResponse.json({ user: userView(u) });
    })),

    http.post(`${BASE}/auth/password/change`, authed(({ body }) => {
      const u = user()!;
      if (body.current_password !== (u.password ?? MOCK_PASSWORD)) return err(403, 'invalid_password', 'That password is not correct.');
      if (u.totp_enabled) {
        if (!body.totp) return err(403, 'totp_required', 'Enter the 6-digit code from your authenticator app.');
        if (body.totp !== MOCK_TOTP) return wrongCode();
      }
      const pw = typeof body.new_password === 'string' ? body.new_password : '';
      if (pw === (u.password ?? MOCK_PASSWORD)) return err(400, 'invalid_request', 'Choose a password different from the current one.', 'new_password');
      const refused = passwordRefusal(pw, 'new_password');
      if (refused) return refused;
      u.password = pw;
      return HttpResponse.json({ ok: true });
    })),

    // Lost app and no codes: a link by email (the same answer for every email), the password starts the wait.
    http.post(`${BASE}/auth/totp/lost`, async ({ request }) => {
      const b = await publicBody(request);
      const u = findUser(String(b.email ?? ''));
      for (const a of state.accounts) for (const x of a.users) x.lost_link = false;
      if (u) {
        settle(u);
        if (u.totp_enabled && u.email_verified !== false && !u.totp_removal_at) u.lost_link = true;
      }
      save();
      return HttpResponse.json({ ok: true });
    }),

    http.post(`${BASE}/auth/totp/lost/check`, async ({ request }) => {
      const b = await publicBody(request);
      const u = lostLinkUser(b.token);
      return u ? HttpResponse.json({ email: u.email }) : noLostLink();
    }),

    http.post(`${BASE}/auth/totp/lost/confirm`, async ({ request }) => {
      const b = await publicBody(request);
      const u = lostLinkUser(b.token);
      if (!u) return noLostLink();
      if (b.password !== (u.password ?? MOCK_PASSWORD)) return err(403, 'invalid_password', 'That password is not correct.');
      // A request already waiting keeps its time.
      u.totp_removal_at ??= new Date(Date.now() + MOCK_REMOVAL_WAIT_MS).toISOString();
      u.lost_link = false;
      save();
      return HttpResponse.json({ removal_at: u.totp_removal_at, cancel_token: MOCK_CANCEL_TOKEN });
    }),

    http.post(`${BASE}/auth/totp/removal/cancel_link`, async ({ request }) => {
      const b = await publicBody(request);
      const u = b.token === MOCK_CANCEL_TOKEN ? state.accounts.flatMap((a) => a.users).find((x) => x.totp_removal_at) : undefined;
      if (!u) return err(400, 'invalid_link', 'This link no longer works. The removal was already cancelled or has happened.');
      u.totp_removal_at = null;
      save();
      return HttpResponse.json({ ok: true });
    }),

    http.get(`${BASE}/overview`, authed(({ request }) => {
      const mode = modeOf(request);
      const now = Date.now();
      const sessions = db.sessions.filter((s) => s.livemode === live(mode));
      const totals = new Map<string, { chain: string; asset: string; received: bigint; fees: bigint; net: bigint }>();
      for (const s of sessions) {
        for (const x of s.settlements ?? (s.settlement ? [s.settlement] : [])) {
          const k = `${x.chain}:${x.asset}`;
          const t = totals.get(k) ?? { chain: x.chain, asset: x.asset, received: 0n, fees: 0n, net: 0n };
          t.received += units(x.gross);
          t.fees += units(x.fee);
          t.net += units(x.net);
          totals.set(k, t);
        }
      }
      const tok = (u: bigint) => {
        const s = u.toString().padStart(7, '0');
        return `${s.slice(0, -6)}.${s.slice(-6)}`;
      };
      const attention: Overview['links_attention'] = [];
      for (const l of db.links.filter((x) => x.livemode === live(mode))) {
        const r = renderLink(db, l, now);
        const name = l.name ?? productOf(l).name;
        const openSessions = db.sessions.some((s) => s.link_id === l.id && (s.status === 'open' || s.status === 'partially_paid'));
        if (r.unavailable_reason === 'product_inactive') {
          if (openSessions) attention.push({ id: l.id, name, reason: 'product_inactive' });
        } else if (l.active && r.unavailable_reason === 'exhausted') attention.push({ id: l.id, name, reason: 'exhausted' });
        else if (l.active && (r.live || r.unavailable_reason === 'reserved') && l.expires_at && Date.parse(l.expires_at) - now < 72 * 3_600_000) attention.push({ id: l.id, name, reason: 'expiring' });
        else if (!l.active && openSessions) attention.push({ id: l.id, name, reason: 'inactive_with_open_sessions' });
      }
      const recent: RecentPayment[] = sessions
        .flatMap((s) =>
          s.payments.map((p) => ({
            id: p.id,
            session_id: s.id,
            link_id: s.link_id ?? null,
            link_name: linkName(s),
            chain: p.chain,
            asset: p.asset,
            amount: p.amount,
            status: p.status,
            late: s.status === 'late_payment',
            tx_hash: p.tx_hash,
            explorer_url: p.simulated ? null : explorer(p.chain, 'tx', p.tx_hash),
            simulated: Boolean(p.simulated),
            detected_at: s.events.find((e) => e.type === 'session.payment_detected')?.created_at ?? s.created_at,
            confirmed_at: p.confirmed_at,
          })),
        )
        .sort((a, b) => b.detected_at.localeCompare(a.detected_at))
        .slice(0, 10);
      const byWallet = new Map<string, Omit<WalletTotal, 'amount'> & { u: bigint }>();
      for (const s of sessions) {
        for (const x of s.settlements ?? (s.settlement ? [s.settlement] : [])) {
          for (const p of x.payouts ?? []) {
            const k = `${p.wallet_id}:${x.chain}:${x.asset}`;
            const w = db.wallets.find((y) => y.id === p.wallet_id);
            const t = byWallet.get(k) ?? { wallet_id: p.wallet_id, label: w?.label ?? p.label, address: p.address, chain: x.chain, asset: x.asset, u: 0n };
            t.u += units(p.amount);
            byWallet.set(k, t);
          }
        }
      }
      const walletOrder = (id: string | null) => db.wallets.findIndex((w) => w.id === id);
      const body: Overview = {
        totals: [...totals.values()].map((t) => ({ chain: t.chain, asset: t.asset, received: tok(t.received), fees: tok(t.fees), net: tok(t.net) })),
        totals_by_wallet: [...byWallet.values()]
          .sort((a, b) => walletOrder(a.wallet_id) - walletOrder(b.wallet_id) || a.chain.localeCompare(b.chain) || a.asset.localeCompare(b.asset))
          .map(({ u, ...t }) => ({ ...t, amount: tok(u) })),
        recent_payments: recent,
        links_attention: attention,
      };
      return HttpResponse.json(body);
    })),

    http.get(`${BASE}/products`, authed(({ request }) => {
      const u = new URL(request.url);
      const mode = modeOf(request);
      const status = u.searchParams.get('status') ?? 'all';
      const q = (u.searchParams.get('q') ?? '').trim();
      const items = db.products
        .filter((p) => p.livemode === live(mode))
        .filter((p) => status === 'all' || (status === 'active' ? p.active : !p.active))
        .filter((p) => !q || p.id === q || p.name.toLowerCase().includes(q.toLowerCase()))
        .sort(productSort)
        .map((p) => renderProduct(db, p));
      return HttpResponse.json(page(items, u.searchParams.get('cursor')));
    })),

    http.post(`${BASE}/products`, authed(({ request, body }) => {
      const key = request.headers.get('Idempotency-Key');
      if (!key) return err(400, 'invalid_request', 'The Idempotency-Key header is required.');
      const mode = modeOf(request);
      const sig = JSON.stringify(body);
      const seen = replay(mode, 'product', key, sig);
      if (seen !== 'new') return seen;
      const b = body as Partial<ProductInput> & { active?: unknown };
      if (b.active !== undefined) return err(400, 'invalid_request', 'A new product is always active.', 'active');
      const bad = checkProductFields(b);
      if (bad) return problem(bad);
      const taken = checkNetworks(mode, (b as { networks?: unknown }).networks, b.payout, null);
      if (taken instanceof Response) return taken;
      const { payout, networks } = taken;
      const t = nowIso();
      const open = b.price_type === 'open';
      const p: StoredProduct = {
        id: ids.id('prod'),
        livemode: live(mode),
        name: b.name!.trim(),
        description: b.description?.trim() || null,
        price_type: b.price_type as PriceType,
        amount: open ? null : b.amount!,
        min_amount: open ? b.min_amount! : null,
        max_amount: open ? b.max_amount ?? null : null,
        suggested_amounts: open ? b.suggested_amounts ?? [] : [],
        active: true,
        success_url: b.success_url ?? null,
        metadata: b.metadata ?? {},
        payout,
        networks,
        created_at: t,
        updated_at: t,
      };
      db.products.push(p);
      db.idempotency[`${mode}:product:${key}`] = { body: sig, id: p.id };
      return HttpResponse.json(renderProduct(db, p));
    })),

    http.get(`${BASE}/products/:id`, authed(({ request, params }) => {
      const p = findProduct(params.id!, modeOf(request));
      return p ? HttpResponse.json(renderProduct(db, p)) : err(404, 'product_not_found', 'No such product.');
    })),

    http.patch(`${BASE}/products/:id`, authed(({ request, params, body }) => {
      const mode = modeOf(request);
      const p = findProduct(params.id!, mode);
      if (!p) return err(404, 'product_not_found', 'No such product.');
      const b = body as Partial<ProductInput> & { active?: boolean; links?: unknown };
      if (b.links !== undefined && b.links !== null && b.links !== 'keep' && b.links !== 'deactivate') return err(400, 'invalid_request', 'links must be keep or deactivate', 'links');
      if (b.price_type !== undefined && b.price_type !== p.price_type) return err(400, 'invalid_request', 'price_type cannot change after the product is created.', 'price_type');
      if (b.active !== undefined && typeof b.active !== 'boolean') return err(400, 'invalid_request', 'active must be true or false.', 'active');
      const { payout: given, networks: givenNetworks, active, links: decision, ...fields } = b as typeof b & { networks?: unknown };
      const next = { ...p, ...fields } as StoredProduct;
      if (typeof next.description === 'string') next.description = next.description.trim() || null;
      const { payout: _stored, ...plain } = next;
      const bad = checkProductFields(plain);
      if (bad) return problem(bad);
      const taken = checkNetworks(mode, givenNetworks, given, p);
      if (taken instanceof Response) return taken;
      const { payout, networks } = taken;
      // A money change asks what happens to the active links; checkouts already open never change.
      const activeLinks = db.links.filter((l) => l.product_id === p.id && l.active);
      const asks = activeLinks.length > 0 && changesMoney(p, { ...next, payout });
      if (asks && !decision) {
        const n = activeLinks.length;
        return err(409, 'links_decision_required', n === 1 ? 'This product has 1 active link. Choose whether to keep it active or deactivate it.' : `This product has ${n} active links. Choose whether to keep them active or deactivate them.`, 'links');
      }
      const deactivate = asks && decision === 'deactivate';
      if (fields.metadata !== undefined && !deactivate) {
        const over = activeLinks.find((l) => Object.keys({ ...next.metadata, ...l.metadata }).length > META_MAX);
        if (over) return err(400, 'invalid_request', `with the metadata of the active link ${over.id}, metadata has more than ${META_MAX} keys`, 'metadata');
      }
      const t = nowIso();
      if (deactivate) for (const l of activeLinks) Object.assign(l, { active: false, updated_at: t });
      Object.assign(p, next, { payout, networks, ...(active !== undefined ? { active } : {}), updated_at: t });
      return HttpResponse.json(renderProduct(db, p));
    })),

    http.get(`${BASE}/products/:id/links`, authed(({ request, params }) => {
      const mode = modeOf(request);
      if (!findProduct(params.id!, mode)) return err(404, 'product_not_found', 'No such product.');
      const u = new URL(request.url);
      return HttpResponse.json(page(linksOfMode(mode, { productId: params.id!, status: u.searchParams.get('status') }), u.searchParams.get('cursor')));
    })),

    http.get(`${BASE}/products/:id/sessions`, authed(({ request, params }) => {
      const mode = modeOf(request);
      if (!findProduct(params.id!, mode)) return err(404, 'product_not_found', 'No such product.');
      const items = db.sessions
        .filter((s) => s.product_id === params.id)
        .sort((a, b) => b.created_at.localeCompare(a.created_at))
        .map(listItem);
      return HttpResponse.json(page(items, new URL(request.url).searchParams.get('cursor')));
    })),

    http.get(`${BASE}/links`, authed(({ request }) => {
      const u = new URL(request.url);
      const items = linksOfMode(modeOf(request), { productId: u.searchParams.get('product_id'), status: u.searchParams.get('status'), q: u.searchParams.get('q') ?? '' });
      return HttpResponse.json(page(items, u.searchParams.get('cursor')));
    })),

    http.post(`${BASE}/links`, authed(({ request, body }) => {
      const key = request.headers.get('Idempotency-Key');
      if (!key) return err(400, 'invalid_request', 'The Idempotency-Key header is required.');
      const mode = modeOf(request);
      const sig = JSON.stringify(body);
      const seen = replay(mode, 'link', key, sig);
      if (seen !== 'new') return seen;
      const b = body as Partial<LinkInput> & { active?: unknown };
      if (b.active !== undefined) return err(400, 'invalid_request', 'A new link is always active.', 'active');
      const product = typeof b.product_id === 'string' ? findProduct(b.product_id, mode) : undefined;
      if (!product) return err(404, 'product_not_found', 'No such product in this mode.', 'product_id');
      if (b.expires_at && Date.parse(b.expires_at) <= Date.now()) return err(400, 'invalid_request', 'expires_at must be in the future.', 'expires_at');
      const bad = checkLinkFields(b, product);
      if (bad) return problem(bad);
      const t = nowIso();
      const l: StoredLink = {
        id: ids.id('plink'),
        livemode: live(mode),
        product_id: product.id,
        name: b.name?.trim() || null,
        amount: b.amount ?? null,
        active: true,
        success_url: b.success_url ?? null,
        metadata: b.metadata ?? {},
        expires_at: b.expires_at ?? null,
        max_payments: b.max_payments ?? null,
        created_at: t,
        updated_at: t,
      };
      db.links.push(l);
      db.idempotency[`${mode}:link:${key}`] = { body: sig, id: l.id };
      return HttpResponse.json(renderLink(db, l));
    })),

    http.get(`${BASE}/links/:id`, authed(({ request, params }) => {
      const l = findLink(params.id!, modeOf(request));
      return l ? HttpResponse.json(renderLink(db, l)) : err(404, 'payment_link_not_found', 'No such payment link.');
    })),

    http.patch(`${BASE}/links/:id`, authed(({ request, params, body }) => {
      const l = findLink(params.id!, modeOf(request));
      if (!l) return err(404, 'payment_link_not_found', 'No such payment link.');
      const b = body as Partial<LinkInput> & { active?: boolean };
      if (b.product_id !== undefined && b.product_id !== l.product_id) return err(400, 'invalid_request', 'product_id cannot change after the link is created.', 'product_id');
      if (b.expires_at && b.expires_at !== l.expires_at && Date.parse(b.expires_at) <= Date.now()) return err(400, 'invalid_request', 'expires_at must be in the future.', 'expires_at');
      const { product_id: _p, ...fields } = b;
      const next = { ...l, ...fields } as StoredLink;
      if (typeof next.name === 'string') next.name = next.name.trim() || null;
      const bad = checkLinkFields(next, productOf(l), { amount: b.amount !== undefined, merged: b.metadata !== undefined || b.active === true });
      if (bad) return problem(bad);
      Object.assign(l, next, { updated_at: nowIso() });
      return HttpResponse.json(renderLink(db, l));
    })),

    http.get(`${BASE}/links/:id/sessions`, authed(({ request, params }) => {
      if (!findLink(params.id!, modeOf(request))) return err(404, 'payment_link_not_found', 'No such payment link.');
      const items = db.sessions
        .filter((s) => s.link_id === params.id)
        .sort((a, b) => b.created_at.localeCompare(a.created_at))
        .map(listItem);
      return HttpResponse.json(page(items, new URL(request.url).searchParams.get('cursor')));
    })),

    http.get(`${BASE}/customer_addresses`, authed(({ request }) => {
      const u = new URL(request.url);
      const mode = modeOf(request);
      const customer = u.searchParams.get('customer_id') ?? '';
      if (customer && !CLIENT_REFERENCE.test(customer)) return err(400, 'invalid_request', "customer_id must have 1 to 128 characters, each a letter, a digit, '.', '_', ':' or '-'", 'customer_id');
      const items = addrs()
        .filter((x) => x.livemode === live(mode) && (!customer || x.customer_id === customer))
        .sort((a, b) => b.created_at.localeCompare(a.created_at) || b.id.localeCompare(a.id))
        .map(renderAddress);
      return HttpResponse.json(page(items, u.searchParams.get('cursor')));
    })),
    http.get(`${BASE}/customer_addresses/:id`, authed(({ request, params }) => {
      const ca = findAddress(params.id!, modeOf(request));
      return ca ? HttpResponse.json(renderAddress(ca)) : addressNotFound();
    })),
    http.get(`${BASE}/customer_addresses/:id/sessions`, authed(({ request, params }) => {
      const ca = findAddress(params.id!, modeOf(request));
      if (!ca) return addressNotFound();
      return HttpResponse.json(page(addressSessions(ca.id).map(listItem), new URL(request.url).searchParams.get('cursor')));
    })),
    http.post(`${BASE}/customer_addresses/:id/rotate`, authed(({ request, params }) => {
      const mode = modeOf(request);
      const ca = findAddress(params.id!, mode);
      if (!ca) return addressNotFound();
      if (ca.status !== 'active') return err(409, 'address_rotated', `this customer address was rotated; the customer's address is now ${ca.replaced_by}`);
      const p = findProduct(ca.product_id);
      if (!p || !p.active) return err(400, 'product_inactive', "this customer address's product is no longer available");
      const payout = snapshotPayout(db, p.payout);
      const at = nowIso();
      const next: StoredCustomerAddress = {
        id: ids.id('caddr'), livemode: ca.livemode, customer_id: ca.customer_id, product_id: ca.product_id, status: 'active', replaced_by: null,
        pay_to: addressPayTo(db, mode, payout, ids), payout, metadata: { ...ca.metadata }, created_at: at, rotated_at: null, fee_bps: FEE_BPS, current_session_id: null,
      };
      Object.assign(ca, { status: 'rotated', replaced_by: next.id, rotated_at: at });
      addrs().push(next);
      save();
      return HttpResponse.json(renderAddress(next));
    })),

    http.get(`${BASE}/sessions`, authed(({ request }) => {
      const u = new URL(request.url);
      const mode = modeOf(request);
      const addressId = u.searchParams.get('customer_address_id');
      const status = u.searchParams.get('status');
      const linkId = u.searchParams.get('link_id');
      const productId = u.searchParams.get('product_id');
      const q = (u.searchParams.get('q') ?? '').trim();
      const ref = u.searchParams.get('client_reference_id') ?? '';
      if (ref && !CLIENT_REFERENCE.test(ref)) {
        return err(400, 'invalid_request', "client_reference_id must have 1 to 128 characters, each a letter, a digit, '.', '_', ':' or '-'");
      }
      const items = db.sessions
        .filter((s) => s.livemode === live(mode))
        .filter((s) => !status || s.status === status)
        .filter((s) => !productId || s.product_id === productId)
        .filter((s) => !linkId || s.link_id === linkId)
        .filter((s) => !addressId || s.customer_address_id === addressId)
        .filter((s) => !ref || s.client_reference_id === ref)
        .filter((s) => !q || matchesSearch(s, q))
        .sort((a, b) => b.created_at.localeCompare(a.created_at))
        .map(listItem);
      return HttpResponse.json(page(items, u.searchParams.get('cursor')));
    })),

    http.get(`${BASE}/sessions/:id`, authed(({ request, params }) => {
      const s = sessionDetail(params.id!);
      if (s && s.livemode !== live(modeOf(request))) return err(404, 'session_not_found', 'No such session.');
      return s ? HttpResponse.json(s) : err(404, 'session_not_found', 'No such session.');
    })),

    http.get(`${BASE}/api_keys`, authed(({ request }) => {
      const mode = modeOf(request);
      db.apiKeys.forEach(refreshKey);
      // Keys that still work first, then expired and revoked ones; newest first within each.
      const works = (k: StoredKey) => (k.status === 'active' || k.status === 'expiring' ? 0 : 1);
      const keys = db.apiKeys.filter((k) => k.livemode === live(mode)).sort((a, b) => works(a) - works(b) || b.created_at.localeCompare(a.created_at));
      return HttpResponse.json({ data: keys });
    })),

    http.post(`${BASE}/api_keys`, authed(({ body }) => {
      const mode: Mode = body.mode === 'test' ? 'test' : 'live';
      const name = typeof body.name === 'string' ? body.name.trim() : '';
      const scopes = Array.isArray(body.scopes) ? [...new Set(body.scopes as string[])] : [];
      if (!name) return err(400, 'invalid_request', 'name is required.');
      if (scopes.length === 0) return err(400, 'invalid_request', 'choose at least one scope');
      const unknown = scopes.find((sc) => !KEY_SCOPES.includes(sc));
      if (unknown) return err(400, 'invalid_request', `unknown scope "${unknown}"`);
      const refused = windowStepUp(mode, body, 'api_key');
      if (refused) return refused;
      const secret = `sk_${mode}_${ids.b58(43)}`;
      const k: StoredKey = { id: ids.id('key'), name, scopes, mode, livemode: mode === 'live', prefix: secret.slice(0, 12), created_at: nowIso(), last_used_at: null, revoked_at: null, expires_at: null, replaced_by: null, status: 'active' };
      db.apiKeys.push(k);
      return HttpResponse.json({ ...k, secret });
    })),

    http.post(`${BASE}/api_keys/:id/roll`, authed(({ request, params, body }) => {
      const mode = modeOf(request);
      const overlaps: Record<string, number> = { now: 0, '1h': 3_600_000, '24h': 86_400_000, '7d': 7 * 86_400_000 };
      const overlap = typeof body.overlap === 'string' ? body.overlap : '';
      if (!(overlap in overlaps)) return err(400, 'invalid_request', 'overlap must be one of now, 1h, 24h or 7d');
      const k = db.apiKeys.find((x) => x.id === params.id && x.livemode === live(mode));
      if (!k) return err(404, 'not_found', 'no such API key');
      refreshKey(k);
      if (k.status !== 'active') return err(409, 'invalid_state', 'this key is revoked, expired or already rolled; roll the key that replaced it');
      const refused = liveStepUp(mode, body, 'api_key');
      if (refused) return refused;
      const secret = `sk_${mode}_${ids.b58(43)}`;
      const now = Date.now();
      const fresh: StoredKey = { ...k, id: ids.id('key'), prefix: secret.slice(0, 12), created_at: nowIso(), last_used_at: null, revoked_at: null, expires_at: null, replaced_by: null, status: 'active' };
      const ms = overlaps[overlap]!;
      k.expires_at = new Date(now + ms).toISOString().replace(/\.\d{3}Z$/, 'Z');
      k.replaced_by = fresh.id;
      if (ms === 0) k.revoked_at = k.expires_at;
      refreshKey(k);
      db.apiKeys.push(fresh);
      return HttpResponse.json({ api_key: { ...fresh, secret }, previous: k });
    })),

    http.post(`${BASE}/api_keys/:id/revoke`, authed(({ request, params }) => {
      const k = db.apiKeys.find((x) => x.id === params.id && x.livemode === live(modeOf(request)));
      if (!k) return err(404, 'not_found', 'no such API key');
      k.revoked_at ??= nowIso();
      refreshKey(k);
      return HttpResponse.json(k);
    })),

    http.get(`${BASE}/webhook_endpoints`, authed(({ request }) => {
      const mode = modeOf(request);
      for (const e of db.endpoints) {
        if (e.previous_secret_expires_at && Date.parse(e.previous_secret_expires_at) <= Date.now()) e.previous_secret_expires_at = null;
      }
      return HttpResponse.json({ data: db.endpoints.filter((e) => e.livemode === live(mode)) });
    })),

    http.post(`${BASE}/webhook_endpoints`, authed(({ request, body }) => {
      const mode = modeOf(request);
      const url = typeof body.url === 'string' ? body.url.trim() : '';
      if (endpointUrlError(url, mode)) {
        return err(400, 'invalid_request', mode === 'live' ? 'live webhook endpoints must use https://' : 'url must use https://; plain http:// is accepted only in test mode, for localhost or the sandbox inbox');
      }
      // A live endpoint takes the step-up of a live API key (ADR 0008 C-3).
      const refused = windowStepUp(mode, body, 'webhook_endpoint');
      if (refused) return refused;
      const e: StoredEndpoint = { id: ids.id('we'), url, enabled_events: (body.enabled_events as string[]) ?? [], description: typeof body.description === 'string' && body.description ? body.description : null, livemode: live(mode), created_at: nowIso(), status: 'enabled', disabled_reason: null, disabled_at: null, failing_since: null, previous_secret_expires_at: null };
      db.endpoints.push(e);
      return HttpResponse.json({ ...e, secret: `whsec_${ids.b58(32)}` });
    })),

    http.patch(`${BASE}/webhook_endpoints/:id`, authed(({ request, params, body }) => {
      const mode = modeOf(request);
      const e = db.endpoints.find((x) => x.id === params.id && x.livemode === live(mode));
      if (!e) return err(404, 'not_found', 'no such webhook endpoint');
      if (db.inbox?.endpoint_id === e.id) return err(400, 'invalid_request', 'the sandbox inbox endpoint cannot be edited');
      const keysIn = ['url', 'enabled_events', 'description', 'status'].filter((k) => k in body);
      if (keysIn.length === 0) return err(400, 'invalid_request', 'send at least one of url, enabled_events, description, status');
      if ('url' in body) {
        const url = typeof body.url === 'string' ? body.url.trim() : '';
        if (endpointUrlError(url, mode)) return err(400, 'invalid_request', mode === 'live' ? 'live webhook endpoints must use https://' : 'url must use https://; plain http:// is accepted only in test mode, for localhost or the sandbox inbox');
        if (url !== e.url) {
          const refused = windowStepUp(mode, body, 'webhook_endpoint');
          if (refused) return refused;
        }
      }
      if ('url' in body) e.url = (body.url as string).trim();
      if ('enabled_events' in body) e.enabled_events = (body.enabled_events as string[]) ?? [];
      if ('description' in body) e.description = typeof body.description === 'string' && body.description !== '' ? body.description : null;
      if ('status' in body) {
        if (body.status === 'disabled' && e.status !== 'disabled') {
          e.status = 'disabled';
          e.disabled_reason = 'merchant';
          e.disabled_at = nowIso();
          for (const d of db.deliveries) if (d.endpoint_id === e.id && d.status === 'pending') Object.assign(d, { status: 'dead', next_attempt_at: null, last_error: 'endpoint paused' });
        } else if (body.status === 'enabled' && e.status === 'disabled') {
          e.status = 'enabled';
          e.disabled_reason = null;
          e.disabled_at = null;
          e.failing_since = null;
        } else if (body.status !== 'enabled' && body.status !== 'disabled') {
          return err(400, 'invalid_request', 'status must be enabled or disabled');
        }
      }
      return HttpResponse.json(e);
    })),

    http.post(`${BASE}/webhook_endpoints/:id/rotate_secret`, authed(({ request, params, body }) => {
      const e = db.endpoints.find((x) => x.id === params.id);
      if (!e) return err(404, 'not_found', 'No such endpoint.');
      // Checked before the step-up, so a refused rotation never spends a code.
      if (e.previous_secret_expires_at && Date.parse(e.previous_secret_expires_at) > Date.now()) {
        return err(409, 'secret_rotation_pending', 'The previous secret still signs deliveries until its window ends. Rotate again after that; if the current secret leaked, delete this endpoint and create a new one.');
      }
      const refused = liveStepUp(modeOf(request), body, 'webhook_endpoint');
      if (refused) return refused;
      e.previous_secret_expires_at = new Date(Date.now() + 86_400_000).toISOString().replace(/\.\d{3}Z$/, 'Z');
      return HttpResponse.json({ ...e, secret: `whsec_${ids.b58(32)}` });
    })),

    http.delete(`${BASE}/webhook_endpoints/:id`, authed(({ params }) => {
      const before = db.endpoints.length;
      db.endpoints = db.endpoints.filter((x) => x.id !== params.id);
      return before === db.endpoints.length ? err(404, 'not_found', 'No such endpoint.') : HttpResponse.json({ id: params.id, deleted: true });
    })),

    http.get(`${BASE}/webhook_endpoints/:id/deliveries`, authed(({ request, params }) => {
      const items = db.deliveries.filter((d) => d.endpoint_id === params.id).sort((a, b) => b.created_at.localeCompare(a.created_at));
      return HttpResponse.json(page(items, new URL(request.url).searchParams.get('cursor')));
    })),

    http.post(`${BASE}/webhook_deliveries/:id/resend`, authed(({ params }) => {
      const d = db.deliveries.find((x) => x.id === params.id);
      if (!d) return err(404, 'not_found', 'No such delivery.');
      if (db.endpoints.find((x) => x.id === d.endpoint_id)?.status === 'disabled') return err(409, 'endpoint_disabled', 'the endpoint is disabled; enable it before resending');
      const copy: WebhookDelivery = { ...d, status: 'pending', attempts: 0, next_attempt_at: nowIso() };
      Object.assign(d, copy);
      return HttpResponse.json(d);
    })),

    http.post(`${BASE}/webhook_endpoints/:id/test`, authed(async ({ request, params, body }) => {
      if (modeOf(request) !== 'test') return testOnly();
      const e = db.endpoints.find((x) => x.id === params.id && !x.livemode);
      if (!e) return err(404, 'not_found', 'no such webhook endpoint');
      const type = typeof body.type === 'string' ? body.type : '';
      if (!type) return err(400, 'invalid_request', 'type is required: one of the webhook event types');
      if (!EVENT_TYPES.includes(type)) return err(400, 'invalid_request', `unknown event type "${type}"`);
      // A sample from a Test mode session in the announced state, with fresh ids.
      const want = type === 'payment.reversed' || type === 'session.payment_detected' ? 'open' : type.replace('session.', '');
      const template = db.sessions.find((x) => !x.livemode && x.status === want) ?? db.sessions.find((x) => !x.livemode)!;
      const sampleId = ids.id('ses');
      const sample: StoredSession = {
        ...template,
        id: sampleId,
        checkout_url: `https://prismaport.xyz/s/${sampleId}`,
        status: (want as SessionStatus) ?? template.status,
        payments: template.payments.map((p) => ({ ...p, id: ids.id('pay'), tx_hash: synthHash(p.chain), simulated: true })),
        events: [],
      };
      const eventId = ids.id('evt');
      const bodyText = envelope(sample, eventId, type, nowIso());
      const isInbox = db.inbox?.endpoint_id === e.id;
      const latency = 40 + Math.floor(Math.random() * 160);
      if (opts.latency) await delay(latency);
      const [d] = deliverTest(bodyText, eventId, type, e.id, true);
      const result: TestEventResult = {
        event: { id: eventId, type },
        delivery: d!,
        request: {
          method: 'POST',
          url: e.url,
          headers: [
            { name: 'Content-Type', value: 'application/json' },
            { name: 'User-Agent', value: 'Prismaport-Webhooks/1' },
            { name: 'Prismaport-Event-Id', value: eventId },
            { name: 'Signature', value: sigHeader() },
          ],
          body: bodyText,
        },
        response: { status: 200, latency_ms: latency, body: isInbox ? JSON.stringify({ id: db.inboxRequests[0]!.id, received: true }) : '{"ok":true}', body_truncated: false },
        error: null,
      };
      return HttpResponse.json(result);
    })),

    http.get(`${BASE}/sandbox/inbox`, authed(({ request }) => (modeOf(request) !== 'test' ? testOnly() : HttpResponse.json({ inbox: inboxView() })))),

    http.post(`${BASE}/sandbox/inbox`, authed(({ request }) => {
      if (modeOf(request) !== 'test') return testOnly();
      if (db.inbox) return HttpResponse.json({ inbox: inboxView() });
      const inboxToken = ids.b58(44);
      const e = { id: ids.id('we'), url: `https://prismaport.xyz/v1/sandbox/inbox/${inboxToken}`, enabled_events: [], livemode: false, created_at: nowIso(), status: 'enabled' as const, disabled_reason: null, disabled_at: null, failing_since: null };
      db.endpoints.push(e);
      db.inbox = { id: ids.id('sbx'), token: inboxToken, url: e.url, endpoint_id: e.id, created_at: e.created_at, secret: `whsec_${ids.b58(44)}` };
      db.inboxRequests = [];
      return HttpResponse.json({ inbox: inboxView(true) });
    })),

    http.delete(`${BASE}/sandbox/inbox`, authed(({ request }) => {
      if (modeOf(request) !== 'test') return testOnly();
      if (!db.inbox) return err(404, 'not_found', 'no sandbox inbox: create one first');
      const endpointId = db.inbox.endpoint_id;
      db.endpoints = db.endpoints.filter((x) => x.id !== endpointId);
      db.inbox = null;
      db.inboxRequests = [];
      return HttpResponse.json({ deleted: true });
    })),

    http.get(`${BASE}/sandbox/inbox/requests`, authed(({ request }) => {
      if (modeOf(request) !== 'test') return testOnly();
      if (!db.inbox) return err(404, 'not_found', 'no sandbox inbox: create one first');
      const cutoff = Date.now() - INBOX_RETENTION_DAYS * 86_400_000;
      return HttpResponse.json(page(db.inboxRequests.filter((r) => Date.parse(r.received_at) >= cutoff), new URL(request.url).searchParams.get('cursor')));
    })),

    http.post(`${BASE}/sessions/:id/recheck`, authed(({ request, params }) => {
      const s = db.sessions.find((x) => x.id === params.id && x.livemode === live(modeOf(request)));
      if (!s) return err(404, 'session_not_found', 'no such session');
      if (!recheckAvailable(s) && (s.status === 'paid' || s.status === 'overpaid')) return err(409, 'invalid_state', `the session is ${s.status}`);
      if (supersededOf(s)) return err(409, 'session_superseded', 'a newer session on this customer address watches it now');
      if (!recheckAvailable(s)) return err(409, 'session_still_watched', 'this session is still watched automatically');
      const last = rechecks.get(s.id);
      if (last !== undefined && Date.now() - last < 600_000) {
        return HttpResponse.json({ error: { code: 'rate_limited', message: 'rechecked recently' } }, { status: 429, headers: { 'Retry-After': String(Math.ceil((600_000 - (Date.now() - last)) / 1000)) } });
      }
      rechecks.set(s.id, Date.now());
      return HttpResponse.json({ session_id: s.id, status: 'queued' }, { status: 202 });
    })),

    http.post(`${BASE}/sessions/:id/simulate`, authed(({ request, params, body }) => {
      if (modeOf(request) !== 'test') return testOnly();
      const s = db.sessions.find((x) => x.id === params.id && !x.livemode);
      if (!s) return err(404, 'session_not_found', 'no such session');
      const action = typeof body.action === 'string' ? body.action : '';
      if (!action) return err(400, 'invalid_request', 'action is required: one of pay_exact, pay_partial, overpay, expire, late_payment, reverse_last_payment, under_review');
      const res = simulateSession(s, action as SimAction, typeof body.amount === 'string' ? body.amount : undefined);
      if (res instanceof Response) return res;
      return HttpResponse.json({ action, events: res, session: listItem(s) });
    })),

    http.get(`${BASE}/settings`, authed(({ request }) => {
      const mode = modeOf(request);
      const productNetworks = db.products
        .filter((p) => p.livemode === live(mode) && p.active)
        .sort((a, b) => (a.id < b.id ? -1 : 1))
        .flatMap((p) => renderNetworks(db, mode, p).map((n) => ({ product_id: p.id, product_name: p.name, chain: n.chain, accept: n.accept, ...(n.assets ? { assets: n.assets } : {}) })));
      return HttpResponse.json({ accepted_assets: db.accepted[mode], fee: { fee_bps: FEE_BPS, fixed_fee: FIXED_FEE }, product_networks: productNetworks });
    })),

    http.get(`${BASE}/onboarding`, authed(({ request }) => {
      const mode = modeOf(request);
      const mine = (m: Mode) => walletsOf(m).filter((w) => !w.archived_at);
      const hasProduct = db.products.some((p) => p.livemode === live(mode));
      const firstRun = db.products.length === 0;
      const hasWallet = mine(mode).length > 0;
      const steps = firstRun ? ['choose', 'wallets', 'product'] : ['wallets', 'product'];
      const step = hasProduct ? 'done' : hasWallet ? 'product' : firstRun && mine('live').length === 0 && mine('test').length === 0 ? 'choose' : 'wallets';
      return HttpResponse.json({
        visible: !hasProduct,
        first_run: firstRun,
        has_wallet: hasWallet,
        has_product: hasProduct,
        networks: chainsOf(db, mode).map((chain) => ({ chain, family: familyOf(chain), has_wallet: mine(mode).some((w) => w.family === familyOf(chain)) })),
        steps,
        step,
        done: step === 'done' ? steps.length : steps.indexOf(step),
      });
    })),

    http.get(`${BASE}/wallets`, authed(({ request }) => {
      const mode = modeOf(request);
      const all = new URL(request.url).searchParams.get('include_archived') === 'true';
      // Active wallets oldest first, then the archived ones, as the server lists them.
      const list = [...walletsOf(mode).filter((w) => !w.archived_at), ...(all ? walletsOf(mode).filter((w) => w.archived_at) : [])];
      return HttpResponse.json({ data: list.map((w) => walletView(mode, w)) });
    })),

    http.post(`${BASE}/wallets`, authed(({ request, body }) => {
      const mode = modeOf(request);
      const family = body.family;
      if (family !== 'evm' && family !== 'solana') return err(400, 'invalid_request', 'family must be evm or solana.');
      const label = typeof body.label === 'string' ? body.label.trim() : '';
      if (!label || label.length > 60) return err(400, 'invalid_request', 'label must be 1 to 60 characters.');
      const address = typeof body.address === 'string' ? body.address.trim() : '';
      // The address is checked before the step-up, so a refused address never spends a code.
      const problem = addressProblem(family, address);
      if (problem) return err(400, 'invalid_request', problem);
      const active = walletsOf(mode).filter((w) => w.family === family && !w.archived_at);
      const same = active.find((w) => sameAddr(family, w.address, address));
      if (same) return err(400, 'invalid_request', `This address is already your wallet ${same.label}.`);
      if (active.length >= 20) return err(400, 'invalid_request', `You have 20 active ${familyName(family)} wallets, the most there can be. Archive one first.`);
      const refused = windowStepUp(mode, body, 'wallet_create');
      if (refused) return refused;
      const t = nowIso();
      // Like a server without a Solana devnet provider: Test mode keeps the wallet, unchecked.
      const w: StoredWallet = { id: ids.id('wal'), label, family, address, verified: live(mode) || family === 'evm', archived_at: null, created_at: t, livemode: live(mode) };
      // Adding a wallet changes no product; the family's first wallet becomes the default.
      w.default = active.length === 0;
      db.wallets.push(w);
      return HttpResponse.json(walletView(mode, w));
    })),

    http.post(`${BASE}/wallets/sample`, authed(({ request, body }) => {
      const mode = modeOf(request);
      if (mode !== 'test') return err(403, 'test_mode_only', 'Sample wallets are for Test mode only. Add your own wallet for live payments.');
      const family = body.family ?? 'evm';
      if (family !== 'evm' && family !== 'solana') return err(400, 'invalid_request', 'family must be evm or solana');
      const address = family === 'evm' ? '0x000000000000000000000000000000000000dEaD' : '1nc1nerator11111111111111111111111111111111';
      const there = walletsOf(mode).find((w) => w.family === family && !w.archived_at && w.address === address);
      if (there) return HttpResponse.json(there);
      const t = nowIso();
      const w: StoredWallet = { id: ids.id('wal'), label: 'Sample test wallet (demo, nobody holds its key)', family, address, verified: family === 'evm',
        archived_at: null, created_at: t, livemode: false, default: walletsOf(mode).every((x) => x.family !== family || x.archived_at !== null) };
      db.wallets.push(w);
      return HttpResponse.json(walletView(mode, w));
    })),

    http.post(`${BASE}/wallets/:id/archive`, authed(({ request, params, body }) => {
      const mode = modeOf(request);
      const w = walletsOf(mode).find((x) => x.id === params.id);
      if (!w) return err(404, 'not_found', 'No such wallet.');
      if (w.archived_at) return err(409, 'invalid_state', 'This wallet is already archived.');
      const using = productsPaying(mode, w.id).length;
      if (using > 0) {
        return err(409, 'wallet_in_use', `This wallet still receives payments from ${using === 1 ? '1 product' : `${using} products`}. Change ${using === 1 ? 'that product' : 'those products'} first.`);
      }
      const refused = windowStepUp(mode, body, 'wallet_archive');
      if (refused) return refused;
      w.archived_at = nowIso();
      w.default = false;
      return HttpResponse.json(w);
    })),

    http.post(`${BASE}/wallets/:id/default`, authed(({ request, params }) => {
      const mode = modeOf(request);
      const w = walletsOf(mode).find((x) => x.id === params.id);
      if (!w) return err(404, 'not_found', 'No such wallet.');
      if (w.archived_at) return err(409, 'invalid_state', 'An archived wallet cannot be the default.');
      for (const x of walletsOf(mode)) if (x.family === w.family) x.default = x.id === w.id;
      return HttpResponse.json(walletView(mode, w));
    })),

    // The public endpoints of a link page: tests and the e2e flow open payments with them.
    http.get('/v1/checkout/links/:id', async ({ params }) => {
      if (opts.latency) await delay(opts.latency);
      const l = db.links.find((x) => x.id === params.id);
      if (!l) return err(404, 'payment_link_not_found', 'No such payment link.');
      const p = productOf(l);
      const { live: isLive, reason } = linkLiveness(db, l);
      // A link that sets an amount sells at that one price: the payer meets a fixed price.
      const fixed = p.price_type === 'fixed' || l.amount !== null;
      return HttpResponse.json({
        id: l.id,
        livemode: l.livemode,
        name: p.name,
        description: p.description,
        price_type: fixed ? 'fixed' : 'open',
        amount: p.price_type === 'fixed' ? p.amount : l.amount,
        min_amount: fixed ? null : p.min_amount,
        max_amount: fixed ? null : p.max_amount,
        suggested_amounts: fixed ? [] : p.suggested_amounts,
        currency: 'USD',
        active: isLive,
        live: isLive,
        unavailable_reason: reason,
      });
    }),

    http.post('/v1/checkout/links/:id/sessions', async ({ request, params }) => {
      if (opts.latency) await delay(opts.latency);
      const l = db.links.find((x) => x.id === params.id);
      if (!l) return err(404, 'payment_link_not_found', 'No such payment link.');
      if (!request.headers.get('Idempotency-Key')) return err(400, 'invalid_request', 'The Idempotency-Key header is required.');
      if (!linkLiveness(db, l).live) return err(410, 'link_unavailable', 'This link is no longer available.');
      const p = productOf(l);
      const b = (await request.json().catch(() => ({}))) as { amount?: string; client_reference_id?: string };
      let amount: string;
      if (p.price_type === 'fixed' || l.amount) {
        if (b.amount !== undefined) return err(400, 'invalid_request', 'amount is not accepted: this link has a fixed price.', 'amount');
        amount = p.price_type === 'fixed' ? p.amount! : l.amount!;
      } else {
        if (typeof b.amount !== 'string' || !AMOUNT.test(b.amount)) return err(400, 'invalid_request', 'amount must be a USD amount with at most two decimals.', 'amount');
        if (units(b.amount) < units(p.min_amount!)) return err(400, 'amount_below_minimum', `The amount must be at least ${p.min_amount} USD.`, 'amount');
        if (p.max_amount && units(b.amount) > units(p.max_amount)) return err(400, 'amount_above_maximum', `The amount must be at most ${p.max_amount} USD.`, 'amount');
        amount = b.amount;
      }
      const mode: Mode = l.livemode ? 'live' : 'test';
      const id = ids.id('ses');
      const t = Date.now();
      const ref = typeof b.client_reference_id === 'string' && CLIENT_REFERENCE.test(b.client_reference_id) ? b.client_reference_id : null;
      const s: StoredSession = {
        id,
        version: 1,
        livemode: l.livemode,
        status: 'open',
        product_id: p.id,
        link_id: l.id,
        link_name: l.name ?? p.name,
        product_name: p.name,
        amount,
        currency: 'USD',
        amount_received: '0.00',
        amount_remaining: usdExact(units(amount)),
        checkout_url: `${CHECKOUT_BASE}/s/${id}`,
        pay_to: payToFor(mode, amount, ids).filter((x) => (p.payout[x.chain] ?? []).length > 0).map(unrevealed),
        payment_started_at: null,
        payments: [],
        settlement: null,
        metadata: { ...p.metadata, ...l.metadata },
        client_reference_id: ref,
        client_reference_source: ref ? 'payer' : null,
        expires_at: new Date(t + 3_600_000).toISOString().replace(/\.\d{3}Z$/, 'Z'),
        created_at: nowIso(),
        paid_at: null,
        payout: snapshotPayout(db, p.payout),
        events: [],
      };
      db.sessions.push(s);
      save();
      return HttpResponse.json({ id: s.id, status: s.status, amount: s.amount, currency: s.currency, pay_to: s.pay_to, expires_at: s.expires_at, livemode: s.livemode });
    }),

    http.put(`${BASE}/accepted_assets`, authed(({ request, body }) => {
      const mode = modeOf(request);
      const items = (body.items as AcceptedAsset[]) ?? [];
      if (items.some((a) => typeof a.enabled !== 'boolean')) return err(400, 'invalid_request', 'Each item needs chain, asset and enabled.');
      const allowed = new Set(db.accepted[mode].map((a) => `${a.chain}:${a.asset}`));
      if (items.some((a) => !allowed.has(`${a.chain}:${a.asset}`))) return err(400, 'invalid_request', 'That network and stablecoin are not offered in the pilot.');
      const next = db.accepted[mode].map((a) => ({ ...a, enabled: items.find((x) => x.chain === a.chain && x.asset === a.asset)?.enabled ?? a.enabled }));
      if (!next.some((a) => a.enabled)) return err(400, 'invalid_request', 'Keep at least one stablecoin enabled.');
      db.accepted[mode] = next;
      return HttpResponse.json({ accepted_assets: db.accepted[mode] });
    })),
  ];

  return {
    handlers,
    /** Starts a session as if the user had signed in, with a known CSRF token. */
    signIn(email = 'owner@acme.studio', csrf = TEST_CSRF) {
      const u = accountOf(email);
      if (!u) throw new Error(`no mock user ${email}`);
      db.session = { userId: u.id, csrf };
      save();
    },
    /** Adds a self-serve account as a sign-up would, optionally confirmed already. */
    addSignup(name: string, email: string, password: string, verified = false) {
      const acc = createSignupDb(name, email, password, ids.id);
      acc.users[0]!.email_verified = verified;
      state.accounts.push(acc);
      save();
      return acc;
    },
    db: () => db,
    reset() {
      state = { accounts: [createDb()], active: 0 };
      db = state.accounts[0]!;
      save();
    },
  };
}

export const TEST_CSRF = 'csrf-test-token';
