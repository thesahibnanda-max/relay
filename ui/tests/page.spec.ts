import { test, expect, type Page } from '@playwright/test';

const REPO = 'https://github.com/thesahibnanda-max/relay';

// The site now auto-detects the visitor's OS to pick the starting command/tab
// (see the "OS auto-detection" describe block below). Every other test in this
// file cares about a stable, known default, not about detection itself, so it
// pins a macOS user agent - otherwise it would inherit whatever host platform
// happens to be baked into the project's default device (Desktop Chrome's is
// Windows), which is incidental to what these tests are actually checking.
test.use({ userAgent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36' });

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
  for (const id of ['how', 'demo', 'install']) {
    await expect(page.locator(`section#${id}`)).toHaveCount(1);
  }
  // the problem and the solution are the two slides of one carousel
  await expect(page.locator('section#how #problem.slide')).toHaveCount(1);
  await expect(page.locator('section#how #solution.slide')).toHaveCount(1);
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

// The theme control is a System / Light / Dark radio group. On small screens it
// lives in the menu, so open that first when the menu button is showing.
async function setTheme(page: Page, mode: 'system' | 'light' | 'dark') {
  const menu = page.locator('#menu-btn');
  if (await menu.isVisible()) {
    // iOS drops a tap that lands while the page is still moving (e.g. mid smooth-scroll),
    // so make sure the menu really opened, and tap again if it did not.
    await expect(async () => {
      if ((await menu.getAttribute('aria-expanded')) !== 'true') await menu.click();
      await expect(page.locator('#site-nav')).toBeVisible({ timeout: 1000 });
    }).toPass({ timeout: 10000 });
    await page.locator(`[data-theme-set="${mode}"]`).click();
    await page.keyboard.press('Escape');
  } else {
    await page.locator(`[data-theme-set="${mode}"]`).click();
  }
}

const DARK_BG = 'rgb(10, 7, 20)';
const LIGHT_BG = 'rgb(247, 244, 255)';

test('theme switch: light and dark are remembered, system follows the OS again', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark' });
  await page.goto('/');
  const html = page.locator('html'), body = page.locator('body');
  await expect(html).not.toHaveAttribute('data-theme', /.+/);
  await expect(page.locator('[data-theme-set="system"]')).toHaveAttribute('aria-checked', 'true');
  await setTheme(page, 'light');
  await expect(html).toHaveAttribute('data-theme', 'light');
  await expect(body).toHaveCSS('background-color', LIGHT_BG);
  await page.reload();
  await expect(html).toHaveAttribute('data-theme', 'light');
  await expect(page.locator('[data-theme-set="light"]')).toHaveAttribute('aria-checked', 'true');
  await setTheme(page, 'dark');
  await expect(html).toHaveAttribute('data-theme', 'dark');
  await expect(body).toHaveCSS('background-color', DARK_BG);
  await setTheme(page, 'system');
  await expect(html).not.toHaveAttribute('data-theme', /.+/);
  await page.emulateMedia({ colorScheme: 'light' });
  await expect(body).toHaveCSS('background-color', LIGHT_BG);
  await page.reload();
  await expect(html).not.toHaveAttribute('data-theme', /.+/);
});

test('follows the system light theme by default', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'light' });
  await page.goto('/');
  await expect(page.locator('body')).toHaveCSS('background-color', LIGHT_BG);
});

test('follows the system dark theme by default', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark' });
  await page.goto('/');
  await expect(page.locator('body')).toHaveCSS('background-color', DARK_BG);
});

test('the theme radio group works with the arrow keys', async ({ page }) => {
  test.skip((page.viewportSize()?.width ?? 0) < 768, 'the switch sits in the menu on small screens');
  await page.emulateMedia({ colorScheme: 'dark' });
  await page.goto('/');
  await page.locator('[data-theme-set="system"]').focus();
  await page.keyboard.press('ArrowRight');
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
  await expect(page.locator('[data-theme-set="light"]')).toBeFocused();
  await page.keyboard.press('ArrowRight');
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
});

