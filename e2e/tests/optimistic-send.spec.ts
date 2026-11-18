import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Locator, Page, Route } from '@playwright/test';
import { revealBox } from '../lib/composer';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { binDir, dsn, optimisticSendPort, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { writeFileAtomic } from '../lib/atomic';
import { launchWithBfcache } from '../lib/bfcache';

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
  // Collapsed on a touch screen behind "Type a message" (lib/composer.ts);
  // a restored draft has already revealed it.
  await revealBox(page);
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

// draftsHolding is the saved drafts (this browser's localStorage) that hold
// text.
const draftsHolding = (page: Page, text: string) =>
  page.evaluate((t) => {
    const out: string[] = [];
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i) || '';
      const v = localStorage.getItem(k) || '';
      if (k.startsWith('muster.draft.') && v.includes(t)) out.push(v);
    }
    return out;
  }, text);

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
    // 🔴 SERVICE WORKERS BLOCKED: these tests answer the send with page.route,
    // and once the PWA's worker (web/static/sw.js) claims the page its fetches
    // no longer reach page.route — the worker never handles a POST, but the
    // request is then not the page's to intercept. Measured: with the extra
    // step that reveals the phone's collapsed box (lib/composer.ts) the worker
    // had time to claim the page, a 409 the route meant to give was answered
    // 200 by the server, and ten phone tests failed in order (each passed
    // alone); blocked, all 32 passed.
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] }, serviceWorkers: 'block' });

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

    // 🔴 The row can reach the page BEFORE the host's OK: the host tails the
    // transcript on its own tick (ViewPoll, 300 ms), and on a loaded machine
    // that tick can land between the paste and the answer's arrival (CI, run
    // 42149018442, the case above: two rows). Here the answer is held until
    // the row is drawn, so it is always so. Before, the "Sending…" bubble and
    // the row were both on the page until the OK.
    test('a message whose row arrives before the host answers OK is one row throughout, never two', async ({ page }) => {
      await openView(page);
      const text = `row before the ok ${vp.how} ${stamp}`;
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          const resp = await r.fetch();
          const deadline = Date.now() + 10_000;
          while (Date.now() < deadline && (await page.locator('#mv-log [data-bubble="user"]', { hasText: text }).count()) === 0) await sleep(50);
          await sleep(300);
          return r.fulfill({ response: resp }).catch(() => {});
        },
      );
      const rows = await watchRows(page, text);
      await sendTimed(page, text, vp.how);
      await expect.poll(pasted, { message: 'pasted once', timeout: 10_000 }).toEqual([`${pane} ${text}`]);
      await expect(page.locator('#mv-log [data-bubble="user"]', { hasText: text })).toHaveCount(1, { timeout: 10_000 });
      await expect(page.locator('#mv-log [data-pending-send]'), 'no pending bubble is left').toHaveCount(0, { timeout: 10_000 });
      await sleep(500);
      await expect(rowsOf(page, text)).toHaveCount(1);
      const seen = await rows();
      expect(seen.max, 'never the bubble and its row together').toBe(1);
      expect(seen.gap, 'never neither').toBe(false);
      expect(seen.states[seen.states.length - 1]).toBe('row');
    });

    // The other side of it: a row of its text that came while it was out does
    // not decide anything. Refused after it, the message is shown failed, with
    // Retry, and its text is back in the box.
    test('a message refused after a row of its text arrived is shown failed with Retry, its text back in the box', async ({ page }) => {
      await openView(page);
      const text = `row then refused ${vp.how} ${stamp}`;
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          // Someone types the same words into Claude Code itself meanwhile.
          fs.appendFileSync(tr, prompt(text));
          const deadline = Date.now() + 10_000;
          while (Date.now() < deadline && (await page.locator('#mv-log [data-bubble="user"]', { hasText: text }).count()) === 0) await sleep(50);
          await sleep(300);
          return r.fulfill({ status: 409, headers: { 'Content-Type': 'text/plain' }, body: 'the host said no' });
        },
      );
      await sendTimed(page, text, vp.how);
      const p = pendingOf(page, text);
      await expect(p, 'the refusal is shown').toHaveAttribute('data-pending-send', 'failed', { timeout: 10_000 });
      await expect(p).toContainText('the host said no');
      await expect(p.getByRole('button', { name: 'Retry' })).toBeVisible();
      await expect(page.locator('#mv-log > :not(#mv-asks)').last(), 'at the end of the conversation').toHaveAttribute('data-pending-send', 'failed');
      await expect(page.locator('#mv-input'), 'its text is back in the box').toHaveValue(text);
      expect(pasted(), 'nothing was pasted').toEqual([]);
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
      // D1: the reason is said ONCE — on the bubble, not in the status line too.
      await expect(page.locator('#mv-note'), 'the reason is not said twice').toBeHidden();
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

    // A row of its text that came while it was out does not hide a step-up:
    // the bubble stood aside for that row, and the 403 draws it again, asking,
    // at the end of the conversation, for as long as the dialog is up.
    test('a message asked to step up after a row of its text arrived shows its bubble asking while the dialog is up, and is sent once after the confirm', async ({ page }) => {
      await openView(page);
      const text = `row then step-up ${vp.how} ${stamp}`;
      const refused = new Set<string>();
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          const nonce = new URLSearchParams(r.request().postData() || '').get('nonce') || '';
          if (refused.has(nonce)) return r.continue();
          refused.add(nonce);
          // Someone types the same words into Claude Code itself meanwhile.
          fs.appendFileSync(tr, prompt(text));
          const deadline = Date.now() + 10_000;
          while (Date.now() < deadline && (await page.locator('#mv-log [data-bubble="user"]', { hasText: text }).count()) === 0) await sleep(50);
          await sleep(300);
          return r.fulfill({ status: 403, headers: { 'X-Muster-Step-Up': '1', 'Content-Type': 'text/plain' }, body: 'confirm it is you' });
        },
      );
      await sendTimed(page, text, vp.how);
      await expect(page.locator('#term-confirm')).toBeVisible({ timeout: 10_000 });
      const p = pendingOf(page, text);
      await expect(p, 'the bubble is back, asking, while the dialog is up').toHaveAttribute('data-pending-send', 'stepup');
      await expect(p.getByRole('status')).toContainText('Confirm it');
      await expect(page.locator('#mv-log > :not(#mv-asks)').last(), 'at the end of the conversation').toHaveAttribute('data-pending-send', 'stepup');
      expect(pasted(), 'nothing pasted before the confirm').toEqual([]);
      await page.locator('#term-confirm-password').fill(password);
      await page.locator('#term-confirm-submit').click();
      await expect.poll(pasted, { message: 'sent once confirmed', timeout: 10_000 }).toEqual([`${pane} ${text}`]);
      await expect(page.locator('#mv-log [data-pending-send]'), 'no pending bubble is left').toHaveCount(0, { timeout: 10_000 });
    });

    test('Queue says "Queuing…" in the status line at once, never shows it in the conversation, and the server row lists it', async ({ page }) => {
      setStatus('busy');
      await sleep(1_500);
      await openView(page);
      await delaySends(page, '/send-queue');
      const text = `queued not sent ${vp.how} ${stamp}`;
      await page.locator('#mv-input').fill(text);
      const at = Date.now();
      await press(page.locator('#mv-send-more'), vp.how === 'tap');
      await press(page.locator('#mv-queue-send'), vp.how === 'tap');
      await expect(page.locator('#mv-note'), 'the status line says it at once').toHaveText('Queuing…', { timeout: 1_000 });
      expect(Date.now() - at, 'shown before the server answered').toBeLessThan(SEND_DELAY_MS);
      await expect(page.locator('#mv-input')).toHaveValue('');
      await expect(rowsOf(page, text), 'not in the conversation').toHaveCount(0);
      await expect(page.locator('#mv-queue [data-pending-send]'), 'no row of its own in the queue list').toHaveCount(0);
      await expect(page.locator(`#mv-queue [data-queue-state="queued"]`, { hasText: text }), 'the server row lists it').toHaveCount(1, { timeout: 10_000 });
      expect(pasted(), 'Queue sends nothing now').toEqual([]);
      await expect(page.locator('#mv-note')).toHaveText('Queued.');
      // Tidy: Claude goes idle, the queue sends it.
      setStatus('idle');
      await expect.poll(pasted, { timeout: 20_000 }).toEqual([`${pane} ${text}`]);
    });

    // D2: nothing is kept for a reload but the DRAFT. A message still on its
    // way when the page goes is back in the box as a draft — never redrawn as
    // a bubble, never posted again by itself.
    test('a reload mid-send puts the text back in the box as a draft, and nothing is sent twice', async ({ page }) => {
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
      await expect(page.locator('#mv-input'), 'the box empties at the press').toHaveValue('');
      await page.reload();
      release();
      await page.unrouteAll({ behavior: 'ignoreErrors' });
      await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 20_000 });
      await expect(page.locator('#mv-input'), 'its text is back in the box, as a draft').toHaveValue(text);
      await expect(page.locator('#mv-log [data-pending-send]'), 'no bubble is drawn again').toHaveCount(0);
      await sleep(1_500);
      expect(pasted(), 'nothing was sent by itself').toEqual([]);
      // The person sends it: once.
      await press(page.locator('#mv-send'), vp.how === 'tap');
      await expect.poll(pasted, { timeout: 10_000 }).toEqual([`${pane} ${text}`]);
      await expect(rowsOf(page, text)).toHaveCount(1);
      await sleep(1_000);
      expect(pasted(), 'sent once').toEqual([`${pane} ${text}`]);
    });

    // Y2: the request fails AND the browser's own submit fails too. Before,
    // the press had already cleared the draft: the text was nowhere.
    test('a send whose request and whose browser submit both fail is back in the box as a draft', async ({ page }) => {
      await openView(page);
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        (r: Route) => r.abort('connectionfailed').catch(() => {}),
      );
      const text = `both ways failed ${vp.how} ${stamp}`;
      await page.locator('#mv-input').fill(text);
      if (vp.how === 'enter') await page.locator('#mv-input').press('Enter');
      else await press(page.locator('#mv-send'), true);
      // The browser's own submit lands on its error page.
      await expect.poll(() => page.url(), { timeout: 10_000 }).not.toContain('/mirror/view?');
      await page.unrouteAll({ behavior: 'ignoreErrors' });
      await page.goto(origin + viewPath);
      await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 20_000 });
      await expect(page.locator('#mv-input'), 'nothing typed is lost').toHaveValue(text);
      expect(pasted()).toEqual([]);
    });

    // 🔴 R1: a message waiting its turn is never settled by a row. A reset of
    // the conversation redraws an OLD row of the same text as new; before,
    // that settled the waiting bubble, and post() then skipped it: never sent,
    // no error, the box empty.
    test('a reset of the conversation while a slow send is out does not take a waiting message of an old text for sent: both are pasted once', async ({ page }) => {
      const repeat = `said before ${vp.how} ${stamp}`;
      fs.appendFileSync(tr, prompt(repeat));
      await openView(page);
      await expect(page.locator('#mv-log [data-bubble="user"]', { hasText: repeat })).toHaveCount(1, { timeout: 10_000 });
      // Every row drawn now is marked: after the reset, none of them is left.
      await page.evaluate(() => {
        document.querySelectorAll('#mv-log [data-bubble="user"]').forEach((el) => ((el as unknown as { __old?: boolean }).__old = true));
      });
      await delaySends(page, '/mirror/view/send', 2_500);
      const first = `slow first ${vp.how} ${stamp}`;
      await sendTimed(page, first, vp.how);
      await sendTimed(page, repeat, vp.how);
      await expect(pendingOf(page, first)).toHaveAttribute('data-pending-send', 'sending');
      // The transcript is truncated in place, SHORTER than the host has read
      // (a reset): the host starts over and the page redraws every row — the
      // old one of `repeat` included, as new.
      fs.writeFileSync(tr, prompt('reset') + prompt(repeat));
      await expect
        .poll(() => page.evaluate((t) => {
          const rs = Array.from(document.querySelectorAll('#mv-log [data-bubble="user"]'));
          return rs.some((el) => (el.textContent || '').includes(t)) && !rs.some((el) => (el as unknown as { __old?: boolean }).__old);
        }, repeat), { message: 'the conversation was redrawn: the old row of it is a new row now', timeout: 10_000 })
        .toBe(true);
      expect(pasted(), 'the first is still out').toEqual([]);
      // 🔴 The waiting one is NOT settled by the old row: a message still in
      // line is never a reconcile candidate.
      await expect(pendingOf(page, repeat), 'the waiting message is still drawn as waiting').toHaveAttribute('data-pending-send', 'sending');
      await expect.poll(pasted, { message: 'both are pasted, once each, in order', timeout: 20_000 }).toEqual([`${pane} ${first}`, `${pane} ${repeat}`]);
      await expect(page.locator('#mv-log [data-pending-send]'), 'no bubble is left').toHaveCount(0, { timeout: 10_000 });
      await sleep(1_000);
      expect(pasted(), 'and only once').toEqual([`${pane} ${first}`, `${pane} ${repeat}`]);
    });

    // 🟡 Y3: a refusal holds what came after it. Before, the next message was
    // posted straight after the refusal and delivered FIRST.
    test('a message sent behind one that is refused waits, and goes after it once it is retried or dismissed', async ({ page }) => {
      await openView(page);
      const first = `refused first ${vp.how} ${stamp}`;
      const second = `waits behind ${vp.how} ${stamp}`;
      // The first post of `first` is refused; everything else goes through.
      let refusedOnce = false;
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          const text = new URLSearchParams(r.request().postData() || '').get('text') || '';
          await sleep(800);
          if (text === first && !refusedOnce) {
            refusedOnce = true;
            return r.fulfill({ status: 409, headers: { 'Content-Type': 'text/plain' }, body: 'the host said no' });
          }
          return r.continue().catch(() => {});
        },
      );
      await sendTimed(page, first, vp.how);
      await sendTimed(page, second, vp.how);
      await expect(pendingOf(page, first)).toHaveAttribute('data-pending-send', 'failed', { timeout: 10_000 });
      const w = pendingOf(page, second);
      await expect(w).toHaveAttribute('data-pending-send', 'held');
      await expect(w.getByRole('status')).toHaveText("Waiting — an earlier message wasn't sent");
      await sleep(1_500);
      expect(pasted(), 'the later message is not delivered before the earlier one').toEqual([]);
      if (vp.how === 'tap') {
        await press(pendingOf(page, first).getByRole('button', { name: 'Retry' }), true);
        await expect.poll(pasted, { message: 'the retried one goes first, then the one that waited', timeout: 15_000 }).toEqual([`${pane} ${first}`, `${pane} ${second}`]);
      } else {
        await pendingOf(page, first).getByRole('button', { name: 'Dismiss' }).click();
        await expect.poll(pasted, { message: 'dismissed, the one that waited goes', timeout: 15_000 }).toEqual([`${pane} ${second}`]);
      }
      await expect(page.locator('#mv-log [data-pending-send]'), 'no bubble is left').toHaveCount(0, { timeout: 10_000 });
    });

    // 🟡 Y4: the row is drawn as markdown — a link as its label, a fence as a
    // code block — so its drawn text does not begin with what was typed. The
    // row's raw prompt (data-prompt) does. Before, the "Sent" bubble stayed
    // beside its row for 30 s.
    test('a message with a link and a fenced block is replaced by its row with no frame of both', async ({ page }) => {
      await openView(page);
      await delaySends(page, '/mirror/view/send', 500);
      const tag = `fenced ${vp.how} ${stamp}`;
      const text = `see [the docs](https://example.test/d) first\n\`\`\`\n${tag}\n\`\`\``;
      const rows = await watchRows(page, tag);
      await page.locator('#mv-input').fill(text);
      if (vp.how === 'enter') await page.locator('#mv-input').press('Enter');
      else await press(page.locator('#mv-send'), true);
      await expect.poll(() => read(tr + '.pasted').includes(tag), { message: 'pasted', timeout: 10_000 }).toBe(true);
      await expect(page.locator('#mv-log [data-bubble="user"]', { hasText: tag })).toHaveCount(1, { timeout: 10_000 });
      await expect(page.locator('#mv-log [data-pending-send]'), 'the bubble went with its row').toHaveCount(0);
      const seen = await rows();
      expect(seen.max, 'never the bubble and its row together').toBe(1);
      expect(seen.gap, 'never neither').toBe(false);
    });

    // 🟡 Y1: a message with nothing to compare (an emoji) is not settled by
    // whatever row comes next. Before, an unrelated row arriving while it was
    // still on its way took it for sent, and it was never posted.
    test('an emoji-only message waiting behind a slow one is not settled by an unrelated row, and is pasted once', async ({ page }) => {
      await openView(page);
      await delaySends(page, '/mirror/view/send', 2_000);
      const first = `slow before the emoji ${vp.how} ${stamp}`;
      const emoji = vp.how === 'tap' ? '🛰️🚀' : '🚀🛰️';
      const other = `typed in the terminal ${vp.how} ${stamp}`;
      const rows = await watchRows(page, emoji);
      await sendTimed(page, first, vp.how);
      await sendTimed(page, emoji, vp.how);
      // Someone types into Claude Code itself while the first is out and the
      // emoji waits its turn.
      fs.appendFileSync(tr, prompt(other));
      await expect(page.locator('#mv-log [data-bubble="user"]', { hasText: other })).toHaveCount(1, { timeout: 10_000 });
      await expect(pendingOf(page, emoji), 'the emoji still waits: an unrelated row is not its row').toHaveCount(1);
      await expect.poll(pasted, { message: 'both are pasted, once each, in order', timeout: 15_000 }).toEqual([`${pane} ${first}`, `${pane} ${emoji}`]);
      await expect(page.locator('#mv-log [data-pending-send]'), 'no bubble is left').toHaveCount(0, { timeout: 10_000 });
      await expect(page.locator('#mv-log [data-bubble="user"]', { hasText: emoji })).toHaveCount(1);
      const seen = await rows();
      expect(seen.gap, 'its bubble went with ITS row, not with the unrelated one (no frame with neither)').toBe(false);
      expect(seen.max).toBe(1);
    });

    // 🔴 OPERATOR DECISION (PR #450 round 4): a "Not confirmed" message is
    // NEVER settled by a transcript row — not even one that IS its text. It
    // keeps its text (box and draft) and Send again until the person acts.
    // (This case used to pin the opposite: the row settled it, and the box
    // and the draft let it go.)
    test('a "Not confirmed" message whose row then arrives stays Not confirmed, with Send again, its text in the box and the draft', async ({ page }) => {
      await openView(page);
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          await sleep(300);
          return r.fulfill({ status: 502, headers: { 'Content-Type': 'text/plain', 'X-Muster-Send-Again': '1' }, body: 'it may have been delivered' });
        },
      );
      const text = `not confirmed then landed ${vp.how} ${stamp}`;
      await sendTimed(page, text, vp.how);
      const p = pendingOf(page, text);
      await expect(p).toHaveAttribute('data-pending-send', 'unknown', { timeout: 10_000 });
      await expect(page.locator('#mv-input'), 'its text is back in the box').toHaveValue(text);
      // A row of its text arrives (it may be its own — or not: the page
      // cannot tell).
      fs.appendFileSync(tr, prompt(text));
      await expect(page.locator('#mv-log [data-bubble="user"]', { hasText: text })).toHaveCount(1, { timeout: 10_000 });
      await sleep(1_500);
      await expect(p, 'still Not confirmed: a row never settles it').toHaveAttribute('data-pending-send', 'unknown');
      await expect(p.locator('#mv-send-again'), 'and still offers Send again').toBeVisible();
      await expect(p.getByRole('button', { name: 'Dismiss' })).toBeVisible();
      await expect(page.locator('#mv-input'), 'its text stays in the box').toHaveValue(text);
      expect((await draftsHolding(page, text)).length, 'and in the draft').toBeGreaterThan(0);
      await page.unrouteAll({ behavior: 'ignoreErrors' });
      await page.reload();
      await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 20_000 });
      await expect(page.locator('#mv-input'), 'a reload brings it back as a draft: whether it landed is the person\'s to check').toHaveValue(text);
      expect(pasted(), 'nothing was pasted by the page on its own').toEqual([]);
    });

    // 🔴 The row can arrive BEFORE the "may have been delivered" answer.
    // Round 2 had the answer settle the message at once on that row; by the
    // operator's round-4 decision it stays Not confirmed with Send again
    // until the person acts — here Dismiss, which takes it away and sends
    // nothing.
    test('a "may have been delivered" answer for a message whose row is already here stays Not confirmed with Send again; Dismiss takes it away', async ({ page }) => {
      await openView(page);
      const text = `row before the answer ${vp.how} ${stamp}`;
      let answered = false;
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          // It reaches the server (pasted, its row drawn) ...
          await r.fetch();
          const deadline = Date.now() + 10_000;
          while (Date.now() < deadline && (await page.locator('#mv-log [data-bubble="user"]', { hasText: text }).count()) === 0) await sleep(100);
          // ... and only then is the answer "it may have been delivered".
          answered = true;
          return r.fulfill({ status: 502, headers: { 'Content-Type': 'text/plain', 'X-Muster-Send-Again': '1' }, body: 'it may have been delivered' });
        },
      );
      const rows = await watchRows(page, text);
      await sendTimed(page, text, vp.how);
      await expect.poll(() => answered, { message: 'the answer came after the row', timeout: 15_000 }).toBe(true);
      const p = pendingOf(page, text);
      await expect(p, 'Not confirmed, beside the row').toHaveAttribute('data-pending-send', 'unknown', { timeout: 10_000 });
      await sleep(1_500);
      await expect(p, 'still Not confirmed').toHaveAttribute('data-pending-send', 'unknown');
      await expect(page.locator('#mv-send-again'), 'Send again is offered').toBeVisible();
      await expect(page.locator('#mv-input'), 'its text is back in the box').toHaveValue(text);
      expect(pasted(), 'pasted once').toEqual([`${pane} ${text}`]);
      // The person checks the conversation, sees it landed, and dismisses it.
      await press(p.getByRole('button', { name: 'Dismiss' }), vp.how === 'tap');
      await expect(page.locator('#mv-log [data-pending-send]'), 'Dismiss takes it away').toHaveCount(0);
      await expect(page.locator('#mv-send-again')).toHaveCount(0);
      await expect(rowsOf(page, text)).toHaveCount(1);
      const seen = await rows();
      expect(seen.states, 'it was shown Not confirmed').toContain('unknown');
      await sleep(1_000);
      expect(pasted(), 'Dismiss sends nothing').toEqual([`${pane} ${text}`]);
    });

    // 🟡 PR #450 round 5 (probe R5-A): Dismiss of a Not confirmed message
    // that DID land left its text in the box and the draft, so a reload
    // brought it back (with no Not confirmed marker) and a later plain Send —
    // past the server's 15-minute hold, or after a restart — pasted it twice.
    // Dismiss is the person's "done with it": the box and the draft forget it.
    test('Dismiss of a Not confirmed message that landed forgets its text: box and draft empty, a reload brings nothing back', async ({ page }) => {
      await openView(page);
      const text = `dismissed after it landed ${vp.how} ${stamp}`;
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          // It reaches the server (pasted) — but the page is told it may have
          // been delivered.
          await r.fetch();
          return r.fulfill({ status: 502, headers: { 'Content-Type': 'text/plain', 'X-Muster-Send-Again': '1' }, body: 'it may have been delivered' });
        },
      );
      await sendTimed(page, text, vp.how);
      const p = pendingOf(page, text);
      await expect(p).toHaveAttribute('data-pending-send', 'unknown', { timeout: 10_000 });
      await expect(page.locator('#mv-input'), 'its text is back in the box').toHaveValue(text);
      await expect.poll(pasted, { timeout: 10_000 }).toEqual([`${pane} ${text}`]);
      await press(p.getByRole('button', { name: 'Dismiss' }), vp.how === 'tap');
      await expect(page.locator('#mv-log [data-pending-send]'), 'Dismiss takes it away').toHaveCount(0);
      await expect(page.locator('#mv-input'), 'Dismiss empties the box').toHaveValue('');
      expect(await draftsHolding(page, text), 'and the draft lets it go').toEqual([]);
      await page.unrouteAll({ behavior: 'ignoreErrors' });
      await page.reload();
      await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 20_000 });
      await expect(page.locator('#mv-input'), 'a reload brings nothing back').toHaveValue('');
      expect(await draftsHolding(page, text), 'no draft holds it after the reload').toEqual([]);
      await sleep(1_000);
      expect(pasted(), 'pasted once: nothing can paste it a second time').toEqual([`${pane} ${text}`]);
    });

    // The other half of the round-5 decision: a FAILED message (the server
    // said it did not take it) cannot be a duplicate, so Dismiss leaves its
    // text in the box — the person's words to edit or send.
    test('Dismiss of a failed message keeps its text in the box', async ({ page }) => {
      await openView(page);
      const text = `dismissed after a refusal ${vp.how} ${stamp}`;
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          return r.fulfill({ status: 409, headers: { 'Content-Type': 'text/plain' }, body: 'it is busy' });
        },
      );
      await sendTimed(page, text, vp.how);
      const p = pendingOf(page, text);
      await expect(p).toHaveAttribute('data-pending-send', 'failed', { timeout: 10_000 });
      await press(p.getByRole('button', { name: 'Dismiss' }), vp.how === 'tap');
      await expect(page.locator('#mv-log [data-pending-send]')).toHaveCount(0);
      await expect(page.locator('#mv-input'), 'its text stays in the box').toHaveValue(text);
      expect(pasted(), 'nothing was pasted').toEqual([]);
    });

    // 🟢 Round 2 #3: the draft is saved in pressed order. Before, a refused
    // message back in the box and one held behind it were saved held-first.
    test('a refused message and one held behind it are saved in the order pressed: a reload brings them back refused first', async ({ page }) => {
      await openView(page);
      const first = `pressed first ${vp.how} ${stamp}`;
      const second = `pressed second ${vp.how} ${stamp}`;
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          await sleep(800);
          return r.fulfill({ status: 409, headers: { 'Content-Type': 'text/plain' }, body: 'the host said no' });
        },
      );
      await sendTimed(page, first, vp.how);
      await sendTimed(page, second, vp.how);
      await expect(pendingOf(page, first)).toHaveAttribute('data-pending-send', 'failed', { timeout: 10_000 });
      await expect(pendingOf(page, second)).toHaveAttribute('data-pending-send', 'held');
      await expect(page.locator('#mv-input')).toHaveValue(first);
      await page.unrouteAll({ behavior: 'ignoreErrors' });
      await page.reload();
      await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 20_000 });
      await expect(page.locator('#mv-input'), 'both, in the order they were pressed').toHaveValue(`${first}\n\n${second}`);
      expect(pasted(), 'neither was sent').toEqual([]);
    });

    // 🟢 Round 2 #4: a download link fires beforeunload and the page STAYS.
    // Before, a network failure in the 3 s after it was swallowed: the bubble
    // stayed "Sending…" for good and the message behind it went first.
    test('a network failure just after a download link fired beforeunload (the page stayed) fails the send with Retry and its text, and what came after it waits', async ({ page }) => {
      await openView(page);
      const first = `failed after a download ${vp.how} ${stamp}`;
      const second = `behind the failed one ${vp.how} ${stamp}`;
      let clicked = () => {};
      const downloadClicked = new Promise<void>((r) => (clicked = r));
      let failedOnce = false;
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          const t = new URLSearchParams(r.request().postData() || '').get('text') || '';
          if (t === first && !failedOnce) {
            failedOnce = true;
            await downloadClicked;
            await sleep(200);
            return r.abort('connectionfailed').catch(() => {});
          }
          return r.continue().catch(() => {});
        },
      );
      await page.route(
        (u) => u.pathname === '/e2e-download',
        (r: Route) => r.fulfill({ status: 200, headers: { 'Content-Type': 'application/octet-stream', 'Content-Disposition': 'attachment; filename="e2e.bin"' }, body: 'e2e' }),
      );
      await page.evaluate(() => {
        const a = document.createElement('a');
        a.href = '/e2e-download';
        a.id = 'e2e-dl';
        a.textContent = 'download';
        document.body.appendChild(a);
        const w = window as unknown as { __beforeunload: number };
        w.__beforeunload = 0;
        addEventListener('beforeunload', () => w.__beforeunload++);
      });
      await sendTimed(page, first, vp.how);
      await sendTimed(page, second, vp.how);
      const dl = page.waitForEvent('download');
      await page.evaluate(() => (document.getElementById('e2e-dl') as HTMLAnchorElement).click());
      await dl;
      // The instrument: the link did fire a beforeunload, and the page stayed.
      expect(await page.evaluate(() => (window as unknown as { __beforeunload: number }).__beforeunload), 'the download link fired beforeunload').toBeGreaterThan(0);
      clicked();
      const p = pendingOf(page, first);
      await expect(p, 'the failure is not swallowed').toHaveAttribute('data-pending-send', 'failed', { timeout: 10_000 });
      await expect(p).toContainText('the connection failed');
      await expect(p.getByRole('button', { name: 'Retry' })).toBeVisible();
      await expect(page.locator('#mv-input'), 'its text is kept').toHaveValue(first);
      await expect(pendingOf(page, second), 'the one behind it waits').toHaveAttribute('data-pending-send', 'held');
      await sleep(1_000);
      expect(pasted(), 'nothing went ahead of it').toEqual([]);
      await press(p.getByRole('button', { name: 'Retry' }), vp.how === 'tap');
      await expect.poll(pasted, { message: 'Retry: both go, in order, once each', timeout: 15_000 }).toEqual([`${pane} ${first}`, `${pane} ${second}`]);
      await expect(page.locator('#mv-log [data-pending-send]')).toHaveCount(0, { timeout: 10_000 });
    });

    // The Terminal view, in the view switch's frame: no bubble, so the status
    // line and its buttons carry a refused message.
    async function openTerminal(page: Page) {
      await openView(page);
      await press(page.locator('#view-terminal'), vp.how === 'tap');
      const t = page.frameLocator('iframe[data-muster-view]');
      await expect(t.locator('#mv-input')).toBeEnabled({ timeout: 20_000 });
      return t;
    }

    // 🟢 Before, a later message's press and its OK wiped an earlier one's
    // refusal and its Send again from the status line. A message pressed
    // AFTER the refusal is shown is the person moving on: it goes (it is not
    // held), and the refusal's reason and Send again stay.
    test('in the Terminal view a refusal that may have landed keeps its reason and Send again while a later message is sent past it', async ({ page }) => {
      const t = await openTerminal(page);
      const posted: string[] = [];
      let first = true;
      await page.route(
        (u) => u.pathname === '/mirror/terminal/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          const text = new URLSearchParams(r.request().postData() || '').get('text') || '';
          await sleep(300);
          if (first) {
            first = false;
            return r.fulfill({ status: 502, headers: { 'Content-Type': 'text/plain', 'X-Muster-Send-Again': '1' }, body: 'it may have been delivered' });
          }
          posted.push(text);
          return r.continue().catch(() => {});
        },
      );
      const m1 = `terminal first ${vp.how} ${stamp}`;
      const m2 = `terminal second ${vp.how} ${stamp}`;
      await t.locator('#mv-input').fill(m1);
      await press(t.locator('#mv-send'), vp.how === 'tap');
      await expect(t.locator('#mv-note')).toHaveText('it may have been delivered', { timeout: 10_000 });
      await expect(t.locator('#mv-send-again')).toBeVisible();
      await expect(t.locator('#mv-input'), 'the text is back in the box').toHaveValue(m1);
      await t.locator('#mv-input').fill(m2);
      await press(t.locator('#mv-send'), vp.how === 'tap');
      await expect.poll(() => posted, { message: 'the new message is sent past the refusal', timeout: 10_000 }).toEqual([m2]);
      await sleep(1_000);
      await expect(t.locator('#mv-note'), 'the refusal is still said').toHaveText('it may have been delivered');
      await expect(t.locator('#mv-send-again'), 'and its Send again is still offered').toBeVisible();
      await press(t.locator('#mv-send-again'), vp.how === 'tap');
      await expect.poll(() => posted, { message: 'Send again sends it, once', timeout: 10_000 }).toEqual([m2, m1]);
      await expect(t.locator('#mv-send-again')).toBeHidden();
      await expect(t.locator('#mv-note')).toBeHidden({ timeout: 10_000 });
    });

    // 🔴 Round 3 #1 (probe P3): a "Not confirmed" message that did NOT land,
    // then a later message whose text BEGINS with it. Before, the later
    // message's row settled the earlier one as delivered — bubble, Send again
    // and draft gone — though it was never pasted: silently lost. The later
    // message's answer is held until its row is drawn, so at the batch that
    // draws the row it is still "Sending…" and nothing but the earlier
    // message is a candidate for it.
    test('a "Not confirmed" message is not settled by the row of a later message that begins with its text: it keeps Send again', async ({ page }) => {
      await openView(page);
      const m1 = `yes p3 ${vp.how} ${stamp}`;
      const m3 = `${m1}, go ahead`;
      let refusedOnce = false;
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          const text = new URLSearchParams(r.request().postData() || '').get('text') || '';
          await sleep(300);
          if (text === m1 && !refusedOnce) {
            refusedOnce = true;
            return r.fulfill({ status: 502, headers: { 'Content-Type': 'text/plain', 'X-Muster-Send-Again': '1' }, body: 'p3 may have been delivered' });
          }
          const resp = await r.fetch();
          const deadline = Date.now() + 10_000;
          while (Date.now() < deadline && (await page.locator('#mv-log [data-bubble="user"]', { hasText: text }).count()) === 0) await sleep(100);
          return r.fulfill({ response: resp }).catch(() => {});
        },
      );
      await sendTimed(page, m1, vp.how);
      await expect(pendingOf(page, m1).first()).toHaveAttribute('data-pending-send', 'unknown', { timeout: 10_000 });
      await expect(page.locator('#mv-send-again')).toBeVisible();
      await sendTimed(page, m3, vp.how);
      await expect.poll(pasted, { message: 'the later message is pasted', timeout: 10_000 }).toEqual([`${pane} ${m3}`]);
      await expect(page.locator('#mv-log [data-bubble="user"]', { hasText: m3 })).toHaveCount(1, { timeout: 10_000 });
      await sleep(1_000);
      const u = page.locator('#mv-log [data-pending-send="unknown"]');
      await expect(u, 'the earlier one is still Not confirmed').toHaveCount(1);
      await expect(u).toHaveText(new RegExp('^' + m1.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '\\s*Not confirmed'));
      await expect(u.locator('#mv-send-again'), 'and still offers Send again').toBeVisible();
      await expect(page.locator('#mv-log [data-pending-send]', { hasText: m3 }), 'the later message settles on its row').toHaveCount(0, { timeout: 10_000 });
      expect(pasted(), 'it never landed').toEqual([`${pane} ${m3}`]);
      await press(page.locator('#mv-send-again'), vp.how === 'tap');
      await expect.poll(pasted, { message: 'Send again delivers it, once', timeout: 15_000 }).toEqual([`${pane} ${m3}`, `${pane} ${m1}`]);
      await expect(page.locator('#mv-log [data-pending-send]'), 'both settled').toHaveCount(0, { timeout: 10_000 });
    });

    // ---- Round 4 (probes P6–P10): the operator's decision. A "Not
    // confirmed" message is never settled by a row; only a "Sent" one is
    // matched, and only to take its bubble away. ----

    // answerBy routes the Rendered view's send: `answers` maps a text to what
    // its FIRST post gets ('unknown': 502 "may have been delivered"; 'busy':
    // 409; 'ok': 204 — the page is told it was sent, nothing is pasted).
    // Every other post goes on to the real server. It returns the texts
    // posted, in order.
    async function answerBy(page: Page, answers: Record<string, 'unknown' | 'busy' | 'ok'>) {
      const posted: string[] = [];
      const once = new Set<string>();
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          const text = new URLSearchParams(r.request().postData() || '').get('text') || '';
          posted.push(text);
          await sleep(300);
          const a = once.has(text) ? undefined : answers[text];
          once.add(text);
          if (a === 'unknown') return r.fulfill({ status: 502, headers: { 'Content-Type': 'text/plain', 'X-Muster-Send-Again': '1' }, body: 'it may have been delivered' });
          if (a === 'busy') return r.fulfill({ status: 409, headers: { 'Content-Type': 'text/plain' }, body: 'the host is busy' });
          if (a === 'ok') return r.fulfill({ status: 204 });
          return r.continue().catch(() => {});
        },
      );
      return posted;
    }
    // rowsExactly is the user rows whose raw prompt IS text.
    const rowsExactly = (page: Page, text: string) =>
      page.evaluate((t) => Array.from(document.querySelectorAll('#mv-log [data-bubble="user"]')).filter((e) => (e.getAttribute('data-prompt') ?? e.textContent ?? '').trim() === t).length, text);
    // stillNotConfirmed: u's bubble is Not confirmed, with its text, Send
    // again and Dismiss — nothing took it away.
    async function stillNotConfirmed(page: Page, u: string) {
      const b = page.locator('#mv-log [data-pending-send]').filter({ has: page.locator('div', { hasText: new RegExp('^' + u.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '$') }) });
      await expect(b, `"${u}" is still shown`).toHaveCount(1);
      await expect(b).toHaveAttribute('data-pending-send', 'unknown');
      await expect(b.locator('#mv-send-again'), 'with Send again').toBeVisible();
      await expect(b.getByRole('button', { name: 'Dismiss' })).toBeVisible();
      return b;
    }

    // 🔴 Round 4 #1 (probe P6). S sent OK; then U, of the SAME compared text,
    // came back "may have been delivered" and never landed. At f457028a
    // `owned` kept S off its own row, S went on its 30 s timer, and the next
    // batch gave S's row to U — settled as delivered, box and draft cleared:
    // silently lost.
    test('P6: a Not confirmed message that never landed keeps Send again and its text past a sent message\'s row and timer, though their texts compare equal', async ({ page }) => {
      test.setTimeout(120_000);
      await openView(page);
      const s = `okay p6 ${vp.how} ${stamp}`, u = `OKAY P6! ${vp.how} ${stamp}`;
      await answerBy(page, { [s]: 'ok', [u]: 'unknown' });
      await sendTimed(page, s, vp.how);
      await expect(pendingOf(page, s)).toHaveAttribute('data-pending-send', 'sent', { timeout: 10_000 });
      await sendTimed(page, u, vp.how);
      await expect(pendingOf(page, u)).toHaveAttribute('data-pending-send', 'unknown', { timeout: 10_000 });
      fs.appendFileSync(tr, prompt(s));
      await expect.poll(() => rowsExactly(page, s), { timeout: 10_000 }).toBe(1);
      await expect(pendingOf(page, s), 'the sent one goes with its row').toHaveCount(0, { timeout: 5_000 });
      // Past where S's 30 s timer would have run, and another batch.
      await sleep(31_000);
      const trig = `p6 trigger ${vp.how} ${stamp}`;
      fs.appendFileSync(tr, prompt(trig));
      await expect.poll(() => rowsExactly(page, trig), { timeout: 10_000 }).toBe(1);
      await sleep(1_000);
      await stillNotConfirmed(page, u);
      await expect(page.locator('#mv-input'), 'its text is still in the box').toHaveValue(u);
      expect((await draftsHolding(page, u)).length, 'and in the draft').toBeGreaterThan(0);
    });

    // 🔴 The rule behind it, directly: a SENT match never touches an unknown
    // message. U (pressed first, Not confirmed) and S (sent) compare equal;
    // S's row arrives. S's bubble goes AT ONCE (at f457028a `owned` kept it
    // there for its 30 s timer); U stays, and its Send again delivers it.
    test('a sent message\'s row takes only the sent bubble: an unknown one of an equal text keeps Send again, and Send again delivers it once', async ({ page }) => {
      await openView(page);
      const u = `Ready SV! ${vp.how} ${stamp}`, s = `ready sv ${vp.how} ${stamp}`;
      const posted = await answerBy(page, { [u]: 'unknown', [s]: 'ok' });
      await sendTimed(page, u, vp.how);
      await expect(pendingOf(page, u)).toHaveAttribute('data-pending-send', 'unknown', { timeout: 10_000 });
      await sendTimed(page, s, vp.how);
      await expect(pendingOf(page, s)).toHaveAttribute('data-pending-send', 'sent', { timeout: 10_000 });
      fs.appendFileSync(tr, prompt(s));
      await expect.poll(() => rowsExactly(page, s), { timeout: 10_000 }).toBe(1);
      await expect(pendingOf(page, s), 'the sent bubble goes with its row, not on a timer').toHaveCount(0, { timeout: 5_000 });
      await sleep(1_000);
      const b = await stillNotConfirmed(page, u);
      expect(pasted(), 'nothing was pasted').toEqual([]);
      await press(b.locator('#mv-send-again'), vp.how === 'tap');
      await expect.poll(pasted, { message: 'Send again delivers it, once', timeout: 15_000 }).toEqual([`${pane} ${u}`]);
      expect(posted, 'posted: U, S, then U again').toEqual([u, s, u]);
      await expect(page.locator('#mv-log [data-pending-send]'), 'and it settles on its own row').toHaveCount(0, { timeout: 10_000 });
    });

    // 🔴 Round 4 #2 (probe P7). U "may have been delivered" and did land (its
    // row comes late); Q is refused. U stays Not confirmed with Send again —
    // the intended behaviour now: nothing settles it for the person, and
    // nothing of U or Q is lost.
    test('P7: a Not confirmed message whose row comes late, after a later message was refused, stays Not confirmed with Send again', async ({ page }) => {
      await openView(page);
      const u = `alpha p7 ${vp.how} ${stamp}`, q = `beta p7 ${vp.how} ${stamp}`;
      await answerBy(page, { [u]: 'unknown', [q]: 'busy' });
      await sendTimed(page, u, vp.how);
      await expect(pendingOf(page, u)).toHaveAttribute('data-pending-send', 'unknown', { timeout: 10_000 });
      await sendTimed(page, q, vp.how);
      await expect(pendingOf(page, q)).toHaveAttribute('data-pending-send', 'failed', { timeout: 10_000 });
      fs.appendFileSync(tr, prompt(u));
      await expect.poll(() => rowsExactly(page, u), { timeout: 10_000 }).toBe(1);
      await sleep(2_000);
      await stillNotConfirmed(page, u);
      await expect(pendingOf(page, q).getByRole('button', { name: 'Retry' }), 'the refused one keeps Retry').toBeVisible();
      await expect(page.locator('#mv-input'), 'the refused one\'s text is in the box').toHaveValue(q);
    });

    // 🔴 Round 4 #3 (probe P8). Nothing to compare (emoji only): U unknown,
    // Q sent; both rows arrive. Q's bubble goes (matched by position); U
    // stays Not confirmed with Send again.
    test('P8: an emoji-only Not confirmed message stays Not confirmed with Send again when its row and a sent emoji\'s row arrive', async ({ page }) => {
      await openView(page);
      const u = vp.how === 'tap' ? '👍' : '👌', q = vp.how === 'tap' ? '🎉🎉🎉' : '🎊🎊🎊';
      await answerBy(page, { [u]: 'unknown', [q]: 'ok' });
      await sendTimed(page, u, vp.how);
      await expect(page.locator('#mv-log [data-pending-send="unknown"]')).toHaveCount(1, { timeout: 10_000 });
      await sendTimed(page, q, vp.how);
      await expect(page.locator('#mv-log [data-pending-send="sent"]')).toHaveCount(1, { timeout: 10_000 });
      const ru = await rowsExactly(page, u), rq = await rowsExactly(page, q);
      fs.appendFileSync(tr, prompt(u) + prompt(q));
      await expect.poll(() => rowsExactly(page, q), { timeout: 10_000 }).toBe(rq + 1);
      await expect.poll(() => rowsExactly(page, u), { timeout: 10_000 }).toBe(ru + 1);
      await expect(page.locator('#mv-log [data-pending-send="sent"]'), 'the sent one goes').toHaveCount(0, { timeout: 5_000 });
      await sleep(1_000);
      await stillNotConfirmed(page, u);
    });

    // 🔴 Round 4 #4 (probe P9). U unknown, landed late; the person retypes it
    // (equal compared text) and sends it as Q. Both rows arrive: Q's bubble
    // goes, U stays Not confirmed with Send again (at f457028a `owned` held Q
    // for its 30 s timer).
    test('P9: a Not confirmed message and a retyped copy that was sent: the copy settles on a row at once, the Not confirmed one keeps Send again', async ({ page }) => {
      await openView(page);
      const u = `Yes go p9 ${vp.how} ${stamp}`, q = `yes, go p9 ${vp.how} ${stamp}`;
      await answerBy(page, { [u]: 'unknown', [q]: 'ok' });
      await sendTimed(page, u, vp.how);
      await expect(pendingOf(page, u)).toHaveAttribute('data-pending-send', 'unknown', { timeout: 10_000 });
      await sendTimed(page, q, vp.how);
      await expect(pendingOf(page, q)).toHaveAttribute('data-pending-send', 'sent', { timeout: 10_000 });
      fs.appendFileSync(tr, prompt(u) + prompt(q));
      await expect.poll(() => rowsExactly(page, q), { timeout: 10_000 }).toBe(1);
      await expect(pendingOf(page, q), 'the sent copy goes at once').toHaveCount(0, { timeout: 5_000 });
      await sleep(1_000);
      await stillNotConfirmed(page, u);
    });

    // 🔴 Round 4 (probe P10). The same text sent twice and confirmed twice,
    // their rows arriving only once BOTH bubbles say "Sent": both go with
    // their rows. (A rule refusing a row that IS another open message's text
    // — f457028a's `owned` — keeps each off its row, and both sit beside
    // their rows for 30 s; mutation-checked. With the real host the first
    // row lands while the second is still "Sending…", which never reaches
    // that shape — so the answers here are the page's, and the rows are
    // written once both are "Sent".)
    test('P10: the same text sent twice and confirmed twice: both bubbles go with their rows', async ({ page }) => {
      await openView(page);
      const m = `same p10 ${vp.how} ${stamp}`;
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          await sleep(300);
          return r.fulfill({ status: 204 });
        },
      );
      await sendTimed(page, m, vp.how);
      await page.locator('#mv-input').fill(m);
      await press(page.locator('#mv-send'), vp.how === 'tap');
      await expect(page.locator('#mv-log [data-pending-send="sent"]', { hasText: m }), 'both are Sent').toHaveCount(2, { timeout: 10_000 });
      fs.appendFileSync(tr, prompt(m) + prompt(m));
      await expect.poll(() => rowsExactly(page, m), { timeout: 10_000 }).toBe(2);
      await expect(page.locator('#mv-log [data-pending-send]'), 'both settled on their rows, not on the 30 s timer').toHaveCount(0, { timeout: 5_000 });
    });

    // 🟢 Round 3 #3 (probe P1): M2 pressed BEFORE M1's refusal waits; M3
    // pressed AFTER it is the person moving on — M2 then M3 go, in order,
    // and M1 keeps its Retry and Dismiss, and goes last when retried.
    test('a message held behind a refusal goes before one sent past the refusal, and the refused one, retried, goes last', async ({ page }) => {
      await openView(page);
      const m1 = `p1 one ${vp.how} ${stamp}`, m2 = `p1 two ${vp.how} ${stamp}`, m3 = `p1 three ${vp.how} ${stamp}`;
      let refusedOnce = false;
      await page.route(
        (u) => u.pathname === '/mirror/view/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          const text = new URLSearchParams(r.request().postData() || '').get('text') || '';
          await sleep(800);
          if (text === m1 && !refusedOnce) {
            refusedOnce = true;
            return r.fulfill({ status: 409, headers: { 'Content-Type': 'text/plain' }, body: 'p1 refusal' });
          }
          return r.continue().catch(() => {});
        },
      );
      await sendTimed(page, m1, vp.how);
      await sendTimed(page, m2, vp.how);
      await expect(pendingOf(page, m1)).toHaveAttribute('data-pending-send', 'failed', { timeout: 10_000 });
      await expect(pendingOf(page, m2)).toHaveAttribute('data-pending-send', 'held');
      await sleep(500);
      expect(pasted()).toEqual([]);
      await expect(page.locator('#mv-input'), 'the refused text is back in the box').toHaveValue(m1);
      await sendTimed(page, m3, vp.how);
      await expect.poll(pasted, { message: 'M2 then M3', timeout: 15_000 }).toEqual([`${pane} ${m2}`, `${pane} ${m3}`]);
      await expect(pendingOf(page, m1), 'M1 still refused').toHaveAttribute('data-pending-send', 'failed');
      await expect(pendingOf(page, m1).getByRole('button', { name: 'Dismiss' })).toBeVisible();
      await press(pendingOf(page, m1).getByRole('button', { name: 'Retry' }), vp.how === 'tap');
      await expect.poll(pasted, { message: 'M1, retried, goes last', timeout: 15_000 }).toEqual([`${pane} ${m2}`, `${pane} ${m3}`, `${pane} ${m1}`]);
      await sleep(1_000);
      expect(pasted(), 'once each').toEqual([`${pane} ${m2}`, `${pane} ${m3}`, `${pane} ${m1}`]);
    });

    // 🟡 Round 3 #2 (probe P2): with no bubble, a refused message's Dismiss
    // stays offered after a later message is sent past it, and the later
    // message's "Sending…" is said — the refusal's reason moves beside its
    // buttons, and the line is the refusal's again once the later one is
    // done. Before, Dismiss was hidden as soon as nothing waited, and the
    // stale refusal line hid "Sending…".
    test('in the Terminal view a refused message keeps Retry and Dismiss past later sends, and their "Sending…" is said', async ({ page }) => {
      const t = await openTerminal(page);
      const posted: string[] = [];
      let first = true;
      await page.route(
        (u) => u.pathname === '/mirror/terminal/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          const text = new URLSearchParams(r.request().postData() || '').get('text') || '';
          await sleep(800);
          if (first) {
            first = false;
            return r.fulfill({ status: 409, headers: { 'Content-Type': 'text/plain' }, body: 'p2 refusal' });
          }
          posted.push(text);
          return r.continue().catch(() => {});
        },
      );
      const m1 = `p2 one ${vp.how} ${stamp}`, m2 = `p2 two ${vp.how} ${stamp}`;
      const dismiss = t.locator('[data-send-offer] button', { hasText: 'Dismiss' });
      const beside = t.locator('[data-send-offer] [data-pending-held]');
      await t.locator('#mv-input').fill(m1);
      await press(t.locator('#mv-send'), vp.how === 'tap');
      await expect(t.locator('#mv-note')).toHaveText('p2 refusal', { timeout: 10_000 });
      await expect(t.locator('#mv-send-retry')).toBeVisible();
      await expect(dismiss, 'Dismiss with nothing waiting').toBeVisible();
      await expect(beside, 'the reason is said once: on the line').toBeHidden();
      await t.locator('#mv-input').fill(m2);
      await press(t.locator('#mv-send'), vp.how === 'tap');
      await expect(t.locator('#mv-note'), 'the later send is said').toHaveText('Sending…');
      await expect(beside, 'the refusal, beside its buttons').toHaveText('Not sent: p2 refusal');
      await expect(dismiss).toBeVisible();
      await expect(t.locator('#mv-send-retry')).toBeVisible();
      await expect.poll(() => posted, { timeout: 10_000 }).toEqual([m2]);
      await expect(t.locator('#mv-note'), 'once it went, the line is the refusal\'s again').toHaveText('p2 refusal', { timeout: 10_000 });
      await expect(beside).toBeHidden();
      await expect(dismiss, 'Dismiss still offered').toBeVisible();
      await press(dismiss, vp.how === 'tap');
      await expect(t.locator('#mv-note')).toBeHidden();
      await expect(t.locator('#mv-send-retry')).toHaveCount(0);
      await expect(t.locator('[data-send-offer]')).toBeHidden();
      await sleep(500);
      expect(posted, 'Dismiss sends nothing').toEqual([m2]);
    });

    // 🟢 Round 3 #3: the no-bubble HELD path — a message pressed before the
    // refusal came waits, said beside the refused one's Retry and Dismiss.
    test('in the Terminal view a message pressed before a refusal waits beside it, and goes after it on Retry', async ({ page }) => {
      const t = await openTerminal(page);
      const posted: string[] = [];
      let first = true;
      await page.route(
        (u) => u.pathname === '/mirror/terminal/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          const text = new URLSearchParams(r.request().postData() || '').get('text') || '';
          await sleep(800);
          if (first) {
            first = false;
            return r.fulfill({ status: 409, headers: { 'Content-Type': 'text/plain' }, body: 'held refusal' });
          }
          posted.push(text);
          return r.continue().catch(() => {});
        },
      );
      const m1 = `held one ${vp.how} ${stamp}`, m2 = `held two ${vp.how} ${stamp}`;
      await t.locator('#mv-input').fill(m1);
      await press(t.locator('#mv-send'), vp.how === 'tap');
      await t.locator('#mv-input').fill(m2);
      await press(t.locator('#mv-send'), vp.how === 'tap');
      await expect(t.locator('#mv-note')).toHaveText('held refusal', { timeout: 10_000 });
      await expect(t.locator('[data-send-offer] [data-pending-held]')).toHaveText("Waiting — an earlier message wasn't sent");
      await expect(t.locator('[data-send-offer] button', { hasText: 'Dismiss' })).toBeVisible();
      await expect(t.locator('#mv-input'), 'the refused text is back').toHaveValue(m1);
      await sleep(1_500);
      expect(posted, 'the later message waits').toEqual([]);
      await press(t.locator('#mv-send-retry'), vp.how === 'tap');
      await expect.poll(() => posted, { message: 'retried first, then the one that waited', timeout: 15_000 }).toEqual([m1, m2]);
      await expect(t.locator('#mv-note')).toBeHidden({ timeout: 10_000 });
      await expect(t.locator('[data-send-offer]')).toBeHidden();
    });

    // 🟢 Round 3 #3: a Queue past a refused Queue. Before, the refusal line
    // swallowed the later "Queuing…" and "Queued.".
    test('a Queue sent past a refused one says "Queuing…" then "Queued.", and the refused one keeps Retry and Dismiss', async ({ page }) => {
      setStatus('busy');
      await sleep(1_500);
      await openView(page);
      let first = true;
      await page.route(
        (u) => u.pathname === '/send-queue',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          await sleep(800);
          if (first) {
            first = false;
            return r.fulfill({ status: 409, headers: { 'Content-Type': 'text/plain' }, body: 'queue refusal' });
          }
          return r.continue().catch(() => {});
        },
      );
      const q1 = `queue refused ${vp.how} ${stamp}`, q2 = `queue past it ${vp.how} ${stamp}`;
      const queue = async (text: string) => {
        await page.locator('#mv-input').fill(text);
        await press(page.locator('#mv-send-more'), vp.how === 'tap');
        await press(page.locator('#mv-queue-send'), vp.how === 'tap');
      };
      await queue(q1);
      await expect(page.locator('#mv-note')).toHaveText('queue refusal', { timeout: 10_000 });
      await queue(q2);
      await expect(page.locator('#mv-note'), 'the later Queue is said').toHaveText('Queuing…');
      await expect(page.locator('#mv-note')).toHaveText('Queued.', { timeout: 10_000 });
      await expect(page.locator('[data-send-offer] [data-pending-held]'), 'the refusal, beside its buttons').toHaveText('Not sent: queue refusal');
      await expect(page.locator('#mv-send-retry')).toBeVisible();
      await expect(page.locator('[data-send-offer] button', { hasText: 'Dismiss' })).toBeVisible();
      await expect(page.locator(`#mv-queue [data-queue-state="queued"]`, { hasText: q2 })).toHaveCount(1, { timeout: 10_000 });
      await expect(page.locator(`#mv-queue [data-queue-state="queued"]`, { hasText: q1 })).toHaveCount(0);
      await press(page.locator('[data-send-offer] button', { hasText: 'Dismiss' }), vp.how === 'tap');
      await expect(page.locator('[data-send-offer]')).toBeHidden();
      // Tidy: Claude goes idle, the queue sends the one that was queued.
      setStatus('idle');
      await expect.poll(pasted, { timeout: 20_000 }).toEqual([`${pane} ${q2}`]);
    });

    // 🟢 An older top window's ask takes no cancel callback. Before, closing
    // its dialog left the framed composer — and everything behind it —
    // waiting for good, "Confirm it's you to send".
    test('a step-up asked through an older top window that never reports a cancel does not stall the send: it fails, with the text back', async ({ page }) => {
      const t = await openTerminal(page);
      // The top window as an older page: its ask takes (why, done) only.
      await page.evaluate(() => {
        const s = (window as unknown as { musterStepUp: { ask: (...a: unknown[]) => void } }).musterStepUp;
        const orig = s.ask;
        s.ask = function (why: unknown, cb: unknown) { return orig.call(s, why, cb); };
      });
      const posted: string[] = [];
      const refused = new Set<string>();
      await page.route(
        (u) => u.pathname === '/mirror/terminal/send',
        async (r: Route) => {
          if (r.request().method() !== 'POST') return r.continue();
          const q = new URLSearchParams(r.request().postData() || '');
          const n = q.get('nonce') || '';
          if (!refused.has(n) && refused.size === 0) {
            refused.add(n);
            return r.fulfill({ status: 403, headers: { 'X-Muster-Step-Up': '1', 'Content-Type': 'text/plain' }, body: 'confirm it is you' });
          }
          posted.push(q.get('text') || '');
          return r.continue().catch(() => {});
        },
      );
      const text = `older top window ${vp.how} ${stamp}`;
      await t.locator('#mv-input').fill(text);
      await press(t.locator('#mv-send'), vp.how === 'tap');
      await expect(page.locator('#term-confirm'), 'asked in the top window').toBeVisible();
      await page.locator('#term-confirm-cancel').click();

      await expect(t.locator('#mv-note'), 'the send failed, not stalled').toContainText("you did not confirm it's you", { timeout: 5_000 });
      await expect(t.locator('#mv-input'), 'its text is back').toHaveValue(text);
      // And the box is not stuck: the same text sent again goes.
      await press(t.locator('#mv-send'), vp.how === 'tap');
      await expect.poll(() => posted, { timeout: 10_000 }).toEqual([text]);
    });
  });
}

