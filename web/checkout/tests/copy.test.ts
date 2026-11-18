import { describe, expect, it } from 'vitest';
import { COPY, PAYMENT_PILL, SENT_GRACE_MS, STATUS_PILL, describeSession, errorCopy, paymentPill, reversedNote, selfSendFeeCopy, signedCopy, statusPill } from '../src/lib/copy';
import type { SessionStatus, SignedPayment } from '../src/lib/api';
import { NOW, SOL_ADDRESS, payTo, session } from './fixtures';

const base = payTo();
const solUsdt = payTo({ chain: 'solana', asset: 'USDT', address: SOL_ADDRESS, signed_payment: false });

describe('brand copy', () => {
  it('uses the exact checkout copy from the brand book', () => {
    expect(COPY.waiting).toBe('Waiting for your payment. Keep this page open.');
    expect(COPY.paid).toBe('Paid. You can close this page.');
    expect(COPY.footer).toBe('Paid with Prismaport');
    expect(COPY.late).toBe('This payment arrived after the session expired. It reached the merchant; contact them if you need help.');
  });

  it('writes every session state as its word with its state tone', () => {
    const want: Record<SessionStatus, [string, string]> = {
      open: ['Open', 'idle'],
      partially_paid: ['Partially paid', 'partial'],
      paid: ['Paid', 'paid'],
      overpaid: ['Overpaid', 'paid'],
      expired: ['Expired', 'idle'],
      late_payment: ['Late payment', 'partial'],
      under_review: ['Under review', 'partial'],
    };
    for (const [status, [label, tone]] of Object.entries(want)) {
      expect(STATUS_PILL[status as SessionStatus]).toEqual({ label, tone });
    }
    expect(PAYMENT_PILL.reversed).toEqual({ label: 'Reversed', tone: 'reversed' });
  });

  it('never uses exclamation marks, emoji or hype words', () => {
    const codes = ['session_not_found', 'rate_limited', 'destination_not_ready', 'x', 'authorization_rejected', 'invalid_signature', 'service_unavailable', 'payment_in_progress'];
    const all = [...Object.values(COPY), ...codes.map((c) => errorCopy(c))];
    for (const text of all) {
      expect(text).not.toMatch(/!|\p{Extended_Pictographic}|seamless|magic|web3|revolutionary|\bgas\b/iu);
    }
  });
});

