import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useBlocker, type BlockerFunction } from 'react-router-dom';
import { Dialog } from './Dialog';

// Forms with unsaved changes, innermost last. A router takes one blocker, so the shell's
// LeaveGuard reads this list instead of each form blocking on its own.
const dirtyForms: { title: string; released: { current: boolean } }[] = [];

function DiscardDialog({ title, onDiscard, onKeep }: { title: string; onDiscard: () => void; onKeep: () => void }) {
  return (
    <Dialog
      role="alertdialog"
      title={title}
      description="If you close now, your changes are not saved."
      onClose={onKeep}
      footer={
        <>
          <button type="button" className="btn" onClick={onDiscard}>
            Discard
          </button>
          <button type="button" className="btn btn-primary" onClick={onKeep} data-autofocus>
            Keep editing
          </button>
        </>
      }
    >
      {null}
    </Dialog>
  );
}

interface Options<T> {
  /** The values the form opened with. */
  initial: T;
  value: T;
  onClose: () => void;
  /** The question asked before losing changes, such as "Discard this product?". */
  title: string;
}

/**
 * Keeps what was typed in a form drawer. While `value` differs from `initial`, a close (the X,
 * Escape, the backdrop, Cancel) asks first, and so does leaving the page (LeaveGuard). Call
 * `release()` once the form is saved, before handing the result over, so the navigation that
 * follows is not stopped.
 */
export function useDirtyGuard<T>({ initial, value, onClose, title }: Options<T>) {
  const dirty = useMemo(() => JSON.stringify(value) !== JSON.stringify(initial), [value, initial]);
  const [asking, setAsking] = useState(false);
  const released = useRef(false);

  useEffect(() => {
    if (!dirty) return;
    const entry = { title, released };
    dirtyForms.push(entry);
    return () => void dirtyForms.splice(dirtyForms.indexOf(entry), 1);
  }, [dirty, title]);

  return {
    dirty,
    requestClose: () => (dirty && !released.current ? setAsking(true) : onClose()),
    release: () => {
      released.current = true;
    },
    confirm: asking ? <DiscardDialog title={title} onDiscard={onClose} onKeep={() => setAsking(false)} /> : null,
  };
}

/** Asks before an in-app navigation leaves a page whose form has unsaved changes. Rendered once, in the shell. */
export function LeaveGuard() {
  const shouldBlock = useCallback<BlockerFunction>(
    ({ currentLocation, nextLocation }) => currentLocation.pathname !== nextLocation.pathname && dirtyForms.some((f) => !f.released.current),
    [],
  );
  const blocker = useBlocker(shouldBlock);
  if (blocker.state !== 'blocked') return null;
  const top = dirtyForms.filter((f) => !f.released.current).at(-1);
  return <DiscardDialog title={top?.title ?? 'Discard your changes?'} onDiscard={() => blocker.proceed()} onKeep={() => blocker.reset()} />;
}
