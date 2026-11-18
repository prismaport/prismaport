import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { api } from '../lib/endpoints';
import { writeClipboard } from '../lib/clipboard';
import { keys, useMe } from '../lib/queries';
import { recoveryCodesFile } from '../lib/recovery';
import { Dialog } from './Dialog';
import { Icon } from './Icon';
import { SecondFactorField, useSecondFactor } from './SecondFactor';
import { useToast } from './Toast';
import { ErrorState, Field, isApiError } from './ui';

/**
 * The codes, shown once. Closing is impossible until the box says they are saved: there is no
 * way to see them again, only to create new ones.
 */
export function RecoveryCodesDialog({ codes, onDone }: { codes: string[]; onDone: () => void }) {
  const toast = useToast();
  const email = useMe().data?.user.email;
  const [saved, setSaved] = useState(false);
  const copy = async () => toast.show((await writeClipboard(codes.join('\n'))) ? 'Recovery codes copied' : 'Could not copy. Select the codes and copy them.');
  const download = () => {
    const url = URL.createObjectURL(new Blob([recoveryCodesFile(codes, email)], { type: 'text/plain' }));
    const a = document.createElement('a');
    a.href = url;
    a.download = 'prismaport-recovery-codes.txt';
    document.body.append(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  };
  return (
    <Dialog
      title="Save your recovery codes"
      description="Use one if you lose your authenticator app. Each code works once. We show them only now."
      onClose={onDone}
      busy={!saved}
      footer={
        <button type="button" className="btn btn-primary" disabled={!saved} onClick={onDone}>
          Done
        </button>
      }
    >
      <div className="stack-md">
        <ul className="recovery-codes mono" aria-label="Recovery codes">
          {codes.map((c) => (
            <li key={c}>{c}</li>
          ))}
        </ul>
        <div className="row">
          <button type="button" className="btn" onClick={() => void copy()}>
            <Icon name="copy" />
            Copy all
          </button>
          <button type="button" className="btn" onClick={download}>
            <Icon name="download" />
            Download
          </button>
        </div>
        <p className="small muted">Keep them away from your phone, in a password manager or on paper. Anyone with a code and your password can sign in.</p>
        <label className="check">
          <input type="checkbox" checked={saved} onChange={(e) => setSaved(e.target.checked)} />
          <span>I saved these codes</span>
        </label>
      </div>
    </Dialog>
  );
}

/**
 * Creates the codes: a current authenticator code first or, when codes exist, one of them (the
 * way back for a lost phone); never the password or an emailed code. New codes replace every old one.
 */
export function CreateCodesDialog({ replacing, onClose }: { replacing: boolean; onClose: () => void }) {
  const qc = useQueryClient();
  const factor = useSecondFactor();
  const [codes, setCodes] = useState<string[] | null>(null);
  const create = useMutation({
    mutationFn: () => api.totpRecoveryCodes(factor.payload('code')),
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: keys.me });
      setCodes(r.recovery_codes);
    },
    onError: (e) => {
      if (isApiError(e, 'totp_required')) factor.refuse(e.message || 'That code is not valid. Enter the current code from your authenticator app.');
    },
  });
  if (codes) return <RecoveryCodesDialog codes={codes} onDone={onClose} />;
  const fieldError = isApiError(create.error, 'totp_required');
  return (
    <Dialog
      title={replacing ? 'Create new recovery codes' : 'Create recovery codes'}
      description={replacing ? 'Confirm with a current code from your authenticator app or a recovery code. Your old codes stop working.' : 'Confirm with a current code from your authenticator app.'}
      onClose={onClose}
      busy={create.isPending}
    >
      <form
        className="form-grid"
        noValidate
        data-testid="create-codes"
        onSubmit={(e) => {
          e.preventDefault();
          if (factor.check()) create.mutate();
        }}
      >
        <SecondFactorField s={factor} label="Code" autoFocus recovery={replacing} />
        {create.isError && !fieldError ? <ErrorState error={create.error} /> : null}
        <div className="dialog-foot">
          <button type="button" className="btn" onClick={onClose} disabled={create.isPending}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={create.isPending}>
            {replacing ? 'Create new codes' : 'Create codes'}
          </button>
        </div>
      </form>
    </Dialog>
  );
}
