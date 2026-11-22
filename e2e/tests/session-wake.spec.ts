import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { binDir, dsn, repoRoot, sessionWakeMirrorMCPPort, sessionWakePort, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { writeFileAtomic } from '../lib/atomic';
import { withMCPSession } from '../lib/localmcp';

// A DECISION WAKES THE MIRRORED SESSION IT IS FOR (internal/api session_wake.go;
// docs/attention.md "Waking a mirrored session"), the operator's click path,
// every muster hop real:
//
//   a Claude Code session in a mirrored window asks through its host's loopback
//   MCP endpoint (attention_ask) and does NOT wait → the operator taps an
//   option on /attention → muster queues one message for that window's send
//   queue → the window is idle, so the queue pastes it through the tunnel into
//   the window's Claude Code pane (a real muster-mirror, -tags e2emirror, whose
//   tmux is played: the paste lands in <transcript>.pasted) → the Answered card
//   says "Sent to …".
//
// ⚠ WHAT IS PLAYED: tmux and Claude Code (the e2emirror build reads the window
// list and the session's status from files this spec writes, and records a
// paste instead of typing it). The ask, the answer form, the queue, its
// dispatcher, the gate, the tunnel and the card are muster's own.
//
// A password-mode server: the operator (user 1) owns the host.

const origin = `http://localhost:${sessionWakePort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-wake-' + stamp;
const sid = 'cccccccc-' + stamp;
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'session-wake-'));
const tr = path.join(dir, sid + '.jsonl');
const fakeTMUX = '/tmp/muster-e2e-fake-tmux/default,1,0';
const mcpSecretFile = path.join(dir, 'mcp-secret');
const localMCP = `http://127.0.0.1:${sessionWakeMirrorMCPPort}/mcp`;
const pane = '%e2e-work-@1';
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';

const read = (f: string) => (fs.existsSync(f) ? fs.readFileSync(f, 'utf8') : '');

test.beforeAll(async () => {
  test.setTimeout(240_000);
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, sessionWakePort, {
    MUSTER_MIRROR: '1',
    MUSTER_SEND_QUEUE_IDLE: '1s',
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  fs.writeFileSync(tr, JSON.stringify({ type: 'user', message: { role: 'user', content: 'migrating the billing tables' } }) + '\n');
  // Claude Code idle, as its sessions file says it — written a minute ago:
  // the session asked and ended its turn.
  writeFileAtomic(tr + '.status', JSON.stringify({ status: 'idle', statusUpdatedAt: Date.now() - 60_000 }));
  const windows = path.join(dir, 'windows');
  writeFileAtomic(windows, `work|@1|0|1|claude|${Math.floor(Date.now() / 1000) - 60}|${tr}\n`);
  mirrorProc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
      MUSTER_MIRROR_MCP_LISTEN: `127.0.0.1:${sessionWakeMirrorMCPPort}`,
      MUSTER_MIRROR_MCP_SECRET_FILE: mcpSecretFile,
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
  fs.rmSync(dir, { recursive: true, force: true });
});

// ask is the session's attention_ask through its host's loopback endpoint;
// its id.
async function ask(question: string, options: string[]): Promise<number> {
  await expect.poll(() => fs.existsSync(mcpSecretFile), { message: 'the host wrote its MCP secret', timeout: 20_000 }).toBe(true);
  const secret = fs.readFileSync(mcpSecretFile, 'utf8').trim();
  const auth = { Authorization: 'Bearer ' + secret, 'X-Muster-Pane': pane, 'X-Muster-Tmux': fakeTMUX };
  let id = 0;
  await expect
    .poll(
      async () => {
        const r = await fetch(localMCP, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', ...(await withMCPSession(localMCP, auth)) },
          body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name: 'attention_ask', arguments: { question, options } } }),
        });
        const j = (await r.json()) as { result?: { isError?: boolean; content: { text: string }[] } };
        if (!j.result || j.result.isError) return 0;
        id = (JSON.parse(j.result.content[0].text) as { id: number }).id;
        return id;
      },
      { message: `the session's attention_ask was taken (host log:\n${mirrorLog})`, timeout: 30_000 },
    )
    .toBeGreaterThan(0);
  return id;
}

for (const vp of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1440, height: 900 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`on a ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });
    test('the operator answers a mirrored session\'s ask and the session receives it, once', async ({ page }) => {
      test.setTimeout(150_000);
      const question = `Drop the legacy invoices table (${vp.name})?`;
      const answer = `drop it (${vp.name})`;
      const id = await ask(question, [answer, 'keep it']);
      const before = read(tr + '.pasted');

      await signIn(page, origin);
      await page.goto(origin + '/attention');
      const item = page.locator(`#ask-${id}`);
      await expect(item.locator('[data-attention-text]')).toHaveText(question);
      await Promise.all([page.waitForURL(origin + '/attention'), item.getByRole('button', { name: answer }).click()]);
      // The card settles in place (internal/api asks_done.go).
      await expect(page.locator(`#ask-${id}`)).toHaveAttribute('data-asks-settled', '');

      // The window's Claude Code pane receives muster's line and the answer,
      // fenced — once.
      const line = `muster: you answered ask #${id}, which this session asked.`;
      await expect
        .poll(() => read(tr + '.pasted').slice(before.length), { message: `the session received the answer (host log:\n${mirrorLog})`, timeout: 45_000 })
        .toContain(line);
      const got = read(tr + '.pasted').slice(before.length);
      expect(got).toContain('<<<attention-answer untrusted nonce=');
      expect(got).toContain(`|   ${answer}`);
      expect(got, 'the session\'s own question is not typed back').not.toContain(question);
      // A turn ends after the paste, as Claude Code's would: a newer status.
      writeFileAtomic(tr + '.status', JSON.stringify({ status: 'idle', statusUpdatedAt: Date.now() }));

      // Where it went, on the Answered tab.
      await page.getByRole('tab', { name: 'Done', exact: true }).click();
      const answered = page.locator(`#answered-${id}`);
      await expect(answered).toHaveAttribute('data-delivery', 'wake-delivered', { timeout: 30_000 });
      await expect(answered.locator('[data-attention-delivery-pill]')).toHaveText('Sent to the session');
      await expect(answered.locator('[data-attention-delivery]')).toHaveText(/^Sent to .+\.$/);
      await expect(answered.locator('[data-attention-delivery-pill]')).toBeVisible();

      // Exactly once: nothing more arrives over the next queue passes.
      await page.waitForTimeout(4_000);
      const all = read(tr + '.pasted').slice(before.length);
      expect(all.split(line).length - 1, 'pasted exactly once').toBe(1);
    });
  });
}
