// Playwright config for the calweb UI. global-setup builds and starts calendsd
// + calclient against the shared dev Postgres (start it first with
// `scripts/dev-db.sh start`); global-teardown stops them. calclient runs with
// -dev-assets pointing at the working tree, so tests exercise the live source.
//
// Browsers come from Nix (playwright-driver): run inside `nix develop`, which
// sets PLAYWRIGHT_BROWSERS_PATH, so no browser download happens. Keep
// @playwright/test pinned to pkgs.playwright-driver.version. Add the
// firefox/webkit projects below to check dialog behaviour across engines.
const { defineConfig, devices } = require('@playwright/test');

module.exports = defineConfig({
  testDir: './tests',
  timeout: 30_000,
  expect: { timeout: 7_000 },
  fullyParallel: false,   // one shared server + one dev DB
  workers: 1,
  reporter: [['list']],
  globalSetup: require.resolve('./global-setup.js'),
  globalTeardown: require.resolve('./global-teardown.js'),
  use: {
    baseURL: 'http://127.0.0.1:8091',
    headless: true,
    trace: 'on-first-retry',
  },
  projects: [
    // Nix-provided chromium (no `channel`, so Playwright uses the browser under
    // PLAYWRIGHT_BROWSERS_PATH rather than a system install).
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    // webkit + firefox are in the same Nix bundle; enable to test cross-engine:
    // { name: 'webkit', use: { ...devices['Desktop Safari'] } },
    // { name: 'firefox', use: { ...devices['Desktop Firefox'] } },
  ],
});