test.describe('the four supported CLIs', () => {
  const agents = [
    { cls: 'a-claude', name: 'Claude Code', symbol: '#i-claude' },
    { cls: 'a-codex', name: 'Codex', symbol: '#i-codex' },
    { cls: 'a-copilot', name: 'GitHub Copilot CLI', symbol: '#i-copilot' },
    { cls: 'a-agy', name: 'Antigravity CLI', symbol: '#i-agy' },
  ];
  for (const scheme of ['dark', 'light'] as const) {
    test(`each has a card with its real icon, rendered, in the ${scheme} theme`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await page.goto('/');
      for (const a of agents) {
        const card = page.locator(`#agents .${a.cls}`);
        await expect(card.locator('h3')).toContainText(a.name);
        const icon = card.locator('.agent-icon svg');
        await expect(icon.locator('use')).toHaveAttribute('href', a.symbol);
        const box = (await icon.boundingBox())!;
        expect(box.width).toBeGreaterThanOrEqual(28);
        expect(box.height).toBeGreaterThanOrEqual(28);
      }
      // every symbol the page references exists in the sprite
      for (const a of agents) await expect(page.locator(`symbol${a.symbol}`)).toHaveCount(1);
      await expect(page.locator('#agents .tag-experimental')).toHaveCount(0); // no agent is experimental
    });
  }

  test('all four sit in the hero constellation with visible names', async ({ page }) => {
    await page.goto('/');
    for (const [n, label] of [['n-claude', 'Claude Code'], ['n-codex', 'Codex'], ['n-copilot', 'Copilot'], ['n-agy', 'Antigravity']]) {
      await expect(page.locator(`.constellation .${n} .node-label`)).toContainText(label);
      await expect(page.locator(`.constellation .${n} svg`)).toBeVisible();
    }
  });
});

