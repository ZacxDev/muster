import { Browser, BrowserContext, Frame, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dockPort } from '../lib/env';
import { cc, MirrorRig, mkchat } from '../lib/mirrorhost';
import { launchWithBfcache } from '../lib/bfcache';
import { coarse } from '../lib/switcher';
import { touchDrag } from '../lib/touch';

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
//   5       the backdrop, the close button and Escape close it; "/" opens no
//           palette while it is open;
//   G       the 44px grip CLOSES it; there is one snap, half, and nothing
//           reaches another (operator decision: no full screen);
//   S       a clearly horizontal swipe on the list walks the tabs, through
//           the arrow keys' path; a vertical scroll, a diagonal and a short
//           drag do not; a left-edge swipe in it never opens the sidebar;
//   13      the filter: name, display name, tmux session and host, the counts,
//           "No sessions match", ×, Escape twice, reset on reopen; the keyboard;
//   9 / 9b  closing never touches history, on a boosted page too; a row tap
//           is one real load, the dock closed BEFORE it (back/forward cache);
//           the shell's back-claim pair;
//   15      reduced motion;
//   18      Recent past the bar's 8 (the 30 itself is pinned in Go);
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
const viewOf = (s: string) => origin + rig.viewPath(s) + '&window=' + encodeURIComponent('@1');
const OWN = sess(1);
// One window shown by a CODENAME (the host's labels file), so its tmux session
// is no part of its name: a filter that finds it by "dk29" found it by the
// session, a word, not by the label.
const MAPPED = sess(29);
const CODENAME = 'quillon';

