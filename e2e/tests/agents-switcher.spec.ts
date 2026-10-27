import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { agentsSwitcherPort, hookToken } from '../lib/env';
import { clsInit, readCls, settle } from '../lib/cls';
import { cc, MirrorRig, mkchat } from '../lib/mirrorhost';
import { openMore } from '../lib/nav';

// THE SESSION SWITCHER ON THE AGENTS LIST (internal/ui/switcher.go
// switcherDock), on a REAL muster-server and a REAL muster-mirror
// (`-tags e2emirror`, which fakes tmux only) whose windows carry codenames,
// plus one dispatched agent. The operator asks:
//
//   "Include the bottom bar on the main agent list page as well. Hide the
//    existing search input because the new command palette UX supersedes it.
//    Also hide the pinned and recent on the main agent list page because the
//    bottom bar flow supersedes it."
//   "Also hide the agent replies FAB and deploy agent FAB on agent list page"
//
// so, at a phone (390×844, touch) and a desktop (1280×800) size:
//
//   1. the bar is docked at the bottom of /agents and paints with the page (no
//      layout shift, no sideways page scroll), and the list's search box, its
//      "Recently viewed" strip, its pinned chips and both FABs are gone;
//   2. the list's last card scrolls clear of the bar;
//   3. the view menu works there, and a pin made on a card reaches its Pins
//      view without a reload; nothing is drawn current (the page has no
//      session of its own);
//   4. the search palette ("/" and the magnifier) finds a window by its
//      codename and Enter opens it;
//   5. a filter text a browser stored for the removed search box (or a ?q=)
//      hides no card;
//   6. the FABs are still on the Tasks tab, and a client-side switch to Agents
//      hides the replies FAB and fills the bar;
//   7. the bar's "+" (operator: deploy "In the bottom bar") opens the
//      task-less dispatch sheet the old FAB opened, and a dispatch from it
//      creates an agent; a session page's bar has no "+";
//   8. the palette also finds an entry by its repository, its kind and a
//      window's raw tmux session name — after every codename and title match
//      (operator: "Teach the palette repo+kind").
//
// 🔴 REGRESSION TESTS: at origin/main (baf851ac) /agents has no bar, draws the
// search box, the strip and the chips, and both FABs.
test.use({ storageState: { cookies: [], origins: [] } });

const rig = new MirrorRig(agentsSwitcherPort, 'e2e-asw', {}, { decimalHost: true });
const origin = rig.origin;
const stamp = Date.now().toString(36);
const AGENT = `e2e-asw-ledger-${stamp}`;
const AGENT_TITLE = 'Rotate the ledger';
// Its repository: found by the palette's words, ranked after "wheat"'s codename.
const AGENT_REPO = 'acme/wheatfield';
// Two more windows: one whose codename is NOT its tmux session (a labels file
// names session scratch11 "orchid"), one running opencode (its kind).
const SCRATCH = 'scratch11';
const NIMBUS = 'nimbus';

// Enough windows that the list scrolls at both sizes, so "the last card is
// clear of the bar" is measured with the list scrolled to its end.
const WINDOWS: [string, string][] = [
  ['mango', '@1'],
  ['wheat', '@2'],
  ['vapor', '@3'],
  ['quartz', '@4'],
  ['ember', '@5'],
  ['tundra', '@6'],
  ['saffron', '@7'],
  ['cobalt', '@8'],
  ['lichen', '@9'],
  ['marble', '@10'],
];

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
  const now = Math.floor(Date.now() / 1000);
  const lines = WINDOWS.map(([s, w], i) => {
    const tr = rig.transcript(cc.prompt(`the prompt of ${s}`) + cc.reply(`Answered in ${s}.`), 'w' + i);
    return `${s}|${w}|1|1|claude|${now - 60 * (i + 1)}|${tr}`;
  });
  lines.push(`${SCRATCH}|@11|1|1|zsh|${now - 60 * 20}|`, `${NIMBUS}|@12|1|1|opencode|${now - 60 * 21}|`);
  const labels = rig.transcript(JSON.stringify({ [SCRATCH]: 'orchid' }), 'labels.json');
  await rig.startHost({ MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_E2E_WINDOWS_FILE: rig.transcript(lines.join('\n') + '\n', 'windows'), MUSTER_MIRROR_LABELS_FILE: labels, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
  mkchat(AGENT, [{ role: 'user', content: 'hello' }], { MUSTER_E2E_DISPLAY_NAME: AGENT_TITLE, MUSTER_E2E_REPO: AGENT_REPO });
});

