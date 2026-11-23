import { execFileSync } from 'node:child_process';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { dsn, paletteEverythingPort, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// THE COMMAND PALETTE SEARCHES EVERYTHING (internal/ui palette.go and
// nav_pages.go, internal/api palette_find.go), in a real browser against a
// real muster-server, at a phone (390×844, touch) and a desktop (1280×800):
//
//   - typing a screen's name ("pages", "asks") lists that screen of the app,
//     and Enter opens it;
//   - typing an ask's id ("N", "#N") lists that ask first, and Enter opens
//     its page; a Pages document is found by its title;
//   - the type pills: All by default, each time the palette opens; Asks lists
//     only asks, Pages only documents, Sessions only sessions; the keyboard
//     reaches them (Tab, the arrows);
//   - the session search still finds a session by its name;
//   - nothing found says "No matches";
//   - on the phone the input is BELOW the results (and the pills between),
//     the best match nearest the input; on the desktop it is above them.
//
// The asks are the Asks feed's fixture rows (e2e/asksfeedfix), the documents
// pages written through the real store (e2e/pagesfix titled).
//
// 🔴 REGRESSION: on origin/main the palette searches sessions only — no
// screen, ask or page is ever listed, there are no pills, and the input is at
// the top at every width — so every test here is red there.

const origin = `http://localhost:${paletteEverythingPort}`;
const stamp = Date.now().toString(36);
const agent = 'e2e-pal-' + stamp;
// Two documents: one found by its own words, one sharing a word with the
// fixture's asks ("staging") so a pill has two kinds to narrow.
const DOC = `Sandpiper review ${stamp}`;
const SHARED = `Staging cutover ${stamp}`;
type Ids = { deploy: number; freetext: number; rotate: number; notice: number; answered: number; draft: number; task: number };
let ids: Ids;
let doc = 0;
let shared = 0;
let server: Running | undefined;

const feedFix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/asksfeedfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();
const pagesFix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pagesfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, paletteEverythingPort, { MUSTER_PAGES: '1', MUSTER_PLUGINS: '1', MUSTER_TASK_ROUTING: '1', MUSTER_FOCUS_INTERVAL: '0' });
  ids = JSON.parse(feedFix('seed', '1', agent)) as Ids;
  doc = Number(pagesFix('titled', '1', DOC));
  shared = Number(pagesFix('titled', '1', SHARED));
  expect(doc, 'pagesfix printed the page id').toBeGreaterThan(0);
  expect(shared, 'pagesfix printed the page id').toBeGreaterThan(0);
});

test.afterAll(async () => {
  try {
    feedFix('clear', '1', agent);
    pagesFix('titled-clear', '1', DOC);
    pagesFix('titled-clear', '1', SHARED);
  } finally {
    await server?.stop();
  }
});

const pal = (page: Page) => page.locator('#switcher-search');
const input = (page: Page) => page.locator('#switcher-search-input');
const list = (page: Page) => page.locator('#switcher-search-results');
const options = (page: Page) => page.locator('#switcher-search-results [role="option"]');
const pill = (page: Page, name: string) => page.locator('#switcher-search-types').getByRole('radio', { name });
const types = (page: Page) => options(page).evaluateAll((els) => els.map((e) => e.getAttribute('data-type')));

