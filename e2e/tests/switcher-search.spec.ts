import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { switcherSearchPort } from '../lib/env';
import { clsInit, readCls, resetCls, settle } from '../lib/cls';
import { cc, MirrorRig, mkchat } from '../lib/mirrorhost';

// THE SESSION SWITCHER'S VIEW MENU AND SEARCH (internal/ui/switcher.go), on a
// REAL muster-server and a REAL muster-mirror (`-tags e2emirror`, which fakes
// tmux only) whose windows carry codenames — the "pretty" tmux names the
// operator searches by — plus one dispatched agent whose TITLE (display name)
// holds a codename's letters:
//
//   1. the button on the bar's left opens a menu of the views, animated, opening
//      UPWARD from the bar; a pick shows that view and is kept across loads;
//      the keyboard works it (arrows, Enter, Escape) and a tap outside closes it;
//   2. a search (the magnifier, or "/" outside a text field — never Ctrl/Cmd+K,
//      which is the browser's own) opens a command-palette box
//      whose results fill as the operator types: codename prefix first, then
//      codename substring, then title; arrows move, Enter opens, Escape closes;
//   3. opening either moves nothing on the page (geometry and layout shift);
//
// each at a phone (390×844, touch) and a desktop (1280×800) size.
//
// 🔴 REGRESSION TESTS: at the base (7856a792) the left button CYCLES the views
// (no menu) and there is no search at all.
test.use({ storageState: { cookies: [], origins: [] } });

const rig = new MirrorRig(switcherSearchPort, 'e2e-sws', {}, { decimalHost: true });
const origin = rig.origin;
// 🔴 DECIMAL, like the host (decimalHost): the agent's NAME is its codename to
// the search, which matches it as a substring, so a base-36 stamp can spell a
// query this spec types. It did: a stamp ending "vap" put the agent beside
// vapor:1 for "VAP" ("man" would have done the same to mango). Digits spell
// no query.
const stamp = String(Date.now());
// The agent: its NAME carries none of the codenames' letters in a row; its
// display name (its title) does.
const AGENT = `e2e-sws-ledger-${stamp}`;
const AGENT_TITLE = 'Rotate the wheat ledger';

// The windows (session = codename, all index 1): "buckwheat" CONTAINS
// "wheat" (the substring tier), "wheat" starts with it (the prefix tier).
// 🔴 buckwheat is MORE RECENT than wheat, so the All view lists it first: a
// search that kept the list's order instead of ranking would put it first too.
const WINDOWS: [string, string][] = [
  ['mango', '@1'],
  ['buckwheat', '@4'],
  ['wheat', '@2'],
  ['vapor', '@3'],
];
const label = (s: string) => s + ':1';
const viewOf = (s: string) => `${origin}${rig.viewPath(s)}&window=${encodeURIComponent(WINDOWS.find((w) => w[0] === s)![1])}`;

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
  const now = Math.floor(Date.now() / 1000);
  const lines = WINDOWS.map(([s, w], i) => {
    const tr = rig.transcript(cc.prompt(`the prompt of ${s}`) + cc.reply(`Answered in ${s}.`), 'w' + i);
    return `${s}|${w}|1|1|claude|${now - 60 * (i + 1)}|${tr}`;
  });
  await rig.startHost({ MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_E2E_WINDOWS_FILE: rig.transcript(lines.join('\n') + '\n', 'windows'), MUSTER_MIRROR_E2E_REFRESH: '300ms' });
  mkchat(AGENT, [{ role: 'user', content: 'hello' }], { MUSTER_E2E_DISPLAY_NAME: AGENT_TITLE });
});

test.afterAll(async () => {
  mkchat(AGENT, null);
  await rig.stop();
});

const bar = (page: Page) => page.locator('#session-switcher');
const toggle = (page: Page) => page.locator('#switcher-toggle');
const menu = (page: Page) => page.locator('#switcher-views');
const pal = (page: Page) => page.locator('#switcher-search');
const input = (page: Page) => page.locator('#switcher-search-input');
const options = (page: Page) => page.locator('#switcher-search-results [role="option"]');
const optionLabels = (page: Page) => options(page).evaluateAll((els) => els.map((e) => (e.firstElementChild as HTMLElement).textContent));

