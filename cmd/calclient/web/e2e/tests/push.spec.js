// End-to-end tests for the "Instant updates" (WebDAV-Push) preference in
// Settings. Real push delivery needs a push service and can't run headless, so
// these cover what the browser can verify deterministically: the toggle is
// enabled on a secure origin, and flipping it persists the preference (which
// drives pushSync.setPushEnabled → push.supported / syncSubscriptions). The
// pure push.js logic is unit-tested in tests/push.test.js.
const { test, expect } = require('@playwright/test');
const { login } = require('./helpers');

const PREF = 'calclient.push.enabled';

async function gotoSettings(page) {
  await page.evaluate(() => { location.hash = 'settings'; });
  await page.locator('#st-push').waitFor({ state: 'visible' });
}

test.describe('instant updates (push) preference', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
    await page.evaluate(k => localStorage.removeItem(k), PREF);   // known-off start
  });

  test('the toggle is available on a secure origin and turning it on persists', async ({ page }) => {
    await gotoSettings(page);
    const box = page.locator('#st-push');
    // 127.0.0.1 is a secure context, and headless Chromium has SW + Push +
    // Notification, so the capability gate opens the toggle.
    await expect(box).toBeEnabled();
    await expect(box).not.toBeChecked();

    await box.check();
    await expect(box).toBeChecked();
    expect(await page.evaluate(k => localStorage.getItem(k), PREF)).toBe('1');

    // Re-entering Settings re-reads the pref: the toggle stays on.
    await page.evaluate(() => { location.hash = 'calendar'; });
    await gotoSettings(page);
    await expect(page.locator('#st-push')).toBeChecked();
  });

  test('turning it off writes the disabled preference', async ({ page }) => {
    await page.evaluate(k => localStorage.setItem(k, '1'), PREF);
    await gotoSettings(page);
    const box = page.locator('#st-push');
    await expect(box).toBeChecked();

    await box.uncheck();
    await expect(box).not.toBeChecked();
    expect(await page.evaluate(k => localStorage.getItem(k), PREF)).toBe('0');
  });
});
