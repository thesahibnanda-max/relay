import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const rootScript = readFileSync(path.resolve(here, '../../install.sh'));
const rootPs1 = readFileSync(path.resolve(here, '../../install.ps1'));

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

test('GET /install.ps1 is plain text and byte-identical to the repo-root install.ps1', async ({ request }) => {
  const res = await request.get('/install.ps1');
  expect(res.status()).toBe(200);
  expect(res.headers()['content-type']).toMatch(/^text\/plain/);
  const body = await res.body();
  expect(body.subarray(0, 9).toString()).toBe('#Requires');
  expect(Buffer.compare(body, rootPs1)).toBe(0); // the drift guard
});

test('the served install.ps1 is valid PowerShell (parses with pwsh)', async ({ request }) => {
  const body = await (await request.get('/install.ps1')).body();
  const dir = mkdtempSync(path.join(tmpdir(), 'relay-ui-'));
  try {
    const file = path.join(dir, 'install.ps1');
    writeFileSync(file, body);
    execFileSync('pwsh', [
      '-NoProfile',
      '-Command',
      `$errors = $null; [System.Management.Automation.Language.Parser]::ParseFile('${file}', [ref]$null, [ref]$errors) | Out-Null; if ($errors.Count -gt 0) { $errors | ForEach-Object { Write-Error $_ }; exit 1 }`,
    ]);
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

test('vercel.json (used with Root Directory = ui) points at real paths and serves install.sh/install.ps1 as text', () => {
  const repo = path.resolve(here, '../..');
  const uiDir = path.join(repo, 'ui'); // Vercel runs the build from the Root Directory
  const cfg = JSON.parse(readFileSync(path.join(repo, 'vercel.json'), 'utf8'));
  expect(existsSync(path.join(uiDir, cfg.outputDirectory, 'index.html'))).toBe(true);
  const script = /^node (\S+)$/.exec(cfg.buildCommand);
  expect(script, 'buildCommand should be "node <script>"').not.toBeNull();
  expect(existsSync(path.join(uiDir, script![1]))).toBe(true);
  for (const source of ['/install.sh', '/install.ps1']) {
    const rule = cfg.headers.find((h: { source: string }) => h.source === source);
    expect(rule, source).toBeTruthy();
    expect(rule.headers).toContainEqual({ key: 'Content-Type', value: 'text/plain; charset=utf-8' });
  }
});

test('every place that shows the install command uses the site URL, not raw.githubusercontent.com', () => {
  const repo = path.resolve(here, '../..');
  const site = 'https://relay-sahib-nanda.vercel.app/install.sh';
  const sitePs1 = 'https://relay-sahib-nanda.vercel.app/install.ps1';
  for (const f of ['README.md', 'install.sh', '.goreleaser.yaml', 'ui/site/index.html']) {
    const text = readFileSync(path.join(repo, f), 'utf8');
    expect(text, f).toContain(site);
    expect(text, f).not.toContain('raw.githubusercontent.com');
  }
  // index.html never hardcodes the PowerShell command as literal text: like the
  // WSL tab (which reuses the Unix command literal), the Windows tab's command is
  // only ever computed by app.js when that tab is selected - there's nothing to
  // check for literally here.
  for (const f of ['README.md', 'install.ps1', '.goreleaser.yaml']) {
    const text = readFileSync(path.join(repo, f), 'utf8');
    expect(text, f).toContain(sitePs1);
  }
});
