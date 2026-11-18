import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

describe('entry point (ADR-0008 D-4)', () => {
  const src = readFileSync(resolve(__dirname, '../src/main.ts'), 'utf8');
  it('loads the pay page and wallet code lazily, not statically', () => {
    expect(src).not.toMatch(/^import .*(pages\/pay|wallet\/(evm|solana))/m);
    expect(src).toMatch(/import\('\.\/pages\/pay'\)/);
  });
  it('has a global fallback for unhandled errors (ADR-0008 D-1)', () => {
    expect(src).toContain("'unhandledrejection'");
    expect(src).toContain("'error'");
  });
});
