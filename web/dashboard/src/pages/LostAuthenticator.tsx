import { useEffect, useRef, useState, type FormEvent } from 'react';
import { Link, useLocation, useNavigate } from 'react-router-dom';
import { AuthLayout, emailProblem, FormError } from '../components/AuthLayout';
import { Icon, type IconName } from '../components/Icon';
import { Field } from '../components/ui';
import { ApiError } from '../lib/api';
import { api } from '../lib/endpoints';
import { errorMessage } from '../lib/errors';
import { formatUtc, RECOVERY_LOCK_HOURS, REMOVAL_WAIT_HOURS } from '../lib/recovery';

declare const __MOCK__: boolean;

/** The tokens every lost-app link and cancel link carry in the preview build (the mocks' MOCK_LOST_TOKEN). */
const PREVIEW_LOST_TOKEN = 'PreviewLostLinkToken_0123456789abcdefghijklmn';

/** The token of an emailed link. It rides in the fragment, which browsers never send to a server. */
function linkToken(hash: string): string {
  return new URLSearchParams(hash.replace(/^#/, '')).get('token') ?? '';
}

/** The token leaves the address bar and the history at once; the page keeps it in memory. */
function useTokenFromFragment(): string {
  const navigate = useNavigate();
  const location = useLocation();
  const [token] = useState(() => linkToken(location.hash));
  useEffect(() => {
    if (location.hash) navigate({ pathname: location.pathname, search: location.search }, { replace: true });
  }, [location.hash, location.pathname, location.search, navigate]);
  return token;
}

const BackToSignIn = ({ ghost = false }: { ghost?: boolean }) => (
  <Link className={ghost ? 'btn btn-ghost btn-block' : 'btn btn-block'} to="/login">
    Back to sign in
  </Link>
);

/**
 * Asks for the link that starts removing two-factor authentication. The answer is the same for
 * every email, so the page always says a link may be on its way.
 */
export function LostAuthenticatorPage() {
  const state = (useLocation().state ?? {}) as { email?: string };
  const [email, setEmail] = useState(state.email ?? '');
  const [sent, setSent] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const ref = useRef<HTMLInputElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);

  useEffect(() => {
    if (sent) heading.current?.focus();
  }, [sent]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setError(null);
    const problem = emailProblem(email);
    if (problem) {
      setError(problem);
      ref.current?.focus();
      return;
    }
    setBusy(true);
    try {
      await api.totpLost(email.trim().toLowerCase());
      setSent(true);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  if (sent) {
    return (
      <AuthLayout title="Check your email">
        <section className="card stack" aria-labelledby="lost-sent-title">
          <div className="stack-sm">
            <h1 className="display" id="lost-sent-title" ref={heading} tabIndex={-1}>
              Check your email
            </h1>
            <p className="muted">If that account has two-factor authentication on, a link is on its way. It works for 30 minutes.</p>
          </div>
          {__MOCK__ ? (
            <p className="small muted">
              Preview build: <Link to={`/lost-authenticator/confirm#token=${PREVIEW_LOST_TOKEN}`}>open the link</Link>.
            </p>
          ) : null}
          <BackToSignIn />
        </section>
      </AuthLayout>
    );
  }

  return (
    <AuthLayout
      title="Lost your authenticator app"
      foot={
        <p>
          Saved your recovery codes? <Link to="/login">Sign in with one</Link>. It is much faster.
        </p>
      }
    >
      <form className="card stack" onSubmit={submit} noValidate aria-labelledby="lost-title">
        <div className="stack-sm">
          <h1 className="display" id="lost-title">
            Lost your authenticator app?
          </h1>
          <p className="muted">Enter your account email. If two-factor authentication is on for it, we send a link to remove it.</p>
        </div>
        <Field label="Email">
          {(p) => <input {...p} ref={ref} className="input" type="email" autoComplete="email" value={email} onChange={(e) => setEmail(e.target.value)} />}
        </Field>
        <FormError message={error} />
        <button type="submit" className="btn btn-primary btn-block" disabled={busy}>
          {busy ? <span className="spinner" aria-hidden="true" /> : null}
          Send link
        </button>
      </form>
    </AuthLayout>
  );
}

type LinkState = { kind: 'checking' } | { kind: 'ready'; email: string } | { kind: 'invalid' } | { kind: 'error'; message: string };
type Done = { removalAt: string; cancelToken: string; cancelled: boolean };

function InvalidLink({ children }: { children: string }) {
  return (
    <AuthLayout title="This link no longer works" foot={<p><Link to="/login">Back to sign in</Link></p>}>
      <section className="card stack" aria-labelledby="invalid-title">
        <div className="stack-sm">
          <h1 className="display" id="invalid-title">
            This link no longer works
          </h1>
          <p className="muted">{children}</p>
        </div>
        <Link className="btn btn-primary btn-block" to="/lost-authenticator">
          Send a new link
        </Link>
      </section>
    </AuthLayout>
  );
}

const FACTS: { icon: IconName; text: string }[] = [
  { icon: 'clock', text: `It is removed after ${REMOVAL_WAIT_HOURS} hours. You can cancel until then.` },
  { icon: 'send', text: 'We email you now, 24 hours before it is removed, and when it is removed.' },
  { icon: 'wallet', text: 'Payments keep going to your wallet while you wait.' },
  { icon: 'key', text: `Afterwards, adding a wallet and creating live API keys stay locked for ${RECOVERY_LOCK_HOURS} more hours.` },
];

/**
 * Where the emailed link lands: it checks the link, takes the password and starts the wait. The
 * link is spent only by a right password.
 */
export function LostConfirmPage() {
  const token = useTokenFromFragment();
  const [link, setLink] = useState<LinkState>({ kind: 'checking' });
  const [attempt, setAttempt] = useState(0);
  const [password, setPassword] = useState('');
  const [passwordError, setPasswordError] = useState<string | undefined>();
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState<Done | null>(null);
  const [cancelError, setCancelError] = useState<string | null>(null);

  // Checking spends nothing: only a right password does.
  useEffect(() => {
    if (!token) {
      setLink({ kind: 'invalid' });
      return;
    }
    let live = true;
    setLink({ kind: 'checking' });
    api.totpLostCheck(token).then(
      (r) => {
        if (live) setLink({ kind: 'ready', email: r.email });
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

  const start = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || link.kind !== 'ready') return;
    setError(null);
    if (!password) return setPasswordError('Enter your password to confirm.');
    setBusy(true);
    try {
      const r = await api.totpLostConfirm({ token, password });
      setDone({ removalAt: r.removal_at, cancelToken: r.cancel_token, cancelled: false });
    } catch (err) {
      if (err instanceof ApiError && err.code === 'invalid_password') setPasswordError(err.message || 'That password is not correct.');
      else if (err instanceof ApiError && err.code === 'invalid_link') setLink({ kind: 'invalid' });
      else setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const cancel = async () => {
    if (!done || busy) return;
    setCancelError(null);
    setBusy(true);
    try {
      await api.totpRemovalCancelLink(done.cancelToken);
      setDone({ ...done, cancelled: true });
    } catch (err) {
      setCancelError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const foot = (
    <p>
      <Link to="/login">Back to sign in</Link>
    </p>
  );

  if (done) {
    const who = link.kind === 'ready' ? link.email : 'your account';
    if (done.cancelled) {
      return (
        <AuthLayout title="Removal cancelled">
          <section className="card stack" aria-labelledby="cancelled-title">
            <div className="stack-sm">
              <h1 className="display" id="cancelled-title">
                Removal cancelled
              </h1>
              <p className="muted">
                Two-factor authentication stays on for <strong>{who}</strong>. If you did not ask for this, change your password.
              </p>
            </div>
            <BackToSignIn />
          </section>
        </AuthLayout>
      );
    }
    return (
      <AuthLayout title="Removal scheduled">
        <section className="card stack" aria-labelledby="pending-title">
          <div className="stack-sm">
            <h1 className="display" id="pending-title">
              Removal scheduled
            </h1>
            <p className="muted">
              Two-factor authentication on <strong>{who}</strong> is removed on {formatUtc(done.removalAt)}. After that, your password signs you in.
            </p>
          </div>
          <p className="small muted">Until then your authenticator app and recovery codes keep working. Signing in with either one cancels this request.</p>
          <FormError message={cancelError} />
          <div className="stack-sm">
            <button type="button" className="btn btn-block" disabled={busy} onClick={() => void cancel()}>
              Cancel removal
            </button>
            <BackToSignIn ghost />
          </div>
        </section>
      </AuthLayout>
    );
  }

  if (link.kind === 'checking') {
    return (
      <AuthLayout title="Remove two-factor authentication" foot={foot}>
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
      <AuthLayout title="Remove two-factor authentication" foot={foot}>
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
    return <InvalidLink>This link works for 30 minutes and only once, and a newer link replaces an older one. Ask for a new one.</InvalidLink>;
  }

  return (
    <AuthLayout title="Remove two-factor authentication" foot={foot}>
      <form className="card stack" onSubmit={start} noValidate aria-labelledby="confirm-title">
        <div className="stack-sm">
          <h1 className="display" id="confirm-title">
            Remove two-factor authentication
          </h1>
          <p className="muted">
            For the account <strong>{link.email}</strong>.
          </p>
        </div>
        <ul className="bullets">
          {FACTS.map((f) => (
            <li key={f.text}>
              <Icon name={f.icon} />
              <span>{f.text}</span>
            </li>
          ))}
        </ul>
        <input type="email" name="username" autoComplete="username" value={link.email} readOnly hidden />
        <Field label="Password" error={passwordError}>
          {(p) => (
            <input
              {...p}
              className="input"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => {
                setPassword(e.target.value);
                setPasswordError(undefined);
              }}
            />
          )}
        </Field>
        <FormError message={error} />
        <button type="submit" className="btn btn-primary btn-block" disabled={busy}>
          {busy ? <span className="spinner" aria-hidden="true" /> : null}
          Start the {REMOVAL_WAIT_HOURS}-hour wait
        </button>
      </form>
    </AuthLayout>
  );
}

/**
 * The emailed "Cancel removal" link lands here. Opening it changes nothing; only the button
 * cancels, so a mail scanner that fetches every link cannot.
 */
export function CancelRemovalPage() {
  const token = useTokenFromFragment();
  const [state, setState] = useState<'ask' | 'done' | 'invalid'>(token ? 'ask' : 'invalid');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const cancel = async () => {
    if (busy) return;
    setError(null);
    setBusy(true);
    try {
      await api.totpRemovalCancelLink(token);
      setState('done');
    } catch (err) {
      if (err instanceof ApiError && err.code === 'invalid_link') setState('invalid');
      else setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  if (state === 'invalid') {
    return (
      <AuthLayout title="This link no longer works" foot={<p><Link to="/login">Back to sign in</Link></p>}>
        <section className="card stack" aria-labelledby="cancel-invalid-title">
          <div className="stack-sm">
            <h1 className="display" id="cancel-invalid-title">
              This link no longer works
            </h1>
            <p className="muted">The removal was already cancelled, or two-factor authentication was already removed. Sign in to see where your account stands.</p>
          </div>
          <BackToSignIn />
        </section>
      </AuthLayout>
    );
  }

  if (state === 'done') {
    return (
      <AuthLayout title="Removal cancelled">
        <section className="card stack" aria-labelledby="cancel-done-title">
          <div className="stack-sm">
            <h1 className="display" id="cancel-done-title">
              Removal cancelled
            </h1>
            <p className="muted">Two-factor authentication stays on. If you did not ask for this, sign in and change your password.</p>
          </div>
          <BackToSignIn />
        </section>
      </AuthLayout>
    );
  }

  return (
    <AuthLayout title="Cancel the removal" foot={<p><Link to="/login">Back to sign in</Link></p>}>
      <section className="card stack" aria-labelledby="cancel-title">
        <div className="stack-sm">
          <h1 className="display" id="cancel-title">
            Cancel the removal?
          </h1>
          <p className="muted">Someone asked to remove two-factor authentication from your account. Cancel now if this was not you, then sign in and change your password.</p>
        </div>
        <FormError message={error} />
        <button type="button" className="btn btn-primary btn-block" disabled={busy} onClick={() => void cancel()}>
          {busy ? <span className="spinner" aria-hidden="true" /> : null}
          Cancel removal
        </button>
      </section>
    </AuthLayout>
  );
}
