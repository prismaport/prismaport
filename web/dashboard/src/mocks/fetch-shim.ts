import { getResponse } from 'msw';
import { createMockApi } from './handlers';

/** Artifact preview only: answers /v1/dashboard by wrapping fetch, since the host page cannot run a service worker. */
export function installFetchShim(): void {
  const api = createMockApi({ persist: false, latency: 180 });
  const original = globalThis.fetch.bind(globalThis);
  globalThis.fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(input, init);
    const mocked = await getResponse(api.handlers, request);
    return mocked ?? original(input, init);
  };
}
