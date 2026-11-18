import { networkName } from './format';
import type { Pill } from './status';
import type { AddressAttention, CustomerAddress } from './types';

export const ADDRESS_PILL: Record<CustomerAddress['status'], Pill> = {
  active: { label: 'Active', tone: 'paid' },
  rotated: { label: 'Rotated', tone: 'idle' },
};

const list = (items: string[]): string => (items.length <= 1 ? items.join('') : `${items.slice(0, -1).join(', ')} and ${items.at(-1)}`);
const networks = (chains: string[] = []): string => list(chains.map(networkName));

/** One sentence per thing a rotation would change, in the server's order. */
export function attentionLine(a: AddressAttention): string {
  switch (a.reason) {
    case 'product_inactive':
      return 'Its product is inactive: no new payment opens on this address, and it can be rotated only once the product is active again.';
    case 'archived_wallet': {
      const names = (a.wallets ?? []).map((w) => w.label || 'a wallet');
      return `It still pays ${list(names)}, which you archived.`;
    }
    case 'payout_changed':
      return `The product pays different wallets or shares on ${networks(a.chains)} now. This address keeps the payout it was created with.`;
    case 'fee_changed':
      return `The Prismaport fee on ${networks(a.chains)} changed since this address was created. It keeps the fee it was created with.`;
    case 'new_networks':
      return `It does not offer ${list((a.assets ?? []).map((x) => `${x.asset} on ${networkName(x.chain)}`))}, which you accept now.`;
    case 'no_networks':
      return 'None of its networks is accepted any more, so no new payment opens on it.';
  }
}

/** Reasons a rotation fixes now (an inactive product blocks the rotation itself). */
export function rotationAdvised(ca: Pick<CustomerAddress, 'status' | 'attention'>): boolean {
  return ca.status === 'active' && ca.attention.some((a) => a.reason !== 'product_inactive');
}

/** Rotating needs an active address and an active product. */
export function canRotate(ca: Pick<CustomerAddress, 'status' | 'product'>): boolean {
  return ca.status === 'active' && ca.product?.active !== false;
}
