import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, hookToken, repoRoot, sidebarOfficersConnectPort, sidebarOfficersPort, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { signIn } from '../lib/signin';
import { openMore } from '../lib/nav';

// THE SIDEBAR IS A LIST OF AGENTS, AND HOME IS THE CHIEF'S CHAT
// (internal/ui/nav_agents.go, internal/api/nav_agents.go), on the shared
// server, in a real browser — at a phone's size, set HERE (390×844 and 360
// wide, touch), and at a desktop's (1280×800):
//
//   1. `/` opens the chief agent's chat; with no chief, the session opened
//      last (the Agents list it once opened is retired);
//   2. the sidebar lists the agents: the chief first and marked as the page
//      on its own chat, then whoever waits on the operator (an open ask), then
//      the rest; a tap on a row opens that agent, whose row is then marked;
//   3. the pages are in ONE "More", shut by default, whose header still shows
//      the Asks count; opened, it reaches Tasks;
//   4. phone: it is the slide-out drawer, nothing scrolls sideways at 360 or
//      390, and every row and page entry is a 44px target; desktop: it is a
//      persistent rail with no close "×" (it cannot be closed), and More is
//      shut again on every load — nothing remembers it;
//   5. the list is fetched only where it can be seen (a shut phone drawer
//      fetches nothing until it opens; the rail loads with the page), follows
//      the Agents list's refresh events, and a focused row keeps focus;
//   6. `/` and the sidebar's first row name the same chief — a labelled one,
//      a relabelled reserved slug, or none.
//
// 🔴 REGRESSION TESTS: on origin/main `/` is the Tasks board, the sidebar has
// no agent rows and no More, and the pages are at its top level.
//
// The chief and the asker are this spec's own rows (e2e/pluginfix), deleted
// afterwards so no later spec on the shared database sees a chief (`/` would
// open it) or the ask.

const stamp = Date.now().toString(36);
const CHIEF = `e2e-nav-chief-${stamp}`;
const ASKER = `e2e-nav-asker-${stamp}`;
const QUESTION = 'Which branch should the rebase target?';
let chiefId = '';
let askId = '';

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

// Screenshots for review, when asked for (never in CI).
const shotDir = process.env.MUSTER_E2E_SHOTS ?? '';
async function shot(page: Page, name: string) {
  if (!shotDir) return;
  fs.mkdirSync(shotDir, { recursive: true });
  await page.screenshot({ path: path.join(shotDir, name + '.png') });
}

test.beforeAll(() => {
  test.setTimeout(120_000);
  chiefId = fix('chief', '1', CHIEF, `e2e-nav-chief-token-not-a-real-one-${stamp}`);
  fix('agent', '1', ASKER, `e2e-nav-asker-token-not-a-real-one-${stamp}`);
  askId = fix('ask', '1', ASKER, QUESTION);
});

test.afterAll(() => {
  if (askId) fix('rmask', '1', askId);
  fix('rmagent', '1', ASKER);
  fix('rmagent', '1', CHIEF);
});

// navRequests records every GET of the sidebar's list partial from now on.
function navRequests(page: Page): string[] {
  const seen: string[] = [];
  page.on('request', (r) => {
    if (new URL(r.url()).pathname === '/ui/nav/agents') seen.push(r.url());
  });
  return seen;
}

const rows = (page: Page) => page.locator('#sidebar-agents-list a[data-nav-agent]');
const rowOf = (page: Page, name: string) => page.locator(`#sidebar-agents-list a[href^="/agents/${name}"]`);
const onAgent = (name: string) => new RegExp(`/agents/${name}(\\?|$)`);

