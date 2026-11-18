import { useQueryClient } from '@tanstack/react-query';
import { useState, type ReactNode } from 'react';
import { useNavigate } from 'react-router-dom';
import { ApiError } from '../lib/api';
import { useMode } from '../lib/mode';
import { keys, useMe } from '../lib/queries';
import { formatUtc, formatUtcDay, lockedUntil, RECOVERY_LOCK_HOURS } from '../lib/recovery';
import { Dialog } from './Dialog';

export interface RecoveryLock {
  /** True (and the dialog opens) when the lock is on; the caller then sends nothing. */
  blocked: () => boolean;
  /** True when the server refused with recovery_locked: the dialog opens and the caller shows no error. */
  absorbs: (e: unknown) => boolean;
  /** Call from onError. */
  onError: (e: unknown) => void;
  dialog: ReactNode;
}

/**
 * After 2FA was removed by a lost-app request, adding a live wallet and creating or rolling a
 * live API key are locked for a while. The page knows the time from /me and says so before it
 * sends anything; if the server still refuses (a lock that started after /me was read), the same
 * dialog opens.
 */
export function useRecoveryLock(): RecoveryLock {
  const live = useMode().mode === 'live';
  const me = useMe().data;
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const until = live ? lockedUntil(me?.user) : null;
  const absorbs = (e: unknown) => e instanceof ApiError && e.code === 'recovery_locked';
  return {
    blocked: () => {
      if (!until) return false;
      setOpen(true);
      return true;
    },
    absorbs,
    onError: (e) => {
      if (!absorbs(e)) return;
      void qc.invalidateQueries({ queryKey: keys.me });
      setOpen(true);
    },
    dialog: open ? <RecoveryLockedDialog until={until ?? me?.user.recovery_locked_until ?? null} onClose={() => setOpen(false)} /> : null,
  };
}

function RecoveryLockedDialog({ until, onClose }: { until: string | null; onClose: () => void }) {
  const navigate = useNavigate();
  // The lock lasts a fixed time from the removal, so the day it happened is the lock's start.
  const removed = until ? formatUtcDay(new Date(Date.parse(until) - RECOVERY_LOCK_HOURS * 3_600_000).toISOString()) : null;
  return (
    <Dialog
      title="Adding a wallet is locked"
      description={until ? `Until ${formatUtc(until)}.` : undefined}
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose}>
            Close
          </button>
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => {
              onClose();
              navigate('/settings/account');
            }}
          >
            Turn on two-factor
          </button>
        </>
      }
    >
      <div className="stack-md">
        <p>
          Two-factor authentication was removed by a recovery request{removed ? ` on ${removed}` : ''}. For {RECOVERY_LOCK_HOURS} hours you cannot add a wallet or create a live API key. This protects where your payments go.
        </p>
        <p className="small muted">Your current wallets keep receiving payments. Turn on two-factor authentication now, so you are protected when the lock ends.</p>
      </div>
    </Dialog>
  );
}
