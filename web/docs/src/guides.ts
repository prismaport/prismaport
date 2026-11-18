// Task and concept guides: quickstart, payment lifecycle, networks, links/addresses/splits and
// help. Every number here is checked against the code it names in tests.
import { esc, highlightCode, md, pretty } from './format';
import { EVM_CONFIRMATIONS, FAUCETS, FEE_BPS, FIXED_FEE, MIN_AMOUNT, NETWORKS, COMING_LATER, openNetworks } from './networks';
import { codeBox, DASHBOARD_KEYS, DASHBOARD_WEBHOOKS, handlerBox, ICON_INFO, langTabs, stateDiagram, type ViewState } from './pages';
import { snippet } from './snippets';
import { apiBase, events, operations, type Operation } from './spec';

const op = (id: string): Operation => {
  const found = operations.find((o) => o.id === id);
  if (!found) throw new Error(`no operation ${id} in openapi.json`);
  return found;
};
const ref = (id: string, text?: string) => {
  const o = op(id);
  return `<a href="#${o.id}">${esc(text ?? `${o.method} ${o.path}`)}</a>`;
};
const ic = (s: string) => `<code class="inline-code">${esc(s)}</code>`;
const DASHBOARD_WALLETS = '/dashboard/settings/networks';
const DASHBOARD_START = '/dashboard/get-started';
/** Default watch after expiry: internal/service/recheck.go DefaultWatchAfterExpiry (a server setting). */
export const WATCH_AFTER_EXPIRY = '23 h 30 min';
/** The smallest balance the fee rule settles: the least gross with 0.10 + floor(gross × 0.5%) below it. */
export const MIN_SETTLING = '0.100503';

/* ---------- quickstart ---------- */

/** The operations the quickstart calls, in order. Their snippets come from the spec examples. */
export const QUICKSTART_OPS = ['listWallets', 'createProduct', 'createWebhookEndpoint', 'createSession', 'simulateSession', 'getSession'];

/** One step's request box. The quickstart is a test-mode walk-through, so it always shows test keys. */
export function quickstartBox(step: number, state: ViewState): string {
  const o = op(QUICKSTART_OPS[step]!);
  return `<div class="codebox" data-box="qs" data-step="${step}"><div class="codebox-head">${langTabs(state)}<button class="copy" type="button">Copy</button></div><pre>${highlightCode(snippet(o, state.lang, false))}</pre></div>`;
}

function step(n: number, title: string, prose: string, aside: string): string {
  return `<h2>${n}. ${title}</h2><div class="split"><div class="prose">${prose}</div><div class="sticky">${aside}</div></div>`;
}

