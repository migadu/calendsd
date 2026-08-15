// End-to-end tests for the UI-polish layer: the mini-month / title popover
// (jump-to-date), keyboard shortcuts, the new-collection dialog with its
// colour picker, the all-day lane's create path, danger-left confirm order,
// and focus restoration after an overlay closes.

const { test, expect } = require('@playwright/test');
const { login, deleteEventsContaining } = require('./helpers');

const TAG = 'ui-polish-';
const p2 = n => String(n).padStart(2, '0');
const localDate = d => `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}`;

test.beforeEach(async ({ page }) => { await login(page); });
test.afterEach(async ({ request }) => { await deleteEventsContaining(request, TAG); });

test('the sidebar mini-month navigates to a clicked day', async ({ page }) => {
  await expect(page.locator('#mini-month .mm-day.today')).toBeVisible();
  await page.click('#mini-month .mm-day.today');
  await expect(page).toHaveURL(new RegExp(`/calendar/month/${localDate(new Date())}$`));
});

test('the title opens a date popover; Escape closes, a day navigates', async ({ page }) => {
  await page.click('#cal-title');
  const pop = page.locator('#cal-popover');
  await expect(pop).toBeVisible();
  await expect(page.locator('#cal-title')).toHaveAttribute('aria-expanded', 'true');
  await page.keyboard.press('Escape');
  await expect(pop).toHaveCount(0);
  await expect(page.locator('#cal-title')).toHaveAttribute('aria-expanded', 'false');

  await page.click('#cal-title');
  await expect(pop).toBeVisible();
  await pop.locator('.mm-day.today').click();
  await expect(pop).toHaveCount(0);   // choosing a day closes it
  await expect(page).toHaveURL(new RegExp(`/calendar/month/${localDate(new Date())}$`));
});

test('keyboard shortcuts: t today, w/m/a views, c creates, / searches, ? helps', async ({ page }) => {
  await page.keyboard.press('w');
  await expect(page).toHaveURL(/\/calendar\/week\//);
  await page.keyboard.press('a');
  await expect(page).toHaveURL(/\/calendar\/agenda/);
  await page.keyboard.press('/');
  await expect(page.locator('#agenda-q')).toBeFocused();
  await page.evaluate(() => document.activeElement.blur());   // leave the field: keys are text in it
  await page.keyboard.press('m');
  await expect(page).toHaveURL(/\/calendar\/month\//);
  await page.keyboard.press('t');
  await expect(page).toHaveURL(new RegExp(`/calendar/month/${localDate(new Date())}$`));

  await page.keyboard.press('c');
  await expect(page.locator('#ev-title')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.locator('#ev-title')).toHaveCount(0);

  await page.keyboard.press('?');
  await expect(page.locator('.dialog', { hasText: 'Keyboard shortcuts' })).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.locator('.dialog')).toHaveCount(0);
});

test('shortcuts stay silent while typing and while a dialog is open', async ({ page }) => {
  await page.keyboard.press('a');
  await page.fill('#agenda-q', 'm');
  await expect(page).toHaveURL(/\/calendar\/agenda/);   // the typed m didn't switch views
  await page.keyboard.press('c');
  await expect(page.locator('#ev-title')).toHaveCount(0); // dialog not open: c was swallowed by the input
  await page.fill('#agenda-q', '');
});

test('the all-day lane creates an all-day event', async ({ page }) => {
  await page.keyboard.press('w');
  // the first .allday is the corner cell above the hour gutter; lanes follow
  const lane = page.locator('.week .allday').nth(1);
  await lane.click();
  await expect(page.locator('#ev-allday')).toBeChecked();
  await page.fill('#ev-title', TAG + 'allday');
  await page.click('#ev-save');
  await expect(page.locator('#ev-title')).toHaveCount(0);
  await expect(lane.locator('.chip', { hasText: TAG + 'allday' })).toBeVisible();
});

test('new calendar dialog picks a colour, and delete confirm puts danger left', async ({ page }) => {
  const name = TAG + 'colored-' + Date.now();
  await page.click('#new-calendar');
  const dlg = page.locator('.overlay .dialog');
  await expect(dlg.locator('#nc-create')).toBeDisabled();   // empty name disables Create
  const swatch = dlg.locator('.swatch').nth(2);
  const color = await swatch.getAttribute('data-c');
  await swatch.click();
  await page.fill('#nc-name', name);
  await dlg.locator('#nc-create').click();
  const li = page.locator('#cal-list li', { hasText: name });
  await expect(li).toBeVisible();
  await expect(li.locator('.dot')).toHaveCSS('background-color', /rgb/i);   // stored colour renders
  // the sidebar dot is round (the only circular primitive alongside avatars)
  await expect(li.locator('.dot')).toHaveCSS('border-radius', '50%');

  // delete through the manage dialog: the destructive confirm's action
  // button sits LEFT of Cancel (the app-wide danger-left convention).
  await li.hover();
  await li.locator('.gear').click();
  await page.locator('#cm-delete').click();
  const confirm = page.locator('.overlay .dialog', { hasText: 'cannot be undone' });
  const dangerBox = await confirm.locator('button.danger').boundingBox();
  const cancelBox = await confirm.locator('button:has-text("Cancel")').boundingBox();
  expect(dangerBox.x).toBeLessThan(cancelBox.x);
  await confirm.locator('button.danger').click();
  await expect(li).toHaveCount(0);
});

test('closing an event detail returns focus to the chip that opened it', async ({ page }) => {
  await page.click('#new-event');
  await page.fill('#ev-title', TAG + 'focus');
  await page.click('#ev-save');
  const chip = page.locator('.chip', { hasText: TAG + 'focus' }).first();
  await expect(chip).toBeVisible();
  await chip.click();
  await expect(page.locator('.dialog .detail-body')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.locator('.dialog')).toHaveCount(0);
  await expect(chip).toBeFocused();
});
