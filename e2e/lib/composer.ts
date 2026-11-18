import { Locator } from '@playwright/test';
import { expect } from './fixtures';

// THE RENDERED VIEW'S BOX STARTS COLLAPSED ON A TOUCH SCREEN (a coarse
// pointer: internal/ui/composer_reveal.go). Its row is "Type a message"
// (#mv-text) beside the mic; the box (#mv-input) is in the DOM but hidden
// until that button — or any writer of the box — reveals it.
//
// revealBox opens it the way a person does, by tapping "Type a message", and
// leaves it focused. Where the box is already shown (a fine pointer, any
// Terminal view, a box already revealed) it does nothing but check that it
// is — so a spec can call it on any composer and still assert what it did.
// scope is a page, a frame or a frame locator (the view swap's framed view).
export async function revealBox(page: { locator(selector: string): Locator }): Promise<void> {
  const box = page.locator('#mv-input');
  await expect(box, 'the composer is drawn').toHaveCount(1);
  const btn = page.locator('#mv-text');
  if ((await btn.count()) > 0 && (await btn.isVisible())) {
    await expect(box, 'collapsed behind “Type a message”').toBeHidden();
    await btn.click();
    await expect(btn).toHaveAttribute('aria-expanded', 'true');
    await expect(box).toBeFocused();
  }
  await expect(box).toBeVisible();
}