export function renderQuickstart(state: ViewState): string {
  const box = (i: number) => quickstartBox(i, state);
  return (
    '<div class="crumb">Start here</div><h1>First payment in 5 minutes</h1>' +
    '<p class="lede">From a test key to a paid session in test mode, with the same calls you will make in live mode. Test mode runs on Base Sepolia, and test tokens have no value.</p>' +
    step(
      1,
      'Get a test key',
      `<p>Sign in to the dashboard, switch to <b>Test</b> and create a key under <a href="${DASHBOARD_KEYS}">Developers, API keys</a>. It shows once. Keep it on your server: every call below sends it as a bearer token.</p>`,
      codeBox('Shell', 'export PRISMAPORT_SECRET_KEY=sk_test_…'),
    ) +
    step(
      2,
      'Add a wallet',
      `<p>Payouts go to your own wallets. Add them in the dashboard under <a href="${DASHBOARD_WALLETS}">Settings, Networks</a> or select <b>Use a sample test wallet</b> on <a href="${DASHBOARD_START}">Get started</a>. The API only reads wallets: list them to get the ids for the payout.</p>`,
      box(0),
    ) +
    step(
      3,
      'Create a product',
      `<p>A product is what you sell: a name, a price and who gets paid on each network. <code>payout</code> maps a network to 1 to 5 wallets whose shares add up to <code>100.00</code>. A network you leave out pays your oldest active wallet of its family in full. Prices start at ${MIN_AMOUNT} USD. See ${ref('createProduct')}.</p>`,
      box(1),
    ) +
    step(
      4,
      'Add a webhook endpoint',
      `<p>We POST a signed event to it when a session changes. Keep the <code>secret</code> from the answer: it shows only here and when you rotate it. In test mode the URL can be <code>http://localhost</code>, or the sandbox inbox under <a href="${DASHBOARD_WEBHOOKS}">Developers, Webhooks</a>.</p>`,
      box(2),
    ) +
    step(
      5,
      'Open a session for the order',
      `<p>One session per order, with one <code>Idempotency-Key</code> per order so a retry never opens two. Send the payer to <code>checkout_url</code>. Put your order id in <code>client_reference_id</code>: it comes back in every event.</p>`,
      box(3),
    ) +
    step(
      6,
      'Pay it',
      '<p><b>With test tokens.</b> Open <code>checkout_url</code>, pick a network and select <b>Generate payment</b>, then send the exact amount. Get test tokens here:</p>' +
        `<ul>${FAUCETS.map(([label, href, what]) => `<li><a href="${href}" rel="noopener">${esc(label)}</a>: ${esc(what)}.</li>`).join('')}</ul>` +
        `<p><b>Without tokens.</b> ${ref('simulateSession')} runs one step through the same state machine and webhooks as a real payment. Its payments carry <code>simulated: true</code> and move no funds. A test-mode checkout has the same tool in its Simulate panel. Both answer <code>403 test_mode_only</code> in live mode.</p>`,
      box(4),
    ) +
    step(
      7,
      'Receive session.paid',
      '<p>Check the <code>Signature</code> header over the raw body, answer 2xx within 10 seconds, and dedupe by event <code>id</code>: deliveries are at least once. More on the <a href="#webhooks">Webhooks</a> page.</p>',
      handlerBox(state),
    ) +
    step(
      8,
      'Read the session, then fulfil',
      `<p>An event tells you to look; the session is what you act on. Fetch it, check that <code>status</code> is <code>paid</code> or <code>overpaid</code> and that <code>client_reference_id</code> is your order, then fulfil. What to do in every other status: <a href="#lifecycle">Payment lifecycle</a>.</p>`,
      box(5),
    ) +
    `<div class="callout">${ICON_INFO}<span><b>Going live.</b> The same calls with an <code class="inline-code">sk_live_</code> key, on Base. Live mode is open as soon as you confirm your email. Live keys and wallets ask for your password when you create them.</span></div>`
  );
}

/* ---------- payment lifecycle ---------- */

type Act = 'fulfil' | 'wait' | 'top-up' | 'reconcile' | 'review';
const ACT_PILL: Record<Act, [string, string]> = {
  fulfil: ['paid', 'Fulfil'],
  wait: ['idle', 'Wait'],
  'top-up': ['partial', 'Ask for a top-up'],
  reconcile: ['partial', 'Reconcile'],
  review: ['partial', 'Review'],
};

/** Every session status: what it means and what to do. Keys are the Session.status enum. */
export const STATUS_GUIDE: Record<string, [string, Act, string]> = {
  open: [
    'Nothing confirmed yet. A new session waits up to 1 hour for the payer to choose a network; the first Generate payment starts the pay window (`expires_in`, default 30 minutes).',
    'wait',
    'Show the order as awaiting payment.',
  ],
  partially_paid: [
    'Confirmed payments add up to less than `amount`. The session stays open until `expires_at`.',
    'top-up',
    'Ask the payer to send `amount_remaining` to the same deposit address before `expires_at`.',
  ],
  paid: ['Confirmed payments equal `amount` exactly, in the token base units.', 'fulfil', 'Read the session and fulfil.'],
  overpaid: ['Confirmed payments exceed `amount`. The excess settles to you with the rest.', 'fulfil', 'Fulfil. Returning the excess is your decision, made off-platform.'],
  expired: [
    'The expiry passed before the session was paid, with or without partial funds. Partial funds still settle to you.',
    'reconcile',
    'Do not fulfil. If `amount_received` is above zero, settle it with the payer: deliver part, ask for a new payment, or refund them yourself.',
  ],
  late_payment: [
    'A payment first seen after the expiry was confirmed. The funds still reach you.',
    'reconcile',
    'Compare `amount_received` with the order, then fulfil or refund the payer yourself.',
  ],
  under_review: [
    'Held out of the paid flow and not settled. Reserved for screening, which is not live: today only the test-mode simulator sets it.',
    'review',
    'Do not fulfil while the session is under review.',
  ],
};

