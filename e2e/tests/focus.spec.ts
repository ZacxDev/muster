import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { binDir, dsn, focusPort, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { clsInit, readCls, resetCls, settle } from '../lib/cls';
import { writeFileAtomic } from '../lib/atomic';

// SESSION FOCUS, in a real browser, against a real muster-server
// (MUSTER_MIRROR=1, its own port) and a real muster-mirror over the tunnel (the
// `-tags e2emirror` per-window fake). A dispatched agent of this spec's own
// (e2e/mkchat) and a mirrored Claude Code window each carry a focus (written by
// mkchat's MUSTER_E2E_FOCUS — the mirrored one keyed by the window's CLAUDE
// SESSION ID, which the host reports from the transcript's name). On a phone
// and on a desktop: the session switcher's palette finds each session by a
// word only its focus carries (and a window with no Claude session has none).
//
// FOCUS HISTORY (migration 0022), on the session's own page: the rendered
// view's "Your messages" sheet is tabbed with "Focus history" (arrow keys, the
// choice kept across a reload) and scrolls back through 46 history rows a page
// at a time, newest — the current focus, with its description and age — first;
// the agent's page has the same history in a dialog, inside the viewport; and
// none of it shifts the layout. (The retired Agents list's card focus chip and
// its popover are gone.)

const origin = `http://localhost:${focusPort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-focus-' + stamp;
const agentName = 'e2e-focus-' + stamp;
const sid = '0b2c3d4e-5f60-4718-8a9b-' + stamp.padStart(12, '0').slice(-12);
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'focus-'));
const windowsFile = path.join(dir, 'windows');
const transcript = path.join(dir, sid + '.jsonl');
// A second Claude window whose only refresh FAILED (no focus, a recorded
// problem): the palette must not find it by the other window's focus.
const sid2 = '0b2c3d4e-5f60-4718-8a9c-' + stamp.padStart(12, '0').slice(-12);
const transcript2 = path.join(dir, sid2 + '.jsonl');
// `history`: that many earlier focuses are written first (e2e/mkchat), so the
// mirrored session's history is 46 rows — three pages of 20 — and the agent's 4.
const MIRROR_HISTORY = 45;
const AGENT_HISTORY = 3;
const focusEnv = JSON.stringify([
  { subject: 'self', title: 'Reconciling the quarterly ledgers', description: 'Matching payouts to the bank export. Two left.', history: AGENT_HISTORY },
  { subject: 'mirror:' + sid, title: 'Bisecting the flaky tunnel test', description: 'The backoff resets too early.', history: MIRROR_HISTORY },
  { subject: 'mirror:' + sid2, error: 'no OpenRouter key' },
]);
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';
let agentCleanup = false;

function mkchat(extra: NodeJS.ProcessEnv, input = '') {
  execFileSync('go', ['run', './e2e/mkchat'], {
    cwd: repoRoot,
    input,
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_E2E_AGENT: agentName, MUSTER_E2E_FOCUS: focusEnv, ...extra },
  });
}

test.beforeAll(async () => {
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  agentCleanup = true;
  mkchat({}, JSON.stringify([{ role: 'user', content: 'Reconcile the ledgers.' }]));
  server = await startServer(versionTerm, focusPort, {
    MUSTER_MIRROR: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_FOCUS_INTERVAL: '0',
  });
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  fs.writeFileSync(transcript, '');
  fs.writeFileSync(transcript2, '');
  const now = Math.floor(Date.now() / 1000);
  // session|id|index|active|command|activity|claude-transcript
  writeFileAtomic(windowsFile, `focuswin|@1|1|1|claude|${now - 60}|${transcript}\nfocuswin|@2|2|0|zsh|${now - 120}|\nfocuswin|@3|3|0|claude|${now - 180}|${transcript2}\nreconciling|@4|1|1|zsh|${now - 240}|\n`);
  mirrorProc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  mirrorProc.stdout?.on('data', (d) => (mirrorLog += d));
  mirrorProc.stderr?.on('data', (d) => (mirrorLog += d));
});

test.afterAll(async () => {
  if (mirrorProc && mirrorProc.exitCode === null) {
    const done = new Promise((r) => mirrorProc!.once('exit', r));
    mirrorProc.kill('SIGTERM');
    await done;
  }
  await server?.stop();
  if (agentCleanup) mkchat({ MUSTER_E2E_DELETE: '1' });
  fs.rmSync(dir, { recursive: true, force: true });
});

const viewURL = origin + '/mirror/view?' + new URLSearchParams({ host, session: 'focuswin', window: '@1' }).toString();

// navRows is the sidebar's list partial (GET /ui/nav/agents: the rows the
// session switcher's palette also searches), as markup.
const navRows = async (page: Page) => (await page.request.get(origin + '/ui/nav/agents')).text();
// rowOf is the sidebar row (its whole <a>…</a>) of this host's window `win`.
const rowOf = (html: string, win: string) =>
  [...html.matchAll(/<a\b[^>]*data-nav-agent="mirror"[^>]*>[\s\S]*?<\/a>/g)].map((m) => m[0]).find((a) => a.includes(`host=${host}`) && a.includes(`window=%40${win}`)) ?? '';

// listed waits until the sidebar lists the host's Claude window @1 and the
// shell window @2, and this spec's agent (the host is up and has reported).
async function listed(page: Page) {
  await signIn(page, origin);
  await expect
    .poll(async () => {
      const html = await navRows(page);
      return !!rowOf(html, '1') && !!rowOf(html, '2') && html.includes(`/agents/${agentName}`);
    }, { message: `the host's windows and the agent are listed (log:\n${mirrorLog})`, timeout: 20_000 })
    .toBe(true);
}

