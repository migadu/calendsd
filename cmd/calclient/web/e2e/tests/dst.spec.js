// End-to-end tests for zoned event times (docs/dst-recurring-events.md): with
// the browser in UTC and the display-zone preference on Europe/Zurich, what
// the user types is Zurich wall time, it is stored as TZID=Europe/Zurich, a
// weekly series keeps its hour across the spring change, and moves across
// DST (drag, "all events" edits) move by wall-clock days in each value's own
// zone. Objects live in bob's default calendar and carry the TAG, which the
// afterEach sweep deletes by.
const { test, expect } = require('@playwright/test');
const { login, setPrefs, deleteEventsContaining } = require('./helpers');

test.use({ timezoneId: 'UTC' });

const TAG = 'dst-e2e-';
const CAL = '/calendars/bob/default/';
const auth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };

async function stored(request, uid) {
  const r = await request.fetch(`${CAL}${uid}.ics`, { headers: auth });
  expect(r.status(), `GET ${uid}`).toBe(200);
  return r.text();
}

// findStored returns the body of the one object in bob's calendar whose text
// contains needle (for events created through the UI, whose UID is random).
async function findStored(request, needle) {
  const r = await request.fetch(CAL, { method: 'PROPFIND', headers: { ...auth, Depth: '1' } });
  for (const [, href] of (await r.text()).matchAll(/href>([^<]+\.ics)</g)) {
    const body = await (await request.fetch(href, { headers: auth })).text();
    if (body.includes(needle)) return { href, body };
  }
  throw new Error(`no stored object contains ${needle}`);
}

async function seed(request, uid, lines) {
  const ics = ['BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//calclient//EN', 'BEGIN:VEVENT',
    `UID:${uid}`, 'DTSTAMP:20260101T000000Z', `SUMMARY:${uid}`, ...lines,
    'END:VEVENT', 'END:VCALENDAR', ''].join('\r\n');
  const put = await request.fetch(`${CAL}${uid}.ics`, {
    method: 'PUT', headers: { ...auth, 'Content-Type': 'text/calendar; charset=utf-8' }, data: ics,
  });
  expect(put.status(), `PUT ${uid}`).toBeLessThan(300);
}

test.beforeEach(async ({ page }) => {
  await login(page);
  await setPrefs(page, { tz: 'Europe/Zurich', hour12: false });
});

test.afterEach(async ({ page, request }) => {
  await deleteEventsContaining(request, TAG);
  await setPrefs(page, { tz: '' });   // bob's prefs are shared state
});

test('a weekly series typed in Zurich time is stored zoned and keeps its hour after the spring change', async ({ page, request }) => {
  const title = TAG + 'weekly-' + Date.now();
  await page.goto('/cal/calendar/week/2026-03-23');
  await page.click('#new-event');
  await page.fill('#ev-title', title);
  await page.fill('#ev-start', '2026-03-27');
  await page.fill('#ev-start-time', '09:00');
  await page.locator('#ev-start-time').blur();
  await page.fill('#ev-end', '2026-03-27');
  await page.fill('#ev-end-time', '10:00');
  await page.locator('#ev-end-time').blur();
  await page.selectOption('#ev-freq', 'WEEKLY');
  await page.click('#ev-save');
  await expect(page.locator('#ev-title')).toHaveCount(0);

  const { body } = await findStored(request, title);
  expect(body).toContain('DTSTART;TZID=Europe/Zurich:20260327T090000');
  expect(body).toContain('BEGIN:VTIMEZONE');   // the server attached the definition
  const uid = /UID:(.*)\r?\n/.exec(body)[1].trim();

  // 3 April is after the switch to summer time: still 09:00 in the grid...
  // (The week can hold two occurrences: a new weekly event also keeps
  // today's weekday selected.)
  await page.goto('/cal/calendar/week/2026-04-03');
  const blocks = page.locator('.block', { hasText: title });
  await expect(blocks.first()).toBeVisible();
  for (const text of await blocks.allTextContents()) expect(text).toMatch(/^09:00 /);
  // ...and in the reopened editor (the occurrence is 07:00Z).
  await page.goto(`/cal/event/${encodeURIComponent(uid)}/edit?occ=2026-04-03T07:00:00`);
  await expect(page.locator('#ev-start')).toHaveValue('2026-04-03');
  await expect(page.locator('#ev-start-time')).toHaveValue('09:00');
});

