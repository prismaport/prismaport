// Empties dist/ but keeps the placeholder the Go embed directive needs when nothing is built.
import { mkdirSync, readdirSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const dist = fileURLToPath(new URL('../dist/', import.meta.url));
mkdirSync(dist, { recursive: true });
for (const name of readdirSync(dist)) {
  if (name !== '.gitkeep') rmSync(join(dist, name), { recursive: true, force: true });
}
