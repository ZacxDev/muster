import fs from 'node:fs';
import { Page } from '@playwright/test';
import { test, expect, takeNew } from '../lib/fixtures';
import { agentsHoldPort } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { clsInit, readCls, resetCls } from '../lib/cls';
import { writeFileAtomic } from '../lib/atomic';
import { openMore } from '../lib/nav';

// THE AGENTS LIST HOLDS A BACKGROUND REFRESH BEHIND "N new ↑"
// (internal/ui/agents_filter.go): a refresh that would move a card the reader
// can see waits for a tap, and nothing it changes in place moves a card,
// closes a popover or blurs the focused element.
//
// ⚠ THIS SPEC ALSO TESTED THE LIST'S SEARCH BOX, which ranked the cards as you
// typed. The box is gone (operator ask: "Hide the existing search input
// because the new command palette UX supersedes it"): its ranking tests, and
// the hold tests that only existed because of the rank (a typed filter
// re-ranking a held refresh), are removed; the session switcher's palette is
// agents-switcher.spec.ts and switcher-search.spec.ts.
//
// The cards are three mirrored sessions of this spec's own host (qbase,
// old-qdock, mqtail), listed most recently active first: mqtail, old-qdock,
// qbase.

const rig = new MirrorRig(agentsHoldPort, 'e2e-search', {}, { decimalHost: true });

test.use({ storageState: { cookies: [], origins: [] } });

let windowsFile = '';
const t0 = Math.floor(Date.now() / 1000);
const windowLine = (session: string, id: string, command: string, ago: number) => `${session}|${id}|1|1|${command}|${t0 - ago}|`;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  await rig.start();
  const now = t0;
  windowsFile = rig.transcript('', 'windows');
  // session|id|index|active|command|activity|claude-transcript — newest first by activity.
  writeFileAtomic(
    windowsFile,
    [`qbase|@1|1|1|zsh|${now - 3000}|`, `old-qdock|@2|1|1|zsh|${now - 2000}|`, `mqtail|@3|1|1|zsh|${now - 1000}|`].join('\n') + '\n',
  );
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile });
});

test.afterAll(async () => {
  await rig.stop();
});

const names = ['qbase', 'old-qdock', 'mqtail'];

// shown is the visible cards of this host, in the order they are DRAWN (by
// box: top to bottom, then left to right), as their session names.
async function shown(page: Page): Promise<string[]> {
  return page.locator('#agents-list > article').evaluateAll((els, ns) => {
    const vis = els
      .map((e) => ({ e, r: e.getBoundingClientRect() }))
      .filter(({ r }) => r.width > 0 && r.height > 0)
      .sort((a, b) => a.r.top - b.r.top || a.r.left - b.r.left);
    return vis.flatMap(({ e }) => {
      const t = (e.getAttribute('data-f-session') || '').trim();
      return (ns as string[]).includes(t) ? [t] : [];
    });
  }, names);
}

