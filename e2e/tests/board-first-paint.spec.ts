import { test, expect } from '../lib/fixtures';
import type { Browser, Page } from '@playwright/test';
import { authState, baseURL } from '../lib/env';
import { launchWithBfcache } from '../lib/bfcache';
import { waitControlled } from '../lib/sw';

// THE BOARD IS IN THE `/tasks` DOCUMENT, AND THE FIRST SSE OPEN DOES NOT FETCH IT
// AGAIN (internal/ui/tasks_list_ssr.go). (`/` served the board too until home
// became the chief's chat, internal/api/nav_agents.go; the offline copy the
// worker keeps under "/" is still the shell, tested below.)
//
// The board used to draw a skeleton and fill it from its own GET /ui/tasks, and the
// stream's first open then fired muster:resync, which fetched /ui/tasks a
// second time to morph an identical board over it. The document now carries
// the board; the first open skips its resync only when the stream's hello
// carries the mark the board was drawn at. Every other open — a reconnect, or a
// first open after an event the board predates — still re-fetches, and those
// are the cases below that must keep working.

const tagFilterKey = 'muster.tasks.tagfilter';
const seeded = 'Rotate the staging webhook secret'; // e2e/seed: tags infra, security
const seededDocs = 'Draft release notes for 0.4'; // e2e/seed: tag docs
const card = (page: Page, title: string) => page.locator('#tasks-list [id^="task-"]', { hasText: title });

// watch records, from the document's start, every GET /ui/tasks (the board
// partial — not /ui/tasks/<anything>) and every muster:resync the page fires.
async function watch(page: Page) {
  const fetches: string[] = [];
  page.on('request', (r) => {
    const u = new URL(r.url());
    if (u.pathname === '/ui/tasks') fetches.push(u.search);
  });
  await page.addInitScript(() => {
    (window as any).__resyncs = 0;
    document.addEventListener('muster:resync', () => ((window as any).__resyncs += 1), true);
  });
  return {
    fetches,
    resyncs: () => page.evaluate(() => (window as any).__resyncs as number),
  };
}

// firstOpenSettled waits until the stream's first open has consumed the
// board's mark, and then past the 3 s a missing hello would wait.
async function firstOpenSettled(page: Page) {
  await expect(page.locator('#tasks-list[data-tasks-drawn]')).toHaveCount(1);
  await expect(page.locator('#tasks-list[data-sse-mark]'), 'the first SSE open consumed the mark').toHaveCount(0, { timeout: 10_000 });
  await page.waitForTimeout(3_500);
}

// reconnect drops the page's EventSource the way a network failure does and
// lets the htmx sse extension reconnect it (its onerror on a CLOSED source).
async function reconnect(page: Page) {
  await page.evaluate(() => {
    const src = (document.querySelector('[sse-connect]') as any)['htmx-internal-data'].sseEventSource;
    src.close();
    src.onerror(new Event('error'));
  });
}

test.beforeEach(async ({ page }) => {
  await page.addInitScript((k) => localStorage.removeItem(k), tagFilterKey);
});

test('/tasks carries the board in its own response, and the first SSE open fetches nothing', async ({ page }) => {
  const w = await watch(page);
  const resp = await page.goto('/tasks');
  expect(await resp!.text(), 'the /tasks response itself carries a seeded task').toContain(seeded);
  await expect(card(page, seeded)).toBeVisible();
  await firstOpenSettled(page);
  expect(await w.resyncs(), 'muster:resync fired by the first open').toBe(0);
  expect(w.fetches, 'GET /ui/tasks issued by the first paint and the first open').toEqual([]);
});

test('a reconnect re-fetches the board even when nothing changed', async ({ page }) => {
  const w = await watch(page);
  await page.goto('/tasks');
  await firstOpenSettled(page);
  expect(w.fetches, 'control: nothing fetched before the reconnect').toEqual([]);
  await reconnect(page);
  await expect.poll(() => w.fetches.length, { message: 'GET /ui/tasks after the reconnect', timeout: 10_000 }).toBeGreaterThan(0);
  expect(await w.resyncs()).toBeGreaterThan(0);
});

// 🔴 THE OFFLINE SHELL OUTLIVES THE SESSION THAT FILLED IT. The worker stores
// "/" at install and serves it to an offline open — after a sign-out too. The
// stored copy must hold no board (internal/api offlineShellParam, sw.js
// SHELL_FETCH). The auditor's path, end to end: signed in, the worker installs;
// signed out (cookies gone); offline; open "/".
test('the offline shell the worker stores holds no task, and an offline open after sign-out shows none', async ({ page, context }) => {
  await page.goto('/tasks');
  await expect(card(page, seeded), 'control: the signed-in board draws the seeded task').toBeVisible();
  await waitControlled(page);
  await expect.poll(() => page.evaluate(async () => !!(await caches.match('/'))), { message: 'the worker stored "/"' }).toBe(true);
  const stored = await page.evaluate(async () => (await (await caches.match('/'))!.text()));
  expect(stored, 'control: the stored "/" is the shell').toContain('id="tasks-list"');
  expect(stored, 'the stored "/" carries a seeded task').not.toContain(seeded);
  expect(stored, 'the stored "/" carries a drawn board').not.toContain('data-tasks-drawn');

  await context.clearCookies();
  await context.setOffline(true);
  try {
    await page.goto('/');
    await expect(page.locator('#tasks-list'), 'control: offline, "/" is the stored shell').toHaveCount(1);
    await expect(page.getByText(seeded), 'an offline open after sign-out shows a task').toHaveCount(0);
  } finally {
    await context.setOffline(false);
  }
});

