// End-to-end test for the read-only guard on the detail popup's Delete. bob
// owns a calendar holding a repeating event and shares it read-only with
// user01 (the server auto-accepts, so it lands in user01's home). When user01
// clicks Delete, the code must NOT offer the This/All choice or attempt a write
// that would 403 — it routes to the editor, which explains the calendar is
// read-only and disables its write actions (event-detail.js → isReadOnly).
//
// Setup is done over CalDAV (MKCALENDAR + PUT + cs:share, mirroring
// store.createCalendar / shareCalendar); the sharee is driven in a second
// browser context so it has its own clean session.
const { test, expect } = require('@playwright/test');
const { login } = require('./helpers');

const TAG = 'shareRODel-e2e-';    // space-free: also the calendar's URL slug
const bobAuth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };
const SHAREE = 'user01@example.org';

const XMLNS = 'xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:cs="http://calendarserver.org/ns/"';
const utc = d => d.toISOString().replace(/[-:]/g, '').replace(/\.\d+Z$/, 'Z');

// mkcalendar creates a VEVENT calendar under bob's home at a href we control
// (the slug is `name`, so no spaces), and returns that href.
async function mkcalendar(request, name) {
  const href = `/calendars/bob/${name}/`;
  const body = `<?xml version="1.0" encoding="utf-8"?>\n<c:mkcalendar ${XMLNS}><d:set><d:prop>`
    + `<d:displayname>${name}</d:displayname>`
    + `<c:supported-calendar-component-set><c:comp name="VEVENT"/></c:supported-calendar-component-set>`
    + `</d:prop></d:set></c:mkcalendar>`;
  const r = await request.fetch(href, {
    method: 'MKCALENDAR', headers: { ...bobAuth, 'Content-Type': 'application/xml; charset=utf-8' }, data: body,
  });
  expect(r.status(), `MKCALENDAR ${href}`).toBe(201);
  return href;
}

// putRepeating drops a daily-repeating event into the given calendar today.
async function putRepeating(request, calHref, title) {
  const noon = new Date(); noon.setHours(12, 0, 0, 0);
  const href = `${calHref}${title}.ics`;
  const ics = [
    'BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//e2e//EN', 'BEGIN:VEVENT',
    `UID:${title}`, `DTSTAMP:${utc(noon)}`, `DTSTART:${utc(noon)}`,
    `DTEND:${utc(new Date(+noon + 30 * 60000))}`, `SUMMARY:${title}`,
    'RRULE:FREQ=DAILY;COUNT=5', 'END:VEVENT', 'END:VCALENDAR', '',
  ].join('\r\n');
  const r = await request.fetch(href, {
    method: 'PUT', headers: { ...bobAuth, 'Content-Type': 'text/calendar; charset=utf-8' }, data: ics,
  });
  expect(r.status(), `PUT ${href}`).toBeLessThan(300);
  return href;
}

// shareReadOnly grants `email` read-only access (no <cs:read-write/>).
async function shareReadOnly(request, calHref, email) {
  const body = `<?xml version="1.0" encoding="utf-8"?>\n<cs:share ${XMLNS}><cs:set>`
    + `<d:href>mailto:${email}</d:href><cs:summary></cs:summary></cs:set></cs:share>`;
  const r = await request.fetch(calHref, {
    method: 'POST', headers: { ...bobAuth, 'Content-Type': 'application/xml; charset=utf-8' }, data: body,
  });
  expect(r.status(), `share ${calHref}`).toBe(200);
}

test.describe('read-only share: deleting from the detail popup', () => {
  let calHref;

  test.afterEach(async ({ request }) => {
    // Deleting the calendar drops the event and the share it granted.
    if (calHref) await request.fetch(calHref, { method: 'DELETE', headers: bobAuth });
    calHref = null;
  });

  test('Delete on a read-only shared event routes to the editor, not a write', async ({ browser, request }) => {
    const name = TAG + Date.now();
    const title = name + '-ev';
    calHref = await mkcalendar(request, name);
    const objHref = await putRepeating(request, calHref, title);
    await shareReadOnly(request, calHref, SHAREE);

    // The share auto-accepts: it's really in user01's home now.
    const home = await request.fetch('/calendars/user01/', {
      method: 'PROPFIND',
      headers: { Authorization: 'Basic ' + Buffer.from('user01:user01').toString('base64'), Depth: '1' },
    });
    expect(await home.text()).toContain(name);

    // Drive the sharee in a clean context.
    const ctx = await browser.newContext();
    try {
      const page = await ctx.newPage();
      await login(page, 'user01', 'user01');

      await page.locator('.chip', { hasText: title }).first().click();
      await expect(page.locator('#ed-del')).toBeVisible();
      await page.click('#ed-del');

      // No scope choice, no confirm — the editor opened and explains why the
      // event can't be changed, with Save disabled.
      await expect(page.getByRole('button', { name: 'This event', exact: true })).toHaveCount(0);
      await expect(page.locator('#ev-error')).toContainText('read-only');
      await expect(page.locator('#ev-save')).toBeDisabled();
    } finally {
      await ctx.close();
    }

    // Nothing was deleted: the object is intact on the server.
    const still = await request.fetch(objHref, { headers: bobAuth });
    expect(still.status()).toBe(200);
    expect(await still.text()).toContain('RRULE:FREQ=DAILY');
  });
});