test.describe('problem -> solution carousel', () => {
  const problem = (page: Page) => page.locator('#problem');
  const solution = (page: Page) => page.locator('#solution');

  test('opens on the problem, with the solution slide out of reach', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('#problem .slide-title')).toContainText('island');
    await expect(page.locator('.btn-add')).toHaveText(/Add Relay/);
    await expect(problem(page)).toHaveAttribute('aria-hidden', 'false');
    await expect(solution(page)).toHaveAttribute('aria-hidden', 'true');
    await expect(solution(page)).toHaveAttribute('inert', '');
    await expect(page.locator('#step-problem')).toHaveAttribute('aria-selected', 'true');
    await expect(page.locator('#solution .sea')).not.toHaveClass(/\blit\b/);
  });

  test('"Add Relay" slides to the solution, brings in the boat and lights every agent', async ({ page, browserName }) => {
    await page.goto('/');
    await page.locator('.btn-add').click();
    await expect(solution(page)).toHaveAttribute('aria-hidden', 'false');
    await expect(problem(page)).toHaveAttribute('inert', '');
    await expect(page.locator('#step-solution')).toHaveAttribute('aria-selected', 'true');
    await expect(page.locator('#solution .slide-title')).toContainText('boat');
    await expect(page.locator('.btn-add')).toHaveText(/Remove Relay/);
    // The button stays put under the tabs, so focus stays on it (Safari never focuses a
    // button on a mouse click, so there is nothing to check there).
    if (browserName !== 'webkit') await expect(page.locator('.btn-add')).toBeFocused();
    await expect(page.locator('#solution .sea')).toHaveClass(/\blit\b/);
    // once the boat has passed, all four islands are in full colour
    for (const n of [1, 2, 3, 4]) {
      await expect.poll(() => page.locator(`#solution .isl-${n} .island-tile`).evaluate((e) => getComputedStyle(e).filter), { timeout: 6000 }).toBe('none');
    }
    const sea = (await page.locator('#solution .sea').boundingBox())!;
    const boat = (await page.locator('#solution .boat').boundingBox())!;
    expect(boat.x).toBeGreaterThanOrEqual(sea.x - 1);
    expect(boat.x + boat.width).toBeLessThanOrEqual(sea.x + sea.width + 1);
    // the solution slide is actually on screen, the problem slide is not
    const vw = page.viewportSize()!.width;
    const sBox = (await page.locator('#solution .slide-copy').boundingBox())!;
    expect(sBox.x).toBeGreaterThanOrEqual(0);
    expect(sBox.x).toBeLessThan(vw);
  });

  test('"Remove Relay" goes back to the problem, and the islands go dark again', async ({ page }) => {
    await page.goto('/');
    await page.locator('.btn-add').click();
    await page.locator('.btn-add').click();
    await expect(problem(page)).toHaveAttribute('aria-hidden', 'false');
    await expect(page.locator('.btn-add')).toHaveText(/Add Relay/);
    await expect(page.locator('#solution .sea')).not.toHaveClass(/\blit\b/);
  });

  test('the stepper works with the mouse and the arrow keys', async ({ page }) => {
    await page.goto('/');
    await page.locator('#step-solution').click();
    await expect(solution(page)).toHaveAttribute('aria-hidden', 'false');
    await page.locator('#step-solution').press('ArrowLeft');
    await expect(page.locator('#step-problem')).toBeFocused();
    await expect(problem(page)).toHaveAttribute('aria-hidden', 'false');
    await page.locator('#step-problem').press('End');
    await expect(solution(page)).toHaveAttribute('aria-hidden', 'false');
  });

  test('the Add Relay button sits right under the problem / solution tabs', async ({ page }) => {
    await page.goto('/');
    const tabs = (await page.locator('.stepper').boundingBox())!;
    const btn = (await page.locator('.btn-add').boundingBox())!;
    expect(btn.y).toBeGreaterThan(tabs.y + tabs.height - 1);
    expect(btn.y - (tabs.y + tabs.height)).toBeLessThan(24);
    const first = (await page.locator('#problem .slide-copy').boundingBox())!;
    expect(first.y).toBeGreaterThan(btn.y + btn.height - 1);
  });

  test('a reload after switching always opens on the problem again', async ({ page }) => {
    await page.goto('/');
    await page.locator('.btn-add').click();
    await expect(solution(page)).toHaveAttribute('aria-hidden', 'false');
    await page.reload();
    await expect(problem(page)).toHaveAttribute('aria-hidden', 'false');
    await expect(solution(page)).toHaveAttribute('aria-hidden', 'true');
  });

  test('even a link straight to #solution tells the story from the problem', async ({ page }) => {
    await page.goto('/#solution');
    await expect(problem(page)).toHaveAttribute('aria-hidden', 'false');
    await expect(solution(page)).toHaveAttribute('aria-hidden', 'true');
  });

  test('a horizontal swipe switches slides', async ({ page }) => {
    await page.goto('/');
    const sea = page.locator('#problem .sea');
    await sea.scrollIntoViewIfNeeded();
    const b = (await sea.boundingBox())!;
    const y = b.y + b.height / 2;
    await page.mouse.move(b.x + b.width * .8, y);
    await page.mouse.down();
    await page.mouse.move(b.x + b.width * .2, y + 5, { steps: 6 });
    await page.mouse.up();
    await expect(solution(page)).toHaveAttribute('aria-hidden', 'false');
  });

  test('with reduced motion the solution is lit straight away, with the boat in the middle', async ({ page }) => {
    await page.emulateMedia({ reducedMotion: 'reduce' });
    await page.goto('/');
    await page.locator('.btn-add').click();
    for (const n of [1, 2, 3, 4]) {
      expect(await page.locator(`#solution .isl-${n} .island-tile`).evaluate((e) => getComputedStyle(e).filter)).toBe('none');
    }
    expect(await page.evaluate(() => document.getAnimations().length)).toBe(0);
    const sea = (await page.locator('#solution .sea').boundingBox())!;
    const boat = (await page.locator('#solution .boat').boundingBox())!;
    expect(Math.abs(boat.x + boat.width / 2 - (sea.x + sea.width / 2))).toBeLessThan(sea.width * .1);
  });
});

