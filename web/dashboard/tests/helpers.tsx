import { render } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { RequestHandler } from 'msw';
import { setupServer } from 'msw/node';
import { afterAll, afterEach, beforeAll } from 'vitest';
import { TestApp } from '../src/App';
import { makeQueryClient } from '../src/lib/queries';
import type { Mode } from '../src/lib/types';
import { createMockApi } from '../src/mocks/handlers';

export const server = setupServer();

export function useMockServer(): void {
  beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
  afterEach(() => server.resetHandlers());
  afterAll(() => server.close());
}

export function renderApp(path: string, opts: { signedIn?: boolean; mode?: Mode; email?: string; handlers?: RequestHandler[]; noEmail?: boolean; window?: boolean } = {}) {
  if (opts.mode) localStorage.setItem('prismaport.dashboard.mode', opts.mode);
  // noEmail: a server with no email set up, where only the authenticator confirms. window: the
  // confirmation window on (D59/D61), as on every hosted server; off by default here, so each Live
  // change shows its own code field.
  const mock = createMockApi({ email: !opts.noEmail, confirmWindow: opts.window ?? false });
  if (opts.signedIn ?? true) mock.signIn(opts.email);
  server.use(...mock.handlers);
  // Later handlers win, so overrides go last.
  if (opts.handlers) server.use(...opts.handlers);
  const client = makeQueryClient();
  client.setDefaultOptions({ queries: { retry: false, staleTime: 0 }, mutations: { retry: false } });
  const user = userEvent.setup();
  const utils = render(<TestApp client={client} path={path} mode={opts.mode ?? 'live'} />);
  return { ...utils, mock, client, user };
}

/** Confirms a Live action the way a person without 2FA does: asks for the emailed code, then types it. */
export async function emailCode(user: ReturnType<typeof userEvent.setup>, scope: { getByRole: (r: 'button', o: { name: string }) => HTMLElement; findByLabelText: (l: string) => Promise<HTMLElement> }, code = '246810'): Promise<void> {
  await user.click(scope.getByRole('button', { name: 'Email me a code' }));
  await user.type(await scope.findByLabelText('Code from your email'), code);
}
