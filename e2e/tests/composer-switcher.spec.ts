import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { baseURL, dsn, repoRoot } from '../lib/env';
import { clsInit, readCls, resetCls, settle, ClsResult } from '../lib/cls';
import { mkchat } from '../lib/mirrorhost';

// THE SESSION PAGE'S COMPOSER CHANGES, on the shared server's dispatched agent
// pages (a real browser, the seeded agents):
//
//   1. "Hide until next ask" on the asks block (ui/attention.go): hides it in
//      THIS view and sends NOTHING — no request at all, so neither the queue's
//      answer/dismiss route nor the session is reached, and the ask stays open
//      on /attention. A "N waiting" chip reopens it. A NEW ask brings it back by
//      itself; one that only leaves keeps it hidden; the hide survives a reload.
//   2. The session switcher below the input (ui/switcher.go): server-rendered
//      with the page, Recent / Pins / All picked from the menu the button on its left opens, the
//      choice kept across loads, each entry a real navigation, the page's own
//      session left out of Recent and marked in All, and never wider than the
//      page at 390px.
//   3. No layout shift (< 0.01) on the page with the bar in each view and with
//      a hidden ask restored, at a phone and a desktop size.
//
// The seeded agents are every spec's: what this asserts is about the asks,
// visits and pins it makes itself, and it removes them.

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

const BAR = 0.01;
const block = (page: Page) => page.locator('#asker-asks');
const chip = (page: Page) => page.locator('#asker-asks-chip');
const bar = (page: Page) => page.locator('#session-switcher');
const row = (page: Page, view: string) => bar(page).locator(`[data-switcher-list="${view}"]`);
// An agent's entry links its page with the session the list saw (ui.AgentListHref:
// /agents/<name>?session=<id>&from=list), so entries are compared by path.
const hrefs = (page: Page, view: string) =>
  row(page, view).locator('a[data-switcher-entry]').evaluateAll((els) => els.map((e) => new URL(e.getAttribute('href')!, location.href).pathname));

// pickView opens the view menu (the bar's left button) and picks a view.
async function pickView(page: Page, view: string) {
  await page.locator('#switcher-toggle').click();
  await expect(page.locator('#switcher-views')).toBeVisible();
  await page.locator(`#switcher-views [data-switcher-pick="${view}"]`).click();
  await expect(page.locator('#switcher-views')).toBeHidden();
}

async function openAgent(page: Page, name: string) {
  await page.goto('/agents/' + name);
  await page.waitForURL((u) => u.pathname === '/agents/' + name && u.searchParams.has('session'));
  await expect(bar(page)).toBeVisible();
}

async function agentID(page: Page, name: string): Promise<string> {
  // The agent's row in the sidebar's list (GET /ui/nav/agents): its id is
  // nav-agent-<id>, its link the agent's page.
  const html = await (await page.request.get('/ui/nav/agents')).text();
  const m = new RegExp(`<a id="nav-agent-(\\d+)" href="/agents/${name}[?"]`).exec(html);
  expect(m, `${name} is in the sidebar's list`).not.toBeNull();
  return m![1];
}

async function pin(page: Page, id: string, on: boolean) {
  const r = await page.request.post(on ? '/agents/pins' : '/agents/pins/delete', {
    form: { kind: 'agent', agent_id: id },
    headers: { Origin: baseURL, 'HX-Request': 'true' },
  });
  expect(r.status(), await r.text()).toBe(200);
}

const asks: string[] = [];
let pinned = '';
test.afterEach(async ({ page }) => {
  for (const id of asks.splice(0)) fix('rmask', '1', id);
  if (pinned) await pin(page, pinned, false).catch(() => undefined);
  pinned = '';
});

