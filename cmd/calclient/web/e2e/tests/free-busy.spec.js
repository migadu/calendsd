const { test, expect } = require('@playwright/test');
const { login, deleteEventsContaining } = require('./helpers');

// The spec writes a uniquely-named event to the shared dev DB; sweep this
// run's copy AND any strays from earlier runs, or they pile up in today's
// month cell until new chips hide behind "+N more" and the locator times out.
test.afterEach(async ({ request }) => {
  await deleteEventsContaining(request, 'fb-self-');
});

// Regression for the "busy against itself" bug: editing a saved event must not
// count that event's own slot as a conflict for its invitees.
test('editing an event does not report the event itself as busy', async ({ page }) => {
  await login(page);
  const title = 'fb-self-' + Date.now();
  const d = new Date();
  const pad = n => String(n).padStart(2, '0');
  const ymd = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;

  // Create a timed event today with a guest, then save.
  await page.click('#new-event');
  await page.fill('#ev-title', title);
  await page.fill('#ev-start', `${ymd}T09:00`);
  await page.fill('#ev-end', `${ymd}T09:30`);
  await page.fill('#ev-guest', 'bob@example.org');
  await page.locator('#ev-guest').press('Enter');
  await page.click('#ev-save');
  await expect(page.locator('#ev-title')).toHaveCount(0);

  // Reopen it (edit), so `inst` is set and self-exclusion applies.
  await page.locator('.chip', { hasText: title }).first().click();
  await expect(page.locator('#ed-edit')).toBeVisible();
  await page.click('#ed-edit');

  // Check availability at the unchanged (own) time: the guest's own copy of this
  // event must be discounted, so the proposed slot is free.
  await page.click('#ev-check');
  await expect(page.locator('.avail-sum')).toBeVisible();
  await expect(page.locator('.avail-sum')).toContainText('Everyone is free');
});