// HELD, NOT MOVED (the operator's decision; tap-only, even at the top): a
// background refresh of the open list — an activity change, an arrival — moves
// no card. The new order waits behind an "N new ↑" pill and a tap applies it. A
// card's own content still updates in place. It rewrites the host's windows.
test.describe('held behind the pill', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('an activity change and an arrival wait behind "N new ↑"; nothing moves; content updates in place; a tap applies', async ({ page }) => {
    test.setTimeout(120_000);
    await page.addInitScript(clsInit);
    await rig.login(page);
    await rig.waitListed(page, 'mqtail');
    await page.goto(rig.origin + '/agents');
    const domOrder = () =>
      page.locator('#agents-list > article:not([data-held])').evaluateAll((els, ns) =>
        els.map((e) => e.getAttribute('data-f-session') || '').filter((n) => (ns as string[]).includes(n)), [...names, 'qzest']);
    await expect.poll(domOrder).toEqual(['mqtail', 'old-qdock', 'qbase']);
    const pill = page.locator('#agents-new-pill');
    await expect(pill).toBeHidden();
    await resetCls(page);

    // qbase becomes the most recently active, and its command changes.
    writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'vim', 0), windowLine('old-qdock', '@2', 'zsh', 2040), windowLine('mqtail', '@3', 'zsh', 1020)].join('\n') + '\n');
    await expect(pill, 'the change waits behind the pill').toHaveText('1 new ↑', { timeout: 30_000 });
    expect(await domOrder(), 'held: no card moved').toEqual(['mqtail', 'old-qdock', 'qbase']);
    await expect(page.locator('#agents-list > article[data-f-session="qbase"]'), 'its content updated in place').toHaveAttribute('data-f-kind', 'vim');

    // An arrival: kept out of view, counted by the pill — and NOT counted by
    // its host's tab while it is held: a tab counts what is drawn.
    const hostCount = page.locator(`#agents-hosts [role="tab"][data-host="${rig.host}"] [data-tab-count]`);
    await expect(hostCount, 'positive control: the host tab counts its drawn cards').toHaveText('3');
    writeFileAtomic(windowsFile, fs.readFileSync(windowsFile, 'utf8') + windowLine('qzest', '@4', 'qtool', 0) + '\n');
    await expect(pill).toHaveText('2 new ↑', { timeout: 30_000 });
    await expect(page.locator('#agents-list > article[data-f-session="qzest"]')).toBeHidden();
    await expect(hostCount, 'a held arrival is not counted by its host tab').toHaveText('3');
    expect(await domOrder()).toEqual(['mqtail', 'old-qdock', 'qbase']);
    const held = await readCls(page);
    expect(held.total, `nothing moved while held: ${JSON.stringify(held.shifts)}`).toBeLessThan(0.01);

    // A tap applies the live order, arrival included, and the pill goes.
    await pill.tap();
    await expect(pill).toBeHidden();
    await expect.poll(domOrder, { message: 'the live order, applied' }).toEqual(['qbase', 'qzest', 'mqtail', 'old-qdock']);
    await expect(hostCount, 'drawn, it is counted').toHaveText('4');
  });
});

// The rest of the pill's contract, on a keyboard-shaped desktop. Each test
// writes the host's windows back to a known order first. Runs after the test
// above (it rewrites the windows too).
const sessionsOrder = (page: Page) =>
  page.locator('#agents-list > article:not([data-held])').evaluateAll((els, ns) =>
    els.map((e) => e.getAttribute('data-f-session') || '').filter((n) => (ns as string[]).includes(n)), names);

async function freshList(page: Page, lines: string[], want: string[]) {
  writeFileAtomic(windowsFile, lines.join('\n') + '\n');
  await rig.login(page);
  await page.goto(rig.origin + '/agents');
  await expect
    .poll(async () => {
      await takeNew(page);
      return sessionsOrder(page);
    }, { timeout: 30_000, message: 'the host reported the written windows' })
    .toEqual(want);
  await expect(page.locator('#agents-list > article[data-f-session="qzest"]')).toHaveCount(0);
}

