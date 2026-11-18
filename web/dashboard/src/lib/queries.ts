import { QueryClient, useInfiniteQuery, useQuery } from '@tanstack/react-query';
import { useSyncExternalStore } from 'react';
import { ApiError, setCsrfToken } from './api';
import { api, type CustomerAddressListParams, type LinkListParams, type ProductListParams, type SessionListParams } from './endpoints';
import { useMode } from './mode';
import { sessionInProgress } from './status';
import type { Me } from './types';

export function makeQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 15_000,
        // Client errors (4xx) will not change on a retry; network and server errors may.
        retry: (count, e) => !(e instanceof ApiError && e.status >= 400 && e.status < 500) && count < 2,
        refetchOnWindowFocus: true,
      },
      mutations: { retry: false },
    },
  });
}

export const keys = {
  me: ['me'] as const,
  overview: (mode: string) => ['overview', mode] as const,
  products: (mode: string) => ['products', mode] as const,
  productList: (mode: string, p: ProductListParams) => ['products', mode, 'list', p] as const,
  product: (mode: string, id: string) => ['product', mode, id] as const,
  productLinks: (mode: string, id: string) => ['product', mode, id, 'links'] as const,
  productSessions: (mode: string, id: string) => ['product', mode, id, 'sessions'] as const,
  links: (mode: string) => ['links', mode] as const,
  linkList: (mode: string, p: LinkListParams) => ['links', mode, 'list', p] as const,
  link: (mode: string, id: string) => ['link', mode, id] as const,
  linkSessions: (mode: string, id: string) => ['link', mode, id, 'sessions'] as const,
  sessions: (mode: string, p?: SessionListParams) => (p ? (['sessions', mode, p] as const) : (['sessions', mode] as const)),
  session: (mode: string, id: string) => ['session', mode, id] as const,
  customers: (mode: string) => ['customers', mode] as const,
  customerList: (mode: string, p: CustomerAddressListParams) => ['customers', mode, 'list', p] as const,
  customer: (mode: string, id: string) => ['customer', mode, id] as const,
  customerSessions: (mode: string, id: string) => ['customer', mode, id, 'sessions'] as const,
  apiKeys: (mode: string) => ['api_keys', mode] as const,
  webhooks: (mode: string) => ['webhooks', mode] as const,
  deliveries: (mode: string, id: string) => ['deliveries', mode, id] as const,
  settings: (mode: string) => ['settings', mode] as const,
  inbox: (mode: string) => ['sandbox_inbox', mode] as const,
  wallets: (mode: string) => ['wallets', mode] as const,
  onboarding: (mode: string) => ['onboarding', mode] as const,
  inboxRequests: (mode: string) => ['sandbox_inbox', mode, 'requests'] as const,
};

/** The signed-in user, or null when there is no session. Keeps the CSRF token in step. */
export async function fetchMe(): Promise<Me | null> {
  try {
    const me = await api.me();
    setCsrfToken(me.csrf_token);
    return me;
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) {
      setCsrfToken(null);
      return null;
    }
    throw e;
  }
}

export function useMe() {
  return useQuery({ queryKey: keys.me, queryFn: fetchMe, staleTime: 60_000, retry: 1 });
}

export function useOverview() {
  const { mode } = useMode();
  return useQuery({ queryKey: keys.overview(mode), queryFn: () => api.overview(mode) });
}

export function useProducts(p: ProductListParams, enabled = true) {
  const { mode } = useMode();
  return useInfiniteQuery({
    queryKey: keys.productList(mode, p),
    queryFn: ({ pageParam }) => api.products(mode, { ...p, cursor: pageParam }),
    initialPageParam: null as string | null,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    enabled,
  });
}

export function useProduct(id: string | null | undefined) {
  const { mode } = useMode();
  return useQuery({ queryKey: keys.product(mode, id ?? ''), queryFn: () => api.product(mode, id!), enabled: Boolean(id) });
}

export function useProductLinks(id: string) {
  const { mode } = useMode();
  return useInfiniteQuery({
    queryKey: keys.productLinks(mode, id),
    queryFn: ({ pageParam }) => api.productLinks(mode, id, pageParam),
    initialPageParam: null as string | null,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });
}

export function useProductSessions(id: string) {
  const { mode } = useMode();
  return useInfiniteQuery({
    queryKey: keys.productSessions(mode, id),
    queryFn: ({ pageParam }) => api.productSessions(mode, id, pageParam),
    initialPageParam: null as string | null,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });
}

/** After a product or link changes: every list and page that shows either one. */
export function invalidateCatalog(qc: QueryClient): void {
  for (const k of ['products', 'product', 'links', 'link', 'overview', 'wallets', 'settings']) void qc.invalidateQueries({ queryKey: [k] });
  qc.removeQueries({ queryKey: ['home'] });
  invalidateOnboarding(qc);
}

/** After a wallet, a default wallet or the accepted networks change: where Get started stands. */
export function invalidateOnboarding(qc: QueryClient): void {
  void qc.invalidateQueries({ queryKey: ['onboarding'] });
}

export function useLinks(p: LinkListParams, enabled = true) {
  const { mode } = useMode();
  return useInfiniteQuery({
    queryKey: keys.linkList(mode, p),
    queryFn: ({ pageParam }) => api.links(mode, { ...p, cursor: pageParam }),
    initialPageParam: null as string | null,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    enabled,
  });
}

export function useLink(id: string) {
  const { mode } = useMode();
  return useQuery({ queryKey: keys.link(mode, id), queryFn: () => api.link(mode, id), enabled: Boolean(id) });
}

