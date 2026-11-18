import { useEffect, useMemo, useState } from 'react';
import { Link as RouterLink, useSearchParams } from 'react-router-dom';
import { Icon } from '../components/Icon';
import { Amounts, ErrorState, Loading, PageHead, Pill } from '../components/ui';
import { ADDRESS_PILL, canRotate, rotationAdvised } from '../lib/customers';
import { formatDateTime, formatRelative } from '../lib/format';
import { useMode } from '../lib/mode';
import { useCustomerAddresses } from '../lib/queries';
import { CLIENT_REFERENCE } from '../lib/reference';
import { sessionPill } from '../lib/status';
import type { CustomerAddress } from '../lib/types';
import { RotateDialog } from './RotateDialog';

function CustomerRow({ ca, onRotate }: { ca: CustomerAddress; onRotate: () => void }) {
  const last = ca.stats.last_session;
  const advised = rotationAdvised(ca);
  return (
    <li className="data-row customer-row" data-testid="customer-row" data-address-id={ca.id}>
      <div className="cell c-name">
        <RouterLink to={`/customers/${ca.id}`} className="row-link address" data-testid="customer-id">
          {ca.customer_id}
        </RouterLink>
        <div className="meta">
          {ca.product ? (
            <RouterLink to={`/products/${ca.product.id}`} className="small" data-testid="customer-product">
              {ca.product.name}
            </RouterLink>
          ) : null}
          {ca.attention.length > 0 ? (
            <span className="tag with-logo" data-testid="customer-attention">
              <Icon name="alert" />
              {advised ? 'Rotate advised' : 'Needs attention'}
            </span>
          ) : null}
        </div>
      </div>
      <div className="cell c-status">
        <span className="cell-label">Status</span>
        <Pill pill={ADDRESS_PILL[ca.status]} />
      </div>
      <div className="c-stats">
        <div className="cell">
          <span className="cell-label">Last payment</span>
          {last ? (
            <span className="small row">
              <RouterLink to={`/payments/${last.id}`} title={formatDateTime(last.created_at)}>
                {formatRelative(last.created_at)}
              </RouterLink>
              <Pill pill={sessionPill(last.status)} />
            </span>
          ) : (
            <span className="small muted">None yet</span>
          )}
        </div>
        <div className="cell c-received">
          <span className="cell-label">Total received</span>
          <Amounts items={ca.stats.received} />
        </div>
        <div className="cell">
          <span className="cell-label">Payments</span>
          <span className="mono num small">{ca.stats.sessions.toLocaleString('en-US')}</span>
          {advised && canRotate(ca) ? (
            <button type="button" className="btn btn-small customer-rotate" onClick={onRotate} aria-label={`Rotate the address of ${ca.customer_id}`}>
              <Icon name="rotate" />
              Rotate
            </button>
          ) : null}
        </div>
      </div>
    </li>
  );
}

export function CustomersPage() {
  const { mode } = useMode();
  const [params, setParams] = useSearchParams();
  const customer = params.get('customer_id') ?? '';
  const [search, setSearch] = useState(customer);
  const [rotating, setRotating] = useState<CustomerAddress | null>(null);
  const typed = search.trim();
  const invalid = typed !== '' && !CLIENT_REFERENCE.test(typed);

  useEffect(() => {
    const t = setTimeout(() => {
      if (invalid || typed === customer) return;
      const next = new URLSearchParams(params);
      if (typed) next.set('customer_id', typed);
      else next.delete('customer_id');
      setParams(next, { replace: true });
    }, 250);
    return () => clearTimeout(t);
  }, [search]);

  const filters = useMemo(() => ({ customer_id: customer || undefined }), [customer]);
  const list = useCustomerAddresses(filters);
  const rows = list.data?.pages.flatMap((p) => p.data) ?? [];

  return (
    <>
      <PageHead title="Customers" lead="Each customer pays to the same deposit address every time. It is watched only while a payment on it is open." />

      <div className="toolbar" role="search">
        <div className="search">
          <Icon name="search" />
          <label htmlFor="customer-search" className="sr-only">
            Find a customer by ID
          </label>
          <input
            id="customer-search"
            className="input"
            type="search"
            placeholder="Exact customer ID, such as user_1042"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            aria-invalid={invalid || undefined}
            aria-describedby={invalid ? 'customer-search-error' : undefined}
          />
        </div>
        {invalid ? (
          <p className="small" id="customer-search-error" role="alert">
            Customer IDs use letters, digits, &apos;.&apos;, &apos;_&apos;, &apos;:&apos; and &apos;-&apos;.
          </p>
        ) : null}
      </div>

      {list.isPending ? (
        <Loading what="customers" />
      ) : list.isError ? (
        <ErrorState error={list.error} onRetry={() => list.refetch()} />
      ) : rows.length === 0 && !customer ? (
        <section className="card empty" aria-labelledby="empty-title">
          <h2 className="display" id="empty-title">
            No customer addresses yet
          </h2>
          <p className="muted">
            Your server creates one per customer with <span className="address">POST /v1/customer_addresses</span>. The customer can then pay to the same address every time, from its page.
          </p>
          <p className="actions empty-actions">
            <RouterLink to="/developers/api-keys" className="btn">
              Get an API key
            </RouterLink>
          </p>
        </section>
      ) : rows.length === 0 ? (
        <div className="card empty">
          <p className="subheading">No customer with this ID.</p>
          <p className="muted">{mode === 'test' ? 'You are in Test mode: live customers are not shown.' : 'The ID must match exactly.'}</p>
          <p className="actions empty-actions">
            <button
              type="button"
              className="btn"
              onClick={() => {
                setSearch('');
                setParams(new URLSearchParams(), { replace: true });
              }}
            >
              Show every customer
            </button>
          </p>
        </div>
      ) : (
        <>
          <ul className="rows" aria-label="Customers">
            <li className="rows-head customers label" aria-hidden="true">
              <span>Customer</span>
              <span>Status</span>
              <span>Last payment</span>
              <span>Total received</span>
              <span>Payments</span>
            </li>
            {rows.map((ca) => (
              <CustomerRow key={ca.id} ca={ca} onRotate={() => setRotating(ca)} />
            ))}
          </ul>
          {list.hasNextPage ? (
            <div className="more">
              <button type="button" className="btn" onClick={() => list.fetchNextPage()} disabled={list.isFetchingNextPage}>
                {list.isFetchingNextPage ? 'Loading more customers' : 'Show more customers'}
              </button>
            </div>
          ) : null}
        </>
      )}
      {rotating ? <RotateDialog address={rotating} onClose={() => setRotating(null)} /> : null}
    </>
  );
}