for (const leg of [
  { name: 'moveBefore', strip: false },
  { name: 'no moveBefore', strip: true },
]) {
  test.describe(`held behind the pill, desktop (${leg.name})`, () => {
    test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

    // 🔴 A REGRESSION TEST (round-2 audit): reorder() re-appended EVERY card, so
    // a card holding focus was detached and focus fell to <body>. A refresh that
    // lands while a card is focused, and the pill's tap that moves that very
    // card, must both leave focus where it was.
    test('a focused card keeps focus through a held refresh and through the pill tap that moves it', async ({ page }) => {
      test.setTimeout(120_000);
      // Without Element.moveBefore (Firefox and Safari today) the hand-back must
      // not depend on a browser that keeps focus through a move by itself.
      if (leg.strip) await page.addInitScript(() => { delete (Element.prototype as unknown as { moveBefore?: unknown }).moveBefore; });
      await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
      expect(await page.evaluate(() => typeof (Element.prototype as unknown as { moveBefore?: unknown }).moveBefore), 'the leg runs the mover it names').toBe(leg.strip ? 'undefined' : 'function');
      // Tab starts from the host tabs' All tab (above the list; the search box
      // that was the start is gone).
      const box = page.locator('#agents-host-all');
      const pill = page.locator('#agents-new-pill');

      // Tab from the All tab into a card; mark the element and count its blurs.
      const tabInto = async (session: string) => {
        await box.focus();
        for (let i = 0; i < 60; i++) {
          await page.keyboard.press('Tab');
          if (await page.evaluate((s) => !!document.activeElement?.closest(`#agents-list > article[data-f-session="${s}"]`), session)) break;
        }
        const ok = await page.evaluate(() => {
          const w = window as unknown as { __f?: Element; __blurs: number };
          w.__f = document.activeElement ?? undefined;
          w.__blurs = 0;
          w.__f?.addEventListener('blur', () => w.__blurs++);
          return !!w.__f && w.__f !== document.body;
        });
        expect(ok, `Tab reached the ${session} card`).toBe(true);
      };
      const stillFocused = () =>
        page.evaluate(() => {
          const w = window as unknown as { __f?: Element; __blurs: number };
          return { same: document.activeElement === w.__f, connected: !!w.__f?.isConnected, blurs: w.__blurs, now: document.activeElement?.outerHTML.slice(0, 80) };
        });

      // 1. Focus in a card, and a refresh that re-orders the list (qbase
      //    becomes the newest, its command changes).
      await tabInto('mqtail');
      writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'vim', 0), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)].join('\n') + '\n');
      await expect(page.locator('#agents-list > article[data-f-session="qbase"]'), 'the refresh landed').toHaveAttribute('data-f-kind', 'vim', { timeout: 30_000 });
      await expect(pill).toBeVisible();
      expect(await stillFocused(), 'focus survived the refresh').toMatchObject({ same: true, connected: true });
      // The tap applies the live order and does not take focus.
      await pill.click();
      await expect(pill).toBeHidden();
      expect(await stillFocused(), 'focus survived the pill tap').toMatchObject({ same: true, connected: true });

      // 2. Focus in the LAST card, and a refresh that makes it the newest: the
      //    pill's tap moves the focused card itself to the top.
      await expect.poll(() => sessionsOrder(page)).toEqual(['qbase', 'mqtail', 'old-qdock']);
      await tabInto('old-qdock');
      writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'vim', 600), windowLine('old-qdock', '@2', 'zsh', 0), windowLine('mqtail', '@3', 'zsh', 1000)].join('\n') + '\n');
      await expect(pill, 'the re-order waits').toBeVisible({ timeout: 30_000 });
      expect(await stillFocused(), 'focus survived the second refresh').toMatchObject({ same: true, connected: true });
      await pill.click();
      await expect.poll(() => sessionsOrder(page), { message: 'the tap moved the focused card' }).toEqual(['old-qdock', 'qbase', 'mqtail']);
      const after = await stillFocused();
      expect(after, 'focus is on the moved card').toMatchObject({ same: true, connected: true });
      // moveBefore moves the card without detaching it: no blur. Without it the
      // move detaches the card, so it blurs and is refocused — a transient blur
      // the hand-back repairs. Counted, not hidden.
      if (leg.strip) expect(after.blurs, 'the moved card was detached and refocused').toBeGreaterThanOrEqual(1);
      else expect(after.blurs, 'moveBefore moved it without a blur').toBe(0);
    });
  });
}

