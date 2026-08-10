// End-to-end tests for group invites. A: a schedulable group principal (a
// principal carrying a group-member-set) reports calendar-user-type=GROUP and
// the picker tags it (the server expands it on save). B: the user's own
// address-book group is expanded client-side into its members' addresses when
// picked (searchDirectory / guest-field).
const { test, expect } = require('@playwright/test');
const { login } = require('./helpers');

const auth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };
const BOOK = '/addressbooks/bob/contacts/';
const TAG = 'grp-e2e';

const vcard = (uid, lines) => ['BEGIN:VCARD', 'VERSION:3.0', `UID:${uid}`, ...lines, 'END:VCARD', ''].join('\r\n');

async function put(request, href, body) {
  const r = await request.fetch(href, {
    method: 'PUT', headers: { ...auth, 'Content-Type': 'text/vcard; charset=utf-8' }, data: body,
  });
  expect(r.status(), `PUT ${href}`).toBeLessThan(300);
}

test('a schedulable group principal is tagged in the picker (A)', async ({ page }) => {
  await login(page);
  await page.click('#new-event');
  await page.fill('#ev-guest', 'group 02');       // group02 has members → GROUP
  const sug = page.locator('#ev-suggest button', { hasText: 'Group 02' }).first();
  await expect(sug).toBeVisible();
  await expect(sug.locator('.type-tag')).toHaveText('group');
});

test('an address-book group expands into its members (B)', async ({ page, request }) => {
  const uidA = `${TAG}-a-${Date.now()}`, uidB = `${TAG}-b-${Date.now()}`, uidG = `${TAG}-g-${Date.now()}`;
  const emailA = `${uidA}@example.org`, emailB = `${uidB}@example.org`;
  const hrefs = [BOOK + uidA + '.vcf', BOOK + uidB + '.vcf', BOOK + uidG + '.vcf'];
  await put(request, hrefs[0], vcard(uidA, [`FN:${TAG} Alice`, `EMAIL:${emailA}`]));
  await put(request, hrefs[1], vcard(uidB, [`FN:${TAG} Ben`, `EMAIL:${emailB}`]));
  await put(request, hrefs[2], vcard(uidG, [`FN:${TAG} Team`, 'X-ADDRESSBOOKSERVER-KIND:group',
    `X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:${uidA}`, `X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:${uidB}`]));

  try {
    await login(page);
    await page.waitForTimeout(2000);              // let the contacts load into the store
    await page.click('#new-event');

    await page.fill('#ev-guest', `${TAG} Team`);
    const sug = page.locator('#ev-suggest button', { hasText: `${TAG} Team` }).first();
    await expect(sug).toBeVisible();
    await expect(sug.locator('.type-tag')).toHaveText('group');
    await expect(sug.locator('.mail')).toHaveText('2 people');

    await sug.click();
    // both members are now chips
    await expect(page.locator('#ev-chips .guest', { hasText: emailA })).toBeVisible();
    await expect(page.locator('#ev-chips .guest', { hasText: emailB })).toBeVisible();
  } finally {
    for (const h of hrefs) await request.fetch(h, { method: 'DELETE', headers: auth });
  }
});
