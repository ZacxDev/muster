import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { dsn, repoRoot, notifyTmuxPort } from '../lib/env';
import { cc, MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';

// "SHOW TMUX SESSION IN NOTIFICATIONS" (internal/api/notify_prefs.go;
// docs/focus.md#show-tmux-sessions), in a real browser against
// a real muster-server (MUSTER_MIRROR=1, its own port) and a real
// muster-mirror over the tunnel (the `-tags e2emirror` fake), on a phone
// (390×844, touch) and a desktop (1280×800).
//
// Each viewport has a window of its own, WAITING on its owner (its
// transcript's .waiting file), whose conversation is named by its first good
// focus title (e2e/mkchat writes it as the refresh does). The Asks list calls
// it by that name. Then, on Account → Notifications:
//
//   1. the switch is OFF for the signed-in user who never chose, and the Asks
//      row is exactly the name;
//   2. turned ON, it survives a reload, and the Asks row reads
//      "<name> · <codename>:<index> @ <host>" — the window as muster named it
//      before sessions had names;
//   3. turned OFF again, the row is exactly the name again.
//
// The same switch draws the place on the session lists: the bar's pills and
// the dock's rows (one test), and the sidebar's rows and the search picker —
// the command palette, opened from the bottom bar's magnifier and from the
// sidebar's — with the sidebar's rows refreshed live over SSE (another).
//
// 🔴 REGRESSION TEST: on origin/main the Account page has no such switch and
// POST /account/notifications/tmux does not exist. The suite runs one worker
// (playwright.config.ts), and the operator's switch is deleted before and
// after, so no other spec sees it on.

const rig = new MirrorRig(notifyTmuxPort, 'e2e-ntmux');
const origin = rig.origin;
const host = rig.host;
const stamp = Date.now().toString(36);
const agentName = 'e2e-ntmux-' + stamp;
const windowsFile = path.join(rig.dir, 'windows');
const labelsFile = path.join(rig.dir, 'labels.json');
const now = Math.floor(Date.now() / 1000);
const sids = { phone: 'ntmux-phone-' + stamp, desktop: 'ntmux-desktop-' + stamp };
const windows = { phone: 1, desktop: 2 };
const names = { phone: 'Billing migration', desktop: 'Ledger cleanup' };
const unnamed = [3, 4, 5, 6, 7];
// An unnamed window each viewport's Asks test sets waiting, live, for its SSE
// refresh (not 7: the bar test's page is window 7's).
const unnamedWaitingOf = { phone: 4, desktop: 5 };
// The unnamed window the sidebar test sets waiting, live, for its SSE refresh.
const sidebarWaiting = 6;
const TAIL = [
  { role: 'user', text: 'Migrate the billing tables to the v2 schema' },
  { role: 'assistant', text: 'Reading the current schema first.' },
];

function mkchat(env: NodeJS.ProcessEnv, input = '') {
  execFileSync('go', ['run', './e2e/mkchat'], { cwd: repoRoot, input, env: { ...process.env, DATABASE_URL: dsn, MUSTER_E2E_AGENT: agentName, ...env } });
}

const clearSwitch = () =>
  execFileSync('go', ['run', './e2e/pluginfix', 'notifypref', '1', 'clear'], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } });

test.beforeAll(async () => {
  clearSwitch();
  await rig.start();
  mkchat({}, JSON.stringify([{ role: 'user', content: 'Hold the fixtures.' }]));
  const rows: string[] = [];
  for (const k of ['phone', 'desktop'] as const) {
    const w = windows[k];
    const tr = path.join(rig.dir, sids[k] + '.jsonl');
    fs.writeFileSync(tr, cc.prompt('Migrate the billing tables') + cc.reply('Reading the schema.'));
    fs.writeFileSync(tr + '.waiting', '');
    rows.push(`tnotify|@${w}|${w}|${w === 1 ? 1 : 0}|claude|${now - 60 * w}|${tr}`);
  }
  // Five more windows with NO human name (called by their codename): the bar
  // holds 7 pills, and an unnamed one must never draw its codename twice.
  for (const w of unnamed) {
    const tr = path.join(rig.dir, `ntmux-plain-${w}-${stamp}.jsonl`);
    fs.writeFileSync(tr, cc.prompt('hi') + cc.reply('Hello! How can I help?'));
    rows.push(`tnotify|@${w}|${w}|0|claude|${now - 60 * w}|${tr}`);
  }
  writeFileAtomic(windowsFile, rows.join('\n') + '\n');
  mkchat({
    MUSTER_E2E_FOCUS_ONLY: '1',
    MUSTER_E2E_FOCUS: JSON.stringify((['phone', 'desktop'] as const).map((k) => ({ subject: 'mirror:' + sids[k], title: names[k], tail: TAIL }))),
  });
  // The host labels the tmux session with the SYNTHETIC codename "cobalt":
  // its windows are "cobalt:1", "cobalt:2" — what the switch adds.
  fs.writeFileSync(labelsFile, JSON.stringify({ tnotify: 'cobalt' }));
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile, MUSTER_MIRROR_LABELS_FILE: labelsFile, MUSTER_MIRROR_INTERACTIVE: '1' });
});

test.afterAll(async () => {
  clearSwitch();
  await rig.stop();
  mkchat({ MUSTER_E2E_DELETE: '1', MUSTER_E2E_FOCUS: JSON.stringify(Object.values(sids).map((s) => ({ subject: 'mirror:' + s, title: 'x' }))) });
});

// askRow is the Asks list's name of the waiting window w: its card's title
// (<span data-attention-name>), in the card whose place under it names w.
const askRow = (page: Page, w: number) =>
  page
    .locator('[data-attention-item][data-kind="session"]', { has: page.locator('[data-attention-source]', { hasText: `tnotify:${w} @` }) })
    .locator('[data-attention-name]');