describe('describeSession', () => {
  it('open: the instruction and waiting line', () => {
    const v = describeSession(session(), base, NOW);
    expect(v.instruction).toBe('Send exactly 25.00 USDC on Base');
    expect(v.message).toBe(COPY.waiting);
    expect(v.exchangeWarning).toBe('Sending from an exchange? Add its withdrawal fee so 25.00 USDC arrives.');
    expect(v.payable).toBe(true);
    expect(v.pill.label).toBe('Open');
    expect(v.due).toBe('25.000000000000000000');
  });

  it('follows the selected asset and network', () => {
    expect(describeSession(session(), solUsdt, NOW).instruction).toBe('Send exactly 25.00 USDT on Solana');
  });

  it('partial: received, remaining, same address', () => {
    const s = session({
      status: 'partially_paid', amount_received: '10.00', amount_remaining: '15.00',
      payments: [{ chain: 'base', asset: 'USDC', amount: '10.000000', status: 'confirmed', tx_hash: '0xabc' }],
    });
    const v = describeSession(s, base, NOW);
    expect(v.message).toBe('Received 10.00 of 25.00 USDC. Send the remaining 15.00 to the same address.');
    expect(v.instruction).toBe('Send exactly 15.00 USDC on Base');
    expect(v.exchangeWarning).toBe('Sending from an exchange? Add its withdrawal fee so 15.00 USDC arrives.');
    expect(v.pill.label).toBe('Partially paid');
  });

  it('partial by a millionth: never rounds the remaining amount', () => {
    const s = session({ status: 'partially_paid', amount_received: '24.999999', amount_remaining: '0.000001' });
    const v = describeSession(s, base, NOW);
    expect(v.message).toBe('Received 24.999999 of 25.00 USDC. Send the remaining 0.000001 to the same address.');
    expect(v.instruction).toBe('Send exactly 0.000001 USDC on Base');
  });

  it('a detected payment covering the rest: waiting for confirmations, nothing more to send', () => {
    const s = session({ payments: [{ chain: 'base', asset: 'USDC', amount: '25.000000', status: 'detected', tx_hash: '0x1' }] });
    const v = describeSession(s, base, NOW);
    expect(v.payable).toBe(false);
    expect(v.instruction).toBeNull();
    expect(v.message).toBe(COPY.confirming);
  });

  it('a detected partial payment: what is still due after it', () => {
    const s = session({ payments: [{ chain: 'base', asset: 'USDC', amount: '20.000000', status: 'detected', tx_hash: '0x1' }] });
    const v = describeSession(s, base, NOW);
    expect(v.payable).toBe(true);
    expect(v.message).toBe('A payment of 20.00 USDC is confirming. Send the remaining 5.00 to the same address.');
    expect(v.instruction).toBe('Send exactly 5.00 USDC on Base');
  });

  it('paid and overpaid', () => {
    expect(describeSession(session({ status: 'paid', amount_received: '25.00', amount_remaining: '0.00' }), base, NOW)).toMatchObject({
      message: COPY.paid, payable: false, instruction: null, pill: { label: 'Paid', tone: 'paid' },
    });
    const over = describeSession(session({
      status: 'overpaid', amount_received: '26.50', amount_remaining: '0.00',
      payments: [{ chain: 'base', asset: 'USDC', amount: '26.500000', status: 'confirmed', tx_hash: '0x1' }],
    }), base, NOW);
    expect(over.message).toBe(COPY.complete);
    expect(over.detail).toBe('The merchant received your payment. You can close this page.');
    expect(over.overpaid).toEqual({ amount: '25.00', received: '26.50', extra: '1.50', unit: 'USDC' });
    expect(over.pill.label).toBe('Overpaid');
    const named = describeSession(session({ status: 'overpaid', amount_received: '26.50', amount_remaining: '0.00', merchant: { name: 'Acme Store', logo_url: '' } }), base, NOW);
    expect(named.detail).toBe('Acme Store received your payment. You can close this page.');
    expect(describeSession(session({ status: 'paid', amount_received: '25.00', amount_remaining: '0.00' }), base, NOW).overpaid).toBeNull();
    // A blank merchant name reads as "The merchant", never as a sentence with a hole.
    const blank = describeSession(session({ status: 'overpaid', amount_received: '26.50', amount_remaining: '0.00', merchant: { name: '', logo_url: '' } }), base, NOW);
    expect(blank.detail).toBe('The merchant received your payment. You can close this page.');
    // An open-amount session cannot be overpaid in this sense: it keeps its own copy and no breakdown.
    const open = describeSession(session({ status: 'overpaid', open_amount: true, amount: null, amount_received: '26.50', amount_remaining: null }), base, NOW);
    expect(open.overpaid).toBeNull();
    expect(open.message).toBe(COPY.paid);
  });

  it('late payment uses the brand copy', () => {
    const v = describeSession(session({ status: 'late_payment', amount_received: '25.00', amount_remaining: '0.00' }), base, NOW);
    expect(v.message).toBe(COPY.late);
    expect(v.payable).toBe(false);
    expect(v.pill).toEqual({ label: 'Late payment', tone: 'partial' });
  });

  it('expired, with and without funds', () => {
    expect(describeSession(session({ status: 'expired' }), base, NOW).message).toBe(COPY.expired);
    const v = describeSession(session({
      status: 'expired', amount_received: '10.00', amount_remaining: '15.00',
      payments: [{ chain: 'solana', asset: 'USDT', amount: '10.000000', status: 'confirmed', tx_hash: 'sig' }],
    }), base, NOW);
    expect(v.message).toBe('This session expired after receiving 10.00 of 25.00 USDT. Contact the merchant about this payment.');
    expect(v.payable).toBe(false);
  });

  it('hides the address once the local countdown ends, before the server catches up', () => {
    const v = describeSession(session(), base, Date.parse('2026-10-23T19:00:00Z'));
    expect(v.payable).toBe(false);
    expect(v.pill.label).toBe('Expired');
    expect(v.message).toBe(COPY.expired);
  });

  it('after expiry a pending payment still reads as confirming', () => {
    const s = session({ payments: [{ chain: 'base', asset: 'USDC', amount: '5.000000', status: 'detected', tx_hash: '0x1' }] });
    expect(describeSession(s, base, Date.parse('2026-10-23T19:00:01Z')).message).toBe(COPY.confirming);
  });

  it('under review never asks for more money', () => {
    const v = describeSession(session({ status: 'under_review' }), base, NOW);
    expect(v.payable).toBe(false);
    expect(v.message).toBe(COPY.review);
  });

  it('reversed payments explain themselves', () => {
    expect(reversedNote({ chain: 'base', asset: 'USDC', amount: '10.000000', status: 'reversed', tx_hash: '0x' }))
      .toBe('A payment of 10.00 USDC was reversed by the network and no longer counts.');
  });
});

