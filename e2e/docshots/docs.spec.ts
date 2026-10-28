import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import { AddressInfo } from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, docsBrowserPort, docsGotruePort, docsMainPort, dsn, e2eRoot, gotrueSecret, repoRoot, versionA } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { authenticator } from '../lib/webauthn';
import { writeFileAtomic } from '../lib/atomic';

// THE DOCS SCREENSHOTS (docs/img/*.png) AND THE README'S PHONE-FLOW ANIMATION
// (docs/img/phone-flow.gif), generated rather than hand-made, so they can be
// re-taken after any UI change and can never show real data. `make screenshots`
// runs this file alone (MUSTER_E2E_DOCSHOTS=1); it is never part of the suite
// or of CI.
//
// 🔴 FIXTURE DATA ONLY. The board is e2e/seed's (global-setup refuses any
// database not named *_e2e, and so does the check below). The mirrored hosts
// are `muster-mirror -tags e2emirror` — real tunnel, FAKE tmux — whose windows
// and Claude Code transcripts are written by this file. The account is a
// throwaway GoTrue user in a throwaway container. The speech-to-text service is
// a fake played by this file, and the microphone is Chromium's fake device.
// Every host, session, codename, path and message below is invented.
//
// One gotrue-mode server (so the step-up dialog can offer a PASSKEY, from
// Chromium's virtual authenticator against the real GoTrue image), with the
// mirror plugin and dictation on.
//
// Phone shots are 390×844 CSS pixels at 2× (780×1688 PNGs); the desktop shot is
// 1280×800 at 2×. The animation's frames are written to e2e/.state/phone-flow/
// with an ffmpeg concat list; `make screenshots` assembles them.

const outDir = path.join(repoRoot, 'docs', 'img');
const framesDir = path.join(e2eRoot, '.state', 'phone-flow');
const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
const project = `muster-e2e-gotrue-${docsGotruePort}`;
const browserURL = `http://localhost:${docsBrowserPort}`;
const machineURL = `http://localhost:${docsMainPort}`;
const gotrueURL = `http://127.0.0.1:${docsGotruePort}`;
const composeEnv = {
  ...process.env,
  MUSTER_E2E_GOTRUE_PROJECT: project,
  MUSTER_E2E_GOTRUE_PORT: String(docsGotruePort),
  MUSTER_E2E_GOTRUE_SITE: browserURL,
  MUSTER_E2E_GOTRUE_PASSKEYS: 'true',
};
// Two invented machines, to show one board across several.
const host = 'workstation';
const host2 = 'laptop';
const adminEmail = `docs-admin-${Date.now()}@example.com`;
const adminPW = 'e2e-docs-password-not-a-real-one';
const sttToken = 'e2e-docs-stt-token-not-a-real-one';
const DICTATED = 'Yes, add the index and rerun the benchmark.';
const mirrorBin = path.join(binDir, 'muster-mirror-e2e-docs');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-docshots-'));
const windowsFile = path.join(dir, 'windows');
const windowsFile2 = path.join(dir, 'windows2');
const pasteFile = path.join(dir, 'pasted');
const tr = (n: string) => path.join(dir, `${n}.jsonl`);

let server: Running | undefined;
const mirrors: ChildProcess[] = [];
let mirrorLog = '';

// --- the fake speech-to-text service: answers every recording with DICTATED --
const stt = http.createServer((req, res) => {
  req.resume();
  req.on('end', () => {
    if (req.url !== '/v1/audio/transcriptions' || req.headers.authorization !== 'Bearer ' + sttToken) {
      res.writeHead(404).end();
      return;
    }
    res.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify({ text: DICTATED, duration_s: 2.0, elapsed_s: 0.2 }));
  });
});

// --- the transcripts -----------------------------------------------------------
const rec = (o: unknown) => JSON.stringify(o) + '\n';
// A turn's usage: the context is input + cache creation + cache read, which is
// what the per-tool-call context badges are measured from.
const usage = (ctx: number) => ({ input_tokens: 5, cache_creation_input_tokens: 1200, cache_read_input_tokens: ctx - 1205, output_tokens: 180 });
const asst = (block: object, ctx: number) => rec({ type: 'assistant', message: { model: 'claude-fixture', content: [block], usage: usage(ctx) } });
const say = (t: string) => ({ type: 'text', text: t });
const call = (id: string, name: string, input: object) => ({ type: 'tool_use', id, name, input });
const result = (id: string, out: string) => rec({ type: 'user', promptId: 'p1', message: { content: [{ type: 'tool_result', tool_use_id: id, content: out }] } });
const prompt = (t: string, id = 'p1') => rec({ type: 'user', promptId: id, origin: { kind: 'human' }, message: { role: 'user', content: t } });

