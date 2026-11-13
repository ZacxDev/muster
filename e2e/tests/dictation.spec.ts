import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { Page, request as pwRequest } from '@playwright/test';
import { test, expect, expectAgentsDestroyed } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { authState, baseURL, binDir, dictationPort, dsn, fakeSttPort, hookToken, password, repoRoot, sttToken, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { writeFileAtomic } from '../lib/atomic';
import { revealBox } from '../lib/composer';
import { createAgent } from '../lib/seed';
import { toTerminal } from '../lib/agent-menu';

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
  // Created stopped through POST /api/agents (lib/seed.ts), which replaced the
  // deleted dispatch sheet's "Save for later".
  const made = await createAgent(origin, { kind: 'claude-code' });
  const created = (await agentRows()).filter((a) => !before.has(a.name));
  expect(created.length, 'exactly one agent created').toBe(1);
  expect(created[0].name).toBe(made.name);
  await page.goto(origin + '/agents/' + created[0].name);
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

type BB = { x: number; y: number; width: number; height: number };
// expectMicPlaced: the mic is a 40px target where its box puts it — on the
// Rendered view's button row, BELOW the box's own row (twoRows: the page
// draws #mv-input-row; mirror_compose.go); beside the box, on its row,
// everywhere else (a Terminal view's one-row composer, the agent chat, an ask).
function expectMicPlaced(twoRows: boolean, mb: BB, ib: BB, viewportWidth: number) {
  expect(mb.height, 'a 40px target').toBeGreaterThanOrEqual(40);
  expect(mb.width, 'a 40px target').toBeGreaterThanOrEqual(40);
  expect(mb.x + mb.width, 'the mic fits the viewport').toBeLessThanOrEqual(viewportWidth);
  if (twoRows) expect(mb.y, 'the mic is on the button row, below the box').toBeGreaterThanOrEqual(ib.y + ib.height - 0.5);
  else expect(Math.abs(mb.y + mb.height - (ib.y + ib.height)), 'the mic sits on the box’s row').toBeLessThanOrEqual(2);
}

// dictate types `before|after` with the caret at the bar, records about a
// second, stops, and checks the transcript landed AT THE CARET with the caret
// after it and the box focused. Returns the box's final value.
async function dictate(page: Page, box: Box, viewportWidth: number): Promise<string> {
  const input = page.locator(box.input);
  const mic = page.locator(box.mic);
  await expect(mic, 'the script revealed the mic').toBeVisible();
  // A 40px target where its box puts it, on screen, and the page does not scroll sideways.
  const ib = (await input.boundingBox())!;
  expectMicPlaced((await page.locator('#mv-input-row').count()) > 0, (await mic.boundingBox())!, ib, viewportWidth);
  await noHorizontalScroll(page);

  await input.fill('please now');
  await input.evaluate((el: HTMLTextAreaElement) => el.setSelectionRange(7, 7)); // "please |now"
  const calls = sttCalls.length;
  const bg = () => mic.evaluate((el) => getComputedStyle(el).backgroundColor);
  const idleBg = await bg();
  const posted = page.waitForResponse((r) => new URL(r.url()).pathname === '/transcribe');
  await mic.click();
  await expect(mic).toHaveAttribute('data-state', 'recording');
  // Visibly recording: a raised colour, and the ✓ glyph in place of the mic.
  // (Polled: the colour change is a CSS transition.)
  await expect.poll(bg, { message: 'the recording mic is coloured differently' }).not.toBe(idleBg);
  await expect(mic.locator('svg').nth(0)).toBeHidden();
  await expect(mic.locator('svg[data-dictate-check]')).toBeVisible();
  await expect(mic).toHaveAttribute('aria-label', 'Stop and insert transcript');
  // The waveform has the box's place while recording (dictate.go).
  const wave = page.locator(box.mic + '-wave');
  await expect(wave).toBeVisible();
  await expect(input).toBeHidden();
  await expect(page.locator(box.note)).toContainText('Recording 0:0');
  await expect(page.locator(box.note)).toContainText('/ 2:00');
  // The row did not move when recording began (the note grows the composer
  // UPWARD; the waveform sits where the box's row was, at the bottom).
  const wb = (await wave.boundingBox())!;
  expect(Math.abs(wb.y + wb.height - (ib.y + ib.height)), 'the input row stayed put').toBeLessThanOrEqual(2);
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
      // On a touch screen the box is collapsed behind "Type a message"; this
      // test types into it (the transcript's own reveal has its own test).
      await revealBox(page);
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
      // The box empties at the press (composer_pending.go); the answer follows.
      await expect.poll(() => sends, { message: 'the dictated text was sent, once' }).toEqual([204]);

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
          // A claude-code agent's chat has its Terminal in the ⋯ menu (PR #527).
          if (c.agent) await toTerminal(page);
          else await page.locator('a[data-view-swap]:visible').first().click();
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
      if (c.agent) await toTerminal(page);
      else await page.locator('a[data-view-swap]:visible').first().click();
      await expect(iframe).toHaveCSS('visibility', 'visible');
      await expect(sub.input).toHaveValue(FAKE_TEXT);
      await expect(sub.mic).toHaveAttribute('data-state', 'idle');
      await expect(sub.note).toBeHidden();
    });
  }
});

