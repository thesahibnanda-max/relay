import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
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

test('root vercel.json points at real paths and serves install.sh as text', () => {
  const repo = path.resolve(here, '../..');
  const cfg = JSON.parse(readFileSync(path.join(repo, 'vercel.json'), 'utf8'));
  expect(existsSync(path.join(repo, cfg.outputDirectory, 'index.html'))).toBe(true);
  const script = /^node (\S+)$/.exec(cfg.buildCommand);
  expect(script, 'buildCommand should be "node <script>"').not.toBeNull();
  expect(existsSync(path.join(repo, script![1]))).toBe(true);
  const rule = cfg.headers.find((h: { source: string }) => h.source === '/install.sh');
  expect(rule.headers).toContainEqual({ key: 'Content-Type', value: 'text/plain; charset=utf-8' });
});