async function ready(page: Page) {
  await rig.login(page);
  await rig.waitListed(page, 'buckwheat');
}

// openView opens mango's Rendered view (mango is then the page's own session,
// never a search result), with the stored view cleared and layout shift
// recorded from the first paint.
async function openView(page: Page, s = 'mango') {
  await page.addInitScript(clsInit);
  await page.goto(viewOf(s));
  await expect(bar(page)).toBeVisible();
  await settle(page);
}

// geometry is the page's own boxes that must not move when an overlay opens.
const geometry = (page: Page) =>
  page.evaluate(() =>
    ['body > header', '#mv-log', '#mv-compose', '#mv-input', '#session-switcher', '#switcher-toggle', '[data-switcher-list]:not([hidden])'].map((sel) => {
      const el = document.querySelector(sel);
      if (!el) return `${sel}: missing`;
      const r = el.getBoundingClientRect();
      return `${sel}: ${r.x.toFixed(1)},${r.y.toFixed(1)} ${r.width.toFixed(1)}×${r.height.toFixed(1)}`;
    }),
  );

// noShift: what was measured from `before` on moved nothing — the boxes, and
// layout shift INCLUDING what follows an input (the open is the reader's own
// tap, which CLS itself would excuse; a menu that pushed the page would hide
// there).
async function expectNoShift(page: Page, before: string[], what: string) {
  await page.waitForTimeout(400); // past the 160ms animation
  expect(await geometry(page), `${what}: no box on the page moved`).toEqual(before);
  const cls = await readCls(page);
  expect(cls.unsupported, 'the browser reports layout-shift entries').toBeUndefined();
  expect(cls.total + cls.excluded, `${what}: layout shift ${JSON.stringify([...cls.shifts, ...cls.excludedShifts])}`).toBe(0);
}

