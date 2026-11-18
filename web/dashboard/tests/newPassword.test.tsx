import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { NewPasswordField, useNewPassword } from '../src/components/NewPassword';
import { ApiError } from '../src/lib/api';
import { commonOrRepeated, missingMessage, missingParts, passwordChecks, passwordProblem, passwordTooLong } from '../src/lib/password';

const GOOD = 'Tr4vel-the-river';

function Form({ serverError }: { serverError?: ApiError }) {
  const pw = useNewPassword();
  return (
    <form
      noValidate
      onSubmit={(e) => {
        e.preventDefault();
        if (pw.validate() && serverError) pw.refused(serverError);
      }}
    >
      <NewPasswordField {...pw.field} note="Saving signs you out on every device." />
      <button type="submit">Save</button>
    </form>
  );
}

function setup(serverError?: ApiError) {
  const user = userEvent.setup();
  render(<Form serverError={serverError} />);
  return { user, input: screen.getByLabelText('Password') as HTMLInputElement };
}

const rule = (name: string) => screen.getByText(name).closest('li') as HTMLElement;
const done = (name: string) => rule(name).querySelector('[role=img]')?.getAttribute('aria-label') === 'Done';

describe('password rules, mirrored from the server', () => {
  it('counts runes, not code units', () => {
    expect(passwordChecks('😀😀😀').length).toBe(3);
  });

  it('accepts what the server accepts', () => {
    for (const pw of [GOOD, 'Xk7$mvqz', 'Tr4vel the river', 'Ñandú ٣ río']) expect(passwordProblem(pw)).toBeNull();
  });

  it('names each missing item, joined as a sentence', () => {
    expect(missingMessage(missingParts(passwordChecks('abcdefgh')))).toBe('Add an uppercase letter, a number and a special character.');
    expect(missingMessage(missingParts(passwordChecks('abcde1')))).toBe('Add 2 more characters, an uppercase letter and a special character.');
    expect(missingMessage(missingParts(passwordChecks('Abcdef1')))).toBe('Add 1 more character and a special character.');
    expect(missingMessage(missingParts(passwordChecks('Abcdefg1')))).toBe('Add a special character.');
    expect(missingMessage(missingParts(passwordChecks('ABCDEFG1!')))).toBe('Add a lowercase letter.');
  });

  it('refuses common and repeated passwords in any case', () => {
    for (const pw of ['Password123!', 'pASSWORD123!', 'Prismaport1!', 'Senha123!', 'Aa1!Aa1!']) expect(commonOrRepeated(pw)).toBe(true);
    expect(commonOrRepeated('Aa1!Bb2@')).toBe(false);
  });

  it('bounds the length in bytes, like the server', () => {
    expect(passwordTooLong('é'.repeat(128))).toBe(false);
    expect(passwordTooLong('é'.repeat(129))).toBe(true);
    expect(passwordProblem(`Aa1!${'é'.repeat(200)}`)).toBe('Use a password with at most 256 characters.');
  });
});

