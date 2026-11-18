import { Page, expect } from '@playwright/test';

// THE SESSION SWITCHER'S VIEW PICK, IN EITHER LAYOUT (internal/ui/switcher.go,
// switcher_dock.go). The layout is decided by ONE media query, (pointer:
// coarse), at any width: a fine pointer gets the view menu (#switcher-views)
// from the bar's view button; a coarse one gets the dock, a modal <dialog>
// whose tabs are the views. pickView picks a view the way a person on that
// device does: the menu and its item (clicked), or the dock, its tab and its
// close button (tapped). Either way the bar shows the view afterwards — the
// dock moves the bar through its musterSwitcherShow hook.
//
// It reads the layout from the page rather than from the test's context
// options, so a context that did not emulate what it claimed (a touch context
// with a fine pointer) is not hidden: the caller's own assertions then see the
// wrong layout.
export const coarse = (page: Page) => page.evaluate(() => matchMedia('(pointer: coarse)').matches);

export async function pickView(page: Page, view: string): Promise<void> {
  const toggle = page.locator('#switcher-toggle');
  if (!(await coarse(page))) {
    const menu = page.locator('#switcher-views');
    await toggle.click();
    await expect(menu).toBeVisible();
    await menu.locator(`[data-switcher-pick="${view}"]`).click();
    await expect(menu).toBeHidden();
    return;
  }
  const dock = page.locator('#switcher-dock');
  await toggle.tap();
  await expect(dock, 'the view button opens the dock in the touch layout').toHaveJSProperty('open', true);
  const tab = dock.locator(`[data-dock-tab="${view}"]`);
  await tab.tap();
  await expect(tab).toHaveAttribute('aria-selected', 'true');
  await dock.locator('[data-dock-close]').tap();
  await expect(dock).toHaveJSProperty('open', false);
  // Gone from the top layer (its close animation has played), so the next
  // tap reaches the page.
  await expect(dock).toBeHidden();
}
