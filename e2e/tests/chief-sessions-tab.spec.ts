import path from 'node:path';
import { Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { mkchat } from '../lib/mirrorhost';
import { bar, icon, openSessions, sessionsTab, sheet } from '../lib/chief-sessions';

// THE CHIEF'S SESSIONS, AS THE OPERATOR ASKED FOR THEM (docs/chief.md
// "Primary and side chats"), in a real browser on the shared server:
//
//   1. "Reopening the Chief tab should return it to the Chief session that I
//      was most recently on." — the chief page (bare), its sidebar row, home
//      `/` and the chief panel all open on the chat viewed last, on either
//      surface; an explicit ?session= still wins; an archived one falls back
//      to the primary.
//   2. "make it part of the same button that opens the slide up that shows the
//      focus and the user messages, and just make it another tab … we could
//      probably add like a search filter controls" — the Sessions tab, FIRST in
//      the sheet the list icon opens (the page's header, the panel's title
//      bar): every chat, a search and filter chips, a pick that switches and
//      shuts the sheet, a ⋯ action whose answer opens it again on Sessions, a
//      dialog by keyboard (focus in, Tab kept in, Escape out — but an open ⋯
//      menu's Escape is the menu's, and the sheet's is not the panel's); the
//      chat on screen named in plain text where the Sessions button was, the
//      unread dot on the list icon. On a phone and a desktop.
//   3. "when I navigate back to a Chief session after navigating away, it
//      should restore my scroll position" — per chat, on the page and in the
//      panel; a chat left at its end opens at its end.
//
// 🔴 REGRESSION: tests 1 and 3 move between chats only by ways that exist on
// main too (a URL, the sidebar row, home, a row of the panel's list of chats,
// closing and reopening the panel), so each fails there for its own reason: main opened
// the sidebar row and home on the chat touched last, the bare page and the
// panel on the primary, and every chat at its end. The Sessions tab tests fail
// on main because there is no Sessions tab (the sheet was Your messages and
// What it worked on; the chats were a drawer), and the two-sheets test because
// the panel had no sheet. The message-search tests fail before the fold of the
// panel's thread list into the tab: the tab's search filtered titles only.
//
// The chief is an agent of this spec's own (display name "chief", e2e/mkchat's
// threads mode, the primary elected as the oldest), removed afterwards. Each
// test makes its own, so the server-side memory one test leaves cannot reach
// another.

const sessionOf = (u: string) => new URL(u).searchParams.get('session');

// shot saves a screenshot when MUSTER_E2E_CHIEF_SHOTS names a directory (the
// images a reviewer looks at, as chief-ux.spec.ts's); a generator, never an
// assertion.
async function shot(page: Page, name: string) {
  const dir = process.env.MUSTER_E2E_CHIEF_SHOTS;
  if (dir) await page.screenshot({ path: path.join(dir, name) });
}

// A tall transcript: 40 numbered turns, so the log scrolls on every viewport.
const talk = (tag: string, n: number) =>
  Array.from({ length: n }, (_, i) => ({
    role: i % 2 ? 'assistant' : 'user',
    content: `${tag} line ${i + 1}. ` + 'This is a long enough line to wrap across the chat column more than once. '.repeat(3),
  }));

let seq = 0;
type Chief = { name: string; primary: string; alpha: string; beta: string };

// makeChief creates the chief and reads its chats' ids off its page (opening
// the primary, which is what a never-opened chief opens on).
async function makeChief(page: Page): Promise<Chief> {
  // 🔴 ONE CHIEF AT A TIME: "the chief" is the lowest-id agent carrying the
  // display name, so an earlier test's would still be it.
  for (const n of made.splice(0)) mkchat(n, null);
  const name = `e2e-sessions-chief-${Date.now().toString(36)}-${++seq}`;
  mkchat(name, null);
  mkchat(
    name,
    [
      { title: 'the primary', hoursAgo: 50, messages: talk('primary', 6) },
      { title: 'side alpha', hoursAgo: 30, messages: talk('alpha', 40) },
      { title: 'side beta', hoursAgo: 1, messages: talk('beta', 40) },
    ],
    { MUSTER_E2E_DISPLAY_NAME: 'chief', MUSTER_E2E_THREADS: '1' },
  );
  made.push(name);
  // The ids, from the chief page's Sessions tab: each row links its chat and
  // carries its title. (The bare page opens — and so views — the primary, which
  // is what a never-opened chief opens on anyway.)
  const html = await (await page.request.get('/agents/' + name)).text();
  const rows = Array.from(html.matchAll(/data-chief-chat-row="(\d+)" title="([^"]*)"/g));
  const idOf = (title: string) => {
    const row = rows.find((r) => r[2] === title);
    expect(row, `the Sessions tab names "${title}"`).toBeTruthy();
    return row![1];
  };
  return { name, primary: idOf('the primary'), alpha: idOf('side alpha'), beta: idOf('side beta') };
}
const made: string[] = [];
test.afterAll(() => {
  for (const n of made) mkchat(n, null);
});

async function onPage(page: Page, path: string) {
  await page.goto(path);
  await expect(page.locator('#chat-form')).toBeVisible();
}

// The panel: its toggle is delegated on the document, so a launcher's markup
// clicked by script opens and shuts it.
async function togglePanel(page: Page) {
  await page.evaluate(() => {
    const b = document.createElement('button');
    b.setAttribute('data-chief-panel-toggle', '');
    b.style.cssText = 'position:fixed;left:-9999px;top:0';
    document.body.appendChild(b);
    b.click();
    b.remove();
  });
}
const panel = (page: Page) => page.locator('#chief-panel');
const panelOn = (page: Page) => panel(page).locator('[data-chief-active-session]');
async function openPanel(page: Page) {
  // The panel remembers it was open (its localStorage): a page loaded after
  // it was left open restores it open, and a toggle then would SHUT it.
  await page.waitForLoadState('load');
  if ((await panel(page).getAttribute('data-chief-panel-open')) !== '1') await togglePanel(page);
  await expect(panel(page)).toHaveAttribute('data-chief-panel-open', '1');
  await expect(panel(page).locator('#chat-form')).toBeVisible({ timeout: 15_000 });
}
async function closePanel(page: Page) {
  await togglePanel(page);
  await expect(panel(page)).toHaveAttribute('data-chief-panel-open', '0');
}

// The first message on screen in a chat log, and whether the log is at its end.
async function place(log: Locator) {
  return log.evaluate((el) => {
    const top = el.getBoundingClientRect().top;
    let first = '';
    for (const b of Array.from(el.querySelectorAll('[data-msg-id]'))) {
      if (b.getBoundingClientRect().bottom > top + 1) { first = b.getAttribute('data-msg-id') || ''; break; }
    }
    return { first, atEnd: el.scrollHeight - el.scrollTop - el.clientHeight < 48, scrollable: el.scrollHeight > el.clientHeight + 100 };
  });
}
// scrollToMiddle scrolls a log as a reader would (a scroll event), and lets
// the memory's beat pass.
async function scrollToMiddle(page: Page, log: Locator) {
  await log.evaluate((el) => {
    el.scrollTop = Math.round((el.scrollHeight - el.clientHeight) * 0.4);
    el.dispatchEvent(new Event('scroll'));
  });
  await page.waitForTimeout(500);
  const p = await place(log);
  expect(p.scrollable, 'the transcript is taller than its log').toBe(true);
  expect(p.atEnd, 'scrolled away from the end').toBe(false);
  expect(p.first).not.toBe('');
  return p.first;
}

test.describe('reopening the chief', () => {
  test.use({ viewport: { width: 1280, height: 800 } });

  test('the page, its sidebar row, home and the panel open on the chat viewed last; ?session= wins; archived falls back', async ({ page }) => {
    const c = await makeChief(page);
    // Never opened: the primary.
    await onPage(page, '/agents/' + c.name);
    expect(sessionOf(page.url()), 'a chief nobody has opened opens on its primary').toBe(c.primary);

    // The owner reads side alpha on the page.
    await onPage(page, `/agents/${c.name}?session=${c.alpha}`);
    // …and comes back by each way in that names no chat.
    await page.goto('/tasks');
    await page.locator('a[data-nav-chief]').first().click();
    await page.waitForURL((u) => u.pathname === '/agents/' + c.name);
    await expect(page.locator('#chat-form')).toBeVisible();
    expect(sessionOf(page.url()), 'the sidebar row opens the chat viewed last').toBe(c.alpha);
    await onPage(page, '/');
    expect(sessionOf(page.url()), 'home opens the chat viewed last').toBe(c.alpha);
    await onPage(page, '/agents/' + c.name);
    expect(sessionOf(page.url()), 'the bare page opens the chat viewed last').toBe(c.alpha);

    // The panel opens on it too, though it was viewed on the page.
    await page.goto('/tasks');
    await openPanel(page);
    await expect(panelOn(page), 'the panel opens on the chat viewed on the page').toHaveAttribute('data-chief-active-session', c.alpha);
    // Picked in the panel (its Sessions tab): beta.
    await openSessions(sessionsTab(page, 'panel'));
    await sessionsTab(page, 'panel').locator(`[data-chief-chat-row="${c.beta}"]`).click();
    await expect(panelOn(page)).toHaveAttribute('data-chief-active-session', c.beta);
    // Shut and reopened: beta.
    await closePanel(page);
    await openPanel(page);
    await expect(panelOn(page), 'a reopened panel is on the chat picked in it').toHaveAttribute('data-chief-active-session', c.beta);
    // And the page follows the panel.
    await onPage(page, '/agents/' + c.name);
    expect(sessionOf(page.url()), 'the page opens on the chat picked in the panel').toBe(c.beta);

    // A link that names a chat still wins, and is then the one viewed last.
    await onPage(page, `/agents/${c.name}?session=${c.primary}`);
    expect(sessionOf(page.url())).toBe(c.primary);
    await onPage(page, `/agents/${c.name}?session=${c.alpha}`);
    expect(sessionOf(page.url()), 'an explicit ?session= wins').toBe(c.alpha);

    // Archived while it is the one viewed last: the primary.
    const agentID = /\/agents\/(\d+)\/sessions\/side/.exec(await page.content())?.[1];
    expect(agentID, 'the page names the chief\'s side chat route').toBeTruthy();
    const r = await page.request.post(`/agents/${agentID}/sessions/${c.alpha}/archive`, { headers: { Origin: new URL(page.url()).origin }, maxRedirects: 0 });
    expect(r.status(), 'archived').toBeLessThan(400);
    await onPage(page, '/agents/' + c.name);
    expect(sessionOf(page.url()), 'an archived chat viewed last falls back to the primary').toBe(c.primary);
    await page.goto('/tasks');
    await openPanel(page);
    await expect(panelOn(page)).toHaveAttribute('data-chief-active-session', c.primary);
  });
});

for (const vp of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`on a ${vp.name}`, () => {
    test.use(vp.use);

    test('a chat comes back where it was left — on the page and in the panel — and one left at its end at its end', async ({ page }) => {
      test.setTimeout(90_000);
      const c = await makeChief(page);
      const log = page.locator('#chat-log');

      // The page: alpha read back to its middle, beta at its end.
      await onPage(page, `/agents/${c.name}?session=${c.alpha}`);
      const alphaAt = await scrollToMiddle(page, log);
      await onPage(page, `/agents/${c.name}?session=${c.beta}`);
      expect((await place(log)).atEnd, 'a chat never scrolled opens at its end').toBe(true);
      // Back to alpha: where it was left.
      await onPage(page, `/agents/${c.name}?session=${c.alpha}`);
      await expect.poll(async () => (await place(log)).first, { message: 'alpha opens where it was left' }).toBe(alphaAt);
      expect((await place(log)).atEnd).toBe(false);
      // Left at its end: opens at its end.
      await log.evaluate((el) => { el.scrollTop = el.scrollHeight; el.dispatchEvent(new Event('scroll')); });
      await page.waitForTimeout(500);
      await page.goto('/tasks');
      await onPage(page, `/agents/${c.name}?session=${c.alpha}`);
      expect((await place(log)).atEnd, 'a chat left at its end opens at its end').toBe(true);

      // The panel: beta (the chat viewed last) read back, the panel shut and
      // reopened.
      await page.goto('/tasks');
      await onPage(page, `/agents/${c.name}?session=${c.beta}`);
      await page.goto('/tasks');
      await openPanel(page);
      const plog = panel(page).locator('#chat-log');
      await expect(panelOn(page)).toHaveAttribute('data-chief-active-session', c.beta);
      await expect.poll(async () => (await place(plog)).atEnd).toBe(true);
      const betaAt = await scrollToMiddle(page, plog);
      await closePanel(page);
      await openPanel(page);
      await expect.poll(async () => (await place(plog)).first, { message: 'the reopened panel is where it was left' }).toBe(betaAt);
      // And the page, on the same chat, too.
      await onPage(page, `/agents/${c.name}?session=${c.beta}`);
      await expect.poll(async () => (await place(log)).first, { message: 'the page is where the panel left it' }).toBe(betaAt);
    });

    test('the Sessions tab on the page: first in the list icon\'s sheet, search, chips, a pick switches, a ⋯ action reopens it, Escape', async ({ page }) => {
      test.setTimeout(120_000);
      const tap = (l: Locator) => (vp.use.hasTouch ? l.tap() : l.click());
      const c = await makeChief(page);
      // A reply lands in beta (the newest chat) after it was last read: beta is
      // unread while the owner is on the primary.
      mkchat(c.name, [{ role: 'assistant', content: 'a reply while you were away' }], { MUSTER_E2E_APPEND: '1' });
      await onPage(page, `/agents/${c.name}`);
      expect(sessionOf(page.url())).toBe(c.primary);
      const b = bar(page, 'page');
      const ic = icon(page, 'page');
      const sh = sheet(page, 'page');
      const tab = sessionsTab(page, 'page');
      // The bar is plain text where the Sessions button was; the dot is on the
      // header's list icon, which says so in its name.
      await expect(b).toHaveText('Session: ★Primary');
      await expect(b.locator('button, a, input')).toHaveCount(0);
      await expect(page.locator('button[data-chief-sessions-open], [data-chief-sessions-drawer]')).toHaveCount(0);
      await expect(page.locator('header #chat-index-open')).toHaveCount(1);
      await expect(ic.locator('[data-chief-sessions-unread]')).toHaveCount(1);
      await expect(ic).toHaveAccessibleName('Sessions, your messages and what it worked on, unread replies in other sessions');
      const barBox = (await b.boundingBox())!;
      await shot(page, `sessions-tab-page-shut-${vp.name}.png`);

      // Open: Sessions is the first tab and selected (a first use), its label
      // counts the unread chat, the focus is on the chat on screen's row.
      await tap(ic);
      await expect(sh).toBeVisible();
      await expect(ic).toHaveAttribute('aria-expanded', 'true');
      await expect(sh.locator('[role="tab"]')).toHaveText(['Sessions1, 1 unread', 'Your messages', 'What it worked on']);
      await expect(sh.locator('[role="tab"][data-tab="sessions"]')).toHaveAttribute('aria-selected', 'true');
      await expect(tab).toBeVisible();
      await expect(tab.locator(`[data-chief-chat-row="${c.primary}"]`)).toBeFocused();
      const ids = await tab.locator('[data-chief-chip]').evaluateAll((els) => els.map((e) => e.getAttribute('data-chief-chip')));
      expect(ids).toEqual([c.primary, c.beta, c.alpha]);
      await expect(tab.locator(`[data-chief-chat-row="${c.primary}"]`)).toContainText('★Primary');
      await expect(tab.locator(`[data-chief-chip="${c.beta}"] [data-chief-unread]`)).toHaveCount(1);
      await expect(tab.getByRole('button', { name: 'New session' })).toBeVisible();
      const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
      expect(sw, 'the page does not scroll sideways').toBeLessThanOrEqual(cw);
      // The sheet moved nothing outside it; it is a fixed height.
      expect((await b.boundingBox())!, 'opening the sheet moves nothing under it').toEqual(barBox);
      const sheetBox = (await sh.locator('[data-chat-index-close] + div').boundingBox())!;
      await shot(page, `sessions-tab-page-open-${vp.name}.png`);

      // The search: by title, instant; the chips: toggle buttons.
      const search = tab.getByRole('searchbox', { name: 'Search sessions' });
      const visibleRows = () => tab.locator('[data-chief-chip]:visible').evaluateAll((els) => els.map((e) => e.getAttribute('data-chief-chip')));
      await search.fill('ALPHA');
      await expect.poll(visibleRows).toEqual([c.alpha]);
      await search.fill('no such chat at all');
      await expect.poll(visibleRows).toEqual([]);
      await expect(tab.getByText('No sessions match')).toBeVisible();
      expect((await sh.locator('[data-chat-index-close] + div').boundingBox())!.height, 'the sheet keeps its height').toBeCloseTo(sheetBox.height, 0);
      await search.fill('');
      await expect.poll(visibleRows).toEqual([c.primary, c.beta, c.alpha]);
      await expect(tab.getByText('No sessions match')).toBeHidden();
      const unreadChip = tab.getByRole('button', { name: 'Unread', exact: true });
      const allChip = tab.getByRole('button', { name: 'All', exact: true });
      await expect(allChip).toHaveAttribute('aria-pressed', 'true');
      await tap(unreadChip);
      await expect(unreadChip).toHaveAttribute('aria-pressed', 'true');
      await expect(allChip).toHaveAttribute('aria-pressed', 'false');
      await expect.poll(visibleRows, { message: 'Unread: the unread chat alone (filters apply to the chat on screen too)' }).toEqual([c.beta]);
      // Pressed again: back to All.
      await tap(unreadChip);
      await expect(allChip).toHaveAttribute('aria-pressed', 'true');
      await expect.poll(visibleRows).toEqual([c.primary, c.beta, c.alpha]);

      // Tab stays inside the sheet.
      await tab.locator(`[data-chief-chat-row="${c.primary}"]`).focus();
      for (let i = 0; i < 14; i++) {
        await page.keyboard.press('Tab');
        expect(await page.evaluate(() => !!document.activeElement?.closest('#chat-index')), `Tab ${i + 1} stays in the sheet`).toBe(true);
      }
      // Escape on an open ⋯ menu closes the MENU; the next shuts the sheet,
      // the focus back on the icon.
      await tap(tab.locator(`[data-chief-chip="${c.alpha}"] button[aria-haspopup="menu"]`));
      await expect(page.locator(`#chief-chat-menu-${c.alpha}`)).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(page.locator(`#chief-chat-menu-${c.alpha}`)).toBeHidden();
      await expect(sh, 'Escape on a menu leaves the sheet open').toBeVisible();
      await page.keyboard.press('Escape');
      await expect(sh).toBeHidden();
      await expect(ic).toHaveAttribute('aria-expanded', 'false');
      await expect(ic).toBeFocused();

      // A pick switches through the same link, and shuts the sheet.
      await tap(ic);
      await tap(tab.locator(`[data-chief-chat-row="${c.alpha}"]`));
      await page.waitForURL((u) => sessionOf(u.toString()) === c.alpha);
      await expect(b).toHaveText('Session: side alpha');
      await expect(sh).toBeHidden();
      // The tab is remembered: the next open is on Sessions again.
      await tap(ic);
      await expect(sh.locator('[role="tab"][data-tab="sessions"]')).toHaveAttribute('aria-selected', 'true');
      // Switching tabs moves nothing outside the sheet, and the sheet keeps
      // its height.
      const onAlpha = (await b.boundingBox())!;
      const box = sh.locator('[data-chat-index-close] + div');
      const h0 = (await box.boundingBox())!.height;
      await tap(sh.locator('[role="tab"][data-tab="messages"]'));
      await expect(sh.locator('#chat-index-tabs-panel-messages')).toBeVisible();
      expect((await b.boundingBox())!, 'switching tabs moves nothing under the sheet').toEqual(onAlpha);
      expect((await box.boundingBox())!.height, 'switching tabs keeps the sheet\'s height').toBeCloseTo(h0, 0);
      await tap(sh.locator('[role="tab"][data-tab="sessions"]'));

      // A row's ⋯ action (Archive beta) answers with the sheet open again on
      // Sessions, beta behind "Archived (1)" — and the Archived chip and the
      // search both reach it.
      await tap(tab.locator(`[data-chief-chip="${c.beta}"] button[aria-haspopup="menu"]`));
      const reloaded = page.waitForEvent('load');
      await page.locator(`#chief-chat-menu-${c.beta}`).getByRole('menuitem', { name: 'Archive' }).click();
      await reloaded;
      await expect(sh).toBeVisible();
      await expect(ic).toHaveAttribute('aria-expanded', 'true');
      await expect(sh.locator('[role="tab"][data-tab="sessions"]')).toHaveAttribute('aria-selected', 'true');
      expect(sessionOf(page.url()), 'archiving another chat leaves you on yours').toBe(c.alpha);
      const toggle = tab.locator('button[data-chief-archived]');
      await expect(toggle).toHaveText('Archived (1)');
      await expect(tab.locator(`[data-chief-chip="${c.beta}"]`)).toBeHidden();
      await tap(tab.getByRole('button', { name: 'Archived', exact: true }));
      await expect.poll(visibleRows, { message: 'the Archived chip lists the archived chat' }).toEqual([c.beta]);
      await expect(toggle).toBeHidden();
      await tap(allChip);
      await expect(tab.locator(`[data-chief-chip="${c.beta}"]`)).toBeHidden();
      await search.fill('beta');
      await expect.poll(visibleRows, { message: 'the search reaches an archived chat' }).toEqual([c.beta]);
      await search.fill('');
      await tap(toggle);
      await expect(toggle).toHaveAttribute('aria-expanded', 'true');
      await expect(tab.locator(`[data-chief-chip="${c.beta}"]`)).toHaveAttribute('data-chief-chip-kind', 'archived');
      await expect(tab.locator(`[data-chief-chip="${c.beta}"]`)).toBeVisible();
      await shot(page, `sessions-tab-page-archived-${vp.name}.png`);
    });

    test('the Sessions tab in the panel: the title bar\'s list icon, a sheet in the panel\'s column, a pick and a ⋯ action stay in the grid, Escape shuts the sheet and not the panel', async ({ page }) => {
      test.setTimeout(120_000);
      const tap = (l: Locator) => (vp.use.hasTouch ? l.tap() : l.click());
      const c = await makeChief(page);
      await page.goto('/tasks');
      await openPanel(page);
      const grid = page.url();
      const ic = icon(page, 'panel');
      const sh = sheet(page, 'panel');
      const tab = sessionsTab(page, 'panel');
      await expect(panel(page).locator('button[data-chief-sessions-open], [data-chief-sessions-drawer]')).toHaveCount(0);
      await expect(bar(page, 'panel')).toHaveText('Session: ★Primary');
      await expect(ic).toBeVisible();
      await tap(ic);
      await expect(sh).toBeVisible();
      await expect(sh.locator('[role="tab"]')).toHaveText(['Sessions', 'Your messages', 'What it worked on']);
      await expect(tab.locator(`[data-chief-chat-row="${c.primary}"]`)).toBeFocused();
      await shot(page, `sessions-tab-panel-open-${vp.name}.png`);
      // Inside the panel's column.
      const pb = (await panel(page).boundingBox())!;
      const sb = (await sh.boundingBox())!;
      expect(sb.x, 'inside the panel').toBeGreaterThanOrEqual(pb.x - 0.5);
      expect(sb.x + sb.width, 'inside the panel').toBeLessThanOrEqual(pb.x + pb.width + 0.5);
      // The search, in the panel too.
      const search = tab.getByRole('searchbox', { name: 'Search sessions' });
      await search.fill('beta');
      await expect.poll(() => tab.locator('[data-chief-chip]:visible').evaluateAll((els) => els.map((e) => e.getAttribute('data-chief-chip')))).toEqual([c.beta]);
      await search.fill('');
      // Escape: the sheet shuts, the panel stays.
      await page.keyboard.press('Escape');
      await expect(sh).toBeHidden();
      await expect(ic).toBeFocused();
      await page.waitForTimeout(300);
      await expect(panel(page), 'Escape on the sheet leaves the panel open').toHaveAttribute('data-chief-panel-open', '1');
      // A pick switches the panel's chat, in the grid.
      await tap(ic);
      await tap(tab.locator(`[data-chief-chat-row="${c.alpha}"]`));
      await expect(panelOn(page)).toHaveAttribute('data-chief-active-session', c.alpha);
      await expect(bar(page, 'panel')).toHaveText('Session: side alpha');
      await expect(sh).toBeHidden();
      expect(page.url(), 'the grid stays').toBe(grid);
      // A ⋯ action (Archive beta): the panel body answers, the sheet open again
      // on Sessions, the panel on alpha.
      await tap(ic);
      await tap(tab.locator(`[data-chief-chip="${c.beta}"] button[aria-haspopup="menu"]`));
      await page.locator(`#chief-panel-chat-menu-${c.beta}`).getByRole('menuitem', { name: 'Archive' }).click();
      await expect(tab.locator(`[data-chief-chip="${c.beta}"]`)).toHaveAttribute('data-chief-chip-kind', 'archived');
      await expect(sh, 'the sheet is open again').toBeVisible();
      await expect(sh.locator('[role="tab"][data-tab="sessions"]')).toHaveAttribute('aria-selected', 'true');
      await expect(panelOn(page)).toHaveAttribute('data-chief-active-session', c.alpha);
      expect(page.url(), 'the grid stays').toBe(grid);
      // Escape with the sheet open is the sheet's; then the panel's again.
      await page.keyboard.press('Escape');
      await expect(sh).toBeHidden();
      await expect(panel(page)).toHaveAttribute('data-chief-panel-open', '1');
      await page.keyboard.press('Escape');
      await expect(panel(page)).toHaveAttribute('data-chief-panel-open', '0');
    });

    // THE MESSAGE SEARCH, folded in from the chief panel's thread list (an
    // operator decision: one list of the chief's chats). The tab's box filters
    // the rows by title at once, and the words in the chats' MESSAGES come from
    // the server under "Matches in messages" — a chat whose title matches is a
    // row already, so it is listed there only when its messages match too.
    test('the Sessions tab searches the chats\' messages too, on the page and in the panel; a match switches', async ({ page }) => {
      test.setTimeout(90_000);
      const tap = (l: Locator) => (vp.use.hasTouch ? l.tap() : l.click());
      for (const n of made.splice(0)) mkchat(n, null);
      const name = `e2e-msgsearch-chief-${Date.now().toString(36)}-${++seq}`;
      mkchat(name, null);
      mkchat(
        name,
        [
          { title: 'Kestrel notes', hoursAgo: 50, messages: [{ role: 'user', content: 'first words here' }] },
          { title: 'about the kestrel', hoursAgo: 30, messages: [{ role: 'user', content: 'third' }] },
          { title: 'unrelated', hoursAgo: 10, messages: [{ role: 'user', content: 'the kestrel came back at dawn' }] },
          { title: 'nothing here', hoursAgo: 1, messages: [{ role: 'user', content: 'nope' }] },
        ],
        { MUSTER_E2E_DISPLAY_NAME: 'chief', MUSTER_E2E_THREADS: '1' },
      );
      made.push(name);

      // The page.
      await onPage(page, `/agents/${name}`);
      const tab = sessionsTab(page, 'page');
      await openSessions(tab, vp.use.hasTouch);
      const search = tab.getByRole('searchbox', { name: 'Search sessions' });
      const titles = () => tab.locator('[data-chief-chip]:visible [data-chief-chat-row]').evaluateAll((els) => els.map((e) => e.getAttribute('title')));
      const matches = tab.locator('[data-chief-message-match]');
      const none = tab.getByText('No sessions match');
      await search.fill('kestrel');
      await expect.poll(titles, { message: 'the rows whose title holds the words, at once' }).toEqual(['Kestrel notes', 'about the kestrel']);
      await expect(matches, 'the one chat whose MESSAGES hold them').toHaveCount(1, { timeout: 10_000 });
      await expect(matches).toContainText('unrelated');
      await expect(matches).toContainText('kestrel came back');
      await expect(tab.getByRole('heading', { name: 'Matches in messages' })).toBeVisible();
      // Words only a message holds: no row, a match, and "No sessions match" stays away.
      await search.fill('came back');
      await expect.poll(titles).toEqual([]);
      await expect(matches).toHaveCount(1, { timeout: 10_000 });
      await expect(none).toBeHidden();
      await shot(page, `message-search-page-${vp.name}.png`);
      // Words nowhere: no row, no match, the line.
      await search.fill('zqxj-nowhere');
      await expect(matches).toHaveCount(0, { timeout: 10_000 });
      await expect(none).toBeVisible();
      // A match switches through its link, and shuts the sheet.
      await search.fill('came back');
      await expect(matches).toHaveCount(1, { timeout: 10_000 });
      const unrelated = (await matches.getAttribute('data-chief-message-match'))!;
      await tap(matches);
      await page.waitForURL((u) => sessionOf(u.toString()) === unrelated);
      await expect(sheet(page, 'page')).toBeHidden();
      await expect(bar(page, 'page')).toHaveText('Session: unrelated');

      // The panel: the same search, and a match switches the panel's chat, in the grid.
      await page.goto('/tasks');
      await openPanel(page);
      const grid = page.url();
      const ptab = sessionsTab(page, 'panel');
      await openSessions(ptab, vp.use.hasTouch);
      const psearch = ptab.getByRole('searchbox', { name: 'Search sessions' });
      await psearch.fill('first words');
      const pmatch = ptab.locator('[data-chief-message-match]');
      await expect(pmatch).toHaveCount(1, { timeout: 10_000 });
      await expect(pmatch).toContainText('Kestrel notes');
      const kestrel = (await pmatch.getAttribute('data-chief-message-match'))!;
      await shot(page, `message-search-panel-${vp.name}.png`);
      await tap(pmatch);
      await expect(panelOn(page)).toHaveAttribute('data-chief-active-session', kestrel);
      await expect(sheet(page, 'panel')).toBeHidden();
      expect(page.url(), 'the grid stays').toBe(grid);
    });
  });
}

