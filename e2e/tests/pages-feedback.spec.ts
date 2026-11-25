import { execFileSync } from 'node:child_process';
import http from 'node:http';
import type { Browser, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { dsn, pagesFeedbackConnectPort, pagesFeedbackPort, pagesFeedbackSttPort, repoRoot, sttToken, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// THE PAGE VIEWER'S FEEDBACK PILL (internal/pages feedback.go, internal/api
// page_feedback.go; docs/pages.md#feedback), in a real Chromium with its FAKE
// microphone, against a real muster-server whose MUSTER_STT_* point at a fake
// speech-to-text service played here. The page (e2e/pagesfix feedback) was
// made by a mirrored session on a host that does not mirror to this server,
// so a message is stored and its sender is told the session is not reachable
// (the wake itself is the Go tests' page_feedback_pg_test.go).
//
// At a phone's size (390×844, touch) and a desktop's (1440×900), both set
// here:
//   - a share link's viewer: the pill pinned bottom right; tapped, the panel
//     opens below the header — at phone width the full width, the textarea on
//     its own row and Send beneath it — with NO microphone; Escape folds it
//     back with the draft kept; Send clears the box and thanks them (the
//     same words whether or not the session is reachable), the panel still
//     open; the owner then reads the message, kind
//     "feedback", through their responses route;
//   - the owner's own view: the same pill with the microphone, which records
//     (the shell's Permissions-Policy grants it) and inserts the transcript;
//     Send, told the session is not reachable; the owner's response is theirs
//     (by_owner).
//
// ⚠ WHAT THIS CANNOT SHOW: a real phone's safe-area insets (Chromium's are
// 0), a real microphone, or the session's wake.
//
// Every value below is an obvious fake.

const origin = `http://localhost:${pagesFeedbackPort}`;
const KEY = Buffer.alloc(32, 0x5d).toString('base64');
const FAKE_TEXT = 'feedback from the fake mic';
const NEUTRAL = 'Sent. Thanks for the feedback.';
const UNREACHABLE = "Saved with this page's responses, but the session that made this page isn't reachable, so it was not sent to it.";
let server: Running | undefined;
let token = '';
let pageID = 0;

const sttCalls: string[] = [];
const stt = http.createServer((req, res) => {
  req.on('data', () => {});
  req.on('end', () => {
    sttCalls.push(req.headers.authorization ?? '');
    if (req.url !== '/v1/audio/transcriptions' || req.headers.authorization !== 'Bearer ' + sttToken) {
      res.writeHead(401).end();
      return;
    }
    res.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify({ text: ` ${FAKE_TEXT} `, duration_s: 1.0, elapsed_s: 0.1 }));
  });
});

const fix = (cmd: 'feedback' | 'feedback-clear') =>
  execFileSync('go', ['run', './e2e/pagesfix', cmd, '1'], {
    cwd: repoRoot,
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_CONNECTOR_ENCRYPTION_KEY: KEY },
  }).toString();

test.use({
  storageState: { cookies: [], origins: [] },
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] },
  permissions: ['microphone'],
});

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await new Promise<void>((r) => stt.listen(pagesFeedbackSttPort, '127.0.0.1', () => r()));
  server = await startServer(versionA, pagesFeedbackPort, {
    MUSTER_PAGES: '1',
    MUSTER_PUBLIC_URL: origin,
    MUSTER_CONNECTOR_ENCRYPTION_KEY: KEY,
    // A link key is the connector key, and that one needs its listener.
    MUSTER_CONNECT_PORT: String(pagesFeedbackConnectPort),
    MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${pagesFeedbackConnectPort}`,
    MUSTER_STT_URL: `http://127.0.0.1:${pagesFeedbackSttPort}`,
    MUSTER_STT_TOKEN: sttToken,
  });
  const out = JSON.parse(fix('feedback')) as { page: number; token: string };
  token = out.token;
  pageID = out.page;
  expect(token.length, 'the fixture made a live link').toBeGreaterThan(20);
});

