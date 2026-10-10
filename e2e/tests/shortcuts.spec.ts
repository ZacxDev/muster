import { test, expect } from '../lib/fixtures';

// Every shortcut the manifest declares resolves, signed in, to the right tab —
// derived from the served manifest, so a new shortcut is covered on arrival.
test('every manifest shortcut opens the right view', async ({ page, request }) => {
  const manifest = await (await request.get('/manifest.webmanifest')).json();
  const shortcuts: { name: string; url: string }[] = manifest.shortcuts;
  expect(shortcuts.length).toBe(3);

  for (const sc of shortcuts) {
    const res = await page.goto(sc.url);
    expect(res?.status(), sc.url).toBe(200);
    const tab = new URL(sc.url, 'http://x').pathname.slice(1);
    await expect(page.locator(`#sidebar a[data-tab="${tab}"]`)).toHaveAttribute('aria-current', 'page');
    const composer = page.locator('#task-modal textarea[name="body"]');
    if (sc.url.includes('new=1')) {
      await expect(composer, `${sc.name} opens the composer`).toBeVisible();
      await expect(composer).toHaveValue('');
      await expect(page).toHaveURL(/\/tasks$/);
    } else {
      await expect(page.locator('#task-modal')).toBeHidden();
    }
  }
});
