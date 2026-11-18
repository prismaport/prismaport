// Fails the production build when it would contact another origin or lose hashing.
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

for (const f of files.filter((p) => /\.(js|css|html)$/.test(p))) {
  if (/fonts\.googleapis|fonts\.gstatic/.test(readFileSync(f, 'utf8'))) problems.push(`${rel(f)} loads fonts from Google`);
}

const assets = names.filter((n) => n.startsWith('assets/'));
for (const a of assets) {
  if (!/-[A-Za-z0-9_-]{8,}\.[a-z0-9]+$/.test(a)) problems.push(`${a} has no content hash`);
}

if (names.includes('index.html')) {
  const html = readFileSync(join(dist, 'index.html'), 'utf8');
  for (const m of html.matchAll(/(?:src|href)="([^"]+)"/g)) {
    if (!m[1].startsWith('/pay/')) problems.push(`index.html references ${m[1]} outside /pay/`);
  }
  // The API origin is written by `prismaport pages build`, never by the Vite build.
  if ((html.match(/<meta name="prismaport-api" content=""/g) ?? []).length !== 1) problems.push('index.html must have one empty prismaport-api meta tag');
}

if (problems.length > 0) {
  console.error(`dist check failed:\n- ${problems.join('\n- ')}`);
  process.exit(1);
}
console.log(`dist check passed: ${names.length} files, ${assets.length} hashed assets.`);
