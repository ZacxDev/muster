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
  const toggle = page.locator('header #preview-toggle');
  await expect(toggle).toHaveCount(1);
  await expect(toggle).toHaveAttribute('data-preview-count', '3');
  await expect(toggle).toHaveAttribute('aria-label', 'Preview: 1 preview open, 2 servers to preview');
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  // At this phone width it is a 44×44 icon: its word is not drawn.
  const box = (await toggle.boundingBox())!;
  expect([Math.round(box.width), Math.round(box.height)]).toEqual([44, 44]);
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
  await expect(term.locator('#preview-toggle')).toHaveCount(1);

  // A window with NO rendered view opens on /mirror/terminal: the button is
  // there, aimed at that window, and its panel's New tab lands for it.
  const t = await page.goto(origin + viewOf('@2'));
  expect(new URL(page.url()).pathname).toBe('/mirror/terminal');
  expect(t!.headers()['content-security-policy']).toContain(`frame-src 'self' ${ZONE}; `);
  await expect(page.locator('#preview-toggle')).toHaveAttribute('data-preview-count', '1');
  await expect(page.locator('#preview-panel')).toHaveAttribute('data-window', '@2');
  await page.locator('#preview-toggle').click();
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
  await expect(page.locator('#preview-toggle')).toHaveCount(0);
  const nav = () => page.request.get(origin + '/ui/nav/agents').then((r) => r.text());
  expect(await nav()).not.toContain('data-nav-preview');

  // The host reports an offer: the open page's button appears at the slot's
  // next poll, with no reload, and the sidebar marks @1 alone.
  playPreviews('@1 1 0\n');
  await expect(page.locator('#preview-toggle'), 'the button appeared live').toHaveAttribute('data-preview-count', '1', { timeout: 30_000 });
  const marked = mirroredEntries(await nav()).filter((e) => e.markup.includes('data-nav-preview'));
  expect(marked.map((e) => entryMatches(e.markup, 'data-mirror-claude'))).toEqual([true]);

  // And goes when the host has nothing again.
  playPreviews('@1 0 0\n');
  await expect(page.locator('#preview-toggle'), 'the button went live').toHaveCount(0, { timeout: 30_000 });
  await expect.poll(nav, { timeout: 20_000 }).not.toContain('data-nav-preview');
  playPreviews('@1 2 1\n@2 1 0\n');
});
