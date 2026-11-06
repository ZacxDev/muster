import fs from 'node:fs';
import path from 'node:path';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { sessionTagsPort } from '../lib/env';
import { cc, MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';
import { clsInit, readCls, settle } from '../lib/cls';

// SESSION TAGS AND THE COLLAPSIBLE COMPOSER, in a real browser against a real
// muster-server (MUSTER_MIRROR=1, its own port) and a real muster-mirror over
// the tunnel (the `-tags e2emirror` per-window fake), on a phone (390×844,
// touch: a coarse pointer) and a desktop (1280×800, a mouse).
//
// The operator asked:
//   - the Rendered view's message box hidden by default on a phone, behind a
//     "Type a message" button beside the mic, revealed by it — and revealed by
//     anything that puts text in the box (a draft, the mic, the / menu, Send
//     again, a suggestion), never collapsed again while the page is open;
//   - the session's ⋯ in the row with the message controls on that view;
//   - Tags… in the ⋯ menu: chips with × and an input, per user;
//   - the tags in both views' headers (one chip, or the first and "+N" opening
//     the full list), in place of the raw tmux session name;
//   - the palette (and the dock) finding a session by its tags.
//
// The host gives the session "tagx" the SYNTHETIC codename "walnut" through
// its labels file, so the header's title is a label and the raw session name
// is what the removed span used to draw beside it.

const rig = new MirrorRig(sessionTagsPort, 'e2e-tag');
const origin = rig.origin;
const host = rig.host;
const windowsFile = path.join(rig.dir, 'windows');
const labelsFile = path.join(rig.dir, 'labels.json');
const now = Math.floor(Date.now() / 1000);
const view = (session: string, id: number) => `/mirror/view?host=${host}&session=${session}&window=%40${id}`;
const term = (session: string, id: number) => `/mirror/terminal?host=${host}&session=${session}&window=%40${id}`;

test.beforeAll(async () => {
  await rig.start();
  const tr = (s: string) => rig.transcript(cc.prompt('the prompt of ' + s) + cc.reply('Answered in ' + s + '.'), s);
  // session|id|index|active|command|activity|claude-transcript — one Claude
  // Code window per test that types (a draft is per window), two of them in
  // the session the tags are on.
  const rows = [`tagx|@1|1|1|claude|${now - 60}|${tr('tagx')}`, `tagx|@2|2|0|claude|${now - 120}|${tr('tagx2')}`, `tagy|@3|1|1|claude|${now - 600}|${tr('tagy')}`];
  for (let i = 0; i < 12; i++) rows.push(`tagd${i}|@${10 + i}|1|1|claude|${now - 900 - i}|${tr('tagd' + i)}`);
  writeFileAtomic(windowsFile, rows.join('\n') + '\n');
  fs.writeFileSync(labelsFile, JSON.stringify({ tagx: 'walnut' }));
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile, MUSTER_MIRROR_LABELS_FILE: labelsFile, MUSTER_MIRROR_INTERACTIVE: '1' });
});

test.afterAll(async () => {
  await rig.stop();
});

const box = (page: Page) => page.locator('#mv-input');
const textBtn = (page: Page) => page.locator('#mv-text');

async function open(page: Page, path: string) {
  await signIn(page, origin);
  await rig.waitListed(page, 'walnut');
  await page.goto(origin + path);
  await expect(page.locator('#mv-send')).toBeEnabled({ timeout: 20_000 });
}

// clearTags removes every tag of the operator's on tagx, through the route.
async function clearTags(page: Page) {
  const r = await page.request.get(origin + `/sessions/tags?kind=mirror&host=${host}&session=tagx`);
  expect(r.status()).toBe(200);
  for (const t of ((await r.json()) as { tags: string[] }).tags) {
    const d = await page.request.post(origin + '/sessions/tags/delete', { form: { kind: 'mirror', host, session: 'tagx', tag: t }, headers: { Origin: origin }, maxRedirects: 0 });
    expect(d.status()).toBe(303);
  }
}

// collapse puts the box back in its collapsed state, as it is on a fresh load —
// so a writer that runs while it is open can be watched revealing it.
const collapse = (page: Page) =>
  page.evaluate(() => {
    document.getElementById('mv-form')!.setAttribute('data-mv-collapsed', '');
    document.getElementById('mv-text')!.setAttribute('aria-expanded', 'false');
  });