// 🔴 A REGRESSION TEST: at the base there is no hide button (the block can only
// be answered or dismissed, both of which post).
test('"Hide until next ask" hides the asks here without sending anything; a new ask brings them back; the chip reopens them', async ({ page }) => {
  const q1 = fix('ask', '1', 'docs-sweeper', 'Publish the docs build?');
  asks.push(q1);
  await openAgent(page, 'docs-sweeper');
  await expect(block(page).locator(`#ask-${q1}`)).toBeVisible();
  await expect(chip(page)).toBeHidden();

  // Every request the page makes from the tap on: there must be none at all.
  const sent: string[] = [];
  page.on('request', (r) => {
    if (r.resourceType() !== 'eventsource') sent.push(`${r.method()} ${new URL(r.url()).pathname}`);
  });
  const hide = block(page).getByRole('button', { name: 'Hide until next ask' });
  expect((await hide.boundingBox())!.height, 'the hide is a 44px target').toBeGreaterThanOrEqual(44);
  await hide.click();
  await expect(block(page)).toBeHidden();
  await expect(chip(page)).toBeVisible();
  await expect(chip(page)).toHaveText('1 waiting');
  expect((await chip(page).boundingBox())!.height, 'the chip is a 44px target').toBeGreaterThanOrEqual(44);
  await page.waitForTimeout(500);
  expect(sent, 'the hide made no request (no answer, no dismiss, nothing to the session)').toEqual([]);
  // Still open in the queue.
  expect(await (await page.request.get('/attention')).text(), 'the ask stays in the Asks queue').toContain(`id="ask-${q1}"`);

  // Kept across a reload: hidden from the first paint, the chip in its place.
  await page.reload();
  await expect(chip(page)).toHaveText('1 waiting');
  await expect(block(page)).toBeHidden();
  await expect(block(page).locator(`#ask-${q1}`), 'the block still holds the ask (it is hidden, not removed)').toHaveCount(1);

  // A NEW ask, arriving live (the block's own refresh): the block is back.
  const q2 = fix('ask', '1', 'docs-sweeper', 'And tag the release?');
  asks.push(q2);
  await page.evaluate(() => (window as any).htmx.trigger(document.body, 'sse:attention_queue.changed'));
  await expect(block(page).locator(`#ask-${q2}`)).toBeVisible();
  await expect(block(page)).toBeVisible();
  await expect(chip(page)).toBeHidden();

  // Hidden again; then one ask LEAVES (answered elsewhere): still hidden, the
  // count follows.
  await block(page).getByRole('button', { name: 'Hide until next ask' }).click();
  await expect(chip(page)).toHaveText('2 waiting');
  fix('rmask', '1', q2);
  asks.splice(asks.indexOf(q2), 1);
  await page.evaluate(() => (window as any).htmx.trigger(document.body, 'sse:attention_queue.changed'));
  await expect(block(page).locator(`#ask-${q2}`)).toHaveCount(0);
  await expect(block(page)).toBeHidden();
  await expect(chip(page)).toHaveText('1 waiting');

  // The chip reopens it, and that is remembered too.
  await chip(page).click();
  await expect(block(page)).toBeVisible();
  await expect(chip(page)).toBeHidden();
  await page.reload();
  await expect(block(page).locator(`#ask-${q1}`)).toBeVisible();
  await expect(chip(page)).toBeHidden();
  expect(sent.filter((s) => !s.startsWith('GET ')), 'nothing but page loads and the block\'s own refreshes').toEqual([]);
});

// 🔴 A REGRESSION TEST: AFTER BACK, EVERYTHING STILL WORKS. htmx's history
// snapshot of the page carries the DOM as it was — so a double-bind guard
// written as an ATTRIBUTE came back already "bound", the re-run scripts bailed,
// and the Hide button, the chip and the view toggle were all dead. The guards
// are JS properties now (a snapshot does not carry them).
test('after a boosted navigation and Back, the chip, Hide, a new ask and the view toggle all still work', async ({ page }) => {
  const q1 = fix('ask', '1', 'docs-sweeper', 'Back-button probe?');
  asks.push(q1);
  await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} }).catch(() => undefined);
  await openAgent(page, 'docs-sweeper');
  await block(page).getByRole('button', { name: 'Hide until next ask' }).click();
  await expect(chip(page)).toHaveText('1 waiting');
  // A boosted navigation (the sidebar's own link), then Back.
  await page.evaluate(() => (document.querySelector('.sidebar-tab[data-tab="tasks"]') as HTMLElement).click());
  await expect.poll(() => new URL(page.url()).pathname).toBe('/tasks');
  await page.goBack();
  await expect.poll(() => new URL(page.url()).pathname).toBe('/agents/docs-sweeper');
  await expect(bar(page)).toBeVisible();

  // The view menu still works.
  const toggle = page.locator('#switcher-toggle');
  await expect(toggle.locator('[data-switcher-icon]:visible')).toHaveAttribute('data-switcher-icon', 'recent');
  await pickView(page, 'pins');
  await expect(toggle.locator('[data-switcher-icon]:visible')).toHaveAttribute('data-switcher-icon', 'pins');
  await expect(row(page, 'pins')).toBeVisible();
  await pickView(page, 'recent');
  await expect(toggle.locator('[data-switcher-icon]:visible')).toHaveAttribute('data-switcher-icon', 'recent');
  // The chip reopens; Hide hides again.
  await expect(chip(page)).toHaveText('1 waiting');
  await chip(page).click();
  await expect(block(page)).toBeVisible();
  await expect(chip(page)).toBeHidden();
  await block(page).getByRole('button', { name: 'Hide until next ask' }).click();
  await expect(block(page)).toBeHidden();
  await expect(chip(page)).toHaveText('1 waiting');
  // A new ask, arriving by the block's own refresh, brings it back.
  const q2 = fix('ask', '1', 'docs-sweeper', 'And after Back?');
  asks.push(q2);
  await page.evaluate(() => (window as any).htmx.trigger(document.body, 'sse:attention_queue.changed'));
  await expect(block(page).locator(`#ask-${q2}`)).toBeVisible();
  await expect(chip(page)).toBeHidden();
});

