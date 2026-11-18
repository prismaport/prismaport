import { Link as RouterLink, useParams } from 'react-router-dom';
import { Icon } from '../components/Icon';
import { AssetBadge, AssetOnNetwork, NetworkLogo, TestnetTag, TokenAmount } from '../components/Logos';
import { linkLabel, sessionSource } from '../components/SessionRows';
import { CopyValue, ErrorState, Loading, ModeTag, PageHead, Pill, ShortValue, UpdatingLive } from '../components/ui';
import { displayAmount, explorerUrl, formatDateTime, networkName, sessionAmount, tokenAmount } from '../lib/format';
import { addAmounts, compareAmounts, formatExact, scaled } from '../lib/money';
import { useSession } from '../lib/queries';
import { referenceNote } from '../lib/reference';
import { eventLabel, PAYMENT_PILL, sessionInProgress, sessionPill } from '../lib/status';
import type { ExplorerLink, Payment, Payout, SessionDetail, Settlement, SettledWith } from '../lib/types';
import { SimulateButton } from './Simulate';
import { RecheckPanel } from './Recheck';

/** The server's explorer link for a reference when it sent one, otherwise the known explorer. */
function explorerFor(s: SessionDetail, chain: string, ref: string, explicit?: string): string | null {
  if (explicit) return explicit;
  const fromServer = s.explorer_links?.find((x: ExplorerLink) => x.value === ref && x.chain === chain && x.type !== 'address');
  if (fromServer) return fromServer.url;
  return explorerUrl(chain, 'tx', ref);
}

/** The fee as a share of what arrived, to two decimals of a percent, computed on exact units. */
export function feePercent(s: Pick<Settlement, 'gross' | 'fee'>): string | null {
  try {
    const gross = scaled(s.gross);
    if (gross === 0n) return null;
    const hundredths = (scaled(s.fee) * 10000n + gross / 2n) / gross;
    return `${hundredths / 100n}.${String(hundredths % 100n).padStart(2, '0')}%`;
  } catch {
    return null;
  }
}

/** fee + sum(payouts) against gross, in the token's decimals; null when there are no payouts. */
export function settlementCheck(s: Pick<Settlement, 'gross' | 'fee' | 'payouts'>): { ok: boolean; paid: string; total: string } | null {
  if (!s.payouts || s.payouts.length === 0) return null;
  const decimals = (s.gross.split('.')[1] ?? '').length;
  try {
    const paid = addAmounts(...s.payouts.map((p) => p.amount));
    const total = addAmounts(s.fee, paid);
    return { ok: compareAmounts(total, s.gross) === 0, paid: formatExact(paid, decimals), total: formatExact(total, decimals) };
  } catch {
    return { ok: false, paid: '', total: '' };
  }
}

function PayoutRow({ p, chain, asset }: { p: Payout; chain: string; asset: string }) {
  const label = p.label ?? 'Unnamed wallet';
  return (
    <li className="payout-row" data-testid="payout">
      <span className="payout-who">
        <span className="subheading">{label}</span>
        <span className="tag with-logo network-badge">
          <NetworkLogo chain={chain} size={16} decorative tag={false} />
          <span>{networkName(chain)}</span>
        </span>
        <TestnetTag chain={chain} />
      </span>
      <span className="payout-address">
        <CopyValue value={p.address} what={`${label} address`} />
      </span>
      <span className="amount payout-amount">
        <TokenAmount asset={asset}>
          {p.amount} {asset}
        </TokenAmount>
      </span>
    </li>
  );
}

