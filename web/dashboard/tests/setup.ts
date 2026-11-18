import '@testing-library/jest-dom/vitest';
import { cleanup, configure } from '@testing-library/react';
import { afterEach } from 'vitest';

// Node's fetch has no document base, so the app's relative /v1 URLs are resolved like a browser would.
const nodeFetch = globalThis.fetch;
globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) =>
  nodeFetch(typeof input === 'string' && input.startsWith('/') ? new URL(input, window.location.href) : input, init)) as typeof fetch;

// The suite runs files in parallel; on a busy machine the first render of a file can take more
// than the default second, and a busy self-hosted runner more than 6 s.
configure({ asyncUtilTimeout: 12_000 });

afterEach(() => {
  cleanup();
  try {
    localStorage.clear();
  } catch {
    // jsdom without storage
  }
});
