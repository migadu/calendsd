// End-to-end test for the organizer's per-attendee delivery status
// (SCHEDULE-STATUS). The e2e server runs with iMIP disabled, so inviting an
// external attendee has no delivery path and the server writes a 5.x
// SCHEDULE-STATUS back onto the organizer's copy. The detail popup must then
// show a "not sent" pill on that guest and a warning banner (ui.js
// scheduleStatusTag + event-detail.js).
const { test, expect } = require('@playwright/test');
const { login, deleteEventsContaining } = require('./helpers');

const TAG = 'delivery-e2e-';
const auth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };

test.afterEach(async ({ request }) => { await deleteEventsContaining(request, TAG); });

const utc = d => d.toISOString().replace(/[-:]/g, '').replace(/\.\d+Z$/, 'Z');

// putUndeliverable creates an event bob organizes for an unroutable external
// attendee; with iMIP disabled the server stamps a 5.x SCHEDULE-STATUS it can't
// retry. Returns { title, href }.
async function putUndeliverable(request) {
  const title = TAG + Date.now() + '-' + Math.floor(Math.random() * 1e6);
  const noon = new Date(); noon.setHours(12, 0, 0, 0);
  const href = `/calendars/bob/calendar/${title}.ics`;
  const ics = [
    'BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//e2e//EN', 'BEGIN:VEVENT',
    `UID:${title}`, `DTSTAMP:${utc(noon)}`, `DTSTART:${utc(noon)}`,
    `DTEND:${utc(new Date(+noon + 30 * 60000))}`, `SUMMARY:${title}`,
    'ORGANIZER;CN=Bob:mailto:bob@example.org',
    'ATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:nobody@nowhere.invalid',
    'END:VEVENT', 'END:VCALENDAR', '',
  ].join('\r\n');
  const put = await request.fetch(href, {
    method: 'PUT', headers: { ...auth, 'Content-Type': 'text/calendar; charset=utf-8' }, data: ics,
  });
  expect(put.status(), `PUT ${href}`).toBeLessThan(300);
  return { title, href };
}

test('a failed invite shows a warning banner and a "not sent" pill', async ({ page, request }) => {
  const { title, href } = await putUndeliverable(request);

  // Sanity: the server really did stamp a 5.x SCHEDULE-STATUS on the attendee.
  const stored = await (await request.fetch(href, { headers: auth })).text();
  expect(stored).toMatch(/SCHEDULE-STATUS="?5\./);

  await login(page);
  await page.locator('.chip', { hasText: title }).first().click();
  await expect(page.locator('#ed-del')).toBeVisible();

  await expect(page.locator('.detail-warn')).toContainText('couldn’t be delivered');
  await expect(page.locator('.type-tag.bad')).toContainText('not sent');
});

test('Resend forces a re-send and the server strips SCHEDULE-FORCE-SEND', async ({ page, request }) => {
  const { title, href } = await putUndeliverable(request);

  await login(page);
  await page.locator('.chip', { hasText: title }).first().click();
  await expect(page.locator('.detail-warn')).toContainText('couldn’t be delivered');

  await page.locator('#ed-resend').click();
  // The popup closes on a successful resend.
  await expect(page.locator('.detail-warn')).toBeHidden();

  // The re-PUT carried SCHEDULE-FORCE-SEND=REQUEST; the server honored it (the
  // broker re-ran, re-stamping the status) and stripped the parameter so it
  // won't persist and re-fire.
  const stored = await (await request.fetch(href, { headers: auth })).text();
  expect(stored).not.toContain('SCHEDULE-FORCE-SEND');
  expect(stored).toMatch(/SCHEDULE-STATUS="?5\./);
});
