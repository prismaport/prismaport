import { useState } from 'react';
import { Link as RouterLink, useParams } from 'react-router-dom';
import { Icon } from '../components/Icon';
import { SessionRows } from '../components/SessionRows';
import { useToast } from '../components/Toast';
import { Amounts, CopyValue, ErrorState, Loading, PageHead, Pill } from '../components/ui';
import { formatConversion, formatDateTime, formatRelative, productPrice, usd } from '../lib/format';
import { useProduct, useProductLinks, useProductSessions } from '../lib/queries';
import { productPill } from '../lib/status';
import type { Link, Product } from '../lib/types';
import { DeactivateDialog, QrDialog, useLinkCreatedToast } from './LinkActions';
import { LinkDrawer } from './LinkDrawer';
import { LinkRows } from './Links';
import { DeactivateProductDialog, useProductSavedToast, useSetProductActive } from './ProductActions';
import { ProductDrawer } from './ProductDrawer';
import type { ProductStep } from '../lib/productForm';
import { PayWith } from './ProductNetworks';
import { ProductPayoutCard } from './ProductPayout';

function ProductLinks({ product, onCreate }: { product: Product; onCreate: () => void }) {
  const links = useProductLinks(product.id);
  const toast = useToast();
  const rows = links.data?.pages.flatMap((p) => p.data) ?? [];
  const [qr, setQr] = useState<Link | null>(null);
  const [edit, setEdit] = useState<Link | null>(null);
  const [deactivate, setDeactivate] = useState<Link | null>(null);
  if (links.isPending) return <Loading what="payment links" />;
  if (links.isError) return <ErrorState error={links.error} onRetry={() => links.refetch()} />;
  return (
    <>
      {rows.length === 0 ? (
        <div className="card stack-sm">
          <p className="muted">{product.active ? 'No payment links yet. Create one to start selling this product.' : 'No payment links. Activate the product to create one.'}</p>
          {product.active ? (
            <div>
              <button type="button" className="btn btn-primary" onClick={onCreate}>
                <Icon name="plus" />
                Create payment link
              </button>
            </div>
          ) : null}
        </div>
      ) : (
        <LinkRows links={rows} label={`Payment links of ${product.name}`} showProduct={false} onQr={setQr} onEdit={setEdit} onDeactivate={setDeactivate} />
      )}
      {links.hasNextPage ? (
        <div className="more">
          <button type="button" className="btn" onClick={() => links.fetchNextPage()} disabled={links.isFetchingNextPage}>
            Show more links
          </button>
        </div>
      ) : null}
      {qr ? <QrDialog link={qr} onClose={() => setQr(null)} /> : null}
      {edit ? (
        <LinkDrawer
          link={edit}
          onClose={() => setEdit(null)}
          onSaved={() => {
            setEdit(null);
            toast.show('Changes saved');
          }}
        />
      ) : null}
      {deactivate ? <DeactivateDialog link={deactivate} onClose={() => setDeactivate(null)} /> : null}
    </>
  );
}

function ProductSessions({ id }: { id: string }) {
  const sessions = useProductSessions(id);
  const rows = sessions.data?.pages.flatMap((p) => p.data) ?? [];
  if (sessions.isPending) return <Loading what="payments" />;
  if (sessions.isError) return <ErrorState error={sessions.error} onRetry={() => sessions.refetch()} />;
  if (rows.length === 0) return <p className="card muted">No payments of this product yet. Share one of its links to get paid.</p>;
  return (
    <>
      <SessionRows sessions={rows} show="link" label="Payments of this product" />
      {sessions.hasNextPage ? (
        <div className="more">
          <button type="button" className="btn" onClick={() => sessions.fetchNextPage()} disabled={sessions.isFetchingNextPage}>
            Show more payments
          </button>
        </div>
      ) : null}
    </>
  );
}

