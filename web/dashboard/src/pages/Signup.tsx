import { useRef, useState, type FormEvent } from 'react';
import { Link, Navigate, useNavigate } from 'react-router-dom';
import { AuthLayout, emailProblem, FormError } from '../components/AuthLayout';
import { NewPasswordField, useNewPassword } from '../components/NewPassword';
import { Field } from '../components/ui';
import { api } from '../lib/endpoints';
import { errorMessage } from '../lib/errors';
import { setPendingSignup } from '../lib/pending';
import { useMe } from '../lib/queries';

const NAME_MAX = 200;

export function SignupPage() {
  const me = useMe();
  const navigate = useNavigate();
  const [name, setName] = useState('');
  const [email, setEmail] = useState('');
  const newPassword = useNewPassword();
  const { password } = newPassword;
  const [submitted, setSubmitted] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const refs = { name: useRef<HTMLInputElement>(null), email: useRef<HTMLInputElement>(null), password: newPassword.inputRef };

  if (me.data) return <Navigate to="/" replace />;

  const problems = {
    name: !name.trim() ? 'Enter the name of your business or project.' : name.trim().length > NAME_MAX ? `Use at most ${NAME_MAX} characters.` : null,
    email: emailProblem(email),
    password: newPassword.problem,
  };
  const show = (k: keyof typeof problems) => (submitted ? problems[k] ?? undefined : undefined);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    setError(null);
    newPassword.validate();
    const first = (['name', 'email', 'password'] as const).find((k) => problems[k]);
    if (first) {
      refs[first].current?.focus();
      return;
    }
    setBusy(true);
    try {
      const address = email.trim().toLowerCase();
      await api.signup({ business_name: name.trim(), email: address, password });
      setPendingSignup(address, password);
      navigate('/verify-email', { state: { email: address, sent: true } });
    } catch (err) {
      if (!newPassword.refused(err)) setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthLayout
      title="Create your account"
      foot={
        <>
          <p>
            Already have an account? <Link to="/login">Sign in</Link>
          </p>
          <p>Live and Test mode both open once you confirm your email.</p>
        </>
      }
    >
      <form className="card stack" onSubmit={submit} noValidate aria-labelledby="signup-title">
        <div className="stack-sm">
          <h1 className="display" id="signup-title">
            Create your account
          </h1>
          <p className="muted">Accept stablecoins with a link or an API. Every payment goes straight to your wallet.</p>
        </div>
        <div className="form-grid">
          <Field label="Business name" hint="The name of your business or project." error={show('name')}>
            {(p) => <input {...p} ref={refs.name} className="input" autoComplete="organization" maxLength={NAME_MAX + 20} value={name} onChange={(e) => setName(e.target.value)} />}
          </Field>
          <Field label="Email" hint="We send a code to confirm it." error={show('email')}>
            {(p) => <input {...p} ref={refs.email} className="input" type="email" autoComplete="email" value={email} onChange={(e) => setEmail(e.target.value)} />}
          </Field>
          <NewPasswordField {...newPassword.field} />
        </div>
        <FormError message={error} />
        <button type="submit" className="btn btn-primary btn-block" disabled={busy}>
          {busy ? <span className="spinner" aria-hidden="true" /> : null}
          Create account
        </button>
      </form>
    </AuthLayout>
  );
}
