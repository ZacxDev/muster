import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import type { Browser, BrowserContext, Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { dsn, pagesDeclutterConnectPort, pagesDeclutterPort, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { clumsyCounts, pageWords, setupJargon } from '../lib/plainwords';

// THE PAGES SCREENS, PLAIN (internal/api pages.go, internal/ui
// page_rows.go; docs/pages.md): the list, a page's own screen, the owner's
// version popover and a link's view, on a phone (390×844) and a desktop
// (1280×800), over pages the real store wrote (e2e/pagesfix) whose counts are
// pairwise distinct:
//
//   - the top layer spells no setup jargon and no clumsy count ("(s)", "0 live
//     link", "0 responses", "0 waiting");
//   - each screen's words stay under a cap pinned a little above what it draws
//     (the main region's text; a page's own content frame is not counted);
//   - a row's badges are exactly the ones it has, in words that agree;
//   - a tap on a row opens the page's own view; the row's ⋯ reaches Share and
//     Details by keyboard and Escape closes it;
//   - a page's screen draws Links only with a live or waiting link, the ⋯ to
//     revoke one only with a live one, its versions newest first, and a tap
//     on one opens it;
//   - a long title wraps whole words and never overflows;
//   - the owner's view's header: "Pages ›" goes to the list, the title opens
//     a popover with the whole title, the page's id and Copy ID, Share
//     copies the page's link (or hands it to the share sheet), and a title
//     far wider than a phone keeps the header one row inside the screen;
//   - no screen scrolls sideways, every target is at least 44px, and on a
//     desktop the list is at least 600px wide.
//
// MUSTER_E2E_PAGES_DECLUTTER_SHOTS=<dir> also saves a screenshot of each
// screen there, and each screen's word count is logged ("WORDS …").
//
// Every value below is an obvious fake.

const origin = `http://localhost:${pagesDeclutterPort}`;
const KEY = Buffer.alloc(32, 0x5a).toString('base64');
const shots = process.env.MUSTER_E2E_PAGES_DECLUTTER_SHOTS ?? '';
let server: Running | undefined;
type Seeded = { plan: number; notes: number; survey: number; budget: number; long: number; token: string };
let seeded: Seeded;

const PHONE = { width: 390, height: 844 };
const DESKTOP = { width: 1280, height: 800 };
const sizes = [
  ['phone', PHONE],
  ['desktop', DESKTOP],
] as const;

// The word caps, a little above what each screen draws (measured on this
// fixture at both sizes; the PR records the before and after counts). The
// list's rose from 48 to 64 (60 drawn) when the operator asked for
// its search, its filters and each page's number: "Search pages", "Waiting on
// you" and its count, and "#<id> ·" on each of the five rows.
const CAPS = { list: 64, detail: 72, versions: 34, link: 2 } as const;

// WHEN is a time in words once the app script has run (internal/ui
// plain_when.go): the day and the clock in the reader's own zone ("today at
// 9:14 PM" in this browser's en-US), never "UTC". The fixture's times are
// 1 to 24 hours old, so the day is today or yesterday.
const WHEN = String.raw`(today|yesterday) at \d{1,2}:\d{2}\s[AP]M`;
const when = (before: string, after = '') => new RegExp('^' + before + WHEN + after + '$');

const fix = (cmd: 'seed' | 'clear') =>
  execFileSync('go', ['run', './e2e/pagesfix', cmd, '1'], {
    cwd: repoRoot,
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_CONNECTOR_ENCRYPTION_KEY: KEY },
  }).toString();

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, pagesDeclutterPort, {
    MUSTER_PAGES: '1',
    MUSTER_PUBLIC_URL: origin,
    MUSTER_CONNECTOR_ENCRYPTION_KEY: KEY,
    // A link key is the connector key, and that one needs its listener.
    MUSTER_CONNECT_PORT: String(pagesDeclutterConnectPort),
    MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${pagesDeclutterConnectPort}`,
  });
  seeded = JSON.parse(fix('seed')) as Seeded;
  expect(seeded.token.length, 'the fixture made a live link').toBeGreaterThan(20);
});

test.afterAll(async () => {
  if (server) fix('clear');
  await server?.stop();
});

async function signedIn(browser: Browser, viewport: { width: number; height: number }): Promise<[BrowserContext, Page]> {
  const ctx = await browser.newContext({ viewport, baseURL: origin, hasTouch: viewport === PHONE, isMobile: viewport === PHONE });
  const page = await ctx.newPage();
  await signIn(page, origin);
  return [ctx, page];
}

// FIXTURE is every string e2e/pagesfix writes that a screen draws as the
// page's own: titles, the maker's name, the form's question and the answers.
// The word caps count CHROME only, so a title change cannot trip them.
const FIXTURE = [
  'Launch thread: approve the announcement text and the media before anything is posted Supercalifragilisticexpialidociouslylongunbrokenwordthatisfarwiderthanaphone',
  'Quarter plan', 'Launch notes', 'Venue survey', 'Budget review', 'Release helper', 'Which day suits you?',
  'Monday', 'Tuesday', 'Thursday', 'Friday',
];
// words is the number of chrome words a person reads in root's text: its
// innerText (a page's own frame is not in it) less every FIXTURE string.
const words = async (p: Page, root: string) => {
  let text = await p.locator(root).first().evaluate((el) => (el as HTMLElement).innerText);
  for (const f of FIXTURE) text = text.split(f).join(' ');
  return text.split(/\s+/).filter(Boolean).length;
};

async function shot(p: Page, name: string) {
  if (!shots) return;
  fs.mkdirSync(shots, { recursive: true });
  await p.screenshot({ path: path.join(shots, name + '.png') });
}

// plain asserts root's top layer spells no setup jargon and no clumsy count;
// CONTROL: it has words to read, and both patterns catch what they must.
async function plain(p: Page, root: string, what: string) {
  expect('by a · 0 live link(s), 0 waiting for you · 0 response(s)', 'CONTROL: the counts pattern sees the old row').toMatch(clumsyCounts);
  expect('MUSTER_CONNECTOR_ENCRYPTION_KEY is unset', 'CONTROL: the setup pattern sees a variable').toMatch(setupJargon);
  const text = await pageWords(p, root);
  expect(text.length, `CONTROL: ${what} has words to read`).toBeGreaterThan(20);
  expect(text, `${what} spells setup`).not.toMatch(setupJargon);
  expect(text, `${what} says a clumsy count`).not.toMatch(clumsyCounts);
}

// layout asserts the page does not scroll sideways and every visible target in
// root is at least 44px tall and wide.
async function layout(p: Page, root: string, what: string) {
  const over = await p.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  expect(over, `${what} scrolls sideways`).toBeLessThanOrEqual(0);
  const small = await p.locator(root).first().evaluate((el) => {
    const out: string[] = [];
    el.querySelectorAll('a, button, summary, input:not([type=hidden])').forEach((x) => {
      const r = x.getBoundingClientRect();
      if (r.width === 0 && r.height === 0) return; // not drawn (a closed menu's)
      if (r.height < 44 || r.width < 44) out.push(`${(x as HTMLElement).innerText || x.getAttribute('aria-label')}: ${Math.round(r.width)}×${Math.round(r.height)}`);
    });
    return out;
  });
  expect(small, `${what}: targets under 44px`).toEqual([]);
}

for (const [size, viewport] of sizes) {
  test(`the Pages list (${size}): plain, its badges only when they say something, a row opens the page`, async ({ browser }) => {
    const [ctx, page] = await signedIn(browser, viewport);
    try {
      await page.goto('/pages');
      await expect(page.locator(`li[data-page-id="${seeded.plan}"]`)).toBeVisible();
      await shot(page, `${size}-01-list`);
      const n = await words(page, 'main');
      console.log(`WORDS list ${size} ${n}`);
      await plain(page, 'main', 'the list');
      expect(n, 'the list says more than it needs').toBeLessThanOrEqual(CAPS.list);
      const badges = async (id: number) => page.locator(`li[data-page-id="${id}"] [data-page-badge]`).allInnerTexts();
      expect(await badges(seeded.plan), 'Quarter plan').toEqual(['v4']);
      expect(await badges(seeded.notes), 'Launch notes').toEqual(['Live', '3 replies']);
      expect(await badges(seeded.survey), 'Venue survey').toEqual(['Live', '2 waiting']);
      expect(await badges(seeded.budget), 'Budget review').toEqual(['v2', 'Live', '1 waiting', '1 reply']);
      expect(await badges(seeded.long), 'the long-titled page').toEqual([]);
      // Its number, who made it and when, in words in the reader's own zone.
      const meta = page.locator(`li[data-page-id="${seeded.plan}"] [data-page-meta]`);
      await expect(meta).toHaveText(when(`#${seeded.plan} · by Release helper · `));
      await expect(page.locator(`li[data-page-id="${seeded.budget}"] [data-page-meta]`)).toHaveText(when(`#${seeded.budget} · by Release helper · `));
      await layout(page, 'main', 'the list');
      if (size === 'desktop') {
        const w = (await page.locator('[data-testid="pages"]').boundingBox())!.width;
        expect(w, 'the desktop list is wide enough to read').toBeGreaterThanOrEqual(600);
      }
      // A tap anywhere on the row opens the page's own view.
      await Promise.all([page.waitForURL(`${origin}/pages/${seeded.budget}/view`), page.locator(`li[data-page-id="${seeded.budget}"] [data-page-meta]`).click()]);
    } finally {
      await ctx.close();
    }
  });
}