test.afterAll(async () => {
  if (server) fix('feedback-clear');
  await server?.stop();
  await new Promise<void>((r) => stt.close(() => r()));
});

type Box = { x: number; y: number; width: number; height: number };
const box = async (page: Page, sel: string): Promise<Box> => {
  const b = await page.locator(sel).boundingBox();
  expect(b, `${sel} is laid out`).not.toBeNull();
  return b!;
};

// openPanel taps the pill and checks the panel's place: pinned at the bottom,
// below the header, and at phone width the screen's full width with the
// textarea on its own row and Send beneath it.
async function openPanel(page: Page, width: number, height: number) {
  const pill = page.locator('[data-feedback-pill]');
  await expect(pill).toBeVisible();
  const pb = await box(page, '[data-feedback-pill]');
  expect(pb.x + pb.width, 'the pill is at the right edge').toBeGreaterThan(width - 40);
  expect(pb.y + pb.height, 'the pill is at the bottom').toBeGreaterThan(height - 40);
  expect(pb.height, 'the pill is a 44px target').toBeGreaterThanOrEqual(44);
  // 🔴 The pill sits below the page's frame, never over it: a tap meant for
  // the page's own bottom-right corner (a gallery's Save bar) must reach it.
  const fr = await box(page, 'iframe#page');
  expect(pb.y, 'the pill does not cover the page').toBeGreaterThanOrEqual(fr.y + fr.height - 0.5);
  await expect(page.locator('#page-feedback')).toBeHidden();
  await pill.click();
  await expect(page.locator('#page-feedback')).toBeVisible();
  await expect(pill).toBeHidden();
  await expect(page.locator('#page-feedback-text')).toBeFocused();
  const panel = await box(page, '#page-feedback');
  const header = await box(page, 'header');
  expect(panel.y, 'the panel does not cover the header').toBeGreaterThanOrEqual(header.y + header.height);
  expect(Math.round(panel.y + panel.height), 'the panel is pinned to the bottom').toBeGreaterThanOrEqual(height - 20);
  const ta = await box(page, '#page-feedback-text');
  const send = await box(page, '[data-feedback-send]');
  expect(send.y, 'Send is on a row beneath the textarea').toBeGreaterThanOrEqual(ta.y + ta.height);
  if (width < 640) {
    expect(Math.round(panel.x), 'the panel is the full width').toBe(0);
    expect(Math.round(panel.width), 'the panel is the full width').toBe(width);
    expect(ta.width, 'the textarea fills its own row').toBeGreaterThan(width - 40);
  } else {
    expect(panel.width, 'a card on a desktop').toBeLessThan(500);
    expect(panel.x + panel.width, 'at the right').toBeGreaterThan(width - 40);
  }
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), 'no sideways scroll').toBe(true);
}

// send presses Send and returns the recorded response's id, once the panel
// says where it went and the box is empty.
async function send(page: Page, said: string): Promise<number> {
  const answered = page.waitForResponse((r) => new URL(r.url()).pathname.endsWith('/feedback') && r.request().method() === 'POST');
  await page.locator('[data-feedback-send]').click();
  const r = await answered;
  expect(r.status(), 'the message was recorded').toBe(200);
  const body = (await r.json()) as { id: number; delivery: string; message: string };
  await expect(page.locator('[data-feedback-status]')).toHaveText(said);
  await expect(page.locator('#page-feedback-text')).toHaveValue('');
  await expect(page.locator('#page-feedback'), 'the panel stays open for another message').toBeVisible();
  return body.id;
}

// ownerReads is the stored response, read through the owner's own responses
// route (the raw download, JSON).
async function ownerReads(browser: Browser, id: number): Promise<Record<string, unknown>> {
  const ctx = await browser.newContext();
  try {
    const p = await ctx.newPage();
    await signIn(p, origin);
    const r = await p.request.get(`${origin}/pages/${pageID}/responses/${id}/raw`);
    expect(r.status(), 'the owner reads the response').toBe(200);
    return (await r.json()) as Record<string, unknown>;
  } finally {
    await ctx.close();
  }
}

