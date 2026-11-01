import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { agentsSwitcherPort, hookToken } from '../lib/env';
import { clsInit, readCls, settle } from '../lib/cls';
import { openDispatchSheet } from '../lib/dispatch';
import { cc, MirrorRig, mkchat } from '../lib/mirrorhost';

// THE SESSION SWITCHER'S BAR AND THE SEARCH PALETTE (internal/ui/switcher.go,
// palette.go), on a REAL muster-server and a REAL muster-mirror
// (`-tags e2emirror`, which fakes tmux only) whose windows carry codenames,
// plus one dispatched agent. The bar was once docked on the Agents list page
// too; that page is retired (the sidebar and the bar are how a person moves
// between agents), so the bar is read on a SESSION's page — a mirrored
// window's Rendered view — and the palette also from a page with no bar. At a
// phone (390×844, touch) and a desktop (1280×800) size:
//
//   1. a session page's bar paints with the page (no layout shift from it, no
//      sideways page scroll), and its view menu works: All holds every entry,
//      the page's own drawn current; Pins starts empty;
//   2. the search palette ("/" and the magnifier) finds a window by its
//      codename and Enter opens it;
//   3. the palette also finds an entry by its repository, its kind, a window's
//      raw tmux session name and — the retired list's host tabs' successor —
//      its HOST's name, after every codename and title match (operator:
//      "Teach the palette repo+kind");
//   4. a pill shows the whole codename, ":<index>" and all;
//   5. the sidebar's magnifier opens the same palette, on a session page and
//      on a page with no bar, where it reads the sidebar's rows;
//   6. the bar's pin toggle pins and unpins the page's own window;
//   7. the dispatch "+" — beside the sidebar's Agents label now (it was the
//      list's bar's) — opens the task-less dispatch sheet, and a dispatch from
//      it creates an agent and opens its page; a session page's bar has no "+";
//   8. a window that ran an older switcher still swaps the rows cleanly, and a
//      focused pill keeps focus through a reorder.

test.use({ storageState: { cookies: [], origins: [] } });

// DECIMAL, like the host: the agent's name is a codename the search matches as
// a substring, and a base-36 stamp can spell a query this spec types (the same
// flake switcher-search.spec.ts had with "vap"). Digits spell no query.
const rig = new MirrorRig(agentsSwitcherPort, 'e2e-asw', {}, { decimalHost: true });
const origin = rig.origin;
const stamp = String(Date.now());
const AGENT = `e2e-asw-ledger-${stamp}`;
const AGENT_TITLE = 'Rotate the ledger';
// Its repository: found by the palette's words, ranked after "wheat"'s codename.
const AGENT_REPO = 'acme/wheatfield';
// Two more windows: one whose codename is NOT its tmux session (a labels file
// names session scratch11 "orchid"), one running opencode (its kind).
const SCRATCH = 'scratch11';
const NIMBUS = 'nimbus';

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
const toggle = (page: Page) => page.locator('#switcher-toggle');
const menu = (page: Page) => page.locator('#switcher-views');
const pal = (page: Page) => page.locator('#switcher-search');
const input = (page: Page) => page.locator('#switcher-search-input');
const options = (page: Page) => page.locator('#switcher-search-results [role="option"]');
const row = (page: Page, key: string) => page.locator(`#session-switcher [data-switcher-list="${key}"]`);
const viewOf = (session: string, w: string) => origin + rig.viewPath(session) + '&window=' + encodeURIComponent(w);
const MANGO = viewOf('mango', '@1');

