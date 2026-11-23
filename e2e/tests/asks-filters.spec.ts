import { execFileSync } from 'node:child_process';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { asksFiltersPort, dsn, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// THE ASKS PAGE'S FILTER CHIPS (internal/api asks_filter.go, ui/asks_chips.go;
// operator decisions "One-tap chips in the URL", source = "The officer or
// agent"), in a real browser against a real muster-server with plugins on, at
// a phone (390×844, touch), a desktop (1440×900) and a phone with no script:
//
//   - the row sits under the search box: Questions, Confirmations, Drafts,
//     Notices, then the sources, the marketing officer first (the busiest);
//   - ONE TAP on the marketing officer's chip keeps only what it raised — its
//     questions and the step-up card a SESSION filed for its draft on Open,
//     its answered question on Answered, its draft on Drafts — and the address
//     says so (?from=officer:…); a tap on the active chip clears it, and Back
//     undoes the tap;
//   - nothing of the other owner's (the same officer id) is ever listed.
//
// The queue is fixture rows (e2e/asksfiltfix), re-seeded before every test.
//
// 🔴 RED AT THE BASE: there is no chip row.

const origin = `http://localhost:${asksFiltersPort}`;
const stamp = 'filters' + Date.now().toString(36);
let server: Running | undefined;
type Ids = Record<string, number>;
let ids: Ids;

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/asksfiltfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, asksFiltersPort, { MUSTER_PLUGINS: '1', MUSTER_FOCUS_INTERVAL: '0' });
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

// The ids of the cards a list holds, by their element ids' prefix.
async function listed(page: Page, list: string, prefix: string): Promise<number[]> {
  const out = await page.locator(`${list} > li[id^="${prefix}"]`).evaluateAll((els, p) => els.map((e) => Number(e.id.slice((p as string).length))), prefix);
  return out.sort((a, b) => a - b);
}

// What the fixture's own ids are, of those listed (the leg's database is
// shared: another spec's rows may be on the page too).
const OWN = ['mkt_ask', 'mkt_ask2', 'mkt_stepup', 'mkt_answered', 'mkt_draft', 'product_ask', 'agent_ask', 'agent_ask2', 'agent_answered', 'agent_draft',
  'session_ask', 'notice', 'other_mkt_ask', 'other_sales_ask', 'other_draft'];
const mine = (got: number[]) => got.filter((id) => OWN.some((k) => ids[k] === id)).sort((a, b) => a - b);
const sorted = (...n: number[]) => n.slice().sort((a, b) => a - b);
const marketingChip = (page: Page) => page.locator('[data-asks-chips] a[data-asks-chip="from"][data-asks-chip-key="officer:1:marketing:acme"]');

for (const vp of [
  { name: 'phone 390×844', js: true, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', js: true, use: { viewport: { width: 1440, height: 900 } } },
  { name: 'phone 390×844, no script', js: false, use: { viewport: { width: 390, height: 844 }, javaScriptEnabled: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('one tap on the Marketing chip shows only what the marketing officer raised', async ({ page }) => {
      await signIn(page, origin);
      await page.goto(origin + '/attention');
      const row = page.locator('nav[data-asks-chips]');
      await expect(row).toBeVisible();
      // Under the search box.
      const sb = await page.locator('form[data-asks-search]').boundingBox();
      const rb = await row.boundingBox();
      expect(rb!.y, 'the chip row is under the search box').toBeGreaterThanOrEqual(sb!.y + sb!.height - 1);
      await expect(row.locator('a[data-asks-chip="type"] > span:first-child')).toHaveText(['Questions', 'Confirmations', 'Drafts', 'Notices']);
      const chip = marketingChip(page);
      await expect(chip).toBeVisible();
      await expect(chip.locator('span').first()).toHaveText('Marketing — Acme');
      // The busiest source comes first: the session whose release notes fill
      // the feed; the marketing officer is inline too, not under More.
      await expect(row.locator('a[data-asks-chip="from"]').first()).toHaveAttribute('data-asks-chip-key', 'session:ws-filters:Release notes');
      await expect(row.locator('[data-asks-chips-more] [data-asks-chip-key="officer:1:marketing:acme"]')).toHaveCount(0);
      // No other owner's officer is a chip, anywhere on the page.
      await expect(page.locator('[data-asks-chip-key$=":sales:zeta"]')).toHaveCount(0);
      await expect(page.locator('body')).not.toContainText('OTHER OWNER');
      const allOpen = mine(await listed(page, '#attention-items', 'ask-'));
      expect(allOpen.length, 'control: the unfiltered Open tab lists the other sources too').toBeGreaterThan(3);

      // ONE TAP.
      if (vp.use.hasTouch) await chip.tap();
      else await chip.click();
      await expect(page).toHaveURL(/[?&]from=officer%3A1%3Amarketing%3Aacme/);
      await expect(marketingChip(page)).toHaveAttribute('aria-current', 'true');
      expect(mine(await listed(page, '#attention-items', 'ask-')), 'Open').toEqual(sorted(ids.mkt_ask, ids.mkt_ask2, ids.mkt_stepup));
      expect(mine(await listed(page, '#attention-answered', 'answered-')), 'Done').toEqual([ids.mkt_answered]);
      expect(mine(await listed(page, '#asks-drafts-pending', 'draft-')), 'Drafts').toEqual([ids.mkt_draft]);
      await expect(page.locator('body')).not.toContainText('OTHER OWNER');

      // A tap on the active chip clears it.
      if (vp.use.hasTouch) await marketingChip(page).tap();
      else await marketingChip(page).click();
      await expect(page).toHaveURL(origin + '/attention');
      expect(mine(await listed(page, '#attention-items', 'ask-'))).toEqual(allOpen);
      // Back is the filtered page again.
      await page.goBack();
      await expect(page).toHaveURL(/[?&]from=officer%3A1%3Amarketing%3Aacme/);
      expect(mine(await listed(page, '#attention-items', 'ask-')), 'Back').toEqual(sorted(ids.mkt_ask, ids.mkt_ask2, ids.mkt_stepup));
    });

    test('a type chip and a source chip together, and Clear', async ({ page }) => {
      await signIn(page, origin);
      await page.goto(origin + '/attention?from=officer%3A1%3Amarketing%3Aacme');
      const confirmations = page.locator('[data-asks-chips] a[data-asks-chip="type"][data-asks-chip-key="confirmation"]');
      if (vp.use.hasTouch) await confirmations.tap();
      else await confirmations.click();
      await expect(page).toHaveURL(/type=confirmation/);
      await expect(page).toHaveURL(/from=officer%3A1%3Amarketing%3Aacme/);
      expect(mine(await listed(page, '#attention-items', 'ask-'))).toEqual([ids.mkt_stepup]);
      await page.locator('[data-asks-chips-clear]').click();
      await expect(page).toHaveURL(origin + '/attention');
      await expect(page.locator('[data-asks-chips] a[aria-current="true"]')).toHaveCount(0);
    });
  });
}
