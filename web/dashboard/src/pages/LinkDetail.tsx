import { useState } from 'react';
import { Link as RouterLink, useParams } from 'react-router-dom';
import { Icon } from '../components/Icon';
import { QrCode, QrDownloads } from '../components/QrCode';
import { SessionRows } from '../components/SessionRows';
import { useToast } from '../components/Toast';
import { Amounts, CopyButton, CopyField, CopyValue, ErrorState, Loading, PageHead, Pill } from '../components/ui';
import { formatConversion, formatDateTime, formatRelative, linkPrice, linkTitle, plural, productPrice } from '../lib/format';
import { useLink, useLinkSessions, useProduct } from '../lib/queries';
import { linkPill, linkState } from '../lib/status';
import type { Link } from '../lib/types';
import { DeactivateDialog, useSetActive } from './LinkActions';
import { LinkDrawer } from './LinkDrawer';
import { PayoutLines } from './ProductPayout';

function unavailableCopy(l: Link): string | null {
  switch (linkState(l)) {
    case 'live':
      return null;
    case 'product_inactive':
      return `Its product, ${l.product.name}, is inactive, so this link opens no new payments and the payment page says it is no longer available. Activate the product to bring the link back. Payments already opened keep working.`;
    case 'inactive':
      return 'This link is inactive. It opens no new payments and the payment page says it is no longer available. Payments already opened keep working.';
    case 'expired':
      return `This link stopped accepting payments on ${formatDateTime(l.expires_at)}. Edit it to set a later time.`;
    case 'exhausted':
      return `This link reached its limit of ${plural(l.max_payments ?? 0, 'paid payment')}. Edit it to raise or remove the limit.`;
    case 'reserved':
      return `Payments in progress hold the last of its ${plural(l.max_payments ?? 0, 'payment')}. It opens again if one of them expires unpaid; edit it to raise the limit.`;
  }
}

/** The product a link sells: its price and where its money goes, read from the product. */
function LinkProductCard({ link }: { link: Link }) {
  const product = useProduct(link.product_id);
  return (
    <section className="card stack-md" aria-labelledby="product-title" data-testid="link-product-card">
      <div className="row between">
        <div>
          <h2 className="heading" id="product-title">
            Product
          </h2>
          <p className="small muted">The link sells this product. Its price and where its money goes are set on the product, for every link of it.</p>
        </div>
        <RouterLink to={`/products/${link.product_id}`} className="btn btn-small">
          Open product
        </RouterLink>
      </div>
      <dl className="details">
        <dt>Product</dt>
        <dd>
          <RouterLink to={`/products/${link.product_id}`} data-testid="link-product">
            {link.product.name}
          </RouterLink>
        </dd>
        <dt>Product price</dt>
        <dd className="amount">{productPrice(link.product)}</dd>
        <dt>Where the money goes</dt>
        <dd>
          {product.data ? (
            <PayoutLines payout={product.data.payout} />
          ) : product.isError ? (
            <ErrorState error={product.error} onRetry={() => product.refetch()} />
          ) : (
            <span className="muted">Loading.</span>
          )}
        </dd>
      </dl>
    </section>
  );
}

