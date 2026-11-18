import { ApiError, type CheckoutApi, type Network, type PublicAddress } from '../lib/api';
import { COPY, PAUSED, errorCopy } from '../lib/copy';
import { h, icon } from '../lib/dom';
import { formatAmount } from '../lib/format';
import { merchantLogos } from '../lib/merchant';
import { networkName, payToKey } from '../lib/networks';
import { brandHeader, footer, offlineNotice, replace, tickChecked } from '../ui';
import { depositBlock, networkPicker } from './deposit';

// The page of a static customer address (/a/{caddr_id}): the
// customer's deposit address per network, always the same, and a button that opens (or resumes)
// the session on it and moves to the session page /pay/{id}, where the payment is followed (and
// locked at expiry, D45). The address is watched only while a session on it is open (D37).

export interface AddressDeps {
  api: CheckoutApi;
  navigate?: (path: string) => void;
  /** How often a visible page refreshes (presence on the open session, its button state), in ms. */
  pollMs?: number;
}

/** What the button says: resume the open session, or open one at the address's price. */
export function startLabel(a: Pick<PublicAddress, 'session' | 'open_amount' | 'amount'>): string {
  if (a.session) return 'Continue to payment';
  if (!a.open_amount && a.amount) return `Pay ${formatAmount(a.amount)} USD`;
  return 'Start a payment';
}

function visible(): boolean {
  return typeof document !== 'undefined' && document.visibilityState === 'visible';
}

