import { esc } from '../format';
import { codeBox } from '../pages';
import { X, type Excerpt } from './excerpts';
import {
  BPS_TOTAL,
  MAX_FEE_BPS,
  MAX_RECIPIENTS,
  formatBaseUnits,
  formatBps,
  formatUnits,
  parseDecimal,
  revertText,
  settle,
  type Outcome,
} from './math';

const DECIMALS = 6;

/** The calculator's inputs as typed, kept across re-renders (mode switches, navigation). */
const S = {
  gross: '30.00',
  feeBps: 50,
  fixed: '0.10',
  shares: ['70.00', '30.00'],
  units: false,
  path: 'deposit' as PathId,
  step: 0,
  attempt: 'swap',
};

const reduceMotion = () => window.matchMedia?.('(prefers-reduced-motion: reduce)').matches === true;
// On a phone the prism drops most of the incoming beam so the exit rays and their labels fit.
const narrow = () => window.matchMedia?.('(max-width: 560px)').matches === true;
const prismViewBox = () => (narrow() ? '110 0 630 330' : '0 0 740 330');
const excerptBoxes = (list: Excerpt[]) => list.map((x) => codeBox(esc(x.source), x.code)).join('');

/* ---------- calculator ---------- */

type View = Outcome | { kind: 'input'; message: string };

function compute(): View {
  const gross = parseDecimal(S.gross.trim(), DECIMALS);
  if (gross === null) return { kind: 'input', message: 'Enter the amount in USDC: digits, with up to 6 decimals.' };
  if (gross === 0n) return { kind: 'input', message: 'Enter an amount above zero. A zero balance has nothing to settle.' };
  const fixed = parseDecimal(S.fixed.trim(), DECIMALS);
  if (fixed === null) return { kind: 'input', message: 'Enter the fixed fee in USD: digits, with up to 6 decimals.' };
  const bps: number[] = [];
  for (const [i, raw] of S.shares.entries()) {
    const u = parseDecimal(raw.trim(), 2);
    if (u === null) return { kind: 'input', message: `Recipient ${i + 1}: enter a percentage with up to two decimals.` };
    if (u > BigInt(BPS_TOTAL)) return { kind: 'input', message: `Recipient ${i + 1}: a share is at most 100%.` };
    bps.push(Number(u));
  }
  return settle({ gross, decimals: DECIMALS, feeBps: S.feeBps, fixedFeeMicros: fixed, bps });
}

const amount = (u: bigint) => (S.units ? formatBaseUnits(u) : formatUnits(u, DECIMALS));
const withUnit = (u: bigint) => amount(u) + (S.units ? '' : ' USDC');

function revertReason(v: Extract<View, { kind: 'revert' }>): string {
  const [arg] = v.revert.args;
  switch (v.revert.error) {
    case 'FeeBpsTooHigh':
      return `The percentage is above the cap of ${formatBps(MAX_FEE_BPS)} (${MAX_FEE_BPS} bps). Nothing moves.`;
    case 'FixedFeeTooHigh':
      return 'The fixed fee is above the cap of 5.00 USD. Nothing moves.';
    case 'ZeroShare':
      return `Recipient ${Number(arg) + 1} has a zero share. Every recipient needs a share above zero.`;
    case 'InvalidShareSum':
      return `The shares add up to ${formatBps(Number(arg))}. They must add up to exactly 100% (10,000 bps), or the Factory does not even compute the address.`;
    default:
      return 'The contracts refuse these parameters. Nothing moves.';
  }
}

function verdictHtml(v: View): string {
  if (v.kind === 'input') return `<div class="cx-verdict cx-note"><b>Check the inputs</b><span>${esc(v.message)}</span></div>`;
  if (v.kind === 'revert') {
    return `<div class="cx-verdict cx-bad"><b>Reverted</b><code>${esc(revertText(v.revert))}</code><span>${esc(revertReason(v))}</span></div>`;
  }
  if (v.kind === 'dust') {
    return `<div class="cx-verdict cx-bad"><b>Not settled</b><span>The fee (${esc(amount(v.wouldBeFee))}) would be at or above the amount (${esc(amount(v.gross))}). With a signature, the Gateway reverts with <code>DustPayment()</code> before using the authorization, so the payer keeps the funds. At a deposit address, <code>flush</code> moves nothing and the balance waits.</span></div>`;
  }
  const effective = (v.fee * 20_000n / v.gross + 1n) / 2n;
  const rounding =
    v.remainder > 0n
      ? `Rounding gave Recipient 1 ${v.remainder} extra base unit${v.remainder === 1n ? '' : 's'}, so the shares add up to exactly the net.`
      : 'The shares add up to exactly the net.';
  return `<div class="cx-verdict cx-ok"><b>Settled</b><span>Effective fee: ${formatBps(Number(effective))} of this amount. ${rounding}</span></div>`;
}

