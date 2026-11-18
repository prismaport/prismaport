import { Link as RouterLink } from 'react-router-dom';
import { displayAmount, formatDateTime, sessionAmount } from '../lib/format';
import { sessionPill } from '../lib/status';
import type { Session } from '../lib/types';
import { AssetOnNetwork } from './Logos';
import { Pill, ShortValue } from './ui';

/** Each asset and network a counted payment came in on, once; reversed ones no longer count. */
function paidWithList(s: Session): { asset: string; chain: string }[] {
  const byKey = new Map<string, { asset: string; chain: string }>();
  for (const p of s.payments) {
    if (p.status !== 'reversed' && !byKey.has(`${p.chain}:${p.asset}`)) byKey.set(`${p.chain}:${p.asset}`, { asset: p.asset, chain: p.chain });
  }
  return [...byKey.values()];
}

/** The label of the link a session came from, when the link has one of its own (not just the product's name). */
export function linkLabel(s: Pick<Session, 'link_id' | 'link_name' | 'product_name'>): string | null {
  return s.link_id && s.link_name && s.link_name !== s.product_name ? s.link_name : null;
}

/** Where a payment came from: "Founder plan · Instagram bio", "Founder plan", "Top-up · Customer address" or "Custom order · API". */
export function sessionSource(s: Pick<Session, 'link_id' | 'link_name' | 'product_name' | 'customer_address_id'>): string | null {
  const product = s.product_name ?? s.link_name;
  if (!product) return null;
  if (s.customer_address_id) return `${product} · Customer address`;
  if (!s.link_id) return `${product} · API`;
  const label = linkLabel(s);
  return label ? `${product} · ${label}` : product;
}

/**
 * `source` (Payments): product and link label. `link` (a product's page): the link label, or
 * whether it came from a link or the API. `session` (a link's page): the session id.
 */
export type SessionColumn = 'source' | 'link' | 'session';

export function SessionRows({ sessions, show = 'source', label }: { sessions: Session[]; show?: SessionColumn; label: string }) {
  const head = show === 'source' ? 'Product' : show === 'link' ? 'Link' : 'Session';
  const cell = (s: Session) => {
    if (show === 'source') return sessionSource(s);
    if (show === 'link') return s.link_id ? linkLabel(s) ?? 'Payment link' : s.customer_address_id ? 'Customer address' : 'API';
    return null;
  };
  return (
    <ul className="rows" aria-label={label}>
      <li className="rows-head sessions label" aria-hidden="true">
        <span>Amount</span>
        <span>Status</span>
        <span>{head}</span>
        <span>Received</span>
        <span>Created</span>
      </li>
      {sessions.map((s) => (
        <li key={s.id}>
          <RouterLink to={`/payments/${s.id}`} className="data-row session-row" data-testid="session-row" aria-label={`${sessionAmount(s.amount)}, ${sessionPill(s.status).label}, ${formatDateTime(s.created_at)}`}>
            <span className="cell amount">{sessionAmount(s.amount)}</span>
            <span className="cell">
              <Pill pill={sessionPill(s.status)} />
            </span>
            <span className="c-sub">
              <span className="cell">
                {cell(s) ?? <ShortValue value={s.id} head={8} tail={4} />}
                {s.client_reference_id ? (
                  <span className="small muted ref-line" data-testid="session-ref">
                    <span className="address ref-value" title={s.client_reference_id}>
                      {s.client_reference_id}
                    </span>
                    {s.client_reference_source === 'payer' ? ' (from the payer)' : ''}
                  </span>
                ) : null}
              </span>
              <span className="cell">
                <span className="amount small">{displayAmount(s.amount_received)} USD</span>
                {paidWithList(s).map((x, i) => (
                  <span key={`${x.chain}:${x.asset}`} className="small muted">
                    {i === 0 ? ' · ' : ', '}
                    <span className="paid-with">
                      <AssetOnNetwork asset={x.asset} chain={x.chain} size={16} />
                    </span>
                  </span>
                ))}
              </span>
              <span className="cell small">{formatDateTime(s.created_at)}</span>
            </span>
          </RouterLink>
        </li>
      ))}
    </ul>
  );
}