// A change that moves nothing the reader can see — the whole list below the
// screen — is applied at once: no pill, no shift.
test.describe('held behind the pill: off screen', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

  test('a re-order wholly below the screen is applied at once, with no pill and no shift', async ({ page }) => {
    test.setTimeout(120_000);
    await page.addInitScript(clsInit);
    await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
    // Push the whole list below the fold (a test-only spacer above it).
    await page.evaluate(() => {
      const list = document.getElementById('agents-list')!;
      const d = document.createElement('div');
      d.id = 'test-spacer';
      d.style.height = '3000px';
      list.parentElement!.insertBefore(d, list);
      window.scrollTo(0, 0);
    });
    expect(await page.evaluate(() => document.getElementById('agents-list')!.getBoundingClientRect().top), 'the list is below the screen').toBeGreaterThan(800);
    await resetCls(page);
    writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'vim', 0), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)].join('\n') + '\n');
    await expect(page.locator('#agents-list > article[data-f-session="qbase"]'), 'the refresh landed').toHaveAttribute('data-f-kind', 'vim', { timeout: 30_000 });
    await expect.poll(() => sessionsOrder(page), { message: 'applied at once' }).toEqual(['qbase', 'mqtail', 'old-qdock']);
    await expect(page.locator('#agents-new-pill')).toBeHidden();
    const c = await readCls(page);
    expect(c.total, `nothing in view moved: ${JSON.stringify(c.shifts)}`).toBeLessThan(0.01);
  });
});

// 🔴 A REGRESSION TEST (round-1 audit 🔴): the pill was built once and kept in
// a variable. A boosted navigation swaps the body and Back restores a snapshot
// of it, so on the SECOND boosted entry to the Agents tab the variable named a
// detached node: an arrival was held hidden with no pill in the document to tap.
test.describe('held behind the pill: across navigations', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

  test('after detail → Agents → Back → Agents, a held arrival still has its pill, and the tap applies it', async ({ page }) => {
    test.setTimeout(120_000);
    await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
    const agentsLink = page.locator('.sidebar-tab[data-tab="agents"]');
    // A detail page, by a full load: no Agents panel, so its Agents link is a
    // boosted navigation that swaps the body.
    await page.goto(rig.origin + '/tasks/1');
    await openMore(page);
    await expect(agentsLink).toBeVisible();
    const pill = page.locator('#agents-new-pill');
    // First entry: a held change, so the pill is MADE on this body, and tapped.
    await agentsLink.click();
    await expect(page, 'first entry').toHaveURL(/\/agents$/);
    await expect.poll(() => sessionsOrder(page), { message: 'first entry' }).toEqual(['mqtail', 'old-qdock', 'qbase']);
    writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'vim', 0), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)].join('\n') + '\n');
    await expect(pill, 'the first entry made its pill').toHaveText('1 new ↑', { timeout: 30_000 });
    await pill.click();
    await expect.poll(() => sessionsOrder(page)).toEqual(['qbase', 'mqtail', 'old-qdock']);
    // Back to the detail page (a history restore), and Agents again (boosted).
    await page.goBack();
    await expect(page).toHaveURL(/\/tasks\/1$/);
    await expect(agentsLink).toBeVisible();
    await agentsLink.click();
    await expect(page, 'second entry').toHaveURL(/\/agents$/);
    await expect.poll(() => sessionsOrder(page), { message: 'second entry' }).toEqual(['qbase', 'mqtail', 'old-qdock']);
    // An arrival and a re-order, from the host: held, with ONE pill to tap.
    writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'vim', 600), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 0), windowLine('qzest', '@4', 'zsh', 5)].join('\n') + '\n');
    const zest = page.locator('#agents-list > article[data-f-session="qzest"]');
    await expect(zest, 'the arrival reached the list').toHaveCount(1, { timeout: 30_000 });
    await expect(zest, 'held').toBeHidden();
    await expect(pill, 'one pill in the document').toHaveCount(1);
    await expect(pill, 'the held arrival has its pill').toHaveText('2 new ↑');
    await pill.click();
    await expect(zest, 'the tap drew it').toBeVisible();
    await expect(pill).toBeHidden();
  });
});

