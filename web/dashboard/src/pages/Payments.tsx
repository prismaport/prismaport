import { useEffect, useMemo, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import { Icon } from '../components/Icon';
import { SessionRows } from '../components/SessionRows';
import { ErrorState, FieldError, Loading, PageHead } from '../components/ui';
import { CLIENT_REFERENCE, CLIENT_REFERENCE_HINT } from '../lib/reference';
import { useMode } from '../lib/mode';
import { linkTitle } from '../lib/format';
import { useLink, useLinks, useSessions } from '../lib/queries';
import { SESSION_PILL } from '../lib/status';
import type { SessionStatus } from '../lib/types';
import { ProductFilter } from './Links';

/** The links to filter by: those of the chosen product, or every link. */
function LinkFilter({ value, productId, onChange }: { value: string; productId: string; onChange: (v: string) => void }) {
  const links = useLinks({ status: 'all', product_id: productId || undefined });
  const chosen = useLink(value);
  const list = links.data?.pages.flatMap((p) => p.data) ?? [];
  const label = (l: (typeof list)[number]) => (productId || !l.name ? linkTitle(l) : `${l.product.name} · ${l.name}`);
  return (
    <div className="field">
      <label htmlFor="link-filter" className="sr-only">
        Payment link
      </label>
      <select id="link-filter" className="select" value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">Every link</option>
        {list.map((l) => (
          <option key={l.id} value={l.id}>
            {label(l)}
          </option>
        ))}
        {value && !list.some((l) => l.id === value) ? <option value={value}>{chosen.data ? label(chosen.data) : value}</option> : null}
      </select>
    </div>
  );
}

export const SEARCH_HINT =
  'Type the start of a session ID or of a transaction hash (8 characters or more), a full deposit address, or an exact amount such as 25.00.';

export function PaymentsPage() {
  const { mode } = useMode();
  const [params, setParams] = useSearchParams();
  const status = params.get('status') ?? '';
  const linkId = params.get('link_id') ?? '';
  const productId = params.get('product_id') ?? '';
  const q = params.get('q') ?? '';
  const ref = params.get('ref') ?? '';
  const [search, setSearch] = useState(q);
  const [refInput, setRefInput] = useState(ref);

  const update = (k: string, v: string) => {
    const next = new URLSearchParams(params);
    if (v) next.set(k, v);
    else next.delete(k);
    setParams(next, { replace: true });
  };

  useEffect(() => {
    const t = setTimeout(() => {
      if (search.trim() !== q) update('q', search.trim());
    }, 250);
    return () => clearTimeout(t);
  }, [search]);

  const refValue = refInput.trim();
  const refError = refValue && !CLIENT_REFERENCE.test(refValue) ? CLIENT_REFERENCE_HINT : undefined;
  useEffect(() => {
    const t = setTimeout(() => {
      const next = refValue && !CLIENT_REFERENCE.test(refValue) ? ref : refValue;
      if (next !== ref) update('ref', next);
    }, 250);
    return () => clearTimeout(t);
  }, [refInput]);

  const filters = useMemo(() => ({ status, product_id: productId, link_id: linkId, q, client_reference_id: ref }), [status, productId, linkId, q, ref]);
  const sessions = useSessions(filters);
  const rows = sessions.data?.pages.flatMap((p) => p.data) ?? [];
  const filtered = Boolean(status || productId || linkId || q || ref);
  const setProduct = (v: string) => {
    const next = new URLSearchParams(params);
    if (v) next.set('product_id', v);
    else next.delete('product_id');
    // A link of another product would match nothing.
    next.delete('link_id');
    setParams(next, { replace: true });
  };

  return (
    <>
      <PageHead title="Payments" lead="Every payment session, with what arrived on which network and the fee split." />
      <div className="toolbar" role="search">
        <div className="field">
          <label htmlFor="status-filter" className="sr-only">
            Status
          </label>
          <select id="status-filter" className="select" value={status} onChange={(e) => update('status', e.target.value)}>
            <option value="">Any status</option>
            {(Object.keys(SESSION_PILL) as SessionStatus[]).map((s) => (
              <option key={s} value={s}>
                {SESSION_PILL[s].label}
              </option>
            ))}
          </select>
        </div>
        <div className="search">
          <Icon name="search" />
          <label htmlFor="payment-search" className="sr-only">
            Search payments
          </label>
          <input
            id="payment-search"
            className="input"
            type="search"
            placeholder="Session ID, transaction hash, address or amount"
            aria-describedby="payment-search-hint"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
        </div>
        <div className="field ref-filter">
          <label htmlFor="ref-filter" className="sr-only">
            Client reference
          </label>
          <input
            id="ref-filter"
            className="input mono"
            type="search"
            placeholder="Client reference"
            aria-describedby={refError ? 'ref-filter-error' : undefined}
            aria-invalid={refError ? true : undefined}
            value={refInput}
            onChange={(e) => setRefInput(e.target.value)}
          />
          <FieldError id="ref-filter-error" message={refError} />
        </div>
        <ProductFilter id="product-filter" value={productId} onChange={setProduct} />
        <LinkFilter value={linkId} productId={productId} onChange={(v) => update('link_id', v)} />
        <p className="hint toolbar-hint" id="payment-search-hint">
          {SEARCH_HINT}
        </p>
      </div>

      {sessions.isPending ? (
        <Loading what="payments" />
      ) : sessions.isError ? (
        <ErrorState error={sessions.error} onRetry={() => sessions.refetch()} />
      ) : rows.length === 0 ? (
        <div className="card empty">
          <p className="subheading">{filtered ? 'No payments match these filters.' : `No payments in ${mode === 'test' ? 'Test' : 'Live'} mode yet.`}</p>
          <p className="muted">{filtered ? 'Try another status or search.' : 'Share a payment link and payments show up here as they arrive.'}</p>
        </div>
      ) : (
        <>
          <SessionRows sessions={rows} label="Payments" />
          {sessions.hasNextPage ? (
            <div className="more">
              <button type="button" className="btn" onClick={() => sessions.fetchNextPage()} disabled={sessions.isFetchingNextPage}>
                {sessions.isFetchingNextPage ? 'Loading more payments' : 'Show more payments'}
              </button>
            </div>
          ) : null}
        </>
      )}
    </>
  );
}
