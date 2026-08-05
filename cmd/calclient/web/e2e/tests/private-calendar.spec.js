// End-to-end test for the "Private" calendar toggle in the manage dialog. The
// toggle PROPPATCHes schedule-calendar-transp (transparent = private, hidden
// from free-busy; opaque = shared), so the checks read that property back off
// the server and confirm the checkbox reflects the persisted state on reopen.
//
// The calendar is created and torn down per test; its slug carries the TAG so
// cleanup only touches this spec's data on the shared dev DB.
const { test, expect } = require('@playwright/test');
const { login } = require('./helpers');

const TAG = 'PrivateE2E';
const auth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };

// newCalendarSettings creates a fresh calendar via the sidebar and opens its
// manage dialog (the ⚙ gear is revealed on hover); returns the dialog locator.
async function newCalendarSettings(page, name) {
  await page.click('#new-calendar');
  await page.fill('.dialog input', name);
  await page.click('.dialog button.primary');
  const li = page.locator('#cal-list li', { hasText: name });
  await expect(li).toBeVisible();
  await li.hover();
  await li.locator('.gear').click();
  return page.locator('.overlay .dialog');
}

// openSettings reopens the manage dialog for an existing calendar by name.
async function openSettings(page, name) {
  const li = page.locator('#cal-list li', { hasText: name });
  await li.hover();
  await li.locator('.gear').click();
  return page.locator('.overlay .dialog');
}

// transpProp returns the schedule-calendar-transp element text for bob's
// calendar whose displayname matches name (e.g. "<cal:transparent/>").
async function transpProp(request, name) {
  const home = '/calendars/bob/';
  const r = await request.fetch(home, {
    method: 'PROPFIND',
    headers: { ...auth, Depth: '1', 'Content-Type': 'application/xml; charset=utf-8' },
    data: '<?xml version="1.0" encoding="utf-8"?>' +
      '<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">' +
      '<d:prop><d:displayname/><c:schedule-calendar-transp/></d:prop></d:propfind>',
  });
  const body = await r.text();
  // Split into per-response blocks and pick the one carrying our displayname.
  const block = body.split(/<\/?d:response>/i).find(b => b.includes(`>${name}<`));
  return block ?? '';
}

// removeTaggedCalendars deletes any calendar under bob's home whose slug carries
// the TAG, so runs don't accumulate leftovers on the shared dev DB.
async function removeTaggedCalendars(request) {
  const home = '/calendars/bob/';
  const r = await request.fetch(home, { method: 'PROPFIND', headers: { ...auth, Depth: '1' } });
  if (!r.ok() && r.status() !== 207) return;
  const hrefs = [...(await r.text()).matchAll(/href>([^<]+)</g)].map(m => m[1]);
  for (const h of hrefs) {
    if (h !== home && h.endsWith('/') && h.includes(TAG.toLowerCase())) {
      await request.fetch(h, { method: 'DELETE', headers: auth });
    }
  }
}

test.describe('private calendar toggle', () => {
  test.beforeEach(async ({ request }) => { await removeTaggedCalendars(request); });
  test.afterEach(async ({ request }) => { await removeTaggedCalendars(request); });

  test('marking a calendar private sets it transparent; unsetting restores opaque', async ({ page, request }) => {
    await login(page);
    const name = `${TAG} Cal`;

    // A brand-new calendar is opaque (shared): the checkbox starts unchecked.
    const dialog = await newCalendarSettings(page, name);
    const box = dialog.locator('#cm-private');
    await expect(box).not.toBeChecked();

    // Turn on Private and save → the server records it transparent.
    await box.check();
    await dialog.locator('#cm-save').click();
    await expect(dialog).toBeHidden();
    expect(await transpProp(request, name)).toContain('transparent');

    // Reopen: the checkbox reflects the persisted private state.
    const dialog2 = await openSettings(page, name);
    await expect(dialog2.locator('#cm-private')).toBeChecked();

    // Turn it back off → the calendar is opaque again (not transparent).
    await dialog2.locator('#cm-private').uncheck();
    await dialog2.locator('#cm-save').click();
    await expect(dialog2).toBeHidden();
    expect(await transpProp(request, name)).not.toContain('transparent');
  });
});
