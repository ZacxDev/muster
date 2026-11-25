import path from 'node:path';
import { Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { mkchat } from '../lib/mirrorhost';

// THE CHIEF'S SESSIONS, AS THE OPERATOR ASKED FOR THEM (docs/chief.md
// "Primary and side chats"), in a real browser on the shared server:
//
//   1. "Reopening the Chief tab should return it to the Chief session that I
//      was most recently on." — the chief page (bare), its sidebar row, home
//      `/` and the chief panel all open on the chat viewed last, on either
//      surface; an explicit ?session= still wins; an archived one falls back
//      to the primary.
//   2. "a slide out from the right side of the screen instead of being big
//      pills at the top" — one Sessions button, a drawer from the right edge
//      (of the screen on the page, of the panel's column in the panel), a
//      dialog by keyboard: focus in, Tab kept in, Escape / backdrop / ✕ out
//      with the focus back on the button; a pick shuts it.
//   3. "when I navigate back to a Chief session after navigating away, it
//      should restore my scroll position" — per chat, on the page and in the
//      panel; a chat left at its end opens at its end.
//
// 🔴 REGRESSION: tests 1 and 3 move between chats only by ways that exist on
// main too (a URL, the sidebar row, home, the panel's thread list, closing and
// reopening the panel), so each fails there for its own reason: main opened
// the sidebar row and home on the chat touched last, the bare page and the
// panel on the primary, and every chat at its end. The drawer tests fail on
// main because there is no drawer.
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
  const name = `e2e-drawer-chief-${Date.now().toString(36)}-${++seq}`;
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
  // The ids, from the panel's thread list (on main and here alike): its rows
  // are the chats, by title.
  const html = await (await page.request.get('/ui/chief/threads/search?q=')).text();
  const rows = html.split('data-chief-thread="').slice(1);
  const idOf = (title: string) => {
    // The title as an element's whole text ("Make this the primary chat" is a
    // side chat's control's title).
    const row = rows.find((r) => r.includes('>' + title + '<'));
    expect(row, `the thread list names "${title}"`).toBeTruthy();
    return /^\d+/.exec(row!)![0];
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
    // Picked in the panel (its thread list: on main too): beta.
    await panel(page).locator('label[for="chief-threads"]').first().click();
    await panel(page).locator(`[data-chief-thread="${c.beta}"]`).click();
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

    test('the Sessions drawer: one button, a dialog from the right, a pick switches and shuts it, a chat keeps its place across picks', async ({ page }) => {
      test.setTimeout(90_000);
      const tap = (l: Locator) => (vp.use.hasTouch ? l.tap() : l.click());
      const c = await makeChief(page);
      // The bare page: never opened, the primary (and its election).
      await onPage(page, `/agents/${c.name}`);
      expect(sessionOf(page.url())).toBe(c.primary);
      const bar = page.locator('body > [data-chief-strip-row] [data-chief-chat-strip]');
      const button = bar.locator('button[data-chief-sessions-open]');
      const drawer = page.locator('#chief-sessions');
      // No pills: the bar holds the one button; the rows are in the drawer.
      await expect(button).toHaveCount(1);
      await expect(button).toHaveAccessibleName('Sessions: Primary');
      await expect(button).toHaveAttribute('aria-expanded', 'false');
      await expect(button).toHaveAttribute('aria-controls', 'chief-sessions');
      await expect(drawer).toHaveAttribute('role', 'dialog');
      await expect(drawer).toHaveAttribute('aria-modal', 'true');
      await expect(drawer).not.toBeInViewport();
      const bb = (await button.boundingBox())!;
      expect(bb.height, 'a 44px target').toBeGreaterThanOrEqual(44);
      await shot(page, `drawer-page-shut-${vp.name}.png`);

      // Open: from the right edge, the focus on the chat on screen's row.
      await tap(button);
      await expect(button).toHaveAttribute('aria-expanded', 'true');
      await expect(drawer).toBeInViewport({ ratio: 0.99 });
      await expect(drawer.locator(`[data-chief-chat-row="${c.primary}"]`)).toBeFocused();
      await page.waitForTimeout(300); // the slide
      await shot(page, `drawer-page-open-${vp.name}.png`);
      const db = (await drawer.boundingBox())!;
      const vw = page.viewportSize()!.width;
      expect(Math.abs(db.x + db.width - vw), 'the drawer is against the screen\'s right edge').toBeLessThanOrEqual(1);
      expect(db.height, 'the drawer is the screen\'s height').toBeGreaterThanOrEqual(page.viewportSize()!.height - 1);
      // Every chat is a row: the primary first with its ★, then the side chats.
      const ids = await drawer.locator('[data-chief-chip]').evaluateAll((els) => els.map((e) => e.getAttribute('data-chief-chip')));
      expect(ids).toEqual([c.primary, c.beta, c.alpha]);
      await expect(drawer.locator(`[data-chief-chat-row="${c.primary}"]`)).toContainText('★Primary');
      await expect(drawer.locator('[data-chief-chat-row][aria-current="true"]')).toHaveCount(1);
      await expect(drawer.getByRole('button', { name: 'New session' })).toBeVisible();
      await expect(drawer.locator(`[data-chief-chip="${c.alpha}"] button[aria-haspopup="menu"]`)).toHaveAccessibleName('Actions for side alpha');
      const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
      expect(sw, 'the page does not scroll sideways').toBeLessThanOrEqual(cw);

      // Tab stays inside it.
      for (let i = 0; i < 12; i++) {
        await page.keyboard.press('Tab');
        expect(await page.evaluate(() => !!document.activeElement?.closest('#chief-sessions')), `Tab ${i + 1} stays in the drawer`).toBe(true);
      }
      // Escape shuts it, the focus back on the button.
      await page.keyboard.press('Escape');
      await expect(button).toHaveAttribute('aria-expanded', 'false');
      await expect(button).toBeFocused();
      await expect(drawer).toHaveAttribute('inert', '');
      // The backdrop shuts it too.
      await tap(button);
      await expect(drawer).toBeInViewport({ ratio: 0.99 });
      await page.locator('[data-chief-sessions-backdrop="chief-sessions"]').click({ position: { x: 10, y: 200 } });
      await expect(button).toHaveAttribute('aria-expanded', 'false');
      // And ✕.
      await tap(button);
      await tap(drawer.getByRole('button', { name: 'Close sessions' }));
      await expect(button).toHaveAttribute('aria-expanded', 'false');

      // A pick switches: alpha, read back to its middle.
      await tap(button);
      await tap(drawer.locator(`[data-chief-chat-row="${c.alpha}"]`));
      await page.waitForURL((u) => sessionOf(u.toString()) === c.alpha);
      await expect(button).toHaveAccessibleName('Sessions: side alpha');
      await expect(button).toHaveAttribute('aria-expanded', 'false');
      const log = page.locator('#chat-log');
      await expect(log).toContainText('alpha line 40');
      const alphaAt = await scrollToMiddle(page, log);
      // To beta and back, through the drawer: alpha is where it was left.
      await tap(button);
      await tap(drawer.locator(`[data-chief-chat-row="${c.beta}"]`));
      await page.waitForURL((u) => sessionOf(u.toString()) === c.beta);
      await tap(button);
      await tap(drawer.locator(`[data-chief-chat-row="${c.alpha}"]`));
      await page.waitForURL((u) => sessionOf(u.toString()) === c.alpha);
      await expect.poll(async () => (await place(log)).first, { message: 'back on alpha, where it was left' }).toBe(alphaAt);

      // A row's ⋯ action (Archive beta) answers with the drawer open again,
      // beta behind "Archived (1)", which expands inline.
      await tap(button);
      await tap(drawer.locator(`[data-chief-chip="${c.beta}"] button[aria-haspopup="menu"]`));
      const reloaded = page.waitForEvent('load');
      await page.locator(`#chief-chat-menu-${c.beta}`).getByRole('menuitem', { name: 'Archive' }).click();
      await reloaded;
      await expect(button).toHaveAttribute('aria-expanded', 'true');
      await expect(drawer).toBeInViewport({ ratio: 0.99 });
      const toggle = drawer.locator('button[data-chief-archived]');
      await expect(toggle).toHaveText('Archived (1)');
      await expect(drawer.locator(`[data-chief-chip="${c.beta}"]`)).toBeHidden();
      await tap(toggle);
      await expect(toggle).toHaveAttribute('aria-expanded', 'true');
      await expect(drawer.locator(`[data-chief-chip="${c.beta}"]`)).toHaveAttribute('data-chief-chip-kind', 'archived');
      await expect(drawer.locator(`[data-chief-chip="${c.beta}"]`)).toBeVisible();
      expect(sessionOf(page.url()), 'archiving another chat leaves you on yours').toBe(c.alpha);
      await shot(page, `drawer-page-archived-${vp.name}.png`);
    });

    test('the panel\'s Sessions drawer slides from the panel\'s right edge; Escape shuts it and not the panel; a pick switches the panel', async ({ page }) => {
      test.setTimeout(90_000);
      const tap = (l: Locator) => (vp.use.hasTouch ? l.tap() : l.click());
      const c = await makeChief(page);
      await page.goto('/tasks');
      await openPanel(page);
      const grid = page.url();
      const button = panel(page).locator('button[data-chief-sessions-open]');
      const drawer = page.locator('#chief-panel-sessions');
      await expect(button).toHaveAccessibleName('Sessions: Primary');
      await expect(button).toHaveAttribute('aria-controls', 'chief-panel-sessions');
      await tap(button);
      await expect(button).toHaveAttribute('aria-expanded', 'true');
      await expect(drawer.locator(`[data-chief-chat-row="${c.primary}"]`)).toBeFocused();
      await page.waitForTimeout(300);
      await shot(page, `drawer-panel-open-${vp.name}.png`);
      const pb = (await panel(page).boundingBox())!;
      const db = (await drawer.boundingBox())!;
      expect(db.x + db.width, 'inside the panel').toBeLessThanOrEqual(pb.x + pb.width + 0.5);
      expect(pb.x + pb.width - (db.x + db.width), 'against the panel\'s right edge (its padding)').toBeLessThanOrEqual(16);
      expect(db.x, 'inside the panel').toBeGreaterThanOrEqual(pb.x - 0.5);
      // Escape: the drawer shuts, the panel stays.
      await page.keyboard.press('Escape');
      await expect(button).toHaveAttribute('aria-expanded', 'false');
      await expect(button).toBeFocused();
      await page.waitForTimeout(300);
      await expect(panel(page), 'Escape on the drawer leaves the panel open').toHaveAttribute('data-chief-panel-open', '1');
      // A pick switches the panel's chat, in the grid.
      await tap(button);
      await tap(drawer.locator(`[data-chief-chat-row="${c.alpha}"]`));
      await expect(panelOn(page)).toHaveAttribute('data-chief-active-session', c.alpha);
      await expect(button).toHaveAccessibleName('Sessions: side alpha');
      await expect(button).toHaveAttribute('aria-expanded', 'false');
      expect(page.url(), 'the grid stays').toBe(grid);
      // Escape with the drawer shut is the panel's again.
      await page.keyboard.press('Escape');
      await expect(panel(page)).toHaveAttribute('data-chief-panel-open', '0');
    });
  });
}