// 🔴 A REGRESSION TEST: two tabs each hiding asks must not overwrite each
// other's hidden ids — a write merges with what is stored NOW, not with a copy
// read earlier — and a hide in one tab applies in the other at once (the
// `storage` event).
test('hidden asks merge across tabs, and a hide in one tab applies in the other', async ({ page }) => {
  const q1 = fix('ask', '1', 'docs-sweeper', 'Tab probe one?');
  asks.push(q1);
  await openAgent(page, 'docs-sweeper');
  await page.evaluate(() => localStorage.removeItem('muster.asks.hidden.v1'));
  const other = await page.context().newPage();
  try {
    // Tab B on the same page; tab A hides — B follows at once.
    await other.goto('/agents/docs-sweeper');
    await page.reload();
    await expect(block(other).locator(`#ask-${q1}`)).toBeVisible();
    await block(page).getByRole('button', { name: 'Hide until next ask' }).click();
    await expect(chip(page)).toHaveText('1 waiting');
    await expect(block(other), 'the hide applied in the other tab (storage event)').toBeHidden();
    await expect(chip(other)).toHaveText('1 waiting');

    // Tab B moves to ANOTHER agent and hides its ask there; tab A, which
    // already read the list, then hides a new ask of its own. A's write must
    // keep B's id.
    const qb = fix('ask', '1', 'release-helper', 'Tab probe, the other agent?');
    asks.push(qb);
    await other.goto('/agents/release-helper');
    await expect(block(other).locator(`#ask-${qb}`)).toBeVisible();
    await block(other).getByRole('button', { name: 'Hide until next ask' }).click();
    await expect(chip(other)).toBeVisible();
    const q2 = fix('ask', '1', 'docs-sweeper', 'Tab probe two?');
    asks.push(q2);
    await page.evaluate(() => (window as any).htmx.trigger(document.body, 'sse:attention_queue.changed'));
    await expect(block(page).locator(`#ask-${q2}`)).toBeVisible();
    await block(page).getByRole('button', { name: 'Hide until next ask' }).click();
    const stored = await page.evaluate(() => JSON.parse(localStorage.getItem('muster.asks.hidden.v1') || '[]'));
    expect(stored, 'every tab\'s hidden ids are kept').toEqual(expect.arrayContaining([`ask-${q1}`, `ask-${qb}`, `ask-${q2}`]));
  } finally {
    await other.close();
    await page.evaluate(() => localStorage.removeItem('muster.asks.hidden.v1'));
  }
});