// expectOrder: the chief's row first, then every waiting row (the asker among
// them), then the rest.
async function expectOrder(page: Page) {
  await expect(rows(page).first()).toHaveAttribute('href', new RegExp(`^/agents/${CHIEF}`));
  const flags = await rows(page).evaluateAll((as) =>
    as.map((a) => ({ href: a.getAttribute('href') || '', chief: a.hasAttribute('data-nav-chief'), wait: a.hasAttribute('data-nav-wait') })),
  );
  expect(flags.length, 'the chief, the asker and the seeded agents').toBeGreaterThanOrEqual(3);
  expect(flags[0].chief, 'the first row is the chief').toBe(true);
  expect(flags.filter((f) => f.chief).length, 'one chief').toBe(1);
  const asker = flags.findIndex((f) => f.href.startsWith(`/agents/${ASKER}`));
  expect(asker, 'the asker is listed').toBeGreaterThan(0);
  expect(flags[asker].wait, 'the asker (an open ask) is waiting').toBe(true);
  const waits = flags.slice(1).map((f) => f.wait);
  const firstPlain = waits.indexOf(false);
  if (firstPlain >= 0) {
    expect(waits.slice(firstPlain).includes(true), `a waiting row is below a row that is not: ${JSON.stringify(flags)}`).toBe(false);
  }
}

// expectMoreShutWithCount: More is shut, its body not shown, and its header
// shows the Asks count (this spec's ask is open).
async function expectMoreShutWithCount(page: Page) {
  await expect(page.locator('#sidebar-more')).toHaveJSProperty('open', false);
  await expect(page.locator('#sidebar-more-body')).toBeHidden();
  await expect(page.locator('#sidebar a[data-tab="tasks"]')).toBeHidden();
  const badge = page.locator('#sidebar-more-summary #attention-badge');
  await expect(badge).toBeVisible();
  await expect.poll(async () => Number(await badge.getAttribute('data-count'))).toBeGreaterThanOrEqual(1);
}

async function expectNoSidewaysScroll(page: Page, why: string) {
  const w = await page.evaluate(() => ({
    sw: document.documentElement.scrollWidth,
    cw: document.documentElement.clientWidth,
    panel: (document.getElementById('sidebar') as HTMLElement).scrollWidth - (document.getElementById('sidebar') as HTMLElement).clientWidth,
  }));
  expect(w.sw, `${why}: the page is ${w.sw}px wide in a ${w.cw}px viewport`).toBeLessThanOrEqual(w.cw);
  expect(w.panel, `${why}: the sidebar scrolls sideways by ${w.panel}px`).toBeLessThanOrEqual(0);
}

