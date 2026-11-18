import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { ConfirmDialog } from '../components/Dialog';
import { useToast } from '../components/Toast';
import { ErrorState } from '../components/ui';
import { writeClipboard } from '../lib/clipboard';
import { api } from '../lib/endpoints';
import { errorMessage } from '../lib/errors';
import { plural } from '../lib/format';
import { useMode } from '../lib/mode';
import { invalidateCatalog, keys } from '../lib/queries';
import type { Product } from '../lib/types';
import type { ProductSaved } from './ProductDrawer';

/** Activates or deactivates a product, which starts or stops every link of it. */
export function useSetProductActive() {
  const qc = useQueryClient();
  const toast = useToast();
  const { mode } = useMode();
  return useMutation({
    mutationFn: ({ product, active }: { product: Product; active: boolean }) => api.updateProduct(mode, product.id, { active }),
    onSuccess: (saved, { active }) => {
      qc.setQueryData(keys.product(mode, saved.id), saved);
      invalidateCatalog(qc);
      toast.show(active ? `${saved.name} is active` : `${saved.name} deactivated`);
    },
  });
}

export function DeactivateProductDialog({ product, onClose }: { product: Product; onClose: () => void }) {
  const setActive = useSetProductActive();
  const [error, setError] = useState<unknown>(null);
  const links = product.links.total;
  return (
    <ConfirmDialog
      title={`Deactivate ${product.name}?`}
      confirmLabel="Deactivate product"
      busy={setActive.isPending}
      onClose={onClose}
      onConfirm={() => setActive.mutate({ product, active: false }, { onSuccess: onClose, onError: setError })}
      body={
        <div className="stack-sm">
          <p>
            {links === 0
              ? 'Your server cannot open new payments for it.'
              : `Its ${plural(links, 'payment link')} stop opening new payments and their pages say they are no longer available. Your server cannot open new payments for it either.`}
          </p>
          <p>Payments already opened keep working. You can activate it again later, and its links come back with it.</p>
        </div>
      }
      error={error ? <ErrorState error={error} /> : null}
    />
  );
}

/** What a create or edit of a product says when it is done: with "Copy link" when a link came along. */
export function useProductSavedToast() {
  const toast = useToast();
  return ({ product, created, link, linkError }: ProductSaved) => {
    if (!created) return toast.show('Changes saved');
    if (link) {
      return toast.show('Product and link created', {
        label: 'Copy link',
        onClick: async () => toast.show((await writeClipboard(link.url)) ? 'Link copied' : 'Copy failed. Select the text and copy it yourself.'),
      });
    }
    if (linkError) return toast.show(`${product.name} created, but its link was not: ${errorMessage(linkError)} Create it from the product page.`);
    toast.show(`${product.name} created`);
  };
}