describe('signed payment progress', () => {
  const sp = (status: 'pending' | 'submitted' | 'done' | 'failed', outcome: string | null = null) =>
    ({ id: 'txj_1', chain: 'base', asset: 'USDC', status, outcome, tx_hash: null });

  it('maps job states to copy', () => {
    expect(signedCopy(sp('pending')).text).toMatch(/^Payment signed/);
    expect(signedCopy(sp('submitted')).failed).toBe(false);
    expect(signedCopy(sp('done', 'settled'))).toEqual({ text: 'Payment sent. Waiting for confirmations.', done: true, failed: false });
    expect(signedCopy(sp('done', 'reverted')).failed).toBe(true);
    expect(signedCopy(sp('failed', 'rejected')).text).toBe('This signed payment did not go through. You can sign again.');
  });
});

describe('errors', () => {
  it('say what happened and what to do next', () => {
    expect(errorCopy('session_not_found')).toBe(COPY.notFound);
    expect(errorCopy('rate_limited', 12)).toBe('Too many attempts from this connection. Try again in 12 seconds.');
    expect(errorCopy('product_inactive')).toBe('This link is no longer available. Contact the merchant if you need help.');
    // G-3: common codes are not blamed on the server; the fallback carries a reference.
    expect(errorCopy('not_found')).toBe(COPY.notFound);
    expect(errorCopy('session_expired')).toBe(COPY.expired);
    expect(errorCopy('invalid_request')).toMatch(/^Some of the details were not accepted/);
    expect(errorCopy('invalid_state')).not.toMatch(/our side/);
    expect(errorCopy('weird_code')).toMatch(/our side.*Reference: weird_code\.$/);
    expect(errorCopy('http_error', undefined, 502)).toMatch(/Reference: 502 http_error\.$/);
    expect(errorCopy('link_unavailable')).toBe('This link is no longer available. Contact the merchant if you need help.');
    expect(errorCopy('amount_below_minimum')).toMatch(/address below/);
    expect(errorCopy('destination_not_ready')).toMatch(/address below/);
    expect(errorCopy('payment_in_progress')).toBe('A payment for this checkout is already in progress. If you cancelled it, wait a minute and try again.');
  });
});

