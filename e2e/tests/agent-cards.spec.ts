import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { binDir, cardsConnectPort, cardsPort, dsn, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { writeFileAtomic } from '../lib/atomic';

// WHAT THE AGENTS LIST'S CARDS SHOWED, WHERE IT IS NOW, the operator's click
// path, every hop real (the list page is retired: the sidebar's rows and the
// agent's own page carry it):
//
//   - ATTENTION: an agent asks over the real /mcp door, and a mirrored Claude
//     Code window (a real muster-mirror over the tunnel, -tags e2emirror)
//     waits at a prompt. Each one's sidebar row shows its count and the most
//     urgent kind (navAttentionPill); a second ask raises the agent's count
//     LIVE (no reload); a tap on the row opens that agent's page.
//   - REPO: the window's Claude Code works in a SUBDIRECTORY of a git
//     repository; its host names the repository (the top level's name), and
//     the row carries it as a word the palette finds it by.
//   - EVERY WINDOW HAS ONE: a plain shell window (no Claude Code at all) in
//     another repository carries that repository (and its kind) too.
//   - WORK, AND THE LIFECYCLE ACTIONS: a running dispatched agent's pod
//     reports its work; the agent's page header strip shows the chip LIVE and
//     its sidebar row the count; the page's menu starts it, stops it (the
//     chip goes though the pod never posts a clear) and deletes it (home).
//
// A password-mode server: the operator (user 1) owns the agent and the host.

const origin = `http://localhost:${cardsPort}`;
const connectURL = `http://127.0.0.1:${cardsConnectPort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-cards-' + stamp;
const sid = 'cccccccc-' + stamp;
const agentName = 'e2e-cards-agent-' + stamp;
const agentToken = 'e2e-cards-agent-token-not-a-real-one-' + stamp;
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cards-'));
const tr = path.join(dir, sid + '.jsonl');
// A repository whose name appears nowhere else on the page: not in the host,
// the session, the window or the agent.
const repoName = 'quokkafixture';
const repoDir = path.join(dir, 'src', repoName);
// The shell window's repository, at its root.
const shellRepo = 'wombatfixture';
const shellRepoDir = path.join(dir, 'src', shellRepo);
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';
let agentMade = false;
let agentID = '';

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(240_000);
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, cardsPort, {
    MUSTER_MIRROR: '1',
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x4e).toString('base64'),
    MUSTER_CONNECT_PORT: String(cardsConnectPort),
    MUSTER_AGENT_CONNECT_URL: connectURL,
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  agentID = fix('agent', '1', agentName, agentToken);
  agentMade = true;
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  // The repository, and the subdirectory the window's Claude Code runs in.
  fs.mkdirSync(path.join(repoDir, '.git'), { recursive: true });
  fs.mkdirSync(path.join(repoDir, 'deploy', 'scripts'), { recursive: true });
  fs.mkdirSync(path.join(shellRepoDir, '.git'), { recursive: true });
  fs.writeFileSync(tr, JSON.stringify({ type: 'user', message: { role: 'user', content: 'rolling the fleet' } }) + '\n');
  // Claude Code idle at first; the first test puts it waiting while the list
  // is open.
  writeFileAtomic(tr + '.status', JSON.stringify({ status: 'idle', statusUpdatedAt: Date.now() - 60_000 }));
  const now = Math.floor(Date.now() / 1000);
  const windows = path.join(dir, 'windows');
  writeFileAtomic(
    windows,
    [
      `fleet|@5|5|1|claude|${now - 60}|${tr}||${path.join(repoDir, 'deploy', 'scripts')}`,
      `fleet|@6|6|0|zsh|${now - 120}|||${shellRepoDir}`,
      `notes|@7|0|1|vim|${now - 180}|`,
    ].join('\n') + '\n',
  );
  mirrorProc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
      MUSTER_MIRROR_E2E_REFRESH: '300ms',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  mirrorProc.stdout?.on('data', (d) => (mirrorLog += d));
  mirrorProc.stderr?.on('data', (d) => (mirrorLog += d));
});

test.afterAll(async () => {
  try {
    if (agentMade) fix('cleanup', '1', agentName);
  } finally {
    if (mirrorProc && mirrorProc.exitCode === null) {
      const done = new Promise((r) => mirrorProc!.once('exit', r));
      mirrorProc.kill('SIGTERM');
      await done;
    }
    await server?.stop();
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

async function ask(question: string): Promise<number> {
  const r = await fetch(connectURL + '/mcp', {
    method: 'POST',
    headers: { 'X-Muster-Token': agentToken, 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name: 'attention_ask', arguments: { question } } }),
  });
  expect(r.status, 'the /mcp door answered').toBe(200);
  const j = (await r.json()) as { result: { content: { text: string }[]; isError?: boolean } };
  expect(j.result.isError, j.result.content[0].text).toBeFalsy();
  return (JSON.parse(j.result.content[0].text) as { id: number }).id;
}

// reportWork is what ccd's work reporter sends (cmd/ccd/work.go): POST
// /agent/work on the connect listener, with the agent's own token.
async function reportWork(body: object, token = agentToken): Promise<number> {
  const r = await fetch(connectURL + '/agent/work', {
    method: 'POST',
    headers: { 'X-Muster-Token': token, 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  return r.status;
}

type P = import('@playwright/test').Page;
const row = (page: P, title: string) => page.locator(`#sidebar-agents-list a[data-nav-agent][title="${title}"]`);
const agentRow = (page: P) => page.locator(`#sidebar-agents-list a#nav-agent-${agentID}`);

// openSidebar shows the sidebar's list (the drawer, on this phone).
async function openSidebar(page: P) {
  await page.locator('#sidebar-open').click();
  await expect(page.locator('#sidebar-root')).toHaveAttribute('data-open', 'true');
}

test('each sidebar row shows its attention, live, and a tap on the row opens the agent', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page, origin);
  await ask('Ship the importer today?');
  await page.goto(origin + '/tasks');
  await openSidebar(page);
  const agentPill = agentRow(page).locator('[data-nav-attention]');
  await expect(agentPill).toHaveAttribute('data-count', '1', { timeout: 20_000 });
  await expect(agentPill).toHaveAttribute('data-kind', 'ask');
  await expect(agentPill).toContainText('❓');
  await expect(row(page, 'fleet:5'), mirrorLog).toBeVisible({ timeout: 30_000 });
  const winPill = row(page, 'fleet:5').locator('[data-nav-attention]');
  await expect(winPill, 'control: an idle window has no count').toHaveCount(0);
  let navigations = 0;
  page.on('framenavigated', (f) => {
    if (f === page.mainFrame()) navigations++;
  });

  // The window goes waiting at a prompt while the sidebar is open: its row's
  // count appears LIVE.
  writeFileAtomic(tr + '.status', JSON.stringify({ status: 'waiting', statusUpdatedAt: Date.now() - 1_000 }));
  await expect(winPill, mirrorLog).toHaveAttribute('data-kind', 'session', { timeout: 30_000 });
  await expect(winPill).toHaveAttribute('data-count', '1');
  await expect(winPill).toContainText('🔒');
  await expect(row(page, 'fleet:5'), 'a waiting row is marked').toHaveAttribute('data-nav-wait', '');

  // A second ask while it is open raises the agent's count too.
  await ask('Rotate the staging key too?');
  await expect(agentPill).toHaveAttribute('data-count', '2', { timeout: 15_000 });
  expect(navigations, 'the counts moved without a page load').toBe(0);

  // A tap on the row opens the agent's page (the count is not a link of its own).
  await Promise.all([page.waitForURL((u) => u.pathname === '/agents/' + agentName), agentRow(page).tap()]);
});

