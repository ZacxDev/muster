import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { BrowserContext, Page, Route } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, pluginChipPort, repoRoot } from '../lib/env';
import { cc, entryMatches, mirroredEntries, MirrorRig } from '../lib/mirrorhost';

// THE SESSION VIEWS' PREVIEW BUTTON AND PANEL (docs/plugins.md#the-session-view-chip),
// on the operator's click path: a REAL muster-server with the mirror plugin,
// plugins and MUSTER_PLUGIN_ORIGINS on, a REAL muster-mirror (`-tags
// e2emirror`) with a Claude Code window (@1) and a shell (@2), and a viewer
// plugin minted through the real /plugins form. What each window has to
// preview is PLAYED by the host's MUSTER_MIRROR_E2E_PREVIEWS_FILE (the real
// -preview source is previews.spec.ts's), and travels the real previews
// message to muster.
//
// ⚠ WHAT IS PLAYED: the plugin's ENTRY PAGE. Its URL is https under an
// invented zone, so the browser's requests to it are answered by page.route
// here — which, like a real plugin's entry route, REDEEMS the code with the
// plugin's own token (POST /plugin/v1/handoffs/redeem on the real server) and
// draws what it learned. So a landing proves the whole hand-off: the panel
// minted at click time, the browser went to the entry URL with muster_code and
// muster_plugin, and the code was good for this owner and this window.
//
// 🔴 ONLY THIS SPEC'S SERVER HAS MUSTER_PLUGIN_ORIGINS SET, and afterAll
// removes the plugins it made (pluginfix rmplugin), so no other spec on the
// shared database draws a Preview button.
const ZONE = 'https://*.viewer.example.net';
const ENTRY = 'https://w.viewer.example.net/__muster/enter';
const SANDBOX = 'allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals';
const OWNER = '1'; // the password-mode operator (as chief-succession.spec's fixture)

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

const rig = new MirrorRig(pluginChipPort, 'e2e-chip', { MUSTER_PLUGINS: '1', MUSTER_PLUGIN_ORIGINS: ZONE });
const origin = rig.origin;
const names: string[] = [];

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

const q = (win: string) => `host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent(win)}`;
const viewOf = (win: string) => `/mirror/view?${q(win)}`;
const termOf = (win: string) => `/mirror/terminal?${q(win)}`;
let previewsFile = '';
// playPreviews is what the host reports each window has to preview, as
// `<window> <offered> <open>` lines (e2emirror.go).
const playPreviews = (lines: string) => fs.writeFileSync(previewsFile, lines);

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
  const now = Math.floor(Date.now() / 1000);
  const tr = rig.transcript(cc.prompt('the prompt of the chip window') + cc.reply('Answered.'), 'one');
  const windows = rig.transcript(`work|@1|1|1|claude|${now - 60}|${tr}\nwork|@2|2|0|zsh|${now - 120}|\n`, 'windows');
  previewsFile = path.join(rig.dir, 'previews-played');
  playPreviews('@1 2 1\n@2 1 0\n');
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms', MUSTER_MIRROR_E2E_PREVIEWS_FILE: previewsFile });
});

test.afterAll(async () => {
  for (const n of names) fix('rmplugin', OWNER, n);
  await rig.stop();
});

// ready signs in and waits until the host lists the Claude window as one (a
// window not yet reported as one has no rendered view and redirects).
async function ready(page: Page): Promise<void> {
  await rig.login(page);
  await expect
    .poll(
      async () =>
        mirroredEntries(await (await page.request.get(origin + '/ui/nav/agents')).text()).filter(
          (e) => e.state === 'online' && e.markup.includes(`host=${encodeURIComponent(rig.host)}&`) && entryMatches(e.markup, 'data-mirror-claude'),
        ).length,
      { message: `the Claude window listed (muster-mirror log:\n${rig.log})`, timeout: 20_000 },
    )
    .toBe(1);
}

// mintViewer creates a viewer-only plugin through the real /plugins form and
// returns its token and id.
async function mintViewer(page: Page, label: string): Promise<{ token: string; id: number }> {
  const name = 'e2e-chip-' + Date.now().toString(36) + '-' + names.length;
  names.push(name);
  const form = new URLSearchParams({ name, label, data_scope: 'example', origin: ZONE, entry_url: ENTRY });
  form.append('capability', 'viewer');
  const r = await page.request.post(origin + '/plugins', {
    headers: { Origin: origin, 'Content-Type': 'application/x-www-form-urlencoded' },
    data: form.toString(),
  });
  const html = await r.text();
  expect(r.status(), html.slice(0, 400)).toBe(200);
  const token = /id="minted-plugin-token" readonly value="(mpl_[A-Za-z0-9_-]+)"/.exec(html)?.[1];
  expect(token, 'a plugin token was minted').toBeTruthy();
  const self = await page.request.get(origin + '/plugin/v1/self', { headers: { Authorization: `Bearer ${token}` } });
  const body = (await self.json()) as { plugin_id: number; capabilities: string[] };
  expect(body.capabilities).toEqual(['viewer']);
  return { token: token!, id: body.plugin_id };
}

