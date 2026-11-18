import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { dsn, hookToken, officerNeedsYouPort, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// AN OFFICER'S PAGE AS THE PLACE ITS OWNER DEALS WITH IT (officers UX, PR 3),
// in a real browser against a real muster-server with plugins on, at a phone
// (390×844, touch) and a desktop (1440×900). The officer is declared through
// the admin API; what an instance and its plugin would leave (a post its
// plugin checked, the day's follows, a refusal on cadence, a finished item and
// a sent post) is the fixture's (e2e/officerfix), because the harness runs no
// officer instance.
//
// The person, on the officer's page: sees what needs them counted; approves
// the post INLINE (the Asks page's own card and route, so its "Confirm it's
// you" applies); ticks one follow of the batch and sends it; re-files the
// cadence refusal for later; and reads What it did — the sent post with a link
// to it, the finished item's summary, marked new. Nothing scrolls sideways.
//
// 🔴 RED at origin/main: the officer's page has no #officer-needs-you, no
// draft card, no batch, no refusal and no #officer-timeline.

const origin = `http://localhost:${officerNeedsYouPort}`;
const stamp = Date.now().toString(36);
let server: Running | undefined;

// The fixture's texts (e2e/officerfix: pinned there too).
const POST_BODY = 'The officers page now shows what needs you first.';
const SENT_BODY = 'Shipped: every officer decision on one page.';
const SENT_REF = '2122780902921224250';
const REFUSAL_DETAIL = 'cadence: 1 days since the last post, next window tomorrow';
const DONE_SUMMARY = 'Read four replies; none needed an answer.';

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/officerfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

async function declare(id: string) {
  const r = await fetch(`${origin}/api/officers/declared`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${hookToken}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ id, name: `Marketing ${id.split(':')[0].slice(-6)}`, charter: 'Draft posts for approval. Never post by any other means.',
      runtime: 'host', host: 'e2e-laptop', mode: 'shadow' }),
  });
  expect(r.status, `officer ${id} is declared: ${await r.text()}`).toBe(201);
}

async function remove(id: string) {
  await fetch(`${origin}/api/officers/declared/${encodeURIComponent(id)}`, { method: 'DELETE', headers: { Authorization: `Bearer ${hookToken}` } });
}

async function noSidewaysScroll(page: Page) {
  const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
  expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(iw);
}

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, officerNeedsYouPort, { MUSTER_PLUGINS: '1', MUSTER_FOCUS_INTERVAL: '0' });
});

test.afterAll(async () => {
  await server?.stop();
});

for (const vp of [
  { name: 'phone 390×844', key: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', key: 'desktop', use: { viewport: { width: 1440, height: 900 } } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);
    const id = `e2e-mkt-${vp.key}-${stamp}:main`;
    const plugin = `e2e-needs-${vp.key}-${stamp}`;
    test.afterEach(async () => {
      try {
        fix('rmplugin', '1', plugin);
      } finally {
        await remove(id);
      }
    });

    test('approve a post inline, tick the outreach batch, re-file a refusal, read what it did', async ({ page }, testInfo) => {
      await declare(id);
      const ids = JSON.parse(fix('seed', '1', id, plugin)) as { post: number; follows: number[]; refusal: number; sent: number };
      await signIn(page, origin);
      await page.goto(`${origin}/officers/${encodeURIComponent(id)}`);

      const needs = page.locator('#officer-needs-you');
      await expect(needs).toBeVisible();
      // The post, the batch and the refusal wait on the person.
      await expect(needs).toHaveAttribute('data-needs-count', '3');
      await expect(needs.locator('[data-needs-badge]')).toHaveText('3');
      const post = needs.locator(`[data-draft-id="${ids.post}"]`);
      await expect(post.locator('[data-draft-body]')).toHaveText(POST_BODY);
      const batch = needs.locator('[data-outreach-batch]');
      await expect(batch.locator('[data-outreach-entry]')).toHaveCount(2);
      await expect(batch.locator('[data-outreach-person]').first()).toContainText('untrusted');
      await expect(batch.locator('[data-outreach-person-bio]').first()).toHaveText('Builds small tools. <b>Not markup.</b>');
      const refusal = needs.locator(`[data-refusal="${ids.refusal}"]`);
      await expect(refusal.locator('[data-refusal-why]')).toContainText(REFUSAL_DETAIL);
      await noSidewaysScroll(page);
      const shot = testInfo.outputPath(`officer-needs-you-${vp.key}.png`);
      await page.screenshot({ path: shot, fullPage: true });
      await testInfo.attach(`officer-needs-you-${vp.key}.png`, { path: shot, contentType: 'image/png' });

      // 1. Approve the post where it is: back on this page, the card gone.
      await post.locator('[data-draft-approve]').click();
      await page.waitForURL((u) => u.pathname === `/officers/${id}` && u.searchParams.get('notice') === 'draft-approved');
      await expect(page.locator('#officer-notice')).toContainText('Approved');
      await expect(page.locator(`#officer-needs-you [data-draft-id="${ids.post}"]`)).toHaveCount(0);
      await expect(page.locator('#officer-needs-you')).toHaveAttribute('data-needs-count', '2');

      // 2. Tick the first follow only and send: the batch is decided.
      const b = page.locator('#officer-needs-you [data-outreach-batch]');
      await b.locator(`[data-outreach-entry="${ids.follows[0]}"] [data-outreach-tick]`).check();
      await b.locator('[data-outreach-approve]').click();
      await page.waitForURL((u) => u.pathname === `/officers/${id}` && u.searchParams.get('notice') === 'batch-decided');
      await expect(page.locator('#officer-needs-you [data-outreach-batch]')).toHaveCount(0);

      // 3. Re-file the refusal for later: it leaves Needs you.
      const r = page.locator(`#officer-needs-you [data-refusal="${ids.refusal}"]`);
      await r.locator('select[name=when]').selectOption('1h');
      await r.locator('[data-refusal-refile-submit]').click();
      await page.waitForURL((u) => u.pathname === `/officers/${id}` && u.searchParams.get('notice') === 'refiled');
      await expect(page.locator('#officer-needs-you [data-refusal]')).toHaveCount(0);
      await expect(page.locator('#officer-needs-you')).toHaveAttribute('data-needs-count', '0');
      await expect(page.locator('#officer-needs-none')).toBeVisible();

      // 4. What it did: the sent post, with a link to it, and the finished
      // item's summary — newest first, ids behind the ⓘ only.
      const tl = page.locator('#officer-timeline');
      const sent = tl.locator(`[data-timeline-draft="${ids.sent}"]`);
      await expect(sent.locator('[data-timeline-title]')).toHaveText('Posted on X');
      await expect(sent.locator('[data-timeline-body]')).toHaveText(SENT_BODY);
      await expect(sent.locator('[data-timeline-post-link]')).toHaveAttribute('href', `https://x.com/i/web/status/${SENT_REF}`);
      await expect(tl.locator('[data-timeline-summary]', { hasText: DONE_SUMMARY })).toBeVisible();
      await expect(tl).not.toContainText(SENT_REF, { useInnerText: true });
      await sent.getByRole('button', { name: 'Details of this post' }).click();
      await expect(page.locator(`[data-info-sheet]:popover-open [data-info-code]`)).toContainText(SENT_REF);
      await page.keyboard.press('Escape');
      await noSidewaysScroll(page);
      const after = testInfo.outputPath(`officer-needs-you-${vp.key}-after.png`);
      await page.screenshot({ path: after, fullPage: true });
      await testInfo.attach(`officer-needs-you-${vp.key}-after.png`, { path: after, contentType: 'image/png' });
    });
  });
}
