import { merchantLogos } from '../lib/merchant';
import { ApiError, type CheckoutApi, type PublicLink } from '../lib/api';
import { COPY, PAUSED, errorCopy } from '../lib/copy';
import { h, icon } from '../lib/dom';
import { compareAmounts, formatAmount, parseUsdInput, type UsdInputError } from '../lib/format';
import { brandHeader, footer, messageCard, renderFailedCard, replace } from '../ui';

export interface LinkDeps {
  api: CheckoutApi;
  navigate?: (path: string) => void;
  newKey?: () => string;
  /** From the link URL (?ref=), already checked; sent with the session it opens. */
  clientReference?: string;
  /** Waits before each silent retry of a first load that could not reach Prismaport, in ms. */
  retryMs?: number[];
}

/**
 * The first load retries a failed connection three times, backing off, before it says so. A
 * pressed "Try again" is one attempt at once: the payer is waiting on it.
 */
const LOAD_RETRY_MS = [1000, 2000, 4000];

const INPUT_ERRORS: Record<UsdInputError, string> = {
  empty: 'Enter an amount.',
  format: 'Enter an amount in USD, such as 10.00.',
  precision: 'Use at most two decimals, such as 10.50.',
  zero: 'Enter an amount above zero.',
};

/** Checks a typed amount against the product's bounds. Returns the normalized value or why not. */
export function validateOpenAmount(raw: string, p: Pick<PublicLink, 'min_amount' | 'max_amount'>): { value: string } | { error: string } {
  const parsed = parseUsdInput(raw);
  if ('error' in parsed) return { error: INPUT_ERRORS[parsed.error] };
  if (p.min_amount && compareAmounts(parsed.value, p.min_amount) < 0) return { error: `Enter at least ${formatAmount(p.min_amount)} USD.` };
  if (p.max_amount && compareAmounts(parsed.value, p.max_amount) > 0) return { error: `Enter at most ${formatAmount(p.max_amount)} USD.` };
  return { value: parsed.value };
}

export function rangeHint(p: Pick<PublicLink, 'min_amount' | 'max_amount'>): string {
  if (p.min_amount && p.max_amount) return `Between ${formatAmount(p.min_amount)} and ${formatAmount(p.max_amount)} USD.`;
  if (p.min_amount) return `At least ${formatAmount(p.min_amount)} USD.`;
  return 'Any amount in USD.';
}

function submitError(err: unknown, p: PublicLink): string {
  if (!(err instanceof ApiError) || err.code === 'network_error') return 'Cannot reach Prismaport right now. Check your connection and try again.';
  if (err.code === 'amount_below_minimum' && p.min_amount) return `Enter at least ${formatAmount(p.min_amount)} USD.`;
  if (err.code === 'amount_above_maximum' && p.max_amount) return `Enter at most ${formatAmount(p.max_amount)} USD.`;
  if (err.code === 'chain_unavailable') return PAUSED.unavailable;
  if (err.code === 'service_unavailable') return 'Payments for this link are not available right now. Try again later.';
  return errorCopy(err.code, err.retryAfter, err.status);
}

/** The link, retried silently while the connection fails; any answer from the server is final. */
async function loadLink(api: CheckoutApi, linkId: string, waits: number[]): Promise<PublicLink> {
  for (let attempt = 0; ; attempt++) {
    try {
      return (await api.link(linkId)).data;
    } catch (e) {
      if ((e instanceof ApiError && e.code !== 'network_error') || attempt >= waits.length) throw e;
      await new Promise((r) => setTimeout(r, waits[attempt]));
    }
  }
}

const loading = () => h('p', { class: 'skeleton' }, 'Loading the payment details.');

/** The pressed "Try again" went with the skeleton: unless a new one took focus, the card's title gets it. */
function focusTitle(card: HTMLElement): void {
  const active = document.activeElement;
  const title = card.querySelector<HTMLElement>('#title');
  if (!title || (active && active !== document.body)) return;
  title.tabIndex = -1;
  title.focus({ preventScroll: true });
}

export async function mountLink(root: HTMLElement, linkId: string, deps: LinkDeps): Promise<void> {
  const card = h('section', { class: 'card stack', 'aria-labelledby': 'title' }, loading());
  root.replaceChildren(brandHeader(), card, footer());
  root.removeAttribute('aria-busy');

  // A product URL from before links were their own: nothing was shared, so it is simply gone.
  if (linkId.startsWith('prod_')) {
    replace(card, ...messageCard('Payment link', errorCopy('link_unavailable'), 'title').childNodes);
    return;
  }
  await showLink(card, linkId, deps, false);
}