test.describe('the four perks on the solution slide', () => {
  const keys = ['mess', 'polite', 'captain', 'private'];
  async function toSolution(page: Page) {
    await page.goto('/');
    await page.locator('.btn-add').click();
    await expect(page.locator('#solution')).toHaveAttribute('aria-hidden', 'false');
  }

  test('each opens its own description, one at a time, pointing at its chip', async ({ page }) => {
    await toSolution(page);
    for (const k of keys) {
      const btn = page.locator(`#perk-btn-${k}`), detail = page.locator(`#perk-${k}`);
      await btn.click();
      await expect(btn).toHaveAttribute('aria-expanded', 'true');
      await expect(detail).toBeVisible();
      await expect(detail.locator('.perk-detail-text')).not.toBeEmpty();
      for (const other of keys.filter((o) => o !== k)) {
        await expect(page.locator(`#perk-btn-${other}`)).toHaveAttribute('aria-expanded', 'false');
        await expect(page.locator(`#perk-${other}`)).toBeHidden();
      }
      // the pointer sits over the chip that opened it
      const b = (await btn.boundingBox())!, wrap = (await page.locator('.perks-wrap').boundingBox())!;
      const ax = parseFloat(await detail.evaluate((e) => getComputedStyle(e).getPropertyValue('--ax')));
      expect(Math.abs(wrap.x + ax - (b.x + b.width / 2))).toBeLessThan(b.width / 2);
    }
  });

  test('tapping the same chip again, Esc, a click elsewhere or changing slide all close it', async ({ page }) => {
    await toSolution(page);
    const btn = page.locator('#perk-btn-captain'), detail = page.locator('#perk-captain');
    await btn.click();
    await btn.click();
    await expect(detail).toBeHidden();
    await btn.click();
    await page.keyboard.press('Escape');
    await expect(detail).toBeHidden();
    await expect(btn).toBeFocused();
    await btn.click();
    await page.locator('#solution .slide-title').click();
    await expect(detail).toBeHidden();
    await btn.click();
    await page.locator('.btn-add').click(); // Remove Relay
    await expect(page.locator('#problem')).toHaveAttribute('aria-hidden', 'false');
    await expect(btn).toHaveAttribute('aria-expanded', 'false');
  });

  test('an open description floats: the slide does not grow', async ({ page }) => {
    await toSolution(page);
    await page.waitForTimeout(800);
    const before = (await page.locator('.carousel-viewport').boundingBox())!.height;
    await page.locator('#perk-btn-mess').click();
    await page.waitForTimeout(400);
    const after = (await page.locator('.carousel-viewport').boundingBox())!.height;
    expect(Math.abs(after - before)).toBeLessThan(1);
  });
});

test('with reduced motion nothing animates and everything is visible', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await page.goto('/');
  await page.waitForLoadState('networkidle');
  expect(await page.evaluate(() => document.getAnimations().length)).toBe(0);
  for (const sel of ['#problem .card', '#agents .card', '.term .ln']) {
    const hidden = await page.locator(sel).evaluateAll((els) => els.filter((e) => {
      const cs = getComputedStyle(e);
      return cs.opacity !== '1' || cs.visibility !== 'visible';
    }).length);
    expect(hidden, sel).toBe(0);
  }
});

test('the hero names the detected system and links to the others', async ({ page }) => {
  await page.goto('/'); // this file pins a macOS user agent
  await expect(page.locator('[data-os-label]')).toHaveText('Detected: macOS');
  await expect(page.locator('.os-pill a[href="#install"]')).toBeVisible();
});

test('install tabs switch panels with mouse and keyboard', async ({ page }) => {
  await page.goto('/');
  const mac = page.locator('#tab-mac'), linux = page.locator('#tab-linux'), wsl = page.locator('#tab-wsl');
  const windows = page.locator('#tab-windows');
  await expect(page.locator('#panel-mac')).toBeVisible();
  await expect(page.locator('#panel-linux')).toBeHidden();
  await linux.click();
  await expect(page.locator('#panel-linux')).toBeVisible();
  await expect(page.locator('#panel-mac')).toBeHidden();
  await expect(linux).toHaveAttribute('aria-selected', 'true');
  await linux.press('ArrowRight');
  await expect(wsl).toBeFocused();
  await expect(page.locator('#panel-wsl')).toBeVisible();
  await wsl.press('ArrowRight');
  await expect(windows).toBeFocused();
  await expect(page.locator('#panel-windows')).toBeVisible();
  await windows.press('Home');
  await expect(mac).toBeFocused();
  await expect(page.locator('#panel-mac')).toBeVisible();
  await mac.press('End');
  await expect(windows).toBeFocused();
  await expect(page.locator('#panel-windows')).toBeVisible();
});

