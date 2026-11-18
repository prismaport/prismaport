import { useId, useRef, useState, type ReactNode, type RefObject } from 'react';
import { ApiError } from '../lib/api';
import {
  BREACHED_PASSWORD_ERROR,
  COMMON_PASSWORD_ERROR,
  commonOrRepeated,
  missingParts,
  PASSWORD_MIN_CHARS,
  passwordChecks,
  passwordProblem,
} from '../lib/password';
import { Icon } from './Icon';

interface NewPasswordFieldProps {
  value: string;
  onChange: (v: string) => void;
  error: string | null;
  inputRef: RefObject<HTMLInputElement>;
  label?: string;
  /** Said before the password manager hint, about what setting this password does. */
  note?: ReactNode;
  id?: string;
}

/** The field for a password being created or changed, with its live checklist. */
export function NewPasswordField({ value, onChange, error, inputRef, label = 'Password', note, id: given }: NewPasswordFieldProps) {
  const auto = useId();
  const id = given ?? auto;
  const [show, setShow] = useState(false);
  const c = passwordChecks(value);
  const longEnough = c.length >= PASSWORD_MIN_CHARS;
  const rules: { text: string; ok: boolean; count?: string }[] = [
    { text: `${PASSWORD_MIN_CHARS} characters or more`, ok: longEnough, count: `${c.length} / ${PASSWORD_MIN_CHARS}` },
    { text: 'Uppercase and lowercase letters', ok: c.upper && c.lower },
    { text: 'A number', ok: c.number },
    { text: 'A special character', ok: c.special },
    { text: 'Not a common or repeated password', ok: longEnough && !commonOrRepeated(value) },
  ];
  const describedBy = [`${id}-hint`, `${id}-rules`, error ? `${id}-error` : null].filter(Boolean).join(' ');
  return (
    <div className="field">
      <label htmlFor={id}>{label}</label>
      <div className="password-wrap">
        <input
          ref={inputRef}
          id={id}
          className="input"
          type={show ? 'text' : 'password'}
          autoComplete="new-password"
          spellCheck={false}
          autoCapitalize="none"
          value={value}
          onChange={(e) => onChange(e.target.value)}
          aria-describedby={describedBy}
          {...(error ? { 'aria-invalid': true as const } : {})}
        />
        <button type="button" className="password-toggle" aria-label={show ? 'Hide password' : 'Show password'} aria-pressed={show} onClick={() => setShow((s) => !s)}>
          <Icon name={show ? 'eyeOff' : 'eye'} size={20} />
        </button>
      </div>
      <p className="hint" id={`${id}-hint`}>
        {note ? <>{note} </> : null}Your password manager can fill this in.
      </p>
      <ul className="password-rules" id={`${id}-rules`}>
        {rules.map((r) => (
          <li key={r.text} className={r.ok ? 'password-rule-ok' : undefined}>
            <span className="password-rule-mark" role="img" aria-label={r.ok ? 'Done' : 'Not yet'}>
              <Icon name={r.ok ? 'check' : 'circle'} />
            </span>
            <span className="password-rule-text">{r.text}</span>
            {r.count ? <span className="mono password-rule-count">{r.count}</span> : null}
          </li>
        ))}
      </ul>
      {error ? (
        <p className="error" id={`${id}-error`} role="alert">
          <Icon name="alert" />
          <span>{error}</span>
        </p>
      ) : null}
    </div>
  );
}

/** The state of a {@link NewPasswordField}: its value, what is wrong with it and when to say so. */
export function useNewPassword() {
  const [password, setPassword] = useState('');
  const [submitted, setSubmitted] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  const problem = passwordProblem(password);
  // Everything waits for a submit except a common password, which no amount of typing more fixes.
  const met = missingParts(passwordChecks(password)).length === 0;
  const early = met && commonOrRepeated(password) ? COMMON_PASSWORD_ERROR : null;
  const error = (submitted ? problem : early) ?? refusal;

  const onChange = (v: string) => {
    setPassword(v);
    setSubmitted(false);
    setRefusal(null);
  };

  return {
    password,
    problem,
    inputRef,
    /** Call on submit: shows what is wrong, and false when the password cannot be sent. */
    validate(): boolean {
      setSubmitted(true);
      setRefusal(null);
      return problem === null;
    },
    /** Shows the server's refusal of the password on the field; false for any other error. */
    refused(err: unknown): boolean {
      if (!(err instanceof ApiError)) return false;
      if (err.code === 'breached_password') setRefusal(BREACHED_PASSWORD_ERROR);
      else if (err.code === 'common_password') setRefusal(COMMON_PASSWORD_ERROR);
      else if (err.code === 'weak_password' || (err.code === 'invalid_request' && err.param === 'new_password')) setRefusal(err.message);
      else return false;
      inputRef.current?.focus();
      return true;
    },
    /** Props for NewPasswordField. */
    field: { value: password, onChange, error, inputRef },
  };
}
