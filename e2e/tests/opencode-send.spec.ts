import fs from 'node:fs';
import path from 'node:path';
import { Page, Route } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { openCodeSendPort, password } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';
import { revealBox } from '../lib/composer';

// SENDING TO OPENCODE THROUGH ITS API (docs/design/opencode-first-class.md),
// ON A REAL SERVER (the e2eterm binary, mirror plugin on, a 1 s send-queue
// idle) AND REAL muster-mirror processes whose fakes are tmux and the opencode
// server (cmd/muster-mirror/e2eopencode.go). The opted-in host runs with
// -interactive and -opencode-send; its window @2 is LAUNCHER-BOUND (a real
// launcher record, read by the real resolver), @3 is only title-matched. A
// second host is -interactive WITHOUT -opencode-send. A message the fake
// opencode server admits becomes the session's user message, so the view's
// own row for it is the proof that it landed — and how many times. Every value
// is invented fixture data.
//
// 🔴 REGRESSION TEST. On origin/main every opencode composer is disabled
// ("Sending to opencode isn't supported yet"), with no queue.

const rig = new MirrorRig(openCodeSendPort, 'e2e-ocsend', { MUSTER_SEND_QUEUE_IDLE: '1s' });
const bound = 'ses_0e2esend1a2b3c4d';
const titled = 'ses_0e2esend5e6f7a8b';
const t0 = Math.floor(Date.now() / 1000);
let ops = '';
let ocWindows = '';
let offHost = '';
let stopOff: (() => Promise<void>) | undefined;

function op(o: Record<string, unknown>) {
  fs.appendFileSync(ops, JSON.stringify(o) + '\n');
}

function windowsLine(boundTitle: string) {
  // sess|id|index|active|activity|pane title
  return `oc|@2|0|1|${t0 - 10}|${boundTitle}\noc|@3|1|0|${t0 - 20}|OC | Title-matched fixture session\n`;
}

test.beforeAll(async ({ browser }) => {
  test.setTimeout(180_000);
  await rig.start();
  const dir = fs.mkdtempSync(path.join(rig.dir, 'opencode-'));
  ops = path.join(dir, 'ops.jsonl');
  op({ op: 'session', id: bound, title: 'Bound fixture session' });
  op({ op: 'session', id: titled, title: 'Title-matched fixture session' });
  // No status op: a session that has never run is idle WITH a status time
  // (mirrorhost's OpenCodeSource), so it takes a send before its first turn.
  op({ op: 'bind', session: 'oc', window: '@2', id: bound });
  const windows = path.join(rig.dir, 'windows');
  writeFileAtomic(windows, `plain|@1|0|1|zsh|${t0 - 50}|\n`);
  ocWindows = path.join(rig.dir, 'opencode-windows');
  writeFileAtomic(ocWindows, windowsLine('OC | Bound fixture session'));
  await rig.startHost({
    MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
    MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE: ocWindows,
    MUSTER_MIRROR_E2E_OPENCODE_DIR: dir,
    MUSTER_MIRROR_E2E_REFRESH: '200ms',
    MUSTER_MIRROR_INTERACTIVE: '1',
    MUSTER_MIRROR_OPENCODE_SEND: '1',
  });

  // The host that did NOT opt in: the same bound window, its own fake server.
  const offDir = fs.mkdtempSync(path.join(rig.dir, 'opencode-off-'));
  fs.writeFileSync(
    path.join(offDir, 'ops.jsonl'),
    [
      { op: 'session', id: bound, title: 'Bound fixture session' },
      { op: 'status', session: bound, status: 'idle' },
      { op: 'bind', session: 'off', window: '@2', id: bound },
    ]
      .map((o) => JSON.stringify(o))
      .join('\n') + '\n',
  );
  const offWindows = path.join(rig.dir, 'opencode-windows-off');
  writeFileAtomic(offWindows, `off|@2|0|1|${t0 - 10}|OC | Bound fixture session\n`);
  const offPlain = path.join(rig.dir, 'windows-off');
  writeFileAtomic(offPlain, `plainoff|@1|0|1|zsh|${t0 - 50}|\n`);
  offHost = rig.host + '-off';
  stopOff = rig.extraHost(offHost, {
    MUSTER_MIRROR_E2E_WINDOWS_FILE: offPlain,
    MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE: offWindows,
    MUSTER_MIRROR_E2E_OPENCODE_DIR: offDir,
    MUSTER_MIRROR_E2E_REFRESH: '200ms',
    MUSTER_MIRROR_INTERACTIVE: '1',
  });

  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await rig.login(page);
  await rig.waitListed(page, 'session=oc');
  await ctx.close();
});