/** Loads the link into the card: its form, or why it cannot be shown. `again` is a "Try again" press. */
async function showLink(card: HTMLElement, linkId: string, deps: LinkDeps, again: boolean): Promise<void> {
  const navigate = deps.navigate ?? ((path: string) => window.location.assign(path));
  const newKey = deps.newKey ?? (() => crypto.randomUUID());
  let product: PublicLink;
  try {
    product = await loadLink(deps.api, linkId, again ? [] : deps.retryMs ?? LOAD_RETRY_MS);
  } catch (e) {
    const code = e instanceof ApiError ? e.code : 'network_error';
    if (code === 'network_error') {
      const retry = h('button', { type: 'button', class: 'btn btn-primary', 'data-fid': 'try-again', onclick: () => {
        replace(card, loading());
        void showLink(card, linkId, deps, true)
          // Out of mountLink's promise nothing else catches a throw: say so rather than load forever.
          .catch(() => replace(card, ...renderFailedCard().childNodes))
          .then(() => focusTitle(card));
      } }, icon('refresh'), 'Try again');
      replace(card, ...messageCard('Payment link', COPY.linkLoadFailed, 'title').childNodes, retry);
      // The pressed button went with the skeleton: give focus back to the new one.
      if (again) retry.focus();
      return;
    }
    replace(card, ...messageCard(code === 'payment_link_not_found' || code === 'product_not_found' ? 'Link not found' : 'Payment link', errorCopy(code), 'title').childNodes);
    return;
  }
  document.title = `${product.name} · Prismaport`;
  const unavailable = () => replace(card, h('h1', { class: 'display', id: 'title' }, product.name), h('p', {}, errorCopy('link_unavailable')));
  if (!(product.live ?? product.active)) {
    unavailable();
    return;
  }

  // One key per payer attempt and amount: a retry after a dropped connection reuses it and gets
  // the same session back instead of opening a second one.
  const keys = new Map<string, string>();
  const keyFor = (amount: string) => {
    let k = keys.get(amount);
    if (!k) {
      k = newKey();
      keys.set(amount, k);
    }
    return k;
  };

  const error = h('p', { class: 'note', id: 'amount-error', 'data-emphasis': true, role: 'alert', hidden: true });
  const submit = h('button', { type: 'submit', class: 'btn btn-primary', 'data-fid': 'continue' }, 'Continue to payment', icon('arrow'));
  const form = h('form', { class: 'stack', novalidate: true });
  const parts: (Node | null)[] = [];
  if (!product.livemode) parts.push(h('p', { class: 'note' }, icon('info'), h('span', {}, COPY.testMode)));
  const hero = h('div', { class: 'hero' },
    merchantLogos((path) => deps.api.url(path))(product.merchant),
    h('h1', { class: 'display', id: 'title' }, product.name),
    product.description ? h('p', { class: 'muted description', 'data-fid': 'description' }, product.description) : null);
  parts.push(hero);

  let input: HTMLInputElement | null = null;
  // Brings the chips and the hint in line with the field; set once there is a field.
  let sync = () => {};
  if (product.price_type === 'fixed' && product.amount) {
    hero.append(h('p', { class: 'amount-hero' }, `${formatAmount(product.amount)} USD`),
      h('p', { class: 'small muted' }, 'You choose the network and stablecoin on the next step. You send exactly this amount.'));
  } else {
    input = h('input', {
      class: 'input', id: 'amount', name: 'amount', inputmode: 'decimal', autocomplete: 'off', spellcheck: 'false',
      'aria-describedby': 'amount-hint amount-error', placeholder: product.min_amount ? formatAmount(product.min_amount) : '0.00', 'data-fid': 'amount',
    });
    const amountInput = input;
    const chips = h('div', { class: 'chips', role: 'group', 'aria-label': 'Suggested amounts' });
    const hint = h('p', { class: 'small muted', id: 'amount-hint' }, rangeHint(product));
    sync = () => {
      for (const c of chips.querySelectorAll('button')) c.setAttribute('aria-pressed', String(c.dataset.value === amountInput.value));
      // What was typed ("10,50", "$7") is not what is sent: the hint says the amount that is.
      const v = validateOpenAmount(amountInput.value, product);
      hint.replaceChildren(rangeHint(product));
      if ('value' in v && v.value !== amountInput.value.trim()) hint.append(' You pay ', h('span', { class: 'mono' }, v.value), ' USD.');
    };
    for (const s of product.suggested_amounts) {
      const v = formatAmount(s);
      chips.append(h('button', { type: 'button', class: 'btn btn-small chip mono', 'data-value': v, 'aria-pressed': 'false',
        onclick: () => { amountInput.value = v; error.hidden = true; amountInput.removeAttribute('aria-invalid'); sync(); } }, `${v} USD`));
    }
    amountInput.addEventListener('input', () => {
      error.hidden = true;
      amountInput.removeAttribute('aria-invalid');
      sync();
    });
    form.append(h('div', {},
      h('label', { class: 'label', for: 'amount' }, 'Amount'),
      h('div', { class: 'input-wrap' }, amountInput, h('span', { class: 'unit', 'aria-hidden': 'true' }, 'USD')),
      hint));
    if (product.suggested_amounts.length > 0) form.append(chips);
  }
  form.append(error, submit);
  parts.push(form);
  replace(card, ...parts.filter((p): p is Node => p !== null));

  const showError = (text: string) => {
    error.textContent = text;
    error.hidden = false;
    input?.setAttribute('aria-invalid', 'true');
  };

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    let amount: string | undefined;
    if (input) {
      const v = validateOpenAmount(input.value, product);
      if ('error' in v) {
        showError(v.error);
        input.focus();
        return;
      }
      amount = v.value;
      input.value = v.value;
      sync();
    }
    submit.disabled = true;
    submit.textContent = 'Opening the payment';
    try {
      const { data } = await deps.api.openSession(product.id, keyFor(amount ?? ''), amount, deps.clientReference);
      navigate(`/pay/${encodeURIComponent(data.id)}`);
    } catch (err) {
      // The link stopped after this page loaded (deactivated, expired or its limit reached):
      // no retry can open it, so the form goes.
      if (err instanceof ApiError && err.code === 'link_unavailable') {
        unavailable();
        return;
      }
      showError(submitError(err, product));
      submit.disabled = false;
      submit.replaceChildren('Continue to payment', icon('arrow'));
    }
  });
}
