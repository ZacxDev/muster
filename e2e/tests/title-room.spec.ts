import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { titleRoomPort } from '../lib/env';
import { clsInit, readCls, settle } from '../lib/cls';
import { cc, MirrorRig } from '../lib/mirrorhost';

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
//      the agent page's pinned list — and the Agents list's Recent strip are
//      up to 26rem wide (were 10rem and 14rem, then 20rem), never wider than
//      their row;
//   4. the Agents list's pinned chips are padded 10px a side with a 4px gap
//      (were 16px and 6px): they have no max-width, but at 390px a chip runs
//      to the row's edge, and that padding is what clipped a 36-character
//      title.
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
// 49, 52 and 84.
const WINDOWS: [string, string][] = [
  ['release-notes-drafter', '@1'],
  ['billing-reconcile-nightly', '@2'],
  ['payments-ledger-migration-review', '@3'],
  ['customer-onboarding-email-sequence', '@4'],
  ['warehouse-inventory-sync-backfill-job-v2', '@5'],
  ['quarterly-revenue-forecast-reconciliation-batch', '@9'],
  ['marketplace-seller-payout-dispute-resolution-queue', '@10'],
  ['enterprise-customer-data-warehouse-schema-migration-validation-and-audit-report-v3', '@11'],
];
// Windows that are NOT pinned, so the Agents list's Recent strip shows them
// once visited (a pinned entry is never in the strip). Lengths 27, 34, 36.
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