test.afterAll(async () => {
  mkchat(AGENT, null);
  await rig.stop();
});

const bar = (page: Page) => page.locator('#session-switcher');
const dock = (page: Page) => page.locator('#agents-switcher-dock');
const toggle = (page: Page) => page.locator('#switcher-toggle');
const menu = (page: Page) => page.locator('#switcher-views');
const pal = (page: Page) => page.locator('#switcher-search');
const input = (page: Page) => page.locator('#switcher-search-input');
const options = (page: Page) => page.locator('#switcher-search-results [role="option"]');
const row = (page: Page, key: string) => page.locator(`#session-switcher [data-switcher-list="${key}"]`);
const cards = (page: Page) => page.locator('#agents-list > article');
// listed is how many entries the list holds: these windows, this agent, and
// the seeded fixture's own agents (e2e/seed) — every one a card, and every one
// an entry of the switcher's All view.
async function listed(page: Page): Promise<number> {
  await expect.poll(() => cards(page).count(), { timeout: 15_000 }).toBeGreaterThanOrEqual(WINDOWS.length + 3);
  return cards(page).count();
}

async function ready(page: Page) {
  await rig.login(page);
  await rig.waitListed(page, 'marble');
  // Each test starts from no stored view, no pins.
  await page.addInitScript(() => {
    try {
      if (!sessionStorage.getItem('asw-cleared')) {
        localStorage.removeItem('muster.switcher.view');
        sessionStorage.setItem('asw-cleared', '1');
      }
    } catch (e) {}
  });
}

// openAgents loads /agents with layout shift recorded from the first paint.
async function openAgents(page: Page, path = '/agents') {
  await page.addInitScript(clsInit);
  await page.goto(origin + path);
  await expect(bar(page)).toBeVisible();
  await settle(page);
}

// noSidewaysScroll: the page itself never scrolls sideways (the bar's rows
// scroll inside themselves).
async function expectNoSidewaysScroll(page: Page) {
  const w = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth }));
  expect(w.sw, `the page is ${w.sw}px wide in a ${w.cw}px viewport`).toBeLessThanOrEqual(w.cw);
}

// unpinAll leaves nothing pinned (the pins are the viewer's, server-side).
async function unpinAll(page: Page) {
  const pinned = page.locator('#agents-list > article[data-pinned]');
  for (let n = await pinned.count(); n > 0; n--) {
    await pinned.first().locator('[data-pin-toggle]').click();
    await expect(pinned).toHaveCount(n - 1);
  }
}

