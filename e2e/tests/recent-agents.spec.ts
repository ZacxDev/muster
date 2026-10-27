import { Browser, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { baseURL, password } from '../lib/env';
import { touchDrag } from '../lib/touch';
import { agentLink } from '../lib/agent-link';

// RECENTLY VIEWED, in a real browser on the shared server: the session
// switcher's Recent view, docked at the bottom of the Agents list (it replaced
// the "Recently viewed" strip above the list — operator ask: "hide the pinned
// and recent on the main agent list page because the bottom bar flow
// supersedes it"): the entries the operator last opened, newest first, one tap
// each. It is stored on the SERVER per user (internal/recents, migration
// 0018), so a second browser context — its own sign-in, its own storage — sees
// the same view. Unlike the strip it keeps a pinned entry: Pins is a view of
// its own beside it.
//
// The seeded agents are shared by every spec (one database, one operator), so
// this asserts about the entries it opened itself, in the order it opened
// them, and unpins what it pinned.
//
// Phone-sized with touch: the row is a row of tap targets.
test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

const strip = (page: Page) => page.locator('#session-switcher [data-switcher-list="recent"]');
const entries = (page: Page) => strip(page).locator('a[data-switcher-entry]');
// Which agent each entry opens (its path; the query is the session it names,
// pinned by agent-open-hops.spec.ts).
const hrefs = (page: Page) => entries(page).evaluateAll((els) => els.map((e) => (e.getAttribute('href') || '').split('?')[0]));

async function openAgent(page: Page, name: string) {
  await page.goto('/agents/' + name);
  await page.waitForURL((u) => u.pathname === '/agents/' + name && u.searchParams.has('session'));
}

async function openList(page: Page) {
  // The Recent view is the one shown (the viewer's stored choice cleared).
  await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} }).catch(() => undefined);
  await page.goto('/agents');
  await expect(page.locator('#agents-list > article').first()).toBeVisible();
}

async function pin(page: Page, agentID: string, on: boolean) {
  const r = await page.request.post(on ? '/agents/pins' : '/agents/pins/delete', {
    form: { kind: 'agent', agent_id: agentID },
    headers: { Origin: baseURL, 'HX-Request': 'true' },
  });
  expect(r.status(), await r.text()).toBe(200);
}

