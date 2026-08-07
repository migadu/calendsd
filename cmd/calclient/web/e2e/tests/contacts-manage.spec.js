// End-to-end tests for address-book / group management and the reworked import
// (progress + skip-already-present). All artifacts carry a TAG so cleanup only
// touches this spec's data on the shared dev DB.
const { test, expect } = require('@playwright/test');
const { login } = require('./helpers');

const TAG = 'ManageE2E';
const auth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };

async function propfind(request, path) {
  const r = await request.fetch(path, { method: 'PROPFIND', headers: { ...auth, Depth: '1' } });
  if (!r.ok() && r.status() !== 207) return { hrefs: [], body: '' };
  const body = await r.text();
  return { hrefs: [...body.matchAll(/href>([^<]+)</g)].map(m => m[1]), body };
}

// removeTagged deletes tagged vCards and any address book whose displayname or
// path carries the TAG.
async function removeTagged(request) {
  const home = '/addressbooks/bob/';
  const { hrefs: books } = await propfind(request, home);
  for (const book of books.filter(h => h.endsWith('/') && h !== home)) {
    const { hrefs: cards } = await propfind(request, book);
    for (const card of cards.filter(h => h.endsWith('.vcf'))) {
      const cardBody = await (await request.fetch(card, { headers: auth })).text();
      if (cardBody.includes(TAG)) await request.fetch(card, { method: 'DELETE', headers: auth });
    }
    // Only delete a book this suite CREATED (its slug carries the tag). Never key
    // off the listing body: a real book transiently holding a tagged card (e.g.
    // the import test writes into bob's default book) would match and the whole
    // book — plus every unrelated contact in it — would be destroyed.
    if (book.includes(TAG.toLowerCase())) {
      await request.fetch(book, { method: 'DELETE', headers: auth });
    }
  }
}

async function gotoContacts(page) {
  await page.evaluate(() => { location.hash = 'contacts'; });
  await page.locator('#ct-add').waitFor({ state: 'visible' });
}

