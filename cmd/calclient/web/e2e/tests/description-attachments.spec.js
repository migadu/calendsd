// End-to-end tests for two small surfaces: editing a calendar's description
// (calendar-description) and showing an event's ATTACH attachments in the
// detail popup.
const { test, expect } = require('@playwright/test');
const { login, deleteEventsContaining } = require('./helpers');

const auth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };
const XMLNS = 'xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"';
const TAG = 'descatt-e2e-';

test.afterEach(async ({ request }) => { await deleteEventsContaining(request, TAG); });

test('editing a calendar description persists', async ({ page, request }) => {
  const name = `${TAG}cal-${Date.now()}`;
  const href = `/calendars/bob/${name}/`;
  const mk = await request.fetch(href, { method: 'MKCALENDAR', headers: { ...auth, 'Content-Type': 'application/xml' },
    data: `<?xml version="1.0"?><c:mkcalendar ${XMLNS}><d:set><d:prop><d:displayname>${name}</d:displayname><c:supported-calendar-component-set><c:comp name="VEVENT"/></c:supported-calendar-component-set></d:prop></d:set></c:mkcalendar>` });
  expect(mk.status()).toBe(201);
  try {
    await login(page);
    const li = page.locator(`#cal-list li[data-href="${href}"]`);
    await li.hover();                                     // reveal the settings gear (visibility:hidden until hover)
    await li.locator('.gear').click();
    await page.fill('#cm-desc', 'Team planning calendar');
    await page.click('#cm-save');
    await expect(page.locator('#cm-desc')).toHaveCount(0); // dialog closed

    // persisted on the server, and shown when reopened
    const pf = await request.fetch(href, { method: 'PROPFIND', headers: { ...auth, Depth: '0', 'Content-Type': 'application/xml' },
      data: `<?xml version="1.0"?><d:propfind ${XMLNS}><d:prop><c:calendar-description/></d:prop></d:propfind>` });
    expect(await pf.text()).toContain('Team planning calendar');
    // and the client reads it back into the calendar model (so the dialog shows it)
    const inState = await page.evaluate(async (h) => {
      const s = await import('/shared/store.js');
      await s.loadCalendars();
      return s.state.calendars.find(c => c.href === h)?.description;
    }, href);
    expect(inState).toBe('Team planning calendar');
  } finally {
    await request.fetch(href, { method: 'DELETE', headers: auth });
  }
});

test('an event attachment shows as a link in the detail popup', async ({ page, request }) => {
  const title = TAG + Date.now();
  const noon = new Date(); noon.setHours(12, 0, 0, 0);
  const utc = d => d.toISOString().replace(/[-:]/g, '').replace(/\.\d+Z$/, 'Z');
  const href = `/calendars/bob/calendar/${title}.ics`;
  const ics = [
    'BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//e2e//EN', 'BEGIN:VEVENT',
    `UID:${title}`, `DTSTAMP:${utc(noon)}`, `DTSTART:${utc(noon)}`,
    `DTEND:${utc(new Date(+noon + 3600000))}`, `SUMMARY:${title}`,
    'ATTACH;FILENAME=agenda.pdf:https://files.example.org/agenda.pdf',
    'END:VEVENT', 'END:VCALENDAR', '',
  ].join('\r\n');
  const put = await request.fetch(href, { method: 'PUT', headers: { ...auth, 'Content-Type': 'text/calendar; charset=utf-8' }, data: ics });
  expect(put.status()).toBeLessThan(300);

  await login(page);
  await page.locator('.chip', { hasText: title }).first().click();
  await expect(page.locator('#ed-del')).toBeVisible();
  const link = page.locator('.detail-row', { hasText: 'Attachments' }).locator('a', { hasText: 'agenda.pdf' });
  await expect(link).toBeVisible();
  await expect(link).toHaveAttribute('href', 'https://files.example.org/agenda.pdf');
});
