import { beforeEach, describe, expect, it } from 'vitest';
import { bindContracts, renderContracts } from '../src/contracts/page';

const page = document.createElement('div');
const $ = <T extends Element = HTMLElement>(sel: string) => page.querySelector(sel) as T;
const type = (sel: string, value: string) => {
  const el = $<HTMLInputElement>(sel);
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
};
const verdict = () => $('#cx-verdict').textContent ?? '';
const ledger = () => [...page.querySelectorAll('#cx-ledger tr')].map((r) => r.textContent ?? '');

beforeEach(() => {
  document.body.replaceChildren(page);
  page.innerHTML = renderContracts();
  bindContracts(page);
});

describe('the live prism', () => {
  it('renders the marked example: 30.00 USDC, 0.10 + 0.5%, 70/30', () => {
    expect($('h1').textContent).toBe('One payment goes in. The fee and every recipient come out together.');
    expect($('.cx-example').textContent).toBe('Example values');
    expect(verdict()).toContain('Settled');
    const rows = ledger();
    expect(rows[0]).toContain('30.00 USDC');
    expect(rows[1]).toContain('0.10');
    expect(rows[2]).toContain('0.15');
    expect(rows[3]).toContain('29.75 USDC');
    expect(rows[4]).toMatch(/Recipient 1 · 70\.00%.*20\.825/);
    expect(rows[5]).toMatch(/Recipient 2 · 30\.00%.*8\.925/);
    const rays = page.querySelectorAll('#cx-prism .cx-ray');
    expect(rays).toHaveLength(3);
    const widths = [...rays].map((r) => Number(r.getAttribute('stroke-width')));
    expect(widths[0]).toBeGreaterThan(widths[1]!);
    expect(widths[1]).toBeGreaterThan(widths[2]!);
    expect($('#cx-prism').getAttribute('aria-label')).toContain('the fee of 0.25 goes to the treasury');
    expect(page.innerHTML).not.toContain(' style="');
  });

  it('shows FeeBpsTooHigh when the slider passes the cap', () => {
    type('#cx-bps', '350');
    expect(verdict()).toContain('Reverted');
    expect(verdict()).toContain('FeeBpsTooHigh(350)');
    expect($('#cx-bps-v').textContent).toBe('3.50% (350 bps)');
    expect($('#cx-bps').getAttribute('aria-valuetext')).toBe('3.50%, 350 bps');
    expect(page.querySelectorAll('#cx-prism .cx-ray')).toHaveLength(0);
    expect(ledger()).toHaveLength(0);
    type('#cx-bps', '300');
    expect(verdict()).toContain('Settled');
  });

  it('shows FixedFeeTooHigh above 5.00', () => {
    type('#cx-fixed', '5.000001');
    expect(verdict()).toContain('FixedFeeTooHigh(5000001)');
    type('#cx-fixed', '0.10');
  });

  it('shows InvalidShareSum when the shares do not add up to 100%', () => {
    type('#cx-share-1', '20');
    expect(verdict()).toContain('InvalidShareSum(9000)');
    expect(verdict()).toContain('90.00%');
    expect($('#cx-sum').textContent).toBe('Total 90.00%');
    type('#cx-share-1', '30.00');
    expect(verdict()).toContain('Settled');
  });

  it('says what is wrong with an input instead of computing', () => {
    type('#cx-gross', '30.0000001');
    expect(verdict()).toContain('Check the inputs');
    type('#cx-gross', '30.00');
  });

  it('shows dust when the fee reaches the amount', () => {
    type('#cx-gross', '0.05');
    expect(verdict()).toContain('Not settled');
    expect(verdict()).toContain('DustPayment()');
    type('#cx-gross', '30.00');
  });

  it('adds and removes recipients, 1 to 5, and the first takes the remainder', () => {
    const add = $<HTMLButtonElement>('#cx-add');
    add.click();
    add.click();
    add.click();
    expect(page.querySelectorAll('[data-share]')).toHaveLength(5);
    expect(add.disabled).toBe(true);
    expect($('#cx-sum').textContent).toBe('Total 100.00%');
    expect(document.activeElement?.id).toBe('cx-share-4');
    type('#cx-gross', '0.200003');
    expect(page.querySelectorAll('#cx-prism .cx-ray')).toHaveLength(6);
    for (let i = 4; i > 0; i--) $<HTMLButtonElement>(`[data-remove="${i}"]`).click();
    expect(page.querySelectorAll('[data-share]')).toHaveLength(1);
    expect($<HTMLButtonElement>('[data-remove="0"]').disabled).toBe(true);
    type('#cx-share-0', '100');
    type('#cx-gross', '30.00');
    expect(verdict()).toContain('Settled');
    $<HTMLButtonElement>('#cx-add').click();
    expect(($('#cx-share-0') as HTMLInputElement).value).toBe('50.00');
    expect(($('#cx-share-1') as HTMLInputElement).value).toBe('50.00');
  });

  it('switches to base units', () => {
    const units = $<HTMLInputElement>('#cx-units');
    units.checked = true;
    units.dispatchEvent(new Event('change', { bubbles: true }));
    expect(ledger()[0]).toContain('30,000,000');
    units.checked = false;
    units.dispatchEvent(new Event('change', { bubbles: true }));
    expect(ledger()[0]).toContain('30.00 USDC');
  });
});

