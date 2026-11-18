import { describe, expect, it } from 'vitest';
import { qrSvg } from '../src/lib/qr';
import { EVM_ADDRESS, SOL_ADDRESS } from './fixtures';
import { decode } from './qrdecode';

describe('QR codes', () => {
  it('encode the full deposit address, locally', () => {
    expect(decode(EVM_ADDRESS)).toBe(EVM_ADDRESS);
    expect(decode(SOL_ADDRESS)).toBe(SOL_ADDRESS);
  });

  it('render dark modules on white with a label', () => {
    const el = qrSvg(EVM_ADDRESS, 'QR code of the Base deposit address');
    expect(el.getAttribute('role')).toBe('img');
    expect(el.getAttribute('aria-label')).toBe('QR code of the Base deposit address');
    expect(el.querySelector('rect')!.getAttribute('fill')).toBe('#ffffff');
    expect(el.querySelector('path')!.getAttribute('fill')).toBe('#12141c');
  });
});
