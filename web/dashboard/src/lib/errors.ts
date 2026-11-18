import { ApiError } from './api';

/** Plain-language copy for API errors: what happened and what to do next. */
export function errorMessage(e: unknown): string {
  if (!(e instanceof ApiError)) return 'Something went wrong on our side. Try again in a moment.';
  switch (e.code) {
    case 'network_error':
      return 'Cannot reach Prismaport right now. Check your connection and try again.';
    case 'unauthenticated':
      return 'Your session ended. Sign in again to continue.';
    case 'csrf_failed':
      return 'This page was open for too long. Reload it and try again.';
    case 'totp_required':
      // The server says whether the code was missing or wrong.
      return e.message || 'Enter the 6-digit code from your authenticator app to continue.';
    case 'invalid_totp':
      return 'That code did not match. Enter the current code from your authenticator app.';
    case 'rate_limited':
      return `Too many attempts. Try again in ${e.retryAfter ?? 60} seconds.`;
    case 'link_unavailable':
      return 'This link is no longer available.';
    case 'idempotency_conflict':
      return 'This request was already sent with different details. Close the form and try again.';
    case 'not_found':
    case 'link_not_found':
    case 'product_not_found':
    case 'session_not_found':
      return 'We could not find this. It may belong to the other mode, Live or Test.';
    case 'server_error':
    case 'internal_error':
      return 'Something went wrong on our side. Try again in a moment.';
    default:
      // Validation and policy errors carry a message written for people.
      return e.message || 'Something went wrong on our side. Try again in a moment.';
  }
}
