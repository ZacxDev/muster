import fs from 'node:fs';
import path from 'node:path';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { agentsIdlePort } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { clsInit, readCls, resetCls, settle, ClsResult } from '../lib/cls';
import { writeFileAtomic } from '../lib/atomic';

// THE IDLE GROUP, IN THE SIDEBAR (internal/ui/nav_agents.go navIdleGroup — the
// retired Agents list's Idle section, internal/ui/idle_section.go before), in
// a real browser at a phone and a desktop, against a real muster-server with
// the DEFAULT threshold (MUSTER_AGENTS_IDLE_AFTER unset: 14 days) and a real
// muster-mirror over the tunnel whose tmux is the e2e fake:
//
//   1. Windows with no activity for more than 14 days are drawn last, under a
//      COLLAPSED "Idle (N)" disclosure — collapsed on the first paint, no shift.
//   2. A pinned window never collapses, however old.
//   3. The disclosure opens and closes the group; the choice survives a reload
//      (localStorage muster.agents.idle.open) — no shift either way.
//   4. The palette finds an idle window while the group is collapsed.
//   5. A window that becomes active again leaves the group on the next live
//      refresh (no hold: the sidebar has no "N new" pill), and the count
//      follows.
//
// 🔴 REGRESSION TESTS: on the tree before, there was no section — every window
// was drawn in the one list however long it had been idle.
//
// Activity times are pairwise distinct; the nearest to the boundary on either
// side is 13 days 20 hours (working) and 15 days 3 hours (idle). The boundary
// itself is the Go suite's (InIdleSection, the API test with an injected clock).

const BAR = 0.01;
const rig = new MirrorRig(agentsIdlePort, 'e2e-idle', { MUSTER_FOCUS_INTERVAL: '0' }, { decimalHost: true });
const day = 86400;
const now = Math.floor(Date.now() / 1000);
// session -> seconds idle. Names carry no letter the seeded agents' cards hold
// together, so a search for one finds only it.
const WORKING: Record<string, number> = { ia1: 180, ia2: 2 * 3600 + 17 * 60, ia3: 6 * day + 5 * 3600, ia4: 13 * day + 20 * 3600 };
const IDLE: Record<string, number> = { iz1: 15 * day + 3 * 3600, iz2: 21 * day + 7 * 3600, iz3: 40 * day + 11 * 3600 };
const PINNED = { izpin: 50 * day + 13 * 3600 };
const ALL = { ...WORKING, ...IDLE, ...PINNED };
const OPEN_KEY = 'muster.agents.idle.open';
let windowsFile = '';
let pinned = false;

function writeWindows(over: Record<string, number> = {}) {
  const lines = Object.entries({ ...ALL, ...over }).map(([s, ago], i) => `${s}|@${i + 1}|1|1|zsh|${now - ago}|`);
  writeFileAtomic(windowsFile, lines.join('\n') + '\n');
}

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(120_000);
  await rig.start();
  windowsFile = path.join(rig.dir, 'windows');
  writeWindows();
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
});

test.afterAll(async () => {
  await rig.stop();
});

// row is one of this host's windows' sidebar rows, by its tmux session.
const rowOf = (page: Page, host: string, s: string) => page.locator(`#sidebar-agents-list a[data-nav-agent="mirror"][href*="host=${host}&session=${s}&"]`);
const row = (page: Page, s: string) => rowOf(page, rig.host, s);
const group = (page: Page) => page.locator('#sidebar-idle');
const summary = (page: Page) => page.locator('#sidebar-idle > summary');
// inGroup: the row is drawn in the Idle group.
const inGroup = (page: Page, s: string) => page.locator(`#sidebar-idle a[data-nav-agent="mirror"][href*="host=${rig.host}&session=${s}&"]`);