// Once per rig: the windows are listed (as cards: once pinned they are chips,
// which carry no data-mirror-claude), then every one is pinned.
let pinned = false;
async function ready(page: Page) {
  await rig.login(page);
  if (pinned) return;
  await expect
    .poll(async () => ((await (await page.request.get(origin + '/ui/agents')).text()).match(/data-mirror-claude/g) || []).length, {
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

// The switcher's entry label for a window in a view (the truncating span).
const switcherLabel = (page: Page, title: string, view = 'all') =>
  page.locator(`#session-switcher [data-switcher-list="${view}"] [data-switcher-entry] span.truncate`, { hasText: title }).first();
const chipLabel = (page: Page, title: string) => page.locator(`#agents-list [data-pinned-chip-label]`, { hasText: title }).first();
const recentLabel = (page: Page, title: string) => page.locator(`#agents-recent a[data-recent-entry] span.truncate`, { hasText: title }).first();

// visitUnpinned opens each unpinned window's rendered view once, so the
// Recent strip lists them.
async function visitUnpinned(page: Page) {
  for (const [s, w] of UNPINNED) {
    await page.goto(viewOf(s, w));
    await expect(page.locator('#view-rendered')).toBeVisible();
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
  await expect(page.locator('#view-rendered')).toBeVisible();
  await expect(page.locator('#session-switcher')).toBeVisible();
}

async function openAgent(page: Page, view: string) {
  await setView(page, view);
  await page.addInitScript(clsInit);
  await page.goto(origin + '/agents/docs-sweeper');
  await page.waitForURL((u) => u.pathname === '/agents/docs-sweeper' && u.searchParams.has('session'));
  await expect(page.locator('#session-switcher')).toBeVisible();
}

// The titles the width assertions use. In the switcher (a 114px label at the
// first base, so every one was clipped) the 23- and 27-character titles fit at
// both sizes; at 1280 the 34-, 36- and 49-character ones fit too, while at 390
// the bar's own row (286px) is the cap.
//
// 🔴 THE 49-CHARACTER TITLE IS THE 26rem ASK: at 20rem a switcher label got
// 274px and it needs 337 (red there); a 26rem entry gives 370. The
// 52-character window measures 368 — it fits too, with 2px to spare on the
// machine that measured it, so it is in the measurement and NOT asserted (a
// system font stack makes 2px a host property, not a contract). The
// 84-character one (605px) still clips at 1280: the cap is a cap.
const FITS_NOW = ['release-notes-drafter', 'billing-reconcile-nightly'];
const FITS_DESKTOP = ['payments-ledger-migration-review', 'customer-onboarding-email-sequence', 'quarterly-revenue-forecast-reconciliation-batch'];
const STILL_CLIPS = 'payments-ledger-migration-review';
const VERY_LONG = 'enterprise-customer-data-warehouse-schema-migration-validation-and-audit-report-v3';
// The cap, in px: 26rem.
const CAP_PX = 416;
// The pinned chip (Agents list) a 390px row clipped at the base: 315px of
// label in 302px. And the Recent strip's: 27 and 34 characters (a 156px
// label at the base, a 252px one here).
const CHIP_FITS = 'customer-onboarding-email-sequence';
const RECENT_FITS = ['cache-warmup-orchestrator', 'inventory-forecast-model-refresh'];

// entryBox is a switcher entry's width beside its scrolling row's.
const entryBox = (page: Page, title: string, view: string) =>
  switcherLabel(page, title, view).evaluate((el) => ({
    clipped: (el as HTMLElement).scrollWidth > (el as HTMLElement).clientWidth + 1,
    box: el.closest('[data-switcher-entry]')!.getBoundingClientRect().width,
    row: (el.closest('[data-switcher-list]') as HTMLElement).clientWidth,
  }));

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
      await page.goto(origin + '/agents');
      await expect(chipLabel(page, titleOf(WINDOWS[0][0]))).toBeVisible();
      const chips: Record<string, Fit> = {};
      for (const [s] of WINDOWS) chips[titleOf(s)] = await chipLabel(page, titleOf(s)).evaluate(fitOf);
      out.agentsListChips = chips;
      await visitUnpinned(page);
      await page.goto(origin + '/agents');
      const recent: Record<string, Fit> = {};
      for (const [s] of UNPINNED) {
        await expect(recentLabel(page, titleOf(s))).toBeVisible();
        recent[titleOf(s)] = await recentLabel(page, titleOf(s)).evaluate(fitOf);
      }
      out.agentsRecentStrip = recent;
      out.overflowX = await overflowX(page);
      console.log('TITLE-ROOM ' + JSON.stringify(out));
    });

    // 1. 🔴 RED AT THE BASE: each segment's side padding was 10px.
    test('the Rendered | Terminal segments are padded 6px a side, still 44px tall', async ({ page }) => {
      await ready(page);
      await openView(page, LONG);
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

    // 3 and 4. 🔴 RED AT THE BASE: an entry's label had 114px, so every title
    // was clipped — in the rendered view's bar (All) and in the agent page's
    // pinned list (Pins).
    test('the switcher fits realistic titles, below the rendered view and in the agent page\'s pins; never wider than its row; no shift', async ({ page }) => {
      await ready(page);
      await openView(page, LONG);
      await settle(page);
      const fits = vp.w === 390 ? FITS_NOW : [...FITS_NOW, ...FITS_DESKTOP];
      for (const s of fits) {
        const f = await switcherLabel(page, titleOf(s)).evaluate(fitOf);
        expect(f.clipped, `rendered view, All: ${titleOf(s)} fits (${JSON.stringify(f)})`).toBe(false);
      }
      // The cap: at most 26rem, and never wider than the row it scrolls in.
      const b = await entryBox(page, titleOf(STILL_CLIPS), 'all');
      expect(b.box, `an entry is at most 26rem (${JSON.stringify(b)})`).toBeLessThanOrEqual(CAP_PX);
      expect(b.box, `an entry is never wider than its row (${JSON.stringify(b)})`).toBeLessThanOrEqual(b.row);
      if (vp.w === 390) {
        expect(b.clipped, 'at 390 the row is the cap: a 34-character title clips').toBe(true);
        expect(await overflowX(page), 'no sideways scroll at 390px').toBe(0);
      }
      // An 84-character title still clips at both sizes, its entry AT the cap
      // (26rem at 1280, the row at 390): the cap still exists.
      const v = await entryBox(page, titleOf(VERY_LONG), 'all');
      expect(v.clipped, `an 84-character title still clips (${JSON.stringify(v)})`).toBe(true);
      expect(Math.abs(v.box - Math.min(CAP_PX, v.row)), `its entry is exactly the cap (${JSON.stringify(v)})`).toBeLessThan(1);
      let cls = await readCls(page);
      expect(cls.total, `rendered view layout shift ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);

      await openAgent(page, 'pins');
      await settle(page);
      for (const s of fits) {
        const f = await switcherLabel(page, titleOf(s), 'pins').evaluate(fitOf);
        expect(f.clipped, `agent page, Pins: ${titleOf(s)} fits (${JSON.stringify(f)})`).toBe(false);
      }
      const l = await entryBox(page, titleOf(VERY_LONG), 'pins');
      expect(l.clipped, `an 84-character title still clips (the cap is a cap) (${JSON.stringify(l)})`).toBe(true);
      expect(l.box, `never wider than its row (${JSON.stringify(l)})`).toBeLessThanOrEqual(Math.min(CAP_PX, l.row));
      if (vp.w === 390) expect(await overflowX(page), 'no sideways scroll at 390px').toBe(0);
      cls = await readCls(page);
      expect(cls.total, `agent page layout shift ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);
    });

    // 🔴 RED AT THE BASE: a 14rem entry left the Recent strip's label 156px.
    test('the Agents list\'s Recent strip fits a 27- and a 34-character title; no shift', async ({ page }) => {
      await ready(page);
      await visitUnpinned(page);
      await page.addInitScript(clsInit);
      await page.goto(origin + '/agents');
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

    // 🔴 RED AT THE BASE (at 390): the chip runs to the row's edge, and its
    // 16px padding and 6px gap left a 36-character title 302px of its 315.
    test('a pinned chip on the Agents list fits a 36-character title; no sideways scroll; no shift', async ({ page }) => {
      await ready(page);
      await page.addInitScript(clsInit);
      await page.goto(origin + '/agents');
      await settle(page);
      const f = await chipLabel(page, titleOf(CHIP_FITS)).evaluate(fitOf);
      expect(f.clipped, `${titleOf(CHIP_FITS)} fits (${JSON.stringify(f)})`).toBe(false);
      const chip = await page.locator('#agents-list [data-pinned-chip-link]', { hasText: titleOf(CHIP_FITS) }).first().evaluate((el) => {
        const cs = getComputedStyle(el);
        return { pl: cs.paddingLeft, pr: cs.paddingRight, gap: cs.columnGap, h: el.getBoundingClientRect().height };
      });
      expect(chip, 'the chip is padded 10px a side with a 4px gap, still a 44px target').toEqual({ pl: '10px', pr: '10px', gap: '4px', h: 44 });
      if (vp.w === 390) expect(await overflowX(page), 'no sideways scroll at 390px').toBe(0);
      const cls = await readCls(page);
      expect(cls.total, `layout shift ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);
    });
  });
}
