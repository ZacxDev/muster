import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { Page, request as pwRequest } from '@playwright/test';
import { test, expect, expectAgentsDestroyed } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { authState, baseURL, binDir, dictationPort, dsn, fakeSttPort, hookToken, repoRoot, sttToken, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { writeFileAtomic } from '../lib/atomic';
import { openDispatchSheet } from '../lib/dispatch';

// DICTATION: the microphone on every message box, in a real Chromium with its
// FAKE microphone (--use-fake-device-for-media-stream, which records a tone),
// against a REAL muster-server whose MUSTER_STT_* point at a FAKE
// speech-to-text service played by this file. The fake checks what the real
// one would (the bearer, a multipart `file` part named for its container) and
// answers a fixed transcript.
//
// The four message boxes: a mirrored session's Rendered view (a Claude Code
// window) and Terminal view (a shell window) — a REAL muster-mirror over the
// tunnel, tmux played by its `-tags e2emirror` fake — and a running agent's
// chat and Terminal views.
//
// ⚠ WHAT THIS CANNOT SHOW: a real microphone, iOS Safari's audio/mp4
// recorder, Android's keyboard, or the real service's transcription. The live
// check against the real service is in the pull request, not here.
//
// Each box test runs at a phone's size (390×844, touch) and at a desktop's.

const origin = `http://localhost:${dictationPort}`;
const FAKE_TEXT = 'hello from the fake mic';
const host = 'e2e-dict-' + Date.now().toString(36);
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dictation-'));
const windowsFile = path.join(dir, 'windows');
const pasteFile = path.join(dir, 'pasted');
const transcript = path.join(dir, 'cccccccc-0001.jsonl');
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';

// --- the fake speech-to-text service -----------------------------------------
interface SttCall {
  auth: string;
  ctype: string;
  filename: string;
  bytes: number;
}
const sttCalls: SttCall[] = [];
let sttMode: 'ok' | 'down' = 'ok';
const stt = http.createServer((req, res) => {
  const chunks: Buffer[] = [];
  req.on('data', (c) => chunks.push(c));
  req.on('end', () => {
    const body = Buffer.concat(chunks);
    const m = /filename="([^"]+)"/.exec(body.toString('latin1'));
    sttCalls.push({ auth: req.headers.authorization ?? '', ctype: req.headers['content-type'] ?? '', filename: m?.[1] ?? '', bytes: body.length });
    if (req.url !== '/v1/audio/transcriptions' || req.method !== 'POST') {
      res.writeHead(404).end();
      return;
    }
    if (req.headers.authorization !== 'Bearer ' + sttToken) {
      res.writeHead(401, { 'Content-Type': 'application/json' }).end('{"detail":"invalid or missing bearer token"}');
      return;
    }
    if (sttMode === 'down') {
      res.writeHead(503, { 'Content-Type': 'application/json' }).end('{"detail":"model not loaded yet"}');
      return;
    }
    res.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify({ text: ` ${FAKE_TEXT} `, duration_s: 1.0, elapsed_s: 0.1 }));
  });
});

// --- the mirrored host: one session, a Claude Code window and a shell ---------
interface Win {
  session: string;
  id: string;
  index: number;
  active: boolean;
  command: string;
  activity: number;
  claude?: string;
}
const now = Math.floor(Date.now() / 1000);
const claudeWin: Win = { session: 'dict', id: '@1', index: 1, active: true, command: 'claude', activity: now - 60, claude: transcript };
const shellWin: Win = { session: 'dict', id: '@2', index: 2, active: false, command: 'zsh', activity: now - 600 };
const q = (w: Win) => `host=${encodeURIComponent(host)}&session=${encodeURIComponent(w.session)}&window=${encodeURIComponent(w.id)}`;
const read = (f: string) => (fs.existsSync(f) ? fs.readFileSync(f, 'utf8') : '');

test.use({
  storageState: { cookies: [], origins: [] },
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] },
  permissions: ['microphone'],
});