test("a row's ⋯ reaches Share and Details by keyboard, and Escape closes it", async ({ browser }) => {
  const [ctx, page] = await signedIn(browser, DESKTOP);
  try {
    await page.goto('/pages');
    const row = page.locator(`li[data-page-id="${seeded.notes}"]`);
    const more = row.getByRole('button', { name: 'More for “Launch notes”' });
    const menu = page.locator(`#page-menu-${seeded.notes}`);
    await more.focus();
    await page.keyboard.press('Enter');
    await expect(menu).toBeVisible();
    await shot(page, 'desktop-01b-row-menu');
    await page.keyboard.press('Tab');
    await expect(menu.getByRole('button', { name: 'Share “Launch notes”' })).toBeFocused();
    await page.keyboard.press('Tab');
    const details = menu.getByRole('link', { name: 'Details' });
    await expect(details).toBeFocused();
    // The live link's own actions are there too.
    await expect(menu.getByRole('button', { name: 'Revoke link' })).toBeVisible();
    await expect(menu.getByRole('button', { name: 'New link' })).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(menu).toBeHidden();
    await expect(more, 'Escape gives the ⋯ its focus back').toBeFocused();
    await page.keyboard.press('Enter');
    await expect(menu).toBeVisible();
    await page.keyboard.press('Tab');
    await page.keyboard.press('Tab');
    await expect(details).toBeFocused();
    await page.keyboard.press('Enter');
    await page.waitForURL(`${origin}/pages/${seeded.notes}`, { timeout: 10_000 });
    // A page with no link has no link actions to offer.
    await page.goto('/pages');
    await page.locator(`li[data-page-id="${seeded.plan}"]`).getByRole('button', { name: 'More for “Quarter plan”' }).click();
    const bare = page.locator(`#page-menu-${seeded.plan}`);
    await expect(bare.getByRole('button', { name: 'Share “Quarter plan”' })).toBeVisible();
    await expect(bare.getByRole('button', { name: 'Revoke link' })).toHaveCount(0);
  } finally {
    await ctx.close();
  }
});

