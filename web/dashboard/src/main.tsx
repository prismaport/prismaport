import { createRoot } from 'react-dom/client';
import { App } from './App';
import { makeQueryClient } from './lib/queries';
import './styles/fonts.css';
import './styles/tokens.css';
import './styles/app.css';

declare const __MOCK__: boolean;
declare const __ARTIFACT__: boolean;

async function start() {
  // Compiled out of the production build: __MOCK__ is the literal false there.
  if (__ARTIFACT__) {
    const { installFetchShim } = await import('./mocks/fetch-shim');
    installFetchShim();
  } else if (__MOCK__) {
    const { startMockWorker } = await import('./mocks/browser');
    await startMockWorker();
  }
  const root = document.getElementById('root');
  if (!root) return;
  createRoot(root).render(<App client={makeQueryClient()} />);
}

void start();
