// End-to-end test for the richer repeat builder: INTERVAL ("every N weeks")
// and weekly BYDAY (weekday toggles) round-trip into the stored RRULE
// (event-dialog.js + ical.buildRRule). The rule-string math is unit-tested in
// tests/ical.test.js.
const { test, expect } = require('@playwright/test');
const { login, deleteEventsContaining } = require('./helpers');

const TAG = 'rrule-e2e-';
const auth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };

test.afterEach(async ({ request }) => { await deleteEventsContaining(request, TAG); });

// storedRRule finds the object bob just saved (by title, across all his
// calendars) and returns its RRULE line.
async function storedRRule(request, title) {
  const home = '/calendars/bob/';
  const propfind = async p => {
    const r = await request.fetch(p, { method: 'PROPFIND', headers: { ...auth, Depth: '1' } });
    return [...(await r.text()).matchAll(/href>([^<]+)</g)].map(m => m[1]);
  };
  const cols = (await propfind(home)).filter(h => h.endsWith('/') && h !== home);
  for (const col of cols) {
    for (const h of (await propfind(col)).filter(x => x.endsWith('.ics'))) {
      const body = await (await request.fetch(h, { headers: auth })).text();
      if (body.includes(title)) return body.split(/\r?\n/).find(l => l.startsWith('RRULE:')) || '';
    }
  }
  return '';
}

test('every-2-weeks on selected weekdays writes INTERVAL and BYDAY', async ({ page, request }) => {
  const title = TAG + Date.now();
  await login(page);
  await page.click('#new-event');
  await page.fill('#ev-title', title);
  await page.selectOption('#ev-freq', 'WEEKLY');

  await expect(page.locator('#ev-interval-wrap')).toBeVisible();
  await expect(page.locator('#ev-interval-unit')).toHaveText('weeks');
  await page.fill('#ev-interval', '2');
  await page.locator('#ev-byday .wd[data-wd="MO"]').click();
  await page.locator('#ev-byday .wd[data-wd="WE"]').click();

  await page.click('#ev-save');
  await expect(page.locator('#ev-title')).toHaveCount(0);

  const rrule = await storedRRule(request, title);
  expect(rrule).toContain('INTERVAL=2');
  expect(rrule).toMatch(/BYDAY=(MO,WE|WE,MO)/);
});