test.describe('phone', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('home opens the chief; the drawer lists the agents; a tap opens one; More reaches Tasks', async ({ page }) => {
    const nav = navRequests(page);
    // The page's own load requests (the Asks badge's) have landed when this does.
    const badge = page.waitForResponse((r) => r.url().includes('/attention/badge'));
    await page.goto('/');
    await expect(page, '`/` opens the chief agent').toHaveURL(onAgent(CHIEF));
    await badge;

    // The drawer, shut until its hamburger opens it — and while it is shut the
    // list is not fetched, on load or on a refresh event.
    await expect(page.locator('#sidebar')).not.toBeInViewport();
    expect(await page.evaluate(() => (window as any).musterNavShown()), 'a shut drawer is not shown').toBe(false);
    await page.evaluate(() => document.body.dispatchEvent(new CustomEvent('muster:resync')));
    await page.waitForTimeout(500);
    expect(nav, 'the shut drawer fetched its list').toEqual([]);
    await page.locator('#sidebar-open').click();
    await expect.poll(() => nav.length, { message: 'opening the drawer fetches the list' }).toBeGreaterThan(0);
    await expect(page.locator('#sidebar-root')).toHaveAttribute('data-open', 'true');
    await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });

    await expectOrder(page);
    const chiefRow = rowOf(page, CHIEF);
    await expect(chiefRow, "on the chief's chat its row is marked").toHaveAttribute('aria-current', 'true');
    await expect(page.locator('#sidebar-agents-list [data-nav-current]')).toHaveCount(1);
    await expectMoreShutWithCount(page);
    await shot(page, 'phone-drawer-more-collapsed');

    // A tap on the asker opens its chat, and there its row is the marked one.
    await rowOf(page, ASKER).click();
    await expect(page).toHaveURL(onAgent(ASKER));
    await page.locator('#sidebar-open').click();
    await expect(rowOf(page, ASKER)).toHaveAttribute('aria-current', 'true');
    await expect(page.locator('#sidebar-agents-list [data-nav-current]')).toHaveCount(1);
    await expect(rowOf(page, ASKER).locator('[data-nav-attention]'), "the asker's row carries its attention count").toHaveAttribute('data-count', '1');

    // More opens and reaches Tasks.
    await openMore(page);
    await shot(page, 'phone-drawer-more-expanded');
    await page.locator('#sidebar a[data-tab="tasks"]').click();
    await expect(page).toHaveURL(/\/tasks$/);
    await expect(page.locator('#panel-tasks')).toBeVisible();
  });

  for (const width of [360, 390]) {
    test(`nothing scrolls sideways at ${width}px, and every entry is a 44px target`, async ({ page }) => {
      await page.setViewportSize({ width, height: 800 });
      await page.goto(`/agents/${ASKER}`);
      await page.locator('#sidebar-open').click();
      await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });
      await expect(rows(page).first()).toBeVisible();
      await expectNoSidewaysScroll(page, 'the drawer open');
      await openMore(page);
      await expectNoSidewaysScroll(page, 'More open');
      const small = await page.evaluate(() =>
        [...document.querySelectorAll('#sidebar-agents-list a[data-nav-agent], #sidebar-more-summary, #sidebar-more-body a, #sidebar-more-body button')]
          .filter((e) => (e as HTMLElement).getClientRects().length > 0)
          .map((e) => ({ what: (e.getAttribute('href') || e.id || e.textContent || '').trim().slice(0, 40), h: Math.round(e.getBoundingClientRect().height) }))
          .filter((x) => x.h < 44),
      );
      expect(small, 'entries under 44px tall').toEqual([]);
    });
  }
});

