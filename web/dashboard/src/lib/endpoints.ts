import { request } from './api';
import type {
  AcceptedAsset,
  ApiKey,
  CreatedApiKey,
  CustomerAddress,
  Family,
  Link,
  LinkInput,
  LinkPatch,
  InboxRequest,
  Me,
  Mode,
  Overview,
  Page,
  Product,
  ProductInput,
  ProductPatch,
  RolledApiKey,
  RollOverlap,
  SandboxInbox,
  Session,
  SessionDetail,
  Onboarding,
  Settings,
  SimAction,
  SimulateResult,
  Confirmation,
  ConfirmationPurpose,
  ConfirmationSent,
  ConfirmationWindow,
  TestEventResult,
  User,
  Wallet,
  WebhookDelivery,
  WebhookEndpoint,
  WebhookEndpointPatch,
  WebhookEndpointWithSecret,
} from './types';

const id = encodeURIComponent;

export type LinkStatusFilter = 'live' | 'inactive' | 'all';
export type ProductStatusFilter = 'active' | 'inactive' | 'all';

export interface ProductListParams {
  status?: ProductStatusFilter;
  q?: string;
  cursor?: string | null;
}

export interface LinkListParams {
  status?: LinkStatusFilter;
  product_id?: string;
  /** The link label, the product name or the exact id. */
  q?: string;
  cursor?: string | null;
}

export interface SessionListParams {
  status?: string;
  product_id?: string;
  link_id?: string;
  q?: string;
  /** Exact match, whoever set it. */
  client_reference_id?: string;
  /** The sessions opened on one customer address. */
  customer_address_id?: string;
  cursor?: string | null;
}

export interface CustomerAddressListParams {
  /** Exact match: the merchant's own id for the customer. */
  customer_id?: string;
  cursor?: string | null;
}

