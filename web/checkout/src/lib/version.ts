// Build identity injected by Vite at build time (vite.config.ts). The release tracks the root
// VERSION file; the commit is the deployed SHA.
declare const __APP_VERSION__: string;
declare const __GIT_COMMIT__: string;

export const APP_VERSION = typeof __APP_VERSION__ !== 'undefined' ? __APP_VERSION__ : '0.0.0';
export const GIT_COMMIT = typeof __GIT_COMMIT__ !== 'undefined' ? __GIT_COMMIT__ : 'dev';

export const versionLabel = `v${APP_VERSION}+${GIT_COMMIT}`;
