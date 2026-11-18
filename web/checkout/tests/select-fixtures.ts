import type { PayTo, Session } from '../src/lib/api';
import { EVM_ADDRESS, SOL_ADDRESS, session } from './fixtures';

/** A session nobody generated a payment for: no address anywhere (D48), the selection deadline in `expires_at`. */
export function unrevealedSession(over: Partial<Session> = {}): Session {
  const bare = (chain: string, asset: string): PayTo => ({ chain, asset, amount: '25.000000', signed_payment: false, revealed: false });
  return session({
    pay_to: [bare('base', 'USDC'), bare('solana', 'USDC'), bare('solana', 'USDT')],
    payment_started_at: null,
    pay_window_seconds: 1800,
    expires_at: '2026-10-23T19:00:00Z',
    ...over,
  });
}

/** `s` with every entry of `chain` generated: the address back, `revealed` true, the deadline moved to the pay window. */
export function withRevealed(s: Session, chain: string, expiresAt = '2026-10-23T18:30:00Z'): Session {
  return {
    ...s,
    payment_started_at: s.payment_started_at ?? '2026-10-23T18:00:00Z',
    expires_at: expiresAt,
    pay_to: s.pay_to.map((p) => p.chain !== chain ? p : {
      ...p,
      revealed: true,
      address: chain === 'solana' ? SOL_ADDRESS : EVM_ADDRESS,
      signed_payment: chain === 'base',
    }),
  };
}
