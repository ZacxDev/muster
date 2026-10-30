import fs from 'node:fs';
import path from 'node:path';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { workCardsPort } from '../lib/env';
import { MirrorRig, cc } from '../lib/mirrorhost';
import { clsInit, readCls, resetCls, settle } from '../lib/cls';
import { writeFileAtomic } from '../lib/atomic';

// LIVE WORK COUNTS AND THE TMUX IDENTITY COLOUR, ON THE SIDEBAR'S ROWS
// (docs/design/agent-cards.md (a) and (b); the Agents list's cards carried them
// until the list page was retired — internal/ui/nav_agents.go navWork and the
// row's identity edge now), every hop real: a real muster-mirror (-tags
// e2emirror) whose fake windows carry what the real host reads — a Claude Code
// window's RAW session status, its process-tree shells and a real transcript
// the host's ledger reads for running Agent calls, and each window's
// @muster-color value — over the real tunnel to a real server.
//
//   - THE COUNT: a busy window with two running Agent calls and a shell says
//     "2 subagents · 1+ shell" (a turn may be running, so the shell count is a
//     lower bound) — its name and tooltip the whole sentence, its face one
//     count; a window whose CLI says "shell" with three says "3 shells"; it
//     goes away LIVE when the CLI goes idle.
//   - THE TINT: the row's left edge is the window's colour, the server's own
//     per-theme value, in BOTH themes; a row with no colour keeps a plain
//     (transparent) edge of the same width; a shell window is tinted too.
//   - AN OLDER HOST (MUSTER_MIRROR_E2E_NO_WORK: it sends neither message) whose
//     windows carry the same status and colour renders exactly as before: no
//     count, no tint.
//   - Phone (390×844, the drawer open) and desktop (1280×800, the rail), with
//     the layout shift of the list carrying counts and tints under the cls
//     spec's bar.

const BAR = 0.01;
const viewports = [
  { name: 'phone 390×844', viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true },
  { name: 'desktop 1280×800', viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false },
];

const rig = new MirrorRig(workCardsPort, 'e2e-work');
const oldHost = 'e2e-work-old-' + Date.now().toString(36);
let stopOld: (() => Promise<void>) | undefined;
let trBusy = '';
let trShell = '';
let trOld = '';

// status writes a fake window's <transcript>.status: its RAW Claude Code
// status and what the real host reads from /proc.
function status(tr: string, s: string, shells: number) {
  writeFileAtomic(tr + '.status', JSON.stringify({ status: s, statusUpdatedAt: Date.now() - 5_000, startedAt: Date.now() - 3_600_000, shells }));
}

// An Agent call at a time the ledger counts (after the CLI started).
const agentCall = (id: string) =>
  JSON.stringify({
    type: 'assistant',
    timestamp: new Date(Date.now() - 60_000).toISOString(),
    message: { content: [{ type: 'tool_use', id, name: 'Agent', input: { description: 'fixture job', prompt: 'p', subagent_type: 'general-purpose' } }] },
  }) + '\n';

test.describe.configure({ mode: 'serial' });

test.beforeAll(async () => {
  test.setTimeout(240_000);
  await rig.start();
  trBusy = rig.transcript(cc.prompt('fan out') + agentCall('toolu_e2e_w1') + agentCall('toolu_e2e_w2'), 'busy');
  trShell = rig.transcript(cc.prompt('run the watcher') + cc.reply('Started.'), 'shell');
  trOld = rig.transcript(cc.prompt('older host') + agentCall('toolu_e2e_old'), 'old');
  status(trBusy, 'busy', 1);
  status(trShell, 'shell', 3);
  status(trOld, 'busy', 2);
  const now = Math.floor(Date.now() / 1000);
  const windows = path.join(rig.dir, 'windows');
  writeFileAtomic(
    windows,
    [
      `fleet|@5|5|1|claude|${now - 60}|${trBusy}|||#d79921`,
      `fleet|@6|6|0|claude|${now - 90}|${trShell}|||#458588`,
      `notes|@7|0|1|zsh|${now - 120}||||#b16286`,
      `plain|@8|0|1|claude|${now - 150}|${trShell}`,
    ].join('\n') + '\n',
  );
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
  const oldWindows = path.join(rig.dir, 'windows-old');
  writeFileAtomic(oldWindows, `relic|@9|0|1|claude|${now - 30}|${trOld}|||#d79921\n`);
  stopOld = rig.extraHost(oldHost, { MUSTER_MIRROR_E2E_WINDOWS_FILE: oldWindows, MUSTER_MIRROR_E2E_REFRESH: '300ms', MUSTER_MIRROR_E2E_NO_WORK: '1' });
});

