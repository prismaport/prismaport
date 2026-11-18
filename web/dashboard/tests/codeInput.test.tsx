import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { CodeInput, sanitizeCode, type CodeInputProps, type CodeStatus } from '../src/components/CodeInput';

type HarnessProps = Partial<Omit<CodeInputProps, 'value' | 'onChange'>> & { initial?: string; onValue?: (v: string) => void };

function Harness({ initial = '', onValue, label = 'Confirmation code', ...rest }: HarnessProps) {
  const [value, setValue] = useState(initial);
  return (
    <>
      <CodeInput
        id="code"
        label={label}
        value={value}
        onChange={(v) => {
          setValue(v);
          onValue?.(v);
        }}
        {...rest}
      />
      <p id="extra">Extra help</p>
    </>
  );
}

const boxes = () => [...document.querySelectorAll('.code-box')].map((b) => b.textContent);
const activeBox = () => [...document.querySelectorAll('.code-box')].findIndex((b) => b.hasAttribute('data-active'));
const field = () => document.querySelector('.code-field')!;

describe('sanitizeCode', () => {
  it('keeps digits only in numeric mode and letters and digits in alphanumeric mode, up to the length', () => {
    expect(sanitizeCode('246 810', 'numeric', 6)).toBe('246810');
    expect(sanitizeCode('246-810-99', 'numeric', 6)).toBe('246810');
    expect(sanitizeCode('a1b2', 'numeric', 6)).toBe('12');
    expect(sanitizeCode(' 36-58 A5 ', 'alphanumeric', 6)).toBe('3658A5');
    expect(sanitizeCode('0x1234ab', 'alphanumeric', 4)).toBe('0x12');
  });
});

describe('CodeInput', () => {
  it('is a single labelled textbox over six hidden boxes, ready for one-time-code autofill', () => {
    render(<Harness hint="6 digits, from the email." describedBy="extra" />);
    const boxesEl = document.querySelector('.code-boxes')!;
    expect(boxesEl).toHaveAttribute('aria-hidden', 'true');
    expect(boxes()).toHaveLength(6);
    expect(screen.getAllByRole('textbox')).toHaveLength(1);
    const input = screen.getByRole('textbox', { name: 'Confirmation code' });
    expect(input).toHaveAttribute('autocomplete', 'one-time-code');
    expect(input).toHaveAttribute('inputmode', 'numeric');
    expect(input).toHaveAttribute('pattern', '[0-9]*');
    expect(input).toHaveAttribute('aria-describedby', 'code-hint code-message extra');
    expect(input).toHaveAccessibleDescription(/^6 digits, from the email\.\s+Extra help$/);
    expect(input).not.toHaveAttribute('aria-invalid');
  });

  it('fills the boxes as digits are typed, moves the active box along, and Backspace goes back', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    const input = screen.getByRole('textbox');
    expect(activeBox()).toBe(0);
    await user.type(input, '246');
    expect(input).toHaveValue('246');
    expect(boxes()).toEqual(['2', '4', '6', '', '', '']);
    expect(activeBox()).toBe(3);
    expect(document.querySelectorAll('.code-box[data-filled]')).toHaveLength(3);
    await user.keyboard('{Backspace}');
    expect(input).toHaveValue('24');
    expect(activeBox()).toBe(2);
    await user.keyboard('{Backspace}{Backspace}{Backspace}');
    expect(input).toHaveValue('');
    expect(activeBox()).toBe(0);
  });

  it('drops anything but digits in numeric mode and stops at the length', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    const input = screen.getByRole('textbox');
    await user.type(input, '2a4 -6b8x1y0z9');
    expect(input).toHaveValue('246810');
    expect(boxes()).toEqual(['2', '4', '6', '8', '1', '0']);
    expect(activeBox()).toBe(5);
  });

  it.each(['246 810', '246-810', ' 2 4 6 8 1 0 '])('fills all six from a paste of "%s" and completes once', async (pasted) => {
    const user = userEvent.setup();
    const onComplete = vi.fn();
    render(<Harness onComplete={onComplete} />);
    await user.click(screen.getByRole('textbox'));
    await user.paste(pasted);
    expect(screen.getByRole('textbox')).toHaveValue('246810');
    expect(boxes()).toEqual(['2', '4', '6', '8', '1', '0']);
    expect(onComplete).toHaveBeenCalledTimes(1);
    expect(onComplete).toHaveBeenCalledWith('246810');
  });

  it('a paste replaces a half-typed code', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.type(screen.getByRole('textbox'), '99');
    await user.paste('246-810');
    expect(screen.getByRole('textbox')).toHaveValue('246810');
  });

  it('calls onComplete only when the last box is filled', async () => {
    const user = userEvent.setup();
    const onComplete = vi.fn();
    render(<Harness onComplete={onComplete} />);
    const input = screen.getByRole('textbox');
    await user.type(input, '24681');
    expect(onComplete).not.toHaveBeenCalled();
    await user.type(input, '0');
    expect(onComplete).toHaveBeenCalledExactlyOnceWith('246810');
    await user.type(input, '7');
    expect(onComplete).toHaveBeenCalledTimes(1);
  });

  it('alphanumeric and case-insensitive: shows what was typed and completes with it lowercased', async () => {
    const user = userEvent.setup();
    const onComplete = vi.fn();
    render(<Harness mode="alphanumeric" caseInsensitive onComplete={onComplete} label="Confirm the last 6 characters" />);
    const input = screen.getByRole('textbox', { name: 'Confirm the last 6 characters' });
    expect(input).toHaveAttribute('autocomplete', 'one-time-code');
    expect(input).toHaveAttribute('inputmode', 'text');
    expect(input).not.toHaveAttribute('pattern');
    expect(input).toHaveAttribute('autocapitalize', 'off');
    expect(input).toHaveAttribute('spellcheck', 'false');
    await user.type(input, '36-58 A5');
    expect(input).toHaveValue('3658A5');
    expect(boxes()).toEqual(['3', '6', '5', '8', 'A', '5']);
    expect(onComplete).toHaveBeenCalledExactlyOnceWith('3658a5');
  });

  it('alphanumeric and case-sensitive: completes with the exact case', async () => {
    const user = userEvent.setup();
    const onComplete = vi.fn();
    render(<Harness mode="alphanumeric" onComplete={onComplete} />);
    await user.type(screen.getByRole('textbox'), 'Lr2Ae1');
    expect(onComplete).toHaveBeenCalledExactlyOnceWith('Lr2Ae1');
  });

  it('draws as many boxes as the length', async () => {
    const user = userEvent.setup();
    render(<Harness length={4} />);
    expect(boxes()).toHaveLength(4);
    await user.type(screen.getByRole('textbox'), '123456');
    expect(screen.getByRole('textbox')).toHaveValue('1234');
  });
});

