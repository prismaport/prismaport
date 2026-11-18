import { useEffect, useId, useRef, useState, type ReactNode } from 'react';
import { ApiError } from '../lib/api';
import { writeClipboard } from '../lib/clipboard';
import { displayAmount } from '../lib/format';
import { truncateMiddle } from '../lib/money';
import type { Pill as PillModel } from '../lib/status';
import type { AssetAmount } from '../lib/types';
import { errorMessage } from '../lib/errors';
import { Icon } from './Icon';
import { TokenAmount } from './Logos';
import { useToast } from './Toast';

export function Pill({ pill }: { pill: PillModel }) {
  return (
    <span className="pill" data-tone={pill.tone}>
      {pill.label}
    </span>
  );
}

/**
 * A middle-truncated id, address or hash; the full value goes to assistive tech and the tooltip.
 * The hidden full value is left out of text selections, so copying a sentence that holds one
 * gives the short form once instead of both glued together.
 */
export function ShortValue({ value, head = 4, tail = 4, className = 'address' }: { value: string; head?: number; tail?: number; className?: string }) {
  return (
    <span className={className} title={value}>
      <span aria-hidden="true">{truncateMiddle(value, head, tail)}</span>
      <span className="sr-only no-copy">{value}</span>
    </span>
  );
}

/** Copies the full value, whatever is displayed next to it. */
export function CopyButton({ value, what, label = 'Copy', small = true, iconOnly = false, className = '', ariaLabel }: { value: string; what: string; label?: string; small?: boolean; iconOnly?: boolean; className?: string; ariaLabel?: string }) {
  const toast = useToast();
  const [state, setState] = useState<'idle' | 'done' | 'failed'>('idle');
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const onClick = async () => {
    const ok = await writeClipboard(value);
    setState(ok ? 'done' : 'failed');
    const cap = what.charAt(0).toUpperCase() + what.slice(1);
    toast.show(ok ? `${cap} copied` : 'Copy failed. Select the text and copy it yourself.');
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setState('idle'), 1500);
  };
  const text = state === 'done' ? 'Copied' : state === 'failed' ? 'Copy failed' : label;
  return (
    <button type="button" className={`btn ${small ? 'btn-small' : ''} ${className}`} onClick={onClick} aria-label={ariaLabel ?? `Copy ${what}`} title={iconOnly ? `Copy ${what}` : undefined}>
      <Icon name={state === 'done' ? 'check' : 'copy'} />
      {iconOnly ? null : <span className="action-text">{text}</span>}
    </button>
  );
}

/** A short value with a copy button for the full one. */
export function CopyValue({ value, what, head, tail }: { value: string; what: string; head?: number; tail?: number }) {
  return (
    <span className="inline-copy">
      <ShortValue value={value} head={head} tail={tail} />
      <CopyButton value={value} what={what} iconOnly className="btn-ghost btn-icon" />
    </span>
  );
}

/** A full value in a bordered field with a copy button: share URLs and secrets. `openLabel` adds a button that opens the URL in a new tab. */
export function CopyField({ value, what, id, openLabel }: { value: string; what: string; id?: string; openLabel?: string }) {
  return (
    <div className="copyable">
      <span className="address value" id={id}>
        {value}
      </span>
      <span className="copyable-actions">
        {openLabel && (
          <a className="btn btn-small btn-icon" href={value} target="_blank" rel="noopener noreferrer" aria-label={openLabel} title={openLabel}>
            <Icon name="external" />
          </a>
        )}
        <CopyButton value={value} what={what} />
      </span>
    </div>
  );
}

export function Amounts({ items, empty = 'None yet' }: { items: AssetAmount[]; empty?: string }) {
  const shown = items.filter((r) => /[1-9]/.test(r.amount));
  if (shown.length === 0) return <span className="muted">{empty}</span>;
  return (
    <ul className="amount-list">
      {shown.map((r) => (
        <li key={r.asset} className="amount">
          <TokenAmount asset={r.asset}>
            {displayAmount(r.amount)} {r.asset}
          </TokenAmount>
        </li>
      ))}
    </ul>
  );
}

