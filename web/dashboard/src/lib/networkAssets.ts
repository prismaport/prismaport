// Which stablecoins a product takes on each network (D60), and what payers can pay with.
// Effective = on in Settings ∩ the product's choice, on the networks the product takes.
// Nothing here is stored on the product: it is recomputed from Settings and the choice.
import { networkName } from './format';
import { takeChain, type PayoutForm } from './payout';
import type { AcceptedAsset, NetworksInput, PaymentMethod, Product, ProductNetwork, SettingsProductNetwork, Wallet } from './types';

/** The product's choice for one network in the drawer: all, only some (ticked), or off. */
export interface NetworkChoice {
  accept: 'all' | 'some' | 'off';
  /** Ticked stablecoins; kept while the choice is All or Off so switching back restores them. */
  assets: string[];
}

/** Per chain id of the mode. */
export type NetworksForm = Record<string, NetworkChoice>;

/** The stablecoins on in Settings for a network, in the order Settings lists them. */
export function enabledAssets(accepted: AcceptedAsset[], chain: string): string[] {
  return accepted.filter((a) => a.chain === chain && a.enabled).map((a) => a.asset);
}

/** What payers can choose on one network: Settings ∩ the choice. */
export function effectiveAssets(choice: NetworkChoice | undefined, accepted: AcceptedAsset[], chain: string): string[] {
  const on = enabledAssets(accepted, chain);
  if (!choice || choice.accept === 'off') return [];
  if (choice.accept === 'all') return on;
  return on.filter((a) => choice.assets.includes(a));
}

/** Every payment method the form would give payers, in network then Settings order. */
export function effectiveMethods(form: NetworksForm, accepted: AcceptedAsset[], chains: string[]): PaymentMethod[] {
  return chains.flatMap((c) => effectiveAssets(form[c], accepted, c).map((asset) => ({ chain: c, chain_name: networkName(c), asset })));
}

/** The form for a stored product: a network it does not take is Off. */
export function networksFormOf(networks: ProductNetwork[] | undefined, chains: string[]): NetworksForm {
  const out: NetworksForm = {};
  for (const c of chains) {
    const n = networks?.find((x) => x.chain === c);
    out[c] = n ? { accept: n.accept, assets: n.assets ?? [] } : { accept: 'off', assets: [] };
  }
  for (const n of networks ?? []) out[n.chain] ??= { accept: n.accept, assets: n.assets ?? [] };
  return out;
}

/** A stored product with no `networks` (an older API) takes every network it pays, with All. */
export function networksOfProduct(p: Pick<Product, 'networks' | 'payout'>): ProductNetwork[] {
  if (p.networks) return p.networks;
  return Object.entries(p.payout ?? {})
    .filter(([, items]) => items.length > 0)
    .map(([chain]) => ({ chain, chain_name: networkName(chain), accept: 'all' as const }));
}

/** The request for one network of the form. */
function inputOf(c: NetworkChoice): NetworksInput[string] {
  if (c.accept === 'off') return { accept: 'none' };
  if (c.accept === 'all') return { accept: 'all' };
  return { accept: 'some', assets: c.assets };
}

/** Every network of the form, explicitly, as a new product sends it. */
export function networksBody(form: NetworksForm): NetworksInput {
  return Object.fromEntries(Object.entries(form).map(([c, v]) => [c, inputOf(v)]));
}

/** Only the networks whose choice changed; undefined when none did. */
export function networksPatch(original: ProductNetwork[] | undefined, next: NetworksInput): NetworksInput | undefined {
  const out: NetworksInput = {};
  for (const [c, n] of Object.entries(next)) {
    const was = original?.find((x) => x.chain === c);
    const changed = !was ? n.accept !== 'none' : n.accept !== was.accept || (n.accept === 'some' && [...(n.assets ?? [])].sort().join() !== [...(was.assets ?? [])].sort().join());
    if (changed) out[c] = n;
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

/** What is wrong with the choices, by chain: Some with nothing ticked. */
export function networksErrors(form: NetworksForm): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [c, v] of Object.entries(form)) {
    if (v.accept === 'some' && v.assets.length === 0) out[`networks.${c}`] = `Tick at least one stablecoin, or set ${networkName(c)} to Off.`;
  }
  return out;
}

