import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';
import { Dialog } from '../components/Dialog';
import { Icon } from '../components/Icon';
import { useToast } from '../components/Toast';
import { ErrorState, Field } from '../components/ui';
import { api } from '../lib/endpoints';
import { useMode } from '../lib/mode';
import { keys } from '../lib/queries';
import { OPEN_AMOUNT_DEFAULT, simActionInfo, simActionsFor, simAllowed, simAmountError } from '../lib/sandbox';
import { eventLabel } from '../lib/status';
import type { SessionDetail, SimAction } from '../lib/types';

/** Runs one lifecycle step on a Test mode session, through the same path as real payments. */
function SimulateDialog({ session, onClose }: { session: SessionDetail; onClose: () => void }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const toast = useToast();
  const reversible = session.payments.some((p) => p.simulated && (p.status === 'detected' || p.status === 'confirmed'));
  const openAmount = session.amount === null;
  const actions = simActionsFor(openAmount);
  const firstAllowed = actions.find((a) => simAllowed(a, session.status, reversible))?.action ?? 'under_review';
  const [action, setAction] = useState<SimAction>(firstAllowed);
  const [amount, setAmount] = useState('');
  const [submitted, setSubmitted] = useState(false);
  const info = simActionInfo(action, openAmount);
  const asset = session.pay_to[0]?.asset ?? 'USDC';
  const amountError = submitted && info.amount ? simAmountError(amount) : undefined;
  const run = useMutation({
    mutationFn: () => {
      const typed = info.amount ? amount.trim() || (openAmount ? OPEN_AMOUNT_DEFAULT : '') : '';
      return api.simulate(mode, session.id, { action, ...(typed ? { amount: typed } : {}) });
    },
    onSuccess: (res) => {
      for (const k of [keys.session(mode, session.id), keys.sessions(mode), keys.overview(mode), keys.inboxRequests(mode)]) void qc.invalidateQueries({ queryKey: k });
      if (session.link_id) void qc.invalidateQueries({ queryKey: keys.link(mode, session.link_id) });
      if (session.product_id) void qc.invalidateQueries({ queryKey: keys.product(mode, session.product_id) });
      toast.show(res.events.length > 0 ? `Simulated: ${res.events.map((e) => eventLabel(e.type)).join(', ')}` : 'Simulated');
      onClose();
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    if (info.amount && simAmountError(amount)) return;
    run.mutate();
  };
  return (
    <Dialog
      title="Simulate"
      onClose={onClose}
      busy={run.isPending}
      description="Moves this Test mode session as a real payment would. Your webhook endpoints get the same events production sends."
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={run.isPending}>
            Cancel
          </button>
          <button type="submit" form="simulate-form" className="btn btn-primary" disabled={run.isPending || !simAllowed(info, session.status, reversible)}>
            {run.isPending ? <span className="spinner" aria-hidden="true" /> : null}
            Run simulation
          </button>
        </>
      }
    >
      <form id="simulate-form" className="form-grid mt-4" onSubmit={submit} noValidate>
        <fieldset className="plain">
          <legend>What happens</legend>
          <div className="stack-sm">
            {actions.map((a) => {
              const allowed = simAllowed(a, session.status, reversible);
              return (
                <label key={a.action} className="check sim-option" data-disabled={allowed ? undefined : 'true'}>
                  <input type="radio" name="sim-action" value={a.action} checked={action === a.action} disabled={!allowed} onChange={() => setAction(a.action)} />
                  <span className="stack-sm">
                    <span className="subheading block">{a.label}</span>
                    <span className="small muted block">
                      {allowed ? a.description : a.action === 'reverse_last_payment' && a.from.includes(session.status) ? 'No simulated payment to reverse.' : 'Not possible in the current state.'}
                    </span>
                    <span className="small address muted block">{a.events.join(', ')}</span>
                  </span>
                </label>
              );
            })}
          </div>
        </fieldset>
        {info.amount ? (
          <Field label={`Amount in ${asset}`} optional hint="The synthetic payment itself, in the token. Empty uses the default above." error={amountError}>
            {(p) => <input {...p} className="input mono" inputMode="decimal" placeholder={openAmount ? OPEN_AMOUNT_DEFAULT : '10.50'} value={amount} onChange={(e) => setAmount(e.target.value)} />}
          </Field>
        ) : null}
        <p className="small muted">
          <Icon name="info" /> Simulated payments are marked as such, have synthetic transaction hashes and never move funds.
        </p>
        {run.isError ? <ErrorState error={run.error} /> : null}
      </form>
    </Dialog>
  );
}

export function SimulateButton({ session }: { session: SessionDetail }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button" className="btn" onClick={() => setOpen(true)}>
        <Icon name="code" />
        Simulate
      </button>
      {open ? <SimulateDialog session={session} onClose={() => setOpen(false)} /> : null}
    </>
  );
}
