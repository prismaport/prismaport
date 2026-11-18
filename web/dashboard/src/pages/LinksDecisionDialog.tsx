import { useQuery } from '@tanstack/react-query';
import { useState, type ReactNode } from 'react';
import { Dialog } from '../components/Dialog';
import { Icon } from '../components/Icon';
import { api } from '../lib/endpoints';
import { useMode } from '../lib/mode';
import { decisionCopy, linksDialogTitle, openCheckoutsNote, ownAmountNote } from '../lib/productLinks';
import { keys } from '../lib/queries';
import type { LinksDecision, Product } from '../lib/types';

interface Props {
  product: Product;
  /** The active links the server counted, when it answered links_decision_required; else the live ones are counted. */
  count: number | null;
  busy: boolean;
  error?: ReactNode;
  onConfirm: (choice: LinksDecision) => void;
  onClose: () => void;
}

const CHOICES: LinksDecision[] = ['keep', 'deactivate'];

/** Before a price or payout change reaches a product's links: keep them active, or turn them off. */
export function LinksDecisionDialog({ product, count, busy, error, onConfirm, onClose }: Props) {
  const { mode } = useMode();
  const [choice, setChoice] = useState<LinksDecision>('keep');
  const n = count ?? (product.links.live > 0 ? product.links.live : null);
  // Only a link of a variable price can set its own amount.
  const live = useQuery({
    queryKey: [...keys.productLinks(mode, product.id), 'live'],
    queryFn: () => api.productLinks(mode, product.id, null, { status: 'live', limit: 100 }),
    enabled: product.price_type === 'open',
  });
  const own = live.data ? ownAmountNote(live.data.data, product.name) : null;
  const openNote = openCheckoutsNote(product.stats.open ?? 0);

  return (
    <Dialog
      title={linksDialogTitle(n)}
      onClose={onClose}
      busy={busy}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button type="button" className="btn btn-primary" onClick={() => onConfirm(choice)} disabled={busy}>
            {busy ? <span className="spinner" aria-hidden="true" /> : null}
            {decisionCopy(choice, n).confirm}
          </button>
        </>
      }
    >
      <div className="stack-md">
        <fieldset className="plain">
          <legend className="sr-only">Active links of {product.name}</legend>
          <div className="choice-list">
            {CHOICES.map((c) => {
              const copy = decisionCopy(c, n);
              return (
                <label key={c} className="kind-option">
                  <input type="radio" name="links-decision" value={c} checked={choice === c} onChange={() => setChoice(c)} data-autofocus={choice === c || undefined} />
                  <span className="kind-title">{copy.title}</span>
                  <span className="small muted">{copy.description}</span>
                  {c === 'keep' && own ? <span className="small muted" data-testid="own-amount-links">{own}</span> : null}
                </label>
              );
            })}
          </div>
        </fieldset>
        {openNote ? (
          <p className="note" data-testid="open-checkouts">
            <Icon name="info" />
            <span>{openNote}</span>
          </p>
        ) : null}
        {error}
      </div>
    </Dialog>
  );
}
