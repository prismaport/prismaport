// @vitest-environment node
import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { APP_VERSION, GIT_COMMIT, versionLabel } from '../src/lib/version';

const pkg = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf8')) as { version: string };

describe('version', () => {
  it('exposes the release as semver', () => {
    expect(APP_VERSION).toMatch(/^\d+\.\d+\.\d+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/);
  });

  // Guards against a dead Vite define: the fallback '0.0.0' would pass the regex above but not this.
  it('injects the package.json version at build time', () => {
    expect(APP_VERSION).toBe(pkg.version);
  });

  it('exposes a non-empty commit', () => {
    expect(GIT_COMMIT.length).toBeGreaterThan(0);
  });

  it('builds a v<semver>+<commit> label', () => {
    expect(versionLabel).toMatch(/^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?\+.+$/);
  });
});