async function open(page: Page) {
  await rig.login(page);
  await rig.waitListed(page, 'data-f-session="iz3"');
  if (!pinned) {
    const i = Object.keys(ALL).indexOf('izpin') + 1;
    const r = await page.request.post(rig.origin + '/agents/pins', {
      form: { kind: 'mirror', host: rig.host, session: 'izpin', window: `@${i}` },
      headers: { Origin: rig.origin },
    });
    expect(r.status(), 'pinned izpin').toBe(200);
    pinned = true;
  }
  await page.addInitScript(clsInit);
}

// tapByScript clicks without input: a scripted click is not the reader's, so
// whatever it moves is counted by the layout-shift record.
const tapByScript = (page: Page, sel: string) => page.evaluate((s) => (document.querySelector(s) as HTMLElement).click(), sel);

// showList loads `at` and shows the sidebar's list: the rail on a desktop, the
// drawer on a phone — opened by script, so what its opening moves is counted.
async function showList(page: Page, at: string) {
  await page.goto(rig.origin + at);
  if ((page.viewportSize()?.width ?? 0) < 1024) {
    await tapByScript(page, '#sidebar-open');
    await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });
  }
  await expect(row(page, 'ia1')).toBeVisible({ timeout: 20_000 });
}

for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('idle windows are drawn last, collapsed; open/close persists; the palette finds them; no shift', async ({ page }) => {
      test.setTimeout(120_000);
      await open(page);
      await page.goto(rig.origin + '/account');
      await page.evaluate((k) => localStorage.removeItem(k), OPEN_KEY);
      const results: Record<string, ClsResult> = {};
      await showList(page, '/account');
      await settle(page);
      results['load, collapsed'] = await readCls(page);

      // 1. Collapsed: the working windows and the pinned one shown, the idle hidden.
      for (const s of Object.keys(WORKING)) await expect(row(page, s), `${s} (working) is shown`).toBeVisible();
      await expect(row(page, 'izpin'), 'a pinned window never collapses').toBeVisible();
      await expect(inGroup(page, 'izpin')).toHaveCount(0);
      for (const s of Object.keys(IDLE)) {
        await expect(inGroup(page, s), `${s} (idle) is in the group`).toHaveCount(1);
        await expect(row(page, s), `${s} is collapsed`).toBeHidden();
      }
      for (const s of Object.keys(WORKING)) await expect(inGroup(page, s), `${s} (working) is not in the group`).toHaveCount(0);
      await expect(summary(page)).toBeVisible();
      await expect(summary(page).locator('[data-nav-idle-count]')).toHaveText('(3)');
      await expect(group(page)).toHaveJSProperty('open', false);
      // The group is drawn after every working row.
      const below = await page.evaluate(() => {
        const h = document.querySelector('#sidebar-idle > summary')!.getBoundingClientRect().top;
        return [...document.querySelectorAll('#sidebar-agents-list > ul a[data-nav-agent]')]
          .filter((a) => a.getClientRects().length)
          .every((a) => a.getBoundingClientRect().top < h);
      });
      expect(below, 'the Idle group is below every working row').toBe(true);

      // 3. Opened (a scripted click: not input, so any shift counts).
      await resetCls(page);
      await tapByScript(page, '#sidebar-idle > summary');
      for (const s of Object.keys(IDLE)) await expect(row(page, s), `${s} shown once open`).toBeVisible();
      await expect(group(page)).toHaveJSProperty('open', true);
      await settle(page, 500);
      results['open'] = await readCls(page);
      expect(await page.evaluate((k) => localStorage.getItem(k), OPEN_KEY)).toBe('1');

      // Reloaded: open again.
      await showList(page, '/account');
      await settle(page);
      results['load, restored open'] = await readCls(page);
      for (const s of Object.keys(IDLE)) await expect(row(page, s)).toBeVisible();

      // Closed, and reloaded closed.
      await resetCls(page);
      await tapByScript(page, '#sidebar-idle > summary');
      for (const s of Object.keys(IDLE)) await expect(row(page, s)).toBeHidden();
      await settle(page, 500);
      results['close'] = await readCls(page);
      expect(await page.evaluate((k) => localStorage.getItem(k), OPEN_KEY)).toBe('0');
      await showList(page, '/account');
      await settle(page);
      await expect(row(page, 'iz2')).toBeHidden();

      // 4. The palette (the sidebar's magnifier) finds an idle window without
      //    opening the group.
      await page.locator('#sidebar-search').click();
      await page.locator('#switcher-search-input').fill('iz2');
      await expect(page.locator('#switcher-search-results [role="option"]'), 'the palette finds the idle window').toHaveCount(1);
      await page.keyboard.press('Escape');
      await expect(row(page, 'iz2'), 'the group stays collapsed').toBeHidden();
      await expect(summary(page).locator('[data-nav-idle-count]')).toHaveText('(3)');

      console.log(`[idle ${vp.name}] ` + Object.entries(results).map(([k, r]) => `${k} ${r.total}`).join(' | '));
      const bad = Object.entries(results).filter(([, r]) => r.total >= BAR).map(([k, r]) => `${k}: ${r.total} ${JSON.stringify(r.shifts)}`);
      expect(bad).toEqual([]);
      // POSITIVE CONTROL: the same instrument sees a shift in the list when
      // one happens — a block pushed in above the rows. (Past the 500 ms after
      // the Escape above, inside which a shift counts as input's.)
      await page.waitForTimeout(600);
      await resetCls(page);
      await page.evaluate(() => {
        const d = document.createElement('div');
        d.style.height = '160px';
        document.querySelector('#sidebar-agents-list')!.prepend(d);
      });
      await settle(page);
      expect((await readCls(page)).total, 'the instrument can observe a shift').toBeGreaterThan(BAR);
    });

    test('a window active again leaves the group on the next live refresh, and the count follows', async ({ page }) => {
      test.setTimeout(120_000);
      await open(page);
      await page.goto(rig.origin + '/account');
      await page.evaluate((k) => localStorage.removeItem(k), OPEN_KEY);
      // A page that holds the event stream: the sidebar's live refresh is
      // heard only on such a document. A seeded agent's page.
      await showList(page, '/agents/notes-tidy');
      await expect(inGroup(page, 'iz2')).toHaveCount(1);
      await expect(summary(page).locator('[data-nav-idle-count]')).toHaveText('(3)');
      let navigations = 0;
      page.on('framenavigated', (f) => {
        if (f === page.mainFrame()) navigations++;
      });
      try {
        // iz2 used a minute ago: the host reports it on its next listing.
        writeWindows({ iz2: 60 });
        await expect(row(page, 'iz2'), 'it leaves the collapsed group, shown').toBeVisible({ timeout: 15_000 });
        await expect(inGroup(page, 'iz2')).toHaveCount(0);
        await expect(summary(page).locator('[data-nav-idle-count]'), 'and the count follows').toHaveText('(2)');
        expect(navigations, 'live, without a page load').toBe(0);
        // A reload agrees: it is a working window now.
        await showList(page, '/account');
        await expect(row(page, 'iz2')).toBeVisible();
      } finally {
        writeWindows();
      }
      // Idle again (the host reports its old time): back in the group.
      await expect
        .poll(async () => (await (await page.request.get(rig.origin + '/ui/nav/agents')).text()).match(/data-nav-idle-count="" class="tabular-nums">\((\d+)\)/)?.[1], { timeout: 15_000 })
        .toBe('3');
    });
  });
}

