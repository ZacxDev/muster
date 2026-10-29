import { FrameLocator, Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { titleRoomPort } from '../lib/env';
import { clsInit, readCls, settle } from '../lib/cls';
import { cc, entryMatches, mirroredEntries, MirrorRig } from '../lib/mirrorhost';

// ROOM FOR A TITLE — four operator asks about titles cut off a few characters
// short, measured on a REAL muster-server and a REAL per-window muster-mirror
// (`-tags e2emirror`) whose windows carry realistic 23–42 character titles:
//
//   1. the Rendered | Terminal switch (viewToggle) is narrower: its segments'
//      side padding is 6px, not 10px, so the header's title gets the room;
//   2. a header title that is still clipped opens a popover with the WHOLE
//      title on a tap (mirrorViewTitle) — and a title that fits opens none;
//   3. the session switcher's entries — the bar below the message input,
//      whose Recent/All rows the rendered view draws and whose Pins row is
//      the agent page's pinned list — show their WHOLE title, however long
//      (were 10rem, 20rem, then 26rem and still "often cut off"): the pill is
//      as wide as its label and the row scrolls sideways, never the page;
//   4. the same switcher's Pins and Recent views draw whole too. (They were
//      the retired Agents list's docked bar, which had replaced its pinned
//      chips — a 36-character title once clipped at 390px — and its "Recently
//      viewed" strip, whose entries were capped at 26rem. That bar is gone with
//      the list; the guarantee is kept on the session pages' bar: Pins below a
//      rendered view, Recent on an agent's page.)
//
// Each width assertion is a title that was clipped at the base (red there) and
// fits here; the title lengths are pairwise distinct so no assertion can pass
// on a neighbour's entry. Layout shift stays under 0.01 at both sizes, and the
// page never scrolls sideways at 390px.
test.use({ storageState: { cookies: [], origins: [] } });

const rig = new MirrorRig(titleRoomPort, 'e2e-title');
const origin = rig.origin;

// The windows: session, window id. A window with no label is titled
// "<session>:<index>" (every index here is 1). Lengths 23, 27, 34, 36, 42,
// 49 and 84.
const WINDOWS: [string, string][] = [
  ['release-notes-drafter', '@1'],
  ['billing-reconcile-nightly', '@2'],
  ['payments-ledger-migration-review', '@3'],
  ['customer-onboarding-email-sequence', '@4'],
  ['warehouse-inventory-sync-backfill-job-v2', '@5'],
  ['quarterly-revenue-forecast-reconciliation-batch', '@9'],
  ['enterprise-customer-data-warehouse-schema-migration-validation-and-audit-report-v3', '@11'],
];
// Windows that are NOT pinned, visited so the switcher shows them in Recent.
// Lengths 27, 34, 36.
const UNPINNED: [string, string][] = [
  ['cache-warmup-orchestrator', '@6'],
  ['inventory-forecast-model-refresh', '@7'],
  ['subscription-renewal-reminder-job', '@8'],
];
const ALL_WINDOWS = [...WINDOWS, ...UNPINNED];
const titleOf = (s: string) => s + ':1';
// The window whose rendered view the header tests open: its title is 42
// characters, clipped in the header at both sizes.
const LONG = 'warehouse-inventory-sync-backfill-job-v2';

const viewOf = (session: string, win: string) =>
  `${origin}/mirror/view?host=${encodeURIComponent(rig.host)}&session=${encodeURIComponent(session)}&window=${encodeURIComponent(win)}`;
const winOf = (s: string) => ALL_WINDOWS.find((w) => w[0] === s)![1];

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
  const now = Math.floor(Date.now() / 1000);
  const lines = ALL_WINDOWS.map(([s, w], i) => {
    const tr = rig.transcript(cc.prompt(`the prompt of ${s}`) + cc.reply(`Answered in ${s}.`), 'w' + i);
    return `${s}|${w}|1|1|claude|${now - 60 * (i + 1)}|${tr}`;
  });
  const windows = rig.transcript(lines.join('\n') + '\n', 'windows');
  await rig.startHost({ MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
});

test.afterAll(async () => {
  await rig.stop();
});

// Once per rig: the windows are listed, then every one of WINDOWS is pinned.
let pinned = false;
async function ready(page: Page) {
  await rig.login(page);
  if (pinned) return;
  // The sidebar's rows (GET /ui/nav/agents): this host's windows, online,
  // each a row that opens its Rendered view.
  await expect
    .poll(async () => mirroredEntries(await (await page.request.get(origin + '/ui/nav/agents')).text()).filter((e) => e.state === 'online' && e.markup.includes(`host=${rig.host}`) && entryMatches(e.markup, 'data-mirror-claude')).length, {
      message: `every window listed as Claude Code (muster-mirror log:\n${rig.log})`,
      timeout: 20_000,
    })
    .toBeGreaterThanOrEqual(ALL_WINDOWS.length);
  for (const [s, w] of WINDOWS) {
    const r = await page.request.post(origin + '/agents/pins', { form: { kind: 'mirror', host: rig.host, session: s, window: w }, headers: { Origin: origin } });
    expect(r.status(), `pinned ${s}`).toBe(200);
  }
  pinned = true;
}

// fits is a text box's truncation state: its scrollWidth against its clientWidth.
type Fit = { text: string; scroll: number; client: number; clipped: boolean };
const fitOf = (el: Element): Fit => {
  const h = el as HTMLElement;
  return { text: (h.textContent || '').trim(), scroll: h.scrollWidth, client: h.clientWidth, clipped: h.scrollWidth > h.clientWidth + 1 };
};

// The switcher's entry label for a window in a view: the pill's last span
// (spelled by structure, not by its class, so the base's truncating span and
// this tree's whole one are the same element).
const switcherLabel = (page: Page, title: string, view = 'all') =>
  page.locator(`#session-switcher [data-switcher-list="${view}"] [data-switcher-entry] > span > span:last-child`, { hasText: title }).first();
// The switcher's Pins and Recent labels.
const chipLabel = (page: Page, title: string) => switcherLabel(page, title, 'pins');
const recentLabel = (page: Page, title: string) => switcherLabel(page, title, 'recent');

// visitUnpinned opens each unpinned window's rendered view once, so the
// switcher's Recent view lists them.
async function visitUnpinned(page: Page) {
  for (const [s, w] of UNPINNED) {
    await page.goto(viewOf(s, w));
    await expect(page.locator('nav[aria-label="View"]')).toBeVisible();
  }
}
// The header's truncating title box: the <h1> at the base, the button in it here.
const headerFit = (page: Page) =>
  page.evaluate(() => {
    const h1 = document.querySelector('header h1') as HTMLElement;
    const el = (h1.querySelector('button') as HTMLElement) || h1;
    return { text: (el.textContent || '').trim(), scroll: el.scrollWidth, client: el.clientWidth, clipped: el.scrollWidth > el.clientWidth + 1 };
  });
const title = (page: Page) => page.locator('#mv-title');
const pop = (page: Page) => page.locator('#mv-title-full');
const overflowX = (page: Page) => page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);

