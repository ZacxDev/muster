import { request as pwRequest, Browser, BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { agentActionsPort, hookToken, password, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// THE AGENTS LIST PAGE IS RETIRED, AND WHAT ONLY IT OFFERED MOVED (operator
// decision: the sidebar and the session switcher's bar are how a person moves
// between agents; the default view opens to the chief). Driven through the
// real UI against a REAL muster-server with an agent provisioner (the noop
// driver, which records instead of provisioning), at a phone (390×844) and a
// desktop (1280×800):
//
//   1. GET /agents — any old link to the list, a `?filter=` one included —
//      lands home (here, with no chief: the session opened last), never the list;
//   2. the "+" beside the sidebar's Agents label opens the task-less dispatch
//      sheet on any page; "Save for later" lands on the new agent's page, which
//      the sidebar lists, and a refused Dispatch says why (on the agent's page
//      too) and creates nothing;
//   3. the agent's page carries the card's actions in its header's ⋯ menu:
//      Rename (the header, the tab title and the sidebar follow), Start / Stop
//      (the live status strip and the menu follow), Delete (lands home; the
//      agent is gone) — and its title keeps its room on a phone.
//
// REGRESSION TEST: red at dd25e646, where /agents drew the list, the sidebar
// had no "+", and the agent page had no ⋯ menu or status strip.

const origin = `http://localhost:${agentActionsPort}`;
let server: Running | undefined;
let cookies: Parameters<BrowserContext['addCookies']>[0] = [];

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async ({ browser }: { browser: Browser }) => {
  test.setTimeout(120_000);
  server = await startServer(versionA, agentActionsPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  // ONE sign-in for the whole spec: the form is throttled per server.
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  cookies = await ctx.cookies(origin);
  await ctx.close();
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
  await page.context().addCookies(cookies);
}

// sidebarShown puts the sidebar on screen: the rail on a desktop, the drawer
// opened on a phone.
async function sidebarShown(page: Page): Promise<void> {
  const sb = page.locator('#sidebar');
  if (!(await page.locator('#sidebar-deploy').isVisible())) await page.locator('#sidebar-open').click();
  await expect(sb).toBeInViewport();
}

// dispatchFromSidebar opens the sheet from the sidebar's "+", types an ad-hoc
// task and presses the button named `action`; it returns the new agent's row
// and waits until the page is that agent's page.
async function dispatchFromSidebar(page: Page, action: 'Save for later', task: string): Promise<AgentRow> {
  const before = new Set((await agentRows()).map((a) => a.id));
  await sidebarShown(page);
  await page.locator('#sidebar-deploy').click();
  const form = page.locator('#agent-modal form');
  await expect(form).toBeVisible();
  await form.locator('textarea[name="note_text"]').fill(task);
  // The POST's answer, for the failure message.
  let answered = 'no POST /agents was sent';
  const onResponse = async (r: import('@playwright/test').Response) => {
    if (r.request().method() === 'POST' && new URL(r.url()).pathname === '/agents') {
      answered = `POST /agents → ${r.status()} ${JSON.stringify(r.headers()['hx-redirect'] ?? '')} ${(await r.text().catch(() => '')).slice(0, 200)}`;
    }
  };
  page.on('response', onResponse);
  await form.getByRole('button', { name: action, exact: true }).click();
  let made: AgentRow | undefined;
  await expect
    .poll(async () => {
      made = (await agentRows()).find((a) => !before.has(a.id));
      return made?.name ?? '';
    }, { message: `${action} made an agent (${answered})` })
    .not.toBe('');
  page.off('response', onResponse);
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

    test('the sidebar "+" saves for later onto the new agent\'s page, and says why a dispatch is refused', async ({ page }) => {
      await signedIn(page);
      await page.goto(origin + '/tasks');
      const saved = await dispatchFromSidebar(page, 'Save for later', `Saved for later at the ${label} size.`);
      // A saved agent is not started: its menu offers Start.
      const m = await menu(page);
      await expect(m.locator('[data-agent-action="Start"]')).toBeVisible();
      await page.keyboard.press('Escape');
      await sidebarShown(page);
      await expect(page.locator(`#sidebar-agents-list a#nav-agent-${saved.id}`)).toHaveAttribute('aria-current', 'true');

      // From the agent's page, the same "+": this deployment (the noop driver,
      // no gateway) cannot deliver a first turn, so Dispatch is refused — and
      // the refusal is SAID, on this page too, with nothing created.
      const before = (await agentRows()).length;
      await page.locator('#sidebar-deploy').click();
      const form = page.locator('#agent-modal form');
      await expect(form).toBeVisible();
      await form.locator('textarea[name="note_text"]').fill(`Dispatched at the ${label} size.`);
      await form.getByRole('button', { name: 'Dispatch', exact: true }).click();
      await expect(page.locator('#action-toasts')).toContainText('dispatch REFUSED and NOTHING was provisioned');
      await expect(form, 'the sheet stays open with the typed task').toBeVisible();
      expect((await agentRows()).length, 'nothing was created').toBe(before);
      expect(new URL(page.url()).pathname).toBe(`/agents/${saved.name}`);
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
      const a = await dispatchFromSidebar(page, 'Save for later', `The menu's agent, ${label}.`);

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
      await expect(page.locator(`#nav-agent-${a.id}`)).toHaveAttribute('title', name);
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
