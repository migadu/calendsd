// End-to-end tests for contact deduplication: the per-contact "possible
// duplicate" affordance + merge, and the Settings sweep. Duplicates are created
// through the real New-contact dialog after first selecting a single address
// book in the sidebar — deduplication is within-book by design, and bob has
// more than one book, so without pinning one the two cards can land apart. The
// merge runs through the real dialogs. Leftovers on the shared dev DB are
// cleaned before and after each test, keyed on a marker in the contacts' names.
const { test, expect } = require('@playwright/test');
const { login } = require('./helpers');

const TAG = 'DedupE2E';
const auth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };

// removeTagged deletes every vCard whose body carries the TAG, across all books.
async function removeTagged(request) {
  const home = '/addressbooks/bob/';
  const propfind = async path => {
    const r = await request.fetch(path, { method: 'PROPFIND', headers: { ...auth, Depth: '1' } });
    if (!r.ok() && r.status() !== 207) return [];
    return [...(await r.text()).matchAll(/href>([^<]+)</g)].map(m => m[1]);
  };
  for (const book of (await propfind(home)).filter(h => h.endsWith('/') && h !== home)) {
    for (const card of (await propfind(book)).filter(h => h.endsWith('.vcf'))) {
      const body = await (await request.fetch(card, { headers: auth })).text();
      if (body.includes(TAG)) await request.fetch(card, { method: 'DELETE', headers: auth });
    }
  }
}

async function gotoContacts(page) {
  await page.evaluate(() => { location.hash = 'contacts'; });
  await page.locator('#ct-add').waitFor({ state: 'visible' });
  // Pin one book so both new contacts share it (index 0 is "All Contacts").
  await page.locator('.ct-side .ct-book').nth(1).click();
}

// createContact drives the New-contact dialog into the currently selected book.
async function createContact(page, { name, email, phone }) {
  await page.click('#ct-add');
  await page.locator('#cd-fn').waitFor({ state: 'visible' });
  await page.fill('#cd-fn', name);
  await page.locator('#cd-emails .mr-val').first().fill(email);
  await page.locator('#cd-tels .mr-val').first().fill(phone);
  await page.click('#cd-save');
  // The dialog closes only after the PUT + reload resolve, serializing creates.
  await page.locator('#cd-fn').waitFor({ state: 'hidden' });
}

test.describe('contact deduplication', () => {
  test.beforeEach(async ({ request }) => { await removeTagged(request); });
  test.afterEach(async ({ request }) => { await removeTagged(request); });

  test('per-contact affordance merges two duplicates into one', async ({ page }) => {
    await login(page);
    await gotoContacts(page);
    const name = `${TAG} Ada`;
    await createContact(page, { name, email: 'ada.dup@example.org', phone: '+1 555 111 0001' });
    await createContact(page, { name, email: 'ada.dup@example.org', phone: '+1 555 222 0002' });
    await expect(page.locator('.contact', { hasText: name })).toHaveCount(2);

    // Opening one card offers to merge its duplicate.
    await page.locator('.contact', { hasText: name }).first().click();
    const banner = page.locator('.dup-banner');
    await expect(banner).toBeVisible();
    await expect(banner).toContainText('Possible duplicate');
    await banner.getByRole('button', { name: /review/i }).click();

    // The preview unions both phones into editable rows under one name.
    const preview = page.locator('.mg-preview');
    const telValues = () => preview.locator('.mr-val').evaluateAll(els => els.map(e => e.value).join('\n'));
    await expect.poll(telValues).toContain('555 111 0001');
    await expect.poll(telValues).toContain('555 222 0002');

    await page.click('#mg-merge');
    await page.locator('[data-ok]').click();               // danger confirm

    // Collapses to a single card carrying both phones.
    await expect(page.locator('.contact', { hasText: name })).toHaveCount(1);
    await page.locator('.contact', { hasText: name }).click();
    const detail = page.locator('.detail-body');
    await expect(detail).toContainText('555 111 0001');
    await expect(detail).toContainText('555 222 0002');
  });

  test('Settings sweep lists a cluster and clears it after merging', async ({ page }) => {
    await login(page);
    await gotoContacts(page);
    const name = `${TAG} Grace`;
    await createContact(page, { name, email: 'grace.dup@example.org', phone: '+1 555 333 0003' });
    await createContact(page, { name, email: 'grace.dup@example.org', phone: '+1 555 444 0004' });

    // Open the sweep from Settings.
    await page.evaluate(() => { location.hash = 'settings'; });
    await page.locator('#st-dedup').waitFor({ state: 'visible' });
    await page.click('#st-dedup');

    const sweep = page.locator('.dd-list');
    await expect(page.locator('#dd-title')).toContainText('Duplicate contacts');
    await expect(sweep.locator('.dd-row', { hasText: name })).toHaveCount(1);

    await sweep.locator('.dd-row', { hasText: name }).getByRole('button', { name: /review/i }).click();
    await page.click('#mg-merge');
    await page.locator('[data-ok]').click();

    // Back in the sweep, this cluster is gone.
    await expect(sweep.locator('.dd-row', { hasText: name })).toHaveCount(0);
  });

  test('removing a phone in the preview drops it from the merged card', async ({ page }) => {
    await login(page);
    await gotoContacts(page);
    const name = `${TAG} Lin`;
    await createContact(page, { name, email: 'lin.dup@example.org', phone: '+1 555 555 0005' });
    await createContact(page, { name, email: 'lin.dup@example.org', phone: '+1 555 666 0006' });

    await page.locator('.contact', { hasText: name }).first().click();
    await page.locator('.dup-banner').getByRole('button', { name: /review/i }).click();

    // Remove the 0006 phone row from the editable phone group before merging.
    const telGroup = page.locator('.mg-multi', { hasText: 'Phone' });
    const rows = telGroup.locator('.multi-row');
    for (let i = 0, n = await rows.count(); i < n; i++) {
      if ((await rows.nth(i).locator('.mr-val').inputValue()).includes('0006')) {
        await rows.nth(i).locator('.mr-del').click();
        break;
      }
    }

    await page.click('#mg-merge');
    await page.locator('[data-ok]').click();

    // The survivor keeps only the phone we left in place.
    await expect(page.locator('.contact', { hasText: name })).toHaveCount(1);
    await page.locator('.contact', { hasText: name }).click();
    const detail = page.locator('.detail-body');
    await expect(detail).toContainText('555 555 0005');
    await expect(detail).not.toContainText('555 666 0006');
  });

  test('Escape closes the merge preview without merging', async ({ page }) => {
    await login(page);
    await gotoContacts(page);
    const name = `${TAG} Otto`;
    await createContact(page, { name, email: 'otto.dup@example.org', phone: '+1 555 777 0007' });
    await createContact(page, { name, email: 'otto.dup@example.org', phone: '+1 555 888 0008' });

    await page.locator('.contact', { hasText: name }).first().click();
    await page.locator('.dup-banner').getByRole('button', { name: /review/i }).click();
    await expect(page.locator('.mg-preview')).toBeVisible();

    await page.keyboard.press('Escape');
    await expect(page.locator('.mg-preview')).toHaveCount(0);
    // Nothing merged: both cards remain.
    await expect(page.locator('.contact', { hasText: name })).toHaveCount(2);
  });
});