test.describe('phone 390×844 touch', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('the box is collapsed behind “Type a message”, beside the mic; a tap reveals and focuses it, and a send does not collapse it', async ({ page }) => {
    await open(page, view('tagd0', 10));
    await expect(box(page), 'the box is in the DOM').toHaveCount(1);
    await expect(box(page), 'and hidden').toBeHidden();
    await expect(textBtn(page)).toBeVisible();
    await expect(textBtn(page)).toHaveAccessibleName('Type a message');
    await expect(textBtn(page)).toHaveAttribute('aria-expanded', 'false');
    await expect(textBtn(page)).toHaveAttribute('aria-controls', 'mv-input');
    // Beside the mic (the server has no dictation here: its muted mic), the
    // ⋯ and Send in the same row, all 44px targets inside the screen.
    const row = await Promise.all(['#mv-text', '#mv-dictate-off', '#switcher-more-btn', '#mv-send'].map((s) => page.locator(s).boundingBox()));
    for (const b of row) {
      expect(b, 'every control of the row is drawn').toBeTruthy();
      expect(b!.height).toBeGreaterThanOrEqual(44);
      expect(b!.x + b!.width).toBeLessThanOrEqual(390);
    }
    const mid = (b: { y: number; height: number }) => b.y + b.height / 2;
    for (const b of row.slice(1)) expect(Math.abs(mid(b!) - mid(row[0]!)), 'one row').toBeLessThanOrEqual(2);
    expect(row[1]!.x - (row[0]!.x + row[0]!.width), '“Type a message” is right before the mic').toBeLessThanOrEqual(12);
    await expect(page.locator('#session-switcher #switcher-more-btn'), 'the ⋯ is not in the bar on this view').toHaveCount(0);

    await textBtn(page).tap();
    await expect(box(page)).toBeVisible();
    await expect(box(page)).toBeFocused();
    await expect(textBtn(page)).toBeHidden();
    await expect(textBtn(page)).toHaveAttribute('aria-expanded', 'true');
    await box(page).fill('first message');
    await page.locator('#mv-send').tap();
    await expect(box(page)).toHaveValue('');
    await expect(box(page), 'a send leaves it open').toBeVisible();
  });

  test('a restored draft reveals the box, on a load that shifts nothing', async ({ page }) => {
    await open(page, view('tagd1', 11));
    await textBtn(page).tap();
    await box(page).fill('a draft I was writing');
    await box(page).blur();
    await expect.poll(() => page.evaluate(() => Object.keys(localStorage).filter((k) => k.includes('tagd1')).length)).toBeGreaterThan(0);
    await page.addInitScript(clsInit);
    await page.reload();
    await expect(box(page), 'the draft is back, in a box that is shown').toBeVisible({ timeout: 20_000 });
    await expect(box(page)).toHaveValue('a draft I was writing');
    await expect(textBtn(page)).toBeHidden();
    await settle(page);
    const r = await readCls(page);
    expect(r.unsupported, 'layout-shift is observable here').toBeUndefined();
    expect(r.total, `layout shift of the load with a draft: ${JSON.stringify(r.shifts)}`).toBeLessThan(0.01);
    // The positive control (composer-menu.spec's): an unprovoked shift IS counted.
    await page.waitForTimeout(600);
    await page.evaluate(() => {
      const d = document.createElement('div');
      d.style.cssText = 'height:150px;flex:none';
      document.body.prepend(d);
    });
    await page.waitForTimeout(300);
    expect((await readCls(page)).total, 'the instrument sees a shift').toBeGreaterThan(0.01);
    await page.reload();
    await box(page).fill('');
  });

  test('the / menu’s insert reveals the box', async ({ page }) => {
    await open(page, view('tagd2', 12));
    await textBtn(page).tap();
    await box(page).fill('');
    await page.keyboard.type('/compa');
    const item = page.locator('#mv-complete [data-name="compact"]');
    await expect(item).toBeVisible();
    await collapse(page);
    await expect(box(page)).toBeHidden();
    await item.tap();
    await expect(box(page)).toBeVisible();
    await expect(box(page)).toHaveValue('/compact ');
    await expect(textBtn(page)).toHaveAttribute('aria-expanded', 'true');
    await box(page).fill('');
  });

  test('a message that may have been delivered comes back to the box with Send again, and reveals it', async ({ page }) => {
    await open(page, view('tagd3', 13));
    let release: () => void = () => {};
    const held = new Promise<void>((r) => (release = r));
    await page.route('**/mirror/view/send**', async (route) => {
      await held;
      await route.fulfill({ status: 502, headers: { 'X-Muster-Send-Again': '1', 'Content-Type': 'text/plain' }, body: 'Not confirmed: the host did not answer.' });
    });
    await textBtn(page).tap();
    await box(page).fill('did this land');
    await page.locator('#mv-send').tap();
    await expect(box(page)).toHaveValue('');
    await collapse(page);
    await expect(box(page)).toBeHidden();
    release();
    await expect(page.locator('#mv-send-again')).toBeVisible();
    await expect(box(page)).toBeVisible();
    await expect(box(page)).toHaveValue('did this land');
    await page.unroute('**/mirror/view/send**');
    await box(page).fill('');
  });
});