async function setView(page: Page, view: string) {
  await page.addInitScript((v) => { try { localStorage.setItem('muster.switcher.view', v); } catch (e) {} }, view);
}

async function openView(page: Page, session: string, view = 'all') {
  await setView(page, view);
  await page.addInitScript(clsInit);
  await page.goto(viewOf(session, winOf(session)));
  await expect(page.locator('nav[aria-label="View"]')).toBeVisible();
  await expect(page.locator('#session-switcher')).toBeVisible();
}

async function openAgent(page: Page, view: string) {
  await setView(page, view);
  await page.addInitScript(clsInit);
  await page.goto(origin + '/agents/docs-sweeper');
  await page.waitForURL((u) => u.pathname === '/agents/docs-sweeper' && u.searchParams.has('session'));
  await expect(page.locator('#session-switcher')).toBeVisible();
}

// The switcher's titles (WINDOWS) are 23 to 84 characters; every one must be
// drawn whole (the 26rem cap clipped the 84-character one at both sizes, and
// everything from 34 characters at 390).
// The titles the retired Agents list's pinned chips and Recent strip once
// clipped (a 36-character one at 390px; 27 and 34 characters in a 14rem
// entry), drawn whole by the switcher's Pins and Recent views.
const CHIP_FITS = 'customer-onboarding-email-sequence';
const RECENT_FITS = ['cache-warmup-orchestrator', 'inventory-forecast-model-refresh'];