// listed is how many entries there are: these windows, this agent, and the
// seeded fixture's own agents (e2e/seed) — every one a row of the sidebar's
// list (GET /ui/nav/agents, the Idle group's included), and every one an entry
// of the switcher's All view.
async function listed(page: Page): Promise<number> {
  let n = 0;
  await expect
    .poll(async () => {
      const html = await (await page.request.get(origin + '/ui/nav/agents')).text();
      n = (html.match(/<a\b[^>]*\bdata-nav-agent="/g) || []).length;
      return n;
    }, { timeout: 15_000 })
    .toBeGreaterThanOrEqual(WINDOWS.length + 3);
  return n;
}

async function ready(page: Page) {
  await rig.login(page);
  await rig.waitListed(page, 'marble');
  // Each test starts from no stored view.
  await page.addInitScript(() => {
    try {
      if (!sessionStorage.getItem('asw-cleared')) {
        localStorage.removeItem('muster.switcher.view');
        sessionStorage.setItem('asw-cleared', '1');
      }
    } catch (e) {}
  });
}

// openView loads a window's Rendered view with layout shift recorded from the
// first paint.
async function openView(page: Page, url = MANGO) {
  await page.addInitScript(clsInit);
  await page.goto(url);
  await expect(bar(page)).toBeVisible();
  await settle(page);
}

// noSidewaysScroll: the page itself never scrolls sideways (the bar's rows
// scroll inside themselves).
async function expectNoSidewaysScroll(page: Page) {
  const w = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth }));
  expect(w.sw, `the page is ${w.sw}px wide in a ${w.cw}px viewport`).toBeLessThanOrEqual(w.cw);
}

const unpin = (page: Page, session: string, w: string) =>
  page.request.post(origin + '/agents/pins/delete', { form: { kind: 'mirror', host: rig.host, session, window: w }, headers: { Origin: origin } });

