import { execFileSync } from 'node:child_process';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { asksScrollPort, dsn, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// THE ASKS PAGE OPENS AT ITS TOP (operator: "The Asks page always opens
// partially scrolled down"), in a real browser against a real muster-server
// with plugins on, at a phone (390×844, touch) and a desktop (1440×900), over
// a queue long enough to scroll (e2e/asksfiltfix):
//
//   - a load of each tab, a search result page and Back to the page land at
//     scrollY 0 (invariant guards: the base lands there too);
//   - 🔴 a LIVE REFRESH never moves the page: after an answer moved focus to
//     the next card deep in the feed and the reader scrolled back up, the
//     next refresh (the queue's event — here another card dismissed elsewhere;
//     the 60 s backstop and the app coming back to the foreground run the
//     same refresh) pulled the page back down to that card (ui/asks_card.go's
//     reveal handler, focusCard → scrollIntoView). RED at the base: scrollY
//     1200 at 390×844;
//   - 🔴 a TAB SWITCH lands at the top: the new tab opened at the old tab's
//     offset. RED at the base;
//   - 🔴 a tab switch DROPS the fragment a decision landed on (#draft-N): it
//     re-opened the Drafts tab and its card on every reload. RED at the base;
//   - a deliberate jump still lands on its card: #ask-N, and a search for a
//     draft's id (its card, ?tab=drafts&draft=N#draft-N).

const origin = `http://localhost:${asksScrollPort}`;
const stamp = 'scroll' + Date.now().toString(36);
let server: Running | undefined;
type Ids = Record<string, number>;
let ids: Ids;

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/asksfiltfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, asksScrollPort, { MUSTER_PLUGINS: '1', MUSTER_FOCUS_INTERVAL: '0' });
});

test.beforeEach(async () => {
  ids = JSON.parse(fix('seed', '1', stamp)) as Ids;
});

test.afterAll(async () => {
  try {
    fix('clear', '1', stamp);
  } finally {
    await server?.stop();
  }
});

// scrollY once the page has settled: its scripts, its fonts, a frame.
async function settledY(page: Page): Promise<number> {
  await page.waitForLoadState('load');
  await page.evaluate(() => (document.fonts ? document.fonts.ready.then(() => undefined) : undefined));
  await page.waitForTimeout(600);
  return page.evaluate(() => Math.round(window.scrollY));
}

const selectedTab = (page: Page) => page.locator('#asks-tabs [role="tab"][aria-selected="true"]');

