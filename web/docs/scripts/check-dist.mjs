// Fails the build when it would contact another origin, lose hashing or ship without the contract.
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const dist = fileURLToPath(new URL('../dist/', import.meta.url));
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
const rel = (p) => relative(dist, p).replaceAll('\\', '/');
const names = files.map(rel);

for (const required of ['index.html', 'openapi.json']) {
  if (!names.includes(required)) problems.push(`${required} is missing`);
}
const contract = readFileSync(fileURLToPath(new URL('../../../docs/openapi.json', import.meta.url)), 'utf8');
if (names.includes('openapi.json') && readFileSync(join(dist, 'openapi.json'), 'utf8') !== contract) {
  problems.push('dist/openapi.json differs from docs/openapi.json');
}
for (const f of files.filter((p) => /\.(js|css|html)$/.test(p))) {
  const text = readFileSync(f, 'utf8');
  if (/fonts\.googleapis|fonts\.gstatic/.test(text)) problems.push(`${rel(f)} loads fonts from Google`);
  const inline = [
    [/\sstyle\s*=\s*["']/, 'a style attribute'],
    [/<style[\s>]/, 'a <style> element'],
    [/<script(?![^>]*\ssrc=)[^>]*>/, 'an inline <script>'],
    [/\son[a-z]+\s*=\s*["']/, 'an inline event handler'],
    [/\.style\.|\.style\s*=|setAttribute\(\s*["']style/, 'a style set from script'],
  ];
  for (const [re, what] of inline) {
    if (f.endsWith('.css') && what !== 'a <style> element') continue;
    if (re.test(text)) problems.push(`${rel(f)} has ${what}, which the CSP blocks`);
  }
}
for (const a of names.filter((n) => n.startsWith('assets/'))) {
  if (!/-[A-Za-z0-9_-]{8,}\.[a-z0-9]+$/.test(a)) problems.push(`${a} has no content hash`);
}
// One build serves at /docs/ (the binary) and at the root of the docs host (Pages): its own files
// are referenced relatively, only the dashboard by path.
const html = readFileSync(join(dist, 'index.html'), 'utf8');
for (const m of html.matchAll(/(?:src|href)="([^"]+)"/g)) {
  const ref = m[1];
  const own = /^(\.\/)?[A-Za-z0-9_-][^:]*$/.test(ref) && !ref.includes('..');
  if (!own && !ref.startsWith('/dashboard/') && !ref.startsWith('#')) problems.push(`index.html references ${ref}, neither relative nor a dashboard page`);
}
for (const f of files.filter((p) => /\.(js|css|html)$/.test(p))) {
  if (/["'(]\/docs\//.test(readFileSync(f, 'utf8'))) problems.push(`${rel(f)} refers to /docs/, which is not the portal's path on the docs host`);
}

if (problems.length) {
  console.error('check-dist failed:\n  ' + problems.join('\n  '));
  process.exit(1);
}
console.log(`check-dist: ${names.length} files OK`);
