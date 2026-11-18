import { useState } from 'react';
import { Link as RouterLink, useNavigate, useSearchParams } from 'react-router-dom';
import { Icon } from '../components/Icon';
import { AssetBadge, AssetOnNetwork, NetworkLogo, TestnetTag, TokenAmount } from '../components/Logos';
import { useToast } from '../components/Toast';
import { CopyValue, ErrorState, Loading, PageHead, Pill, Segmented } from '../components/ui';
import { displayAmount, formatDateTime, linkTitle, networkName } from '../lib/format';
import { addAmounts, compareAmounts, subtractAmounts } from '../lib/money';
import { useMode } from '../lib/mode';
import { isPaused, usePauses } from '../lib/pauses';
import { useOverview } from '../lib/queries';
import { ATTENTION_COPY, PAYMENT_PILL, SESSION_PILL } from '../lib/status';
import type { OverviewTotal, RecentPayment, WalletTotal } from '../lib/types';
import { LinkDrawer } from './LinkDrawer';

function RecentPayments({ items }: { items: RecentPayment[] }) {
  return (
    <ul className="rows" aria-label="Recent payments">
      {items.map((p) => (
        <li key={p.id}>
          <RouterLink to={`/payments/${p.session_id}`} className="simple-row recent-row" data-testid="recent-payment">
            <span className="row-lead grow">
              <AssetBadge asset={p.asset} chain={p.chain} decorative tag={false} />
              <span className="stack-sm grow">
                <span className="block">
                  <span className="amount">
                    {displayAmount(p.amount)} {p.asset}
                  </span>{' '}
                  <span className="small muted">on {networkName(p.chain)}</span>
                  <TestnetTag chain={p.chain} />
                </span>
                <span className="small muted block">
                  {p.link_name ?? 'Created with the API'} · {formatDateTime(p.detected_at)}
                </span>
              </span>
            </span>
            <span className="row">
              {p.simulated ? <span className="tag">Simulated</span> : null}
              {p.late ? <Pill pill={SESSION_PILL.late_payment} /> : null}
              <Pill pill={PAYMENT_PILL[p.status] ?? { label: p.status, tone: 'idle' }} />
            </span>
          </RouterLink>
        </li>
      ))}
    </ul>
  );
}

function PausedTag() {
  return (
    <span className="testnet-tag paused-tag" data-testid="paused-tag">
      Paused
    </span>
  );
}

function NetworkTotals({ totals }: { totals: OverviewTotal[] }) {
  const pauses = usePauses();
  return (
    <div className="totals">
      {totals.map((t) => (
        <div key={`${t.chain}:${t.asset}`} className="card" data-testid="total">
          <p className="label">
            <AssetOnNetwork asset={t.asset} chain={t.chain} />
            {isPaused(pauses, t.chain, t.asset) ? <PausedTag /> : null}
          </p>
          <p className="amount-hero">
            <TokenAmount asset={t.asset} size={24}>
              {displayAmount(t.net)} {t.asset}
            </TokenAmount>
          </p>
          <p className="small muted">Net to your wallets</p>
          <dl className="total-lines small">
            <dt className="muted">Received</dt>
            <dd className="mono num">
              {displayAmount(t.received)} {t.asset}
            </dd>
            <dt className="muted">Prismaport fee</dt>
            <dd className="mono num">
              {displayAmount(t.fees)} {t.asset}
            </dd>
            {compareAmounts(addAmounts(t.fees, t.net), t.received) < 0 ? (
              <>
                <dt className="muted">Waiting to be forwarded</dt>
                <dd className="mono num" data-testid="unforwarded">
                  {displayAmount(subtractAmounts(t.received, addAmounts(t.fees, t.net)))} {t.asset}
                </dd>
              </>
            ) : null}
          </dl>
        </div>
      ))}
    </div>
  );
}

