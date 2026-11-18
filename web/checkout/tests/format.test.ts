import { describe, expect, it } from 'vitest';
import {
  AmountError,
  addAmounts,
  compareAmounts,
  formatAmount,
  formatCountdown,
  formatDateTime,
  formatExact,
  fromUnits,
  parseUsdInput,
  subtractAmounts,
  toUnits,
  truncateMiddle,
} from '../src/lib/format';

describe('exact amounts', () => {
  it('shows two decimals for people, and every non-zero digit beyond', () => {
    expect(formatAmount('25')).toBe('25.00');
    expect(formatAmount('25.00')).toBe('25.00');
    expect(formatAmount('25.000000')).toBe('25.00');
    expect(formatAmount('25.5')).toBe('25.50');
    expect(formatAmount('14.999999')).toBe('14.999999');
    expect(formatAmount('0.000001')).toBe('0.000001');
    expect(formatAmount('10.120000000000000000')).toBe('10.12');
    expect(formatAmount('007.10')).toBe('7.10');
  });

  it('never rounds', () => {
    expect(formatAmount('24.995')).toBe('24.995');
    expect(formatAmount('0.105')).toBe('0.105');
  });

  it('shows the token decimals where the on-chain value matters', () => {
    expect(formatExact('25', 6)).toBe('25.000000');
    expect(formatExact('15.000000000000000000', 6)).toBe('15.000000');
    expect(formatExact('0.000001', 6)).toBe('0.000001');
    expect(() => formatExact('0.0000001', 6)).toThrow(AmountError);
  });

  it('converts to and from base units without floats', () => {
    expect(toUnits('25.00', 6)).toBe(25_000_000n);
    expect(toUnits('0.1', 6)).toBe(100_000n);
    expect(toUnits('123456789012345678901234567890.123456', 6)).toBe(123456789012345678901234567890123456n);
    expect(toUnits('1.500000000', 6)).toBe(1_500_000n);
    expect(() => toUnits('1.0000001', 6)).toThrow(AmountError);
    expect(fromUnits(1n, 6)).toBe('0.000001');
    expect(fromUnits(25_000_000n, 6)).toBe('25.000000');
    expect(fromUnits(5n, 0)).toBe('5');
    expect(() => fromUnits(-1n, 6)).toThrow(AmountError);
  });

  it('rejects anything that is not a plain decimal', () => {
    for (const bad of ['', '-1', '1e6', '1,000.00', ' . ', 'NaN', '0x10', '1.2.3']) {
      expect(() => toUnits(bad, 6), bad).toThrow(AmountError);
    }
  });

  it('adds, subtracts and compares across decimals', () => {
    expect(compareAmounts('25.00', '25.000000')).toBe(0);
    expect(compareAmounts('24.999999', '25')).toBe(-1);
    expect(compareAmounts('25.000001', '25')).toBe(1);
    expect(formatAmount(subtractAmounts('25.00', '10.000000'))).toBe('15.00');
    expect(formatAmount(subtractAmounts('25.00', '24.999999'))).toBe('0.000001');
    expect(formatAmount(subtractAmounts('10', '25'))).toBe('0.00');
    expect(formatAmount(addAmounts('0.1', '0.2'))).toBe('0.30');
  });
});

describe('truncation', () => {
  it('shortens EVM addresses in the middle like the brand sample', () => {
    expect(truncateMiddle('0x7a3F9c2d4E5b6A7c8D9e0F1a2B3c4D5e6F7ae91c')).toBe('0x7a3F…e91c');
  });

  it('shortens Solana addresses and hashes', () => {
    expect(truncateMiddle('9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin')).toBe('9xQe…VFin');
    expect(truncateMiddle('0x' + 'ab'.repeat(32), 6, 6)).toBe('0xababab…ababab');
  });

  it('leaves short values whole', () => {
    expect(truncateMiddle('0x1234abcd')).toBe('0x1234abcd');
    expect(truncateMiddle('abcdefghi')).toBe('abcdefghi');
  });
});

describe('date and time', () => {
  it('prints a timestamp in the payer locale, and nothing for a bad one', () => {
    const iso = '2026-10-23T18:42:10Z';
    expect(formatDateTime(iso)).toBe(new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(Date.parse(iso)));
    expect(formatDateTime('not a date')).toBeNull();
  });
});