describe('CodeInput states', () => {
  function Controlled({ status: initial = 'idle', clearOnInvalid, message }: { status?: CodeStatus; clearOnInvalid?: boolean; message?: string }) {
    const [value, setValue] = useState('246811');
    const [status, setStatus] = useState<CodeStatus>(initial);
    return (
      <>
        <CodeInput id="code" label="Confirmation code" value={value} onChange={setValue} status={status} message={message} clearOnInvalid={clearOnInvalid} />
        <button type="button" onClick={() => setStatus('invalid')}>
          wrong
        </button>
      </>
    );
  }

  it('idle: no message, neutral boxes', () => {
    render(<Controlled />);
    expect(field()).toHaveAttribute('data-status', 'idle');
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
  });

  it('wrong code: amber, a short shake, the message, then cleared and focused again', async () => {
    const user = userEvent.setup();
    render(<Controlled message="That code isn't right. 4 tries left." />);
    const input = screen.getByRole('textbox');
    await user.click(screen.getByRole('button', { name: 'wrong' }));
    expect(field()).toHaveAttribute('data-status', 'invalid');
    expect(field()).toHaveAttribute('data-shake');
    expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByRole('status')).toHaveTextContent("That code isn't right. 4 tries left.");
    expect(screen.getByRole('status')).toHaveAttribute('data-tone', 'error');
    expect(input).toHaveAccessibleDescription("That code isn't right. 4 tries left.");
    expect(input).toHaveValue('246811');
    await waitFor(() => expect(input).toHaveValue(''));
    expect(input).toHaveFocus();
    expect(field()).not.toHaveAttribute('data-shake');
    // The message stays until the next attempt.
    expect(screen.getByRole('status')).toHaveTextContent("That code isn't right.");
  });

  it('keeps the typed value after a wrong one when clearing is off', async () => {
    vi.useFakeTimers();
    try {
      render(<Controlled clearOnInvalid={false} />);
      act(() => screen.getByRole('button', { name: 'wrong' }).click());
      expect(field()).toHaveAttribute('data-status', 'invalid');
      act(() => vi.advanceTimersByTime(2000));
      expect(screen.getByRole('textbox')).toHaveValue('246811');
    } finally {
      vi.useRealTimers();
    }
  });

  it('confirmed: green boxes, the message, no active box', () => {
    render(<Controlled status="confirmed" message="Email confirmed. Opening your dashboard." />);
    expect(field()).toHaveAttribute('data-status', 'confirmed');
    expect(screen.getByRole('status')).toHaveTextContent('Email confirmed. Opening your dashboard.');
    expect(screen.getByRole('status')).toHaveAttribute('data-tone', 'ok');
    expect(activeBox()).toBe(-1);
    expect(screen.getByRole('textbox')).not.toHaveAttribute('aria-invalid');
  });

  it('expired: amber with the code kept', () => {
    render(<Controlled status="expired" message="This code expired. Send a new one." />);
    expect(field()).toHaveAttribute('data-status', 'expired');
    expect(screen.getByRole('textbox')).toHaveValue('246811');
    expect(screen.getByRole('textbox')).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByRole('status')).toHaveTextContent('This code expired. Send a new one.');
    expect(screen.getByRole('status')).toHaveAttribute('data-tone', 'error');
  });

  it('too many tries: amber and the field is locked', () => {
    render(<Controlled status="locked" message="Too many wrong codes. Send a new one in a few minutes." />);
    expect(field()).toHaveAttribute('data-status', 'locked');
    expect(screen.getByRole('textbox')).toBeDisabled();
    expect(activeBox()).toBe(-1);
    expect(screen.getByRole('status')).toHaveTextContent('Too many wrong codes. Send a new one in a few minutes.');
  });

  it('a neutral message on an idle field, such as a resend notice in another tone', () => {
    render(<CodeInput label="Code" value="" onChange={() => {}} message="New code sent. Check your inbox." tone="ok" />);
    expect(screen.getByRole('status')).toHaveAttribute('data-tone', 'ok');
    expect(document.querySelector('.code-field')).toHaveAttribute('data-status', 'idle');
  });
});
