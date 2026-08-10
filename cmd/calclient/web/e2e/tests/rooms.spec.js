// End-to-end tests for surfacing rooms in the attendee picker. The e2e server
// runs with -auto-schedule-prefixes resource,location, so the seeded resource*/
// location* principals report calendar-user-type=ROOM; the picker tags them and
// the chip round-trips as CUTYPE=ROOM (guest-field.js + ical.js).
const { test, expect } = require('@playwright/test');
const { login, deleteEventsContaining } = require('./helpers');

test.beforeEach(async ({ page }) => { await login(page); });
test.afterEach(async ({ request }) => { await deleteEventsContaining(request, 'room-e2e-'); });

test('the picker tags a room and leaves people untagged', async ({ page }) => {
  await page.click('#new-event');

  await page.fill('#ev-guest', 'resource');
  const room = page.locator('#ev-suggest button', { hasText: 'Resource' }).first();
  await expect(room).toBeVisible();
  await expect(room.locator('.type-tag')).toHaveText('room');

  await page.fill('#ev-guest', 'user');
  await expect(page.locator('#ev-suggest button').first()).toBeVisible();
  await expect(page.locator('#ev-suggest button .type-tag')).toHaveCount(0); // people aren't rooms
});

test('a booked room round-trips as a room chip after reopen', async ({ page }) => {
  const title = 'room-e2e-' + Date.now();
  await page.click('#new-event');
  await page.fill('#ev-title', title);

  await page.fill('#ev-guest', 'resource');
  await page.locator('#ev-suggest button', { hasText: 'Resource' }).first().click();
  await expect(page.locator('#ev-chips .guest.room')).toBeVisible();   // marked on pick

  await page.click('#ev-save');
  await expect(page.locator('#ev-title')).toHaveCount(0);

  // Reopen: the room chip persists, proving the CUTYPE=ROOM round-trip.
  await page.locator('.chip', { hasText: title }).first().click();
  await page.click('#ed-edit');
  await expect(page.locator('#ev-chips .guest.room')).toBeVisible();
});
