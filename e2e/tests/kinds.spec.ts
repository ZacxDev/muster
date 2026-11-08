import { request as pwRequest, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { hookToken, kindsPort, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { createAgent, postAgents } from '../lib/seed';
import { openAgentMenu } from '../lib/agent-menu';

// ccAccount is the agent page's Claude account (its plain account name,
// data-cc-account) — an item of the header's ⋯ menu since PR #527 (a claude-code
// agent's page has one header row: agent_header_folded.go); the menu's
// Terminal item beside it is what says the kind. Open the menu (openAgentMenu)
// before asking whether it is visible.
const ccAccount = (page: Page, account: string) => page.locator(`#agent-actions [data-cc-account="${account}"]`);


// Agent kinds and the Claude account pin, against a REAL muster-server with
// the claude-code kind enabled over the noop driver, read back on the agent's
// page at a phone width (412×915). Agents are created stopped through POST
// /api/agents (lib/seed.ts) — the route that replaced the dispatch form and its
// kind picker, which docs/design/chief-dispatch.md phase 2 deleted — so the row
// is made and nothing is provisioned: no pod, no image and no token is ever
// used. The tokens below are obvious fakes and never leave this process.
test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

const origin = `http://localhost:${kindsPort}`;
let server: Running | undefined;

test.beforeAll(async () => {
  server = await startServer(versionA, kindsPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha,beta',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-alpha',
    MUSTER_AGENT_CC_TOKEN_BETA: 'sk-ant-oat01-FAKE-e2e-beta',
  });
});
test.afterAll(async () => {
  await server?.stop();
});

interface AgentRow {
  id: number;
  name: string;
  kind: string;
  ccAccount: string;
  workerPool: string;
}

async function agentRows(): Promise<AgentRow[]> {
  const ctx = await pwRequest.newContext({ baseURL: origin, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
  const r = await ctx.get('/api/agents');
  expect(r.status(), await r.text()).toBe(200);
  const rows = (await r.json()) as AgentRow[];
  await ctx.dispose();
  return rows;
}

test('a Claude Code agent lands on a pinned or pooled account, a pool worker in its pool, and Standard stays the default', async ({ page }) => {
  await signIn(page, origin);

  // save creates one stopped agent and returns the ONE row it created (the set
  // of rows is diffed, so a row left by an earlier run cannot answer).
  const save = async (opts: Parameters<typeof createAgent>[1]): Promise<AgentRow> => {
    const before = new Set((await agentRows()).map((a) => a.name));
    const made = await createAgent(origin, opts);
    const created = (await agentRows()).filter((a) => !before.has(a.name));
    expect(created.length, 'exactly one agent created').toBe(1);
    expect(created[0].name).toBe(made.name);
    return created[0];
  };

  // 1. Claude Code + a pinned account.
  const pinned = await save({ kind: 'claude-code', cc_account: 'beta', prompt: 'e2e kinds: pinned to beta' });

  // 2. Claude Code with the pool choosing: no marks and no LIVE agent anywhere
  //    (a stopped agent is not live), so the name tiebreak picks alpha — NOT the
  //    beta pinned above, which is the control that this one was not pinned.
  const pooled = await save({ kind: 'claude-code', prompt: 'e2e kinds: pool choice' });

  // 2b. Claude Code in a worker pool. The server is the only check on the
  //     name: a name it refuses is a 409 with its sentence, and no row is made.
  const beforeBad = (await agentRows()).length;
  const refused = await postAgents(origin, { kind: 'claude-code', worker_pool: '-bad', prompt: 'e2e kinds: bad pool' });
  expect(refused.status, refused.text).toBe(409);
  expect(JSON.parse(refused.text).error).toContain(`worker_pool "-bad" is not 1-63 of [a-z0-9-], not starting with '-'`);
  expect((await agentRows()).length, 'a refused pool creates no agent').toBe(beforeBad);
  const worker = await save({ kind: 'claude-code', worker_pool: 'reviewers', prompt: 'e2e kinds: pool worker' });

  // 3. No kind named: a gateway agent with no account and no pool.
  const standard = await save({ prompt: 'e2e kinds: standard' });

  expect([pinned.kind, pinned.ccAccount]).toEqual(['claude-code', 'beta']);
  expect([pooled.kind, pooled.ccAccount]).toEqual(['claude-code', 'alpha']);
  expect([standard.kind, standard.ccAccount]).toEqual(['gateway', '']);
  expect([worker.kind, worker.ccAccount, worker.workerPool]).toEqual(['claude-code', 'alpha', 'reviewers']);
  expect([pinned.workerPool, pooled.workerPool, standard.workerPool]).toEqual(['', '', '']);

  // 4. The agent's own page (where its retired list card's chips went: the
  //    header's ⋯ menu) says which kind and account, and nothing on the
  //    page carries a token.
  //    (Since PR #527 those are items of the header's ⋯ menu.)
  await page.goto(origin + '/agents/' + pinned.name);
  await expect(ccAccount(page, 'beta')).toHaveText('beta');
  await openAgentMenu(page);
  await expect(ccAccount(page, 'beta')).toBeVisible();
  await expect(page.locator('[data-worker-pool]'), 'a worker pinned to no pool shows no pool chip').toHaveCount(0);
  expect(await page.content()).not.toContain('sk-ant-oat01');
  // …and a pool worker's page says which pool.
  await page.goto(origin + '/agents/' + worker.name);
  await expect(ccAccount(page, 'alpha')).toHaveText('alpha');
  await openAgentMenu(page);
  await expect(ccAccount(page, 'alpha')).toBeVisible();
  await expect(page.locator('#agent-actions [data-worker-pool="reviewers"]')).toBeVisible();
  expect(await page.content()).not.toContain('sk-ant-oat01');
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'no sideways scroll at 412px').toBeLessThanOrEqual(cw);
});
