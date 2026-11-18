import { ApiError, type CheckoutApi, type PayTo, type Session, type TestPanelAction, type TestPanelResult } from '../lib/api';
import { errorCopy } from '../lib/copy';
import { h, icon } from '../lib/dom';
import { replace } from '../ui';

export interface PanelButton {
  action: TestPanelAction;
  label: string;
  enabled: boolean;
}

export const PANEL_LABELS: Record<TestPanelAction, string> = {
  pay_exact: 'Pay exact amount',
  pay_partial: 'Pay part',
  overpay: 'Overpay',
  expire: 'Let it expire',
  settle: 'Settle',
};

const ORDER: TestPanelAction[] = ['pay_exact', 'pay_partial', 'overpay', 'expire', 'settle'];

/** What "Pay this amount" sends on an open-amount session until the tester types another. */
const OPEN_AMOUNT_DEFAULT = '10.00';

/** Which steps the session can take now, and one line on why the others cannot. */
export function panelState(s: Session, settled: boolean): { buttons: PanelButton[]; hint: string } {
  const open = s.status === 'open' || s.status === 'partially_paid';
  // An open-amount session has no part to pay or amount to exceed; a paid one keeps crediting.
  const anyAmount = s.open_amount === true;
  const counted = s.payments.some((p) => p.status === 'confirmed' || p.status === 'finalized');
  const closed = s.status === 'paid' || s.status === 'overpaid' || s.status === 'expired' || s.status === 'late_payment';
  const canSettle = closed && counted && !settled;
  const buttons = ORDER.filter((a) => !anyAmount || (a !== 'pay_partial' && a !== 'overpay')).map((action) => ({
    action,
    label: anyAmount && action === 'pay_exact' ? 'Pay this amount' : PANEL_LABELS[action],
    enabled: action === 'settle' ? canSettle : action === 'pay_exact' && anyAmount ? open || s.status === 'paid' : open,
  }));
  let hint: string;
  if (settled) hint = 'Settled. The dashboard shows the fee and each payout.';
  else if (open) hint = 'Settle works once the session is paid or has expired.';
  else if (canSettle) hint = 'Settle forwards the test funds as the network would: the fee, then each payout.';
  else if (s.status === 'under_review') hint = 'A session under review takes no more steps.';
  else hint = 'Nothing was paid, so there is nothing to settle. Open the link again to start a new payment.';
  return { buttons, hint };
}

const sentence = (text: string) => {
  const t = text.trim();
  return t ? `${t[0]!.toUpperCase()}${t.slice(1)}${/[.!?]$/.test(t) ? '' : '.'}` : t;
};

/** What a finished step says. */
export function resultCopy(res: TestPanelResult): string {
  if (res.action === 'settle') return 'Settled. The dashboard shows the fee and each payout.';
  const events = res.events.map((e) => e.type).join(', ');
  return events ? `Simulated. Events sent: ${events}.` : 'Simulated.';
}

export interface PanelDeps {
  api: CheckoutApi;
  session: () => Session | null;
  selected: () => PayTo | undefined;
  onSession: (s: Session) => void;
  announce: (text: string) => void;
}

/**
 * The Simulate panel of the hosted checkout. It exists only for test-mode sessions and starts
 * collapsed: each button runs one simulator step on the server, which refuses live sessions anyway.
 */
export function createTestPanel(deps: PanelDeps) {
  const el = h('section', { class: 'stack-tight test-panel', 'aria-labelledby': 'test-panel-title', 'data-testid': 'test-panel' });
  let busy = false;
  let settled = false;
  let expanded = false;
  let note: { text: string; failed: boolean } | null = null;
  const amount = h('input', { class: 'input', id: 'test-amount', inputmode: 'decimal', autocomplete: 'off', 'data-fid': 'test-amount' });
  amount.value = OPEN_AMOUNT_DEFAULT;

  async function run(action: TestPanelAction): Promise<void> {
    const s = deps.session();
    if (!s || s.livemode || busy) return;
    const sel = deps.selected();
    busy = true;
    note = null;
    render();
    try {
      const body: { action: TestPanelAction; chain?: string; asset?: string; amount?: string } =
        action === 'settle' || action === 'expire' || !sel ? { action } : { action, chain: sel.chain, asset: sel.asset };
      // An open-amount session pays what the payer sends: the panel sends the typed amount.
      if (s.open_amount && action === 'pay_exact') body.amount = amount.value.trim();
      const res = (await deps.api.simulate(s.id, body)).data;
      if (res.settled) settled = true;
      note = { text: resultCopy(res), failed: false };
      deps.onSession(res.session);
    } catch (e) {
      const text = e instanceof ApiError && (e.code === 'invalid_state' || e.code === 'invalid_request') ? sentence(e.message)
        : e instanceof ApiError ? errorCopy(e.code, e.retryAfter, e.status) : errorCopy('network_error');
      note = { text, failed: true };
    } finally {
      busy = false;
      render();
      if (note) deps.announce(note.text);
    }
  }

  function render(): HTMLElement | null {
    const s = deps.session();
    if (!s || s.livemode) {
      el.replaceChildren();
      return null;
    }
    const { buttons, hint } = panelState(s, settled);
    // Open, every step shows so the tester sees what exists. Closed, only the steps left show and the
    // hint says why: a grid of greyed-out buttons reads as broken.
    const open = s.status === 'open' || s.status === 'partially_paid';
    const shown = open ? buttons : buttons.filter((b) => b.enabled);
    const actions = h('div', { class: 'test-actions' });
    for (const b of shown) {
      actions.append(h('button', { type: 'button', class: 'btn btn-small', 'data-fid': `test-${b.action}`, disabled: busy || !b.enabled,
        onclick: () => void run(b.action) }, b.label));
    }
    const body: Node[] = [
      h('p', { class: 'small' }, 'Try the payment without a wallet. Each button acts as the payer or the network, and webhooks, the event feed and the dashboard get the same events a real payment sends.'),
      ...(s.open_amount && shown.some((b) => b.action === 'pay_exact') ? [h('div', {}, h('label', { class: 'label', for: 'test-amount' }, `Amount to send (${deps.selected()?.asset ?? 'token units'})`), amount)] : []),
      ...(shown.length > 0 ? [actions] : []),
      h('p', { class: 'small muted', 'data-testid': 'test-panel-hint' }, hint),
    ];
    if (note) body.push(h('p', { class: 'note', 'data-emphasis': note.failed || undefined, role: 'status' }, icon('info'), h('span', {}, note.text)));
    const head = h('h2', { class: 'label test-panel-title', id: 'test-panel-title' },
      h('button', { type: 'button', class: 'test-panel-toggle', 'aria-expanded': String(expanded), 'aria-controls': 'test-panel-body', 'data-fid': 'test-toggle',
        onclick: () => { expanded = !expanded; render(); } },
      h('span', {}, 'Simulate'), h('span', { class: 'testnet-tag' }, 'Not a real payment'), icon('chevron', 16)));
    replace(el, head, h('div', { class: 'stack-tight', id: 'test-panel-body', hidden: !expanded }, ...body));
    return el;
  }

  return { el, render };
}
