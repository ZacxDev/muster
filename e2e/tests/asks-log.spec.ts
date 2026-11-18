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
//     the question asked three times with its usual answer, and the list,
//     where the row whose text the retention sweep cleared says so;
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

      const top = panel.locator('[data-asklog-top-row]');
      await expect(top).toHaveCount(1);
      await expect(top.locator('[data-asklog-norm]')).toHaveText('shall i continue with the next step');
      await expect(top.locator('[data-asklog-top-facts]')).toHaveText('3 times · you answered 2 · usually “yes” (2) · median 20s');

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