test.describe('failures, said inline', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  // reveal: open the box (collapsed on a touch screen: lib/composer.ts), as
  // the tests that type into it need; the transcript's own reveal test opens
  // it collapsed.
  async function openRendered(page: Page, reveal = true) {
    await signIn(page, origin);
    await page.goto(origin + '/mirror/view?' + q(claudeWin));
    await expect(page.locator('#mv-dictate')).toBeVisible({ timeout: 20_000 });
    if (reveal) await revealBox(page);
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

  // THE RENDERED VIEW'S BOX IS COLLAPSED ON A TOUCH SCREEN (composer_reveal.go),
  // and the mic's transcript REVEALS it: dictation reaches the hidden box
  // (it is in the DOM: dictate.go's isConnected) and the text is then shown.
  test('on a touch screen the mic’s transcript reveals the collapsed box', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, permissions: ['microphone'] });
    const page = await ctx.newPage();
    try {
      await openRendered(page, false);
      const input = page.locator('#mv-input');
      await expect(input, 'collapsed').toBeHidden();
      await expect(page.locator('#mv-text')).toHaveAttribute('aria-expanded', 'false');
      const posted = page.waitForResponse((r) => new URL(r.url()).pathname === '/transcribe');
      await page.locator('#mv-dictate').tap();
      await expect(page.locator('#mv-dictate')).toHaveAttribute('data-state', 'recording');
      await page.waitForTimeout(1300);
      await page.locator('#mv-dictate').tap();
      expect((await posted).status()).toBe(200);
      await expect(input).toHaveValue(FAKE_TEXT);
      await expect(input, 'the transcript revealed the box').toBeVisible();
      await expect(page.locator('#mv-text')).toHaveAttribute('aria-expanded', 'true');
      await input.fill('');
    } finally {
      await ctx.close();
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

// THE ASKS' ANSWER BOXES (internal/api attention_ask_page.go, attention.go):
// the ask page's pinned box and every ask card on the Asks list carry the same
// mic. Its ✓ FILLS the box at the caret and sends nothing; its ↑ fills it and
// clicks Answer, which is the send either way.
// The asker is a seeded agent (e2e/pluginfix), so each card also links the
// asking agent's page as "View in session".
//
// The list is refreshed by its own script (ui.AttentionLiveScript's innerHTML,
// no htmx), so the two things only a browser can show are driven here: a
// refresh's NEW card has its mic shown (ui.DictateRevealEvent), and a refresh
// that lands while a mic records (the box empty and never focused) is held
// back, so the recording's text still has its box (askBusyFunc).
//
// 🔴 /transcribe IS ANSWERED IN THE PAGE (page.route), for the rate-limit
// reason the view-switch tests give: the route allows twelve recordings per
// user per minute and this file already spends ten. The real route and the
// fake service behind it are what the message-box tests above drive.
test.describe('an ask’s answer boxes', () => {
  const stamp = Date.now().toString(36);
  const asker = 'e2e-dict-asker-' + stamp;
  const fix = (...args: string[]) =>
    execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();
  let made = false;
  test.beforeAll(() => {
    fix('agent', '1', asker, 'e2e-dict-asker-token-not-a-real-one-' + stamp);
    made = true;
  });
  // The agent's asks go with it.
  test.afterAll(() => {
    if (made) fix('cleanup', '1', asker);
  });
  const ask = (q: string) => Number(fix('ask', '1', asker, q));

  async function routeTranscribe(page: Page, body = ` ${FAKE_TEXT} `): Promise<() => number> {
    let n = 0;
    await page.route('**/transcribe', async (r) => {
      if (r.request().method() !== 'POST') return r.continue();
      n++;
      await r.fulfill({ status: 200, contentType: 'text/plain; charset=utf-8', body });
    });
    return () => n;
  }

  // answers counts the answer POSTs the page makes for ask id.
  function answers(page: Page, id: number): () => number {
    let n = 0;
    page.on('request', (r) => {
      if (r.method() === 'POST' && new URL(r.url()).pathname === `/attention/asks/${id}/answer`) n++;
    });
    return () => n;
  }

  // fill is dictate() above for a routed /transcribe: the mic where its box
  // puts it, a 40px target, no sideways scroll; `please |now` → the text at
  // the caret, the box focused, and nothing posted but the recording.
  async function fill(page: Page, box: Box, width: number, posted: () => number, sent: () => number) {
    const input = page.locator(box.input);
    const mic = page.locator(box.mic);
    await expect(mic, 'the script revealed the mic').toBeVisible();
    expectMicPlaced((await page.locator('#mv-input-row').count()) > 0, (await mic.boundingBox())!, (await input.boundingBox())!, width);
    await noHorizontalScroll(page);
    await input.fill('please now');
    await input.evaluate((el: HTMLTextAreaElement) => el.setSelectionRange(7, 7));
    const p0 = posted();
    await mic.click();
    await expect(mic).toHaveAttribute('data-state', 'recording');
    await expect(page.locator(box.mic + '-wave'), 'the waveform has the box’s place').toBeVisible();
    await expect(input).toBeHidden();
    await expect(page.locator(box.note)).toContainText('Recording 0:0');
    await page.waitForTimeout(1300);
    await mic.click();
    await expect(input).toHaveValue(`please ${FAKE_TEXT} now`);
    await expect(mic).toHaveAttribute('data-state', 'idle');
    await expect(page.locator(box.note)).toBeHidden();
    expect(posted() - p0, 'one recording posted').toBe(1);
    expect(sent(), 'dictation answered nothing by itself').toBe(0);
  }

  for (const vp of [
    { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
    { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
  ]) {
    test.describe(vp.name, () => {
      test.use(vp.use);

      test('the ask page: the mic fills the answer box, Answer sends it', async ({ page }) => {
        const id = ask('Which branch, ask page? ' + vp.name);
        const posted = await routeTranscribe(page);
        const sent = answers(page, id);
        await signIn(page, origin);
        await page.goto(`${origin}/attention/asks/${id}`);
        await expect(page.locator('[data-ask-context]'), 'the ask links its asker’s session').toHaveText('View in session');
        await expect(page.locator('[data-ask-context]')).toHaveAttribute('href', `/agents/${asker}`);
        await fill(page, { input: '#ask-input', mic: '#ask-dictate', note: '#ask-dictate-note', send: '[data-ask-send]' }, vp.use.viewport.width, posted, sent);
        await page.locator('[data-ask-send]').click();
        await expect(page.locator('[data-ask-answer]')).toContainText(`please ${FAKE_TEXT} now`);
        expect(sent(), 'answered once').toBe(1);
      });

      test('the Asks list: each card’s own mic, revealed again by a live refresh; Answer sends it', async ({ page }) => {
        const mine = ask('Which branch, list? ' + vp.name);
        const other = ask('Dismissed elsewhere? ' + vp.name);
        const posted = await routeTranscribe(page);
        const sent = answers(page, mine);
        await signIn(page, origin);
        await page.goto(origin + '/attention');
        const card = page.locator(`#ask-${mine}`);
        // The card's head is its session link: the asker's name, no words.
        await expect(card.locator('[data-attention-session]')).toContainText(asker);
        await expect(card.locator('[data-attention-session]')).toHaveAttribute('href', `/agents/${asker}`);
        const mic = page.locator(`#answer-${mine}-dictate`);
        await expect(mic).toBeVisible();
        // Another card's mic is its own control, addressing its own box.
        await expect(page.locator(`#answer-${other}-dictate`)).toHaveAttribute('data-dictate-input', `answer-${other}`);
        // A live refresh (the other ask dismissed elsewhere) REPLACES the list;
        // the new card's mic must be shown, not left hidden.
        await mic.evaluate((el) => ((el as unknown as { __old: boolean }).__old = true));
        const r = await page.request.post(`${origin}/attention/asks/${other}/cancel`, { headers: { Origin: origin }, form: { x: '' } });
        expect(r.status(), await r.text()).toBeLessThan(400);
        await expect(page.locator(`#ask-${other}`), 'the refresh ran').toHaveCount(0);
        expect(await mic.evaluate((el) => !!(el as unknown as { __old?: boolean }).__old), 'the card was redrawn').toBe(false);
        await expect(mic, 'the redrawn card’s mic is shown').toBeVisible();
        await fill(page, { input: `#answer-${mine}`, mic: `#answer-${mine}-dictate`, note: `#answer-${mine}-dictate-note`, send: '' }, vp.use.viewport.width, posted, sent);
        await card.getByRole('button', { name: 'Answer', exact: true }).click();
        await expect.poll(sent, { message: 'answered' }).toBe(1);
        await expect
          .poll(async () => (await page.request.get(`${origin}/attention/asks/${mine}`)).text())
          .toContain(`please ${FAKE_TEXT} now`);
      });
    });
  }

  // THE BOX BEHIND THE OPTIONS (the Asks page's design pass): an ask that
  // offers options shows them first; its box and mic are shut behind "Type an
  // answer", which opens them in place — focused, the mic shown — and sends
  // nothing; Answer still sends what was typed.
  test('the Asks list: an ask\u2019s options come first, its box and mic behind Type an answer', async ({ page }) => {
    const id = Number(fix('ask', '1', asker, 'Which region?', 'eu-west', 'us-east'));
    const sent = answers(page, id);
    await signIn(page, origin);
    await page.goto(origin + '/attention');
    const card = page.locator(`#ask-${id}`);
    await expect(card.locator('[data-attention-option]')).toHaveCount(2);
    const box = page.locator(`#answer-${id}`);
    const mic = page.locator(`#answer-${id}-dictate`);
    const toggle = card.getByText('Type an answer', { exact: true });
    await expect(box, 'the box is shut behind the options').toBeHidden();
    await expect(mic).toBeHidden();
    await toggle.click();
    await expect(box).toBeVisible();
    await expect(box).toBeFocused();
    await expect(mic, 'the opened box has its mic').toBeVisible();
    await expect(toggle).toBeHidden();
    await noHorizontalScroll(page);
    expect(sent(), 'opening the box sent nothing').toBe(0);
    await box.fill('ap-south');
    await card.getByRole('button', { name: 'Answer', exact: true }).click();
    await expect.poll(sent, { message: 'answered' }).toBe(1);
    await expect.poll(async () => (await page.request.get(`${origin}/attention/asks/${id}`)).text()).toContain('ap-south');
  });

  // dismissElsewhere dismisses ask id from outside the page and resolves once
  // the page has HEARD the change (asks:changed is dispatched before the
  // refresh's busy check), so what follows is the refresh's decision.
  async function dismissElsewhere(page: Page, id: number) {
    const heard = page.evaluate(() => new Promise<void>((res) => document.addEventListener('asks:changed', () => res(), { once: true })));
    const r = await page.request.post(`${origin}/attention/asks/${id}/cancel`, { headers: { Origin: origin }, form: { x: '' } });
    expect(r.status(), await r.text()).toBeLessThan(400);
    await heard;
    await page.waitForTimeout(500);
  }

  // 🔴 THE PERMISSION PROMPT IS PART OF THE RECORDING. While the browser asks
  // for the microphone (held here: getUserMedia answers only when the test
  // says), a refresh must hold back too; otherwise it swapped the card, the
  // grant then started a recording for a button no longer on the page, and
  // its text was thrown away.
  test('a refresh waits while a card’s mic is still asking for the microphone', async ({ page }) => {
    await page.addInitScript(() => {
      const w = window as unknown as { __gumGate: Promise<void>; __gumOpen: () => void };
      const gum = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
      w.__gumGate = new Promise<void>((r) => (w.__gumOpen = r));
      navigator.mediaDevices.getUserMedia = (cs) => w.__gumGate.then(() => gum(cs));
    });
    const mine = ask('Held while the mic opens?');
    const other = ask('Dismissed while the mic opens?');
    await routeTranscribe(page);
    await signIn(page, origin);
    await page.goto(origin + '/attention');
    const mic = page.locator(`#answer-${mine}-dictate`);
    const input = page.locator(`#answer-${mine}`);
    await expect(mic).toBeVisible();
    await mic.click();
    await dismissElsewhere(page, other);
    await expect(page.locator(`#ask-${other}`), 'the list was not swapped while the microphone was being opened').toHaveCount(1);
    await page.evaluate(() => (window as unknown as { __gumOpen: () => void }).__gumOpen());
    await expect(mic).toHaveAttribute('data-state', 'recording');
    await page.waitForTimeout(1300);
    await mic.click();
    await expect(input, 'the recording landed in its box').toHaveValue(FAKE_TEXT);
  });

  // A refresh a recording held back runs when the mic is idle again — here a
  // recording that heard nothing, so no box holds it and nothing moves focus
  // (the refresh used to wait for a focusout that never came).
  test('a refresh a recording held back runs once the mic is idle, nothing heard', async ({ page }) => {
    const mine = ask('Nothing heard?');
    const other = ask('Dismissed during the silent recording?');
    await routeTranscribe(page, ' ');
    await signIn(page, origin);
    await page.goto(origin + '/attention');
    const mic = page.locator(`#answer-${mine}-dictate`);
    await expect(mic).toBeVisible();
    await mic.click();
    await expect(mic).toHaveAttribute('data-state', 'recording');
    await dismissElsewhere(page, other);
    await expect(page.locator(`#ask-${other}`), 'held back while recording').toHaveCount(1);
    await mic.click();
    await expect(page.locator(`#ask-${other}`), 'the held-back refresh ran once the mic was idle').toHaveCount(0, { timeout: 5000 });
    await expect(page.locator(`#answer-${mine}`)).toHaveValue('');
  });

  test('a refresh waits while a card’s mic records into its empty box', async ({ page }) => {
    const mine = ask('Held while recording?');
    const other = ask('Dismissed mid-recording?');
    await routeTranscribe(page);
    await signIn(page, origin);
    await page.goto(origin + '/attention');
    const mic = page.locator(`#answer-${mine}-dictate`);
    const input = page.locator(`#answer-${mine}`);
    await expect(mic).toBeVisible();
    await expect(input).not.toBeFocused();
    await mic.click();
    await expect(mic).toHaveAttribute('data-state', 'recording');
    // The queue changes while recording: the page hears it (asks:changed is
    // dispatched before the busy check) but must not swap the list.
    const heard = page.evaluate(() => new Promise<void>((res) => document.addEventListener('asks:changed', () => res(), { once: true })));
    const r = await page.request.post(`${origin}/attention/asks/${other}/cancel`, { headers: { Origin: origin }, form: { x: '' } });
    expect(r.status(), await r.text()).toBeLessThan(400);
    await heard;
    await page.waitForTimeout(500);
    await expect(page.locator(`#ask-${other}`), 'the list was not swapped mid-recording').toHaveCount(1);
    await mic.click();
    await expect(input, 'the recording landed in its box').toHaveValue(FAKE_TEXT);
  });
  // 🔴 THE ↑ SENDS ONCE (operator decision 2026-11-12): a double tap on the ↑
  // arms it once (it is disabled from the first tap), and a REPEAT TAP on the
  // spot just after the send — where Answer is back by then — is swallowed
  // for DictateSendHoldMs. The ask page's Answer is a native form post (no
  // script of its own empties the box), so a second tap that got through
  // WOULD post the answer twice: the answer post is held open here (routed,
  // released after the second tap) so the page is still there to be tapped.
  for (const vp of [
    { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
    { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
  ]) {
    test(`${vp.name}: the ask page's ↑ answers once — a double tap and a repeat tap send nothing more`, async ({ browser }) => {
      const ctx = await browser.newContext({ ...vp.use, permissions: ['microphone'] });
      const page = await ctx.newPage();
      try {
        const id = ask('Voice answer once? ' + vp.name);
        await routeTranscribe(page);
        const sent = answers(page, id);
        let release!: () => void;
        const gate = new Promise<void>((r) => (release = r));
        await page.route(`**/attention/asks/${id}/answer`, async (r) => {
          await gate;
          await r.continue();
        });
        await signIn(page, origin);
        await page.goto(`${origin}/attention/asks/${id}`);
        const input = page.locator('#ask-input');
        const mic = page.locator('#ask-dictate');
        const up = page.locator('#ask-dictate-send');
        const answer = page.locator('#ask-send');
        await expect(mic).toBeVisible();
        await expect(up, 'the ↑ is hidden until a recording').toBeHidden();
        const ab = (await answer.boundingBox())!;
        await mic.click();
        await expect(mic).toHaveAttribute('data-state', 'recording');
        await expect(up).toBeVisible();
        await expect(up).toHaveAttribute('aria-label', 'Transcribe and send');
        await expect(answer, 'the ↑ has Answer’s place').toBeHidden();
        await page.waitForTimeout(1000);
        const spot = (await up.boundingBox())!;
        // Control: the ↑'s centre is on Answer's own box, so a tap there once
        // Answer is back lands on Answer.
        const cx = spot.x + spot.width / 2, cy = spot.y + spot.height / 2;
        expect(cx >= ab.x && cx <= ab.x + ab.width && cy >= ab.y && cy <= ab.y + ab.height, `the ↑ ${JSON.stringify(spot)} sits on Answer ${JSON.stringify(ab)}`).toBe(true);
        const posted = page.waitForRequest((r) => r.method() === 'POST' && new URL(r.url()).pathname === `/attention/asks/${id}/answer`);
        // The double tap: two presses on the ↑, back to back.
        await page.mouse.click(cx, cy);
        await page.mouse.click(cx, cy);
        await posted;
        const at = Date.now();
        expect(sent(), 'the ↑ answered').toBe(1);
        // The repeat tap, on the same spot: Answer is back there (both are
        // the row's last control, right-aligned, and Answer is the wider).
        // Raw mouse input: the page is mid-navigation (the post is held), and
        // a locator would wait for that navigation to finish.
        await page.mouse.click(cx, cy);
        // Inside the hold (DictateSendHoldMs, 1500 ms): the tap the guard is for.
        expect(Date.now() - at, 'the repeat tap came within the hold').toBeLessThan(1000);
        await page.waitForTimeout(600);
        expect(sent(), 'one answer post for one ↑, double tap and repeat tap included').toBe(1);
        release();
        await expect(page.locator('[data-ask-answer]')).toContainText(FAKE_TEXT);
        expect(sent(), 'still one').toBe(1);
      } finally {
        await ctx.close();
      }
    });
  }
});

// THE RECORDER (operator decision 2026-11-12, internal/ui/dictate.go): while
// the mic records, a LIVE WAVEFORM of the microphone takes the box's place,
// with the elapsed time and a × that discards; the mic is a ✓ (stop and
// insert, no send) and Send's place is a ↑ (transcribe and send, through the
// box's own Send). Every way a recording ends lets go of the microphone's
// tracks, the waveform's animation frame and its AudioContext.
//
// The page is instrumented from its first script (instrument): every stream
// getUserMedia hands out, every AudioContext made, every MediaRecorder, and
// every animation frame whose callback is the waveform's (dictateFrame) —
// the browser's own answers to "is the microphone held", "is the audio graph
// open", "is the animation still running". Each is read live first (the
// positive control) before it is read as released.
//
// /transcribe is answered in the page (page.route), for the rate-limit reason
// the view-switch tests give; what these tests check is the page.
type Instr = { live: number; streams: number; open: number; acs: number; frames: number };
async function instrument(page: Page) {
  await page.addInitScript(() => {
    const w = window as unknown as Record<string, unknown> & { __streams: MediaStream[]; __acs: AudioContext[]; __recs: MediaRecorder[]; __frames: number };
    w.__streams = [];
    w.__acs = [];
    w.__recs = [];
    w.__frames = 0;
    const gum = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
    navigator.mediaDevices.getUserMedia = (cs) =>
      gum(cs).then((s) => {
        w.__streams.push(s);
        return s;
      });
    const AC = window.AudioContext;
    w.AudioContext = class extends AC {
      constructor(o?: AudioContextOptions) {
        super(o);
        w.__acs.push(this);
      }
    };
    const MR = window.MediaRecorder;
    w.MediaRecorder = class extends MR {
      constructor(s: MediaStream, o?: MediaRecorderOptions) {
        super(s, o);
        w.__recs.push(this);
      }
    };
    const raf = window.requestAnimationFrame.bind(window);
    window.requestAnimationFrame = (cb: FrameRequestCallback) => {
      const ours = cb.name === 'dictateFrame';
      return raf((t) => {
        if (ours) w.__frames++;
        cb(t);
      });
    };
  });
}
async function instr(page: Page): Promise<Instr> {
  return page.evaluate(() => {
    const w = window as unknown as { __streams: MediaStream[]; __acs: AudioContext[]; __frames: number };
    return {
      live: w.__streams.flatMap((s) => s.getTracks()).filter((t) => t.readyState === 'live').length,
      streams: w.__streams.length,
      open: w.__acs.filter((a) => a.state !== 'closed').length,
      acs: w.__acs.length,
      frames: w.__frames,
    };
  });
}
// expectHeld: the positive control — a recording holds one live track, one
// open AudioContext, and its frames run.
async function expectHeld(page: Page, what: string) {
  const a = await instr(page);
  expect(a.live, `${what}: the recording holds a live track`).toBe(1);
  expect(a.open, `${what}: the waveform's AudioContext is open`).toBe(1);
  await expect.poll(async () => (await instr(page)).frames, { message: `${what}: the waveform's frames run` }).toBeGreaterThan(a.frames + 3);
}
// expectReleased: no live track, every AudioContext closed, and no frame of
// the waveform's runs any more.
async function expectReleased(page: Page, what: string) {
  await expect.poll(async () => (await instr(page)).live, { message: `${what}: no microphone track is live`, timeout: 10_000 }).toBe(0);
  await expect.poll(async () => (await instr(page)).open, { message: `${what}: every AudioContext is closed`, timeout: 10_000 }).toBe(0);
  const f0 = (await instr(page)).frames;
  await page.waitForTimeout(400);
  expect((await instr(page)).frames, `${what}: no waveform frame ran after the end`).toBe(f0);
}
// canvasFrames samples the waveform's canvas n times, every ms.
async function canvasFrames(page: Page, sel: string, n: number, ms: number): Promise<string[]> {
  const out: string[] = [];
  for (let i = 0; i < n; i++) {
    out.push(await page.locator(sel).evaluate((c: HTMLCanvasElement) => c.toDataURL()));
    await page.waitForTimeout(ms);
  }
  return out;
}
async function inked(page: Page, sel: string): Promise<number> {
  return page.locator(sel).evaluate((c: HTMLCanvasElement) => {
    const d = c.getContext('2d')!.getImageData(0, 0, c.width, c.height).data;
    let n = 0;
    for (let i = 3; i < d.length; i += 4) if (d[i] > 0) n++;
    return n;
  });
}

test.describe('the recorder: a live waveform, ✓ inserts, ↑ sends, × discards', () => {
  // posts counts /transcribe POSTs; body is what it answers (status 200
  // unless status says otherwise).
  async function routeStt(page: Page, o: { body?: string; status?: number } = {}): Promise<() => number> {
    let n = 0;
    await page.route('**/transcribe', async (r) => {
      if (r.request().method() !== 'POST') return r.continue();
      n++;
      await r.fulfill({ status: o.status ?? 200, contentType: 'text/plain; charset=utf-8', body: o.body ?? ` ${FAKE_TEXT} ` });
    });
    return () => n;
  }
  // mirrorSends counts the Rendered view's send posts.
  function mirrorSends(page: Page): () => number {
    let n = 0;
    page.on('request', (r) => {
      if (r.method() === 'POST' && new URL(r.url()).pathname === '/mirror/view/send') n++;
    });
    return () => n;
  }
  async function openRendered(page: Page) {
    await signIn(page, origin);
    fs.rmSync(transcript + '.pasted', { force: true });
    await page.goto(origin + '/mirror/view?' + q(claudeWin));
    await expect(page.locator('#mv-dictate'), `the composer (mirror log:\n${mirrorLog})`).toBeVisible({ timeout: 20_000 });
    await revealBox(page);
  }
  const ui = (page: Page) => ({
    input: page.locator('#mv-input'),
    mic: page.locator('#mv-dictate'),
    wave: page.locator('#mv-dictate-wave'),
    canvas: '#mv-dictate-wave canvas',
    clock: page.locator('#mv-dictate-wave [data-dictate-clock]'),
    discard: page.locator('#mv-dictate-wave [data-dictate-discard]'),
    up: page.locator('#mv-dictate-send'),
    send: page.locator('#mv-send'),
    note: page.locator('#mv-dictate-note'),
  });
  async function expectIdle(page: Page, what: string) {
    const u = ui(page);
    await expect(u.mic, `${what}: the mic is idle`).toHaveAttribute('data-state', 'idle');
    await expect(u.wave, `${what}: the waveform is gone`).toBeHidden();
    await expect(u.input, `${what}: the box is back`).toBeVisible();
    await expect(u.up, `${what}: the ↑ is gone`).toBeHidden();
    await expect(u.send, `${what}: Send is back`).toBeVisible();
    await expect(u.send).toHaveAttribute('aria-label', 'Send');
  }

  for (const vp of [
    { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
    { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
  ]) {
    test.describe(vp.name, () => {
      test.use(vp.use);

      test('the waveform takes the box’s place and moves with the microphone; × throws the recording away', async ({ page }) => {
        await instrument(page);
        const posts = await routeStt(page);
        const sends = mirrorSends(page);
        await openRendered(page);
        const u = ui(page);
        await expect(u.wave, 'no waveform before a recording').toBeHidden();
        await expect(u.up, 'no ↑ before a recording').toBeHidden();
        await u.input.fill('kept words');
        await u.mic.click();
        await expect(u.mic).toHaveAttribute('data-state', 'recording');
        await expect(u.mic).toHaveAttribute('aria-label', 'Stop and insert transcript');
        await expect(u.wave).toBeVisible();
        await expect(u.input, 'the waveform is in the box’s place').toBeHidden();
        await expect(u.discard).toHaveAttribute('aria-label', 'Discard recording');
        await expect(u.up, 'the ↑ is in Send’s place').toBeVisible();
        await expect(u.up).toHaveAttribute('aria-label', 'Transcribe and send');
        await expect(u.send).toBeHidden();
        await expect(u.wave).toHaveAttribute('data-motion', 'full');
        // The waveform fits the screen and the page does not scroll sideways.
        const wb = (await u.wave.boundingBox())!;
        expect(wb.x + wb.width).toBeLessThanOrEqual(vp.use.viewport.width);
        await noHorizontalScroll(page);
        await expectHeld(page, 'recording');
        // It is DRAWN, and it MOVES: the canvas has ink, and its pixels change
        // from frame to frame (the fake microphone's tone comes and goes).
        await expect.poll(() => inked(page, u.canvas), { message: 'the waveform is drawn' }).toBeGreaterThan(0);
        const shots = await canvasFrames(page, u.canvas, 8, 200);
        expect(new Set(shots).size, 'the waveform changes over frames').toBeGreaterThanOrEqual(3);
        await expect(u.clock).toHaveText(/^0:0[1-9]$/);
        await u.discard.click();
        await expectIdle(page, '×');
        await expect(u.input, '× left the box as it was').toHaveValue('kept words');
        await expect(u.note).toBeHidden();
        await expectReleased(page, '×');
        await page.waitForTimeout(300);
        expect(posts(), '× posted nothing to /transcribe').toBe(0);
        expect(sends(), '× sent nothing').toBe(0);
      });

      test('✓ inserts the transcript at the caret and sends nothing', async ({ page }) => {
        await instrument(page);
        const posts = await routeStt(page);
        const sends = mirrorSends(page);
        await openRendered(page);
        const u = ui(page);
        await u.input.fill('please now');
        await u.input.evaluate((el: HTMLTextAreaElement) => el.setSelectionRange(7, 7));
        await u.mic.click();
        await expect(u.mic).toHaveAttribute('data-state', 'recording');
        await expectHeld(page, '✓');
        await page.waitForTimeout(800);
        await u.mic.click();
        await expect(u.input).toHaveValue(`please ${FAKE_TEXT} now`);
        await expectIdle(page, '✓');
        await expect(u.input).toBeFocused();
        await expectReleased(page, '✓');
        expect(posts(), 'one recording posted').toBe(1);
        await page.waitForTimeout(500);
        expect(sends(), '✓ sent nothing').toBe(0);
        expect(read(transcript + '.pasted'), 'nothing pasted').toBe('');
      });

      // ↑ goes through the box's OWN Send, so a mirrored send that needs a
      // fresh sign-in still asks. ⚠ The stale refusal is PLAYED (one
      // intercepted response, shaped as the server's: 403 + X-Muster-Step-Up),
      // as mirrored.spec.ts does, because a password-mode session cannot be
      // aged in a browser test; the dialog, the confirm route and the retried
      // send are real, and so is the paste.
      test('↑ transcribes and sends once through Send — and the confirm-it’s-you step-up still asks', async ({ page }) => {
        await instrument(page);
        const posts = await routeStt(page);
        const sends = mirrorSends(page);
        let refused = 0;
        await page.route('**/mirror/view/send?*', async (route) => {
          if (refused++ === 0) {
            await route.fulfill({ status: 403, headers: { 'X-Muster-Step-Up': 'required', 'Content-Type': 'text/plain' }, body: "confirm it's you" });
            return;
          }
          await route.continue();
        });
        await openRendered(page);
        const u = ui(page);
        await u.input.fill('');
        await u.mic.click();
        await expect(u.mic).toHaveAttribute('data-state', 'recording');
        await expectHeld(page, '↑');
        await page.waitForTimeout(800);
        await u.up.click();
        const dialog = page.locator('#term-confirm');
        await expect(dialog, 'the send asked for the step-up').toBeVisible();
        expect(read(transcript + '.pasted'), 'nothing is pasted while the confirm is open').toBe('');
        await expectReleased(page, '↑');
        await page.fill('#term-confirm-password', password);
        await page.click('#term-confirm-submit');
        await expect(dialog).toBeHidden();
        await expect.poll(() => read(transcript + '.pasted'), { message: `pasted (log:\n${mirrorLog})` }).toBe(`%e2e-dict-@1 ${FAKE_TEXT}\n`);
        await expectIdle(page, '↑');
        expect(posts(), 'one recording posted').toBe(1);
        expect(sends(), 'one send, refused once for the step-up and retried once').toBe(2);
        await page.waitForTimeout(800);
        expect(read(transcript + '.pasted'), 'pasted once').toBe(`%e2e-dict-@1 ${FAKE_TEXT}\n`);
        await page.unroute('**/mirror/view/send?*');
      });

      // 🔴 NOTHING IS SENT UNLESS A TRANSCRIPT WAS INSERTED. The box holds text
      // here, so a send that slipped through would be seen (an empty box sends
      // nothing anyway).
      for (const c of [
        { name: 'nothing heard', stt: { body: ' ' }, note: 'Nothing was heard in that recording. Try again, closer to the mic.' },
        { name: 'the service failed', stt: { status: 503, body: 'The transcription service did not answer, so nothing was transcribed. Try again in a moment.' }, note: 'The transcription service did not answer, so nothing was transcribed. Try again in a moment.' },
      ]) {
        test(`↑ with ${c.name}: nothing is sent, the reason is shown, the box is as it was`, async ({ page }) => {
          await instrument(page);
          const posts = await routeStt(page, c.stt);
          const sends = mirrorSends(page);
          await openRendered(page);
          const u = ui(page);
          await u.input.fill('kept');
          await u.mic.click();
          await expect(u.mic).toHaveAttribute('data-state', 'recording');
          await page.waitForTimeout(800);
          await u.up.click();
          await expect.poll(posts).toBe(1);
          await expect(u.mic).toHaveAttribute('data-state', 'idle');
          await page.waitForTimeout(600);
          // The targeted assertion first: a send that leaked would also empty
          // the box, and this must be what reports it.
          expect(sends(), `${c.name}: nothing was sent`).toBe(0);
          expect(read(transcript + '.pasted'), 'nothing pasted').toBe('');
          await expectIdle(page, c.name);
          await expect(u.note).toBeVisible();
          await expect(u.note).toHaveText(c.note);
          await expect(u.input).toHaveValue('kept');
          await expectReleased(page, c.name);
        });
      }

      // EVERY WAY A RECORDING ENDS lets go of the microphone, the audio graph
      // and the animation: ✓, ↑, ×, a recorder error, the length cap, the page
      // going (pagehide), and the view switch hiding the view.
      test('every way a recording ends releases the microphone, the AudioContext and the animation frame', async ({ page }) => {
        test.setTimeout(120_000);
        await instrument(page);
        await routeStt(page);
        await openRendered(page);
        const u = ui(page);
        const exits: { name: string; end: () => Promise<void> }[] = [
          { name: '✓', end: () => u.mic.click() },
          { name: '↑', end: () => u.up.click() },
          { name: '×', end: () => u.discard.click() },
          { name: 'a recorder error', end: () => page.evaluate(() => { const r = (window as unknown as { __recs: MediaRecorder[] }).__recs; r[r.length - 1].dispatchEvent(new Event('error')); }) },
          { name: 'the length cap', end: () => page.waitForTimeout(2500) },
          { name: 'pagehide', end: () => page.evaluate(() => window.dispatchEvent(new Event('pagehide'))) },
          { name: 'the view switch hiding the view', end: () => page.evaluate(() => window.dispatchEvent(new Event('muster:view-hidden'))) },
        ];
        for (const x of exits) {
          await u.input.fill('');
          if (x.name === 'the length cap') await u.mic.evaluate((b) => b.setAttribute('data-dictate-max', '2'));
          else await u.mic.evaluate((b) => b.setAttribute('data-dictate-max', '120'));
          await u.mic.click();
          await expect(u.mic, x.name).toHaveAttribute('data-state', 'recording');
          await expectHeld(page, x.name);
          await x.end();
          await expectReleased(page, x.name);
          await expectIdle(page, x.name);
          if (x.name === 'a recorder error') await expect(u.note).toHaveText('The recording stopped with an error, so nothing was transcribed. Try again.');
        }
      });

      test('under prefers-reduced-motion the waveform is a calm level meter', async ({ page }) => {
        await page.emulateMedia({ reducedMotion: 'reduce' });
        await instrument(page);
        await routeStt(page);
        await openRendered(page);
        const u = ui(page);
        await u.mic.click();
        await expect(u.wave).toHaveAttribute('data-motion', 'reduced');
        await expect.poll(() => inked(page, u.canvas), { message: 'the meter is drawn' }).toBeGreaterThan(0);
        // Calm: at most 5 redraws a second (a full waveform redraws 20).
        const a = await instr(page);
        await page.waitForTimeout(1000);
        const shots = await canvasFrames(page, u.canvas, 5, 50);
        expect(new Set(shots).size, 'no more than two distinct meter frames in 250 ms').toBeLessThanOrEqual(3);
        await u.discard.click();
        await expectReleased(page, 'reduced');
        expect(a.frames).toBeGreaterThan(0);
      });

      // Y1: the ↑ follows its Send's disabled state LIVE (an agent's turn
      // disables #chat-send and enables it again; here the Rendered view's
      // Send is disabled and enabled by hand, the same attribute). And a
      // transcript that comes back while Send is disabled stays in the box,
      // unsent, saying why.
      test('the ↑ follows Send’s disabled state live; a Send disabled during the transcription leaves the text in the box, unsent', async ({ page }) => {
        await instrument(page);
        let release!: () => void;
        const gate = new Promise<void>((r) => (release = r));
        let posts = 0;
        await page.route('**/transcribe', async (r) => {
          if (r.request().method() !== 'POST') return r.continue();
          posts++;
          await gate;
          await r.fulfill({ status: 200, contentType: 'text/plain; charset=utf-8', body: ` ${FAKE_TEXT} ` });
        });
        const sends = mirrorSends(page);
        await openRendered(page);
        const u = ui(page);
        await u.input.fill('');
        await u.mic.click();
        await expect(u.mic).toHaveAttribute('data-state', 'recording');
        await expect(u.up).toBeEnabled();
        await u.send.evaluate((b: HTMLButtonElement) => (b.disabled = true));
        await expect(u.up, 'Send disabled mid-recording disables the ↑').toBeDisabled();
        await u.send.evaluate((b: HTMLButtonElement) => (b.disabled = false));
        await expect(u.up, 'Send enabled again enables the ↑').toBeEnabled();
        await page.waitForTimeout(500);
        await u.up.click();
        await expect.poll(() => posts).toBe(1);
        await u.send.evaluate((b: HTMLButtonElement) => (b.disabled = true));
        release();
        await expect(u.note).toHaveText('Inserted, not sent: Send is unavailable right now.');
        await expect(u.input).toHaveValue(FAKE_TEXT);
        await page.waitForTimeout(600);
        expect(sends(), 'nothing was sent').toBe(0);
        await expectReleased(page, 'Y1');
        await u.send.evaluate((b: HTMLButtonElement) => (b.disabled = false));
      });

      // Y2: the hold swallows a TAP on Send, never the script's own click for
      // the next voice send: two ↑ in quick succession both send.
      test('two voice sends in quick succession both send', async ({ page }) => {
        await instrument(page);
        let k = 0;
        await page.route('**/transcribe', async (r) => {
          if (r.request().method() !== 'POST') return r.continue();
          k++;
          await r.fulfill({ status: 200, contentType: 'text/plain; charset=utf-8', body: `voice send number ${k}` });
        });
        const times: number[] = [];
        page.on('request', (r) => {
          if (r.method() === 'POST' && new URL(r.url()).pathname === '/mirror/view/send') times.push(Date.now());
        });
        await openRendered(page);
        const u = ui(page);
        await u.input.fill('');
        for (let i = 0; i < 2; i++) {
          await u.mic.click();
          await expect(u.mic).toHaveAttribute('data-state', 'recording');
          await page.waitForTimeout(300);
          await u.up.click();
          await expect.poll(() => times.length, { timeout: 10_000 }).toBe(i + 1);
        }
        expect(times[1] - times[0], 'the second voice send came inside the first one’s hold').toBeLessThan(1500);
        await expect
          .poll(() => read(transcript + '.pasted'), { message: `pasted (log:\n${mirrorLog})` })
          .toBe('%e2e-dict-@1 voice send number 1\n%e2e-dict-@1 voice send number 2\n');
      });

      // Y3 (operator decision "Draft + transcript, draft shown"): the ↑ sends
      // what was in the box FOLLOWED BY the transcript — wherever the caret
      // was — and while recording the draft is previewed on one line (cut
      // with an ellipsis, whole in its title). An empty box shows no preview.
      test('the ↑ sends the draft followed by the transcript, and previews the draft while recording', async ({ page }) => {
        await instrument(page);
        await routeStt(page);
        await openRendered(page);
        const u = ui(page);
        const preview = page.locator('#mv-dictate-wave [data-dictate-draft]');
        const draft = 'my draft that runs on well past the width of the waveform row so the preview has to cut it short';
        await u.input.fill(draft);
        await u.input.evaluate((el: HTMLTextAreaElement) => el.setSelectionRange(0, 0));
        await u.mic.click();
        await expect(u.mic).toHaveAttribute('data-state', 'recording');
        await expect(preview, 'the draft is previewed').toBeVisible();
        await expect(preview).toContainText(draft);
        await expect(preview).toHaveAttribute('title', draft);
        const pb = (await preview.boundingBox())!;
        expect(pb.height, 'one line').toBeLessThan(30);
        expect(await preview.evaluate((el) => el.scrollWidth > el.clientWidth), 'cut with an ellipsis').toBe(true);
        await page.waitForTimeout(500);
        await u.up.click();
        await expect.poll(() => read(transcript + '.pasted'), { message: `pasted (log:\n${mirrorLog})` }).toBe(`%e2e-dict-@1 ${draft} ${FAKE_TEXT}\n`);
        await expect(preview).toBeHidden();
        // An empty box: no preview.
        await u.input.fill('');
        await u.mic.click();
        await expect(u.mic).toHaveAttribute('data-state', 'recording');
        await expect(u.wave).toBeVisible();
        await expect(preview, 'no draft, no preview').toBeHidden();
        await u.discard.click();
        await expectIdle(page, 'empty');
      });

      // Y4: the ↑ presses the Send on screen WHEN THE TRANSCRIPT COMES BACK.
      // ⚠ SIMULATED CHIEF MODE: this server runs no chief, so the page is
      // given a stand-in of view_chief.go's switch — a checkbox and an
      // #mv-send-chief, shown and hidden by the same kind of :has() rule
      // (viewChiefOn). What is real is the ↑'s choice between the two.
      test('the chief switch flipped while recording: the ↑ presses the Send on screen at the end', async ({ page }) => {
        await instrument(page);
        await routeStt(page);
        const sends = mirrorSends(page);
        await openRendered(page);
        const u = ui(page);
        await page.evaluate(() => {
          const st = document.createElement('style');
          st.textContent =
            '#mv-send-chief{display:none}#mv-compose:has(#wave-chief:checked) #mv-send-chief{display:inline-flex}' +
            '#mv-compose:has(#wave-chief:checked) :is(#mv-send,#mv-send-group){display:none!important}';
          document.head.appendChild(st);
          const cb = document.createElement('input');
          cb.type = 'checkbox';
          cb.id = 'wave-chief';
          const b = document.createElement('button');
          b.type = 'button';
          b.id = 'mv-send-chief';
          b.textContent = 'chief';
          (window as unknown as { __chief: number }).__chief = 0;
          b.addEventListener('click', () => (window as unknown as { __chief: number }).__chief++);
          const row = document.getElementById('mv-button-row')!;
          row.prepend(cb);
          row.appendChild(b);
        });
        const chief = () => page.evaluate(() => (window as unknown as { __chief: number }).__chief);
        // Agent mode at the start, chief mode at the end: the chief's Send.
        await u.input.fill('');
        await u.mic.click();
        await expect(u.mic).toHaveAttribute('data-state', 'recording');
        await page.locator('#wave-chief').check();
        await page.waitForTimeout(400);
        await u.up.click();
        await expect.poll(chief, { message: 'the chief’s Send was pressed' }).toBe(1);
        await page.waitForTimeout(500);
        expect(sends(), 'the agent’s Send was not').toBe(0);
        // Chief mode at the start, agent mode at the end: the agent's Send.
        await u.input.fill('');
        await page.waitForTimeout(1600); // past the hold
        await u.mic.click();
        await expect(u.mic).toHaveAttribute('data-state', 'recording');
        await page.locator('#wave-chief').uncheck();
        await page.waitForTimeout(400);
        await u.up.click();
        await expect.poll(sends, { message: 'the agent’s Send was pressed' }).toBe(1);
        expect(await chief(), 'the chief’s was not').toBe(1);
        // Let the paste land before the test ends: the host writes it after
        // the request, and a later test's fresh paste file must not get it.
        await expect.poll(() => read(transcript + '.pasted'), { message: `pasted (log:\n${mirrorLog})` }).toBe(`%e2e-dict-@1 ${FAKE_TEXT}\n`);
      });

      // Y5: focus goes to the ✓ when a recording starts, and back into the
      // box on every end — ✓, ↑ and ×.
      test('focus: on the ✓ while recording, back in the box after ✓, ↑ and ×', async ({ page }) => {
        await instrument(page);
        await routeStt(page);
        await openRendered(page);
        const u = ui(page);
        const focused = () => page.evaluate(() => document.activeElement?.id || document.activeElement?.tagName);
        for (const x of [
          { name: '✓', end: () => u.mic.click() },
          { name: '↑', end: () => u.up.click() },
          { name: '×', end: () => u.discard.click() },
        ]) {
          await u.input.fill('');
          await page.waitForTimeout(1600); // past any hold
          await u.mic.click();
          await expect(u.mic).toHaveAttribute('data-state', 'recording');
          await expect.poll(focused, { message: `${x.name}: focus is on the ✓ while recording` }).toBe('mv-dictate');
          await page.waitForTimeout(300);
          await x.end();
          await expect(u.mic).toHaveAttribute('data-state', 'idle');
          await expect.poll(focused, { message: `${x.name}: focus is back in the box` }).toBe('mv-input');
        }
      });

      // G2: a voice send whose step-up is CANCELLED is not sent, and its text
      // is back in the box (the send path's own rule; the ↑ adds nothing).
      test('↑ then the step-up cancelled: nothing is sent and the transcript is back in the box', async ({ page }) => {
        await instrument(page);
        await routeStt(page);
        let refused = 0;
        await page.route('**/mirror/view/send?*', async (route) => {
          refused++;
          await route.fulfill({ status: 403, headers: { 'X-Muster-Step-Up': 'required', 'Content-Type': 'text/plain' }, body: "confirm it's you" });
        });
        await openRendered(page);
        const u = ui(page);
        await u.input.fill('');
        await u.mic.click();
        await expect(u.mic).toHaveAttribute('data-state', 'recording');
        await page.waitForTimeout(500);
        await u.up.click();
        const dialog = page.locator('#term-confirm');
        await expect(dialog).toBeVisible();
        await page.locator('#term-confirm-cancel').click();
        await expect(dialog).toBeHidden();
        await expect(u.input, 'the transcript is back in the box').toHaveValue(FAKE_TEXT);
        await page.waitForTimeout(600);
        expect(refused, 'one send, refused for the step-up, never retried').toBe(1);
        expect(read(transcript + '.pasted'), 'nothing pasted').toBe('');
        await page.unroute('**/mirror/view/send?*');
      });

      // The agent chat's ↑ clicks #chat-send: the message goes over the chat's
      // own socket, once.
      test('the agent chat: ↑ transcribes and sends once through #chat-send', async ({ page }) => {
        test.setTimeout(150_000);
        await instrument(page);
        await signIn(page, origin);
        const a = await runningAgent(page);
        await page.goto(`${origin}/agents/${a.name}`);
        const mic = page.locator('#chat-dictate');
        const up = page.locator('#chat-dictate-send');
        const send = page.locator('#chat-send');
        const log = page.locator('#chat-log');
        await expect(mic).toBeVisible({ timeout: 20_000 });
        const said = `${FAKE_TEXT} ${vp.use.isMobile ? 'phone' : 'desktop'} ${Date.now().toString(36)}`;
        let n = 0;
        await page.route('**/transcribe', async (r) => {
          if (r.request().method() !== 'POST') return r.continue();
          n++;
          await r.fulfill({ status: 200, contentType: 'text/plain; charset=utf-8', body: said });
        });
        const before = await log.locator('[data-bubble="user"]', { hasText: said }).count();
        await page.locator('#chat-input').fill('');
        await mic.click();
        await expect(mic).toHaveAttribute('data-state', 'recording');
        await expect(page.locator('#chat-dictate-wave')).toBeVisible();
        await expect(page.locator('#chat-input')).toBeHidden();
        await expect(up).toBeVisible();
        await expect(send).toBeHidden();
        await expectHeld(page, 'chat');
        await page.waitForTimeout(800);
        await up.click();
        await expect(log.locator('[data-bubble="user"]', { hasText: said })).toHaveCount(before + 1, { timeout: 30_000 });
        await expect(page.locator('#chat-input')).toHaveValue('');
        await expect(send).toBeVisible();
        await expect(up).toBeHidden();
        await expectReleased(page, 'chat');
        await page.waitForTimeout(1500);
        await expect(log.locator('[data-bubble="user"]', { hasText: said }), 'sent once').toHaveCount(before + 1);
        expect(n, 'one recording posted').toBe(1);
      });
    });
  }
});