test.beforeAll(async () => {
  test.setTimeout(240_000);
  await rig.start();
  const now = Math.floor(Date.now() / 1000);
  const windowLines = Array.from({ length: N }, (_, k) => {
    const s = sess(k + 1);
    const tr = rig.transcript(cc.prompt(`the prompt of ${s}`) + cc.reply(`Answered in ${s}.`), 'w' + s);
    return `${s}|@1|1|1|claude|${now - 60 * (k + 1)}|${tr}`;
  });
  const windowsFile = rig.transcript(windowLines.join('\n') + '\n', 'windows');
  const labelsFile = rig.transcript(JSON.stringify({ [MAPPED]: CODENAME }), 'labels');
  await rig.startHost({ MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile, MUSTER_MIRROR_E2E_REFRESH: '300ms', MUSTER_MIRROR_LABELS_FILE: labelsFile });
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
const tab = (p: Page, v: string) => p.locator(`#switcher-dock [data-dock-tab="${v}"]`);
const filter = (p: Page) => p.locator('#dock-filter');
const clearBtn = (p: Page) => p.locator('#switcher-dock [data-dock-filter-clear]');

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

  test('2/19/5: the view button opens the dock, not the menu, on the shown view; a tab moves the bar; backdrop, ×, Escape close it; "/" stands down', async ({ page }) => {
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

    // Half: 60% of the visual viewport (the grip closes it: case G below).
    const vvh = await page.evaluate(() => window.visualViewport!.height);
    await dock(page).evaluate((d) => Promise.all(d.getAnimations().map((a) => a.finished)));
    expect(Math.abs((await dock(page).boundingBox())!.height - Math.round(0.6 * vvh)), 'half is 60% of the visual viewport').toBeLessThanOrEqual(2);

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

  // The sheet's sides are padded by the safe area (a notched phone held
  // sideways). A tap there lands on the dialog element itself, as a backdrop
  // tap does; only the point tells them apart. This emulator's insets are 0,
  // so the case gives the sheet a 24px side padding of its own.
  test('5b: a tap on the sheet\'s own side padding keeps it open; control: a tap on the backdrop closes it', async ({ page }) => {
    test.setTimeout(90_000);
    await ready(page);
    await openOwn(page);
    expect(await coarse(page)).toBe(true);
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    await dock(page).evaluate((d) => { (d as HTMLElement).style.paddingLeft = '24px'; (d as HTMLElement).style.paddingRight = '24px'; });
    await dock(page).evaluate((d) => Promise.all(d.getAnimations().map((a) => a.finished)));
    const box = (await dock(page).boundingBox())!;
    const x = box.x + 12, y = box.y + box.height / 2;
    // REACHED: the point is inside the sheet's box and hits the dialog itself.
    expect(await page.evaluate(([px, py]) => document.elementFromPoint(px, py)?.id, [x, y]), 'the padding hits the dialog element').toBe('switcher-dock');
    await page.touchscreen.tap(x, y);
    await page.waitForTimeout(400);
    expect(await dockOpen(page), 'a tap on the sheet\'s padding closed it').toBe(true);
    // CONTROL: the backdrop, above the sheet, still closes it.
    expect(await page.evaluate(([px, py]) => document.elementFromPoint(px, py)?.id, [195, box.y / 2]), 'the backdrop hits the dialog element too').toBe('switcher-dock');
    await page.touchscreen.tap(195, box.y / 2);
    await expect.poll(() => dockOpen(page)).toBe(false);
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

  // G: ONE SNAP, AND THE GRIP CLOSES (operator decision: "drop full screen
  // entirely"). The grip, the 44px button at the sheet's top, used to toggle
  // half and full; a tap on it now closes the dock, through the one cleanup
  // point (the claim released, focus back on the view button). In every state
  // this case visits the sheet is the half snap, and even a page that set the
  // old full attribute by hand gets no other height (its rule is gone).
  test('G: a tap on the grip closes the dock; nothing reaches a full snap', async ({ page }) => {
    test.setTimeout(90_000);
    await ready(page);
    await stubBridge(page);
    await openOwn(page);
    expect(await coarse(page), 'the touch layout').toBe(true);
    const vvh = await page.evaluate(() => window.visualViewport!.height);
    const half = Math.round(0.6 * vvh);
    const grip = dock(page).locator('[data-dock-grip]');
    const atHalf = async (when: string) => {
      await dock(page).evaluate((d) => Promise.all(d.getAnimations().map((a) => a.finished)));
      const h = (await dock(page).boundingBox())!.height;
      expect(Math.abs(h - half), `${when}: the sheet is the half snap (${h}px, want ${half})`).toBeLessThanOrEqual(2);
      expect(await dock(page).getAttribute('data-snap'), `${when}: no snap attribute`).toBeNull();
    };

    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    await atHalf('opened');
    expect(Math.round((await grip.boundingBox())!.height), 'the grip is 44px tall').toBe(44);
    await expect(grip).toHaveAccessibleName('Close sessions');
    // The states the dock can be in: another tab, a filter typed, nothing matched.
    await dock(page).locator('[data-dock-tab="all"]').tap();
    await atHalf('the All tab');
    await filter(page).fill('dk0');
    await atHalf('filtered');
    await filter(page).fill('no-such-session-anywhere');
    await atHalf('nothing matches');
    await filter(page).fill('');
    // The old full snap, set by hand: no rule draws it any more.
    await dock(page).evaluate((d) => d.setAttribute('data-snap', 'full'));
    await page.waitForTimeout(50);
    expect(Math.abs((await dock(page).boundingBox())!.height - half), 'data-snap="full" draws nothing taller').toBeLessThanOrEqual(2);
    await dock(page).evaluate((d) => d.removeAttribute('data-snap'));

    // The tap: closed, the claim released, focus on the view button.
    await page.evaluate(() => ((window as any).__claims = []));
    await grip.tap();
    await expect.poll(() => dockOpen(page), 'a tap on the grip closes the dock').toBe(false);
    await expect(toggle(page)).toBeFocused();
    await expect(toggle(page)).toHaveAttribute('aria-expanded', 'false');
    await expect.poll(async () => (await claims(page)).map((c) => c.m), 'the claim released').toEqual([CLAIM_OFF]);
    // Opened again: still the half snap.
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    await atHalf('reopened');
    await grip.tap();
    await expect.poll(() => dockOpen(page)).toBe(false);
    await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
  });

  // S: THE TAB SWIPE, ON THE DOCK'S LIST ONLY. The page's tab-switch swipe was
  // removed (6424ce7d) because it owned every tabpanel and the horizontal axis
  // the recents strip scrolls on. The dock's list scrolls only vertically and
  // nothing in it scrolls sideways (asserted below), so the horizontal axis
  // there is free; the swipe is bound to the list alone (TestNoTabSwitchSwipe).
  // Real touches (CDP, lib/touch.ts), through Chrome's own gesture handling.
  test('S: a horizontal swipe on the list walks the tabs (aria-selected, focus, the bar); a vertical scroll, a diagonal and a short drag do not; a left-edge swipe in the dock opens no sidebar', async ({ page }) => {
    test.setTimeout(120_000);
    await ready(page);
    // Enough visits that Recent's list is taller than the sheet.
    for (let i = 20; i >= 2; i--) await visit(page, sess(i));
    await page.goto(viewOf(OWN));
    await expect(page.locator('#mv-status')).toHaveText('Live');
    expect(await coarse(page), 'the touch layout').toBe(true);
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    await dock(page).evaluate((d) => Promise.all(d.getAnimations().map((a) => a.finished)));
    await expect(tab(page, 'recent')).toHaveAttribute('aria-selected', 'true');
    const lb = (await page.locator('#dock-list').boundingBox())!;
    const y = lb.y + Math.min(120, lb.height / 2);
    // POSITIVE CONTROL: the swipe's points are on the list.
    for (const x of [lb.x + lb.width - 40, lb.x + 40]) {
      expect(await page.evaluate(([px, py]) => !!document.elementFromPoint(px, py)?.closest('#dock-list'), [x, y]), `(${x}, ${y}) is on the list`).toBe(true);
    }
    // The list leaves sideways moves to the swipe, and a pinch to the browser.
    expect(await page.locator('#dock-list').evaluate((l) => getComputedStyle(l).touchAction), 'pan-y and pinch-zoom').toBe('pan-y pinch-zoom');
    // Nothing on the list scrolls sideways, so the horizontal axis is free.
    expect(await page.evaluate(() => Array.from(document.querySelectorAll('#dock-list, #dock-list *')).filter((e) => {
      const ox = getComputedStyle(e).overflowX;
      return (ox === 'auto' || ox === 'scroll') && e.scrollWidth > e.clientWidth + 1;
    }).length), 'no sideways scroller on the list').toBe(0);
    const shown = async (v: string, why: string) => {
      await expect(tab(page, v), why).toHaveAttribute('aria-selected', 'true');
      await expect(tab(page, v)).toHaveAttribute('tabindex', '0');
      await expect(tab(page, v), `${why}: focus on the tab, as the arrow keys put it`).toBeFocused();
      await expect(page.locator('#dock-list')).toHaveAttribute('aria-labelledby', 'dock-tab-' + v);
      await expect(page.locator('#session-switcher'), `${why}: the bar moved too`).toHaveAttribute('data-switcher-view', v);
      expect(await page.locator('[data-dock-tab][aria-selected="true"]').count()).toBe(1);
    };
    const left = () => touchDrag(page, { x: lb.x + lb.width - 40, y }, { x: lb.x + 40, y: y + 6 });
    const right = () => touchDrag(page, { x: lb.x + 40, y }, { x: lb.x + lb.width - 40, y: y + 6 });

    await left();
    await shown('waiting', 'a left swipe: the next tab');
    await left();
    await shown('pins', 'again: the one after');
    await right();
    await shown('waiting', 'a right swipe: the previous tab');
    await right();
    await shown('recent', 'back to the first');
    // The ends are ends: no wrap.
    await right();
    await page.waitForTimeout(150);
    await shown('recent', 'a right swipe on the first tab stays');

    // A VERTICAL SCROLL of the long list, drifting sideways by more than the
    // swipe's minimum: it scrolls, and the tab stays.
    expect(await page.locator('#dock-list').evaluate((l) => l.scrollHeight > l.clientHeight + 100), 'Recent overflows the sheet').toBe(true);
    const top0 = await page.locator('#dock-list').evaluate((l) => l.scrollTop);
    await touchDrag(page, { x: lb.x + lb.width - 60, y: lb.y + lb.height - 30 }, { x: lb.x + lb.width - 150, y: lb.y + 40 });
    await page.waitForTimeout(250);
    expect(await page.locator('#dock-list').evaluate((l) => l.scrollTop), 'the drag scrolled the list').toBeGreaterThan(top0 + 50);
    await expect(tab(page, 'recent'), 'a vertical scroll changed the tab').toHaveAttribute('aria-selected', 'true');
    // A DIAGONAL: more across than down, but not 1.5 times more.
    await touchDrag(page, { x: lb.x + lb.width - 40, y: lb.y + 200 }, { x: lb.x + lb.width - 160, y: lb.y + 110 });
    await page.waitForTimeout(250);
    await expect(tab(page, 'recent'), 'a diagonal changed the tab').toHaveAttribute('aria-selected', 'true');
    // A SHORT sideways drag, under the minimum.
    await touchDrag(page, { x: lb.x + 200, y }, { x: lb.x + 160, y });
    await page.waitForTimeout(250);
    await expect(tab(page, 'recent'), 'a short drag changed the tab').toHaveAttribute('aria-selected', 'true');
    // CONTROL: the same place still swipes.
    await left();
    await shown('waiting', 'control: a real swipe after them');

    // THE SIDEBAR'S EDGE SWIPE stands down inside the dock (data-own-swipe):
    // from x = 4 on the open dock, rightwards, well past its 45px.
    await expect(page.locator('#sidebar-root')).not.toHaveAttribute('data-open', 'true');
    expect(await page.evaluate(([py]) => !!document.elementFromPoint(4, py)?.closest('#switcher-dock'), [y]), 'x = 4 is on the dock').toBe(true);
    await touchDrag(page, { x: 4, y }, { x: 220, y: y + 4 });
    await page.waitForTimeout(250);
    await expect(page.locator('#sidebar-root'), 'an edge swipe in the dock opened the sidebar').not.toHaveAttribute('data-open', 'true');
    // CONTROL: shut, the same edge swipe on the page opens the sidebar.
    await page.keyboard.press('Escape');
    await expect.poll(() => dockOpen(page)).toBe(false);
    await expect(dock(page)).toBeHidden();
    await touchDrag(page, { x: 4, y }, { x: 220, y: y + 4 });
    await expect(page.locator('#sidebar-root'), 'control: the edge swipe opens the sidebar off the dock').toHaveAttribute('data-open', 'true');
    await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
  });

  // 13: THE FILTER. It matches the palette's fields through the palette's own
  // rank (musterPaletteMatch over the pills' data-switcher-code/-title/-words/
  // -prev): a name (the label), an agent's DISPLAY name (its title only: its
  // codename spells it with hyphens), a tmux session (a word only: that
  // window is shown by a codename), and a host (a word: the mirror host's
  // whole name matches every window and no agent, "cluster" every agent and no
  // window). Each tab's count is its matches.
  test('13: the filter narrows the rows by name, display name, tmux session and host; the counts follow; "No sessions match"; × and Escape clear it; a second Escape closes; reopened, it is empty', async ({ page }) => {
    test.setTimeout(120_000);
    await ready(page);
    await openOwn(page);
    expect(await coarse(page), 'the touch layout').toBe(true);
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    // Opening does not raise a keyboard: focus is on the tab, not the field.
    await expect(tab(page, 'recent')).toBeFocused();
    await expect(filter(page)).not.toBeFocused();
    await expect(filter(page)).toHaveAccessibleName('Filter sessions');
    await expect(filter(page)).toHaveAttribute('type', 'search');
    expect(await filter(page).evaluate((e) => getComputedStyle(e).fontSize), '16px: no zoom on focus').toBe('16px');
    await expect(clearBtn(page)).toBeHidden();
    await tab(page, 'all').tap();
    const allRows = await rows(page).count();
    expect(allRows, 'All holds the windows and the agents').toBeGreaterThan(N);
    const labels = () => rows(page).locator('[data-dock-label]').allTextContents();
    const counts = () => page.locator('[data-dock-count]').evaluateAll((es) => Object.fromEntries(es.map((e) => [e.getAttribute('data-dock-count'), Number(e.textContent)])));
    const unfiltered = await counts();
    expect(unfiltered.all).toBe(allRows);

    // A NAME.
    await filter(page).tap();
    await page.keyboard.type('dk07');
    await expect.poll(labels, 'by name').toEqual([sess(7) + ':1']);
    await expect(clearBtn(page)).toBeVisible();
    expect((await counts()).all, 'All counts its matches').toBe(1);
    // A TMUX SESSION, shown by a codename: found by the session alone.
    await filter(page).fill(MAPPED);
    await expect.poll(labels, 'by tmux session').toEqual([CODENAME + ':1']);
    expect((await labels()).some((l) => l.includes(MAPPED)), 'the label does not spell the session').toBe(false);
    // A DISPLAY NAME: the agent's title, not its codename.
    await filter(page).fill('dock threads');
    await expect.poll(labels, 'by display name').toEqual(['Dock threads']);
    // A HOST: the mirror host's name is every window and no agent…
    await filter(page).fill(rig.host);
    await expect.poll(() => rows(page).count(), 'by the mirror host').toBe(N);
    expect((await rows(page).locator('[data-dock-host]').allTextContents()).every((h) => h === rig.host), 'every row is on that host').toBe(true);
    // …and "cluster" every dispatched agent and no window.
    await filter(page).fill('cluster');
    await expect.poll(async () => (await labels()).includes('Dock threads'), 'by the agents’ host').toBe(true);
    expect((await labels()).some((l) => /^dk\d\d:1$/.test(l) || l === CODENAME + ':1'), 'no window is on the cluster').toBe(false);
    expect((await counts()).all).toBe(await rows(page).count());

    // THE COUNTS, per tab: each tab's count is its own rows under the filter.
    await filter(page).fill('dk0');
    const c = await counts();
    for (const v of ['recent', 'waiting', 'pins', 'all']) {
      await tab(page, v).tap();
      await expect(filter(page), `${v}: switching tabs keeps the query`).toHaveValue('dk0');
      expect(await rows(page).count(), `${v}: the count is its rows`).toBe(c[v]);
      expect(c[v], `${v}: filtered, at most the unfiltered`).toBeLessThanOrEqual(unfiltered[v]);
    }
    expect(c.all, 'All under "dk0": dk01–dk09 (dk01 the page itself, drawn current)').toBe(9);

    // NOTHING MATCHES: the line, every count 0.
    await filter(page).fill('no-such-session-anywhere');
    await expect(rows(page)).toHaveCount(0);
    await expect(dock(page).locator('[data-dock-empty]')).toBeVisible();
    await expect(dock(page).locator('[data-dock-empty]')).toHaveText('No sessions match');
    expect(Object.values(await counts()), 'every count 0').toEqual([0, 0, 0, 0]);

    // ×: cleared, every row back, the × hidden again.
    await clearBtn(page).tap();
    await expect(filter(page)).toHaveValue('');
    await expect(rows(page)).toHaveCount(allRows);
    await expect(clearBtn(page)).toBeHidden();
    await expect(dock(page).locator('[data-dock-empty]')).toBeHidden();
    expect(await counts()).toEqual(unfiltered);

    // ESCAPE: the first clears a typed filter and leaves the dock open…
    await filter(page).tap();
    await page.keyboard.type('dk1');
    await expect.poll(() => rows(page).count()).toBeLessThan(allRows);
    await page.keyboard.press('Escape');
    await expect(filter(page), 'the first Escape cleared it').toHaveValue('');
    expect(await dockOpen(page), 'and left the dock open').toBe(true);
    await expect(rows(page)).toHaveCount(allRows);
    // …the second closes it.
    await page.keyboard.press('Escape');
    await expect.poll(() => dockOpen(page), 'the second Escape closed it').toBe(false);
    // 🔴 THE SAME WITH FOCUS OFF THE FIELD. In the field, Chromium's own
    // search-field Escape also clears it, so the case above passes without the
    // dock's handler (mutation-checked); on a tab only the dock's handler does.
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    await filter(page).fill('dk1');
    await tab(page, 'all').focus();
    await expect(tab(page, 'all')).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(filter(page), 'Escape on a tab cleared the filter').toHaveValue('');
    expect(await dockOpen(page), 'Escape on a tab with a filter typed kept the dock open').toBe(true);
    await page.keyboard.press('Escape');
    await expect.poll(() => dockOpen(page), 'then closed it').toBe(false);

    // RESET ON CLOSE: typed, closed by ×, reopened: empty, every row.
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    await tab(page, 'all').tap();
    await filter(page).fill('dk07');
    await expect(rows(page)).toHaveCount(1);
    await dock(page).locator('[data-dock-close]').tap();
    await expect.poll(() => dockOpen(page)).toBe(false);
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    await expect(filter(page), 'reopened, the filter is empty').toHaveValue('');
    await expect(rows(page)).toHaveCount(allRows);
    await page.keyboard.press('Escape');
    await expect.poll(() => dockOpen(page)).toBe(false);
    await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
  });

  // 13k: THE KEYBOARD. Where a soft keyboard shrinks only the VISUAL viewport
  // (the agent page: no resizes-content), the sheet sits on it, so the filter
  // being typed into stays above it. The keyboard is emulated by a visual
  // viewport whose height the case sets (installed before the page's scripts).
  test('13k: with the visual viewport shortened by a keyboard, the sheet sits on top of it at half of what is left', async ({ page }) => {
    test.setTimeout(90_000);
    await ready(page);
    await page.addInitScript(() => {
      const real = window.visualViewport!;
      const fake = new EventTarget() as any;
      for (const k of ['width', 'offsetLeft', 'pageLeft', 'pageTop', 'scale']) Object.defineProperty(fake, k, { get: () => (real as any)[k] });
      Object.defineProperty(fake, 'height', { get: () => (window as any).__vvH ?? real.height });
      Object.defineProperty(fake, 'offsetTop', { get: () => 0 });
      real.addEventListener('resize', () => fake.dispatchEvent(new Event('resize')));
      Object.defineProperty(window, 'visualViewport', { value: fake, configurable: true });
    });
    await openOwn(page);
    expect(await coarse(page)).toBe(true);
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    await dock(page).evaluate((d) => Promise.all(d.getAnimations().map((a) => a.finished)));
    const inner = await page.evaluate(() => window.innerHeight);
    expect(Math.round((await dock(page).boundingBox())!.y + (await dock(page).boundingBox())!.height), 'CONTROL: no keyboard, on the bottom').toBe(inner);
    await page.evaluate(() => { (window as any).__vvH = 420; window.visualViewport!.dispatchEvent(new Event('resize')); });
    await page.waitForTimeout(100);
    const b = (await dock(page).boundingBox())!;
    expect(Math.abs(b.y + b.height - 420), `the sheet's foot is on the keyboard's top (${b.y + b.height})`).toBeLessThanOrEqual(2);
    const head = await dock(page).locator('[data-dock-header]').evaluate((e) => (e as HTMLElement).offsetHeight);
    expect(Math.abs(b.height - Math.max(Math.round(0.6 * 420), head + 3 * 56)), 'half of what is left, never under the header and three rows').toBeLessThanOrEqual(2);
    const fb = (await filter(page).boundingBox())!;
    expect(fb.y >= 0 && fb.y + fb.height <= 420, 'the filter is above the keyboard').toBe(true);
    await page.evaluate(() => { delete (window as any).__vvH; window.visualViewport!.dispatchEvent(new Event('resize')); });
    await page.keyboard.press('Escape');
    await expect.poll(() => dockOpen(page)).toBe(false);

    // 🔴 OPENED FROM THE VIEW SWITCH'S FRAME, the sheet still measures ITS OWN
    // window — the top document it is drawn in (switcher_dock.go measure) —
    // not the frame's that opened it: the top window's keyboard moves it, and
    // the frame's viewport (left full height here) does not.
    await page.locator('#view-terminal').tap();
    await expect(frameEl(page)).toBeVisible();
    const fl = page.frameLocator('iframe[data-muster-view]');
    await expect(fl.locator('#switcher-toggle')).toBeVisible();
    await fl.locator('#switcher-toggle').tap();
    await expect.poll(() => dockOpen(page), 'the top dock opens from the frame').toBe(true);
    await dock(page).evaluate((d) => Promise.all(d.getAnimations().map((a) => a.finished)));
    await page.evaluate(() => { (window as any).__vvH = 420; window.visualViewport!.dispatchEvent(new Event('resize')); });
    await page.waitForTimeout(100);
    const fb2 = (await dock(page).boundingBox())!;
    expect(Math.abs(fb2.y + fb2.height - 420), `from the frame: the sheet's foot is on the top window's keyboard (${fb2.y + fb2.height})`).toBeLessThanOrEqual(2);
    await page.evaluate(() => { delete (window as any).__vvH; window.visualViewport!.dispatchEvent(new Event('resize')); });
    await page.keyboard.press('Escape');
    await expect.poll(() => dockOpen(page)).toBe(false);
  });

  // 13m: CLOSED WHILE THE KEYBOARD MARK IS SET. Typing in the dock's filter
  // is typing in a text field, so with the keyboard up the bar's keyboard
  // mark (html[data-keyboard], switcherKeyboardJS) is set, and the Rendered
  // view's CSS then hides the bar — the dock's opener. Closed with focus still
  // in the filter (Escape twice; on iOS a tap on × does not move focus either),
  // focus must still land on the opener, visible: the cleanup clears the mark
  // before it focuses the opener. The keyboard is the 13k emulation.
  test('13m: closed while the keyboard mark is set, focus lands on the view button, visible', async ({ page }) => {
    test.setTimeout(90_000);
    await ready(page);
    await page.addInitScript(() => {
      const real = window.visualViewport!;
      const fake = new EventTarget() as any;
      for (const k of ['width', 'offsetLeft', 'pageLeft', 'pageTop', 'scale']) Object.defineProperty(fake, k, { get: () => (real as any)[k] });
      Object.defineProperty(fake, 'height', { get: () => (window as any).__vvH ?? real.height });
      Object.defineProperty(fake, 'offsetTop', { get: () => 0 });
      real.addEventListener('resize', () => fake.dispatchEvent(new Event('resize')));
      Object.defineProperty(window, 'visualViewport', { value: fake, configurable: true });
    });
    await openOwn(page);
    expect(await coarse(page)).toBe(true);
    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    await filter(page).tap();
    await page.keyboard.type('dk');
    await expect(filter(page)).toBeFocused();
    await page.evaluate(() => { (window as any).__vvH = 480; window.visualViewport!.dispatchEvent(new Event('resize')); });
    // REACHED: the mark is set, and it hides the opener.
    await expect(page.locator('html'), 'the keyboard mark is set while typing in the filter').toHaveAttribute('data-keyboard', '');
    await expect(toggle(page), 'the mark hides the opener').toBeHidden();
    await page.keyboard.press('Escape');
    await expect(filter(page)).toHaveValue('');
    await expect(filter(page), 'focus still in the filter').toBeFocused();
    await page.keyboard.press('Escape');
    await expect.poll(() => dockOpen(page)).toBe(false);
    await expect(toggle(page), 'the opener is visible again').toBeVisible();
    await expect(toggle(page), 'focus lands on the opener').toBeFocused();
    await page.evaluate(() => { delete (window as any).__vvH; window.visualViewport!.dispatchEvent(new Event('resize')); });
  });

  // The server half (30 kept, visit order, gone windows dropped) is
  // TestRecentHoldsThirtyForTheDock's; this is the script's: what it draws.
  test('18: the dock\'s Recent is the bar\'s 8, then the hidden RecentMore hrefs, each drawn from its All pill, in order', async ({ page }) => {
    test.setTimeout(120_000);
    await ready(page);
    expect(await coarse(page)).toBe(true);
    for (let i = 2; i <= 13; i++) await visit(page, sess(i));
    await page.goto(viewOf(OWN));
    await expect(page.locator('#session-switcher')).toBeVisible();
    const bar = await page.locator('#session-switcher [data-switcher-list="recent"] a[data-switcher-entry]').evaluateAll((es) => es.map((e) => e.getAttribute('href')!));
    expect(bar.length, 'the bar shows 8').toBe(8);
    const more = await page.evaluate(() => (document.querySelector('#session-switcher [data-switcher-recent-more]')?.getAttribute('data-switcher-recent-more') || '').split('\n').filter(Boolean));
    expect(more.length, 'twelve visits leave entries past the bar\'s 8').toBeGreaterThanOrEqual(4);
    // Each extra href's label, as its All pill draws it.
    const allLabel = await page.evaluate((hs) => hs.map((h) => {
      const a = Array.from(document.querySelectorAll('#session-switcher [data-switcher-list="all"] a[data-switcher-entry]')).find((x) => x.getAttribute('href') === h);
      return a ? a.querySelector('span.whitespace-nowrap')?.textContent ?? a.getAttribute('title') : null;
    }), more);
    expect(allLabel.every((l) => !!l), 'every extra href has an All pill').toBe(true);

    await toggle(page).tap();
    await expect.poll(() => dockOpen(page)).toBe(true);
    await dock(page).locator('[data-dock-tab="recent"]').tap();
    const hs = await rows(page).evaluateAll((es) => es.map((e) => e.getAttribute('href')));
    expect(hs, "the dock's Recent: the bar's 8, then the extras, in order").toEqual([...bar, ...more]);
    const labels = await rows(page).locator('[data-dock-label]').allTextContents();
    expect(labels.slice(8), 'each extra drawn from its All pill').toEqual(allLabel);
    await expect(dock(page).locator('[data-dock-tab="recent"] [data-dock-count]')).toHaveText(String(8 + more.length));
    await page.keyboard.press('Escape');
    await expect.poll(() => dockOpen(page)).toBe(false);
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
