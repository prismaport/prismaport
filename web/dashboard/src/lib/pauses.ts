import type { ChainPause } from './types';
import { useMe } from './queries';
import { useMode } from './mode';

/** The pauses of the mode on screen (D64). */
export function usePauses(): ChainPause[] {
  const { mode } = useMode();
  const pauses = useMe().data?.chain_pauses ?? [];
  return pauses.filter((p) => p.livemode === (mode === 'live'));
}

/** Whether new payments are paused for this chain and asset: the whole chain, or that asset on it. */
export function isPaused(pauses: ChainPause[], chain: string, asset?: string): boolean {
  return pauses.some((p) => p.chain === chain && (p.asset === null || asset === undefined || p.asset === asset));
}

/** The fixed text of the strip (no free-text reason). */
export function pauseText(pauses: ChainPause[]): string {
  const one = (p: ChainPause) => (p.asset ? `${p.asset} on ${p.chain_name}` : p.chain_name);
  if (pauses.length === 1) {
    const p = pauses[0]!;
    return p.asset
      ? `${p.asset} payments on ${p.chain_name} are paused. New checkouts won't offer ${p.asset} on ${p.chain_name} for now; payments already sent are still credited and paid out.`
      : `Payments on ${p.chain_name} are paused. New checkouts won't offer ${p.chain_name} for now; payments already sent there are still credited and paid out.`;
  }
  return `Payments on some networks are paused: ${pauses.map(one).join(', ')}. New checkouts won't offer them for now; payments already sent are still credited and paid out.`;
}
