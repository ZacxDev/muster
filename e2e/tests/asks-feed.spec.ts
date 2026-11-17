import { execFileSync } from 'node:child_process';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { asksFeedPort, dsn, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { touchDrag } from '../lib/touch';

// THE ASKS CARD FEED (internal/ui/asks_card.go, internal/api/asks_feed.go;
// docs/attention.md "The card feed"), in a real browser against a real
// muster-server with plugins, task routing and chief nudges on — so the Open,
// Answered, Drafts, Nudges and Log tabs are all drawn — at a phone (390×844)
// and a desktop (1280×800), both with touch:
//
//   - a sideways swipe on the feed switches tabs, in its direction, and the
//     address follows; a vertical scroll never does (with a positive control:
//     the page DID scroll);
//   - a tap on a card expands it in place, and again collapses it; its
//     "View in session" is the asking agent's page, at the card's bottom;
//   - tapping an option answers it: the card leaves, the next card takes
//     focus, the open count drops, and the server has the answer;
//   - every answerable card carries the dictation mic;
//   - the keyboard: j/k move focus between cards (visibly), Enter expands, a
//     focuses the answer box, d starts the mic, Esc leaves the box, then
//     collapses;
//   - Dismiss is sent at once (an htmx post answered 204), the next card
//     takes focus; a live refresh while a card leaves never brings it back;
//   - prefers-reduced-motion: reduce turns every card transition off.
//
// The queue is fixture rows (e2e/asksfeedfix) written through the real store
// and re-seeded before every test.
//
// 🔴 REGRESSION: on origin/main the page is grouped lists with no swipe, no
// expand, no Answered tab and no keys — every test here is red there.

const origin = `http://localhost:${asksFeedPort}`;
const stamp = Date.now().toString(36);
const agent = 'e2e-feed-' + stamp;
let server: Running | undefined;
type Ids = { deploy: number; freetext: number; rotate: number; notice: number; answered: number; draft: number; task: number };
let ids: Ids;

// A fake microphone (a tone), so d's recording starts without a prompt.
test.use({ launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] } });

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/asksfeedfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, asksFeedPort, {
    MUSTER_PLUGINS: '1',
    MUSTER_TASK_ROUTING: '1',
    MUSTER_CHIEF_NUDGES: '1',
    MUSTER_FOCUS_INTERVAL: '0',
    // Dictation ON (the mic is drawn); nothing here records to the end, so
    // the service is never reached.
    MUSTER_STT_URL: 'http://127.0.0.1:9',
    MUSTER_STT_TOKEN: 'e2e-stt-token-not-a-real-one',
  });
});

test.beforeEach(async () => {
  ids = JSON.parse(fix('seed', '1', agent)) as Ids;
});

test.afterAll(async () => {
  try {
    fix('clear', '1', agent);
  } finally {
    await server?.stop();
  }
});

const selectedTab = (page: Page) => page.locator('#asks-tabs [role="tab"][aria-selected="true"]');
const openCount = (page: Page) => page.locator('[data-asks-open-count]');
// The open count as a number. The suite's database is shared within a leg, so
// an earlier spec's open asks may be on the page too: counts are asserted
// relative to the one read here, never as absolutes.
const openN = async (page: Page) => Number(((await openCount(page).textContent()) ?? '').replace(' open', ''));

async function openFeed(page: Page, path = '/attention') {
  await signIn(page, origin);
  await page.goto(origin + path);
  await expect(page.locator(`#ask-${ids.deploy}`)).toBeVisible();
}