function SettlementCard({ session, s }: { session: SessionDetail; s: Settlement }) {
  // A simulated payment's hash is synthetic: no explorer has it.
  const simulated = Boolean(s.simulated) || session.payments.some((p) => p.simulated && p.tx_hash === s.tx_hash);
  const url = simulated ? null : explorerFor(session, s.chain, s.tx_hash, s.explorer_url);
  const pct = feePercent(s);
  const check = settlementCheck(s);
  return (
    <div className="card stack-md" data-testid="settlement">
      <div className="row between">
        <h3 className="subheading with-logo">
          <NetworkLogo chain={s.chain} decorative tag={false} />
          <span>Settled on {networkName(s.chain)}</span>
          <TestnetTag chain={s.chain} />
          {simulated ? <span className="tag">Simulated</span> : null}
        </h3>
        <span className="small muted">{formatDateTime(s.settled_at)}</span>
      </div>
      <dl className="total-lines">
        <dt>Received</dt>
        <dd className="amount">
          <TokenAmount asset={s.asset}>
            {s.gross} {s.asset}
          </TokenAmount>
        </dd>
        <dt>Prismaport fee{pct ? ` (${pct})` : ''}</dt>
        <dd className="amount">
          <TokenAmount asset={s.asset}>
            {s.fee} {s.asset}
          </TokenAmount>
        </dd>
        {check ? null : (
          <>
            <dt className="subheading">Net to your wallet</dt>
            <dd className="amount subheading">
              <TokenAmount asset={s.asset}>
                {s.net} {s.asset}
              </TokenAmount>
            </dd>
          </>
        )}
      </dl>
      {check ? (
        <div className="stack-sm">
          <p className="label">Paid to your wallets</p>
          <ul className="list-plain payout-rows" aria-label={`Payouts on ${networkName(s.chain)}`}>
            {s.payouts!.map((p, i) => (
              <PayoutRow key={`${p.wallet_id}-${i}`} p={p} chain={s.chain} asset={s.asset} />
            ))}
          </ul>
          <p className="small check-line" data-testid="settlement-check" data-ok={check.ok || undefined}>
            <Icon name={check.ok ? 'check' : 'alert'} />
            {check.ok ? (
              <span>
                Fee <span className="mono">{s.fee}</span> + payouts <span className="mono">{check.paid}</span> = <span className="mono">{s.gross} {s.asset}</span> received.
              </span>
            ) : (
              <span>
                Fee and payouts add up to <span className="mono">{check.total} {s.asset}</span>, not the <span className="mono">{s.gross} {s.asset}</span> received. Contact us with this session ID.
              </span>
            )}
          </p>
        </div>
      ) : (
        <p className="small muted" data-testid="settlement-no-payouts">
          No per-wallet payout breakdown is recorded for this payment.
        </p>
      )}
      <p className="small">
        Prismaport fee {displayAmount(s.fee)} {s.asset} · Net {displayAmount(s.net)} {s.asset} ·{' '}
        {url ? (
          <a href={url} target="_blank" rel="noreferrer">
            View transaction
          </a>
        ) : (
          <CopyValue value={s.tx_hash} what="transaction hash" />
        )}
      </p>
    </div>
  );
}

/** A flush that found this session's funds already moved by another session's flush of the same customer address. */
function SettledWithNote({ w }: { w: SettledWith }) {
  return (
    <div className="callout" role="note" data-testid="settled-with">
      <Icon name="info" />
      <p>
        {w.asset} on {networkName(w.chain)} was settled together with{' '}
        <RouterLink to={`/payments/${w.session_id}`}>
          <ShortValue value={w.session_id} head={8} tail={4} />
        </RouterLink>
        : both payments went to the same customer address, so one transaction moved them and the settlement is recorded on that payment.
      </p>
    </div>
  );
}

function PaymentItem({ session, p }: { session: SessionDetail; p: Payment }) {
  // A simulated payment's hash is synthetic: no explorer has it.
  const url = p.simulated ? null : explorerFor(session, p.chain, p.tx_hash, p.explorer_url);
  return (
    <li className="simple-row" data-testid="payment">
      <div className="row-lead grow">
        <AssetBadge asset={p.asset} chain={p.chain} decorative tag={false} />
        <div className="stack-sm grow">
          <p>
            <span className="amount">
              {displayAmount(p.amount)} {p.asset}
            </span>{' '}
            <span className="muted small">on {networkName(p.chain)}</span>
            <TestnetTag chain={p.chain} />
            {p.simulated ? <span className="tag">Simulated</span> : null}
          </p>
          <p className="small muted row">
            <CopyValue value={p.tx_hash} what="transaction hash" head={6} tail={4} />
            {url ? (
              <a href={url} target="_blank" rel="noreferrer" className="row">
                <Icon name="external" />
                View transaction
              </a>
            ) : null}
          </p>
        </div>
      </div>
      <div className="stack-sm">
        <Pill pill={PAYMENT_PILL[p.status] ?? { label: p.status, tone: 'idle' }} />
        <p className="small muted">{p.confirmed_at ? formatDateTime(p.confirmed_at) : 'Not confirmed yet'}</p>
      </div>
    </li>
  );
}

