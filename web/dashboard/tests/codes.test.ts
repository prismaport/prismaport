import { describe, expect, it } from 'vitest';
import { ApiError } from '../src/lib/api';
import { CODE_LIFETIME_MS, codeFailure, incompleteCode, wrongCode } from '../src/lib/codes';

const invalid = new ApiError(400, 'invalid_code', 'That code is not valid or has expired.');
const limited = new ApiError(429, 'rate_limited', 'Too many attempts. Wait a minute and try again.', 40);
const SENT = 1_000_000;

describe('codeFailure', () => {
  it('counts the tries left of a code this page asked for', () => {
    expect(codeFailure(invalid, { failures: 1, sentAt: SENT }, SENT + 1000)).toEqual({ status: 'invalid', message: "That code isn't right. 4 tries left." });
    expect(codeFailure(invalid, { failures: 4, sentAt: SENT }, SENT + 1000)).toEqual({ status: 'invalid', message: "That code isn't right. 1 try left." });
  });

  it('the fifth wrong code locks the field until a new one is sent', () => {
    expect(codeFailure(invalid, { failures: 5, sentAt: SENT }, SENT + 1000)).toEqual({ status: 'locked', message: 'Too many wrong codes. Send a new one in a few minutes.' });
  });

  it('a code past its 15 minutes is expired, whatever the tries', () => {
    expect(codeFailure(invalid, { failures: 1, sentAt: SENT }, SENT + CODE_LIFETIME_MS)).toEqual({ status: 'expired', message: 'This code expired. Send a new one.' });
    expect(codeFailure(invalid, { failures: 5, sentAt: SENT }, SENT + CODE_LIFETIME_MS + 1)).toMatchObject({ status: 'expired' });
  });

  it('does not invent a count for a code it did not send', () => {
    expect(codeFailure(invalid, { failures: 1, sentAt: null }, SENT)).toEqual({
      status: 'invalid',
      message: "That code isn't right, or it expired. Check the newest email, or send a new code.",
    });
    expect(codeFailure(invalid, { failures: 5, sentAt: null }, SENT)).toMatchObject({ status: 'locked' });
  });

  it('too many tries in a minute lock the field', () => {
    expect(codeFailure(limited, { failures: 0, sentAt: SENT }, SENT)).toEqual({ status: 'locked', message: 'Too many wrong codes. Send a new one in a few minutes.' });
  });

  it('leaves other errors to the page', () => {
    expect(codeFailure(new ApiError(0, 'network_error', 'offline'), { failures: 0, sentAt: SENT }, SENT)).toBeNull();
    expect(codeFailure(new ApiError(400, 'invalid_request', 'Use 8 to 256 characters.'), { failures: 0, sentAt: SENT }, SENT)).toBeNull();
    expect(codeFailure(new Error('boom'), { failures: 0, sentAt: SENT }, SENT)).toBeNull();
  });
});

describe('code copy', () => {
  it('says how many digits are missing', () => {
    expect(incompleteCode(0)).toBe('Enter the 6-digit code from the email.');
    expect(incompleteCode(5)).toBe('Type all 6 digits (5 of 6).');
    expect(wrongCode(2)).toBe("That code isn't right. 2 tries left.");
  });
});
