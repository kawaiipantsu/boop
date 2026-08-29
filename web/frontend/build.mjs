// Build the Boop WebUI into ./dist.
//
// Output is a flat, relative-path bundle so it works when the Go server mounts
// it at "/" (the normal case) and still works behind a path-prefixing reverse
// proxy. Everything is bundled: no CDN, no network access required at runtime.
//
//   node build.mjs            one-shot production build
//   node build.mjs --watch    rebuild on change (point the Go server at dist/)

import { build, context } from 'esbuild';
import { cp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = dirname(fileURLToPath(import.meta.url));
const outdir = resolve(root, 'dist');
const watch = process.argv.includes('--watch');

/** @type {import('esbuild').BuildOptions} */
const options = {
  absWorkingDir: root,
  entryPoints: [resolve(root, 'src/main.ts')],
  outdir,
  entryNames: 'assets/boop',
  assetNames: 'assets/[name]',
  bundle: true,
  format: 'esm',
  target: ['es2020', 'chrome100', 'firefox100', 'safari15'],
  platform: 'browser',
  splitting: false,
  sourcemap: watch ? 'inline' : false,
  minify: !watch,
  legalComments: 'none',
  treeShaking: true,
  metafile: true,
  logLevel: 'info',
  define: { 'process.env.NODE_ENV': JSON.stringify(watch ? 'development' : 'production') },
};

async function emitHtml() {
  const html = await readFile(resolve(root, 'index.html'), 'utf8');
  await writeFile(resolve(outdir, 'index.html'), html, 'utf8');
}

async function copyPublic() {
  await cp(resolve(root, 'public'), outdir, { recursive: true });
}

async function prepare() {
  await rm(outdir, { recursive: true, force: true });
  await mkdir(outdir, { recursive: true });
  await copyPublic();
  await emitHtml();
}

if (watch) {
  await prepare();
  const ctx = await context(options);
  await ctx.watch();
  console.log('[boop] watching; output in dist/');
} else {
  await prepare();
  const result = await build(options);
  const bytes = Object.values(result.metafile.outputs).reduce((n, o) => n + o.bytes, 0);
  console.log(`[boop] built dist/ (${(bytes / 1024).toFixed(1)} kB of js+css)`);
}
