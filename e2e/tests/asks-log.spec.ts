import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { asksLogPort, baseURL, dsn, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// THE ASKS PAGE'S LOG TAB (internal/api/ask_log_read.go; docs/asks.md,
// "Reading the log"), in a real browser against a real muster-server with
// MUSTER_ASK_LOG=1, at a phone (390×844, touch) and a desktop (1440×900):
//
//   - the owner opens /attention and TAPS the Log tab — the panel is fetched
//     then (the page's own render reads nothing from the log) — and sees the
//     summary (7 asks, 5 answered by you, median 30s, 90th percentile 2m),
//     the breakdowns as plain "label N" rows under the page's one heading
//     style (no pill: nothing there is a control), the log's unended ask as
//     "No answer" (never "Open", the header's word for what waits on you
//     now), the question asked three times AS IT WAS ASKED with its usual
//     answer, and the list, where the row whose text the retention sweep
//     cleared says so;
//   - the Kind filter, applied, reloads the page on the Log tab with the
//     filter in the address and only that kind listed;
//   - nothing scrolls sideways.
//
// And on the suite's shared server (ask log OFF), the tab says how to turn it
// on.
//
// The asks are fixture rows written through the real store (e2e/askfix):
// what is under test is how the tab reads and draws them, not their capture
// (internal/api's ask_log_pg_test.go drives that).
//
// 🔴 REGRESSION: on origin/main the Asks page has no Log tab and
// /attention/log is a 404.

const origin = `http://localhost:${asksLogPort}`;
let server: Running | undefined;
let expiredID = '';

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/askfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, asksLogPort, { MUSTER_ASK_LOG: '1', MUSTER_FOCUS_INTERVAL: '0' });
  fix('clear', '1');
  expiredID = fix('seed', '1');
  expect(expiredID, 'askfix printed the expired row id').toMatch(/^\d+$/);
});

test.afterAll(async () => {
  try {
    fix('clear', '1');
  } finally {
    await server?.stop();
  }
});

async function noSidewaysScroll(page: Page) {
  const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
  expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(iw);
}