for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', use: { viewport: { width: 1440, height: 900 } } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('every tab, a search and Back land at the top (invariant)', async ({ page }) => {
      await signIn(page, origin);
      await page.goto(origin + '/attention');
      await expect(page.locator(`#ask-${ids.mkt_ask}`)).toBeVisible();
      // The page is long enough to scroll (else 0 is no measurement).
      expect(await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight)).toBeGreaterThan(400);
      expect(await settledY(page), 'Open').toBe(0);
      for (const tab of ['done', 'drafts']) {
        await page.goto(origin + '/attention?tab=' + tab);
        await expect(selectedTab(page)).toHaveAttribute('data-tab', tab);
        expect(await settledY(page), tab).toBe(0);
      }
      await page.goto(origin + '/attention?q=release');
      await expect(page.locator('[data-testid="asks-search-results"]')).toBeVisible();
      expect(await settledY(page), 'a search result page').toBe(0);
      await page.goto(origin + '/attention');
      await page.goto(origin + '/agents');
      await page.goBack();
      await expect(page.locator(`#ask-${ids.mkt_ask}`)).toBeVisible();
      expect(await settledY(page), 'Back').toBe(0);
    });

    // 🔴 A PAGE THAT OPENS BEFORE IT CAN SCROLL STAYS AT THE TOP ONCE IT CAN.
    // With the feed's scroll snap on while the document has no scroll range,
    // every snap position clamps to 0, Chromium picks the first card among
    // equals, and re-snaps to it the moment the page can scroll: the page
    // "opens partially scrolled down". A slow load does it (one card laid out,
    // then the rest: the flake this was found by, 154px at 1440×900); here a
    // window opened taller than the page and then made shorter does it every
    // time. RED at the base: scrollY 154 at both sizes.
    test('a page opened before it could scroll stays at the top once it can, and the feed still snaps', async ({ page }) => {
      const { width, height } = vp.use.viewport;
      await page.setViewportSize({ width, height: 10_000 });
      await signIn(page, origin);
      await page.goto(origin + '/attention');
      await expect(page.locator(`#ask-${ids.mkt_ask}`)).toBeVisible();
      await page.waitForLoadState('load');
      // Nothing to scroll yet (else this is the invariant above again).
      expect(await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight)).toBe(0);
      await page.setViewportSize({ width, height });
      // The layout after the resize, and the frame after it (a re-snap is not
      // smooth: it lands in that layout).
      await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(() => r(undefined)))));
      expect(await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight)).toBeGreaterThan(400);
      expect(await page.evaluate(() => Math.round(window.scrollY)), 'the page re-snapped to the first card').toBe(0);
      // Still a snapping feed: a wheel that stops between cards settles on a
      // card's top, under the sticky header (its scroll padding).
      await expect(page.locator('html')).toHaveAttribute('data-asks-snap', '');
      await page.mouse.move(width / 2, height / 2);
      // Settled: the browser's scrollend, which comes once the scroll AND its
      // snap are done.
      await page.evaluate(() => {
        (window as unknown as { __ended: Promise<void> }).__ended = new Promise((r) => window.addEventListener('scrollend', () => r(), { once: true }));
      });
      await page.mouse.wheel(0, 333);
      await page.evaluate(() => (window as unknown as { __ended: Promise<void> }).__ended);
      const { y, tops } = await page.evaluate(() => {
        const pad = parseFloat(getComputedStyle(document.documentElement).scrollPaddingTop);
        return {
          y: Math.round(window.scrollY),
          tops: Array.from(document.querySelectorAll('#attention-items > li[data-asks-card]')).map((c) => Math.round(c.getBoundingClientRect().top + window.scrollY - pad)),
        };
      });
      expect(y, 'the wheel scrolled').toBeGreaterThan(0);
      expect(Math.min(...tops.map((t) => Math.abs(t - y))), `the wheel settled on a card's top (scrollY ${y}, tops ${tops.slice(0, 6)})`).toBeLessThanOrEqual(1);
    });

    test('a live refresh never pulls the page down to the last focused card', async ({ page }) => {
      await signIn(page, origin);
      await page.goto(origin + '/attention');
      // Answer the tenth card: focus moves to the card after it, deep in the
      // feed (the page scrolls there — the reader asked for that).
      const card = page.locator('#attention-items > li[data-asks-card]').nth(9);
      await card.scrollIntoViewIfNeeded();
      const answered = await card.getAttribute('id');
      await card.locator('[data-attention-option]').first().click();
      // It settles in place (internal/api asks_done.go), and stays.
      await expect(page.locator(`[id="${answered}"]`)).toHaveAttribute('data-asks-settled', '');
      const focused = await page.evaluate(() => document.activeElement?.id ?? '');
      expect(focused, 'focus moved to the next card').toMatch(/^ask-\d+$/);
      expect(await page.evaluate(() => Math.round(window.scrollY)), 'the answer scrolled to the next card').toBeGreaterThan(400);
      // The reader scrolls back to the top.
      await page.evaluate(() => window.scrollTo(0, 0));
      // Another card is dismissed elsewhere: the queue's event, a refresh.
      // (Counted: the open cards; the settled one is kept through it.)
      const before = await page.locator('#attention-items > li[data-asks-card]:not([data-asks-settled])').count();
      const st = await page.evaluate((id) => fetch('/attention/asks/' + id + '/cancel', { method: 'POST', credentials: 'same-origin' }).then((r) => r.status), ids.product_ask);
      expect(st, 'the dismissal elsewhere').toBeLessThan(400);
      await expect(page.locator(`#ask-${ids.product_ask}`), 'the refresh ran').toHaveCount(0);
      expect(await page.locator('#attention-items > li[data-asks-card]:not([data-asks-settled])').count()).toBe(before - 1);
      await expect(page.locator(`[id="${answered}"]`), 'the settled card is kept through the refresh').toHaveAttribute('data-asks-settled', '');
      expect(await settledY(page), 'the refresh moved the page').toBe(0);
      // Focus came back to the card it was on (without moving the page).
      expect(await page.evaluate(() => document.activeElement?.id ?? '')).toBe(focused);
    });

    test('a tab switch lands at the top of the new tab, and drops a decision\'s fragment', async ({ page }) => {
      await signIn(page, origin);
      await page.goto(origin + '/attention');
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      expect(await page.evaluate(() => window.scrollY)).toBeGreaterThan(400);
      // A SWIPE switches the tab by clicking it from script (ui/tab_swipe.go):
      // no focus moves, so nothing but the page's own script can bring the
      // new tab's top into view.
      await page.evaluate(() => (document.getElementById('asks-tabs-tab-done') as HTMLElement).click());
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'done');
      // The Done tab is long too, so an offset carried over would show.
      expect(await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight)).toBeGreaterThan(400);
      expect(await settledY(page), 'the new tab opened part-way down').toBe(0);
      // A tap on a tab, the same.
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      await page.locator('#asks-tabs-tab-open').click();
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'open');
      expect(await settledY(page), 'a tapped tab opened part-way down').toBe(0);

      // Where a decision lands: its draft's card, by its fragment.
      await page.goto(origin + `/attention?tab=drafts#draft-${ids.agent_draft}`);
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'drafts');
      await page.locator('#asks-tabs-tab-open').click();
      await expect(page).toHaveURL(origin + '/attention');
      await page.reload();
      await expect(selectedTab(page), 'the reload re-opened the tab the fragment named').toHaveAttribute('data-tab', 'open');
      expect(await settledY(page)).toBe(0);
    });

    test('a deliberate jump still lands on its card', async ({ page }) => {
      await signIn(page, origin);
      // An ask's fragment, deep in the Open feed.
      const deep = await (async () => {
        await page.goto(origin + '/attention');
        return page.locator('#attention-items > li[data-asks-card]').nth(12).getAttribute('id');
      })();
      await page.goto(origin + `/attention#${deep}`);
      await expect(page.locator(`[id="${deep}"]`)).toBeVisible();
      expect(await settledY(page), 'the page scrolled to the card').toBeGreaterThan(400);
      // At the top of the screen, under the sticky header (its scroll margin).
      const top = await page.locator(`[id="${deep}"]`).evaluate((e) => Math.round(e.getBoundingClientRect().top));
      expect(top, 'the card is at the top of the screen').toBeGreaterThanOrEqual(0);
      expect(top, 'the card is at the top of the screen').toBeLessThan(160);
      // A jump INTO another tab (#ask-N on ?tab=answered — the Done tab's old
      // name — opens Open): the tab it opened is the tab on screen, so a tap
      // back to Done is a tab change — the top of the page, the fragment
      // dropped.
      await page.goto(origin + `/attention?tab=answered#${deep}`);
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'open');
      expect(await settledY(page), 'the jump scrolled to its card').toBeGreaterThan(400);
      await page.locator('#asks-tabs-tab-done').click();
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'done');
      expect(await settledY(page), 'the tap back landed part-way down').toBe(0);
      expect(await page.evaluate(() => location.hash), 'the tap back kept the fragment').toBe('');
      // A search for a draft's id goes to its card on the Drafts tab (the
      // agent's reply draft: no step-up card names it, so the id finds one
      // thing).
      await page.goto(origin + `/attention?q=${encodeURIComponent('draft ' + ids.agent_draft)}`);
      await expect(page).toHaveURL(new RegExp(`#draft-${ids.agent_draft}$`));
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'drafts');
      await expect(page.locator(`#draft-${ids.agent_draft}`)).toBeInViewport();
    });
  });
}