for (const vp of [
  { name: 'phone 390×844', w: 390, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', w: 1280, use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    // THE MEASUREMENT (the PR's before/after table): every surface's title
    // boxes, printed as one JSON line. It asserts only that each box exists,
    // so the same file measures the base and this tree.
    test('measure: each surface\'s title boxes (scrollWidth vs clientWidth)', async ({ page }) => {
      test.setTimeout(120_000);
      await ready(page);
      await openView(page, LONG);
      await settle(page);
      const out: Record<string, unknown> = { viewport: vp.name };
      out.tabs = await page.evaluate(() =>
        ['view-rendered', 'view-terminal'].map((id) => {
          const el = document.getElementById(id)!;
          const cs = getComputedStyle(el);
          return { id, width: +el.getBoundingClientRect().width.toFixed(2), paddingLeft: cs.paddingLeft, paddingRight: cs.paddingRight };
        }),
      );
      out.header = await headerFit(page);
      const sw: Record<string, Fit> = {};
      for (const [s] of WINDOWS) {
        if (s === LONG) continue; // the page's own entry is the "current" chip
        sw[titleOf(s)] = await switcherLabel(page, titleOf(s)).evaluate(fitOf);
      }
      out.switcherRenderedViewAll = sw;
      await openAgent(page, 'pins');
      const pins: Record<string, Fit> = {};
      for (const [s] of WINDOWS) pins[titleOf(s)] = await switcherLabel(page, titleOf(s), 'pins').evaluate(fitOf);
      out.switcherAgentPagePins = pins;
      await openView(page, LONG, 'pins');
      await expect(chipLabel(page, titleOf(WINDOWS[0][0]))).toBeVisible();
      const chips: Record<string, Fit> = {};
      for (const [s] of WINDOWS) {
        if (s === LONG) continue; // the page's own entry is the "current" chip
        chips[titleOf(s)] = await chipLabel(page, titleOf(s)).evaluate(fitOf);
      }
      out.switcherRenderedViewPins = chips;
      await visitUnpinned(page);
      await openAgent(page, 'recent');
      const recent: Record<string, Fit> = {};
      for (const [s] of UNPINNED) {
        await expect(recentLabel(page, titleOf(s))).toBeVisible();
        recent[titleOf(s)] = await recentLabel(page, titleOf(s)).evaluate(fitOf);
      }
      out.switcherAgentPageRecent = recent;
      out.overflowX = await overflowX(page);
      console.log('TITLE-ROOM ' + JSON.stringify(out));
    });

    // 1. 🔴 RED AT THE BASE: each segment's side padding was 10px. Desktop
    // only: below `sm` the switch is one icon (the 393 describe below).
    if (vp.w !== 390) test('the Rendered | Terminal segments are padded 6px a side, still 44px tall', async ({ page }) => {
      await ready(page);
      await openView(page, LONG);
      // 🔴 RED AT fade163f (no phone icon exists): at 1280 BOTH word segments
      // are displayed, and the phone's icon and its sr-only name are not.
      await expect(page.locator('#view-rendered')).toBeVisible();
      await expect(page.locator('#view-terminal')).toBeVisible();
      await expect(page.locator('#view-terminal[data-view-toggle] svg')).toHaveCount(1);
      await expect(page.locator('#view-terminal[data-view-toggle] svg')).toBeHidden();
      await expect(page.locator('#view-terminal .sr-only')).toHaveCSS('display', 'none');
      await expect(page.locator('#view-terminal')).toHaveAccessibleName('Terminal');
      for (const id of ['view-rendered', 'view-terminal']) {
        const m = await page.locator('#' + id).evaluate((el) => {
          const cs = getComputedStyle(el);
          const r = document.createRange();
          r.selectNodeContents(el);
          return { pl: cs.paddingLeft, pr: cs.paddingRight, width: el.getBoundingClientRect().width, text: r.getBoundingClientRect().width, height: el.getBoundingClientRect().height };
        });
        expect(m.pl, `${id} padding-left`).toBe('6px');
        expect(m.pr, `${id} padding-right`).toBe('6px');
        // The box is its label plus 12px: the padding is all the room it takes.
        expect(Math.abs(m.width - (m.text + 12)), `${id} is its label + 12px (${JSON.stringify(m)})`).toBeLessThan(1);
        expect(m.height, `${id} is a 44px target`).toBeGreaterThanOrEqual(44);
      }
    });

    // 2. 🔴 RED AT THE BASE: the title was a plain <h1>; no popover existed.
    if (vp.w === 390) {
      test('a clipped header title opens the whole title in a popover; Escape and a tap outside close it; no shift', async ({ page }) => {
        await ready(page);
        await openView(page, LONG);
        await settle(page);
        expect((await headerFit(page)).clipped, 'the 42-character title is clipped at 390px').toBe(true);
        await expect(title(page)).toBeEnabled();
        await expect(pop(page)).toBeHidden();
        // 🔴 A 44px TARGET (red at 9ec9ee40, a 20px line): the points 21px
        // above and below the title's middle — over the header's padding and
        // the status row — are the button's, and the header did not grow.
        const hit = await title(page).evaluate((b) => {
          const r = b.getBoundingClientRect();
          const col = b.closest('header > div')!.getBoundingClientRect();
          const x = r.left + Math.min(r.width, 40) / 2, mid = col.top + col.height / 2;
          const at = (y: number) => b.contains(document.elementFromPoint(x, y));
          return { up: at(mid - 21), down: at(mid + 21), header: document.querySelector('header')!.getBoundingClientRect().height };
        });
        expect(hit, 'the title is a 44px target and the header is its old 57px').toEqual({ up: true, down: true, header: 57 });
        await title(page).click();
        await expect(pop(page)).toBeVisible();
        // The whole header line, the badge's host included.
        await expect(pop(page)).toHaveText(`${titleOf(LONG)} · Mirrored · ${rig.host}`);
        await expect(title(page)).toHaveAttribute('aria-expanded', 'true');
        const box = (await pop(page).boundingBox())!;
        expect(box.x, 'the popover starts inside the screen').toBeGreaterThanOrEqual(0);
        expect(box.x + box.width, 'the popover ends inside the screen').toBeLessThanOrEqual(390);
        await page.keyboard.press('Escape');
        await expect(pop(page)).toBeHidden();
        await expect(title(page)).toHaveAttribute('aria-expanded', 'false');
        // By keyboard: focus the title, Enter opens it.
        await title(page).focus();
        await page.keyboard.press('Enter');
        await expect(pop(page)).toBeVisible();
        // Light dismiss: a tap on the conversation closes it.
        await page.mouse.click(195, 500);
        await expect(pop(page)).toBeHidden();
        expect(await overflowX(page), 'no sideways scroll at 390px').toBe(0);
        const cls = await readCls(page);
        expect(cls.total, `layout shift ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);
      });
    } else {
      // The same 42-character title fits a desktop header: the title is not
      // interactive, and a tap on it opens nothing.
      test('a header title that fits is not a popover trigger', async ({ page }) => {
        await ready(page);
        await openView(page, LONG);
        await settle(page);
        expect((await headerFit(page)).clipped, 'the title fits at 1280px').toBe(false);
        await expect(title(page)).toBeDisabled();
        await title(page).click({ force: true });
        await page.waitForTimeout(300);
        await expect(pop(page)).toBeHidden();
        const cls = await readCls(page);
        expect(cls.total, `layout shift ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);
      });
    }

    // 3. 🔴 RED AT THE BASE (b613a382, 26rem): the 84-character title was
    // CLIPPED at both sizes, and at 390 so were the 34-character and longer
    // ones. Operator ask: the pills must be "always wide enough to show the full
    // title". Every window's title is now drawn whole — in the rendered view's
    // All row and in the agent page's Pins row — its pill as wide as its label,
    // the row scrolling sideways (the positive control: at 390 there IS more
    // than fits), the page never.
    test('the switcher shows every title whole, below the rendered view and in the agent page\'s pins; the row scrolls, the page does not; no shift', async ({ page }) => {
      await ready(page);
      await openView(page, LONG);
      await settle(page);
      const whole = async (view: string, skip?: string) => {
        const out: string[] = [];
        for (const [s] of WINDOWS) {
          if (s === skip) continue; // the page's own entry is the "current" chip
          const label = switcherLabel(page, titleOf(s), view);
          const f = await label.evaluate((el) => {
            const h = el as HTMLElement;
            const pill = h.parentElement!, entry = pill.parentElement!;
            const lr = h.getBoundingClientRect(), pr = pill.getBoundingClientRect();
            return {
              text: (h.textContent || '').trim(),
              clipped: h.scrollWidth > h.clientWidth + 1,
              // The label inside its pill, and the pill inside its entry.
              outside: lr.right > pr.right + 0.5 || pr.right > entry.getBoundingClientRect().right + 0.5 || pill.scrollWidth > pill.clientWidth + 1,
            };
          });
          if (f.text !== titleOf(s) || f.clipped || f.outside) out.push(`${view}: ${titleOf(s)} ${JSON.stringify(f)}`);
        }
        return out;
      };
      expect(await whole('all', LONG), 'every title in the rendered view\'s All row is whole').toEqual([]);
      const rowScroll = await page.locator('#session-switcher [data-switcher-list="all"]').evaluate((el) => [el.scrollWidth, el.clientWidth]);
      expect(rowScroll[0], `the row itself scrolls sideways (there is more than fits): ${rowScroll}`).toBeGreaterThan(rowScroll[1]);
      expect(await overflowX(page), 'the page never scrolls sideways').toBe(0);
      let cls = await readCls(page);
      expect(cls.total, `rendered view layout shift ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);

      await openAgent(page, 'pins');
      await settle(page);
      expect(await whole('pins'), 'every title in the agent page\'s Pins row is whole').toEqual([]);
      expect(await overflowX(page), 'the page never scrolls sideways').toBe(0);
      cls = await readCls(page);
      expect(cls.total, `agent page layout shift ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);
    });

    // A 14rem entry once left the Recent strip's label 156px; the switcher's
    // Recent view, on an agent's page, draws the title whole.
    test('the switcher\'s Recent view, on an agent\'s page, draws a 27- and a 34-character title whole; no shift', async ({ page }) => {
      await ready(page);
      await visitUnpinned(page);
      await openAgent(page, 'recent');
      await settle(page);
      for (const s of RECENT_FITS) {
        await expect(recentLabel(page, titleOf(s))).toBeVisible();
        const f = await recentLabel(page, titleOf(s)).evaluate(fitOf);
        expect(f.clipped, `Recent: ${titleOf(s)} fits (${JSON.stringify(f)})`).toBe(false);
      }
      if (vp.w === 390) expect(await overflowX(page), 'no sideways scroll at 390px').toBe(0);
      const cls = await readCls(page);
      expect(cls.total, `layout shift ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);
    });

    // At 390 a pinned chip once left a 36-character title 302px of its 315;
    // the switcher's Pins view, below a rendered view, draws it whole.
    test('the switcher\'s Pins view, below a rendered view, draws a 36-character title whole; no sideways scroll; no shift', async ({ page }) => {
      await ready(page);
      await openView(page, LONG, 'pins');
      await settle(page);
      await expect(chipLabel(page, titleOf(CHIP_FITS))).toBeVisible();
      const f = await chipLabel(page, titleOf(CHIP_FITS)).evaluate(fitOf);
      expect(f.clipped, `${titleOf(CHIP_FITS)} fits (${JSON.stringify(f)})`).toBe(false);
      if (vp.w === 390) expect(await overflowX(page), 'no sideways scroll at 390px').toBe(0);
      const cls = await readCls(page);
      expect(cls.total, `layout shift ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);
    });
  });
}