async function setSwitch(page: Page, on: boolean) {
  await page.goto(origin + '/account');
  const sw = page.locator('[data-testid="notify-tmux"]');
  await expect(sw).toHaveAttribute('data-state', on ? 'off' : 'on');
  const button = page.locator('#notify-tmux-toggle');
  expect((await button.boundingBox())!.height, 'a 44px target').toBeGreaterThanOrEqual(44);
  await Promise.all([page.waitForURL(/\/account\?notify=saved#notify-tmux$/), button.click()]);
  await expect(sw).toHaveAttribute('data-state', on ? 'on' : 'off');
  await page.reload();
  await expect(sw, 'the choice survives a reload').toHaveAttribute('data-state', on ? 'on' : 'off');
}

for (const vp of [
  { key: 'phone' as const, name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { key: 'desktop' as const, name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });
    const w = windows[vp.key];
    const name = names[vp.key];
    const unnamedWaiting = unnamedWaitingOf[vp.key];

    // THE SESSION LISTS: the bar's pills and the dock's rows. The page is
    // window 7's (unnamed); the bar shows its All view, 7 pills.
    test('the bar\'s pills and the dock\'s rows gain the codename line only with the switch on, and keep it on a reload and a live refresh', async ({ page }) => {
      test.setTimeout(120_000);
      await signIn(page, origin);
      await rig.waitListed(page, `cobalt:${w}`);
      const bar = page.locator('#session-switcher');
      const allRow = bar.locator('[data-switcher-list="all"]');
      const named = allRow.locator(`a[data-switcher-entry][href*="window=%40${w}"]`);
      const plain = allRow.locator(`a[data-switcher-entry][href*="window=%403"]`);
      const open = async () => {
        await page.goto(origin + `/mirror/view?host=${host}&session=tnotify&window=%407`);
        await page.evaluate(() => (window as any).musterSwitcherShow('all'));
        await expect(allRow).toBeVisible();
        await expect(named).toHaveAttribute('data-switcher-name', name);
      };
      const noOverflow = async () => {
        const o = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth }));
        expect(o.sw, 'the page does not scroll sideways').toBeLessThanOrEqual(o.cw);
        const b = (await bar.boundingBox())!;
        expect(b.x + b.width, 'the bar fits the viewport').toBeLessThanOrEqual(vp.use.viewport.width + 0.5);
      };
      const dockRow = async (href: string) => {
        if (vp.use.hasTouch) await page.locator('#switcher-toggle').tap();
        else await page.evaluate(() => (window as any).musterDock.open(document.getElementById('session-switcher'), null));
        await expect.poll(() => page.evaluate(() => (document.getElementById('switcher-dock') as HTMLDialogElement).open)).toBe(true);
        await page.locator('#switcher-dock [data-dock-tab="all"]').click();
        return page.locator(`#dock-list a[data-dock-row][href="${href}"]`);
      };
      const closeDock = () => page.keyboard.press('Escape');

      // OFF: every pill one line, no codename anywhere; the dock's row two lines.
      await open();
      expect(await allRow.locator('a[data-switcher-entry]').count(), '7 pills').toBeGreaterThanOrEqual(7);
      await expect(bar.locator('[data-switcher-tmux], [data-switcher-tmux-line]')).toHaveCount(0);
      const offHeight = (await bar.boundingBox())!.height;
      await noOverflow();
      const href = (await named.getAttribute('href'))!;
      let row = await dockRow(href);
      await expect(row.locator('[data-dock-label]')).toHaveText(name);
      await expect(row.locator('[data-dock-focus]')).toHaveCount(1);
      await expect(row.locator('[data-dock-tmux]')).toHaveCount(0);
      await expect(page.locator('#switcher-dock')).not.toContainText(`cobalt:${w} @`);
      await closeDock();

      // ON: the named pill's codename line, inside the pill; the dock's third line.
      await setSwitch(page, true);
      await open();
      const line = named.locator('[data-switcher-tmux-line]');
      await expect(line).toHaveText(`cobalt:${w}`);
      await expect(line).toBeVisible();
      await expect(named).toHaveAttribute('data-switcher-tmux', `cobalt:${w} @ ${host}`);
      await expect(named, 'the codename is part of the pill\'s accessible name').toHaveAccessibleName(new RegExp(`${name} cobalt:${w}$`));
      const lb = (await line.boundingBox())!, pb = (await named.locator(':scope > span').boundingBox())!;
      expect(lb.y >= pb.y - 0.5 && lb.y + lb.height <= pb.y + pb.height + 0.5, 'the line is inside the pill').toBe(true);
      await expect(plain.locator('[data-switcher-tmux-line]'), 'an unnamed pill draws no codename twice').toHaveCount(0);
      await expect(plain).not.toHaveAttribute('data-switcher-tmux', /.*/);
      // Exactly the two NAMED windows (this viewport's and the other's) draw a
      // codename line: no unnamed window, and no agent — an agent has no tmux
      // window. The agent pill is there (the control), and draws none.
      await expect(allRow.locator('[data-switcher-tmux-line]'), 'only the named windows').toHaveCount(2);
      await expect(allRow.locator('[data-switcher-entry][data-switcher-tmux]')).toHaveCount(2);
      const agents = allRow.locator('[data-switcher-entry="agent"]');
      expect(await agents.count(), 'control: an agent pill is on the bar').toBeGreaterThanOrEqual(1);
      await expect(agents.locator('[data-switcher-tmux-line]'), 'an agent draws no codename').toHaveCount(0);
      await expect(allRow.locator('[data-switcher-entry="agent"][data-switcher-tmux]')).toHaveCount(0);
      const onHeight = (await bar.boundingBox())!.height;
      expect(Math.abs(onHeight - offHeight), 'the bar grows at most 4px').toBeLessThanOrEqual(4);
      await noOverflow();
      row = await dockRow(href);
      await expect(row.locator('[data-dock-tmux]')).toHaveText(`cobalt:${w} @ ${host}`);
      await expect(row, 'the codename is part of the row\'s accessible name').toHaveAccessibleName(new RegExp(`cobalt:${w} @ ${host}`));
      const fb = (await row.locator('[data-dock-focus]').boundingBox())!, tb = (await row.locator('[data-dock-tmux]').boundingBox())!;
      expect(tb.y, 'the third line, under the focus').toBeGreaterThanOrEqual(fb.y + fb.height - 0.5);
      const plainRow = page.locator(`#dock-list a[data-dock-row][href="${(await plain.getAttribute('href'))!}"]`);
      await expect(plainRow.locator('[data-dock-tmux]')).toHaveCount(0);
      // The dock's rows: the codename line on the two named windows only (no
      // unnamed window's, no agent's), and an agent row is there to have none.
      await expect(page.locator('#dock-list [data-dock-tmux]'), 'only the named windows\' dock rows').toHaveCount(2);
      const agentRows = page.locator('#dock-list a[data-dock-row][href^="/agents/"]');
      expect(await agentRows.count(), 'control: an agent row is in the dock').toBeGreaterThanOrEqual(1);
      await expect(agentRows.locator('[data-dock-tmux]'), 'an agent row draws no codename').toHaveCount(0);
      await closeDock();

      // A reload keeps it; so does a live refresh of the bar (the ⋯ menu's
      // pin toggle answers with the rows re-rendered out of band).
      await page.reload();
      await page.evaluate(() => (window as any).musterSwitcherShow('all'));
      await expect(named.locator('[data-switcher-tmux-line]')).toHaveText(`cobalt:${w}`);
      const more = page.locator('#switcher-more-btn');
      const pin = page.locator('#switcher-pin');
      await (vp.use.hasTouch ? more.tap() : more.click());
      await expect(pin).toBeVisible();
      await page.evaluate(() => { (document.querySelector('#switcher-list-all a[data-switcher-entry]') as HTMLElement).dataset.staleMark = '1'; });
      await (vp.use.hasTouch ? pin.tap() : pin.click());
      await expect(page.locator('#switcher-pin')).toHaveAttribute('data-pinned', 'true');
      await expect(allRow.locator('[data-stale-mark]'), 'the row was re-rendered').toHaveCount(0);
      await expect(named.locator('[data-switcher-tmux-line]'), 'kept by the live refresh').toHaveText(`cobalt:${w}`);
      await (vp.use.hasTouch ? more.tap() : more.click());
      await (vp.use.hasTouch ? pin.tap() : pin.click());
      await expect(page.locator('#switcher-pin')).toHaveAttribute('data-pinned', 'false');

      // OFF again: gone.
      await setSwitch(page, false);
      await open();
      await expect(bar.locator('[data-switcher-tmux], [data-switcher-tmux-line]')).toHaveCount(0);
      expect(Math.abs((await bar.boundingBox())!.height - offHeight)).toBeLessThanOrEqual(0.5);
    });

    // THE SIDEBAR'S ROWS AND THE SEARCH PICKER (the command palette). The
    // sidebar is read on /tasks (the shell: its /events stream wraps the
    // sidebar, so the rows are re-fetched live on an SSE event); the palette is
    // opened from the bottom bar's magnifier on window 7's view (its results
    // are built from the bar's pills) and from the sidebar's on /tasks (built
    // from the rows).
    test('the sidebar\'s rows and the search picker gain the place line only with the switch on, and keep it across an SSE refresh of the sidebar', async ({ page }) => {
      test.setTimeout(150_000);
      await signIn(page, origin);
      await rig.waitListed(page, `cobalt:${w}`);
      const place = `cobalt:${w} @ ${host}`;
      const phone = vp.use.viewport.width < 1024;
      const list = page.locator('#sidebar-agents-list');
      const sideRow = (n: number) => list.locator(`a[data-nav-agent="mirror"][href*="window=%40${n}"]`);
      const showSidebar = async () => {
        if (phone || !(await list.isVisible())) await page.locator('#sidebar-open').click();
        // The drawer slides in: measured only once it is all on screen.
        await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });
        await expect(sideRow(w)).toBeVisible({ timeout: 20_000 });
        await expect(sideRow(w)).toHaveAttribute('data-switcher-name', name);
      };
      const opts = page.locator('#switcher-search-results [role="option"]');
      const opt = (n: number) => page.locator(`#switcher-search-results [role="option"][data-href*="window=%40${n}"]`);
      const view7 = async () => {
        await page.goto(origin + `/mirror/view?host=${host}&session=tnotify&window=%407`);
        await expect(page.locator('#session-switcher [data-switcher-list="all"] a[data-switcher-entry]').first()).toBeAttached();
      };
      const openFromBar = async () => {
        const b = page.locator('#switcher-search-open');
        await (vp.use.hasTouch ? b.tap() : b.click());
        await expect(page.locator('#switcher-search')).toBeVisible();
        await expect(opt(w)).toBeVisible();
      };
      const noOverflow = async () => {
        const o = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth }));
        expect(o.sw, 'the page does not scroll sideways').toBeLessThanOrEqual(o.cw);
      };

      // OFF: no place anywhere, in the rows or the picker.
      await page.goto(origin + '/tasks');
      await showSidebar();
      await expect(list.locator('[data-nav-tmux], [data-switcher-tmux]')).toHaveCount(0);
      await view7();
      await openFromBar();
      expect(await opts.count(), 'the picker lists the other sessions').toBeGreaterThanOrEqual(6);
      await expect(page.locator('#switcher-search-results [data-palette-tmux]')).toHaveCount(0);
      await expect(opt(w)).not.toContainText(`cobalt:${w} @`);
      // A name with no focus title: the line under it is the bare codename
      // (main's behaviour; ON replaces it with the place, below).
      const bareCode = opt(w).locator('span', { hasText: new RegExp(`^cobalt:${w}$`) });
      await expect(bareCode, 'control: OFF draws the bare codename under the name').toHaveCount(1);
      // The OFF picker's whole list, for the merge-base differential (run with
      // MUSTER_E2E_PALETTE_DUMP=<file> at main and at this branch, then diff).
      if (process.env.MUSTER_E2E_PALETTE_DUMP) {
        const h = await page.locator('#switcher-search-results').innerHTML();
        fs.writeFileSync(process.env.MUSTER_E2E_PALETTE_DUMP + '.' + vp.key, h.split(host).join('<HOST>').split(stamp).join('<STAMP>'));
      }
      await page.keyboard.press('Escape');

      // ON.
      await setSwitch(page, true);
      // The sidebar: the line under the named row's name, only on the two
      // named windows (this viewport's and the other's) — no unnamed window
      // (called by its codename already), no agent (no tmux window).
      await page.goto(origin + '/tasks');
      await showSidebar();
      const line = sideRow(w).locator('[data-nav-tmux]');
      await expect(line).toHaveText(place);
      await expect(line).toBeVisible();
      await expect(sideRow(w)).toHaveAttribute('data-switcher-tmux', place);
      await expect(sideRow(w), 'the place is part of the row\'s accessible name').toHaveAccessibleName(new RegExp(place));
      const nb = (await sideRow(w).locator('[data-nav-name]').boundingBox())!, lb = (await line.boundingBox())!;
      expect(lb.y, 'the line is under the name').toBeGreaterThanOrEqual(nb.y + nb.height - 0.5);
      await expect(list.locator('[data-nav-tmux]'), 'only the named windows').toHaveCount(2);
      await expect(list.locator('a[data-switcher-tmux]')).toHaveCount(2);
      await expect(sideRow(3).locator('[data-nav-tmux]'), 'an unnamed row draws no place').toHaveCount(0);
      const agentRows = list.locator('a[data-nav-agent="agent"]');
      expect(await agentRows.count(), 'control: an agent row is in the sidebar').toBeGreaterThanOrEqual(1);
      await expect(list.locator('a[data-nav-agent="agent"] [data-nav-tmux], a[data-nav-agent="agent"][data-switcher-tmux]'), 'an agent row draws no place').toHaveCount(0);
      await noOverflow();

      // A LIVE refresh of the sidebar keeps it: another window starts
      // waiting, the server's attention_queue.changed event re-fetches the
      // rows over SSE (the marked row is replaced: the refresh happened).
      // Window 6: not one the other tests set waiting (4, 5) or open (7).
      const extra = path.join(rig.dir, `ntmux-plain-${sidebarWaiting}-${stamp}.jsonl.waiting`);
      await sideRow(w).evaluate((el) => el.setAttribute('data-stale-mark', '1'));
      try {
        fs.writeFileSync(extra, '');
        await expect(list.locator('[data-stale-mark]'), 'the rows were re-fetched live').toHaveCount(0, { timeout: 20_000 });
        await expect(sideRow(w).locator('[data-nav-tmux]'), 'kept by the live refresh').toHaveText(place);
        // The picker opened from the SIDEBAR here (no bar on this page) is
        // built from those refreshed rows.
        await page.locator('#sidebar-search').click();
        await expect(opt(w).locator('[data-palette-tmux]')).toHaveText(place);
        await expect(page.locator('#switcher-search-results [data-palette-tmux]')).toHaveCount(2);
        await page.keyboard.press('Escape');
      } finally {
        fs.rmSync(extra, { force: true });
      }
      await expect(sideRow(sidebarWaiting), 'and it stops waiting again').not.toHaveAttribute('data-nav-wait', /.*/, { timeout: 30_000 });

      // The picker opened from the BOTTOM BAR (built from the pills).
      await view7();
      await openFromBar();
      const pl = opt(w).locator('[data-palette-tmux]');
      await expect(pl).toHaveText(place);
      await expect(pl).toBeVisible();
      await expect(opt(w), 'the place is part of the option\'s accessible name').toHaveAccessibleName(new RegExp(place));
      // The codename is not drawn twice: the bare codename line OFF drew is
      // gone, the place (which starts with it) in its stead.
      await expect(bareCode, 'no bare codename next to the place').toHaveCount(0);
      // Under the name.
      const ob = (await opt(w).locator('span').first().boundingBox())!, pb = (await pl.boundingBox())!;
      expect(pb.y, 'the line is under the name').toBeGreaterThanOrEqual(ob.y + ob.height - 0.5);
      await expect(page.locator('#switcher-search-results [data-palette-tmux]'), 'only the named windows').toHaveCount(2);
      await expect(opt(3).locator('[data-palette-tmux]'), 'an unnamed option draws no place').toHaveCount(0);
      const agentOpts = page.locator('#switcher-search-results [role="option"][data-kind="agent"]');
      expect(await agentOpts.count(), 'control: an agent is in the picker').toBeGreaterThanOrEqual(1);
      await expect(agentOpts.locator('[data-palette-tmux]'), 'an agent draws no place').toHaveCount(0);
      // Search is unchanged: the codename finds the window by its code, as
      // before, and the result still draws the place.
      await page.locator('#switcher-search-input').fill(`cobalt:${w}`);
      await expect(opt(w)).toHaveAttribute('data-match', 'code');
      await expect(opt(w).locator('[data-palette-tmux]')).toHaveText(place);
      await page.keyboard.press('Escape');

      // LONG TITLES: the name and the place cut with an ellipsis inside the
      // row / the option, never widening the sidebar, the picker or the page.
      // (Lengthened in the page: the fixture's names are short.)
      const long = 'Reconcile the quarterly billing ledger migration across every regional shard ';
      // The picker is opened first, then the pill lengthened and the results
      // redrawn (the input's own handler) in ONE synchronous evaluate, so a
      // live re-render of the bar cannot land in between.
      await openFromBar();
      await page.locator('#session-switcher [data-switcher-list="all"] a[data-switcher-entry][href*="window=%40' + w + '"]').evaluate((a, s) => {
        a.setAttribute('data-switcher-name', s.repeat(3));
        a.setAttribute('data-switcher-tmux', s.repeat(3) + ' @ <b>x</b>');
        document.getElementById('switcher-search-input')!.dispatchEvent(new Event('input'));
      }, long);
      await expect(opt(w).locator('span').first(), 'control: the option was redrawn long').toHaveText(long.repeat(3).trim());
      // Read once the picker has settled (no running animation), both boxes in
      // one frame: two separate reads straddled its opening on a CI phone run.
      const fitP = await opt(w).evaluate(async (li) => {
        const pal = document.getElementById('switcher-search')!;
        await Promise.all(pal.getAnimations({ subtree: true }).map((a) => a.finished.catch(() => null)));
        const b = pal.getBoundingClientRect(), r = li.getBoundingClientRect();
        return { opt: r.right, pal: b.right };
      });
      expect(fitP.opt, 'the option fits the picker').toBeLessThanOrEqual(fitP.pal + 0.5);
      for (const sel of ['span >> nth=0', '[data-palette-tmux]']) {
        const cut = await opt(w).locator(sel).evaluate((el) => ({ sw: el.scrollWidth, cw: el.clientWidth, ov: getComputedStyle(el).textOverflow }));
        expect(cut.sw, `${sel} is cut, not wrapped wide`).toBeGreaterThan(cut.cw);
        expect(cut.ov).toBe('ellipsis');
      }
      await expect(opt(w).locator('b'), 'the place is drawn as text').toHaveCount(0);
      await expect(opt(w).locator('[data-palette-tmux]')).toContainText('@ <b>x</b>');
      await noOverflow();
      await page.keyboard.press('Escape');
      await page.goto(origin + '/tasks');
      await showSidebar();
      // Lengthened AND measured in ONE synchronous evaluate: the rail's rows
      // are re-fetched live (an SSE event, the minute's tick), and a swap
      // landing between a separate write and read measured the real short
      // name (CI run 42565032895, desktop). Layout is forced by the reads.
      // 🔴 The row is FOUND in that evaluate too, not handed in by a locator:
      // a locator's evaluate resolves the element in one round trip and runs
      // the function in a second, so a swap between them hands the function
      // the replaced row — detached, every box 0 — and the row "fits" (0 ≤
      // the sidebar) while the name "is cut" read 0 > 0 (a desktop CI run).
      const m = await page.evaluate(({ sel, s }) => {
        const rows = document.querySelectorAll(sel);
        if (rows.length !== 1) throw new Error(`${rows.length} sidebar rows match ${sel}, want 1`);
        const a = rows[0] as HTMLElement;
        const name = a.querySelector('[data-nav-name]') as HTMLElement, tmux = a.querySelector('[data-nav-tmux]') as HTMLElement;
        name.textContent = s.repeat(3);
        tmux.textContent = s.repeat(3);
        const side = document.getElementById('sidebar')!.getBoundingClientRect(), r = a.getBoundingClientRect();
        const cut = (el: HTMLElement) => ({ sw: el.scrollWidth, cw: el.clientWidth, ov: getComputedStyle(el).textOverflow });
        return { row: r.right, side: side.right, name: cut(name), tmux: cut(tmux), sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth };
      }, { sel: `#sidebar-agents-list a[data-nav-agent="mirror"][href*="window=%40${w}"]`, s: long });
      expect(m.row, 'the row fits the sidebar').toBeLessThanOrEqual(m.side + 0.5);
      for (const [sel, cut] of [['[data-nav-name]', m.name], ['[data-nav-tmux]', m.tmux]] as const) {
        expect(cut.sw, `${sel} is cut`).toBeGreaterThan(cut.cw);
        expect(cut.ov).toBe('ellipsis');
      }
      expect(m.sw, 'the page does not scroll sideways').toBeLessThanOrEqual(m.cw);
      await noOverflow();

      // OFF again: gone, rows and picker.
      await setSwitch(page, false);
      await page.goto(origin + '/tasks');
      await showSidebar();
      await expect(list.locator('[data-nav-tmux], [data-switcher-tmux]')).toHaveCount(0);
      await view7();
      await openFromBar();
      await expect(page.locator('#switcher-search-results [data-palette-tmux]')).toHaveCount(0);
    });

    test('the switch is off by default, turns on and persists, and the Asks row gains and loses the tmux address', async ({ page }) => {
      await signIn(page, origin);
      await rig.waitListed(page, `cobalt:${w}`);

      // 1. Off by default: exactly the name.
      await page.goto(origin + '/account');
      await expect(page.locator('[data-testid="notify-tmux"]')).toHaveAttribute('data-state', 'off');
      await expect(page.locator('[data-testid="notify-tmux"]')).toContainText('Show tmux sessions');
      await page.goto(origin + '/attention');
      await expect(askRow(page, w)).toHaveText(name);

      // 2. On: persists, and the row names where the session is.
      await setSwitch(page, true);
      await page.goto(origin + '/attention');
      await expect(askRow(page, w)).toHaveText(`${name} · cobalt:${w} @ ${host}`);

      // A LIVE refresh keeps it: another window starts waiting, the server's
      // attention_queue.changed event re-fetches the list over SSE (the
      // marked row is replaced: the refresh happened), and the named row is
      // still "<name> · <codename> @ <host>" — the refresh renders as its owner.
      const extra = path.join(rig.dir, `ntmux-plain-${unnamedWaiting}-${stamp}.jsonl.waiting`);
      const extraRow = page.locator('[data-attention-item][data-kind="session"]', { hasText: `tnotify:${unnamedWaiting} @` });
      await expect(extraRow).toHaveCount(0);
      await askRow(page, w).evaluate((el) => { el.setAttribute('data-stale-mark', '1'); });
      try {
        fs.writeFileSync(extra, '');
        await expect(extraRow.first(), 'the list refreshed live with the new waiting window').toBeVisible({ timeout: 20_000 });
        await expect(page.locator('#attention-items [data-stale-mark]'), 'the row was re-rendered').toHaveCount(0);
        await expect(askRow(page, w), 'kept by the live refresh').toHaveText(`${name} · cobalt:${w} @ ${host}`);
      } finally {
        fs.rmSync(extra, { force: true });
      }
      await expect(extraRow, 'and gone again once it stops waiting').toHaveCount(0, { timeout: 20_000 });

      // 3. Off again: exactly the name again.
      await setSwitch(page, false);
      await page.goto(origin + '/attention');
      await expect(askRow(page, w)).toHaveText(name);
    });

    // "START NOTIFICATION TITLES WITH THE TMUX NAME" (the second switch,
    // migration 0117): its own state, saved and read back, independent of
    // "Show tmux sessions" — and, since it changes PUSH titles only, the Asks
    // row is exactly the name with it on. (The push titles themselves are
    // pinned in Go: internal/api/notify_codename*_test.go.)
    // 🔴 REGRESSION TEST: on origin/main the Account page has no such switch
    // and POST /account/notifications/codename does not exist.
    test('the tmux-name-first switch is off by default, saves and survives a reload on its own, and leaves the Asks row as it was', async ({ page }) => {
      await signIn(page, origin);
      await rig.waitListed(page, `cobalt:${w}`);
      const sw = page.locator('[data-testid="notify-codename"]');
      const tmux = page.locator('[data-testid="notify-tmux"]');
      const button = page.locator('#notify-codename-toggle');
      const flip = async (on: boolean) => {
        await page.goto(origin + '/account');
        await expect(sw).toHaveAttribute('data-state', on ? 'off' : 'on');
        await Promise.all([page.waitForURL(/\/account\?notify=codename-saved#notify-codename$/), button.click()]);
        await expect(sw).toHaveAttribute('data-state', on ? 'on' : 'off');
        await expect(sw.getByRole('status'), 'the save is confirmed at this switch').toHaveText('Saved.');
        await expect(tmux.getByRole('status'), 'and not at the other').toHaveCount(0);
        await page.reload();
        await expect(sw, 'the choice survives a reload').toHaveAttribute('data-state', on ? 'on' : 'off');
        await expect(tmux, '"Show tmux sessions" is untouched').toHaveAttribute('data-state', 'off');
      };
      try {
        await page.goto(origin + '/account');
        await expect(sw).toHaveAttribute('data-state', 'off');
        await expect(sw).toContainText('Start notification titles with the tmux name: Off.');
        await expect(sw).toContainText('like mango:1@laptop');
        await expect(button).toHaveText('Turn on: Start notification titles with the tmux name');
        expect((await button.boundingBox())!.height, 'a 44px target').toBeGreaterThanOrEqual(44);
        const o = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth }));
        expect(o.sw, 'the Account page does not scroll sideways').toBeLessThanOrEqual(o.cw);

        await flip(true);
        await expect(button).toHaveText('Turn off: Start notification titles with the tmux name');
        await page.goto(origin + '/attention');
        await expect(askRow(page, w), 'a list is not a push title: exactly the name').toHaveText(name);

        await flip(false);
      } finally {
        clearSwitch();
      }
    });
  });
}
