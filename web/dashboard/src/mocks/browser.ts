import { setupWorker } from 'msw/browser';
import { createMockApi } from './handlers';

/** Dev and mock builds only: answers /v1/dashboard from an in-browser store kept per tab. */
export async function startMockWorker(): Promise<void> {
  const api = createMockApi({ persist: true, latency: 180 });
  const worker = setupWorker(...api.handlers);
  await worker.start({
    serviceWorker: { url: `${import.meta.env.BASE_URL}mockServiceWorker.js`, options: { scope: import.meta.env.BASE_URL } },
    onUnhandledRequest: 'bypass',
    quiet: true,
  });
}
