import { QueryClientProvider, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { lazy, Suspense, useEffect, useState, type ReactNode } from 'react';
import { createBrowserRouter, createMemoryRouter, Navigate, Outlet, RouterProvider, useLocation, type RouteObject } from 'react-router-dom';
import { AppError, PageError } from './components/RouteError';
import { BootSplash, Shell } from './components/Shell';
import { ToastProvider } from './components/Toast';
import { ErrorState, Loading } from './components/ui';
import { setUnauthenticatedHandler } from './lib/api';
import { api } from './lib/endpoints';
import { ModeProvider, useMode } from './lib/mode';
import { keys, useMe, useOnboarding } from './lib/queries';
import { endedSession, noteSessionEnded, openAuthPages, pageName, sessionEndedNotice } from './lib/sessionEnd';
import type { Me, Mode } from './lib/types';
import { LoginPage } from './pages/Login';
import { CancelRemovalPage, LostAuthenticatorPage, LostConfirmPage } from './pages/LostAuthenticator';
import { NotFoundPage } from './pages/NotFound';
import { ForgotPasswordPage, ResetPasswordPage } from './pages/PasswordReset';
import { SignupPage } from './pages/Signup';
import { VerifyEmailPage } from './pages/VerifyEmail';

// Route-level code splitting: each heavy page is its own chunk, fetched on first visit. The auth
// pages and the shell stay in the entry chunk so sign-in is not blocked on a second request. A chunk
// that fails to load throws while rendering and lands in the route's errorElement.
const CustomerDetailPage = lazy(() => import('./pages/CustomerDetail').then((m) => ({ default: m.CustomerDetailPage })));
const CustomersPage = lazy(() => import('./pages/Customers').then((m) => ({ default: m.CustomersPage })));
const DevelopersPage = lazy(() => import('./pages/Developers').then((m) => ({ default: m.DevelopersPage })));
const WebhookDeliveriesPage = lazy(() => import('./pages/Developers').then((m) => ({ default: m.WebhookDeliveriesPage })));
const GetStartedPage = lazy(() => import('./pages/GetStarted').then((m) => ({ default: m.GetStartedPage })));
const LinkDetailPage = lazy(() => import('./pages/LinkDetail').then((m) => ({ default: m.LinkDetailPage })));
const LinksPage = lazy(() => import('./pages/Links').then((m) => ({ default: m.LinksPage })));
const OverviewPage = lazy(() => import('./pages/Overview').then((m) => ({ default: m.OverviewPage })));
const PaymentDetailPage = lazy(() => import('./pages/PaymentDetail').then((m) => ({ default: m.PaymentDetailPage })));
const PaymentsPage = lazy(() => import('./pages/Payments').then((m) => ({ default: m.PaymentsPage })));
const ProductDetailPage = lazy(() => import('./pages/ProductDetail').then((m) => ({ default: m.ProductDetailPage })));
const ProductsPage = lazy(() => import('./pages/Products').then((m) => ({ default: m.ProductsPage })));
const SettingsPage = lazy(() => import('./pages/Settings').then((m) => ({ default: m.SettingsPage })));

function Lazy({ children }: { children: ReactNode }) {
  return <Suspense fallback={<Loading what="this page" />}>{children}</Suspense>;
}

function RequireAuth({ children }: { children: ReactNode }) {
  const me = useMe();
  const location = useLocation();
  if (me.isPending) return <BootSplash />;
  if (me.isError) return <div className="auth-page"><div className="auth-card"><ErrorState error={me.error} onRetry={() => me.refetch()} /></div></div>;
  if (!me.data) {
    // After a session that ended by itself the sign-in page says why and offers the same email.
    const ended = endedSession();
    const notice = ended ? { email: ended.email, notice: sessionEndedNotice(pageName(location.pathname)), ended: true } : {};
    return <Navigate to="/login" replace state={{ from: location.pathname + location.search, ...notice }} />;
  }
  if (me.data.user.email_verified === false) return <Navigate to="/verify-email" replace />;
  return <>{children}</>;
}

/** The sign-in, sign-up, email and password pages: arriving forgets a session that ended (see sessionEnd). */
function AuthPages() {
  useEffect(() => openAuthPages(), []);
  return <Outlet />;
}

/**
 * Payment links is home when the merchant has at least one link. A mode with no product yet, and
 * an account whose live mode is closed, start at Get started; the rest at the overview.
 */
function Home() {
  const { mode } = useMode();
  const me = useMe().data;
  const q = useQuery({ queryKey: ['home', mode], queryFn: () => api.links(mode, { status: 'all' }), staleTime: Infinity, retry: false });
  const onboarding = useOnboarding();
  if (q.isPending || onboarding.loading) return <Loading what="your dashboard" />;
  if (q.data && q.data.data.length > 0) return <Navigate to="/links" replace />;
  const start = onboarding.visible || (mode === 'live' && me?.merchant.live_enabled === false);
  return <Navigate to={start ? '/get-started' : '/overview'} replace />;
}

// A page that throws while rendering (an amount that is not a decimal string, say) shows an
// error inside the shell, keeping the navigation; the auth pages and the shell have their own.
export const routes: RouteObject[] = [
  {
    errorElement: <AppError />,
    children: [
      {
        element: <AuthPages />,
        children: [
          { path: '/login', element: <LoginPage /> },
          { path: '/signup', element: <SignupPage /> },
          { path: '/verify-email', element: <VerifyEmailPage /> },
          { path: '/forgot-password', element: <ForgotPasswordPage /> },
          { path: '/reset-password', element: <ResetPasswordPage /> },
          { path: '/lost-authenticator', element: <LostAuthenticatorPage /> },
          { path: '/lost-authenticator/confirm', element: <LostConfirmPage /> },
          { path: '/cancel-2fa-removal', element: <CancelRemovalPage /> },
        ],
      },
      {
        path: '/',
        element: (
          <RequireAuth>
            <Shell />
          </RequireAuth>
        ),
        children: [
          {
            errorElement: <PageError />,
            children: [
              { index: true, element: <Home /> },
              { path: 'get-started', element: <Lazy><GetStartedPage /></Lazy> },
              { path: 'overview', element: <Lazy><OverviewPage /></Lazy> },
              { path: 'products', element: <Lazy><ProductsPage /></Lazy> },
              { path: 'products/:id', element: <Lazy><ProductDetailPage /></Lazy> },
              { path: 'links', element: <Lazy><LinksPage /></Lazy> },
              { path: 'links/:id', element: <Lazy><LinkDetailPage /></Lazy> },
              { path: 'payments', element: <Lazy><PaymentsPage /></Lazy> },
              { path: 'payments/:id', element: <Lazy><PaymentDetailPage /></Lazy> },
              { path: 'customers', element: <Lazy><CustomersPage /></Lazy> },
              { path: 'customers/:id', element: <Lazy><CustomerDetailPage /></Lazy> },
              { path: 'developers', element: <Navigate to="/developers/api-keys" replace /> },
              { path: 'developers/api-keys', element: <Lazy><DevelopersPage tab="keys" /></Lazy> },
              { path: 'developers/webhooks', element: <Lazy><DevelopersPage tab="webhooks" /></Lazy> },
              { path: 'developers/webhooks/:id', element: <Lazy><WebhookDeliveriesPage /></Lazy> },
              { path: 'settings', element: <Navigate to="/settings/networks" replace /> },
              { path: 'settings/networks', element: <Lazy><SettingsPage tab="networks" /></Lazy> },
              { path: 'settings/wallets', element: <Navigate to="/settings/networks" replace /> },
              { path: 'settings/stablecoins', element: <Navigate to="/settings/networks" replace /> },
              { path: 'settings/account', element: <Lazy><SettingsPage tab="account" /></Lazy> },
              { path: '*', element: <NotFoundPage /> },
            ],
          },
        ],
      },
    ],
  },
];

function UnauthenticatedBridge() {
  const qc = useQueryClient();
  useEffect(() => {
    // Drop every cached query, not only the user: keys are scoped by mode, so the next account to
    // sign in in this tab would otherwise be served this one's data. The user query itself stays
    // (set to null) because qc.clear() would detach RequireAuth's observer and it would never
    // redirect to the login page.
    setUnauthenticatedHandler(() => {
      // Read before the wipe: someone signed in means the session ended by itself (not a first
      // visit, not a sign-out), and the sign-in page can say so.
      const was = qc.getQueryData<Me | null>(keys.me);
      if (was) noteSessionEnded(was.user.email);
      qc.removeQueries({ predicate: (q) => q.queryKey[0] !== keys.me[0] });
      qc.setQueryData(keys.me, null);
    });
    return () => setUnauthenticatedHandler(null);
  }, [qc]);
  return null;
}

export function Providers({ client, children, mode }: { client: QueryClient; children: ReactNode; mode?: Mode }) {
  return (
    <QueryClientProvider client={client}>
      <ModeProvider initial={mode}>
        <ToastProvider>
          <UnauthenticatedBridge />
          {children}
        </ToastProvider>
      </ModeProvider>
    </QueryClientProvider>
  );
}

declare const __ARTIFACT__: boolean;

export function App({ client }: { client: QueryClient }) {
  const [router] = useState(() =>
    __ARTIFACT__
      ? createMemoryRouter(routes, { initialEntries: ['/login'] })
      : createBrowserRouter(routes, { basename: import.meta.env.BASE_URL.replace(/\/$/, '') }),
  );
  return (
    <Providers client={client}>
      <RouterProvider router={router} />
    </Providers>
  );
}

/** For tests: the same routes on an in-memory history. */
export function TestApp({ client, path, mode }: { client: QueryClient; path: string; mode?: Mode }) {
  const [router] = useState(() => createMemoryRouter(routes, { initialEntries: [path] }));
  return (
    <Providers client={client} mode={mode}>
      <RouterProvider router={router} />
    </Providers>
  );
}
