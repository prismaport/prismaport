import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { CopyValue, ShortValue } from '../src/components/ui';
import { ToastProvider } from '../src/components/Toast';

const FULL = '0xD544632988655aa58d0C15b925970526e93658A5';
const SHORT = '0xD54463…58A5';

/** The text a selection copies: everything but what copying leaves out. */
function copiedText(el: Element): string {
  const clone = el.cloneNode(true) as Element;
  clone.querySelectorAll('.no-copy').forEach((n) => n.remove());
  return clone.textContent ?? '';
}

/** The text a screen reader reads: everything but what is hidden from it. */
function spokenText(el: Element): string {
  const clone = el.cloneNode(true) as Element;
  clone.querySelectorAll('[aria-hidden="true"]').forEach((n) => n.remove());
  return clone.textContent ?? '';
}

const count = (text: string, part: string) => text.split(part).length - 1;

describe('ShortValue', () => {
  it('shows and copies the short form once, and reads the full value to screen readers', () => {
    render(
      <p data-testid="sentence">
        Payments go to <strong>Acme Store Wallet</strong> <ShortValue value={FULL} head={6} tail={4} />. It is where your links pay by default.
      </p>,
    );
    const p = screen.getByTestId('sentence');
    expect(copiedText(p)).toBe(`Payments go to Acme Store Wallet ${SHORT}. It is where your links pay by default.`);
    expect(count(copiedText(p), SHORT)).toBe(1);
    expect(copiedText(p)).not.toContain(FULL);
    expect(spokenText(p)).toBe(`Payments go to Acme Store Wallet ${FULL}. It is where your links pay by default.`);
    expect(screen.getByText(FULL)).toHaveClass('sr-only', 'no-copy');
    expect(screen.getByText(SHORT)).toHaveAttribute('aria-hidden', 'true');
    expect(screen.getByTitle(FULL)).toHaveClass('address');
  });

  it('a copy value keeps the full address on its button', () => {
    render(
      <ToastProvider>
        <CopyValue value={FULL} what="Acme Store Wallet address" head={6} tail={4} />
      </ToastProvider>,
    );
    const wrap = document.querySelector('.inline-copy')!;
    expect(count(copiedText(wrap), SHORT)).toBe(1);
    expect(copiedText(wrap)).not.toContain(FULL);
    expect(screen.getByRole('button', { name: 'Copy Acme Store Wallet address' })).toBeInTheDocument();
  });
});
