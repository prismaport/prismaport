import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState, type RefObject } from 'react';
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom';
import { ApiError, setCsrfToken } from '../lib/api';
import { api } from '../lib/endpoints';
import { useMode } from '../lib/mode';
import { pauseText, usePauses } from '../lib/pauses';
import { keys, useMe, useOnboarding } from '../lib/queries';
import type { Mode } from '../lib/types';
import { versionLabel } from '../lib/version';
import { useModal } from './Dialog';
import { LeaveGuard } from './DirtyGuard';
import { Icon, type IconName } from './Icon';
import { RemovalStrip } from './Removal';
import { LockStatus } from './LockStatus';
import { useToast } from './Toast';
import { Segmented } from './ui';

const BRAND = `${import.meta.env.BASE_URL}brand/`;

const NAV: { to: string; label: string; icon: IconName }[] = [
  { to: '/overview', label: 'Overview', icon: 'overview' },
  { to: '/products', label: 'Products', icon: 'box' },
  { to: '/links', label: 'Payment links', icon: 'link' },
  { to: '/payments', label: 'Payments', icon: 'receipt' },
  { to: '/customers', label: 'Customers', icon: 'users' },
  { to: '/developers', label: 'Developers', icon: 'code' },
  { to: '/settings', label: 'Settings', icon: 'settings' },
];

export function Mark({ size = 26 }: { size?: number }) {
  return (
    <picture>
      <source srcSet={`${BRAND}prismaport-mark-white.svg`} media="(prefers-color-scheme: dark)" />
      <img src={`${BRAND}prismaport-mark-ink.svg`} alt="" width={size} height={size} />
    </picture>
  );
}

/** The full-page wait while the session is checked; it fades in late so a quick answer shows nothing. */
export function BootSplash() {
  return (
    <div className="boot-splash" role="status">
      <Mark size={40} />
      <span className="sr-only">Loading your dashboard.</span>
    </div>
  );
}

export function Lockup() {
  return (
    <span className="lockup">
      <Mark />
      <span>Prismaport</span>
    </span>
  );
}

/** Live mode handles real money: said under the lockup for as long as it is on (Test mode has its strip). */
function LiveBadge({ compact = false }: { compact?: boolean }) {
  return (
    <span className="live-badge" data-testid="live-badge">
      {compact ? 'Live' : 'Live · real payments'}
    </span>
  );
}

function ModeSwitch({ compact = false }: { compact?: boolean }) {
  const { mode, setMode } = useMode();
  return (
    <Segmented<Mode>
      legend="Mode"
      className={`mode ${compact ? '' : 'segmented-block'}`}
      value={mode}
      onChange={setMode}
      options={[
        { value: 'live', label: 'Live' },
        { value: 'test', label: 'Test' },
      ]}
    />
  );
}

function Nav({ onNavigate }: { onNavigate?: () => void }) {
  // Get started stays in the menu while the mode on screen has no product.
  const onboarding = useOnboarding();
  return (
    <nav aria-label="Main">
      <ul className="nav-list">
        {onboarding.visible ? (
          <li>
            <NavLink to="/get-started" className="nav-link" onClick={onNavigate}>
              <Icon name="arrow" size={20} />
              Get started
              <span className="nav-count" data-testid="get-started-count">
                {onboarding.progress}
              </span>
            </NavLink>
          </li>
        ) : null}
        {NAV.map((n) => (
          <li key={n.to}>
            <NavLink to={n.to} className="nav-link" onClick={onNavigate}>
              <Icon name={n.icon} size={20} />
              {n.label}
            </NavLink>
          </li>
        ))}
      </ul>
    </nav>
  );
}

function Account() {
  const me = useMe().data;
  const qc = useQueryClient();
  const navigate = useNavigate();
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const signOut = async () => {
    setBusy(true);
    try {
      await api.logout();
    } catch (err) {
      // Only a 401 means the session is already gone. Any other failure leaves the httpOnly
      // cookie valid, so a reload would sign the user back in: stay put and say so.
      if (!(err instanceof ApiError && err.status === 401)) {
        setBusy(false);
        toast.show('We could not sign you out. Try again.');
        return;
      }
    }
    setCsrfToken(null);
    qc.clear();
    qc.setQueryData(keys.me, null);
    navigate('/login', { replace: true });
  };
  return (
    <div className="account">
      <div>
        <p className="subheading">{me?.merchant.name}</p>
        <p className="small muted email" title={me?.user.email}>
          {me?.user.email}
        </p>
      </div>
      <button type="button" className="btn btn-small" onClick={signOut} disabled={busy}>
        <Icon name="logout" />
        Sign out
      </button>
      <p className="small muted version" title="Running build">
        {versionLabel}
      </p>
    </div>
  );
}

