// Copies the repo's install.sh into site/ so the website can serve it from its own base URL.
// The root install.sh stays the single source of truth; the copy is gitignored and rebuilt on
// every dev/build/test run, so it can never drift. Fails loudly instead of shipping a bad file.
import { readFileSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const src = path.resolve(here, '../../install.sh');
const dst = path.resolve(here, '../site/install.sh');

function fail(msg) {
  console.error(`sync-install: ${msg}`);
  process.exit(1);
}

let data;
try {
  data = readFileSync(src);
} catch {
  fail(
    `cannot read ${src}\n` +
      '  On Vercel: Project Settings > General > Root Directory = "ui", and keep ' +
      '"Include source files outside of the Root Directory in the Build Step" turned ON.',
  );
}
if (data.length === 0) fail(`${src} is empty`);
if (!data.subarray(0, 2).equals(Buffer.from('#!'))) fail(`${src} does not start with a #! line`);

writeFileSync(dst, data);
console.log(`sync-install: copied install.sh (${data.length} bytes) -> site/install.sh`);