for (const [size, viewport] of sizes) {
  test(`a page's screen (${size}): plain, links and Revoke only when there is one, versions newest first`, async ({ browser }) => {
    const [ctx, page] = await signedIn(browser, viewport);
    try {
      // A page with no link and one version: no Links, no Versions, no ⋯.
      await page.goto(`/pages/${seeded.long}`);
      await expect(page.locator('[data-page-title]')).toBeVisible();
      await expect(page.locator('[data-page-view]'), 'Open').toHaveText('Open');
      await expect(page.locator('[data-testid="page-links"]')).toHaveCount(0);
      await expect(page.locator('[data-testid="page-versions"]')).toHaveCount(0);
      await expect(page.getByRole('button', { name: /^Link options for/ })).toHaveCount(0);
      await expect(page.getByRole('button', { name: 'Revoke link' })).toHaveCount(0);
      await plain(page, 'main', 'a bare page');
      await layout(page, 'main', 'a bare page');

      // A page with a live link, a waiting one, a reply and two versions.
      await page.goto(`/pages/${seeded.budget}`);
      await expect(page.locator('[data-testid="page-links"]')).toBeVisible();
      await shot(page, `${size}-02-detail`);
      const n = await words(page, 'main');
      console.log(`WORDS detail ${size} ${n}`);
      await plain(page, 'main', "the page's screen");
      expect(n, "the page's screen says more than it needs").toBeLessThanOrEqual(CAPS.detail);
      await expect(page.locator('[data-page-meta]')).toHaveText('by Release helper · v2');
      // The Share link's row copies it, on a desktop as on a phone; no link
      // or version row has an ⓘ, and nothing beside Share holds the address.
      await expect(page.locator('[data-link-state="live"] [data-link-copy]')).toBeVisible();
      await expect(page.locator('[data-link-state="live"] [data-link-copy]')).toHaveText('Copy');
      await expect(page.locator('li[data-link-id] [data-info-open], li[data-version] [data-info-open]')).toHaveCount(0);
      await expect(page.getByRole('button', { name: "This page's link" })).toHaveCount(0);
      await expect(page.locator('[data-page-copy]')).toHaveCount(0);
      // A version's exact time and size are its time's tooltip.
      await expect(page.locator('[data-version="1"] time')).toHaveAttribute('title', /^[A-Z][a-z]{2} \d{1,2}, \d{4}, \d{1,2}:\d{2}\s[AP]M · [0-9.]+ [kKMG]?B$/);
      await expect(page.locator('[data-link-state="live"] [data-link-revoke]')).toHaveText('Revoke');
      await page.getByRole('button', { name: 'Link options for “Budget review”' }).click();
      await expect(page.getByRole('button', { name: 'Revoke link' })).toBeVisible();
      await shot(page, `${size}-02c-actions`);
      await page.keyboard.press('Escape');
      // Newest first; each row says its version, current, and when.
      await expect(page.locator('[data-testid="page-versions"] [data-version-view]')).toHaveText([when('v2 · current · '), when('v1 · ')]);
      await layout(page, 'main', "the page's screen");
      await Promise.all([page.waitForURL(`${origin}/pages/${seeded.budget}/view?v=1`), page.locator('[data-version="1"] [data-version-view]').click()]);
      await expect(page.locator('[data-version-chip]')).toHaveText(/v1/);
    } finally {
      await ctx.close();
    }
  });
}