/** The stablecoins ticked in Settings that are off right now: a stored `some` keeps them ticked. */
export function offInSettings(choice: NetworkChoice, accepted: AcceptedAsset[], chain: string): string[] {
  const on = enabledAssets(accepted, chain);
  return choice.assets.filter((a) => !on.includes(a));
}

// ---- Settings: how many products use a stablecoin, and who a change would leave with nothing.

const takes = (n: SettingsProductNetwork, asset: string) => n.accept === 'all' || (n.assets ?? []).includes(asset);

/** Products that take the stablecoin now: All on that network, or Some with it ticked. */
export function usedBy(nets: SettingsProductNetwork[], chain: string, asset: string): number {
  return new Set(nets.filter((n) => n.chain === chain && takes(n, asset)).map((n) => n.product_id)).size;
}

/** Products that keep the stablecoin ticked (Some), for a row that is off. */
export function tickedOn(nets: SettingsProductNetwork[], chain: string, asset: string): number {
  return new Set(nets.filter((n) => n.chain === chain && n.accept === 'some' && (n.assets ?? []).includes(asset)).map((n) => n.product_id)).size;
}

const effective = (n: SettingsProductNetwork, on: string[]): string[] => (n.accept === 'all' ? on : on.filter((a) => (n.assets ?? []).includes(a)));

/** Products that take something on the network today and would take nothing once Settings read as `draft`. */
export function leftWithNothing(nets: SettingsProductNetwork[], saved: AcceptedAsset[], draft: AcceptedAsset[], chain: string): { product_id: string; product_name: string }[] {
  const before = enabledAssets(saved, chain);
  const after = enabledAssets(draft, chain);
  const seen = new Map<string, string>();
  for (const n of nets) {
    if (n.chain !== chain) continue;
    if (effective(n, before).length > 0 && effective(n, after).length === 0) seen.set(n.product_id, n.product_name);
  }
  return [...seen].map(([product_id, product_name]) => ({ product_id, product_name }));
}

export const productsText = (n: number) => (n === 1 ? '1 product' : `${n} products`);

// ---- The drawer: choosing All, Some or Off keeps the payout in step with it.

const OFF: NetworkChoice = { accept: 'off', assets: [] };

/** The choice of a network, Off when the form does not name it. */
export const choiceOf = (form: NetworksForm, chain: string): NetworkChoice => form[chain] ?? OFF;

/**
 * Switches a network to All, Some or Off. Off empties its payout (the product does not take it);
 * taking it again fills the family's default wallet at 100% when the payout is empty. Some starts
 * with the stablecoins on in Settings ticked when none was ticked yet.
 */
export function chooseNetwork(
  state: { networks: NetworksForm; payout: PayoutForm },
  chain: string,
  accept: NetworkChoice['accept'],
  wallets: Wallet[],
  accepted: AcceptedAsset[],
): { networks: NetworksForm; payout: PayoutForm } {
  const before = choiceOf(state.networks, chain);
  const assets = accept === 'some' && before.assets.length === 0 ? enabledAssets(accepted, chain) : before.assets;
  const rows = accept === 'off' ? [] : (state.payout[chain] ?? []).length > 0 ? state.payout[chain]! : takeChain(wallets, chain);
  // No wallet to pay: the network stays Off.
  const next: NetworkChoice = accept !== 'off' && rows.length === 0 ? OFF : { accept, assets };
  return { networks: { ...state.networks, [chain]: next }, payout: { ...state.payout, [chain]: rows } };
}

/** Ticks or unticks one stablecoin of a Some network. */
export function toggleAsset(form: NetworksForm, chain: string, asset: string): NetworksForm {
  const c = choiceOf(form, chain);
  const assets = c.assets.includes(asset) ? c.assets.filter((a) => a !== asset) : [...c.assets, asset];
  return { ...form, [chain]: { ...c, assets } };
}

/** A new product: every network it can pay (it has rows) is All, the rest Off. */
export function prefillNetworks(payout: PayoutForm, chains: string[]): NetworksForm {
  return Object.fromEntries(chains.map((c) => [c, (payout[c] ?? []).length > 0 ? { accept: 'all' as const, assets: [] } : OFF]));
}