describe('the two paths', () => {
  it('steps through a path and back, and switches paths', () => {
    const step = () => $('#cx-step').textContent ?? '';
    expect(step()).toContain('Step 1 of 5');
    expect($<HTMLButtonElement>('#cx-prev').disabled).toBe(true);
    $<HTMLButtonElement>('#cx-next').click();
    expect(step()).toContain('Step 2 of 5');
    expect(step()).toContain('The payer sends USDC');
    expect(document.activeElement?.id).toBe('cx-next');
    $<HTMLButtonElement>('[data-step="4"]').click();
    expect(step()).toContain('The net is split');
    expect($('#cx-next').textContent).toBe('Start over');
    expect($('[data-step="4"]').getAttribute('aria-current')).toBe('step');
    $<HTMLButtonElement>('#cx-next').click();
    expect(step()).toContain('Step 1 of 5');
    expect(page.querySelector('#cx-graph .cx-node.cx-on text')?.textContent).toBe('Prismaport');

    $<HTMLButtonElement>('[data-path-id="signature"]').click();
    expect($('[data-path-id="signature"]').getAttribute('aria-pressed')).toBe('true');
    expect($('[data-path-id="deposit"]').getAttribute('aria-pressed')).toBe('false');
    expect(step()).toContain('The request already names the destination');
    $<HTMLButtonElement>('#cx-next').click();
    expect(step()).toContain('USDC (EIP-3009)');
    $<HTMLButtonElement>('#cx-prev').click();
    expect(step()).toContain('Step 1 of 5');
    expect(page.querySelectorAll('#cx-graph .cx-gedge.cx-on')).toHaveLength(1);
    $<HTMLButtonElement>('[data-path-id="deposit"]').click();
  });
});

describe('the tamper lab', () => {
  it('lists ten attempts and shows the chosen one', () => {
    const buttons = page.querySelectorAll('[data-attempt]');
    expect(buttons).toHaveLength(10);
    $<HTMLButtonElement>('[data-attempt="fee"]').click();
    expect($('[data-attempt="fee"]').getAttribute('aria-pressed')).toBe('true');
    expect(page.querySelectorAll('[data-attempt][aria-pressed="true"]')).toHaveLength(1);
    const result = $('#cx-result').textContent ?? '';
    expect(result).toContain('Reverted: FeeBpsTooHigh(1000)');
    expect(result).toContain('MAX_FEE_BPS = 300');
    $<HTMLButtonElement>('[data-attempt="front"]').click();
    expect($('#cx-result').textContent).toContain('USDC (EIP-3009)');
    expect($('#cx-result').textContent).toContain('require(to == msg.sender)');
  });
});

describe('copy', () => {
  it('uses plain words: no em dashes, no line counts, no gas for the fixed fee', () => {
    const paths = page.querySelectorAll('[data-path-id]');
    let text = '';
    for (const p of paths) {
      (p as HTMLElement).click();
      for (let i = 0; i < 5; i++) {
        text += $('#cx-step').textContent;
        $<HTMLButtonElement>('#cx-next').click();
      }
    }
    for (const a of page.querySelectorAll<HTMLButtonElement>('[data-attempt]')) {
      a.click();
      text += $('#cx-result').textContent;
    }
    text += page.textContent;
    expect(text).not.toContain('—');
    expect(text).not.toMatch(/\bgas\b/i);
    expect(text).not.toMatch(/\d+ lines/);
    expect(text).toContain('Three contracts, two libraries');
    expect(text).toContain('Anchor program');
  });
});
