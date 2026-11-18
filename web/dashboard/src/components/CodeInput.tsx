import { forwardRef, useEffect, useId, useImperativeHandle, useRef, useState, type ClipboardEvent, type CSSProperties, type ReactNode } from 'react';
import { Icon } from './Icon';

export type CodeMode = 'numeric' | 'alphanumeric';

/**
 * idle: typing. invalid: a wrong value (amber, a short shake). confirmed: accepted (green).
 * expired and locked: amber with no way forward but a new code; locked also disables the field.
 */
export type CodeStatus = 'idle' | 'invalid' | 'confirmed' | 'expired' | 'locked';

export type CodeTone = 'neutral' | 'error' | 'ok';

export interface CodeInputProps {
  id?: string;
  length?: number;
  mode?: CodeMode;
  /** Letters match in any case: `onComplete` gets the value lowercased; the boxes show what was typed. */
  caseInsensitive?: boolean;
  value: string;
  onChange: (value: string) => void;
  onComplete?: (value: string) => void;
  status?: CodeStatus;
  label: ReactNode;
  hint?: ReactNode;
  /** Shown under the boxes and announced politely. */
  message?: ReactNode;
  /** The message's colour; by default it follows the status. */
  tone?: CodeTone;
  /** More ids for aria-describedby, after the hint and the message. */
  describedBy?: string;
  /** Empty the field and focus it again shortly after a wrong value (codes); off where the typed value must stay visible. */
  clearOnInvalid?: boolean;
  disabled?: boolean;
  name?: string;
  /** Focus the field when it appears, for a field that shows up in a dialog already open. */
  autoFocus?: boolean;
  /** The field a dialog focuses first when it opens; the dialog still sees what opened it. */
  dialogFocus?: boolean;
}

const CLEAR_AFTER_MS = 650;
const SHAKE_MS = 340;

/** What the field keeps of typed or pasted text: allowed characters only, at most `length`. */
export function sanitizeCode(raw: string, mode: CodeMode, length: number): string {
  const kept = mode === 'numeric' ? raw.replace(/\D/g, '') : raw.replace(/[^0-9A-Za-z]/g, '');
  return kept.slice(0, length);
}

const toneOf = (s: CodeStatus): CodeTone => (s === 'confirmed' ? 'ok' : s === 'idle' ? 'neutral' : 'error');

/**
 * A code drawn as one box per character over a single real input, so paste, one-time-code
 * autofill and screen readers all see one labelled text field.
 */
export const CodeInput = forwardRef<HTMLInputElement, CodeInputProps>(function CodeInput(
  { id: given, length = 6, mode = 'numeric', caseInsensitive = false, value, onChange, onComplete, status = 'idle', label, hint, message, tone, describedBy, clearOnInvalid = true, disabled, name, autoFocus, dialogFocus },
  ref,
) {
  const auto = useId();
  const id = given ?? auto;
  const input = useRef<HTMLInputElement>(null);
  useImperativeHandle(ref, () => input.current!, []);
  const [shake, setShake] = useState(false);
  const latest = useRef({ onChange, status });
  latest.current = { onChange, status };

  useEffect(() => {
    if (status !== 'invalid') return;
    setShake(true);
    const stop = setTimeout(() => setShake(false), SHAKE_MS);
    const clear = clearOnInvalid
      ? setTimeout(() => {
          if (latest.current.status !== 'invalid') return;
          latest.current.onChange('');
          input.current?.focus();
        }, CLEAR_AFTER_MS)
      : undefined;
    return () => {
      clearTimeout(stop);
      clearTimeout(clear);
    };
  }, [status, clearOnInvalid]);

  const complete = (v: string) => onComplete?.(caseInsensitive ? v.toLowerCase() : v);

  const emit = (raw: string) => {
    const next = sanitizeCode(raw, mode, length);
    if (next === value) return;
    onChange(next);
    if (next.length === length) complete(next);
  };

  const onPaste = (e: ClipboardEvent<HTMLInputElement>) => {
    // A pasted code replaces what is there, spaces and dashes dropped ("246 810", "246-810").
    const pasted = sanitizeCode(e.clipboardData.getData('text'), mode, length);
    if (!pasted) return;
    e.preventDefault();
    // Pasting the same full code again is another try.
    if (pasted === value && pasted.length === length) complete(pasted);
    else emit(pasted);
  };

  // The caret always sits after the last character, where the next box is drawn.
  const toEnd = () => {
    const el = input.current;
    if (el && el.selectionStart !== el.value.length) el.setSelectionRange(el.value.length, el.value.length);
  };

  const locked = status === 'locked' || Boolean(disabled);
  const active = status === 'confirmed' || locked ? -1 : Math.min(value.length, length - 1);
  const hintId = hint ? `${id}-hint` : undefined;
  const messageId = `${id}-message`;
  const shownTone = tone ?? toneOf(status);
  const chars = [...value];

  return (
    <div className="code-field" data-status={status} data-mode={mode} data-shake={shake || undefined}>
      <label htmlFor={id}>{label}</label>
      {hint ? (
        <p className="hint" id={hintId}>
          {hint}
        </p>
      ) : null}
      <div className="code-entry" style={{ '--code-length': length } as CSSProperties}>
        <input
          ref={input}
          id={id}
          name={name}
          className="code-native"
          type="text"
          value={value}
          onChange={(e) => emit(e.target.value)}
          onPaste={onPaste}
          onFocus={toEnd}
          onClick={toEnd}
          onKeyUp={toEnd}
          disabled={locked}
          autoFocus={autoFocus}
          data-autofocus={dialogFocus ? '' : undefined}
          inputMode={mode === 'numeric' ? 'numeric' : 'text'}
          pattern={mode === 'numeric' ? '[0-9]*' : undefined}
          autoComplete="one-time-code"
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
          aria-describedby={[hintId, messageId, describedBy].filter(Boolean).join(' ')}
          aria-invalid={status === 'invalid' || status === 'expired' || status === 'locked' || shownTone === 'error' ? true : undefined}
        />
        <div className="code-boxes" aria-hidden="true">
          {Array.from({ length }, (_, i) => (
            <span key={i} className="code-box" data-filled={chars[i] !== undefined || undefined} data-active={i === active || undefined}>
              {chars[i] ?? ''}
            </span>
          ))}
        </div>
      </div>
      <p className="code-message" id={messageId} data-tone={shownTone} role="status">
        {message ? (
          <>
            <Icon name={shownTone === 'ok' ? 'check' : shownTone === 'error' ? 'alert' : 'info'} />
            <span>{message}</span>
          </>
        ) : null}
      </p>
    </div>
  );
});