test.describe('contacts management', () => {
  test.beforeEach(async ({ request }) => { await removeTagged(request); });
  test.afterEach(async ({ request }) => { await removeTagged(request); });

  test('the contact editor saves on Enter and discards on Escape', async ({ page }) => {
    await login(page);
    await gotoContacts(page);
    const name = `${TAG} EnterKey`;

    // Enter in a text field saves, like a form.
    await page.click('#ct-add');
    await page.fill('#cd-first', name);
    await page.locator('#cd-first').press('Enter');
    await expect(page.locator('#cd-save')).toHaveCount(0);
    await expect(page.locator('.contact', { hasText: name })).toBeVisible();

    // Escape closes the editor without creating anything.
    await page.click('#ct-add');
    await page.fill('#cd-first', `${TAG} Discarded`);
    await page.locator('#cd-first').press('Escape');
    await expect(page.locator('#cd-save')).toHaveCount(0);
    await expect(page.locator('.contact', { hasText: 'Discarded' })).toHaveCount(0);
  });

  test('create and delete an address book from the sidebar', async ({ page }) => {
    await login(page);
    await gotoContacts(page);
    const name = `${TAG} Book`;

    await page.getByRole('button', { name: 'Add Address Books' }).click();
    await page.fill('.dialog input', name);
    await page.click('.dialog button.primary');

    const nav = page.locator('.ct-nav', { hasText: name });
    await expect(nav).toBeVisible();

    // Delete it (hover the row to reveal the × control).
    const row = page.locator('.ct-nav-row', { hasText: name });
    await row.hover();
    await row.locator('.ct-nav-del').click();
    await page.locator('[data-ok]').click();               // danger confirm
    await expect(page.locator('.ct-nav', { hasText: name })).toHaveCount(0);
  });

  test('rename an address book from the sidebar', async ({ page }) => {
    await login(page);
    await gotoContacts(page);
    const name = `${TAG} Book`;
    const renamed = `${TAG} Renamed`;

    await page.getByRole('button', { name: 'Add Address Books' }).click();
    await page.fill('.dialog input', name);
    await page.click('.dialog button.primary');
    await expect(page.locator('.ct-nav', { hasText: name })).toBeVisible();

    // Rename via the row's ✎ (revealed on hover).
    const row = page.locator('.ct-nav-row', { hasText: name });
    await row.hover();
    await row.getByRole('button', { name: `Rename ${name}` }).click();
    await page.fill('.dialog input', renamed);
    await page.click('.dialog button.primary');

    await expect(page.locator('.ct-nav', { hasText: renamed })).toBeVisible();
    await expect(page.locator('.ct-nav', { hasText: name })).toHaveCount(0);
  });

  test('create and delete a group from the sidebar', async ({ page }) => {
    await login(page);
    await gotoContacts(page);
    const name = `${TAG} Team`;

    await page.getByRole('button', { name: 'Add Groups' }).click();
    await page.fill('.dialog input', name);
    await page.click('.dialog button.primary');

    const nav = page.locator('.ct-group', { hasText: name });
    await expect(nav).toBeVisible();

    const row = page.locator('.ct-nav-row', { hasText: name });
    await row.hover();
    await row.locator('.ct-nav-del').click();
    await page.locator('[data-ok]').click();
    await expect(page.locator('.ct-group', { hasText: name })).toHaveCount(0);
  });

  test('rename a group from the sidebar', async ({ page }) => {
    await login(page);
    await gotoContacts(page);
    const name = `${TAG} Team`;
    const renamed = `${TAG} Squad`;

    await page.getByRole('button', { name: 'Add Groups' }).click();
    await page.fill('.dialog input', name);
    await page.click('.dialog button.primary');
    await expect(page.locator('.ct-group', { hasText: name })).toBeVisible();

    const row = page.locator('.ct-nav-row', { hasText: name });
    await row.hover();
    await row.getByRole('button', { name: `Rename ${name}` }).click();
    await page.fill('.dialog input', renamed);
    await page.click('.dialog button.primary');

    await expect(page.locator('.ct-group', { hasText: renamed })).toBeVisible();
    await expect(page.locator('.ct-group', { hasText: name })).toHaveCount(0);
  });

  test('re-importing the same .vcf skips existing contacts', async ({ page }) => {
    await login(page);
    await page.evaluate(() => { location.hash = 'settings'; });
    await page.locator('#st-import').waitFor();

    const vcf = [
      'BEGIN:VCARD', 'VERSION:3.0', `UID:${TAG}-1`, `FN:${TAG} One`, 'EMAIL:one@example.org', 'END:VCARD',
      'BEGIN:VCARD', 'VERSION:3.0', `UID:${TAG}-2`, `FN:${TAG} Two`, 'EMAIL:two@example.org', 'END:VCARD',
    ].join('\r\n') + '\r\n';
    const file = { name: 'import.vcf', mimeType: 'text/vcard', buffer: Buffer.from(vcf) };

    // First import creates both.
    await page.setInputFiles('#st-import', file);
    await expect(page.locator('#st-import-note')).toContainText('Imported 2');

    // Second import of the same file skips both as already present.
    await page.setInputFiles('#st-import', file);
    await expect(page.locator('#st-import-note')).toContainText('skipped 2 already present');
  });

  test('Apple item-grouped emails/phones become editable fields and normalize on save', async ({ page, request }) => {
    // Isolate this in a dedicated tagged book so cleanup only ever removes THIS
    // book — never one of bob's real address books. (removeTagged deletes any
    // book whose path carries the tag.)
    const bookHref = `/addressbooks/bob/${TAG.toLowerCase()}-apple/`;
    const mkcol = await request.fetch(bookHref, {
      method: 'MKCOL', headers: { ...auth, 'Content-Type': 'application/xml; charset=utf-8' },
      data: '<?xml version="1.0" encoding="utf-8"?>' +
        '<d:mkcol xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:set><d:prop>' +
        '<d:resourcetype><d:collection/><card:addressbook/></d:resourcetype>' +
        `<d:displayname>${TAG} Apple</d:displayname></d:prop></d:set></d:mkcol>`,
    });
    expect([201, 405]).toContain(mkcol.status());

    // A card exactly as Contacts.app exports it: value + custom label paired
    // under an item group, no TYPE param. (The filename deliberately omits the
    // tag; the card body carries it via FN so per-card cleanup still catches it.)
    const card = [
      'BEGIN:VCARD', 'VERSION:3.0', 'UID:apple-grouped', `FN:${TAG} Apple`, `N:Apple;${TAG};;;`,
      'item1.EMAIL;type=INTERNET;type=pref:grp.home@example.org',
      'item1.X-ABLabel:_$!<Home>!$_',
      'item2.TEL;type=pref:+1 555 909 0009',
      'item2.X-ABLabel:_$!<Mobile>!$_',
      'END:VCARD',
    ].join('\r\n') + '\r\n';
    const cardHref = bookHref + 'apple-grouped.vcf';
    const put = await request.fetch(cardHref, { method: 'PUT', headers: { ...auth, 'Content-Type': 'text/vcard; charset=utf-8' }, data: card });
    expect(put.status(), `PUT ${cardHref}`).toBeLessThan(300);

    await login(page);
    await gotoContacts(page);
    await page.locator('.contact', { hasText: `${TAG} Apple` }).click();

    // The grouped values show as normal Email/Phone lines in the detail card…
    const detail = page.locator('.detail-body');
    await expect(detail).toContainText('grp.home@example.org');
    await expect(detail).toContainText('555 909 0009');

    // …and as editable rows with the label-derived type in the editor.
    await page.click('#cv-edit');
    await expect(page.locator('#cd-emails .mr-val').first()).toHaveValue('grp.home@example.org');
    await expect(page.locator('#cd-emails .mr-type').first()).toHaveValue('HOME');
    await expect(page.locator('#cd-tels .mr-val').first()).toHaveValue('+1 555 909 0009');
    await expect(page.locator('#cd-tels .mr-type').first()).toHaveValue('CELL');

    // Saving re-emits them as plain TYPE-tagged lines; the item-group + X-ABLABEL
    // are gone (no double-write).
    await page.click('#cd-save');
    await page.locator('#cd-save').waitFor({ state: 'hidden' });
    const saved = await (await request.fetch(cardHref, { headers: auth })).text();
    expect(saved).toMatch(/EMAIL;TYPE=INTERNET;TYPE=HOME:grp\.home@example\.org/);
    expect(saved).toMatch(/TEL;TYPE=CELL:\+1 555 909 0009/);
    expect(saved).not.toMatch(/item1\.|item2\.|X-ABLABEL/i);
  });
});