test.afterAll(async () => {
  await stopOld?.();
  await rig.stop();
});

// row is a mirrored window's sidebar row, by its label.
const row = (page: Page, title: string) => page.locator(`#sidebar-agents-list a[data-nav-agent="mirror"][title="${title}"]`);
const work = (page: Page, title: string) => row(page, title).locator('[data-nav-work]');

// variant is a style attribute's custom property ("R G B") as a computed rgb().
async function variant(page: Page, title: string, prop: string): Promise<string> {
  const style = (await row(page, title).getAttribute('style')) || '';
  const m = new RegExp(`${prop}:(\\d+) (\\d+) (\\d+)`).exec(style);
  expect(m, `${title}'s style ${style} names ${prop}`).toBeTruthy();
  return `rgb(${m![1]}, ${m![2]}, ${m![3]})`;
}

const edgeOf = (page: Page, title: string) => row(page, title).evaluate((el) => getComputedStyle(el).borderLeftColor);

// openList signs in, opens a page with the sidebar (the account page: its own
// content never changes live, so what moves is the sidebar's) and shows its
// list — the rail on a desktop, the drawer opened on a phone (by script when
// asked, so what it moves is counted by a layout-shift record) — once this
// host's and the older host's rows are drawn.
async function openList(page: Page, scripted = false, at = '/account') {
  await rig.login(page);
  await page.goto(rig.origin + at);
  if ((page.viewportSize()?.width ?? 0) < 1024) {
    if (scripted) await page.evaluate(() => (document.getElementById('sidebar-open') as HTMLElement).click());
    else await page.locator('#sidebar-open').click();
    await expect(page.locator('#sidebar-root')).toHaveAttribute('data-open', 'true');
    // Slid all the way in (its transform settled), so a box read is final.
    await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });
  }
  await expect(row(page, 'fleet:5'), rig.log).toHaveCount(1, { timeout: 30_000 });
  await expect(row(page, 'relic:0'), rig.log).toHaveCount(1, { timeout: 30_000 });
}

