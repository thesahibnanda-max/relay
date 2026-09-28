import { test, expect } from '@playwright/test';

const WIDTHS = [320, 375, 768, 1024, 1440, 1920];

for (const width of WIDTHS) {
  test.describe(`at ${width}px wide`, () => {
    test.use({ viewport: { width, height: 900 } });

    test('nothing overflows sideways', async ({ page }) => {
      await page.goto('/');
      await page.waitForLoadState('networkidle');
      const m = await page.evaluate(() => ({
        doc: document.documentElement.scrollWidth,
        body: document.body.scrollWidth,
        inner: window.innerWidth,
      }));
      expect(m.doc).toBeLessThanOrEqual(m.inner);
      expect(m.body).toBeLessThanOrEqual(m.inner);
      // Every important block sits inside the viewport.
      for (const sel of ['.hero-copy', '.cmd', '.constellation', '.term', '.stepper', '.carousel', '.slide[aria-hidden="false"] .sea', '.demo-switch', '.shot-frame', '.legend', '.tabs', '.site-footer .foot']) {
        for (const box of await page.locator(sel).evaluateAll((els) => els.map((e) => { const r = e.getBoundingClientRect(); return { l: r.left, r: r.right }; }))) {
          expect(box.l, sel).toBeGreaterThanOrEqual(-0.5);
          expect(box.r, sel).toBeLessThanOrEqual(width + 0.5);
        }
      }
    });

    for (const scheme of ['dark', 'light'] as const) {
      test(`${scheme} demo screenshot fills the width, keeps its 2:1 shape and stays on screen`, async ({ page }) => {
        await page.emulateMedia({ colorScheme: scheme });
        await page.goto('/');
        await page.locator('#demo-tab-local').click();
        const frame = page.locator('#demo-local .shot-frame:visible');
        await frame.scrollIntoViewIfNeeded();
        const box = (await frame.boundingBox())!;
        expect(box.width / box.height).toBeCloseTo(2, 1);
        expect(box.x).toBeGreaterThanOrEqual(0);
        expect(box.x + box.width).toBeLessThanOrEqual(width);
        // Markers scale with the image and never leave it.
        const bad = await frame.locator('.marker').evaluateAll((els) => {
          const f = els[0].closest('.shot-frame')!.getBoundingClientRect();
          return els.filter((e) => { const r = e.getBoundingClientRect(); return r.left < f.left || r.right > f.right || r.top < f.top || r.bottom > f.bottom; }).length;
        });
        expect(bad).toBe(0);
      });

      test(`${scheme} cross-machine image fills the width, keeps its 2:1 shape and stays on screen`, async ({ page }) => {
        await page.emulateMedia({ colorScheme: scheme });
        await page.goto('/');
        const frame = page.locator('#together .shot-frame:visible');
        await frame.scrollIntoViewIfNeeded();
        const box = (await frame.boundingBox())!;
        expect(box.width / box.height).toBeCloseTo(2, 1);
        expect(box.x).toBeGreaterThanOrEqual(0);
        expect(box.x + box.width).toBeLessThanOrEqual(width);
        // The relay bridge drawn over the image scales with it and stays inside it.
        const bridge = (await frame.locator('.bridge').boundingBox())!;
        expect(bridge.x).toBeGreaterThanOrEqual(box.x);
        expect(bridge.x + bridge.width).toBeLessThanOrEqual(box.x + box.width);
        expect(bridge.y).toBeGreaterThanOrEqual(box.y);
        expect(bridge.y + bridge.height).toBeLessThanOrEqual(box.y + box.height);
      });
    }

    test('the solution slide fits the screen too', async ({ page }) => {
      await page.goto('/');
      await page.locator('.btn-add').click();
      // let the slide transition finish
      await expect.poll(async () => Math.round((await page.locator('#solution .slide-copy').boundingBox())!.x), { timeout: 5000 }).toBeLessThan(width / 4);
      await page.waitForTimeout(300);
      const m = await page.evaluate(() => ({ doc: document.documentElement.scrollWidth, inner: window.innerWidth }));
      expect(m.doc).toBeLessThanOrEqual(m.inner);
      for (const sel of ['#solution .slide-copy', '#solution .sea', '#solution .mini-cards', '#solution .perks']) {
        const r = (await page.locator(sel).boundingBox())!;
        expect(r.x, sel).toBeGreaterThanOrEqual(-0.5);
        expect(r.x + r.width, sel).toBeLessThanOrEqual(width + 0.5);
      }
    });

    test('an open perk description stays on screen', async ({ page }) => {
      await page.goto('/');
      await page.locator('.btn-add').click();
      await expect.poll(async () => Math.round((await page.locator('#solution .slide-copy').boundingBox())!.x), { timeout: 5000 }).toBeLessThan(width / 4);
      for (const k of ['mess', 'private']) {
        await page.locator(`#perk-btn-${k}`).click();
        await page.waitForTimeout(400);
        const r = (await page.locator(`#perk-${k}`).boundingBox())!;
        expect(r.x).toBeGreaterThanOrEqual(-0.5);
        expect(r.x + r.width).toBeLessThanOrEqual(width + 0.5);
      }
      const m = await page.evaluate(() => ({ doc: document.documentElement.scrollWidth, inner: window.innerWidth }));
      expect(m.doc).toBeLessThanOrEqual(m.inner);
    });

    test('header collapses into a menu below 768px only', async ({ page }) => {
      await page.goto('/');
      if (width < 768) {
        await expect(page.locator('#menu-btn')).toBeVisible();
        await expect(page.locator('#site-nav')).toBeHidden();
      } else {
        await expect(page.locator('#menu-btn')).toBeHidden();
        await expect(page.locator('#site-nav')).toBeVisible();
      }
    });

    test('buttons and standalone links are big enough to tap (44px)', async ({ page }) => {
      await page.goto('/');
      if (width < 768) await page.locator('#menu-btn').click();
      const small = await page.locator('button:visible, .btn:visible, .brand:visible, .foot-links a:visible, .nav a:visible').evaluateAll((els) =>
        els
          .map((e) => { const r = e.getBoundingClientRect(); return { what: (e.textContent || e.getAttribute('aria-label') || e.className).trim().slice(0, 30), w: Math.round(r.width), h: Math.round(r.height) }; })
          .filter((b) => b.h < 44 || b.w < 44),
      );
      expect(small).toEqual([]);
    });
  });
}