// otter: the session the animation and most shots open. Three tool calls in a
// row (one "3 tool calls" group), each followed by a turn with usage, so every
// row carries a measured context badge: +1.2k, +4.8k, +610.
const OTTER =
  prompt('The task-list endpoint is slow with 5k tasks. Find out why and fix it.') +
  asst(say("I'll profile the list query first."), 21005) +
  asst(call('t1', 'Bash', { command: 'go test -run TestListTasks -bench . ./internal/notes' }), 21005) +
  result('t1', 'BenchmarkListTasks  412 ms/op') +
  asst(call('t2', 'Read', { file_path: 'internal/notes/list.go' }), 22240) +
  result('t2', '…') +
  asst(call('t3', 'Edit', { file_path: 'internal/notes/list.go' }), 27050) +
  result('t3', 'ok') +
  asst(say('Comments were loaded one query per task. They now load in one batched query: 412 ms → 9 ms. Want me to add an index on the comments table as well?'), 27660);
// What the "agent" answers after the animation's send, appended by this file
// as Claude Code would write it. (The prompt itself is appended by the fake
// host, as the CLI writes what it was sent.)
const OTTER_NEXT =
  asst(say('Adding a migration for the index, then rerunning the benchmark.'), 27900) +
  asst(call('t4', 'Bash', { command: 'go test -run TestListTasks -bench . ./internal/notes' }), 28120) +
  result('t4', 'BenchmarkListTasks  6 ms/op') +
  asst(say('Done: 9 ms → 6 ms with the index. Migration 0042 adds it.'), 28400);

const simple = (q: string, a: string) => prompt(q) + asst(say(a), 15000);

const now = Math.floor(Date.now() / 1000);
// <session>|<window id>|<index>|<active 0/1>|<command>|<activity>|<transcript>
// The session names are the codenames the switcher's search finds.
const windows = [
  ['otter', '@1', 1, 1, 'claude', now - 20, tr('otter')],
  ['falcon', '@2', 1, 1, 'claude', now - 95, tr('falcon')],
  ['quartz', '@3', 1, 1, 'codex', now - 400, ''],
  ['kestrel', '@4', 1, 1, 'zsh', now - 900, ''],
  ['otter', '@5', 2, 0, 'zsh', now - 1500, ''],
];
const windows2 = [
  ['juniper', '@1', 1, 1, 'claude', now - 60, tr('juniper')],
  ['maple', '@2', 1, 1, 'opencode', now - 700, ''],
  ['heron', '@3', 1, 1, 'claude', now - 2400, tr('heron')],
];

function writeTranscripts() {
  fs.writeFileSync(tr('otter'), OTTER);
  fs.writeFileSync(tr('falcon'), simple('Write the release notes for 2.4.', 'Drafted in CHANGELOG.md — three features, two fixes.'));
  fs.writeFileSync(tr('juniper'), simple('Rename the billing feature flags to the new scheme.', 'Renamed 14 flags across 9 files; the tests pass.'));
  fs.writeFileSync(tr('heron'), simple('Why is the nightly build red?', 'A flaky timeout in the upload test. I pinned the fake clock.'));
}

test.describe.configure({ mode: 'serial' });
test.use({
  viewport: { width: 390, height: 844 },
  deviceScaleFactor: 2,
  isMobile: true,
  hasTouch: true,
  storageState: { cookies: [], origins: [] },
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] },
  permissions: ['microphone'],
});

const serverEnv = () => ({ ...process.env, DATABASE_URL: dsn, MUSTER_PUBLIC_URL: browserURL, MUSTER_GOTRUE_URL: gotrueURL, MUSTER_GOTRUE_JWT_SECRET: gotrueSecret });

