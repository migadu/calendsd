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
    const { hrefs: cards, body } = await propfind(request, book);
    for (const card of cards.filter(h => h.endsWith('.vcf'))) {
      const cardBody = await (await request.fetch(card, { headers: auth })).text();
      if (cardBody.includes(TAG)) await request.fetch(card, { method: 'DELETE', headers: auth });
    }
    if (book.includes(TAG.toLowerCase()) || body.includes(TAG)) {
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
});
