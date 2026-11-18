import { execSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import react from '@vitejs/plugin-react';
import type { Plugin } from 'vite';
import { defineConfig } from 'vitest/config';

const BASE = '/dashboard/';
const WORKER = 'mockServiceWorker.js';

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

// The MSW worker lives outside public/ so the production build can never ship it. In mock mode it
// is served by the dev server and emitted into the mock build only.
const mockWorker = (): Plugin => {
  const source = () => readFileSync(new URL(`./mock-public/${WORKER}`, import.meta.url), 'utf8');
  return {
    name: 'prismaport-mock-worker',
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        if (req.url?.split('?')[0] !== `${BASE}${WORKER}`) return next();
        res.setHeader('Content-Type', 'text/javascript');
        res.end(source());
      });
    },
    generateBundle() {
      this.emitFile({ type: 'asset', fileName: WORKER, source: source() });
    },
  };
};

export default defineConfig(({ mode }) => ({
  base: mode === 'artifact' ? './' : BASE,
  plugins: [react(), versionJSON(), ...(mode === 'mock' ? [mockWorker()] : [])],
  define: {
    __MOCK__: JSON.stringify(mode === 'mock' || mode === 'artifact'),
    __ARTIFACT__: JSON.stringify(mode === 'artifact'),
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
    port: 5190,
    proxy: mode === 'mock' ? undefined : { '/v1': 'http://localhost:8080' },
  },
  preview: {
    port: 5191,
  },
  test: {
    environment: 'jsdom',
    include: ['tests/**/*.test.{ts,tsx}'],
    setupFiles: ['tests/setup.ts'],
    // The self-hosted runners share a few CPUs with every other job on the machine: in CI two
    // workers keep each test fast enough for its timeout, where one per core starves them all.
    maxWorkers: process.env.CI ? 2 : undefined,
    testTimeout: 30_000,
  },
}));