function ledgerHtml(v: View): string {
  if (v.kind !== 'settled') return '';
  const shares = S.shares.map((s) => Number(parseDecimal(s.trim(), 2)));
  let rows = `<tr><th scope="row">Payment</th><td></td><td>${esc(withUnit(v.gross))}</td></tr>`;
  rows += `<tr><th scope="row">Fixed fee</th><td class="cx-hint">fixedFeeMicros × 10^(decimals − 6)</td><td>− ${esc(amount(v.fixed))}</td></tr>`;
  rows += `<tr><th scope="row">Fee ${formatBps(S.feeBps)}</th><td class="cx-hint">⌊amount × ${S.feeBps} ÷ 10,000⌋</td><td>− ${esc(amount(v.pct))}</td></tr>`;
  rows += `<tr class="cx-total"><th scope="row">Net</th><td></td><td>${esc(withUnit(v.net))}</td></tr>`;
  v.shares.forEach((a, i) => {
    rows += `<tr><th scope="row">Recipient ${i + 1} · ${formatBps(shares[i]!)}</th><td class="cx-hint">${i === 0 ? 'net − the other shares' : '⌊net × bps ÷ 10,000⌋'}</td><td>${esc(amount(a))}</td></tr>`;
  });
  return `<caption class="cx-sr">How the example payment splits</caption><tbody>${rows}</tbody>`;
}

/* ---------- the prism ---------- */

// The header mark (viewBox 64) scaled up: its triangle, the beam it draws entering on the left and
// the point where that beam leaves for the two exit rays.
const SCALE = 6.5;
const OX = 135;
const OY = -40;
const P = (x: number, y: number): [number, number] => [OX + x * SCALE, OY + y * SCALE];
const APEX = P(32, 10);
const BASE_L = P(9, 50);
const BASE_R = P(55, 50);
const ENTRY = P(3, 36);
const EXIT = P(43, 33);
const IN_Y = ENTRY[1] + ((EXIT[1] - ENTRY[1]) / (EXIT[0] - ENTRY[0])) * -ENTRY[0];
const MAX_T = 40;
const OUT_X = 575;
const r1 = (n: number) => Math.round(n * 10) / 10;

interface Ray {
  amount: bigint;
  name: string;
  cls: string;
}

function prismSvg(v: View): string {
  const settled = v.kind === 'settled';
  const rays: Ray[] = settled
    ? [...v.shares.map((a, i) => ({ amount: a, name: `Recipient ${i + 1}`, cls: `cx-r${i}` })), { amount: v.fee, name: 'Treasury (fee)', cls: 'cx-fee' }]
    : [];
  const gross = settled ? v.gross : 0n;
  const thick = (u: bigint) => (gross > 0n ? Math.max(2, (Number((u * 10_000n) / gross) / 10_000) * MAX_T) : 2);
  const flow = reduceMotion() ? 'cx-flow cx-still' : 'cx-flow';
  const [ex, ey] = EXIT;
  let s = '';

  const inPath = `M0 ${r1(IN_Y)} L${ex} ${ey}`;
  s += `<path d="${inPath}" class="cx-beam-in${settled ? '' : ' cx-idle'}" stroke-width="${settled ? MAX_T : MAX_T / 2}"/>`;
  s += `<path d="${inPath}" class="${flow}" stroke-width="1.5"/>`;

  const n = rays.length;
  const gap = n > 1 ? Math.min(118, 236 / (n - 1)) : 0;
  const widths = rays.map((r) => thick(r.amount));
  const total = widths.reduce((a, b) => a + b, 0);
  let cum = 0;
  rays.forEach((ray, i) => {
    const t = widths[i]!;
    const y0 = r1(ey - total / 2 + cum + t / 2);
    cum += t;
    const y = r1(181 + (i - (n - 1) / 2) * gap);
    const d = `M${ex} ${y0} C ${ex + 60} ${y0}, ${OUT_X - 70} ${y}, ${OUT_X} ${y}`;
    s += `<path d="${d}" class="cx-ray ${ray.cls}" stroke-width="${r1(t)}"/>`;
    s += `<path d="${d}" class="${flow}" stroke-width="${r1(Math.min(1.5, t * 0.3))}"/>`;
    s += `<text x="${OUT_X + 12}" y="${y - 2}" class="cx-amt">${esc(amount(ray.amount))}</text>`;
    s += `<text x="${OUT_X + 12}" y="${y + 15}" class="cx-lab">${esc(ray.name)}</text>`;
  });

  const tri = `${APEX[0]},${APEX[1]} ${BASE_L[0]},${BASE_L[1]} ${BASE_R[0]},${BASE_R[1]}`;
  s += `<polygon points="${tri}" class="cx-glass"/>`;
  s += `<polygon points="${APEX[0]},${APEX[1]} ${APEX[0]},${BASE_L[1]} ${BASE_R[0]},${BASE_R[1]}" class="cx-glass-shade"/>`;
  s += `<polygon points="${tri}" class="cx-edge"/>`;

  const inLabel = settled ? withUnit(v.gross) : v.kind === 'dust' ? withUnit(v.gross) : '';
  const labelX = narrow() ? 118 : 8;
  if (inLabel) s += `<text x="${labelX}" y="${r1(IN_Y - MAX_T / 2 - (narrow() ? 36 : 10))}" class="cx-amt cx-big">${esc(inLabel)}</text>`;
  s += `<text x="${labelX}" y="${r1(IN_Y + MAX_T / 2 + 20)}" class="cx-lab">payment</text>`;
  if (!settled) {
    const lines = v.kind === 'dust' ? ['Not settled:', 'the fee would', 'reach the amount'] : v.kind === 'revert' ? ['Reverted:', 'nothing moves'] : ['Waiting for', 'valid inputs'];
    const lh = narrow() ? 26 : 20;
    lines.forEach((line, i) => {
      s += `<text x="520" y="${r1(ey - ((lines.length - 1) * lh) / 2 + i * lh)}" class="cx-lab cx-mid">${line}</text>`;
    });
  }
  return s;
}

