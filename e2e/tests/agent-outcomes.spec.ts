import http from 'node:http';
import { execFileSync } from 'node:child_process';
import { Page, request as pwRequest } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { hookToken, outcomesCatalogPort, outcomesPort, outcomesRuntimePort, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// EVERY DISPATCHED AGENT ENDS IN A VISIBLE OUTCOME (internal/api agent_outcome.go,
// agent_watchdog.go; docs/provisioner.md#how-an-agents-work-ends), in a real
// browser on a real server, on a phone and on a desktop:
//
//   - an agent dispatched with a model's DISPLAY NAME has it resolved to the
//     catalogue's id; its first turn fails mid-stream with the runtime's own
//     "Unknown model" (the incident's shape) → its page shows `error` with the
//     reason, its sidebar row says "failed", the outcome API says failed, and
//     the owner's device was sent exactly ONE "<agent> failed: …" notification;
//   - an agent whose runtime takes its first turn and then says nothing → the
//     3 s watchdog calls it `stuck` ("no reply since kickoff"): the page's strip
//     and notice say so, and one "<agent> is stuck: …" notification was sent;
//   - a model the catalogue does not have is refused before any agent exists.
//
// ⚠ WHAT IS PLAYED: the agent runtime (an http server here, every agent's
// endpoint pointed at it by the e2eterm build's MUSTER_E2E_AGENT_ENDPOINT), the
// model catalogue (MUSTER_E2E_OPENROUTER_MODELS), and the owner's push device (a
// subscription whose keys cannot encrypt, so nothing leaves this machine — the
// notification is observed where muster sends it, its `push: delivered tag=…`
// line). The dispatch, the kickoff, the gateway transport, the outcome tracker,
// the watchdog, the push pipeline and every page are muster's own.

const origin = `http://localhost:${outcomesPort}`;
const FAILURE = 'FailoverError: Unknown model: openai/Acme v9 ultra';
let server: Running | undefined;
let serverEnv: Record<string, string> = {};
let runtime: http.Server | undefined;
let catalog: http.Server | undefined;
const held: http.ServerResponse[] = [];

const sse = (type: string, data: Record<string, unknown>) => `event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`;

test.beforeAll(async () => {
  test.setTimeout(180_000);
  runtime = http.createServer((req, res) => {
    let body = '';
    req.on('data', (d) => (body += d));
    req.on('end', () => {
      if (req.method !== 'POST' || !(req.url || '').endsWith('/v1/responses')) {
        res.statusCode = 404;
        return res.end();
      }
      res.setHeader('Content-Type', 'text/event-stream');
      if (body.includes('OUTCOME-HANG')) {
        // Took the turn, and says nothing: the watchdog's case.
        res.flushHeaders();
        held.push(res);
        return;
      }
      if (body.includes('OUTCOME-FAIL')) {
        res.write(sse('response.created', { response: { status: 'in_progress' } }));
        return res.end(sse('response.failed', { response: { status: 'failed', error: { code: 'server_error', message: FAILURE } } }));
      }
      res.end(sse('response.completed', { response: { status: 'completed', output: [{ type: 'message', content: [{ type: 'output_text', text: 'Done.' }] }] } }));
    });
  });
  await new Promise<void>((r) => runtime!.listen(outcomesRuntimePort, '127.0.0.1', () => r()));
  catalog = http.createServer((_req, res) => {
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ data: [
      { id: 'acme/acme-v4.1-flash', name: 'Acme: Acme V4.1 Flash' },
      { id: 'acme/acme-v4.1-pro', name: 'Acme: Acme V4.1 Pro' },
    ] }));
  });
  await new Promise<void>((r) => catalog!.listen(outcomesCatalogPort, '127.0.0.1', () => r()));
  const vapid: Record<string, string> = { MUSTER_VAPID_SUBJECT: 'mailto:e2e@example.com' };
  const out = execFileSync(serverBinary(versionTerm), ['vapid', 'generate'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
  for (const line of out.trim().split('\n')) {
    const i = line.indexOf('=');
    vapid[line.slice(0, i)] = line.slice(i + 1);
  }
  serverEnv = {
    ...vapid,
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_GATEWAY: 'hooks-sha256',
    MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
    MUSTER_AGENT_RUNTIME_CONFIG: '{}',
    MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
    MUSTER_AGENT_OPENROUTER_KEY: 'sk-or-v1-FAKE-e2e-outcomes-key-000000',
    MUSTER_E2E_AGENT_ENDPOINT: `http://127.0.0.1:${outcomesRuntimePort}`,
    MUSTER_E2E_OPENROUTER_MODELS: `http://127.0.0.1:${outcomesCatalogPort}/models`,
    // Seconds, not the 5 min default, so a browser test can watch it fire.
    MUSTER_AGENT_STUCK_NO_REPLY: '3s',
    MUSTER_AGENT_WATCHDOG_INTERVAL: '1s',
  };
  server = await startServer(versionTerm, outcomesPort, serverEnv);
});