// 🔴 WITHOUT SCRIPT THE BOX IS SHOWN: "Type a message" is a script's button,
// so only the script collapses the box (composer_reveal.go). On a phone with
// script off, the box must still be there to type into.
test('on a phone with no script the box is shown, and “Type a message” is not', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, javaScriptEnabled: false });
  const page = await ctx.newPage();
  try {
    await signIn(page, origin);
    await page.goto(origin + view('tagd5', 15));
    await expect(box(page)).toBeVisible({ timeout: 20_000 });
    await expect(textBtn(page)).toBeHidden();
    await expect(page.locator('#mv-form')).not.toHaveAttribute('data-mv-collapsed', '');
  } finally {
    await ctx.close();
  }
});

test.describe('desktop 1280×800', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

  test('the box is shown as before and “Type a message” is never drawn; the ⋯ is in the input row', async ({ page }) => {
    await open(page, view('tagd4', 14));
    await expect(box(page)).toBeVisible();
    await expect(textBtn(page)).toBeHidden();
    await box(page).click();
    await expect(box(page)).toBeFocused();
    const [more, send] = await Promise.all([page.locator('#mv-form #switcher-more-btn').boundingBox(), page.locator('#mv-send').boundingBox()]);
    expect(more && send && more.x < send.x, 'the ⋯ is in the row, before Send').toBeTruthy();
    // The Terminal view keeps its ⋯ in the bar.
    await page.goto(origin + term('tagd4', 14));
    await expect(page.locator('#session-switcher #switcher-more-btn')).toBeVisible({ timeout: 20_000 });
  });
});