for (const vp of [
  { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    // (The Agents list's cards — retired — headed each session with its focus
    // title, a popover trigger with the description and "updated … ago"; four
    // tests here pinned that chip, its keyboard path, its survival of a list
    // morph, and a failed refresh's "unavailable" on it. The focus is now shown
    // on the session's own page — its focus history, whose newest row is the
    // current focus with its description and age (the tests below) — and is the
    // title the palette finds a session by.)

    test('a window whose host reports no Claude session has no focus, and the session switcher finds a session by its focus', async ({ page }) => {
      await listed(page);
      // The shell window (@2, no Claude session) is listed with no focus in
      // what the palette finds it by; POSITIVE CONTROL: the Claude window's
      // row (@1) does carry its focus title there.
      const html = await navRows(page);
      expect(rowOf(html, '1'), "the Claude window's row is searched by its focus").toContain('Bisecting the flaky tunnel test');
      expect(rowOf(html, '2'), 'the shell window has no focus').not.toContain('Bisecting the flaky tunnel test');
      // The command palette, opened from the sidebar's magnifier on a page
      // that is neither session (the board: so neither is left out as the
      // page's own), finds an entry by its focus title: one option, whose
      // second line is it.
      await page.goto(origin + '/tasks');
      if (await page.locator('#sidebar-open').isVisible()) await page.locator('#sidebar-open').click();
      await expect(page.locator(`#sidebar-agents-list a[href^="/agents/${agentName}"]`)).toHaveCount(1);
      await page.locator('#sidebar-search').click();
      const input = page.locator('#switcher-search-input');
      const options = page.locator('#switcher-search-results [role="option"]');
      await input.fill('quarterly ledgers');
      await expect(options).toHaveCount(1);
      await expect(options.first()).toContainText('Reconciling the quarterly ledgers');
      await input.fill('flaky tunnel');
      await expect(options).toHaveCount(1);
      await expect(options.first()).toContainText('Bisecting the flaky tunnel test');
      await page.keyboard.press('Escape');
    });

    test('the rendered view\'s sheet tabs "Your messages" with "Focus history", by arrow keys, and keeps the choice', async ({ page }) => {
      await signIn(page, origin);
      await page.goto(viewURL);
      // Start from no stored choice.
      await page.evaluate(() => { try { localStorage.clear(); } catch (e) {} });
      await page.reload();
      const opener = page.locator('#chat-index-open');
      const sheet = page.locator('#chat-index');
      const msgsTab = sheet.locator('[role="tab"][data-tab="messages"]');
      const histTab = sheet.locator('[role="tab"][data-tab="focus"]');
      const msgsPanel = sheet.locator('#chat-index-tabs-panel-messages');
      const histPanel = sheet.locator('#chat-index-tabs-panel-focus');
      await opener.click();
      await expect(sheet).toBeVisible();
      await expect(sheet.locator('[role="tablist"]')).toBeVisible();
      await expect(msgsTab).toHaveAttribute('aria-selected', 'true');
      await expect(histTab).toHaveAttribute('aria-selected', 'false');
      await expect(msgsTab).toHaveAttribute('tabindex', '0');
      await expect(histTab).toHaveAttribute('tabindex', '-1');
      await expect(msgsPanel).toBeVisible();
      await expect(histPanel).toBeHidden();
      // ArrowRight: the next tab is focused AND selected; its panel shows.
      await msgsTab.focus();
      await page.keyboard.press('ArrowRight');
      await expect(histTab).toBeFocused();
      await expect(histTab).toHaveAttribute('aria-selected', 'true');
      await expect(histTab).toHaveAttribute('tabindex', '0');
      await expect(msgsTab).toHaveAttribute('tabindex', '-1');
      await expect(histPanel).toBeVisible();
      await expect(msgsPanel).toBeHidden();
      await expect(histPanel.locator('[data-focus-history-item]').first()).toContainText('Bisecting the flaky tunnel test');
      // ArrowRight wraps; ArrowLeft goes back; End/Home jump.
      await page.keyboard.press('ArrowRight');
      await expect(msgsTab).toBeFocused();
      await expect(msgsTab).toHaveAttribute('aria-selected', 'true');
      await page.keyboard.press('ArrowLeft');
      await expect(histTab).toHaveAttribute('aria-selected', 'true');
      await page.keyboard.press('Home');
      await expect(msgsTab).toHaveAttribute('aria-selected', 'true');
      await page.keyboard.press('End');
      await expect(histTab).toHaveAttribute('aria-selected', 'true');
      // Kept across a reload, for this window.
      await page.reload();
      await page.locator('#chat-index-open').click();
      await expect(sheet.locator('[role="tab"][data-tab="focus"]')).toHaveAttribute('aria-selected', 'true');
      await expect(sheet.locator('#chat-index-tabs-panel-focus')).toBeVisible();
      await expect(sheet.locator('#chat-index-tabs-panel-messages')).toBeHidden();
      // A click selects too, and is kept.
      await sheet.locator('[role="tab"][data-tab="messages"]').click();
      await page.reload();
      await page.locator('#chat-index-open').click();
      await expect(sheet.locator('[role="tab"][data-tab="messages"]')).toHaveAttribute('aria-selected', 'true');
    });

    test('the focus history scrolls back through every page, newest first, to its start', async ({ page }) => {
      await signIn(page, origin);
      await page.goto(viewURL);
      await page.locator('#chat-index-open').click();
      await page.locator('#chat-index [role="tab"][data-tab="focus"]').click();
      const panel = page.locator('#chat-index-tabs-panel-focus');
      const items = panel.locator('[data-focus-history-item]');
      const total = MIRROR_HISTORY + 1;
      await expect(items.first()).toContainText('Bisecting the flaky tunnel test');
      await expect(items.first().locator('[data-focus-history-description]')).toHaveText('The backoff resets too early.');
      await expect(items.first().locator('[data-focus-history-at]')).toContainText('ago');
      // One page at first: the sentinel below it has not been scrolled to.
      await expect(items).toHaveCount(20);
      await expect(panel.locator('[data-focus-history-end]')).toHaveCount(0);
      const seen: number[] = [];
      for (let i = 0; i < 6 && (await items.count()) < total; i++) {
        await panel.evaluate((p) => { p.scrollTop = p.scrollHeight; });
        await expect.poll(() => items.count()).toBeGreaterThan(seen.length ? seen[seen.length - 1] : 20);
        seen.push(await items.count());
      }
      await expect(items).toHaveCount(total);
      expect(seen, 'the history arrived a page at a time').toEqual([40, total]);
      await panel.evaluate((p) => { p.scrollTop = p.scrollHeight; });
      await expect(panel.locator('[data-focus-history-end]')).toBeVisible();
      await expect(items.last()).toContainText('Earlier focus 01');
      // Newest first, and nothing repeated or skipped across the page joins.
      const titles = await panel.locator('[data-focus-history-title]').allTextContents();
      const want = ['Bisecting the flaky tunnel test'];
      for (let i = MIRROR_HISTORY; i >= 1; i--) want.push(`Earlier focus ${String(i).padStart(2, '0')}`);
      expect(titles).toEqual(want);
    });

    test("the dispatched agent's page has its focus history in a dialog", async ({ page }) => {
      await signIn(page, origin);
      await page.goto(origin + '/agents/' + agentName);
      const open = page.locator('#focus-history-open');
      const dialog = page.locator('dialog#focus-history');
      await expect(open).toBeVisible();
      await expect(dialog).toBeHidden();
      await expect(open).toHaveAttribute('aria-expanded', 'false');
      if (vp.use.hasTouch) await open.tap();
      else await open.click();
      await expect(dialog).toBeVisible();
      await expect(open).toHaveAttribute('aria-expanded', 'true');
      const items = dialog.locator('[data-focus-history-item]');
      await expect(items).toHaveCount(AGENT_HISTORY + 1);
      await expect(items.first()).toContainText('Reconciling the quarterly ledgers');
      // The newest row is the current focus, whole: its description and age.
      await expect(items.first().locator('[data-focus-history-description]')).toHaveText('Matching payouts to the bank export. Two left.');
      await expect(items.first().locator('[data-focus-history-at]')).toContainText('ago');
      await expect(items.last()).toContainText('Earlier focus 01');
      await expect(dialog.locator('[data-focus-history-end]')).toBeVisible();
      const db = (await dialog.boundingBox())!;
      expect(db.x).toBeGreaterThanOrEqual(0);
      expect(db.x + db.width).toBeLessThanOrEqual(vp.use.viewport.width);
      expect(db.y + db.height).toBeLessThanOrEqual(vp.use.viewport.height + 1);
      await page.keyboard.press('Escape');
      await expect(dialog).toBeHidden();
      await expect(open).toHaveAttribute('aria-expanded', 'false');
      // The close button closes it too.
      await open.click();
      await expect(dialog).toBeVisible();
      await dialog.locator('[data-focus-history-close]').click();
      await expect(dialog).toBeHidden();
    });

    test('the rendered view and the agent page shift the layout less than 0.01', async ({ page }) => {
      await signIn(page, origin);
      await page.addInitScript(clsInit);
      const bad: string[] = [];
      for (const [label, url, ready] of [
        ['rendered view', viewURL, '#mv-log'],
        ['agent page', origin + '/agents/' + agentName, '#focus-history-open'],
      ] as const) {
        await page.goto(url);
        await settle(page);
        const r = await readCls(page);
        expect(r.unsupported, 'the browser reports layout-shift entries').toBeUndefined();
        await expect(page.locator(ready).first(), `${label} drew its content`).toBeVisible();
        console.log(`[focus cls ${vp.name}] ${label} ${r.total}`);
        if (r.total >= 0.01) bad.push(`${label}: ${r.total} ${JSON.stringify(r.shifts)}`);
      }
      // The history arriving (by script: no input, so every shift counts)
      // moves nothing: the sheet is a fixed height, the rows land inside it.
      await page.goto(viewURL);
      await settle(page);
      await resetCls(page);
      await page.evaluate(() => {
        (window as any).muIndex.open();
        const tab = document.querySelector('#chat-index [role="tab"][data-tab="focus"]') as HTMLElement;
        (window as any).muTabs.select(tab.closest('[data-tabs]'), tab, {});
      });
      await expect(page.locator('#chat-index-tabs-panel-focus [data-focus-history-item]')).toHaveCount(20);
      await settle(page, 500);
      const opened = await readCls(page);
      console.log(`[focus cls ${vp.name}] history tab loaded ${opened.total}`);
      if (opened.total >= 0.01) bad.push(`history tab: ${opened.total} ${JSON.stringify(opened.shifts)}`);
      expect(bad).toEqual([]);
    });
  });
}
