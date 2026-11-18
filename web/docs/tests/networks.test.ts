import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { EVM_CONFIRMATIONS, FEE_BPS, FIXED_FEE, MIN_AMOUNT, NETWORKS } from '../src/networks';
import { operations } from '../src/spec';

const go = (p: string) => readFileSync(join(process.cwd(), '../..', p), 'utf8');

// tokenOf resolves a Token expression of chain.Pilot() (`nativeUSDC[8453]` or
// `solanaPilotMints["solana"]["USDC"]`) to the address literal its map holds in chain.go.
const tokenOf = (src: string, expr: string): string | undefined => {
  const evm = /^nativeUSDC\[(\d+)\]$/.exec(expr.trim());
  if (evm) return new RegExp(`\\b${evm[1]}:\\s+"(0x[0-9a-fA-F]{40})"`).exec(src.slice(src.indexOf('var nativeUSDC')))?.[1];
  const sol = /^solanaPilotMints\["([\w-]+)"\]\["(\w+)"\]$/.exec(expr.trim());
  if (sol) {
    const block = src.slice(src.indexOf('var solanaPilotMints'));
    const row = new RegExp(`"${sol[1]}":\\s+\\{([^}]*)\\}`).exec(block)?.[1] ?? '';
    return new RegExp(`"${sol[2]}":\\s+"(\\w+)"`).exec(row)?.[1];
  }
  return /^"(.+)"$/.exec(expr.trim())?.[1];
};

describe('networks module', () => {
  it('matches the GET /v1/checkout/networks example', () => {
    const op = operations.find((o) => o.id === 'listNetworks')!;
    const data = (op.responses[0]!.example as { data: Record<string, unknown>[] }).data;
    expect(data.map((n) => n.id).sort()).toEqual(NETWORKS.map((n) => n.id).sort());
    for (const n of NETWORKS) {
      const ex = data.find((d) => d.id === n.id)!;
      expect(ex.livemode, n.id).toBe(n.live);
      expect(ex.family, n.id).toBe(n.family);
      expect(ex.evm_chain_id, n.id).toBe(n.evmChainId);
      expect(ex.fee_bps, n.id).toBe(FEE_BPS);
      expect(ex.fixed_fee, n.id).toBe(FIXED_FEE);
      expect(ex.assets, n.id).toEqual(n.assets.map((a) => ({ symbol: a.symbol, token: a.token, decimals: a.decimals, eip3009: a.signed })));
    }
  });

  it('matches the pilot registry in internal/chain', () => {
    const src = go('internal/chain/chain.go');
    const pilot = src.slice(src.indexOf('func Pilot()'));
    for (const n of NETWORKS) {
      const head = new RegExp(`\\{ID: "${n.id}", Family: (EVM|Solana), Livemode: (true|false)(?:, EVMID: (\\d+))?`).exec(pilot);
      expect(head, n.id).not.toBeNull();
      expect(head![1]!.toLowerCase()).toBe(n.family);
      expect(head![2]).toBe(String(n.live));
      expect(head![3] ? Number(head![3]) : null).toBe(n.evmChainId);
      for (const a of n.assets) {
        // Pilot() names each token through the nativeUSDC / solanaPilotMints maps; resolve the
        // reference to the literal address the map holds.
        const asset = new RegExp(`\\{Symbol: "${a.symbol}", Token: ([^,]+), Decimals: ${a.decimals}${a.signed ? ', EIP3009: true' : '\\}'}`).exec(pilot.slice(head!.index));
        expect(asset, `${n.id} ${a.symbol}`).not.toBeNull();
        expect(tokenOf(src, asset![1]!), `${n.id} ${a.symbol}`).toBe(a.token);
      }
    }
    const fee = /PilotFee\(\) fee\.Params \{ return fee\.Params\{FeeBps: (\d+), FixedFeeMicros: ([\d_]+)\}/.exec(src)!;
    expect(Number(fee[1])).toBe(FEE_BPS);
    expect(Number(fee[2]!.replaceAll('_', '')) / 1e6).toBe(Number(FIXED_FEE));
  });

  it('waits for the detector default and states the fee floor the API enforces', () => {
    expect(go('internal/chainwatch/chainwatch.go')).toContain(`const DefaultConfirmations = ${EVM_CONFIRMATIONS}`);
    // MinSettlingCents: the least c with fixed + c * bps < c * 10 000 (in micros of a cent).
    const fixedMicros = Number(FIXED_FEE) * 1e6;
    const cents = Math.floor(fixedMicros / (10_000 - FEE_BPS)) + 1;
    expect((cents / 100).toFixed(2)).toBe(MIN_AMOUNT);
    expect(go('internal/config/solana.go')).toMatch(/Credit\s+solrpc\.Commitment/);
  });
});
