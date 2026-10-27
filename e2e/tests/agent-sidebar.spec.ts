import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, repoRoot } from '../lib/env';
import { clsInit, readCls, settle } from '../lib/cls';
import { navMoreKey, openMore } from '../lib/nav';

// THE SIDEBAR IS A LIST OF AGENTS, AND HOME IS THE CHIEF'S CHAT
// (internal/ui/nav_agents.go, internal/api/nav_agents.go), on the shared
// server, in a real browser — at a phone's size, set HERE (390×844 and 360
// wide, touch), and at a desktop's (1280×800):
//
//   1. `/` opens the chief agent's chat; with no chief, the Agents list;
//   2. the sidebar lists the agents: the chief first and marked as the page
//      on its own chat, then whoever waits on the operator (an open ask), then
//      the rest; a tap on a row opens that agent, whose row is then marked;
//   3. the pages are in ONE "More", shut by default, whose header still shows
//      the Asks count; opened, it reaches Tasks;
//   4. phone: it is the slide-out drawer, nothing scrolls sideways at 360 or
//      390, and every row and page entry is a 44px target; desktop: it is a
//      persistent rail, and More's open state survives a reload — restored
//      before the first paint, so the reload moves nothing.
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

// Each test starts with no stored More state (once per test, so a reload
// inside it keeps what the test stored).
test.beforeEach(async ({ page }) => {
  await page.addInitScript((key) => {
    try {
      if (!sessionStorage.getItem('nav-more-cleared')) {
        localStorage.removeItem(key);
        sessionStorage.setItem('nav-more-cleared', '1');
      }
    } catch (e) {}
  }, navMoreKey);
});

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
    await page.goto('/');
    await expect(page, '`/` opens the chief agent').toHaveURL(onAgent(CHIEF));

    // The drawer, shut until its hamburger opens it.
    await expect(page.locator('#sidebar')).not.toBeInViewport();
    await page.locator('#sidebar-open').click();
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

  test('the rail is persistent, and More stays as it was left across a reload', async ({ page }) => {
    await page.addInitScript(clsInit);
    await page.goto('/');
    await expect(page).toHaveURL(onAgent(CHIEF));
    await expect(page.locator('#sidebar-open'), 'a rail: no hamburger').toBeHidden();
    await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });
    await expectOrder(page);
    await expect(rowOf(page, CHIEF)).toHaveAttribute('aria-current', 'true');
    await expectMoreShutWithCount(page);

    await openMore(page);
    // Saved on the disclosure's `toggle` event, which fires after `open` is set.
    await expect.poll(() => page.evaluate((k) => localStorage.getItem(k), navMoreKey)).toBe('1');
    await page.goto('/tasks');
    await expect(page.locator('#sidebar-more'), 'More is open again after a navigation').toHaveJSProperty('open', true);
    await page.reload();
    await expect(page.locator('#sidebar-more'), 'More is open again after a reload').toHaveJSProperty('open', true);
    await expect(page.locator('#sidebar a[data-tab="tasks"]')).toBeVisible();
    await expect(page.locator('#sidebar a[data-tab="tasks"]')).toHaveAttribute('aria-current', 'page');
    await settle(page);
    const cls = await readCls(page);
    expect(cls.total, `the reload with More restored open shifted: ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);
    await shot(page, 'desktop-rail-more-expanded');

    // Shut again, it stays shut.
    await page.locator('#sidebar-more-summary').click();
    await expect(page.locator('#sidebar-more')).toHaveJSProperty('open', false);
    await expect.poll(() => page.evaluate((k) => localStorage.getItem(k), navMoreKey)).toBe('0');
    await page.reload();
    await expect(page.locator('#sidebar-more')).toHaveJSProperty('open', false);
    await expect(page.locator('#sidebar-more-body')).toBeHidden();
    await expect(rows(page).first()).toBeVisible();
    await shot(page, 'desktop-rail-more-collapsed');
  });

  test('`/` opens the Agents list when there is no chief', async ({ page }) => {
    fix('label', chiefId, 'retired');
    try {
      await page.goto('/');
      await expect(page).toHaveURL(/\/agents$/);
      await expect(page.locator('#panel-agents')).toBeVisible();
    } finally {
      fix('label', chiefId, 'chief');
    }
    // CONTROL: with the chief back, `/` opens it again.
    await page.goto('/');
    await expect(page).toHaveURL(onAgent(CHIEF));
  });
});
