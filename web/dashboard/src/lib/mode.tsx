import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from 'react';
import type { Mode } from './types';

const KEY = 'prismaport.dashboard.mode';

function readMode(): Mode {
  try {
    return localStorage.getItem(KEY) === 'test' ? 'test' : 'live';
  } catch {
    return 'live';
  }
}

interface ModeApi {
  mode: Mode;
  setMode: (m: Mode) => void;
}

const ModeContext = createContext<ModeApi>({ mode: 'live', setMode: () => {} });

export function ModeProvider({ children, initial }: { children: ReactNode; initial?: Mode }) {
  const [mode, set] = useState<Mode>(() => initial ?? readMode());
  const setMode = useCallback((m: Mode) => {
    set(m);
    try {
      localStorage.setItem(KEY, m);
    } catch {
      // Private windows may refuse storage; the switch still works for this tab.
    }
  }, []);
  const api = useMemo(() => ({ mode, setMode }), [mode, setMode]);
  return <ModeContext.Provider value={api}>{children}</ModeContext.Provider>;
}

export function useMode(): ModeApi {
  return useContext(ModeContext);
}