test.beforeAll(async () => {
  await new Promise<void>((r) => stt.listen(fakeSttPort, '127.0.0.1', () => r()));
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, dictationPort, {
    MUSTER_MIRROR: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-dictation-alpha',
    MUSTER_AGENT_GATEWAY: 'hooks-sha256',
    MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
    MUSTER_AGENT_RUNTIME_CONFIG: '{}',
    MUSTER_AGENT_RUNTIME_INSTALL: 'true',
    MUSTER_STT_URL: `http://127.0.0.1:${fakeSttPort}`,
    MUSTER_STT_TOKEN: sttToken,
  });
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  fs.writeFileSync(transcript, JSON.stringify({ type: 'user', promptId: 'p-1', message: { role: 'user', content: 'the dictation window' } }) + '\n');
  writeFileAtomic(windowsFile, [claudeWin, shellWin].map((w) => [w.session, w.id, w.index, w.active ? 1 : 0, w.command, w.activity, w.claude ?? ''].join('|')).join('\n') + '\n');
  mirrorProc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile,
      MUSTER_MIRROR_E2E_PASTE_FILE: pasteFile,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  mirrorProc.stdout?.on('data', (d) => (mirrorLog += d));
  mirrorProc.stderr?.on('data', (d) => (mirrorLog += d));
});