for (const vp of viewports) {
  test.describe(vp.name, () => {
    test.use({ viewport: vp.viewport, hasTouch: vp.hasTouch, isMobile: vp.isMobile, storageState: { cookies: [], origins: [] } });

    test('each row reads what its window has running, and an older host shows nothing', async ({ page }) => {
      test.setTimeout(90_000);
      await openList(page);
      await expect(work(page, 'fleet:5'), rig.log).toHaveAttribute('data-nav-work', '2 subagents · 1+ shell', { timeout: 20_000 });
      await expect(work(page, 'fleet:5')).toHaveAttribute('aria-label', '2 subagents and at least 1 background shell running');
      await expect(work(page, 'fleet:5').locator('.sr-only')).toHaveText('2 subagents and at least 1 background shell running');
      // Compact: one count — 2 subagents and at least 1 shell — beside the icon.
      await expect(work(page, 'fleet:5').locator('[aria-hidden="true"]').last()).toHaveText('3+');
      await expect(work(page, 'fleet:6')).toHaveAttribute('data-nav-work', '3 shells');
      // A shell window: nothing.
      await expect(work(page, 'notes:0')).toHaveCount(0);
      // The older host: the same busy status, shells and transcript — nothing.
      await expect(work(page, 'relic:0')).toHaveCount(0);
      // The count sits inside its row (no overflow), and nothing scrolls sideways.
      const box = (await row(page, 'fleet:5').boundingBox())!;
      const chip = (await work(page, 'fleet:5').boundingBox())!;
      expect(chip.x >= box.x && chip.x + chip.width <= box.x + box.width + 0.5, `the count ${JSON.stringify(chip)} is inside the row ${JSON.stringify(box)}`).toBe(true);
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    });

    test("the row's edge wears the window colour in both themes; untinted and older-host rows keep a plain edge", async ({ page }) => {
      test.setTimeout(90_000);
      await openList(page);
      await expect(row(page, 'fleet:5')).toHaveAttribute('data-identity', '#d79921', { timeout: 20_000 });
      for (const theme of ['dark', 'light'] as const) {
        if (theme === 'light') {
          // The light theme is unreachable in the product (the toggle is gone and
          // the head script forces dark), but its tokens are still shipped: check
          // the tint under them by dropping the class directly.
          await page.evaluate(() => document.documentElement.classList.remove('dark'));
        }
        const sfx = theme === 'dark' ? 'd' : 'l';
        for (const title of ['fleet:5', 'fleet:6', 'notes:0']) {
          // Polled: the row's colours are transitioned (its hover), so a theme
          // switch reaches the computed value over a few frames.
          const want = await variant(page, title, `--id-bd-${sfx}`);
          await expect.poll(() => edgeOf(page, title), { message: `${title} edge (${theme})` }).toBe(want);
        }
        // No colour, or an older host's: the transparent edge of the same width.
        expect(await edgeOf(page, 'plain:0'), `the untinted row (${theme})`).toBe('rgba(0, 0, 0, 0)');
        expect(await edgeOf(page, 'relic:0'), `the older host's row (${theme})`).toBe('rgba(0, 0, 0, 0)');
        expect(await row(page, 'relic:0').getAttribute('data-identity')).toBeNull();
        expect(await row(page, 'plain:0').getAttribute('style')).toBeNull();
      }
      // The terminal colour itself is the dark edge (it already meets 3:1
      // there); the light variant had to move.
      await page.reload();
      if ((page.viewportSize()?.width ?? 0) < 1024) await page.locator('#sidebar-open').click();
      await expect(row(page, 'fleet:5')).toHaveCount(1, { timeout: 20_000 });
      expect(await edgeOf(page, 'fleet:5')).toBe('rgb(215, 153, 33)');
      expect(await variant(page, 'fleet:5', '--id-bd-l')).not.toBe('rgb(215, 153, 33)');
      // The rendered view's header title is not tinted.
      await page.goto(rig.origin + `/mirror/view?host=${encodeURIComponent(rig.host)}&session=fleet&window=${encodeURIComponent('@5')}`);
      await expect(page.locator('header [data-mirror-name]')).toHaveCount(1, { timeout: 15_000 });
      await expect(page.locator('header [data-identity]')).toHaveCount(0);
    });

    test(`the list with counts and tints shifts less than ${BAR}`, async ({ page }) => {
      test.setTimeout(60_000);
      await page.addInitScript(clsInit);
      await openList(page, true);
      await expect(work(page, 'fleet:5'), rig.log).toHaveCount(1, { timeout: 20_000 });
      await settle(page);
      const r = await readCls(page);
      console.log(`[cls ${vp.name}] the sidebar list with work counts and tints ${r.total}`);
      expect(r.unsupported, 'layout-shift is observable here').toBeUndefined();
      expect(r.total, JSON.stringify(r.shifts)).toBeLessThan(BAR);
      // POSITIVE CONTROL: the same instrument, on the same page, sees a shift
      // when one happens — a block pushed in above the rows.
      await resetCls(page);
      await page.evaluate(() => {
        const d = document.createElement('div');
        d.style.height = '160px';
        document.querySelector('#sidebar-agents-list')!.prepend(d);
      });
      await settle(page);
      const c = await readCls(page);
      console.log(`[cls ${vp.name}] positive control (a block pushed in above the rows) ${c.total}`);
      expect(c.total, 'the instrument can observe a shift').toBeGreaterThan(BAR);
    });
  });
}

test('a count goes away live when its CLI goes idle, and comes back when it runs a shell again', async ({ page }) => {
  test.setTimeout(90_000);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.addInitScript(clsInit);
  // A page that holds the event stream (the sidebar's live refresh is heard
  // only on such a document; the account page's list waits for its minute's
  // tick), whose own content does not move on an agent change: a seeded
  // agent's page with no conversation.
  await openList(page, false, '/agents/notes-tidy');
  await expect(work(page, 'fleet:6'), rig.log).toHaveAttribute('data-nav-work', '3 shells', { timeout: 20_000 });
  await settle(page);
  await resetCls(page);
  status(trShell, 'idle', 0);
  await expect(work(page, 'fleet:6'), 'the count left without a reload').toHaveCount(0, { timeout: 20_000 });
  status(trShell, 'shell', 2);
  await expect(work(page, 'fleet:6'), 'the count came back without a reload').toHaveAttribute('data-nav-work', '2 shells', { timeout: 20_000 });
  await settle(page);
  // The live updates shift the page less than the bar too.
  const r = await readCls(page);
  console.log(`[cls phone] the count leaving and coming back live ${r.total} ${JSON.stringify(r.shifts)}`);
  expect(r.total, JSON.stringify(r.shifts)).toBeLessThan(BAR);
});