export function useLinkSessions(id: string) {
  const { mode } = useMode();
  return useInfiniteQuery({
    queryKey: keys.linkSessions(mode, id),
    queryFn: ({ pageParam }) => api.linkSessions(mode, id, pageParam),
    initialPageParam: null as string | null,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });
}

export function useSessions(p: SessionListParams) {
  const { mode } = useMode();
  return useInfiniteQuery({
    queryKey: keys.sessions(mode, p),
    queryFn: ({ pageParam }) => api.sessions(mode, { ...p, cursor: pageParam }),
    initialPageParam: null as string | null,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });
}

/** How often a payment that can still change by itself is checked again. */
export const SESSION_POLL_MS = 5_000;

/** One payment; while it is open or a transfer is confirming it is fetched again every few seconds. */
export function useSession(id: string) {
  const { mode } = useMode();
  return useQuery({
    queryKey: keys.session(mode, id),
    queryFn: () => api.session(mode, id),
    refetchInterval: (q) => (q.state.data && sessionInProgress(q.state.data) ? SESSION_POLL_MS : false),
  });
}

export function useCustomerAddresses(p: CustomerAddressListParams) {
  const { mode } = useMode();
  return useInfiniteQuery({
    queryKey: keys.customerList(mode, p),
    queryFn: ({ pageParam }) => api.customerAddresses(mode, { ...p, cursor: pageParam }),
    initialPageParam: null as string | null,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });
}

export function useCustomerAddress(id: string) {
  const { mode } = useMode();
  return useQuery({ queryKey: keys.customer(mode, id), queryFn: () => api.customerAddress(mode, id), enabled: Boolean(id) });
}

export function useCustomerAddressSessions(id: string) {
  const { mode } = useMode();
  return useInfiniteQuery({
    queryKey: keys.customerSessions(mode, id),
    queryFn: ({ pageParam }) => api.customerAddressSessions(mode, id, pageParam),
    initialPageParam: null as string | null,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });
}

export function useApiKeys() {
  const { mode } = useMode();
  return useQuery({ queryKey: keys.apiKeys(mode), queryFn: () => api.apiKeys(mode) });
}

export function useWebhookEndpoints() {
  const { mode } = useMode();
  return useQuery({ queryKey: keys.webhooks(mode), queryFn: () => api.webhookEndpoints(mode) });
}

export function useDeliveries(endpointId: string) {
  const { mode } = useMode();
  return useInfiniteQuery({
    queryKey: keys.deliveries(mode, endpointId),
    queryFn: ({ pageParam }) => api.deliveries(mode, endpointId, pageParam),
    initialPageParam: null as string | null,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });
}

export function useSettings() {
  const { mode } = useMode();
  return useQuery({ queryKey: keys.settings(mode), queryFn: () => api.settings(mode) });
}

export function useWallets() {
  const { mode } = useMode();
  return useQuery({ queryKey: keys.wallets(mode), queryFn: () => api.wallets(mode) });
}

/** The merchant's hosted test inbox (test mode only; nothing is asked in live mode). */
export function useSandboxInbox() {
  const { mode } = useMode();
  return useQuery({ queryKey: keys.inbox(mode), queryFn: () => api.sandboxInbox(mode), enabled: mode === 'test' });
}

/** What the inbox received, newest first, refreshed while it is on screen. */
export function useInboxRequests(enabled: boolean) {
  const { mode } = useMode();
  return useInfiniteQuery({
    queryKey: keys.inboxRequests(mode),
    queryFn: ({ pageParam }) => api.sandboxRequests(mode, pageParam),
    initialPageParam: null as string | null,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    enabled: enabled && mode === 'test',
    refetchInterval: 5_000,
  });
}

const STARTED_KEY = 'prismaport.dashboard.started';
const startedListeners = new Set<() => void>();

function readStarted(): string | null {
  try {
    return sessionStorage.getItem(STARTED_KEY);
  } catch {
    return null;
  }
}

/**
 * Records that this user moved past the first Get started step (which mode to start in), for
 * this browser tab. It holds the user's id, so another account signing in on the tab starts over.
 */
export function markStarted(userId: string): void {
  try {
    sessionStorage.setItem(STARTED_KEY, userId);
  } catch {
    // Storage may be refused; the counter just stays where it was.
  }
  startedListeners.forEach((f) => f());
}

/**
 * Where the mode on screen stands in Get started, as the server counts it: the menu item shows
 * while the mode has no product, and the very first time asks where to start (no product in
 * either mode). The tab's own hint only skips that first step after Continue was pressed with no
 * wallet yet.
 */
export function useOnboarding() {
  const { mode } = useMode();
  const q = useQuery({ queryKey: keys.onboarding(mode), queryFn: () => api.onboarding(mode) });
  const userId = useMe().data?.user.id;
  const startedBy = useSyncExternalStore(
    (f) => {
      startedListeners.add(f);
      return () => void startedListeners.delete(f);
    },
    readStarted,
    () => null,
  );
  const started = userId !== undefined && startedBy === userId;
  const d = q.data;
  const firstRun = d?.first_run ?? false;
  const skipped = firstRun && started && d?.step === 'choose';
  const done = (d?.done ?? 0) + (skipped ? 1 : 0);
  const step = skipped ? 'wallets' : (d?.step ?? 'wallets');
  return {
    /** Steps done so far and of how many, for the menu counter. */
    progress: `${done}/${d?.steps.length ?? 0}`,
    hasWallet: d?.has_wallet ?? false,
    loading: q.isPending,
    visible: d?.visible ?? false,
    firstRun,
    /** The step Get started opens at. */
    step,
    steps: d?.steps ?? [],
  };
}
