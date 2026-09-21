import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const rootScript = readFileSync(path.resolve(here, '../../install.sh'));

test('GET /install.sh is plain text and byte-identical to the repo-root install.sh', async ({ request }) => {
  const res = await request.get('/install.sh');
  expect(res.status()).toBe(200);
  expect(res.headers()['content-type']).toMatch(/^text\/plain/);
  const body = await res.body();
  expect(body.subarray(0, 12).toString()).toBe('#!/bin/sh\n# ');
  expect(Buffer.compare(body, rootScript)).toBe(0); // the drift guard
});

test('the served install.sh is valid shell (sh -n and bash -n)', async ({ request }) => {
  const body = await (await request.get('/install.sh')).body();
  const dir = mkdtempSync(path.join(tmpdir(), 'relay-ui-'));
  try {
    const file = path.join(dir, 'install.sh');
    writeFileSync(file, body);
    execFileSync('sh', ['-n', file]);
    execFileSync('bash', ['-n', file]);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('the site does not serve its own source scripts', async ({ request }) => {
  for (const p of ['/scripts/serve.mjs', '/package.json', '/../package.json', '/%2e%2e/package.json']) {
    const res = await request.get(p);
    expect([403, 404]).toContain(res.status());
  }
});
