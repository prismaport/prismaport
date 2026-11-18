import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState } from 'react';
import { api } from '../lib/endpoints';
import { useMode } from '../lib/mode';
import { keys, useMe } from '../lib/queries';
import type { Me } from '../lib/types';
import { UnlockDialog } from './Confirmation';
import { Icon } from './Icon';
import { useToast } from './Toast';

/** "9:41" from seconds. */
const countdown = (s: number) => `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;

/** "14:32", the end of the window, for screen readers. */
const clockTime = (t: number) => new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

const LAST_MINUTE = 60;

// When Lock now last ended a window, on whichever lock it was pressed: that end is not news.
let lockedByHandAt = 0;

/**
 * The lock on Live changes (D61), in Live mode: Locked opens the Unlock dialog; Unlocked counts
 * down to the end of the session's confirmation window and offers Lock now. When the window
 * ends, the dashboard says so once.
 */
export function LockStatus({ compact = false }: { compact?: boolean }) {
  const { mode } = useMode();
  const me = useMe().data;
  const qc = useQueryClient();
  const toast = useToast();
  const [unlocking, setUnlocking] = useState(false);
  // Focus goes to Lock now once Unlock closes its dialog: the Locked button it came from is gone.
  const [justUnlocked, setJustUnlocked] = useState(false);
  const lockNow = useRef<HTMLButtonElement>(null);
  const [now, setNow] = useState(() => Date.now());
  const win = me?.confirmation_window ?? null;
  const end = win ? Date.parse(win.expires_at) : 0;
  const left = Math.max(0, Math.ceil((end - now) / 1000));
  const open = left > 0;
  // The end of the window last seen open: it tells a window that ran out from one locked by hand.
  const lastEnd = useRef(0);

  useEffect(() => {
    if (!end) return;
    setNow(Date.now());
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [end]);

  // The phone bar and the sidebar both show the lock; the sidebar's says when it runs out.
  useEffect(() => {
    if (compact) return;
    if (open) {
      lastEnd.current = end;
      return;
    }
    const ended = lastEnd.current;
    lastEnd.current = 0;
    if (!ended || ended > Date.now() + 1000 || Date.now() - lockedByHandAt < 2000) return;
    toast.show('Locked again. Live changes ask for a code.');
    void qc.invalidateQueries({ queryKey: keys.me });
  }, [compact, open, end, qc, toast]);

  useEffect(() => {
    if (!justUnlocked || !open) return;
    lockNow.current?.focus();
    setJustUnlocked(false);
  }, [justUnlocked, open]);

  const lock = useMutation({
    mutationFn: () => api.lock(),
    onSuccess: () => {
      lockedByHandAt = Date.now();
      qc.setQueryData<Me>(keys.me, (m) => (m ? { ...m, confirmation_window: null } : m));
      toast.show('Locked. Live changes ask for a code.');
    },
    onError: () => toast.show('We could not lock. Try again.'),
  });

  if (mode !== 'live' || !me || !me.confirmation_window_seconds || me.merchant.live_enabled === false) return null;

  const dialog = unlocking ? (
    <UnlockDialog
      seconds={me.confirmation_window_seconds}
      onClose={() => setUnlocking(false)}
      onUnlocked={() => {
        setUnlocking(false);
        setJustUnlocked(true);
      }}
    />
  ) : null;
  const ending = left <= LAST_MINUTE;
  // Announces the switch only: the text changes with Locked/Unlocked, never with the countdown.
  const announce = (
    <span role="status" aria-live="polite" className="sr-only" data-testid="lock-announce">
      {open ? 'Live changes unlocked' : 'Live changes locked'}
    </span>
  );
  if (!compact) {
    // The sidebar: a card under Mode, as wide as the sidebar.
    return (
      <div className="stack-sm">
        <p className="label">Live changes</p>
        {open ? (
          <div className="lock-card" data-state={ending ? 'ending' : 'unlocked'} data-testid="lock-status">
            <div className="lock-card-head">
              <Icon name="unlock" size={18} />
              <span>{ending ? 'Locks in' : 'Unlocked'}</span>
              <span className="lock-card-timer" aria-hidden="true">
                {countdown(left)}
              </span>
              <span className="sr-only">{ending ? `at ${clockTime(end)}` : `until ${clockTime(end)}`}</span>
            </div>
            <p className="small">On this device. It locks itself.</p>
            <button ref={lockNow} type="button" className="btn lock-card-action" onClick={() => lock.mutate()} disabled={lock.isPending}>
              Lock now
            </button>
          </div>
        ) : (
          <div className="lock-card" data-state="locked" data-testid="lock-status">
            <div className="lock-card-head">
              <Icon name="lock" size={18} />
              <span>Locked</span>
            </div>
            <p className="small muted">Wallets, keys and webhooks ask for a code.</p>
            <button type="button" className="btn lock-card-action" aria-label="Unlock Live changes" onClick={() => setUnlocking(true)}>
              Unlock
            </button>
          </div>
        )}
        {announce}
        {dialog}
      </div>
    );
  }
  if (!open) {
    return (
      <>
        <button
          type="button"
          className="lock-pill"
          data-state="locked"
          data-testid="lock-pill"
          title="Live changes ask for a code. Unlock once to make several in a row."
          aria-label="Locked. Unlock Live changes"
          onClick={() => setUnlocking(true)}
        >
          <Icon name="lock" />
        </button>
        {announce}
        {dialog}
      </>
    );
  }
  return (
    <>
    <div
      className="lock-pill"
      data-state={ending ? 'ending' : 'unlocked'}
      data-testid="lock-pill"
      title="Unlocked on this device. Adding wallets, creating keys and webhook endpoints ask for no code."
    >
      <Icon name="unlock" />
      <span className="mono lock-timer" aria-hidden="true">
        {countdown(left)}
      </span>
      <span className="sr-only">{ending ? `at ${clockTime(end)}` : `until ${clockTime(end)}`}</span>
      <button ref={lockNow} type="button" className="lock-now" onClick={() => lock.mutate()} disabled={lock.isPending}>
        Lock now
      </button>
    </div>
    {announce}
    </>
  );
}