for (const vp of [
  { name: 'phone 390×844', w: 390, h: 844, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', w: 1280, h: 800, use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('the bar is docked at the bottom of /agents and paints with the page; the search box, the strip, the chips and both FABs are gone', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await openAgents(page);
      // Docked: the bar's bottom is the viewport's (no safe-area inset here),
      // full width on a phone, right of the sidebar from lg up.
      const box = await bar(page).boundingBox();
      expect(box, 'the bar has a box').not.toBeNull();
      expect(Math.round(box!.y + box!.height), 'the bar sits on the bottom edge').toBe(vp.h);
      expect(box!.height, 'the bar is its fixed 45px row').toBeCloseTo(45, 0);
      expect(await dock(page).evaluate((e) => getComputedStyle(e).position)).toBe('fixed');
      expect(box!.x, vp.w >= 1024 ? 'the bar starts right of the sidebar' : 'the bar starts at the left edge').toBeGreaterThanOrEqual(vp.w >= 1024 ? 288 : 0);
      await expect(row(page, 'recent')).toBeVisible();
      // The superseded UI is not drawn at all.
      await expect(page.locator('#agents-search')).toHaveCount(0);
      await expect(page.locator('#agents-search-clear')).toHaveCount(0);
      await expect(page.locator('#agents-recent')).toHaveCount(0);
      await expect(page.locator('[data-pinned-chip]')).toHaveCount(0);
      await expect(page.locator('#agents-pins-edit')).toHaveCount(0);
      await expect(page.locator('#fab-agents')).toHaveCount(0);
      await expect(page.locator('#notif-fab')).toBeHidden();
      // KEPT: the host tabs and the quick-filter chips.
      await expect(page.locator('#agents-hosts')).toBeVisible();
      await expect(page.locator('#agents-chips')).toBeVisible();
      // Nothing is the page's own session.
      await expect(page.locator('#session-switcher [data-switcher-current]')).toHaveCount(0);
      // The load moved nothing, and the page does not scroll sideways.
      const cls = await readCls(page);
      expect(cls.unsupported, 'the browser reports layout-shift entries').toBeUndefined();
      // The suite's load bar (cls.spec.ts: under 0.01) — and nothing that
      // shifted is the bar or anything in it: it is out of flow and its room
      // is reserved in the panel's first byte.
      expect(cls.total, `layout shift on load ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);
      for (const sh of cls.shifts) {
        for (const src of sh.sources) expect(src, `a shift's source is the switcher: ${JSON.stringify(cls.shifts)}`).not.toMatch(/session-switcher|agents-switcher-dock|switcher-/);
      }
      await expectNoSidewaysScroll(page);
    });

    test('the last card scrolls clear of the bar', async ({ page }) => {
      await ready(page);
      await openAgents(page);
      await listed(page);
      const scrolls = await page.evaluate(() => document.documentElement.scrollHeight > window.innerHeight);
      expect(scrolls, 'instrument check: the list is long enough to scroll, so its end is measured scrolled').toBe(true);
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      await page.waitForTimeout(300);
      const m = await page.evaluate(() => {
        const arts = [...document.querySelectorAll('#agents-list > article')].filter((a) => a.getClientRects().length);
        const lastBottom = Math.max(...arts.map((a) => a.getBoundingClientRect().bottom));
        return { lastBottom, barTop: document.getElementById('session-switcher')!.getBoundingClientRect().top };
      });
      expect(m.lastBottom, `the last card's bottom (${m.lastBottom}) is above the bar's top (${m.barTop})`).toBeLessThanOrEqual(m.barTop);
      await expectNoSidewaysScroll(page);
    });

    test('the view menu works on /agents, and a pin made on a card reaches the Pins view without a reload', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await openAgents(page);
      await unpinAll(page);
      await expect(toggle(page)).toHaveText('Recent');
      // All holds every entry of the list, none of them current.
      await toggle(page).click();
      await expect(menu(page)).toBeVisible();
      await menu(page).locator('[data-switcher-pick="all"]').click();
      await expect(menu(page)).toBeHidden();
      await expect(toggle(page)).toHaveText('All');
      await expect(row(page, 'all')).toBeVisible();
      await expect(row(page, 'all').locator('a[data-switcher-entry]')).toHaveCount(await listed(page));
      await expect(row(page, 'all').locator('a[data-switcher-entry]', { hasText: AGENT_TITLE })).toHaveCount(1);
      // Pins: nothing pinned yet.
      await toggle(page).click();
      await menu(page).locator('[data-switcher-pick="pins"]').click();
      await expect(row(page, 'pins')).toHaveText('Nothing pinned');
      await expect(menu(page).locator('[data-switcher-count="pins"]')).toHaveText('0');
      // Pin wheat from its card: the list's own refresh carries the bar's rows
      // (switcherRefresh), so the Pins view shows it — still the shown view.
      const wheat = page.locator('#agents-list > article[data-mirror-card][data-f-session="wheat"]');
      await wheat.locator('[data-pin-toggle]').click();
      await expect(page.locator('#agents-list > article[data-pinned]')).toHaveCount(1);
      await expect(row(page, 'pins').locator('a[data-switcher-entry]')).toHaveText(['wheat:1']);
      // The refresh put the entries IN the row — its children, not a copy of
      // the row nested in it (htmx's own out-of-band innerHTML swap did that).
      await expect(row(page, 'pins').locator(':scope > a[data-switcher-entry]')).toHaveCount(1);
      await expect(page.locator('[id="switcher-list-pins"]')).toHaveCount(1);
      await expect(page.locator('[id="switcher-count-pins"]')).toHaveCount(1);
      await expect(row(page, 'pins')).toBeVisible();
      await expect(toggle(page)).toHaveText('Pins');
      await expect(menu(page).locator('[data-switcher-count="pins"]')).toHaveText('1');
      // The pinned entry is its CARD, first, and its toggle unpins it.
      await expect(cards(page).first()).toHaveAttribute('data-pinned', '');
      await expect(cards(page).first().locator('[data-pin-toggle]')).toHaveAttribute('aria-pressed', 'true');
      await cards(page).first().locator('[data-pin-toggle]').click();
      await expect(page.locator('#agents-list > article[data-pinned]')).toHaveCount(0);
      await expect(row(page, 'pins')).toHaveText('Nothing pinned');
      await expectNoSidewaysScroll(page);
    });

    test('the search palette: "/" and the magnifier open it, a codename finds its window, Enter opens it', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await openAgents(page);
      await page.locator('body').click({ position: { x: 5, y: 5 } }).catch(() => {});
      await page.keyboard.press('/');
      await expect(pal(page)).toBeVisible();
      await expect(input(page)).toBeFocused();
      // Every entry, none excluded (the page has no session of its own).
      await expect(options(page)).toHaveCount(await listed(page));
      await input(page).pressSequentially('quar');
      await expect(options(page)).toHaveCount(1);
      await expect(options(page).first()).toContainText('quartz');
      await page.keyboard.press('Escape');
      await expect(pal(page)).toBeHidden();
      // The magnifier opens it too; Enter opens the highlighted result.
      await page.locator('#switcher-search-open').click();
      await expect(pal(page)).toBeVisible();
      await input(page).pressSequentially('saff');
      await Promise.all([
        page.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('session') === 'saffron'),
        page.keyboard.press('Enter'),
      ]);
    });

    test('the palette finds an entry by its repository, its kind and its tmux session, after every codename match', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await openAgents(page);
      await page.locator('#switcher-search-open').click();
      const labels = () => options(page).evaluateAll((els) => els.map((e) => (e.firstElementChild as HTMLElement).textContent));
      // The tmux session name: the window called "orchid".
      await input(page).fill(SCRATCH);
      await expect.poll(labels).toEqual(['orchid']);
      // The kind.
      await input(page).fill('opencode');
      await expect.poll(labels).toEqual([`${NIMBUS}:1`]);
      // The repository — and a codename match ranks first: "wheat" is the
      // window's codename prefix, and only a word of the agent's repository.
      await input(page).fill('wheatfield');
      await expect.poll(labels).toEqual([AGENT_TITLE]);
      await input(page).fill('wheat');
      await expect.poll(labels).toEqual(['wheat:1', AGENT_TITLE]);
      // Enter opens the top one: the codename match.
      await Promise.all([
        page.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('session') === 'wheat'),
        page.keyboard.press('Enter'),
      ]);
    });

    test('the bar\'s "+" opens the dispatch sheet and a dispatch from it creates an agent; a session page has no "+"', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await openAgents(page);
      const plus = page.locator('#switcher-deploy');
      await expect(plus).toBeVisible();
      await expect(plus).toHaveAccessibleName('Deploy agent');
      const pb = (await plus.boundingBox())!;
      const bb = (await bar(page).boundingBox())!;
      expect([Math.round(pb.width), Math.round(pb.height)], 'a 44px target').toEqual([44, 44]);
      expect(pb.y >= bb.y && pb.y + pb.height <= bb.y + bb.height + 0.5, 'inside the bar').toBe(true);
      // The pills' row keeps room (measured; printed for the PR).
      const rowW = await row(page, 'recent').evaluate((e) => e.getBoundingClientRect().width);
      console.log(`[agents-switcher ${vp.name}] pill row width ${rowW}`);
      expect(rowW, 'the pill row keeps usable room').toBeGreaterThan(150);
      await expectNoSidewaysScroll(page);
      // Keyboard: focus it and press Enter.
      await plus.focus();
      await page.keyboard.press('Enter');
      const sheet = page.locator('#agent-modal form');
      await expect(sheet).toBeVisible();
      await expect(sheet.locator('textarea[name="note_text"]'), 'the task-less form: an ad-hoc task box').toBeVisible();
      await expect(sheet.locator('[data-combobox]:has(input[name="note_id"])'), 'the task-less form: an existing-task picker, not a fixed task').toHaveCount(1);
      // A dispatch from it creates exactly one agent.
      const names = async () => {
        const r = await page.request.get(origin + '/api/agents', { headers: { Authorization: `Bearer ${hookToken}` } });
        expect(r.status()).toBe(200);
        return (await r.json()) as { id: number; name: string }[];
      };
      const before = new Set((await names()).map((a) => a.name));
      await sheet.locator('textarea[name="note_text"]').fill('e2e agents-switcher deploy');
      const resp = page.waitForResponse((r) => r.url().endsWith('/agents') && r.request().method() === 'POST');
      await sheet.locator('button[value="save"]').click();
      expect((await resp).status()).toBe(200);
      const created = (await names()).filter((a) => !before.has(a.name));
      expect(created.length, 'exactly one agent created').toBe(1);
      await page.request.delete(`${origin}/agents/${created[0].id}`, { headers: { Origin: origin } });
      // A session page's bar: the same switcher, no "+".
      await page.goto(origin + rig.viewPath('mango') + '&window=' + encodeURIComponent('@1'));
      await expect(bar(page)).toBeVisible();
      await expect(page.locator('#switcher-deploy')).toHaveCount(0);
    });

    // 🔴 THE ROW SWAP IS BOUND PER VERSION, NOT UNDER THE "/" FLAG. A window
    // that ran #294's switcher has window.__musterSwitcherKeys set; a boosted
    // swap into /agents then ran this script with that flag already true, and
    // the row swap it guarded was never bound — htmx's own out-of-band swap
    // nested a copy of each row in it. Simulated by setting the flag first.
    // And a swap that reorders the row keeps a focused pill focused.
    test('a window with an older switcher\'s flag still swaps the rows cleanly; a focused pill keeps focus through a reorder', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await page.addInitScript(() => { (window as any).__musterSwitcherKeys = true; try { localStorage.setItem('muster.switcher.view', 'all'); } catch (e) {} });
      await openAgents(page);
      await unpinAll(page);
      await expect(row(page, 'all')).toBeVisible();
      const marble = row(page, 'all').locator('a[data-switcher-entry]', { hasText: 'marble:1' });
      const href = await marble.getAttribute('href');
      const before = await row(page, 'all').locator('a[data-switcher-entry]').evaluateAll((els) => els.map((e) => e.getAttribute('href')));
      await marble.focus();
      await expect(marble).toBeFocused();
      // Pinned elsewhere, then the list refreshes: marble moves to the front
      // of All (pins lead it), and Pins gains it.
      const form = { kind: 'mirror', host: rig.host, session: 'marble', window: '@10' };
      expect((await page.request.post(origin + '/agents/pins', { form, headers: { Origin: origin } })).status()).toBe(200);
      try {
        await page.evaluate(() => (window as any).htmx.trigger(document.body, 'agents:changed'));
        await expect.poll(() => row(page, 'all').locator('a[data-switcher-entry]').first().getAttribute('href'), { message: 'the refresh reordered All' }).toBe(href);
        expect(before[0], 'control: marble was not first before').not.toBe(href);
        // No nesting, no duplicate ids.
        for (const id of ['switcher-list-recent', 'switcher-list-pins', 'switcher-list-all', 'switcher-count-pins']) {
          await expect(page.locator(`[id="${id}"]`), `one #${id}`).toHaveCount(1);
        }
        await expect(row(page, 'pins').locator(':scope > a[data-switcher-entry]')).toHaveCount(1);
        await expect(row(page, 'all').locator('[data-switcher-list], [id^="switcher-list-"]')).toHaveCount(0);
        // Focus is on the same session's pill, in its new place.
        expect(await page.evaluate(() => (document.activeElement as HTMLElement | null)?.closest('#session-switcher a[data-switcher-entry]')?.getAttribute('href') ?? null), 'focus kept on the pill').toBe(href);
      } finally {
        await page.request.post(origin + '/agents/pins/delete', { form, headers: { Origin: origin } });
      }
    });

    test('a filter text stored for the removed search box, or a ?q=, hides no card', async ({ page }) => {
      await ready(page);
      await page.addInitScript(() => {
        try {
          sessionStorage.setItem('muster.agents.filter', 'zzqx-nothing-matches');
          localStorage.setItem('muster.agents.filter', 'zzqx-nothing-matches');
        } catch (e) {}
      });
      await openAgents(page, '/agents?q=zzqx-nothing-matches');
      const n = await listed(page);
      // Every card is shown — but the Idle section's, which the section's own
      // collapse hides (idle_section.go), not a filter.
      const hidden = await page.evaluate(() => [...document.querySelectorAll('#agents-list > article:not([data-idle-section])')].filter((a) => !a.getClientRects().length).map((a) => a.id));
      expect(hidden, 'no card is hidden by a stale filter text').toEqual([]);
      expect(await page.locator('#agents-list > article:not([data-idle-section])').count(), 'instrument check: the windows are drawn outside the Idle section').toBeGreaterThanOrEqual(WINDOWS.length);
      await expect(page.locator('#agents-nomatch')).toBeHidden();
      await expect(page.locator('#agents-count')).toHaveText(`${n} of ${n}`);
      expect(await page.evaluate(() => document.getElementById('agents-filter-style')?.textContent || ''), 'no hide rule but the held arrivals\'').toBe('#agents-list > article[data-held]{display:none !important}');
      // The stale session copy is deleted by the head script.
      expect(await page.evaluate(() => sessionStorage.getItem('muster.agents.filter'))).toBeNull();
    });
  });
}

