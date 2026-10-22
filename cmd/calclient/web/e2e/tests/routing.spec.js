// End-to-end tests for the real-path router (cmd/calclient/main.go SPA fallback
// + web/cal/js/router.js). The grid view, navigated date, and agenda filter live
// in /cal/calendar/<view>/<date>[?q=…]; events are addressable at
// /cal/event/<uid>[/edit]. Everything must survive a reload and a deep load, so
// these drive the real browser against a live calendsd + calclient.
const { test, expect } = require('@playwright/test');
const { login, deleteEventsContaining } = require('./helpers');

const TAG = 'routing-e2e-';   // the afterEach sweep deletes by this substring
const auth = { Authorization: 'Basic ' + Buffer.from('bob:bob').toString('base64') };
const utc = d => d.toISOString().replace(/[-:]/g, '').replace(/\.\d+Z$/, 'Z');
const localDate = d => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
const path = page => new URL(page.url()).pathname;

test.afterEach(async ({ request }) => {
  await deleteEventsContaining(request, TAG);
});

// seed PUTs a plain VEVENT into bob's default calendar; its UID is the title.
async function seed(request, title, when) {
  const href = `/calendars/bob/calendar/${title}.ics`;
  const ics = [
    'BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//e2e//EN', 'BEGIN:VEVENT',
    `UID:${title}`, `DTSTAMP:${utc(when)}`, `DTSTART:${utc(when)}`,
    `DTEND:${utc(new Date(+when + 3600000))}`, `SUMMARY:${title}`,
    'END:VEVENT', 'END:VCALENDAR', '',
  ].join('\r\n');
  const put = await request.fetch(href, {
    method: 'PUT', headers: { ...auth, 'Content-Type': 'text/calendar; charset=utf-8' }, data: ics,
  });
  expect(put.status(), `PUT ${href}`).toBeLessThan(300);
  return href;
}

test('login canonicalizes the URL to a calendar view+date path', async ({ page }) => {
  await login(page);
  await expect.poll(() => path(page)).toMatch(/^\/cal\/calendar\/(day|week|month|agenda)\/\d{4}-\d{2}-\d{2}$/);
});

test('switching the grid view writes the view into the URL', async ({ page }) => {
  await login(page);
  await page.click('#view-seg button[data-view="week"]');
  await expect(page.locator('#view-seg button[data-view="week"]')).toHaveClass(/active/);
  await expect.poll(() => path(page)).toMatch(/^\/cal\/calendar\/week\//);
  await page.click('#view-seg button[data-view="month"]');
  await expect.poll(() => path(page)).toMatch(/^\/cal\/calendar\/month\//);
});

test('a calendar URL restores its view+date on reload', async ({ page }) => {
  await login(page);
  await page.goto('/cal/calendar/week/2026-08-13');
  await expect(page.locator('#view-seg button[data-view="week"]')).toHaveClass(/active/);
  await page.reload();
  await expect(page.locator('#view-seg button[data-view="week"]')).toHaveClass(/active/);
  expect(path(page)).toBe('/cal/calendar/week/2026-08-13');
});

test('agenda search lives in the URL and survives reload', async ({ page, request }) => {
  const title = TAG + 'plane-' + Date.now();
  const noon = new Date(); noon.setHours(12, 0, 0, 0);
  await seed(request, title, noon);
  await login(page);
  await page.goto('/cal/calendar/agenda/' + localDate(noon));
  await page.fill('#agenda-q', 'plane');
  await expect.poll(() => new URL(page.url()).search).toContain('q=plane');
  await expect(page.locator('.agenda .item', { hasText: title })).toBeVisible();
  await page.reload();
  await expect(page.locator('#agenda-q')).toHaveValue('plane');
  await expect(page.locator('.agenda .item', { hasText: title })).toBeVisible();
});

test('an event URL deep-loads its detail overlay; Edit and Done move the URL', async ({ page, request }) => {
  const title = TAG + 'meet-' + Date.now();
  const noon = new Date(); noon.setHours(12, 0, 0, 0);
  await seed(request, title, noon);
  await login(page);

  await page.goto('/cal/event/' + encodeURIComponent(title));
  await expect(page.locator('.overlay .dialog h2', { hasText: title })).toBeVisible();
  expect(path(page)).toBe('/cal/event/' + title);

  await page.click('#ed-edit');
  await expect.poll(() => path(page)).toBe('/cal/event/' + title + '/edit');
  await expect(page.locator('.overlay .dialog h2', { hasText: 'Edit event' })).toBeVisible();

  await page.click('#ev-cancel');
  await expect.poll(() => path(page)).toMatch(/^\/cal\/calendar\//);
  await expect(page.locator('.overlay .dialog')).toHaveCount(0);
});
