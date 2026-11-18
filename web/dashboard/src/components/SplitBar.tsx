import { FULL, formatPercent, type Share } from '../lib/splits';

/**
 * Why a split shares one risk (ADR 0008 I-2): a deposit address pays every wallet in one
 * transaction, so a wallet the token issuer blacklists (USDC on Base) or freezes (Solana) makes
 * the whole payout fail for all of them.
 */
export const SPLIT_RISK_NOTE =
  'Every wallet in a split is paid in the same transaction: if the token issuer blacklists or freezes one of them, no wallet in the split is paid until the issuer lifts it.';

/**
 * A split drawn as one bar cut into each wallet's share, with every segment written out below
 * in the same order: wallet name and percentage, so nothing depends on telling fills apart.
 * The first recipient is magenta, the others brand neutrals; a share not yet assigned (while
 * editing) is a dashed outline. A split of two or more wallets also says that they share the
 * issuer-freeze risk.
 */
export function SplitBar({ shares, testId = 'split-bar' }: { shares: Share[]; testId?: string }) {
  const drawn = shares.filter((s) => s.bps > 0);
  const total = drawn.reduce((a, s) => a + s.bps, 0);
  const rest = total < FULL ? FULL - total : 0;
  return (
    <div className="split-figure" data-testid={testId}>
      <div className="split-bar" aria-hidden="true">
        {drawn.map((s, i) => (
          // flex-grow is set through the CSSOM (allowed by the page CSP), never a style attribute in the HTML.
          <span key={`${s.walletId}-${i}`} className="split-seg" data-seg={Math.min(i + 1, 5)} style={{ flexGrow: s.bps }} />
        ))}
        {rest > 0 && drawn.length > 0 ? <span className="split-seg" data-seg="rest" style={{ flexGrow: rest }} /> : null}
      </div>
      <ol className="split-legend">
        {drawn.map((s, i) => (
          <li key={`${s.walletId}-${i}`} data-testid="split-legend-item">
            <span className="split-swatch" data-seg={Math.min(i + 1, 5)} aria-hidden="true" />
            <span className="split-name">{s.label}</span>
            <span className="split-pct">{formatPercent(s.bps)}</span>
          </li>
        ))}
        {rest > 0 && drawn.length > 0 ? (
          <li data-testid="split-legend-rest">
            <span className="split-swatch" data-seg="rest" aria-hidden="true" />
            <span className="split-name muted">Not assigned</span>
            <span className="split-pct">{formatPercent(rest)}</span>
          </li>
        ) : null}
      </ol>
      {drawn.length > 1 ? (
        <p className="small muted" data-testid="split-risk-note">
          {SPLIT_RISK_NOTE}
        </p>
      ) : null}
    </div>
  );
}
