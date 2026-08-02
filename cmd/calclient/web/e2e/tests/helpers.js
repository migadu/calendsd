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

module.exports = { login, setPrefs };
