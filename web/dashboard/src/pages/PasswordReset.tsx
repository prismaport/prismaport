import { useEffect, useRef, useState, type FormEvent } from 'react';
import { Link, useLocation, useNavigate } from 'react-router-dom';
import { AuthLayout, emailProblem, FormError, ResendRow, useCountdown } from '../components/AuthLayout';
import { NewPasswordField, useNewPassword } from '../components/NewPassword';
import { SecondFactorField, useSecondFactor } from '../components/SecondFactor';
import { Field } from '../components/ui';
import { ApiError } from '../lib/api';
import { api } from '../lib/endpoints';
import { errorMessage } from '../lib/errors';
import { RESEND_SECONDS } from '../lib/pending';

declare const __MOCK__: boolean;

/** The token every reset link carries in the preview build (the mocks' MOCK_RESET_TOKEN). */
const PREVIEW_TOKEN = 'PreviewResetLinkToken_0123456789abcdefghijk';

/**
 * Asks for a reset link. The answer is the same whether or not the email has an account, so the
 * page always says the link was sent.
 */
export function ForgotPasswordPage() {
  const state = (useLocation().state ?? {}) as { email?: string };
  const [email, setEmail] = useState(state.email ?? '');
  const [sentTo, setSentTo] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [left, startTimer] = useCountdown(0);
  const ref = useRef<HTMLInputElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);

  useEffect(() => {
    if (sentTo) heading.current?.focus();
  }, [sentTo]);

  const send = async (address: string) => {
    setError(null);
    setBusy(true);
    try {
      await api.forgotPassword(address);
      setSentTo(address);
      startTimer(RESEND_SECONDS);
    } catch (err) {
      if (err instanceof ApiError && err.code === 'rate_limited') startTimer(err.retryAfter ?? RESEND_SECONDS);
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    const problem = emailProblem(email);
    if (problem) {
      setError(problem);
      ref.current?.focus();
      return;
    }
    void send(email.trim().toLowerCase());
  };

  if (sentTo) {
    return (
      <AuthLayout
        title="Check your email"
        foot={
          <p>
            Wrong address?{' '}
            <button
              type="button"
              className="btn-link"
              onClick={() => {
                setSentTo(null);
                setError(null);
              }}
            >
              Use another email
            </button>
          </p>
        }
      >
        <section className="card stack" aria-labelledby="sent-title">
          <div className="stack-sm">
            <h1 className="display" id="sent-title" ref={heading} tabIndex={-1}>
              Check your email
            </h1>
            <p className="muted">
              We sent a link to <strong>{sentTo}</strong>. Open it to choose a new password. It works for 30 minutes, once.
            </p>
          </div>
          {__MOCK__ ? (
            <p className="small muted">
              Preview build: <Link to={`/reset-password#token=${PREVIEW_TOKEN}`}>open the link</Link>.
            </p>
          ) : null}
          <FormError message={error} />
          <ResendRow left={left} onResend={() => void send(sentTo)} timerId="forgot-resend-timer" label="Send again" />
        </section>
      </AuthLayout>
    );
  }

  return (
    <AuthLayout
      title="Reset your password"
      foot={
        <p>
          Remembered it? <Link to="/login">Sign in</Link>
        </p>
      }
    >
      <form className="card stack" onSubmit={submit} noValidate aria-labelledby="forgot-title">
        <div className="stack-sm">
          <h1 className="display" id="forgot-title">
            Reset your password
          </h1>
          <p className="muted">Enter the email of your account. We send it a link to choose a new password.</p>
        </div>
        <Field label="Email">
          {(p) => <input {...p} ref={ref} className="input" type="email" autoComplete="email" value={email} onChange={(e) => setEmail(e.target.value)} />}
        </Field>
        <FormError message={error} />
        <button type="submit" className="btn btn-primary btn-block" disabled={busy}>
          {busy ? <span className="spinner" aria-hidden="true" /> : null}
          Send reset link
        </button>
      </form>
    </AuthLayout>
  );
}

