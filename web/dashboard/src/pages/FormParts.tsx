import type { ReactNode } from 'react';
import { Icon } from '../components/Icon';
import { FieldError } from '../components/ui';
import type { MetaRow } from '../lib/formFields';

/** The props a `Field` hands its control. */
type ControlProps = { id: string; 'aria-describedby'?: string; 'aria-invalid'?: true };

/** A USD amount input with its unit. */
export function AmountInput({ p, value, onChange, onBlur, placeholder }: { p: ControlProps; value: string; onChange: (v: string) => void; onBlur?: () => void; placeholder: string }) {
  return (
    <div className="input-wrap">
      <input {...p} className="input input-amount" inputMode="decimal" autoComplete="off" placeholder={placeholder} value={value} onChange={(e) => onChange(e.target.value)} onBlur={onBlur} />
      <span className="unit" aria-hidden="true">
        USD
      </span>
    </div>
  );
}

/** "Fix the 2 highlighted fields to continue." and the server's own refusal, above a form. */
export function FormErrors({ count, serverError }: { count: number; serverError: string | null }) {
  return (
    <>
      {count > 0 ? (
        <div className="callout error-summary" role="alert">
          <Icon name="alert" />
          <p>{count === 1 ? 'Fix the highlighted field to continue.' : `Fix the ${count} highlighted fields to continue.`}</p>
        </div>
      ) : null}
      {serverError ? (
        <div className="callout error-summary" role="alert">
          <Icon name="alert" />
          <p>{serverError}</p>
        </div>
      ) : null}
    </>
  );
}

/** A collapsed "More options" section; it opens on its own when it holds a value or an error. */
export function MoreOptions({ open, onToggle, id, summary, children }: { open: boolean; onToggle: () => void; id: string; summary: string; children: ReactNode }) {
  return (
    <div className="more-options">
      <button type="button" className="disclosure" aria-expanded={open} aria-controls={id} onClick={onToggle}>
        <Icon name="down" />
        <span className="disclosure-title">More options</span>
        <span className="small muted">{summary}</span>
      </button>
      {open ? (
        <div id={id} className="stack more-body">
          {children}
        </div>
      ) : null}
    </div>
  );
}

interface MetadataProps {
  rows: MetaRow[];
  onChange: (rows: MetaRow[]) => void;
  /** The message to show for a key, or undefined while it should stay quiet. */
  show: (key: string) => string | undefined;
  touch: (key: string) => () => void;
  fieldId: (key: string) => string;
  legend?: string;
  hint: ReactNode;
}

/** Key and value rows, with add and remove. */
export function MetadataFields({ rows, onChange, show, touch, fieldId, legend = 'Metadata', hint }: MetadataProps) {
  return (
    <fieldset className="plain stack-md">
      <legend>
        {legend} <span className="optional muted">(optional)</span>
      </legend>
      <p className="hint">{hint}</p>
      {rows.map((row, i) => {
        const kErr = show(`metadata.${i}.key`);
        const vErr = show(`metadata.${i}.value`);
        const kId = fieldId(`metadata.${i}.key`);
        const vId = fieldId(`metadata.${i}.value`);
        const update = (patch: Partial<MetaRow>) => onChange(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
        return (
          <div key={i} className="meta-row">
            <div className="field">
              <label htmlFor={kId} className="sr-only">
                Key {i + 1}
              </label>
              <input
                id={kId}
                className="input mono"
                placeholder="key"
                value={row.key}
                aria-invalid={kErr ? true : undefined}
                aria-describedby={kErr ? `${kId}-error` : undefined}
                onChange={(e) => update({ key: e.target.value })}
                onBlur={touch(`metadata.${i}.key`)}
              />
              <FieldError id={`${kId}-error`} message={kErr} />
            </div>
            <div className="field">
              <label htmlFor={vId} className="sr-only">
                Value {i + 1}
              </label>
              <input
                id={vId}
                className="input mono"
                placeholder="value"
                value={row.value}
                aria-invalid={vErr ? true : undefined}
                aria-describedby={vErr ? `${vId}-error` : undefined}
                onChange={(e) => update({ value: e.target.value })}
                onBlur={touch(`metadata.${i}.value`)}
              />
              <FieldError id={`${vId}-error`} message={vErr} />
            </div>
            <button type="button" className="btn btn-ghost btn-icon" aria-label={`Remove metadata row ${i + 1}`} onClick={() => onChange(rows.filter((_, j) => j !== i))}>
              <Icon name="trash" />
            </button>
          </div>
        );
      })}
      <FieldError id={`${fieldId('metadata')}-error`} message={show('metadata')} />
      <div>
        <button type="button" className="btn btn-small" onClick={() => onChange([...rows, { key: '', value: '' }])}>
          <Icon name="plus" />
          Add metadata
        </button>
      </div>
    </fieldset>
  );
}

/** The form field a server refusal names in `param`, in the drawer's own keys. */
export function fieldOfParam(param: string | undefined, known: string[]): string | null {
  if (!param) return null;
  const sug = /^suggested_amounts\[(\d)\]$/.exec(param);
  if (sug) return `suggested.${sug[1]}`;
  if (param === 'suggested_amounts') return 'suggested.0';
  return known.includes(param) ? param : null;
}