export function PaymentDetailPage() {
  const { id = '' } = useParams();
  const q = useSession(id);
  if (q.isPending) return <Loading what="the payment" />;
  if (q.isError && !q.data)
    return (
      <>
        <RouterLink to="/payments" className="back">
          <Icon name="back" />
          Payments
        </RouterLink>
        <ErrorState error={q.error} onRetry={() => q.refetch()} />
      </>
    );
  const s = q.data;
  const settlements = s.settlements && s.settlements.length > 0 ? s.settlements : s.settlement ? [s.settlement] : [];
  const events = [...(s.events ?? [])].sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at));
  const meta = Object.entries(s.metadata ?? {});
  const reversed = s.payments.filter((p) => p.status === 'reversed');

  return (
    <>
      <RouterLink to="/payments" className="back">
        <Icon name="back" />
        Payments
      </RouterLink>
      <PageHead
        title={sessionAmount(s.amount)}
        lead={
          <span className="row">
            <Pill pill={sessionPill(s.status)} />
            <ModeTag livemode={s.livemode} />
            {sessionSource(s) ? <span data-testid="payment-link-name">{sessionSource(s)}</span> : null}
            <CopyValue value={s.id} what="session ID" head={8} tail={4} />
          </span>
        }
        actions={!s.livemode ? <SimulateButton session={s} /> : undefined}
        status={sessionInProgress(s) ? <UpdatingLive updatedAt={q.dataUpdatedAt} /> : undefined}
      />
      <div className="stack">
        <RecheckPanel session={s} />
        {reversed.length > 0 ? (
          <div className="callout" role="note">
            <Icon name="info" />
            <p>
              {reversed.map((p) => `A payment of ${displayAmount(p.amount)} ${p.asset} was reversed by the network and no longer counts.`).join(' ')}
            </p>
          </div>
        ) : null}

        <div className="grid-2">
          <section className="card" aria-labelledby="summary-title">
            <h2 className="heading" id="summary-title">
              Summary
            </h2>
            <dl className="details mt-3">
              <dt>Amount</dt>
              <dd className="amount">
                {sessionAmount(s.amount)}
                {s.amount === null ? <span className="small muted"> (the payer decides)</span> : null}
              </dd>
              <dt>Received</dt>
              <dd className="amount">{displayAmount(s.amount_received)} USD</dd>
              {s.amount !== null ? (
                <>
                  <dt>Remaining</dt>
                  <dd className="amount">{displayAmount(s.amount_remaining)} USD</dd>
                </>
              ) : null}
              <dt>Product</dt>
              <dd>
                {s.product_id ? (
                  <RouterLink to={`/products/${s.product_id}`} data-testid="from-product">
                    {s.product_name ?? s.product_id}
                  </RouterLink>
                ) : (
                  <span className="muted">None</span>
                )}
              </dd>
              {s.customer_address_id ? (
                <>
                  <dt>Customer address</dt>
                  <dd>
                    <RouterLink to={`/customers/${s.customer_address_id}`} data-testid="from-customer-address">
                      <ShortValue value={s.customer_address_id} head={10} tail={4} />
                    </RouterLink>
                  </dd>
                </>
              ) : (
                <>
                  <dt>From link</dt>
                  <dd>
                    {s.link_id ? (
                      <RouterLink to={`/links/${s.link_id}`} data-testid="from-link">
                        {linkLabel(s) ?? 'Open the link'}
                      </RouterLink>
                    ) : (
                      <span className="muted">Created with the API</span>
                    )}
                  </dd>
                </>
              )}
              <dt>Created</dt>
              <dd>{formatDateTime(s.created_at)}</dd>
              <dt>Payment started</dt>
              <dd data-testid="payment-started">{s.payment_started_at ? formatDateTime(s.payment_started_at) : <span className="muted">Not yet</span>}</dd>
              <dt>{s.payment_started_at === null ? 'Selection ends' : 'Expires'}</dt>
              <dd>{formatDateTime(s.expires_at)}</dd>
              <dt>Paid</dt>
              <dd>{s.paid_at ? formatDateTime(s.paid_at) : <span className="muted">Not yet</span>}</dd>
              <dt>Checkout page</dt>
              <dd>
                <CopyValue value={s.checkout_url} what="checkout address" head={16} tail={8} />
              </dd>
            </dl>
          </section>

          <section className="card" aria-labelledby="timeline-title">
            <h2 className="heading" id="timeline-title">
              Timeline
            </h2>
            {events.length === 0 ? (
              <p className="muted mt-3">No events yet.</p>
            ) : (
              <ol className="timeline mt-4" data-testid="timeline">
                {events.map((e) => (
                  <li key={e.id}>
                    <p className="subheading">{eventLabel(e.type)}</p>
                    <p className="small muted">
                      {formatDateTime(e.created_at)} · <span className="address">{e.type}</span>
                    </p>
                  </li>
                ))}
              </ol>
            )}
          </section>
        </div>

        <section aria-labelledby="settlement-title" className="stack-md">
          <h2 className="heading" id="settlement-title">
            Settlement
          </h2>
          {(s.settled_with ?? []).map((w) => (
            <SettledWithNote key={`${w.chain}:${w.asset}:${w.tx_hash}`} w={w} />
          ))}
          {settlements.length === 0 ? (
            (s.settled_with ?? []).length > 0 ? null : <p className="card muted">Nothing settled yet. The fee is split on-chain in the same transaction that pays you.</p>
          ) : (
            settlements.map((st) => <SettlementCard key={st.tx_hash + st.asset} session={s} s={st} />)
          )}
        </section>

        <section aria-labelledby="payments-title" className="stack-md">
          <h2 className="heading" id="payments-title">
            Transfers
          </h2>
          {s.payments.length === 0 ? (
            <p className="card muted">No transfers seen yet.</p>
          ) : (
            <ul className="rows">
              {s.payments.map((p) => (
                <PaymentItem key={p.id} session={s} p={p} />
              ))}
            </ul>
          )}
        </section>

        <section className="card" aria-labelledby="payto-title">
          <h2 className="heading" id="payto-title">
            Payment addresses
          </h2>
          <p className="small muted mt-3">
            {s.amount === null
              ? 'The payer sends any amount to these addresses; every transfer counts.'
              : 'The exact amount a single transfer must carry on each network. A network has an address once the payer generates the payment on it.'}
          </p>
          <ul className="list-plain mt-3">
            {s.pay_to.map((p) => (
              <li key={`${p.chain}:${p.asset}`} className="payto-row" data-testid="pay-to">
                <AssetOnNetwork asset={p.asset} chain={p.chain} />
                <span className="amount">{tokenAmount(p.amount, p.asset)}</span>
                {p.address ? (
                  <div className="stack-sm">
                    <CopyValue value={p.address} what={`${networkName(p.chain)} address`} />
                    {p.revealed_at ? (
                      <span className="small muted" data-testid="revealed-at">
                        Generated {formatDateTime(p.revealed_at)}
                      </span>
                    ) : null}
                  </div>
                ) : (
                  <span className="muted" data-testid="not-generated">
                    Not generated
                  </span>
                )}
              </li>
            ))}
          </ul>
        </section>

        {s.client_reference_id ? (
          <section className="card" aria-labelledby="ref-title" data-testid="client-reference">
            <h2 className="heading" id="ref-title">
              Client reference
            </h2>
            <dl className="details mt-3">
              <dt>Reference</dt>
              <dd>
                <CopyValue value={s.client_reference_id} what="client reference" head={64} tail={64} />
              </dd>
              <dt>Set by</dt>
              <dd>{s.client_reference_source === 'payer' ? 'The payer, from the payment link address' : 'Your server, through the API'}</dd>
            </dl>
            <p className="note mt-3" data-emphasis={s.client_reference_source === 'payer' ? true : undefined}>
              <Icon name="info" />
              <span>{referenceNote(s.client_reference_source)}</span>
            </p>
          </section>
        ) : null}

        {meta.length > 0 ? (
          <section className="card" aria-labelledby="meta-title">
            <h2 className="heading" id="meta-title">
              Metadata
            </h2>
            <dl className="details mt-3">
              {meta.map(([k, v]) => (
                <div key={k} className="contents">
                  <dt className="address">{k}</dt>
                  <dd className="address">{v}</dd>
                </div>
              ))}
            </dl>
          </section>
        ) : null}
      </div>
    </>
  );
}
