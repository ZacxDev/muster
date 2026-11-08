import { request as pwRequest, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { agentActionsPort, hookToken, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { createAgent, postAgents } from '../lib/seed';

// THE AGENTS LIST PAGE IS RETIRED, AND WHAT ONLY IT OFFERED MOVED (operator
// decision: the sidebar and the session switcher's bar are how a person moves
// between agents; the default view opens to the chief). Driven through the
// real UI against a REAL muster-server with an agent provisioner (the noop
// driver, which records instead of provisioning), at a phone (390×844) and a
// desktop (1280×800):
//
//   1. GET /agents — any old link to the list, a `?filter=` one included —
//      lands home (here, with no chief: the session opened last), never the list;
//   2. there is NO "+" beside the sidebar's Agents label, no "not set up" hint
//      and no task-card Dispatch (docs/design/chief-dispatch.md phase 2 deleted
//      the dispatch form, D8: nothing replaces them); an agent created stopped
//      through POST /api/agents (what "Save for later" made) has a page the
//      sidebar lists, and a dispatch there that this deployment cannot deliver
//      is refused with the reason and creates nothing;
//   3. the agent's page carries the card's actions in its header's ⋯ menu:
//      Rename (the header, the tab title and the sidebar follow), Start / Stop
//      (the live status strip and the menu follow), Delete (lands home; the
//      agent is gone) — and its title keeps its room on a phone.
//
// REGRESSION TEST: red at dd25e646, where /agents drew the list and the agent
// page had no ⋯ menu or status strip; and red before phase 2, where the sidebar
// drew the "+" and a task card its Dispatch.

const origin = `http://localhost:${agentActionsPort}`;
let server: Running | undefined;

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(120_000);
  server = await startServer(versionA, agentActionsPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
});
test.afterAll(async () => {
  await server?.stop();
});

interface AgentRow {
  id: number;
  name: string;
  displayName: string;
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

async function signedIn(page: Page): Promise<void> {
  await signIn(page, origin);
}

// sidebarShown puts the sidebar on screen: the rail on a desktop, the drawer
// opened on a phone.
async function sidebarShown(page: Page): Promise<void> {
  const sb = page.locator('#sidebar');
  if (!(await page.locator('#sidebar-agents-list').isVisible())) await page.locator('#sidebar-open').click();
  await expect(sb).toBeInViewport();
}

// createAndOpen creates an agent stopped with an ad-hoc task through POST
// /api/agents (lib/seed.ts) and opens its page; it returns the new agent's row.
async function createAndOpen(page: Page, task: string): Promise<AgentRow> {
  const before = new Set((await agentRows()).map((a) => a.id));
  const agent = await createAgent(origin, { prompt: task });
  const made = (await agentRows()).find((a) => !before.has(a.id));
  expect(made?.name, 'the create made exactly this agent').toBe(agent.name);
  await page.goto(`${origin}/agents/${made!.name}`);
  await page.waitForURL((u) => u.pathname === `/agents/${made!.name}` && u.searchParams.has('session'));
  await expect(page.locator('#agent-strip')).toBeVisible();
  return made!;
}

// menu opens the agent page's ⋯ menu and returns it.
async function menu(page: Page) {
  await page.locator('#agent-actions-open').click();
  const m = page.locator('#agent-actions');
  await expect.poll(() => m.evaluate((el) => el.matches(':popover-open'))).toBe(true);
  return m;
}

const sizes: [string, { width: number; height: number }][] = [
  ['phone', { width: 390, height: 844 }],
  ['desktop', { width: 1280, height: 800 }],
];

for (const [label, viewport] of sizes) {
  test.describe(label, () => {
    test.use({ viewport });

    test('no "+", no hint and no card Dispatch; a stopped agent opens on its page; a refused dispatch creates nothing', async ({ page }) => {
      await signedIn(page);
      await page.goto(origin + '/tasks');
      // The board's cards are drawn (positive control), and none offers a
      // Dispatch; the sidebar's Agents head has neither "+" nor hint.
      await expect(page.locator('[id^="task-"][data-task-status]').first()).toBeVisible();
      await expect(page.getByRole('button', { name: 'Dispatch agent for this task' })).toHaveCount(0);
      await sidebarShown(page);
      await expect(page.locator('#sidebar-agents-label')).toBeVisible();
      await expect(page.locator('#sidebar-deploy')).toHaveCount(0);
      await expect(page.locator('[data-dispatch-off-hint]')).toHaveCount(0);
      await expect(page.locator('#agent-modal')).toHaveCount(0);

      const saved = await createAndOpen(page, `Saved for later at the ${label} size.`);
      // A stopped agent is not started: its menu offers Start.
      const m = await menu(page);
      await expect(m.locator('[data-agent-action="Start"]')).toBeVisible();
      await page.keyboard.press('Escape');
      await sidebarShown(page);
      await expect(page.locator(`#sidebar-agents-list a#nav-agent-${saved.id}`)).toHaveAttribute('aria-current', 'true');

      // This deployment (the noop driver, no gateway) cannot deliver a first
      // turn, so a dispatch is refused with the reason, and nothing is created.
      const before = (await agentRows()).length;
      const refused = await postAgents(origin, { prompt: `Dispatched at the ${label} size.`, start: true });
      expect(refused.status, refused.text).toBe(409);
      expect(refused.text).toContain('dispatch REFUSED and NOTHING was provisioned');
      expect((await agentRows()).length, 'nothing was created').toBe(before);
    });

    test('/agents, and an old filter link to it, land home — never the list', async ({ page }) => {
      await signedIn(page);
      for (const path of ['/agents', '/agents?filter=idle-1h&q=wren']) {
        await page.goto(origin + path);
        await expect.poll(() => new URL(page.url()).pathname, { message: `${path} redirected` }).not.toBe('/agents');
        expect(new URL(page.url()).pathname, `${path} lands on a session's page`).toMatch(/^\/(agents\/[^/]+|mirror\/)/);
        await expect(page.locator('#agents-list, #panel-agents')).toHaveCount(0);
      }
    });

    test("an agent's page renames, stops, starts and deletes it from its ⋯ menu", async ({ page }) => {
      await signedIn(page);
      await page.goto(origin + '/tasks');
      const a = await createAndOpen(page, `The menu's agent, ${label}.`);

      // The title keeps its room: the strip and the ⋯ are on the header's
      // second row, never the title's.
      const titleRow = page.locator('header [data-agent-title]');
      const titleTop = await titleRow.evaluate((el) => el.getBoundingClientRect().top);
      const menuTop = await page.locator('#agent-actions-open').evaluate((el) => el.getBoundingClientRect().top);
      expect(menuTop, 'the ⋯ is below the title row').toBeGreaterThan(titleTop + 20);
      // …and the title's row holds no control it did not hold before: the
      // hamburger (a phone's; the desktop has the rail), the title, New chat,
      // Chat history and Focus history (this server has a focus store).
      const firstRow = await titleRow.evaluate((el) =>
        [...(el.parentElement as HTMLElement).querySelectorAll('button, a, label[role="button"]')]
          .filter((c) => !(el as HTMLElement).contains(c) && (c as HTMLElement).getClientRects().length > 0)
          .map((c) => c.getAttribute('aria-label') || c.id),
      );
      const want = ['New chat session', 'Chat history', 'Focus history'];
      expect(firstRow, "the title row's controls").toEqual(label === 'phone' ? ['Open menu', ...want] : want);
      expect(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth), 'no sideways scroll').toBe(0);

      // An OPEN menu is not refreshed away under the reader: a live refresh
      // of the strip (any agent change) never redraws the menu, which is drawn
      // once; its Start/Stop follows the strip's status (checked below).
      let m = await menu(page);
      await m.locator('[data-agent-action="delete"]').click();
      await expect(m.locator('[data-agent-action="confirm-delete"]')).toBeVisible();
      const refreshed = page.waitForResponse((r) => new URL(r.url()).pathname === `/ui/agents/${a.id}/header`);
      await page.evaluate(() => document.body.dispatchEvent(new CustomEvent('agents:changed', { bubbles: true })));
      await refreshed;
      await page.waitForTimeout(300);
      expect(await m.evaluate((el) => el.matches(':popover-open')), 'the menu is still open after a refresh').toBe(true);
      await expect(m.locator('[data-agent-action="confirm-delete"]'), 'the armed Confirm delete is still there').toBeVisible();
      await page.keyboard.press('Escape');
      await expect(page.locator('#agent-actions-open')).toBeVisible();

      // The strip refreshes for THIS agent's stream event only: another
      // agent's change, and the empty-id broadcast a mirrored host's churn
      // sends, fetch nothing; this agent's does.
      const headerPath = `/ui/agents/${a.id}/header`;
      let fetched = 0;
      const count = (r: import('@playwright/test').Request) => { if (new URL(r.url()).pathname === headerPath) fetched++; };
      page.on('request', count);
      const sse = (id: string) => page.evaluate((id) => (window as any).htmx.trigger(document.body, 'sse:agent.changed', { data: JSON.stringify({ id }) }), id);
      await sse('some-other-agent');
      await sse('');
      await page.waitForTimeout(600);
      expect(fetched, "another agent's change and the empty broadcast refresh nothing").toBe(0);
      const own = page.waitForResponse((r) => new URL(r.url()).pathname === headerPath);
      await sse(a.name);
      await own;
      expect(fetched, "this agent's change refreshes the strip").toBe(1);
      page.off('request', count);

      // Rename.
      const name = `Renamed at ${label}`;
      m = await menu(page);
      await m.locator('[data-agent-action="rename"]').click();
      const form = page.locator('#agent-rename form');
      await expect(form).toBeVisible();
      await form.locator('input[name="name"]').fill(name);
      await Promise.all([page.waitForEvent('load'), form.getByRole('button', { name: 'Save', exact: true }).click()]);
      await expect(page.locator('header [data-agent-title-text]')).toHaveText(name);
      await expect(page).toHaveTitle(new RegExp(`^${name} · muster$`));
      expect((await agentRows()).find((r) => r.id === a.id)?.displayName).toBe(name);
      await sidebarShown(page);
      // The row's tooltip: the name, then the slug the row no longer spells.
      await expect(page.locator(`#nav-agent-${a.id}`)).toHaveAttribute('title', `${name} · ${a.name}`);
      if (label === 'phone') await page.locator('#sidebar-close').click();

      // Start: the status strip and the menu follow, with no reload.
      m = await menu(page);
      await m.locator('[data-agent-action="Start"]').click();
      await expect.poll(async () => (await agentRows()).find((r) => r.id === a.id)?.status, { timeout: 20_000 }).toMatch(/provisioning|running/);
      await expect(page.locator('#agent-status')).toHaveAttribute('data-agent-status', /provisioning|running/, { timeout: 20_000 });
      m = await menu(page);
      await expect(m.locator('[data-agent-action="Stop"]')).toBeVisible();
      await expect(m.locator('[data-agent-action="Start"]'), 'a running agent is not offered Start').toBeHidden();

      // Stop.
      await m.locator('[data-agent-action="Stop"]').click();
      await expect.poll(async () => (await agentRows()).find((r) => r.id === a.id)?.status, { timeout: 20_000 }).toBe('stopped');
      await expect(page.locator('#agent-status')).toHaveAttribute('data-agent-status', 'stopped', { timeout: 20_000 });
      m = await menu(page);
      await expect(m.locator('[data-agent-action="Start"]')).toBeVisible();
      await expect(m.locator('[data-agent-action="Stop"]'), 'a stopped agent is not offered Stop').toBeHidden();

      // Delete, two steps: lands home, and the agent is gone.
      await m.locator('[data-agent-action="delete"]').click();
      await Promise.all([
        page.waitForURL((u) => u.pathname !== `/agents/${a.name}`),
        m.locator('[data-agent-action="confirm-delete"]').click(),
      ]);
      await expect.poll(async () => (await agentRows()).some((r) => r.id === a.id), { message: 'the agent is deleted', timeout: 20_000 }).toBe(false);
      expect(new URL(page.url()).pathname, 'home, not the deleted agent').not.toBe(`/agents/${a.name}`);
    });
  });
}
