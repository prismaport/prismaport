import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { ConfirmDialog } from '../components/Dialog';
import { useToast } from '../components/Toast';
import { ErrorState } from '../components/ui';
import { api } from '../lib/endpoints';
import { useMode } from '../lib/mode';
import { keys } from '../lib/queries';
import type { CustomerAddress } from '../lib/types';

/** Rotates an active customer address and refreshes every page that shows it. */
function useRotate() {
  const qc = useQueryClient();
  const { mode } = useMode();
  return useMutation({
    mutationFn: (ca: CustomerAddress) => api.rotateCustomerAddress(mode, ca.id),
    onSuccess: (next) => {
      qc.setQueryData(keys.customer(mode, next.id), next);
      void qc.invalidateQueries({ queryKey: keys.customers(mode) });
      void qc.invalidateQueries({ queryKey: ['customer', mode] });
    },
  });
}

/**
 * The confirm step before a rotation: what changes for the customer and what happens to the old
 * address. No step-up: like a product payout change, it only points the customer's next payments
 * at the product's current payout, which pays the merchant's own wallets.
 */
export function RotateDialog({ address, onClose, onRotated }: { address: CustomerAddress; onClose: () => void; onRotated?: (next: CustomerAddress) => void }) {
  const rotate = useRotate();
  const toast = useToast();
  const [error, setError] = useState<unknown>(null);
  const product = address.product?.name ?? 'its product';
  return (
    <ConfirmDialog
      title={`Rotate the address of ${address.customer_id}?`}
      confirmLabel="Rotate address"
      busy={rotate.isPending}
      onClose={onClose}
      onConfirm={() =>
        rotate.mutate(address, {
          onSuccess: (next) => {
            toast.show(`${address.customer_id} has a new address`);
            onClose();
            onRotated?.(next);
          },
          onError: setError,
        })
      }
      body={
        <div className="stack-sm" data-testid="rotate-body">
          <p>
            {address.customer_id} gets a new deposit address that pays where {product} pays now, with the current fee.
          </p>
          <p>
            The old address stops being watched. A payment already open on it finishes as usual, but no new payment opens there, and money sent to it later is not credited. Its
            page tells payers the address changed and links the new one.
          </p>
          <p className="muted">Give your customer the new address.</p>
        </div>
      }
      error={error ? <ErrorState error={error} /> : null}
    />
  );
}
