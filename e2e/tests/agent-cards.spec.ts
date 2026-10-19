import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { binDir, cardsConnectPort, cardsPort, dsn, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// THE AGENTS LIST'S CARDS, the operator's click path, every hop real:
//
//   - ATTENTION BADGE: an agent asks over the real /mcp door, and a mirrored
//     Claude Code window (a real muster-mirror over the tunnel, -tags
//     e2emirror) waits at a prompt. Each card shows a badge with its count and
//     the most urgent kind's icon; a second ask raises the agent's count LIVE
//     (no reload); tapping the badge opens that item on /attention — never the
//     card.
//   - REPO CHIP: the window's Claude Code works in a SUBDIRECTORY of a git
//     repository; its host names the repository (the git top level's name) and
//     the card shows it — and typing the repo's name in the filter finds it.
//   - CLAUDE ONLY: the host's two other windows (a shell, an editor) draw no
//     card; the host's tab says "2 other windows".
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
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';
let agentMade = false;

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
  fix('agent', '1', agentName, agentToken);
  agentMade = true;
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  // The repository, and the subdirectory the window's Claude Code runs in.
  fs.mkdirSync(path.join(repoDir, '.git'), { recursive: true });
  fs.mkdirSync(path.join(repoDir, 'deploy', 'scripts'), { recursive: true });
  fs.writeFileSync(tr, JSON.stringify({ type: 'user', message: { role: 'user', content: 'rolling the fleet' } }) + '\n');
  // Claude Code idle at first; the first test puts it waiting while the list
  // is open.
  fs.writeFileSync(tr + '.status', JSON.stringify({ status: 'idle', statusUpdatedAt: Date.now() - 60_000 }));
  const now = Math.floor(Date.now() / 1000);
  const windows = path.join(dir, 'windows');
  fs.writeFileSync(
    windows,
    [
      `fleet|@5|5|1|claude|${now - 60}|${tr}||${path.join(repoDir, 'deploy', 'scripts')}`,
      `fleet|@6|6|0|zsh|${now - 120}|`,
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

async function signIn(page: import('@playwright/test').Page) {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

const mirrorCard = (page: import('@playwright/test').Page) => page.locator('#agents-list > article[data-mirror-card]').filter({ hasText: 'fleet:5' });

test('each card shows its attention badge, live, and tapping it opens the item in the queue', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page);
  const first = await ask('Ship the importer today?');
  await page.goto(origin + '/agents');
  const agentCard = page.locator('#agents-list > article').filter({ hasText: agentName });
  const agentBadge = agentCard.locator('[data-attention-badge]');
  await expect(agentBadge).toHaveAttribute('data-count', '1', { timeout: 20_000 });
  await expect(agentBadge).toHaveAttribute('data-kind', 'ask');
  await expect(agentBadge).toContainText('❓');
  await expect(mirrorCard(page), mirrorLog).toBeVisible({ timeout: 30_000 });
  const winBadge = mirrorCard(page).locator('[data-attention-badge]');
  await expect(winBadge, 'control: an idle window has no badge').toHaveCount(0);
  let navigations = 0;
  page.on('framenavigated', (f) => {
    if (f === page.mainFrame()) navigations++;
  });

  // The window goes waiting at a prompt while the list is open: its card's
  // badge appears LIVE. Nothing but the queue's own event says so — a status
  // change draws no card, so no list nudge carries it.
  fs.writeFileSync(tr + '.status', JSON.stringify({ status: 'waiting', statusUpdatedAt: Date.now() - 1_000 }));
  await expect(winBadge, mirrorLog).toHaveAttribute('data-kind', 'session', { timeout: 30_000 });
  await expect(winBadge).toHaveAttribute('data-count', '1');
  await expect(winBadge).toContainText('🔒');

  // A second ask while the list is open raises the agent's count too.
  const second = await ask('Rotate the staging key too?');
  await expect(agentBadge).toHaveAttribute('data-count', '2', { timeout: 15_000 });
  expect(navigations, 'the badge moved without a page load').toBe(0);
  await expect(agentBadge).toHaveAttribute('href', `/attention#ask-${second}`);

  // Tapping the badge opens THAT item on the queue — not the agent's page.
  await Promise.all([page.waitForURL((u) => u.pathname === '/attention'), agentBadge.tap()]);
  expect(new URL(page.url()).hash).toBe(`#ask-${second}`);
  await expect(page.locator(`#ask-${second}`)).toBeVisible();
  await expect(page.locator(`#ask-${first}`)).toBeVisible();
});

test('typing the repo name in the filter finds the mirrored card, which shows it as a chip', async ({ page }) => {
  test.setTimeout(60_000);
  await signIn(page);
  await page.goto(origin + '/agents');
  const card = mirrorCard(page);
  await expect(card.locator('[data-mirror-repo]'), mirrorLog).toHaveText(new RegExp(`repo\\s*${repoName}`), { timeout: 30_000 });
  // The card's name is unchanged: its window, not its repo.
  await expect(card.locator('[data-mirror-name]')).toHaveText('fleet:5');
  const search = page.locator('#agents-search');
  await search.fill(repoName.slice(0, 6));
  await expect(card).toBeVisible();
  await expect(page.locator('#agents-list > article').filter({ hasText: agentName })).toBeHidden();
  await expect(page.locator('#agents-count')).toHaveText(/^1 of \d+$/);
  // The field token finds it too.
  await search.fill(`repo:${repoName}`);
  await expect(card).toBeVisible();
  await expect(page.locator('#agents-count')).toHaveText(/^1 of \d+$/);
});

test('a host tab says how many of its windows have no card', async ({ page }) => {
  test.setTimeout(60_000);
  await signIn(page);
  await page.goto(origin + '/agents');
  await expect(mirrorCard(page), mirrorLog).toBeVisible({ timeout: 30_000 });
  // Claude only: the shell and the editor are not cards.
  await expect(page.locator('#agents-list > article[data-mirror-card]').filter({ hasText: 'fleet:6' })).toHaveCount(0);
  await expect(page.locator('#agents-list > article[data-mirror-card]').filter({ hasText: 'notes:0' })).toHaveCount(0);
  const tab = page.locator(`#agents-hosts [role="tab"][data-host="${host}"]`);
  await expect(tab.locator('[data-tab-other]')).toHaveText('2 other windows');
  await expect(tab.locator('[data-tab-other]')).toBeVisible();
  // Its count of cards is the one Claude window; All carries no note.
  await expect(tab.locator('[data-tab-count]')).toHaveText('1');
  await expect(page.locator('#agents-host-all [data-tab-other]')).toBeHidden();
});
