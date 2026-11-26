import { Locator, Page, expect } from '@playwright/test';

// THE CHIEF'S SESSIONS TAB (internal/ui chief_chats.go chiefSessionsTab), on
// either surface: the first tab of the sheet the list icon opens — the chief
// page's header's #chat-index-open → #chat-index, the chief panel's title bar's
// #chief-panel-index-open → #chief-panel-index. It replaced the Sessions drawer,
// so the specs that drove the drawer read the tab through these.

export type ChiefSurface = 'page' | 'panel';

export const sheetID = (s: ChiefSurface) => (s === 'panel' ? 'chief-panel-index' : 'chat-index');
// sheet is the surface's sheet; icon its list icon; tab the Sessions tab's
// panel (its rows, New session, the search and the chips); bar the plain-text
// name of the chat on screen over the transcript.
export const sheet = (page: Page, s: ChiefSurface) => page.locator('#' + sheetID(s));
export const icon = (page: Page, s: ChiefSurface) => page.locator('#' + sheetID(s) + '-open');
export const sessionsTab = (page: Page, s: ChiefSurface) => page.locator(`#${sheetID(s)} [data-chief-chats]`);
export const bar = (page: Page, s: ChiefSurface) =>
  s === 'panel' ? page.locator('#chief-panel [data-chief-chat-strip]') : page.locator('body > [data-chief-strip-row] [data-chief-chat-strip]');

// surfaceOf is the surface whose sheet holds tab (a sessionsTab locator).
async function surfaceOf(tab: Locator): Promise<ChiefSurface> {
  const id = await tab.evaluate((el) => el.closest('[data-chat-index-sheet]')?.id ?? '');
  expect(id, 'the Sessions tab is inside a sheet').not.toBe('');
  return id === 'chief-panel-index' ? 'panel' : 'page';
}

// openSessions opens the sheet that holds tab on its Sessions tab (unless it is
// open there already — after a row's ⋯ action the answer opens it again), the
// way a person does: the list icon, then the tab.
export async function openSessions(tab: Locator, hasTouch?: boolean): Promise<void> {
  const page = tab.page();
  const s = await surfaceOf(tab);
  const press = (l: Locator) => (hasTouch ? l.tap() : l.click());
  const sh = sheet(page, s);
  if (!(await sh.isVisible())) await press(icon(page, s));
  await expect(sh).toBeVisible();
  await expect(icon(page, s)).toHaveAttribute('aria-expanded', 'true');
  const t = sh.locator('[role="tab"][data-tab="sessions"]');
  if ((await t.getAttribute('aria-selected')) !== 'true') await press(t);
  await expect(t).toHaveAttribute('aria-selected', 'true');
  await expect(tab).toBeVisible();
}