for (const vp of [
  { name: 'phone 390×844', w: 390, h: 844, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', w: 1280, h: 800, use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test("a session page's bar paints with the page; its view menu works: All holds every entry, Pins starts empty", async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      for (const [s, w] of WINDOWS) await unpin(page, s, w);
      await openView(page);
      // Nothing that shifted is the bar or anything in it: it is out of flow
      // and its room is reserved in the page's first byte.
      const cls = await readCls(page);
      expect(cls.unsupported, 'the browser reports layout-shift entries').toBeUndefined();
      for (const sh of cls.shifts) {
        for (const src of sh.sources) expect(src, `a shift's source is the switcher: ${JSON.stringify(cls.shifts)}`).not.toMatch(/session-switcher|switcher-/);
      }
      await expectNoSidewaysScroll(page);
      await expect(toggle(page)).toHaveText('Recent');
      // All holds every entry, the page's own drawn current (one, not a link).
      await toggle(page).click();
      await expect(menu(page)).toBeVisible();
      await menu(page).locator('[data-switcher-pick="all"]').click();
      await expect(menu(page)).toBeHidden();
      await expect(toggle(page)).toHaveText('All');
      await expect(row(page, 'all')).toBeVisible();
      const n = await listed(page);
      await expect(row(page, 'all').locator('[data-switcher-entry]')).toHaveCount(n);
      await expect(row(page, 'all').locator('[data-switcher-current]')).toHaveText('mango:1');
      await expect(row(page, 'all').locator('a[data-switcher-entry]', { hasText: AGENT_TITLE })).toHaveCount(1);
      // Pins: nothing pinned.
      await toggle(page).click();
      await menu(page).locator('[data-switcher-pick="pins"]').click();
      await expect(row(page, 'pins')).toHaveText('Nothing pinned');
      await expect(menu(page).locator('[data-switcher-count="pins"]')).toHaveText('0');
    });

    test('the search palette: "/" and the magnifier open it, a codename finds its window, Enter opens it', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await openView(page);
      await page.locator('body').click({ position: { x: 5, y: 5 } }).catch(() => {});
      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
      await page.keyboard.press('/');
      await expect(pal(page)).toBeVisible();
      await expect(input(page)).toBeFocused();
      // Every entry but the page's own session.
      await expect(options(page)).toHaveCount((await listed(page)) - 1);
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

    test('the palette finds an entry by its repository, its kind, its tmux session and its host, after every codename match', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await openView(page);
      await page.locator('#switcher-search-open').click();
      const labels = () => options(page).evaluateAll((els) => els.map((e) => (e.firstElementChild as HTMLElement).textContent));
      // The tmux session name: the window called "orchid".
      await input(page).fill(SCRATCH);
      await expect.poll(labels).toEqual(['orchid:1']);
      // The kind.
      await input(page).fill('opencode');
      await expect.poll(labels).toEqual([`${NIMBUS}:1`]);
      // The repository — and a codename match ranks first: "wheat" is the
      // window's codename prefix, and only a word of the agent's repository.
      await input(page).fill('wheatfield');
      await expect.poll(labels).toEqual([AGENT_TITLE]);
      await input(page).fill('wheat');
      await expect.poll(labels).toEqual(['wheat:1', AGENT_TITLE]);
      // THE HOST (the host tabs' successor): this host's name finds every one
      // of its windows but the page's own — and no dispatched agent; the
      // cluster's finds the dispatched agents, and none of this host's windows.
      const windowsHere = WINDOWS.filter(([s]) => s !== 'mango').map(([s]) => `${s}:1`).concat(['orchid:1', `${NIMBUS}:1`]);
      await input(page).fill(rig.host);
      await expect.poll(async () => (await labels()).slice().sort()).toEqual(windowsHere.slice().sort());
      await input(page).fill('cluster');
      await expect.poll(labels).toContain(AGENT_TITLE);
      for (const l of await labels()) expect(windowsHere, `"cluster" found ${l}`).not.toContain(l);
      // Enter opens the top one: the codename match.
      await input(page).fill('wheat');
      await expect.poll(labels).toEqual(['wheat:1', AGENT_TITLE]);
      await Promise.all([
        page.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('session') === 'wheat'),
        page.keyboard.press('Enter'),
      ]);
    });

    // 🔴 A PILL SHOWS THE WHOLE CODENAME, ":<index>" AND ALL (operator report:
    // the pills were "missing the number after the colon"). "orchid" is a
    // codename that is NOT its tmux session (the labels file), alone in every
    // view — exactly where the bar used to draw it without its index. Read on
    // a session page's bar, at this size: the label's text, and its box not
    // clipping it.
    test('a pill shows the whole codename, ":<index>" and all, unclipped', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await page.addInitScript(() => { try { localStorage.setItem('muster.switcher.view', 'all'); } catch (e) {} });
      await page.goto(MANGO);
      await expect(row(page, 'all')).toBeVisible();
      const orchid = row(page, 'all').locator(`a[data-switcher-entry][data-switcher-words~="${SCRATCH}"]`);
      await expect(orchid, 'the orchid window\'s pill').toHaveCount(1);
      await expect(orchid, 'the pill reads the whole codename').toHaveText('orchid:1');
      await expect(orchid).toHaveAttribute('title', 'orchid:1');
      // Not clipped: the label's box holds its whole text, and the pill is
      // at least as wide as the label.
      await orchid.scrollIntoViewIfNeeded();
      const m = await orchid.evaluate((a) => {
        const label = a.querySelector('.whitespace-nowrap') as HTMLElement, pill = a.firstElementChild as HTMLElement;
        return { sw: label.scrollWidth, cw: label.clientWidth, lw: label.getBoundingClientRect().width, pw: pill.getBoundingClientRect().width };
      });
      expect(m.sw, `the label is not clipped (${JSON.stringify(m)})`).toBeLessThanOrEqual(m.cw + 0.5);
      expect(m.pw, `the pill holds the label (${JSON.stringify(m)})`).toBeGreaterThanOrEqual(m.lw);
      // Every window pill ends in its ":<index>".
      const labels = await row(page, 'all').locator('a[data-switcher-entry="mirror"]').evaluateAll((els) => els.map((e) => e.textContent || ''));
      expect(labels.length, 'instrument check: the row holds window pills').toBeGreaterThanOrEqual(WINDOWS.length - 1);
      for (const l of labels) expect(l, 'a window pill without its index').toMatch(/:\d+$/);
      await expectNoSidewaysScroll(page);
      await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
    });

    // THE SIDEBAR'S MAGNIFIER OPENS THE SAME PALETTE (operator ask: "a search
    // icon button that opens the same command palette UI that the bottom bar
    // search opens"): the one #switcher-search, from a session page (where the
    // bar's magnifier opens it too) and from a page with no bar at all, whose
    // palette reads the sidebar's rows. Enter opens what it found.
    test('the sidebar\'s magnifier opens the bar\'s palette, on a session page and on a page with no bar; Enter opens the find', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      const openSidebar = async () => {
        if (vp.w < 1024) {
          await page.locator('#sidebar-open').click();
          await expect(page.locator('#sidebar-root')).toHaveAttribute('data-open', 'true');
        }
      };
      // A session page: the overlay sidebar, opened at every width.
      await page.goto(MANGO);
      await expect(bar(page)).toBeVisible();
      await page.locator('#sidebar-open').click();
      await expect(page.locator('#sidebar-search')).toBeVisible();
      await page.locator('#sidebar-search').click();
      await expect(pal(page)).toBeVisible();
      await expect(page.locator('[id="switcher-search"]'), 'one palette on the page').toHaveCount(1);
      await expect(input(page)).toBeFocused();
      await expect(page.locator('#sidebar-search')).toHaveAttribute('aria-expanded', 'true');
      await input(page).pressSequentially('quar');
      await expect(options(page)).toHaveCount(1);
      await expect(options(page).first()).toContainText('quartz');
      await page.keyboard.press('Escape');
      await expect(pal(page)).toBeHidden();
      await expect(page.locator('#sidebar-search'), 'focus goes back to the button that opened it').toBeFocused();
      await expect(page.locator('#sidebar-search')).toHaveAttribute('aria-expanded', 'false');
      // The bar's magnifier opens the very same element.
      await pal(page).evaluate((el) => { (window as any).__palEl = el; });
      await page.locator('#sidebar-close').click();
      await expect(page.locator('#sidebar-root')).toHaveAttribute('data-open', 'false');
      await page.locator('#switcher-search-open').click();
      await expect(pal(page)).toBeVisible();
      expect(await pal(page).evaluate((el) => el === (window as any).__palEl), 'the bar and the sidebar open one element').toBe(true);
      await page.keyboard.press('Escape');

      // A page with no bar: the account page. The palette reads the sidebar's rows.
      await page.goto(origin + '/account');
      await expect(bar(page)).toHaveCount(0);
      await openSidebar();
      await expect(page.locator('#sidebar-agents-list a[data-nav-agent]').first()).toBeVisible({ timeout: 15_000 });
      await page.locator('#sidebar-search').click();
      await expect(pal(page)).toBeVisible();
      await input(page).pressSequentially('saff');
      await expect(options(page)).toHaveCount(1);
      await Promise.all([
        page.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('session') === 'saffron'),
        page.keyboard.press('Enter'),
      ]);
      // And "/" opens it on a page with no bar wherever the sidebar's button is
      // on screen: the desktop rail (a phone's shut drawer has it off screen).
      await page.goto(origin + '/account');
      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
      await page.keyboard.press('/');
      if (vp.w >= 1024) await expect(pal(page)).toBeVisible();
      else await expect(pal(page)).toBeHidden();
    });

    // THE PIN ON A SESSION'S BAR (operator ask: "a way to pin the current
    // session, the current agent, from within the rendered view"): on a
    // window's Rendered view the bar's pin toggle pins THAT window — the Pins
    // view and its count follow at once, a reload keeps it, another session's
    // bar lists it in Pins — and unpins it again. (With the Agents list's card
    // toggles retired, this is the one way to pin: open it, then pin.)
    test('the bar\'s pin toggle pins and unpins the page\'s own window; the Pins view follows at once', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await unpin(page, 'tundra', '@6');
      const view = viewOf('tundra', '@6');
      await page.goto(view);
      const pin = page.locator('#switcher-pin');
      await expect(pin).toBeVisible();
      await expect(pin).toHaveAttribute('aria-pressed', 'false');
      await expect(pin).toHaveAccessibleName('Pin this session');
      const pb = (await pin.boundingBox())!, bb = (await bar(page).boundingBox())!;
      expect([Math.round(pb.width), Math.round(pb.height)], 'a 44px target').toEqual([44, 44]);
      expect(pb.y >= bb.y - 0.5 && pb.y + pb.height <= bb.y + bb.height + 0.5, 'inside the bar').toBe(true);
      await expect(menu(page).locator('[data-switcher-count="pins"]')).toHaveText('0');
      try {
        await (vp.use.hasTouch ? pin.tap() : pin.click());
        await expect(page.locator('#switcher-pin')).toHaveAttribute('aria-pressed', 'true');
        await expect(page.locator('#switcher-pin')).toHaveAccessibleName('Unpin this session');
        // The Pins view and its count, at once, with no reload: the page's own
        // window, drawn current.
        await expect(menu(page).locator('[data-switcher-count="pins"]')).toHaveText('1');
        await expect(row(page, 'pins').locator('[data-switcher-current]')).toHaveText('tundra:1');
        await expect(page.locator('[id="switcher-list-pins"]'), 'no nested copy of the row').toHaveCount(1);
        // Kept: a reload draws it pressed, and another session's bar lists it.
        await page.reload();
        await expect(page.locator('#switcher-pin')).toHaveAttribute('aria-pressed', 'true');
        await page.goto(MANGO);
        await expect(row(page, 'pins').locator('a[data-switcher-entry]')).toHaveText(['tundra:1']);
        await expect(page.locator('#switcher-pin'), "mango's own toggle is not pressed").toHaveAttribute('aria-pressed', 'false');
        // And unpinned from the same toggle.
        await page.goto(view);
        await page.locator('#switcher-pin').click();
        await expect(page.locator('#switcher-pin')).toHaveAttribute('aria-pressed', 'false');
        await expect(menu(page).locator('[data-switcher-count="pins"]')).toHaveText('0');
        await expectNoSidewaysScroll(page);
      } finally {
        await unpin(page, 'tundra', '@6');
      }
    });

    test('the sidebar\'s "+" opens the dispatch sheet, and a dispatch from it creates an agent and opens it; a session page\'s bar has no "+"', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await page.goto(MANGO);
      await expect(bar(page)).toBeVisible();
      // A session page's bar: the same switcher, no "+" of its own.
      await expect(page.locator('#switcher-deploy')).toHaveCount(0);
      await page.locator('#sidebar-open').click();
      const plus = page.locator('#sidebar-deploy');
      await expect(plus).toBeVisible();
      await expect(plus).toHaveAccessibleName('Deploy agent');
      const pb = (await plus.boundingBox())!;
      expect([Math.round(pb.width), Math.round(pb.height)], 'a 44px target').toEqual([44, 44]);
      await expectNoSidewaysScroll(page);
      // Keyboard: focus it and press Enter.
      await plus.focus();
      await page.keyboard.press('Enter');
      const sheet = page.locator('#agent-modal form');
      await expect(sheet).toBeVisible();
      await expect(sheet.locator('textarea[name="note_text"]'), 'the task-less form: an ad-hoc task box').toBeVisible();
      await expect(sheet.locator('[data-combobox]:has(input[name="note_id"])'), 'the task-less form: an existing-task picker, not a fixed task').toHaveCount(1);
      // A dispatch from it creates exactly one agent, and opens its page.
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
      try {
        await page.waitForURL((u) => u.pathname === '/agents/' + created[0].name, { timeout: 15_000 });
      } finally {
        await page.request.delete(`${origin}/agents/${created[0].id}`, { headers: { Origin: origin } });
      }
      // The same "+" from a page with no bar (openDispatchSheet: the sidebar's).
      await page.goto(origin + '/tasks');
      await openDispatchSheet(page);
    });

    // 🔴 THE ROW SWAP IS BOUND PER VERSION, NOT UNDER THE "/" FLAG. A window
    // that ran #294's switcher has window.__musterSwitcherKeys set; a boosted
    // swap then ran this script with that flag already true, and the row swap
    // it guarded was never bound — htmx's own out-of-band swap nested a copy of
    // each row in it. Simulated by setting the flag first. The swap is the pin
    // toggle's answer (switcher_pin.go: the bar's rows, out of band), clicked by
    // script so focus stays where the reader left it: on a pill, which keeps
    // it through the reorder.
    test('a window with an older switcher\'s flag still swaps the rows cleanly; a focused pill keeps focus through a reorder', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await page.addInitScript(() => { (window as any).__musterSwitcherKeys = true; try { localStorage.setItem('muster.switcher.view', 'all'); } catch (e) {} });
      await unpin(page, 'mango', '@1');
      await openView(page);
      await expect(row(page, 'all')).toBeVisible();
      const marble = row(page, 'all').locator('a[data-switcher-entry]', { hasText: 'marble:1' });
      const href = await marble.getAttribute('href');
      const order = () => row(page, 'all').locator('[data-switcher-entry]').evaluateAll((els) => els.map((e) => e.getAttribute('href') || 'current'));
      const before = await order();
      await marble.focus();
      await expect(marble).toBeFocused();
      try {
        // Pin the page's own window: it moves to the front of All (pins lead
        // it), and Pins gains it.
        await page.evaluate(() => (document.getElementById('switcher-pin') as HTMLElement).click());
        await expect(page.locator('#switcher-pin')).toHaveAttribute('aria-pressed', 'true');
        await expect.poll(async () => (await order())[0], { message: 'the swap reordered All' }).toBe('current');
        expect(before[0], 'control: the page\'s own pill was not first before').not.toBe('current');
        // No nesting, no duplicate ids.
        for (const id of ['switcher-list-recent', 'switcher-list-pins', 'switcher-list-all', 'switcher-count-pins']) {
          await expect(page.locator(`[id="${id}"]`), `one #${id}`).toHaveCount(1);
        }
        await expect(row(page, 'pins').locator(':scope > [data-switcher-entry]')).toHaveCount(1);
        await expect(row(page, 'all').locator('[data-switcher-list], [id^="switcher-list-"]')).toHaveCount(0);
        // Focus is on the same session's pill, in its new place.
        expect(await page.evaluate(() => (document.activeElement as HTMLElement | null)?.closest('#session-switcher a[data-switcher-entry]')?.getAttribute('href') ?? null), 'focus kept on the pill').toBe(href);
      } finally {
        await unpin(page, 'mango', '@1');
      }
    });
  });
}