function prismLabel(v: View): string {
  if (v.kind !== 'settled') return 'The prism: a payment goes in and nothing comes out, because the contracts would not settle it.';
  const parts = v.shares.map((a, i) => `Recipient ${i + 1} ${amount(a)}`);
  return `The prism: ${withUnit(v.gross)} goes in; the fee of ${amount(v.fee)} goes to the treasury; ${parts.join('; ')}.`;
}

function recipientRows(): string {
  const n = S.shares.length;
  return S.shares
    .map(
      (v, i) =>
        `<div class="cx-rrow"><span class="cx-sw cx-r${i}" aria-hidden="true"></span>` +
        `<label for="cx-share-${i}">Recipient ${i + 1}${i === 0 ? ' <small>takes the remainder</small>' : ''}</label>` +
        `<div class="cx-inp"><input id="cx-share-${i}" data-share="${i}" inputmode="decimal" autocomplete="off" value="${esc(v)}" aria-describedby="cx-sum"><span aria-hidden="true">%</span></div>` +
        `<button type="button" class="cx-x" data-remove="${i}" aria-label="Remove recipient ${i + 1}"${n === 1 ? ' disabled' : ''}>×</button></div>`,
    )
    .join('');
}

function sumText(): string {
  let sum = 0;
  for (const raw of S.shares) {
    const u = parseDecimal(raw.trim(), 2);
    if (u === null) return 'Total: check the shares';
    sum += Number(u);
  }
  return `Total ${formatBps(sum)}`;
}

/* ---------- the two paths ---------- */

type PathId = 'deposit' | 'signature';

interface Step {
  edges: string[];
  nodes: string[];
  title: string;
  text: string;
  code: Excerpt[];
}

interface PathDef {
  label: string;
  nodes: Record<string, [x: number, y: number, name: string, sub: string]>;
  edges: Record<string, [from: string, to: string, bend: number, label: string, offChain?: boolean]>;
  steps: Step[];
}