/** The phone navigation: a modal sheet that traps focus and hands it back to the menu button. */
function NavSheet({ onClose, menuButton }: { onClose: () => void; menuButton: RefObject<HTMLButtonElement | null> }) {
  const sheet = useRef<HTMLDivElement>(null);
  useModal(sheet, { onClose, restoreTo: menuButton });
  return (
    <div className="nav-sheet" id="nav-sheet">
      <button type="button" className="nav-sheet-backdrop" aria-label="Close navigation" tabIndex={-1} onClick={onClose} />
      <div className="nav-sheet-panel" ref={sheet} role="dialog" aria-modal="true" aria-label="Navigation">
        <div className="row between">
          <Lockup />
          <button type="button" className="btn btn-ghost btn-icon" aria-label="Close navigation" onClick={onClose}>
            <Icon name="x" size={20} />
          </button>
        </div>
        <Nav onNavigate={onClose} />
        <Account />
      </div>
    </div>
  );
}

/** D64: a chain or asset paused for new payments, said on every page while it lasts. Fixed text. */
function PauseStrip() {
  const pauses = usePauses();
  if (pauses.length === 0) return null;
  return (
    <div className="review-strip pause-strip" role="status" data-testid="pause-strip">
      <Icon name="info" size={16} />
      <span>{pauseText(pauses)}</span>
    </div>
  );
}

export function Shell() {
  const { mode, setMode } = useMode();
  const me = useMe().data;
  const underReview = me?.merchant.live_enabled === false;
  const [open, setOpen] = useState(false);
  const location = useLocation();
  const menuButton = useRef<HTMLButtonElement>(null);

  useEffect(() => setOpen(false), [location.pathname]);

  return (
    <div className="shell" data-mode={mode}>
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      {mode === 'test' ? (
        <div className="test-strip" role="note" data-testid="test-strip">
          <strong>Test mode</strong>
          <span>You are viewing test data. Payments use test networks and have no value.</span>
        </div>
      ) : null}
      {mode === 'live' && underReview ? (
        <div className="review-strip" role="note" data-testid="review-strip">
          <span>Live payments open after a review. Until then, use Test mode.</span>
          <button type="button" className="btn btn-small" onClick={() => setMode('test')}>
            Switch to Test mode
          </button>
        </div>
      ) : null}
      <RemovalStrip />
      <PauseStrip />
      <header className="topbar">
        <button
          ref={menuButton}
          type="button"
          className="btn btn-ghost btn-icon"
          aria-label="Open navigation"
          aria-expanded={open}
          aria-controls="nav-sheet"
          onClick={() => setOpen(true)}
        >
          <Icon name="menu" size={20} />
        </button>
        <NavLink to="/" className="lockup grow" aria-label="Prismaport dashboard home">
          <Mark />
          <span>Prismaport</span>
        </NavLink>
        {mode === 'live' ? <LiveBadge compact /> : null}
        <LockStatus compact />
        <ModeSwitch compact />
      </header>
      {open ? <NavSheet onClose={() => setOpen(false)} menuButton={menuButton} /> : null}
      <LeaveGuard />
      <div className="shell-body">
        <aside className="sidebar">
          <div className="sidebar-inner">
          <div className="sidebar-brand">
            <NavLink to="/" className="lockup" aria-label="Prismaport dashboard home">
              <Mark />
              <span>Prismaport</span>
            </NavLink>
            {mode === 'live' ? <LiveBadge /> : null}
          </div>
          <div className="stack-sm">
            <p className="label">Mode</p>
            <ModeSwitch />
          </div>
          <LockStatus />
          <Nav />
          <Account />
          </div>
        </aside>
        <main className="main" id="main" tabIndex={-1}>
          <div className="main-inner">
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  );
}
