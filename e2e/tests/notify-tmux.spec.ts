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

// askRow is the Asks list's name of the waiting window whose session is
// named name (its <span data-attention-name>, the place in its tooltip).
const askRow = (page: Page, w: number) =>
  page.locator(`[data-attention-item][data-kind="session"] [data-attention-name][title*="tnotify:${w} @"]`);

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

      // 3. Off again: exactly the name again.
      await setSwitch(page, false);
      await page.goto(origin + '/attention');
      await expect(askRow(page, w)).toHaveText(name);
    });
  });
}