describe('NewPasswordField', () => {
  it('shows the checklist before typing, linked to the field, with nothing done', () => {
    const { input } = setup();
    expect(input).toHaveAttribute('autocomplete', 'new-password');
    expect(input).toHaveAttribute('type', 'password');
    expect(input).toHaveAccessibleDescription(/Saving signs you out on every device\. Your password manager can fill this in\./);
    expect(input).toHaveAccessibleDescription(/8 characters or more/);
    expect(input).toHaveAccessibleDescription(/Not a common or repeated password/);
    expect(input).not.toHaveAttribute('aria-invalid');
    const items = screen.getAllByRole('listitem').map((li) => li.textContent);
    expect(items).toEqual(['8 characters or more0 / 8', 'Uppercase and lowercase letters', 'A number', 'A special character', 'Not a common or repeated password']);
    expect(screen.getAllByRole('img', { name: 'Not yet' })).toHaveLength(5);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('updates the checklist and the counter as you type, with no error before submit', async () => {
    const { user, input } = setup();
    await user.type(input, 'Abc1');
    expect(rule('8 characters or more')).toHaveTextContent('4 / 8');
    expect(done('Uppercase and lowercase letters')).toBe(true);
    expect(done('A number')).toBe(true);
    expect(done('A special character')).toBe(false);
    expect(done('8 characters or more')).toBe(false);
    expect(screen.queryByRole('alert')).toBeNull();
    await user.type(input, '-the-river');
    expect(rule('8 characters or more')).toHaveTextContent('14 / 8');
    expect(screen.getAllByRole('img', { name: 'Done' })).toHaveLength(5);
  });

  it('has a show and hide button that does not submit', async () => {
    const { user, input } = setup();
    const toggle = screen.getByRole('button', { name: 'Show password' });
    expect(toggle).toHaveAttribute('aria-pressed', 'false');
    expect(toggle).toHaveAttribute('type', 'button');
    await user.click(toggle);
    expect(input).toHaveAttribute('type', 'text');
    const hide = screen.getByRole('button', { name: 'Hide password' });
    expect(hide).toHaveAttribute('aria-pressed', 'true');
    await user.click(hide);
    expect(input).toHaveAttribute('type', 'password');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('lets paste through', async () => {
    const { user, input } = setup();
    await user.click(input);
    await user.paste(GOOD);
    expect(input).toHaveValue(GOOD);
  });

  it('says what is missing only after a submit, and clears it on the next change', async () => {
    const { user, input } = setup();
    await user.type(input, 'abcde1');
    expect(screen.queryByRole('alert')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(screen.getByRole('alert')).toHaveTextContent('Add 2 more characters, an uppercase letter and a special character.');
    expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(input).toHaveAccessibleDescription(/Add 2 more characters/);
    await user.type(input, 'f');
    expect(screen.queryByRole('alert')).toBeNull();
    expect(input).not.toHaveAttribute('aria-invalid');
  });

  it('flags a common password as soon as the other rules are met', async () => {
    const { user, input } = setup();
    await user.type(input, 'Password123');
    expect(screen.queryByRole('alert')).toBeNull();
    await user.type(input, '!');
    expect(screen.getByRole('alert')).toHaveTextContent('This password is too easy to guess. Choose one that is harder to predict.');
    expect(done('Not a common or repeated password')).toBe(false);
  });

  it('flags a repeated password as soon as the other rules are met', async () => {
    const { user, input } = setup();
    await user.type(input, 'Aa1!Aa1!');
    expect(screen.getByRole('alert')).toHaveTextContent('This password is too easy to guess.');
    expect(done('8 characters or more')).toBe(true);
    expect(done('Not a common or repeated password')).toBe(false);
  });

  it('shows the breach refusal from the API and clears it on change', async () => {
    const { user, input } = setup(new ApiError(400, 'breached_password', 'server text'));
    await user.type(input, GOOD);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(screen.getByRole('alert')).toHaveTextContent('This password appeared in a data breach. Choose one you have not used anywhere else.');
    expect(input).toHaveFocus();
    await user.type(input, '2');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('shows a common password refused by the server with the same words', async () => {
    const { user, input } = setup(new ApiError(400, 'common_password', 'server text'));
    await user.type(input, GOOD);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(screen.getByRole('alert')).toHaveTextContent('This password is too easy to guess.');
  });

  it('shows the server message for a password it refuses on the rules', async () => {
    const { user, input } = setup(new ApiError(400, 'weak_password', 'Use 8 to 256 characters.'));
    await user.type(input, GOOD);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(screen.getByRole('alert')).toHaveTextContent('Use 8 to 256 characters.');
  });

  it('leaves other API errors to the page', () => {
    let refused: boolean | undefined;
    function Probe() {
      const pw = useNewPassword();
      refused = pw.refused(new ApiError(400, 'invalid_request', 'x'));
      return null;
    }
    render(<Probe />);
    expect(refused).toBe(false);
  });
});
