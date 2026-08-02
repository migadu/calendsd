const { test, expect } = require('@playwright/test');
const { login, deleteEventsContaining } = require('./helpers');

// Tasks specs write uniquely-named VTODOs to the shared dev DB; sweep them
// (and strays from crashed runs) so nothing accumulates across runs.
test.afterEach(async ({ request }) => {
  await deleteEventsContaining(request, 'task-e2e-');
});

// openTasks navigates to the Tasks view and, on a dev DB without any
// task-capable calendar, creates one through the empty-state affordance.
async function openTasks(page) {
  await login(page);
  await page.click('#mainnav a[data-nav="tasks"]');
  const quickAdd = page.locator('.task-add #tk-new');
  if (!await quickAdd.isVisible({ timeout: 2000 }).catch(() => false)) {
    await page.click('.task-new-list');
    await page.fill('.overlay form input', 'e2e tasks');
    await page.click('.overlay form button[type="submit"]');
  }
  await expect(quickAdd).toBeVisible();
}

test('quick-add creates a task with a due date in the right section', async ({ page }) => {
  await openTasks(page);
  const title = 'task-e2e-' + Date.now();
  const pad = n => String(n).padStart(2, '0');
  const d = new Date();
  await page.fill('#tk-new', title);
  await page.fill('#tk-due', `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`);
  await page.click('.task-add button[type="submit"]');
  const row = page.locator('.task-row', { hasText: title });
  await expect(row).toBeVisible();
  // due today → the Today section
  await expect(page.locator('.task-section', { hasText: 'Today' })
    .locator('.task-row', { hasText: title })).toBeVisible();
});

test('completing a task moves it behind the completed toggle and persists', async ({ page }) => {
  await openTasks(page);
  const title = 'task-e2e-' + Date.now();
  await page.fill('#tk-new', title);
  await page.click('.task-add button[type="submit"]');
  const row = page.locator('.task-row', { hasText: title });
  await expect(row).toBeVisible();

  await row.locator('input.task-check').check();
  // gone from the open sections, present behind "Show completed"
  await expect(page.locator('.task-row', { hasText: title })).toHaveCount(0);
  await page.click('.task-done-toggle');
  await expect(page.locator('.task-row.done', { hasText: title })).toBeVisible();

  // and the state survives a reload (it is on the server, not in the view)
  await page.reload();
  await page.click('#mainnav a[data-nav="tasks"]');
  await page.click('.task-done-toggle');
  await expect(page.locator('.task-row.done', { hasText: title })).toBeVisible();
});

test('editing a task via the dialog updates title and notes', async ({ page }) => {
  await openTasks(page);
  const title = 'task-e2e-' + Date.now();
  await page.fill('#tk-new', title);
  await page.click('.task-add button[type="submit"]');
  await page.locator('.task-row', { hasText: title }).locator('.task-main').click();

  await expect(page.locator('#tk-title')).toHaveValue(title);
  await page.fill('#tk-title', title + ' edited');
  await page.fill('#tk-notes', 'some detail');
  await page.click('#tk-save');
  const edited = page.locator('.task-row', { hasText: title + ' edited' });
  await expect(edited).toBeVisible();
  await expect(edited.locator('.task-notes')).toHaveText('some detail');
});
