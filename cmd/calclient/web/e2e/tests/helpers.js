const { expect } = require('@playwright/test');

// login signs in and waits for the app chrome to replace the login screen.
async function login(page, user = 'bob', pass = 'bob') {
  await page.goto('/cal/');
  await page.locator('#login-user').waitFor({ state: 'visible' });
  await page.fill('#login-user', user);
  await page.fill('#login-pass', pass);
  await page.click('#login-form button');
  await expect(page.locator('#login')).toBeHidden();
}

// deleteEventsContaining removes every calendar object (and inbox item) of
// `user` whose body contains `needle`. Specs that create events on the shared
// dev DB call this from afterEach with their unique name prefix, so leftovers
// — including those from earlier crashed runs — can't accumulate (a month
// cell renders only the first few chips, so pile-up eventually hides the
// event a spec just created and its locator times out).
async function deleteEventsContaining(request, needle, user = 'bob', pass = 'bob') {
  const auth = { Authorization: 'Basic ' + Buffer.from(`${user}:${pass}`).toString('base64') };
  const propfind = async path => {
    const r = await request.fetch(path, { method: 'PROPFIND', headers: { ...auth, Depth: '1' } });
    if (!r.ok() && r.status() !== 207) return [];   // e.g. the notifications 500
    return [...(await r.text()).matchAll(/href>([^<]+)</g)].map(m => m[1]);
  };
  const home = `/calendars/${user}/`;
  const collections = (await propfind(home)).filter(h => h.endsWith('/') && h !== home);
  for (const col of collections) {
    for (const obj of (await propfind(col)).filter(h => h.endsWith('.ics'))) {
      const body = await (await request.fetch(obj, { headers: auth })).text();
      if (body.includes(needle)) await request.fetch(obj, { method: 'DELETE', headers: auth });
    }
  }
}

// setPrefs flips settings via the real Settings UI and waits out the debounced
// server save, then returns to the calendar.
async function setPrefs(page, { hour12, tz } = {}) {
  await page.evaluate(() => { location.hash = 'settings'; });
  await page.locator('#st-hour12').waitFor({ state: 'visible' });
  if (hour12 !== undefined) await page.selectOption('#st-hour12', hour12 ? '1' : '0');
  if (tz !== undefined) await page.selectOption('#st-tz', tz);
  await page.waitForTimeout(1100); // debounced prefs PROPPATCH
  await page.evaluate(() => { location.hash = 'calendar'; });
}

module.exports = { login, setPrefs, deleteEventsContaining };