export function Loading({ what }: { what: string }) {
  return (
    <p className="loading" role="status">
      Loading {what}.
    </p>
  );
}

export function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  return (
    <div className="callout error-summary" role="alert">
      <Icon name="alert" />
      <div className="stack-sm">
        <p>{errorMessage(error)}</p>
        {onRetry ? (
          <button type="button" className="btn btn-small" onClick={onRetry}>
            Try again
          </button>
        ) : null}
      </div>
    </div>
  );
}

export function FieldError({ id, message }: { id: string; message?: string }) {
  if (!message) return null;
  return (
    <p className="error" id={id}>
      <Icon name="alert" />
      <span>{message}</span>
    </p>
  );
}

interface FieldProps {
  label: ReactNode;
  hint?: ReactNode;
  error?: string;
  optional?: boolean;
  children: (props: { id: string; 'aria-describedby'?: string; 'aria-invalid'?: true }) => ReactNode;
  id?: string;
}

/** A labelled control with its help and error wired through aria-describedby. */
export function Field({ label, hint, error, optional, children, id: given }: FieldProps) {
  const auto = useId();
  const id = given ?? auto;
  const describedBy = [hint ? `${id}-hint` : null, error ? `${id}-error` : null].filter(Boolean).join(' ') || undefined;
  return (
    <div className="field">
      <label htmlFor={id}>
        {label}
        {optional ? <span className="optional"> (optional)</span> : null}
      </label>
      {children({ id, 'aria-describedby': describedBy, ...(error ? { 'aria-invalid': true as const } : {}) })}
      {hint ? (
        <p className="hint" id={`${id}-hint`}>
          {hint}
        </p>
      ) : null}
      <FieldError id={`${id}-error`} message={error} />
    </div>
  );
}

export interface SegOption<T extends string> {
  value: T;
  label: string;
}

/** A radio group drawn as a segmented control. */
export function Segmented<T extends string>({ legend, value, options, onChange, name, className = '' }: { legend: string; value: T; options: SegOption<T>[]; onChange: (v: T) => void; name?: string; className?: string }) {
  const auto = useId();
  return (
    <fieldset className={`segmented ${className}`}>
      <legend className="sr-only">{legend}</legend>
      {options.map((o) => (
        <label key={o.value}>
          <input type="radio" name={name ?? auto} value={o.value} checked={value === o.value} onChange={() => onChange(o.value)} />
          {o.label}
        </label>
      ))}
    </fieldset>
  );
}

export function isApiError(e: unknown, code?: string): e is ApiError {
  return e instanceof ApiError && (code === undefined || e.code === code);
}

/** `status` sits on the right, level with the bottom of the title block, under any actions. */
export function PageHead({ title, lead, actions, status }: { title: string; lead?: ReactNode; actions?: ReactNode; status?: ReactNode }) {
  useEffect(() => {
    document.title = `${title} · Prismaport`;
  }, [title]);
  const side = actions ? <div className="actions">{actions}</div> : null;
  return (
    <div className="page-head">
      <div>
        <h1 className="display">{title}</h1>
        {lead ? <p className="lead">{lead}</p> : null}
      </div>
      {status ? (
        <div className="page-head-side">
          {side}
          {status}
        </div>
      ) : (
        side
      )}
    </div>
  );
}

/** Which mode an object belongs to, next to its state: Test on test data, Live on real payments. */
export function ModeTag({ livemode }: { livemode: boolean }) {
  return livemode ? (
    <span className="tag tag-live" data-testid="mode-tag">
      Live
    </span>
  ) : (
    <span className="tag" data-testid="mode-tag">
      Test
    </span>
  );
}

/**
 * Says the page refreshes by itself, with the age of what it shows. The seconds are left out of
 * the live region so a screen reader is not interrupted every second.
 */
export function UpdatingLive({ updatedAt }: { updatedAt: number }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);
  const seconds = Math.max(0, Math.floor((now - updatedAt) / 1000));
  return (
    <p className="updating-live small muted" role="status" data-testid="updating-live">
      <span className="updating-live-dot" aria-hidden="true" />
      <span>
        Updating live<span aria-hidden="true"> · checked {seconds} s ago</span>
      </span>
    </p>
  );
}