test.describe('landscape phone (844x390)', () => {
  test.use({ viewport: { width: 844, height: 390 } });
  test('nothing overflows sideways and the header stays usable', async ({ page }) => {
    await page.goto('/');
    await page.waitForLoadState('networkidle');
    const m = await page.evaluate(() => ({ doc: document.documentElement.scrollWidth, inner: window.innerWidth }));
    expect(m.doc).toBeLessThanOrEqual(m.inner);
    const header = (await page.locator('.site-header').boundingBox())!;
    expect(header.height).toBeLessThan(120); // never eats the short screen
  });
});

// Each slide of the problem -> solution carousel fits in one screen, on phones
// and desktops alike: the tabs, the button and every card are visible at once.
for (const [w, h] of [[375, 812], [390, 844], [412, 915], [768, 1024], [1440, 900], [1920, 1080]]) {
  test(`both carousel slides fit one ${w}x${h} screen`, async ({ page }) => {
    await page.setViewportSize({ width: w, height: h });
    await page.goto('/');
    await page.evaluate(() => document.fonts.ready);
    // jump straight to the section (no smooth scroll), exactly where the nav link lands
    await page.evaluate(() => { document.documentElement.style.scrollBehavior = 'auto'; document.querySelector('#how')!.scrollIntoView(); });
    const fits = async (last: string) => page.evaluate((sel) => {
      const top = document.querySelector('.carousel-controls')!.getBoundingClientRect().top;
      const bottom = document.querySelector(sel)!.getBoundingClientRect().bottom;
      return { top, bottom, vh: innerHeight };
    }, last);
    const p = await fits('#problem .mini-cards');
    expect(p.top).toBeGreaterThanOrEqual(0);
    expect(p.bottom).toBeLessThanOrEqual(p.vh);
    await page.locator('.btn-add').click();
    await page.waitForTimeout(800);
    await page.evaluate(() => document.querySelector('#how')!.scrollIntoView());
    const s = await fits('#solution .perks');
    expect(s.top).toBeGreaterThanOrEqual(0);
    expect(s.bottom).toBeLessThanOrEqual(s.vh);
  });
}