function startMirror(name: string, file: string) {
  const token = execFileSync(serverBinary(versionA), ['mirror', 'token', 'create', name], { env: serverEnv() }).toString().trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  const p = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: machineURL,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: name,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: file,
      MUSTER_MIRROR_E2E_PASTE_FILE: pasteFile,
      MUSTER_MIRROR_E2E_WINDOW: '120x40',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  p.stdout?.on('data', (d) => (mirrorLog += d));
  p.stderr?.on('data', (d) => (mirrorLog += d));
  mirrors.push(p);
}

test.beforeAll(async () => {
  test.setTimeout(300_000);
  expect(new URL(dsn).pathname, 'screenshots run only against the seeded *_e2e database').toMatch(/_e2e$/);
  fs.mkdirSync(outDir, { recursive: true });
  fs.rmSync(framesDir, { recursive: true, force: true });
  fs.mkdirSync(framesDir, { recursive: true });
  await new Promise<void>((r) => stt.listen(0, '127.0.0.1', () => r()));
  const sttPort = (stt.address() as AddressInfo).port;
  execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
  execFileSync('go', ['run', './e2e/seed', '--accounts-only'], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn } });
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionA, docsMainPort, {
    MUSTER_AUTH_MODE: 'gotrue',
    MUSTER_GOTRUE_URL: gotrueURL,
    MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
    MUSTER_BROWSER_PORT: String(docsBrowserPort),
    MUSTER_PUBLIC_URL: browserURL,
    MUSTER_MIRROR: '1',
    MUSTER_STT_URL: `http://127.0.0.1:${sttPort}`,
    MUSTER_STT_TOKEN: sttToken,
  });
  writeTranscripts();
  writeFileAtomic(windowsFile, windows.map((w) => w.join('|')).join('\n') + '\n');
  writeFileAtomic(windowsFile2, windows2.map((w) => w.join('|')).join('\n') + '\n');
  startMirror(host, windowsFile);
  startMirror(host2, windowsFile2);
});