/** Every webhook event: when it fires and what to do. Keys are the document's webhooks. */
export const EVENT_GUIDE: Record<string, [Act, string]> = {
  'session.payment_detected': ['wait', 'Show "payment seen". It does not count yet, so do not fulfil.'],
  'session.partially_paid': ['top-up', 'Tell the payer what is still due (`amount_remaining`).'],
  'session.paid': ['fulfil', 'Read the session, then fulfil.'],
  'session.overpaid': ['fulfil', 'Read the session, then fulfil. The excess is yours.'],
  'session.expired': ['reconcile', 'Close the order. Reconcile any partial funds.'],
  'session.late_payment': ['reconcile', 'Fulfil if it covers the order, otherwise refund the payer yourself.'],
  'session.under_review': ['review', 'Hold the order.'],
  'payment.reversed': ['reconcile', 'Read the session again: its status is recomputed. If you fulfilled on a status it no longer has, follow up with the payer.'],
};

const docEvents = new Map(events.map((e) => [e.type, e.description]));
const pill = (a: Act) => `<span class="pill ${ACT_PILL[a][0]}">${ACT_PILL[a][1]}</span>`;

export function renderLifecycle(): string {
  const statuses = Object.entries(STATUS_GUIDE)
    .map(([s, [means, act, todo]]) => `<tr><td><code>${s}</code></td><td>${md(means)}</td><td>${pill(act)} ${md(todo)}</td></tr>`)
    .join('');
  const evs = Object.entries(EVENT_GUIDE)
    .map(([e, [act, todo]]) => `<tr><td><code>${e}</code></td><td>${md(docEvents.get(e) ?? '')}</td><td>${pill(act)} ${md(todo)}</td></tr>`)
    .join('');
  return (
    '<div class="crumb">Guides</div><h1>Payment lifecycle</h1>' +
    '<p class="lede">What each session status and event means, and what to do about it. Act on the session you read with <a href="#getSession">GET /v1/sessions/{id}</a>, never on an event alone: events can arrive late, twice or out of order.</p>' +
    stateDiagram() +
    `<h2>Statuses</h2><div class="tbl-wrap"><table class="tbl"><thead><tr><th>Status</th><th>Means</th><th>Do</th></tr></thead><tbody>${statuses}</tbody></table></div>` +
    `<h2>Events</h2><div class="tbl-wrap"><table class="tbl"><thead><tr><th>Event</th><th>When</th><th>Do</th></tr></thead><tbody>${evs}</tbody></table></div>` +
    '<div class="split"><div class="prose">' +
    '<h2>When a payment counts</h2>' +
    `<p>A transfer is <code>detected</code> when first seen, and counts towards the session once <code>confirmed</code>: after ${EVM_CONFIRMATIONS} blocks on Base. Until it is <code>finalized</code> a reorg can still remove it, which sends <code>payment.reversed</code>; a finalized payment is never reversed. Per network: <a href="#networks">Networks and assets</a>.</p>` +
    '<h2>Late funds</h2>' +
    `<p>Funds that reach a deposit address after the expiry still settle to you, and the session becomes <code>late_payment</code>. We keep watching for ${WATCH_AFTER_EXPIRY} after the expiry. After that, funds are found only when you ask: ${ref('recheckSession')} or <b>Check again</b> in the dashboard.</p>` +
    '<h2>Refunds</h2>' +
    '<p>The gateway never refunds. A deposit address can only pay the fee and the wallets in your split, fixed when it was derived, so nobody can send funds back from it. And the address a payment came from is often an exchange hot wallet: sending back to it can lose the money.</p>' +
    '<p>To refund, pay the payer yourself, from your wallet, off-platform, after the payer confirms an address they control. An overpaid excess is already yours, and so are late funds: they settle like the rest.</p>' +
    '<h2>Wrong network or token</h2>' +
    '<p>A deposit address is watched only for the session asset on the session network. Another token, the native coin, or the same address on another network is never detected or credited. It stays at the address and is never refunded. Prismaport may forward a well-known token to your wallet, at its own discretion and with no promise; anything else stays there for good.</p>' +
    '<h2 id="unrecoverable">Funds that cannot be recovered</h2>' +
    '<p>A deposit address has no owner: it can only pay the fee and your split. Only the treasury can forward anything else, and only to those same wallets. Treat these funds as lost, because no support request can force it:</p>' +
    '<ul id="unrecoverable-list">' +
    '<li><b>The native coin.</b> ETH sent to a Base deposit address (and SOL on a Solana deposit address, once Solana opens). Once a Base address has been used, an ETH send to it fails and the ETH stays with the sender.</li>' +
    '<li><b>A token the address does not accept.</b> Any token other than the assets the session offered on that network: other stablecoins, bridged USDC (USDbC) on Base, or any other token.</li>' +
    '<li><b>Token-2022 tokens on Solana (coming later).</b> Deposit addresses move classic SPL Token accounts only.</li>' +
    '<li><b>Tokens with fewer than 6 or more than 36 decimals.</b> The fee rule refuses them, so they can never settle.</li>' +
    '<li><b>The right address on the wrong network.</b> A Base deposit address exists on every EVM network (Ethereum, Arbitrum, Polygon and others), but only Base is watched and only the Base address can forward. Funds sent to it elsewhere are lost.</li>' +
    '</ul>' +
    '<p>Tell payers to check the network and the token in the checkout before they send. A balance that is only too small to settle is different: it is not lost, see below.</p>' +
    '<h2>Too small to settle</h2>' +
    `<p>A balance settles only when the fee is below it. With ${FIXED_FEE} plus ${FEE_BPS / 100}%, the smallest balance that settles is ${MIN_SETTLING} USDC. A smaller one stays at the deposit address; if more funds reach the same address and the sum can settle, it settles as usual. An open-amount session never turns <code>paid</code> on such a balance. Prices start at ${MIN_AMOUNT} USD.</p>` +
    '</div><div class="sticky">' +
    codeBox(
      'Fulfil from the session · Node',
      `const res = await fetch(\`${apiBase}/v1/sessions/\${sessionId}\`, {\n` +
        "  headers: { Authorization: `Bearer ${process.env.PRISMAPORT_SECRET_KEY}` },\n});\nconst session = await res.json();\nif (session.client_reference_id !== order.id) return;\nif (session.status === 'paid' || session.status === 'overpaid') {\n  await fulfil(order); // once: keep it idempotent on order.id\n}",
    ) +
    '</div></div>'
  );
}

