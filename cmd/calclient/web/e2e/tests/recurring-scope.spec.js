// End-to-end tests for the "this and following" recurrence scope. The client
// splits a series into two plain VEVENTs (event-dialog.js / store.deleteEvent /
// ical.splitSeriesRRules); here we drive the real UI and assert the resulting
// occurrences and stored rule. The split date math itself is unit-tested in
// tests/ical.test.js.
const { test, expect } = require('@playwright/test');
const { login, deleteEventsContaining } = require('./helpers');

const TAG = 'scope-e2e-';
const auth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };

test.afterEach(async ({ request }) => { await deleteEventsContaining(request, TAG); });

const utc = d => d.toISOString().replace(/[-:]/g, '').replace(/\.\d+Z$/, 'Z');

// seedWeekly puts a 5-occurrence weekly event starting on the 1st of the
// current month at noon, so every occurrence lands in the visible month grid.
async function seedWeekly(request, title) {
  const now = new Date();
  const first = new Date(now.getFullYear(), now.getMonth(), 1, 12, 0, 0);
  const href = `/calendars/bob/calendar/${title}.ics`;
  const ics = [
    'BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//e2e//EN', 'BEGIN:VEVENT',
    `UID:${title}`, `DTSTAMP:${utc(first)}`, `DTSTART:${utc(first)}`,
    `DTEND:${utc(new Date(+first + 30 * 60000))}`, `SUMMARY:${title}`,
    'RRULE:FREQ=WEEKLY;COUNT=5', 'END:VEVENT', 'END:VCALENDAR', '',
  ].join('\r\n');
  const put = await request.fetch(href, {
    method: 'PUT', headers: { ...auth, 'Content-Type': 'text/calendar; charset=utf-8' }, data: ics,
  });
  expect(put.status(), `PUT ${href}`).toBeLessThan(300);
  return href;
}

test('delete "this and following" truncates the series', async ({ page, request }) => {
  const title = TAG + 'del-' + Date.now();
  const href = await seedWeekly(request, title);
  await login(page);

  const chips = page.locator('.chip', { hasText: title });
  await expect(chips).toHaveCount(5);
  await chips.nth(1).click();                          // the 2nd occurrence
  await expect(page.locator('#ed-del')).toBeVisible();
  await page.click('#ed-del');
  await page.getByRole('button', { name: 'This and following', exact: true }).click();

  await expect(chips).toHaveCount(1);                  // only the first occurrence survives
  const stored = await (await request.fetch(href, { headers: auth })).text();
  expect(stored).toMatch(/UNTIL=/);                    // truncated…
  expect(stored).not.toMatch(/COUNT=/);               // …and COUNT dropped
});

test('edit "this and following" splits into two series', async ({ page, request }) => {
  const title = TAG + 'edit-' + Date.now();
  const moved = TAG + 'moved-' + Date.now();
  await seedWeekly(request, title);
  await login(page);

  const chips = page.locator('.chip', { hasText: title });
  await expect(chips).toHaveCount(5);
  await chips.nth(1).click();                          // the 2nd occurrence
  await page.click('#ed-edit');
  await expect(page.locator('#ev-scope')).toBeVisible();
  await page.selectOption('#ev-scope', 'future');
  await page.fill('#ev-title', moved);
  await page.click('#ev-save');

  // The original keeps only its first occurrence; the rest become a new series.
  await expect(page.locator('.chip', { hasText: title })).toHaveCount(1);
  await expect(page.locator('.chip', { hasText: moved })).toHaveCount(4);
});
