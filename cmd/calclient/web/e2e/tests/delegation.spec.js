// End-to-end tests for calendar delegation (calendar-proxy). Part A grants and
// revokes a delegate through the Settings UI and checks the group-member-set on
// the server; Part B signs in as the delegate and expects the delegator's
// calendar in the sidebar (store/proxy.js + calendars.loadDelegatedCalendars).
const { test, expect } = require('@playwright/test');
const { login } = require('./helpers');

const bobAuth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };
const XMLNS = 'xmlns:d="DAV:"';

// clearProxy empties both of bob's proxy member sets so runs don't accumulate.
async function clearProxy(request) {
  const body = `<?xml version="1.0" encoding="utf-8"?>\n<d:propertyupdate ${XMLNS}><d:set><d:prop><d:group-member-set/></d:prop></d:set></d:propertyupdate>`;
  for (const access of ['write', 'read']) {
    await request.fetch(`/principals/bob/calendar-proxy-${access}/`, {
      method: 'PROPPATCH', headers: { ...bobAuth, 'Content-Type': 'application/xml; charset=utf-8' }, data: body,
    });
  }
}

async function proxyMembers(request, access) {
  const r = await request.fetch(`/principals/bob/calendar-proxy-${access}/`, {
    method: 'PROPFIND', headers: { ...bobAuth, Depth: '0', 'Content-Type': 'application/xml; charset=utf-8' },
    data: `<?xml version="1.0"?><d:propfind ${XMLNS}><d:prop><d:group-member-set/></d:prop></d:propfind>`,
  });
  return r.text();
}

test.beforeEach(async ({ request }) => { await clearProxy(request); });
test.afterEach(async ({ request }) => { await clearProxy(request); });

test('grant and revoke a delegate through Settings', async ({ page, request }) => {
  await login(page);
  await page.evaluate(() => { history.pushState({}, '', '/cal/settings'); dispatchEvent(new PopStateEvent('popstate')); });
  await page.locator('#st-del-email').waitFor({ state: 'visible' });

  await page.fill('#st-del-email', 'user01@example.org');
  await page.selectOption('#st-del-access', 'write');
  await page.click('#st-del-add');

  const row = page.locator('#st-delegates .sharee');
  await expect(row).toContainText('can edit');
  expect(await proxyMembers(request, 'write')).toContain('/principals/user01/');

  await row.locator('[data-remove]').click();
  await page.locator('.dialog [data-ok]').click();   // confirm the revoke
  await expect(page.locator('#st-delegates')).toContainText('No one yet');
  expect(await proxyMembers(request, 'write')).not.toContain('/principals/user01/');
});

test('an ambiguous email match is refused, not granted to a guess', async ({ page, request }) => {
  await login(page);
  await page.evaluate(() => { history.pushState({}, '', '/cal/settings'); dispatchEvent(new PopStateEvent('popstate')); });
  await page.locator('#st-del-email').waitFor({ state: 'visible' });

  // "user0" is a substring of user01…user09; since the principal's exact email
  // can't be confirmed, resolvePrincipal must refuse rather than pick one.
  await page.fill('#st-del-email', 'user0');
  await page.click('#st-del-add');
  await expect(page.locator('#st-del-error')).toContainText('No calendar user found');
  await expect(page.locator('#st-delegates .sharee')).toHaveCount(0);
  expect(await proxyMembers(request, 'write')).not.toContain('/principals/user0');
});

test('the Add-delegate button is disabled until the field is filled', async ({ page }) => {
  await login(page);
  await page.evaluate(() => { history.pushState({}, '', '/cal/settings'); dispatchEvent(new PopStateEvent('popstate')); });
  await page.locator('#st-del-email').waitFor({ state: 'visible' });

  const add = page.locator('#st-del-add');
  await expect(add).toBeDisabled();               // empty → can't submit
  await page.fill('#st-del-email', 'user01');     // a bare username is valid here
  await expect(add).toBeEnabled();
  await page.fill('#st-del-email', '');
  await expect(add).toBeDisabled();               // cleared → disabled again
});

test('a delegated calendar appears in the delegate’s sidebar', async ({ browser, request }) => {
  // Grant user01 write access to bob's calendars over CalDAV.
  const grant = `<?xml version="1.0" encoding="utf-8"?>\n<d:propertyupdate ${XMLNS}><d:set><d:prop>` +
    `<d:group-member-set><d:href>/principals/user01/</d:href></d:group-member-set></d:prop></d:set></d:propertyupdate>`;
  const r = await request.fetch('/principals/bob/calendar-proxy-write/', {
    method: 'PROPPATCH', headers: { ...bobAuth, 'Content-Type': 'application/xml; charset=utf-8' }, data: grant,
  });
  expect(r.status(), 'grant proxy').toBeLessThan(300);

  const ctx = await browser.newContext();
  try {
    const page = await ctx.newPage();
    await login(page, 'user01', 'user01');
    // bob's calendars show in user01's sidebar, each tagged with the delegator
    // (the principal segment, "bob").
    await expect(page.locator('#cal-list .type-tag', { hasText: 'bob' }).first()).toBeVisible({ timeout: 15_000 });
  } finally {
    await ctx.close();
  }
});