for (const vp of [
  { name: 'phone 390×844', key: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', key: 'desktop', use: { viewport: { width: 1440, height: 900 } } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });
    test('the Log tab shows the summary, the repeated question and the list, and filters by kind', async ({ page }, testInfo) => {
      await signIn(page, origin);
      await page.goto(`${origin}/attention`);
      const tab = page.getByRole('tab', { name: 'Log' });
      await expect(tab).toBeVisible();
      // The panel is fetched when the tab is opened, not before.
      const fetched = page.waitForResponse((r) => new URL(r.url()).pathname === '/attention/log' && r.status() === 200);
      await tab.click();
      await fetched;
      await expect(page).toHaveURL(`${origin}/attention?tab=log`);
      const panel = page.getByTestId('asklog-panel');
      await expect(panel).toBeVisible();

      const stat = (k: string) => panel.locator(`[data-asklog-stat="${k}"] span`).first();
      await expect(stat('total')).toHaveText('7');
      await expect(stat('owner')).toHaveText('5');
      await expect(stat('median')).toHaveText('30s');
      await expect(stat('p90')).toHaveText('2m');

      // THE BREAKDOWNS: plain statistics under the Asks page's one heading
      // style, never pills. askfix's 7: 5 answered, 1 declined and 1 never
      // ended — "No answer" (a tie keeps the outcomes' own order), never the
      // header's "N open".
      const counts = (k: string) =>
        panel.locator(`[data-asklog-counts="${k}"] [data-count]`).evaluateAll((els) =>
          els.map((e) => `${e.querySelector('dt')!.textContent!.trim()} ${e.querySelector('dd')!.textContent!.trim()}`));
      expect(await counts('kind')).toEqual(['Question in chat 4', 'Question with choices 1', "Agent's question 1", 'opencode permission request 1']);
      expect(await counts('outcome')).toEqual(['Answered 5', 'No answer 1', 'Declined 1']);
      expect(await counts('answered_by')).toEqual(['You 5', 'The chief 1', 'Nobody 1']);
      await expect(page.locator('[data-asks-open-count]')).toHaveText(/^\d+ open$/);
      await expect(panel.getByText(/\bopen\b/i), 'the log never says "open"').toHaveCount(0);
      await expect(panel.locator('[data-asklog-summary] [data-count].rounded-full, [data-asklog-summary] span.rounded-full'), 'no count is drawn as a pill').toHaveCount(0);
      const heads = panel.locator('h2[data-asks-group-label]');
      await expect(heads).toHaveText(['Kind of question', 'How it ended', 'Who answered', 'Asked most often', 'Every ask, newest first']);
      const headClass = await heads.first().getAttribute('class');
      for (const c of await heads.evaluateAll((els) => els.map((e) => e.getAttribute('class')))) expect(c, 'one heading class').toBe(headClass);
      // The ⓘ explains every outcome the log holds.
      await panel.locator('#asklog-outcome-info-open').click();
      const sheet = page.locator('#asklog-outcome-info');
      await expect(sheet).toBeVisible();
      await expect(sheet.locator('[data-meaning]')).toHaveCount(3);
      await expect(sheet.locator('[data-meaning="open"]')).toContainText('Not the same as waiting on you now.');
      await sheet.getByRole('button', { name: 'Close' }).click();
      await expect(sheet).toBeHidden();

      // Grouped by its normal form, shown as the owner saw it (the newest
      // ask's own text, its last paragraph: what was asked), on one short
      // facts line.
      const top = panel.locator('[data-asklog-top-row]');
      await expect(top).toHaveCount(1);
      await expect(top).toHaveAttribute('data-norm', 'shall i continue with the next step');
      await expect(top.locator('[data-asklog-top-question]')).toHaveText('Shall I continue with the next step?');
      await expect(top.locator('[data-asklog-top-facts]')).toHaveText('3 times · you answered 2 · usually “yes” · median 20s');

      const rows = panel.locator('[data-asklog-row]');
      await expect(rows).toHaveCount(7);
      const expired = panel.locator(`#asklog-${expiredID}`);
      await expect(expired.locator('[data-asklog-expired]')).toContainText('Text expired');
      await expect(expired.locator('[data-asklog-question]')).toHaveCount(0);
      await expect(expired.locator('[data-asklog-answer]')).toHaveText('by you · after 5s');
      await noSidewaysScroll(page);

      const shot = testInfo.outputPath(`asks-log-${vp.key}.png`);
      await page.screenshot({ path: shot, fullPage: true });
      await testInfo.attach(`asks-log-${vp.key}.png`, { path: shot, contentType: 'image/png' });
      // The viewport, at the top and scrolled to "Asked most often" (under the
      // sticky header, which is opaque: nothing scrolled shows through it).
      await page.evaluate(() => {
        document.documentElement.style.scrollBehavior = 'auto';
        window.scrollTo(0, 0);
      });
      const atTop = testInfo.outputPath(`asks-log-${vp.key}-top.png`);
      await page.screenshot({ path: atTop });
      await testInfo.attach(`asks-log-${vp.key}-top.png`, { path: atTop, contentType: 'image/png' });
      await panel.locator('#asklog-top').evaluate((e) => {
        const below = document.querySelector('header')!.getBoundingClientRect().bottom;
        window.scrollTo({ top: window.scrollY + e.getBoundingClientRect().top - below - 4, behavior: 'instant' });
      });
      // The heading sits just under the header, not behind it.
      await expect
        .poll(() => panel.locator('#asklog-top').evaluate((e) => e.getBoundingClientRect().top - document.querySelector('header')!.getBoundingClientRect().bottom))
        .toBeGreaterThanOrEqual(0);
      const bg = await page.locator('header').first().evaluate((e) => getComputedStyle(e).backgroundColor);
      expect(bg, 'the sticky header is opaque').not.toMatch(/rgba\([^)]*,\s*0?\.\d+\)/);
      const scrolled = testInfo.outputPath(`asks-log-${vp.key}-scrolled.png`);
      await page.screenshot({ path: scrolled });
      await testInfo.attach(`asks-log-${vp.key}-scrolled.png`, { path: scrolled, contentType: 'image/png' });

      // THE FILTERS ARE BEHIND ONE "Filters" TOGGLE, SHUT (the design review):
      // the tab's first screen is the summary, not eight fields. RED at
      // origin/main, where every field was laid out on top.
      const toggle = panel.locator('[data-asklog-filters-toggle]');
      await expect(toggle).toHaveText('Filters');
      await expect(panel.locator('select[name="kind"]'), 'the filters start shut').toBeHidden();
      await expect(panel.locator('[data-asklog-summary]')).toBeInViewport();
      await toggle.click();
      // Filter: Kind = Question with choices, applied — the page reloads on the Log tab.
      await expect(panel.locator('select[name="kind"] option[value="ask_user_question"]')).toHaveText('Question with choices');
      await panel.locator('select[name="kind"]').selectOption('ask_user_question');
      await Promise.all([page.waitForURL(/[?&]kind=ask_user_question/), panel.getByRole('button', { name: 'Apply' }).click()]);
      await expect(page).toHaveURL(/[?&]tab=log/);
      const filtered = page.getByTestId('asklog-panel');
      // Filtered: the toggle says how many are set, so it never reads as the whole log.
      await expect(filtered.locator('[data-asklog-filters-toggle]')).toHaveText('Filters (1)');
      await expect(filtered.locator('[data-asklog-row]')).toHaveCount(1);
      await expect(filtered.locator('[data-asklog-row]')).toHaveAttribute('data-kind', 'ask_user_question');
      await expect(filtered.locator('[data-asklog-stat="total"] span').first()).toHaveText('1');
      await noSidewaysScroll(page);
    });
  });
}

