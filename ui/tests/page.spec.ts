import { test, expect } from '@playwright/test';

const REPO = 'https://github.com/thesahibnanda-max/relay';

test('loads cleanly: no console errors, page errors or failed requests', async ({ page }) => {
  const problems: string[] = [];
  page.on('console', (m) => { if (m.type() === 'error') problems.push(`console: ${m.text()}`); });
  page.on('pageerror', (e) => problems.push(`pageerror: ${e.message}`));
  page.on('requestfailed', (r) => problems.push(`failed: ${r.url()}`));
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

test.describe('demo screenshot', () => {
  for (const kind of ['dark', 'light'] as const) {
    test(`${kind} version is a small WebP with a PNG fallback and a full-size link`, async ({ page }) => {
      await page.goto('/');
      const frame = page.locator(`#demo .shot-${kind}`);
      const img = frame.locator('img');
      const alt = await img.getAttribute('alt');
      expect(alt!.length).toBeGreaterThan(40);
      await expect(img).toHaveAttribute('width', '1774');
      await expect(img).toHaveAttribute('height', '887');
      await expect(img).toHaveAttribute('loading', 'lazy');
      const srcset = await frame.locator('picture source').getAttribute('srcset');
      for (const url of srcset!.split(',').map((x) => x.trim().split(' ')[0])) {
        const res = await page.request.get(url);
        expect(res.status(), url).toBe(200);
        expect(res.headers()['content-type'], url).toBe('image/webp');
        expect((await res.body()).length, `${url} should stay light`).toBeLessThan(150_000);
      }
      const full = await page.request.get((await frame.getAttribute('href'))!);
      expect(full.status()).toBe(200);
      expect(full.headers()['content-type']).toBe('image/png');
      await expect(frame).toHaveAttribute('rel', /noopener/);
    });
  }

  test('the dark image shows in dark mode and the light one in light mode', async ({ page }) => {
    await page.emulateMedia({ colorScheme: 'dark' });
    await page.goto('/');
    await expect(page.locator('#demo .shot-dark')).toBeVisible();
    await expect(page.locator('#demo .shot-light')).toBeHidden();
    await page.emulateMedia({ colorScheme: 'light' });
    await expect(page.locator('#demo .shot-light')).toBeVisible();
    await expect(page.locator('#demo .shot-dark')).toBeHidden();
  });

  test('the theme toggle switches the image too, and only the visible one is downloaded', async ({ page }) => {
    await page.emulateMedia({ colorScheme: 'dark' });
    const loaded: string[] = [];
    page.on('response', (r) => { if (/\/(light)?demo[^/]*\.(webp|png)$/.test(r.url())) loaded.push(r.url().split('/').pop()!); });
    await page.goto('/');
    const section = page.locator('#demo');
    await section.scrollIntoViewIfNeeded();
    await expect.poll(() => page.locator('#demo .shot-dark img').evaluate((el: HTMLImageElement) => el.complete && el.naturalWidth > 0)).toBe(true);
    expect(loaded.filter((f) => f.startsWith('lightdemo'))).toEqual([]);
    await page.locator('#theme-btn').click(); // dark -> light
    await expect(page.locator('#demo .shot-light')).toBeVisible();
    await expect(page.locator('#demo .shot-dark')).toBeHidden();
    const light = page.locator('#demo .shot-light img');
    await light.scrollIntoViewIfNeeded();
    await expect.poll(() => light.evaluate((el: HTMLImageElement) => el.complete && el.naturalWidth > 0)).toBe(true);
    expect(await light.evaluate((el: HTMLImageElement) => el.currentSrc)).toMatch(/lightdemo[^/]*\.webp$/);
    await page.locator('#theme-btn').click(); // back to dark
    await expect(page.locator('#demo .shot-dark')).toBeVisible();
  });

  test('actually renders, sharp, without stretching', async ({ page }) => {
    await page.goto('/');
    const img = page.locator('#demo .shot-frame:visible img');
    await img.scrollIntoViewIfNeeded();
    await expect.poll(() => img.evaluate((el: HTMLImageElement) => el.complete && el.naturalWidth > 0)).toBe(true);
    const m = await img.evaluate((el: HTMLImageElement) => ({ nw: el.naturalWidth, nh: el.naturalHeight, w: el.clientWidth, h: el.clientHeight, src: el.currentSrc }));
    expect(m.src).toMatch(/\.webp$/);
    expect(m.nw / m.nh).toBeCloseTo(2, 1);
    expect(m.w / m.h).toBeCloseTo(2, 1);
  });

  for (const scheme of ['dark', 'light'] as const) {
    test(`has three numbered markers on the ${scheme} image and a matching legend`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await page.goto('/');
      const markers = page.locator('#demo .shot-frame:visible .marker');
      await expect(markers).toHaveCount(3);
      await expect(page.locator('#demo .legend li')).toHaveCount(3);
      const inside = await markers.evaluateAll((els) => {
        const frame = els[0].closest('.shot-frame')!.getBoundingClientRect();
        return els.map((e) => { const r = e.getBoundingClientRect(); return r.left >= frame.left && r.right <= frame.right && r.top >= frame.top && r.bottom <= frame.bottom; });
      });
      expect(inside).toEqual([true, true, true]);
    });
  }

  test('there is no video left on the page', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('video')).toHaveCount(0);
  });
});
