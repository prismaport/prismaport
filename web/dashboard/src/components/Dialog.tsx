import { useEffect, useId, useRef, type ReactNode, type RefObject } from 'react';
import { createPortal } from 'react-dom';
import { Icon } from './Icon';

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

// Open dialogs, innermost last: a dialog opened over another one takes the keys
// (Escape, the Tab trap) until it closes.
const open: number[] = [];
let seq = 0;
// The page's own overflow, put back when the last dialog closes: a drawer and the dialog over it
// can close together, in either order.
let pageOverflow = '';

interface Props {
  title: string;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  variant?: 'dialog' | 'drawer';
  /** Closing is blocked while a request is in flight. */
  busy?: boolean;
  description?: string;
  bodyClassName?: string;
  /** Drawers only: `narrow` for a single-column form. */
  size?: 'wide' | 'narrow';
  /** `alertdialog` for a question that interrupts, such as discarding changes. */
  role?: 'dialog' | 'alertdialog';
}

interface ModalOptions {
  onClose: () => void;
  /** Escape does nothing while true. */
  busy?: boolean;
  /** Where focus goes back on close; by default the element focused when the modal opened. */
  restoreTo?: RefObject<HTMLElement | null>;
}

/**
 * The keyboard side of a modal, for as long as the calling component is mounted: focus moves into
 * the panel and is trapped, Escape closes, focus returns on close, and the page behind does not
 * scroll.
 */
export function useModal(panel: RefObject<HTMLElement | null>, { onClose, busy = false, restoreTo }: ModalOptions): void {
  const closeRef = useRef(onClose);
  closeRef.current = onClose;
  const busyRef = useRef(busy);
  busyRef.current = busy;
  const restoreRef = useRef(restoreTo);
  restoreRef.current = restoreTo;

  useEffect(() => {
    const self = ++seq;
    if (open.length === 0) pageOverflow = document.body.style.overflow;
    open.push(self);
    const opener = document.activeElement as HTMLElement | null;
    const el = panel.current;
    const first = el?.querySelector<HTMLElement>('[data-autofocus]') ?? el?.querySelector<HTMLElement>(FOCUSABLE);
    (first ?? el)?.focus();
    document.body.style.overflow = 'hidden';
    const onKey = (e: KeyboardEvent) => {
      if (open[open.length - 1] !== self) return;
      if (e.key === 'Escape' && !busyRef.current) {
        e.stopPropagation();
        closeRef.current();
        return;
      }
      if (e.key !== 'Tab' || !el) return;
      const items = [...el.querySelectorAll<HTMLElement>(FOCUSABLE)].filter((x) => x.offsetParent !== null || x === document.activeElement);
      if (items.length === 0) return;
      const firstEl = items[0]!;
      const lastEl = items[items.length - 1]!;
      if (e.shiftKey && document.activeElement === firstEl) {
        e.preventDefault();
        lastEl.focus();
      } else if (!e.shiftKey && document.activeElement === lastEl) {
        e.preventDefault();
        firstEl.focus();
      }
    };
    document.addEventListener('keydown', onKey);
    return () => {
      open.splice(open.indexOf(self), 1);
      document.removeEventListener('keydown', onKey);
      if (open.length === 0) document.body.style.overflow = pageOverflow;
      const back = restoreRef.current?.current ?? opener;
      if (back && back.isConnected) back.focus();
    };
  }, [panel]);
}

/** A modal dialog or side drawer (see useModal for the keyboard behaviour). */
export function Dialog({ title, onClose, children, footer, variant = 'dialog', busy = false, description, bodyClassName, size = 'wide', role = 'dialog' }: Props) {
  const titleId = useId();
  const descId = useId();
  const panel = useRef<HTMLDivElement>(null);
  useModal(panel, { onClose, busy });

  const head = (
    <div className={variant === 'drawer' ? 'drawer-head' : 'dialog-head'}>
      <h2 className="heading" id={titleId}>
        {title}
      </h2>
      <button type="button" className="btn btn-ghost btn-icon" onClick={() => !busy && onClose()} aria-label="Close" disabled={busy}>
        <Icon name="x" size={20} />
      </button>
    </div>
  );

  const content =
    variant === 'drawer' ? (
      <div className="overlay drawer-overlay" onMouseDown={(e) => e.target === e.currentTarget && !busy && onClose()}>
        <div className="drawer" data-size={size} role={role} aria-modal="true" aria-labelledby={titleId} aria-describedby={description ? descId : undefined} ref={panel} tabIndex={-1}>
          {head}
          {description ? (
            <p id={descId} className="sr-only">
              {description}
            </p>
          ) : null}
          <div className={`drawer-body ${bodyClassName ?? ''}`}>{children}</div>
          {footer ? <div className="drawer-foot">{footer}</div> : null}
        </div>
      </div>
    ) : (
      <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && !busy && onClose()}>
        <div className="dialog" role={role} aria-modal="true" aria-labelledby={titleId} aria-describedby={description ? descId : undefined} ref={panel} tabIndex={-1}>
          {head}
          {description ? (
            <p id={descId} className="muted">
              {description}
            </p>
          ) : null}
          <div className={bodyClassName}>{children}</div>
          {footer ? <div className="dialog-foot">{footer}</div> : null}
        </div>
      </div>
    );
  return createPortal(content, document.body);
}

/** A confirmation with one action: the consequence is written out, not implied. */
export function ConfirmDialog({ title, body, confirmLabel, onConfirm, onClose, busy, error }: { title: string; body: ReactNode; confirmLabel: string; onConfirm: () => void; onClose: () => void; busy?: boolean; error?: ReactNode }) {
  return (
    <Dialog
      title={title}
      onClose={onClose}
      busy={busy}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button type="button" className="btn btn-primary" onClick={onConfirm} disabled={busy} data-autofocus>
            {busy ? <span className="spinner" aria-hidden="true" /> : null}
            {confirmLabel}
          </button>
        </>
      }
    >
      <div className="stack-md">
        <div>{body}</div>
        {error}
      </div>
    </Dialog>
  );
}
