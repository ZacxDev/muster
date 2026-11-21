import { execFileSync } from 'node:child_process';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { asksSearchPort, dsn, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// SEARCHING THE ASKS PAGE AND THE ID ON EVERY CARD (internal/api
// asks_search.go, internal/ui ask_id.go), in a real browser against a real
// muster-server, at a phone (390×844, touch) and a desktop (1440×900):
//
//   - typing an ask's id into the box over the page goes straight to that
//     ask's page, and Back returns to the page (the jump is not a history
//     entry that bounces);
//   - words list the matching asks, each linking to its page, with a count;
//     nothing found is one line with a way back;
//   - each card carries its id ("Ask #N", "Draft #N") as a small button that
//     copies it — keyboard reachable, named "Copy Ask #N" — and pressing it
//     never expands the card; with no script it is plain text.
//
// The queue is the Asks feed's fixture rows (e2e/asksfeedfix) and a step-up
// card for a draft (e2e/stepupfix), written through the real store.
//
// 🔴 REGRESSION: on origin/main there is no search box and no card draws an
// id, so every test here is red there.

const origin = `http://localhost:${asksSearchPort}`;
const stamp = Date.now().toString(36);
const agent = 'e2e-search-' + stamp;
let server: Running | undefined;
type Ids = { deploy: number; freetext: number; rotate: number; notice: number; answered: number; draft: number; task: number };
let ids: Ids;
let card = 0;

const feedFix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/asksfeedfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();
const stepupFix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/stepupfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, asksSearchPort, { MUSTER_PLUGINS: '1', MUSTER_TASK_ROUTING: '1', MUSTER_FOCUS_INTERVAL: '0' });
  ids = JSON.parse(feedFix('seed', '1', agent)) as Ids;
  card = Number(stepupFix('card', '1'));
});

test.afterAll(async () => {
  try {
    feedFix('clear', '1', agent);
    stepupFix('clear', '1');
  } finally {
    await server?.stop();
  }
});

const box = (page: Page) => page.getByRole('searchbox', { name: 'Search asks by id or text' });

async function search(page: Page, q: string) {
  await box(page).fill(q);
  await box(page).press('Enter');
}

// noSideways: the page never scrolls sideways at this width.
async function noSideways(page: Page) {
  const [sw, iw] = await page.evaluate(() => [document.documentElement.scrollWidth, window.innerWidth]);
  expect(sw, 'the page scrolls sideways').toBeLessThanOrEqual(iw);
}

