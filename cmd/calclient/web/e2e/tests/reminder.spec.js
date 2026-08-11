// End-to-end tests for the event editor's Reminders control: presets and custom
// offsets round-trip through a real save (eventLines / setMasterAlarms /
// readReminders against the live server), removing a row clears the stored
// VALARM, and an alarm the editor doesn't model shows "kept as is" and survives
// an unrelated edit untouched.
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

// Reminders control helpers, scoped to the event field container.
const rows = page => page.locator('#ev-remind-field .rem-row');
async function addPreset(page, trigger) {
  await page.locator('#ev-remind-field .rem-add').click();
  await rows(page).last().locator('.rem-preset').selectOption(trigger);
}
async function addCustom(page, { amount, unit, when }) {
  await page.locator('#ev-remind-field .rem-add').click();
  const row = rows(page).last();
  await row.locator('.rem-preset').selectOption('custom');
  await row.locator('.rem-amount').fill(String(amount));
  await row.locator('.rem-unit').selectOption(unit);
  await row.locator('.rem-when').selectOption(when);
}

// startEvent fills the base fields on a new event and returns its title; the
// caller adds reminders before saving.
async function startEvent(page) {
  const title = TAG + Date.now();
  const ymd = todayYMD();
  await page.click('#new-event');
  await page.fill('#ev-title', title);
  await page.fill('#ev-start', ymd);
  await page.fill('#ev-start-time', '09:00');
  await page.fill('#ev-end', ymd);
  await page.fill('#ev-end-time', '09:30');
  return title;
}
async function save(page) {
  await page.click('#ev-save');
  await expect(page.locator('#ev-title')).toHaveCount(0);
}

// reopen clicks the event's chip and opens the editor from the detail popover.
async function reopen(page, title) {
  await page.locator('.chip', { hasText: title }).first().click();
  await expect(page.locator('#ed-edit')).toBeVisible();
  await page.click('#ed-edit');
  await expect(page.locator('#ev-remind-field')).toBeVisible();
}

test('new event has no reminders', async ({ page }) => {
  await login(page);
  await page.click('#new-event');
  await expect(rows(page)).toHaveCount(0);
});

test('a chosen reminder round-trips through save and reopen', async ({ page }) => {
  await login(page);
  const title = await startEvent(page);
  await addPreset(page, '-PT15M');
  await save(page);
  await reopen(page, title);
  // The stored VALARM is read back onto the same preset, not "kept as is".
  await expect(rows(page)).toHaveCount(1);
  await expect(rows(page).first().locator('.rem-preset')).toHaveValue('-PT15M');
  await expect(page.locator('#ev-remind-field .rem-kept')).toHaveCount(0);
});

test('multiple reminders round-trip', async ({ page }) => {
  await login(page);
  const title = await startEvent(page);
  await addPreset(page, '-PT15M');
  await addPreset(page, '-P1D');
  await save(page);
  await reopen(page, title);
  await expect(rows(page)).toHaveCount(2);
  const values = await page.locator('#ev-remind-field .rem-preset')
    .evaluateAll(els => els.map(e => e.value).sort());
  expect(values).toEqual(['-P1D', '-PT15M']);
});

test('a custom offset round-trips', async ({ page }) => {
  await login(page);
  const title = await startEvent(page);
  await addCustom(page, { amount: 2, unit: 'days', when: 'before' });   // -P2D, not a preset
  await save(page);
  await reopen(page, title);
  const r = rows(page).first();
  await expect(r.locator('.rem-preset')).toHaveValue('custom');
  await expect(r.locator('.rem-amount')).toHaveValue('2');
  await expect(r.locator('.rem-unit')).toHaveValue('days');
  await expect(r.locator('.rem-when')).toHaveValue('before');
});

test('removing a reminder clears the stored alarm', async ({ page }) => {
  await login(page);
  const title = await startEvent(page);
  await addPreset(page, '-PT1H');
  await save(page);
  await reopen(page, title);
  await expect(rows(page)).toHaveCount(1);
  await rows(page).first().locator('.rem-remove').click();
  await save(page);
  await reopen(page, title);
  await expect(rows(page)).toHaveCount(0);
});

test('an unmodeled alarm is kept as is and survives an unrelated edit', async ({ page, request }) => {
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
  await expect(page.locator('#ev-remind-field .rem-kept')).toBeVisible();
  await expect(rows(page)).toHaveCount(0);

  // Change an unrelated field and save.
  await page.fill('#ev-loc', 'Room 4');
  await save(page);

  const saved = await (await request.fetch(href, { headers: auth })).text();
  expect(saved).toContain('LOCATION:Room 4');       // the edit landed…
  expect(saved).toContain('ACTION:EMAIL');          // …and the alarm survived intact
  expect(saved).toContain('TRIGGER:-PT20M');
});

test('replacing a kept-as-is alarm writes a modeled reminder', async ({ page, request }) => {
  // Same unmodeled EMAIL alarm, but this time the user hits Replace and picks a
  // preset — the old alarm is dropped and a plain DISPLAY reminder is written.
  const title = TAG + 'replace-' + Date.now();
  const noon = new Date(); noon.setHours(12, 0, 0, 0);
  const dt = min => new Date(+noon + min * 60000).toISOString()
    .replace(/[-:]/g, '').replace(/\.\d+Z$/, 'Z');
  const href = `/calendars/bob/calendar/${title}.ics`;
  const ics = [
    'BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//e2e//EN', 'BEGIN:VEVENT',
    `UID:${title}`, `DTSTAMP:${dt(0)}`, `DTSTART:${dt(0)}`, `DTEND:${dt(30)}`,
    `SUMMARY:${title}`,
    'BEGIN:VALARM', 'ACTION:EMAIL', 'DESCRIPTION:reminder',
    'ATTENDEE:mailto:bob@example.org', 'TRIGGER:-PT20M', 'END:VALARM',
    'END:VEVENT', 'END:VCALENDAR', '',
  ].join('\r\n');
  await request.fetch(href, {
    method: 'PUT', headers: { ...auth, 'Content-Type': 'text/calendar; charset=utf-8' }, data: ics,
  });

  await login(page);
  await reopen(page, title);
  await page.locator('#ev-remind-field .rem-replace').click();
  await addPreset(page, '-PT15M');
  await save(page);

  const saved = await (await request.fetch(href, { headers: auth })).text();
  expect(saved).not.toContain('ACTION:EMAIL');       // the old alarm is gone…
  expect(saved).toContain('ACTION:DISPLAY');         // …replaced by a display reminder
  expect(saved).toContain('TRIGGER;RELATED=START:-PT15M');
});