test.afterAll(async () => {
  if (stopOff) await stopOff();
  await rig.stop();
});

test.use({ storageState: { cookies: [], origins: [] } });

const viewOf = (window: string, host = rig.host, session = 'oc') =>
  `/mirror/view?host=${encodeURIComponent(host)}&session=${encodeURIComponent(session)}&window=${encodeURIComponent(window)}`;
const pendingOf = (page: Page, text: string) => page.locator('#mv-log [data-pending-send]', { hasText: text });
const rowsOf = (page: Page, text: string) => page.locator('#mv-log [data-bubble="user"]', { hasText: text });

async function openBound(page: Page) {
  await rig.login(page);
  await page.goto(rig.origin + viewOf('@2'));
  await expect(page.locator('#mv-log [data-notice-row="opencode"]').first(), `the bound notice (mirror log:\n${rig.log})`).toContainText(
    'started by muster-mirror opencode: ' + bound,
    { timeout: 20_000 },
  );
  await revealBox(page);
  await expect(page.locator('#mv-input')).toBeEnabled();
}

async function send(page: Page, text: string) {
  await page.locator('#mv-input').fill(text);
  await page.locator('#mv-send').click();
}

test('a launcher-bound opencode window sends from the Rendered view: pending at once, then the session’s own row, once', async ({ page }) => {
  await openBound(page);
  await expect(page.locator('#mv-readonly')).toHaveCount(0);
  await expect(page.locator('#mv-input')).toHaveAttribute('placeholder', 'Message opencode…');
  const text = `hello opencode ${Date.now()}`;
  await send(page, text);
  // Drawn at once, pending; the box clears.
  await expect(pendingOf(page, text)).toHaveCount(1);
  await expect(page.locator('#mv-input')).toHaveValue('');
  // The session's own row replaces it: exactly one, no bubble left.
  await expect(rowsOf(page, text)).toHaveCount(1, { timeout: 20_000 });
  await expect(page.locator('#mv-log [data-pending-send]')).toHaveCount(0, { timeout: 10_000 });
  // And stays one across a reload (the fake admitted it once).
  await page.reload();
  await expect(rowsOf(page, text)).toHaveCount(1, { timeout: 20_000 });
});

test('a send opencode does not confirm is Not confirmed, and its Send again lands at most once', async ({ page }) => {
  test.setTimeout(120_000);
  await openBound(page);
  // The server admits at once and answers far later than the host waits.
  op({ op: 'delay', ms: 30_000 });
  const text = `maybe landed ${Date.now()}`;
  await send(page, text);
  const p = pendingOf(page, text);
  await expect(p, `Not confirmed (mirror log:\n${rig.log})`).toHaveAttribute('data-pending-send', 'unknown', { timeout: 40_000 });
  // It DID land (the fake admitted it before the late answer).
  await expect(rowsOf(page, text)).toHaveCount(1, { timeout: 20_000 });
  op({ op: 'delay', ms: 0 });
  await page.locator('#mv-send-again').click();
  // Send again posts the SAME message id: answered with the first admission.
  // Its row arrived BEFORE the Send again, so no new row settles the bubble:
  // a confirmed send whose row never arrives is taken off after 30 s
  // (docs/sending-and-step-up.md, "A sent message shows at once").
  await expect(pendingOf(page, text)).not.toHaveAttribute('data-pending-send', 'unknown', { timeout: 20_000 });
  await expect(page.locator('#mv-log [data-pending-send]')).toHaveCount(0, { timeout: 40_000 });
  await page.reload();
  await expect(rowsOf(page, text), 'landed exactly once').toHaveCount(1, { timeout: 20_000 });
});

