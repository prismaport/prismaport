import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState, type ReactNode } from 'react';
import { ApiError } from '../lib/api';
import { api } from '../lib/endpoints';
import { errorMessage } from '../lib/errors';
import { useMode } from '../lib/mode';
import { keys, useMe } from '../lib/queries';
import type { Confirmation, ConfirmationPurpose, Me } from '../lib/types';
import { CodeInput } from './CodeInput';
import { Dialog } from './Dialog';
import { Icon } from './Icon';
import { useToast } from './Toast';
import { isApiError } from './ui';

const CODE = /^\d{6}$/;

/** "owner@acme.studio" as "o•••@acme.studio", the way the server says where it sent a code. */
function maskEmail(email: string): string {
  const at = email.indexOf('@');
  return at < 1 ? email : `${email[0]}•••${email.slice(at)}`;
}

/** The methods a confirmation_required refusal lists in `error.details.methods`, or null when it lists none. */
function confirmationMethods(e: unknown): string[] | null {
  if (!(e instanceof ApiError) || e.code !== 'confirmation_required') return null;
  const m = e.details?.methods;
  return Array.isArray(m) ? m.filter((x): x is string => typeof x === 'string') : null;
}

/** The actions a confirmation window covers (D59); rolling a key and rotating a webhook secret are not among them. */
const IN_WINDOW: readonly ConfirmationPurpose[] = ['wallet_create', 'wallet_archive', 'api_key', 'webhook_endpoint'];

export interface ConfirmationState {
  /** The open confirmation window this action runs in without a code, or null when it takes one. */
  window: { until: Date; walletsLeft: number | null } | null;
  /** After the action succeeded: a code it carried opened a window, a window it used counted a wallet. */
  done: () => void;
  /** Whether an authenticator code can be used instead of an emailed one. */
  hasApp: boolean;
  /** Whether the server can email a code (false once it says it cannot). */
  canEmail: boolean;
  method: 'email' | 'app';
  setMethod: (m: 'email' | 'app') => void;
  code: string;
  setCode: (v: string) => void;
  /** The masked address a code went to, once one was sent. */
  sentTo: string | null;
  sending: boolean;
  /** True after the server refused another code because one was just sent. */
  limited: boolean;
  /** Seconds until another code can be asked for. */
  wait: number;
  send: () => Promise<void>;
  error: string | undefined;
  /** Checks the field before sending; false when it is not a code. */
  check: () => boolean;
  payload: () => Confirmation;
  /** Shows the missing-code error beside the form's other field errors; nothing when the form asks for no code. */
  showMissing: () => void;
  /** Shows a confirmation refusal in its field. True when it was one, so the caller shows nothing else. */
  absorb: (e: unknown) => boolean;
  /**
   * Covered Live actions on a server with the window on (D61): the form asks for no code; a locked
   * account unlocks first, in the Unlock dialog, and the action then carries on.
   */
  unlockFirst: boolean;
  /** The window's length, for the Unlock dialog. */
  seconds: number;
  /** The Unlock dialog is open for this action. */
  unlocking: boolean;
  /** Runs the action when it may: at once when unlocked, after Unlock when locked, after the typed code is checked otherwise (onMissing when it is not). */
  run: (action: () => void, onMissing?: () => void) => void;
  /** Unlock succeeded: the waiting action runs. */
  unlocked: () => void;
  /** Unlock was closed: the action waits for the next try. */
  cancelUnlock: () => void;
}

/**
 * The code that confirms a sensitive Live action: one emailed to the account owner, or, when 2FA
 * is on, the authenticator code; the person picks. The password is never asked.
 */