/* ---------- networks ---------- */

export function renderNetworks(state: ViewState): string {
  const rows = NETWORKS.filter((n) => !n.comingLater).flatMap((n) =>
    n.assets.map(
      (a) =>
        `<tr><td><b>${esc(n.name)}</b><br><code>${n.id}</code></td><td>${n.live ? 'Live' : 'Test'}</td><td>${a.symbol}</td>` +
        `<td><a href="${n.explorer(a.token)}" rel="noopener"><code class="wrap">${a.token}</code></a></td><td>${a.decimals}</td><td>${esc(n.credit)}</td><td>${esc(n.final)}</td></tr>`,
    ),
  );
  // Networks that are not open yet collapse to one muted row each (live ones only; their test network opens with them).
  for (const n of COMING_LATER) {
    rows.push(
      `<tr class="soon"><td><b>${esc(n.name)}</b></td><td><span class="tag-soon">Coming later</span></td><td>${n.assets.map((a) => a.symbol).join(', ')}</td>` +
        `<td>Listed when ${esc(n.name)} opens</td><td>${n.assets[0]!.decimals}</td><td>${esc(n.credit)}</td><td>${esc(n.final)}</td></tr>`,
    );
  }
  const shown = openNetworks(state.live);
  return (
    '<div class="crumb">Guides</div><h1>Networks and assets</h1>' +
    '<p class="lede">Stablecoins only, priced 1:1 in USD. Live mode pays on Base, test mode on Base Sepolia. Solana is coming later. The same fee on every network.</p>' +
    `<div class="tbl-wrap"><table class="tbl" id="networks-table"><thead><tr><th>Network</th><th>Mode</th><th>Asset</th><th>Token contract or mint</th><th>Decimals</th><th>Counts after</th><th>Final when</th></tr></thead><tbody>${rows.join('')}</tbody></table></div>` +
    '<div class="split"><div class="prose">' +
    '<h2>Fee</h2>' +
    `<p>${FIXED_FEE} in the token paid plus ${FEE_BPS / 100}% of each settlement, the percentage rounded down: <span class="mono">fee = ${FIXED_FEE} + floor(gross × ${FEE_BPS} / 10 000)</span>. It is taken once per settlement, not per payment, and fixed in each deposit address when it is derived.</p>` +
    '<h2>Minimums</h2>' +
    `<p>A product or session price starts at ${MIN_AMOUNT} USD (<code>amount_below_minimum</code> below it). A balance settles only when the fee is below it, so the smallest one that settles is ${MIN_SETTLING} USDC; see <a href="#lifecycle">Payment lifecycle</a>.</p>` +
    '<h2>Confirmations</h2>' +
    `<p>A payment counts after ${EVM_CONFIRMATIONS} blocks on Base (about 20 seconds). Solana, when it opens, counts a payment at the confirmed commitment. Before that it is <code>detected</code>, and a session never turns <code>paid</code> on it. A payment is <code>finalized</code> once its block or slot is final with the same hash; until then a reorg can reverse it.</p>` +
    '<h2>Ways to pay</h2>' +
    '<p>Every network takes a transfer to the deposit address. USDC on Base and Base Sepolia can also be paid by an EIP-3009 signature, which Prismaport sends on chain, paying the network fee. Solana, when it opens, will also take a transaction the checkout builds, or Solana Pay.</p>' +
    '<h2>Test tokens</h2>' +
    `<ul>${FAUCETS.map(([label, href, what]) => `<li><a href="${href}" rel="noopener">${esc(label)}</a>: ${esc(what)}.</li>`).join('')}</ul>` +
    `<p>Or skip tokens entirely with ${ref('simulateSession')}.</p>` +
    '</div><div class="sticky">' +
    codeBox('The registry · public, no key', `curl ${apiBase}/v1/checkout/networks`) +
    codeBox(
      `Response · ${shown.map((n) => n.name).join(' and ')} (excerpt)`,
      pretty({
        data: shown.map((n) => ({
          id: n.id,
          livemode: n.live,
          fee_bps: FEE_BPS,
          fixed_fee: FIXED_FEE,
          assets: n.assets.map((a) => ({ symbol: a.symbol, token: a.token, decimals: a.decimals, eip3009: a.signed })),
        })),
      }),
      true,
    ) +
    '</div></div>'
  );
}

