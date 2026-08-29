// Type-strip the *.test.ts files with esbuild, then run them on Node's
// built-in test runner. No test framework dependency, no watcher, no config.

import { build } from 'esbuild';
import { readdir, rm } from 'node:fs/promises';
import { spawn } from 'node:child_process';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = dirname(fileURLToPath(import.meta.url));
const outdir = resolve(root, '.tmp-test');

async function findTests(dir) {
  const out = [];
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...(await findTests(full)));
    else if (entry.name.endsWith('.test.ts')) out.push(full);
  }
  return out;
}

const entryPoints = await findTests(resolve(root, 'src'));
if (entryPoints.length === 0) {
  console.error('no *.test.ts files found');
  process.exit(1);
}

await rm(outdir, { recursive: true, force: true });
await build({
  absWorkingDir: root,
  entryPoints,
  outdir,
  outbase: 'src',
  bundle: true,
  platform: 'node',
  format: 'esm',
  target: 'node18',
  sourcemap: 'inline',
  // Keep node built-ins and jsdom external; bundle our own source.
  packages: 'external',
  loader: { '.css': 'empty' },
  logLevel: 'warning',
});

const child = spawn(
  process.execPath,
  ['--test', ...(process.argv.includes('--watch') ? ['--watch'] : []), outdir],
  { stdio: 'inherit', cwd: root },
);
child.on('exit', (code) => process.exit(code ?? 1));