// 🔴 A REGRESSION TEST (round-1 audit 🟡): the pill is fixed on <body>, and
// nothing hid it when the Agents tab was: after switching to Tasks it floated
// over the board until the next refresh. It must go with the tab, and come
// back with its count when the tab does — by the sidebar and by Back.
test.describe('held behind the pill: other tabs', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

  test('the pill hides on another tab and comes back, count intact, with the Agents tab', async ({ page }) => {
    test.setTimeout(120_000);
    await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
    const pill = page.locator('#agents-new-pill');
    writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000), windowLine('qzest', '@4', 'zsh', 5)].join('\n') + '\n');
    await expect(pill, 'the arrival waits').toHaveText('1 new ↑', { timeout: 30_000 });
    await expect(pill).toBeVisible();

    await openMore(page);
    await page.locator('.sidebar-tab[data-tab="tasks"]').click();
    await expect(page).toHaveURL(/\/tasks$/);
    await expect(page.locator('#panel-tasks')).toBeVisible();
    await expect(pill, 'not over the Tasks board').toBeHidden();

    await page.locator('.sidebar-tab[data-tab="agents"]').click();
    await expect(page).toHaveURL(/\/agents$/);
    await expect(pill, 'back with the tab').toBeVisible();
    await expect(pill, 'its count intact').toHaveText('1 new ↑');

    // Back (to Tasks) and Forward (to Agents) do the same.
    await page.goBack();
    await expect(page).toHaveURL(/\/tasks$/);
    await expect(pill, 'not over the Tasks board after Back').toBeHidden();
    await page.goForward();
    await expect(page).toHaveURL(/\/agents$/);
    await expect(pill, 'back after Forward').toHaveText('1 new ↑');
    await expect(pill).toBeVisible();
  });
});