/* ---------- links, customer addresses and splits ---------- */

export function renderConcepts(state: ViewState): string {
  const net = state.live ? 'base' : 'base-sepolia';
  return (
    '<div class="crumb">Guides</div><h1>Links, addresses and splits</h1>' +
    '<p class="lede">Three ways to take a payment, all ending in a session, and how a settlement is divided between your wallets.</p>' +
    '<div class="split"><div class="prose">' +
    '<h2>Sessions and payment links</h2>' +
    `<p>A <b>session</b> is one payment for one order. Your server opens it with ${ref('createSession')}, with your <code>client_reference_id</code> and <code>metadata</code>, and sends the payer to <code>checkout_url</code>. Use it when you have an order to attach the payment to.</p>` +
    `<p>A <b>payment link</b> (<code>plink_</code>) sells a product with no code: share its <code>url</code>, and each payer who opens it gets a session of their own through ${ref('openLinkSession', 'the public link open')}. A link has its own name, availability (<code>expires_at</code>, <code>max_payments</code>), <code>success_url</code> and metadata, and can set the amount only for an open-price product. An unavailable link answers <code>410 link_unavailable</code>. Create one with ${ref('createPaymentLink')}.</p>` +
    "<p><b>Payment methods.</b> A product's <code>payment_methods</code> lists the <code>{chain, chain_name, asset}</code> pairs its checkout offers: the stablecoins your account accepts that the product takes, on the networks it pays. Per network a product takes all of the stablecoins your account accepts, only some it names in <code>networks</code>, or none. A link and its sessions offer the product's methods; a link has none of its own. A listed pair can still refuse a session, for example when its deposit address cannot be derived.</p>" +
    '<h2>Customer addresses</h2>' +
    `<p>A <b>customer address</b> (${ref('createCustomerAddress')}) is a deposit address that never changes for one of your customers: balance top-ups, or a customer who saves the address in an exchange. Each <code>customer_id</code> has one active address per mode, with one <code>pay_to</code> entry per network and asset of the product. Different customers never share an address.</p>` +
    '<ul>' +
    '<li><b>Watched only while a session is open.</b> Open one with <a href="#createSession">POST /v1/sessions</a> and <code>customer_address_id</code>, or let the payer open one on the address page. One session at a time: while one is open, the call returns it.</li>' +
    '<li><b>Open-amount sessions send <code>session.paid</code> once.</b> A session opened without an amount is <code>paid</code> as soon as its confirmed sum settles after the fee, and keeps crediting payments until <code>expires_at</code>. A later payment sends <code>session.payment_detected</code> only: no event says it confirmed, so read the session (<code>amount_received</code>, <code>payments[].status</code>) before you credit it.</li>' +
    '<li><b>Funds between sessions go to the next one.</b> What reaches the address while no session is open is credited to the next session on it, as a normal payment.</li>' +
    `<li><b>Rotation.</b> ${ref('rotateCustomerAddress')} creates a new active address with the product's payout and fee now, and marks the old one <code>rotated</code> with <code>replaced_by</code>. A session already open on the old one runs to its end; a new one there answers <code>409 address_rotated</code>, and funds sent to it later are not watched. Rotate after a payout, wallet or fee change.</li>` +
    '</ul>' +
    '<h2>Payout splits</h2>' +
    '<p>A product\'s <code>payout</code> sets, per network, 1 to 5 distinct wallets with shares above zero, at most two decimals, adding up to exactly <code>100.00</code> (10 000 basis points). At settlement the fee comes off the gross first; every recipient after the first gets <span class="mono">floor(net × bps / 10 000)</span>, and the first recipient gets what is left, so no base unit is lost.</p>' +
    '<p>The split is fixed in each deposit address when it is derived. Changing a product payout reaches new sessions only, and a customer address after you rotate it. Try the arithmetic on the <a href="#contracts">Smart contracts</a> page.</p>' +
    '</div><div class="sticky">' +
    codeBox(
      `70/30 split of 25.00 USDC on ${state.live ? 'Base' : 'Base Sepolia'}`,
      `gross   25.000000\nfee      0.225000   ${FIXED_FEE} + floor(25.000000 × ${FEE_BPS} / 10 000)\nnet     24.775000\n\nsecond   7.432500   floor(24.775000 × 3000 / 10 000)\nfirst   17.342500   net − second`,
    ) +
    codeBox(
      'payout on a product',
      pretty({
        payout: {
          [net]: [
            { wallet_id: 'wal_01M5HQPZG07Z0FT53C2WBPX0G0', share: '70.00' },
            { wallet_id: 'wal_01M5HR04F0W4S9Y7FMX0PMPMDS', share: '30.00' },
          ],
        },
      }),
      true,
    ) +
    '</div></div>'
  );
}