test.describe('desktop', () => {
  test.use({ viewport: { width: 1280, height: 800 } });

  test('the rail is persistent with no close button; More opens there and is shut again on the next load', async ({ page }) => {
    await page.goto('/');
    await expect(page).toHaveURL(onAgent(CHIEF));
    await expect(page.locator('#sidebar-open'), 'a rail: no hamburger').toBeHidden();
    // …and no close "×": a rail cannot be closed, so the control would do nothing.
    await expect(page.locator('#sidebar-close'), 'a rail: no close button').toBeHidden();
    await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });
    await expectOrder(page);
    await expect(rowOf(page, CHIEF)).toHaveAttribute('aria-current', 'true');
    await expectMoreShutWithCount(page);

    // On the board: More opens and holds its page, marked as the one shown.
    await page.goto('/tasks');
    await expect(page.locator('#sidebar-close'), 'a rail: no close button on the board either').toBeHidden();
    await expectMoreShutWithCount(page);
    await openMore(page);
    await expect(page.locator('#sidebar a[data-tab="tasks"]')).toHaveAttribute('aria-current', 'page');
    await shot(page, 'desktop-rail-more-expanded');

    // No memory: the next load draws it shut again (operator decision).
    await page.reload();
    await expectMoreShutWithCount(page);
    await expect(rows(page).first()).toBeVisible();
    await shot(page, 'desktop-rail-more-collapsed');
  });

  test('the rail loads its list with the page, refreshes on the Agents list\'s events, and keeps a focused row focused', async ({ page }) => {
    const nav = navRequests(page);
    await page.goto('/tasks');
    await expect(rows(page).first()).toBeVisible();
    // CONTROL for the phone's "nothing fetched": here the rail is on screen.
    expect(nav.length, 'the rail fetched its list on load').toBeGreaterThan(0);
    expect(await page.evaluate(() => (window as any).musterNavShown())).toBe(true);
    for (const ev of ['muster:resync', 'agents:changed', 'sse:agent.changed', 'sse:chat.reply']) {
      const before = nav.length;
      const got = page.waitForResponse((r) => new URL(r.url()).pathname === '/ui/nav/agents');
      await page.evaluate((e) => document.body.dispatchEvent(new CustomEvent(e)), ev);
      await got;
      expect(nav.length, `${ev} refreshed the list`).toBeGreaterThan(before);
    }
    // A row focused by keyboard keeps focus across a refresh: the swap
    // replaces the element, and the new one of the same id is focused.
    const row = rowOf(page, ASKER);
    const id = await row.getAttribute('id');
    expect(id, 'the row has an id').toMatch(/^nav-agent-\d+$/);
    await row.focus();
    await page.evaluate(() => ((document.activeElement as any).__old = true));
    const got = page.waitForResponse((r) => new URL(r.url()).pathname === '/ui/nav/agents');
    await page.evaluate(() => document.body.dispatchEvent(new CustomEvent('muster:resync')));
    await got;
    await expect
      .poll(() => page.evaluate(() => ({ id: document.activeElement && document.activeElement.id, old: !!(document.activeElement as any).__old })))
      .toEqual({ id, old: false });
  });

  test('`/` and the sidebar\'s first row agree on the chief, whatever its shape', async ({ page }) => {
    // The labelled chief: `/` opens it, and it is the first row.
    await page.goto('/');
    await expect(page).toHaveURL(onAgent(CHIEF));
    await expect(rows(page).first()).toHaveAttribute('href', new RegExp(`^/agents/${CHIEF}`));
    // Relabelled away, with the RESERVED SLUG "chief" relabelled too: the slug
    // is the chief — `/` opens it and it is the first row.
    fix('label', chiefId, 'retired');
    const slugId = fix('agent', '1', 'chief', `e2e-nav-slug-token-not-a-real-one-${stamp}`);
    try {
      fix('label', slugId, 'fleet boss');
      await page.goto('/');
      await expect(page, '`/` opens the reserved-slug chief').toHaveURL(onAgent('chief'));
      await expect(rows(page).first(), 'the first row is the chief `/` opened').toHaveAttribute('href', /^\/agents\/chief[?"]|^\/agents\/chief$/);
      await expect(rows(page).first()).toHaveAttribute('data-nav-chief', '');
    } finally {
      fix('rmagent', '1', 'chief');
      fix('label', chiefId, 'chief');
    }
  });

  test('`/` opens the session opened last when there is no chief', async ({ page }) => {
    fix('label', chiefId, 'retired');
    try {
      // The chief's chat was opened last before: open the asker's after it.
      await page.goto(`/agents/${CHIEF}`);
      await page.goto(`/agents/${ASKER}`);
      await expect(page.locator('[data-agent-title]')).toBeVisible();
      await page.goto('/');
      await expect(page, 'with no chief, `/` opens the session opened last').toHaveURL(onAgent(ASKER));
      // …and the sidebar pins no chief either: the two agree.
      await expect(rows(page).first()).toBeVisible();
      await expect(page.locator('#sidebar-agents-list [data-nav-chief]')).toHaveCount(0);
    } finally {
      fix('label', chiefId, 'chief');
    }
    // CONTROL: with the chief back, `/` opens it again.
    await page.goto('/');
    await expect(page).toHaveURL(onAgent(CHIEF));
  });
});

