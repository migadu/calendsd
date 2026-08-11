// End-to-end tests for calendar ordering (calendar-order, drag-reorder) and
// per-user color on a shared calendar (per_user_props). The e2e server runs
// with -per-user-props so a sharee's color is stored per user.
const { test, expect } = require('@playwright/test');
const { login } = require('./helpers');

const TAG = 'ordcol';
const bobAuth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };
const XMLNS = 'xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:a="http://apple.com/ns/ical/" xmlns:cs="http://calendarserver.org/ns/"';

async function mkcal(request, name) {
  const href = `/calendars/bob/${name}/`;
  const body = `<?xml version="1.0"?><c:mkcalendar ${XMLNS}><d:set><d:prop><d:displayname>${name}</d:displayname>` +
    `<c:supported-calendar-component-set><c:comp name="VEVENT"/></c:supported-calendar-component-set></d:prop></d:set></c:mkcalendar>`;
  const r = await request.fetch(href, { method: 'MKCALENDAR', headers: { ...bobAuth, 'Content-Type': 'application/xml' }, data: body });
  expect(r.status(), `MKCALENDAR ${href}`).toBe(201);
  return href;
}
const del = (request, href, auth = bobAuth) => request.fetch(href, { method: 'DELETE', headers: auth });

async function colorOf(request, href, auth) {
  const r = await request.fetch(href, {
    method: 'PROPFIND', headers: { ...auth, Depth: '0', 'Content-Type': 'application/xml' },
    data: `<?xml version="1.0"?><d:propfind ${XMLNS}><d:prop><a:calendar-color/></d:prop></d:propfind>`,
  });
  return (await r.text()).match(/calendar-color[^>]*>([^<]+)/)?.[1] || '';
}

test('reordering persists calendar-order and re-sorts the sidebar', async ({ page, request }) => {
  const a = await mkcal(request, `${TAG}-aaa-${Date.now()}`);
  const b = await mkcal(request, `${TAG}-zzz-${Date.now()}`);
  try {
    await login(page);
    await expect(page.locator(`#cal-list li[data-href="${a}"]`)).toBeVisible();
    await expect(page.locator(`#cal-list li[data-href="${b}"]`)).toBeVisible();

    // Drive the persistence path the drag handler invokes (Playwright can't fire
    // native HTML5 drag events): put zzz before aaa.
    await page.evaluate(async ([b, a]) => {
      const s = await import('/shared/store.js');
      const rest = s.state.calendars.map(c => c.href).filter(h => h !== a && h !== b);
      await s.reorderCalendars([b, a, ...rest]);
    }, [b, a]);

    // sidebar re-sorts, and the order persists across a reload
    await expect(page.locator('#cal-list li').first()).toHaveAttribute('data-href', b);
    await page.reload();
    await expect(page.locator(`#cal-list li[data-href="${b}"]`)).toBeVisible();
    const idx = await page.evaluate(() => [...document.querySelectorAll('#cal-list li')].map(li => li.dataset.href));
    expect(idx.indexOf(b)).toBeLessThan(idx.indexOf(a));

    // and calendar-order is stored on the server
    const r = await request.fetch(b, {
      method: 'PROPFIND', headers: { ...bobAuth, Depth: '0', 'Content-Type': 'application/xml' },
      data: `<?xml version="1.0"?><d:propfind ${XMLNS}><d:prop><a:calendar-order/></d:prop></d:propfind>`,
    });
    expect(await r.text()).toMatch(/calendar-order[^>]*>1</); // 1-based; zzz moved to the top
  } finally {
    await del(request, a); await del(request, b);
  }
});

// A read-write sharee edits the calendar's color in the manage dialog (its
// color input is enabled, like any writable calendar); with per_user_props that
// write is stored per-sharee and doesn't disturb the owner's color. (Read-only
// sharees can't set a color — the server denies the property write — so calweb
// keeps that input disabled.)
test('a read-write sharee sets their own color without changing the owner’s', async ({ request }) => {
  const name = `${TAG}-share-${Date.now()}`;
  const cal = await mkcal(request, name);
  const user02Auth = { Authorization: 'Basic ' + Buffer.from('user02:user02').toString('base64') };
  await request.fetch(cal, { method: 'PROPPATCH', headers: { ...bobAuth, 'Content-Type': 'application/xml' },
    data: `<?xml version="1.0"?><d:propertyupdate ${XMLNS}><d:set><d:prop><a:calendar-color>#112233ff</a:calendar-color></d:prop></d:set></d:propertyupdate>` });
  await request.fetch(cal, { method: 'POST', headers: { ...bobAuth, 'Content-Type': 'application/xml' },
    data: `<?xml version="1.0"?><cs:share ${XMLNS}><cs:set><d:href>mailto:user02@example.org</d:href><cs:summary></cs:summary><cs:read-write/></cs:set></cs:share>` });
  try {
    // The auto-accepted share has a server-assigned href in user02's home.
    const home = await request.fetch('/calendars/user02/', {
      method: 'PROPFIND', headers: { ...user02Auth, Depth: '1', 'Content-Type': 'application/xml' },
      data: `<?xml version="1.0"?><d:propfind ${XMLNS}><d:prop><d:displayname/></d:prop></d:propfind>`,
    });
    const body = await home.text();
    const shareHref = [...body.matchAll(/<d:response>[\s\S]*?<d:href>([^<]+)<\/d:href>[\s\S]*?<d:displayname>([^<]*)<\/d:displayname>/g)]
      .find(m => m[2] === name)?.[1];
    expect(shareHref, 'shared calendar in user02 home').toBeTruthy();

    const patch = await request.fetch(shareHref, {
      method: 'PROPPATCH', headers: { ...user02Auth, 'Content-Type': 'application/xml' },
      data: `<?xml version="1.0"?><d:propertyupdate ${XMLNS}><d:set><d:prop><a:calendar-color>#aa0000ff</a:calendar-color></d:prop></d:set></d:propertyupdate>`,
    });
    expect(patch.status(), 'sharee color proppatch').toBe(207);
    expect((await colorOf(request, shareHref, user02Auth)).toLowerCase()).toContain('aa0000'); // sharee's own
    expect((await colorOf(request, cal, bobAuth)).toLowerCase()).toContain('112233');           // owner unchanged
  } finally {
    await del(request, cal);
  }
});