const PATHS: Record<PathId, PathDef> = {
  deposit: {
    label: 'Deposit address',
    nodes: {
      payer: [90, 155, 'Payer', 'any wallet'],
      pp: [90, 40, 'Prismaport', 'backend'],
      factory: [330, 40, 'Factory', 'CREATE2'],
      fwd: [330, 155, 'Deposit address', 'ERC-1167 clone'],
      treasury: [570, 70, 'Treasury', 'fee'],
      recips: [570, 240, 'Recipients', '1 to 5 wallets'],
    },
    edges: {
      e1: ['pp', 'factory', 22, 'predict()', true],
      e2: ['payer', 'fwd', 0, 'USDC'],
      e3: ['pp', 'factory', -22, 'deployAndFlush()'],
      e4: ['factory', 'fwd', 0, 'deploys'],
      e5: ['fwd', 'treasury', 0, 'fee'],
      e6: ['fwd', 'recips', 0, 'shares'],
    },
    steps: [
      {
        edges: ['e1'],
        nodes: ['pp', 'factory'],
        title: 'The address exists before the contract',
        text: 'Prismaport asks the Factory which address belongs to this salt and these parameters: treasury, fee, recipients and shares. It is a read call. The address is a pure function of those values, and no code is deployed there yet. Parameters the contracts would refuse make the call revert, so every address handed out can be settled.',
        code: [X.factoryPredict, X.factoryEncodeChecks],
      },
      {
        edges: ['e2'],
        nodes: ['payer', 'fwd'],
        title: 'The payer sends USDC',
        text: 'From any wallet, with a plain transfer. The balance sits at the address. Nobody holds a key for it: only the code that will be deployed there can move it, and that code is fixed by the parameters.',
        code: [X.factoryNoOwner],
      },
      {
        edges: ['e3', 'e4'],
        nodes: ['pp', 'factory', 'fwd'],
        title: 'Anyone can deploy and settle',
        text: 'When the session closes, Prismaport calls deployAndFlush. Anyone can call it: the Factory deploys a clone with the parameters written into its code and settles the balance. If the clone already exists, the Factory reuses it.',
        code: [X.factoryDeployAndFlush, X.factoryDeployNotice],
      },
      {
        edges: ['e5'],
        nodes: ['fwd', 'treasury'],
        title: 'The fee goes to the treasury',
        text: 'Fee = the fixed part + the percentage of the balance, rounded down. If the fee would be at or above the balance, nothing moves and the money waits at the address.',
        code: [X.feeCompute],
      },
      {
        edges: ['e6'],
        nodes: ['fwd', 'recips'],
        title: 'The net is split',
        text: 'Every recipient after the first gets its share rounded down. The first gets the rest, so the shares add up to exactly the net. The address emits a Payout per paid share, then Settled with the gross, the fee and the net.',
        code: [X.splitLoop, X.forwarderFlush],
      },
    ],
  },
  signature: {
    label: 'Signed payment',
    nodes: {
      payer: [90, 155, 'Payer', 'signs only'],
      pp: [330, 270, 'Prismaport', 'relayer'],
      usdc: [330, 40, 'USDC contract', 'EIP-3009'],
      gw: [330, 155, 'Gateway', 'holds no balance'],
      treasury: [570, 70, 'Treasury', 'fee'],
      recips: [570, 240, 'Recipients', '1 to 5 wallets'],
    },
    edges: {
      b1: ['pp', 'payer', 26, 'EIP-712 request', true],
      b2: ['payer', 'pp', 26, 'signature', true],
      b3: ['pp', 'gw', 0, 'settle()'],
      b4: ['gw', 'usdc', 0, 'receiveWithAuthorization'],
      b5: ['payer', 'gw', 0, 'USDC pulled'],
      b6: ['gw', 'treasury', 0, 'fee'],
      b7: ['gw', 'recips', 0, 'shares'],
    },
    steps: [
      {
        edges: ['b1'],
        nodes: ['pp', 'payer'],
        title: 'The request already names the destination',
        text: 'Prismaport builds a ReceiveWithAuthorization for the payer to sign. Its nonce is a hash of the chain, the Gateway, the session, the treasury, the fee and the recipients with their shares. Changing any of them changes the nonce.',
        code: [X.gatewayNonce],
      },
      {
        edges: ['b2'],
        nodes: ['payer', 'pp'],
        title: 'The payer signs a message',
        text: 'Signing is not a transaction: nothing goes to the chain and the payer pays no network fee. The signed message holds the amount, the payee (the Gateway), a validity window and the nonce.',
        code: [X.usdcTypedData],
      },
      {
        edges: ['b3'],
        nodes: ['pp', 'gw'],
        title: 'Prismaport submits it and pays the network fee',
        text: 'The relayer calls settle on the Gateway. Before touching any money, the Gateway checks the split, refuses itself as a recipient and refuses dust. So an authorization is never used up for nothing.',
        code: [X.gatewayChecks],
      },
      {
        edges: ['b4', 'b5'],
        nodes: ['gw', 'usdc', 'payer'],
        title: 'USDC checks the signature and moves the money',
        text: 'The Gateway recomputes the nonce from the data it received and asks USDC to pull the payment. If anything was changed, the signature does not match and everything reverts. Then the Gateway measures its own balance: exactly the signed amount must have arrived.',
        code: [X.gatewayPullCall, X.gatewayPull],
      },
      {
        edges: ['b6', 'b7'],
        nodes: ['gw', 'treasury', 'recips'],
        title: 'The fee and the shares leave in the same transaction',
        text: 'The treasury gets the fee and each recipient its share. The Gateway pays out exactly what it pulled in. PaymentReceived records the gross, the fee and the net.',
        code: [X.gatewayPayout],
      },
    ],
  },
};

function edgeGeom(a: [number, number], b: [number, number], bend: number) {
  const [x1, y1] = a;
  const [x2, y2] = b;
  const hw = 78;
  const hh = 24;
  const dx = x2 - x1;
  const dy = y2 - y1;
  const t = Math.min(dx ? hw / Math.abs(dx) : 1e9, dy ? hh / Math.abs(dy) : 1e9);
  const sx = x1 + dx * t;
  const sy = y1 + dy * t;
  const ex = x2 - dx * (t + 0.02);
  const ey = y2 - dy * (t + 0.02);
  const len = Math.hypot(dx, dy) || 1;
  const nx = -dy / len;
  const ny = dx / len;
  const mx = (sx + ex) / 2 + nx * bend;
  const my = (sy + ey) / 2 + ny * bend;
  return {
    d: `M${r1(sx)} ${r1(sy)} Q ${r1(mx)} ${r1(my)} ${r1(ex)} ${r1(ey)}`,
    lx: r1((sx + ex) / 2 + nx * bend * 1.3),
    ly: r1((sy + ey) / 2 + ny * bend * 1.3),
  };
}

function graphSvg(): string {
  const p = PATHS[S.path];
  const st = p.steps[S.step]!;
  const still = reduceMotion();
  const marker = (id: string, cls: string) =>
    `<marker id="${id}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0 0 L10 5 L0 10 z" class="${cls}"/></marker>`;
  let s = `<defs>${marker('cx-ah', 'cx-ah')}${marker('cx-ah-on', 'cx-ah cx-on')}</defs>`;
  const labels: string[] = [];
  for (const [id, [from, to, bend, label, offChain]] of Object.entries(p.edges)) {
    const on = st.edges.includes(id);
    const g = edgeGeom([p.nodes[from]![0], p.nodes[from]![1]], [p.nodes[to]![0], p.nodes[to]![1]], bend);
    s += `<path id="cx-pe-${id}" d="${g.d}" class="cx-gedge${offChain ? ' cx-off' : ''}${on ? ' cx-on' : ''}" marker-end="url(#${on ? 'cx-ah-on' : 'cx-ah'})"/>`;
    if (on) {
      if (!still) s += `<circle r="4.5" class="cx-dot"><animateMotion dur="1.6s" repeatCount="indefinite"><mpath href="#cx-pe-${id}"/></animateMotion></circle>`;
      labels.push(`<text class="cx-elabel" x="${g.lx}" y="${g.ly}" text-anchor="middle" dy="${bend >= 0 ? 4 : -2}">${esc(label)}</text>`);
    }
  }
  for (const [id, [x, y, name, sub]] of Object.entries(p.nodes)) {
    const on = st.nodes.includes(id);
    s += `<g class="cx-node${on ? ' cx-on' : ''}"><rect x="${x - 78}" y="${y - 24}" width="156" height="48" rx="8"/><text x="${x}" y="${y - 2}" text-anchor="middle">${esc(name)}</text><text class="cx-sub" x="${x}" y="${y + 14}" text-anchor="middle">${esc(sub)}</text></g>`;
  }
  return s + labels.join('');
}