// 🟢 Round 2 #5: back from the back/forward cache after a browser submit
// (native) that LANDED. Before, the restored page still had the sent message
// in its box (native put it there for the submit) and the queue simply ran
// again: one Send and it went a second time. Now the page settles that
// message by the draft — the page the submit landed on took it out — before
// anything resumes.
//
// 🔴 IN lib/bfcache.ts's BROWSER: the suite's own cannot restore a page at all
// (see there), so a goBack in it is always the reload path. The request is
// failed IN THE PAGE, not with page.route: routing does not see a restored
// document's requests (bfcache-restore.spec.ts, slowNet).
test.describe('back/forward cache', () => {
  test('back from the cache after a browser submit that landed: the box does not hold the sent message, and it is sent once', async ({ playwright }) => {
    const browser = await launchWithBfcache(playwright.chromium);
    try {
      const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
      await ctx.addInitScript(() => {
        const w = window as unknown as { __doc: string; __restored: number };
        w.__doc = Math.random().toString(36).slice(2);
        w.__restored = 0;
        addEventListener('pageshow', (e) => { if (e.persisted) w.__restored++; });
        const F = window.fetch;
        window.fetch = function (input: RequestInfo | URL, init?: RequestInit) {
          let fail = false;
          try { fail = sessionStorage.getItem('__e2eFailSend') === '1' && String(input).includes('/mirror/view/send'); } catch (e) {}
          return fail ? Promise.reject(new TypeError('Failed to fetch')) : F.call(window, input, init);
        } as typeof window.fetch;
      });
      const page = await ctx.newPage();
      const why: string[] = [];
      const cdp = await ctx.newCDPSession(page);
      await cdp.send('Page.enable');
      cdp.on('Page.backForwardCacheNotUsed', (e: { notRestoredExplanations?: { reason: string }[] }) => {
        why.push(JSON.stringify((e.notRestoredExplanations || []).map((x) => x.reason)));
      });
      await openView(page);
      const doc = await page.evaluate(() => (window as unknown as { __doc: string }).__doc);
      const text = `sent by the browser then back ${stamp}`;
      // The script's request fails: the browser submits the form itself.
      await page.evaluate(() => sessionStorage.setItem('__e2eFailSend', '1'));
      await page.locator('#mv-input').fill(text);
      await page.locator('#mv-input').press('Enter');
      await expect.poll(pasted, { message: 'the browser\'s own submit pastes it', timeout: 15_000 }).toEqual([`${pane} ${text}`]);
      await expect.poll(() => page.evaluate(() => (window as unknown as { __doc: string }).__doc), { message: 'the page it landed on', timeout: 15_000 }).not.toBe(doc);
      await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 20_000 });
      await expect(page.locator('#mv-input'), 'the page it landed on has no draft of it').toHaveValue('');
      await page.evaluate(() => sessionStorage.removeItem('__e2eFailSend'));
      await page.goBack({ waitUntil: 'commit', timeout: 10_000 }).catch((e: Error) => {
        if (!/ERR_ABORTED|frame was detached|Timeout/.test(e.message)) throw e;
      });
      await expect
        .poll(() => page.evaluate(() => { const w = window as unknown as { __doc: string; __restored: number }; return { doc: w.__doc, restored: w.__restored }; }).catch(() => null), {
          message: `Back restored the page that sent it (not restored because: ${why.join(' ')})`,
          timeout: 10_000,
        })
        .toEqual({ doc, restored: 1 });
      await expect(page.locator('#mv-input'), 'the restored box does not hold the message that was sent').toHaveValue('');
      await expect(page.locator('#mv-log [data-pending-send]'), 'and no bubble of it').toHaveCount(0);
      expect(await draftsHolding(page, text), 'nor any draft').toEqual([]);
      await sleep(1_500);
      expect(pasted(), 'nothing was sent again by itself').toEqual([`${pane} ${text}`]);
      // A press of Send now sends what the person types — not the old message.
      const next = `after coming back ${stamp}`;
      await page.locator('#mv-input').fill(next);
      await page.locator('#mv-input').press('Enter');
      await expect.poll(pasted, { timeout: 15_000 }).toEqual([`${pane} ${text}`, `${pane} ${next}`]);
      await ctx.close();
    } finally {
      await browser.close();
    }
  });
});
