const { test, expect } = require('@playwright/test');
const { login, setPrefs } = require('./helpers');

test.beforeEach(async ({ page }) => { await login(page); });

test('Escape closes the editor without saving', async ({ page }) => {
  await page.click('#new-event');
  await expect(page.locator('#ev-title')).toBeVisible();
  await page.fill('#ev-title', 'discard me');
  await page.locator('#ev-title').press('Escape');
  await expect(page.locator('#ev-title')).toHaveCount(0);   // dialog removed
});

test('repeat menu is selectable, and Enter on it does not submit', async ({ page }) => {
  await page.click('#new-event');
  await page.selectOption('#ev-freq', 'WEEKLY');
  await expect(page.locator('#ev-freq')).toHaveValue('WEEKLY');
  // Enter while a <select> is focused must open the menu, not save+close.
  await page.locator('#ev-freq').focus();
  await page.locator('#ev-freq').press('Enter');
  await expect(page.locator('#ev-title')).toBeVisible();    // still open
});

test('Enter from the title field submits the form', async ({ page }) => {
  await page.click('#new-event');
  await page.fill('#ev-title', 'Enter submits');
  await page.locator('#ev-title').press('Enter');
  await expect(page.locator('#ev-title')).toHaveCount(0);   // saved + closed
});

test('"repeat ends after…" shows the occurrences unit', async ({ page }) => {
  await page.click('#new-event');
  await page.selectOption('#ev-freq', 'WEEKLY');
  await page.selectOption('#ev-ends', 'count');
  await expect(page.locator('#ev-count')).toBeVisible();
  await expect(page.locator('#ev-count-unit')).toBeVisible();
  await expect(page.locator('#ev-count-unit')).toHaveText('times');
});

test('changing the start drags the end by the same amount', async ({ page }) => {
  await page.click('#new-event');
  const start = page.locator('#ev-start');
  const end = page.locator('#ev-end');
  await start.fill('2026-08-02T13:00');
  await end.fill('2026-08-02T14:00');
  await start.fill('2026-08-02T17:00');            // +4h
  await expect(end).toHaveValue('2026-08-02T18:00'); // end followed
});

test('24-hour preference yields 24h date inputs (Chromium honours lang)', async ({ page }) => {
  await setPrefs(page, { hour12: false, tz: '' });
  await page.click('#new-event');
  await expect(page.locator('#ev-start')).toHaveAttribute('lang', 'en-GB');
});

test('the guest-notification hint appears once a guest is added', async ({ page }) => {
  await page.click('#new-event');
  await expect(page.locator('#ev-guest-hint')).toBeHidden();
  await page.fill('#ev-guest', 'alice@example.org');
  await page.locator('#ev-guest').press('Enter');
  await expect(page.locator('#ev-guest-hint')).toBeVisible();
});