test('an event between the document and the stream makes the first open re-fetch', async ({ page, api }) => {
  const w = await watch(page);
  // Hold the stream until the task exists: the board was drawn without it, and
  // its event went to no subscriber.
  let release!: () => void;
  const held = new Promise<void>((r) => (release = r));
  await page.route('**/events', async (route) => {
    await held;
    await route.continue();
  });
  await page.goto('/tasks');
  await expect(card(page, seeded)).toBeVisible();
  const title = `Created before the stream ${Date.now()}`;
  await api.createTask(title, 'Created by board-first-paint.spec.');
  expect(await card(page, title).count(), 'control: the drawn board predates the task').toBe(0);
  release();
  await expect(card(page, title)).toBeVisible({ timeout: 10_000 });
  expect(w.fetches.length, 'the first open re-fetched').toBeGreaterThan(0);
});

test('a stored tag filter hides the drawn board until the filtered one lands', async ({ page }) => {
  await page.addInitScript(([k]) => localStorage.setItem(k, JSON.stringify(['docs'])), [tagFilterKey]);
  let release!: () => void;
  const held = new Promise<void>((r) => (release = r));
  await page.route(/\/ui\/tasks\?.*tag=docs/, async (route) => {
    await held;
    await route.continue();
  });
  await page.goto('/tasks');
  // The unfiltered board is in the DOM (the document drew it) but not shown.
  await expect(card(page, seeded), 'control: the drawn board holds the unfiltered card').toHaveCount(1);
  await expect(card(page, seeded), 'the unfiltered card is hidden while the filtered board loads').toBeHidden();
  release();
  await expect(card(page, seededDocs)).toBeVisible({ timeout: 10_000 });
  await expect(card(page, seeded), 'the filtered board does not hold the unfiltered card').toHaveCount(0);
});

// --- Back and Forward (restore.go) -------------------------------------------
//
// A board shown again by Back is a document drawn in the past. Two ways:
//   - RESTORED from the back/forward cache: the same live document; restoreJS
//     reopens its streams, and that open is not the first (the mark was consumed
//     when the page first opened its stream), so it resyncs;
//   - re-run from the HTTP CACHE: the document as first served, its scripts run
//     afresh, carrying the OLD data-sse-mark into a FIRST open. Only the mark
//     comparison stands between that board and a skipped refetch.
// In both, a task created while away must be on the board after Back.

// backShown records, in the document, whether its last pageshow was a restore.
const pageshowRecorder = `addEventListener('pageshow', (e) => { window.__persisted = e.persisted; });`;

test.describe('Back', () => {
  let bfBrowser: Browser;
  test.beforeAll(async ({ playwright }) => {
    bfBrowser = await launchWithBfcache(playwright.chromium);
  });
  test.afterAll(async () => {
    await bfBrowser?.close();
  });

  test('a board restored from the back/forward cache shows a task created while it was away', async ({ api }) => {
    const ctx = await bfBrowser.newContext({ baseURL, storageState: authState, viewport: { width: 390, height: 844 } });
    await ctx.addInitScript(pageshowRecorder);
    await ctx.addInitScript((k) => localStorage.removeItem(k), tagFilterKey);
    const page = await ctx.newPage();
    try {
      await page.goto('/tasks');
      await firstOpenSettled(page);
      // The first worker claims its clients on activation, which evicts a cached
      // page (bfcache-restore.spec.ts, workerSettled): wait for it first.
      await page.evaluate(async () => {
        if (navigator.serviceWorker) await navigator.serviceWorker.ready;
      });
      const doc = await page.evaluate(() => ((window as any).__doc = Math.random().toString(36)));
      await page.goto('/agents');
      const title = `Created while the board was away ${Date.now()}`;
      await api.createTask(title, 'Created by board-first-paint.spec.');
      await page.goBack({ waitUntil: 'commit' });
      await expect.poll(() => page.evaluate(() => (window as any).__persisted), { message: 'Back restored the board' }).toBe(true);
      expect(await page.evaluate(() => (window as any).__doc), 'the same document').toBe(doc);
      await expect(card(page, title), 'the restored board caught up').toBeVisible({ timeout: 10_000 });
    } finally {
      await ctx.close();
    }
  });

  test('a board re-run from the HTTP cache carries its old mark, and still shows a task created while it was away', async ({ browser, api }) => {
    const ctx = await browser.newContext({ baseURL, storageState: authState, viewport: { width: 390, height: 844 } });
    await ctx.addInitScript((k) => localStorage.removeItem(k), tagFilterKey);
    const page = await ctx.newPage();
    try {
      await page.goto('/tasks');
      await firstOpenSettled(page);
      await page.goto('/agents');
      const title = `Created before the cached board ran ${Date.now()}`;
      await api.createTask(title, 'Created by board-first-paint.spec.');
      await page.goBack();
      const nav = await page.evaluate(() => {
        const n = performance.getEntriesByType('navigation')[0] as PerformanceNavigationTiming;
        return { type: n.type, fromCache: n.transferSize === 0 };
      });
      // Control: this IS the stored copy (an old document with an old mark), not
      // a fresh one the server drew after the task existed.
      expect(nav, 'Back took /tasks from the HTTP cache').toEqual({ type: 'back_forward', fromCache: true });
      await expect(card(page, title), 'the cached board re-fetched on its first open').toBeVisible({ timeout: 10_000 });
    } finally {
      await ctx.close();
    }
  });
});