test.afterAll(async () => {
  for (const res of held) res.end();
  await server?.stop();
  await new Promise<void>((r) => (runtime ? runtime.close(() => r()) : r()));
  await new Promise<void>((r) => (catalog ? catalog.close(() => r()) : r()));
});

interface AgentRow {
  id: number;
  name: string;
  status: string;
  model: string;
  outcome: string;
  outcomeReason: string;
  errorMessage: string;
}

async function agentRows(): Promise<AgentRow[]> {
  const ctx = await pwRequest.newContext({ baseURL: origin, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
  const r = await ctx.get('/api/agents');
  expect(r.status(), await r.text()).toBe(200);
  const rows = (await r.json()) as AgentRow[];
  await ctx.dispose();
  return rows;
}

// dispatch starts a gateway agent from the dispatch form's route, as the
// signed-in owner, and returns its row.
async function dispatch(page: Page, model: string, note: string): Promise<AgentRow> {
  const before = new Set((await agentRows()).map((a) => a.id));
  const r = await page.request.post(origin + '/agents', {
    form: { action: 'dispatch', kind: 'gateway', model, note_text: note },
    headers: { Origin: origin, 'HX-Request': 'true' },
  });
  expect(r.status(), await r.text()).toBe(200);
  const made = (await agentRows()).filter((a) => !before.has(a.id));
  expect(made, 'the dispatch made one agent').toHaveLength(1);
  return made[0];
}

// outcomePushes counts the notifications muster sent for agent id's outcome.
const outcomePushes = (id: number) =>
  (server!.log().match(new RegExp(`push: delivered tag=agent-${id}-outcome owner=\\d+ to \\d+/1 device`, 'g')) ?? []).length;

// openSidebar shows the sidebar: on a phone it is a drawer, shut until its
// hamburger opens it.
async function openSidebar(page: Page, phone: boolean) {
  if (!phone) return;
  await page.locator('#sidebar-open').click();
  await expect(page.locator('#sidebar-root')).toHaveAttribute('data-open', 'true');
}

// withDevice runs body with the owner's push device subscribed, and removes it
// after, pass or fail: the e2e database is shared, and a device left behind is
// counted by tests/push.spec.ts's account page.
const device = 'https://fcm.googleapis.com/fcm/send/e2e-outcomes-device';
async function withDevice(page: Page, body: () => Promise<void>) {
  const r = await page.request.post(origin + '/api/push/subscribe', {
    data: { endpoint: device, keys: { p256dh: 'BStubClientKeyForE2E', auth: 'stubauthsecret' } },
    headers: { Origin: origin },
  });
  expect(r.status(), await r.text()).toBe(200);
  try {
    await body();
  } finally {
    const u = await page.request.post(origin + '/api/push/unsubscribe', { data: { endpoint: device }, headers: { Origin: origin } });
    expect(u.status(), await u.text()).toBe(200);
  }
}

for (const vp of [
  { label: 'phone', viewport: { width: 390, height: 844 }, touch: true },
  { label: 'desktop', viewport: { width: 1280, height: 900 }, touch: false },
]) {
  test.describe(vp.label, () => {
    test.use({ viewport: vp.viewport, hasTouch: vp.touch, isMobile: vp.touch, storageState: { cookies: [], origins: [] } });

    test('a model the catalogue does not have is refused before any agent exists', async ({ page }) => {
      await signIn(page, origin);
      const before = (await agentRows()).length;
      const refused = await page.request.post(origin + '/agents', {
        form: { action: 'dispatch', kind: 'gateway', model: 'Acme v9 ultra', note_text: 'never started' },
        headers: { Origin: origin, 'HX-Request': 'true' },
      });
      expect(refused.status()).toBe(409);
      expect(await refused.text()).toContain('Closest ids: openrouter/acme/');
      expect((await agentRows()).length, 'a refused model created no agent').toBe(before);
    });

    test('a dispatched agent whose first turn fails shows error with the reason, and its owner is told once', async ({ page }) => {
      test.setTimeout(90_000);
      await signIn(page, origin);
      await withDevice(page, async () => {

        // The incident's shape: a model's display name, then the runtime fails the
        // turn mid-stream with its own "Unknown model".
        const a = await dispatch(page, 'Acme v4.1 flash', `OUTCOME-FAIL ${vp.label}: summarise the repo`);
        await expect.poll(async () => (await agentRows()).find((x) => x.id === a.id)?.outcome, { timeout: 30_000 }).toBe('failed');
        const row = (await agentRows()).find((x) => x.id === a.id)!;
        expect(row.status).toBe('error');
        expect(row.errorMessage).toContain(FAILURE);
        expect(row.outcomeReason).toContain(FAILURE);
        // The display name was resolved to the catalogue's id.
        expect(row.model).toBe('openrouter/acme/acme-v4.1-flash');

        // The agent's page: the status is error, and the reason is on the page.
        await page.goto(origin + '/agents/' + a.name);
        await expect(page.locator('#agent-status')).toHaveAttribute('data-agent-status', 'error');
        await expect(page.locator('#agent-notices')).toContainText('Unknown model: openai/Acme v9 ultra');
        // The sidebar row says it (on a phone, inside the drawer: opened first,
        // since a shut drawer does not fetch its list).
        await openSidebar(page, vp.touch);
        await expect(page.locator(`#nav-agent-${a.id} [data-nav-outcome="failed"]`)).toBeVisible();
        await expect(page.locator(`#nav-agent-${a.id} [data-nav-outcome="failed"]`)).toHaveAttribute('title', /Unknown model/);

        // Exactly one notification.
        await expect.poll(() => outcomePushes(a.id), { timeout: 10_000 }).toBe(1);
        await page.waitForTimeout(2500); // two watchdog sweeps
        expect(outcomePushes(a.id), 'one notification per change').toBe(1);
      });
    });

    test('a dispatched agent that goes quiet after its kickoff shows stuck', async ({ page }) => {
      test.setTimeout(90_000);
      await signIn(page, origin);
      await withDevice(page, async () => {
        const heldBefore = held.length;
        const a = await dispatch(page, 'openrouter/acme/acme-v4.1-pro', `OUTCOME-HANG ${vp.label}: think about it`);
        // The runtime took the first turn and holds it: while the turn is open
        // in muster it is activity, never "no reply since kickoff".
        await expect.poll(() => held.length, { timeout: 30_000 }).toBe(heldBefore + 1);
        await page.waitForTimeout(4500); // past the 3 s threshold, sweeps every 1 s
        expect((await agentRows()).find((x) => x.id === a.id)?.outcome, 'an open turn is not stuck').toBe('running');
        // muster restarts under the held turn: the turn is no longer open in any
        // muster, nothing shows the agent alive, and it never replied — stuck.
        await server!.stop();
        server = await startServer(versionTerm, outcomesPort, serverEnv);
        await page.goto(origin + '/agents/' + a.name);
        await expect.poll(async () => (await agentRows()).find((x) => x.id === a.id)?.outcome, { timeout: 30_000 }).toBe('stuck');
        // Live: the open page redraws its strip and notices on agent.changed.
        const badge = page.locator('#agent-strip [data-agent-outcome="stuck"]');
        await expect(badge).toBeVisible({ timeout: 15_000 });
        await expect(badge).toHaveText('stuck');
        await expect(page.locator('#agent-notices [data-agent-outcome-notice="stuck"] [data-agent-outcome-reason]')).toHaveText(
          'no reply since kickoff',
        );
        // Its pod is still up: the status is not error.
        await expect(page.locator('#agent-status')).not.toHaveAttribute('data-agent-status', 'error');
        await openSidebar(page, vp.touch);
        await expect(page.locator(`#nav-agent-${a.id} [data-nav-outcome="stuck"]`)).toBeVisible();
        await expect.poll(() => outcomePushes(a.id), { timeout: 10_000 }).toBe(1);
        await page.waitForTimeout(2500);
        expect(outcomePushes(a.id), 'one notification per change').toBe(1);
        // Control: an agent that is not stuck draws no badge.
        const other = await page.context().newPage();
        const failed = (await agentRows()).find((x) => x.outcome === 'failed')!;
        await other.goto(origin + '/agents/' + failed.name);
        await expect(other.locator('#agent-strip')).toBeVisible();
        await expect(other.locator('[data-agent-outcome="stuck"]')).toHaveCount(0);
        await other.close();
      });
    });
  });
}
