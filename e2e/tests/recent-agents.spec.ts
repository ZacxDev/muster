import { Browser, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { baseURL, hookToken } from '../lib/env';
import { touchDrag } from '../lib/touch';

// RECENTLY VIEWED, in a real browser on the shared server: the session
// switcher's Recent view, on a session's page (the Agents list it was once
// docked on is retired; the bar is on every session page — an agent's chat, a
// mirrored window's views): the entries the operator last opened, newest
// first, one tap each. It is stored on the SERVER per user (internal/recents,
// migration 0018), so a second browser context — its own sign-in, its own
// storage — sees the same view. It keeps a pinned entry: Pins is a view of its
// own beside it.
//
// The seeded agents are shared by every spec (one database, one operator), so
// this asserts about the entries it opened itself, in the order it opened
// them, and unpins what it pinned. The page the view is read on is a THIRD
// seeded agent's (notes-tidy): opening it is a visit too, so its own entry —
// drawn current — is left out of the order read.
//
// Phone-sized with touch: the row is a row of tap targets.
test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

const VIEWER = 'notes-tidy';
const strip = (page: Page) => page.locator('#session-switcher [data-switcher-list="recent"]');
const entries = (page: Page) => strip(page).locator('a[data-switcher-entry]');
// Which agent each entry opens (its path; the query is the session it names,
// pinned by agent-open-hops.spec.ts), the viewing page's own left out.
const hrefs = (page: Page) =>
  entries(page).evaluateAll((els, viewer) => els.map((e) => (e.getAttribute('href') || '').split('?')[0]).filter((h) => h !== '/agents/' + viewer), VIEWER);

async function openAgent(page: Page, name: string) {
  await page.goto('/agents/' + name);
  await page.waitForURL((u) => u.pathname === '/agents/' + name && u.searchParams.has('session'));
}

// openBar opens the viewing agent's page with the Recent view shown (the
// viewer's stored choice cleared).
async function openBar(page: Page) {
  await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} }).catch(() => undefined);
  await openAgent(page, VIEWER);
  await expect(page.locator('#session-switcher')).toBeVisible();
}

async function agentID(name: string): Promise<string> {
  const r = await fetch(baseURL + '/api/agents', { headers: { Authorization: `Bearer ${hookToken}` } });
  expect(r.status).toBe(200);
  const a = ((await r.json()) as { id: number; name: string }[]).find((x) => x.name === name);
  expect(a, `${name} is listed`).toBeTruthy();
  return String(a!.id);
}

async function pin(page: Page, id: string, on: boolean) {
  const r = await page.request.post(on ? '/agents/pins' : '/agents/pins/delete', {
    form: { kind: 'agent', agent_id: id },
    headers: { Origin: baseURL, 'HX-Request': 'true' },
  });
  expect(r.status(), await r.text()).toBe(200);
}

async function freshContext(browser: Browser) {
  const ctx = await browser.newContext({ baseURL, viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await signIn(page);
  return { ctx, page };
}

let pinnedID = '';
test.afterEach(async ({ page }) => {
  if (pinnedID) await pin(page, pinnedID, false).catch(() => undefined);
  pinnedID = '';
});

test('a visit shows in the Recent view, newest first, one tap to open; it persists to another browser; a pinned entry stays in it', async ({ page, browser }) => {
  await openAgent(page, 'docs-sweeper');
  await openAgent(page, 'release-helper');
  await openBar(page);

  // Newest first: the two just opened lead the strip (after the page's own).
  await expect(strip(page)).toBeVisible();
  await expect.poll(() => hrefs(page)).toEqual(expect.arrayContaining(['/agents/release-helper', '/agents/docs-sweeper']));
  const h = await hrefs(page);
  expect(h.slice(0, 2), 'the last opened first').toEqual(['/agents/release-helper', '/agents/docs-sweeper']);
  expect(await entries(page).count(), 'at most eight').toBeLessThanOrEqual(8);

  // Condensed and thumb-sized, and it never widens the page.
  const firstOther = strip(page).locator('a[data-switcher-entry]:not([data-switcher-current])').first();
  expect((await firstOther.boundingBox())!.height, 'a 44px tap target').toBeGreaterThanOrEqual(44);
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'no sideways page scroll').toBeLessThanOrEqual(cw);

  // Revisiting moves an entry to the front rather than adding it twice.
  await openAgent(page, 'docs-sweeper');
  await openBar(page);
  await expect.poll(async () => (await hrefs(page)).slice(0, 2)).toEqual(['/agents/docs-sweeper', '/agents/release-helper']);
  expect((await hrefs(page)).filter((x) => x === '/agents/docs-sweeper'), 'one entry per agent').toHaveLength(1);

  // One tap opens it.
  const sweeper = strip(page).locator('a[data-switcher-entry][href^="/agents/docs-sweeper"]');
  await Promise.all([page.waitForURL((u) => u.pathname === '/agents/docs-sweeper'), sweeper.tap()]);

  // Another browser — its own sign-in and its own storage — sees the same view.
  const other = await freshContext(browser);
  try {
    await openBar(other.page);
    await expect.poll(async () => (await hrefs(other.page)).slice(0, 2)).toEqual(['/agents/docs-sweeper', '/agents/release-helper']);
  } finally {
    await other.ctx.close();
  }

  // A pinned entry stays in Recent (in its place), and is in Pins too.
  pinnedID = await agentID('docs-sweeper');
  await openAgent(page, 'docs-sweeper');
  await pin(page, pinnedID, true);
  await openBar(page);
  await expect.poll(async () => (await hrefs(page))[0], { message: 'still the newest in Recent' }).toBe('/agents/docs-sweeper');
  const pins = await page.locator('#session-switcher [data-switcher-list="pins"] a[data-switcher-entry]').evaluateAll((els) => els.map((e) => (e.getAttribute('href') || '').split('?')[0]));
  expect(pins, 'and in Pins').toContain('/agents/docs-sweeper');
  await pin(page, pinnedID, false);
  pinnedID = '';
});

// 🔴 A REGRESSION TEST: swiping the Recent strip sideways switched the PARENT
// tab. A horizontal swipe anywhere over a tabpanel used to walk the tab order
// (appScript's touch handler), and a sideways-scrolling row — the switcher's —
// is exactly where a thumb swipes sideways. The tab-switch swipe is gone; the
// sidebar's edge-swipe stays, and is pinned here in the same breath so the fix
// cannot have been "delete every touch gesture". (Read on a session page now:
// the Agents tab the bar was docked on is retired.)
test('a sideways swipe on the switcher leaves the page alone; an edge-swipe still opens the sidebar', async ({ page }) => {
  await openAgent(page, 'docs-sweeper');
  await openAgent(page, 'release-helper');
  await openBar(page);
  const at = page.url();
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
    expect(page.url(), 'the swipe left the page alone').toBe(at);
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
  expect(page.url()).toBe(at);
});
