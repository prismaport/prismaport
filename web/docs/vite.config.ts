import { execSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import type { Plugin } from 'vite';
import { defineConfig } from 'vitest/config';

const OPENAPI = new URL('../../docs/openapi.json', import.meta.url);

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
// SOURCE_DATE_EPOCH (seconds) pins the time, so CI's build for the Go tests, which embed the
// portal, comes out byte for byte the same and Go's test cache holds.
const epoch = process.env.SOURCE_DATE_EPOCH ? Number(process.env.SOURCE_DATE_EPOCH) : NaN;
const builtAt = new Date(Number.isFinite(epoch) ? epoch * 1000 : Date.now()).toISOString();

// version.json lets the deployed build be probed without parsing the bundle.
const versionJSON = (): Plugin => ({
  name: 'prismaport-version-json',
  apply: 'build',
  generateBundle() {
    this.emitFile({ type: 'asset', fileName: 'version.json', source: JSON.stringify({ version: pkg.version, commit, built_at: builtAt }) });
  },
});

// The contract the portal renders is also published next to it, so clients can generate from it.
const publishOpenAPI = (): Plugin => ({
  name: 'prismaport-openapi',
  configureServer(server) {
    server.middlewares.use((req, res, next) => {
      if (req.url?.split('?')[0] !== '/openapi.json') return next();
      res.setHeader('Content-Type', 'application/json');
      res.end(readFileSync(OPENAPI));
    });
  },
  generateBundle() {
    this.emitFile({ type: 'asset', fileName: 'openapi.json', source: readFileSync(OPENAPI, 'utf8') });
  },
});

// A relative base: the binary serves the portal at /docs/ and its Pages project at the root of
// docs.prismaport.com, from this one build. Its routes are URL fragments, so every document is
// the index next to its files.
export default defineConfig({
  base: './',
  plugins: [publishOpenAPI(), versionJSON()],
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
    port: 5200,
    fs: { allow: ['.', '../../docs'] },
  },
  test: {
    environment: 'jsdom',
    include: ['tests/**/*.test.ts'],
    // The tests that render every page take over 15 s on a busy self-hosted runner.
    testTimeout: 30_000,
  },
});
