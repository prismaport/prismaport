import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { SPLIT_RISK_NOTE, SplitBar } from '../src/components/SplitBar';

// ADR 0008 I-2: a split pays every wallet in one transaction, so one wallet the issuer blacklists
// or freezes blocks all of them. The figure says so whenever it shows two or more wallets.
describe('SplitBar risk note', () => {
  it('warns under a split of two or more wallets', () => {
    render(
      <SplitBar
        shares={[
          { walletId: 'wal_a', label: 'Founder', bps: 7000 },
          { walletId: 'wal_b', label: 'Partner', bps: 3000 },
        ]}
      />,
    );
    expect(screen.getByTestId('split-risk-note')).toHaveTextContent(SPLIT_RISK_NOTE);
    expect(SPLIT_RISK_NOTE).toMatch(/blacklists or freezes one of them, no wallet in the split is paid/);
  });

  it('says nothing for a single wallet, or a second wallet with no share yet', () => {
    const { rerender } = render(<SplitBar shares={[{ walletId: 'wal_a', label: 'Founder', bps: 10000 }]} />);
    expect(screen.queryByTestId('split-risk-note')).toBeNull();
    rerender(
      <SplitBar
        shares={[
          { walletId: 'wal_a', label: 'Founder', bps: 9000 },
          { walletId: 'wal_b', label: 'Partner', bps: 0 },
        ]}
      />,
    );
    expect(screen.queryByTestId('split-risk-note')).toBeNull();
  });
});
