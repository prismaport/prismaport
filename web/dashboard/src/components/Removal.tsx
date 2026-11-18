import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../lib/endpoints';
import { keys, useMe } from '../lib/queries';
import { formatUtc } from '../lib/recovery';
import { useToast } from './Toast';
import { ErrorState } from './ui';

/** Cancels the pending removal of two-factor authentication, from inside the dashboard. */
export function useCancelRemoval() {
  const qc = useQueryClient();
  const toast = useToast();
  return useMutation({
    mutationFn: () => api.totpRemovalCancel(),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.me });
      toast.show('Removal cancelled. Two-factor authentication stays on.');
    },
  });
}

/** On every page while a lost-app request is waiting: when 2FA goes, and the way to stop it. */
export function RemovalStrip() {
  const at = useMe().data?.user.totp_removal_at;
  const cancel = useCancelRemoval();
  if (!at) return null;
  return (
    <div className="review-strip" role="note" data-testid="removal-strip">
      <span>Two-factor authentication will be removed on {formatUtc(at)}, after a recovery request.</span>
      <button type="button" className="btn btn-small" disabled={cancel.isPending} onClick={() => cancel.mutate()}>
        Cancel removal
      </button>
      {cancel.isError ? <ErrorState error={cancel.error} /> : null}
    </div>
  );
}