describe('countdown', () => {
  it('formats minutes and seconds under an hour', () => {
    expect(formatCountdown(59 * 60_000 + 12_000)).toBe('59:12');
    expect(formatCountdown(5_000)).toBe('0:05');
    expect(formatCountdown(60_000)).toBe('1:00');
  });

  it('adds hours above an hour', () => {
    expect(formatCountdown(3_723_000)).toBe('1:02:03');
    expect(formatCountdown(24 * 3_600_000)).toBe('24:00:00');
  });

  it('rounds seconds up and stops at zero', () => {
    expect(formatCountdown(1)).toBe('0:01');
    expect(formatCountdown(59_001)).toBe('1:00');
    expect(formatCountdown(0)).toBe('0:00');
    expect(formatCountdown(-5_000)).toBe('0:00');
  });
});

describe('typed USD amounts', () => {
  it('normalizes to two decimals', () => {
    expect(parseUsdInput('10')).toEqual({ cents: 1000n, value: '10.00' });
    expect(parseUsdInput(' 12.5 ')).toEqual({ cents: 1250n, value: '12.50' });
    expect(parseUsdInput('$7.25')).toEqual({ cents: 725n, value: '7.25' });
    expect(parseUsdInput('.5')).toEqual({ cents: 50n, value: '0.50' });
    expect(parseUsdInput('5.')).toEqual({ cents: 500n, value: '5.00' });
    expect(parseUsdInput('1.5')).toEqual({ cents: 150n, value: '1.50' });
    expect(parseUsdInput('1.50')).toEqual({ cents: 150n, value: '1.50' });
    // Past three digits a dot is a decimal point: trailing zeros are dropped.
    expect(parseUsdInput('5.0000')).toEqual({ cents: 500n, value: '5.00' });
  });

  it('explains what is wrong', () => {
    expect(parseUsdInput('')).toEqual({ error: 'empty' });
    expect(parseUsdInput('abc')).toEqual({ error: 'format' });
    expect(parseUsdInput('-3')).toEqual({ error: 'format' });
    expect(parseUsdInput('5.0001')).toEqual({ error: 'precision' });
    expect(parseUsdInput('0.00')).toEqual({ error: 'zero' });
  });

  it('takes a lone comma as the decimal separator (comma-locale keypads)', () => {
    expect(parseUsdInput('10,50')).toEqual({ cents: 1050n, value: '10.50' });
    expect(parseUsdInput('10,5')).toEqual({ cents: 1050n, value: '10.50' });
    expect(parseUsdInput('10.50')).toEqual({ cents: 1050n, value: '10.50' });
    expect(parseUsdInput('1,50')).toEqual({ cents: 150n, value: '1.50' });
    expect(parseUsdInput('10,')).toEqual({ cents: 1000n, value: '10.00' });
    expect(parseUsdInput(',5')).toEqual({ cents: 50n, value: '0.50' });
    expect(parseUsdInput('$7,25')).toEqual({ cents: 725n, value: '7.25' });
    expect(parseUsdInput('10,5055')).toEqual({ error: 'precision' });
    expect(parseUsdInput('0,00')).toEqual({ error: 'zero' });
  });

  it('refuses mixed or repeated separators instead of guessing', () => {
    expect(parseUsdInput('1.000,50')).toEqual({ error: 'format' });
    // Three digits after a lone separator read as thousands in one locale and decimals in the
    // other ("1,000" in en-US, "1.000" in pt-BR): never guessed, whichever the separator.
    expect(parseUsdInput('1,000')).toEqual({ error: 'format' });
    expect(parseUsdInput('10,505')).toEqual({ error: 'format' });
    expect(parseUsdInput('999,999')).toEqual({ error: 'format' });
    expect(parseUsdInput(',500')).toEqual({ error: 'format' });
    expect(parseUsdInput('1.000')).toEqual({ error: 'format' });
    expect(parseUsdInput('$1.000')).toEqual({ error: 'format' });
    expect(parseUsdInput('5.000')).toEqual({ error: 'format' });
    expect(parseUsdInput('10.505')).toEqual({ error: 'format' });
    expect(parseUsdInput('999.999')).toEqual({ error: 'format' });
    expect(parseUsdInput('.500')).toEqual({ error: 'format' });
    expect(parseUsdInput('1,000.50')).toEqual({ error: 'format' });
    expect(parseUsdInput('1,000,50')).toEqual({ error: 'format' });
    expect(parseUsdInput('1.000.50')).toEqual({ error: 'format' });
    expect(parseUsdInput(',')).toEqual({ error: 'format' });
    expect(parseUsdInput('10 ,50')).toEqual({ error: 'format' });
  });
});
