// End-to-end tests for the /cal/calendar?cal=<uri> deep link that share-invite
// emails carry (docs/share-invite-email.md): opened logged out, the target
// survives sign-in, the shared calendar is shown in the sidebar, and the
// invitations panel opens while the share's notification is unanswered. An
// unknown target — or one belonging to another account — says so.
//
// bob shares a fresh calendar with admin at request level (both exist on a
// bare dev DB, no seed needed); the calendar's slug carries the TAG so
// cleanup only touches this spec's data.
const { test, expect } = require('@playwright/test');

const TAG = 'sharelinke2e';
const basic = user => ({ Authorization: 'Basic ' + Buffer.from(`${user}:${user}`).toString('base64') });

async function removeTaggedCalendars(request) {
  const home = '/calendars/bob/';
  const r = await request.fetch(home, { method: 'PROPFIND', headers: { ...basic('bob'), Depth: '1' } });
  if (!r.ok() && r.status() !== 207) return;
  for (const [, h] of (await r.text()).matchAll(/href>([^<]+)</g)) {
    if (h !== home && h.endsWith('/') && h.includes(TAG)) {
      await request.fetch(h, { method: 'DELETE', headers: basic('bob') });
    }
  }
}

// shareToAdmin creates bob's calendar, shares it read-only with admin, and
// returns the segment of admin's copy — what the email's ?cal= carries.
async function shareToAdmin(request, name) {
  const cal = `/calendars/bob/${TAG}-${Date.now()}/`;
  const mk = await request.fetch(cal, {
    method: 'MKCALENDAR', headers: { ...basic('bob'), 'Content-Type': 'application/xml; charset=utf-8' },
    data: '<?xml version="1.0" encoding="utf-8"?><c:mkcalendar xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">' +
      `<d:set><d:prop><d:displayname>${name}</d:displayname></d:prop></d:set></c:mkcalendar>`,
  });
  expect(mk.status(), 'MKCALENDAR').toBe(201);
  const share = await request.fetch(cal, {
    method: 'POST', headers: { ...basic('bob'), 'Content-Type': 'application/xml; charset=utf-8' },
    data: '<?xml version="1.0" encoding="utf-8"?><cs:share xmlns:cs="http://calendarserver.org/ns/" xmlns:d="DAV:">' +
      '<cs:set><d:href>mailto:admin@example.org</d:href></cs:set></cs:share>',
  });
  expect(share.status(), 'share POST').toBe(200);

  const r = await request.fetch('/calendars/admin/', {
    method: 'PROPFIND', headers: { ...basic('admin'), Depth: '1', 'Content-Type': 'application/xml; charset=utf-8' },
    data: '<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop><d:displayname/></d:prop></d:propfind>',
  });
  const body = await r.text();
  for (const resp of body.split(/<d:response>/).slice(1)) {
    if (resp.includes(`>${name}<`)) return resp.match(/href>\/calendars\/admin\/([^/<]+)\/</)[1];
  }
  throw new Error(`shared calendar ${name} not found in admin's home:\n${body}`);
}

async function signIn(page, user) {
  await page.locator('#login-user').waitFor({ state: 'visible' });
  await page.fill('#login-user', user);
  await page.fill('#login-pass', user);
  await page.click('#login-form button');
  await expect(page.locator('#login')).toBeHidden();
}

test.describe('share-invite deep link', () => {
  test.beforeEach(async ({ request }) => { await removeTaggedCalendars(request); });
  test.afterEach(async ({ request }) => { await removeTaggedCalendars(request); });

  test('opens the shared calendar after sign-in, with the invitations panel', async ({ page, request }) => {
    const name = `Share link ${Date.now()}`;
    const uri = await shareToAdmin(request, name);

    await page.goto(`/cal/calendar?cal=${encodeURIComponent(uri)}`);
    await signIn(page, 'admin');

    const row = page.locator('#cal-list li', { hasText: name });
    await expect(row).toBeVisible();
    await expect(row.locator('input[type="checkbox"]')).toBeChecked();
    // The share's notification is still unanswered, so Decline is one click away.
    await expect(page.locator('#invites-panel')).toBeVisible();
    // Canonicalization filled in the view and date but kept the target, so a
    // reload (or signing in as someone else) can retry it.
    const url = new URL(page.url());
    expect(url.pathname).toMatch(/^\/cal\/calendar\/(day|week|month|agenda)\/\d{4}-\d{2}-\d{2}$/);
    expect(url.searchParams.get('cal')).toBe(uri);
  });

  test('another account gets the unavailable message', async ({ page, request }) => {
    const uri = await shareToAdmin(request, `Share link other ${Date.now()}`);
    await page.goto(`/cal/calendar?cal=${encodeURIComponent(uri)}`);
    await signIn(page, 'bob');
    await expect(page.locator('#toast')).toContainText('isn’t available to this account');
    await expect(page.locator('#topbar')).toBeVisible();   // the calendar view stays usable
  });

  test('an unknown calendar gets the unavailable message', async ({ page }) => {
    await page.goto('/cal/calendar?cal=no-such-calendar');
    await signIn(page, 'admin');
    await expect(page.locator('#toast')).toContainText('isn’t available to this account');
  });
});
