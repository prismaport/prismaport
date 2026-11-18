import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { ApiError } from '../lib/api';
import { CODE_LENGTH, CODE_MESSAGES, codeFailure, incompleteCode, type CodeTries } from '../lib/codes';
import type { CodeStatus, CodeTone } from './CodeInput';
import { Icon, type IconName } from './Icon';
import { Lockup } from './Shell';

/** The page around the sign-in, sign-up and password forms: the lockup, the card, a footnote. */
export function AuthLayout({ title, children, foot }: { title: string; children: ReactNode; foot?: ReactNode }) {
  useEffect(() => {
    document.title = `${title} · Prismaport`;
  }, [title]);
  return (
    <main className="auth-page">
      <div className="auth-card">
        <Lockup />
        {children}
        {foot ? <div className="small muted auth-foot stack-sm">{foot}</div> : null}
      </div>
    </main>
  );
}

export function FormError({ message }: { message: string | null }) {
  if (!message) return null;
  return (
    <p className="error" role="alert">
      <Icon name="alert" />
      <span>{message}</span>
    </p>
  );
}

export function FormNotice({ message, icon = 'info' }: { message: string | null | undefined; icon?: IconName }) {
  if (!message) return null;
  return (
    <p className="callout notice" role="status">
      <Icon name={icon} />
      <span className="notice-text">{message}</span>
    </p>
  );
}

/** A countdown in whole seconds, for "send a new code" buttons. */
export function useCountdown(initial = 0): [number, (seconds: number) => void] {
  const [left, setLeft] = useState(initial);
  const timer = useRef<ReturnType<typeof setInterval>>(undefined);
  const start = useCallback((seconds: number) => {
    clearInterval(timer.current);
    setLeft(seconds);
    timer.current = setInterval(() => {
      setLeft((s) => {
        if (s <= 1) {
          clearInterval(timer.current);
          return 0;
        }
        return s - 1;
      });
    }, 1000);
  }, []);
  useEffect(() => {
    if (initial > 0) start(initial);
    return () => clearInterval(timer.current);
  }, [initial, start]);
  return [left, start];
}

export const clock = (s: number) => `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;

export function emailProblem(email: string): string | null {
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email.trim()) ? null : 'Enter an email address such as you@example.com.';
}

/**
 * The emailed-code field of a page: its value, state and message, and the tries this page has
 * seen since it asked for the code (`sentNow`: the page opened right after a send).
 */
export function useCodeEntry(sentNow: boolean) {
  const [code, setValue] = useState('');
  const [status, setStatus] = useState<CodeStatus>('idle');
  const [message, setMessage] = useState<{ text: string; tone?: CodeTone } | null>(null);
  const tries = useRef<CodeTries>({ failures: 0, sentAt: sentNow ? Date.now() : null });

  const setCode = (v: string) => {
    setValue(v);
    if (!v) return;
    // Typing again puts a wrong or expired code behind; a "sent" notice stays.
    if (status === 'invalid' || status === 'expired') {
      setStatus('idle');
      setMessage(null);
    } else if (message && message.tone !== 'ok') {
      setMessage(null);
    }
  };

  return {
    code,
    setCode,
    status,
    message,
    /** Shows why a code cannot be sent yet; false when it is complete. */
    incomplete(value: string): boolean {
      if (value.length === CODE_LENGTH) return false;
      setMessage({ text: incompleteCode(value.length), tone: 'error' });
      return true;
    },
    /** Takes a failed check; false when the error is not about the code. */
    fail(err: unknown): boolean {
      const wrong = err instanceof ApiError && err.code === 'invalid_code';
      const next = { ...tries.current, failures: tries.current.failures + (wrong ? 1 : 0) };
      const verdict = codeFailure(err, next, Date.now());
      if (!verdict) return false;
      tries.current = next;
      setStatus(verdict.status);
      setMessage({ text: verdict.message });
      return true;
    },
    confirmed(text: string) {
      setStatus('confirmed');
      setMessage({ text });
    },
    sent() {
      tries.current = { failures: 0, sentAt: Date.now() };
      setValue('');
      setStatus('idle');
      setMessage({ text: CODE_MESSAGES.sent, tone: 'ok' });
    },
    /** Props for CodeInput. */
    field: { value: code, onChange: setCode, status, message: message?.text, tone: message?.tone },
  };
}

/** "Didn't get it?" with the resend button and its countdown. */
export function ResendRow({ left, onResend, timerId, label = 'Resend code' }: { left: number; onResend: () => void; timerId: string; label?: string }) {
  return (
    <div className="resend-row small muted">
      <span>Didn't get it? Check spam.</span>
      <span>
        <button type="button" className="btn-link" onClick={onResend} disabled={left > 0} aria-describedby={left > 0 ? timerId : undefined}>
          {label}
        </button>
        {left > 0 ? (
          <>
            {' '}
            <span className="mono" id={timerId}>
              in {clock(left)}
            </span>
          </>
        ) : null}
      </span>
    </div>
  );
}