async function freshContext(browser: Browser) {
  const ctx = await browser.newContext({ baseURL, viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await page.goto('/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  return { ctx, page };
}

let pinnedID = '';
test.afterEach(async ({ page }) => {
  if (pinnedID) await pin(page, pinnedID, false).catch(() => undefined);
  pinnedID = '';
});

// 🔴 A REGRESSION TEST: at origin/main the Agents list has no session switcher.
test('a visit shows in the Recent view, newest first, one tap to open; it persists to another browser; a pinned entry stays in it', async ({ page, browser }) => {
  await openAgent(page, 'docs-sweeper');
  await openAgent(page, 'release-helper');
  await openList(page);

  // Newest first: the two just opened lead the strip.
  await expect(strip(page)).toBeVisible();
  await expect.poll(() => hrefs(page)).toEqual(expect.arrayContaining(['/agents/release-helper', '/agents/docs-sweeper']));
  const h = await hrefs(page);
  expect(h.slice(0, 2), 'the last opened first').toEqual(['/agents/release-helper', '/agents/docs-sweeper']);
  expect(h.length, 'at most eight').toBeLessThanOrEqual(8);

  // Docked below the list, not one of its cards: the filter's count is of cards only.
  await expect(page.locator('#agents-list #session-switcher')).toHaveCount(0);
  const cards = await page.locator('#agents-list > article').count();
  await expect(page.locator('#agents-count')).toHaveText(`${cards} of ${cards}`);
  // Condensed and thumb-sized, and it never widens the page.
  const first = entries(page).first();
  expect((await first.boundingBox())!.height, 'a 44px tap target').toBeGreaterThanOrEqual(44);
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'no sideways page scroll').toBeLessThanOrEqual(cw);

  // Revisiting moves an entry to the front rather than adding it twice.
  await openAgent(page, 'docs-sweeper');
  await openList(page);
  await expect.poll(async () => (await hrefs(page)).slice(0, 2)).toEqual(['/agents/docs-sweeper', '/agents/release-helper']);
  expect((await hrefs(page)).filter((x) => x === '/agents/docs-sweeper'), 'one entry per agent').toHaveLength(1);

  // One tap opens it.
  await Promise.all([page.waitForURL((u) => u.pathname === '/agents/docs-sweeper'), first.tap()]);

  // Another browser — its own sign-in and its own storage — sees the same view.
  const other = await freshContext(browser);
  try {
    await openList(other.page);
    await expect.poll(async () => (await hrefs(other.page)).slice(0, 2)).toEqual(['/agents/docs-sweeper', '/agents/release-helper']);
  } finally {
    await other.ctx.close();
  }

  // A pinned entry stays in Recent (in its place), and is in Pins too.
  await openList(page);
  pinnedID = (await page.locator(`#agents-list > article:has(${agentLink('docs-sweeper')})`).getAttribute('id'))!.replace('agent-', '');
  await pin(page, pinnedID, true);
  await openList(page);
  await expect(page.locator('#agents-list > article').first(), 'pinned first in the list').toHaveId('agent-' + pinnedID);
  await expect.poll(async () => (await hrefs(page))[0], { message: 'still the newest in Recent' }).toBe('/agents/docs-sweeper');
  const pins = await page.locator('#session-switcher [data-switcher-list="pins"] a[data-switcher-entry]').evaluateAll((els) => els.map((e) => (e.getAttribute('href') || '').split('?')[0]));
  expect(pins, 'and in Pins').toContain('/agents/docs-sweeper');
  await pin(page, pinnedID, false);
  pinnedID = '';
});

// 🔴 A REGRESSION TEST: swiping the Recent strip sideways switched the PARENT
// tab. A horizontal swipe anywhere over a tabpanel used to walk the tab order
// (appScript's touch handler), and a sideways-scrolling row — the switcher's,
// now — is exactly where a thumb swipes sideways. The tab-switch swipe is
// gone; the sidebar's edge-swipe stays, and is pinned here in the same breath
// so the fix cannot have been "delete every touch gesture".
test('a sideways swipe on the switcher does not switch the tab; an edge-swipe still opens the sidebar', async ({ page }) => {
  await openAgent(page, 'docs-sweeper');
  await openAgent(page, 'release-helper');
  await openList(page);
  await expect(strip(page)).toBeVisible();
  const sb = (await strip(page).boundingBox())!;
  const bar = (await page.locator('#session-switcher').boundingBox())!;
  // The bar's middle line (its row is the bar's height; the row's own box
  // can run past the screen's edge).
  const y = Math.round(bar.y + bar.height / 2);
  // Instrument check: the swipes below start ON the bar, not on whatever is
  // under the screen's edge.
  const onBar = (x: number) => page.evaluate(([xx, yy]) => !!document.elementFromPoint(xx, yy)?.closest('#session-switcher'), [x, y]);
  expect(await onBar(sb.x + 30), 'the swipe point is on the bar').toBe(true);

  // Both directions, each well past the old 60px threshold and flat.
  for (const [from, to] of [[sb.x + sb.width - 30, sb.x + 30], [sb.x + 30, sb.x + sb.width - 30]]) {
    await touchDrag(page, { x: from, y }, { x: to, y });
    await page.waitForTimeout(250);
    expect(new URL(page.url()).pathname, 'the swipe left the tab alone').toBe('/agents');
    await expect(page.locator('#panel-agents')).toBeVisible();
  }

  // 🔴 The bar begins INSIDE the edge-swipe's 28px zone at this width, so a
  // rightward swipe that starts on it would open the sidebar. It is the bar's
  // swipe (appScript's ownsSideways, data-own-swipe).
  expect(bar.x, 'the bar starts within the 28px edge zone at 390px').toBeLessThan(28);
  expect(await onBar(Math.max(1, bar.x + 2)), 'the edge-zone swipe starts on the bar').toBe(true);
  await expect(page.locator('#sidebar-root')).not.toHaveAttribute('data-open', 'true');
  await touchDrag(page, { x: Math.max(1, bar.x + 2), y }, { x: bar.x + 220, y });
  await page.waitForTimeout(250);
  await expect(page.locator('#sidebar-root'), 'a swipe that began on the bar left the sidebar shut').not.toHaveAttribute('data-open', 'true');

  // The sidebar's edge-swipe: from the left edge, rightwards (above the bar).
  await touchDrag(page, { x: 4, y: 400 }, { x: 220, y: 404 });
  await expect(page.locator('#sidebar-root')).toHaveAttribute('data-open', 'true');
  expect(new URL(page.url()).pathname).toBe('/agents');
});