export async function mountAddress(root: HTMLElement, addressId: string, deps: AddressDeps): Promise<{ stop: () => void }> {
  const navigate = deps.navigate ?? ((path: string) => window.location.assign(path));
  const pollMs = deps.pollMs ?? 15_000;
  const card = h('section', { class: 'card stack', 'aria-labelledby': 'title' }, h('p', { class: 'skeleton' }, 'Loading the payment details.'));
  const status = h('div', { class: 'sr-only', role: 'status', 'aria-live': 'polite' });
  root.replaceChildren(brandHeader(), card, footer(), status);
  root.removeAttribute('aria-busy');

  let addr: PublicAddress | null = null;
  let networks: Network[] = [];
  let selected: string | null = null;
  let fullAddress = false;
  let busy = false;
  let error: string | null = null;
  let lastJson = '';
  let stopped = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  /** The API stopped answering after a first answer; `lastOk` is when it last did. */
  let offline = false;
  let lastOk = 0;
  const logoFor = merchantLogos((path) => deps.api.url(path));

  const announce = (text: string) => {
    if (status.textContent !== text) status.textContent = text;
  };

  function message(title: string, text: string): void {
    replace(card, h('h1', { class: 'display', id: 'title' }, title), h('p', {}, text));
    announce(text);
  }

  function hero(a: PublicAddress, withPrice: boolean): HTMLElement {
    return h('div', { class: 'hero' },
      logoFor(a.merchant),
      h('h1', { class: 'display', id: 'title' }, a.product.name),
      a.product.description ? h('p', { class: 'muted description', 'data-fid': 'description' }, a.product.description) : null,
      withPrice ? h('p', { class: 'amount-hero', 'data-fid': 'price' }, a.open_amount || !a.amount ? COPY.anyAmount : `${formatAmount(a.amount)} USD`) : null);
  }

  /** The session still open on an address that cannot open new ones (rotated, product off). */
  function continueLink(a: PublicAddress): HTMLElement | null {
    if (!a.session) return null;
    return h('div', { class: 'stack-tight' }, h('p', { class: 'small' }, COPY.addressOpenSession),
      h('a', { class: 'btn btn-primary', href: `/pay/${encodeURIComponent(a.session.id)}`, 'data-fid': 'continue' }, 'Continue to payment', icon('arrow')));
  }

  function render(): void {
    const a = addr;
    if (!a) return;
    document.title = `${a.product.name} · Prismaport`;
    const parts: (Node | null)[] = [];
    if (!a.livemode) parts.push(h('p', { class: 'note' }, icon('info'), h('span', {}, COPY.testMode)));
    if (offline) parts.push(offlineNotice(lastOk, Date.now(), loadNow));

    if (a.status === 'rotated') {
      parts.push(hero(a, false),
        h('div', { class: 'stack-tight', 'data-fid': 'replaced' },
          h('h2', { class: 'heading' }, 'This address was replaced'),
          h('p', { class: 'note', 'data-emphasis': true, role: 'alert' }, icon('info'), h('span', {}, COPY.addressReplaced))),
        a.replaced_by ? h('a', { class: 'btn btn-primary', href: `/a/${encodeURIComponent(a.replaced_by)}`, 'data-fid': 'new-address' }, 'Go to the new address', icon('arrow')) : null,
        continueLink(a));
      replace(card, ...parts.filter((x): x is Node => x !== null));
      announce('This address was replaced.');
      return;
    }
    if (!a.live || a.pay_to.length === 0) {
      parts.push(hero(a, false), h('p', {}, COPY.addressUnavailable), continueLink(a));
      replace(card, ...parts.filter((x): x is Node => x !== null));
      announce(COPY.addressUnavailable);
      return;
    }

    const sel = a.pay_to.find((p) => payToKey(p) === selected) ?? a.pay_to[0]!;
    const button = h('button', { type: 'button', class: 'btn btn-primary', 'data-fid': 'start', disabled: busy, onclick: () => void start() },
      busy ? 'Opening the payment' : startLabel(a), busy ? null : icon('arrow'));
    parts.push(hero(a, true),
      h('p', { class: 'small muted' }, a.open_amount ? COPY.anyAmountDetail : 'You send exactly this amount, in the stablecoin you choose.'),
      h('div', { class: 'stack-tight' },
        button,
        error ? h('p', { class: 'note', 'data-emphasis': true, role: 'alert', 'data-fid': 'start-error' }, icon('info'), h('span', {}, error)) : null,
        h('p', { class: 'small muted' }, a.session ? COPY.addressOpenSession : COPY.addressStart)),
      h('p', { class: 'divider' }, 'Your deposit address'));
    if (a.pay_to.length > 1) {
      parts.push(networkPicker(a.pay_to, sel, (key) => {
        selected = key;
        fullAddress = false;
        render();
      }));
    }
    if (sel.paused) parts.push(h('p', { class: 'note', 'data-emphasis': true, role: 'status', 'data-fid': 'paused-note' }, icon('info'), h('span', {}, PAUSED.addressRow(networkName(sel.chain)))));
    parts.push(depositBlock({
      payTo: sel, networks,
      due: a.open_amount ? null : a.amount,
      message: a.product.name,
      fullAddress,
      onToggleAddress: () => { fullAddress = !fullAddress; render(); },
    }), h('p', { class: 'small muted', 'data-fid': 'static-note' }, COPY.addressStatic));
    replace(card, ...parts.filter((x): x is Node => x !== null));
  }

  async function load(): Promise<void> {
    try {
      const r = await deps.api.address(addressId, visible());
      lastOk = Date.now();
      const wasOffline = offline;
      offline = false;
      const json = JSON.stringify(r.data);
      addr = r.data;
      // Back online: the notice goes even when the address did not change.
      if (json !== lastJson || wasOffline) {
        lastJson = json;
        render();
      }
    } catch (e) {
      const code = e instanceof ApiError ? e.code : 'network_error';
      if (code === 'customer_address_not_found') {
        stopped = true;
        message('Address not found', COPY.addressNotFound);
        return;
      }
      if (!addr) {
        message('Deposit address', errorCopy(code));
      } else if (!offline) {
        // What the page shows still holds, but it can no longer tell whether it changed.
        offline = true;
        render();
      }
    }
  }

  /** Loads now instead of at the next poll ("Try now", back online). */
  function loadNow(): void {
    if (!stopped) void load();
  }

  async function start(): Promise<void> {
    if (busy || !addr) return;
    busy = true;
    error = null;
    render();
    try {
      const { data } = await deps.api.openAddressSession(addressId);
      navigate(`/pay/${encodeURIComponent(data.id)}`);
      return;
    } catch (e) {
      const code = e instanceof ApiError ? e.code : 'network_error';
      busy = false;
      if (code === 'address_rotated' || code === 'product_inactive' || code === 'customer_address_not_found') {
        // The address changed since the page loaded: show what it is now.
        lastJson = '';
        await load();
        return;
      }
      error = code === 'service_unavailable' ? 'Payments to this address are not available right now. Try again later.' : errorCopy(code, e instanceof ApiError ? e.retryAfter : undefined, e instanceof ApiError ? e.status : undefined);
      render();
      announce(error);
    }
  }

  function schedule(): void {
    clearTimeout(timer);
    if (stopped) return;
    timer = setTimeout(async () => {
      try {
        if (visible()) await load();
      } finally {
        schedule();
      }
    }, pollMs);
  }

  const onVisible = () => {
    if (visible()) loadNow();
  };
  document.addEventListener('visibilitychange', onVisible);
  window.addEventListener('online', loadNow);
  // Only the offline notice's "Last checked" moves between polls.
  const ticker = setInterval(() => {
    if (offline) tickChecked(card, lastOk, Date.now());
  }, 1000);

  // The network list is only needed for payment-request QR codes; without it the QR is the address.
  const nets = deps.api.networks().then((r) => { networks = r.data.data; }).catch(() => undefined);
  await Promise.all([load(), nets]);
  if (addr && !stopped) render();
  schedule();
  return {
    stop: () => {
      stopped = true;
      clearTimeout(timer);
      clearInterval(ticker);
      document.removeEventListener('visibilitychange', onVisible);
      window.removeEventListener('online', loadNow);
    },
  };
}
