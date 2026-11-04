import { execFileSync } from 'node:child_process';
import { Browser, BrowserContext, Frame, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dockPort, dsn, repoRoot } from '../lib/env';
import { cc, MirrorRig, mkchat } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';
import { launchWithBfcache } from '../lib/bfcache';
import { coarse } from '../lib/switcher';

// THE SWITCHER DOCK, PHASE 1 (docs/design/switcher-dock.md; internal/ui/
// switcher_dock.go): the session switcher's four views as a vertical list in a
// modal bottom sheet, opened by a TAP on the bar's view button in the touch
// layout — a coarse primary pointer, at any width. On a REAL muster-server and
// a REAL muster-mirror (`-tags e2emirror`, fake tmux only) whose 37 windows are
// enough to fill the dock's Recent tab, plus dispatched agents.
//
// The cases are the design's numbers:
//   2 / 19  the view button opens the dock in the touch layout and the menu in
//           the mouse layout (narrow and wide); a tab moves the bar at once;
//   5       the backdrop, the close button and Escape close it; the 44px grip
//           toggles half and full; "/" opens no palette while it is open;
//   9 / 9b  closing never touches history, on a boosted page too; a row tap
//           is one real load, the dock closed BEFORE it (back/forward cache);
//           the shell's back-claim pair;
//   15      reduced motion;
//   18      Recent holds 30;
//   20      the dock opened from the view switch's frame is the TOP document's;
//   21      htmx's history cache never holds the dock open;
//   22      an older parent page: the frame's own view menu, no dock.
//
// 🔴 EVERY TOUCH CASE FIRST ASSERTS (pointer: coarse) MATCHES, and every mouse
// case that it does not: the layout is decided by that one query, so a context
// that did not emulate it would test the other layout and pass.

test.use({ storageState: { cookies: [], origins: [] } });

const rig = new MirrorRig(
  dockPort,
  'e2e-dock',
  {
    // A claude-code agent, so an agent page has a Terminal view to swap to.
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-dock-alpha',
  },
  { decimalHost: true },
);
const origin = rig.origin;
const stamp = String(Date.now());
// An agent with two chat sessions: its session picker's links are BOOSTED
// (agents_detail.go sessionDrawer), so a page reached by one has an htmx
// history entry.
const THREADS = `e2e-dock-threads-${stamp}`;
// A claude-code agent: its page swaps to its Terminal view in a frame.
const CODER = `e2e-dock-coder-${stamp}`;

// The windows: dk01…dk37, one per session, each a Claude Code pane.
const N = 37;
const sess = (i: number) => 'dk' + String(i).padStart(2, '0');
let windowsFile = '';
let windowLines: string[] = [];
const viewOf = (s: string) => origin + rig.viewPath(s) + '&window=' + encodeURIComponent('@1');
const OWN = sess(1);

