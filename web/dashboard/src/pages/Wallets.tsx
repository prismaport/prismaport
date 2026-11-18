import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { Link as RouterLink } from 'react-router-dom';
import { useLiveConfirmation } from '../components/Confirmation';
import { Dialog } from '../components/Dialog';
import { Icon } from '../components/Icon';
import { useToast } from '../components/Toast';
import { CopyValue, ShortValue } from '../components/ui';
import { ApiError } from '../lib/api';
import { api } from '../lib/endpoints';
import { errorMessage } from '../lib/errors';
import { formatDate, plural } from '../lib/format';
import { useMode } from '../lib/mode';
import { invalidateOnboarding, keys } from '../lib/queries';
import type { Wallet } from '../lib/types';
import { isActive } from '../lib/wallets';

/** How many products pay a wallet; an older API that does not count them says nothing. */
const productsUsing = (w: Wallet): number => w.used_by_products ?? 0;

/** "This wallet still receives payments from 2 products. Change those products first." */
function inUseMessage(n: number): string {
  return n === 1 ? 'This wallet still receives payments from 1 product. Change that product first.' : `This wallet still receives payments from ${n} products. Change those products first.`;
}

export function ArchiveDialog({ wallet, onClose }: { wallet: Wallet; onClose: () => void }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const toast = useToast();
  const confirmation = useLiveConfirmation('wallet_archive');
  const [serverError, setServerError] = useState<string | null>(null);
  // The server may know of a product the list did not count yet: its refusal turns this into the same explanation.
  const [refused, setRefused] = useState<string | null>(null);
  const using = productsUsing(wallet);
  const archive = useMutation({
    mutationFn: () => api.archiveWallet(mode, wallet.id, confirmation.payload()),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.wallets(mode) });
      invalidateOnboarding(qc);
      confirmation.done();
      toast.show(`${wallet.label} archived`);
      onClose();
    },
    onError: (e) => {
      if (e instanceof ApiError && e.code === 'wallet_in_use') {
        void qc.invalidateQueries({ queryKey: keys.wallets(mode) });
        setRefused(e.message || inUseMessage(Math.max(using, 1)));
      } else if (!confirmation.absorbs(e)) setServerError(errorMessage(e));
      else confirmation.onError(e);
    },
  });

  if (using > 0 || refused) {
    return (
      <Dialog
        title={`${wallet.label} still receives payments`}
        onClose={onClose}
        footer={
          <>
            <button type="button" className="btn" onClick={onClose}>
              Close
            </button>
            <RouterLink to="/products" className="btn btn-primary" onClick={onClose}>
              Go to products
            </RouterLink>
          </>
        }
      >
        <div className="stack-md" data-testid="archive-blocked">
          <p>{refused ?? inUseMessage(using)}</p>
          <p className="muted">A wallet cannot be archived while a product pays it. In each of those products, choose other wallets under “Where the money goes”, then archive it.</p>
        </div>
      </Dialog>
    );
  }

  return (
    <Dialog
      title={`Archive ${wallet.label}?`}
      onClose={onClose}
      busy={archive.isPending}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={archive.isPending}>
            Cancel
          </button>
          <button type="submit" form="archive-form" className="btn btn-primary" disabled={archive.isPending}>
            {archive.isPending ? <span className="spinner" aria-hidden="true" /> : null}
            Archive wallet
          </button>
        </>
      }
    >
      <form
        id="archive-form"
        className="stack-md"
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          setServerError(null);
          confirmation.run(() => archive.mutate());
        }}
      >
        <p>No product pays it, and it can no longer be chosen for one. Payments already opened keep the wallets they started with. It stays listed under archived wallets.</p>
        <p className="small muted">
          <ShortValue value={wallet.address} head={6} tail={6} />
        </p>
        {serverError ? (
          <div className="callout error-summary" role="alert">
            <Icon name="alert" />
            <p>{serverError}</p>
          </div>
        ) : null}
        {confirmation.fields('archive-step', 'Archiving a wallet')}
      </form>
    </Dialog>
  );
}

/** In Test mode a wallet the network could not check yet is kept; the row says what that means. */
const UNCHECKED_TEST_WALLET = 'Not checked on the network yet. Real test payouts may wait until it is.';

export function WalletRow({ wallet, onArchive, onMakeDefault, busy = false }: { wallet: Wallet; onArchive: () => void; onMakeDefault: () => void; busy?: boolean }) {
  const { mode } = useMode();
  const using = productsUsing(wallet);
  const archived = !isActive(wallet);
  const check = wallet.verified ? 'Checked on the network' : mode === 'test' ? UNCHECKED_TEST_WALLET : 'Not checked on the network yet';
  return (
    <li className="wallet-row" data-testid="wallet" data-archived={archived || undefined} data-default={wallet.default || undefined}>
      <div className="wallet-head">
        <p className="subheading wallet-name">
          <span>{wallet.label}</span>
          {archived ? <span className="tag">Archived</span> : wallet.default ? <span className="tag tag-live">Default</span> : null}
        </p>
        {archived ? (
          <span className="small muted">Archived {formatDate(wallet.archived_at)}</span>
        ) : (
          <span className="actions">
            {wallet.default ? (
              <span className="small muted">New products pay here</span>
            ) : (
              <button type="button" className="btn btn-small" onClick={onMakeDefault} disabled={busy} aria-label={`Make ${wallet.label} the default`}>
                Make default
              </button>
            )}
            <button type="button" className="btn btn-small" onClick={onArchive} aria-label={`Archive ${wallet.label}`}>
              Archive
            </button>
          </span>
        )}
      </div>
      <CopyValue value={wallet.address} what={`${wallet.label} address`} head={4} tail={4} />
      <p className="small muted wallet-meta">
        <span className="with-icon" data-testid="wallet-check">
          <Icon name={wallet.verified ? 'check' : 'info'} />
          {check}
        </span>
        <span data-testid="wallet-usage">{using === 0 ? 'Not used by any product' : `Used by ${plural(using, 'product')}`}</span>
      </p>
    </li>
  );
}
