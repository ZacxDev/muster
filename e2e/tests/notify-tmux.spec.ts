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
// docs/focus.md#show-tmux-session-in-notifications), in a real browser against
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
//      "<name> · <tmux session>:<index> @ <host>";
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
const now = Math.floor(Date.now() / 1000);
const sids = { phone: 'ntmux-phone-' + stamp, desktop: 'ntmux-desktop-' + stamp };
const windows = { phone: 1, desktop: 2 };
const names = { phone: 'Billing migration', desktop: 'Ledger cleanup' };
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
  writeFileAtomic(windowsFile, rows.join('\n') + '\n');
  mkchat({
    MUSTER_E2E_FOCUS_ONLY: '1',
    MUSTER_E2E_FOCUS: JSON.stringify((['phone', 'desktop'] as const).map((k) => ({ subject: 'mirror:' + sids[k], title: names[k], tail: TAIL }))),
  });
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile, MUSTER_MIRROR_INTERACTIVE: '1' });
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

    test('the switch is off by default, turns on and persists, and the Asks row gains and loses the tmux address', async ({ page }) => {
      await signIn(page, origin);
      await rig.waitListed(page, `tnotify:${w}`);

      // 1. Off by default: exactly the name.
      await page.goto(origin + '/account');
      await expect(page.locator('[data-testid="notify-tmux"]')).toHaveAttribute('data-state', 'off');
      await expect(page.locator('[data-testid="notify-tmux"]')).toContainText('Show tmux session in notifications');
      await page.goto(origin + '/attention');
      await expect(askRow(page, w)).toHaveText(name);

      // 2. On: persists, and the row names where the session is.
      await setSwitch(page, true);
      await page.goto(origin + '/attention');
      await expect(askRow(page, w)).toHaveText(`${name} · tnotify:${w} @ ${host}`);

      // 3. Off again: exactly the name again.
      await setSwitch(page, false);
      await page.goto(origin + '/attention');
      await expect(askRow(page, w)).toHaveText(name);
    });
  });
}
