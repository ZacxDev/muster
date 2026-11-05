import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Locator, Page, Route } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { binDir, dsn, optimisticSendPort, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { writeFileAtomic } from '../lib/atomic';

// A SENT MESSAGE SHOWS AT ONCE AS PENDING, THEN RECONCILES (mirror_compose.go's
// pending sends). The operator's ask: no hang between clicking Send and seeing
// the message go out.
//
// A real muster-server (mirror plugin on, MUSTER_SEND_QUEUE_IDLE=1s) and a real
// muster-mirror built with `-tags e2emirror` over the real tunnel: one Claude
// Code window whose status this spec writes (<transcript>.status). The fake
// plays Claude Code taking a paste: it appends the text to <transcript>.pasted
// AND the prompt record to the transcript itself, so the real transcript row
// arrives through the real tailer, as it does from a real Claude Code.
//
// 🔴 THE HOST'S DELAY IS INJECTED IN THE BROWSER (page.route on the send
// route): every send is held for SEND_DELAY_MS before it reaches the server,
// which is a slow host round trip as the page sees it. The pending bubble must
// be on screen long before that answer.

const origin = `http://localhost:${optimisticSendPort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-opt-' + stamp;
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'optimistic-send-'));
const tr = path.join(dir, 'ffffffff-0001.jsonl');
const pane = '%e2e-work-@1';
let server: Running | undefined;
let proc: ChildProcess | undefined;
let mirrorLog = '';

const SEND_DELAY_MS = 1_500;
// What "at once" is held to: the pending bubble within this of the press.
const AT_ONCE_MS = 100;

const prompt = (text: string) => JSON.stringify({ type: 'user', promptId: 'p-' + text.length, origin: { kind: 'human' }, message: { role: 'user', content: text } }) + '\n';
const read = (f: string) => (fs.existsSync(f) ? fs.readFileSync(f, 'utf8') : '');
const setStatus = (status: string) => writeFileAtomic(tr + '.status', JSON.stringify({ status, statusUpdatedAt: Date.now() }));
const pasted = () => read(tr + '.pasted').split('\n').filter((l) => l !== '');
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

test.beforeAll(async () => {
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, optimisticSendPort, {
    MUSTER_MIRROR: '1',
    MUSTER_SEND_QUEUE_IDLE: '1s',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  fs.writeFileSync(tr, prompt('the conversation so far'));
  setStatus('idle');
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  const windows = path.join(dir, 'windows');
  writeFileAtomic(windows, `work|@1|0|1|claude|${Math.floor(Date.now() / 1000) - 60}|${tr}\n`);
  proc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
      MUSTER_MIRROR_E2E_REFRESH: '250ms',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  proc.stdout?.on('data', (d) => (mirrorLog += d));
  proc.stderr?.on('data', (d) => (mirrorLog += d));
  await expect.poll(() => mirrorLog.includes(`as host ${host} `), { message: 'the host connects', timeout: 20_000 }).toBe(true);
});

test.afterAll(async () => {
  if (proc && proc.exitCode === null) {
    const done = new Promise((r) => proc!.once('exit', r));
    proc.kill('SIGTERM');
    await done;
  }
  await server?.stop();
  fs.rmSync(dir, { recursive: true, force: true });
});

test.beforeEach(() => {
  setStatus('idle');
  fs.rmSync(tr + '.pasted', { force: true });
});

const viewPath = `/mirror/view?host=${encodeURIComponent(host)}&session=work&window=%401`;

async function openView(page: Page) {
  await signIn(page, origin);
  await page.goto(origin + viewPath);
  await expect(page.locator('#mv-input'), `the rendered view (log:\n${mirrorLog})`).toBeEnabled({ timeout: 20_000 });
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 20_000 });
}

// delaySends holds every POST to `route` for ms before it goes on to the
// server — a slow host, as the page sees it. It returns the count of sends.
async function delaySends(page: Page, route: string, ms = SEND_DELAY_MS): Promise<{ n: number }> {
  const seen = { n: 0 };
  await page.route(
    (u) => u.pathname === route,
    async (r: Route) => {
      if (r.request().method() !== 'POST') return r.continue();
      seen.n++;
      await sleep(ms);
      await r.continue().catch(() => {});
    },
  );
  return seen;
}

// press is a tap on a touch device and a click otherwise.
async function press(l: Locator, touch: boolean) {
  if (touch) await l.tap();
  else await l.click();
}

// sendTimed sends text and returns the milliseconds from the press to the
// pending bubble (or the bubble that settled) being in the document, measured
// in the page (performance marks) so the measurement is the page's own clock.
async function sendTimed(page: Page, text: string, how: 'tap' | 'click' | 'enter'): Promise<number> {
  await page.locator('#mv-input').fill(text);
  await page.evaluate((t) => {
    const w = window as unknown as { __seenAt?: number; __pressAt?: number };
    w.__seenAt = 0;
    const log = document.getElementById('mv-log')!;
    const found = () =>
      Array.from(log.querySelectorAll('[data-pending-send], [data-bubble="user"]')).some((el) => (el.textContent || '').includes(t));
    new MutationObserver((_, obs) => {
      if (!w.__seenAt && found()) {
        w.__seenAt = performance.now();
        performance.mark('message-visible');
        obs.disconnect();
      }
    }).observe(log, { childList: true, subtree: true, characterData: true });
    const send = document.getElementById('mv-send')!;
    const input = document.getElementById('mv-input')!;
    const at = () => { if (!w.__pressAt) { w.__pressAt = performance.now(); performance.mark('send-pressed'); } };
    w.__pressAt = 0;
    send.addEventListener('pointerdown', at, { once: true, capture: true });
    send.addEventListener('click', at, { once: true, capture: true });
    input.addEventListener('keydown', (e) => { if ((e as KeyboardEvent).key === 'Enter') at(); }, { once: true, capture: true });
  }, text);
  if (how === 'enter') await page.locator('#mv-input').press('Enter');
  else await press(page.locator('#mv-send'), how === 'tap');
  await expect
    .poll(() => page.evaluate(() => (window as unknown as { __seenAt?: number }).__seenAt || 0), { message: `"${text}" is visible`, timeout: 15_000 })
    .toBeGreaterThan(0);
  return page.evaluate(() => {
    const w = window as unknown as { __seenAt: number; __pressAt: number };
    return w.__seenAt - w.__pressAt;
  });
}

const pendingOf = (page: Page, text: string) => page.locator('#mv-log [data-pending-send]', { hasText: text });
// rowsOf is every message row in the conversation holding text: the real
// transcript's user bubbles and any pending bubble — what a reader would count.
const rowsOf = (page: Page, text: string) => page.locator('#mv-log [data-bubble="user"], #mv-log [data-pending-send]', { hasText: text });

// watchRows counts, after EVERY DOM change of the conversation, the rows that
// hold text (the pending bubble and the transcript's own), and the states the
// bubble went through. 🔴 A MutationObserver's callback runs once per task, so
// a bubble removed in the same task its row is drawn reads as one row
// throughout — a duplicate (2) or a gap (0 once it had shown) is a frame a
// reader could see.
async function watchRows(page: Page, text: string) {
  await page.evaluate((t) => {
    const w = window as unknown as { __rows: { max: number; gap: boolean; shown: boolean; states: string[] } };
    w.__rows = { max: 0, gap: false, shown: false, states: [] };
    const log = document.getElementById('mv-log')!;
    const count = () => {
      const n = Array.from(log.querySelectorAll('[data-bubble="user"], [data-pending-send]')).filter((el) => (el.textContent || '').includes(t)).length;
      const r = w.__rows;
      r.max = Math.max(r.max, n);
      if (n > 0) r.shown = true;
      else if (r.shown) r.gap = true;
      const b = Array.from(log.querySelectorAll('[data-pending-send]')).find((el) => (el.textContent || '').includes(t));
      const st = b ? b.getAttribute('data-pending-send') || '' : 'row';
      if (r.states[r.states.length - 1] !== st && (b || n > 0)) r.states.push(st);
    };
    new MutationObserver(count).observe(log, { childList: true, subtree: true, attributes: true, attributeFilter: ['data-pending-send'] });
  }, text);
  return () => page.evaluate(() => (window as unknown as { __rows: { max: number; gap: boolean; shown: boolean; states: string[] } }).__rows);
}

for (const vp of [
  { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true }, how: 'tap' as const },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false }, how: 'enter' as const },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    // 🔴 THE REGRESSION TEST. On origin/main nothing is drawn until the
    // send's answer (the box clears then), so the message is first seen only
    // after the injected delay — and in the conversation only once the
    // transcript row arrives.
    test('a sent message shows at once as pending, the box clears, and it becomes exactly one row when Claude Code takes it', async ({ page }) => {
      await openView(page);
      await delaySends(page, '/mirror/view/send');
      const text = `pending at once ${vp.how} ${stamp}`;
      const rows = await watchRows(page, text);
      const ms = await sendTimed(page, text, vp.how);
      test.info().annotations.push({ type: 'press-to-visible-ms', description: String(Math.round(ms)) });
      console.log(`[${vp.name}] press → message visible: ${ms.toFixed(1)} ms (send held ${SEND_DELAY_MS} ms)`);
      expect(ms, 'the message is on screen at once, not after the host answers').toBeLessThan(AT_ONCE_MS);
      await expect(page.locator('#mv-input'), 'the box clears at once').toHaveValue('');
      const p = pendingOf(page, text);
      await expect(p).toHaveAttribute('data-pending-send', 'sending');
      await expect(p.getByRole('status')).toHaveText('Sending…');
      // It is the conversation's newest row.
      await expect(page.locator('#mv-log > :not(#mv-asks)').last()).toHaveAttribute('data-pending-send', 'sending');
      // Still pending while the host has not answered.
      await sleep(SEND_DELAY_MS / 2);
      await expect(p).toHaveAttribute('data-pending-send', 'sending');
      // The host pastes it (once) and Claude Code's transcript row arrives:
      // it REPLACES the pending bubble — one row, in the same place (the end).
      await expect.poll(pasted, { message: 'the host pastes it once', timeout: 10_000 }).toEqual([`${pane} ${text}`]);
      await expect(page.locator('#mv-log [data-bubble="user"]', { hasText: text })).toHaveCount(1);
      await expect(page.locator('#mv-log [data-pending-send]'), 'no pending bubble is left').toHaveCount(0);
      await expect(rowsOf(page, text), 'exactly one row: no duplicate').toHaveCount(1);
      await expect(page.locator('#mv-log [data-bubble="user"]').last()).toContainText(text);
      // And it stays one: nothing redraws the pending bubble later.
      await sleep(1_000);
      await expect(rowsOf(page, text)).toHaveCount(1);
      const seen = await rows();
      expect(seen.max, 'never two rows for it, not even for a frame').toBe(1);
      expect(seen.gap, 'never no row for it once shown (no flicker)').toBe(false);
      expect(seen.states[0], 'drawn pending').toBe('sending');
      expect(seen.states[seen.states.length - 1], 'ends as the transcript row').toBe('row');
    });

    test('a refused send turns its bubble failed with the reason and Retry, keeps the text, and Retry sends it once', async ({ page }) => {
      await openView(page);
      setStatus('waiting');
      // The host's refresh carries the status to muster (four refreshes).
      await sleep(1_500);
      await delaySends(page, '/mirror/view/send', 500);
      const text = `refused then retried ${vp.how} ${stamp}`;
      const ms = await sendTimed(page, text, vp.how);
      expect(ms).toBeLessThan(AT_ONCE_MS);
      const p = pendingOf(page, text);
      await expect(p).toHaveAttribute('data-pending-send', 'failed', { timeout: 10_000 });
      await expect(p).toContainText('Claude is waiting on a permission prompt');
      await expect(page.locator('#mv-input'), 'the text is put back: nothing is lost').toHaveValue(text);
      expect(pasted(), 'nothing was typed').toEqual([]);
      setStatus('idle');
      await sleep(1_500);
      await press(p.getByRole('button', { name: 'Retry' }), vp.how === 'tap');
      await expect(page.locator('#mv-input'), 'Retry takes the text out of the box again').toHaveValue('');
      await expect.poll(pasted, { message: 'Retry pastes it once', timeout: 10_000 }).toEqual([`${pane} ${text}`]);
      await expect(rowsOf(page, text), 'one row after the retry').toHaveCount(1);
      await expect(page.locator('#mv-log [data-pending-send]')).toHaveCount(0);
    });

    test('a send that needs step-up stays pending while the dialog asks, and is sent after the confirm; a cancelled one fails and keeps its text', async ({ page }) => {
      await openView(page);
      // The server's stale answer, for the first post of each message: the
      // step-up the page must ask through before it sends.
      const refused = new Set<string>();
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          const body = r.request().postData() || '';
          const nonce = new URLSearchParams(body).get('nonce') || '';
          if (!refused.has(nonce)) {
            refused.add(nonce);
            return r.fulfill({ status: 403, headers: { 'X-Muster-Step-Up': '1', 'Content-Type': 'text/plain' }, body: 'confirm it is you' });
          }
          return r.continue();
        },
      );
      const text = `after step-up ${vp.how} ${stamp}`;
      await sendTimed(page, text, vp.how);
      const p = pendingOf(page, text);
      await expect(page.locator('#term-confirm')).toBeVisible();
      await expect(p).toHaveAttribute('data-pending-send', 'stepup');
      await expect(p.getByRole('status')).toContainText('Confirm it');
      await page.locator('#term-confirm-password').fill(password);
      await page.locator('#term-confirm-submit').click();
      await expect.poll(pasted, { message: 'sent once confirmed', timeout: 10_000 }).toEqual([`${pane} ${text}`]);
      await expect(rowsOf(page, text)).toHaveCount(1);
      await expect(page.locator('#mv-log [data-bubble="user"]', { hasText: text })).toHaveCount(1);
      await expect(page.locator('#mv-log [data-pending-send]')).toHaveCount(0);

      // Cancelled: the bubble fails, the text is back in the box.
      const text2 = `step-up cancelled ${vp.how} ${stamp}`;
      await page.evaluate(() => (window as unknown as { musterStepUp: { stale(): void } }).musterStepUp.stale());
      await sendTimed(page, text2, vp.how);
      await expect(page.locator('#term-confirm')).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(pendingOf(page, text2)).toHaveAttribute('data-pending-send', 'failed');
      await expect(page.locator('#mv-input')).toHaveValue(text2);
      expect(pasted()).toEqual([`${pane} ${text}`]);
    });

    test('Queue shows the message as queuing in the queue, never as sent in the conversation', async ({ page }) => {
      setStatus('busy');
      await sleep(1_500);
      await openView(page);
      await delaySends(page, '/send-queue');
      const text = `queued not sent ${vp.how} ${stamp}`;
      await page.locator('#mv-input').fill(text);
      const at = Date.now();
      await press(page.locator('#mv-send-more'), vp.how === 'tap');
      await press(page.locator('#mv-queue-send'), vp.how === 'tap');
      const q = page.locator('#mv-queue [data-pending-send]', { hasText: text });
      await expect(q).toHaveAttribute('data-pending-send', 'queuing', { timeout: 1_000 });
      expect(Date.now() - at, 'shown before the server answered').toBeLessThan(SEND_DELAY_MS);
      await expect(page.locator('#mv-input')).toHaveValue('');
      await expect(rowsOf(page, text), 'not in the conversation').toHaveCount(0);
      await expect(page.locator(`#mv-queue [data-queue-state="queued"]`, { hasText: text }), 'the server row replaces it').toHaveCount(1, { timeout: 10_000 });
      await expect(q, 'the pending row is gone').toHaveCount(0);
      expect(pasted(), 'Queue sends nothing now').toEqual([]);
      await expect(page.locator('#mv-note')).toHaveText('Queued.');
      // Tidy: Claude goes idle, the queue sends it.
      setStatus('idle');
      await expect.poll(pasted, { timeout: 20_000 }).toEqual([`${pane} ${text}`]);
    });

    test('a reload mid-send leaves no ghost: the bubble comes back, and is either reconciled or turns unknown with Send again', async ({ page }) => {
      await openView(page);
      // Held for good: the request never reaches the server, and the reload
      // abandons it.
      let release = () => {};
      const held = new Promise<void>((r) => (release = r));
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          await held;
          await r.abort().catch(() => {});
        },
      );
      const text = `reloaded mid-send ${vp.how} ${stamp}`;
      await sendTimed(page, text, vp.how);
      await page.reload();
      release();
      await page.unrouteAll({ behavior: 'ignoreErrors' });
      await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 20_000 });
      const p = pendingOf(page, text);
      await expect(p, 'the send in flight is shown again after the reload').toHaveCount(1);
      await expect(page.locator('#mv-input'), 'its text is not also put back in the box').toHaveValue('');
      // Nothing confirms it: it turns unknown, with Retry, after the timeout.
      await expect(p).toHaveAttribute('data-pending-send', 'unknown', { timeout: 20_000 });
      await expect(p).toContainText('may or may not have been sent');
      expect(pasted()).toEqual([]);
      // It may have landed, so its retry is the explicit Send again.
      await press(p.getByRole('button', { name: 'Send again' }), vp.how === 'tap');
      await expect.poll(pasted, { timeout: 10_000 }).toEqual([`${pane} ${text}`]);
      await expect(rowsOf(page, text)).toHaveCount(1);
      await expect(page.locator('#mv-log [data-bubble="user"]', { hasText: text })).toHaveCount(1);

      // Delivered before the reload: the row that arrives settles it.
      const text2 = `delivered then reloaded ${vp.how} ${stamp}`;
      let release2 = () => {};
      const held2 = new Promise<void>((r) => (release2 = r));
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          await held2;
          await r.abort().catch(() => {});
        },
      );
      await sendTimed(page, text2, vp.how);
      await page.reload();
      release2();
      await page.unrouteAll({ behavior: 'ignoreErrors' });
      await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 20_000 });
      await expect(pendingOf(page, text2)).toHaveCount(1);
      // Claude Code takes it now (the request that carried it was lost with
      // the page; the transcript is what says it arrived).
      fs.appendFileSync(tr, prompt(text2));
      await expect(rowsOf(page, text2), 'one row').toHaveCount(1);
      await expect(page.locator('#mv-log [data-pending-send]')).toHaveCount(0);
    });
  });
}
