import { test, expect, effectiveThemeColorJS } from '../lib/fixtures';
import { openMore } from '../lib/nav';

const DARK = '#110F1C';

// DARK IS THE ONLY THEME. The sidebar's light-theme toggle is gone, and the
// head script forces dark and deletes the opt-in the toggle used to store
// (`muster-theme` = "light"), so a device that had chosen light loads dark.
//
// Both viewports explicitly: the toggle lived in the sidebar, which is a
// drawer on a phone and a persistent rail on a desktop.
const VIEWPORTS = [
  { name: 'phone', viewport: { width: 390, height: 844 } },
  { name: 'desktop', viewport: { width: 1280, height: 800 } },
];

test('every document tints the status bar with the manifest colour by default', async ({ page, request }) => {
  const manifest = await (await request.get('/manifest.webmanifest')).json();
  expect(manifest.theme_color).toBe(DARK);
  for (const p of ['/tasks', '/agents', '/tasks/1', '/tasks/999999', '/agents/docs-sweeper']) {
    await page.goto(p);
    expect(await page.evaluate(effectiveThemeColorJS), p).toBe(manifest.theme_color);
    expect(await page.evaluate(() => document.documentElement.classList.contains('dark')), p).toBe(true);
  }
});

for (const vp of VIEWPORTS) {
  test.describe(`at ${vp.name} width`, () => {
    test.use({ viewport: vp.viewport });

    test('a device with a stored light theme loads dark, and the choice is cleared', async ({ page }) => {
      await page.goto('/tasks');
      const darkBg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
      await page.evaluate(() => localStorage.setItem('muster-theme', 'light'));

      await page.reload();
      expect(await page.evaluate(() => document.documentElement.classList.contains('dark')), 'html is dark').toBe(true);
      expect(await page.evaluate(effectiveThemeColorJS), 'the status bar is dark').toBe(DARK);
      expect(await page.evaluate(() => getComputedStyle(document.body).backgroundColor), 'the page paints dark').toBe(darkBg);
      expect(await page.evaluate(() => localStorage.getItem('muster-theme')), 'the stored light choice is removed').toBeNull();
    });

    test('the sidebar has no theme toggle', async ({ page }) => {
      await page.goto('/tasks');
      if (vp.name === 'phone') await page.click('#sidebar-open');
      const nav = page.locator('#sidebar');
      await expect(nav).toBeVisible();
      // The device entries are inside the collapsed More (nav_agents.go
      // navMore), where the toggle used to be: open it, so the probe below
      // looks where a toggle would be drawn.
      await openMore(page);
      // Positive control: the probe sees the sidebar's own entries.
      await expect(nav.locator('#sidebar-account')).toBeVisible();
      await expect(nav.locator('[data-theme-toggle]')).toHaveCount(0);
      await expect(nav.getByText('Light theme')).toHaveCount(0);
    });
  });
}

test('the login page loads dark over a stored light choice too', async ({ browser }) => {
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await page.goto('/login');
  expect(await page.evaluate(effectiveThemeColorJS)).toBe(DARK);
  await page.evaluate(() => localStorage.setItem('muster-theme', 'light'));
  await page.reload();
  expect(await page.evaluate(effectiveThemeColorJS)).toBe(DARK);
  expect(await page.evaluate(() => document.documentElement.classList.contains('dark'))).toBe(true);
  expect(await page.evaluate(() => localStorage.getItem('muster-theme'))).toBeNull();
  await ctx.close();
});
