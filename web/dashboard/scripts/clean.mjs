// Empties a build directory but keeps the placeholder the Go embed directive needs when nothing is built.
import { mkdirSync, readdirSync, rmSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../', import.meta.url));
const dir = resolve(root, process.argv[2] ?? 'dist');
mkdirSync(dir, { recursive: true });
for (const name of readdirSync(dir)) {
  if (name !== '.gitkeep') rmSync(join(dir, name), { recursive: true, force: true });
}
