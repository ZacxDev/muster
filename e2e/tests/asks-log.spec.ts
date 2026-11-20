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
//     summary (7 asks, 5 answered by you, typical 30s, 9 in 10 within 2m 40s),
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
      // "9 in 10 answered within" is the DISCRETE 90th percentile
      // (percentile_disc: the smallest answer time at least 90% of answers
      // took no longer than), so the claim is true of the rows. askfix's 6
      // rows with an answer time (5 answered + 1 declined) took 5, 10, 20,
      // 40, 80 and 160 s; 80 s covers only 5 of 6 (83%), 160 s covers 6 of 6
      // — so 2m 40s. (The interpolated p90 the API still reports is 2m: 80 +
      // 0.5 × 80, a time only 5 of 6 were answered within.)
      await expect(stat('p90')).toHaveText('2m 40s');

      // THE BREAKDOWNS: plain statistics under the Asks page's one heading
      // style, never pills. askfix's 7: 5 answered, 1 declined and 1 never
      // ended — "No answer" (a tie keeps the outcomes' own order), never the
      // header's "N open".
      const counts = (k: string) =>
        panel.locator(`[data-asklog-counts="${k}"] [data-count]`).evaluateAll((els) =>
          els.map((e) => `${e.querySelector('dt')!.textContent!.trim()} ${e.querySelector('dd')!.textContent!.trim()}`));
      expect(await counts('kind')).toEqual(['Question in chat 4', 'Question with choices 1', "Agent's question 1", 'Permission (opencode) 1']);
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
      await expect(top.locator('[data-asklog-top-facts]')).toHaveText('3 times · you answered 2 · usually “yes” · answered in about 20s');

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
        await expect(facts).toHaveText('3 times · you answered 2 · always “continue with the plan” · answered in about 11s');
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
          // Positive control: the place really is too wide (a part of it cut
          // short or wrapped onto its clipped line), so the age had something
          // to lose.
          const where = card.querySelector('[data-asklog-sub]') as HTMLElement | null;
          const over = (e: Element) => e.scrollWidth > e.clientWidth || e.scrollHeight > e.clientHeight;
          return { clipped, right: box.right, width: box.width, cardRight: cb.right, vw: window.innerWidth, placeCut: where ? [where, ...Array.from(where.querySelectorAll('*'))].some(over) : null };
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

      // NO PLACE IS CUT TO A STUB (a phone review of #837): a row's sub line
      // is "session on host · repo · age", and when the place did not fit, the
      // repo was what gave way — to "a…" or "ac…", which reads as debris. Now
      // a part of the place either shows at least STUB_MIN characters before
      // its ellipsis or is not shown at all ("ratelimit on work-laptop · 2h
      // ago"); the full place is the sub line's title.
      //
      // Measured on the LAYOUT: each character's box against every clipping
      // ancestor up to the card, and, inside a box drawn with an ellipsis that
      // overflows, against its right edge less the ellipsis's own width — so
      // "visible" is what a reader sees, not what the markup holds.
      //
      // 🔴 REGRESSION, at origin/main 8ac45385: on a phone "ratelimit @
      // work-laptop · acme-corporation/…" shows its repo as a stub.
      test('no place is cut to a stub', async ({ page }, testInfo) => {
        const STUB_MIN = 6;
        await signIn(page, origin);
        await page.goto(`${origin}/attention?tab=log`);
        const panel = page.getByTestId('asklog-panel');
        await expect(panel.locator('[data-asklog-row]')).toHaveCount(9);
        const rows = await panel.locator('[data-asklog-row]').evaluateAll((cards) => {
          const ell = document.createElement('span');
          return cards.map((card) => {
            const sub = card.querySelector('[data-asklog-sub]') as HTMLElement;
            // The rect every clipping ancestor of el (up to the card) leaves
            // visible; null when nothing of it is left.
            const clipOf = (el: Element) => {
              let l = -Infinity, r = Infinity, t = -Infinity, b = Infinity;
              for (let p: Element | null = el; p && p !== card; p = p.parentElement) {
                const cs = getComputedStyle(p);
                if (cs.overflowX === 'visible' && cs.overflowY === 'visible') continue;
                const pb = p.getBoundingClientRect();
                let pr = pb.right - parseFloat(cs.paddingRight);
                // A box that overflows with an ellipsis draws the ellipsis at
                // its end: a character is seen only left of it.
                if (cs.textOverflow === 'ellipsis' && (p as HTMLElement).scrollWidth > (p as HTMLElement).clientWidth) {
                  ell.textContent = '…';
                  ell.style.font = cs.font;
                  ell.style.whiteSpace = 'pre';
                  document.body.appendChild(ell);
                  pr -= ell.getBoundingClientRect().width;
                  ell.remove();
                }
                l = Math.max(l, pb.left); r = Math.min(r, pr); t = Math.max(t, pb.top); b = Math.min(b, pb.bottom);
              }
              return { l, r, t, b };
            };
            // Each text node's visible characters, and what the reader sees on
            // the line, left to right (an ellipsis after a cut box's last one).
            const cut: { text: string; el: string }[] = [];
            const words: { x: number; right: number; text: string }[] = [];
            const walker = document.createTreeWalker(sub, NodeFilter.SHOW_TEXT);
            for (let n = walker.nextNode(); n; n = walker.nextNode()) {
              const el = n.parentElement!;
              const c = clipOf(el);
              const s = n.textContent ?? '';
              let vis = '';
              let x = Infinity, right = -Infinity;
              for (let i = 0; i < s.length; i++) {
                const rg = document.createRange();
                rg.setStart(n, i);
                rg.setEnd(n, i + 1);
                const box = Array.from(rg.getClientRects()).find((x) => x.width > 0);
                if (!box) continue;
                const cx = (box.left + box.right) / 2, cy = (box.top + box.bottom) / 2;
                if ((box.right <= c.r + 0.5 && box.left >= c.l - 0.5) || (cx > c.l && cx < c.r)) { // partly drawn counts as seen
                  if (cy > c.t && cy < c.b) {
                    vis += s[i];
                    x = Math.min(x, box.left);
                    right = Math.max(right, box.right);
                  }
                }
              }
              // The box this text is cut by: its nearest ancestor drawing an ellipsis that overflows.
              let cutter: HTMLElement | null = null;
              for (let p: HTMLElement | null = el; p && p !== card; p = p.parentElement) {
                const cs = getComputedStyle(p);
                if (cs.textOverflow === 'ellipsis' && p.scrollWidth > p.clientWidth) { cutter = p; break; }
              }
              const isCut = cutter !== null && vis.length > 0 && vis.length < s.length;
              if (vis.length > 0) words.push({ x, right: isCut ? c.r + 12 : right, text: vis + (isCut ? '…' : '') });
              if (isCut) cut.push({ text: vis, el: `${el.tagName}[${Array.from(el.attributes).map((a) => a.name).filter((a) => a.startsWith('data-')).join(',')}]` });
            }
            // Separate elements are separate words on the line (a flex gap, not a character).
            words.sort((a, b) => a.x - b.x);
            const seen = words.map((w) => w.text).join(' ');
            // The widest blank between two words on the line: a dropped part
            // must not leave a hole where it was.
            let hole = 0;
            for (let i = 1; i < words.length; i++) hole = Math.max(hole, words[i].x - words[i - 1].right);
            // The row's title and outcome pill (the audit of #867: a long
            // pill never shrinks, so it cut the title beside it): each
            // drawn whole, its text not overflowing its box.
            const head = card.querySelector('[data-asklog-title]') as HTMLElement;
            const pill = card.querySelector('[data-asklog-outcome]') as HTMLElement;
            const whole = (el: HTMLElement) => el.scrollWidth <= el.clientWidth + 1 && el.getBoundingClientRect().right <= card.getBoundingClientRect().right + 0.5;
            return {
              id: card.id, seen: seen.replace(/\s+/g, ' ').trim(), cut, hole, title: sub.getAttribute('title'),
              head: head.textContent, headWhole: whole(head), pill: pill.textContent, pillWhole: whole(pill),
            };
          });
        });
        const report = JSON.stringify(rows.map((r) => r.seen));
        for (const r of rows) {
          // What a cut part shows before its ellipsis, its separator aside:
          // "· a" is a stub of one character.
          for (const c of r.cut) {
            const shown = c.text.split('·').pop()!.trim();
            expect(shown.length, `a place part is cut to a stub "${shown}…" (${c.el}) in "${r.seen}": ${report}`).toBeGreaterThanOrEqual(STUB_MIN);
          }
          // Nowhere on the line does a run of 1–5 characters end in "…".
          expect(r.seen, `the sub line shows a stub: ${report}`).not.toMatch(/(^|[\s·])[^\s·]{1,5}…/);
          // The age is whole, at the line's end.
          expect(r.seen, `the age is cut: ${report}`).toMatch(/ago$/);
          // …and straight after the place: no hole where a dropped part was
          // (a space and the flex gap are about 8px at this size).
          expect(r.hole, `a hole of ${r.hole}px on "${r.seen}": ${report}`).toBeLessThanOrEqual(12);
          expect(r.headWhole, `the title "${r.head}" is cut beside its "${r.pill}" pill: ${report}`).toBe(true);
          expect(r.pillWhole, `the "${r.pill}" pill is cut: ${report}`).toBe(true);
        }
        const by = (s: string) => rows.find((r) => r.seen.startsWith(s))!;
        // The full place is always available to the reader.
        expect(by('ratelimit on work-laptop')?.title, `the full place is the place's title: ${report}`).toBe('ratelimit on work-laptop · acme-corporation/api-gateway-monorepo');
        expect(by('ci on mac')?.title).toBe('ci on mac · acme-corporation/api-gateway-monorepo');
        if (vp.phone) {
          // Positive control: on a phone a repo really is cut, and the rule
          // above measured it — "ci on mac" leaves room for a readable part.
          const ci = by('ci on mac');
          expect(ci.cut.length, `the ci row's repo is cut on a phone: ${report}`).toBeGreaterThan(0);
          expect(ci.seen, `the ci row shows a readable part of its repo: ${report}`).toMatch(/^ci on mac · acme-c[^…]*… · .*ago$/);
          // …and the row with no room for one drops its repo whole.
          expect(by('ratelimit on work-laptop').seen, `the ratelimit row drops its repo: ${report}`).toBe('ratelimit on work-laptop · 2h ago');
          // Positive control for the pill check: the two rows that did not
          // end in an answer are there, with their short pills.
          expect(rows.map((r) => r.pill).filter((p) => p !== 'Answered').sort(), report).toEqual(['Replaced', 'Skipped']);
        } else {
          // At a desktop a normal-length place fits whole: nothing changes.
          expect(by('docs on studio-mac').seen, report).toMatch(/^docs on studio-mac · acme\/api · \S+ ago$/);
          expect(by('ratelimit on work-laptop').seen, report).toBe('ratelimit on work-laptop · acme-corporation/api-gateway-monorepo · 2h ago');
          for (const r of rows.filter((x) => !x.seen.startsWith('ratelimit-backoff'))) expect(r.cut, `a place that fits is cut at a desktop: ${report}`).toEqual([]);
        }
        await noSidewaysScroll(page);
        await panel.locator('[data-asklog-row]').first().evaluate((e) => {
          document.documentElement.style.scrollBehavior = 'auto';
          const below = document.querySelector('header')!.getBoundingClientRect().bottom;
          window.scrollTo({ top: window.scrollY + e.getBoundingClientRect().top - below - 40, behavior: 'instant' });
        });
        const shot = testInfo.outputPath(`asks-log-stubs-${vp.key}.png`);
        await page.screenshot({ path: shot });
        await testInfo.attach(`asks-log-stubs-${vp.key}.png`, { path: shot, contentType: 'image/png' });
      });
    });
  }
});