// 🔴 AN INVARIANT GUARD, NOT REGRESSION COVERAGE: TWO SHEETS ON ONE DOCUMENT
// EACH OPEN THEIR OWN. No page muster serves holds the page's sheet and the
// panel's together today (the chief page and the mirrored view are their own
// documents; the panel is the home shell's), so no served path has hit this.
// It pins the opener-scoped (aria-controls) lookup so a future document holding
// both works, by building that document on purpose: the chief page, with the
// panel's body — its sheet and its title bar's icon — fetched and put beside
// it. Each icon opens its own sheet, the other stays shut, and the page's
// "Your messages" is still built into the page's sheet.
test.describe('two sheets (invariant guard)', () => {
  test.use({ viewport: { width: 1280, height: 800 } });
  test('invariant: the page\'s sheet and the panel\'s, on one document, each open their own', async ({ page }) => {
  const c = await makeChief(page);
  await onPage(page, `/agents/${c.name}?session=${c.alpha}`);
  await page.evaluate(async () => {
    const html = await (await fetch('/ui/chief/panel')).text();
    const box = document.createElement('div');
    box.id = 'e2e-panel-copy';
    box.style.cssText = 'position:fixed;left:0;top:0;width:420px;height:700px;display:flex;flex-direction:column;z-index:60';
    box.innerHTML = html;
    document.body.appendChild(box);
  });
  const pageSheet = sheet(page, 'page');
  const panelSheet = sheet(page, 'panel');
  await expect(panelSheet).toHaveCount(1);
  await page.locator('#chief-panel-index-open').click();
  await expect(panelSheet).toBeVisible();
  await expect(pageSheet, 'the panel\'s icon opens the panel\'s sheet only').toBeHidden();
  await page.keyboard.press('Escape');
  await expect(panelSheet).toBeHidden();
  await page.evaluate(() => document.getElementById('e2e-panel-copy')!.style.setProperty('z-index', '0'));
  await page.locator('header #chat-index-open').click();
  await expect(pageSheet).toBeVisible();
  await expect(panelSheet, 'the page\'s icon opens the page\'s sheet only').toBeHidden();
  await pageSheet.locator('[role="tab"][data-tab="messages"]').click();
  await expect(pageSheet.locator('#chat-index-list > li').first(), 'the page\'s messages are built into the page\'s sheet').toBeVisible();
  });
});
