// End-to-end tests for deleting an event from the read-only detail popup. A
// plain event deletes behind a single confirm; a repeating event offers a
// This-event / All-events choice (event-detail.js → store.deleteEvent), where
// "This event" excludes one occurrence with an EXDATE and "All events" removes
// the whole stored object. Events are seeded over CalDAV, and the resulting
// server state is asserted over CalDAV too, so the whole path is exercised.
const { test, expect } = require('@playwright/test');
const { login, deleteEventsContaining } = require('./helpers');

const TAG = 'recur-del-e2e-';     // distinctive: the sweep deletes by substring
const auth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };

test.afterEach(async ({ request }) => {
  await deleteEventsContaining(request, TAG);
});

// utc renders a Date as a compact iCal UTC value (20260810T120000Z).
const utc = d => d.toISOString().replace(/[-:]/g, '').replace(/\.\d+Z$/, 'Z');

// seed PUTs a VEVENT into bob's auto-provisioned default calendar and returns
// its href. `rrule` omitted → a plain (non-recurring) event. DTSTART is local
// noon today so the chip lands in today's cell in any sane display tz.
async function seed(request, title, rrule) {
  const noon = new Date(); noon.setHours(12, 0, 0, 0);
  const href = `/calendars/bob/calendar/${title}.ics`;
  const ics = [
    'BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//e2e//EN', 'BEGIN:VEVENT',
    `UID:${title}`, `DTSTAMP:${utc(noon)}`, `DTSTART:${utc(noon)}`,
    `DTEND:${utc(new Date(+noon + 30 * 60000))}`, `SUMMARY:${title}`,
    ...(rrule ? [`RRULE:${rrule}`] : []),
    'END:VEVENT', 'END:VCALENDAR', '',
  ].join('\r\n');
  const put = await request.fetch(href, {
    method: 'PUT', headers: { ...auth, 'Content-Type': 'text/calendar; charset=utf-8' }, data: ics,
  });
  expect(put.status(), `PUT ${href}`).toBeLessThan(300);
  return href;
}

// openDetail clicks the (earliest) chip and waits for the detail popup.
async function openDetail(page, title) {
  await page.locator('.chip', { hasText: title }).first().click();
  await expect(page.locator('#ed-del')).toBeVisible();
}

const body = async (request, href) => (await request.fetch(href, { headers: auth })).text();

test('a plain event deletes behind a single confirm — no scope choice', async ({ page, request }) => {
  const title = TAG + 'plain-' + Date.now();
  const href = await seed(request, title);
  await login(page);
  await openDetail(page, title);

  await page.click('#ed-del');
  // Plain event: the confirm dialog, not the This/All choice.
  await expect(page.getByRole('button', { name: 'This event', exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'All events', exact: true })).toHaveCount(0);
  await page.locator('[data-ok]').click();          // "Delete"

  await expect(page.locator('#ed-del')).toHaveCount(0);   // popup closed
  const r = await request.fetch(href, { headers: auth });
  expect(r.status(), 'object should be gone').toBe(404);
});

test('repeating event → "This event" adds an EXDATE and keeps the series', async ({ page, request }) => {
  const title = TAG + 'one-' + Date.now();
  const href = await seed(request, title, 'FREQ=DAILY;COUNT=5');
  await login(page);
  await openDetail(page, title);

  await page.click('#ed-del');
  await expect(page.getByRole('button', { name: 'This event', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'All events', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'This event', exact: true }).click();

  await expect(page.locator('#ed-del')).toHaveCount(0);   // popup closed
  const saved = await body(request, href);
  expect(saved, 'series master survives').toContain('RRULE:FREQ=DAILY');
  expect(saved, 'clicked occurrence excluded').toContain('EXDATE');
});

test('repeating event → "All events" removes the whole series', async ({ page, request }) => {
  const title = TAG + 'all-' + Date.now();
  const href = await seed(request, title, 'FREQ=DAILY;COUNT=5');
  await login(page);
  await openDetail(page, title);

  await page.click('#ed-del');
  await page.getByRole('button', { name: 'All events', exact: true }).click();

  await expect(page.locator('#ed-del')).toHaveCount(0);   // popup closed
  const r = await request.fetch(href, { headers: auth });
  expect(r.status(), 'whole object should be gone').toBe(404);
});

test('cancelling the delete choice changes nothing', async ({ page, request }) => {
  const title = TAG + 'cancel-' + Date.now();
  const href = await seed(request, title, 'FREQ=DAILY;COUNT=5');
  await login(page);
  await openDetail(page, title);

  await page.click('#ed-del');
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  // The choice closes; the detail popup stays open.
  await expect(page.getByRole('button', { name: 'This event', exact: true })).toHaveCount(0);
  await expect(page.locator('#ed-del')).toBeVisible();

  const saved = await body(request, href);
  expect(saved).toContain('RRULE:FREQ=DAILY');
  expect(saved).not.toContain('EXDATE');
});
