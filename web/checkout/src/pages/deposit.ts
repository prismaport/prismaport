import type { Network, PayTo } from '../lib/api';
import { COPY, PAUSED, selfSendFeeCopy, sendOnlyTitle } from '../lib/copy';
import { h, icon } from '../lib/dom';
import { formatAmount, formatExact } from '../lib/format';
import { assetBadge, networkInfo, testnetTag, tokenLogo } from '../lib/logos';
import { SOLANA_EXCHANGE_NOTE, findAsset, findNetwork, isSolana, networkName, payToKey } from '../lib/networks';
import { paymentUri } from '../lib/payuri';
import { qrSvg } from '../lib/qr';
import { copyButton, shortValue } from '../ui';

// The parts of the payment block the session page (/pay/{id}) and the customer address page
// (/a/{id}) share: the network picker and the deposit address with its QR.

/** D64: the "Paused" tag after a network's name or an option's. */
export function pausedTag(): HTMLElement {
  return h('span', { class: 'testnet-tag paused-tag', 'data-fid': 'paused-tag' }, PAUSED.tag);
}

/** The "Testnet" tag after a test network's printed name; nothing on a mainnet. */
export function testnetTagFor(chain: string): HTMLElement | null {
  return networkInfo(chain).testnet ? testnetTag() : null;
}

function coarsePointer(): boolean {
  try {
    return typeof matchMedia === 'function' && matchMedia('(pointer: coarse)').matches;
  } catch {
    return false;
  }
}

type Option = Pick<PayTo, 'chain' | 'asset' | 'paused'>;

/** The network and stablecoin radio group; onSelect gets the option's key (payToKey). */
export function networkPicker(options: Option[], selected: Option, onSelect: (key: string) => void): HTMLElement {
  const fs = h('fieldset', { class: 'picker' }, h('legend', { class: 'label' }, 'Network'));
  const opts = h('div', { class: 'options' });
  for (const p of options) {
    const key = payToKey(p);
    const input = h('input', { type: 'radio', name: 'payto', value: key, 'data-fid': `opt-${key}` });
    input.checked = key === payToKey(selected);
    input.addEventListener('change', () => onSelect(key));
    opts.append(h('label', { class: 'option' }, input, assetBadge(p.asset, p.chain, { size: 24, decorative: true, tag: false }),
      h('span', { class: 'option-text' }, h('span', { class: 'asset' }, p.asset),
        h('span', { class: 'net net-line' }, networkName(p.chain), testnetTagFor(p.chain), p.paused ? pausedTag() : null))));
  }
  fs.append(opts);
  return fs;
}

export interface DepositOptions {
  payTo: Pick<PayTo, 'chain' | 'asset' | 'address' | 'token_account' | 'reference'>;
  networks: Network[];
  /** What to send, in the asset's units; null when the payer sends any amount (no amount in the QR either). */
  due: string | null;
  /** The Solana Pay message (the product name). */
  message: string;
  fullAddress: boolean;
  onToggleAddress: () => void;
  exchangeWarning?: string | null;
}

/**
 * The deposit address, its QR (a payment request when one can be built), the amount and the notes
 * around them. One warning above the QR names the only asset and network that count (ADR-0008 I-4).
 */
export function depositBlock(o: DepositOptions): HTMLElement {
  const sel = o.payTo;
  const net = networkName(sel.chain);
  const address = sel.address ?? '';
  const asset = findAsset(o.networks, sel);
  let exact = '';
  if (asset && o.due !== null) {
    try {
      exact = formatExact(o.due, asset.decimals);
    } catch {
      exact = '';
    }
  }
  const solana = isSolana(sel.chain, o.networks);
  const uri = paymentUri({ payTo: sel, network: findNetwork(o.networks, sel.chain), asset, due: o.due, label: 'Prismaport', message: o.message });
  const amount = o.due === null
    ? h('div', {}, h('p', { class: 'label' }, 'Amount'),
      h('p', { class: 'amount value with-logo', 'data-fid': 'any-amount' }, tokenLogo(sel.asset, { size: 20, decorative: true }), h('span', {}, `${COPY.anyAmount} of ${sel.asset}`)))
    : h('div', {}, h('p', { class: 'label' }, 'Amount'),
      h('div', { class: 'field' }, h('span', { class: 'amount value with-logo' }, tokenLogo(sel.asset, { size: 20, decorative: true }), h('span', {}, `${formatAmount(o.due)} ${sel.asset}`)),
        copyButton(exact || formatAmount(o.due), 'amount', 'copy-amount')));
  return h('div', { class: 'deposit' },
    h('div', { class: 'deposit-head' }, assetBadge(sel.asset, sel.chain, { size: 24, decorative: true, tag: false }),
      h('div', {}, h('p', { class: 'label' }, 'Deposit address'),
        h('p', { class: 'small net-line' }, `${sel.asset} on ${net}`, testnetTagFor(sel.chain)))),
    h('div', { class: 'network-warning', role: 'note', 'data-fid': 'network-warning' }, icon('alert', 18),
      h('div', {}, h('p', { class: 'network-warning-title' }, sendOnlyTitle(sel.asset, sel.chain)), h('p', {}, COPY.sendOnlyDetail))),
    h('div', { class: 'qr-wrap' }, qrSvg(uri ?? address, uri ? `QR code of the ${net} payment request` : `QR code of the ${net} deposit address`)),
    uri && !solana && coarsePointer() ? h('a', { class: 'btn', href: uri, 'data-fid': 'open-wallet' }, icon('wallet', 20), 'Open in wallet app') : null,
    h('div', { class: 'field' }, shortValue(address), copyButton(address, 'address', 'copy-address')),
    h('button', { type: 'button', class: 'btn-link small', 'aria-expanded': String(o.fullAddress), 'data-fid': 'toggle-address',
      onclick: o.onToggleAddress }, o.fullAddress ? 'Hide full address' : 'Show full address'),
    o.fullAddress ? h('p', { class: 'address full-address' }, address) : null,
    sel.token_account ? h('div', {}, h('p', { class: 'label' }, 'Or send to the token account'),
      h('div', { class: 'field' }, shortValue(sel.token_account), copyButton(sel.token_account, 'token account', 'copy-token-account'))) : null,
    amount,
    exact ? h('p', { class: 'small muted' }, 'Exact on-chain amount ', h('span', { class: 'address' }, `${exact} ${sel.asset}`)) : null,
    h('p', { class: 'small muted', 'data-fid': 'fee-note' }, selfSendFeeCopy(sel.chain, solana)),
    o.exchangeWarning ? h('p', { class: 'note' }, icon('info'), h('span', {}, o.exchangeWarning)) : null,
    solana ? h('p', { class: 'note', 'data-emphasis': true }, icon('info'), h('span', {}, SOLANA_EXCHANGE_NOTE)) : null,
  );
}