/** The token of a reset link. It rides in the fragment, which browsers never send to a server. */
function linkToken(hash: string): string {
  return new URLSearchParams(hash.replace(/^#/, '')).get('token') ?? '';
}

type LinkState =
  | { kind: 'checking' }
  | { kind: 'ready'; email: string; totpRequired: boolean }
  | { kind: 'invalid' }
  | { kind: 'error'; message: string };

/** Where a reset link lands: it checks the link, then takes the new password. */
export function ResetPasswordPage() {
  const navigate = useNavigate();
  const location = useLocation();
  const [token] = useState(() => linkToken(location.hash));
  const [link, setLink] = useState<LinkState>({ kind: 'checking' });
  const [attempt, setAttempt] = useState(0);
  const factor = useSecondFactor();
  const newPassword = useNewPassword();
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const refs = { totp: useRef<HTMLInputElement>(null) };

  // The token leaves the address bar and the history at once; the page keeps it in memory.
  useEffect(() => {
    if (location.hash) navigate({ pathname: location.pathname, search: location.search }, { replace: true });
  }, [location.hash, location.pathname, location.search, navigate]);

  // Checking spends nothing: only saving the password does.
  useEffect(() => {
    if (!token) {
      setLink({ kind: 'invalid' });
      return;
    }
    let live = true;
    setLink({ kind: 'checking' });
    api.checkResetLink(token).then(
      (r) => {
        if (live) setLink({ kind: 'ready', email: r.email, totpRequired: r.totp_required });
      },
      (err) => {
        if (!live) return;
        setLink(err instanceof ApiError && err.code === 'invalid_link' ? { kind: 'invalid' } : { kind: 'error', message: errorMessage(err) });
      },
    );
    return () => {
      live = false;
    };
  }, [token, attempt]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || link.kind !== 'ready') return;
    setError(null);
    if (!newPassword.validate()) return newPassword.inputRef.current?.focus();
    if (link.totpRequired && !factor.check()) return refs.totp.current?.focus();
    setBusy(true);
    try {
      await api.resetPassword({ token, new_password: newPassword.password, ...(link.totpRequired ? factor.payload('totp') : {}) });
      navigate('/login', {
        replace: true,
        state: { email: link.email, notice: 'Your password was changed and every session was signed out. Sign in with the new password.' },
      });
    } catch (err) {
      if (err instanceof ApiError && err.code === 'invalid_link') {
        setLink({ kind: 'invalid' });
      } else if (err instanceof ApiError && err.code === 'totp_required') {
        factor.setValue('');
        factor.refuse(errorMessage(err));
        refs.totp.current?.focus();
      } else if (!newPassword.refused(err)) {
        setError(errorMessage(err));
      }
    } finally {
      setBusy(false);
    }
  };

  const foot = (
    <p>
      <Link to="/login">Back to sign in</Link>
    </p>
  );

  if (link.kind === 'checking') {
    return (
      <AuthLayout title="Choose a new password" foot={foot}>
        <section className="card stack" aria-busy="true">
          <p className="muted" role="status">
            <span className="spinner" aria-hidden="true" /> Checking your link.
          </p>
        </section>
      </AuthLayout>
    );
  }

  if (link.kind === 'error') {
    return (
      <AuthLayout title="Choose a new password" foot={foot}>
        <section className="card stack">
          <FormError message={link.message} />
          <button type="button" className="btn btn-primary btn-block" onClick={() => setAttempt((n) => n + 1)}>
            Try again
          </button>
        </section>
      </AuthLayout>
    );
  }

  if (link.kind === 'invalid') {
    return (
      <AuthLayout title="This link no longer works" foot={foot}>
        <section className="card stack" aria-labelledby="invalid-title">
          <div className="stack-sm">
            <h1 className="display" id="invalid-title">
              This link no longer works
            </h1>
            <p className="muted">A reset link works for 30 minutes and only once, and a newer link replaces an older one. Ask for a new one.</p>
          </div>
          <Link className="btn btn-primary btn-block" to="/forgot-password">
            Send a new link
          </Link>
        </section>
      </AuthLayout>
    );
  }

  return (
    <AuthLayout title="Choose a new password" foot={foot}>
      <form className="card stack" onSubmit={submit} noValidate aria-labelledby="reset-title">
        <div className="stack-sm">
          <h1 className="display" id="reset-title">
            Choose a new password
          </h1>
          <p className="muted">
            For the account <strong>{link.email}</strong>.
          </p>
        </div>
        {/* Lets a password manager save the new password under the right account. */}
        <input type="email" name="username" autoComplete="username" value={link.email} readOnly hidden />
        <div className="form-grid">
          <NewPasswordField {...newPassword.field} label="New password" note="Saving signs you out on every device." />
          {link.totpRequired ? <SecondFactorField s={factor} label="Authenticator code" hint="Two-factor authentication is on for this account." inputRef={refs.totp} /> : null}
        </div>
        <FormError message={error} />
        <button type="submit" className="btn btn-primary btn-block" disabled={busy}>
          {busy ? <span className="spinner" aria-hidden="true" /> : null}
          Save new password
        </button>
      </form>
    </AuthLayout>
  );
}