/* ---------- help ---------- */

const FAQ: [string, string][] = [
  [
    'The customer paid with the wrong token, or on the wrong network.',
    'It is not watched or credited, and it does not pay the session. It stays at the address and is never refunded; Prismaport may forward a well-known token to your wallet, at its own discretion and with no promise. Ask the customer for a new payment in the right asset.',
  ],
  [
    'The customer paid too little.',
    'The session is `partially_paid`. Before `expires_at` the customer can send `amount_remaining` to the same deposit address. If it expires first, what arrived still settles to you: deliver part, ask for a new payment, or refund it yourself.',
  ],
  [
    'The customer paid after the expiry.',
    `The session becomes \`late_payment\` and the funds still reach you. We watch for ${WATCH_AFTER_EXPIRY} after the expiry; for funds sent later, call \`POST /v1/sessions/{id}/recheck\` or select Check again in the dashboard.`,
  ],
  [
    'How do I refund?',
    'Yourself, from your own wallet. The gateway never refunds: a deposit address can only pay your split and the fee. Ask the payer for an address they control first; the address a payment came from is often an exchange.',
  ],
  [
    'How long until the funds arrive?',
    `A payment counts after ${EVM_CONFIRMATIONS} Base blocks (about 20 seconds). Once the session closes, one transaction forwards its balance to your wallets; \`settlement\` on the session shows it, with its \`tx_hash\`.`,
  ],
  [
    'What is the difference between test and live mode?',
    'The key prefix sets it: `sk_test_` works on Base Sepolia with test tokens, `sk_live_` on Base. Objects never cross modes. The simulator works only in test mode. Live mode is open as soon as you confirm your email.',
  ],
];

