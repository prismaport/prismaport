import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { ConfirmDialog, Dialog } from '../components/Dialog';
import { QrCode, QrDownloads } from '../components/QrCode';
import { useToast } from '../components/Toast';
import { CopyField, ErrorState } from '../components/ui';
import { writeClipboard } from '../lib/clipboard';
import { api } from '../lib/endpoints';
import { linkPrice, linkTitle } from '../lib/format';
import { useMode } from '../lib/mode';
import { invalidateCatalog, keys } from '../lib/queries';
import { linkState } from '../lib/status';
import type { Link } from '../lib/types';

export function QrDialog({ link, onClose }: { link: Link; onClose: () => void }) {
  const title = linkTitle(link);
  return (
    <Dialog title={`QR code for ${title}`} onClose={onClose} description="Payers scan it to open the payment page. It is made in your browser.">
      <div className="stack-md">
        <div className="qr-head">
          <QrCode text={link.url} label={`QR code of the payment link for ${title}`} large />
          <p className="small muted">
            {link.product.name} · {linkPrice(link)}
          </p>
        </div>
        <CopyField value={link.url} what="link" openLabel="Open the payment page" />
        <QrDownloads text={link.url} name={title} />
      </div>
    </Dialog>
  );
}

const NOT_LIVE: Record<string, string> = {
  product_inactive: 'its product is inactive',
  expired: 'it expired',
  exhausted: 'it reached its payment limit',
  reserved: 'payments in progress hold its last slots',
};

/** Activates or deactivates a link and refreshes every list that shows it. */
export function useSetActive() {
  const qc = useQueryClient();
  const toast = useToast();
  const { mode } = useMode();
  return useMutation({
    mutationFn: ({ link, active }: { link: Link; active: boolean }) => api.updateLink(mode, link.id, { active }),
    onSuccess: (saved, { active }) => {
      qc.setQueryData(keys.link(mode, saved.id), saved);
      invalidateCatalog(qc);
      const title = linkTitle(saved);
      if (!active) toast.show(`${title} deactivated`);
      else if (saved.live) toast.show(`${title} is live`);
      else toast.show(`${title} activated, but it is not live: ${NOT_LIVE[linkState(saved)] ?? 'it cannot open payments now'}`);
    },
  });
}

export function DeactivateDialog({ link, onClose }: { link: Link; onClose: () => void }) {
  const setActive = useSetActive();
  const [error, setError] = useState<unknown>(null);
  const title = linkTitle(link);
  return (
    <ConfirmDialog
      title={`Deactivate ${title}?`}
      confirmLabel="Deactivate link"
      busy={setActive.isPending}
      onClose={onClose}
      onConfirm={() =>
        setActive.mutate(
          { link, active: false },
          {
            onSuccess: onClose,
            onError: setError,
          },
        )
      }
      body={
        <p>
          The link stops opening new payments and the payment page says it is no longer available. Payments already opened from it keep working. The product and its
          other links are not changed. You can activate it again later.
        </p>
      }
      error={error ? <ErrorState error={error} /> : null}
    />
  );
}

/** "Link created" with a button that copies it. */
export function useLinkCreatedToast() {
  const toast = useToast();
  return (link: Link) =>
    toast.show(`${linkTitle(link)} link created`, {
      label: 'Copy link',
      onClick: async () => toast.show((await writeClipboard(link.url)) ? 'Link copied' : 'Copy failed. Select the text and copy it yourself.'),
    });
}
