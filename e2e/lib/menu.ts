// A control in a header's ⋯ menu (a popover: the Rendered view's mv-menu,
// an agent's agent-actions): the menu is opened by its trigger first when the
// control is not on screen, as a person reaches it.
import { Locator, Page } from '@playwright/test';

export async function menuItem(page: Page, sel: string): Promise<Locator> {
  const item = page.locator(sel).first();
  await item.waitFor({ state: 'attached' });
  if (await item.isVisible()) return item;
  const id = await item.evaluate((el) => el.closest('[popover]')?.id ?? '');
  if (id) await page.locator(`[popovertarget="${id}"]`).first().click();
  return item;
}