function stepHtml(): string {
  const p = PATHS[S.path];
  const st = p.steps[S.step]!;
  return `<span class="cx-k">Step ${S.step + 1} of ${p.steps.length}</span><h3>${esc(st.title)}</h3><p>${esc(st.text)}</p>${excerptBoxes(st.code)}`;
}

function stepNavHtml(): string {
  const n = PATHS[S.path].steps.length;
  const dots = Array.from(
    { length: n },
    (_, k) => `<button type="button" data-step="${k}" aria-label="Step ${k + 1}"${k === S.step ? ' aria-current="step"' : ''}></button>`,
  ).join('');
  return `<button type="button" class="btn" id="cx-prev"${S.step === 0 ? ' disabled' : ''}>Previous</button><button type="button" class="btn primary" id="cx-next">${S.step === n - 1 ? 'Start over' : 'Next'}</button><div class="cx-dots" role="group" aria-label="Steps">${dots}</div>`;
}

/* ---------- the tamper lab ---------- */

interface Attempt {
  id: string;
  path: 'Signed payment' | 'Deposit address' | 'Both paths';
  who: string;
  title: string;
  outcome: 'revert' | 'safe';
  result: string;
  text: string;
  code: Excerpt[];
}

export const ATTEMPTS: Attempt[] = [
  {
    id: 'swap',
    path: 'Signed payment',
    who: 'The relayer (whoever submits the transaction)',
    title: 'Swap a recipient for its own address',
    outcome: 'revert',
    result: 'Reverted',
    text: 'The nonce the payer signed is a hash that includes the recipients and their shares. The Gateway recomputes it from the data it receives. With a recipient swapped, the nonce changes, the signature no longer matches and USDC refuses the transfer. Nothing leaves the payer’s wallet.',
    code: [X.gatewayPullCall, X.gatewayNonce],
  },
  {
    id: 'front',
    path: 'Signed payment',
    who: 'Someone who copied the signature',
    title: 'Use the signature directly on the USDC contract',
    outcome: 'revert',
    result: 'Reverted',
    text: 'The authorization is a ReceiveWithAuthorization. Under EIP-3009 the token accepts it only when the caller is the payee, and the payee the payer signed is the Gateway. Nobody else can use it, not even to send the money to the Gateway without the split.',
    code: [X.usdcPayee, X.gatewayPull],
  },
  {
    id: 'replay',
    path: 'Signed payment',
    who: 'The relayer',
    title: 'Use the same signature twice',
    outcome: 'revert',
    result: 'Reverted',
    text: 'Under EIP-3009 every authorization nonce can be used once, and USDC records each one it accepts. The nonce also includes the session salt, so one signature pays one session.',
    code: [X.usdcOnce, X.gatewayNonce],
  },
  {
    id: 'fee',
    path: 'Both paths',
    who: 'Prismaport itself',
    title: 'Charge a 10% fee',
    outcome: 'revert',
    result: 'Reverted: FeeBpsTooHigh(1000)',
    text: 'The cap is in the code every contract runs: 3% (300 bps) plus at most 5.00 USD fixed. Prismaport cannot go above it either. At a deposit address the fee is also part of the address: another fee gives another address, which does not hold the money.',
    code: [X.feeCaps, X.feeCheckParams],
  },
  {
    id: 'steal',
    path: 'Deposit address',
    who: 'Anyone',
    title: 'Withdraw the deposit address balance to yourself',
    outcome: 'safe',
    result: 'There is no function for it',
    text: 'The only function of a deposit address that moves money is flush. It reads the treasury, the recipients and the accepted tokens from the contract’s own code and pays only them. Anyone can call flush, and the money always goes to the same places.',
    code: [X.forwarderNotice],
  },
  {
    id: 'gone',
    path: 'Deposit address',
    who: 'Nobody: Prismaport is offline',
    title: 'Is the money stuck?',
    outcome: 'safe',
    result: 'No',
    text: 'The Factory has no owner. Anyone with the session’s salt and parameters can call deployAndFlush, and the money reaches the recipients with the same fee. With a signature nothing waits: the payment settles in one transaction or does not happen.',
    code: [X.factoryDeployNotice, X.factoryNoOwner],
  },
  {
    id: 'dust',
    path: 'Both paths',
    who: 'A payer',
    title: 'Pay 0.05 USDC, less than the fee',
    outcome: 'safe',
    result: 'Nobody loses',
    text: 'With a signature, the Gateway reverts with DustPayment before using the authorization, so the money stays with the payer. At a deposit address, flush moves nothing and the balance waits until there is enough to settle it all together.',
    code: [X.feeDust, X.gatewayDust],
  },
  {
    id: 'fake',
    path: 'Signed payment',
    who: 'Someone with a token that fakes transfers',
    title: 'A token pretends it transferred',
    outcome: 'revert',
    result: 'Reverted: ReceivedAmountMismatch',
    text: 'The Gateway measures its own balance before and after pulling the payment. If exactly the signed amount did not arrive, everything is undone. So nobody can use tokens sent to the Gateway by mistake to pay themselves.',
    code: [X.gatewayBalanceCheck],
  },
  {
    id: 'split',
    path: 'Both paths',
    who: 'An integration with a bug',
    title: 'Shares that do not add up to 100%, or a repeated recipient',
    outcome: 'revert',
    result: 'Reverted: InvalidShareSum / DuplicateRecipient',
    text: 'The Factory refuses to even compute the address, and the Gateway refuses to settle. Every split that exists has 1 to 5 distinct recipients, no zero address, every share above zero and a sum of exactly 10,000 bps.',
    code: [X.splitCheck],
  },
  {
    id: 'eth',
    path: 'Deposit address',
    who: 'A payer in a hurry',
    title: 'Send ETH or another token instead of USDC',
    outcome: 'revert',
    result: 'Reverted: TokenNotAllowed',
    text: 'A deposit address only settles the tokens written into its code. Once it is deployed it has no receive function, so an ETH send fails in the payer’s wallet and nothing leaves it. ETH sent before the deployment, or any other token, stays at the address: flush refuses it before calling the token. Only the treasury can move it, with recover, and only to the fee address and the recipients written into the address.',
    code: [X.forwarderTokenCheck, X.forwarderNotice],
  },
];