for (const vp of [
  { name: 'phone 390×844', phone: true, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', phone: false, use: { viewport: { width: 1280, height: 800 } } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    // open signs in, goes to the Tasks screen and opens the palette from the
    // sidebar's magnifier (on a phone, the sidebar opened first).
    async function open(page: Page) {
      await signIn(page, origin);
      await page.goto(origin + '/tasks');
      if (vp.phone) {
        await page.locator('#sidebar-open').click();
        await expect(page.locator('#sidebar-root')).toHaveAttribute('data-open', 'true');
      }
      await page.locator('#sidebar-search').click();
      await expect(pal(page)).toBeVisible();
      await expect(input(page)).toBeFocused();
    }

    test('a screen is found by its name, and Enter opens it', async ({ page }) => {
      await open(page);
      await input(page).pressSequentially('pages');
      const first = options(page).first();
      await expect(first).toHaveAttribute('data-type', 'screen');
      await expect(first).toHaveAttribute('data-href', '/pages');
      await expect(first.locator('[data-palette-kind]')).toHaveText('Screen · /pages');
      await expect(first).toHaveAttribute('aria-selected', 'true');
      await input(page).press('Enter');
      await expect(page).toHaveURL(origin + '/pages');

      await page.goto(origin + '/tasks');
      if (vp.phone) await page.locator('#sidebar-open').click();
      await page.locator('#sidebar-search').click();
      await input(page).pressSequentially('asks');
      await expect(options(page).first()).toHaveAttribute('data-href', '/attention');
      await expect(options(page).first().locator('span').first()).toHaveText('Asks');
    });

    test("an ask's id lists that ask first, and Enter opens its page", async ({ page }) => {
      await open(page);
      const n = String(ids.rotate);
      await input(page).pressSequentially(n);
      const first = options(page).first();
      await expect(first).toHaveAttribute('data-type', 'ask');
      await expect(first).toHaveAttribute('data-ask-id', n);
      await expect(first).toHaveAttribute('data-href', `/attention/asks/${n}`);
      await expect(first.locator('span').first()).toHaveText('Rotate the staging key?');
      await expect(first.locator('[data-palette-kind]')).toHaveText(`Ask #${n} · Open`);
      await input(page).press('Enter');
      await expect(page).toHaveURL(`${origin}/attention/asks/${n}`);

      // "#N" is an id too.
      await page.goBack();
      if (vp.phone && (await page.locator('#sidebar-root').getAttribute('data-open')) !== 'true') await page.locator('#sidebar-open').click();
      await page.locator('#sidebar-search').click();
      await input(page).pressSequentially(`#${ids.deploy}`);
      await expect(options(page).first()).toHaveAttribute('data-ask-id', String(ids.deploy));
    });

    test('a Pages document is found by its title', async ({ page }) => {
      await open(page);
      await input(page).pressSequentially('sandpiper rev');
      const hit = page.locator(`#switcher-search-results [role="option"][data-type="page"][data-href="/pages/${doc}"]`);
      await expect(hit).toBeVisible();
      await expect(hit.locator('span').first()).toHaveText(DOC);
      await expect(hit.locator('[data-palette-kind]')).toHaveText(`Page #${doc}`);
      await hit.click();
      await expect(page).toHaveURL(`${origin}/pages/${doc}`);
    });

    test('the pills narrow the results to one kind; All is the default each time', async ({ page }) => {
      await open(page);
      await expect(pill(page, 'All')).toHaveAttribute('aria-checked', 'true');
      await input(page).pressSequentially('staging');
      // All: the fixture's two staging asks AND the document.
      await expect(page.locator(`#switcher-search-results [data-ask-id="${ids.rotate}"]`)).toBeVisible();
      await expect(page.locator(`#switcher-search-results [data-page-id="${shared}"]`)).toBeVisible();
      await expect(page.locator(`#switcher-search-results [data-ask-id="${ids.deploy}"]`)).toBeVisible();

      await pill(page, 'Asks').click();
      await expect(pill(page, 'Asks')).toHaveAttribute('aria-checked', 'true');
      await expect(pill(page, 'All')).toHaveAttribute('aria-checked', 'false');
      await expect(page.locator(`#switcher-search-results [data-page-id="${shared}"]`)).toHaveCount(0);
      await expect(page.locator(`#switcher-search-results [data-ask-id="${ids.rotate}"]`)).toBeVisible();
      expect(new Set(await types(page))).toEqual(new Set(['ask']));
      // A tap on a pill keeps the input focused (a phone's keyboard stays up).
      await expect(input(page)).toBeFocused();

      await pill(page, 'Pages').click();
      await expect(page.locator(`#switcher-search-results [data-page-id="${shared}"]`)).toBeVisible();
      expect(new Set(await types(page))).toEqual(new Set(['page']));

      // The keyboard: Tab from the input reaches the checked pill, the arrows
      // move the choice, Tab goes back to the input.
      await input(page).press('Tab');
      await expect(pill(page, 'Pages')).toBeFocused();
      await page.keyboard.press('ArrowLeft');
      await expect(pill(page, 'Asks')).toBeFocused();
      await expect(pill(page, 'Asks')).toHaveAttribute('aria-checked', 'true');
      expect(new Set(await types(page))).toEqual(new Set(['ask']));
      await page.keyboard.press('Tab');
      await expect(input(page)).toBeFocused();

      // Closed and opened again: All, and the box empty.
      await page.keyboard.press('Escape');
      await expect(pal(page)).toBeHidden();
      if (vp.phone && (await page.locator('#sidebar-root').getAttribute('data-open')) !== 'true') await page.locator('#sidebar-open').click();
      await page.locator('#sidebar-search').click();
      await expect(pill(page, 'All')).toHaveAttribute('aria-checked', 'true');
      await expect(pill(page, 'Asks')).toHaveAttribute('aria-checked', 'false');
      await expect(input(page)).toHaveValue('');
    });

    test('the session search still finds a session; nothing found says "No matches"', async ({ page }) => {
      await open(page);
      await input(page).pressSequentially(agent);
      const first = options(page).first();
      await expect(first).toHaveAttribute('data-type', 'session');
      await expect(first).toHaveAttribute('data-kind', 'agent');
      await expect(first.locator('span').first()).toHaveText(agent);
      await pill(page, 'Sessions').click();
      expect(new Set(await types(page))).toEqual(new Set(['session']));
      await pill(page, 'Screens').click();
      await expect(options(page)).toHaveCount(0);
      await expect(pal(page).locator('[data-switcher-search-empty]')).toHaveText('No matches');
      await expect(pal(page).locator('[data-switcher-search-empty]')).toBeVisible();
    });

    // THE KEYBOARD (as switcher-dock.spec.ts 13k emulates it): where a soft
    // keyboard shrinks only the VISUAL viewport, the phone's box sits on top
    // of it, so the input being typed into stays in sight.
    if (vp.phone) test('with the visual viewport shortened by a keyboard, the box sits on top of it', async ({ page }) => {
      await page.addInitScript(() => {
        const real = window.visualViewport!;
        const fake = new EventTarget() as any;
        for (const k of ['width', 'offsetLeft', 'pageLeft', 'pageTop', 'scale']) Object.defineProperty(fake, k, { get: () => (real as any)[k] });
        Object.defineProperty(fake, 'height', { get: () => (window as any).__vvH ?? real.height });
        Object.defineProperty(fake, 'offsetTop', { get: () => 0 });
        real.addEventListener('resize', () => fake.dispatchEvent(new Event('resize')));
        Object.defineProperty(window, 'visualViewport', { value: fake, configurable: true });
      });
      await open(page);
      await expect(pal(page)).toHaveCSS('transform', 'none');
      const inner = await page.evaluate(() => window.innerHeight);
      const before = (await pal(page).boundingBox())!;
      expect(before.y + before.height, 'CONTROL: no keyboard, the box is on the bottom edge').toBeGreaterThan(inner - 48);
      await page.evaluate(() => { (window as any).__vvH = 420; window.visualViewport!.dispatchEvent(new Event('resize')); });
      await expect.poll(async () => { const b = (await pal(page).boundingBox())!; return Math.round(b.y + b.height); }, 'the box’s foot is just above the keyboard').toBeLessThanOrEqual(420);
      const b = (await pal(page).boundingBox())!;
      expect(b.y + b.height, 'and close to it, not far above').toBeGreaterThan(420 - 48);
      const inp = (await input(page).boundingBox())!;
      expect(inp.y >= 0 && inp.y + inp.height <= 420, 'the input is above the keyboard').toBe(true);
      // The keyboard gone: back on the bottom edge.
      await page.evaluate(() => { delete (window as any).__vvH; window.visualViewport!.dispatchEvent(new Event('resize')); });
      await expect.poll(async () => { const b2 = (await pal(page).boundingBox())!; return b2.y + b2.height; }).toBeGreaterThan(inner - 48);
    });

    test(vp.phone ? 'the input is below the results, the pills between, the best match nearest' : 'the input is above the pills and the results', async ({ page }) => {
      await open(page);
      // "a": many sessions and screens, so the list has several rows.
      await input(page).pressSequentially('a');
      await expect(options(page).nth(1)).toBeVisible();
      // Measured once the opening animation (input.css) has settled.
      await expect(pal(page)).toHaveCSS('transform', 'none');
      await expect(pal(page)).toHaveCSS('opacity', '1');
      const vh = vp.use.viewport.height;
      // 🔴 EVERY BOX FROM ONE SNAPSHOT, TAKEN IN ONE TASK OF THE PAGE. The
      // palette redraws its list WHOLE (render(): the old rows dropped, new
      // ones built) each time something lands after the keystroke — the find
      // answer, DEBOUNCE ms on, and on a phone the sidebar's agent rows (htmx,
      // fetched as the drawer opens). A locator's boundingBox() finds its
      // element and measures it in separate round trips, so a redraw between
      // the two measured a row already dropped: null, and CI's
      // "Cannot read properties of null (reading 'y')" on the rows' line.
      // Measured here in one evaluate, no redraw can fall between two boxes,
      // and every box is of the same list. A missing element is reported by
      // name, not as a TypeError.
      type Box = { x: number; y: number; width: number; height: number };
      const snap = await page.evaluate(() => {
        const box = (e: Element | null): Box | null => {
          if (!e || !e.isConnected || !e.getClientRects().length) return null;
          const r = e.getBoundingClientRect();
          return { x: r.x, y: r.y, width: r.width, height: r.height };
        };
        const q = (s: string) => box(document.querySelector(s));
        const opts = document.querySelectorAll('#switcher-search-results [role="option"]');
        return {
          inp: q('#switcher-search-input'),
          res: q('#switcher-search-results'),
          field: q('[data-palette-field]'),
          pills: q('#switcher-search-types'),
          o0: box(opts[0] ?? null),
          o1: box(opts[1] ?? null),
          box: q('#switcher-search'),
        };
      });
      const laid = (name: keyof typeof snap): Box => {
        const b = snap[name];
        expect(b, `${name} is on the page and laid out`).not.toBeNull();
        expect(b!.height, `${name} has a height`).toBeGreaterThan(0);
        return b!;
      };
      // The input and the results first: they are on the page at any build.
      const inp = laid('inp');
      const res = laid('res');
      if (vp.phone) expect(inp.y, 'the input is below the results').toBeGreaterThanOrEqual(res.y + res.height - 0.5);
      else expect(inp.y + inp.height, 'the input is above the results').toBeLessThanOrEqual(res.y + 0.5);
      const [field, pills, o0, o1, box] = (['field', 'pills', 'o0', 'o1', 'box'] as const).map(laid);
      if (vp.phone) {
        expect(res.y + res.height, 'the results end above the pills').toBeLessThanOrEqual(pills.y + 0.5);
        expect(pills.y + pills.height, 'the pills end above the input').toBeLessThanOrEqual(field.y + 0.5);
        expect(o0.y, 'the best match is the row nearest the input').toBeGreaterThan(o1.y);
        expect(box.y + box.height, 'the box sits on the bottom edge').toBeGreaterThan(vh - 48);
      } else {
        expect(field.y + field.height, 'the input ends above the pills').toBeLessThanOrEqual(pills.y + 0.5);
        expect(pills.y + pills.height, 'the pills end above the results').toBeLessThanOrEqual(res.y + 0.5);
        expect(o0.y, 'the best match is the top row').toBeLessThan(o1.y);
        expect(box.y, 'the box is near the top').toBeLessThan(vh / 3);
      }
      // The arrows move to the next match in the direction it is drawn.
      await expect(options(page).nth(0)).toHaveAttribute('aria-selected', 'true');
      await input(page).press(vp.phone ? 'ArrowUp' : 'ArrowDown');
      await expect(options(page).nth(1)).toHaveAttribute('aria-selected', 'true');
    });

    // 🔴 REGRESSION: the sidebar's agent rows landing (htmx, nav_agents.go)
    // redraw the open palette's list, and that redraw put the highlight back
    // on the first row — so a highlight the operator had moved jumped back
    // under them (and Enter opened the wrong result). The rows land on the
    // drawer's open, on agent and attention events, and every 60s. A server
    // answer already kept the highlight; now the sidebar's rows do too.
    test('the sidebar’s rows landing keep the highlighted result', async ({ page }) => {
      await open(page);
      await input(page).pressSequentially('a');
      await expect(options(page).nth(1)).toBeVisible();
      await input(page).press(vp.phone ? 'ArrowUp' : 'ArrowDown');
      await expect(options(page).nth(1)).toHaveAttribute('aria-selected', 'true');
      const href = await options(page).nth(1).getAttribute('data-href');
      expect(href, 'the second row has a target').toBeTruthy();
      // The sidebar asked for its rows again, and the swap awaited — the
      // palette's own afterSwap listener was bound first, so it has run.
      const swapped = await page.evaluate(() => new Promise<number>((done) => {
        const old = document.querySelector('#switcher-search-results [role="option"]');
        document.addEventListener('htmx:afterSwap', function on(e: Event) {
          const t = (e as CustomEvent).detail?.target as Element | undefined;
          if (t?.id !== 'sidebar-agents-list') return;
          document.removeEventListener('htmx:afterSwap', on);
          setTimeout(() => done(old && !old.isConnected ? 1 : 0), 0);
        });
        document.dispatchEvent(new Event('muster:nav-open'));
      }));
      expect(swapped, 'CONTROL: the swap redrew the list (its old first row is gone)').toBe(1);
      const sel = page.locator('#switcher-search-results [role="option"][aria-selected="true"]');
      await expect(sel).toHaveCount(1);
      await expect(sel).toHaveAttribute('data-href', href!);
    });
  });
}