describe('network fee copy', () => {
  it('names the fee token and the network the payer sends on', () => {
    expect(selfSendFeeCopy('base', false)).toBe('You send this transaction yourself, so your wallet needs a little ETH on Base for the network fee.');
    expect(selfSendFeeCopy('base-sepolia', false)).toBe('You send this transaction yourself, so your wallet needs a little ETH on Base Sepolia for the network fee.');
    expect(selfSendFeeCopy('polygon', false)).toContain('a little POL on Polygon');
    expect(selfSendFeeCopy('solana', true)).toBe('You send this transaction yourself, so your wallet needs a little SOL for the network fee.');
    expect(selfSendFeeCopy('solana-devnet', true)).toContain('a little SOL for');
  });

  it('never guesses the fee token of an unknown chain', () => {
    expect(selfSendFeeCopy('zora', false)).toBe('You send this transaction yourself, so your wallet needs a little of the Zora native token for the network fee.');
  });
});

describe('past expiry with a payment the page sent', () => {
  const EXPIRES = Date.parse('2026-10-23T19:00:00Z');
  const sp = (status: SignedPayment['status']): SignedPayment => ({ id: 'txj_1', chain: 'base', asset: 'USDC', status, outcome: null, tx_hash: null });

  it('keeps checking instead of locking, on the client clock and once the server says expired', () => {
    expect(describeSession(session(), base, EXPIRES + 1000, true)).toMatchObject({ locked: false, grace: true, payable: false, instruction: null });
    expect(describeSession(session({ status: 'expired' }), base, EXPIRES + 1000, true)).toMatchObject({ locked: false, grace: true });
    expect(describeSession(session(), base, EXPIRES + 1000)).toMatchObject({ locked: true, grace: false });
  });

  it('a signed payment Prismaport is still sending counts too; one that failed or finished does not', () => {
    for (const status of ['pending', 'submitted'] as const) {
      expect(describeSession(session({ status: 'expired', signed_payments: [sp(status)] }), base, EXPIRES + 1000).grace, status).toBe(true);
    }
    for (const status of ['failed', 'done'] as const) {
      expect(describeSession(session({ status: 'expired', signed_payments: [sp(status)] }), base, EXPIRES + 1000).locked, status).toBe(true);
    }
  });

  it('ends SENT_GRACE_MS after expiry, and changes nothing before expiry', () => {
    expect(describeSession(session(), base, EXPIRES + SENT_GRACE_MS - 1, true).grace).toBe(true);
    expect(describeSession(session(), base, EXPIRES + SENT_GRACE_MS, true)).toMatchObject({ locked: true, grace: false });
    expect(describeSession(session(), base, NOW, true)).toMatchObject({ payable: true, grace: false, locked: false });
  });

  it('a confirming payment needs no grace: it reads as confirming', () => {
    const s = session({ status: 'expired', payments: [{ chain: 'base', asset: 'USDC', amount: '25.000000', status: 'detected', tx_hash: '0xabc' }] });
    expect(describeSession(s, base, EXPIRES + 1000, true)).toMatchObject({ locked: false, grace: false });
  });
});

describe('statuses this page does not know', () => {
  it('a session status: Processing, nothing to pay, not locked (the page keeps polling)', () => {
    for (const s of [session({ status: 'refund_pending' as never }), session({ status: 'refund_pending' as never, open_amount: true, amount: null, amount_remaining: null })]) {
      const v = describeSession(s, base, NOW);
      expect(v.pill).toEqual({ label: 'Processing', tone: 'idle' });
      expect(v).toMatchObject({ updating: true, payable: false, locked: false, instruction: null, message: COPY.updating });
    }
    expect(statusPill('open')).toEqual(STATUS_PILL.open);
  });

  it('a payment status: Processing', () => {
    expect(paymentPill('settling')).toEqual({ label: 'Processing', tone: 'idle' });
    expect(paymentPill('detected')).toEqual(PAYMENT_PILL.detected);
  });

  it('a signed payment state: neutral copy that offers no second signature', () => {
    const job = { id: 'txj_1', chain: 'base', asset: 'USDC', status: 'queued' as never, outcome: null, tx_hash: null };
    expect(signedCopy(job)).toEqual({ text: 'Payment signed. We are checking how it went. Keep this page open.', done: false, failed: false });
  });
});