// 🔴 A REGRESSION TEST (round-1 audit 🟡): while the pill was up, EVERY
// background refresh moved every card — idiomorph put them in the live order
// and the hold put them back — so a refresh that changed only a card's
// content still closed an open popover and blurred (then refocused) the
// focused element. Measured by the audit: 6 blurs in 3 refreshes, and a badge
// popover that survives at the base closed at the head. A content-only refresh
// while held must move nothing: the popover stays open, the focused element
// gets no blur — with Element.moveBefore and without it (Firefox/Safari today).
for (const leg of [
  { name: 'moveBefore', strip: false },
  { name: 'no moveBefore', strip: true },
]) {
  test.describe(`held behind the pill: a content-only refresh moves nothing (${leg.name})`, () => {
    test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

    test('an open popover stays open', async ({ page }) => {
      test.setTimeout(120_000);
      if (leg.strip) await page.addInitScript(() => { delete (Element.prototype as unknown as { moveBefore?: unknown }).moveBefore; });
      await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
      const pill = page.locator('#agents-new-pill');
      const card = (s: string) => page.locator(`#agents-list > article[data-f-session="${s}"]`);
      const write = (qbaseCmd: string, mqtailCmd: string) =>
        writeFileAtomic(windowsFile, [windowLine('qbase', '@1', qbaseCmd, 0), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', mqtailCmd, 1000)].join('\n') + '\n');

      // Held: qbase becomes the newest, and waits behind the pill.
      write('zsh', 'zsh');
      await expect(pill).toHaveText('1 new ↑', { timeout: 30_000 });
      expect(await sessionsOrder(page)).toEqual(['mqtail', 'old-qdock', 'qbase']);

      // A popover open on a held card (mqtail's "Mirrored" mark), and a
      //    refresh that changes only a card's content.
      await card('mqtail').locator('[popovertarget]').first().click();
      const open = () => card('mqtail').locator(':popover-open').count();
      expect(await open(), 'control: the popover opened').toBe(1);
      write('zsh', 'vim');
      await expect(card('mqtail'), 'the content-only refresh landed').toHaveAttribute('data-f-kind', 'vim', { timeout: 30_000 });
      expect(await open(), 'the popover survived the refresh').toBe(1);
    });

    test('the focused element is never blurred', async ({ page }) => {
      test.setTimeout(120_000);
      if (leg.strip) await page.addInitScript(() => { delete (Element.prototype as unknown as { moveBefore?: unknown }).moveBefore; });
      await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
      const pill = page.locator('#agents-new-pill');
      const card = (s: string) => page.locator(`#agents-list > article[data-f-session="${s}"]`);
      const write = (qbaseCmd: string, mqtailCmd: string) =>
        writeFileAtomic(windowsFile, [windowLine('qbase', '@1', qbaseCmd, 0), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', mqtailCmd, 1000)].join('\n') + '\n');
      write('zsh', 'vim');
      await expect(pill).toHaveText('1 new ↑', { timeout: 30_000 });
      await expect(card('mqtail')).toHaveAttribute('data-f-kind', 'vim', { timeout: 30_000 });

      // Focus on an element of a held card, and a content-only refresh.
      await card('old-qdock').locator('a[href]').first().focus();
      await page.evaluate(() => {
        const w = window as unknown as { __f?: Element; __blurs: number };
        w.__f = document.activeElement ?? undefined;
        w.__blurs = 0;
        w.__f?.addEventListener('blur', () => w.__blurs++);
      });
      write('vim', 'zsh');
      await expect(card('mqtail'), 'the second refresh landed').toHaveAttribute('data-f-kind', 'shell', { timeout: 30_000 });
      await expect(card('qbase')).toHaveAttribute('data-f-kind', 'vim');
      const f = await page.evaluate(() => {
        const w = window as unknown as { __f?: Element; __blurs: number };
        return { same: document.activeElement === w.__f, blurs: w.__blurs };
      });
      expect(f, 'the focused element was never blurred').toEqual({ same: true, blurs: 0 });
      expect(await sessionsOrder(page), 'still held').toEqual(['mqtail', 'old-qdock', 'qbase']);
      await expect(pill).toHaveText('1 new ↑');
    });

    // 🔴 A REGRESSION TEST (round-2 audit 🟡): a held (hidden) arrival sorted
    // above drawn cards made arrange re-append every card — without moveBefore
    // that closed an open popover (audit: 7 nodes removed). A held arrival is
    // sorted last, so nothing drawn moves. (The audit's case was a typed
    // filter's rank; the box is gone, and the newest arrival is the same
    // hazard: the list's own order puts it first.)
    test('a held arrival moves no card and an open popover stays open', async ({ page }) => {
      test.setTimeout(120_000);
      if (leg.strip) await page.addInitScript(() => { delete (Element.prototype as unknown as { moveBefore?: unknown }).moveBefore; });
      await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
      const pill = page.locator('#agents-new-pill');
      const card = (s: string) => page.locator(`#agents-list > article[data-f-session="${s}"]`);
      // A held state first (qbase becomes the newest).
      writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'zsh', 10), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)].join('\n') + '\n');
      await expect(pill).toHaveText('1 new ↑', { timeout: 30_000 });
      await expect.poll(() => sessionsOrder(page)).toEqual(['mqtail', 'old-qdock', 'qbase']);
      // A popover open on a drawn card, and every card removal counted.
      await card('mqtail').locator('[popovertarget]').first().click();
      const open = () => card('mqtail').locator(':popover-open').count();
      expect(await open(), 'control: the popover opened').toBe(1);
      await page.evaluate(() => {
        const w = window as unknown as { __removed: number };
        w.__removed = 0;
        new MutationObserver((ms) => ms.forEach((m) => m.removedNodes.forEach((n) => { if ((n as Element).tagName === 'ARTICLE') w.__removed++; })))
          .observe(document.getElementById('agents-list')!, { childList: true });
      });
      // The arrival: the newest of all, so the live order puts it first — held.
      writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'zsh', 10), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000), windowLine('qzest', '@4', 'zsh', 0)].join('\n') + '\n');
      await expect(card('qzest'), 'the arrival reached the list').toHaveCount(1, { timeout: 30_000 });
      await expect(card('qzest'), 'held').toBeHidden();
      await expect(pill).toHaveText('2 new ↑');
      const removed = await page.evaluate(() => (window as unknown as { __removed: number }).__removed);
      expect(await open(), 'the popover survived the held arrival').toBe(1);
      expect(removed, 'no card was removed and re-added').toBe(0);
      expect(await sessionsOrder(page), 'the drawn cards did not move').toEqual(['mqtail', 'old-qdock', 'qbase']);
    });
  });
}
