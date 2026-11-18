import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState, type FormEvent } from 'react';
import { Link, Navigate, useLocation, useNavigate } from 'react-router-dom';
import { FormNotice } from '../components/AuthLayout';
import { CodeInput } from '../components/CodeInput';
import { Icon } from '../components/Icon';
import { Lockup } from '../components/Shell';
import { Field } from '../components/ui';
import { ApiError } from '../lib/api';
import { api } from '../lib/endpoints';
import { errorMessage } from '../lib/errors';
import { fetchMe, keys, useMe } from '../lib/queries';
import { noteRecoverySignIn, recoveryCodeValue, typedRecoveryCode } from '../lib/recovery';
import { sameEmail } from '../lib/sessionEnd';
import type { Me } from '../lib/types';

const LOGIN_FAILED = 'Email or password is incorrect.';

const AUTH_PATHS = ['/login', '/signup', '/verify-email', '/forgot-password', '/reset-password', '/lost-authenticator', '/lost-authenticator/confirm', '/cancel-2fa-removal'];

/** Where to go after signing in: a path of this app (never another site, never a sign-in page), else home. */
export function returnPath(from: unknown): string {
  // One leading slash and no backslash or whitespace: "//host" and "/\host" would leave the site.
  if (typeof from !== 'string' || !from.startsWith('/') || from.startsWith('//') || /[\\\s]/.test(from)) return '/';
  const path = from.split(/[?#]/)[0]!;
  return AUTH_PATHS.includes(path) ? '/' : from;
}

type Step = 'password' | 'totp' | 'recovery';

// A wrong email or password is 401 unauthenticated; with the right password and 2FA on, the
// server answers totp_required, with a message for a missing or wrong code. After too many wrong
// codes the authenticator waits, but a recovery code is still checked.
function loginError(e: unknown, step: Step): string {
  if (e instanceof ApiError && e.status === 401 && e.code !== 'totp_required') return LOGIN_FAILED;
  if (step === 'totp' && e instanceof ApiError && e.code === 'rate_limited') {
    return `Too many wrong codes. Try again in ${e.retryAfter ?? 60} seconds, or use a recovery code.`;
  }
  return errorMessage(e);
}

export function LoginPage() {
  const me = useMe();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const location = useLocation();
  const nav = (location.state ?? {}) as { from?: string; email?: string; notice?: string; ended?: boolean };
  const from = returnPath(nav.from);
  // From a session that ended, only that account goes back to its page; another one would open the
  // previous account's payment or link and find nothing, so it starts where a plain sign-in does.
  const destination = (signedIn: Me | null) => (nav.ended && !sameEmail(signedIn?.user.email, nav.email) ? returnPath(undefined) : from);
  const [email, setEmail] = useState(nav.email ?? '');
  const [password, setPassword] = useState('');
  const [code, setCode] = useState('');
  const [step, setStep] = useState<Step>('password');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const codeRef = useRef<HTMLInputElement>(null);
  const emailRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    document.title = 'Sign in · Prismaport';
  }, []);

  useEffect(() => {
    if (step !== 'password') codeRef.current?.focus();
  }, [step]);

  if (me.data) return <Navigate to={destination(me.data)} replace />;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    if (step === 'password' && (!email.trim() || !password)) {
      setError('Enter your email and password.');
      return;
    }
    if (step === 'totp' && !/^\d{6}$/.test(code.trim())) {
      setError('Enter the 6-digit code from your authenticator app.');
      return;
    }
    const recoveryCode = step === 'recovery' ? recoveryCodeValue(code) : null;
    if (step === 'recovery' && !recoveryCode) {
      setError('Enter one of your recovery codes, like K7M2Q-9XQPA.');
      return;
    }
    setBusy(true);
    try {
      const r = await api.login({ email: email.trim(), password, ...(step === 'totp' ? { totp: code.trim() } : {}), ...(recoveryCode ? { recovery_code: recoveryCode } : {}) });
      noteRecoverySignIn(r.used_recovery_code === true);
      const user = await fetchMe();
      qc.setQueryData(keys.me, user);
      // Replacing this entry also drops the ended session's state, so a later sign-in starts clean.
      navigate(destination(user), { replace: true });
    } catch (err) {
      if (step === 'password' && err instanceof ApiError && err.code === 'totp_required') {
        // The right password: the account has 2FA on, so ask for its code.
        setStep('totp');
        setCode('');
      } else {
        setError(loginError(err, step));
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <main className="auth-page">
      <div className="auth-card">
        <Lockup />
        <form className="card stack" onSubmit={submit} noValidate aria-labelledby="login-title">
          {step === 'password' ? (
            <>
              <div className="stack-sm">
                <h1 className="display" id="login-title">
                  Sign in
                </h1>
                <p className="muted">Your payment links, payments and settings.</p>
              </div>
              <FormNotice message={nav.notice} icon={nav.ended ? 'clock' : undefined} />
              <div className="form-grid">
                <Field label="Email">
                  {(p) => <input {...p} ref={emailRef} className="input" type="email" autoComplete="username" value={email} onChange={(e) => setEmail(e.target.value)} />}
                </Field>
                <Field label="Password">
                  {(p) => <input {...p} className="input" type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />}
                </Field>
              </div>
            </>
          ) : step === 'totp' ? (
            <>
              <div className="stack-sm">
                <h1 className="display" id="login-title">
                  Two-factor code
                </h1>
                <p className="muted">Two-step sign-in is on for {email.trim()}. Enter the 6-digit code from your authenticator app.</p>
              </div>
              <CodeInput ref={codeRef} label="Code" value={code} onChange={setCode} />
            </>
          ) : (
            <>
              <div className="stack-sm">
                <h1 className="display" id="login-title">
                  Recovery code
                </h1>
                <p className="muted">Enter one of the recovery codes you saved for {email.trim()}. Each code works once.</p>
              </div>
              <Field label="Recovery code">
                {(p) => (
                  <input
                    {...p}
                    ref={codeRef}
                    className="input mono"
                    autoComplete="off"
                    spellCheck={false}
                    maxLength={11}
                    placeholder="XXXXX-XXXXX"
                    value={code}
                    onChange={(e) => setCode(typedRecoveryCode(e.target.value))}
                  />
                )}
              </Field>
            </>
          )}
          {error ? (
            <p className="error" role="alert">
              <Icon name="alert" />
              <span>{error}</span>
            </p>
          ) : null}
          <div className="stack-sm">
            <button type="submit" className="btn btn-primary btn-block" disabled={busy}>
              {busy ? <span className="spinner" aria-hidden="true" /> : null}
              {step === 'password' ? 'Sign in' : 'Verify'}
            </button>
            {step === 'totp' ? (
              <button
                type="button"
                className="btn btn-ghost btn-block"
                onClick={() => {
                  setStep('recovery');
                  setCode('');
                  setError(null);
                }}
              >
                Use a recovery code
              </button>
            ) : null}
            {step === 'recovery' ? (
              <button
                type="button"
                className="btn btn-ghost btn-block"
                onClick={() => {
                  setStep('totp');
                  setCode('');
                  setError(null);
                }}
              >
                Use an authenticator code
              </button>
            ) : null}
            {step !== 'password' ? (
              <button
                type="button"
                className="btn btn-ghost btn-block"
                onClick={() => {
                  setStep('password');
                  setCode('');
                  setError(null);
                }}
              >
                Use a different account
              </button>
            ) : null}
          </div>
        </form>
        <div className="small muted auth-foot stack-sm">
          {step !== 'password' ? (
            <p>
              Lost your app and your codes?{' '}
              <Link to="/lost-authenticator" state={{ email: email.trim() || undefined }}>
                Get back into your account
              </Link>
            </p>
          ) : null}
          <p>
            <Link to="/forgot-password" state={{ email: email.trim() || undefined }}>
              Forgot your password?
            </Link>
          </p>
          <p>
            New to Prismaport? <Link to="/signup">Create an account</Link>
          </p>
        </div>
      </div>
    </main>
  );
}