test.describe('desktop 1280×800, from another tab', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

  test('the FABs stay on the Tasks tab; a switch to Agents hides the replies FAB and fills the bar, and back shows it again', async ({ page }) => {
    test.setTimeout(90_000);
    await ready(page);
    await page.goto(origin + '/tasks');
    await expect(page.locator('#fab-tasks')).toBeVisible();
    await expect(page.locator('#notif-fab')).toBeVisible();
    await expect(page.locator('#fab-agents')).toHaveCount(0);
    // The Tasks document does not build the list: its Agents panel's bar is
    // pending until the list's own load fills it.
    await openMore(page);
    await page.click('.sidebar-tab[data-tab="agents"]');
    await expect(page).toHaveURL(/\/agents$/);
    // The router finished the switch (it once stopped at the launcher, and the
    // sidebar kept naming Tasks as the page).
    await expect(page.locator('.sidebar-tab[data-tab="agents"]')).toHaveAttribute('aria-current', 'page');
    await expect(bar(page)).toBeVisible();
    await expect(page.locator('#notif-fab')).toBeHidden();
    await expect(page.locator('#fab-tasks')).toBeHidden();
    await toggle(page).click();
    await menu(page).locator('[data-switcher-pick="all"]').click();
    const n = await listed(page);
    await expect(row(page, 'all').locator('a[data-switcher-entry]')).toHaveCount(n, { timeout: 15_000 });
    await expect(menu(page).locator('[data-switcher-count="all"]')).toHaveText(String(n));
    // Back to Tasks: its FABs return, and the bar goes with the Agents panel.
    await page.click('.sidebar-tab[data-tab="tasks"]');
    await expect(page.locator('#notif-fab')).toBeVisible();
    await expect(page.locator('#fab-tasks')).toBeVisible();
    await expect(bar(page)).toBeHidden();
    // The replies panel, open on Tasks, closes with its launcher when Back
    // (not a tap, which closes it anyway) shows Agents again.
    // (Opened by script: at 1280 the desktop sidebar covers this FAB; neither
    // its place nor the sidebar's is changed here.)
    await page.locator('#notif-fab').evaluate((e) => (e as HTMLElement).click());
    await expect(page.locator('#notif-panel')).toBeVisible();
    await page.goBack();
    await expect(page).toHaveURL(/\/agents$/);
    await expect(page.locator('.sidebar-tab[data-tab="agents"]')).toHaveAttribute('aria-current', 'page');
    await expect(page.locator('#notif-fab')).toBeHidden();
    await expect(page.locator('#notif-panel')).toBeHidden();
  });
});
