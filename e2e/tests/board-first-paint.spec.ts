import { test, expect } from '../lib/fixtures';
import type { Page } from '@playwright/test';

// THE BOARD IS IN THE `/` DOCUMENT, AND THE FIRST SSE OPEN DOES NOT FETCH IT
// AGAIN (internal/ui/tasks_list_ssr.go).
//
// `/` used to draw a skeleton and fill it from its own GET /ui/tasks, and the
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
// between runs while the stream is down.
async function reconnect(page: Page, between: () => Promise<void> = async () => {}) {
  await page.evaluate(() => {
    const el = document.querySelector('[sse-connect]') as any;
    el['htmx-internal-data'].sseEventSource.close();
  });
  await between();
  await page.evaluate(() => {
    const src = (document.querySelector('[sse-connect]') as any)['htmx-internal-data'].sseEventSource;
    src.onerror(new Event('error'));
  });
}

test.beforeEach(async ({ page }) => {
  await page.addInitScript((k) => localStorage.removeItem(k), tagFilterKey);
});

test('/ carries the board in its own response, and the first SSE open fetches nothing', async ({ page }) => {
  const w = await watch(page);
  const resp = await page.goto('/');
  expect(await resp!.text(), 'the / response itself carries a seeded task').toContain(seeded);
  await expect(card(page, seeded)).toBeVisible();
  await firstOpenSettled(page);
  expect(await w.resyncs(), 'muster:resync fired by the first open').toBe(0);
  expect(w.fetches, 'GET /ui/tasks issued by the first paint and the first open').toEqual([]);
});

test('a reconnect re-fetches the board even when nothing changed', async ({ page }) => {
  const w = await watch(page);
  await page.goto('/');
  await firstOpenSettled(page);
  expect(w.fetches, 'control: nothing fetched before the reconnect').toEqual([]);
  await reconnect(page);
  await expect.poll(() => w.fetches.length, { message: 'GET /ui/tasks after the reconnect', timeout: 10_000 }).toBeGreaterThan(0);
  expect(await w.resyncs()).toBeGreaterThan(0);
});

test('a task created while the stream was down is on the board after the reconnect', async ({ page, api }) => {
  await page.goto('/');
  await firstOpenSettled(page);
  const title = `Missed while offline ${Date.now()}`;
  await reconnect(page, async () => {
    await api.createTask(title, 'Created by board-first-paint.spec.');
  });
  await expect(card(page, title)).toBeVisible({ timeout: 10_000 });
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
  await page.goto('/');
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
  await page.goto('/');
  // The unfiltered board is in the DOM (the document drew it) but not shown.
  await expect(card(page, seeded), 'control: the drawn board holds the unfiltered card').toHaveCount(1);
  await expect(card(page, seeded), 'the unfiltered card is hidden while the filtered board loads').toBeHidden();
  release();
  await expect(card(page, seededDocs)).toBeVisible({ timeout: 10_000 });
  await expect(card(page, seeded), 'the filtered board does not hold the unfiltered card').toHaveCount(0);
});
