import { useState, type ReactNode, type Ref } from 'react';
import { recoveryCodeValue, typedRecoveryCode } from '../lib/recovery';
import { CodeInput } from './CodeInput';
import { Field } from './ui';

const CODE = /^\d{6}$/;

export interface SecondFactorState {
  /** True when a recovery code stands in for the authenticator code. */
  recovery: boolean;
  value: string;
  setValue: (v: string) => void;
  toggle: () => void;
  error: string | undefined;
  /** Checks the field before sending; false when it is not a code. */
  check: () => boolean;
  /** The request fields: `{ [key]: 123456 }`, or `{ recovery_code }` when a recovery code is in use. */
  payload: (key: 'code' | 'totp') => Record<string, string>;
  /** Shows a server refusal in the field. */
  refuse: (message: string) => void;
}

/** The code field of a form that also takes a recovery code: "Lost your app? Use a recovery code" swaps one for the other. */
export function useSecondFactor(missing = 'Enter the 6-digit code from your authenticator app.'): SecondFactorState {
  const [recovery, setRecovery] = useState(false);
  const [value, setValueRaw] = useState('');
  const [error, setError] = useState<string | undefined>();
  const setValue = (v: string) => {
    setValueRaw(recovery ? typedRecoveryCode(v) : v.replace(/\D/g, ''));
    setError(undefined);
  };
  return {
    recovery,
    value,
    setValue,
    toggle: () => {
      setRecovery((r) => !r);
      setValueRaw('');
      setError(undefined);
    },
    error,
    check: () => {
      const ok = recovery ? recoveryCodeValue(value) !== null : CODE.test(value.trim());
      setError(ok ? undefined : recovery ? 'Enter one of your recovery codes, like K7M2Q-9XQPA.' : missing);
      return ok;
    },
    payload: (key) => (recovery ? { recovery_code: recoveryCodeValue(value) ?? value.trim() } : { [key]: value.trim() }),
    refuse: setError,
  };
}

interface FieldProps {
  s: SecondFactorState;
  label: string;
  /** Said under the authenticator code field. */
  hint?: ReactNode;
  inputRef?: Ref<HTMLInputElement>;
  id?: string;
  autoFocus?: boolean;
  /** false hides the swap to a recovery code, for an account that has none. */
  recovery?: boolean;
}

/** The authenticator code field, or the recovery code field once swapped, with the link that swaps it. */
export function SecondFactorField({ s, label, hint, inputRef, id, autoFocus, recovery = true }: FieldProps) {
  const swap =
    recovery || s.recovery ? (
      <button type="button" className="btn-link small" onClick={s.toggle}>
        {s.recovery ? 'Use an authenticator code' : 'Lost your app? Use a recovery code'}
      </button>
    ) : null;
  // One root for both modes, so the swap button stays mounted and keeps focus.
  return (
    <div className="stack-sm">
      {s.recovery ? (
        <Field id={id} label="Recovery code" hint="Each recovery code works once." error={s.error}>
          {(p) => <input {...p} ref={inputRef} className="input mono" autoComplete="off" spellCheck={false} maxLength={11} placeholder="XXXXX-XXXXX" value={s.value} onChange={(e) => s.setValue(e.target.value)} data-autofocus={autoFocus ? '' : undefined} />}
        </Field>
      ) : (
        <CodeInput ref={inputRef} id={id} label={label} hint={hint} value={s.value} onChange={s.setValue} status={s.error ? 'invalid' : 'idle'} message={s.error} clearOnInvalid={false} dialogFocus={autoFocus} />
      )}
      {swap ? <div>{swap}</div> : null}
    </div>
  );
}