test('a message queued while opencode works is sent once it is idle', async ({ page }) => {
  test.setTimeout(60_000);
  await openBound(page);
  op({ op: 'status', session: bound, status: 'busy' });
  await expect(page.locator('#mv-working')).toBeVisible({ timeout: 20_000 });
  const text = `queued for opencode ${Date.now()}`;
  await page.locator('#mv-input').fill(text);
  await page.locator('#mv-send-more').click();
  await page.locator('#mv-queue-send').click();
  await expect(page.locator('#mv-note')).toHaveText('Queued.', { timeout: 10_000 });
  await expect(page.locator('#mv-queue [data-queue-id]', { hasText: text })).toHaveCount(1);
  // Not sent while busy.
  await page.waitForTimeout(2_500);
  await expect(rowsOf(page, text)).toHaveCount(0);
  op({ op: 'status', session: bound, status: 'idle' });
  await expect(rowsOf(page, text), 'delivered once idle').toHaveCount(1, { timeout: 30_000 });
});

test('each refusal says why: a slash command, a title-matched window, a host that did not opt in', async ({ page }) => {
  await openBound(page);
  const text = '/review the fixture branch';
  await send(page, text);
  const p = pendingOf(page, text);
  await expect(p).toHaveAttribute('data-pending-send', 'failed', { timeout: 20_000 });
  await expect(p).toContainText('that is an opencode slash command');
  await expect(rowsOf(page, text)).toHaveCount(0);

  await page.goto(rig.origin + viewOf('@3'));
  await expect(page.locator('#mv-input')).toBeDisabled({ timeout: 20_000 });
  await expect(page.locator('#mv-readonly')).toContainText("wasn't started with `muster-mirror opencode`");
  await expect(page.locator('#mv-queue, #mv-queue-send')).toHaveCount(0);

  await page.goto(rig.origin + viewOf('@2', offHost, 'off'));
  await expect(page.locator('#mv-input')).toBeDisabled({ timeout: 20_000 });
  await expect(page.locator('#mv-readonly')).toContainText('does not send to opencode (it needs -interactive and -opencode-send)');
});

test('a send that needs a fresh sign-in is not delivered until it is confirmed, then lands once', async ({ page }) => {
  await openBound(page);
  // The server's stale answer for the first post (as optimistic-send.spec
  // does): the hub's own stale gate is pinned in Go (mirror's
  // TestAnOpenCodeSendIsRefusedBeforeTheHost, "a stale sign-in").
  const refused = new Set<string>();
  await page.route(
    (u) => u.pathname === '/mirror/view/send',
    async (r: Route) => {
      const nonce = new URLSearchParams(r.request().postData() || '').get('nonce') || '';
      if (!refused.has(nonce)) {
        refused.add(nonce);
        return r.fulfill({ status: 403, headers: { 'X-Muster-Step-Up': '1', 'Content-Type': 'text/plain' }, body: 'confirm it is you' });
      }
      return r.continue();
    },
  );
  const text = `after a step-up ${Date.now()}`;
  await send(page, text);
  await expect(pendingOf(page, text)).toHaveAttribute('data-pending-send', 'stepup');
  await page.waitForTimeout(1_000);
  await expect(rowsOf(page, text), 'nothing lands before the confirm').toHaveCount(0);
  await page.locator('#term-confirm-password').fill(password);
  await page.locator('#term-confirm-submit').click();
  await expect(rowsOf(page, text)).toHaveCount(1, { timeout: 20_000 });
});

test('a bound pane whose title shows another session warns, and its send still lands in the bound session', async ({ page }) => {
  await openBound(page);
  writeFileAtomic(ocWindows, windowsLine('OC | Title-matched fixture session'));
  // The note is drawn when the page loads, not live: wait for the host's next
  // listing (200 ms) to reach the server — read from the served page, since a
  // reload of a live view does not return — then open the view once.
  const warning = "This pane's title shows a different opencode session (Title-matched fixture session)";
  await expect
    .poll(async () => (await (await page.request.get(rig.origin + viewOf('@2'))).text()).includes(warning.replace("'", '&#39;')), {
      timeout: 20_000,
      intervals: [250],
    })
    .toBe(true);
  await page.goto(rig.origin + viewOf('@2'));
  await expect(page.locator('#mv-host-note')).toContainText(warning);
  await revealBox(page);
  const text = `despite the title ${Date.now()}`;
  await send(page, text);
  // The view is the BOUND session's; the row arrives in it.
  await expect(rowsOf(page, text)).toHaveCount(1, { timeout: 20_000 });
  writeFileAtomic(ocWindows, windowsLine('OC | Bound fixture session'));
});