test('the Windows tab swaps in the PowerShell command and its own upgrade/remove notes', async ({ page, baseURL }) => {
  await page.goto('/');
  const cmd = page.locator('#install-cmd-2');
  const unixCards = page.locator('[data-cards="unix"]');
  const windowsCards = page.locator('[data-cards="windows"]');
  await expect(cmd).toHaveText(`curl -fsSL ${baseURL}/install.sh | bash`);
  await expect(unixCards).toBeVisible();
  await expect(windowsCards).toBeHidden();

  await page.locator('#tab-windows').click();
  await expect(cmd).toHaveText(`irm ${baseURL}/install.ps1 | iex`);
  await expect(unixCards).toBeHidden();
  await expect(windowsCards).toBeVisible();
  await expect(windowsCards).toContainText('%LOCALAPPDATA%');

  await page.locator('#tab-mac').click();
  await expect(cmd).toHaveText(`curl -fsSL ${baseURL}/install.sh | bash`);
  await expect(unixCards).toBeVisible();
  await expect(windowsCards).toBeHidden();
});

test.describe('OS auto-detection', () => {
  const cases: { name: string; userAgent: string; tab: string; cmd: (base: string) => string }[] = [
    {
      name: 'Windows',
      userAgent: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36',
      tab: 'tab-windows',
      cmd: (base) => `irm ${base}/install.ps1 | iex`,
    },
    {
      name: 'macOS',
      userAgent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36',
      tab: 'tab-mac',
      cmd: (base) => `curl -fsSL ${base}/install.sh | bash`,
    },
    {
      name: 'Linux',
      userAgent: 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36',
      tab: 'tab-linux',
      cmd: (base) => `curl -fsSL ${base}/install.sh | bash`,
    },
  ];
  for (const c of cases) {
    test(`${c.name} visitors see the ${c.name} tab and command by default`, async ({ browser, baseURL }) => {
      const page = await (await browser.newContext({ userAgent: c.userAgent })).newPage();
      await page.goto('/');
      await expect(page.locator(`#${c.tab}`)).toHaveAttribute('aria-selected', 'true');
      await expect(page.locator('#install-cmd')).toHaveText(c.cmd(baseURL!));
      await expect(page.locator('#install-cmd-2')).toHaveText(c.cmd(baseURL!));
    });
  }

  test('an unrecognized user agent falls back to the macOS tab', async ({ browser, baseURL }) => {
    const page = await (await browser.newContext({ userAgent: 'SomeOtherBrowser/1.0' })).newPage();
    await page.goto('/');
    await expect(page.locator('#tab-mac')).toHaveAttribute('aria-selected', 'true');
    await expect(page.locator('#install-cmd')).toHaveText(`curl -fsSL ${baseURL}/install.sh | bash`);
  });
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
    await page.locator('#demo-tab-local').click();
      const frame = page.locator(`#demo-local .shot-${kind}`);
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
    await page.locator('#demo-tab-local').click();
    await expect(page.locator('#demo-local .shot-dark')).toBeVisible();
    await expect(page.locator('#demo-local .shot-light')).toBeHidden();
    await page.emulateMedia({ colorScheme: 'light' });
    await expect(page.locator('#demo-local .shot-light')).toBeVisible();
    await expect(page.locator('#demo-local .shot-dark')).toBeHidden();
  });

  test('the theme toggle switches the image too, and only the visible one is downloaded', async ({ page }) => {
    await page.emulateMedia({ colorScheme: 'dark' });
    const loaded: string[] = [];
    page.on('response', (r) => { if (/\/(light)?demo[^/]*\.(webp|png)$/.test(r.url())) loaded.push(r.url().split('/').pop()!); });
    await page.goto('/');
    await page.locator('#demo-tab-local').click();
    const section = page.locator('#demo-local');
    await section.scrollIntoViewIfNeeded();
    await expect.poll(() => page.locator('#demo-local .shot-dark img').evaluate((el: HTMLImageElement) => el.complete && el.naturalWidth > 0)).toBe(true);
    expect(loaded.filter((f) => f.startsWith('lightdemo'))).toEqual([]);
    await setTheme(page, 'light');
    await expect(page.locator('#demo-local .shot-light')).toBeVisible();
    await expect(page.locator('#demo-local .shot-dark')).toBeHidden();
    const light = page.locator('#demo-local .shot-light img');
    await light.scrollIntoViewIfNeeded();
    await expect.poll(() => light.evaluate((el: HTMLImageElement) => el.complete && el.naturalWidth > 0)).toBe(true);
    expect(await light.evaluate((el: HTMLImageElement) => el.currentSrc)).toMatch(/lightdemo[^/]*\.webp$/);
    await setTheme(page, 'dark');
    await expect(page.locator('#demo-local .shot-dark')).toBeVisible();
  });

  test('actually renders, sharp, without stretching', async ({ page }) => {
    await page.goto('/');
    await page.locator('#demo-tab-local').click();
    const img = page.locator('#demo-local .shot-frame:visible img');
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
    await page.locator('#demo-tab-local').click();
      const markers = page.locator('#demo-local .shot-frame:visible .marker');
      await expect(markers).toHaveCount(3);
      await expect(page.locator('#demo-local .legend li')).toHaveCount(3);
      const inside = await markers.evaluateAll((els) => {
        const frame = els[0].closest('.shot-frame')!.getBoundingClientRect();
        return els.map((e) => { const r = e.getBoundingClientRect(); return r.left >= frame.left && r.right <= frame.right && r.top >= frame.top && r.bottom <= frame.bottom; });
      });
      expect(inside).toEqual([true, true, true]);
    });
  }

  test('there is no video left on the page', async ({ page }) => {
    await page.goto('/');
    await page.locator('#demo-tab-local').click();
    await expect(page.locator('video')).toHaveCount(0);
  });
});