function labResultHtml(): string {
  const a = ATTEMPTS.find((x) => x.id === S.attempt) ?? ATTEMPTS[0]!;
  return (
    `<span class="cx-who">Who tries: ${esc(a.who)}</span><h3>${esc(a.title)}</h3>` +
    `<span class="cx-out cx-${a.outcome}">${esc(a.result)}</span><p>${esc(a.text)}</p>${excerptBoxes(a.code)}`
  );
}

/* ---------- guarantees and anatomy ---------- */

const GUARANTEES: { title: string; text: string; code: Excerpt[] }[] = [
  {
    title: 'No owner, no pause, no upgrade',
    text: 'The Factory, the deposit addresses and the Gateway have no owner, pause switch or upgrade proxy. The deployed code is final. The Gateway’s treasury is immutable. The one restricted call is recover on a deposit address: only the treasury can make it, never for a token the address is bound to, and the funds can only go to the fee address and the recipients written into the address.',
    code: [X.gatewayTreasury],
  },
  {
    title: 'The destination is in the code',
    text: 'The treasury, the fee, the recipients, the shares and the accepted tokens are written into the deposit address’s code, or into the signed nonce. Changing any of them changes the address or breaks the signature.',
    code: [X.factoryArgs],
  },
  {
    title: 'A capped fee',
    text: 'At most 3% plus 5.00 USD fixed, in every contract. The current fee is 0.10 USD + 0.5%.',
    code: [X.feeCaps],
  },
  {
    title: 'Rounding loses nothing',
    text: 'The shares add up to exactly the net. The first recipient takes the rounding remainder: at most one base unit (a millionth of a USDC) per other recipient.',
    code: [X.splitRemainder],
  },
  {
    title: 'The Gateway holds nothing',
    text: 'Each settlement pays out exactly what it pulled in, in the same transaction. Dust is refused before the authorization is used.',
    code: [X.gatewayNotice],
  },
  {
    title: 'The fee is public on-chain',
    text: 'Every settlement emits the gross, the fee and the net, and one event per paid share. Anyone can check them in a block explorer.',
    code: [X.forwarderEvents],
  },
];

const FILES: [string, string][] = [
  ['Factory.sol', 'Computes and deploys the deposit addresses with CREATE2. No owner, no admin.'],
  ['ForwarderImpl.sol', 'The code behind every deposit address: `flush`, for the tokens bound in its code.'],
  ['Gateway.sol', 'The signed-payment path (EIP-3009): pulls the payment, pays the fee and splits the net in one transaction.'],
  ['FeeLib.sol', 'The fee rule and its caps: 3% and 5.00 USD. Dust is not settled.'],
  ['SplitLib.sol', '1 to 5 recipients. The shares add up to exactly the net.'],
];

/* ---------- page ---------- */

