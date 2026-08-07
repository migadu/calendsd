// End-to-end tests for the event editor's Reminder control: the preset
// round-trips through a real save (eventLines / setMasterAlarm / alarmPreset
// against the live server), clearing removes the stored VALARM, and an alarm
// the editor doesn't model shows as "Custom" and survives an unrelated edit
// untouched. The preset↔trigger mapping itself is unit-tested in
// tests/ical.test.js.
const { test, expect } = require('@playwright/test');
const { login, deleteEventsContaining } = require('./helpers');

const TAG = 'rem-e2e-';           // distinctive: the sweep deletes by substring
const auth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };

// Each test writes a uniquely-named event to the shared dev DB; sweep this
// run's copy and any strays from earlier runs so today's month cell doesn't
// fill up and hide the chip behind "+N more".
test.afterEach(async ({ request }) => {
  await deleteEventsContaining(request, TAG);
});

const pad = n => String(n).padStart(2, '0');
const todayYMD = () => {
  const d = new Date();
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
};

// createEvent makes a timed event today with the given reminder preset and
// returns its title. reminder '' leaves the dropdown at "No reminder".
async function createEvent(page, reminder) {
  const title = TAG + Date.now();
  const ymd = todayYMD();
  await page.click('#new-event');
  await page.fill('#ev-title', title);
  await page.fill('#ev-start', ymd);
  await page.fill('#ev-start-time', '09:00');
  await page.fill('#ev-end', ymd);
  await page.fill('#ev-end-time', '09:30');
  if (reminder) await page.selectOption('#ev-remind', reminder);
  await page.click('#ev-save');
  await expect(page.locator('#ev-title')).toHaveCount(0);
  return title;
}

// reopen clicks the event's chip and opens the editor from the detail popover.
async function reopen(page, title) {
  await page.locator('.chip', { hasText: title }).first().click();
  await expect(page.locator('#ed-edit')).toBeVisible();
  await page.click('#ed-edit');
  await expect(page.locator('#ev-remind')).toBeVisible();
}

test('new event defaults to no reminder', async ({ page }) => {
  await login(page);
  await page.click('#new-event');
  await expect(page.locator('#ev-remind')).toHaveValue('');
});

test('a chosen reminder round-trips through save and reopen', async ({ page }) => {
  await login(page);
  const title = await createEvent(page, '-PT15M');
  await reopen(page, title);
  // The stored VALARM is read back onto the same preset, not shown as Custom.
  await expect(page.locator('#ev-remind')).toHaveValue('-PT15M');
  await expect(page.locator('#ev-remind option[value="CUSTOM"]')).toHaveCount(0);
});

test('clearing the reminder removes the stored alarm', async ({ page }) => {
  await login(page);
  const title = await createEvent(page, '-PT1H');
  await reopen(page, title);
  await expect(page.locator('#ev-remind')).toHaveValue('-PT1H');
  await page.selectOption('#ev-remind', '');
  await page.click('#ev-save');
  await expect(page.locator('#ev-title')).toHaveCount(0);
  await reopen(page, title);
  await expect(page.locator('#ev-remind')).toHaveValue('');
});

test('an unmodeled alarm shows as Custom and survives an unrelated edit', async ({ page, request }) => {
  // PUT an event carrying an EMAIL alarm — a shape the editor can't reproduce.
  // Editing any other field must leave it untouched (the "kept as is"
  // contract); a regression here silently destroys users' existing alarms.
  const title = TAG + 'custom-' + Date.now();
  const noon = new Date(); noon.setHours(12, 0, 0, 0);  // local noon: today in any sane display tz
  const dt = min => new Date(+noon + min * 60000).toISOString()
    .replace(/[-:]/g, '').replace(/\.\d+Z$/, 'Z');
  const href = `/calendars/bob/calendar/${title}.ics`;   // bob's auto-provisioned default
  const ics = [
    'BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//e2e//EN', 'BEGIN:VEVENT',
    `UID:${title}`, `DTSTAMP:${dt(0)}`, `DTSTART:${dt(0)}`, `DTEND:${dt(30)}`,
    `SUMMARY:${title}`,
    'BEGIN:VALARM', 'ACTION:EMAIL', 'SUMMARY:reminder', 'DESCRIPTION:reminder',
    'ATTENDEE:mailto:bob@example.org', 'TRIGGER:-PT20M', 'END:VALARM',
    'END:VEVENT', 'END:VCALENDAR', '',
  ].join('\r\n');
  const put = await request.fetch(href, {
    method: 'PUT', headers: { ...auth, 'Content-Type': 'text/calendar; charset=utf-8' }, data: ics,
  });
  expect(put.status(), `PUT ${href}`).toBeLessThan(300);

  await login(page);
  await reopen(page, title);
  await expect(page.locator('#ev-remind')).toHaveValue('CUSTOM');
  await expect(page.locator('#ev-remind option:checked')).toHaveText(/kept as is/);

  // Change an unrelated field and save.
  await page.fill('#ev-loc', 'Room 4');
  await page.click('#ev-save');
  await expect(page.locator('#ev-title')).toHaveCount(0);

  const saved = await (await request.fetch(href, { headers: auth })).text();
  expect(saved).toContain('LOCATION:Room 4');       // the edit landed…
  expect(saved).toContain('ACTION:EMAIL');          // …and the alarm survived intact
  expect(saved).toContain('TRIGGER:-PT20M');
});
