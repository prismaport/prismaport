import { describe, expect, it } from 'vitest';
import { explorerTxUrl } from '../src/lib/networks';

describe('explorerTxUrl', () => {
  it('links each pilot chain to its explorer and nothing else', () => {
    expect(explorerTxUrl('base', '0xab')).toBe('https://basescan.org/tx/0xab');
    expect(explorerTxUrl('base-sepolia', '0xab')).toBe('https://sepolia.basescan.org/tx/0xab');
    expect(explorerTxUrl('solana', '5Kq')).toBe('https://solscan.io/tx/5Kq');
    expect(explorerTxUrl('solana-devnet', '5Kq')).toBe('https://solscan.io/tx/5Kq?cluster=devnet');
    expect(explorerTxUrl('polygon', '0xab')).toBeNull();
  });

  it('keeps the hash inside the path', () => {
    expect(explorerTxUrl('base', '../x?y=1#z')).toBe('https://basescan.org/tx/..%2Fx%3Fy%3D1%23z');
  });
});