export function useConfirmation(purpose: ConfirmationPurpose, mode: 'live' | 'test' = 'live', inWindow = IN_WINDOW.includes(purpose)): ConfirmationState {
  const me = useMe().data;
  const qc = useQueryClient();
  // The window the server refused (it ended, or its wallets ran out): this form takes a code until
  // another window opens.
  const [refusedWindow, setRefusedWindow] = useState<string | null>(null);
  const seconds = me?.confirmation_window_seconds ?? 0;
  const unlockFirst = inWindow && mode === 'live' && seconds > 0;
  const [unlocking, setUnlocking] = useState(false);
  // The action waiting for Unlock, or the last one sent (a window that ended on the way asks again).
  const pending = useRef<(() => void) | null>(null);
  // A form that closes while Unlock is on the way must not send its change afterwards (a key
  // whose secret nobody would see).
  useEffect(() => () => void (pending.current = null), []);
  // The window switched off on the server while Unlock was open: nothing to unlock any more.
  useEffect(() => {
    if (!unlockFirst) setUnlocking(false);
  }, [unlockFirst]);
  const open = me?.confirmation_window;
  const windowEnd = open ? new Date(open.expires_at) : null;
  const win =
    inWindow && mode === 'live' && open && open.expires_at !== refusedWindow && windowEnd && windowEnd.getTime() > Date.now() && (purpose !== 'wallet_create' || open.wallets_left > 0)
      ? { until: windowEnd, walletsLeft: purpose === 'wallet_create' ? open.wallets_left : null }
      : null;
  // What the server last said this account can confirm with (error.details.methods); until then,
  // email always and the app when 2FA is on.
  const [methods, setMethods] = useState<string[] | null>(null);
  const hasApp = methods ? methods.includes('totp') : Boolean(me?.user.totp_enabled);
  const canEmail = methods ? methods.includes('email') : true;
  const [picked, setPicked] = useState<'email' | 'app'>('app');
  const method = hasApp && (picked === 'app' || !canEmail) ? 'app' : 'email';
  const [code, setCodeRaw] = useState('');
  const [sentTo, setSentTo] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const [until, setUntil] = useState(0);
  // The server said a code was sent a moment ago: the person may already hold a valid one.
  const [limited, setLimited] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const [error, setError] = useState<string | undefined>();
  const wait = Math.max(0, Math.ceil((until - now) / 1000));
  const counting = wait > 0;
  useEffect(() => {
    if (!counting) return;
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [counting, until]);
  const missing = method === 'email' ? 'Enter the 6-digit code we emailed you.' : 'Enter the 6-digit code from your authenticator app.';
  const check = () => {
    if (win) return true;
    const ok = CODE.test(code.trim());
    setError(ok ? undefined : method === 'email' && !sentTo && !limited ? 'Ask for a code first, then enter it here.' : missing);
    return ok;
  };
  return {
    unlockFirst,
    seconds,
    unlocking,
    showMissing: () => {
      if (!win && !unlockFirst) check();
    },
    run: (action, onMissing) => {
      pending.current = action;
      if (win) return action();
      if (unlockFirst) return setUnlocking(true);
      if (check()) action();
      else onMissing?.();
    },
    unlocked: () => {
      setUnlocking(false);
      pending.current?.();
    },
    cancelUnlock: () => {
      setUnlocking(false);
      pending.current = null;
    },
    window: win,
    done: () => void qc.invalidateQueries({ queryKey: keys.me }),
    hasApp,
    canEmail,
    limited,
    method,
    setMethod: (m) => {
      setPicked(m);
      setCodeRaw('');
      setError(undefined);
    },
    code,
    setCode: (v) => {
      setCodeRaw(v.replace(/\D/g, ''));
      setError(undefined);
    },
    sentTo,
    sending,
    wait,
    send: async () => {
      setSending(true);
      setError(undefined);
      try {
        const r = await api.sendConfirmation(mode, purpose);
        setSentTo(r.sent_to);
        setLimited(false);
        const t = Date.now();
        setNow(t);
        setUntil(t + r.resend_after * 1000);
      } catch (e) {
        if (e instanceof ApiError && e.code === 'rate_limited') {
          const t = Date.now();
          setNow(t);
          setUntil(t + (e.retryAfter ?? 60) * 1000);
          setLimited(true);
        }
        // A server with no email set up: only the authenticator can confirm.
        // email_send_failed (a 503 too) is momentary: the message shows and the button stays for another try.
        if (e instanceof ApiError && e.code === 'service_unavailable') setMethods(hasApp ? ['totp'] : []);
        setError(errorMessage(e));
      } finally {
        setSending(false);
      }
    },
    error,
    check,
    payload: () => (unlockFirst || win ? {} : method === 'email' ? { email_code: code.trim() } : { totp: code.trim() }),
    absorb: (e) => {
      const listed = confirmationMethods(e);
      if (listed) setMethods(listed);
      // Locked on the way (the window ended, or its wallets ran out): Unlock, then send again.
      if (unlockFirst && isApiError(e, 'confirmation_required')) {
        void qc.invalidateQueries({ queryKey: keys.me });
        setUnlocking(true);
        return true;
      }
      if (win && isApiError(e, 'confirmation_required')) {
        setRefusedWindow(open?.expires_at ?? null);
        void qc.invalidateQueries({ queryKey: keys.me });
      }
      if (isApiError(e, 'invalid_code') || isApiError(e, 'confirmation_required')) {
        setError(e.message || missing);
        return true;
      }
      return false;
    },
  };
}

export function ConfirmFields({ s, idPrefix, what = 'Adding a wallet', inLive = true }: { s: ConfirmationState; idPrefix: string; what?: string; inLive?: boolean }) {
  const email = useMe().data?.user.email ?? '';
  const masked = s.sentTo ?? maskEmail(email);
  const emailing = s.method === 'email';
  const where = inLive ? ' in Live mode' : '';
  // The form says nothing about the lock: saving opens Unlock when it is locked.
  if (s.unlockFirst) {
    return s.unlocking ? <UnlockDialog seconds={s.seconds} onClose={s.cancelUnlock} onUnlocked={s.unlocked} /> : null;
  }
  if (s.window) {
    return (
      <fieldset className="plain stack-sm step-up" data-testid="step-up">
        <legend>Confirm it&apos;s you</legend>
        <p className="hint" data-testid="confirmation-window">
          Unlocked. No code needed.{s.window.walletsLeft !== null ? ' We email you every new wallet.' : ''}
        </p>
      </fieldset>
    );
  }
  if (!s.hasApp && !s.canEmail) {
    return (
      <fieldset className="plain stack-md step-up" data-testid="step-up">
        <legend>Confirm it&apos;s you</legend>
        <p className="error" role="alert">
          We can&apos;t confirm this right now: codes by email are not available and two-factor authentication is off. Try again later, or turn on two-factor authentication in Settings.
        </p>
      </fieldset>
    );
  }
  return (
    <fieldset className="plain stack-md step-up" data-testid="step-up">
      <legend>Confirm it&apos;s you</legend>
      <p className="hint">
        {s.hasApp && s.canEmail
          ? `${what}${where} is confirmed with a code. Use your authenticator app or a code we email you.`
          : s.hasApp
            ? `${what}${where} is confirmed with the code from your authenticator app.`
            : `${what}${where} is confirmed with a 6-digit code we email to ${masked}.`}
      </p>
      {s.hasApp && s.canEmail ? (
        <div className="kind-switch" role="radiogroup" aria-label="Confirm with">
          <label className="kind-option">
            <input type="radio" name={`${idPrefix}-method`} value="app" checked={!emailing} onChange={() => s.setMethod('app')} />
            <span className="kind-title">Authenticator app</span>
            <span className="small muted">The 6-digit code in your app.</span>
          </label>
          <label className="kind-option">
            <input type="radio" name={`${idPrefix}-method`} value="email" checked={emailing} onChange={() => s.setMethod('email')} />
            <span className="kind-title">Code by email</span>
            <span className="small muted">We send a code to {maskEmail(email)}.</span>
          </label>
        </div>
      ) : null}
      {emailing && !s.sentTo && !s.limited ? (
        <div className="stack-sm">
          <div>
            <button type="button" className="btn" onClick={() => void s.send()} disabled={s.sending || s.wait > 0}>
              {s.sending ? <span className="spinner" aria-hidden="true" /> : null}
              Email me a code
            </button>
          </div>
          {s.error ? (
            <p className="error" role="alert">
              {s.error}
            </p>
          ) : null}
        </div>
      ) : (
        <div className="stack-sm">
          <CodeInput
            id={`${idPrefix}-code`}
            label={emailing ? 'Code from your email' : 'Code from your authenticator app'}
            hint={emailing ? (s.sentTo ? `Sent to ${masked}. It works for 15 minutes.` : `A code was already sent to ${masked}. Enter it here, or wait to ask for a new one.`) : undefined}
            value={s.code}
            onChange={s.setCode}
            status={s.error ? 'invalid' : 'idle'}
            message={s.error}
            clearOnInvalid={false}
          />
          {emailing ? (
            <div>
              <button type="button" className="btn-link small" onClick={() => void s.send()} disabled={s.sending || s.wait > 0}>
                {s.wait > 0 ? `Send a new code in ${s.wait} s` : 'Send a new code'}
              </button>
            </div>
          ) : null}
        </div>
      )}
    </fieldset>
  );
}

/** Unlock (D61): one code, emailed or from the authenticator app, opens the confirmation window. */
export function UnlockDialog({ seconds, onClose, onUnlocked }: { seconds: number; onClose: () => void; onUnlocked: () => void }) {
  const qc = useQueryClient();
  const toast = useToast();
  const s = useConfirmation('unlock', 'live', false);
  const minutes = Math.max(1, Math.round(seconds / 60));
  const [refused, setRefused] = useState<string | null>(null);
  const unlock = useMutation({
    mutationFn: () => api.unlock(s.payload()),
    onSuccess: (r) => {
      qc.setQueryData<Me>(keys.me, (m) => (m ? { ...m, confirmation_window: r.confirmation_window } : m));
      void qc.invalidateQueries({ queryKey: keys.me });
      toast.show(`Unlocked for ${minutes} minutes`);
      onUnlocked();
    },
    onError: (e) => {
      if (!s.absorb(e)) setRefused(isApiError(e) ? e.message : 'We could not unlock. Try again.');
    },
  });
  return (
    <Dialog title={`Unlock Live changes for ${minutes} minutes`} onClose={onClose} busy={unlock.isPending} size="narrow">
      <form
        className="stack-md"
        noValidate
        data-testid="unlock"
        onSubmit={(e) => {
          e.preventDefault();
          // The dialog may open from inside another form: React would hand it this submit too.
          e.stopPropagation();
          setRefused(null);
          if (s.check()) unlock.mutate();
        }}
      >
        <p>Enter one code. On this device, these changes then take no other code:</p>
        <ul className="check-list">
          <li>
            <Icon name="check" />
            Add and archive wallets
          </li>
          <li>
            <Icon name="check" />
            Create API keys
          </li>
          <li>
            <Icon name="check" />
            Add webhook endpoints and change their address
          </li>
        </ul>
        <div className="note-box">
          <p className="label">Always ask for a code</p>
          <p className="small muted">Rolling an API key, rotating a webhook secret, and two-factor settings.</p>
        </div>
        <ConfirmFields s={s} idPrefix="unlock" what="Unlocking Live changes" inLive={false} />
        <p className="small muted">
          It locks itself after {minutes} minutes, when you sign out, or when you press Lock now. We email you every new wallet.
        </p>
        {refused ? (
          <p className="error" role="alert">
            {refused}
          </p>
        ) : null}
        <div className="dialog-foot">
          <button type="button" className="btn" onClick={onClose} disabled={unlock.isPending}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={unlock.isPending}>
            Unlock for {minutes} minutes
          </button>
        </div>
      </form>
    </Dialog>
  );
}

export interface LiveConfirmation {
  /** After the action succeeded; see ConfirmationState.done. */
  done: () => void;
  /** Runs the action when it may; see ConfirmationState.run. Test mode runs it at once. */
  run: (action: () => void) => void;
  /** The confirmation fields of the request body, in Live mode only. */
  payload: () => Partial<Confirmation>;
  /** Checks the field before sending; always true in Test mode. */
  check: () => boolean;
  /** Shows the missing-code error beside the form's other field errors (a server with the window off). */
  showMissing: () => void;
  /** A confirmation refusal, shown in its field, so the caller shows nothing else. */
  absorbs: (e: unknown) => boolean;
  onError: (e: unknown) => void;
  fields: (idPrefix: string, what?: string) => ReactNode;
}

/** The confirmation of a Live change (a live API key, a live webhook endpoint, a wallet); Test mode asks for nothing. */
export function useLiveConfirmation(purpose: ConfirmationPurpose, inWindow?: boolean): LiveConfirmation {
  const live = useMode().mode === 'live';
  const s = useConfirmation(purpose, 'live', inWindow);
  return {
    done: () => {
      if (live) s.done();
    },
    run: (action) => (live ? s.run(action) : action()),
    payload: () => (live ? s.payload() : {}),
    check: () => !live || s.check(),
    showMissing: () => {
      if (live) s.showMissing();
    },
    absorbs: (e) => live && (isApiError(e, 'invalid_code') || isApiError(e, 'confirmation_required')),
    onError: (e) => {
      if (live) s.absorb(e);
    },
    fields: (idPrefix, what) => (live ? <ConfirmFields s={s} idPrefix={idPrefix} what={what} /> : null),
  };
}
