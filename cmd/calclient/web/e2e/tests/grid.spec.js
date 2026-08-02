const { test, expect } = require('@playwright/test');
const { login } = require('./helpers');

test.beforeEach(async ({ page }) => { await login(page); });

test('week view draws a current-time line in today\'s column', async ({ page }) => {
  await page.click('#view-seg button[data-view="week"]');
  await page.locator('.week .col').first().waitFor();
  await expect(page.locator('.week .now-line')).toHaveCount(1);
});

test('opening the week view scrolls to the current time', async ({ page }) => {
  await page.click('#view-seg button[data-view="week"]');
  await page.locator('.week .now-line').waitFor();
  const scrollTop = await page.evaluate(() => document.querySelector('#content').scrollTop);
  expect(scrollTop).toBeGreaterThan(0);   // not left at midnight
});