/** What each wallet was paid, per network and stablecoin, in the order the API lists wallets. */
function WalletTotals({ items }: { items: WalletTotal[] }) {
  const pauses = usePauses();
  const byWallet = new Map<string, WalletTotal[]>();
  for (const t of items) {
    const k = t.wallet_id ?? t.address;
    byWallet.set(k, [...(byWallet.get(k) ?? []), t]);
  }
  return (
    <div className="totals">
      {[...byWallet.values()].map((rows) => {
        const w = rows[0]!;
        const label = w.label ?? 'Unnamed wallet';
        const chains = [...new Set(rows.map((r) => r.chain))];
        return (
          <div key={w.wallet_id ?? w.address} className="card stack-sm" data-testid="wallet-total">
            <p className="subheading with-logo">
              {chains.map((c) => (
                <NetworkLogo key={c} chain={c} size={20} decorative tag={false} />
              ))}
              <span>{label}</span>
              <TestnetTag chain={chains[0]!} />
            </p>
            <p className="small muted">
              <CopyValue value={w.address} what={`${label} address`} />
            </p>
            <ul className="list-plain">
              {rows.map((r) => (
                <li key={`${r.chain}:${r.asset}`} className="amount-hero wallet-amount">
                  <TokenAmount asset={r.asset} size={24}>
                    {displayAmount(r.amount)} {r.asset}
                  </TokenAmount>
                  {isPaused(pauses, r.chain, r.asset) ? <PausedTag /> : null}
                </li>
              ))}
            </ul>
            <p className="small muted">Paid out on {[...new Set(rows.map((r) => networkName(r.chain)))].join(' and ')}</p>
          </div>
        );
      })}
    </div>
  );
}

type Grouping = 'network' | 'wallet';

export function OverviewPage() {
  const { mode } = useMode();
  const overview = useOverview();
  const navigate = useNavigate();
  const toast = useToast();
  const [creating, setCreating] = useState(false);
  const [params, setParams] = useSearchParams();
  // Local state answers the click at once; the address follows, so a reload keeps the view.
  const [grouping, setGrouping] = useState<Grouping>(() => (params.get('group') === 'wallet' ? 'wallet' : 'network'));
  const byWallet = overview.data?.totals_by_wallet ?? [];
  const group: Grouping = grouping === 'wallet' && byWallet.length > 0 ? 'wallet' : 'network';
  const setGroup = (g: Grouping) => {
    setGrouping(g);
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        if (g === 'wallet') next.set('group', 'wallet');
        else next.delete('group');
        return next;
      },
      { replace: true },
    );
  };

  return (
    <>
      <PageHead
        title="Overview"
        lead={`What arrived in ${mode === 'test' ? 'Test' : 'Live'} mode, per stablecoin and network.`}
        actions={
          <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
            <Icon name="plus" />
            New link
          </button>
        }
      />
      {overview.isPending ? (
        <Loading what="the overview" />
      ) : overview.isError ? (
        <ErrorState error={overview.error} onRetry={() => overview.refetch()} />
      ) : (
        <div className="stack">
          <section aria-labelledby="totals-title" className="stack-md">
            <div className="row between">
              <h2 className="heading" id="totals-title">
                Received
              </h2>
              {byWallet.length > 0 ? (
                <Segmented<Grouping>
                  legend="Group received totals by"
                  name="overview-group"
                  value={group}
                  onChange={setGroup}
                  options={[
                    { value: 'network', label: 'By network' },
                    { value: 'wallet', label: 'By wallet' },
                  ]}
                />
              ) : null}
            </div>
            {overview.data.totals.length === 0 ? (
              <p className="card muted">No payments yet. Add a product, share its payment link and get paid.</p>
            ) : group === 'wallet' ? (
              <WalletTotals items={byWallet} />
            ) : (
              <NetworkTotals totals={overview.data.totals} />
            )}
          </section>

          {overview.data.links_attention.length > 0 ? (
            <section aria-labelledby="attention-title" className="stack-md">
              <h2 className="heading" id="attention-title">
                Links to look at
              </h2>
              <ul className="rows">
                {overview.data.links_attention.map((a) => (
                  <li key={a.id} className="simple-row">
                    <RouterLink to={`/links/${a.id}`} className="row-link">
                      {a.name}
                    </RouterLink>
                    <span className="small muted">{ATTENTION_COPY[a.reason] ?? a.reason}</span>
                  </li>
                ))}
              </ul>
            </section>
          ) : null}

          <section aria-labelledby="recent-title" className="stack-md">
            <div className="row between">
              <h2 className="heading" id="recent-title">
                Recent payments
              </h2>
              <RouterLink to="/payments" className="small">
                All payments
              </RouterLink>
            </div>
            {overview.data.recent_payments.length === 0 ? <p className="card muted">No payments yet.</p> : <RecentPayments items={overview.data.recent_payments} />}
          </section>
        </div>
      )}
      {creating ? (
        <LinkDrawer
          onClose={() => setCreating(false)}
          onSaved={(saved) => {
            setCreating(false);
            toast.show(`${linkTitle(saved)} link created`);
            navigate(`/links/${saved.id}`);
          }}
        />
      ) : null}
    </>
  );
}