// playEntry answers the plugin's entry URL in this context: it redeems the
// code with the plugin's token and draws the answer, and records each landing.
type Landing = { url: URL; redeemed: { status: number; body: any }; referer: string | undefined };
async function playEntry(context: BrowserContext, token: string): Promise<Landing[]> {
  const landings: Landing[] = [];
  await context.route('https://*.viewer.example.net/**', async (route: Route) => {
    const req = route.request();
    const url = new URL(req.url());
    const code = url.searchParams.get('muster_code') ?? '';
    const res = await fetch(origin + '/plugin/v1/handoffs/redeem', {
      method: 'POST',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ code }),
    });
    const body = await res.json().catch(() => ({}));
    landings.push({ url, redeemed: { status: res.status, body }, referer: req.headers()['referer'] });
    const target = body?.target?.mirror ?? {};
    await route.fulfill({
      status: 200,
      headers: { 'Content-Type': 'text/html; charset=utf-8', 'Referrer-Policy': 'no-referrer' },
      body: `<!doctype html><title>Fake plugin</title><h1 id="fake-entry">fake plugin entry</h1>
<p id="fake-redeem">${res.status} ${target.host ?? ''} ${target.window ?? ''}</p>
<p id="fake-opener"></p><p id="fake-referrer"></p>
<script>document.getElementById('fake-opener').textContent = 'opener:' + (window.opener === null ? 'none' : 'present');
document.getElementById('fake-referrer').textContent = 'referrer:' + (document.referrer || 'none');</script>`,
    });
  });
  return landings;
}

// buttonFor polls until the button's own route answers the button for win
// (present) or nothing (absent): the host's report reached muster.
async function buttonFor(page: Page, win: string, present: boolean): Promise<void> {
  await expect
    .poll(async () => (await (await page.request.get(origin + `/ui/mirror/preview-button?${q(win)}`)).text()).includes('id="preview-toggle"'), {
      message: `the host's report for ${win} reached muster (muster-mirror log:\n${rig.log})`,
      timeout: 20_000,
    })
    .toBe(present);
}

