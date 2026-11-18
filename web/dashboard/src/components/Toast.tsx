import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { Icon } from './Icon';

/** One button on a toast, such as "Copy link". */
export interface ToastAction {
  label: string;
  onClick: () => void;
}

interface ToastApi {
  show: (text: string, action?: ToastAction) => void;
}

const ToastContext = createContext<ToastApi>({ show: () => {} });

export function useToast(): ToastApi {
  return useContext(ToastContext);
}

/** Short confirmations ("Link copied"), announced politely and gone after a few seconds. */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<{ id: number; text: string; action?: ToastAction }[]>([]);
  const next = useRef(1);
  const timers = useRef(new Set<ReturnType<typeof setTimeout>>());
  // A toast still on screen when the provider goes away must not update it afterwards.
  useEffect(() => {
    const pending = timers.current;
    return () => {
      pending.forEach(clearTimeout);
      pending.clear();
    };
  }, []);
  const show = useCallback((text: string, action?: ToastAction) => {
    const id = next.current++;
    setItems((xs) => [...xs.slice(-2), { id, text, action }]);
    // A toast with a button stays long enough to reach it.
    const t = setTimeout(() => {
      timers.current.delete(t);
      setItems((xs) => xs.filter((x) => x.id !== id));
    }, action ? 8000 : 3500);
    timers.current.add(t);
  }, []);
  const api = useMemo(() => ({ show }), [show]);
  return (
    <ToastContext.Provider value={api}>
      {children}
      <div className="toasts" role="status" aria-live="polite">
        {items.map((t) => (
          <div key={t.id} className="toast">
            <Icon name="check" />
            <span>{t.text}</span>
            {t.action ? (
              <button type="button" className="btn btn-small toast-action" onClick={t.action.onClick}>
                {t.action.label}
              </button>
            ) : null}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}