for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: true } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] }, permissions: ['microphone'] });

    test('a sideways swipe switches tabs and the address; a vertical scroll does not', async ({ page }) => {
      await openFeed(page);
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'open');
      // swipe drags across the feed area as it is NOW (measured each time: a
      // tab change moves the page), at a height inside the viewport.
      const swipe = async (dir: 'left' | 'right') => {
        const a = (await page.locator('[data-asks-swipe]').boundingBox())!;
        const y = Math.max(140, Math.min(a.y + 150, vp.use.viewport.height - 80));
        const l = a.x + a.width * 0.85, r = a.x + a.width * 0.15;
        if (dir === 'left') await touchDrag(page, { x: l, y }, { x: r, y: y + 6 });
        else await touchDrag(page, { x: r, y }, { x: l, y: y - 4 });
      };
      // Leftwards: the next tab.
      await swipe('left');
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'answered');
      await expect(page).toHaveURL(`${origin}/attention?tab=answered`);
      await expect(page.locator(`#answered-${ids.answered}`)).toBeVisible();
      // Again: Drafts.
      await swipe('left');
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'drafts');
      await expect(page).toHaveURL(`${origin}/attention?tab=drafts`);
      // Rightwards twice: back to Open, its address bare.
      await swipe('right');
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'answered');
      await swipe('right');
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'open');
      await expect(page).toHaveURL(`${origin}/attention`);
      // On the first tab a rightwards swipe goes nowhere.
      await swipe('right');
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'open');

      // A VERTICAL scroll, drifting LEFT by more than a swipe's minimum (as
      // a thumb does): it is locked vertical at its first moves, so no tab
      // change — and the page did scroll (the gesture was real, not ignored).
      await page.evaluate(() => window.scrollTo(0, 0));
      const before = await page.evaluate(() => window.scrollY);
      const area = (await page.locator('[data-asks-swipe]').boundingBox())!;
      const x0 = area.x + area.width * 0.75;
      await touchDrag(page, { x: x0, y: vp.use.viewport.height - 120 }, { x: x0 - Math.max(110, area.width * 0.25), y: 160 }, 16);
      await page.waitForTimeout(300);
      await expect(selectedTab(page), 'a vertical scroll changed the tab').toHaveAttribute('data-tab', 'open');
      await expect(page).toHaveURL(`${origin}/attention`);
      await expect.poll(() => page.evaluate(() => window.scrollY), { message: 'the vertical drag scrolled the page' }).toBeGreaterThan(before);

      // The same, DOWNWARDS at the page's top, where there is nothing to scroll:
      // the browser never takes the gesture (no touchcancel), so only the axis
      // lock keeps this mostly-vertical drag from switching the tab.
      await expect
        .poll(async () => {
          await page.evaluate(() => window.scrollTo({ top: 0, behavior: 'instant' }));
          await page.waitForTimeout(250);
          return page.evaluate(() => window.scrollY);
        }, { timeout: 15_000 })
        .toBe(0);
      const top = (await page.locator('[data-asks-swipe]').boundingBox())!;
      const tx = top.x + top.width * 0.75;
      await touchDrag(page, { x: tx, y: top.y + 20 }, { x: tx - Math.max(110, top.width * 0.25), y: top.y + 520 }, 16);
      await page.waitForTimeout(300);
      await expect(selectedTab(page), 'a downward drag at the top changed the tab').toHaveAttribute('data-tab', 'open');

      // A sideways swipe that starts in the answer box is the box's (measured
      // once the page has settled at the top: a snap may still be moving it).
      // (The drag above ends in a fling, which keeps scrolling for a while.)
      await expect
        .poll(async () => {
          await page.evaluate(() => window.scrollTo({ top: 0, behavior: 'instant' }));
          await page.waitForTimeout(250);
          return page.evaluate(() => window.scrollY);
        }, { timeout: 15_000 })
        .toBe(0);
      const box = (await page.locator(`#answer-${ids.freetext}`).boundingBox())!;
      await touchDrag(page, { x: box.x + box.width - 10, y: box.y + box.height / 2 }, { x: box.x + 10 - box.width * 0.2, y: box.y + box.height / 2 });
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'open');
    });

    test('a tap expands a card in place and collapses it; View in session is at its bottom', async ({ page }) => {
      await openFeed(page);
      const card = page.locator(`#ask-${ids.deploy}`);
      const toggle = card.locator('[data-asks-toggle]');
      const since = card.locator('[data-asks-since]');
      await expect(card).toHaveAttribute('data-expanded', 'false');
      await expect(toggle).toHaveAttribute('aria-expanded', 'false');
      await expect(since, 'the more region is shut').toBeHidden();
      // A tap on the question (not a control) opens it.
      await card.locator('[data-attention-text]').tap();
      await expect(card).toHaveAttribute('data-expanded', 'true');
      await expect(toggle).toHaveAttribute('aria-expanded', 'true');
      await expect(since).toBeVisible();
      // View in session: the asking agent's page, the card's last row.
      const view = card.locator('[data-asks-view-session]');
      await expect(view).toHaveText('View in session');
      await expect(view).toHaveAttribute('href', `/agents/${agent}`);
      const cardBox = (await card.boundingBox())!, viewBox = (await view.boundingBox())!, sinceBox = (await since.boundingBox())!;
      expect(viewBox.y, 'View in session sits below the expanded context').toBeGreaterThan(sinceBox.y);
      expect(cardBox.y + cardBox.height - (viewBox.y + viewBox.height), 'and at the card’s bottom').toBeLessThan(24);
      // The task link sits beside it.
      await expect(card.locator('[data-asks-task]')).toHaveAttribute('href', `/tasks/${ids.task}`);
      // A tap on the head collapses it again.
      await toggle.tap();
      await expect(card).toHaveAttribute('data-expanded', 'false');
      await expect(since).toBeHidden();
      // A tap on an option does NOT expand (it answers): covered below; the
      // link goes where it says.
      await view.tap();
      await expect(page).toHaveURL(new RegExp(`/agents/${agent}(\\?|$)`));
    });

    test('tapping an option answers it: the card leaves and the next card takes focus', async ({ page }) => {
      await openFeed(page);
      const n0 = await openN(page);
      expect(n0, 'the fixture\u2019s five cards are counted').toBeGreaterThanOrEqual(5);
      const rotate = page.locator(`#ask-${ids.rotate}`);
      // The card after it in the feed (with only the fixture open: the notice,
      // questions coming first; an older open question from another spec on
      // the shared database would sit between them).
      const nextId = await rotate.locator('xpath=following-sibling::li[1]').getAttribute('id');
      expect(nextId, 'a card follows the one answered').toBeTruthy();
      const next = page.locator(`[id="${nextId}"]`);
      await rotate.getByRole('button', { name: 'Rotate it', exact: true }).click();
      await expect(rotate).toHaveCount(0);
      await expect(next).toBeFocused();
      await expect(openCount(page)).toHaveText(`${n0 - 1} open`);
      await expect.poll(async () => (await page.request.get(`${origin}/attention/asks/${ids.rotate}`)).text()).toContain('Your answer: Rotate it');
      // Still gone after a reload: the server has it.
      await page.reload();
      await expect(page.locator(`#ask-${ids.rotate}`)).toHaveCount(0);
    });

    test('every answerable card carries the dictation mic', async ({ page }) => {
      await openFeed(page);
      // The fixture agent's question cards (another spec's may be open too).
      const answerable = page.locator('#attention-items [data-asks-card][data-kind="ask"]').filter({ has: page.locator('[data-attention-title]', { hasText: agent }) });
      await expect(answerable).toHaveCount(3);
      for (const card of await answerable.all()) {
        await expect(card.locator('button[data-dictate]'), `${await card.getAttribute('id')} has its mic`).toHaveCount(1);
        await expect(card.locator('textarea[name="answer"]')).toHaveCount(1);
      }
      // A notice has nothing to answer: no box, no mic.
      await expect(page.locator(`#ask-${ids.notice} button[data-dictate]`)).toHaveCount(0);
    });

    test('the keyboard moves between cards, expands, answers and collapses', async ({ page }) => {
      await openFeed(page);
      const first = page.locator(`#ask-${ids.deploy}`), second = page.locator(`#ask-${ids.freetext}`);
      await page.locator('h1').click();
      await page.keyboard.press('j');
      await expect(first).toBeFocused();
      expect(await first.evaluate((el) => el.matches(':focus-visible')), 'the focus is visible').toBe(true);
      await page.keyboard.press('j');
      await expect(second).toBeFocused();
      await page.keyboard.press('ArrowDown');
      await expect(page.locator(`#ask-${ids.rotate}`)).toBeFocused();
      await page.keyboard.press('k');
      await page.keyboard.press('ArrowUp');
      await expect(first).toBeFocused();
      await page.keyboard.press('Enter');
      await expect(first).toHaveAttribute('data-expanded', 'true');
      await page.keyboard.press('a');
      await expect(page.locator(`#answer-${ids.deploy}`)).toBeFocused();
      await page.keyboard.type('hold');
      await expect(page.locator(`#answer-${ids.deploy}`), 'a typed a, then the text; no shortcut fired').toHaveValue('hold');
      await page.keyboard.press('Escape');
      await expect(first).toBeFocused();
      await page.keyboard.press('Escape');
      await expect(first).toHaveAttribute('data-expanded', 'false');
      // d: the card's mic starts (the fake device answers).
      const mic = first.locator('button[data-dictate]');
      await expect(mic).toHaveAttribute('data-state', 'idle');
      await page.keyboard.press('d');
      await expect(mic).not.toHaveAttribute('data-state', 'idle');
    });

    test('Dismiss is sent at once: the card leaves and the next takes focus', async ({ page }) => {
      await openFeed(page);
      const card = page.locator(`#ask-${ids.freetext}`);
      const nextId = await card.locator('xpath=following-sibling::li[1]').getAttribute('id');
      const n0 = await openN(page);
      const sent = page.waitForResponse((r) => new URL(r.url()).pathname === `/attention/asks/${ids.freetext}/cancel` && r.request().method() === 'POST');
      await card.getByRole('button', { name: 'Dismiss without answering' }).click();
      expect((await sent).status(), 'the card post is answered 204').toBe(204);
      await expect(card).toHaveCount(0);
      await expect(page.locator(`[id="${nextId}"]`)).toBeFocused();
      await expect(openCount(page)).toHaveText(`${n0 - 1} open`);
      await expect(page.locator('[data-asks-toast]'), 'no Undo toast').toHaveCount(0);
      expect((await page.request.get(`${origin}/attention/asks/${ids.freetext}`)).ok()).toBe(true);
      await expect.poll(async () => (await page.request.get(`${origin}/attention/asks/${ids.freetext}`)).text()).toContain('Dismissed');
    });

    // 🔴 A LEAVING CARD IS NOT RESURRECTED. The answer's POST is held in the
    // network while the queue changes elsewhere: the page hears the event and
    // re-draws the feed from the server, where the ask is still open. The card
    // must stay gone (it is leaving), and settle once the POST lands.
    test('an attention event while a card leaves does not bring it back', async ({ page }) => {
      await openFeed(page);
      const rotate = page.locator(`#ask-${ids.rotate}`);
      let release!: () => void;
      const gate = new Promise<void>((r) => (release = r));
      await page.route(`**/attention/asks/${ids.rotate}/answer`, async (route) => {
        await gate;
        await route.continue();
      });
      await rotate.getByRole('button', { name: 'Rotate it', exact: true }).click();
      await expect(rotate).toBeHidden();
      // Positive control: the refresh really runs, and really lists the card.
      // (The Open feed's refresh: /attention/items with no query — the
      // Answered feed's is ?part=answered.)
      const refreshed = page.waitForResponse((r) => {
        const u = new URL(r.url());
        return u.pathname === '/attention/items' && u.search === '' && r.status() === 200;
      });
      const dismissed = await page.request.post(`${origin}/attention/asks/${ids.notice}/cancel`, { headers: { Origin: origin }, form: { x: '' } });
      expect(dismissed.status(), await dismissed.text()).toBeLessThan(400);
      const body = await (await refreshed).text();
      expect(body, 'the refresh still lists the leaving card (its answer has not landed)').toContain(`id="ask-${ids.rotate}"`);
      await expect(page.locator(`#ask-${ids.notice}`), 'the refresh was applied').toHaveCount(0);
      await page.waitForTimeout(300);
      await expect(rotate, 'the leaving card came back').toBeHidden();
      release();
      await expect(rotate).toHaveCount(0);
      await expect.poll(async () => (await page.request.get(`${origin}/attention/asks/${ids.rotate}`)).text()).toContain('Your answer: Rotate it');
    });

    test('reduced motion turns the card transitions off', async ({ page }) => {
      const durations = () =>
        page.evaluate(() => {
          const card = document.querySelector('[data-asks-card]')!, more = card.querySelector('.asks-more')!;
          return [getComputedStyle(card).transitionDuration, getComputedStyle(more).transitionDuration];
        });
      // Positive control: with motion allowed the card and its more region move.
      await page.emulateMedia({ reducedMotion: 'no-preference' });
      await openFeed(page);
      const moving = await durations();
      expect(moving.some((d) => d.split(',').some((x) => parseFloat(x) > 0)), `motion allowed: ${moving}`).toBe(true);
      await page.emulateMedia({ reducedMotion: 'reduce' });
      await page.reload();
      const still = await durations();
      for (const d of still) for (const x of d.split(',')) expect(parseFloat(x), `reduced: ${still}`).toBe(0);
    });

    test('every old ?tab= value and a draft deep link still land', async ({ page }) => {
      await signIn(page, origin);
      for (const [q, tab] of [
        ['', 'open'],
        ['?tab=ask', 'open'],
        ['?tab=open', 'open'],
        ['?tab=answered', 'answered'],
        ['?tab=drafts', 'drafts'],
        ['?tab=archived', 'drafts'],
        ['?tab=nudges', 'nudges'],
        ['?tab=log', 'log'],
        ['?tab=nonsense', 'open'],
      ]) {
        await page.goto(`${origin}/attention${q}`);
        await expect(selectedTab(page), q || '(none)').toHaveAttribute('data-tab', tab);
      }
      // archived: the Drafts tab with its archived list shown (no second tab
      // strip); the toggle shuts it and the address follows, and back.
      await page.goto(`${origin}/attention?tab=archived`);
      await expect(page.locator('#asks-drafts-tabs')).toHaveCount(0);
      const toggle = page.locator('[data-drafts-show-archived]');
      await expect(toggle).toHaveAttribute('aria-expanded', 'true');
      await expect(page.locator('#asks-drafts-archived-section')).toBeVisible();
      await toggle.click();
      await expect(page.locator('#asks-drafts-archived-section')).toBeHidden();
      await expect(toggle).toHaveText('Show archived');
      await expect(page).toHaveURL(`${origin}/attention?tab=drafts`);
      await toggle.click();
      await expect(page).toHaveURL(`${origin}/attention?tab=archived`);
      await page.goto(`${origin}/attention?tab=drafts`);
      await expect(toggle).toHaveAttribute('aria-expanded', 'false');
      await expect(page.locator('#asks-drafts-archived-section')).toBeHidden();
      // The push deep link of a pending draft: the Drafts tab, on its card.
      await page.goto(`${origin}/attention?tab=drafts#draft-${ids.draft}`);
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'drafts');
      await expect(page.locator(`#draft-${ids.draft}`)).toBeVisible();
    });

    // THE COUNT IS THE OPEN FEED'S, WHATEVER TAB SHOWS (round 1, Y1): a live
    // refresh while Drafts is shown keeps "N open", and a feed of stalled rows
    // only is not "All clear".
    test('a refresh while another tab shows keeps the open count; stalled rows alone are not All clear', async ({ page }) => {
      await openFeed(page);
      const n0 = await openN(page);
      await page.getByRole('tab', { name: /^Drafts/ }).click();
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'drafts');
      const refreshed = page.waitForResponse((r) => {
        const u = new URL(r.url());
        return u.pathname === '/attention/items' && u.search === '' && r.status() === 200;
      });
      const r = await page.request.post(`${origin}/attention/asks/${ids.notice}/cancel`, { headers: { Origin: origin }, form: { x: '' } });
      expect(r.status()).toBeLessThan(400);
      await refreshed;
      await expect(page.locator(`#ask-${ids.notice}`), 'the refresh was applied').toHaveCount(0);
      await expect(openCount(page)).toHaveText(`${n0 - 1} open`);
      await expect(page.locator('#attention-items [data-asks-empty]')).toHaveCount(0);
      // A feed of ONE stalled row (session state's), redrawn while hidden.
      await page.evaluate((ev) => {
        const list = document.getElementById('attention-items')!;
        list.innerHTML = '<li class="min-w-0" data-asks-episodes><ul><li id="episode-990" data-attention-episode><a href="#">stalled</a></li></ul></li>';
        list.dispatchEvent(new Event(ev, { bubbles: true }));
      }, 'muster:dictate-reveal');
      await expect(page.locator('#attention-items [data-asks-empty]'), 'All clear beside a stalled row').toHaveCount(0);
      await expect(openCount(page)).toHaveText('1 open');
      // Positive control: truly empty IS All clear.
      await page.evaluate((ev) => {
        const list = document.getElementById('attention-items')!;
        list.innerHTML = '';
        list.dispatchEvent(new Event(ev, { bubbles: true }));
      }, 'muster:dictate-reveal');
      await expect(page.locator('#attention-items [data-asks-empty]')).toHaveCount(1);
      await expect(openCount(page)).toHaveText('0 open');
    });

    // A REFUSED CARD COMES BACK SAYING WHY (round 1, Y4, N1). Text typed in a
    // card holds the live refresh back (an answer being typed is never
    // replaced), so the card is still on screen when its ask is dismissed
    // elsewhere: Answer is refused by the server, and the card returns with
    // the server's fragment. A 403 (JSON) and a network failure each show a
    // plain sentence, never the raw body or an empty slot.
    test('a refused card comes back with the reason in its error slot', async ({ page }) => {
      await openFeed(page);
      const card = page.locator(`#ask-${ids.freetext}`);
      const box = page.locator(`#answer-${ids.freetext}`);
      const slot = card.locator('[data-asks-card-error]');
      await box.fill('main, please');
      const r = await page.request.post(`${origin}/attention/asks/${ids.freetext}/cancel`, { headers: { Origin: origin }, form: { x: '' } });
      expect(r.status()).toBeLessThan(400);
      const refused = page.waitForResponse((x) => new URL(x.url()).pathname === `/attention/asks/${ids.freetext}/answer`);
      await card.getByRole('button', { name: 'Answer', exact: true }).click();
      expect((await refused).status(), 'the server refuses the answer').toBe(409);
      await expect(card).toBeVisible();
      await expect(slot.locator('[data-asks-card-refusal]')).toHaveText('That question is no longer open (it is cancelled).');
      await expect(slot.locator('[role="alert"]')).toHaveCount(1);

      // Not the server's fragment: a 403 with JSON, then a dropped connection.
      // (A 401 is the shell's auth gate's: it navigates to the sign-in.)
      const rotate = page.locator(`#ask-${ids.rotate}`), rslot = rotate.locator('[data-asks-card-error]');
      await page.route(`**/attention/asks/${ids.rotate}/answer`, (route) =>
        route.fulfill({ status: 403, contentType: 'application/json', body: '{"error":"forbidden"}' }),
      );
      await rotate.getByRole('button', { name: 'Rotate it', exact: true }).click();
      await expect(rslot).toHaveText('Not sent: muster refused it. Reload the page and try again.');
      await expect(rslot).not.toContainText('{');
      await page.unroute(`**/attention/asks/${ids.rotate}/answer`);
      await page.route(`**/attention/asks/${ids.rotate}/answer`, (route) => route.abort('connectionfailed'));
      await rotate.getByRole('button', { name: 'Leave it', exact: true }).click();
      await expect(rslot).toHaveText('Not sent: muster could not be reached. Try again.');
      await expect(rotate).toBeVisible();
    });

    // A POST THAT FAILS AFTER A REFRESH TOOK THE CARD AWAY (round 1, N2): the
    // ask is dismissed elsewhere while the answer is in flight; the refresh no
    // longer lists the card, and the refusal is said in the feed's notice.
    test('a card refused after a refresh took it away is reported, not silent', async ({ page }) => {
      await openFeed(page);
      const rotate = page.locator(`#ask-${ids.rotate}`);
      let release!: () => void;
      const gate = new Promise<void>((r) => (release = r));
      await page.route(`**/attention/asks/${ids.rotate}/answer`, async (route) => {
        await gate;
        await route.continue();
      });
      await rotate.getByRole('button', { name: 'Rotate it', exact: true }).click();
      await expect(rotate).toBeHidden();
      const refreshed = page.waitForResponse((r) => {
        const u = new URL(r.url());
        return u.pathname === '/attention/items' && u.search === '' && r.status() === 200;
      });
      const r = await page.request.post(`${origin}/attention/asks/${ids.rotate}/cancel`, { headers: { Origin: origin }, form: { x: '' } });
      expect(r.status()).toBeLessThan(400);
      expect(await (await refreshed).text(), 'the refresh no longer lists it').not.toContain(`id="ask-${ids.rotate}"`);
      release();
      const notice = page.locator('[data-asks-feed-notice]');
      await expect(notice).toBeVisible();
      await expect(notice).toHaveText(/^Your answer was not recorded \(“Rotate the staging key\?”\): That question is no longer open \(it is cancelled\)\.$/);
      await expect(rotate, 'no card comes back for a closed ask').toHaveCount(0);
    });

    // THE KEYS STAND DOWN UNDER AN OVERLAY (round 1, N3).
    test('the feed keys stand down while a modal overlay is open', async ({ page }) => {
      await openFeed(page);
      const first = page.locator(`#ask-${ids.deploy}`);
      await page.locator('h1').click();
      await page.evaluate(() => {
        const d = document.createElement('div');
        d.id = 'e2e-modal';
        d.setAttribute('role', 'dialog');
        d.setAttribute('aria-modal', 'true');
        d.textContent = 'an overlay';
        document.body.appendChild(d);
      });
      await page.keyboard.press('j');
      await expect(first, 'j moved focus under an aria-modal overlay').not.toBeFocused();
      await page.evaluate(() => document.getElementById('e2e-modal')!.remove());
      // Positive control: the same key, the overlay gone.
      await page.keyboard.press('j');
      await expect(first).toBeFocused();
    });

    // THE ANSWERED FEED'S REFRESH KEEPS WHAT YOU HAD (round 1, N4).
    test('the Answered refresh keeps an expanded card and its focus', async ({ page }) => {
      await openFeed(page);
      await page.getByRole('tab', { name: 'Answered', exact: true }).click();
      const card = page.locator(`#answered-${ids.answered}`);
      await card.locator('[data-asks-toggle]').click();
      await expect(card).toHaveAttribute('data-expanded', 'true');
      await page.locator('h1').click();
      await page.keyboard.press('j');
      await expect(card).toBeFocused();
      await card.evaluate((el) => ((el as unknown as { __old: boolean }).__old = true));
      const refreshed = page.waitForResponse((r) => {
        const u = new URL(r.url());
        return u.pathname === '/attention/items' && u.search === '?part=answered' && r.status() === 200;
      });
      const r = await page.request.post(`${origin}/attention/asks/${ids.notice}/cancel`, { headers: { Origin: origin }, form: { x: '' } });
      expect(r.status()).toBeLessThan(400);
      await refreshed;
      await expect.poll(() => card.evaluate((el) => !!(el as unknown as { __old?: boolean }).__old), { message: 'the card was redrawn' }).toBe(false);
      await expect(card).toHaveAttribute('data-expanded', 'true');
      await expect(card).toBeFocused();
    });

    // PINCH-ZOOMED IN, A SIDEWAYS DRAG PANS (round 1, Y3): the page scale is
    // emulated (CDP Emulation.setPageScaleFactor, which visualViewport.scale
    // reports); a swipe then changes no tab, and the swipe area leaves
    // touch-action to the browser so it can pan sideways.
    test('a pinch-zoomed page takes a sideways drag as a pan, not a tab swipe', async ({ page }) => {
      await openFeed(page);
      expect(await page.locator('[data-asks-swipe]').evaluate((el) => getComputedStyle(el).touchAction)).toBe('auto');
      const cdp = await page.context().newCDPSession(page);
      // drag across the swipe area as it is on screen: its layout rect mapped
      // through the visual viewport (offset, scale) to the window coordinates
      // a touch is given in — so a zoomed drag does start on it.
      const drag = async (dir: 'left' | 'right') => {
        const pts = await page.locator('[data-asks-swipe]').evaluate((el, d) => {
          const r = el.getBoundingClientRect(), v = window.visualViewport!;
          const at = (x: number, y: number) => ({ x: (x - v.offsetLeft) * v.scale, y: (y - v.offsetTop) * v.scale });
          const y = r.top + 150, a = r.left + r.width * 0.2, b = r.left + r.width * 0.8;
          const [x0, x1] = d === 'left' ? [b, a] : [a, b];
          return { from: at(x0, y), to: at(x1, y + 3), hit: document.elementFromPoint(x0, y)?.closest('[data-asks-swipe]') != null };
        }, dir);
        expect(pts.hit, 'the drag starts on the swipe area').toBe(true);
        await touchDrag(page, pts.from, pts.to);
        await page.waitForTimeout(300);
      };
      const zoom = async (f: number) => {
        await cdp.send('Emulation.setPageScaleFactor', { pageScaleFactor: f });
        await expect.poll(() => page.evaluate(() => Math.round(window.visualViewport!.scale * 100) / 100)).toBe(f);
      };
      // At scale 1, to the Answered tab (the positive control: a drag here
      // IS a swipe).
      await drag('left');
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'answered');
      // Zoomed in, the visual viewport at its left edge: a drag RIGHTWARDS has
      // nothing to pan towards, so the browser does not take the gesture —
      // only the feed's zoom guard keeps it from swiping back to Open.
      await zoom(1.3);
      expect(await page.evaluate(() => window.visualViewport!.offsetLeft), 'the zoomed view is at its left edge').toBe(0);
      await drag('right');
      await expect(selectedTab(page), 'a zoomed sideways drag switched the tab').toHaveAttribute('data-tab', 'answered');
      // A leftward one pans the zoomed page instead of switching.
      await drag('left');
      await expect(selectedTab(page), 'a zoomed sideways drag switched the tab').toHaveAttribute('data-tab', 'answered');
      // Back at scale 1, the same rightward drag swipes.
      await zoom(1);
      await page.evaluate(() => window.scrollTo({ top: 0, behavior: 'instant' }));
      await drag('right');
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'open');
      await cdp.detach();
    });
  });
}