for (const [size, viewport] of sizes) {
  test(`a long title wraps whole words and never overflows (${size})`, async ({ browser }) => {
    const [ctx, page] = await signedIn(browser, viewport);
    try {
      await page.goto(`/pages/${seeded.long}`);
      const h1 = page.locator('h1[data-page-title]');
      await expect(h1).toBeVisible();
      const style = await h1.evaluate((el) => ({ wb: getComputedStyle(el).wordBreak, ow: getComputedStyle(el).overflowWrap }));
      expect(style.wb, 'word-break').not.toBe('break-all');
      expect(['break-word', 'anywhere'], 'overflow-wrap').toContain(style.ow);
      // Each word that fits on a line is drawn on ONE line (a word split across
      // two lines has two boxes); the one unbroken run longer than a line may
      // break — but stays inside the title's box.
      const { split, broke } = await h1.evaluate((el) => {
        const node = el.firstChild as Text;
        const text = node.data;
        const split: string[] = [];
        const broke: string[] = [];
        const width = el.clientWidth;
        // A word's own width, on one line, in the title's font.
        const probe = document.createElement('span');
        probe.style.whiteSpace = 'nowrap';
        el.appendChild(probe);
        let at = 0;
        for (const w of text.split(' ')) {
          const r = document.createRange();
          r.setStart(node, at);
          r.setEnd(node, at + w.length);
          at += w.length + 1;
          probe.textContent = w;
          const fits = probe.getBoundingClientRect().width <= width;
          const lines = new Set(Array.from(r.getClientRects()).map((b) => Math.round(b.top))).size;
          if (lines > 1) (fits ? split : broke).push(w);
        }
        probe.remove();
        return { split, broke };
      });
      expect(split, 'words that fit on a line, split across two').toEqual([]);
      // CONTROL: the run wider than a line is the one thing that breaks — the
      // measure sees a break when there is one.
      expect(broke, 'the unbroken run breaks to stay inside').toEqual([await h1.evaluate((el) => (el.textContent ?? '').split(' ').pop())]);
      const box = await h1.evaluate((el) => ({ sw: el.scrollWidth, cw: el.clientWidth }));
      expect(box.sw, 'the title overflows its box').toBeLessThanOrEqual(box.cw);
      await layout(page, 'main', 'the long-titled page');
    } finally {
      await ctx.close();
    }
  });
}

