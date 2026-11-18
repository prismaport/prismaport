// Fails the production build when it would ship mocks, contact another origin or lose hashing.
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../', import.meta.url));
const dist = resolve(root, process.argv[2] ?? 'dist');

const files = [];
const walk = (dir) => {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) walk(p);
    else files.push(p);
  }
};
walk(dist);

const problems = [];
const rel = (p) => relative(dist, p).replace(/\\/g, '/');
const names = files.map(rel);

if (!names.includes('index.html')) problems.push('index.html is missing');
if (names.some((n) => /mockServiceWorker/.test(n))) problems.push('the mock service worker is in the build');

for (const f of files.filter((p) => /\.(js|css|html)$/.test(p))) {
  const text = readFileSync(f, 'utf8');
  if (/mockServiceWorker|setupWorker|msw\//.test(text)) problems.push(`${rel(f)} references the mocks`);
  if (/fonts\.googleapis|fonts\.gstatic/.test(text)) problems.push(`${rel(f)} loads fonts from Google`);
}

const assets = names.filter((n) => n.startsWith('assets/'));
for (const a of assets) {
  if (!/-[A-Za-z0-9_-]{8,}\.[a-z0-9]+$/.test(a)) problems.push(`${a} has no content hash`);
}

const html = readFileSync(join(dist, 'index.html'), 'utf8');
// The API origin and the docs address are written by `prismaport pages build`, never by the Vite build.
for (const name of ['prismaport-api', 'prismaport-docs']) {
  if ((html.match(new RegExp(`<meta name="${name}" content=""`, 'g')) ?? []).length !== 1) problems.push(`index.html must have one empty ${name} meta tag`);
}
for (const m of html.matchAll(/(?:src|href)="([^"]+)"/g)) {
  if (!m[1].startsWith('/dashboard/')) problems.push(`index.html references ${m[1]} outside /dashboard/`);
}

if (problems.length > 0) {
  console.error(`dist check failed:\n- ${problems.join('\n- ')}`);
  process.exit(1);
}
console.log(`dist check passed: ${names.length} files, ${assets.length} hashed assets, no mocks.`);