// THE OFFICERS GROUP (internal/ui nav_officers.go): one row per declared
// officer under an "Officers" heading, below the list and above Idle; an
// asleep officer's row opens its page; an instance that waits on the operator
// is lifted to the waiting rows at the top and its officer's row is marked.
//
// Its own server over the noop driver, so an officer declared to run in pods
// RUNS there (it is in the roster's registry, and its instance's agent row is
// bound to it by the server's rule). Two officers: a sleeper declared in
// MUSTER_OFFICERS (a daily budget is an env declaration's field; no instance,
// nothing spent: "$0.00 / $8"), and a waiter declared through the admin API
// whose first instance's row is this spec's own, with an open ask.
//
// 🔴 RED at origin/main: no #sidebar-officers-group, no data-nav-officer row,
// and the waiter's instance is an ordinary row with no officer row to mark.
test.describe('the Officers group', () => {
  const origin = `http://localhost:${sidebarOfficersPort}`;
  const SLEEPER = 'e2e-sleeper:main';
  const WAITER = `e2e-waiter-${stamp}:main`;
  const OFF_QUESTION = 'Which release notes template should the digest use?';
  let server: Running | undefined;
  let instance = '';
  let offAsk = '';

  async function declare(id: string, name: string, extra: Record<string, unknown>) {
    const r = await fetch(`${origin}/api/officers/declared`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${hookToken}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ id, name, charter: 'Summarise the queue. Never post anything.', runtime: 'pod', mode: 'shadow', ...extra }),
    });
    const body = await r.json();
    expect(r.status, `officer ${id} is declared: ${JSON.stringify(body)}`).toBe(201);
    return body as { instances?: string[]; running?: boolean; reason?: string };
  }
  async function remove(id: string) {
    await fetch(`${origin}/api/officers/declared/${encodeURIComponent(id)}`, { method: 'DELETE', headers: { Authorization: `Bearer ${hookToken}` } });
  }

  test.beforeAll(async () => {
    test.setTimeout(120_000);
    server = await startServer(versionA, sidebarOfficersPort, {
      MUSTER_AGENT_PROVISIONER: 'noop',
      MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
      MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
      MUSTER_AGENT_KINDS: 'gateway,claude-code',
      MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
      MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
      MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-sidebar-officers',
      MUSTER_OFFICERS: JSON.stringify([{ id: SLEEPER, backend: 'builtin', daily_budget_usd: 8 }]),
      MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x3c).toString('base64'),
      MUSTER_CONNECT_PORT: String(sidebarOfficersConnectPort),
      MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${sidebarOfficersConnectPort}`,
    });
    const w = await declare(WAITER, 'E2E waiter', {});
    expect(w.running, `the waiter runs in a pod here: ${w.reason ?? ''}`).toBe(true);
    instance = (w.instances ?? [])[0] ?? '';
    expect(instance, 'the waiter names its first instance').not.toBe('');
    fix('agent', '1', instance, `e2e-nav-officer-token-not-a-real-one-${stamp}`);
    offAsk = fix('ask', '1', instance, OFF_QUESTION);
  });

  test.afterAll(async () => {
    if (offAsk) fix('rmask', '1', offAsk);
    if (server) await remove(WAITER);
    if (instance) fix('rmagent', '1', instance);
    await server?.stop();
  });

  // officerRow is the group's row of the officer called name.
  const officerRow = (page: Page, name: string) =>
    page.locator('#sidebar-officers-group a[data-nav-officer]').filter({ has: page.locator('[data-nav-name]', { hasText: name }) });

  async function expectGroup(page: Page) {
    const group = page.locator('#sidebar-officers-group');
    await expect(group).toBeVisible();
    await expect(page.locator('#sidebar-officers-label')).toHaveText('Officers');
    // The sleeper: asleep, its page, its budget against nothing spent.
    const sleeper = officerRow(page, SLEEPER);
    await expect(sleeper).toHaveCount(1);
    await expect(sleeper).toHaveAttribute('data-nav-officer-state', 'asleep');
    await expect(sleeper).toHaveAttribute('href', `/officers/${SLEEPER}`);
    await expect(sleeper.locator('[data-nav-officer-spend]')).toHaveText('$0.00 / $8');
    await expect(sleeper.locator('[data-nav-officer-facts]')).toHaveText('asleep · 0 queued');
    // The waiter: one row in the group, marked, opening its running
    // instance's chat; that instance (waiting) is ALSO a session row of the
    // list — not in the group — above every row that does not wait.
    const waiter = officerRow(page, 'E2E waiter');
    await expect(waiter).toHaveCount(1);
    await expect(waiter.locator('[data-nav-officer-waiting]')).toHaveText('↑ waiting');
    const inst = page.locator(`#sidebar-agents-list a[data-nav-agent][href^="/agents/${instance}"]`);
    await expect(inst).toHaveCount(1);
    await expect(inst).toHaveAttribute('data-nav-wait', '');
    await expect(waiter).toHaveAttribute('data-nav-officer-state', 'running');
    await expect(waiter).toHaveAttribute('href', new RegExp(`^/agents/${instance}`));
    await expect(group.locator('a[data-nav-agent]'), 'no session row is drawn inside the group').toHaveCount(0);
    const flags = await rows(page).evaluateAll((as) =>
      as.filter((a) => !a.closest('#sidebar-idle')).map((a) => ({ href: a.getAttribute('href') || '', chief: a.hasAttribute('data-nav-chief'), wait: a.hasAttribute('data-nav-wait') })),
    );
    const at = flags.findIndex((f) => f.href.startsWith(`/agents/${instance}`));
    const firstPlain = flags.findIndex((f) => !f.chief && !f.wait);
    expect(at, `the waiting instance is listed: ${JSON.stringify(flags)}`).toBeGreaterThanOrEqual(0);
    if (firstPlain >= 0) expect(at, `the waiting instance is above every row that does not wait: ${JSON.stringify(flags)}`).toBeLessThan(firstPlain);
    // The group is below the list and above Idle (when there is one).
    const order = await page.evaluate(() => {
      const list = document.querySelector('#sidebar-agents-list [aria-labelledby="sidebar-agents-label"]');
      const g = document.getElementById('sidebar-officers-group');
      const idle = document.getElementById('sidebar-idle');
      const after = (a: Element | null, b: Element | null) => !!a && !!b && !!(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
      return { listFirst: !list || after(list, g), idleAfter: !idle || after(g, idle) };
    });
    expect(order).toEqual({ listFirst: true, idleAfter: true });
  }

  test('desktop: the rail shows the Officers group; an asleep officer opens its page', async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 800 });
    await signIn(page, origin);
    await page.goto(`${origin}/tasks`);
    await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });
    await expectGroup(page);
    await shot(page, 'desktop-officers-group');
    // A page with no switcher: the palette reads the sidebar's rows, the
    // Officers rows among them, and opens what the officer's row opens.
    await page.locator('#sidebar-search').click();
    await expect(page.locator('#switcher-search')).toBeVisible();
    await page.locator('#switcher-search-input').pressSequentially('e2e-sleeper');
    const opts = page.locator('#switcher-search-results [role="option"]');
    await expect(opts).toHaveCount(1);
    await expect(opts.first()).toHaveAttribute('data-kind', 'officer');
    await expect(opts.first()).toHaveAttribute('data-href', `/officers/${SLEEPER}`);
    await page.keyboard.press('Escape');
    await officerRow(page, SLEEPER).click();
    await expect(page).toHaveURL(`${origin}/officers/${SLEEPER}`);
  });

  test('phone: the drawer shows the Officers group, every row a 44px target', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await signIn(page, origin);
    await page.goto(`${origin}/tasks`);
    await page.locator('#sidebar-open').click();
    await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });
    await expectGroup(page);
    await expectNoSidewaysScroll(page, 'the drawer open with officers');
    const small = await page.locator('#sidebar-officers-group a[data-nav-officer]').evaluateAll((as) =>
      as.map((a) => Math.round(a.getBoundingClientRect().height)).filter((h) => h < 44),
    );
    expect(small, 'officer rows under 44px tall').toEqual([]);
    await shot(page, 'phone-officers-group');
  });
});
