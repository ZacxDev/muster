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
    // refusal and its Send again from the status line.
    test('in the Terminal view a refusal that may have landed keeps its reason and Send again while a later message waits behind it', async ({ page }) => {
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
      await expect(t.locator('[data-pending-held]')).toHaveText("Waiting — an earlier message wasn't sent");
      await sleep(1_000);
      await expect(t.locator('#mv-note'), 'the refusal is still said').toHaveText('it may have been delivered');
      await expect(t.locator('#mv-send-again'), 'and its Send again is still offered').toBeVisible();
      expect(posted, 'the later message waits').toEqual([]);
      await press(t.locator('#mv-send-again'), vp.how === 'tap');
      await expect.poll(() => posted, { message: 'Send again goes first, then the one that waited', timeout: 10_000 }).toEqual([m1, m2]);
      await expect(t.locator('#mv-send-again')).toBeHidden();
      await expect(t.locator('#mv-note')).toBeHidden({ timeout: 10_000 });
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