/** One term per concept, as the API names it. */
export const GLOSSARY: [string, string][] = [
  ['Session', 'One payment for one order (`ses_`), priced in USD. Its status follows the payments confirmed for it.'],
  ['pay_to', 'The list of ways to pay a session: one entry per network and asset, with its amount, and its deposit address once generated.'],
  ['Deposit address', 'The address in a `pay_to` entry that the payer sends to. Derived for the session or the customer address, it can only forward to your split and the fee.'],
  ['Forwarder', 'The contract behind a deposit address: an ERC-1167 clone on Base (a program-derived address on Solana, coming later).'],
  ['Flush', 'The transaction that empties a forwarder: the fee to Prismaport, then each share to its wallet. Anyone can send it.'],
  ['Settlement', 'What a flush or a signed payment paid out: gross, fee, net and each payout, in `settlement` on the session.'],
  ['Finalization', 'A confirmed payment whose block or slot is final. A reorg can reverse a payment until then, never after.'],
  ['Customer address', 'A deposit address that stays the same for one customer, watched while a session on it is open.'],
  ['Payment link', 'A shareable page that sells a product (`plink_`); each payer who opens it gets a session.'],
  ['Payment method', 'A network and asset pair a product checkout offers, in `payment_methods`.'],
  ['Dead letter', 'Where a webhook delivery goes after its last retry. Resend it from the dashboard.'],
  ['Test mode, live mode', 'Two separate sets of data, chosen by the key prefix: test networks and tokens, or real ones.'],
];

export function renderHelp(): string {
  return (
    '<div class="crumb">Guides</div><h1>Help and glossary</h1>' +
    '<p class="lede">What payers and merchants ask most, and the words these pages use.</p>' +
    '<div class="split"><div class="prose">' +
    '<h2>Questions</h2>' +
    FAQ.map(([q, a]) => `<h3>${esc(q)}</h3><p>${md(a)}</p>`).join('') +
    '</div><div class="sticky"><section class="try" aria-label="Glossary"><div class="try-head"><strong>Glossary</strong></div><div class="try-body">' +
    `<dl class="kv" id="glossary">${GLOSSARY.map(([t, d]) => `<dt>${esc(t)}</dt><dd>${md(d)}</dd>`).join('')}</dl>` +
    '</div></section></div></div>'
  );
}