// The tags, end to end, at both sizes: added from ⋯ → Tags…, drawn in both
// views' headers, kept across a reload, found by the palette, "+N" with its
// list, removed again.
for (const vp of [
  { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`tags, ${vp.name}`, () => {
    test.use(vp.use);
    const press = (page: Page, sel: string) => (vp.use.hasTouch ? page.locator(sel).tap() : page.locator(sel).click());

    test('⋯ → Tags… adds a tag that the header shows in both views, a reload keeps, and the palette finds', async ({ page }) => {
      await open(page, view('tagx', 1));
      await clearTags(page);
      await page.reload();
      await expect(page.locator('#mv-send')).toBeEnabled({ timeout: 20_000 });
      const head = page.locator('header #session-tags-head');
      await expect(head).toHaveAttribute('data-session-tags-head', '0');
      // The raw tmux session name is not drawn beside the label any more.
      await expect(page.locator('header [data-mirror-label]')).toHaveText('walnut:1');
      await expect(page.locator('[data-mirror-session]')).toHaveCount(0);
      await expect(page.locator('header [data-mirror-name]')).toHaveAttribute('title', 'tmux session tagx');

      await press(page, '#switcher-more-btn');
      await expect(page.locator('#switcher-more')).toBeVisible();
      await press(page, '#switcher-tags');
      await expect(page.locator('#switcher-more'), 'the menu closed for the editor').toBeHidden();
      const editor = page.locator('#session-tags-editor');
      await expect(editor).toBeVisible();
      await expect(page.locator('#session-tags-input')).toBeFocused();
      await page.locator('#session-tags-input').fill('  Needs Review ');
      await page.locator('#session-tags-input').press('Enter');
      // Normalized as the task tags are, and drawn in the editor and the header.
      await expect(editor.locator('[data-session-tag-chip="needs-review"]')).toBeVisible();
      await expect(head.locator('[data-session-tag="needs-review"]')).toBeVisible();
      await expect(head).toHaveAttribute('data-session-tags-head', '1');
      await expect(page.locator('#session-tags-input')).toBeFocused();
      // A refusal is said in the editor, and adds nothing.
      await page.locator('#session-tags-input').fill('no;semicolons');
      await page.locator('#session-tags-input').press('Enter');
      await expect(editor.locator('[data-session-tags-error]')).toContainText('invalid character');
      await expect(head).toHaveAttribute('data-session-tags-head', '1');
      await page.keyboard.press('Escape');

      await page.reload();
      await expect(page.locator('header #session-tags-head [data-session-tag="needs-review"]'), 'kept across a reload').toBeVisible({ timeout: 20_000 });
      // The other view of the window, and another window of the same session.
      for (const p of [term('tagx', 1), view('tagx', 2)]) {
        await page.goto(origin + p);
        await expect(page.locator('header #session-tags-head [data-session-tag="needs-review"]'), p).toBeVisible({ timeout: 20_000 });
      }
      // The palette, from another session's page, finds tagx by its tag.
      await page.goto(origin + view('tagy', 3));
      await expect(page.locator('#mv-send')).toBeEnabled({ timeout: 20_000 });
      await press(page, '#switcher-search-open');
      await page.locator('#switcher-search-input').fill('needs-review');
      // Both windows of the session carry it (tags are on the tmux session),
      // matched by their words.
      const hits = page.locator('#switcher-search-results [role="option"]');
      await expect(hits).toHaveCount(2);
      for (const h of await hits.all()) {
        await expect(h).toHaveAttribute('data-href', /session=tagx/);
        await expect(h).toHaveAttribute('data-match', 'words');
      }
      // And a tag nobody carries finds nothing (the search is not matching everything).
      await page.locator('#switcher-search-input').fill('no-such-tag');
      await expect(hits).toHaveCount(0);
      // The Sessions dock's filter (a touch screen's view button opens the
      // dock: switcher_dock.go) matches the same words: both windows of tagx.
      if (vp.use.hasTouch) {
        await page.keyboard.press('Escape');
        await page.locator('#switcher-toggle').tap();
        await expect(page.locator('#switcher-dock')).toBeVisible();
        await page.locator('#switcher-dock [data-dock-tab="all"]').tap();
        await page.locator('#dock-filter').fill('needs-review');
        await expect(page.locator('#dock-list [data-dock-row]')).toHaveCount(2);
        await page.locator('#dock-filter').fill('no-such-tag');
        await expect(page.locator('#dock-list [data-dock-row]')).toHaveCount(0);
      }
    });

    // The view switch keeps both views alive (view_swap.go: the other one is a
    // frame, never reloaded): an edit in one is in the other's header too.
    test('a tag added in the Rendered view is in the already-open Terminal view’s header', async ({ page }) => {
      await open(page, view('tagx', 1));
      await clearTags(page);
      await page.reload();
      await expect(page.locator('#mv-send')).toBeEnabled({ timeout: 20_000 });
      const frame = page.frameLocator('iframe[data-muster-view]');
      await press(page, '#view-terminal');
      await expect(frame.locator('header #session-tags-head')).toHaveAttribute('data-session-tags-head', '0', { timeout: 20_000 });
      await (vp.use.hasTouch ? frame.locator('#view-rendered').tap() : frame.locator('#view-rendered').click());
      await expect(page.locator('iframe[data-muster-view]')).toBeHidden();
      await press(page, '#switcher-more-btn');
      await press(page, '#switcher-tags');
      await page.locator('#session-tags-input').fill('swapped');
      await page.locator('#session-tags-input').press('Enter');
      await expect(page.locator('header #session-tags-head [data-session-tag="swapped"]')).toBeVisible();
      await page.keyboard.press('Escape');
      await press(page, '#view-terminal');
      await expect(frame.locator('header #session-tags-head [data-session-tag="swapped"]'), 'no reload of the frame').toBeVisible();
      await clearTags(page);
    });

    test('several tags are the first chip and +N, whose popover lists them all; × removes one, live', async ({ page }) => {
      await open(page, view('tagx', 1));
      await clearTags(page);
      for (const t of ['alpha', 'beta', 'gamma']) {
        const r = await page.request.post(origin + '/sessions/tags', { form: { kind: 'mirror', host, session: 'tagx', tag: t }, headers: { Origin: origin }, maxRedirects: 0 });
        expect(r.status()).toBe(303);
      }
      await page.reload();
      const head = page.locator('header #session-tags-head');
      await expect(head.locator(':scope > [data-session-tag="alpha"]')).toBeVisible({ timeout: 20_000 });
      const more = head.locator('[data-session-tags-more]');
      await expect(more).toHaveText('+2');
      await press(page, 'header [data-session-tags-more]');
      const all = page.locator('#session-tags-all');
      await expect(all).toBeVisible();
      await expect(all.locator('[data-session-tag]')).toHaveText(['alpha', 'beta', 'gamma']);
      const [pb] = await Promise.all([all.boundingBox()]);
      expect(pb!.x).toBeGreaterThanOrEqual(0);
      expect(pb!.x + pb!.width).toBeLessThanOrEqual(vp.use.viewport.width);
      await page.keyboard.press('Escape');

      await press(page, '#switcher-more-btn');
      await press(page, '#switcher-tags');
      await press(page, '#session-tags-editor [data-session-tag-remove="beta"]');
      await expect(page.locator('#session-tags-editor [data-session-tag-chip]')).toHaveCount(2);
      await expect(more).toHaveText('+1');
      await press(page, '#session-tags-editor [data-session-tag-remove="alpha"]');
      await expect(head.locator('[data-session-tags-more]')).toHaveCount(0);
      await expect(head.locator(':scope > [data-session-tag="gamma"]')).toBeVisible();
      await clearTags(page);
    });
  });
}