// 🔴 THIS SPEC SHARES ITS DATABASE WITH EVERY OTHER: the agent it makes is
// deleted again, and the deletion is checked (agent-composer's rule).
let agent: AgentRow | undefined;
test.afterAll(async ({ browser }) => {
  try {
    if (agent) {
      const ctx = await browser.newContext();
      const page = await ctx.newPage();
      await signIn(page, origin);
      const r = await page.request.delete(`${origin}/agents/${agent.id}`, { headers: { Origin: origin } });
      expect(r.status(), `deleting ${agent.name}: ${await r.text()}`).toBeLessThan(300);
      await expectAgentsDestroyed(agentRows, [agent.id], 'the agent this spec made is still there');
      await ctx.close();
    }
  } finally {
    if (mirrorProc && mirrorProc.exitCode === null) {
      const done = new Promise((r) => mirrorProc!.once('exit', r));
      mirrorProc.kill('SIGTERM');
      await done;
    }
    await server?.stop();
    await new Promise((r) => stt.close(r));
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

interface AgentRow {
  id: number;
  name: string;
  status: string;
}

async function agentRows(): Promise<AgentRow[]> {
  const ctx = await pwRequest.newContext({ baseURL: origin, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
  const r = await ctx.get('/api/agents');
  expect(r.status(), await r.text()).toBe(200);
  const rows = (await r.json()) as AgentRow[];
  await ctx.dispose();
  return rows;
}

// runningAgent is one claude-code agent, saved and started once for the spec.
async function runningAgent(page: Page): Promise<AgentRow> {
  if (agent) return agent;
  const before = new Set((await agentRows()).map((a) => a.name));
  await page.goto(origin + '/tasks');
  await openDispatchSheet(page);
  const sheet = page.locator('#agent-modal form');
  await expect(sheet.locator('[data-kind-picker]')).toBeVisible();
  await sheet.locator('[data-kind-option="claude-code"]').click();
  await sheet.locator('textarea[name="note_text"]').fill('');
  const resp = page.waitForResponse((r) => r.url().endsWith('/agents') && r.request().method() === 'POST');
  await sheet.locator('button[value="save"]').click();
  expect((await resp).status()).toBe(200);
  const created = (await agentRows()).filter((a) => !before.has(a.name));
  expect(created.length, 'exactly one agent created').toBe(1);
  // The sheet's answer sends the page to the new agent's own page.
  await page.waitForURL((u) => u.pathname === '/agents/' + created[0].name);
  agent = created[0];
  const started = await page.request.post(`${origin}/agents/${agent.id}/start`, { headers: { Origin: origin } });
  expect(started.status(), await started.text()).toBe(200);
  // The status promoter ticks every 10 s.
  await expect
    .poll(async () => (await agentRows()).find((a) => a.name === agent!.name)?.status, { timeout: 45_000 })
    .toBe('running');
  return agent;
}

async function noHorizontalScroll(page: Page) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'document scrollWidth').toBeLessThanOrEqual(cw);
}

interface Box {
  input: string;
  mic: string;
  note: string;
  send: string;
}
const mvBox: Box = { input: '#mv-input', mic: '#mv-dictate', note: '#mv-dictate-note', send: '#mv-send' };
const chatBox: Box = { input: '#chat-input', mic: '#chat-dictate', note: '#chat-dictate-note', send: '#chat-send' };

// dictate types `before|after` with the caret at the bar, records about a
// second, stops, and checks the transcript landed AT THE CARET with the caret
// after it and the box focused. Returns the box's final value.
async function dictate(page: Page, box: Box, viewportWidth: number): Promise<string> {
  const input = page.locator(box.input);
  const mic = page.locator(box.mic);
  await expect(mic, 'the script revealed the mic').toBeVisible();
  // A 44px target in the input row, on screen, and the page does not scroll sideways.
  const mb = (await mic.boundingBox())!;
  const ib = (await input.boundingBox())!;
  expect(mb.height, 'a 44px target').toBeGreaterThanOrEqual(44);
  expect(mb.width, 'a 44px target').toBeGreaterThanOrEqual(44);
  expect(mb.x + mb.width, 'the mic fits the viewport').toBeLessThanOrEqual(viewportWidth);
  expect(Math.abs(mb.y + mb.height - (ib.y + ib.height)), 'the mic sits on the input row').toBeLessThanOrEqual(2);
  await noHorizontalScroll(page);

  await input.fill('please now');
  await input.evaluate((el: HTMLTextAreaElement) => el.setSelectionRange(7, 7)); // "please |now"
  const calls = sttCalls.length;
  const bg = () => mic.evaluate((el) => getComputedStyle(el).backgroundColor);
  const idleBg = await bg();
  const posted = page.waitForResponse((r) => new URL(r.url()).pathname === '/transcribe');
  await mic.click();
  await expect(mic).toHaveAttribute('data-state', 'recording');
  // Visibly recording: the danger colour, and the stop glyph in place of the mic.
  // (Polled: the colour change is a CSS transition.)
  await expect.poll(bg, { message: 'the recording mic is coloured differently' }).not.toBe(idleBg);
  await expect(mic.locator('svg').nth(0)).toBeHidden();
  await expect(mic.locator('svg').nth(1)).toBeVisible();
  await expect(mic).toHaveAttribute('aria-label', 'Stop recording');
  // Pressing the mic did not take focus from the box (the caret, and a phone's
  // keyboard, stay where they were).
  await expect(input).toBeFocused();
  await expect(page.locator(box.note)).toContainText('Recording 0:0');
  await expect(page.locator(box.note)).toContainText('/ 2:00');
  // The row did not move when recording began (the note grows the composer
  // UPWARD; the input row keeps its place at the bottom).
  const ib2 = (await input.boundingBox())!;
  expect(Math.abs(ib2.y + ib2.height - (ib.y + ib.height)), 'the input row stayed put').toBeLessThanOrEqual(1);
  await page.waitForTimeout(1300);
  await mic.click();
  const resp = await posted;
  expect(resp.status(), 'the transcription answered').toBe(200);
  await expect(input).toHaveValue(`please ${FAKE_TEXT} now`);
  await expect(mic).toHaveAttribute('data-state', 'idle');
  await expect(page.locator(box.note)).toBeHidden();
  await expect(input).toBeFocused();
  await expect.poll(bg, { message: 'the mic is back to its resting colour' }).toBe(idleBg);
  const caret = await input.evaluate((el: HTMLTextAreaElement) => [el.selectionStart, el.selectionEnd]);
  const at = `please ${FAKE_TEXT} `.length;
  expect(caret, 'the caret is after the inserted text').toEqual([at, at]);
  // What the fake service saw: one call, muster's bearer, a webm file part.
  expect(sttCalls.length - calls, 'one transcription per recording').toBe(1);
  const call = sttCalls[sttCalls.length - 1];
  expect(call.auth, 'the service got muster’s token (the browser never had it)').toBe('Bearer ' + sttToken);
  expect(call.filename).toBe('recording.webm');
  expect(call.bytes, 'a recording, not an empty file').toBeGreaterThan(1000);
  return input.inputValue();
}

for (const vp of [
  { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('a mirrored session’s Rendered view: dictate at the caret, then Send delivers it', async ({ page }) => {
      await signIn(page, origin);
      fs.rmSync(transcript + '.pasted', { force: true });
      await page.goto(origin + '/mirror/view?' + q(claudeWin));
      await expect(page.locator('#mv-input'), `the composer (mirror log:\n${mirrorLog})`).toBeEnabled({ timeout: 20_000 });
      const text = await dictate(page, mvBox, vp.use.viewport.width);
      // The page never sent it: nothing pasted until Send.
      expect(read(transcript + '.pasted'), 'dictation sent nothing by itself').toBe('');
      await page.locator('#mv-send').click();
      await expect(page.locator('#mv-input')).toHaveValue('');
      await expect.poll(() => read(transcript + '.pasted'), { message: `pasted (log:\n${mirrorLog})` }).toBe(`%e2e-dict-@1 ${text}\n`);
    });

    test('a mirrored session’s Terminal view: dictate at the caret, then Send delivers it', async ({ page }) => {
      await signIn(page, origin);
      fs.rmSync(pasteFile, { force: true });
      await page.goto(origin + '/mirror/terminal?' + q(shellWin));
      await expect(page.locator('#mv-input'), `the composer (mirror log:\n${mirrorLog})`).toBeEnabled({ timeout: 20_000 });
      const text = await dictate(page, mvBox, vp.use.viewport.width);
      expect(read(pasteFile), 'dictation sent nothing by itself').toBe('');
      await page.locator('#mv-send').click();
      await expect(page.locator('#mv-input')).toHaveValue('');
      await expect.poll(() => read(pasteFile), { message: `pasted (log:\n${mirrorLog})` }).toBe(`%e2e-dict-@2 ${text}\n`);
    });

    test('an agent’s chat and Terminal views: dictate at the caret, then Send delivers it', async ({ page }) => {
      test.setTimeout(150_000);
      await signIn(page, origin);
      const a = await runningAgent(page);

      await page.goto(`${origin}/agents/${a.name}/terminal`);
      await expect(page.locator('#mv-input')).toBeEnabled();
      const termText = await dictate(page, mvBox, vp.use.viewport.width);
      const sends: number[] = [];
      page.on('response', (r) => {
        if (new URL(r.url()).pathname === `/agents/${a.name}/send`) sends.push(r.status());
      });
      await page.locator('#mv-send').click();
      await expect(page.locator('#mv-input')).toHaveValue('');
      expect(sends, 'the dictated text was sent, once').toEqual([204]);

      await page.goto(`${origin}/agents/${a.name}`);
      const log = page.locator('#chat-log');
      await expect(log.locator('[data-bubble="user"]').last()).toContainText(termText, { timeout: 30_000 });
      const chatText = await dictate(page, chatBox, vp.use.viewport.width);
      await page.locator('#chat-send').click();
      await expect(page.locator('#chat-input')).toHaveValue('');
      await expect(log.locator('[data-bubble="user"]').last()).toContainText(chatText, { timeout: 30_000 });
    });
  });
}

// 🔴 THE RENDERED|TERMINAL SWITCH DOES NOT UNLOAD THE VIEW IT HIDES
// (internal/ui/view_swap.go): each view is its own document, with its own
// dictation script, so pagehide never fires there. The switch tells the hidden
// view's window 'muster:view-hidden' — the TOP page when the frame is revealed,
// the FRAME when the top page is shown again. A recording in that view is
// FINISHED: its microphone tracks are stopped, and what it recorded is
// transcribed once into THAT view's own box (still there, only off screen),
// which is what the view shows when it comes back. Both directions are driven,
// since a different document receives the event in each, on a mirrored
// window's two views and on a claude-code agent's chat and terminal.
test.describe('the view switch finishes the recording in the view it hides', () => {
  test.use({ viewport: { width: 1280, height: 800 } });

  // Every document (top and frame) records the streams getUserMedia hands it,
  // so the test can read each track's readyState — the browser's own answer to
  // "is this stream still holding the microphone".
  async function tracks(f: { evaluate: Page['evaluate'] }): Promise<string[]> {
    return f.evaluate(() =>
      ((window as unknown as { __streams?: MediaStream[] }).__streams ?? []).flatMap((s) => s.getTracks().map((t) => t.readyState)),
    );
  }

  for (const c of [
    { name: 'a mirrored window: Rendered ↔ Terminal', agent: false, top: mvBox },
    { name: 'a claude-code agent: chat ↔ Terminal', agent: true, top: chatBox },
  ]) {
    test(`${c.name}: the hidden view's mic stops, releases its tracks, and transcribes once into its own box`, async ({ page }) => {
      test.setTimeout(150_000);
      await page.addInitScript(() => {
        const w = window as unknown as { __streams: MediaStream[] };
        w.__streams = [];
        const gum = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
        navigator.mediaDevices.getUserMedia = (cs) =>
          gum(cs).then((s) => {
            w.__streams.push(s);
            return s;
          });
      });
      // 🔴 /transcribe IS ANSWERED HERE, not by the server: the route allows
      // twelve recordings per user per minute (internal/api/transcribe.go), and
      // the rest of this file spends ten of them, as the one signed-in user,
      // inside the same minute; four more here run the service-down test into
      // a 429 instead of its 503.
      // The real route (and the fake service behind it) is what every other
      // test in this file drives; what this one checks is the page — which
      // document posts, how often, and where the text lands. Each POST is
      // counted here, from either document (page.route sees frames too), with
      // the recording it carried.
      let posts = 0;
      const sizes: number[] = [];
      await page.route('**/transcribe', async (r) => {
        if (r.request().method() !== 'POST') return r.continue();
        posts++;
        sizes.push(r.request().postDataBuffer()?.length ?? 0);
        await r.fulfill({ status: 200, contentType: 'text/plain; charset=utf-8', body: ` ${FAKE_TEXT} ` });
      });
      await signIn(page, origin);
      if (c.agent) {
        const a = await runningAgent(page);
        await page.goto(`${origin}/agents/${a.name}`);
      } else {
        await page.goto(origin + '/mirror/view?' + q(claudeWin));
      }
      const top = { input: page.locator(c.top.input), mic: page.locator(c.top.mic), note: page.locator(c.top.note) };
      await expect(top.mic).toBeVisible({ timeout: 20_000 });
      const iframe = page.locator('iframe[data-muster-view]');
      const fr = page.frameLocator('iframe[data-muster-view]');
      const sub = { input: fr.locator(mvBox.input), mic: fr.locator(mvBox.mic), note: fr.locator(mvBox.note) };

      // recordThenHide records about a second in one view, flips the switch
      // (from the view on screen), and checks the hidden view finished it.
      async function recordThenHide(
        v: typeof top,
        doc: () => { evaluate: Page['evaluate'] },
        flip: () => Promise<void>,
        what: string,
      ) {
        await v.input.fill('');
        const p0 = posts;
        await v.mic.click();
        await expect(v.mic).toHaveAttribute('data-state', 'recording');
        await expect(v.note).toContainText('Recording 0:01');
        // Positive control: the instrument sees a held microphone as `live`.
        expect(await tracks(doc()), `${what}: the recording holds a live track`).toEqual(['live']);
        await flip();
        await expect.poll(() => tracks(doc()), { message: `${what}: the hidden view released its microphone` }).toEqual(['ended']);
        await expect(v.mic, `${what}: the hidden view is not recording`).not.toHaveAttribute('data-state', 'recording');
        await expect(v.input, `${what}: the transcript is in the hidden view's own box`).toHaveValue(FAKE_TEXT, { timeout: 15_000 });
        await expect(v.mic).toHaveAttribute('data-state', 'idle');
        await expect(v.note).toBeHidden();
        expect(posts - p0, `${what}: exactly one /transcribe POST for the recording`).toBe(1);
        expect(sizes[sizes.length - 1], `${what}: the POST carried the recording, not an empty body`).toBeGreaterThan(1000);
      }

      // 1. The TOP view records; the switch reveals the frame over it.
      await recordThenHide(
        top,
        () => page,
        async () => {
          await page.locator('a[data-view-swap]:visible').first().click();
          await expect(sub.mic).toBeVisible({ timeout: 30_000 });
          await expect(iframe).toHaveCSS('visibility', 'visible');
        },
        'top',
      );
      // 2. The FRAME records; the switch hides it and shows the top view again,
      // whose box still has its transcript and whose mic is at rest.
      const frame = () => page.frames().find((f) => f !== page.mainFrame())!;
      await recordThenHide(
        sub,
        frame,
        async () => {
          await fr.locator('a[data-view-swap]:visible').first().click();
          await expect(iframe).toHaveCSS('visibility', 'hidden');
        },
        'frame',
      );
      await expect(top.input).toHaveValue(FAKE_TEXT);
      await expect(top.mic).toHaveAttribute('data-state', 'idle');
      await expect(top.note).toBeHidden();
      // 3. The frame shown again: its box has its transcript, its mic at rest.
      await page.locator('a[data-view-swap]:visible').first().click();
      await expect(iframe).toHaveCSS('visibility', 'visible');
      await expect(sub.input).toHaveValue(FAKE_TEXT);
      await expect(sub.mic).toHaveAttribute('data-state', 'idle');
      await expect(sub.note).toBeHidden();
    });
  }
});

test.describe('failures, said inline', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  async function openRendered(page: Page) {
    await signIn(page, origin);
    await page.goto(origin + '/mirror/view?' + q(claudeWin));
    await expect(page.locator('#mv-dictate')).toBeVisible({ timeout: 20_000 });
  }

  // `inApp` plays the Android app's WebView: the app injects window.musterApp
  // (ui.AppBridgeName), and there a refusal is the app's Android permission, so
  // the note points at Android's settings rather than the browser's.
  for (const [name, err, inApp, want] of [
    ['permission denied', 'NotAllowedError', false, "Microphone access was blocked. Allow it for this site in the browser's settings, then tap the mic again."],
    [
      'permission denied inside the Android app',
      'NotAllowedError',
      true,
      "Microphone access is off for the muster app. Allow it in Android's Settings for muster, under Permissions, then Microphone, then tap the mic again. If Microphone isn't listed, update the muster app.",
    ],
    ['no microphone', 'NotFoundError', false, 'No microphone was found.'],
  ] as const) {
    test(`${name}: the reason is shown and nothing is posted`, async ({ page }) => {
      // The browser's own refusal, as getUserMedia reports it.
      await page.addInitScript(
        ([name, inApp]) => {
          navigator.mediaDevices.getUserMedia = () => Promise.reject(new DOMException('e2e', name));
          if (inApp) (window as unknown as { musterApp: { postMessage(m: string): void } }).musterApp = { postMessage: () => {} };
        },
        [err, inApp] as const,
      );
      await openRendered(page);
      const calls = sttCalls.length;
      await page.locator('#mv-input').fill('kept');
      await page.locator('#mv-dictate').click();
      await expect(page.locator('#mv-dictate-note')).toHaveText(want);
      await expect(page.locator('#mv-dictate')).toHaveAttribute('data-state', 'idle');
      await expect(page.locator('#mv-input')).toHaveValue('kept');
      expect(sttCalls.length, 'nothing reached the service').toBe(calls);
    });
  }

  test('the service is down: muster says so and the box is untouched', async ({ page }) => {
    await openRendered(page);
    sttMode = 'down';
    try {
      await page.locator('#mv-input').fill('kept');
      await page.locator('#mv-dictate').click();
      await expect(page.locator('#mv-dictate')).toHaveAttribute('data-state', 'recording');
      await page.waitForTimeout(800);
      const posted = page.waitForResponse((r) => new URL(r.url()).pathname === '/transcribe');
      await page.locator('#mv-dictate').click();
      expect((await posted).status()).toBe(503);
      await expect(page.locator('#mv-dictate-note')).toHaveText(
        'The transcription service did not answer, so nothing was transcribed. Try again in a moment.',
      );
      await expect(page.locator('#mv-input')).toHaveValue('kept');
      await expect(page.locator('#mv-dictate')).toHaveAttribute('data-state', 'idle');
    } finally {
      sttMode = 'ok';
    }
  });

  test('a recording stops by itself at its cap and is transcribed', async ({ page }) => {
    await openRendered(page);
    // The cap is the server's (two minutes); shortened here on the element the
    // script reads it from, so the test does not wait two minutes.
    await page.locator('#mv-dictate').evaluate((b) => b.setAttribute('data-dictate-max', '2'));
    await page.locator('#mv-input').fill('');
    await page.locator('#mv-dictate').click();
    await expect(page.locator('#mv-dictate-note')).toContainText('/ 0:02');
    await expect(page.locator('#mv-input')).toHaveValue(FAKE_TEXT, { timeout: 15_000 });
  });

  test('with no script there is no microphone, and the box is still a form', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, javaScriptEnabled: false });
    const page = await ctx.newPage();
    try {
      await signIn(page, origin);
      await page.goto(origin + '/mirror/view?' + q(claudeWin));
      await expect(page.locator('#mv-input')).toBeVisible();
      await expect(page.locator('#mv-dictate')).toHaveCount(1);
      await expect(page.locator('#mv-dictate')).toBeHidden();
    } finally {
      await ctx.close();
    }
  });
});

// The shared server has no MUSTER_STT_*: no microphone, and the route is not there.
test('a server with no speech-to-text service draws no microphone', async ({ browser }) => {
  const ctx = await browser.newContext({ storageState: authState, baseURL });
  const page = await ctx.newPage();
  try {
    await page.goto(baseURL + '/agents/docs-sweeper');
    await expect(page.locator('#chat-input')).toBeVisible();
    await expect(page.locator('[data-dictate]')).toHaveCount(0);
    const r = await page.request.post(baseURL + '/transcribe', { data: 'x', headers: { Origin: baseURL } });
    expect(r.status(), 'the route answers 404 with dictation off').toBe(404);
  } finally {
    await ctx.close();
  }
});