for (const vp of [
  { name: 'phone', width: 390, height: 844, touch: true },
  { name: 'desktop', width: 1440, height: 900, touch: false },
]) {
  test.describe(vp.name, () => {
    test.use({ viewport: { width: vp.width, height: vp.height }, hasTouch: vp.touch, isMobile: vp.touch });

    test("a share link's viewer types feedback (no microphone), and the owner reads it", async ({ page, browser }) => {
      const resp = await page.goto(`${origin}/s/${token}`);
      expect(resp?.headers()['permissions-policy'], "the link's shell grants no microphone").toContain('microphone=()');
      await expect(page.frameLocator('iframe#page').locator('main h1')).toHaveText('Feedback check');
      await openPanel(page, vp.width, vp.height);
      await expect(page.locator('#page-feedback-mic'), "a link's viewer has no microphone").toHaveCount(0);
      await expect(page.locator('[data-dictate]')).toHaveCount(0);

      const msg = `share-${vp.name}-${Date.now().toString(36)}: the second paragraph repeats`;
      await page.locator('#page-feedback-text').fill(msg);
      // Escape folds it back to the pill; the draft is still there.
      await page.keyboard.press('Escape');
      await expect(page.locator('#page-feedback')).toBeHidden();
      await expect(page.locator('[data-feedback-pill]')).toBeVisible();
      await page.locator('[data-feedback-pill]').click();
      await expect(page.locator('#page-feedback-text')).toHaveValue(msg);

      // A stranger is told nothing of the session's reach: the neutral words.
      const id = await send(page, NEUTRAL);
      const stored = await ownerReads(browser, id);
      expect(stored.kind).toBe('feedback');
      expect(stored.part).toBe('__feedback');
      expect(stored.by_owner).toBe(false);
      expect(Number(stored.link_id)).toBeGreaterThan(0);
      expect((stored.data as { text: string }).text).toBe(msg);
      // The close button folds it too.
      await page.locator('[data-feedback-close]').click();
      await expect(page.locator('#page-feedback')).toBeHidden();
    });

    test("the owner's own view has the microphone: dictate, then Send", async ({ page, browser }) => {
      await signIn(page, origin);
      const resp = await page.goto(`${origin}/pages/${pageID}/view`);
      expect(resp?.headers()['permissions-policy'], "the owner's shell grants its own origin the microphone").toContain('microphone=(self)');
      await openPanel(page, vp.width, vp.height);
      const mic = page.locator('#page-feedback-mic');
      await expect(mic, 'the owner has the microphone').toBeVisible();
      const mb = await box(page, '#page-feedback-mic');
      const ta = await box(page, '#page-feedback-text');
      expect(mb.y, 'the microphone is beneath the textarea').toBeGreaterThanOrEqual(ta.y + ta.height);
      expect(mb.width, 'a 44px target').toBeGreaterThanOrEqual(44);

      await page.locator('#page-feedback-text').fill('owner says: ');
      const calls = sttCalls.length;
      const posted = page.waitForResponse((r) => new URL(r.url()).pathname === '/transcribe');
      await mic.click();
      await expect(mic).toHaveAttribute('data-state', 'recording');
      await expect(mic.locator('svg[data-dictate-check]')).toBeVisible();
      await page.waitForTimeout(1300);
      await mic.click();
      expect((await posted).status(), 'the transcription answered').toBe(200);
      await expect(page.locator('#page-feedback-text')).toHaveValue(new RegExp(FAKE_TEXT));
      await expect(mic).toHaveAttribute('data-state', 'idle');
      expect(sttCalls.length, 'the fake service was called once').toBe(calls + 1);
      const typed = await page.locator('#page-feedback-text').inputValue();

      const id = await send(page, UNREACHABLE);
      const stored = await ownerReads(browser, id);
      expect(stored.kind).toBe('feedback');
      expect(stored.by_owner).toBe(true);
      expect((stored.data as { text: string }).text).toBe(typed);
    });
  });
}