function LinkSessions({ id }: { id: string }) {
  const sessions = useLinkSessions(id);
  const rows = sessions.data?.pages.flatMap((p) => p.data) ?? [];
  if (sessions.isPending) return <Loading what="payments" />;
  if (sessions.isError) return <ErrorState error={sessions.error} onRetry={() => sessions.refetch()} />;
  if (rows.length === 0) return <p className="card muted">No payments opened from this link yet. Share it to get paid.</p>;
  return (
    <>
      <SessionRows sessions={rows} show="session" label="Payments opened from this link" />
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

export function LinkDetailPage() {
  const { id = '' } = useParams();
  const link = useLink(id);
  const toast = useToast();
  const setActive = useSetActive();
  const [editing, setEditing] = useState(false);
  const [deactivating, setDeactivating] = useState(false);

  if (link.isPending) return <Loading what="the link" />;
  if (link.isError)
    return (
      <>
        <RouterLink to="/links" className="back">
          <Icon name="back" />
          Payment links
        </RouterLink>
        <ErrorState error={link.error} onRetry={() => link.refetch()} />
      </>
    );
  const l = link.data;
  const title = linkTitle(l);
  const note = unavailableCopy(l);
  const meta = Object.entries(l.metadata ?? {});

  return (
    <>
      <RouterLink to="/links" className="back">
        <Icon name="back" />
        Payment links
      </RouterLink>
      <PageHead
        title={title}
        lead={
          <span className="row">
            <Pill pill={linkPill(l)} />
            {l.name ? (
              <RouterLink to={`/products/${l.product_id}`} data-testid="link-head-product">
                {l.product.name}
              </RouterLink>
            ) : null}
            <span className="amount" data-testid="link-price">
              {linkPrice(l)}
            </span>
          </span>
        }
        actions={
          <>
            <CopyButton value={l.url} what="link" label="Copy link" small={false} />
            <button type="button" className="btn" onClick={() => setEditing(true)}>
              <Icon name="edit" />
              Edit
            </button>
            {l.active ? (
              <button type="button" className="btn" onClick={() => setDeactivating(true)}>
                <Icon name="power" />
                Deactivate
              </button>
            ) : (
              <button type="button" className="btn" onClick={() => setActive.mutate({ link: l, active: true })} disabled={setActive.isPending}>
                <Icon name="power" />
                Activate
              </button>
            )}
          </>
        }
      />

      <div className="stack">
        {note ? (
          <div className="callout" role="note">
            <Icon name="info" />
            <p>{note}</p>
          </div>
        ) : null}

        <section className="stats" aria-label="Link statistics">
          <div className="stat">
            <p className="label">Opened</p>
            <p className="value" data-testid="stat-opened">
              {l.stats.opened.toLocaleString('en-US')}
            </p>
          </div>
          <div className="stat">
            <p className="label">Paid</p>
            <p className="value" data-testid="stat-paid">
              {l.stats.paid.toLocaleString('en-US')}
            </p>
          </div>
          <div className="stat">
            <p className="label">Conversion</p>
            <p className="value">{formatConversion(l.stats.conversion, l.stats.opened, l.stats.paid)}</p>
          </div>
          <div className="stat stat-wide">
            <p className="label">Received</p>
            <div className="value amounts">
              <Amounts items={l.stats.received} />
            </div>
          </div>
          <div className="stat stat-wide">
            <p className="label">Last payment</p>
            <p className="value amounts" title={formatDateTime(l.stats.last_paid_at)}>
              {formatRelative(l.stats.last_paid_at)}
            </p>
          </div>
        </section>

        <section className="card" aria-labelledby="share-title">
          <h2 className="heading" id="share-title">
            Share
          </h2>
          <div className="share mt-4">
            <QrCode text={l.url} label={`QR code of the payment link for ${title}`} />
            <div className="stack-md">
              <div className="field">
                <span className="field-label" id="share-url-label">
                  Link address
                </span>
                <CopyField value={l.url} what="link address" />
              </div>
              <p className="small muted">Payers open it, choose a network and stablecoin, and pay. Each opening starts its own payment.</p>
              <QrDownloads text={l.url} name={title} />
              <p>
                <a href={l.url} target="_blank" rel="noreferrer" className="row" onClick={() => toast.show('Opened the payment page in a new tab')}>
                  <Icon name="external" />
                  Open the payment page
                </a>
              </p>
            </div>
          </div>
        </section>

        <LinkProductCard link={l} />

        <section className="card" aria-labelledby="details-title">
          <h2 className="heading" id="details-title">
            Details
          </h2>
          <dl className="details mt-3">
            <dt>Link ID</dt>
            <dd>
              <CopyValue value={l.id} what="link ID" head={10} tail={4} />
            </dd>
            <dt>Label</dt>
            <dd>{l.name ?? <span className="muted">None: this dashboard shows the product name</span>}</dd>
            <dt>Price the payer sees</dt>
            <dd className="amount">
              {linkPrice(l)}
              {l.amount ? <span className="small muted"> (set on this link)</span> : l.product.price_type === 'open' ? <span className="small muted"> (the payer chooses)</span> : null}
            </dd>
            <dt>Return address</dt>
            <dd>{l.success_url ? <span className="address">{l.success_url}</span> : <span className="muted">The product&apos;s</span>}</dd>
            <dt>Stops accepting payments</dt>
            <dd>{l.expires_at ? formatDateTime(l.expires_at) : <span className="muted">Never</span>}</dd>
            <dt>Payment limit</dt>
            <dd>{l.max_payments ? `${plural(l.max_payments, 'paid payment')} (${l.stats.paid.toLocaleString('en-US')} so far)` : <span className="muted">No limit</span>}</dd>
            <dt>Extra metadata</dt>
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
            <dd>{formatDateTime(l.created_at)}</dd>
            <dt>Last changed</dt>
            <dd>{formatDateTime(l.updated_at)}</dd>
          </dl>
        </section>

        <section aria-labelledby="sessions-title" className="stack-md">
          <div className="row between">
            <h2 className="heading" id="sessions-title">
              Payments from this link
            </h2>
            <RouterLink to={`/payments?link_id=${encodeURIComponent(l.id)}`} className="small">
              Open in Payments
            </RouterLink>
          </div>
          <LinkSessions id={l.id} />
        </section>
      </div>

      {editing ? (
        <LinkDrawer
          link={l}
          onClose={() => setEditing(false)}
          onSaved={() => {
            setEditing(false);
            toast.show('Changes saved');
          }}
        />
      ) : null}
      {deactivating ? <DeactivateDialog link={l} onClose={() => setDeactivating(false)} /> : null}
    </>
  );
}
