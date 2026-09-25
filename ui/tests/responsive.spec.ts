import { test, expect } from '@playwright/test';

const WIDTHS = [320, 375, 768, 1024, 1440];

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
      for (const sel of ['.hero-copy', '.cmd', '.term', '.shot-frame', '.legend', '.tabs', '.site-footer .foot']) {
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
        const frame = page.locator('#demo .shot-frame:visible');
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
      });
    }

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