for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', use: { viewport: { width: 1440, height: 900 } } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('typing an id goes straight to the ask, and Back returns to the page', async ({ page }) => {
      await signIn(page, origin);
      await page.goto(origin + '/attention');
      // The box is on the heading's row ("Asks · N open"), not a line of its own.
      const form = (await page.locator('[data-asks-search]').boundingBox())!;
      const h1 = (await page.getByRole('heading', { level: 1, name: 'Asks' }).boundingBox())!;
      expect(form.height).toBeLessThanOrEqual(48);
      expect(form.y, 'the box starts above the heading\u2019s bottom').toBeLessThan(h1.y + h1.height);
      expect(form.y + form.height, 'the box ends below the heading\u2019s top').toBeGreaterThan(h1.y);
      await expect(page.locator(`#ask-${ids.rotate}`)).toBeVisible();
      await noSideways(page);

      await search(page, String(ids.rotate));
      await expect(page).toHaveURL(`${origin}/attention/asks/${ids.rotate}`);
      const chip = page.locator('[data-asks-page-card] [data-id-copy]');
      await expect(chip).toHaveText(`Ask #${ids.rotate}`);
      await page.goBack();
      await expect(page).toHaveURL(`${origin}/attention`);
      await expect(box(page)).toBeVisible();

      // "Ask #N" and "#N" are ids too.
      await search(page, `Ask #${ids.freetext}`);
      await expect(page).toHaveURL(`${origin}/attention/asks/${ids.freetext}`);
      await page.goto(origin + '/attention');
      await search(page, `#${ids.deploy}`);
      await expect(page).toHaveURL(`${origin}/attention/asks/${ids.deploy}`);
      // A draft's id goes to its card on the Drafts tab.
      await page.goto(origin + '/attention');
      await search(page, `draft ${ids.draft}`);
      await expect(page).toHaveURL(new RegExp(`/attention\\?tab=drafts&draft=${ids.draft}#draft-${ids.draft}$`));
      await expect(page.locator(`#draft-${ids.draft} [data-id-copy]`)).toHaveText(`Draft #${ids.draft}`);
    });

    test('words list the matching asks with a count; nothing found is one line', async ({ page }) => {
      await signIn(page, origin);
      await page.goto(origin + '/attention');
      // Who asked: every one of the fixture agent's asks, answered too.
      await search(page, agent.toUpperCase());
      await expect(page).toHaveURL(`${origin}/attention?q=${encodeURIComponent(agent.toUpperCase())}`);
      const results = page.locator('[data-asks-search-result]');
      await expect(results).toHaveCount(4);
      const got = await results.evaluateAll((els) => els.map((e) => e.getAttribute('data-ask-id')).sort());
      expect(got).toEqual([ids.answered, ids.deploy, ids.freetext, ids.rotate].map(String).sort());
      await expect(page.locator('[data-asks-search-count]')).toHaveText(/^4 results\s*Clear$/);
      await expect(box(page)).toHaveValue(agent.toUpperCase());
      await noSideways(page);
      // A result opens its ask.
      await page.locator(`#search-ask-${ids.rotate} [data-asks-search-open]`).click();
      await expect(page).toHaveURL(`${origin}/attention/asks/${ids.rotate}`);
      await page.goBack();
      await expect(results).toHaveCount(4);

      // A step-up card's post text finds it; its title names the draft.
      await search(page, 'acme-two');
      const stepUp = page.locator(`#search-ask-${card}`);
      await expect(stepUp).toBeVisible();
      await expect(stepUp.locator('[data-asks-search-title]')).toHaveText('Post to X? · Draft #41');

      await search(page, 'no-such-words-' + stamp);
      const empty = page.locator('[data-asks-search-empty]');
      await expect(empty).toHaveText(`No asks match “no-such-words-${stamp}”. Clear`);
      await empty.getByRole('link', { name: 'Clear' }).click();
      await expect(page).toHaveURL(`${origin}/attention`);
      await expect(page.locator('[data-asks-tabs]')).toBeVisible();
    });

    test('the id copies at a tap or a key, and never expands its card', async ({ page, context }) => {
      await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin });
      await signIn(page, origin);
      await page.goto(origin + '/attention');
      const cardEl = page.locator(`#ask-${ids.deploy}`);
      await expect(cardEl).toHaveAttribute('data-expanded', 'false');
      const chip = cardEl.locator('button[data-id-copy]');
      await expect(chip).toHaveCount(1);
      await expect(chip).toHaveAccessibleName(`Copy Ask #${ids.deploy}`);
      await expect(chip).toHaveText(`Ask #${ids.deploy}`);
      // Control: this card DOES expand when its head is tapped (so "it did not
      // expand" below is a claim about the chip).
      await cardEl.locator('[data-asks-toggle]').click();
      await expect(cardEl).toHaveAttribute('data-expanded', 'true');
      await cardEl.locator('[data-asks-toggle]').click();
      await expect(cardEl).toHaveAttribute('data-expanded', 'false');

      // The copy is announced in ONE polite live region, there before the
      // copy (a region added with its words is not reliably read out).
      const said = page.locator('[data-id-copy-status]');
      await expect(said).toHaveCount(1);
      await expect(said).toHaveAttribute('aria-live', 'polite');
      await expect(said).toHaveText('');
      await chip.click();
      await expect(chip).toHaveText('Copied');
      await expect(said).toHaveText(`Copied Ask #${ids.deploy}`);
      // The focused button keeps its name while it says Copied.
      await expect(chip).toHaveAccessibleName(`Copy Ask #${ids.deploy}`);
      expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(`Ask #${ids.deploy}`);
      await expect(cardEl).toHaveAttribute('data-expanded', 'false');
      await expect(chip).toHaveText(`Ask #${ids.deploy}`, { timeout: 5_000 });
      await expect(chip).toHaveAccessibleName(`Copy Ask #${ids.deploy}`);

      // By keyboard: the chip takes focus and Enter copies — still no expand.
      await page.evaluate(() => navigator.clipboard.writeText(''));
      await chip.focus();
      await expect(chip).toBeFocused();
      await page.keyboard.press('Enter');
      await expect(chip).toHaveText('Copied');
      expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(`Ask #${ids.deploy}`);
      await expect(cardEl).toHaveAttribute('data-expanded', 'false');

      // The step-up card names its draft in its title, and is an ask with its own id.
      const stepUp = page.locator(`#ask-${card}`);
      await expect(stepUp.locator('[data-stepup-title]')).toHaveText('Post to X? · Draft #41');
      await expect(stepUp.getByRole('button', { name: `Copy Ask #${card}` })).toBeVisible();
      await noSideways(page);
    });
  });
}

test.describe('no script', () => {
  test.use({ javaScriptEnabled: false, storageState: { cookies: [], origins: [] }, viewport: { width: 390, height: 844 } });

  test('the id is plain text, and a search still works', async ({ page }) => {
    await signIn(page, origin);
    await page.goto(origin + '/attention');
    const chip = page.locator(`#ask-${ids.rotate} [data-id-copy]`);
    await expect(chip).toHaveText(`Ask #${ids.rotate}`);
    expect(await chip.evaluate((e) => e.tagName)).toBe('SPAN');
    await expect(page.locator(`#ask-${ids.rotate}`).getByRole('button', { name: /Copy/ })).toHaveCount(0);
    await page.goto(`${origin}/attention?q=${ids.rotate}`);
    await expect(page).toHaveURL(`${origin}/attention/asks/${ids.rotate}`);
  });
});