test('the Preview button on both session views: drawn only while the window has something to preview, its panel frames the plugin sandboxed, New tab lands with no opener, the view swap still frames this origin, and a revoked token draws none', async ({ page, context }) => {
  test.setTimeout(150_000);
  await ready(page);
  const { token, id } = await mintViewer(page, 'Example viewer');
  const landings = await playEntry(context, token);
  await buttonFor(page, '@1', true);

  // The Rendered view: the button in the header, counting @1's 2 offers and
  // 1 open preview; the CSP frames the plugin's origin and this one.
  const resp = await page.goto(origin + viewOf('@1'));
  expect(resp!.headers()['content-security-policy']).toContain(`frame-src 'self' ${ZONE}; `);
  await expect(page.locator('#mv-log [data-bubble="user"]').first()).toContainText('the prompt of the chip window');
  const toggle = page.locator('header #preview-toggle-row');
  await expect(toggle).toHaveCount(1);
  await expect(toggle).toHaveAttribute('data-preview-count', '3');
  await expect(toggle).toHaveAttribute('aria-label', 'Preview: 1 preview open, 2 servers to preview');
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  // At this phone width it is in the header's SECOND row, under the title,
  // and the header row's own button is not drawn (operator decision: the
  // title keeps its width).
  await expect(page.locator('#preview-toggle')).toBeHidden();
  const box = (await toggle.boundingBox())!;
  const title = (await page.locator('header h1').first().boundingBox())!;
  expect(box.y + box.height / 2, 'the button is below the title').toBeGreaterThan(title.y + title.height);
  expect(Math.round(box.height), 'a 20px pill in the second row').toBe(20);
  // Its tap goes to it, not to the title's hit area over the same row.
  const hit = await page.evaluate(([x, y]) => (document.elementFromPoint(x, y) as Element | null)?.closest('[data-preview-toggle]')?.id ?? 'none', [box.x + box.width / 2, box.y + box.height / 2]);
  expect(hit, 'the second-row button is on top at its centre').toBe('preview-toggle-row');
  // No row above the chat any more.
  await expect(page.locator('#plugin-chips')).toHaveCount(0);
  await expect(page.locator('#preview-panel')).toBeHidden();
  await expect(page.locator('#preview-panel')).toHaveAttribute('data-plugin', String(id));
  const newTab = page.locator('#preview-panel [data-preview-newtab]');
  await expect(newTab).toHaveAttribute('target', '_blank');
  await expect(newTab).toHaveAttribute('rel', 'noopener noreferrer');
  // Nothing is minted to draw it.
  expect(landings, 'no landing before a click').toHaveLength(0);

  // THE PANEL: the button opens it (the whole screen at this width), its
  // frame — exactly the sandbox set, no top-navigation — lands on the entry
  // page for THIS window.
  await toggle.click();
  const panel = page.locator('#preview-panel');
  await expect(panel).toBeVisible();
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  // In a browser (no window.musterApp) the header's hand-off control is New
  // tab, never the Android app's Full screen.
  await expect(newTab).toBeVisible();
  await expect(newTab).toHaveText('New tab');
  await expect(newTab).toHaveAttribute('aria-label', 'Open the preview in a new tab');
  const pbox = (await panel.boundingBox())!;
  expect([Math.round(pbox.x), Math.round(pbox.width)], 'the panel is the whole screen on a phone').toEqual([0, 390]);
  const frameEl = page.locator('#preview-panel iframe');
  await expect(frameEl).toHaveAttribute('sandbox', SANDBOX);
  await expect(frameEl).toHaveAttribute('allow', '');
  await expect(frameEl).toHaveAttribute('referrerpolicy', 'no-referrer');
  expect(await frameEl.getAttribute('sandbox')).not.toContain('allow-top-navigation');
  const framed = page.frameLocator('#preview-panel iframe');
  await expect(framed.locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
  expect(landings).toHaveLength(1);
  // The plugin's content cannot navigate muster's tab.
  await framed.locator('body').evaluate(() => {
    try {
      window.top!.location.href = 'about:blank#hijacked';
    } catch (e) {
      /* refused by the sandbox */
    }
  });
  await page.waitForTimeout(300);
  expect(new URL(page.url()).pathname, 'the framed plugin could not navigate the top window').toBe('/mirror/view');

  // NEW TAB: minted at the click, on the entry page with muster_code and
  // muster_plugin, redeemed for THIS window, no opener. A double tap mints
  // and opens ONCE.
  const tabs = context.pages().length;
  const [tab] = await Promise.all([context.waitForEvent('page'), newTab.dblclick()]);
  await expect(tab.locator('#fake-entry')).toBeVisible();
  const landed = new URL(tab.url());
  expect(landed.origin + landed.pathname).toBe(ENTRY);
  expect(landed.searchParams.get('muster_plugin')).toBe(String(id));
  expect(landed.searchParams.get('muster_code') ?? '').toMatch(/^.{20,}$/);
  await expect(tab.locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
  await expect(tab.locator('#fake-opener')).toHaveText('opener:none');
  await expect(tab.locator('#fake-referrer')).toHaveText('referrer:none');
  await page.waitForTimeout(500);
  expect(landings, 'a double tap minted once').toHaveLength(2);
  expect(context.pages().length, 'a double tap opened one tab').toBe(tabs + 1);
  expect(landings[1].url.searchParams.get('muster_code'), 'a fresh code per click').not.toBe(landings[0].url.searchParams.get('muster_code'));
  expect(landings[1].referer, 'no Referer reaches the plugin').toBeUndefined();
  await tab.close();

  // A BLOCKED TAB says so, visibly, and mints nothing.
  await page.evaluate(() => {
    (window as unknown as { __open: typeof window.open }).__open = window.open;
    window.open = () => null;
  });
  await newTab.click();
  await expect(page.locator('#preview-error')).toBeVisible();
  await expect(page.locator('#preview-error')).toContainText('blocked the new tab');
  await page.waitForTimeout(500);
  expect(landings, 'a blocked tab minted no code').toHaveLength(2);
  await page.evaluate(() => {
    window.open = (window as unknown as { __open: typeof window.open }).__open;
  });

  // CLOSE unloads the plugin's page and gives the button back its state.
  await page.locator('#preview-panel [data-preview-close]').click();
  await expect(panel).toBeHidden();
  await expect(frameEl).toHaveAttribute('src', 'about:blank');
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');

  // POSITIVE CONTROL for 'self' in frame-src: the Rendered | Terminal switch
  // still opens this origin's other view in its frame, which draws the button
  // too, aimed at the same window.
  await page.locator('#view-terminal').click();
  await expect(page.locator('iframe[data-muster-view]')).toBeVisible();
  const term = page.frameLocator('iframe[data-muster-view]');
  await expect(term.locator('#term')).toHaveCount(1);
  await expect(term.locator('#preview-panel[data-window="@1"]')).toHaveCount(1);
  await expect(term.locator('#preview-toggle-row')).toHaveCount(1);

  // A window with NO rendered view opens on /mirror/terminal: the button is
  // there, aimed at that window, and its panel's New tab lands for it.
  const t = await page.goto(origin + viewOf('@2'));
  expect(new URL(page.url()).pathname).toBe('/mirror/terminal');
  expect(t!.headers()['content-security-policy']).toContain(`frame-src 'self' ${ZONE}; `);
  await expect(page.locator('#preview-toggle-row')).toHaveAttribute('data-preview-count', '1');
  await expect(page.locator('#preview-panel')).toHaveAttribute('data-window', '@2');
  await page.locator('#preview-toggle-row').click();
  await expect(page.frameLocator('#preview-panel iframe').locator('#fake-redeem')).toHaveText(`200 ${rig.host} @2`);
  const [tab2] = await Promise.all([context.waitForEvent('page'), page.locator('#preview-panel [data-preview-newtab]').click()]);
  await expect(tab2.locator('#fake-redeem')).toHaveText(`200 ${rig.host} @2`);
  await tab2.close();

  // AN EXPIRED SIGN-IN: the mint is sent to the sign-in page; the panel says
  // to sign in again, and the blank tab is closed.
  const pagesBefore = context.pages().length;
  await context.clearCookies();
  await page.locator('#preview-panel [data-preview-newtab]').click();
  await expect(page.locator('#preview-error')).toContainText('sign in again');
  await expect.poll(() => context.pages().length, { message: 'the blank tab of a refused New tab is closed' }).toBe(pagesBefore);
  await rig.login(page);

  // Any other page: no frame-src.
  const other = await page.goto(origin + '/tasks');
  expect(other!.headers()['content-security-policy']).not.toContain('frame-src');

  // REVOKED: no slot, no panel, no frame-src.
  const rv = await page.request.post(origin + `/plugins/${id}/revoke`, { headers: { Origin: origin }, maxRedirects: 0 });
  expect(rv.status(), await rv.text()).toBe(303);
  for (const p of [viewOf('@1'), termOf('@2')]) {
    const r = await page.goto(origin + p);
    expect(r!.headers()['content-security-policy'], p).not.toContain('frame-src');
    await expect(page.locator('#preview-slot'), p).toHaveCount(0);
    await expect(page.locator('#preview-panel'), p).toHaveCount(0);
  }
});

test('the Preview button follows the host: hidden while the window has nothing to preview, it appears and goes on an open page as the host\'s report moves, and the sidebar marks the window meanwhile', async ({ page }) => {
  test.setTimeout(120_000);
  await ready(page);
  await mintViewer(page, 'Live viewer');
  playPreviews('');
  await buttonFor(page, '@1', false);

  // NOTHING TO PREVIEW: no button on either view, and no sidebar mark.
  await page.goto(origin + viewOf('@1'));
  await expect(page.locator('#preview-slot')).toHaveCount(1);
  await expect(page.locator('#preview-slot-row')).toHaveCount(1);
  await expect(page.locator('[data-preview-toggle]')).toHaveCount(0);
  await expect(page.locator('#mv-log [data-bubble="user"]').first()).toBeVisible();
  // The geometry the button must not move when it appears: the title, the
  // header, and where the conversation starts.
  const geometry = () =>
    page.evaluate(() => {
      const r = (q: string) => {
        const b = document.querySelector(q)!.getBoundingClientRect();
        return [Math.round(b.x), Math.round(b.y), Math.round(b.width), Math.round(b.height)];
      };
      return { title: r('header h1'), header: r('header'), log: r('#mv-log') };
    });
  const before = await geometry();
  const nav = () => page.request.get(origin + '/ui/nav/agents').then((r) => r.text());
  expect(await nav()).not.toContain('data-nav-preview');

  // The host reports an offer: the open page's button appears at the slot's
  // next poll, with no reload, and the sidebar marks @1 alone.
  playPreviews('@1 1 0\n');
  await expect(page.locator('#preview-toggle-row'), 'the button appeared live').toHaveAttribute('data-preview-count', '1', { timeout: 30_000 });
  await expect(page.locator('#preview-toggle-row')).toBeVisible();
  expect(await geometry(), 'the title (its width included), the header and the conversation did not move when the button appeared').toEqual(before);
  const marked = mirroredEntries(await nav()).filter((e) => e.markup.includes('data-nav-preview'));
  expect(marked.map((e) => entryMatches(e.markup, 'data-mirror-claude'))).toEqual([true]);

  // And goes when the host has nothing again.
  playPreviews('@1 0 0\n');
  await expect(page.locator('#preview-toggle-row'), 'the button went live').toHaveCount(0, { timeout: 30_000 });
  await expect.poll(nav, { timeout: 20_000 }).not.toContain('data-nav-preview');
  playPreviews('@1 2 1\n@2 1 0\n');
});

// IN THE ANDROID APP (window.musterApp, the bridge object the app's WebView
// injects; played here by an init script that records what is posted to it):
// the panel's New tab is Full screen. The app hands every origin but muster's
// to the system browser, so a new tab there left the app (operator report);
// full screen keeps the SAME frame — not reloaded, no new mint — filling the
// screen edge to edge, with an Exit button; Exit, Escape and the shell's Back
// (window.musterBack) each leave it. The panel holds the back-claim from open
// to close, full screen or not (the next test). From inside the view-swap
// frame the claim goes to the MAIN frame's bridge (the app refuses a
// subframe's) and the main frame's Back leaves the frame's full screen.
test('in the Android app New tab is Full screen: the same frame fills the screen, nothing is opened or minted, and Exit, Escape and Back leave it', async ({ page, context }) => {
  test.setTimeout(150_000);
  await page.addInitScript(() => {
    const w = window as unknown as { __appMsgs: string[]; musterApp: { postMessage(m: string): void } };
    w.__appMsgs = [];
    w.musterApp = { postMessage: (m: string) => w.__appMsgs.push(m) };
  });
  const CLAIM_ON = JSON.stringify({ type: 'back-claim', open: true });
  const CLAIM_OFF = JSON.stringify({ type: 'back-claim', open: false });
  const msgs = () => page.evaluate(() => (window as unknown as { __appMsgs: string[] }).__appMsgs.slice());
  await ready(page);
  const { token } = await mintViewer(page, 'App viewer');
  const landings = await playEntry(context, token);
  await buttonFor(page, '@1', true);

  await page.goto(origin + viewOf('@1'));
  const panel = page.locator('#preview-panel');
  // The ONE hand-off control: New tab in a browser, Full screen here.
  const full = panel.locator('[data-preview-newtab]');
  const exit = panel.locator('[data-preview-exit-full]');
  const head = panel.locator('h2#preview-panel-title');
  const frameEl = panel.locator('iframe');
  const framed = page.frameLocator('#preview-panel iframe');
  await page.locator('#preview-toggle-row').click();
  await expect(framed.locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
  expect(landings).toHaveLength(1);
  await expect(full).toBeVisible();
  await expect(full, 'the control reads Full screen in the app').toHaveText('Full screen');
  await expect(full).toHaveAttribute('aria-label', 'Show the preview full screen');
  await expect(panel.locator('a, button').filter({ hasText: 'New tab' }), 'no New tab in the app').toHaveCount(0);
  await expect(exit).toBeHidden();
  expect(await msgs(), 'the open panel claims Back').toEqual([CLAIM_ON]);

  // FULL SCREEN: the same document in the same frame (a mark set in it
  // survives), no new page, no new mint; panel and frame are the viewport.
  await framed.locator('body').evaluate(() => { (window as unknown as { __mark: number }).__mark = 42; });
  const src = await frameEl.getAttribute('src');
  const pages = context.pages().length;
  await full.click();
  await expect(panel).toHaveAttribute('data-preview-full', '');
  await expect(head, 'the header is hidden').toBeHidden();
  await expect(exit).toBeVisible();
  await expect(exit).toBeFocused();
  const vp = page.viewportSize()!;
  const box = async (l: typeof panel) => { const b = (await l.boundingBox())!; return [Math.round(b.x), Math.round(b.y), Math.round(b.width), Math.round(b.height)]; };
  expect(await box(panel), 'the panel is the viewport').toEqual([0, 0, vp.width, vp.height]);
  expect(await box(frameEl), 'the frame is the viewport, edge to edge').toEqual([0, 0, vp.width, vp.height]);
  expect(await frameEl.getAttribute('src'), 'the frame was not sent anywhere').toBe(src);
  expect(await framed.locator('body').evaluate(() => (window as unknown as { __mark?: number }).__mark), 'the frame was not reloaded').toBe(42);
  await page.waitForTimeout(500);
  expect(context.pages().length, 'no tab was opened').toBe(pages);
  expect(landings, 'nothing was minted').toHaveLength(1);
  expect(await msgs(), 'full screen keeps the panel\'s claim, posting nothing').toEqual([CLAIM_ON]);

  // EXIT: back to the panel, its header and Full screen, the claim kept (the
  // panel is still open).
  await exit.click();
  await expect(panel).not.toHaveAttribute('data-preview-full', '');
  await expect(head).toBeVisible();
  await expect(full).toBeFocused();
  await expect(panel).toBeVisible();
  expect(await msgs()).toEqual([CLAIM_ON]);

  // THE SHELL'S BACK: the first press leaves full screen (true) and keeps the
  // claim; the second closes the panel (true) and releases it.
  await full.click();
  await expect(panel).toHaveAttribute('data-preview-full', '');
  expect(await page.evaluate(() => (window as unknown as { musterBack(): boolean }).musterBack()), 'Back left full screen').toBe(true);
  await expect(panel).not.toHaveAttribute('data-preview-full', '');
  await expect(panel).toBeVisible();
  expect(await msgs(), 'leaving full screen kept the claim').toEqual([CLAIM_ON]);
  expect(await page.evaluate(() => (window as unknown as { musterBack(): boolean }).musterBack()), 'the second Back closed the panel').toBe(true);
  await expect(panel).toBeHidden();
  expect(await msgs()).toEqual([CLAIM_ON, CLAIM_OFF]);

  // ESCAPE: the first leaves full screen, the second closes the panel.
  await page.locator('#preview-toggle-row').click();
  await expect(framed.locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
  await full.click();
  await expect(panel).toHaveAttribute('data-preview-full', '');
  await page.keyboard.press('Escape');
  await expect(panel).not.toHaveAttribute('data-preview-full', '');
  await expect(panel).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(panel).toBeHidden();
  expect(await msgs()).toEqual([CLAIM_ON, CLAIM_OFF, CLAIM_ON, CLAIM_OFF]);

  // 🔴 BACK STILL REACHES THE DOCK AFTER FULL SCREEN HAS BEEN USED: full
  // screen wrapped this window's musterBack, and with full screen gone the
  // wrapper must hand the press to the dock's close (the touch layout draws
  // the dock here). A wrapper answering false would leave Back unable to
  // close the dock for the rest of the page.
  const dockOpen = () => page.evaluate(() => !!(document.getElementById('switcher-dock') as HTMLDialogElement | null)?.open);
  await page.locator('#switcher-toggle').tap();
  await expect.poll(dockOpen, 'the dock opened').toBe(true);
  expect(await page.evaluate(() => (window as unknown as { musterBack(): boolean }).musterBack()), 'Back closed the dock through the wrapper').toBe(true);
  await expect.poll(dockOpen, 'the dock closed').toBe(false);
  await expect.poll(async () => (await msgs()).slice(-2), 'the dock\'s own claim pair').toEqual([CLAIM_ON, CLAIM_OFF]);
  const settled = (await msgs()).length;

  // CLOSE from full screen (reopened: its tap, with the header hidden, is a
  // script's) releases the claim with it.
  await page.locator('#preview-toggle-row').click();
  await expect(framed.locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
  await full.click();
  await expect(panel).toHaveAttribute('data-preview-full', '');
  await page.evaluate(() => (document.querySelector('#preview-panel [data-preview-close]') as HTMLElement).click());
  await expect(panel).toBeHidden();
  expect((await msgs()).slice(settled)).toEqual([CLAIM_ON, CLAIM_OFF]);

  // A WIDE SCREEN in the app (a tablet): the side panel goes the whole width.
  await page.setViewportSize({ width: 1024, height: 768 });
  await page.locator('#preview-toggle').click();
  await expect(framed.locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
  expect((await box(panel))[2], 'a side panel before full screen').toBeLessThan(1024);
  await full.click();
  expect(await box(frameEl), 'full screen at a wide width').toEqual([0, 0, 1024, 768]);
  await exit.click();
  await panel.locator('[data-preview-close]').click();
  await page.setViewportSize(vp);

  // FROM INSIDE THE VIEW-SWAP FRAME (the Terminal view, framed): the claim
  // is posted to the MAIN frame's bridge, not the frame's own, and the main
  // frame's Back leaves the frame's full screen.
  await page.locator('#view-terminal').click();
  await expect(page.locator('iframe[data-muster-view]')).toBeVisible();
  const term = page.frameLocator('iframe[data-muster-view]');
  const before = (await msgs()).length;
  await term.locator('#preview-toggle-row').click();
  await expect(term.frameLocator('#preview-panel iframe').locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
  await expect(term.locator('#preview-panel [data-preview-newtab]'), 'Full screen in the frame too').toHaveText('Full screen');
  await term.locator('#preview-panel [data-preview-newtab]').click();
  await expect(term.locator('#preview-panel')).toHaveAttribute('data-preview-full', '');
  expect((await msgs()).slice(before), 'the claim went to the main frame').toEqual([CLAIM_ON]);
  const frameMsgs = await term.locator('body').evaluate(() => (window as unknown as { __appMsgs?: string[] }).__appMsgs ?? []);
  expect(frameMsgs, 'nothing posted to the subframe\'s bridge').toEqual([]);
  expect(await page.evaluate(() => (window as unknown as { musterBack(): boolean }).musterBack()), 'the main frame\'s Back left the frame\'s full screen').toBe(true);
  await expect(term.locator('#preview-panel')).not.toHaveAttribute('data-preview-full', '');
  expect((await msgs()).slice(before), 'the frame\'s open panel still holds the claim').toEqual([CLAIM_ON]);
  expect(await page.evaluate(() => (window as unknown as { musterBack(): boolean }).musterBack()), 'the main frame\'s next Back closed the frame\'s panel').toBe(true);
  await expect(term.locator('#preview-panel')).toBeHidden();
  expect((await msgs()).slice(before)).toEqual([CLAIM_ON, CLAIM_OFF]);
});

// 🔴 BACK IN THE REGULAR PANEL, IN THE ANDROID APP (operator report: Back in
// the panel, header showing, "doesn't actually go back, it just causes the
// preview to go to a white screen and then break"). With no back-claim the
// shell's Back is the WebView's goBack, which walks the tab's JOINT session
// history — subframe navigations included — so it sent the panel's frame back
// to an already-spent hand-off URL. Two layers, each pinned here: the panel
// claims Back from open to close (a press closes it), and loading the frame
// adds no entry to the top window's history (history.length), so even an
// unclaimed Back cannot strand it. The switcher dock shares the app's ONE
// claim flag, and from `sm` up it can open over the side panel: closing the
// dock must not release the claim the still-open panel holds.
test('in the Android app Back closes the regular panel: the open panel claims Back, its frame adds no history entry, and the dock opened over the side panel leaves the panel its claim', async ({ page, context }) => {
  test.setTimeout(150_000);
  await page.addInitScript(() => {
    const w = window as unknown as { __appMsgs: string[]; musterApp: { postMessage(m: string): void } };
    w.__appMsgs = [];
    w.musterApp = { postMessage: (m: string) => w.__appMsgs.push(m) };
  });
  const CLAIM_ON = JSON.stringify({ type: 'back-claim', open: true });
  const CLAIM_OFF = JSON.stringify({ type: 'back-claim', open: false });
  const msgs = () => page.evaluate(() => (window as unknown as { __appMsgs: string[] }).__appMsgs.slice());
  const back = () => page.evaluate(() => (window as unknown as { musterBack(): boolean }).musterBack());
  const historyLength = () => page.evaluate(() => history.length);
  const dockOpen = () => page.evaluate(() => !!(document.getElementById('switcher-dock') as HTMLDialogElement | null)?.open);
  await ready(page);
  // Two live viewers, so the panel's header has the plugin picker, whose
  // change reloads a frame that already holds a page.
  const other = await mintViewer(page, 'Back viewer other');
  const { token, id } = await mintViewer(page, 'Back viewer');
  const landings = await playEntry(context, token);
  await buttonFor(page, '@1', true);

  await page.goto(origin + viewOf('@1'));
  const panel = page.locator('#preview-panel');
  const framed = page.frameLocator('#preview-panel iframe');
  const toggle = page.locator('#preview-toggle-row');
  await expect(toggle).toBeVisible();
  const h0 = await historyLength();
  expect(await msgs(), 'nothing claimed while the panel is shut').toEqual([]);

  // (a) THE REGULAR PANEL CLAIMS BACK, and the hand-off's load adds no entry.
  await toggle.click();
  await expect(framed.locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
  await expect(panel.locator('h2#preview-panel-title'), 'the regular panel, its header showing').toBeVisible();
  await expect(panel).not.toHaveAttribute('data-preview-full', '');
  expect(await msgs(), 'the open regular panel claimed Back').toEqual([CLAIM_ON]);
  expect(await historyLength(), 'loading the hand-off in the frame added no history entry').toBe(h0);
  // The frame is sandboxed exactly as drawn (it is a new element per load).
  await expect(panel.locator('iframe')).toHaveAttribute('sandbox', SANDBOX);
  await expect(panel.locator('iframe')).toHaveAttribute('referrerpolicy', 'no-referrer');
  await expect(panel.locator('iframe')).toHaveAttribute('allow', '');
  await expect(panel.locator('iframe')).toHaveCount(1);
  // THE SHELL'S BACK closes it (true: handled, no goBack) and releases.
  expect(await back(), 'Back closed the regular panel').toBe(true);
  await expect(panel).toBeHidden();
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  await expect(panel.locator('iframe')).toHaveAttribute('src', 'about:blank');
  expect(await msgs(), 'closing released the claim').toEqual([CLAIM_ON, CLAIM_OFF]);
  expect(await historyLength(), 'unloading the frame added no history entry').toBe(h0);

  // A second opening: a fresh hand-off, still no entry; Close releases too.
  await toggle.click();
  await expect.poll(() => landings.length).toBe(2);
  await expect(framed.locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
  expect(await historyLength(), 'a second hand-off added no history entry').toBe(h0);
  // The picker's change mints and loads ANOTHER hand-off into the frame that
  // holds the first one's page: still no entry, either way.
  const picker = panel.locator('[data-preview-plugin]');
  await picker.selectOption(String(other.id));
  await expect.poll(() => landings.length).toBe(3);
  await expect(framed.locator('#fake-entry')).toBeVisible();
  expect(landings[2].url.searchParams.get('muster_plugin')).toBe(String(other.id));
  expect(await historyLength(), 'the picker\'s reload of a loaded frame added no history entry').toBe(h0);
  await picker.selectOption(String(id));
  await expect.poll(() => landings.length).toBe(4);
  await expect(framed.locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
  expect(await historyLength()).toBe(h0);
  await panel.locator('[data-preview-close]').click();
  await expect(panel).toBeHidden();
  expect(await msgs()).toEqual([CLAIM_ON, CLAIM_OFF, CLAIM_ON, CLAIM_OFF]);
  expect(await historyLength()).toBe(h0);

  // (d) THE DOCK STILL HAS BACK after the regular panel was used: the panel's
  // wrapper hands the press to the dock's close.
  await page.locator('#switcher-toggle').tap();
  await expect.poll(dockOpen, 'the dock opened').toBe(true);
  expect(await back(), 'Back closed the dock through the wrapper').toBe(true);
  await expect.poll(dockOpen, 'the dock closed').toBe(false);
  await expect.poll(async () => (await msgs()).slice(4), 'the dock\'s own claim pair').toEqual([CLAIM_ON, CLAIM_OFF]);
  expect(await back(), 'with nothing open Back is the shell\'s').toBe(false);

  // A WIDE SCREEN (a tablet): nothing behind the side panel is inert, so the
  // dock can open OVER the open panel. The one claim stays held until BOTH
  // have closed, whichever way the dock closes.
  await page.setViewportSize({ width: 1024, height: 768 });
  await page.locator('#preview-toggle').click();
  await expect(framed.locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
  const n = (await msgs()).length;
  expect((await msgs()).slice(n - 1), 'the side panel claimed Back').toEqual([CLAIM_ON]);
  await page.locator('#switcher-toggle').tap();
  await expect.poll(dockOpen, 'the dock opened over the side panel').toBe(true);
  // Back closes the DOCK first (it is modal, on top), and the panel keeps the claim.
  expect(await back(), 'Back closed the dock over the panel').toBe(true);
  await expect.poll(dockOpen).toBe(false);
  await expect(panel).toBeVisible();
  await expect.poll(async () => (await msgs()).slice(-1), 'the dock\'s close left the panel\'s claim on').toEqual([CLAIM_ON]);
  // The dock closed its own way (Escape) does the same.
  await page.locator('#switcher-toggle').tap();
  await expect.poll(dockOpen).toBe(true);
  await page.keyboard.press('Escape');
  await expect.poll(dockOpen).toBe(false);
  await expect(panel, 'Escape was the dock\'s, not the panel\'s').toBeVisible();
  await expect.poll(async () => (await msgs()).slice(-1)).toEqual([CLAIM_ON]);
  // Then Back closes the panel and the claim ends.
  expect(await back(), 'the next Back closed the side panel').toBe(true);
  await expect(panel).toBeHidden();
  expect((await msgs()).slice(-1)).toEqual([CLAIM_OFF]);
  expect(await historyLength(), 'nothing on the wide screen added a history entry').toBe(h0);
});
