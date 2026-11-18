import { execSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import type { Plugin } from 'vite';
import { defineConfig } from 'vitest/config';

// Build identity: the release from package.json (kept equal to the root VERSION file by
// scripts/set-version.sh), the commit from the deploy's environment or the local checkout.
const pkg = JSON.parse(readFileSync(new URL('./package.json', import.meta.url), 'utf8')) as { version: string };
const gitHead = (): string => {
  try {
    return execSync('git rev-parse HEAD', { stdio: ['ignore', 'pipe', 'ignore'] }).toString().trim();
  } catch {
    return ''; // the image build has no .git
  }
};
const commit = (process.env.RAILWAY_GIT_COMMIT_SHA || process.env.GIT_COMMIT || gitHead() || 'dev').slice(0, 7);
const builtAt = new Date().toISOString();

// version.json lets the deployed build be probed without parsing the bundle.
const versionJSON = (): Plugin => ({
  name: 'prismaport-version-json',
  apply: 'build',
  generateBundle() {
    this.emitFile({ type: 'asset', fileName: 'version.json', source: JSON.stringify({ version: pkg.version, commit, built_at: builtAt }) });
  },
});

// In dev, /l/{product_id} and /a/{customer_address_id} are served by the same app shell as /pay/ (the Go server does this in
// production).
const linkShell = (): Plugin => ({
  name: 'prismaport-link-shell',
  configureServer(server) {
    server.middlewares.use((req, _res, next) => {
      if (req.url && /^\/(l\/prod_|a\/caddr_|pay\/ses_)[0-9A-Za-z]+$/.test(req.url.split('?')[0] ?? '')) req.url = '/pay/index.html';
      next();
    });
  },
});

export default defineConfig({
  base: '/pay/',
  plugins: [linkShell(), versionJSON()],
  define: {
    __APP_VERSION__: JSON.stringify(pkg.version),
    __GIT_COMMIT__: JSON.stringify(commit),
  },
  build: {
    outDir: 'dist',
    emptyOutDir: false,
    target: 'es2022',
    sourcemap: false,
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
  },
  server: {
    port: 5180,
    proxy: { '/v1': 'http://localhost:8080' },
  },
  test: {
    environment: 'jsdom',
    include: ['tests/**/*.test.ts'],
  },
});
