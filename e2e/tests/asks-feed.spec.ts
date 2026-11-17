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
//   - Dismiss is undoable for a moment, and sent when the moment passes;
//   - prefers-reduced-motion: reduce turns every card transition off.
//
// The queue is fixture rows (e2e/asksfeedfix) written through the real store
// and re-seeded before every test.
//
// 🔴 REGRESSION: on origin/main the page is grouped lists with no swipe, no
// expand, no Answered tab, no keys and no Undo — every test here is red there.

const origin = `http://localhost:${asksFeedPort}`;
const stamp = Date.now().toString(36);
const agent = 'e2e-feed-' + stamp;
const agentToken = 'e2e-feed-agent-token-not-a-real-one-' + stamp;
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
  ids = JSON.parse(fix('seed', '1', agent, agentToken)) as Ids;
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

      // A VERTICAL scroll, drifting sideways as a thumb does: no tab change —
      // and the page did scroll (the gesture was real, not ignored).
      await page.evaluate(() => window.scrollTo(0, 0));
      const before = await page.evaluate(() => window.scrollY);
      const area = (await page.locator('[data-asks-swipe]').boundingBox())!;
      await touchDrag(page, { x: area.x + area.width * 0.5, y: vp.use.viewport.height - 120 }, { x: area.x + area.width * 0.5 + 40, y: 160 }, 16);
      await expect.poll(() => page.evaluate(() => window.scrollY), { message: 'the vertical drag scrolled the page' }).toBeGreaterThan(before);
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'open');
      await expect(page).toHaveURL(`${origin}/attention`);

      // A sideways swipe that starts in the answer box is the box's.
      await page.evaluate(() => window.scrollTo(0, 0));
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
      await expect(openCount(page)).toHaveText('5 open');
      const rotate = page.locator(`#ask-${ids.rotate}`);
      // The card after it in the feed (questions, then notices: the notice).
      const next = page.locator(`#ask-${ids.notice}`);
      await expect(rotate.locator('xpath=following-sibling::li[1]')).toHaveAttribute('id', `ask-${ids.notice}`);
      await rotate.getByRole('button', { name: 'Rotate it', exact: true }).click();
      await expect(rotate).toHaveCount(0);
      await expect(next).toBeFocused();
      await expect(openCount(page)).toHaveText('4 open');
      await expect.poll(async () => (await page.request.get(`${origin}/attention/asks/${ids.rotate}`)).text()).toContain('Your answer: Rotate it');
      // Still gone after a reload: the server has it.
      await page.reload();
      await expect(page.locator(`#ask-${ids.rotate}`)).toHaveCount(0);
    });

    test('every answerable card carries the dictation mic', async ({ page }) => {
      await openFeed(page);
      const answerable = page.locator('#attention-items [data-asks-card][data-kind="ask"]');
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

    test('Dismiss can be undone, and is sent once the moment passes', async ({ page }) => {
      await openFeed(page);
      const card = page.locator(`#ask-${ids.freetext}`);
      const askState = async () => (await page.request.get(`${origin}/attention/asks/${ids.freetext}`)).text();
      await card.getByRole('button', { name: 'Dismiss without answering' }).click();
      await expect(card).toBeHidden();
      const toast = page.locator('[data-asks-toast]');
      await expect(toast).toBeVisible();
      await expect(toast).toContainText('Dismissed.');
      await expect(openCount(page)).toHaveText('4 open');
      await toast.getByRole('button', { name: 'Undo' }).click();
      await expect(card).toBeVisible();
      await expect(openCount(page)).toHaveText('5 open');
      expect(await askState(), 'Undo sent nothing').not.toContain('Dismissed');
      // Again, and let it lapse: now it is sent.
      await card.getByRole('button', { name: 'Dismiss without answering' }).click();
      await expect(card).toBeHidden();
      await expect.poll(askState, { timeout: 12_000 }).toContain('Dismissed');
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
      await page.goto(`${origin}/attention?tab=archived`);
      await expect(page.locator('#asks-drafts-tabs [role="tab"][aria-selected="true"]')).toHaveAttribute('data-tab', 'archived');
      // The push deep link of a pending draft: the Drafts tab, on its card.
      await page.goto(`${origin}/attention?tab=drafts#draft-${ids.draft}`);
      await expect(selectedTab(page)).toHaveAttribute('data-tab', 'drafts');
      await expect(page.locator(`#draft-${ids.draft}`)).toBeVisible();
    });
  });
}