// LONG LINES (#832's phone review at 390×844): a facts line that wraps never
// leaves a "·" ending a line — each separator travels with the fact after it —
// and a row whose place is wider than its card still shows WHEN it was asked:
// the place gives way (its ellipsis inside it), the age never does. Measured
// on the LAYOUT (each character's box, each clipping ancestor), not on the
// markup. Its own log (askfix wraps), seeded after the tests above.
//
// 🔴 REGRESSION, at origin/main 585e8042: on a phone the facts line's first
// line ends "you answered 2 ·"; on a phone AND a desktop the row's <time> sits
// past the right edge of the truncated span that holds it. (At a desktop's
// width the facts line does not wrap, so there its test is a no-regression
// check, not a reproduction.)
const wrapsVPs = [
  { name: 'phone 390×844', key: 'phone', phone: true, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', key: 'desktop', phone: false, use: { viewport: { width: 1440, height: 900 } } },
];
test.describe('long lines', () => {
  let newestID = '';
  test.beforeAll(() => {
    fix('clear', '1');
    newestID = fix('wraps', '1');
    expect(newestID, 'askfix printed the newest row id').toMatch(/^\d+$/);
  });

  for (const vp of wrapsVPs) {
    test.describe(vp.name, () => {
      test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

      test('no facts line ends with "·"', async ({ page }, testInfo) => {
        await signIn(page, origin);
        await page.goto(`${origin}/attention?tab=log`);
        const panel = page.getByTestId('asklog-panel');
        const facts = panel.locator('[data-asklog-top-row] [data-asklog-top-facts]');
        await expect(facts).toHaveText('3 times · you answered 2 · always “continue with the plan” · median 11s');
        // Every visible character's box, grouped into the lines they sit on:
        // a line's text is its characters in order, and what ENDS it is its
        // last non-space one.
        const lines = await facts.evaluate((el) => {
          const out: { top: number; text: string }[] = [];
          const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
          for (let n = walker.nextNode(); n; n = walker.nextNode()) {
            const s = n.textContent ?? '';
            for (let i = 0; i < s.length; i++) {
              const r = document.createRange();
              r.setStart(n, i);
              r.setEnd(n, i + 1);
              const box = Array.from(r.getClientRects()).find((b) => b.width > 0);
              if (!box) continue; // a collapsed space at a break
              const top = Math.round(box.top);
              let line = out.find((l) => Math.abs(l.top - top) < 4);
              if (!line) out.push((line = { top, text: '' }));
              line.text += s[i];
            }
          }
          return out.sort((a, b) => a.top - b.top).map((l) => l.text.trim());
        });
        // Positive control: on a phone the line really wraps, so "never ends
        // with ·" is measured at a break, not vacuously.
        if (vp.phone) expect(lines.length, `the facts line wraps on a phone: ${JSON.stringify(lines)}`).toBeGreaterThanOrEqual(2);
        for (const l of lines) expect(l.endsWith('·'), `a facts line ends with "·": ${JSON.stringify(lines)}`).toBe(false);
        await noSidewaysScroll(page);

        await panel.locator('#asklog-top').evaluate((e) => {
          document.documentElement.style.scrollBehavior = 'auto';
          const below = document.querySelector('header')!.getBoundingClientRect().bottom;
          window.scrollTo({ top: window.scrollY + e.getBoundingClientRect().top - below - 4, behavior: 'instant' });
        });
        const shot = testInfo.outputPath(`asks-log-wraps-${vp.key}.png`);
        await page.screenshot({ path: shot });
        await testInfo.attach(`asks-log-wraps-${vp.key}.png`, { path: shot, contentType: 'image/png' });
      });

      test("a row's age is never cut by a long place", async ({ page }, testInfo) => {
        await signIn(page, origin);
        await page.goto(`${origin}/attention?tab=log`);
        const panel = page.getByTestId('asklog-panel');
        // The newest row's age: its text box lies inside every ancestor that
        // clips (overflow other than visible) up to the card, and inside the
        // card and the viewport.
        const row = panel.locator(`#asklog-${newestID}`);
        await expect(row.locator('time')).toHaveText('15m ago');
        const age = await row.locator('time').evaluate((t) => {
          const r = document.createRange();
          r.selectNodeContents(t);
          const box = r.getBoundingClientRect();
          const card = t.closest('[data-asklog-row]')!;
          const clipped: string[] = [];
          for (let p = t.parentElement; p && p !== card.parentElement; p = p.parentElement) {
            if (getComputedStyle(p).overflowX === 'visible') continue;
            const pb = p.getBoundingClientRect();
            if (box.left < pb.left - 0.5 || box.right > pb.right + 0.5) clipped.push(`${p.tagName}.${p.className} [${pb.left},${pb.right}] cuts [${box.left},${box.right}]`);
          }
          const cb = card.getBoundingClientRect();
          // Positive control: the place really is too wide (its ellipsis
          // showing), so the age had something to lose.
          const where = card.querySelector('[data-asklog-where]') as HTMLElement | null;
          return { clipped, right: box.right, width: box.width, cardRight: cb.right, vw: window.innerWidth, placeCut: where ? where.scrollWidth > where.clientWidth : null };
        });
        expect(age.clipped, 'the age is cut by a clipping ancestor').toEqual([]);
        expect(age.width, 'the age has a width').toBeGreaterThan(0);
        expect(age.right, "the age's right edge is within the card").toBeLessThanOrEqual(age.cardRight + 0.5);
        expect(age.right, "the age's right edge is within the viewport").toBeLessThanOrEqual(age.vw);
        expect(age.placeCut, 'the long place is truncated').toBe(true);
        await noSidewaysScroll(page);
        await row.scrollIntoViewIfNeeded();
        const shot = testInfo.outputPath(`asks-log-wraps-row-${vp.key}.png`);
        await page.screenshot({ path: shot });
        await testInfo.attach(`asks-log-wraps-row-${vp.key}.png`, { path: shot, contentType: 'image/png' });
      });
    });
  }
});

// With the ask log off there is no Log tab at all (it used to be a tab of
// setup prose: "set MUSTER_ASK_LOG=1 and restart"), and ?tab=log opens Open.
// RED at origin/main d1ff2dbe: the tab was drawn, with that prose. The
// positive control is this file's own log-on server, whose Log tab the tests
// above open.
test('with the ask log off there is no Log tab', async ({ page }) => {
  await signIn(page, baseURL);
  await page.goto(`${baseURL}/attention`);
  await expect(page.getByRole('tab', { name: 'Open', exact: true })).toBeVisible();
  await expect(page.getByRole('tab', { name: 'Log' })).toHaveCount(0);
  await page.goto(`${baseURL}/attention?tab=log`);
  await expect(page.locator('#asks-tabs [role="tab"][aria-selected="true"]')).toHaveAttribute('data-tab', 'open');
  await expect(page.locator('[data-asklog-off]')).toHaveCount(0);
  await expect(page.getByText('MUSTER_ASK_LOG')).toHaveCount(0);
});