// 🔴 A CLAUDE WINDOW IS IDLE BY ITS LAST MESSAGE, NOT BY TMUX. tmux's window
// activity is reset for every window by a tmux restart (a tmux-resurrect
// restore), so on a real host every window read "active a minute ago". A
// second host here lists one Claude window whose tmux activity is a minute old
// and whose transcript's last message is 30 days old, followed by what a
// RESUME appends (an attachment and a system row with today's time): the real
// muster-mirror reads the last message (internal/mirrorhost/lastmsg.go), sends
// it, and the row is in the Idle group, its recency dot dated 30 days back.
test('a Claude window with fresh tmux activity but a 30-day-old last message is idle, and says so', async ({ page }) => {
  test.setTimeout(120_000);
  const hostL = rig.host.replace('e2e-idle-', 'e2e-idlel-');
  const tr = path.join(rig.dir, 'claude-old.jsonl');
  const oldMs = Date.now() - 30 * day * 1000 - 5 * 3600 * 1000;
  const old = new Date(oldMs).toISOString();
  const fresh = new Date().toISOString();
  writeFileAtomic(
    tr,
    [
      { type: 'user', timestamp: old, message: { role: 'user', content: 'tidy the notes' } },
      { type: 'assistant', timestamp: old, message: { role: 'assistant', content: [{ type: 'text', text: 'Done.' }] } },
      { type: 'cost-state' },
      { type: 'attachment', timestamp: fresh, attachment: { type: 'deferred_tools_delta' } },
      { type: 'system', subtype: 'informational', timestamp: fresh, content: 'resumed' },
    ].map((r) => JSON.stringify(r)).join('\n') + '\n',
  );
  // The same 30-day-old conversation in a second window whose session is
  // WORKING (a tool running: Claude Code writes no row meanwhile). Busy counts
  // as active: that row reads active now, and is not idle.
  const trBusy = path.join(rig.dir, 'claude-busy.jsonl');
  fs.copyFileSync(tr, trBusy);
  writeFileAtomic(trBusy + '.status', JSON.stringify({ status: 'busy', statusUpdatedAt: Date.now() - 40 * 60_000 }));
  const lwin = path.join(rig.dir, 'windows-l');
  writeFileAtomic(lwin, `iclaude|@1|1|1|claude|${Math.floor(Date.now() / 1000) - 60}|${tr}\nicbusy|@2|1|1|claude|${Math.floor(Date.now() / 1000) - 60}|${trBusy}\n`);
  const stop = rig.extraHost(hostL, { MUSTER_MIRROR_E2E_WINDOWS_FILE: lwin, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
  try {
    await rig.login(page);
    await rig.waitListed(page, 'data-f-session="iclaude"');
    await page.goto(rig.origin + '/account');
    await page.evaluate((k) => localStorage.removeItem(k), OPEN_KEY);
    await showList(page, '/account');
    const c = rowOf(page, hostL, 'iclaude');
    await expect(page.locator(`#sidebar-idle a[href*="host=${hostL}&session=iclaude&"]`), 'the row is in the Idle group').toHaveCount(1);
    await expect(c, 'collapsed with it').toBeHidden();
    await tapByScript(page, '#sidebar-idle > summary');
    await expect(c).toBeVisible();
    // Its recency dot is dated by the last message (30 days back), and wears
    // the oldest bucket.
    const ts = Number(await c.locator('[data-recency-ts]').getAttribute('data-recency-ts'));
    expect(Math.abs(ts - oldMs), `the dot's time ${new Date(ts).toISOString()} is the last message's ${old}`).toBeLessThan(3600 * 1000);
    const b = rowOf(page, hostL, 'icbusy');
    await expect(page.locator(`#sidebar-idle a[href*="host=${hostL}&session=icbusy&"]`), 'the working window is not idle').toHaveCount(0);
    await expect(b).toBeVisible();
    const bts = Number(await b.locator('[data-recency-ts]').getAttribute('data-recency-ts'));
    expect(Date.now() - bts, 'and reads active now, not 30 days ago').toBeLessThan(5 * 60 * 1000);
    expect(Number(await c.locator('[data-recency]').getAttribute('data-recency')), 'the idle one wears an older bucket').toBeGreaterThan(Number(await b.locator('[data-recency]').getAttribute('data-recency')));
    await tapByScript(page, '#sidebar-idle > summary');
  } finally {
    await stop();
  }
});