for (const [size, viewport] of sizes) {
  test(`the owner's version popover says when, in words (${size})`, async ({ browser }) => {
    const [ctx, page] = await signedIn(browser, viewport);
    try {
      await page.goto(`/pages/${seeded.plan}/view`);
      await page.locator('[data-version-chip]').click();
      const pop = page.locator('#page-versions');
      await expect(pop).toBeVisible();
      await shot(page, `${size}-03b-versions`);
      const n = await words(page, '#page-versions');
      console.log(`WORDS versions ${size} ${n}`);
      expect(n, 'the popover says more than it needs').toBeLessThanOrEqual(CAPS.versions);
      await expect(pop.locator('[data-version-link] .w')).toHaveText([when('current · '), when(''), when(''), when('')]);
      await plain(page, '#page-versions', 'the version popover');
    } finally {
      await ctx.close();
    }
  });
}

for (const [size, viewport] of sizes) {
  test(`a link's view: the brand, the title, the page — nothing else (${size})`, async ({ browser }) => {
    const ctx = await browser.newContext({ viewport, hasTouch: viewport === PHONE, isMobile: viewport === PHONE });
    try {
      const page = await ctx.newPage();
      await page.goto(`${origin}/s/${seeded.token}`);
      const frame = page.frameLocator('iframe#page');
      await expect(frame.locator('main')).toContainText('The first version.');
      await shot(page, `${size}-04-link`);
      const n = await words(page, 'body');
      console.log(`WORDS link ${size} ${n}`);
      expect(n, "a link's view says more than it needs").toBeLessThanOrEqual(CAPS.link);
      await plain(page, 'body', "a link's view");
      await layout(page, 'header', "a link's header");
    } finally {
      await ctx.close();
    }
  });
}

// THE OWNER'S HEADER (internal/pages render.go Shell; docs/pages.md "The
// owner's view"): "Pages ›" in the brand name's place, a link to the Pages
// screen; the title a button whose popover holds the whole title wrapped,
// "Page #<id> · v<n> · updated <when>" and Copy ID; Share at the header's
// end. At 390px a title far wider than the phone stays one row, cut short,
// with nothing past the screen's edge.
const press = (viewport: { width: number }, l: Locator) => (viewport === PHONE ? l.tap() : l.click());
const noShareSheet = () => {
  try {
    delete (Navigator.prototype as unknown as { share?: unknown }).share;
  } catch {
    /* none to delete */
  }
};

