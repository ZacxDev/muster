import { test, expect } from '../lib/fixtures';
import { password } from '../lib/env';

// The share target, as Android fires it: GET /share with the link inside
// `text` and `url` empty.
const shared = '/share?title=Shared%20title&text=see%20https%3A%2F%2Fex.example%2Fa&url=';

test('a share opens a pre-filled composer, creates nothing by itself, and Save makes the task', async ({ page, api }) => {
  const before = (await api.tasks()).length;
  await page.goto(shared);

  const body = page.locator('#task-modal textarea[name="body"]');
  await expect(body).toBeVisible();
  await expect(body).toHaveValue('Shared title\nsee https://ex.example/a');
  // The address bar no longer says /share, so a reload does not reopen it.
  await expect(page).toHaveURL(/\/tasks$/);
  // Navigation alone wrote nothing.
  expect((await api.tasks()).length).toBe(before);

  await page.getByRole('button', { name: 'Save task', exact: true }).click();
  await expect(page.locator('#task-modal')).toBeHidden();
  await expect.poll(async () => (await api.tasks()).length).toBe(before + 1);
  const created = (await api.tasks()).find((t) => t.body.includes('https://ex.example/a'));
  expect(created, 'the created task carries the shared link').toBeTruthy();
});

test('a share made while signed out survives the sign-in and lands pre-filled', async ({ browser }) => {
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] }, viewport: { width: 390, height: 844 } });
  const page = await ctx.newPage();
  await page.goto(shared);
  await expect(page).toHaveURL(/\/login\?next=/);
  expect(decodeURIComponent(new URL(page.url()).searchParams.get('next') ?? '')).toBe(decodeURIComponent(shared));

  await page.fill('#muster-password', password);
  await page.click('button[type=submit]');
  await expect(page.locator('#task-modal textarea[name="body"]')).toHaveValue('Shared title\nsee https://ex.example/a');
  await ctx.close();
});

test('shared markup arrives as text', async ({ page }) => {
  await page.goto('/share?title=' + encodeURIComponent('<img src=x onerror="window.__pwned=1">') + '&text=&url=');
  const body = page.locator('#task-modal textarea[name="body"]');
  await expect(body).toHaveValue('<img src=x onerror="window.__pwned=1">');
  expect(await page.evaluate(() => (window as unknown as { __pwned?: number }).__pwned)).toBeUndefined();
  expect(await page.locator('img[src="x"]').count()).toBe(0);
});
