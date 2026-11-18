import { chainFamily } from './logos';
import type { AcceptedAsset, Family, Wallet } from './types';

export const MAX_ACTIVE_WALLETS = 20;
export const LABEL_MAX = 60;
export const CONFIRM_CHARS = 6;

export const FAMILIES: Family[] = ['evm', 'solana'];

/** What merchants call a family: EVM wallets pay out on Base, the only EVM network today. */
const FAMILY_LABEL: Record<Family, string> = { evm: 'Base', solana: 'Solana' };

/** The families with at least one chain in the settings: the rest are "Coming soon" (chains off, D62). */
export const availableFamilies = (accepted: AcceptedAsset[]): Family[] => FAMILIES.filter((f) => accepted.some((a) => chainFamily(a.chain) === f));

export const familyLabel = (f: Family): string => FAMILY_LABEL[f] ?? f;

/** A first check of the address shape; the server runs the full checks (contracts, token accounts). */
export function walletAddressError(family: Family, raw: string): string | null {
  const v = raw.trim();
  if (!v) return 'Paste your wallet address.';
  if (family === 'evm') {
    if (!/^0x[0-9a-fA-F]{40}$/.test(v)) return 'This is not a Base wallet address. It starts with 0x and has 40 more letters and numbers.';
    if (/^0x0{40}$/.test(v)) return 'This is the zero address, which nobody controls. Paste your wallet address instead.';
    return null;
  }
  if (!/^[1-9A-HJ-NP-Za-km-z]{32,44}$/.test(v)) return 'This is not a Solana wallet address. Paste the address from your wallet.';
  return null;
}

export function walletLabelError(raw: string): string | null {
  const v = raw.trim();
  if (!v) return 'Give the wallet a name, such as Founder or Reserve.';
  if (v.length > LABEL_MAX) return `Use at most ${LABEL_MAX} characters.`;
  return null;
}

/** Whether the typed characters are the end of the address: EVM hex in any case, base58 exactly. */
export function tailMatches(family: Family, address: string, typed: string): boolean {
  const tail = address.trim().slice(-CONFIRM_CHARS);
  return family === 'evm' ? tail.toLowerCase() === typed.toLowerCase() : tail === typed;
}

/** How the confirmation compares letters, for the field's help. */
export const confirmCaseNote = (family: Family): string =>
  family === 'evm' ? 'Upper or lower case, either works.' : 'Solana addresses are case-sensitive, so match each letter exactly.';

/**
 * The merchant retypes the end of the address they pasted, so a clipboard swap or a truncated
 * paste is caught before money is routed there. A mismatch is only reported once every
 * character is in; before that the field says how many are still missing.
 */
export function confirmTailError(family: Family, address: string, typed: string): string | null {
  const t = typed.trim();
  if (!t) return `Type the last ${CONFIRM_CHARS} characters of the address to confirm it.`;
  if (t.length < CONFIRM_CHARS) return `Type all ${CONFIRM_CHARS} characters (${t.length} of ${CONFIRM_CHARS}).`;
  return tailMatches(family, address, t) ? null : `These are not the last ${CONFIRM_CHARS} characters of the address you pasted. Check the address.`;
}

export const isActive = (w: Wallet): boolean => !w.archived_at;

/** A family's active wallets, the default first and then oldest first: the first one is where new products pay. */
export function activeWallets(wallets: Wallet[], family: Family): Wallet[] {
  return wallets.filter((w) => w.family === family && isActive(w)).sort((a, b) => Number(Boolean(b.default)) - Number(Boolean(a.default)) || a.created_at.localeCompare(b.created_at));
}

/** Where new products pay on a family: the wallet marked default, else the oldest active one. */
export const defaultWallet = (wallets: Wallet[], family: Family): Wallet | undefined => activeWallets(wallets, family)[0];