// 🔴 A REGRESSION TEST: at the base there is no #session-switcher.
test('the switcher below the input: Recent without this page, Pins, All with it marked; the choice is kept; a tap navigates', async ({ page }) => {
  await openAgent(page, 'notes-tidy');
  await openAgent(page, 'release-helper');
  await openAgent(page, 'docs-sweeper');
  pinned = await agentID(page, 'release-helper');
  await pin(page, pinned, true);
  await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
  await openAgent(page, 'docs-sweeper');

  // Below the message input, inside the page's width.
  // The bar sits in the room the chat form reserves at its bottom (it is
  // absolute there, so its late arrival moves nothing): below the input row,
  // inside the form's box.
  const inputBox = (await page.locator('#chat-input').boundingBox())!;
  const form = (await page.locator('#chat-form').boundingBox())!;
  const b = (await bar(page).boundingBox())!;
  expect(b.y, 'the bar is below the input row').toBeGreaterThanOrEqual(inputBox.y + inputBox.height - 1);
  expect(Math.abs(b.y + b.height - (form.y + form.height)), 'in the room the form reserves at its bottom').toBeLessThanOrEqual(1);
  expect(b.y + b.height, 'and on screen').toBeLessThanOrEqual(page.viewportSize()!.height + 1);

  // Every control in the bar is a 44px target. (The ⋯ menu's item is in a
  // closed popover here: zero-sized, so skipped.)
  const small = await bar(page).locator('a, button').evaluateAll((els) =>
    els.filter((e) => { const r = e.getBoundingClientRect(); return r.width > 0 && (r.height < 44 || r.width < 44); }).map((e) => e.outerHTML.slice(0, 80)));
  expect(small, 'controls under 44px in the bar').toEqual([]);

  // Recent: the visits, newest first, without this page.
  const toggle = page.locator('#switcher-toggle');
  await expect(toggle.locator('[data-switcher-icon]:visible')).toHaveAttribute('data-switcher-icon', 'recent');
  await expect(row(page, 'recent')).toBeVisible();
  const recent = await hrefs(page, 'recent');
  expect(recent.slice(0, 2), 'newest first').toEqual(['/agents/release-helper', '/agents/notes-tidy']);
  expect(recent, 'not the page itself').not.toContain('/agents/docs-sweeper');

  // Pins.
  await pickView(page, 'pins');
  await expect(toggle.locator('[data-switcher-icon]:visible')).toHaveAttribute('data-switcher-icon', 'pins');
  await expect(row(page, 'recent')).toBeHidden();
  await expect(row(page, 'pins')).toBeVisible();
  expect(await hrefs(page, 'pins')).toEqual(['/agents/release-helper']);

  // All: every agent, this one drawn as current (not a link).
  await pickView(page, 'all');
  await expect(toggle.locator('[data-switcher-icon]:visible')).toHaveAttribute('data-switcher-icon', 'all');
  const all = row(page, 'all');
  await expect(all).toBeVisible();
  await expect(all.locator('[data-switcher-current]')).toHaveText('docs-sweeper');
  await expect(all.locator('[data-switcher-current]')).toHaveAttribute('aria-current', 'page');
  expect(await hrefs(page, 'all')).toEqual(expect.arrayContaining(['/agents/release-helper', '/agents/notes-tidy']));
  expect(await hrefs(page, 'all')).not.toContain('/agents/docs-sweeper');

  // Kept: the next page comes up in All, drawn so before it is painted.
  await openAgent(page, 'notes-tidy');
  await expect(toggle.locator('[data-switcher-icon]:visible')).toHaveAttribute('data-switcher-icon', 'all');
  await expect(row(page, 'all')).toBeVisible();
  await expect(row(page, 'recent')).toBeHidden();
  // Back to Recent from the menu.
  await pickView(page, 'recent');
  await expect(toggle.locator('[data-switcher-icon]:visible')).toHaveAttribute('data-switcher-icon', 'recent');

  // A tap is a navigation to that session.
  await Promise.all([page.waitForURL((u) => u.pathname === '/agents/release-helper'), row(page, 'recent').locator('a[href^="/agents/release-helper?"], a[href="/agents/release-helper"]').first().click()]);
});

// Many long names: the bar scrolls sideways inside itself; the page does not.
test('at 390px the bar never widens the page, whatever it holds', async ({ page }) => {
  const stamp = Date.now().toString(36);
  const names = Array.from({ length: 7 }, (_, i) => `e2e-switcher-a-rather-long-agent-name-${i}-${stamp}`);
  for (const n of names) mkchat(n, [{ role: 'user', content: 'hello' }]);
  try {
    await openAgent(page, 'docs-sweeper');
    await page.evaluate(() => localStorage.setItem('muster.switcher.view', 'all'));
    await openAgent(page, 'docs-sweeper');
    await expect(row(page, 'all').locator('a[data-switcher-entry]')).not.toHaveCount(0);
    expect((await hrefs(page, 'all')).length).toBeGreaterThanOrEqual(names.length);
    const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(sw, 'no page-level horizontal overflow').toBeLessThanOrEqual(cw);
    const scroll = await row(page, 'all').evaluate((el) => [el.scrollWidth, el.clientWidth]);
    expect(scroll[0], 'the row itself overflows (the positive control: there IS more than fits)').toBeGreaterThan(scroll[1]);
    const bb = (await bar(page).boundingBox())!;
    expect(bb.x + bb.width, 'the bar ends inside the viewport').toBeLessThanOrEqual(390 + 0.5);
  } finally {
    for (const n of names) mkchat(n, null);
    await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
  }
});

