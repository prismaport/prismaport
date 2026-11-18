import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState, type FormEvent } from 'react';
import { Link, Navigate, useLocation, useNavigate } from 'react-router-dom';
import { AuthLayout, emailProblem, FormError, ResendRow, useCodeEntry, useCountdown } from '../components/AuthLayout';
import { CodeInput } from '../components/CodeInput';
import { NewPasswordField, useNewPassword } from '../components/NewPassword';
import { Field, Loading } from '../components/ui';
import { ApiError, setCsrfToken } from '../lib/api';
import { CONFIRMED_PAUSE_MS } from '../lib/codes';
import { api } from '../lib/endpoints';
import { errorMessage } from '../lib/errors';
import { useMode } from '../lib/mode';
import { clearPendingSignup, pendingSignup, RESEND_SECONDS } from '../lib/pending';
import { fetchMe, keys, useMe } from '../lib/queries';
import type { Me } from '../lib/types';

declare const __MOCK__: boolean;

const SIGN_IN_NOTICE = 'Your email is confirmed. Sign in to continue.';

export function VerifyEmailPage() {
  const me = useMe();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const { setMode } = useMode();
  const state = (useLocation().state ?? {}) as { email?: string; sent?: boolean };
  const signedIn = me.data ?? null;
  const known = signedIn?.user.email ?? state.email ?? '';
  const [email, setEmail] = useState(known);
  const newPassword = useNewPassword();
  const entry = useCodeEntry(Boolean(state.sent));
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [left, startTimer] = useCountdown(state.sent ? RESEND_SECONDS : 0);
  const codeRef = useRef<HTMLInputElement>(null);
  const emailRef = useRef<HTMLInputElement>(null);
  const inFlight = useRef(false);
  const leave = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => {
    if (known) setEmail(known);
  }, [known]);
  const focused = useRef(false);
  useEffect(() => {
    if (me.isPending || focused.current) return;
    focused.current = true;
    (known ? codeRef : emailRef).current?.focus();
  }, [me.isPending, known]);
  useEffect(() => () => clearTimeout(leave.current), []);

  if (me.isPending) return <AuthLayout title="Verify your email"><Loading what="your account" /></AuthLayout>;
  if (signedIn && signedIn.user.email_verified !== false && entry.status !== 'confirmed') return <Navigate to="/" replace />;

  const address = email.trim().toLowerCase();
  const done = entry.status === 'confirmed' || entry.status === 'locked';
  // The code is tied to the password (ADR 0008 C-1): a browser signed in as this user proved it,
  // the sign-up that just sent the code holds it, and any other browser asks for it. The server
  // sets it with the code, so the password is always the one of whoever holds the email.
  // Once confirmed the sign-up's password is gone from memory, but nothing is asked any more:
  // the screen only waits to leave.
  const creds = signedIn ? null : pendingSignup(address);
  const askPassword = !signedIn && !creds && entry.status !== 'confirmed';

  const submit = async (code: string) => {
    if (inFlight.current || done) return;
    setError(null);
    if (!known && emailProblem(email)) {
      setError(emailProblem(email));
      emailRef.current?.focus();
      return;
    }
    if (askPassword && !newPassword.password) {
      setError('Enter the password of this account.');
      newPassword.inputRef.current?.focus();
      return;
    }
    if (askPassword && !newPassword.validate()) {
      newPassword.inputRef.current?.focus();
      return;
    }
    if (entry.incomplete(code)) {
      codeRef.current?.focus();
      return;
    }
    inFlight.current = true;
    setBusy(true);
    const secret = creds?.password ?? newPassword.password;
    try {
      await api.verifyEmail({ email: address, code, ...(signedIn ? {} : { password: secret }) });
      let user: Me | null = null;
      try {
        if (!signedIn) await api.login({ email: address, password: secret });
        user = await fetchMe();
      } catch {
        user = null;
      }
      clearPendingSignup();
      entry.confirmed(user ? 'Email confirmed. Opening your dashboard.' : 'Email confirmed. Opening sign-in.');
      leave.current = setTimeout(() => {
        if (user) {
          qc.setQueryData(keys.me, user);
          // A new account starts in Live mode, on Get started; Test mode is one switch away.
          setMode('live');
          navigate('/', { replace: true });
        } else {
          navigate('/login', { replace: true, state: { email: address, notice: SIGN_IN_NOTICE } });
        }
      }, CONFIRMED_PAUSE_MS);
    } catch (err) {
      if (newPassword.refused(err)) {
        // The sign-up's password was refused after all (the breach check was down then): the
        // field appears so another one can be set with the code, which is still good.
        clearPendingSignup();
      } else if (!entry.fail(err)) {
        setError(errorMessage(err));
        codeRef.current?.focus();
      }
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  };

  const resend = async () => {
    setError(null);
    if (emailProblem(email)) {
      setError(emailProblem(email));
      emailRef.current?.focus();
      return;
    }
    try {
      await api.resendCode(address);
      entry.sent();
      startTimer(RESEND_SECONDS);
      codeRef.current?.focus();
    } catch (err) {
      if (err instanceof ApiError && err.code === 'rate_limited') startTimer(err.retryAfter ?? RESEND_SECONDS);
      setError(errorMessage(err));
    }
  };

  const signOut = async () => {
    try {
      await api.logout();
    } catch {
      // Gone either way.
    }
    setCsrfToken(null);
    qc.clear();
    qc.setQueryData(keys.me, null);
    navigate('/login', { replace: true });
  };

  return (
    <AuthLayout
      title="Confirm your email"
      foot={
        signedIn ? (
          <p>
            Signed in as {signedIn.user.email}.{' '}
            <button type="button" className="btn-link" onClick={signOut}>
              Sign out
            </button>
          </p>
        ) : (
          <p>
            Wrong email? <Link to="/signup">Create the account again</Link> or <Link to="/login">sign in</Link>.
          </p>
        )
      }
    >
      <form
        className="card stack"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          void submit(entry.code);
        }}
        noValidate
        aria-labelledby="verify-title"
      >
        <div className="stack-sm">
          <h1 className="display" id="verify-title">
            Confirm your email
          </h1>
          <p className="muted">
            {known ? (
              <>
                We sent a 6-digit code to <strong>{known}</strong>. It works for 15 minutes.
              </>
            ) : (
              'Enter your email and the 6-digit code we sent to it. It works for 15 minutes.'
            )}
          </p>
        </div>
        <div className="form-grid">
          {known ? null : (
            <Field label="Email">
              {(p) => <input {...p} ref={emailRef} className="input" type="email" autoComplete="email" value={email} onChange={(e) => setEmail(e.target.value)} />}
            </Field>
          )}
          {askPassword ? (
            <NewPasswordField {...newPassword.field} note="The password of this account. Confirming the email with the code also sets it." />
          ) : null}
          <CodeInput ref={codeRef} id="verify-code" label="Confirmation code" {...entry.field} onComplete={(v) => void submit(v)} />
        </div>
        {__MOCK__ ? <p className="small muted">Preview build: the code is 246810.</p> : null}
        <FormError message={error} />
        <div className="stack-md">
          <button type="submit" className="btn btn-primary btn-block" disabled={busy || done}>
            {busy ? <span className="spinner" aria-hidden="true" /> : null}
            Confirm email
          </button>
          <ResendRow left={left} onResend={resend} timerId="resend-timer" />
        </div>
      </form>
    </AuthLayout>
  );
}