// What the palette finds an entry by: a row's data-switcher-words.
const words = (page: P, title: string) => row(page, title).getAttribute('data-switcher-words');

test("a mirrored window's row carries its repo, and the palette finds it by it", async ({ page }) => {
  test.setTimeout(60_000);
  await signIn(page, origin);
  await page.goto(origin + '/tasks');
  await openSidebar(page);
  await expect(row(page, 'fleet:5'), mirrorLog).toBeVisible({ timeout: 30_000 });
  await expect.poll(() => words(page, 'fleet:5'), { timeout: 30_000 }).toContain(repoName.toLowerCase());
  // The row's name is unchanged: its window, not its repo.
  await expect(row(page, 'fleet:5')).toContainText('fleet:5');
  // The palette, from the sidebar's magnifier, finds it by the repo alone.
  await page.locator('#sidebar-search').click();
  await page.locator('#switcher-search-input').fill(repoName);
  await expect(page.locator('#switcher-search-results [role="option"]')).toHaveCount(1);
  await expect(page.locator('#switcher-search-results [role="option"]').first()).toContainText('fleet:5');
});

test('a plain shell window carries its repo and its kind too', async ({ page }) => {
  test.setTimeout(60_000);
  await signIn(page, origin);
  await page.goto(origin + '/tasks');
  await openSidebar(page);
  // Every window keeps its row, a shell's included.
  await expect(row(page, 'fleet:6'), mirrorLog).toHaveCount(1, { timeout: 30_000 });
  await expect.poll(() => words(page, 'fleet:6'), { timeout: 30_000 }).toContain(shellRepo.toLowerCase());
  expect((await words(page, 'fleet:6'))!.split(' ')).toContain('shell');
  // The editor's pane has no working directory the host could read: no repo.
  await expect(row(page, 'notes:0')).toHaveCount(1);
  for (const r of [repoName, shellRepo]) expect((await words(page, 'notes:0')) ?? '').not.toContain(r.toLowerCase());
});

