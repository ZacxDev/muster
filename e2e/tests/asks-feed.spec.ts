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
      const question = card.locator('[data-attention-text]');
      await expect(card).toHaveAttribute('data-expanded', 'false');
      await expect(toggle).toHaveAttribute('aria-expanded', 'false');
      // A tap on the question (not a control) opens it.
      await question.tap();
      await expect(card).toHaveAttribute('data-expanded', 'true');
      await expect(toggle).toHaveAttribute('aria-expanded', 'true');
      // View in session: the asking agent's page, the card's last row.
      const view = card.locator('[data-asks-view-session]');
      await expect(view).toHaveText('View in session');
      await expect(view).toHaveAttribute('href', `/agents/${agent}`);
      const cardBox = (await card.boundingBox())!, viewBox = (await view.boundingBox())!, qBox = (await question.boundingBox())!;
      expect(viewBox.y, 'View in session sits below the question').toBeGreaterThan(qBox.y + qBox.height);
      expect(cardBox.y + cardBox.height - (viewBox.y + viewBox.height), 'and at the card’s bottom').toBeLessThan(24);
      // The task link sits beside it.
      await expect(card.locator('[data-asks-task]')).toHaveAttribute('href', `/tasks/${ids.task}`);
      // A tap on the head collapses it again.
      await toggle.tap();
      await expect(card).toHaveAttribute('data-expanded', 'false');
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

    // 🔴 THE COLLAPSE PLAYS BEFORE THE CARD IS REMOVED (a frame probe). The
    // answer's POST lands in tens of milliseconds, inside the 240ms collapse;
    // settle() used to remove the card right then, so it vanished at full
    // height (docs/attention.md promised "it collapses out"). Sampled per
    // animation frame from the click: partway through the transition the card
    // is still in the DOM and mid-collapse, and the last frame it is still
    // connected it has collapsed to nothing. RED at origin/main d1ff2dbe: gone
    // by ~65ms, at its full height.
    test('an answered card plays its collapse before it is removed (frame probe)', async ({ page }) => {
      await page.emulateMedia({ reducedMotion: 'no-preference' });
      await openFeed(page);
      const id = `ask-${ids.rotate}`;
      const rotate = page.locator('#' + id);
      await rotate.scrollIntoViewIfNeeded();
      const h0 = await rotate.evaluate((el) => el.getBoundingClientRect().height);
      expect(h0, 'the card has a height to collapse').toBeGreaterThan(60);
      await page.evaluate((id) => {
        const el = document.getElementById(id)!;
        const out: { t: number; connected: boolean; h: number; shown: boolean }[] = [];
        (window as unknown as { __probe: typeof out }).__probe = out;
        el.addEventListener('click', () => {
          const t0 = performance.now();
          (function f() {
            const t = performance.now() - t0;
            const connected = el.isConnected;
            out.push({ t, connected, h: connected ? el.getBoundingClientRect().height : 0, shown: connected && getComputedStyle(el).display !== 'none' });
            if (t < 700) requestAnimationFrame(f);
          })();
        }, { capture: true, once: true });
      }, id);
      const posted = page.waitForResponse((r) => new URL(r.url()).pathname === `/attention/asks/${ids.rotate}/answer` && r.request().method() === 'POST');
      await rotate.getByRole('button', { name: 'Rotate it', exact: true }).click();
      expect((await posted).status(), 'the answer is taken (204)').toBe(204);
      await expect(rotate).toHaveCount(0);
      const frames = await page.waitForFunction(() => {
        const p = (window as unknown as { __probe: { t: number }[] }).__probe;
        return p.length && p[p.length - 1].t >= 700 ? p : null;
      }).then((h) => h.jsonValue() as Promise<{ t: number; connected: boolean; h: number; shown: boolean }[]>);
      const mid = frames.filter((f) => f.t >= 100 && f.t <= 180);
      expect(mid.length, 'frames were sampled partway through the collapse').toBeGreaterThan(0);
      for (const f of mid) {
        expect(f.connected && f.shown, `at ${Math.round(f.t)}ms the card is still on the page: ${JSON.stringify(frames)}`).toBe(true);
        expect(f.h, `at ${Math.round(f.t)}ms it is mid-collapse`).toBeGreaterThan(1);
        expect(f.h, `at ${Math.round(f.t)}ms it is mid-collapse`).toBeLessThan(h0 - 1);
      }
      const lastShown = [...frames].reverse().find((f) => f.shown)!;
      expect(lastShown.h, `the last frame it was shown, it had collapsed (${Math.round(lastShown.t)}ms)`).toBeLessThan(h0 * 0.25);
      expect(frames.some((f) => !f.connected), 'and then it was removed').toBe(true);
    });

    // An INVARIANT GUARD (green at origin/main too): the wait for the
    // collapse must not hold a reduced-motion card on the page.
    test('under reduced motion an answered card leaves at once', async ({ page }) => {
      await page.emulateMedia({ reducedMotion: 'reduce' });
      await openFeed(page);
      const rotate = page.locator(`#ask-${ids.rotate}`);
      await rotate.getByRole('button', { name: 'Rotate it', exact: true }).click();
      // Hidden as the post starts (no transition), then removed when the POST
      // lands: nothing waits for a transitionend that never comes.
      await expect(rotate, 'hidden at once').toBeHidden({ timeout: 300 });
      await expect(rotate).toHaveCount(0, { timeout: 1500 });
    });

    // AN ACCEPTED ANSWER SAYS WHERE IT WENT: window.toast's info tone, "Sent
    // to <the card's session name>" (role=status, not the error toast). A
    // dismissal says nothing. RED at origin/main d1ff2dbe: no confirmation.
    test('an accepted answer shows "Sent to <session>"; a dismissal does not', async ({ page }) => {
      await openFeed(page);
      const rotate = page.locator(`#ask-${ids.rotate}`);
      const name = ((await rotate.locator('[data-attention-title]').textContent()) ?? '').trim();
      expect(name, 'the card is titled').toBeTruthy();
      const info = page.locator('#action-toasts [data-toast-tone="info"]');
      await expect(info).toHaveCount(0);
      await rotate.getByRole('button', { name: 'Rotate it', exact: true }).click();
      await expect(info).toHaveCount(1);
      await expect(info).toHaveText(`Sent to ${name}`);
      await expect(info).toHaveAttribute('role', 'status');
      await expect(page.locator('#action-toasts [role="alert"]'), 'not drawn as an error').toHaveCount(0);
      // A dismissal of a question (a card that HAS a session name to say):
      // no new toast (the info one is removed first; it fades on its own clock).
      await info.evaluate((el) => el.remove());
      const freetext = page.locator(`#ask-${ids.freetext}`);
      expect(((await freetext.getAttribute('data-asks-who')) ?? '').trim(), 'the dismissed card has a name a toast could say').toBeTruthy();
      const sent = page.waitForResponse((r) => new URL(r.url()).pathname === `/attention/asks/${ids.freetext}/cancel`);
      await freetext.getByRole('button', { name: 'Dismiss without answering' }).click();
      expect((await sent).status()).toBe(204);
      await page.waitForTimeout(500);
      // Read once, NOT retried: a toast fades by itself after 5s, so a retried
      // toHaveCount(0) passes on a toast that WAS shown (it did, mutation-tested).
      expect(await page.locator('#action-toasts [data-toast-tone="info"]').count(), 'a dismissal shows no "Sent to" toast').toBe(0);
    });

    // "SENT TO" NAMES THE SESSION IT WENT TO (audit of #787, 🟡3): the
    // error toasts coalesce by a digit-blind 40-character key, so two answers
    // to sessions whose names differ only in a digit read "Sent to … fern:1
    // (×2)". An info toast is never merged with a different message. RED
    // before: one toast, the first name, ×2.
    test('two "Sent to" toasts for names that differ in a digit are both shown, each exact', async ({ page }) => {
      await openFeed(page);
      await page.evaluate(() => {
        const t = (window as unknown as { toast: (m: string, o: object) => void }).toast;
        t('Sent to Rate-limit the public API fern:1', { tone: 'info' });
        t('Sent to Rate-limit the public API fern:2', { tone: 'info' });
      });
      const info = page.locator('#action-toasts [data-toast-tone="info"]');
      await expect(info).toHaveText(['Sent to Rate-limit the public API fern:1', 'Sent to Rate-limit the public API fern:2']);
    });

    // THE COUNT'S EMPTY STATE IS THE FEED'S OWN (🟢): count() looked for the
    // first [data-asks-empty] anywhere in the feed, so a nested empty line (the
    // stalled rows' "could not be read") was taken for All clear and deleted
    // when a card left. RED before.
    test('answering a card keeps a nested "could not be read" line', async ({ page }) => {
      await openFeed(page);
      await page.evaluate(() => {
        const list = document.getElementById('attention-items')!;
        const li = document.createElement('li');
        li.setAttribute('data-asks-episodes', '');
        li.innerHTML = '<ul><li data-asks-empty="episodes-error" data-attention-episodes-error>The stalled and waiting sessions could not be read.</li></ul>';
        list.appendChild(li);
      });
      // The answer's POST is held, so no live refresh redraws the feed: what
      // is on the page is what count() left (it runs as the card leaves).
      let release!: () => void;
      const gate = new Promise<void>((r) => (release = r));
      await page.route(`**/attention/asks/${ids.rotate}/answer`, async (route) => { await gate; await route.continue(); });
      await page.locator(`#ask-${ids.rotate}`).getByRole('button', { name: 'Rotate it', exact: true }).click();
      await expect(page.locator(`#ask-${ids.rotate}`)).toBeHidden();
      await page.waitForTimeout(500);
      expect(await page.locator('[data-attention-episodes-error]').count(), 'the nested line was taken for the empty state and removed').toBe(1);
      release();
    });

    // ONE OPEN COUNT, AS THE PAGE KEEPS IT (asks_count.go; the Go test
    // TestEverySurfaceShowsTheSameOpenCount pins the server's): the header's
    // "N open", the sidebar's More badge and the ☰ badge agree on load and
    // again after a card leaves. RED at origin/main d1ff2dbe: the header left
    // the fixture's reply draft out and the ☰ counted sidebar rows.
    test('the header, the More badge and the menu badge show one count', async ({ page }) => {
      await openFeed(page);
      const more = page.locator('#attention-badge');
      const menu = page.locator('#nav-waiting-badge');
      // The ☰ is the phone's (beside the desktop rail it is hidden and never
      // loads its count): there the third surface is the More badge again.
      const phone = !!vp.use.isMobile;
      const three = async () => [await openN(page), Number(await more.getAttribute('data-count')), Number(await (phone ? menu : more).getAttribute('data-count'))];
      await expect.poll(async () => { const [a, b, c] = await three(); return a === b && b === c ? a : `${a}/${b}/${c}`; }, { timeout: 10_000 }).toBeGreaterThanOrEqual(5);
      const n0 = await openN(page);
      await page.locator(`#ask-${ids.rotate}`).getByRole('button', { name: 'Rotate it', exact: true }).click();
      await expect(openCount(page)).toHaveText(`${n0 - 1} open`);
      await expect.poll(three, { timeout: 15_000 }).toEqual([n0 - 1, n0 - 1, n0 - 1]);
    });

    // DENSITY (the design review, 8): with the fixture's fleet of asks open,
    // a phone shows at least 2.5 cards above the fold (each card's visible
    // fraction of its own height, summed, at the page's top) — no footer of a
    // lone ✕, no empty slot, no "Question" badge. RED at origin/main (about 2
    // cards: a ✕-only footer band, two empty slots and a badge on every card).
    if (vp.use.isMobile) {
      test('a phone shows at least 2.5 cards above the fold', async ({ page }) => {
        // Suggested replies OFF, their default (suggest.DefaultDisabled) and the
        // review's fleet: another spec on this leg's database may have turned
        // them on for this user, which adds the chip row to the first card.
        await signIn(page, origin);
        const off = await page.request.post(origin + '/account/suggest', { form: { on: '0' }, headers: { Origin: origin }, maxRedirects: 0 });
        expect(off.status(), await off.text()).toBe(204);
        await openFeed(page);
        await expect(page.locator('#attention-items [data-asks-suggestions]'), 'no chip row').toHaveCount(0);
        await page.evaluate(() => window.scrollTo({ top: 0, behavior: 'instant' }));
        await page.waitForTimeout(300);
        const m = await page.evaluate(() => {
          const vh = window.innerHeight;
          let sum = 0;
          const cards = [...document.querySelectorAll('#attention-items > [data-asks-card]')];
          for (const c of cards) {
            const r = c.getBoundingClientRect();
            sum += r.height ? Math.max(0, Math.min(r.bottom, vh) - Math.max(r.top, 0)) / r.height : 0;
          }
          return {
            sum: Math.round(sum * 100) / 100,
            cards: cards.length,
            tops: cards.map((c) => { const r = c.getBoundingClientRect(); return `${c.id}@${Math.round(r.top)}+${Math.round(r.height)}`; }).join(' '),
            // A footer whose one control is the icon-only ✕ (no words of its own).
            loneX: cards.filter((c) => { const f = c.querySelector('[data-asks-footer]'); return f && !f.querySelector('a') && [...f.querySelectorAll('button')].every((b) => !b.textContent!.trim()); }).length,
            emptySlots: cards.filter((c) => { const e = c.querySelector('[data-asks-card-error]'); return e && !e.textContent!.trim(); }).length,
            questionBadges: document.querySelectorAll('#attention-items [data-asks-badge="question"]').length,
          };
        });
        console.log(`asks density at 390×844: ${JSON.stringify(m)}`);
        expect(m.cards, 'the fixture\u2019s cards are on the feed').toBeGreaterThanOrEqual(5);
        expect(m.loneX, 'a footer holding only ✕').toBe(0);
        expect(m.emptySlots, 'an empty error slot').toBe(0);
        expect(m.questionBadges, 'a "Question" badge').toBe(0);
        expect(m.sum, `cards above the fold at 390×844: ${JSON.stringify(m)}`).toBeGreaterThanOrEqual(2.5);
      });
    }

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
          // A card with a more region (an Answered card always has one; an
          // Open card only when there is something to show expanded).
          const more = document.querySelector('[data-asks-card] .asks-more')!, card = more.closest('[data-asks-card]')!;
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
        // No ask log on this server: no Log tab, so its address opens Open.
        ['?tab=log', 'open'],
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
