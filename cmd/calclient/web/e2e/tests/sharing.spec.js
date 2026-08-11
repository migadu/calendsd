// End-to-end tests for calendar sharing, owner side: the manage dialog's
// invite form and live roster (store.shareCalendar / loadSharees /
// unshareCalendar against the real server), plus a request-level check that
// the share really lands in (and leaves) the sharee's own calendar home. The
// sharee's notification/accept UI isn't covered — this server auto-accepts
// shares and has no pending invitation inbox — so the invite-reply path is
// unit-tested in tests/sharing.test.js.
//
// The calendar is created and torn down per test; its slug carries the TAG so
// cleanup only touches this spec's data on the shared dev DB.
const { test, expect } = require('@playwright/test');
const { login } = require('./helpers');

const TAG = 'ShareE2E';
const SHAREE = 'user01@example.org';           // a real principal on the dev DB
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

// shareeHomeNames lists the displaynames in the sharee's own calendar home.
// A share clones the owner instance's displayname into the sharee's row, so
// the calendar name shows up here once the (auto-accepted) share exists.
async function shareeHomeNames(request, user = 'user01') {
  const shareeAuth = { Authorization: 'Basic ' + Buffer.from(`${user}:${user}`).toString('base64') };
  const r = await request.fetch(`/calendars/${user}/`, {
    method: 'PROPFIND',
    headers: { ...shareeAuth, Depth: '1', 'Content-Type': 'application/xml; charset=utf-8' },
    data: '<?xml version="1.0" encoding="utf-8"?>' +
      '<d:propfind xmlns:d="DAV:"><d:prop><d:displayname/></d:prop></d:propfind>',
  });
  return r.text();
}

// removeTaggedCalendars deletes any calendar under bob's home whose slug carries
// the TAG (deleting the collection also drops the shares it granted).
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

test.describe('calendar sharing (owner side)', () => {
  test.beforeEach(async ({ request }) => { await removeTaggedCalendars(request); });
  test.afterEach(async ({ request }) => { await removeTaggedCalendars(request); });

  test('share a calendar with another user, then revoke it', async ({ page, request }) => {
    await login(page);
    const name = `${TAG} Cal`;

    // Create a fresh calendar (slug: sharee2e-cal) and open its settings; a
    // brand-new calendar has no sharees yet.
    const dialog = await newCalendarSettings(page, name);
    await expect(dialog.locator('#cm-sharees')).toContainText('Not shared with anyone yet');

    // Invite user01 with edit rights.
    await dialog.locator('#cm-share-email').fill(SHAREE);
    await dialog.locator('#cm-share-rw').check();
    await dialog.locator('#cm-share').click();

    // The roster now lists the sharee, marked "can edit".
    const row = dialog.locator('.sharee', { hasText: SHAREE });
    await expect(row).toBeVisible();
    await expect(row.locator('.lvl')).toContainText('can edit');

    // The server auto-accepts, so the calendar really is in user01's home now.
    expect(await shareeHomeNames(request)).toContain(name);

    // Revoke, and the roster returns to empty…
    await row.locator('[data-remove]').click();
    await page.locator('.dialog [data-ok]').click();   // confirm stop-sharing
    await expect(dialog.locator('.sharee', { hasText: SHAREE })).toHaveCount(0);
    await expect(dialog.locator('#cm-sharees')).toContainText('Not shared with anyone yet');
    // …and the calendar is gone from user01's home again.
    expect(await shareeHomeNames(request)).not.toContain(name);
  });

  test('a read-only invite is listed as read-only', async ({ page }) => {
    await login(page);
    const name = `${TAG} RO`;

    const dialog = await newCalendarSettings(page, name);
    await dialog.locator('#cm-share-email').fill(SHAREE);
    // leave "Can edit" unchecked → read-only share; Enter in the email field
    // runs the Share action (not the dialog's Save)
    await dialog.locator('#cm-share-email').press('Enter');

    const row = dialog.locator('.sharee', { hasText: SHAREE });
    await expect(row).toBeVisible();
    await expect(row.locator('.lvl')).toContainText('read-only');
  });

  test('the Share button is disabled until a valid address is entered', async ({ page }) => {
    await login(page);
    const dialog = await newCalendarSettings(page, `${TAG} Gate`);
    const share = dialog.locator('#cm-share');
    const email = dialog.locator('#cm-share-email');
    await expect(share).toBeDisabled();          // empty field → can't submit
    await email.fill('not-an-email');
    await expect(share).toBeDisabled();          // no @ → still blocked
    await email.fill(SHAREE);
    await expect(share).toBeEnabled();           // valid address → enabled
    await email.fill('');
    await expect(share).toBeDisabled();          // cleared → disabled again
  });
});
