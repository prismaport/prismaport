import './fonts.css';
import './styles.css';
import { CheckoutApi } from './lib/api';
import { apiOrigin } from './lib/origin';
import { COPY } from './lib/copy';
import { mountAddress } from './pages/address';
import { mountLink } from './pages/link';
import { clientReferenceFromSearch } from './lib/reference';
import { brandHeader, footer, messageCard, renderFailedCard } from './ui';

/** Last resort for anything that escapes a page's own handling: never leave the payer on a spinner. */
export function showFatal(root: HTMLElement): void {
  root.replaceChildren(brandHeader(), renderFailedCard(), footer());
  root.removeAttribute('aria-busy');
}

const root = document.getElementById('app')!;
const api = new CheckoutApi(apiOrigin());
const path = window.location.pathname;
const pay = /^\/pay\/(ses_[0-9A-Za-z]+)\/?$/.exec(path);
// Links are plink_ ids; a prod_ id is a product URL from before products and links were apart.
const link = /^\/l\/((?:plink|prod)_[0-9A-Za-z]+)\/?$/.exec(path);
// A static customer address: the same deposit address every time.
const address = /^\/a\/(caddr_[0-9A-Za-z]+)\/?$/.exec(path);

const fatal = () => showFatal(root);
// Stray errors after the page has mounted (a wallet extension, say) must not wipe a payment in progress.
const stray = () => {
  if (root.hasAttribute('aria-busy')) fatal();
};
window.addEventListener('unhandledrejection', stray);
window.addEventListener('error', stray);

if (pay?.[1]) {
  const sessionId = pay[1];
  // Wallet code (viem, Solana) is only needed to pay, so only /pay loads it.
  void Promise.all([import('./pages/pay'), import('./wallet/evm'), import('./wallet/solana')])
    .then(([{ mountPay }, { discoverWallets, watchWallets }, { backendSolanaApi }]) =>
      mountPay(root, sessionId, { api, discoverEvm: () => discoverWallets(window), watchEvm: (cb) => watchWallets(window, cb), solanaApi: backendSolanaApi(api) }))
    .catch(fatal);
} else if (link?.[1]) {
  mountLink(root, link[1], { api, clientReference: clientReferenceFromSearch(window.location.search) }).catch(fatal);
} else if (address?.[1]) {
  mountAddress(root, address[1], { api }).catch(fatal);
} else {
  root.replaceChildren(brandHeader(), messageCard('Page not found', COPY.notFound, 'title'), footer());
  root.removeAttribute('aria-busy');
}