for (const [size, viewport] of sizes) {
  test(`the owner's header (${size}): Pages ›, the title's popover with its id and Copy ID, one row at any title`, async ({ browser }) => {
    const ctx = await browser.newContext({ viewport, baseURL: origin, hasTouch: viewport === PHONE, isMobile: viewport === PHONE,
      permissions: ['clipboard-read', 'clipboard-write'] });
    try {
      await ctx.addInitScript(noShareSheet);
      const page = await ctx.newPage();
      await signIn(page, origin);
      await page.goto(`/pages/${seeded.long}/view`);
      const header = page.locator('header[data-page-brand]');
      const crumb = header.locator('a[data-page-crumb]');
      await expect(crumb).toHaveText('Pages');
      await expect(crumb).toHaveAttribute('href', '/pages');
      await expect(header.locator('[data-brand-name]'), 'the brand name is drawn beside the crumbs').toHaveCount(0);
      await expect(header.getByRole('button', { name: `Share “${FIXTURE[0]}”` })).toBeVisible();
      await shot(page, `${size}-05-owner-header`);

      // 1. One row; the title cut short but there; nothing past the edge.
      const fit = await header.evaluate((h) => {
        const t = h.querySelector('[data-page-title]') as HTMLElement;
        const items = Array.from(h.querySelectorAll('img, .cr, .sep, [data-page-title], .sess, .chip, form>button')).map((e) => e.getBoundingClientRect());
        return {
          doc: document.documentElement.scrollWidth - window.innerWidth,
          past: items.filter((r) => r.width > 0 && (r.left < 0 || r.right > window.innerWidth + 0.5)).length,
          rows: new Set(items.filter((r) => r.width > 0).map((r) => Math.round(r.top + r.height / 2))).size <= 2,
          height: h.getBoundingClientRect().height,
          titleW: t.getBoundingClientRect().width,
          cut: t.scrollWidth > t.clientWidth,
        };
      });
      expect(fit.doc, 'the page scrolls sideways').toBeLessThanOrEqual(0);
      expect(fit.past, 'a header item past the screen').toBe(0);
      expect(fit.height, 'the header is one slim row').toBeLessThanOrEqual(44);
      expect(fit.titleW, 'the title is squeezed out').toBeGreaterThanOrEqual(48);
      expect(fit.cut, 'the long title is cut short').toBe(true);

      // 2. The title opens its popover: the whole title, wrapped; the id,
      // version and when; Copy ID copies the id.
      const title = header.locator('[data-page-title]');
      const about = page.locator('#page-about');
      await expect(title).toHaveAttribute('aria-expanded', 'false');
      await press(viewport, title);
      await expect(about).toBeVisible();
      await expect(title).toHaveAttribute('aria-expanded', 'true');
      await expect(about.locator('[data-page-about-title]')).toHaveText(FIXTURE[0]);
      const wrap = await about.locator('[data-page-about-title]').evaluate((e) => ({ sw: e.scrollWidth, cw: e.clientWidth, h: e.getBoundingClientRect().height }));
      expect(wrap.sw, 'the full title overflows the popover').toBeLessThanOrEqual(wrap.cw);
      expect(wrap.h, 'the full title is not wrapped onto several lines').toBeGreaterThan(30);
      await expect(about.locator('[data-page-about-meta]')).toHaveText(new RegExp(`^Page #${seeded.long} · v1 · updated ${WHEN}$`));
      const pb = (await about.boundingBox())!;
      if (viewport === PHONE) {
        expect(Math.round(pb.y + pb.height), 'on a phone the popover is a bottom sheet').toBe(viewport.height);
        expect(Math.round(pb.width)).toBe(viewport.width);
      } else {
        expect(Math.round(pb.y), 'on a desktop the popover hangs under the header').toBe(44);
        expect(Math.round(pb.x), 'on a desktop the popover is under the title, at the left').toBe(8);
      }
      await shot(page, `${size}-06-title-popover`);
      const copy = about.getByRole('button', { name: 'Copy ID' });
      await expect(copy).toBeFocused();
      expect((await copy.boundingBox())!.height, 'Copy ID is at least 44px').toBeGreaterThanOrEqual(44);
      await page.evaluate(() => navigator.clipboard.writeText('cleared'));
      await press(viewport, copy);
      await expect(page.locator('[data-page-toast]').filter({ hasText: 'ID copied' })).toBeVisible();
      await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(String(seeded.long));
      await page.keyboard.press('Escape');
      await expect(about).toBeHidden();
      await expect(title).toHaveAttribute('aria-expanded', 'false');
      await expect(title).toBeFocused();

      // 3. One popover at a time: the title's opening closes the chip's.
      // A tap outside (the scrim) closes it.
      const chip = header.locator('[data-version-chip]');
      await press(viewport, chip);
      await expect(page.locator('#page-versions')).toBeVisible();
      await title.dispatchEvent('click');
      await expect(about).toBeVisible();
      await expect(page.locator('#page-versions')).toBeHidden();
      await expect(chip).toHaveAttribute('aria-expanded', 'false');
      await page.mouse.click(viewport.width - 12, 120);
      await expect(about).toBeHidden();
      await expect(title).toHaveAttribute('aria-expanded', 'false');

      // 4. Pages › goes to the Pages screen.
      await Promise.all([page.waitForURL(`${origin}/pages`), press(viewport, crumb)]);
      await expect(page.locator(`li[data-page-id="${seeded.long}"]`)).toBeVisible();
    } finally {
      await ctx.close();
    }
  });

  test(`the owner's header Share (${size}): copies the page's link with a toast, or hands it to the share sheet`, async ({ browser }) => {
    // Launch notes already has its live link: Share hands out that one.
    const name = `Share “${FIXTURE[2]}”`;
    const copying = await browser.newContext({ viewport, baseURL: origin, hasTouch: viewport === PHONE, isMobile: viewport === PHONE,
      permissions: ['clipboard-read', 'clipboard-write'] });
    const sharing = await browser.newContext({ viewport, baseURL: origin, hasTouch: viewport === PHONE, isMobile: viewport === PHONE });
    try {
      // No share sheet: the link goes on the clipboard, and a toast says so.
      await copying.addInitScript(noShareSheet);
      const page = await copying.newPage();
      await signIn(page, origin);
      const link = (await (await page.request.post(origin + `/pages/${seeded.notes}/share`, { headers: { Origin: origin, Accept: 'application/json' } })).json()).url as string;
      expect(link).toMatch(new RegExp(`^${origin}/s/[A-Za-z0-9_-]+$`));
      await page.goto(`/pages/${seeded.notes}/view`);
      const share = page.locator('header').getByRole('button', { name });
      await page.evaluate(() => navigator.clipboard.writeText('cleared'));
      await press(viewport, share);
      await expect(page.locator('[data-page-toast]').filter({ hasText: 'Link copied' })).toBeVisible();
      await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(link);
      await shot(page, `${size}-07-share-copied`);

      // A share sheet (stubbed): called with the title and the same link.
      await sharing.addInitScript(() => {
        const w = window as unknown as { __shared: unknown[] };
        w.__shared = [];
        Object.defineProperty(Navigator.prototype, 'share', { configurable: true, value: (d: unknown) => (w.__shared.push(d), Promise.resolve()) });
        Object.defineProperty(Navigator.prototype, 'canShare', { configurable: true, value: () => true });
      });
      const sp = await sharing.newPage();
      await signIn(sp, origin);
      await sp.goto(`/pages/${seeded.notes}/view`);
      await press(viewport, sp.locator('header').getByRole('button', { name }));
      await expect.poll(() => sp.evaluate(() => (window as unknown as { __shared: unknown[] }).__shared.length)).toBe(1);
      expect(await sp.evaluate(() => (window as unknown as { __shared: unknown[] }).__shared[0])).toEqual({ title: FIXTURE[2], url: link });
      await expect(sp.locator('[data-page-toast]')).toHaveCount(0);
    } finally {
      await copying.close();
      await sharing.close();
    }
  });
}