test.afterAll(async () => {
  try {
    for (const p of mirrors) {
      if (p.exitCode === null && p.signalCode === null) {
        const done = new Promise((r) => p.once('exit', r));
        p.kill('SIGTERM');
        await done;
      }
    }
    await server?.stop();
    await new Promise((r) => stt.close(r));
  } finally {
    execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

const q = (h: string, session: string, id: string) => `host=${h}&session=${session}&window=${encodeURIComponent(id)}`;
// view is a session's window-1 Rendered view, its window id looked up in the tables above.
const view = (h: string, session: string) => {
  const w = [...windows, ...windows2].find((r) => r[0] === session && r[2] === 1)!;
  return browserURL + '/mirror/view?' + q(h, session, String(w[1]));
};

// The push-permission line reads "Notifications are blocked…" in a headless
// browser: true of the harness, not of the app. Hidden, as e2e/capture does.
const HIDE = '#push-status{display:none!important} *{caret-color:transparent!important}';
async function shoot(page: Page, file: string) {
  await page.addStyleTag({ content: HIDE });
  await page.waitForTimeout(600);
  await page.screenshot({ path: path.join(outDir, file) });
}

// One sign-in for the whole file: the admin claims user 1 (the owner of the
// seeded board and of the mirror tokens), and the cookies are reused.
let cookies: Awaited<ReturnType<BrowserContext['cookies']>> | undefined;
async function signIn(page: Page) {
  if (cookies) {
    await page.context().addCookies(cookies);
    return;
  }
  const link = execFileSync(serverBinary(versionA), ['invite', '--claim-admin'], { env: serverEnv() }).toString().trim();
  await page.goto(link);
  await page.fill('#claim-email', adminEmail);
  await page.fill('#claim-password', adminPW);
  await page.fill('#claim-password2', adminPW);
  await Promise.all([page.waitForURL(browserURL + '/'), page.click('button[type=submit]')]);
  cookies = await page.context().cookies(browserURL);
}

// GoTrue rotates the refresh token when a page refreshes an expired access
// token, and refuses the old one after that: each test hands the next the
// cookies it ended with, not the first sign-in's.
// The step-up shot forges a STALE access cookie; it is never handed on.
test.afterEach(async ({ context }, info) => {
  if (cookies && !info.title.startsWith('stepup')) cookies = await context.cookies(browserURL);
});

async function listed(page: Page) {
  await page.goto(browserURL + '/agents');
  await expect(page.locator('#agents-list article[data-mirror-card]', { hasText: 'heron' }).first()).toBeVisible({ timeout: 20_000 });
}

// visitAll opens each session once, so the switcher's Recent view (the
// viewer's server-side recents) has entries, otter newest.
let visited = false;
async function visitAll(page: Page) {
  if (visited) return;
  for (const [h, s] of [
    [host2, 'heron'],
    [host, 'kestrel'],
    [host2, 'juniper'],
    [host, 'falcon'],
  ]) {
    await page.goto(view(h, s));
    await expect(page.locator('#mv-input')).toBeVisible({ timeout: 20_000 });
  }
  visited = true;
}

async function openOtter(page: Page) {
  await page.goto(view(host, 'otter'));
  await expect(page.locator('#mv-log [data-bubble="assistant"]').last()).toContainText('batched query', { timeout: 20_000 });
}

test('tasks.png — the task board', async ({ page }) => {
  await signIn(page);
  await page.goto(browserURL + '/tasks');
  await expect(page.locator('#tasks-list article').first()).toBeVisible();
  await shoot(page, 'tasks.png');
});

// It runs SECOND, while the GoTrue session is minutes old: after the longer
// shots below, the passkeys page answered "Passkeys could not be loaded".
//
// A stale sign-in is made by re-signing the access cookie with its `amr` moved
// 13 hours back — the token GoTrue would mint for a sign-in that long ago (see
// e2e/tests/mirror-stepup.spec.ts, which this mirrors).
function b64urlJSON(part: string): Record<string, unknown> {
  return JSON.parse(Buffer.from(part, 'base64url').toString('utf8'));
}
function sign(payload: Record<string, unknown>): string {
  const h = Buffer.from(JSON.stringify({ alg: 'HS256', typ: 'JWT' })).toString('base64url');
  const p = Buffer.from(JSON.stringify(payload)).toString('base64url');
  return `${h}.${p}.${crypto.createHmac('sha256', gotrueSecret).update(h + '.' + p).digest('base64url')}`;
}

test('stepup-passkey.png — confirm it is you, with a passkey', async ({ page, context }) => {
  test.setTimeout(120_000);
  await authenticator(page);
  await signIn(page);
  await page.goto(browserURL + '/account/passkeys');
  await page.locator('#passkey-add').click();
  await expect(page.locator('#passkey-list [data-passkey-id]')).toHaveCount(1);

  await page.goto(view(host, 'falcon'));
  await expect(page.locator('#mv-input')).toBeEnabled({ timeout: 20_000 });
  const real = (await context.cookies(browserURL)).find((c) => c.name === 'muster_at')!.value;
  const claims = b64urlJSON(real.split('.')[1]);
  const t = Math.floor(Date.now() / 1000);
  claims.amr = [{ method: 'password', timestamp: t - 13 * 3600 }];
  claims.exp = t + 600;
  await context.addCookies([{ name: 'muster_at', value: sign(claims), url: browserURL, httpOnly: true, secure: true, sameSite: 'Lax' }]);

  await page.locator('#mv-input').fill('Ship it');
  await page.locator('#mv-send').tap();
  await expect(page.locator('#term-confirm')).toBeVisible();
  await expect(page.locator('#term-confirm-passkey')).toBeVisible();
  await shoot(page, 'stepup-passkey.png');
  expect(fs.existsSync(tr('falcon') + '.pasted'), 'nothing was pasted while the sign-in is stale').toBe(false);
  expect(mirrorLog).not.toContain('panic');
});

test('agents-list.png — every session on two machines, and the bottom bar', async ({ page }) => {
  await signIn(page);
  await listed(page);
  await visitAll(page);
  await listed(page);
  // Pin two sessions, one per machine: pins are drawn first.
  for (const name of ['juniper:1', 'otter:1']) {
    const card = page.locator('#agents-list > article[data-mirror-card]', { hasText: name }).first();
    // Idempotent: a re-run against the same database finds them pinned already.
    if ((await card.locator('[data-pin-toggle]').getAttribute('aria-pressed')) !== 'true') await card.locator('[data-pin-toggle]').click();
    await expect(card.locator('[data-pin-toggle]')).toHaveAttribute('aria-pressed', 'true');
  }
  await listed(page);
  await expect(page.locator('#agents-list > article[data-pinned]')).toHaveCount(2);
  await expect(page.locator('#session-switcher')).toBeVisible();
  await page.evaluate(() => window.scrollTo(0, 0));
  await shoot(page, 'agents-list.png');
});

test('agents-filter.png — the Agents list with the switcher search open', async ({ page }) => {
  await signIn(page);
  await listed(page);
  await page.locator('#switcher-search-open').click();
  // A kind matches too, after every codename and title: "claude" lists the
  // Claude Code windows on both machines.
  await page.locator('#switcher-search-input').fill('claude');
  await expect(page.locator('#switcher-search-results [role="option"]').first()).toBeVisible();
  await shoot(page, 'agents-filter.png');
});

test('switcher-search.png — a session page, its bottom bar, and the search palette finding a codename', async ({ page }) => {
  await signIn(page);
  await visitAll(page);
  await openOtter(page);
  await page.locator('#switcher-search-open').click();
  await page.locator('#switcher-search-input').fill('fal');
  await expect(page.locator('#switcher-search-results [role="option"]').first()).toContainText('falcon');
  await shoot(page, 'switcher-search.png');
});

test('mirror-rendered.png — a Claude Code window as a chat, with the message box and mic', async ({ page }) => {
  await signIn(page);
  await visitAll(page);
  await openOtter(page);
  await expect(page.locator('#mv-dictate')).toBeVisible();
  await page.locator('#mv-input').fill('Yes, add the index and rerun the benchmark');
  await shoot(page, 'mirror-rendered.png');
});

test('tool-context.png — each tool call shows the context it added', async ({ page }) => {
  await signIn(page);
  await openOtter(page);
  const group = page.locator('#mv-log [data-tool-group]').first();
  await expect(group.locator('summary [data-tool-group-count]')).toHaveText('3 tool calls', { timeout: 15_000 });
  await group.locator(':scope > summary').click();
  const badges = page.locator('#mv-log [data-tool-ctx]');
  await expect(badges).toHaveCount(3);
  await expect(badges.nth(1)).toHaveText('+4.8k ctx');
  await shoot(page, 'tool-context.png');
});

test('dictation.png — the mic recording into the message box', async ({ page }) => {
  await signIn(page);
  await openOtter(page);
  const mic = page.locator('#mv-dictate');
  await expect(mic).toBeVisible();
  await page.locator('#mv-input').focus();
  await mic.click();
  await expect(mic).toHaveAttribute('data-state', 'recording');
  await expect(page.locator('#mv-dictate-note')).toContainText('Recording 0:0');
  await page.waitForTimeout(1200);
  await page.addStyleTag({ content: HIDE });
  await page.screenshot({ path: path.join(outDir, 'dictation.png') });
  await mic.click();
  await expect(page.locator('#mv-input')).toHaveValue(new RegExp(DICTATED.replace(/[.]/g, '\\.')));
  await page.locator('#mv-input').fill('');
});

test('mirror-terminal.png — the terminal view of a shell window', async ({ page }) => {
  await signIn(page);
  await page.goto(browserURL + '/mirror/terminal?' + q(host, 'kestrel', '@4'));
  await expect(page.locator('#term .xterm-rows')).toContainText('FAKE-MIRROR', { timeout: 20_000 });
  await expect(page.locator('#mv-input')).toBeEnabled();
  await page.locator('#mv-input').fill('make test');
  await shoot(page, 'mirror-terminal.png');
});

test.describe('desktop', () => {
  test.use({ viewport: { width: 1280, height: 800 }, deviceScaleFactor: 2, isMobile: false, hasTouch: false });
  test('desktop.png — a session and the bar at a desktop size', async ({ page }) => {
    await signIn(page);
    await openOtter(page);
    const group = page.locator('#mv-log [data-tool-group]').first();
    await group.locator(':scope > summary').click();
    await expect(page.locator('#mv-log [data-tool-ctx]')).toHaveCount(3);
    await shoot(page, 'desktop.png');
  });
});

// --- the README's animation ------------------------------------------------------
// Frames are screenshots, not a video: a screenshot is sharp at 2×, and each
// frame's hold is chosen. A translucent dot marks each tap, since a phone
// recording has none. frames.txt is an ffmpeg concat list.
let frameNo = 0;
const concat: string[] = [];
async function frame(page: Page, hold: number) {
  const f = path.join(framesDir, `f${String(frameNo++).padStart(3, '0')}.png`);
  await page.screenshot({ path: f });
  concat.push(`file '${f}'`, `duration ${hold}`);
}
async function tapDot(page: Page, target: Locator, hold = 0.5) {
  const b = (await target.boundingBox())!;
  await page.evaluate(
    ([x, y]) => {
      const d = document.createElement('div');
      d.id = 'docshots-tap';
      d.style.cssText = `position:fixed;left:${x - 22}px;top:${y - 22}px;width:44px;height:44px;border-radius:50%;background:rgba(255,255,255,.35);border:2px solid rgba(255,255,255,.75);z-index:2147483647;pointer-events:none`;
      document.body.appendChild(d);
    },
    [b.x + b.width / 2, b.y + b.height / 2],
  );
  await frame(page, hold);
  await page.evaluate(() => document.getElementById('docshots-tap')?.remove());
}

test('phone-flow.gif frames — open, switch by search, dictate, send', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page);
  await visitAll(page);
  // Start from juniper's page, on the other machine.
  await page.goto(view(host2, 'juniper'));
  await expect(page.locator('#mv-log [data-bubble="assistant"]').last()).toBeVisible({ timeout: 20_000 });
  await page.addStyleTag({ content: HIDE });
  await page.waitForTimeout(500);
  await frame(page, 1.4);

  // The bar's search: type a codename.
  await tapDot(page, page.locator('#switcher-search-open'));
  await page.locator('#switcher-search-open').click();
  await expect(page.locator('#switcher-search-input')).toBeVisible();
  await page.waitForTimeout(250);
  await frame(page, 0.5);
  for (const ch of 'ott') {
    await page.keyboard.type(ch);
    await page.waitForTimeout(120);
    await frame(page, 0.3);
  }
  await expect(page.locator('#switcher-search-results [role="option"]').first()).toContainText('otter');
  await frame(page, 0.8);
  await Promise.all([page.waitForURL(/session=otter/), page.keyboard.press('Enter')]);
  await expect(page.locator('#mv-log [data-bubble="assistant"]').last()).toContainText('batched query', { timeout: 20_000 });
  await page.addStyleTag({ content: HIDE });
  await page.waitForTimeout(500);
  await frame(page, 2.0);

  // Dictate a reply.
  const mic = page.locator('#mv-dictate');
  await page.locator('#mv-input').focus();
  await tapDot(page, mic);
  await mic.click();
  await expect(mic).toHaveAttribute('data-state', 'recording');
  for (let i = 0; i < 3; i++) {
    await page.waitForTimeout(600);
    await frame(page, 0.6);
  }
  await tapDot(page, mic, 0.4);
  await mic.click();
  await expect(page.locator('#mv-input')).toHaveValue(new RegExp(DICTATED.replace(/[.]/g, '\\.')));
  await expect(mic).toHaveAttribute('data-state', 'idle');
  await page.waitForTimeout(300);
  await frame(page, 1.6);

  // Send it; the session answers.
  const send = page.locator('#mv-send');
  await tapDot(page, send);
  await send.click();
  await expect(page.locator('#mv-input')).toHaveValue('');
  await expect.poll(() => (fs.existsSync(tr('otter') + '.pasted') ? fs.readFileSync(tr('otter') + '.pasted', 'utf8') : ''), { message: `pasted (log:\n${mirrorLog})` }).toContain(DICTATED);
  await page.waitForTimeout(300);
  await frame(page, 0.6);
  fs.appendFileSync(tr('otter'), OTTER_NEXT);
  await expect(page.locator('#mv-log [data-bubble="assistant"]').last()).toContainText('9 ms → 6 ms', { timeout: 20_000 });
  await page.waitForTimeout(500);
  await frame(page, 3.0);

  // The concat demuxer drops the last entry's duration; repeat the file.
  concat.push(concat[concat.length - 2]);
  fs.writeFileSync(path.join(framesDir, 'frames.txt'), concat.join('\n') + '\n');
});
