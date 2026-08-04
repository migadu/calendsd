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
  await page.fill('#ev-start', '2026-08-02');
  await page.fill('#ev-start-time', '13:00');
  await page.locator('#ev-start-time').blur();     // commit the time
  await page.fill('#ev-end', '2026-08-02');
  await page.fill('#ev-end-time', '14:00');
  await page.fill('#ev-start-time', '17:00');      // +4h
  await page.locator('#ev-start-time').blur();
  await expect(page.locator('#ev-end-time')).toHaveValue(/^(18:00|6:00 PM)$/); // end followed
  await expect(page.locator('#ev-end')).toHaveValue('2026-08-02');
});

// The time fields are app-rendered (not the native datetime-local half, whose
// format follows the browser locale and ignores the app's preference), so the
// displayed value itself must match the 12/24-hour setting.
test('24-hour preference shows 24h times in the editor', async ({ page }) => {
  await setPrefs(page, { hour12: false, tz: '' });
  await page.click('#new-event');
  await expect(page.locator('#ev-start-time')).toHaveValue(/^\d{2}:\d{2}$/); // e.g. 14:00 — no AM/PM
  await expect(page.locator('#ev-end-time')).toHaveValue(/^\d{2}:\d{2}$/);
  // typed 12-hour input still parses, and is normalized to the 24-hour clock
  await page.fill('#ev-start-time', '1:30 pm');
  await page.locator('#ev-start-time').blur();
  await expect(page.locator('#ev-start-time')).toHaveValue('13:30');
});

test('12-hour preference shows AM/PM times in the editor', async ({ page }) => {
  await setPrefs(page, { hour12: true, tz: '' });
  try {
    await page.click('#new-event');
    await expect(page.locator('#ev-start-time')).toHaveValue(/^\d{1,2}:\d{2} [AP]M$/);
    // typed 24-hour input still parses, and is normalized to the 12-hour clock
    await page.fill('#ev-start-time', '13:30');
    await page.locator('#ev-start-time').blur();
    await expect(page.locator('#ev-start-time')).toHaveValue('1:30 PM');
    await page.locator('#ev-title').press('Escape');
  } finally {
    await setPrefs(page, { hour12: false });      // bob's prefs are shared state
  }
});

test('an unparseable time is flagged and blocks the save', async ({ page }) => {
  await page.click('#new-event');
  await page.fill('#ev-start-time', 'noon-ish');
  await page.locator('#ev-start-time').blur();
  await expect(page.locator('#ev-start-time')).toHaveClass(/invalid/);
  await page.click('#ev-save');
  await expect(page.locator('#ev-error')).toContainText('Please enter a time');
  await expect(page.locator('#ev-title')).toBeVisible();   // still open, not saved
});

test('the all-day toggle hides the time fields', async ({ page }) => {
  await page.click('#new-event');
  await expect(page.locator('#ev-start-time')).toBeVisible();
  await page.check('#ev-allday');
  await expect(page.locator('#ev-start-time')).toBeHidden();
  await expect(page.locator('#ev-end-time')).toBeHidden();
  await page.uncheck('#ev-allday');
  await expect(page.locator('#ev-start-time')).toBeVisible();
});

test('the guest-notification hint appears once a guest is added', async ({ page }) => {
  await page.click('#new-event');
  await expect(page.locator('#ev-guest-hint')).toBeHidden();
  await page.fill('#ev-guest', 'alice@example.org');
  await page.locator('#ev-guest').press('Enter');
  await expect(page.locator('#ev-guest-hint')).toBeVisible();
});