export function renderContracts(): string {
  const v = compute();
  const pathBtn = (id: PathId) => `<button type="button" data-path-id="${id}" aria-pressed="${S.path === id}">${PATHS[id].label}</button>`;
  return (
    '<div class="cx">' +
    '<div class="crumb">Guides</div><h1>One payment goes in. The fee and every recipient come out together.</h1>' +
    '<p class="lede">When a payment settles, one transaction sends the fee to the Prismaport treasury and the net to the merchant’s wallets, split by fixed shares. Prismaport never holds the merchant’s money. Change the values: the numbers below follow the contracts’ arithmetic, in whole base units, rounding down.</p>' +
    '<div class="cx-hero">' +
    '<div class="cx-stage">' +
    `<div class="cx-svgwrap"><svg class="cx-svg" id="cx-prism" viewBox="${prismViewBox()}" role="img" aria-label="${esc(prismLabel(v))}">${prismSvg(v)}</svg></div>` +
    `<div id="cx-verdict" role="status">${verdictHtml(v)}</div>` +
    `<table class="cx-ledger" id="cx-ledger">${ledgerHtml(v)}</table>` +
    `<label class="cx-check"><input type="checkbox" id="cx-units"${S.units ? ' checked' : ''}> Show base units, as the contract sees them (1 USDC = 1,000,000)</label>` +
    '</div>' +
    '<form class="cx-calc" id="cx-calc" novalidate>' +
    '<div class="cx-calc-head"><h2>Payment</h2><span class="cx-example">Example values</span></div>' +
    `<div class="cx-fld"><label for="cx-gross">Amount paid</label><div class="cx-inp"><input id="cx-gross" inputmode="decimal" autocomplete="off" value="${esc(S.gross)}"><span aria-hidden="true">USDC</span></div></div>` +
    `<div class="cx-fld"><label for="cx-bps">Percentage fee <output id="cx-bps-v" for="cx-bps">${formatBps(S.feeBps)} (${S.feeBps} bps)</output></label>` +
    `<input type="range" id="cx-bps" min="0" max="400" step="5" value="${S.feeBps}" aria-valuetext="${formatBps(S.feeBps)}, ${S.feeBps} bps" aria-describedby="cx-bps-hint">` +
    `<span class="cx-hint" id="cx-bps-hint">The contracts cap it at ${formatBps(MAX_FEE_BPS)}. The slider goes to 4.00% so you can see them refuse.</span></div>` +
    `<div class="cx-fld"><label for="cx-fixed">Fixed fee</label><div class="cx-inp"><input id="cx-fixed" inputmode="decimal" autocomplete="off" value="${esc(S.fixed)}" aria-describedby="cx-fixed-hint"><span aria-hidden="true">USD</span></div>` +
    '<span class="cx-hint" id="cx-fixed-hint">Charged in the token paid, 1 USD = 1 USDC. Capped at 5.00.</span></div>' +
    `<fieldset class="cx-fld"><legend>Recipients <span class="cx-hint" id="cx-sum" aria-live="polite">${sumText()}</span></legend>` +
    `<div class="cx-recips" id="cx-recips">${recipientRows()}</div>` +
    `<button type="button" class="cx-add" id="cx-add"${S.shares.length >= MAX_RECIPIENTS ? ' disabled' : ''}>Add recipient</button></fieldset>` +
    '<p class="cx-hint">These are example values. The current fee is 0.10 USD + 0.5%.</p>' +
    '</form></div>' +
    '<h2>How the money reaches the contracts</h2>' +
    '<p class="cx-lede2">Both paths apply the same fee and split rules. What changes is who sends the transaction and who pays the network fee. Signed payments are for USDC on Base.</p>' +
    `<div class="cx-seg" role="group" aria-label="Payment path">${pathBtn('deposit')}${pathBtn('signature')}</div>` +
    '<div class="cx-paths">' +
    `<div class="cx-graph"><svg id="cx-graph" viewBox="0 0 660 310" role="img" aria-label="How the payment moves on this path; the active step is highlighted">${graphSvg()}</svg></div>` +
    `<div class="cx-step"><div id="cx-step" aria-live="polite">${stepHtml()}</div><div class="cx-nav" id="cx-nav">${stepNavHtml()}</div></div>` +
    '</div>' +
    '<h2>Try to cheat the contracts</h2>' +
    '<p class="cx-lede2">Pick an attempt. Each answer shows the code that stops it or guarantees the result.</p>' +
    '<div class="cx-lab">' +
    `<div class="cx-attempts" role="group" aria-label="Attempts">${ATTEMPTS.map((a) => `<button type="button" class="cx-att" data-attempt="${a.id}" aria-pressed="${a.id === S.attempt}"><span class="cx-chip">${esc(a.path)}</span><b>${esc(a.title)}</b></button>`).join('')}</div>` +
    `<div class="cx-result" id="cx-result" aria-live="polite">${labResultHtml()}</div>` +
    '</div>' +
    '<h2>What the code guarantees</h2>' +
    `<div class="cx-gs">${GUARANTEES.map((g) => `<div class="cx-g"><h3>${esc(g.title)}</h3><p>${esc(g.text)}</p>${excerptBoxes(g.code)}</div>`).join('')}</div>` +
    '<h2>Three contracts, two libraries</h2>' +
    '<p class="cx-lede2">In <code class="inline-code">contracts/src</code>. On Solana (coming later), an Anchor program applies the same fee and split rules to deposit addresses that are program-derived addresses (PDAs), seeded by the session salt and a hash of the parameters.</p>' +
    `<div class="cx-files">${FILES.map(([f, d]) => `<div class="cx-file"><code>${f}</code><p>${esc(d).replace(/`([^`]+)`/g, '<code>$1</code>')}</p></div>`).join('')}</div>` +
    '<p class="cx-foot">The code excerpts are copied from <code class="inline-code">contracts/src</code> and tested against it, so they change when the contracts do. The <span class="mono">USDC (EIP-3009)</span> excerpts paraphrase the token standard’s rules. The calculator values are examples. Events to index and addresses: <a href="#onchain">On-chain contracts</a>.</p>' +
    '</div>'
  );
}

function updateCalc(root: HTMLElement): void {
  const v = compute();
  const svg = root.querySelector<SVGSVGElement>('#cx-prism')!;
  svg.innerHTML = prismSvg(v);
  svg.setAttribute('aria-label', prismLabel(v));
  svg.setAttribute('viewBox', prismViewBox());
  root.querySelector('#cx-verdict')!.innerHTML = verdictHtml(v);
  root.querySelector('#cx-ledger')!.innerHTML = ledgerHtml(v);
  root.querySelector('#cx-sum')!.textContent = sumText();
  const out = root.querySelector('#cx-bps-v')!;
  out.textContent = `${formatBps(S.feeBps)} (${S.feeBps} bps)`;
  root.querySelector('#cx-bps')!.setAttribute('aria-valuetext', `${formatBps(S.feeBps)}, ${S.feeBps} bps`);
}

function updateRecipients(root: HTMLElement, focus: string): void {
  root.querySelector('#cx-recips')!.innerHTML = recipientRows();
  (root.querySelector('#cx-add') as HTMLButtonElement).disabled = S.shares.length >= MAX_RECIPIENTS;
  updateCalc(root);
  root.querySelector<HTMLElement>(focus)?.focus();
}

function updatePath(root: HTMLElement, focus?: string): void {
  root.querySelector('#cx-graph')!.innerHTML = graphSvg();
  root.querySelector('#cx-step')!.innerHTML = stepHtml();
  root.querySelector('#cx-nav')!.innerHTML = stepNavHtml();
  root.querySelectorAll('[data-path-id]').forEach((b) => b.setAttribute('aria-pressed', String((b as HTMLElement).dataset.pathId === S.path)));
  if (focus) root.querySelector<HTMLElement>(focus)?.focus();
}

/** Wires the controls of the page rendered into `page`. Listeners live on the page's own root, so a re-render drops them. */
export function bindContracts(page: HTMLElement): void {
  const root = page.querySelector<HTMLElement>('.cx');
  if (!root) return;
  root.querySelector('#cx-calc')!.addEventListener('submit', (e) => e.preventDefault());
  root.addEventListener('input', (e) => {
    const t = e.target as HTMLInputElement;
    if (t.id === 'cx-gross') S.gross = t.value;
    else if (t.id === 'cx-fixed') S.fixed = t.value;
    else if (t.id === 'cx-bps') S.feeBps = Number(t.value);
    else if (t.dataset.share !== undefined) S.shares[Number(t.dataset.share)] = t.value;
    else return;
    updateCalc(root);
  });
  root.addEventListener('change', (e) => {
    const t = e.target as HTMLInputElement;
    if (t.id !== 'cx-units') return;
    S.units = t.checked;
    updateCalc(root);
  });
  root.addEventListener('click', (e) => {
    const t = e.target as HTMLElement;
    const remove = t.closest<HTMLElement>('[data-remove]');
    if (remove) {
      const i = Number(remove.dataset.remove);
      if (S.shares.length > 1) S.shares.splice(i, 1);
      updateRecipients(root, `#cx-share-${Math.max(0, i - 1)}`);
      return;
    }
    if (t.closest('#cx-add')) {
      if (S.shares.length >= MAX_RECIPIENTS) return;
      const last = S.shares.length - 1;
      const lastBps = parseDecimal(S.shares[last]!.trim(), 2);
      const half = lastBps === null ? 0n : lastBps / 2n;
      if (half > 0n) {
        S.shares[last] = formatShareInput(lastBps! - half);
        S.shares.push(formatShareInput(half));
      } else S.shares.push('0');
      updateRecipients(root, `#cx-share-${S.shares.length - 1}`);
      return;
    }
    const pathBtn = t.closest<HTMLElement>('[data-path-id]');
    if (pathBtn) {
      S.path = pathBtn.dataset.pathId as PathId;
      S.step = 0;
      updatePath(root);
      return;
    }
    const n = PATHS[S.path].steps.length;
    if (t.closest('#cx-prev')) {
      if (S.step > 0) S.step--;
      updatePath(root, S.step === 0 ? '#cx-next' : '#cx-prev');
      return;
    }
    if (t.closest('#cx-next')) {
      S.step = (S.step + 1) % n;
      updatePath(root, '#cx-next');
      return;
    }
    const dot = t.closest<HTMLElement>('[data-step]');
    if (dot) {
      S.step = Number(dot.dataset.step);
      updatePath(root, `[data-step="${S.step}"]`);
      return;
    }
    const att = t.closest<HTMLElement>('[data-attempt]');
    if (att) {
      S.attempt = att.dataset.attempt!;
      root.querySelectorAll('[data-attempt]').forEach((b) => b.setAttribute('aria-pressed', String(b === att)));
      root.querySelector('#cx-result')!.innerHTML = labResultHtml();
    }
  });
}

const formatShareInput = (bps: bigint) => `${bps / 100n}.${String(bps % 100n).padStart(2, '0')}`;