// A REAL TAP on a phone. At 393px the header row holds three 44px icon
// buttons and the Rendered | Terminal nav, which leaves a title about 70px:
// the tap is how a phone reads a title at all. The viewport, touch and mobile
// flags are set HERE, not inherited from a project default, so this case
// cannot silently become a desktop click. `.tap()` dispatches touch events;
// `.click()` or a scripted showPopover() would prove the mouse path instead.
test.describe('phone 393×852, a real tap', () => {
  test.use({ viewport: { width: 393, height: 852 }, isMobile: true, hasTouch: true });

  const TAPPED = 'customer-onboarding-email-sequence'; // 36 characters
  const W = 393;
  const toggleBox = (loc: Locator) =>
    loc.evaluate((el) => {
      const r = el.getBoundingClientRect();
      return { w: r.width, h: r.height, display: getComputedStyle(el).display };
    });
  // The switch's displayed children (its script excluded): what a reader sees.
  const shownSegments = (root: Page | FrameLocator) =>
    root.locator('nav[aria-label="View"]').evaluate((nav) =>
      Array.from(nav.children).filter((c) => c.tagName !== 'SCRIPT' && getComputedStyle(c).display !== 'none').map((c) => c.id),
    );

  // 🔴 RED AT fade163f: the two-segment switch (147px) left the title 70px.
  test('the switch is ONE 44×44 icon button to the other view; the title gets the room; a tap goes there and back', async ({ page }) => {
    await ready(page);
    await openView(page, TAPPED);
    await settle(page);
    expect(page.viewportSize(), 'the pinned phone viewport').toEqual({ width: W, height: 852 });
    const fit = await headerFit(page);
    const navW = await page.locator('nav[aria-label="View"]').evaluate((n) => n.getBoundingClientRect().width);
    console.log('TITLE-ROOM-393 ' + JSON.stringify({ title: fit, nav: navW }));
    expect(fit.client, `the title box is at least 150px (${JSON.stringify(fit)})`).toBeGreaterThanOrEqual(150);
    // One displayed control in the switch: the link to the terminal. The
    // current segment takes no width (display:none), not merely invisible.
    expect(await shownSegments(page), 'the switch shows only its link').toEqual(['view-terminal']);
    await expect(page.locator('#view-rendered')).toHaveCSS('display', 'none');
    const toggle = page.getByRole('link', { name: 'Show terminal', exact: true });
    await expect(toggle).toBeVisible();
    await expect(toggle).toHaveAttribute('data-view-toggle', '');
    await expect(toggle, 'a mouse gets the name as a tooltip').toHaveAttribute('title', 'Show terminal');
    await expect(toggle.locator('svg')).toBeVisible();
    const box = await toggleBox(page.locator('#view-terminal'));
    expect(box.w, `the toggle is at least 44px wide (${JSON.stringify(box)})`).toBeGreaterThanOrEqual(44);
    expect(box.h, `the toggle is at least 44px tall (${JSON.stringify(box)})`).toBeGreaterThanOrEqual(44);
    // Keyboard order: Tab from the chat-index button reaches the toggle, never
    // the hidden segment.
    await page.locator('#view-terminal').focus();
    await page.keyboard.press('Shift+Tab');
    await page.keyboard.press('Tab');
    expect(await page.evaluate(() => document.activeElement && document.activeElement.id), 'Tab lands on the toggle').toBe('view-terminal');
    expect(await overflowX(page), 'no sideways scroll on the rendered view at 393px').toBe(0);

    // A real tap: the terminal opens (in the view frame) and its own switch
    // is the one icon back.
    await toggle.tap();
    const frame = page.frameLocator('iframe[data-muster-view]');
    await expect(page.locator('iframe[data-muster-view]')).toBeVisible();
    await expect(frame.locator('#view-terminal')).toHaveAttribute('aria-current', 'page');
    await expect.poll(() => new URL(page.url()).pathname).toBe('/mirror/terminal');
    expect(await shownSegments(frame), 'the terminal\'s switch shows only its link').toEqual(['view-rendered']);
    await expect(frame.locator('#view-terminal')).toHaveCSS('display', 'none');
    const back = frame.getByRole('link', { name: 'Show rendered view', exact: true });
    await expect(back).toBeVisible();
    await expect(back).toHaveAttribute('title', 'Show rendered view');
    const tb = await toggleBox(frame.locator('#view-rendered'));
    expect(Math.min(tb.w, tb.h), `the terminal's toggle is at least 44×44 (${JSON.stringify(tb)})`).toBeGreaterThanOrEqual(44);
    // The terminal view's header does not scroll sideways at 393px either.
    const termOverflow = await frame.locator('html').evaluate((h) => ({ page: h.scrollWidth - h.clientWidth, header: (document.querySelector('header') as HTMLElement).scrollWidth - (document.querySelector('header') as HTMLElement).clientWidth }));
    expect(termOverflow, 'no sideways scroll on the terminal view or its header at 393px').toEqual({ page: 0, header: 0 });
    expect(await overflowX(page), 'no sideways scroll at 393px').toBe(0);

    await back.tap();
    await expect(page.locator('iframe[data-muster-view]')).toBeHidden();
    await expect.poll(() => new URL(page.url()).pathname).toBe('/mirror/view');
    await expect(page.getByRole('link', { name: 'Show terminal', exact: true })).toBeVisible();
  });

  test('a tap on a clipped header title opens the whole title inside the screen; Escape closes it', async ({ page }) => {
    await ready(page);
    await openView(page, TAPPED);
    await settle(page);
    expect(page.viewportSize(), 'the pinned phone viewport').toEqual({ width: 393, height: 852 });
    const fit = await headerFit(page);
    expect(fit.clipped, `the 36-character title is clipped at 393px (${JSON.stringify(fit)})`).toBe(true);
    const trigger = page.locator('[data-title-fit=disable] [data-title-more]');
    await expect(trigger).toHaveCount(1);
    await expect(trigger).toBeEnabled();
    await expect(pop(page)).toBeHidden();

    await trigger.tap();
    await expect.poll(() => pop(page).evaluate((el) => el.matches(':popover-open')), { message: '#mv-title-full is :popover-open after a tap' }).toBe(true);
    await expect(pop(page)).toContainText(titleOf(TAPPED));
    const r = await pop(page).evaluate((el) => {
      const b = el.getBoundingClientRect();
      return { left: b.left, right: b.right };
    });
    expect(r.left, `the popover starts inside the screen (${JSON.stringify(r)})`).toBeGreaterThanOrEqual(0);
    expect(r.right, `the popover ends inside the screen (${JSON.stringify(r)})`).toBeLessThanOrEqual(393);
    expect(await overflowX(page), 'no sideways scroll at 393px').toBe(0);

    await page.keyboard.press('Escape');
    await expect.poll(() => pop(page).evaluate((el) => el.matches(':popover-open')), { message: '#mv-title-full is closed after Escape' }).toBe(false);
    await expect(pop(page)).toBeHidden();
    expect(await overflowX(page), 'no sideways scroll at 393px').toBe(0);
  });
});
