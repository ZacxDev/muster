import { request as pwRequest, Page } from '@playwright/test';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { hookToken, provPort, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { openDispatchSheet } from '../lib/dispatch';

// "Provisioning agent…" must leave an OPEN agent page when the agent comes up
// without sending a message — and "No messages yet" must take its place — with
// no reload. Measured before the fix: the row is rendered server-side from the
// status at page load and only a persisted (chat.reply) or streamed message ever
// re-rendered it, so an agent that reached `running` silently kept the row on
// screen for ever while a fresh load showed the empty state.
//
// Against a REAL muster-server (the e2eterm build over the noop driver) with its
// readiness HOLD armed: every instance reports not-ready until the test creates
// a file named after it (cmd/muster-server/e2eterm.go). The agent is saved with
// no note, Started (the Start route the UI posts to), the page is opened while it
// provisions, and only THEN is the instance released — so the provisioning →
// running move is made by the shipped status promoter (internal/agentstatus) and
// announced by its real agent.changed broadcast, never by the test.
//
// The Start is the agent page's own (its ⋯ menu: internal/ui/agent_actions.go;
// the Agents list's card had it until that page was retired), and the page's
// status strip follows both moves live: provisioning on the Start, running on
// the promotion, with the menu's action turning to Stop.
test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

const origin = `http://localhost:${provPort}`;
let server: Running | undefined;
let holdDir = '';

test.beforeAll(async () => {
  holdDir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-e2e-hold-'));
  server = await startServer(versionTerm, provPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-prov-alpha',
    MUSTER_E2E_HOLD_READY_DIR: holdDir,
  });
});
test.afterAll(async () => {
  await server?.stop();
  if (holdDir) fs.rmSync(holdDir, { recursive: true, force: true });
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

async function storedStatus(name: string): Promise<string> {
  return (await agentRows()).find((a) => a.name === name)?.status ?? '(missing)';
}

async function login(page: Page) {
  await signIn(page, origin);
}

// saveAgent creates one agent of the kind with NO note through the dispatch
// sheet's "Save for later" (the sidebar's "+") and returns its row (diffed, so
// an earlier run's rows cannot answer). The save opens the new agent's page.
async function saveAgent(page: Page, kind: 'gateway' | 'claude-code'): Promise<AgentRow> {
  const before = new Set((await agentRows()).map((a) => a.name));
  await page.goto(origin + '/tasks');
  await openDispatchSheet(page);
  const sheet = page.locator('#agent-modal form');
  await expect(sheet.locator('[data-kind-picker]')).toBeVisible();
  await sheet.locator(`[data-kind-option="${kind}"]`).click();
  await sheet.locator('textarea[name="note_text"]').fill('');
  const resp = page.waitForResponse((r) => r.url().endsWith('/agents') && r.request().method() === 'POST');
  await sheet.locator('button[value="save"]').click();
  expect((await resp).status()).toBe(200);
  const created = (await agentRows()).filter((a) => !before.has(a.name));
  expect(created.length, 'exactly one agent created').toBe(1);
  await page.waitForURL((u) => u.pathname === '/agents/' + created[0].name, { timeout: 15_000 });
  return created[0];
}

for (const kind of ['gateway', 'claude-code'] as const) {
  test(`a ${kind} agent that comes up with no message clears "Provisioning agent…" and shows the empty state live`, async ({ page }) => {
    // The promoter ticks every 10 s; the release-to-promotion wait is bounded by it.
    test.setTimeout(90_000);
    await login(page);
    const agent = await saveAgent(page, kind);

    // Started from the page's ⋯ menu.
    const status = page.locator('#agent-status');
    const menu = page.locator('#agent-actions');
    await page.locator('#agent-actions-open').click();
    const started = page.waitForResponse((r) => r.url().endsWith(`/agents/${agent.id}/start`) && r.request().method() === 'POST');
    await menu.locator('[data-agent-action="Start"]').click();
    expect((await started).status()).toBe(200);
    // Start runs in the background; it stores `provisioning` once the instance exists.
    await expect.poll(() => storedStatus(agent.name), { timeout: 15_000 }).toBe('provisioning');
    // The strip follows (the Start's agents:changed and the stream's agent.changed).
    await expect(status, 'the strip shows provisioning without a reload').toHaveAttribute('data-agent-status', 'provisioning', { timeout: 15_000 });

    await page.goto(`${origin}/agents/${agent.name}`);
    const indicator = page.locator('#chat-provisioning');
    const empty = page.locator('#chat-log [data-chat-empty]');
    await expect(indicator).toBeVisible();
    await expect(indicator).toContainText('Provisioning agent…');
    await expect(empty).toHaveCount(0);

    // A marker that a reload would wipe: the assertions below must hold on THIS
    // document, not on a fresh one (a fresh load was always correct).
    await page.evaluate(() => ((window as unknown as { __noReload: number }).__noReload = 1));

    // Still held: the indicator is right to be there. Release the instance.
    expect(await storedStatus(agent.name)).toBe('provisioning');
    fs.writeFileSync(path.join(holdDir, agent.name), '');

    // The control: the promoter really moved the stored status (and so really
    // broadcast). Without this, a red below could be "nothing happened" rather
    // than "the page did not react".
    await expect.poll(() => storedStatus(agent.name), { timeout: 30_000 }).toBe('running');

    await expect(indicator).toHaveCount(0, { timeout: 10_000 });
    await expect(empty).toBeVisible();
    await expect(empty).toContainText('No messages yet');
    expect(await page.evaluate(() => (window as unknown as { __noReload?: number }).__noReload)).toBe(1);
    // The strip and the menu follow the promotion on this document too.
    await expect(status, 'the strip shows running without a reload').toHaveAttribute('data-agent-status', 'running', { timeout: 15_000 });
    await page.locator('#agent-actions-open').click();
    await expect(menu.locator('[data-agent-action="Stop"]'), 'the menu offers Stop').toBeVisible();
    await expect(menu.locator('[data-agent-action="Start"]'), 'Start is not offered while it runs').toBeHidden();
    await page.keyboard.press('Escape');

    // Nothing scrolls sideways at 412px.
    const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(sw).toBeLessThanOrEqual(cw);
  });
}