// THE LOG TAB'S LOADING AND ERROR STATES (the asks audit): while the panel is
// fetched it says "Loading…" (it used to show a link to the very page it was
// on); a failed fetch says "Couldn't load the log." with a Retry that fetches
// it again, in place.
test.describe('log loading and error, phone 390×844', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });
  test('the Log tab says Loading…, then on a failure offers a Retry that loads it', async ({ page }) => {
    await signIn(page, origin);
    await page.goto(`${origin}/attention`);
    let release = () => {};
    const held = new Promise<void>((r) => (release = r));
    let fail = true;
    await page.route('**/attention/log*', async (route) => {
      if (new URL(route.request().url()).pathname !== '/attention/log') return route.continue();
      await held;
      if (fail) return route.fulfill({ status: 500, contentType: 'text/plain', body: 'boom' });
      return route.continue();
    });
    await page.getByRole('tab', { name: 'Log' }).click();
    const line = page.locator('[data-asklog-loading]');
    await expect(line).toHaveText('Loading…');
    await expect(page.getByRole('link', { name: 'Open the ask log' })).toHaveCount(0);
    release();
    await expect(line).toHaveText("Couldn't load the log. Retry");
    fail = false;
    const fetched = page.waitForResponse((r) => new URL(r.url()).pathname === '/attention/log' && r.status() === 200);
    await line.getByRole('link', { name: 'Retry' }).click();
    await fetched;
    await expect(page.getByTestId('asklog-panel')).toBeVisible();
    await expect(page).toHaveURL(`${origin}/attention?tab=log`);
  });
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