export function ProductDetailPage() {
  const { id = '' } = useParams();
  const product = useProduct(id);
  const setActive = useSetProductActive();
  const savedToast = useProductSavedToast();
  const linkToast = useLinkCreatedToast();
  const [editing, setEditing] = useState<ProductStep | null>(null);
  const [creatingLink, setCreatingLink] = useState(false);
  const [deactivating, setDeactivating] = useState(false);

  const back = (
    <RouterLink to="/products" className="back">
      <Icon name="back" />
      Products
    </RouterLink>
  );
  if (product.isPending) return <Loading what="the product" />;
  if (product.isError)
    return (
      <>
        {back}
        <ErrorState error={product.error} onRetry={() => product.refetch()} />
      </>
    );
  const p = product.data;
  const meta = Object.entries(p.metadata ?? {});

  return (
    <>
      {back}
      <PageHead
        title={p.name}
        lead={
          <span className="row">
            <Pill pill={productPill(p)} />
            <span className="amount" data-testid="product-price">
              {productPrice(p)}
            </span>
          </span>
        }
        actions={
          <>
            <button type="button" className="btn btn-primary" onClick={() => setCreatingLink(true)} disabled={!p.active}>
              <Icon name="plus" />
              Create payment link
            </button>
            <button type="button" className="btn" onClick={() => setEditing('details')}>
              <Icon name="edit" />
              Edit
            </button>
            {p.active ? (
              <button type="button" className="btn" onClick={() => setDeactivating(true)}>
                <Icon name="power" />
                Deactivate
              </button>
            ) : (
              <button type="button" className="btn" onClick={() => setActive.mutate({ product: p, active: true })} disabled={setActive.isPending}>
                <Icon name="power" />
                Activate
              </button>
            )}
          </>
        }
      />

      <div className="stack">
        {p.active ? null : (
          <div className="callout" role="note">
            <Icon name="info" />
            <p>This product is inactive. Its links open no new payments and their pages say they are no longer available; your server cannot open new payments for it either. Payments already opened keep working.</p>
          </div>
        )}
        {p.description ? (
          <p className="lead product-description" data-testid="product-description">
            {p.description}
          </p>
        ) : null}

        <section className="stats" aria-label="Product statistics">
          <div className="stat">
            <p className="label">Opened</p>
            <p className="value" data-testid="stat-opened">
              {p.stats.opened.toLocaleString('en-US')}
            </p>
          </div>
          <div className="stat">
            <p className="label">Paid</p>
            <p className="value" data-testid="stat-paid">
              {p.stats.paid.toLocaleString('en-US')}
            </p>
          </div>
          <div className="stat">
            <p className="label">Conversion</p>
            <p className="value">{formatConversion(p.stats.conversion, p.stats.opened, p.stats.paid)}</p>
          </div>
          <div className="stat stat-wide">
            <p className="label">Received</p>
            <div className="value amounts">
              <Amounts items={p.stats.received} />
            </div>
          </div>
          <div className="stat stat-wide">
            <p className="label">Last payment</p>
            <p className="value amounts" title={formatDateTime(p.stats.last_paid_at)}>
              {formatRelative(p.stats.last_paid_at)}
            </p>
          </div>
        </section>

        <section aria-labelledby="links-title" className="stack-md">
          <div className="row between">
            <h2 className="heading" id="links-title">
              Payment links
            </h2>
            {p.links.total > 0 && p.active ? (
              <button type="button" className="btn btn-small" onClick={() => setCreatingLink(true)}>
                <Icon name="plus" />
                Create payment link
              </button>
            ) : null}
          </div>
          <ProductLinks product={p} onCreate={() => setCreatingLink(true)} />
        </section>

        <ProductPayoutCard payout={p.payout} onEdit={() => setEditing('payout')} />

        <section className="card" aria-labelledby="details-title">
          <h2 className="heading" id="details-title">
            Details
          </h2>
          <dl className="details mt-3">
            {p.payment_methods ? (
              <>
                <dt>Payers can pay with</dt>
                <dd>
                  <PayWith product={p} />
                </dd>
              </>
            ) : null}
            <dt>Product ID</dt>
            <dd>
              <CopyValue value={p.id} what="product ID" head={10} tail={4} />
            </dd>
            <dt>Price</dt>
            <dd className="amount">{productPrice(p)}</dd>
            {p.price_type === 'open' && p.suggested_amounts.length > 0 ? (
              <>
                <dt>Suggested amounts</dt>
                <dd className="amount">{p.suggested_amounts.map(usd).join(' · ')}</dd>
              </>
            ) : null}
            <dt>Description</dt>
            <dd>{p.description ? p.description : <span className="muted">None</span>}</dd>
            <dt>Return address</dt>
            <dd>{p.success_url ? <span className="address">{p.success_url}</span> : <span className="muted">None</span>}</dd>
            <dt>Metadata</dt>
            <dd>
              {meta.length === 0 ? (
                <span className="muted">None</span>
              ) : (
                <ul className="list-plain">
                  {meta.map(([k, v]) => (
                    <li key={k} className="address">
                      {k}: {v}
                    </li>
                  ))}
                </ul>
              )}
            </dd>
            <dt>Created</dt>
            <dd>{formatDateTime(p.created_at)}</dd>
            <dt>Last changed</dt>
            <dd>{formatDateTime(p.updated_at)}</dd>
          </dl>
        </section>

        <section aria-labelledby="sessions-title" className="stack-md">
          <div className="row between">
            <h2 className="heading" id="sessions-title">
              Payments of this product
            </h2>
            <RouterLink to={`/payments?product_id=${encodeURIComponent(p.id)}`} className="small">
              Open in Payments
            </RouterLink>
          </div>
          <ProductSessions id={p.id} />
        </section>
      </div>

      {editing ? (
        <ProductDrawer
          product={p}
          initialStep={editing}
          onClose={() => setEditing(null)}
          onSaved={(saved) => {
            setEditing(null);
            savedToast(saved);
          }}
        />
      ) : null}
      {creatingLink ? (
        <LinkDrawer
          productId={p.id}
          lockProduct
          onClose={() => setCreatingLink(false)}
          onSaved={(saved) => {
            setCreatingLink(false);
            linkToast(saved);
          }}
        />
      ) : null}
      {deactivating ? <DeactivateProductDialog product={p} onClose={() => setDeactivating(false)} /> : null}
    </>
  );
}
