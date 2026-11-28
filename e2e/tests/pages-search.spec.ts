import { execFileSync } from 'node:child_process';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { dsn, pagesSearchPort, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// THE PAGES LIST'S SEARCH AND FILTERS (internal/api pages_search.go,
// internal/ui page_search.go; docs/pages.md "Finding a page"), in a real
// browser against a real muster-server with pages on, over four pages the real
// store wrote (e2e/pagesfix search) whose every property the filters read is
// pairwise distinct — room's TITLE holds venue's number:
//
//   - each row shows its page's number, "#<id>";
//   - typing filters the list in place (no navigation), and the address
//     follows (?q=…);
//   - a number ("#<id>", a bare id) lists that page FIRST, then room, whose
//     title holds the digits (operator decision: "ID first, then titles");
//     a number that is no page's id lists ledger, whose title holds it;
//   - "/" puts the cursor in the box when focus is not in a field, and types
//     a "/" when it is;
//   - "Waiting on you" lists venue and room (an unanswered question; one
//     answered only on an older version), never budget (answered) or notes
//     (an html block, which cannot be judged); "Made by" lists one maker's;
//   - a reload keeps the search and both filters, in the address and the form;
//   - nothing matching says "No pages match." with Clear filters;
//   - with no script, the address alone filters and Apply is drawn.
//
// Every value below is an obvious fake.
//
// NEW FEATURE: at the base the list has no box, no filters and no numbers, so
// every test here is red there; at bab5ee17f a number was the id only, so the
// number test is red there.

const origin = `http://localhost:${pagesSearchPort}`;
let server: Running | undefined;
type Seeded = { venue: number; budget: number; notes: number; room: number; ledger: number; number: string };
let ids: Seeded;

const fix = (cmd: 'search' | 'search-clear') =>
  execFileSync('go', ['run', './e2e/pagesfix', cmd, '1'], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString();

test.use({ storageState: { cookies: [], origins: [] }, viewport: { width: 1280, height: 800 } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, pagesSearchPort, { MUSTER_PAGES: '1', MUSTER_PUBLIC_URL: origin });
  ids = JSON.parse(fix('search')) as Seeded;
  expect(ids.room, 'the fixture made four pages').toBeGreaterThan(ids.venue);
});

test.afterAll(async () => {
  try {
    if (server) fix('search-clear');
  } finally {
    await server?.stop();
  }
});

const box = (page: Page) => page.locator('#pages-search-q');
const waiting = (page: Page) => page.locator('[data-pages-waiting-input]');
const maker = (page: Page) => page.locator('select[data-pages-maker]');
const sorted = (...n: number[]) => n.slice().sort((a, b) => a - b);

// listed is the page ids the list draws rows for, sorted.
async function listed(page: Page): Promise<number[]> {
  const got = await page.locator('[data-pages-list-region] li[data-page-id]').evaluateAll((els) => els.map((e) => Number(e.getAttribute('data-page-id'))));
  return got.sort((a, b) => a - b);
}

// inOrder is the page ids the list draws rows for, in the list's order.
const inOrder = (page: Page) =>
  page.locator('[data-pages-list-region] li[data-page-id]').evaluateAll((els) => els.map((e) => Number(e.getAttribute('data-page-id'))));

// expectListed waits for the list to draw exactly want.
async function expectListed(page: Page, want: number[], what: string) {
  await expect.poll(() => listed(page), { message: what, timeout: 10_000 }).toEqual(sorted(...want));
}

// open signs in and opens the list (with query), and marks the window so a
// later check can tell an in-place update from a navigation.
async function open(page: Page, query = '') {
  await signIn(page, origin);
  await page.goto(origin + '/pages' + query);
  await expect(box(page)).toBeVisible();
  await page.evaluate(() => ((window as unknown as { __pagesMark: number }).__pagesMark = 7));
}

const stillHere = (page: Page) => page.evaluate(() => (window as unknown as { __pagesMark?: number }).__pagesMark);

test('each row shows its number, and typing filters the list in place', async ({ page }) => {
  await open(page);
  await expectListed(page, [ids.venue, ids.budget, ids.notes, ids.room, ids.ledger], 'CONTROL: the unfiltered list');
  for (const id of [ids.venue, ids.budget, ids.notes, ids.room, ids.ledger]) {
    await expect(page.locator(`li[data-page-id="${id}"] [data-page-number]`), `page ${id}'s number`).toHaveText(`#${id}`);
  }
  await box(page).pressSequentially('budg', { delay: 30 });
  await expectListed(page, [ids.budget], 'typing "budg"');
  await expect(page).toHaveURL(`${origin}/pages?q=budg`);
  expect(await stillHere(page), 'the list updated in place, without a navigation').toBe(7);
  await expect(page.locator('[data-pages-count]')).toHaveText(/1 page/);
  // A maker's name finds that maker's pages.
  await box(page).fill('design');
  await expectListed(page, [ids.budget, ids.room], 'a maker by name');
});

test('a number lists its page first, then the titles that hold it', async ({ page }) => {
  await open(page);
  await expect(page.locator(`li[data-page-id="${ids.room}"] [data-page-title]`), 'CONTROL: room\'s title holds venue\'s number').toContainText(String(ids.venue));
  const all = await inOrder(page);
  expect(all.indexOf(ids.room), 'CONTROL: unfiltered, room is listed above venue').toBeLessThan(all.indexOf(ids.venue));
  await box(page).fill(`#${ids.venue}`);
  await expect.poll(() => inOrder(page), { message: `#${ids.venue}: venue first, then room` }).toEqual([ids.venue, ids.room]);
  await expect(page).toHaveURL(`${origin}/pages?q=%23${ids.venue}`);
  await box(page).fill(String(ids.venue));
  await expect.poll(() => inOrder(page), { message: `a bare ${ids.venue}` }).toEqual([ids.venue, ids.room]);
  // A number that is no page's id still finds the title that holds it.
  await box(page).fill(ids.number);
  await expectListed(page, [ids.ledger], `${ids.number}, no page's id`);
  await expect(page.locator('[data-pages-none]')).toHaveCount(0);
});

test('"/" focuses the search box, unless focus is already in a field', async ({ page }) => {
  await open(page);
  await page.locator('h1').click();
  await expect(box(page)).not.toBeFocused();
  await page.keyboard.press('/');
  await expect(box(page), '"/" focused the box').toBeFocused();
  await expect(box(page), 'the "/" was not typed').toHaveValue('');
  // In the box, "/" is a character like any other.
  await page.keyboard.type('a/b');
  await expect(box(page)).toHaveValue('a/b');
  // In another field ("Made by"), "/" does not take focus away.
  await maker(page).focus();
  await page.keyboard.press('/');
  await expect(maker(page)).toBeFocused();
  // And no palette opened over the list.
  await expect(page.locator('dialog[open]')).toHaveCount(0);
});

test('Waiting on you and Made by filter, together, and a reload keeps them', async ({ page }) => {
  await open(page);
  await expect(page.locator('[data-pages-waiting-count]'), 'the chip says how many wait').toHaveText('2');
  await waiting(page).check();
  await expectListed(page, [ids.venue, ids.room], 'waiting on you');
  await expect(page).toHaveURL(`${origin}/pages?waiting=1`);
  await maker(page).selectOption('Design helper');
  await expectListed(page, [ids.room], 'waiting, made by Design helper');
  await box(page).fill('room');
  await expectListed(page, [ids.room], 'and the words');
  // The server writes the address (url.Values: its keys sorted).
  await expect(page).toHaveURL(`${origin}/pages?by=Design+helper&q=room&waiting=1`);
  expect(await stillHere(page), 'every change updated in place').toBe(7);

  await page.reload();
  await expect(box(page)).toHaveValue('room');
  await expect(waiting(page)).toBeChecked();
  await expect(maker(page)).toHaveValue('Design helper');
  await expectListed(page, [ids.room], 'after a reload');

  // Made by alone.
  await page.goto(origin + '/pages?by=Release+helper');
  await expectListed(page, [ids.venue, ids.notes, ids.ledger], 'made by Release helper');
});

test('nothing matching says so, and Clear filters brings the whole list back', async ({ page }) => {
  await open(page);
  await box(page).fill('zebra crossing');
  await expect(page.locator('[data-pages-none]')).toContainText('No pages match.');
  await expectListed(page, [], 'nothing matches');
  await page.locator('[data-pages-clear]').click();
  await expect(page).toHaveURL(`${origin}/pages`);
  await expectListed(page, [ids.venue, ids.budget, ids.notes, ids.room, ids.ledger], 'cleared');
  await expect(box(page)).toHaveValue('');
});

test.describe('no script', () => {
  test.use({ javaScriptEnabled: false });

  test('the address alone filters, and Apply is drawn', async ({ page }) => {
    await signIn(page, origin);
    await page.goto(`${origin}/pages?waiting=1&by=Release+helper`);
    await expectListed(page, [ids.venue], 'waiting, made by Release helper, no script');
    await expect(page.locator('[data-pages-filter-apply]')).toBeVisible();
    // The toggle and Apply: the form's own GET.
    await page.locator('[data-pages-waiting]').click();
    await page.locator('[data-pages-filter-apply]').click();
    await expect(page).toHaveURL(`${origin}/pages?q=&by=Release+helper`);
    await expectListed(page, [ids.venue, ids.notes, ids.ledger], 'made by Release helper, no script');
  });
});

test('with a script, Apply is not drawn', async ({ page }) => {
  await open(page);
  await expect(page.locator('[data-pages-filter-apply]')).toBeHidden();
});