for (const [label, from, to, week] of [
  ['spring', '20260328', '20260329', '2026-03-23'],
  ['autumn', '20261024', '20261025', '2026-10-19'],
]) {
  test(`dragging a zoned event to the next day across the ${label} change keeps 09:00`, async ({ page, request }) => {
    const uid = `${TAG}drag-${label}-${Date.now()}`;
    await seed(request, uid, [`DTSTART;TZID=Europe/Zurich:${from}T090000`, `DTEND;TZID=Europe/Zurich:${from}T100000`]);
    await page.goto(`/cal/calendar/week/${week}`);
    const block = page.locator('.block', { hasText: uid });
    await expect(block).toContainText('09:00');
    const box = await block.boundingBox();
    const cols = page.locator('.week .col');
    const src = await block.evaluate(b => b.closest('.col').dataset.date);
    const idx = await cols.evaluateAll((cs, d) => cs.findIndex(c => c.dataset.date === d), src);
    const next = await cols.nth(idx + 1).boundingBox();
    const y = box.y + box.height / 2;
    await page.mouse.move(box.x + box.width / 2, y);
    await page.mouse.down();
    await page.mouse.move(box.x + box.width / 2 + 10, y, { steps: 3 });
    await page.mouse.move(next.x + next.width / 2, y, { steps: 5 });
    await page.mouse.up();
    await expect(page.locator('#toast')).toContainText('Moved');
    await expect.poll(async () => stored(request, uid)).toContain(`DTSTART;TZID=Europe/Zurich:${to}T090000`);
    expect(await stored(request, uid)).toContain(`DTEND;TZID=Europe/Zurich:${to}T100000`);
  });
}

test('an "all events" day move of a New York series moves it in New York wall time', async ({ page, request }) => {
  // Daily at 09:00 New York time. New York is already on summer time (8
  // March); Zurich switches on 29 March. The 28 March occurrence (13:00Z)
  // shows at 14:00 in Zurich; moving it to 14:00 Zurich on 29 March (12:00Z)
  // is one day minus an hour in New York, and the series follows exactly that.
  const uid = `${TAG}ny-${Date.now()}`;
  await seed(request, uid, ['DTSTART;TZID=America/New_York:20260320T090000',
    'DTEND;TZID=America/New_York:20260320T100000', 'RRULE:FREQ=DAILY']);
  await page.goto(`/cal/event/${encodeURIComponent(uid)}/edit?occ=2026-03-28T13:00:00`);
  await expect(page.locator('#ev-start-time')).toHaveValue('14:00');
  await page.selectOption('#ev-scope', 'all');
  await page.fill('#ev-start', '2026-03-29');
  await page.dispatchEvent('#ev-start', 'input');
  await page.click('#ev-save');
  await expect(page.locator('#ev-title')).toHaveCount(0);
  const body = await stored(request, uid);
  expect(body).toContain('DTSTART;TZID=America/New_York:20260321T080000');
  expect(body).toContain('DTEND;TZID=America/New_York:20260321T090000');
});

test('editing one occurrence of a drifted UTC series shows and keeps the original 10:00', async ({ page, request }) => {
  // Written in UTC by an older calweb: 10:00 CEST in September (08:00Z), so
  // after the October change it has drifted to 09:00 CET.
  const uid = `${TAG}drifted-${Date.now()}`;
  await seed(request, uid, ['DTSTART:20260911T080000Z', 'DTEND:20260911T090000Z', 'RRULE:FREQ=WEEKLY']);
  const shown = async wk => {
    await page.goto(`/cal/calendar/week/${wk}`);
    const b = page.locator('.block', { hasText: uid });
    await expect(b.first()).toBeVisible();
    return (await b.allTextContents()).map(t => t.slice(0, 5));
  };
  expect(await shown('2026-10-26')).toEqual(['09:00']);

  // The editor opens at the series' own 10:00 (and shows its repeat rule);
  // saving with the default "only this event" scope keeps 10:00 everywhere.
  await page.locator('.block', { hasText: uid }).click();
  await page.click('#ed-edit');
  await expect(page.locator('#ev-start-time')).toHaveValue('10:00');
  await expect(page.locator('#ev-freq')).toHaveValue('WEEKLY');
  await expect(page.locator('#ev-scope')).toHaveValue('one');
  await page.fill('#ev-title', uid + ' renamed');
  await page.click('#ev-save');
  await expect(page.locator('#ev-title')).toHaveCount(0);
  expect(await shown('2026-10-26')).toEqual(['10:00']);
  expect(await shown('2026-09-14')).toEqual(['10:00']);
  expect(await shown('2026-12-07')).toEqual(['10:00']);
  expect(await stored(request, uid)).toContain('DTSTART;TZID=Europe/Zurich:20260911T100000');
});
