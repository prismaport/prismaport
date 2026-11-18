import { useState } from 'react';
import { Link as RouterLink, useNavigate, useParams } from 'react-router-dom';
import { Icon } from '../components/Icon';
import { AssetOnNetwork, NetworkLogo, TestnetTag } from '../components/Logos';
import { QrCode } from '../components/QrCode';
import { SessionRows } from '../components/SessionRows';
import { useToast } from '../components/Toast';
import { Amounts, CopyButton, CopyField, CopyValue, ErrorState, Loading, ModeTag, PageHead, Pill, ShortValue } from '../components/ui';
import { ADDRESS_PILL, attentionLine, canRotate } from '../lib/customers';
import { formatDateTime, formatRelative, networkName } from '../lib/format';
import { useCustomerAddress, useCustomerAddressSessions } from '../lib/queries';
import type { CustomerAddress } from '../lib/types';
import { RotateDialog } from './RotateDialog';

function AddressSessions({ id }: { id: string }) {
  const sessions = useCustomerAddressSessions(id);
  const rows = sessions.data?.pages.flatMap((p) => p.data) ?? [];
  if (sessions.isPending) return <Loading what="payments" />;
  if (sessions.isError) return <ErrorState error={sessions.error} onRetry={() => sessions.refetch()} />;
  if (rows.length === 0) return <p className="card muted">No payments on this address yet. One opens when the customer presses Pay on its page, or when your server opens one.</p>;
  return (
    <>
      <SessionRows sessions={rows} show="session" label="Payments on this address" />
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

/** The wallets and shares the address committed to when it was created, per network. */
function AddressPayout({ ca }: { ca: CustomerAddress }) {
  const chains = Object.keys(ca.payout).filter((c) => (ca.payout[c]?.length ?? 0) > 0);
  if (chains.length === 0) return <span className="muted">None</span>;
  return (
    <ul className="list-plain stack-sm" data-testid="address-payout">
      {chains.map((c) => (
        <li key={c} className="row">
          <span className="tag with-logo network-badge">
            <NetworkLogo chain={c} size={16} decorative tag={false} />
            <span>{networkName(c)}</span>
          </span>
          <span className="small">{ca.payout[c]!.map((r) => `${r.label ?? 'Unnamed wallet'} ${r.share ?? `${(r.bps / 100).toFixed(2)}`}%`).join(', ')}</span>
        </li>
      ))}
    </ul>
  );
}

export function CustomerDetailPage() {
  const { id = '' } = useParams();
  const q = useCustomerAddress(id);
  const navigate = useNavigate();
  const toast = useToast();
  const [rotating, setRotating] = useState(false);

  const back = (
    <RouterLink to="/customers" className="back">
      <Icon name="back" />
      Customers
    </RouterLink>
  );
  if (q.isPending) return <Loading what="the customer" />;
  if (q.isError)
    return (
      <>
        {back}
        <ErrorState error={q.error} onRetry={() => q.refetch()} />
      </>
    );
  const ca = q.data;
  const rotatable = canRotate(ca);
  const meta = Object.entries(ca.metadata ?? {});

  return (
    <>
      {back}
      <PageHead
        title={ca.customer_id}
        lead={
          <span className="row">
            <Pill pill={ADDRESS_PILL[ca.status]} />
            <ModeTag livemode={ca.livemode} />
            {ca.product ? (
              <RouterLink to={`/products/${ca.product.id}`} data-testid="customer-head-product">
                {ca.product.name}
              </RouterLink>
            ) : null}
            <CopyValue value={ca.id} what="customer address ID" head={10} tail={4} />
          </span>
        }
        actions={
          <>
            <CopyButton value={ca.url} what="address page link" label="Copy page link" small={false} />
            {ca.status === 'active' ? (
              <button type="button" className="btn" onClick={() => setRotating(true)} disabled={!rotatable} data-testid="rotate-button">
                <Icon name="rotate" />
                Rotate
              </button>
            ) : null}
          </>
        }
      />

      <div className="stack">
        {ca.status === 'rotated' ? (
          <div className="callout" role="note" data-testid="address-rotated">
            <Icon name="info" />
            <p>
              Rotated {ca.rotated_at ? formatDateTime(ca.rotated_at) : ''}. No new payment opens here and money sent to it is not credited; payments that were already open
              finish as usual.{' '}
              {ca.replaced_by ? (
                <>
                  The customer&apos;s address is now{' '}
                  <RouterLink to={`/customers/${ca.replaced_by}`} data-testid="replaced-by">
                    <ShortValue value={ca.replaced_by} head={10} tail={4} />
                  </RouterLink>
                  .
                </>
              ) : null}
            </p>
          </div>
        ) : null}

        {ca.status === 'active' && ca.attention.length > 0 ? (
          <section className="callout" aria-labelledby="attention-title" data-testid="address-attention">
            <Icon name="alert" />
            <div className="stack-sm">
              <h2 className="subheading" id="attention-title">
                {rotatable ? 'This address is out of date' : 'This address cannot take new payments'}
              </h2>
              <ul className="list-plain stack-sm">
                {ca.attention.map((a) => (
                  <li key={a.reason} data-reason={a.reason}>
                    {attentionLine(a)}
                  </li>
                ))}
              </ul>
              {rotatable ? (
                <>
                  <p className="small muted">Rotating gives the customer a new address with your current settings.</p>
                  <p>
                    <button type="button" className="btn" onClick={() => setRotating(true)} data-testid="attention-rotate">
                      <Icon name="rotate" />
                      Rotate address
                    </button>
                  </p>
                </>
              ) : null}
            </div>
          </section>
        ) : null}

        <section className="stats" aria-label="Customer statistics">
          <div className="stat">
            <p className="label">Payments</p>
            <p className="value" data-testid="stat-sessions">
              {ca.stats.sessions.toLocaleString('en-US')}
            </p>
          </div>
          <div className="stat">
            <p className="label">Paid</p>
            <p className="value" data-testid="stat-paid">
              {ca.stats.paid.toLocaleString('en-US')}
            </p>
          </div>
          <div className="stat stat-wide">
            <p className="label">Total received</p>
            <div className="value amounts" data-testid="stat-received">
              <Amounts items={ca.stats.received} />
            </div>
          </div>
          <div className="stat stat-wide">
            <p className="label">Last payment</p>
            <p className="value amounts" title={formatDateTime(ca.stats.last_session?.created_at)}>
              {ca.stats.last_session ? (
                <RouterLink to={`/payments/${ca.stats.last_session.id}`}>{formatRelative(ca.stats.last_session.created_at)}</RouterLink>
              ) : (
                'None yet'
              )}
            </p>
          </div>
        </section>

        <section className="card" aria-labelledby="deposit-title">
          <h2 className="heading" id="deposit-title">
            Deposit addresses
          </h2>
          <p className="small muted mt-3">
            The same for every payment of this customer. Prismaport watches them only while a payment is open on this address, and for about a day after it ends.
          </p>
          <ul className="list-plain mt-3">
            {ca.pay_to.map((p) => (
              <li key={`${p.chain}:${p.asset}`} className="payto-row" data-testid="address-pay-to">
                <AssetOnNetwork asset={p.asset} chain={p.chain} />
                <TestnetTag chain={p.chain} />
                <CopyValue value={p.address} what={`${networkName(p.chain)} address`} />
              </li>
            ))}
          </ul>
        </section>

        <section className="card" aria-labelledby="page-title">
          <h2 className="heading" id="page-title">
            Address page
          </h2>
          <div className="share mt-4">
            <QrCode text={ca.url} label={`QR code of the address page of ${ca.customer_id}`} />
            <div className="stack-md">
              <div className="field">
                <span className="field-label">Page link</span>
                <CopyField value={ca.url} what="address page link" />
              </div>
              <p className="small muted">The customer opens it, sees the address and presses Pay to start a payment. {ca.status === 'rotated' ? 'It now says the address changed.' : ''}</p>
              <p>
                <a href={ca.url} target="_blank" rel="noreferrer" className="row" data-testid="address-page-link" onClick={() => toast.show('Opened the address page in a new tab')}>
                  <Icon name="external" />
                  Open the address page
                </a>
              </p>
            </div>
          </div>
        </section>

        <section className="card" aria-labelledby="details-title">
          <h2 className="heading" id="details-title">
            Details
          </h2>
          <dl className="details mt-3">
            <dt>Customer ID</dt>
            <dd>
              <CopyValue value={ca.customer_id} what="customer ID" head={64} tail={64} />
            </dd>
            <dt>Address ID</dt>
            <dd>
              <CopyValue value={ca.id} what="customer address ID" head={10} tail={4} />
            </dd>
            <dt>Product</dt>
            <dd>{ca.product ? <RouterLink to={`/products/${ca.product.id}`}>{ca.product.name}</RouterLink> : <span className="muted">{ca.product_id}</span>}</dd>
            <dt>Where the money goes</dt>
            <dd>
              <AddressPayout ca={ca} />
              <p className="small muted">Set when the address was created. Rotate it to use the product&apos;s payout now.</p>
            </dd>
            <dt>Created</dt>
            <dd>{formatDateTime(ca.created_at)}</dd>
            {ca.rotated_at ? (
              <>
                <dt>Rotated</dt>
                <dd>{formatDateTime(ca.rotated_at)}</dd>
              </>
            ) : null}
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
          </dl>
        </section>

        <section aria-labelledby="sessions-title" className="stack-md">
          <h2 className="heading" id="sessions-title">
            Payments on this address
          </h2>
          <AddressSessions id={ca.id} />
        </section>
      </div>

      {rotating ? <RotateDialog address={ca} onClose={() => setRotating(false)} onRotated={(next) => navigate(`/customers/${next.id}`)} /> : null}
    </>
  );
}