test.beforeAll(async () => {
  test.setTimeout(240_000);
  await rig.start();
  const now = Math.floor(Date.now() / 1000);
  windowLines = Array.from({ length: N }, (_, k) => {
    const s = sess(k + 1);
    const tr = rig.transcript(cc.prompt(`the prompt of ${s}`) + cc.reply(`Answered in ${s}.`), 'w' + s);
    return `${s}|@1|1|1|claude|${now - 60 * (k + 1)}|${tr}`;
  });
  windowsFile = rig.transcript(windowLines.join('\n') + '\n', 'windows');
  await rig.startHost({ MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
  mkchat(THREADS, [
    { title: 'First thread', hoursAgo: 3, messages: [{ role: 'user', content: 'the first thread' }] },
    { title: 'Second thread', hoursAgo: 1, messages: [{ role: 'user', content: 'the second thread' }] },
  ], { MUSTER_E2E_THREADS: '1', MUSTER_E2E_DISPLAY_NAME: 'Dock threads' });
  mkchat(CODER, [{ role: 'user', content: 'Fix the tunnel.' }], { MUSTER_E2E_KIND: 'claude-code', MUSTER_E2E_CC_ACCOUNT: 'alpha' });
});

test.afterAll(async () => {
  mkchat(THREADS, null);
  mkchat(CODER, null);
  await rig.stop();
});

const dock = (p: Page) => p.locator('#switcher-dock');
const toggle = (p: Page) => p.locator('#switcher-toggle');
const menu = (p: Page) => p.locator('#switcher-views');
const rows = (p: Page) => p.locator('#dock-list [data-dock-row]');
const dockOpen = (p: Page | Frame) => p.evaluate(() => (document.getElementById('switcher-dock') as HTMLDialogElement).open);
const menuOpen = (p: Page | Frame) => p.evaluate(() => document.getElementById('switcher-views')!.matches(':popover-open'));
const frameEl = (p: Page) => p.locator('iframe[data-muster-view]');

async function ready(page: Page) {
  await rig.login(page);
  await rig.waitListed(page, sess(N));
  await page.addInitScript(() => {
    try {
      if (!sessionStorage.getItem('dock-cleared')) {
        localStorage.removeItem('muster.switcher.view');
        sessionStorage.setItem('dock-cleared', '1');
      }
    } catch (e) {}
  });
}

// visit records that the viewer opened window s (the page records it as it
// renders), without driving the browser there.
async function visit(page: Page, s: string) {
  const r = await page.request.get(viewOf(s));
  expect(r.status(), `the view of ${s}`).toBe(200);
}

// openOwn loads OWN's Rendered view, a few windows visited first so Recent
// has rows.
async function openOwn(page: Page) {
  for (const s of [sess(5), sess(4), sess(3), sess(2)]) await visit(page, s);
  await page.goto(viewOf(OWN));
  await expect(page.locator('#session-switcher')).toBeVisible();
  await expect(page.locator('#mv-status')).toHaveText('Live');
}

// stubBridge stands in for the Android shell's bridge (window.musterApp) in
// every document, the frame's too: what each posts lands in the TOP window's
// __claims, with whether the poster was the top window.
async function stubBridge(page: Page | BrowserContext) {
  await page.addInitScript(() => {
    (window as any).musterApp = {
      postMessage: (m: string) => {
        const top = window.top as any;
        (top.__claims = top.__claims || []).push({ top: window.top === window, m });
      },
    };
  });
}
const claims = (p: Page) => p.evaluate(() => ((window as any).__claims || []) as { top: boolean; m: string }[]);
const CLAIM_ON = JSON.stringify({ type: 'back-claim', open: true });
const CLAIM_OFF = JSON.stringify({ type: 'back-claim', open: false });

// ---------------------------------------------------------------------------
// THE TOUCH LAYOUT: a phone
// ---------------------------------------------------------------------------
test.describe('phone 390×844 touch', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('2/19/5: the view button opens the dock, not the menu, on the shown view; a tab moves the bar; backdrop, ×, Escape close it; the grip toggles half and full; "/" stands down', async ({ page }) => {
    test.setTimeout(120_000);
    await ready(page);
    await stubBridge(page);
    await openOwn(page);
    expect(await coarse(page), 'this context is the touch layout').toBe(true);
    // A closed dock is display:none: nothing on first paint (case 16).
    expect(await dock(page).evaluate((d) => getComputedStyle(d).display)).toBe('none');
    await expect(toggle(page)).toHaveAttribute('aria-haspopup', 'dialog');
    await expect(toggle(page)).toHaveAttribute('aria-controls', 'switcher-dock');
    await page.evaluate(() => ((window as any).__marker = 'kept'));
    const hist0 = await page.evaluate(() => ({ n: history.length, s: JSON.stringify(history.state) }));

    // A tap opens the dock, modal, on Recent's tab (focused: no keyboard), and
    // NOT the view menu.
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    expect(await dock(page).evaluate((d) => d.matches(':modal')), 'a modal dialog').toBe(true);
    expect(await menuOpen(page), 'the view menu stays shut').toBe(false);
    await expect(dock(page).locator('[data-dock-tab="recent"]')).toBeFocused();
    await expect(dock(page).locator('[data-dock-tab="recent"]')).toHaveAttribute('aria-selected', 'true');
    await expect(toggle(page)).toHaveAttribute('aria-expanded', 'true');
    expect(await page.evaluate(() => ({ n: history.length, s: JSON.stringify(history.state) })), 'opening touches no history').toEqual(hist0);
    // Its Recent rows are the bar's, whole names, each a 56px link; the
    // page's own window is not in Recent.
    const barRecent = await page.locator('#session-switcher [data-switcher-list="recent"] a[data-switcher-entry]').evaluateAll((es) => es.map((e) => e.getAttribute('href')));
    expect(barRecent.length, 'the bar has recent entries').toBeGreaterThanOrEqual(4);
    // …followed by the entries past the bar's 8 (the Recent row's hidden list).
    const more = await page.evaluate(() => (document.querySelector('#session-switcher [data-switcher-recent-more]')?.getAttribute('data-switcher-recent-more') || '').split('\n').filter(Boolean));
    const dockRecent = await rows(page).evaluateAll((es) => es.map((e) => e.getAttribute('href')));
    expect(dockRecent, "the dock's Recent is the bar's, then the rest").toEqual([...barRecent, ...more]);
    await expect(dock(page).locator('[data-dock-tab="recent"] [data-dock-count]')).toHaveText(String(dockRecent.length));
    await expect(rows(page).first().locator('[data-dock-label]')).toHaveText(sess(2) + ':1');
    await expect(rows(page).first().locator('[data-dock-host]')).toHaveText(rig.host);
    for (const h of await rows(page).evaluateAll((es) => es.map((e) => (e as HTMLElement).offsetHeight))) expect(h, "a row is at least 56px").toBeGreaterThanOrEqual(56);

    // Half: 60% of the visual viewport. The grip, 44px tall, toggles full and back.
    const vvh = await page.evaluate(() => window.visualViewport!.height);
    await dock(page).evaluate((d) => Promise.all(d.getAnimations().map((a) => a.finished)));
    expect(Math.abs((await dock(page).boundingBox())!.height - Math.round(0.6 * vvh)), 'half is 60% of the visual viewport').toBeLessThanOrEqual(2);
    const grip = dock(page).locator('[data-dock-grip]');
    expect(Math.round((await grip.boundingBox())!.height), 'the grip is 44px tall').toBe(44);
    await expect(grip).toHaveAccessibleName('Expand');
    await grip.tap();
    await expect(dock(page)).toHaveAttribute('data-snap', 'full');
    await expect(grip).toHaveAccessibleName('Shrink to half');
    expect(Math.abs((await dock(page).boundingBox())!.height - (vvh - 8)), 'full is the visual viewport less 8px').toBeLessThanOrEqual(2);
    await grip.tap();
    await expect(dock(page)).toHaveAttribute('data-snap', 'half');

    // A tab: Pins. The BAR moves to Pins at once, in this document, no reload.
    await dock(page).locator('[data-dock-tab="pins"]').tap();
    await expect(dock(page).locator('[data-dock-tab="pins"]')).toHaveAttribute('aria-selected', 'true');
    await expect(page.locator('#session-switcher')).toHaveAttribute('data-switcher-view', 'pins');
    await expect(toggle(page).locator('[data-switcher-icon]:visible')).toHaveAttribute('data-switcher-icon', 'pins');
    expect(await page.locator('#session-switcher [data-switcher-list="pins"]').evaluate((e) => (e as HTMLElement).hidden)).toBe(false);
    expect(await page.evaluate(() => (window as any).__marker), 'no reload').toBe('kept');
    // The arrow keys move between the tabs, and show them.
    await page.keyboard.press('ArrowRight');
    await expect(dock(page).locator('[data-dock-tab="all"]')).toBeFocused();
    await expect(page.locator('#session-switcher')).toHaveAttribute('data-switcher-view', 'all');
    // All draws the page's own window current, not a link.
    await expect(dock(page).locator('#dock-list span[data-dock-row][aria-current="page"] [data-dock-label]')).toHaveText(OWN + ':1');
    await page.keyboard.press('Home');
    await expect(dock(page).locator('[data-dock-tab="recent"]')).toBeFocused();

    // "/" on a focused tab opens no palette behind the modal.
    await page.keyboard.press('/');
    await page.waitForTimeout(200);
    expect(await page.evaluate(() => document.getElementById('switcher-search')!.matches(':popover-open')), '"/" opened the palette under the dock').toBe(false);

    // The close button closes it; focus is back on the view button.
    await dock(page).locator('[data-dock-close]').tap();
    await expect.poll(() => dockOpen(page)).toBe(false);
    await expect(toggle(page)).toBeFocused();
    await expect(toggle(page)).toHaveAttribute('aria-expanded', 'false');
    await expect(dock(page)).toBeHidden();
    // CONTROL for "/": with the dock shut, "/" opens the palette.
    await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
    await page.keyboard.press('/');
    await expect.poll(() => page.evaluate(() => document.getElementById('switcher-search')!.matches(':popover-open'))).toBe(true);
    await page.keyboard.press('Escape');

    // The backdrop: a tap above the sheet.
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    await dock(page).evaluate((d) => Promise.all(d.getAnimations().map((a) => a.finished)));
    const top = (await dock(page).boundingBox())!.y;
    expect(top, 'the sheet leaves room above it').toBeGreaterThan(100);
    await page.touchscreen.tap(195, top / 2);
    await expect.poll(() => dockOpen(page)).toBe(false);

    // Escape.
    await toggle(page).focus();
    await page.keyboard.press('ArrowUp');
    await expect.poll(() => dockOpen(page), 'ArrowUp on the view button opens the dock').toBe(true);
    expect(await menuOpen(page)).toBe(false);
    await page.keyboard.press('Escape');
    await expect.poll(() => dockOpen(page)).toBe(false);
    await expect(toggle(page)).toBeFocused();

    // Nothing touched history, and nothing reloaded.
    expect(await page.evaluate(() => ({ n: history.length, s: JSON.stringify(history.state) }))).toEqual(hist0);
    expect(await page.evaluate(() => (window as any).__marker)).toBe('kept');

    // 9b: the shell's claim, one pair per open, from the top window. (The
    // release rides the close EVENT, a few ms after the close: waited for.)
    await expect.poll(async () => (await claims(page)).map((c) => c.m), 'each open posts the claim and each close releases it').toEqual([CLAIM_ON, CLAIM_OFF, CLAIM_ON, CLAIM_OFF, CLAIM_ON, CLAIM_OFF]);
    expect((await claims(page)).every((c) => c.top), 'from the top window').toBe(true);
    expect(await page.evaluate(() => (window as any).musterBack()), 'musterBack with the dock shut').toBe(false);
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    expect(await page.evaluate(() => (window as any).musterBack()), 'musterBack with the dock open').toBe(true);
    await expect.poll(() => dockOpen(page)).toBe(false);
    await expect.poll(async () => (await claims(page)).slice(-2).map((c) => c.m)).toEqual([CLAIM_ON, CLAIM_OFF]);

    // 🔴 A CLOSE WHOSE EVENT LANDS AFTER THE NEXT OPEN (the close event is a
    // task, a few ms late): closed and opened again in one task, the dock
    // stays open, CLAIMED, and still closes cleanly. Red before the fix: the
    // late event released the claim under the open dock and dropped its state.
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    await expect.poll(async () => (await claims(page)).slice(-1)[0].m).toBe(CLAIM_ON);
    await page.evaluate(() => {
      const w = window as any;
      w.musterDock.close();
      w.musterDock.open(document.getElementById('session-switcher'), document.getElementById('switcher-toggle'));
    });
    await page.waitForTimeout(300);
    expect(await dockOpen(page), 'open again').toBe(true);
    expect((await claims(page)).slice(-3).map((c) => c.m), 'released once, claimed again, and NOT released by the late event').toEqual([CLAIM_ON, CLAIM_OFF, CLAIM_ON]);
    await dock(page).locator('[data-dock-tab="waiting"]').tap();
    await expect(page.locator('#session-switcher'), "the reopened dock still drives the bar").toHaveAttribute('data-switcher-view', 'waiting');
    await page.keyboard.press('Escape');
    await expect.poll(() => dockOpen(page)).toBe(false);
    await expect.poll(async () => (await claims(page)).slice(-1)[0].m).toBe(CLAIM_OFF);
    await expect(toggle(page)).toBeFocused();
    await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
  });

  test('9/21: on a page reached by a boosted link, no close touches history; htmx\'s history cache never holds the dock open; a row tap is one real load', async ({ page }) => {
    test.setTimeout(120_000);
    await ready(page);
    expect(await coarse(page)).toBe(true);
    await page.goto(origin + '/agents/' + THREADS);
    await page.waitForURL((u) => u.searchParams.has('session'));
    const first = new URL(page.url()).searchParams.get('session');
    // Through the session picker: a boosted link to the other thread.
    // The drawer's links (boosted: no hx-boost of their own), not the bar's or
    // the sidebar's (hx-boost="false").
    const other = page.locator(`a[href^="/agents/${THREADS}?session="]:not([hx-boost]):not([href*="session=${first}"])`).first();
    await expect(other).toBeAttached();
    await other.evaluate((a) => (a as HTMLAnchorElement).click());
    await page.waitForURL((u) => u.searchParams.get('session') !== first);
    await expect(page.locator('#session-switcher')).toBeVisible();
    expect(await page.evaluate(() => !!(history.state && history.state.htmx)), 'a boosted entry: htmx state').toBe(true);
    await page.evaluate(() => {
      const m = document.createElement('div');
      m.id = 'dock-marker';
      document.body.appendChild(m);
      (window as any).__restores = 0;
      document.addEventListener('htmx:historyRestore', () => (window as any).__restores++);
    });
    const n0 = await page.evaluate(() => history.length);
    const shut = async (how: string, close: () => Promise<void>) => {
      await toggle(page).tap();
      await expect.poll(() => dockOpen(page), `${how}: open`).toBe(true);
      await dock(page).evaluate((d) => Promise.all(d.getAnimations().map((a) => a.finished)));
      await close();
      await expect.poll(() => dockOpen(page), `${how}: shut`).toBe(false);
      await page.waitForTimeout(150);
      expect(await page.locator('#dock-marker').count(), `${how}: the body was not swapped`).toBe(1);
      expect(await page.evaluate(() => (window as any).__restores), `${how}: no htmx restore`).toBe(0);
      expect(await page.evaluate(() => history.length), `${how}: history.length`).toBe(n0);
    };
    await shut('Escape', () => page.keyboard.press('Escape'));
    await shut('the backdrop', async () => page.touchscreen.tap(195, 60));
    await shut('the close button', () => dock(page).locator('[data-dock-close]').tap());

    // 21: open, Back to the previous boosted entry (htmx restores it), Forward:
    // the page htmx brings back from its cache has the dock shut, nothing inert.
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    await page.goBack();
    await expect.poll(() => new URL(page.url()).searchParams.get('session')).toBe(first);
    await expect.poll(() => page.evaluate(() => (window as any).__restores), 'POSITIVE CONTROL: a real Back restores one boosted entry').toBe(1);
    await page.goForward();
    await expect.poll(() => new URL(page.url()).searchParams.get('session')).not.toBe(first);
    await expect.poll(() => page.evaluate(() => (window as any).__restores)).toBe(2);
    await expect(page.locator('#session-switcher')).toBeVisible();
    expect(await page.evaluate(() => document.getElementById('switcher-dock')!.hasAttribute('open')), 'the restored dock is not open').toBe(false);
    expect(await page.evaluate(() => [document.body.inert, !!document.querySelector('[inert]'), !!document.querySelector('dialog[open]')]), 'nothing inert, no open dialog').toEqual([false, false, false]);

    // A row tap: one real load, history +1; Back lands here with the dock shut.
    const n1 = await page.evaluate(() => history.length);
    const here = page.url();
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    await dock(page).locator('[data-dock-tab="all"]').tap();
    const row = rows(page).filter({ has: page.locator('[data-dock-label]', { hasText: sess(3) + ':' }) });
    await expect(row).toHaveCount(1);
    await Promise.all([page.waitForURL((u) => u.searchParams.get('session') === sess(3)), row.tap()]);
    expect(await page.evaluate(() => history.length), 'a row tap adds exactly one entry').toBe(n1 + 1);
    await page.goBack();
    await expect.poll(() => page.url()).toBe(here);
    await expect(page.locator('#session-switcher')).toBeVisible();
    expect(await dockOpen(page), 'back on the page, the dock is shut').toBe(false);
    await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
  });

  test('15: reduced motion — no transition either way; control: one without it', async ({ page }) => {
    await ready(page);
    await openOwn(page);
    expect(await coarse(page)).toBe(true);
    const durations = () => dock(page).evaluate((d) => [getComputedStyle(d).transitionDuration, getComputedStyle(d, '::backdrop').transitionDuration]);
    const still = (ds: string[]) => ds.every((d) => d.split(',').every((x) => parseFloat(x) === 0));
    // CONTROL: animated without the emulation, open and shut.
    expect(still(await durations()), `animated when shut: ${await durations()}`).toBe(false);
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    expect(still(await durations()), `animated when open: ${await durations()}`).toBe(false);
    await page.keyboard.press('Escape');
    await page.emulateMedia({ reducedMotion: 'reduce' });
    expect(await durations(), 'shut, reduced').toEqual(['0s', '0s']);
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    expect(await durations(), 'open, reduced').toEqual(['0s', '0s']);
    await page.keyboard.press('Escape');
    await expect.poll(() => dockOpen(page)).toBe(false);
  });

  test('20: from the Rendered view\'s and the agent page\'s Terminal frame, the dock is the TOP document\'s, reads the frame bar\'s rows, returns focus into the frame, and a row navigates the top window', async ({ page }) => {
    test.setTimeout(150_000);
    await ready(page);
    await stubBridge(page);
    expect(await coarse(page)).toBe(true);
    for (const from of ['rendered', 'agent'] as const) {
      if (from === 'rendered') await openOwn(page);
      else {
        await page.goto(origin + '/agents/' + CODER);
        await expect(page.locator('#view-rendered')).toHaveAttribute('aria-current', 'page');
      }
      await page.evaluate(() => ((window as any).__claims = []));
      await page.locator('#view-terminal').tap();
      await expect(frameEl(page)).toBeVisible();
      const f = (await (await frameEl(page).elementHandle())!.contentFrame())!;
      await f.waitForSelector('#switcher-toggle');
      await expect.poll(() => f.evaluate(() => !!document.getElementById('session-switcher') && !document.getElementById('session-switcher')!.hasAttribute('data-switcher-pending'))).toBe(true);
      expect(await f.evaluate(() => document.getElementById('switcher-toggle')!.getAttribute('aria-haspopup')), `${from}: the frame's button names the dialog`).toBe('dialog');

      // A pin toggled in the FRAME (its own bar's ⋯), so the frame's Pins row
      // holds this session and the top bar's — not refreshed — does not.
      const fl = page.frameLocator('iframe[data-muster-view]');
      await fl.locator('#switcher-more-btn').tap();
      await expect(fl.locator('#switcher-pin')).toBeVisible();
      await expect(fl.locator('#switcher-pin'), `${from}: precondition, not pinned yet`).toHaveAttribute('data-pinned', 'false');
      await fl.locator('#switcher-pin').tap();
      await expect(fl.locator('#switcher-pin')).toHaveAttribute('data-pinned', 'true');
      const framePins = await f.evaluate(() => document.querySelectorAll('#session-switcher [data-switcher-list="pins"] [data-switcher-entry]').length);
      const topPins = await page.evaluate(() => document.querySelectorAll('#session-switcher [data-switcher-list="pins"] [data-switcher-entry]').length);
      expect(framePins, `${from}: the frame's Pins row has the pin`).toBeGreaterThanOrEqual(1);
      expect(topPins, `${from}: the top bar's Pins row was not refreshed`).toBe(framePins - 1);

      // The frame's view button opens the TOP document's dock.
      await fl.locator('#switcher-toggle').tap();
      await expect.poll(() => dockOpen(page), `${from}: the top dock opens`).toBe(true);
      expect(await dockOpen(f), `${from}: the frame's own dock stays shut`).toBe(false);
      expect(await menuOpen(f), `${from}: and the frame's menu`).toBe(false);
      await dock(page).locator('[data-dock-tab="pins"]').tap();
      await expect(rows(page), `${from}: the dock's Pins are the frame bar's`).toHaveCount(framePins);
      // Escape: focus goes back to the view button INSIDE the frame.
      await page.keyboard.press('Escape');
      await expect.poll(() => dockOpen(page)).toBe(false);
      await expect.poll(() => f.evaluate(() => document.activeElement && document.activeElement.id)).toBe('switcher-toggle');
      expect(await page.evaluate(() => document.activeElement!.tagName)).toBe('IFRAME');
      // The claim was posted by the top window only.
      const cs = await claims(page);
      expect(cs.map((c) => c.m), `${from}: one claim pair`).toEqual([CLAIM_ON, CLAIM_OFF]);
      expect(cs.every((c) => c.top), `${from}: posted by the top window`).toBe(true);

      // Unpin (from the frame's bar again), so the next round starts clean.
      await fl.locator('#switcher-more-btn').tap();
      await fl.locator('#switcher-pin').tap();
      await expect(fl.locator('#switcher-pin')).toHaveAttribute('data-pinned', 'false');

      // A row tap navigates the TOP window: history +1, no frame left.
      const n0 = await page.evaluate(() => history.length);
      await fl.locator('#switcher-toggle').tap();
      await expect.poll(() => dockOpen(page)).toBe(true);
      await dock(page).locator('[data-dock-tab="all"]').tap();
      const row = rows(page).filter({ has: page.locator('[data-dock-label]', { hasText: sess(6) + ':' }) });
      await Promise.all([page.waitForURL((u) => u.searchParams.get('session') === sess(6)), row.tap()]);
      await expect(page.locator('#mv-status')).toHaveText('Live');
      expect(await page.evaluate(() => history.length), `${from}: one entry`).toBe(n0 + 1);
      await expect(frameEl(page), `${from}: no frame left`).toHaveCount(0);
    }
    await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
  });

  test('22: an older parent page (no musterDock): the frame\'s button opens the frame\'s own view menu and no dock; control: with musterDock, the top dock', async ({ page }) => {
    test.setTimeout(90_000);
    await ready(page);
    await openOwn(page);
    expect(await coarse(page)).toBe(true);
    await page.locator('#view-terminal').tap();
    await expect(frameEl(page)).toBeVisible();
    const f = (await (await frameEl(page).elementHandle())!.contentFrame())!;
    const fl = page.frameLocator('iframe[data-muster-view]');
    await expect(fl.locator('#switcher-toggle')).toBeVisible();
    // The parent loses its dock, as a page from before it would have none.
    await page.evaluate(() => { (window as any).__dock = (window as any).musterDock; delete (window as any).musterDock; });
    await fl.locator('#switcher-toggle').tap();
    await expect.poll(() => menuOpen(f), "the frame's view menu opens").toBe(true);
    expect(await dockOpen(page), 'no dock in the top document').toBe(false);
    expect(await dockOpen(f), "and none in the frame's").toBe(false);
    expect(await f.evaluate(() => document.querySelectorAll('dialog[open]').length) + (await page.evaluate(() => document.querySelectorAll('dialog[open]').length))).toBe(0);
    await fl.locator('[data-switcher-pick="all"]').tap();
    await expect.poll(() => menuOpen(f)).toBe(false);
    // CONTROL: with musterDock back, the same tap opens the top dock.
    await page.evaluate(() => { (window as any).musterDock = (window as any).__dock; });
    await fl.locator('#switcher-toggle').tap();
    await expect.poll(() => dockOpen(page), 'the top dock opens').toBe(true);
    expect(await menuOpen(f)).toBe(false);
    await page.keyboard.press('Escape');
    await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
  });

  test('18: Recent holds 30 — 35 visits, two to windows since gone: the bar shows 8, the dock 30 in visit order, its first 8 the bar\'s, the gone ones absent; control: 5 visits, 5 in both', async ({ page }) => {
    test.setTimeout(150_000);
    await ready(page);
    expect(await coarse(page)).toBe(true);
    const clear = () => execFileSync('go', ['run', './e2e/recentsfix', 'clear'], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();
    const barHrefs = () => page.locator('#session-switcher [data-switcher-list="recent"] a[data-switcher-entry]').evaluateAll((es) => es.map((e) => e.getAttribute('href')!));
    const dockRecent = async () => {
      await toggle(page).tap();
      await expect.poll(() => dockOpen(page)).toBe(true);
      await dock(page).locator('[data-dock-tab="recent"]').tap();
      const hs = await rows(page).evaluateAll((es) => es.map((e) => e.getAttribute('href')!));
      await page.keyboard.press('Escape');
      await expect.poll(() => dockOpen(page)).toBe(false);
      return hs;
    };
    const sessOf = (h: string) => new URL(h, origin).searchParams.get('session');

    // CONTROL: 5 visits.
    clear();
    for (let i = 2; i <= 6; i++) await visit(page, sess(i));
    await page.goto(viewOf(OWN));
    await expect(page.locator('#session-switcher')).toBeVisible();
    expect((await barHrefs()).map(sessOf), 'the bar, 5 visits').toEqual([6, 5, 4, 3, 2].map(sess));
    expect((await dockRecent()).map(sessOf), 'the dock, 5 visits').toEqual([6, 5, 4, 3, 2].map(sess));

    // 35 visits, dk02…dk36, then two of them gone (dk30, dk12).
    expect(Number(clear()), 'the control\'s visits were cleared').toBeGreaterThanOrEqual(5);
    for (let i = 2; i <= 36; i++) await visit(page, sess(i));
    const gone = [sess(30), sess(12)];
    writeFileAtomic(windowsFile, windowLines.filter((l) => !gone.some((g) => l.startsWith(g + '|'))).join('\n') + '\n');
    try {
      await expect
        .poll(async () => { const html = await (await page.request.get(origin + '/ui/nav/agents')).text(); return gone.filter((g) => html.includes(`session=${g}&`)).length; }, { message: 'the two windows are gone from the list', timeout: 20_000 })
        .toBe(0);
      await page.goto(viewOf(OWN));
      await expect(page.locator('#session-switcher')).toBeVisible();
      const want: string[] = [];
      for (let i = 36; i >= 2 && want.length < 30; i--) if (!gone.includes(sess(i))) want.push(sess(i));
      const bar = (await barHrefs()).map(sessOf);
      expect(bar, 'the bar shows exactly 8, newest first').toEqual(want.slice(0, 8));
      const d = (await dockRecent()).map(sessOf);
      expect(d, 'the dock shows 30 in visit order, the gone ones absent').toEqual(want);
      expect(d.slice(0, 8), "its first 8 are the bar's").toEqual(bar);
      await expect(dock(page).locator('[data-dock-tab="recent"] [data-dock-count]')).toHaveText('30');
    } finally {
      writeFileAtomic(windowsFile, windowLines.join('\n') + '\n');
    }
  });
});

// ---------------------------------------------------------------------------
// THE BACK/FORWARD CACHE: a browser that restores pages (lib/bfcache.ts)
// ---------------------------------------------------------------------------
test.describe('phone 390×844 touch, back/forward cache on', () => {
  let browser: Browser;
  test.beforeAll(async ({ playwright }) => {
    browser = await launchWithBfcache(playwright.chromium);
  });
  test.afterAll(async () => {
    await browser.close();
  });

  test('9: a row tap closes the dock BEFORE the page is stored; a page stored with the dock open comes back shut (pageshow.persisted)', async () => {
    test.setTimeout(120_000);
    const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
    await stubBridge(ctx);
    // What the dock was as each page was stored, and how each page came back.
    await ctx.addInitScript(() => {
      window.addEventListener('pagehide', (e) => {
        const d = document.getElementById('switcher-dock') as HTMLDialogElement | null;
        try { sessionStorage.setItem('dock-at-hide', JSON.stringify({ open: !!d && d.open, persisted: e.persisted })); } catch (x) {}
      });
      window.addEventListener('pageshow', (e) => ((window as any).__persisted = e.persisted));
    });
    const page = await ctx.newPage();
    // 🔴 EACH HALF STARTS FROM A FRESH LOAD, AND ASSERTS THE TOUCH LAYOUT FIRST:
    // measured here, a page this browser restores from its back/forward cache
    // no longer matches (pointer: coarse) — the context's touch emulation is
    // not re-applied to it, so its view button opens the MENU, as a mouse's
    // would. A real phone's pointer does not change on a restore; that is the
    // harness's, so no tap is made on a restored page.
    const fresh = async () => {
      await page.goto(viewOf(OWN));
      await expect(page.locator('#session-switcher')).toBeVisible();
      expect(await coarse(page), 'a fresh load is the touch layout').toBe(true);
    };
    try {
      await ready(page);
      await openOwn(page);
      const own = page.url();

      // A page stored WITH the dock open (left by something other than a
      // row): restored, it is shut, and its claim released.
      await fresh();
      await page.evaluate(() => ((window as any).__claims = []));
      await toggle(page).tap();
      await expect.poll(() => dockOpen(page)).toBe(true);
      await page.goto(viewOf(sess(3)));
      expect(JSON.parse((await page.evaluate(() => sessionStorage.getItem('dock-at-hide')))!), 'stored with the dock OPEN').toEqual({ open: true, persisted: true });
      await page.goBack({ waitUntil: 'commit' });
      await expect.poll(() => page.evaluate(() => (window as any).__persisted), 'restored from the back/forward cache').toBe(true);
      expect(page.url()).toBe(own);
      await expect.poll(() => dockOpen(page), 'restored, the dock is shut').toBe(false);
      expect(await page.evaluate(() => [document.querySelectorAll('[inert]').length, !!document.querySelector(':modal')]), 'nothing inert, nothing modal').toEqual([0, false]);
      // Its claim: posted at open, and released (as the page was hidden, and
      // by the close on the restore).
      const cs = (await claims(page)).map((c) => c.m);
      expect(cs[0]).toBe(CLAIM_ON);
      expect(cs[cs.length - 1]).toBe(CLAIM_OFF);

      // A row tap: the dock is shut BEFORE the page goes into the cache.
      await fresh();
      await toggle(page).tap();
      await expect.poll(() => dockOpen(page)).toBe(true);
      const row = rows(page).filter({ has: page.locator('[data-dock-label]', { hasText: sess(2) + ':' }) });
      await Promise.all([page.waitForURL((u) => u.searchParams.get('session') === sess(2)), row.tap()]);
      expect(JSON.parse((await page.evaluate(() => sessionStorage.getItem('dock-at-hide')))!), 'stored in the cache with the dock shut').toEqual({ open: false, persisted: true });
      await page.goBack({ waitUntil: 'commit' });
      await expect.poll(() => page.evaluate(() => (window as any).__persisted), 'restored from the back/forward cache').toBe(true);
      expect(page.url()).toBe(own);
      expect(await dockOpen(page)).toBe(false);
      await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
    } finally {
      await ctx.close();
    }
  });
});

// ---------------------------------------------------------------------------
// THE MOUSE LAYOUT: a fine pointer, narrow and wide — the view menu, no dock
// ---------------------------------------------------------------------------
for (const vp of [
  { name: 'narrow mouse 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: false, isMobile: false } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);
    test('2/19: the view button and ArrowUp open the view menu, not the dock', async ({ page }) => {
      await ready(page);
      await openOwn(page);
      expect(await coarse(page), 'this context is the mouse layout').toBe(false);
      expect(await dock(page).evaluate((d) => getComputedStyle(d).display)).toBe('none');
      await expect(toggle(page)).toHaveAttribute('aria-haspopup', 'menu');
      await expect(toggle(page)).toHaveAttribute('aria-controls', 'switcher-views');
      await toggle(page).click();
      await expect.poll(() => menuOpen(page)).toBe(true);
      expect(await dockOpen(page), 'no dock in the mouse layout').toBe(false);
      await page.keyboard.press('Escape');
      await expect.poll(() => menuOpen(page)).toBe(false);
      await toggle(page).focus();
      await page.keyboard.press('ArrowUp');
      await expect.poll(() => menuOpen(page)).toBe(true);
      expect(await dockOpen(page)).toBe(false);
      await page.keyboard.press('Escape');
    });
  });
}