// Past links fold under their count: the survey page has 1 live link and 2
// waiting, listed, and 2 revoked and 1 declined under "Past links (3)",
// whose rows show once it is opened.
test('a page’s past links are folded under “Past links (3)”', async ({ browser }) => {
  const [ctx, page] = await signedIn(browser, PHONE);
  try {
    await page.goto(`/pages/${seeded.survey}`);
    await expect(page.locator('[data-testid="page-links"] li')).toHaveCount(3);
    const fold = page.locator('[data-page-links-past-summary]');
    await expect(fold).toHaveText('Past links (3)');
    const past = page.locator('[data-testid="page-links-past"] li');
    await expect(past.first()).toBeHidden();
    await fold.click();
    await expect(past).toHaveCount(3);
    await expect(past.first()).toBeVisible();
    await expect(page.locator('[data-testid="page-links-past"] [data-link-state="revoked"]')).toHaveCount(2);
    await expect(page.locator('[data-testid="page-links-past"] [data-link-state="denied"]')).toHaveCount(1);
  } finally {
    await ctx.close();
  }
});

// Delete asks in place: one tap on "Delete this page" deletes nothing and
// turns into the question; Escape and Cancel put it back; Delete deletes.
// (Last: it deletes the survey page.)
test('Delete asks in place: one tap deletes nothing, Delete deletes', async ({ browser }) => {
  const [ctx, page] = await signedIn(browser, PHONE);
  try {
    const at = `/pages/${seeded.survey}`;
    await page.goto(at);
    const ask = page.getByRole('button', { name: 'Delete this page', exact: true });
    const question = page.getByText('Delete this page and its responses?');
    await ask.click();
    await expect(question).toBeVisible();
    await expect(ask).toBeHidden();
    expect((await page.request.get(origin + at)).status(), 'one tap deleted the page').toBe(200);
    await page.keyboard.press('Escape');
    await expect(question).toBeHidden();
    await expect(ask).toBeVisible();
    await ask.click();
    await page.getByRole('button', { name: 'Cancel', exact: true }).click();
    await expect(question).toBeHidden();
    await expect(ask).toBeFocused();
    expect((await page.request.get(origin + at)).status(), 'Cancel deleted the page').toBe(200);
    await ask.click();
    await page.getByRole('button', { name: 'Delete', exact: true }).click();
    await page.waitForURL((u) => u.pathname !== at);
    expect((await page.request.get(origin + at)).status(), 'Delete left the page').toBe(404);
  } finally {
    await ctx.close();
  }
});
