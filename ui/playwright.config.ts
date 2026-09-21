import { defineConfig, devices } from '@playwright/test';

const CI = !!process.env.CI;
const PORT = 4173;

export default defineConfig({
  testDir: 'tests',
  fullyParallel: true,
  forbidOnly: CI,
  retries: CI ? 1 : 0,
  reporter: CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: { baseURL: `http://localhost:${PORT}`, trace: 'retain-on-failure' },
  webServer: {
    command: 'node scripts/serve.mjs',
    url: `http://localhost:${PORT}/`,
    reuseExistingServer: !CI,
    timeout: 20_000,
    env: { PORT: String(PORT) },
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    { name: 'mobile-chromium', use: { ...devices['Pixel 7'] } },
    // CI only: real Chrome can decode the H.264 demo video (Playwright's bundled Chromium cannot),
    // and WebKit covers Safari on iPhone.
    ...(CI
      ? [
          { name: 'chrome', use: { ...devices['Desktop Chrome'], channel: 'chrome' } },
          { name: 'mobile-safari', use: { ...devices['iPhone 14'] } },
        ]
      : []),
  ],
});