test.describe('demo views', () => {
  test('opens on the cross-machine view, and switches with mouse and keyboard', async ({ page }) => {
    await page.goto('/');
    const laptops = page.locator('#demo-tab-laptops'), local = page.locator('#demo-tab-local');
    await expect(laptops).toHaveAttribute('aria-selected', 'true');
    await expect(page.locator('#together')).toBeVisible();
    await expect(page.locator('#demo-local')).toBeHidden();
    await local.click();
    await expect(page.locator('#demo-local')).toBeVisible();
    await expect(page.locator('#together')).toBeHidden();
    await local.press('ArrowLeft');
    await expect(laptops).toBeFocused();
    await expect(page.locator('#together')).toBeVisible();
    await laptops.press('End');
    await expect(local).toBeFocused();
    await expect(page.locator('#demo-local')).toBeVisible();
  });

  test('switching demo views never changes the install command', async ({ page, baseURL }) => {
    await page.goto('/');
    await page.locator('#tab-windows').click();
    await page.locator('#demo-tab-local').click();
    await page.locator('#demo-tab-laptops').click();
    await expect(page.locator('#install-cmd-2')).toHaveText(`irm ${baseURL}/install.ps1 | iex`);
    await expect(page.locator('#tab-windows')).toHaveAttribute('aria-selected', 'true');
  });

  test('the cross-machine view explains its three steps', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('#together .legend li')).toHaveCount(3);
    await expect(page.locator('#together .shot-frame:visible .bridge')).toBeVisible();
  });
});

test.describe('cross-machine image', () => {
  for (const kind of ['dark', 'light'] as const) {
    test(`${kind} version is a small WebP with a PNG fallback and a full-size link`, async ({ page }) => {
      await page.goto('/');
      const frame = page.locator(`#together .shot-${kind}`);
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
    await expect(page.locator('#together .shot-dark')).toBeVisible();
    await expect(page.locator('#together .shot-light')).toBeHidden();
    await page.emulateMedia({ colorScheme: 'light' });
    await expect(page.locator('#together .shot-light')).toBeVisible();
    await expect(page.locator('#together .shot-dark')).toBeHidden();
  });

  test('actually renders, sharp, without stretching', async ({ page }) => {
    await page.goto('/');
    const img = page.locator('#together .shot-frame:visible img');
    await img.scrollIntoViewIfNeeded();
    await expect.poll(() => img.evaluate((el: HTMLImageElement) => el.complete && el.naturalWidth > 0)).toBe(true);
    const m = await img.evaluate((el: HTMLImageElement) => ({ nw: el.naturalWidth, nh: el.naturalHeight, w: el.clientWidth, h: el.clientHeight, src: el.currentSrc }));
    expect(m.src).toMatch(/\.webp$/);
    expect(m.nw / m.nh).toBeCloseTo(2, 1);
    expect(m.w / m.h).toBeCloseTo(2, 1);
  });
});