// THE BAR ON A MIRRORED WINDOW'S TERMINAL VIEW (operator ask: "Include the
// bottom bar in the agent terminal view as well … so that I can easily switch
// agents from the terminal view"), at 390×844, touch: the bar paints with the
// page at the column's foot, below the composer (which stays above it, a soft
// keyboard open too); the page's own window is marked current and is not in
// Recent; it moves nothing (CLS < 0.01, no shift from the bar); the terminal
// is not resized by it — the far side (the fake mirror prints `size=CxR` per
// attach and per resize) heard one size, and a view switch in the bar sends
// none; and a tap on an entry opens that session.
test.describe('the Terminal view at 390×844', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test("a mirrored window's Terminal view draws the bar: current marked, no shift, no resize, a tap switches", async ({ page }) => {
    test.setTimeout(90_000);
    await ready(page);
    const term = origin + rig.terminalPath('mango') + '&window=' + encodeURIComponent('@1');
    await page.addInitScript(clsInit);
    await page.goto(term);
    const screen = page.locator('#term .xterm-rows');
    await expect(screen).toContainText('FAKE-MIRROR session=mango');
    await expect(bar(page)).toBeVisible();
    await settle(page);
    await page.waitForTimeout(500);

    // Current: the page's own window, drawn current in All, not in Recent.
    await expect(row(page, 'all').locator('[data-switcher-current]')).toHaveText('mango:1');
    await expect(row(page, 'recent').locator('[data-switcher-current]')).toHaveCount(0);
    await expect(row(page, 'recent').locator('[data-switcher-code="mango:1"]')).toHaveCount(0);

    // No layout shift, and none from the bar.
    const cls = await readCls(page);
    expect(cls.unsupported, 'the browser reports layout-shift entries').toBeUndefined();
    expect(cls.total, `layout shift ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);
    for (const sh of cls.shifts) {
      for (const src of sh.sources) expect(src, `a shift's source is the switcher: ${JSON.stringify(cls.shifts)}`).not.toMatch(/session-switcher|switcher-/);
    }
    await expectNoSidewaysScroll(page);

    // The column: terminal, key bar, composer, then the bar at the bottom edge.
    const box = async (sel: string) => (await page.locator(sel).first().boundingBox())!;
    const order = async (h: number) => {
      const [t, k, input, b] = [await box('#term-scroll'), await box('#term-keys'), await box('#mv-input'), await box('#session-switcher')];
      expect(t.y + t.height, 'the terminal ends above the key bar').toBeLessThanOrEqual(k.y + 0.5);
      expect(input.y + input.height, 'the input is above the bar').toBeLessThanOrEqual(b.y + 0.5);
      expect(Math.round(b.y + b.height), 'the bar is the bottom of the screen').toBe(h);
      expect(Math.round(b.height), 'the bar is its fixed 45px').toBe(45);
    };
    await order(844);

    // The far side heard ONE size: the attach's. Picking a view in the bar
    // changes no layout and so sends no resize.
    // The attach line is wider than a phone's terminal, so xterm wraps it
    // ("…siz" / "e=47x40"): the rows are read joined.
    const sizes = async () => [...(await screen.innerText()).replace(/\n/g, '').matchAll(/size=(\d+x\d+)/g)].map((m) => m[1]);
    const heard = await sizes();
    expect(heard.length, `a size on screen: ${await screen.innerText()}`).toBeGreaterThan(0);
    expect(new Set(heard).size, `one size, no churn: ${heard}`).toBe(1);
    await toggle(page).tap();
    await expect(menu(page)).toBeVisible();
    await menu(page).locator('[data-switcher-pick="all"]').tap();
    await expect(row(page, 'all')).toBeVisible();
    await page.waitForTimeout(1000);
    expect(await sizes(), 'a view switch sent no resize').toEqual(heard);

    // A soft keyboard (interactive-widget=resizes-content shrinks the layout
    // viewport): the bar stays at the foot, the input above it.
    await page.setViewportSize({ width: 390, height: 500 });
    await expect.poll(async () => Math.round((await box('#session-switcher')).y + 45)).toBe(500);
    await order(500);
    await page.setViewportSize({ width: 390, height: 844 });
    await expect.poll(async () => Math.round((await box('#session-switcher')).y + 45)).toBe(844);

    // A tap on an entry opens that session (its Rendered view: wheat is a
    // Claude pane), with a real load.
    const wheat = row(page, 'all').locator('a[data-switcher-entry]', { hasText: 'wheat:1' });
    await Promise.all([
      page.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('session') === 'wheat'),
      wheat.tap(),
    ]);
  });
});