// THE COMPACT BAR at a phone's size (the operator asked for icons in place
// of the view labels, the pin in a three-dots menu, and less padding so more
// of the session pills show): the pills' row gets the width, the view button
// shows the view's icon and changes it with the view, and the ⋯ menu pins the
// session. The Recent row's clientWidth on this page at 390px, measured: 174px
// on the base (main before this change: a 72px labelled view button, 44px
// search and pin, 8px gaps, in the pane's 358px), 226px here (three 44×44
// icon buttons, no gaps). 210 is clear of the base and below this.
test.describe('phone 390×844, the compact bar', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
  test('the pills get the width, the view button\'s icon follows the view, and the ⋯ menu pins the session into Pins', async ({ page }) => {
    await page.addInitScript(() => { try { if (!sessionStorage.getItem('cmp-cleared')) { localStorage.removeItem('muster.switcher.view'); sessionStorage.setItem('cmp-cleared', '1'); } } catch (e) {} });
    await openAgent(page, 'docs-sweeper');
    const id = await agentID(page, 'docs-sweeper');
    await pin(page, id, false);
    await page.reload();
    await expect(bar(page)).toBeVisible();

    // More pill width: the shown row's clientWidth.
    const rowW = await row(page, 'recent').evaluate((el) => el.clientWidth);
    expect(rowW, 'the pills\' row at 390px (174px on the base, 226px here)').toBeGreaterThanOrEqual(210);
    // Every icon button: a 44×44 target (operator decision), inside the bar; the bar inside the page.
    for (const sel of ['#switcher-toggle', '#switcher-search-open', '#switcher-more-btn']) {
      const b = (await page.locator(sel).boundingBox())!;
      expect([Math.round(b.width), Math.round(b.height)], `${sel} is 44×44`).toEqual([44, 44]);
    }
    const bb = (await bar(page).boundingBox())!;
    expect(bb.x + bb.width, 'the bar ends inside the viewport').toBeLessThanOrEqual(390 + 0.5);

    // The view button: an icon, no words, named for the view; it changes with
    // the view and the button does not move or resize.
    const toggle = page.locator('#switcher-toggle');
    const shown = toggle.locator('[data-switcher-icon]:visible');
    await expect(toggle).toHaveText('');
    await expect(shown).toHaveAttribute('data-switcher-icon', 'recent');
    await expect(toggle).toHaveAccessibleName('Showing Recent sessions; choose a view');
    const tb = (await toggle.boundingBox())!;
    for (const [key, name] of [['all', 'All'], ['waiting', 'Waiting'], ['pins', 'Pins']]) {
      await toggle.tap();
      await expect(page.locator(`#switcher-views [data-switcher-pick="${key}"] [data-switcher-menu-icon="${key}"]`), 'the menu draws the icon beside the words').toBeVisible();
      await page.locator(`#switcher-views [data-switcher-pick="${key}"]`).tap();
      await expect(page.locator('#switcher-views')).toBeHidden();
      await expect(shown).toHaveCount(1);
      await expect(shown).toHaveAttribute('data-switcher-icon', key);
      await expect(toggle).toHaveAccessibleName(`Showing ${name} sessions; choose a view`);
      await expect(toggle).toHaveAttribute('title', `Showing ${name} sessions; choose a view`);
      expect(await toggle.boundingBox(), `the button stays put in ${key}`).toEqual(tb);
    }
    // Kept across a load, drawn before paint: Pins' icon, not Recent's.
    await page.reload();
    await expect(shown).toHaveAttribute('data-switcher-icon', 'pins');
    const curPin = row(page, 'pins').locator('[data-switcher-current]');
    await expect(curPin, 'control: this session is not pinned yet').toHaveCount(0);
    const count = page.locator('#switcher-views [data-switcher-count="pins"]');
    const n0 = Number(await count.textContent());

    // The ⋯ menu: opened by a tap, above its button and on screen; Pin pins
    // this session, the menu closes, and the Pins view holds it at once.
    const more = page.locator('#switcher-more-btn');
    const menu = page.locator('#switcher-more');
    const item = page.locator('#switcher-pin');
    await expect(item, 'no pin in the bar itself').toBeHidden();
    await more.tap();
    await expect(menu).toBeVisible();
    await expect(more).toHaveAttribute('aria-expanded', 'true');
    // Measured once its open animation (a rise and a scale) has finished.
    await menu.evaluate((m) => Promise.all(m.getAnimations().map((a) => a.finished)));
    const mb = (await menu.boundingBox())!, ob = (await more.boundingBox())!;
    expect(mb.y + mb.height, 'the menu opens above the ⋯').toBeLessThanOrEqual(ob.y + 0.5);
    expect(mb.x >= 0 && mb.x + mb.width <= 390 + 0.5, 'the menu is on screen').toBe(true);
    await expect(item).toHaveAccessibleName('Pin this session');
    pinned = id;
    await item.tap();
    await expect(menu, 'the menu closes once used').toBeHidden();
    await expect(item).toHaveAttribute('data-pinned', 'true');
    await expect(curPin, 'the Pins view holds this session').toHaveText('docs-sweeper');
    await expect(curPin).toBeVisible();
    await expect(count).toHaveText(String(n0 + 1));
    // And Unpin from the same menu.
    await more.tap();
    await expect(item).toHaveAccessibleName('Unpin this session');
    await item.tap();
    await expect(item).toHaveAttribute('data-pinned', 'false');
    await expect(curPin).toHaveCount(0);
    await expect(count).toHaveText(String(n0));
    await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
  });
});

