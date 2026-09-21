import { test, expect } from '@playwright/test';

const REPO = 'https://github.com/thesahibnanda-max/relay';

test('loads cleanly: no console errors, page errors or failed requests', async ({ page }) => {
  const problems: string[] = [];
  page.on('console', (m) => { if (m.type() === 'error') problems.push(`console: ${m.text()}`); });
  page.on('pageerror', (e) => problems.push(`pageerror: ${e.message}`));
  page.on('requestfailed', (r) => { if (!r.url().endsWith('.mp4')) problems.push(`failed: ${r.url()}`); });
  page.on('response', (r) => { if (r.status() >= 400) problems.push(`${r.status()} ${r.url()}`); });
  await page.goto('/');
  await page.waitForLoadState('networkidle');
  expect(problems).toEqual([]);
});

test('has the expected structure', async ({ page }) => {
  await page.goto('/');
  await expect(page).toHaveTitle(/Relay/);
  await expect(page.locator('h1')).toHaveCount(1);
  for (const id of ['problem', 'solution', 'demo', 'install']) {
    await expect(page.locator(`section#${id}`)).toHaveCount(1);
  }
  await expect(page.locator('html')).toHaveAttribute('lang', 'en');
  await expect(page.locator('meta[name="viewport"]')).toHaveCount(1);
});

test('links to the GitHub repository', async ({ page }) => {
  await page.goto('/');
  const hrefs = await page.locator('a[href^="https://github.com/"]').evaluateAll((els) => els.map((e) => e.getAttribute('href')));
  expect(hrefs.filter((h) => h === REPO).length).toBeGreaterThanOrEqual(3); // header, hero, footer
  for (const h of hrefs) expect(h!.startsWith(REPO) || h === 'https://github.com/psrth').toBe(true);
  const external = await page.locator('a[href^="http"]').evaluateAll((els) => els.map((e) => e.getAttribute('rel')));
  for (const rel of external) expect(rel).toContain('noopener');
});

test('the install command points at this site\'s own install.sh', async ({ page, baseURL }) => {
  await page.goto('/');
  for (const id of ['install-cmd', 'install-cmd-2']) {
    await expect(page.locator(`#${id}`)).toHaveText(`curl -fsSL ${baseURL}/install.sh | bash`);
  }
  await expect(page.locator('body')).not.toContainText('raw.githubusercontent.com');
});

test('copy button copies the exact command', async ({ page, context, baseURL, browserName }) => {
  if (browserName === 'chromium') await context.grantPermissions(['clipboard-read', 'clipboard-write']);
  await page.goto('/');
  const btn = page.locator('button[data-copy="#install-cmd"]');
  await btn.click();
  await expect(btn.locator('.copy-label')).toHaveText('Copied!');
  if (browserName === 'chromium') {
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(`curl -fsSL ${baseURL}/install.sh | bash`);
  }
  await expect(btn.locator('.copy-label')).toHaveText('Copy', { timeout: 5000 });
});

test('theme toggle switches and remembers the choice', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark' });
  await page.goto('/');
  const html = page.locator('html');
  await page.locator('#theme-btn').click();
  await expect(html).toHaveAttribute('data-theme', 'light');
  await expect(page.locator('body')).toHaveCSS('background-color', 'rgb(250, 250, 247)');
  await page.reload();
  await expect(html).toHaveAttribute('data-theme', 'light');
  await page.locator('#theme-btn').click();
  await expect(html).toHaveAttribute('data-theme', 'dark');
  await expect(page.locator('body')).toHaveCSS('background-color', 'rgb(10, 11, 13)');
});

test('follows the system light theme by default', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'light' });
  await page.goto('/');
  await expect(page.locator('body')).toHaveCSS('background-color', 'rgb(250, 250, 247)');
});