// LIVE WORK COUNTS ON A DISPATCHED AGENT (docs/design/agent-cards.md (a)):
// what a RUNNING agent's pod reports draws the chip in the agent page's header
// strip, LIVE while the page is open (no reload), and the count on its sidebar
// row; a report of nothing takes it off; a stranger's token is refused; and
// stopping the agent — from the page's menu — takes it off too, though its
// pod never posts a clear. (A report that simply stops expires after its TTL:
// the Go tests pin that, at agentWorkTTL, which is longer than an e2e run
// should wait.) The agent is started first, from the same menu — the noop
// provisioner then reports it live — because only a running agent wears a chip.
test("a dispatched agent's page shows the work its pod reports, live, and loses it; its menu starts and stops it", async ({ page }) => {
  test.setTimeout(90_000);
  await signIn(page, origin);
  await page.goto(origin + '/agents/' + agentName);
  const status = page.locator('#agent-status');
  const more = page.locator('#agent-actions-open');
  const menu = page.locator('#agent-actions');
  const chip = page.locator('#agent-strip [data-mirror-work] > [aria-hidden="true"]');
  await expect(status).toHaveAttribute('data-agent-status', /.+/);
  expect(await status.getAttribute('data-agent-status'), 'the agent starts out not running').not.toBe('running');
  await more.click();
  await menu.locator('[data-agent-action="Start"]').click();
  await expect
    .poll(async () => { await page.reload(); return status.getAttribute('data-agent-status'); }, { message: 'the agent runs', timeout: 30_000 })
    .toBe('running');
  await expect(chip, 'control: an agent that reported nothing has no chip').toHaveCount(0);
  let navigations = 0;
  page.on('framenavigated', (f) => {
    if (f === page.mainFrame()) navigations++;
  });
  expect(await reportWork({ subagents: 1 }, 'nobody-has-this-token-0123')).toBe(401);
  expect(await reportWork({ subagents: 3, shells: 2, shellsAtLeast: true })).toBe(204);
  await expect(chip).toHaveText('3 subagents · 2+ shells', { timeout: 15_000 });
  await expect(page.locator('#agent-strip [data-mirror-work] .sr-only')).toHaveText('3 subagents and at least 2 background shells running');
  expect(await reportWork({ shells: 1 })).toBe(204);
  await expect(chip).toHaveText('1 shell', { timeout: 15_000 });
  expect(await reportWork({})).toBe(204);
  await expect(chip).toHaveCount(0, { timeout: 15_000 });
  expect(navigations, 'the chip came and went without a page load').toBe(0);
  // The sidebar row carries the count, and the sentence as its name.
  expect(await reportWork({ subagents: 2 })).toBe(204);
  await expect(chip).toHaveText('2 subagents', { timeout: 15_000 });
  await openSidebar(page);
  await expect(agentRow(page).locator('[data-nav-work]')).toHaveAttribute('aria-label', '2 subagents running', { timeout: 15_000 });
  await page.locator('#sidebar-close').click();
  // Stopped from the menu with a report up: the pod is gone and posts no
  // clear, and the page loses the chip anyway; the menu offers Start again.
  await more.click();
  await menu.locator('[data-agent-action="Stop"]').click();
  await expect(status, 'the strip follows the stop').toHaveAttribute('data-agent-status', 'stopped', { timeout: 20_000 });
  await page.reload();
  await expect(chip, 'a stopped agent wears no chip').toHaveCount(0);
  await more.click();
  await expect(menu.locator('[data-agent-action="Start"]')).toBeVisible();
});

// DELETE, from the page's menu: two steps, and the answer sends the page
// home (the agent it showed is going). A throwaway agent of this test's own.
test("the agent page's menu deletes the agent in two steps and goes home", async ({ page }) => {
  test.setTimeout(60_000);
  const name = 'e2e-cards-del-' + stamp;
  fix('agent', '1', name, 'e2e-cards-del-token-not-a-real-one-' + stamp);
  let deleted = false;
  try {
    await signIn(page, origin);
    await page.goto(origin + '/agents/' + name);
    await page.locator('#agent-actions-open').click();
    const menu = page.locator('#agent-actions');
    await menu.locator('summary[data-agent-action="delete"]').click();
    const confirm = menu.locator('[data-agent-action="confirm-delete"]');
    await expect(confirm).toBeVisible();
    const resp = page.waitForResponse((r) => r.request().method() === 'DELETE');
    await confirm.click();
    expect((await resp).status()).toBeLessThan(300);
    deleted = true;
    // Home: anywhere but the deleted agent's page.
    await page.waitForURL((u) => !u.pathname.startsWith('/agents/' + name), { timeout: 15_000 });
    await expect
      .poll(async () => (await (await page.request.get(origin + '/ui/nav/agents')).text()).includes('/agents/' + name + '"') ||
        (await (await page.request.get(origin + '/ui/nav/agents')).text()).includes('/agents/' + name + '?'), { timeout: 15_000 })
      .toBe(false);
  } finally {
    if (!deleted) fix('rmagent', '1', name);
  }
});