// LAYOUT SHIFT: the agent page with the bar, loaded in each view (the stored
// choice applied before paint) and with a hidden ask restored, at both sizes.
//
// ⚠ ON notes-tidy, AN AGENT WITH NO TRANSCRIPT, ON PURPOSE. docs-sweeper's page
// shifts by itself at load on main (289986e7: 0.00469 at 1280×800 in 3 of 4
// cls.spec runs, sources #task-banner and the transcript's bubbles, inside
// #chat-log — none of it this bar), and with the bar's 45px taken from the
// log that pre-existing shift read up to 0.010 here. This test measures the
// bar and the asks block; a page without a transcript keeps main's own shift
// out of the number, and cls.spec still measures docs-sweeper's page whole.
for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);
    test(`the agent page with the switcher, in every view and with hidden asks, shifts less than ${BAR}`, async ({ page }) => {
      test.setTimeout(120_000);
      await page.addInitScript(clsInit);
      // A recent entry and a pin, so no view is empty.
      await openAgent(page, 'release-helper');
      pinned = await agentID(page, 'release-helper');
      await pin(page, pinned, true);
      const results: Record<string, ClsResult> = {};
      for (const view of ['recent', 'pins', 'all']) {
        await page.evaluate((v) => localStorage.setItem('muster.switcher.view', v), view);
        await page.goto('/agents/notes-tidy');
        await settle(page);
        results[view] = await readCls(page);
        await expect(row(page, view), `${view} is the row drawn`).toBeVisible();
        await expect(row(page, view).locator('[data-switcher-entry]').first()).toBeVisible();
      }
      // A hidden ask restored on load.
      const q = fix('ask', '1', 'notes-tidy', 'Layout probe?');
      asks.push(q);
      await page.goto('/agents/notes-tidy');
      await block(page).getByRole('button', { name: 'Hide until next ask' }).click();
      await expect(chip(page)).toBeVisible();
      await page.goto('/agents/notes-tidy');
      await settle(page);
      results['a hidden ask restored'] = await readCls(page);
      await expect(chip(page)).toHaveText('1 waiting');
      await expect(block(page)).toBeHidden();
      await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); localStorage.removeItem('muster.asks.hidden.v1'); } catch (e) {} });
      // 🔴 THE POSITIVE CONTROL, on this same page: a block pushed in above the
      // transcript with no input MUST read above the bar, or the zeros above
      // are an instrument that sees nothing.
      await resetCls(page);
      await page.evaluate(() => {
        const d = document.createElement('div');
        d.style.height = '160px';
        const log = document.getElementById('chat-log')!;
        log.parentElement!.insertBefore(d, log);
      });
      await settle(page, 500);
      const control = await readCls(page);
      expect(control.unsupported, 'the browser reports layout-shift entries').toBeUndefined();
      console.log(`[cls ${vp.name}] positive control (an injected shift) ${control.total}`);
      expect(control.total, 'the positive control reads a shift').toBeGreaterThan(BAR);
      console.log(`[cls ${vp.name}] ` + Object.entries(results).map(([k, r]) => `${k} ${r.total}`).join(' | '));
      const bad = Object.entries(results).filter(([, r]) => r.total >= BAR).map(([k, r]) => `${k}: ${r.total} ${JSON.stringify(r.shifts)}`);
      expect(bad).toEqual([]);
    });
  });
}