for (const vp of [
  { name: 'phone 390×844', w: 390, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', w: 1280, use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('the view button opens an animated menu upward; a pick switches the view and is kept; keyboard, Escape and a tap outside work; nothing moves', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await page.addInitScript(() => { try { if (!sessionStorage.getItem('sws-cleared')) { localStorage.removeItem('muster.switcher.view'); sessionStorage.setItem('sws-cleared', '1'); } } catch (e) {} });
      await openView(page);
      await expect(toggle(page)).toHaveText('Recent');
      await expect(toggle(page)).toHaveAttribute('aria-haspopup', 'menu');
      await expect(toggle(page)).toHaveAttribute('aria-expanded', 'false');
      await expect(menu(page)).toBeHidden();

      // It ANIMATES: the open starts opacity and transform transitions on the
      // menu (read in the same task as the tap, before they finish).
      const before = await geometry(page);
      await resetCls(page);
      const running = await page.evaluate(() => {
        (document.getElementById('switcher-toggle') as HTMLElement).click();
        return document.getElementById('switcher-views')!.getAnimations().map((a) => (a as CSSTransition).transitionProperty).sort();
      });
      expect(running, 'opening the menu runs its transitions').toEqual(expect.arrayContaining(['opacity', 'transform']));
      await expect(menu(page)).toBeVisible();
      await expect(toggle(page)).toHaveAttribute('aria-expanded', 'true');
      await expectNoShift(page, before, 'opening the view menu');
      await expect.poll(() => menu(page).evaluate((m) => getComputedStyle(m).opacity)).toBe('1');

      // It opens UPWARD from the button, inside the screen.
      const t = (await toggle(page).boundingBox())!;
      const m = (await menu(page).boundingBox())!;
      expect(m.y + m.height, 'the menu sits above the button').toBeLessThanOrEqual(t.y + 0.5);
      expect(m.x, 'inside the screen (left)').toBeGreaterThanOrEqual(0);
      expect(m.x + m.width, 'inside the screen (right)').toBeLessThanOrEqual(vp.w);

      // The views, the shown one checked, focus on it; every item a 44px target.
      const items = menu(page).getByRole('menuitemradio');
      await expect(items).toHaveCount(4);
      expect(await items.evaluateAll((els) => els.map((e) => `${e.querySelector('.flex-1')!.textContent}:${e.getAttribute('aria-checked')}`))).toEqual(['Recent:true', 'Waiting on you:false', 'Pins:false', 'All:false']);
      await expect(menu(page).locator('[data-switcher-pick="recent"]')).toBeFocused();
      for (const b of await items.all()) expect((await b.boundingBox())!.height, 'a 44px item').toBeGreaterThanOrEqual(44);

      // A pick: the view is shown, the menu closes, and it is kept.
      await (vp.w === 390 ? menu(page).locator('[data-switcher-pick="all"]').tap() : menu(page).locator('[data-switcher-pick="all"]').click());
      await expect(menu(page)).toBeHidden();
      await expect(toggle(page)).toHaveText('All');
      await expect(page.locator('[data-switcher-list="all"]')).toBeVisible();
      await expect(page.locator('[data-switcher-list="recent"]')).toBeHidden();
      expect(await page.evaluate(() => localStorage.getItem('muster.switcher.view'))).toBe('all');
      await page.reload();
      await expect(toggle(page)).toHaveText('All');
      await expect(page.locator('[data-switcher-list="all"]')).toBeVisible();

      // The keyboard: ArrowUp on the button opens with the shown view focused;
      // the arrows move (wrapping); Enter picks; Escape closes to the button.
      await toggle(page).focus();
      await page.keyboard.press('ArrowUp');
      await expect(menu(page)).toBeVisible();
      await expect(menu(page).locator('[data-switcher-pick="all"]')).toBeFocused();
      await page.keyboard.press('ArrowDown');
      await expect(menu(page).locator('[data-switcher-pick="recent"]')).toBeFocused();
      await page.keyboard.press('ArrowDown');
      await expect(menu(page).locator('[data-switcher-pick="waiting"]')).toBeFocused();
      await page.keyboard.press('ArrowDown');
      await expect(menu(page).locator('[data-switcher-pick="pins"]')).toBeFocused();
      await page.keyboard.press('Enter');
      await expect(menu(page)).toBeHidden();
      await expect(toggle(page)).toHaveText('Pins');
      await expect(toggle(page)).toBeFocused();
      await page.keyboard.press('ArrowDown');
      await expect(menu(page)).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(menu(page)).toBeHidden();
      await expect(toggle(page)).toHaveAttribute('aria-expanded', 'false');
      await expect(toggle(page)).toBeFocused();
      await expect(toggle(page)).toHaveText('Pins');

      // A tap outside closes it, and picks nothing.
      await toggle(page).click();
      await expect(menu(page)).toBeVisible();
      const log = (await page.locator('#mv-log').boundingBox())!;
      await page.mouse.click(log.x + log.width / 2, log.y + Math.min(60, log.height / 2));
      await expect(menu(page)).toBeHidden();
      await expect(toggle(page)).toHaveText('Pins');

      // Reduced motion: it opens at once, no transition.
      await page.emulateMedia({ reducedMotion: 'reduce' });
      const still = await page.evaluate(() => {
        (document.getElementById('switcher-toggle') as HTMLElement).click();
        return document.getElementById('switcher-views')!.getAnimations().length;
      });
      expect(still, 'no transition under prefers-reduced-motion').toBe(0);
      await expect(menu(page)).toBeVisible();
      await page.keyboard.press('Escape');
      await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
    });

    test('search: "/" opens the palette, results fill as a codename is typed (prefix, then substring, then title), arrows and Enter open one; nothing moves', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await openView(page);
      const before = await geometry(page);
      await resetCls(page);
      // "/" with focus on no text field.
      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
      await page.keyboard.press('/');
      await expect(pal(page)).toBeVisible();
      await expect(input(page)).toBeFocused();
      await expect(input(page)).toHaveValue('');
      await expect(page.locator('#switcher-search-open')).toHaveAttribute('aria-expanded', 'true');
      await expectNoShift(page, before, 'opening the search palette');

      // A floating box near the top, inside the screen, in the top half (a
      // phone's keyboard takes the bottom).
      const p = (await pal(page).boundingBox())!;
      expect(p.x).toBeGreaterThanOrEqual(0);
      expect(p.x + p.width).toBeLessThanOrEqual(vp.w);
      expect(p.y, 'the palette sits in the top half').toBeLessThan(844 / 2);

      // Empty: every session the All view links (the page's own, mango, left out).
      const all = await optionLabels(page);
      expect(all, 'every other session').toEqual(expect.arrayContaining([label('wheat'), label('vapor'), label('buckwheat'), AGENT_TITLE]));
      expect(all, 'not the page itself').not.toContain(label('mango'));

      // Typing fills the list as it goes.
      await input(page).pressSequentially('wh');
      await expect.poll(() => optionLabels(page)).toEqual(expect.arrayContaining([label('wheat'), label('buckwheat')]));
      expect((await optionLabels(page)).slice(0, 2), 'the codename prefix match first, then the substring').toEqual([label('wheat'), label('buckwheat')]);
      expect(await optionLabels(page), 'what holds no "wh" is gone').not.toContain(label('vapor'));
      await input(page).pressSequentially('eat');
      // 🔴 THE RANK: "wheat" starts wheat:1, is inside buckwheat:1, and only in
      // the agent's TITLE — in that order, and nothing else.
      await expect.poll(() => optionLabels(page)).toEqual([label('wheat'), label('buckwheat'), AGENT_TITLE]);
      await expect(options(page).first()).toHaveAttribute('aria-selected', 'true');
      await expect(input(page)).toHaveAttribute('aria-activedescendant', 'switcher-opt-0');
      // Case-insensitive.
      await input(page).fill('VAP');
      await expect.poll(() => optionLabels(page)).toEqual([label('vapor')]);
      // Nothing: the empty line.
      await input(page).fill('zzqx');
      await expect(options(page)).toHaveCount(0);
      await expect(pal(page).locator('[data-switcher-search-empty]')).toBeVisible();
      await expect(pal(page).locator('[data-switcher-search-empty]')).toHaveText('No session matches');

      // 🔴 TYPED AS THE OPERATOR TYPES IT: the bare codename "wheat" (no ":1")
      // ranks wheat:1 first, and a session whose TITLE holds the word (the
      // agent's "Rotate the wheat ledger") matches too, after the codenames.
      // Arrows move (wrapping); Enter opens the highlighted one — its own view.
      await input(page).fill('wheat');
      await expect.poll(() => optionLabels(page)).toEqual([label('wheat'), label('buckwheat'), AGENT_TITLE]);
      await page.keyboard.press('ArrowDown');
      await expect(options(page).nth(1)).toHaveAttribute('aria-selected', 'true');
      await page.keyboard.press('ArrowUp');
      await page.keyboard.press('ArrowUp');
      await expect(options(page).nth(2)).toHaveAttribute('aria-selected', 'true');
      await page.keyboard.press('ArrowDown');
      await page.keyboard.press('ArrowDown');
      await expect(options(page).nth(1)).toHaveAttribute('aria-selected', 'true');
      await Promise.all([
        page.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('session') === 'buckwheat'),
        page.keyboard.press('Enter'),
      ]);
      await expect(bar(page)).toBeVisible();
    });

    test('search: the magnifier opens it, Escape and a tap outside close it, Ctrl+K is left to the browser, "/" in the message box is typed, a tap opens a result', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await openView(page);
      const open = page.locator('#switcher-search-open');
      expect((await open.boundingBox())!.height, 'a 44px target').toBeGreaterThanOrEqual(44);
      expect((await open.boundingBox())!.width, 'a 44px target').toBeGreaterThanOrEqual(44);

      // The magnifier; Escape closes and gives focus back to it.
      await (vp.w === 390 ? open.tap() : open.click());
      await expect(pal(page)).toBeVisible();
      await expect(input(page)).toBeFocused();
      await page.keyboard.press('Escape');
      await expect(pal(page)).toBeHidden();
      await expect(open).toHaveAttribute('aria-expanded', 'false');
      await expect(open).toBeFocused();

      // A tap outside closes it.
      await (vp.w === 390 ? open.tap() : open.click());
      await expect(pal(page)).toBeVisible();
      await page.mouse.click(5, 836);
      await expect(pal(page)).toBeHidden();

      // Ctrl+K and Cmd+K are the browser's: neither opens the search, nor is
      // kept from the browser (not default-prevented). Measured from no text
      // field, where "/" WOULD open it (the control, just below).
      await page.evaluate(() => {
        (document.activeElement as HTMLElement | null)?.blur();
        (window as any).__swsK = [];
        document.addEventListener('keydown', (e) => { if (e.key === 'k') (window as any).__swsK.push(e.defaultPrevented); });
      });
      await page.keyboard.press('Control+k');
      await page.keyboard.press('Meta+k');
      await page.waitForTimeout(200);
      await expect(pal(page)).toBeHidden();
      expect(await page.evaluate(() => (window as any).__swsK), 'both keys reached the page, neither prevented').toEqual([false, false]);
      await page.keyboard.press('/');
      await expect(pal(page), 'CONTROL: "/" from the same place opens it').toBeVisible();
      await page.keyboard.press('Escape');
      await expect(pal(page)).toBeHidden();

      // "/" in the message box is a character, not the search.
      await page.locator('#mv-input').fill('');
      await page.locator('#mv-input').focus();
      await page.keyboard.press('/');
      await expect(page.locator('#mv-input')).toHaveValue('/');
      await expect(pal(page)).toBeHidden();
      await page.locator('#mv-input').fill('');

      // A tap (a click on desktop) on a result opens it.
      await (vp.w === 390 ? open.tap() : open.click());
      await input(page).pressSequentially('vap');
      await expect.poll(() => optionLabels(page)).toEqual([label('vapor')]);
      const first = options(page).first();
      await Promise.all([
        page.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('session') === 'vapor'),
        vp.w === 390 ? first.tap() : first.click(),
      ]);
    });

    // 🔴 THE OPEN PALETTE OWNS TAB AND ESCAPE: at acfcd699 a Tab took focus out
    // of it, and the Escape that then closed it ALSO reached the document and
    // closed the message index's sheet underneath in the same keypress.
    test('with the message index open: Tab stays in the palette, and Escape closes the palette only', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await openView(page);
      const sheet = page.locator('#chat-index');
      await page.locator('#chat-index-open').click();
      await expect(sheet).toBeVisible();
      // Focus is on the sheet's close button (not a text field): "/" opens the search.
      await page.keyboard.press('/');
      await expect(pal(page)).toBeVisible();
      await expect(input(page)).toBeFocused();
      await page.keyboard.press('Tab');
      await page.keyboard.press('Escape');
      await expect(pal(page)).toBeHidden();
      await expect(sheet, 'after a Tab, Escape closed the palette and left the sheet underneath open').toBeVisible();
      // And focus never left it.
      await page.keyboard.press('/');
      await expect(input(page)).toBeFocused();
      await page.keyboard.press('Tab');
      await expect(input(page), 'Tab stays in the open palette').toBeFocused();
      await page.keyboard.press('Shift+Tab');
      await expect(input(page), 'and so does Shift+Tab').toBeFocused();
      await page.keyboard.press('Escape');
      await expect(pal(page)).toBeHidden();
      await expect(sheet).toBeVisible();
      // CONTROL: the next Escape is the sheet's, and closes it.
      await page.keyboard.press('Escape');
      await expect(sheet).toBeHidden();
    });

    test('on an agent\'s chat page: "/" finds a window by codename and Enter opens it; nothing moves', async ({ page }) => {
      test.setTimeout(90_000);
      await ready(page);
      await page.addInitScript(clsInit);
      await page.goto(`${origin}/agents/${AGENT}`);
      await expect(bar(page)).toBeVisible();
      await settle(page);
      const at = () => page.evaluate(() => ['#chat-form', '#chat-input', '#session-switcher'].map((s) => JSON.stringify(document.querySelector(s)!.getBoundingClientRect())));
      const before = await at();
      await resetCls(page);
      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
      await page.keyboard.press('/');
      await expect(pal(page)).toBeVisible();
      await page.waitForTimeout(400);
      expect(await at(), 'no box on the page moved').toEqual(before);
      const cls = await readCls(page);
      expect(cls.total + cls.excluded, `layout shift ${JSON.stringify([...cls.shifts, ...cls.excludedShifts])}`).toBe(0);
      await input(page).pressSequentially('man');
      await expect.poll(() => optionLabels(page)).toEqual([label('mango')]);
      await Promise.all([
        page.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('session') === 'mango'),
        page.keyboard.press('Enter'),
      ]);
    });
  });
}