export const api = {
  // `used_recovery_code` is true when a recovery code stood in for the authenticator code.
  login: (body: { email: string; password: string; totp?: string; recovery_code?: string }) =>
    request<{ user: User; used_recovery_code?: boolean }>('/auth/login', { method: 'POST', body }),
  // Self-serve accounts: each answers the same whether or not the email has an account.
  signup: (body: { business_name: string; email: string; password: string }) => request<{ ok: true }>('/auth/signup', { method: 'POST', body }),
  // A browser not signed in as the user sends the password: the code sets it (ADR 0008 C-1).
  verifyEmail: (body: { email: string; code: string; password?: string }) => request<{ verified: true }>('/auth/verify-email', { method: 'POST', body }),
  resendCode: (email: string) => request<{ ok: true }>('/auth/resend-code', { method: 'POST', body: { email } }),
  forgotPassword: (email: string) => request<{ ok: true }>('/auth/password/forgot', { method: 'POST', body: { email } }),
  // A reset link names no account: the token finds it. Checking spends nothing; resetting spends the link.
  checkResetLink: (token: string) =>
    request<{ email: string; totp_required: boolean }>('/auth/password/reset/check', { method: 'POST', body: { token } }),
  resetPassword: (body: { token: string; new_password: string; totp?: string; recovery_code?: string }) =>
    request<{ ok: true }>('/auth/password/reset', { method: 'POST', body }),
  logout: () => request<void>('/auth/logout', { method: 'POST' }),
  me: () => request<Me>('/me'),
  setLogo: (image: string) => request<{ logo_url: string }>('/logo', { method: 'PUT', body: { image } }),
  removeLogo: () => request<{ logo_url: null }>('/logo', { method: 'DELETE' }),
  // Signed in: the current password, and the code when 2FA is on. Ends every other session.
  changePassword: (body: { current_password: string; new_password: string; totp?: string }) =>
    request<{ ok: true }>('/auth/password/change', { method: 'POST', body }),
  // When replacing the app, a recovery code may stand in for `code`.
  totpEnroll: (body: { email_code?: string; code?: string; recovery_code?: string }) =>
    request<{ secret: string; otpauth_url: string }>('/auth/totp/enroll', { method: 'POST', body }),
  // The recovery codes come back only when this turned 2FA on for the first time, not on a replace.
  totpVerify: (code: string) => request<{ user: User; recovery_codes?: string[] }>('/auth/totp/verify', { method: 'POST', body: { code } }),
  // Exactly one of code or recovery_code.
  totpDisable: (body: { code?: string; recovery_code?: string }) => request<unknown>('/auth/totp/disable', { method: 'POST', body }),
  // Replaces every code; takes a current authenticator code or a recovery code (one of them).
  totpRecoveryCodes: (body: { code?: string; recovery_code?: string }) => request<{ recovery_codes: string[] }>('/auth/totp/recovery_codes', { method: 'POST', body }),
  totpRemovalCancel: () => request<{ user: User }>('/auth/totp/removal/cancel', { method: 'POST' }),
  // Lost app and codes: a link by email (same answer for every email), then the password starts the wait.
  totpLost: (email: string) => request<{ ok: true }>('/auth/totp/lost', { method: 'POST', body: { email } }),
  totpLostCheck: (token: string) => request<{ email: string }>('/auth/totp/lost/check', { method: 'POST', body: { token } }),
  totpLostConfirm: (body: { token: string; password: string }) =>
    request<{ removal_at: string; cancel_token: string }>('/auth/totp/lost/confirm', { method: 'POST', body }),
  // The emailed cancel link: only this POST cancels, so a mail scanner opening the page cannot.
  totpRemovalCancelLink: (token: string) => request<{ ok: true }>('/auth/totp/removal/cancel_link', { method: 'POST', body: { token } }),

  overview: (mode: Mode) => request<Overview>('/overview', { query: { mode } }),

  products: (mode: Mode, p: ProductListParams) => request<Page<Product>>('/products', { query: { mode, status: p.status, q: p.q, cursor: p.cursor } }),
  product: (mode: Mode, productId: string) => request<Product>(`/products/${id(productId)}`, { query: { mode } }),
  createProduct: (mode: Mode, body: ProductInput, idempotencyKey: string) =>
    request<Product>('/products', { method: 'POST', body, idempotencyKey, query: { mode } }),
  updateProduct: (mode: Mode, productId: string, body: ProductPatch) =>
    request<Product>(`/products/${id(productId)}`, { method: 'PATCH', body, query: { mode } }),
  productLinks: (mode: Mode, productId: string, cursor?: string | null, f: { status?: 'live' | 'inactive' | 'all'; limit?: number } = {}) =>
    request<Page<Link>>(`/products/${id(productId)}/links`, { query: { mode, cursor, status: f.status, limit: f.limit } }),
  productSessions: (mode: Mode, productId: string, cursor?: string | null) =>
    request<Page<Session>>(`/products/${id(productId)}/sessions`, { query: { mode, cursor } }),

  links: (mode: Mode, p: LinkListParams) =>
    request<Page<Link>>('/links', { query: { mode, status: p.status, product_id: p.product_id, q: p.q, cursor: p.cursor } }),
  link: (mode: Mode, linkId: string) => request<Link>(`/links/${id(linkId)}`, { query: { mode } }),
  createLink: (mode: Mode, body: LinkInput, idempotencyKey: string) =>
    request<Link>('/links', { method: 'POST', body, idempotencyKey, query: { mode } }),
  updateLink: (mode: Mode, linkId: string, body: LinkPatch) => request<Link>(`/links/${id(linkId)}`, { method: 'PATCH', body, query: { mode } }),
  linkSessions: (mode: Mode, linkId: string, cursor?: string | null) =>
    request<Page<Session>>(`/links/${id(linkId)}/sessions`, { query: { mode, cursor } }),

  sessions: (mode: Mode, p: SessionListParams) =>
    request<Page<Session>>('/sessions', { query: { mode, status: p.status, product_id: p.product_id, link_id: p.link_id, q: p.q, client_reference_id: p.client_reference_id, customer_address_id: p.customer_address_id, cursor: p.cursor } }),
  session: (mode: Mode, sessionId: string) => request<SessionDetail>(`/sessions/${id(sessionId)}`, { query: { mode } }),

  customerAddresses: (mode: Mode, p: CustomerAddressListParams) =>
    request<Page<CustomerAddress>>('/customer_addresses', { query: { mode, customer_id: p.customer_id, cursor: p.cursor } }),
  customerAddress: (mode: Mode, addressId: string) => request<CustomerAddress>(`/customer_addresses/${id(addressId)}`, { query: { mode } }),
  customerAddressSessions: (mode: Mode, addressId: string, cursor?: string | null) =>
    request<Page<Session>>(`/customer_addresses/${id(addressId)}/sessions`, { query: { mode, cursor } }),
  /** Replaces an active address with a new one (the product's payout and the fee now); answers the new address. */
  rotateCustomerAddress: (mode: Mode, addressId: string) => request<CustomerAddress>(`/customer_addresses/${id(addressId)}/rotate`, { method: 'POST', query: { mode } }),

  apiKeys: (mode: Mode) => request<{ data: ApiKey[] }>('/api_keys', { query: { mode } }),
  createApiKey: (body: { name: string; scopes: string[]; mode: Mode } & Partial<Confirmation>) =>
    request<CreatedApiKey>('/api_keys', { method: 'POST', body, query: { mode: body.mode } }),
  revokeApiKey: (mode: Mode, keyId: string) => request<ApiKey>(`/api_keys/${id(keyId)}/revoke`, { method: 'POST', query: { mode } }),
  rollApiKey: (mode: Mode, keyId: string, body: { overlap: RollOverlap } & Partial<Confirmation>) =>
    request<RolledApiKey>(`/api_keys/${id(keyId)}/roll`, { method: 'POST', body, query: { mode } }),

  webhookEndpoints: (mode: Mode) => request<{ data: WebhookEndpoint[] }>('/webhook_endpoints', { query: { mode } }),
  // Live mode create, URL change and secret rotation take the step-up of a live API key.
  createWebhookEndpoint: (mode: Mode, body: { url: string; enabled_events: string[]; description?: string } & Partial<Confirmation>) =>
    request<WebhookEndpointWithSecret>('/webhook_endpoints', { method: 'POST', body, query: { mode } }),
  updateWebhookEndpoint: (mode: Mode, endpointId: string, body: WebhookEndpointPatch & Partial<Confirmation>) =>
    request<WebhookEndpoint>(`/webhook_endpoints/${id(endpointId)}`, { method: 'PATCH', body, query: { mode } }),
  rotateWebhookSecret: (mode: Mode, endpointId: string, body: Partial<Confirmation> = {}) =>
    request<WebhookEndpointWithSecret>(`/webhook_endpoints/${id(endpointId)}/rotate_secret`, { method: 'POST', body, query: { mode } }),
  deleteWebhookEndpoint: (mode: Mode, endpointId: string) => request<void>(`/webhook_endpoints/${id(endpointId)}`, { method: 'DELETE', query: { mode } }),
  deliveries: (mode: Mode, endpointId: string, cursor?: string | null) =>
    request<Page<WebhookDelivery>>(`/webhook_endpoints/${id(endpointId)}/deliveries`, { query: { mode, cursor } }),
  resendDelivery: (mode: Mode, deliveryId: string) =>
    request<WebhookDelivery>(`/webhook_deliveries/${id(deliveryId)}/resend`, { method: 'POST', query: { mode } }),

  // Sandbox: test mode only (the server answers 403 test_mode_only in live mode).
  sendTestEvent: (mode: Mode, endpointId: string, type: string) =>
    request<TestEventResult>(`/webhook_endpoints/${id(endpointId)}/test`, { method: 'POST', body: { type }, query: { mode } }),
  sandboxInbox: (mode: Mode) => request<{ inbox: SandboxInbox | null }>('/sandbox/inbox', { query: { mode } }),
  createSandboxInbox: (mode: Mode) => request<{ inbox: SandboxInbox }>('/sandbox/inbox', { method: 'POST', query: { mode } }),
  deleteSandboxInbox: (mode: Mode) => request<{ deleted: boolean }>('/sandbox/inbox', { method: 'DELETE', query: { mode } }),
  sandboxRequests: (mode: Mode, cursor?: string | null) => request<Page<InboxRequest>>('/sandbox/inbox/requests', { query: { mode, cursor } }),
  simulate: (mode: Mode, sessionId: string, body: { action: SimAction; amount?: string }) =>
    request<SimulateResult>(`/sessions/${id(sessionId)}/simulate`, { method: 'POST', body, query: { mode } }),

  /** Queues a one-off late check of the session's deposit entries; answers 202. */
  recheckSession: (mode: Mode, sessionId: string) =>
    request<unknown>(`/sessions/${id(sessionId)}/recheck`, { method: 'POST', query: { mode } }),

  onboarding: (mode: Mode) => request<Onboarding>('/onboarding', { query: { mode } }),
  settings: (mode: Mode) => request<Settings>('/settings', { query: { mode } }),
  setAcceptedAssets: (mode: Mode, items: AcceptedAsset[]) =>
    request<unknown>('/accepted_assets', { method: 'PUT', body: { items }, query: { mode } }),

  // Wallets. In Live mode, adding or archiving one is confirmed with an emailed code or the authenticator code.
  // A product's payout is part of the product and needs none.
  wallets: (mode: Mode) => request<{ data: Wallet[] }>('/wallets', { query: { mode, include_archived: true } }),
  createWallet: (mode: Mode, body: { label: string; family: Family; address: string } & Confirmation) =>
    request<Wallet>('/wallets', { method: 'POST', body, query: { mode } }),
  /** Test mode only: a demo address nobody holds a key for, so a new account can try the flow. */
  sampleWallet: (mode: Mode, body: { family?: Family }) => request<Wallet>('/wallets/sample', { method: 'POST', body, query: { mode } }),
  archiveWallet: (mode: Mode, walletId: string, body: Confirmation) =>
    request<Wallet>(`/wallets/${id(walletId)}/archive`, { method: 'POST', body, query: { mode } }),
  /** New products pay this wallet on its network. It moves no money of existing products, so it asks for no confirmation. */
  makeDefaultWallet: (mode: Mode, walletId: string) => request<Wallet>(`/wallets/${id(walletId)}/default`, { method: 'POST', query: { mode } }),
  /** Unlock (D61): a code opens the session's confirmation window without an action. */
  unlock: (body: Confirmation) => request<{ confirmation_window: ConfirmationWindow }>('/confirmation_window', { method: 'POST', body, query: { mode: 'live' } }),
  /** Lock now (D61): ends the session's confirmation window. */
  lock: () => request<{ confirmation_window: null }>('/confirmation_window', { method: 'DELETE' }),
  /** Emails the owner a 6-digit code for one purpose; asking again within resend_after seconds answers 429. */
  sendConfirmation: (mode: Mode, purpose: ConfirmationPurpose) =>
    request<ConfirmationSent>('/confirmations', { method: 'POST', body: { purpose }, query: purpose === 'totp_enroll' ? {} : { mode } }),
};
