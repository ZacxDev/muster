import { execFileSync } from 'node:child_process';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { dsn, repoRoot, teamPagesConnectPort, teamPagesPort, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';

// A TEAM'S PAGE, read by a member who is NOT its creator (migration 0165;
// docs/pages.md "A team's pages"), at a phone (390×844) and a desktop
// (1280×800), each set here, never inherited from the config.
//
// The signed-in operator (user 1) is a VIEWER of a team whose page another
// person created (e2e/mkteampage: the page's created_by_user is that person).
// The member finds the page in Pages marked with its team, opens it, is drawn
// no Delete, no "Show this version", no Revoke and no ⋯ menu — but Share —
// and answers its question from the page's own view; the response is
// recorded as theirs (responder_user = 1); its file field says files can't
// be sent from a team page yet. Its own server: pages on, a link key. Every
// value is invented.
//
// 🔴 REGRESSION TEST: red at origin/main, where a team's page is in no
// member's Pages list (the pages tables were personal).

const origin = `http://localhost:${teamPagesPort}`;
const stamp = Date.now().toString(36);
let server: Running | undefined;

const sizes = [
  { name: 'phone', viewport: { width: 390, height: 844 }, touch: true },
  { name: 'desktop', viewport: { width: 1280, height: 800 }, touch: false },
];

function mkteampage(...args: string[]): string {
  return execFileSync('go', ['run', './e2e/mkteampage', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();
}

const confirmBar = (p: Page) => p.locator('[data-send-confirm]');

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionTerm, teamPagesPort, {
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_PAGES: '1',
    MUSTER_PUBLIC_URL: origin,
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x5a).toString('base64'),
    MUSTER_CONNECT_PORT: String(teamPagesConnectPort),
    MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${teamPagesConnectPort}`,
  });
});

test.afterAll(async () => {
  await server?.stop();
});

for (const size of sizes) {
  test.describe(`a team's page at ${size.name} size`, () => {
    test.use({ viewport: size.viewport, hasTouch: size.touch, isMobile: size.touch });
    let fx: { team: number; name: string; page: number; creator: number } | undefined;
    test.beforeAll(() => {
      fx = JSON.parse(mkteampage('create', `${stamp}-${size.name}`));
    });
    test.afterAll(() => {
      if (fx) mkteampage('remove', String(fx.team));
    });

    test('a member who is not its creator finds it marked with its team, is drawn no managing control, and answers it as themselves', async ({ page }) => {
      const f = fx!;
      await signIn(page, origin);

      // 1. Pages lists it, marked "Team: <name>".
      await page.goto(origin + '/pages');
      const row = page.locator(`li[data-page-id="${f.page}"]`);
      await expect(row).toBeVisible();
      await expect(row.locator(`[data-page-team="${f.team}"]`)).toHaveText(`Team: ${f.name}`);
      await expect(row.locator('[data-page-link-actions]'), 'the ⋯ menu is drawn for a member who is not the creator').toHaveCount(0);
      const width = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
      expect(width, 'Pages scrolls sideways').toBeLessThanOrEqual(0);

      // 2. Its screen (the row's ⋯ → Details): Share, and no Delete, Make
      // current, Revoke, Revoke link or New link.
      await page.goto(origin + `/pages/${f.page}`);
      await expect(page.locator(`[data-testid="page-detail"] [data-page-team="${f.team}"]`)).toContainText(`Team: ${f.name}`);
      await expect(page.locator('[data-page-share-box] button').first()).toBeVisible();
      for (const sel of ['[data-page-delete]', '[data-version-current]', '[data-link-revoke]', '[data-page-link-actions]']) {
        await expect(page.locator(sel), `${sel} is drawn for a member who is not the creator`).toHaveCount(0);
      }

      // 3. Its view: the member answers its question; it is recorded as
      // theirs.
      await Promise.all([page.waitForURL(origin + `/pages/${f.page}/view`), page.locator('[data-page-view]').click()]);
      const frame = page.frameLocator('iframe#page');
      // 3a. Its header: "Pages › <team> ›" before the title, Pages a link;
      // no Share for a member who may only view (pages.MayManage); one row,
      // nothing past the screen's edge.
      const header = page.locator('header[data-page-brand]');
      await expect(header.locator('[data-page-crumb]')).toHaveText(['Pages', f.name]);
      await expect(header.locator('a[data-page-crumb]')).toHaveAttribute('href', '/pages');
      await expect(page.locator('[data-page-share]'), 'Share is drawn for a member who may only view').toHaveCount(0);
      const past = await header.evaluate((h) =>
        Array.from(h.querySelectorAll('img, .cr, .sep, [data-page-title], .sess, .chip'))
          .map((e) => e.getBoundingClientRect())
          .filter((r) => r.width > 0 && (r.left < 0 || r.right > window.innerWidth + 0.5)).length,
      );
      expect(past, 'a header item past the screen').toBe(0);
      const pick = frame.locator('[data-muster-component="pick"]');
      await pick.locator('input[name="answer"]').fill('Thursday, from ' + size.name);
      await pick.getByRole('button', { name: 'Send pick' }).click();
      // The page has no html block, so no script but muster's own: the
      // member's own press records at once, with no "Send as you?" bar (the
      // operator's decision; pages.spec.ts covers a page that keeps it).
      await expect(pick.locator('[data-muster-status]')).toHaveText('Sent. Thank you.');
      await expect(confirmBar(page), 'a script-free page drew the bar').toHaveCount(0);
      expect(mkteampage('responses', String(f.page)).split('\n'), 'the response is recorded as the member (user 1)').toEqual(['1']);

      // 3b. Its file field says files can't be sent from a team page yet, and
      // nothing is recorded.
      const deck = frame.locator('[data-muster-component="deck"]');
      await expect(deck.locator('[data-muster-status]')).toHaveText("Files can't be sent from a team page yet.");
      await expect(deck.locator('input[type="file"]')).toBeDisabled();
      await expect(deck.getByRole('button', { name: 'Upload slides' })).toHaveCount(0);
      expect(mkteampage('responses', String(f.page)).split('\n'), 'a file was recorded').toEqual(['1']);

      // 4. Back on the screen, the member reads it as theirs.
      await page.goto(origin + `/pages/${f.page}`);
      await expect(page.locator('[data-response-by="you"]')).toHaveCount(1);
    });
  });
}
