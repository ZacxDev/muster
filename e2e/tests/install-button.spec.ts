import { test, expect } from '../lib/fixtures';

// The real install prompt cannot be produced in automation (it needs user
// engagement the harness cannot fake), so the browser's event is synthesised
// with a stubbed prompt() — what is under test is OUR handling of it.
const fire = `(() => {
  const e = new Event('beforeinstallprompt', { cancelable: true });
  window.__prompted = 0;
  e.prompt = () => { window.__prompted++; return Promise.resolve(); };
  e.userChoice = Promise.resolve({ outcome: 'accepted', platform: 'web' });
  window.dispatchEvent(e);
  return e.defaultPrevented;
})()`;

test('the install button appears on beforeinstallprompt and prompts once', async ({ page }) => {
  await page.goto('/tasks');
  await page.click('#sidebar-open');
  const btn = page.locator('#sidebar [data-install-app]');
  await expect(btn).toBeHidden();
  expect(await page.evaluate(fire), 'the default mini-infobar is suppressed').toBe(true);
  await expect(btn).toBeVisible();
  await btn.click();
  expect(await page.evaluate(() => (window as unknown as { __prompted: number }).__prompted)).toBe(1);
  await expect(btn).toBeHidden();
});

test('appinstalled hides it', async ({ page }) => {
  await page.goto('/tasks');
  await page.click('#sidebar-open');
  await page.evaluate(fire);
  const btn = page.locator('#sidebar [data-install-app]');
  await expect(btn).toBeVisible();
  await page.evaluate(() => window.dispatchEvent(new Event('appinstalled')));
  await expect(btn).toBeHidden();
});

test('never offered inside the installed app', async ({ page }) => {
  await page.addInitScript(() => {
    const orig = window.matchMedia.bind(window);
    window.matchMedia = (q: string) =>
      q.includes('display-mode: standalone') ? ({ matches: true, media: q, addEventListener() {}, removeEventListener() {} } as unknown as MediaQueryList) : orig(q);
  });
  await page.goto('/tasks');
  await page.click('#sidebar-open');
  await page.evaluate(fire);
  await page.waitForTimeout(300);
  await expect(page.locator('#sidebar [data-install-app]')).toBeHidden();
});
