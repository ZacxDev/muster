import { execFileSync } from 'node:child_process';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { clsInit, readCls, settle } from '../lib/cls';
import { dsn, morningReportPort, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// THE MORNING REPORT'S PAGE (internal/api/morning_report.go, internal/ui
// morning_report.go; docs/features.md#morning-report), in a real browser
// against a real muster-server with MUSTER_MORNING_REPORT=1, on a phone
// (390×844, touch):
//
//   - the open question is FIRST and above the fold, as the Asks queue's own
//     card, and is answered in place: tapping an option records it and lands
//     back on the report, the question gone;
//   - what got done, what is stuck and the usage lines are drawn, each part
//     in plain words; every link and button is at least 44px tall;
//   - nothing scrolls sideways, and the page does not shift as it settles
//     (this is the page's CLS measurement: the shared servers run with the
//     report off, where /morning answers 404 — cls.spec.ts names it here).
//
// The report is a fixture row written through the real store (e2e/morningfix)
// for 2026-03-08, a day the server's own job never writes; the question is a
// real attention ask.
//
// 🔴 REGRESSION: on origin/main /morning/{date} is a 404.

const origin = `http://localhost:${morningReportPort}`;
let server: Running | undefined;
let date = '';

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/morningfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, morningReportPort, { MUSTER_MORNING_REPORT: '1', MUSTER_FOCUS_INTERVAL: '0' });
  date = fix('seed', '1');
  expect(date, 'morningfix printed the report date').toBe('2026-03-08');
});

test.afterAll(async () => {
  try {
    fix('clear', '1');
  } finally {
    await server?.stop();
  }
});

test.describe('phone 390×844', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

  test('the report puts the open question first and answers it in place', async ({ page }) => {
    await signIn(page, origin);
    await page.addInitScript(clsInit);
    await page.goto(`${origin}/morning/${date}`);
    await settle(page);
    const cls = await readCls(page);
    expect(cls.total, `layout shift on /morning/${date}: ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);

    const report = page.getByTestId('morning-report');
    await expect(report.getByRole('heading', { name: 'While you were away' })).toBeVisible();
    await expect(report).toContainText('Sunday, March 8. From 8:00 AM Saturday to 8:00 AM Sunday.');

    // The question first, above the fold.
    const questions = page.getByTestId('morning-questions');
    const card = questions.locator('li').filter({ hasText: 'Hang the spring banners before the doors open?' });
    await expect(card).toBeVisible();
    const box = await card.boundingBox();
    expect(box, 'the question card has a box').not.toBeNull();
    expect(box!.y + 40, 'the question is above the fold').toBeLessThan(844);
    const sections = await page.locator('[data-testid^="morning-"][aria-label]').evaluateAll((els) => els.map((e) => e.getAttribute('data-testid')));
    expect(sections).toEqual(['morning-questions', 'morning-done', 'morning-stuck', 'morning-usage']);

    // Every part, in plain words.
    await expect(page.getByTestId('morning-done')).toContainText('Add the festival map page');
    await expect(page.getByTestId('morning-done')).not.toContainText('festival-site#128');
    await expect(page.getByTestId('morning-done')).toContainText('Complete task, changed overnight');
    await expect(page.getByTestId('morning-done')).toContainText('Print the volunteer rota');
    await expect(page.getByTestId('morning-stuck')).toContainText('Ticket scanner');
    await expect(page.getByTestId('morning-stuck')).toContainText('stopped on an error');
    await expect(page.getByTestId('morning-usage')).toContainText('Your chief worked 47m over 14 turns.');
    await expect(page.getByTestId('morning-usage')).toContainText('Claude plan 1: this 5-hour period 41% used, this week 73% used');
    await expect(page.getByTestId('morning-usage')).toContainText("Cost isn't tracked for Claude plans.");
    // How long the question has left, above its card (asks wait 24h).
    await expect(questions.locator('[data-morning-left]')).toContainText(/^Answer within \d+h/);
    await expect(report).not.toContainText('MUSTER_');

    // Phone sizing: no sideways scroll; every report link and button 44px tall.
    const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
    expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(iw);
    // The report's own controls; the question card is the Asks queue's own
    // markup (attentionItemsHTMLTo), unchanged here and measured on /attention.
    // (Its "Dictation is off" hint link is under 44px on /attention too.)
    const sizes = await report.locator('a:visible, button:visible').evaluateAll((els) =>
      els.filter((e) => !e.closest('[data-testid="morning-question-cards"]'))
        .map((e) => ({ name: (e.textContent || e.getAttribute('aria-label') || '').trim(), h: e.getBoundingClientRect().height })));
    expect(sizes.length, 'the report draws controls of its own (the done and stuck links, the ⓘs)').toBeGreaterThanOrEqual(2);
    expect(sizes.filter((s) => s.h < 44).map((s) => s.name), 'controls under 44px tall').toEqual([]);

    // Answer in place: the option is recorded and the page comes back here.
    await card.getByRole('button', { name: 'Hang them' }).click();
    await expect(page).toHaveURL(new RegExp(`/morning/${date}$`));
    await expect(page.getByTestId('morning-questions-none')).toHaveText('Nothing is waiting on you.');
  });
});