test('install tabs switch panels with mouse and keyboard', async ({ page }) => {
  await page.goto('/');
  const mac = page.locator('#tab-mac'), linux = page.locator('#tab-linux'), wsl = page.locator('#tab-wsl');
  await expect(page.locator('#panel-mac')).toBeVisible();
  await expect(page.locator('#panel-linux')).toBeHidden();
  await linux.click();
  await expect(page.locator('#panel-linux')).toBeVisible();
  await expect(page.locator('#panel-mac')).toBeHidden();
  await expect(linux).toHaveAttribute('aria-selected', 'true');
  await linux.press('ArrowRight');
  await expect(wsl).toBeFocused();
  await expect(page.locator('#panel-wsl')).toBeVisible();
  await wsl.press('Home');
  await expect(mac).toBeFocused();
  await expect(page.locator('#panel-mac')).toBeVisible();
});

test.describe('small screens', () => {
  test.use({ viewport: { width: 375, height: 700 } });
  test('menu opens, closes on link click and on Escape', async ({ page }) => {
    await page.goto('/');
    const btn = page.locator('#menu-btn'), nav = page.locator('#site-nav');
    await expect(btn).toBeVisible();
    await expect(nav).toBeHidden();
    await btn.click();
    await expect(nav).toBeVisible();
    await expect(btn).toHaveAttribute('aria-expanded', 'true');
    await page.keyboard.press('Escape');
    await expect(nav).toBeHidden();
    await btn.click();
    await nav.getByRole('link', { name: 'Install' }).click();
    await expect(nav).toBeHidden();
  });
});

test.describe('demo video', () => {
  test('is set up to autoplay, muted, on a loop, and is served with range support', async ({ page }) => {
    await page.goto('/');
    const v = page.locator('#demo-video');
    await expect(v).toHaveAttribute('loop', '');
    await expect(v).toHaveAttribute('autoplay', '');
    await expect(v).toHaveAttribute('playsinline', '');
    expect(await v.evaluate((el) => (el as HTMLVideoElement).muted)).toBe(true);
    const poster = await page.request.get(await v.getAttribute('poster') as string);
    expect(poster.status()).toBe(200);
    expect(poster.headers()['content-type']).toBe('image/jpeg');
    const res = await page.request.get('assets/demo.mp4', { headers: { Range: 'bytes=0-1023' } });
    expect(res.status()).toBe(206);
    expect(res.headers()['content-type']).toBe('video/mp4');
  });

  test('plays, and when it ends it starts again by itself', async ({ page }) => {
    await page.goto('/');
    const canPlay = await page.evaluate(() => document.createElement('video').canPlayType('video/mp4; codecs="avc1.42E01E"') !== '');
    test.skip(!canPlay, 'this browser build cannot decode H.264 (Playwright\'s bundled Chromium); covered by the chrome and safari projects in CI');
    const v = page.locator('#demo-video');
    await v.scrollIntoViewIfNeeded();
    await expect.poll(() => v.evaluate((el: HTMLVideoElement) => !el.paused && el.currentTime > 0.3), { timeout: 15_000 }).toBe(true);
    const duration = await v.evaluate((el: HTMLVideoElement) => el.duration);
    expect(duration).toBeGreaterThan(10);
    await v.evaluate((el: HTMLVideoElement) => { el.currentTime = el.duration - 0.6; });
    // It must wrap around to the start and keep playing, not freeze on the last frame.
    await expect.poll(() => v.evaluate((el: HTMLVideoElement) => el.currentTime < 5 && !el.paused && !el.ended), { timeout: 15_000 }).toBe(true);
  });

  test.describe('with reduced motion', () => {
    test.use({ reducedMotion: 'reduce' });
    test('does not autoplay and offers a play button', async ({ page }) => {
      await page.goto('/');
      const v = page.locator('#demo-video');
      await page.waitForTimeout(500);
      expect(await v.evaluate((el: HTMLVideoElement) => el.paused)).toBe(true);
      await expect(page.locator('#play-overlay')).toBeVisible();
    });
  });
});
