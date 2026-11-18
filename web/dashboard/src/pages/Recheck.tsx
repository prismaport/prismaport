import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Link as RouterLink } from 'react-router-dom';
import { Icon } from '../components/Icon';
import { ShortValue } from '../components/ui';
import { api } from '../lib/endpoints';
import { useMode } from '../lib/mode';
import { keys } from '../lib/queries';
import { RECHECK_COPY, canRecheck, recheckError, supersededBy } from '../lib/recheck';
import type { SessionDetail } from '../lib/types';

/**
 * "Check again" for a session past its watch window: funds sent to the deposit address later are
 * credited only through this action. Idle, pending after the 202, or an error message.
 */
export function RecheckPanel({ session }: { session: SessionDetail }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const run = useMutation({
    mutationFn: () => api.recheckSession(mode, session.id),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.session(mode, session.id) });
    },
  });
  const newer = supersededBy(session);
  // A superseded session's recheck_available is false whatever its watch: past the watch window it
  // says why (the newer session watches the address) instead of hiding the panel.
  if (!canRecheck(newer ? { ...session, recheck_available: undefined } : session, Date.now())) return null;
  if (newer)
    return (
      <section className="callout" data-testid="recheck-superseded" aria-label="Check again">
        <Icon name="info" />
        <div className="stack-sm">
          <p>Check again is not available here. {RECHECK_COPY.superseded}</p>
          <p>
            <RouterLink to={`/payments/${newer}`} data-testid="recheck-newer">
              Open the newer payment <ShortValue value={newer} head={8} tail={4} />
            </RouterLink>
          </p>
        </div>
      </section>
    );
  const pending = run.isSuccess;
  return (
    <section className="callout" data-testid="recheck" aria-label="Check again">
      <Icon name="info" />
      <div className="stack-sm">
        <p>
          {session.status === 'paid' || session.status === 'overpaid'
            ? RECHECK_COPY.paid
            : 'This payment is past its watch window, so Prismaport no longer looks for funds on its own. If someone sent money to its deposit address later, check again to credit it.'}
        </p>
        <div className="row">
          <button type="button" className="btn" data-testid="recheck-button" disabled={run.isPending || pending} onClick={() => run.mutate()}>
            {run.isPending ? <span className="spinner" aria-hidden="true" /> : null}
            Check again
          </button>
          {pending ? (
            <span role="status" data-testid="recheck-pending">
              {RECHECK_COPY.pending}
            </span>
          ) : null}
        </div>
        {run.isError ? (
          <p role="alert" data-testid="recheck-error">
            {recheckError(run.error)}
          </p>
        ) : null}
      </div>
    </section>
  );
}
