// Copies the repo's install.sh and install.ps1 into site/ so the website can serve them from its
// own base URL. The root files stay the single source of truth; the copies are gitignored and
// rebuilt on every dev/build/test run, so they can never drift. Fails loudly instead of shipping
// a bad file.
import { readFileSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));

function fail(msg) {
  console.error(`sync-install: ${msg}`);
  process.exit(1);
}

function sync(name, mustStartWith) {
  const src = path.resolve(here, '../../', name);
  const dst = path.resolve(here, '../site', name);
  let data;
  try {
    data = readFileSync(src);
  } catch {
    fail(
      `cannot read ${src}\n` +
        '  The build needs the whole repository. On Vercel, set Root Directory to "ui" (the root vercel.json ' +
        `paths are relative to it); the repository is cloned in full, so ../${name} is available.`,
    );
  }
  if (data.length === 0) fail(`${src} is empty`);
  if (!data.subarray(0, mustStartWith.length).equals(Buffer.from(mustStartWith))) {
    fail(`${src} does not start with ${JSON.stringify(mustStartWith)}`);
  }
  writeFileSync(dst, data);
  console.log(`sync-install: copied ${name} (${data.length} bytes) -> site/${name}`);
}

sync('install.sh', '#!');
sync('install.ps1', '#Requires');
