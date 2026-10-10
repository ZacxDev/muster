import { test, expect, effectiveThemeColorJS } from '../lib/fixtures';

const DARK = '#110F1C';
const LIGHT = '#F6F4FB';

test('every document tints the status bar with the manifest colour by default', async ({ page, request }) => {
  const manifest = await (await request.get('/manifest.webmanifest')).json();
  expect(manifest.theme_color).toBe(DARK);
  for (const p of ['/tasks', '/agents', '/tasks/1', '/tasks/999999', '/agents/docs-sweeper']) {
    await page.goto(p);
    expect(await page.evaluate(effectiveThemeColorJS), p).toBe(manifest.theme_color);
    expect(await page.evaluate(() => document.documentElement.classList.contains('dark')), p).toBe(true);
  }
});

test('the light opt-in switches the page AND the status-bar colour, and persists', async ({ page }) => {
  await page.goto('/tasks');
  expect(await page.evaluate(effectiveThemeColorJS)).toBe(DARK);
  const bg = () => page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  const darkBg = await bg();

  await page.click('#sidebar-open');
  const toggle = page.locator('#sidebar [data-theme-toggle]');
  await expect(toggle).toHaveAttribute('aria-pressed', 'false');
  await toggle.click();
  await expect(toggle).toHaveAttribute('aria-pressed', 'true');
  expect(await page.evaluate(() => document.documentElement.classList.contains('dark'))).toBe(false);
  expect(await page.evaluate(effectiveThemeColorJS)).toBe(LIGHT);
  expect(await bg(), 'the page itself changed colour').not.toBe(darkBg);

  // Survives a full reload (stored per device), and is applied before paint.
  await page.reload();
  expect(await page.evaluate(effectiveThemeColorJS)).toBe(LIGHT);
  expect(await page.evaluate(() => document.documentElement.classList.contains('dark'))).toBe(false);

  await page.click('#sidebar-open');
  await page.locator('#sidebar [data-theme-toggle]').click();
  expect(await page.evaluate(effectiveThemeColorJS)).toBe(DARK);
  expect(await bg()).toBe(darkBg);
});

test('the login page follows the stored choice too', async ({ browser }) => {
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await page.goto('/login');
  expect(await page.evaluate(effectiveThemeColorJS)).toBe(DARK);
  await page.evaluate(() => localStorage.setItem('muster-theme', 'light'));
  await page.reload();
  expect(await page.evaluate(effectiveThemeColorJS)).toBe(LIGHT);
  await ctx.close();
});
