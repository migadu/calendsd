// End-to-end tests for task (VTODO) reminders: a reminder added in the task
// editor round-trips through a real save and is stored as a DISPLAY VALARM
// anchored to the task's DUE (RELATED=END), mirroring the event reminder path
// but on the VTODO side (buildTodo / setMasterAlarms(..., 'VTODO', 'END') /
// readReminders(todo, 'END')).
const { test, expect } = require('@playwright/test');
const { login } = require('./helpers');

const TAG = 'rem-task-e2e-';
const user = 'bob', pass = 'bob';
const auth = { Authorization: 'Basic ' + Buffer.from(`${user}:${pass}`).toString('base64') };

let listHref;   // a dedicated VTODO list per test, torn down in afterEach

// A fresh task list makes the task land somewhere predictable (the quick-add
// picker only appears with >1 writable list) and reduces cleanup to one
// collection DELETE. Created before login so it's present when the app boots.
test.beforeEach(async ({ request }) => {
  const name = TAG + Date.now();
  listHref = `/calendars/${user}/${name}/`;
  const body = `<?xml version="1.0" encoding="utf-8"?>
<C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:set><D:prop>
    <D:displayname>${name}</D:displayname>
    <C:supported-calendar-component-set><C:comp name="VTODO"/></C:supported-calendar-component-set>
  </D:prop></D:set>
</C:mkcalendar>`;
  const mk = await request.fetch(listHref, {
    method: 'MKCALENDAR', headers: { ...auth, 'Content-Type': 'application/xml; charset=utf-8' }, data: body,
  });
  expect(mk.status(), 'MKCALENDAR').toBeLessThan(300);
});

test.afterEach(async ({ request }) => {
  if (listHref) await request.fetch(listHref, { method: 'DELETE', headers: auth });
});

const pad = n => String(n).padStart(2, '0');
const inDays = n => {
  const d = new Date(); d.setDate(d.getDate() + n);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
};

async function gotoTasks(page) {
  await page.evaluate(() => { history.pushState({}, '', '/cal/tasks'); dispatchEvent(new PopStateEvent('popstate')); });
  await page.locator('.task-add, .tasks .empty').first().waitFor({ state: 'visible' });
}

async function openTask(page, title) {
  await page.locator('.task-row', { hasText: title }).locator('.task-main').click();
  await expect(page.locator('#tk-remind-field')).toBeVisible();
}

test('a task reminder round-trips and is stored relative to DUE', async ({ page, request }) => {
  await login(page);
  await gotoTasks(page);

  const title = TAG + 'item-' + Date.now();
  await page.fill('#tk-new', title);
  await page.fill('#tk-due', inDays(3));
  const picker = page.locator('#tk-list');
  if (await picker.count()) await picker.selectOption(listHref);
  await page.click('.task-add button[type="submit"]');

  // add a reminder and save
  await openTask(page, title);
  await page.locator('#tk-remind-field .rem-add').click();
  await page.locator('#tk-remind-field .rem-row').last().locator('.rem-preset').selectOption('-P1D');
  await page.click('#tk-save');
  await expect(page.locator('#tk-title')).toHaveCount(0);

  // reopen: the reminder reads back onto the same preset
  await openTask(page, title);
  const row = page.locator('#tk-remind-field .rem-row');
  await expect(row).toHaveCount(1);
  await expect(row.first().locator('.rem-preset')).toHaveValue('-P1D');
  await page.click('#tk-cancel');

  // on the wire: a DISPLAY alarm anchored to DUE (RELATED=END)
  const propfind = await request.fetch(listHref, { method: 'PROPFIND', headers: { ...auth, Depth: '1' } });
  const hrefs = [...(await propfind.text()).matchAll(/href>([^<]+)</g)]
    .map(m => m[1]).filter(h => h.endsWith('.ics'));
  let found = '';
  for (const h of hrefs) {
    const t = await (await request.fetch(h, { headers: auth })).text();
    if (t.includes(title)) { found = t; break; }
  }
  expect(found, 'stored VTODO').toContain('BEGIN:VALARM');
  expect(found).toContain('ACTION:DISPLAY');
  expect(found).toContain('TRIGGER;RELATED=END:-P1D');
});
