import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { BrowserContext, Page, Route } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, repoRoot, sessionOpenPort } from '../lib/env';
import { cc, entryMatches, mirroredEntries, MirrorRig } from '../lib/mirrorhost';
import { touchDrag } from '../lib/touch';

// A SESSION'S PREVIEWS AND PAGES IN ONE PLACE (operator ask: "When I'm looking
// at a session, I can't see pages that it's created … fold pages in with the
// preview button and then make it easy to navigate between previews and pages
// in the same view"), on the operator's click path: a REAL muster-server with
// the mirror plugin, plugins (MUSTER_PLUGIN_ORIGINS) and pages on, a REAL
// muster-mirror (`-tags e2emirror`) with three Claude Code windows, and pages
// written as a session's create_page writes them (pluginfix page).
//
//   - the header's ONE "Open" button counts previews + pages, and is drawn
//     for a session with pages and NO viewer plugin;
//   - its panel's "Previews (n) · Pages (n)" tabs, swiped on a phone, open on
//     the tab that has something, else the one used last (stored);
//   - the Pages tab lists exactly the session's own pages, newest first, and
//     opens one in the panel (the owner's own view, framed), with "Open full
//     page";
//   - the header fits ONE row at 390×844 and 1280×800, its secondary actions
//     in a ⋯ menu, and the button's arrival and new count move nothing.
//
// What each window has to preview is PLAYED by the host's
// MUSTER_MIRROR_E2E_PREVIEWS_FILE; the viewer plugin's entry page by
// page.route (plugin-chip.spec.ts's, which owns the hand-off itself).
//
// 🔴 ONLY THIS SPEC'S SERVER HAS MUSTER_PLUGIN_ORIGINS SET, and afterAll
// removes the plugin, the pages, the agents and the team it made.
const ZONE = 'https://*.viewer.example.net';
const ENTRY = 'https://w.viewer.example.net/__muster/enter';
const SANDBOX = 'allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals';
const OWNER = '1'; // the password-mode operator

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

const stamp = Date.now().toString(36);
const rig = new MirrorRig(sessionOpenPort, 'e2e-open', { MUSTER_PLUGINS: '1', MUSTER_PLUGIN_ORIGINS: ZONE, MUSTER_PAGES: '1' });
const origin = rig.origin;

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();
const team = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/mkteam', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

// The three windows' Claude sessions (a transcript's name is its session id,
// e2emirror.go), and one session no window runs.
const SA = `open-a-${stamp}`, SB = `open-b-${stamp}`, SC = `open-c-${stamp}`, SZ = `open-z-${stamp}`;

const q = (win: string) => `host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent(win)}`;
const viewOf = (win: string) => `/mirror/view?${q(win)}`;
let previewsFile = '';
const playPreviews = (lines: string) => fs.writeFileSync(previewsFile, lines);

// @1's pages (session SA), oldest first as made: title, versions, live link.
// Their versions are pairwise distinct, and distinct from every count below.
const A_PAGES = [
  { title: `Alpha launch plan ${stamp}`, versions: 3, live: false, age: 400 },
  { title: `Alpha pricing ${stamp}`, versions: 1, live: false, age: 300 },
  { title: `Alpha contact form ${stamp}`, versions: 6, live: true, age: 200 },
  { title: `Alpha recap ${stamp}`, versions: 5, live: false, age: 100 },
];
const B_PAGES = [
  { title: `Beta brief ${stamp}`, versions: 2, live: false, age: 250 },
  { title: `Beta notes ${stamp}`, versions: 1, live: false, age: 150 },
];
// The agent's pages.
const AG_PAGES = [
  { title: `Agent report ${stamp}`, versions: 2, live: true, age: 500 },
  { title: `Agent table ${stamp}`, versions: 1, live: false, age: 50 },
];
const made: string[] = [];
const ids: Record<string, string> = {};
let teamID = '';
let agentID = '', otherAgentID = '';
const agentName = `e2e-open-agent-${stamp}`, otherAgentName = `e2e-open-other-${stamp}`;
const pluginNames: string[] = [];

function page(owner: string, title: string, agent: string, host: string, session: string, versions: number, live: boolean, age: number): string {
  const id = fix('page', owner, title, agent, host, session, String(versions), live ? '1' : '0', String(age));
  made.push(id);
  ids[title] = id;
  return id;
}

test.beforeAll(async () => {
  test.setTimeout(240_000);
  await rig.start();
  const now = Math.floor(Date.now() / 1000);
  const tr = (sid: string, prompt: string) => {
    const p = path.join(rig.dir, `${sid}.jsonl`);
    fs.writeFileSync(p, cc.prompt(prompt) + cc.reply('Answered.'));
    return p;
  };
  const windows = rig.transcript(
    `work|@1|1|1|claude|${now - 60}|${tr(SA, 'the alpha window')}\n` +
      `work|@2|2|0|claude|${now - 90}|${tr(SB, 'the beta window')}\n` +
      `work|@3|3|0|claude|${now - 120}|${tr(SC, 'the gamma window')}\n`,
    'windows',
  );
  previewsFile = path.join(rig.dir, 'previews-played');
  playPreviews('');
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms', MUSTER_MIRROR_E2E_PREVIEWS_FILE: previewsFile });

  for (const p of A_PAGES) page(OWNER, p.title, '0', rig.host, SA, p.versions, p.live, p.age);
  for (const p of B_PAGES) page(OWNER, p.title, '0', rig.host, SB, p.versions, p.live, p.age);
  // NOT @1's: another session on the same host; a page made before #802
  // recorded the session (none recorded); the same session on ANOTHER host.
  page(OWNER, `Zeta other session ${stamp}`, '0', rig.host, SZ, 4, false, 50);
  page(OWNER, `Pre-0152 page ${stamp}`, '0', rig.host, '-', 4, false, 60);
  page(OWNER, `Other host ${stamp}`, '0', `e2e-open-elsewhere-${stamp}`, SA, 4, false, 70);
  // A TEAM's page from the same host name and session (a team's own host
  // called the same): the team's row, which the viewer never sees.
  teamID = team('create', `e2e-open-team-${stamp}`);
  page(teamID, `Team page ${stamp}`, '0', rig.host, SA, 4, true, 10);
  // The agents: a claude-code one with pages, and another whose page is not
  // its.
  agentID = fix('ccagent', OWNER, agentName, `e2e-open-token-not-real-${stamp}`);
  otherAgentID = fix('agent', OWNER, otherAgentName, `e2e-open-other-token-not-real-${stamp}`);
  for (const p of AG_PAGES) page(OWNER, p.title, agentID, '-', '-', p.versions, p.live, p.age);
  page(OWNER, `Other agent's page ${stamp}`, otherAgentID, '-', '-', 4, false, 20);
});

test.afterAll(async () => {
  for (const id of made) fix('rmpage', id);
  for (const n of pluginNames) fix('rmplugin', OWNER, n);
  if (agentID) fix('rmagent', OWNER, agentName);
  if (otherAgentID) fix('rmagent', OWNER, otherAgentName);
  if (teamID) team('remove', teamID);
  await rig.stop();
});

// ready signs in and waits until the host lists its three Claude windows.
async function ready(page: Page): Promise<void> {
  await rig.login(page);
  await expect
    .poll(
      async () =>
        mirroredEntries(await (await page.request.get(origin + '/ui/nav/agents')).text()).filter(
          (e) => e.state === 'online' && e.markup.includes(`host=${encodeURIComponent(rig.host)}&`) && entryMatches(e.markup, 'data-mirror-claude'),
        ).length,
      { message: `the Claude windows listed (muster-mirror log:\n${rig.log})`, timeout: 20_000 },
    )
    .toBe(3);
}

// mintViewer creates a viewer-only plugin through the real /plugins form,
// once per worker (a failed test's retry starts a new one, with its own rig).
let minted: { token: string; id: number } | null = null;
async function mintViewer(page: Page): Promise<{ token: string; id: number }> {
  if (minted) return minted;
  const name = 'e2e-open-' + Date.now().toString(36);
  pluginNames.push(name);
  const form = new URLSearchParams({ name, label: 'Example viewer', data_scope: 'example', origin: ZONE, entry_url: ENTRY });
  form.append('capability', 'viewer');
  const r = await page.request.post(origin + '/plugins', { headers: { Origin: origin, 'Content-Type': 'application/x-www-form-urlencoded' }, data: form.toString() });
  const html = await r.text();
  expect(r.status(), html.slice(0, 400)).toBe(200);
  const token = /id="minted-plugin-token"[^>]*>(mpl_[A-Za-z0-9_-]+)</.exec(html)?.[1];
  expect(token, 'a plugin token was minted').toBeTruthy();
  const self = await page.request.get(origin + '/plugin/v1/self', { headers: { Authorization: `Bearer ${token}` } });
  minted = { token: token!, id: ((await self.json()) as { plugin_id: number }).plugin_id };
  return minted;
}

// playEntry answers the plugin's entry URL: it redeems the code and draws the
// answer (plugin-chip.spec.ts's).
async function playEntry(context: BrowserContext, token: string): Promise<URL[]> {
  const landings: URL[] = [];
  await context.route('https://*.viewer.example.net/**', async (route: Route) => {
    const url = new URL(route.request().url());
    const res = await fetch(origin + '/plugin/v1/handoffs/redeem', {
      method: 'POST',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ code: url.searchParams.get('muster_code') ?? '' }),
    });
    const body = await res.json().catch(() => ({}));
    landings.push(url);
    const target = body?.target?.mirror ?? {};
    await route.fulfill({
      status: 200,
      headers: { 'Content-Type': 'text/html; charset=utf-8' },
      body: `<!doctype html><title>Fake plugin</title><h1 id="fake-entry">fake plugin entry</h1><p id="fake-redeem">${res.status} ${target.host ?? ''} ${target.window ?? ''}</p>`,
    });
  });
  return landings;
}

// buttonCount polls the window's button route until it answers count n (0:
// no button).
async function buttonCount(page: Page, win: string, n: number): Promise<void> {
  await expect
    .poll(
      async () => {
        const b = await (await page.request.get(origin + `/ui/mirror/preview-button?${q(win)}`)).text();
        return Number(/data-preview-count="(\d+)"/.exec(b)?.[1] ?? 0);
      },
      { message: `the button route's count for ${win} (muster-mirror log:\n${rig.log})`, timeout: 20_000 },
    )
    .toBe(n);
}

const toggle = (page: Page) => page.locator('header #preview-toggle');
const panel = (page: Page) => page.locator('#preview-panel');
const tab = (page: Page, name: string) => page.locator(`#open-tabs [data-tab="${name}"]`);
const selected = (page: Page) => page.locator('#open-tabs [role="tab"][aria-selected="true"]');
const listTitles = (page: Page) => page.locator('#preview-panel [data-session-page]').evaluateAll((els) => els.map((e) => e.getAttribute('data-page-title')));

test.describe.serial('the Open button and its panel', () => {
  test('a session with pages and NO viewer plugin draws the Open button, counting its pages', async ({ page }) => {
    await ready(page);
    await page.goto(origin + viewOf('@2'));
    await expect(page.locator('#mv-log [data-bubble="user"]').first()).toContainText('the beta window');
    await expect(toggle(page)).toBeVisible();
    await expect(toggle(page)).toHaveAttribute('data-preview-count', '2');
    await expect(toggle(page)).toHaveAttribute('data-open-pages', '2');
    await expect(toggle(page)).toHaveAttribute('data-open-previews', '0');
    await expect(toggle(page)).toHaveAttribute('aria-label', 'Open: 2 pages');
    // A window whose session made none: no button (and no panel to open).
    await page.goto(origin + viewOf('@3'));
    await expect(page.locator('#mv-log [data-bubble="user"]').first()).toContainText('the gamma window');
    await expect(toggle(page)).toHaveCount(0);
  });

  test('the Pages tab lists exactly this session’s pages, newest first, with each one’s version and share state; one opens in the panel', async ({ page }) => {
    await ready(page);
    await page.goto(origin + viewOf('@1'));
    await expect(toggle(page)).toHaveAttribute('data-preview-count', '4');
    await toggle(page).click();
    await expect(panel(page)).toBeVisible();
    // Only pages to open: the panel opens on Pages.
    await expect(selected(page)).toHaveAttribute('data-tab', 'pages');
    await expect(tab(page, 'pages')).toHaveText('Pages(4)');
    await expect(tab(page, 'previews')).toHaveText('Previews(0)');
    await expect(page.locator('#preview-panel [data-open-empty="previews"]')).toHaveText('Nothing to preview in this session.');
    await expect.poll(() => listTitles(page)).toEqual([...A_PAGES].reverse().map((p) => p.title));
    const versions = await page.locator('#preview-panel [data-session-page]').evaluateAll((els) => els.map((e) => e.getAttribute('data-page-version')));
    expect(versions).toEqual(['v5', 'v6', 'v1', 'v3']);
    const shared = await page.locator('#preview-panel [data-page-share]').evaluateAll((els) => els.map((e) => e.getAttribute('data-page-share')));
    expect(shared).toEqual(['false', 'true', 'false', 'false']);
    await expect(page.locator(`#preview-panel [data-session-page="${ids[A_PAGES[2].title]}"] [data-page-share]`)).toHaveText('Shared · live link');
    // NONE of: the other session's, the pre-0152 one, another host's, the
    // team's, an agent's.
    const body = await page.locator('#preview-panel [data-session-pages-list]').innerText();
    for (const t of ['Zeta other session', 'Pre-0152 page', 'Other host', 'Team page', 'Agent report', "Other agent's page"]) {
      expect(body, t).not.toContain(t);
    }
    // ONE PAGE IN THE PANEL: the owner's own view, framed, its version chip;
    // "Open full page" is the page's own view.
    const recap = A_PAGES[3];
    await page.locator(`#preview-panel [data-session-page="${ids[recap.title]}"]`).click();
    const open = page.locator('#preview-panel [data-page-open]');
    await expect(open).toBeVisible();
    await expect(open.locator('[data-page-title]')).toHaveText(recap.title);
    await expect(open.locator('[data-page-version]')).toHaveText('v5');
    await expect(open.locator('[data-page-full]')).toHaveAttribute('href', `/pages/${ids[recap.title]}/view`);
    await expect(open.locator('[data-page-frame]')).toHaveAttribute('src', `/pages/${ids[recap.title]}/view?embed=1`);
    const shell = page.frameLocator('#preview-panel [data-page-frame]');
    await expect(shell.locator('[data-version-chip]')).toContainText('v5');
    await expect(shell.frameLocator('#page').locator('h1')).toHaveText(recap.title);
    // Back to the list; then Open full page goes to /pages/{id}/view.
    await open.locator('[data-page-back]').click();
    await expect(open).toBeHidden();
    await expect(page.locator('#preview-panel [data-session-page]').first()).toBeVisible();
    await page.locator(`#preview-panel [data-session-page="${ids[recap.title]}"]`).click();
    await open.locator('[data-page-full]').click();
    await expect(page).toHaveURL(`${origin}/pages/${ids[recap.title]}/view`);
  });

  test('the panel’s page frame is the owner’s view only: framed by muster, never by a share link', async ({ page }) => {
    await ready(page);
    const id = ids[A_PAGES[0].title];
    const embed = await page.request.get(origin + `/pages/${id}/view?embed=1`);
    expect(embed.status()).toBe(200);
    expect(embed.headers()['content-security-policy']).toContain("frame-ancestors 'self';");
    expect(embed.headers()['x-frame-options']).toBe('SAMEORIGIN');
    const full = await page.request.get(origin + `/pages/${id}/view`);
    expect(full.headers()['content-security-policy']).toContain("frame-ancestors 'none';");
    expect(full.headers()['x-frame-options']).toBe('DENY');
  });

  test('an agent session lists its agent’s pages', async ({ page }) => {
    await ready(page);
    await page.goto(origin + `/agents/${agentName}`);
    await expect(toggle(page)).toHaveAttribute('data-preview-count', '2');
    await toggle(page).click();
    await expect(selected(page)).toHaveAttribute('data-tab', 'pages');
    await expect.poll(() => listTitles(page)).toEqual([...AG_PAGES].reverse().map((p) => p.title));
    const body = await page.locator('#preview-panel [data-session-pages-list]').innerText();
    expect(body).not.toContain("Other agent's page");
    expect(body).not.toContain('Alpha');
    await page.locator('#preview-panel [data-preview-close]').click();
    await expect(panel(page)).toBeHidden();
    // The other agent: its one page.
    await page.goto(origin + `/agents/${otherAgentName}`);
    await expect(toggle(page)).toHaveAttribute('data-preview-count', '1');
  });

  test('with a viewer plugin: the badge is previews + pages; the Previews tab is the preview panel as before; the tab used last is remembered', async ({ page, context }) => {
    test.setTimeout(120_000);
    await ready(page);
    const { token } = await mintViewer(page);
    const landings = await playEntry(context, token);
    // @1: 2 servers to preview and 1 open (3) + its 4 pages = 7.
    playPreviews('@1 2 1\n');
    await buttonCount(page, '@1', 7);
    await page.goto(origin + viewOf('@1'));
    await expect(toggle(page)).toHaveAttribute('data-preview-count', '7');
    await expect(toggle(page)).toHaveAttribute('data-open-previews', '3');
    await expect(toggle(page)).toHaveAttribute('data-open-pages', '4');
    await expect(toggle(page).locator('[data-open-badge]')).toHaveText('7');
    await expect(toggle(page)).toHaveAttribute('aria-label', 'Open: 1 preview open, 2 servers to preview, 4 pages');
    // Nothing chosen yet, both have something: Previews.
    expect(await page.evaluate(() => localStorage.getItem('muster:tabs:session-open:1'))).toBeNull();
    await toggle(page).click();
    await expect(selected(page)).toHaveAttribute('data-tab', 'previews');
    await expect(tab(page, 'previews')).toHaveText('Previews(3)');
    await expect(tab(page, 'pages')).toHaveText('Pages(4)');
    // THE PREVIEWS TAB AS IT WAS: the plugin's frame, sandboxed exactly,
    // landed on the entry page for THIS window; New tab beside it.
    const frameEl = page.locator('#preview-panel [data-preview-frame]');
    await expect(frameEl).toHaveAttribute('sandbox', SANDBOX);
    await expect(frameEl).toHaveAttribute('allow', '');
    await expect(frameEl).toHaveAttribute('referrerpolicy', 'no-referrer');
    await expect(page.frameLocator('#preview-panel [data-preview-frame]').locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
    expect(landings).toHaveLength(1);
    await expect(page.locator('#preview-panel [data-preview-newtab]')).toBeVisible();
    // Pages: New tab is the Previews tab's only.
    await tab(page, 'pages').click();
    await expect(page.locator('#preview-panel [data-preview-newtab]')).toBeHidden();
    await expect.poll(() => listTitles(page)).toEqual([...A_PAGES].reverse().map((p) => p.title));
    expect(await page.evaluate(() => localStorage.getItem('muster:tabs:session-open:1'))).toBe('pages');
    // Closed and opened again — and after a reload — it opens on Pages.
    await page.locator('#preview-panel [data-preview-close]').click();
    await expect(page.locator('#preview-panel [data-preview-frame]')).toHaveAttribute('src', 'about:blank');
    await toggle(page).click();
    await expect(selected(page)).toHaveAttribute('data-tab', 'pages');
    expect(landings, 'opening on Pages mints nothing').toHaveLength(1);
    await page.reload();
    await toggle(page).click();
    await expect(selected(page)).toHaveAttribute('data-tab', 'pages');
    // Previews chosen: remembered the same way; and back on it the preview
    // loads again (a fresh hand-off).
    await tab(page, 'previews').click();
    await expect(page.frameLocator('#preview-panel [data-preview-frame]').locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
    expect(landings).toHaveLength(2);
    expect(await page.evaluate(() => localStorage.getItem('muster:tabs:session-open:1'))).toBe('previews');
    await page.keyboard.press('Escape');
    await expect(panel(page)).toBeHidden();
    await toggle(page).click();
    await expect(selected(page)).toHaveAttribute('data-tab', 'previews');
    // ONE TAB HAS SOMETHING: it wins over the stored choice. @2 has pages
    // only: Pages, though Previews was used last.
    await page.goto(origin + viewOf('@2'));
    await toggle(page).click();
    await expect(selected(page)).toHaveAttribute('data-tab', 'pages');
    expect(await page.evaluate(() => localStorage.getItem('muster:tabs:session-open:1')), 'choosing on open stores nothing').toBe('previews');
  });

  test('a sideways swipe on the panel switches between Previews and Pages (touch, 390×844)', async ({ page }) => {
    await ready(page);
    await mintViewer(page);
    playPreviews('@1 2 1\n');
    await buttonCount(page, '@1', 7);
    await page.goto(origin + viewOf('@1'));
    await page.evaluate(() => localStorage.setItem('muster:tabs:session-open:1', 'pages'));
    await toggle(page).click();
    await expect(selected(page)).toHaveAttribute('data-tab', 'pages');
    await expect(page.locator('#preview-panel [data-session-page]').first()).toBeVisible();
    // on: the pages list, or the panel's header (over the Previews tab's
    // frame a touch is the frame's own document's, so its tabs are swiped on
    // the header, tab_swipe.go).
    const swipe = async (dir: 'left' | 'right', on: 'list' | 'header') => {
      const a = (await page.locator('#preview-panel [data-tab-swipe]').nth(on === 'header' ? 0 : 1).boundingBox())!;
      const y = on === 'header' ? a.y + a.height / 2 : a.y + Math.min(260, a.height / 2);
      const l = a.x + a.width * 0.85, r = a.x + a.width * 0.15;
      if (dir === 'left') await touchDrag(page, { x: l, y }, { x: r, y: y + 6 });
      else await touchDrag(page, { x: r, y }, { x: l, y: y - 4 });
    };
    // Rightwards on the list: the previous tab, Previews.
    await swipe('right', 'list');
    await expect(selected(page)).toHaveAttribute('data-tab', 'previews');
    // Leftwards on the header, over the preview's frame: Pages again, its
    // list re-read.
    await swipe('left', 'header');
    await expect(selected(page)).toHaveAttribute('data-tab', 'pages');
    await expect(page.locator('#preview-panel [data-session-page]').first()).toBeVisible();
    // On the last tab a leftwards swipe goes nowhere.
    await swipe('left', 'list');
    await expect(selected(page)).toHaveAttribute('data-tab', 'pages');
    // And the tab chosen by swiping is remembered, as a tap's is.
    expect(await page.evaluate(() => localStorage.getItem('muster:tabs:session-open:1'))).toBe('pages');
  });
});

// THE HEADER: one row, no overlap, every target 44px, the title clear of the
// controls — for a mirrored window and an agent, at both sizes, with the Open
// button drawn.
type Box = { what: string; x: number; y: number; w: number; h: number; tw: number; th: number; inTitle?: boolean };
async function headerBoxes(page: Page, header: string): Promise<{ controls: Box[]; title: Box; header: Box; docWide: number; vw: number }> {
  return page.evaluate((sel) => {
    const h = document.querySelector(sel) as HTMLElement;
    const shown = (el: Element) => {
      const r = el.getBoundingClientRect();
      return r.width > 0 && r.height > 0 && getComputedStyle(el).visibility !== 'hidden';
    };
    const box = (el: Element, what: string) => {
      const r = el.getBoundingClientRect();
      // Its tap target: the box, or a ::before hit area laid over it (the
      // status row's ⓘ, InfoSheet.Inline), whichever is larger.
      const b = getComputedStyle(el, '::before');
      const hit = b.content && b.content !== 'none' && b.position === 'absolute';
      return { what, x: r.x, y: r.y, w: r.width, h: r.height,
        tw: Math.max(r.width, hit ? parseFloat(b.width) || 0 : 0), th: Math.max(r.height, hit ? parseFloat(b.height) || 0 : 0) };
    };
    const title = h.querySelector('h1')!;
    const els = Array.from(h.querySelectorAll('button, a, select, label[role="button"], [role="button"]'))
      .filter((el) => !el.closest('[popover]') && !el.closest('h1') && shown(el))
      // A control inside another (a link inside a button) is its parent's.
      .filter((el, _i, all) => !all.some((o) => o !== el && o.contains(el)));
    // The row is what holds the last control (the ⋯); the title's column is
    // its child holding the <h1>, whose second line (the status row) is the
    // title's, not the row's.
    const row = els[els.length - 1].parentElement!.closest('header > div, header') as HTMLElement;
    let col: Element = title;
    while (col.parentElement && col.parentElement !== row) col = col.parentElement;
    const controls = els.map((el) => ({ ...box(el, el.id || el.getAttribute('aria-label') || el.textContent!.trim().slice(0, 20)), inTitle: col.contains(el) }));
    return { controls, title: box(title, 'title'), header: box(h, 'header'), docWide: document.documentElement.scrollWidth, vw: window.innerWidth };
  }, header);
}

function expectOneRow(m: Awaited<ReturnType<typeof headerBoxes>>, label: string) {
  const overlap = (a: Box, b: Box) => a.x < b.x + b.w - 0.5 && b.x < a.x + a.w - 0.5 && a.y < b.y + b.h - 0.5 && b.y < a.y + a.h - 0.5;
  expect(m.docWide, `${label}: the document is no wider than the screen`).toBeLessThanOrEqual(m.vw);
  for (const c of m.controls) {
    expect(Math.round(c.tw), `${label}: ${c.what} is a 44px target (width)`).toBeGreaterThanOrEqual(44);
    expect(Math.round(c.th), `${label}: ${c.what} is a 44px target (height)`).toBeGreaterThanOrEqual(44);
    expect(c.x + c.w, `${label}: ${c.what} is inside the screen`).toBeLessThanOrEqual(m.vw + 0.5);
  }
  for (let i = 0; i < m.controls.length; i++) {
    for (let j = i + 1; j < m.controls.length; j++) {
      expect(overlap(m.controls[i], m.controls[j]), `${label}: ${m.controls[i].what} overlaps ${m.controls[j].what}`).toBe(false);
    }
    // The title's own column (its second line, the status row's ⓘ) is the
    // title's block; every control of the ROW must clear the title's box.
    if (!m.controls[i].inTitle) expect(overlap(m.controls[i], m.title), `${label}: the title collides with ${m.controls[i].what}`).toBe(false);
  }
  // ONE ROW: every control's middle on one line, and the header one row tall.
  const mids = m.controls.filter((c) => !c.inTitle).map((c) => c.y + c.h / 2);
  expect(Math.max(...mids) - Math.min(...mids), `${label}: the controls are on one line (nothing wrapped)`).toBeLessThanOrEqual(1);
  expect(m.header.h, `${label}: the header is one row tall`).toBeLessThanOrEqual(58);
  expect(m.title.w, `${label}: the title keeps some room`).toBeGreaterThan(40);
}

// cls sums the page's layout shifts (not input-caused) while fn runs.
async function cls(page: Page, fn: () => Promise<void>): Promise<number> {
  await page.evaluate(() => {
    const w = window as unknown as { __cls: number; __clsOn?: boolean };
    w.__cls = 0;
    // ONE observer per document: a second would count every shift twice.
    if (w.__clsOn) return;
    w.__clsOn = true;
    new PerformanceObserver((l) => {
      for (const e of l.getEntries() as unknown as { value: number; hadRecentInput: boolean }[]) if (!e.hadRecentInput) w.__cls += e.value;
    }).observe({ type: 'layout-shift', buffered: false });
  });
  await fn();
  await page.waitForTimeout(500);
  return page.evaluate(() => (window as unknown as { __cls: number }).__cls);
}

for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`the session header at ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('a mirrored window: one row, no overlap, 44px targets; the ⋯ menu holds Home and the message index, by keyboard; the Open button’s count moves nothing', async ({ page }) => {
      test.setTimeout(120_000);
      await ready(page);
      await mintViewer(page);
      playPreviews('@1 2 1\n');
      await buttonCount(page, '@1', 7);
      await page.goto(origin + viewOf('@1'));
      await expect(toggle(page)).toHaveAttribute('data-preview-count', '7');
      expectOneRow(await headerBoxes(page, 'header'), `mirror ${vp.name}`);
      // THE ⋯ MENU: Home and the message index, opened and walked by keyboard,
      // closed by Escape.
      const trigger = page.locator('#mv-menu-open');
      const menu = page.locator('#mv-menu');
      await expect(menu).toBeHidden();
      await trigger.focus();
      await page.keyboard.press('Enter');
      await expect(menu).toBeVisible();
      await expect(trigger).toHaveAttribute('aria-expanded', 'true');
      await expect(menu.locator('#mv-back')).toHaveText('Home');
      await expect(menu.locator('#chat-index-open')).toBeVisible();
      await page.keyboard.press('Tab');
      expect(await page.evaluate(() => !!document.activeElement?.closest('#mv-menu')), 'Tab moves into the menu').toBe(true);
      await page.keyboard.press('Escape');
      await expect(menu).toBeHidden();
      await expect(trigger).toHaveAttribute('aria-expanded', 'false');
      // Its message index item opens the sheet, and closes the menu.
      await trigger.click();
      await menu.locator('#chat-index-open').click();
      await expect(menu).toBeHidden();
      await expect(page.locator('#chat-index')).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(page.locator('#chat-index')).toBeHidden();
      // Focus goes back to the ⋯ trigger: the item that opened the sheet is
      // in the closed menu and cannot hold it (it fell to <body>).
      await expect(trigger, 'focus returns to the ⋯ trigger when the sheet closes').toBeFocused();
      // A NEW COUNT MOVES NOTHING: 7 → 9.
      const shift = await cls(page, async () => {
        playPreviews('@1 4 1\n');
        await expect(toggle(page)).toHaveAttribute('data-preview-count', '9', { timeout: 25_000 });
      });
      expect(shift, 'layout shift when the count changed').toBeLessThan(0.001);
      expectOneRow(await headerBoxes(page, 'header'), `mirror ${vp.name}, count 9`);
    });

    test('the Open button appearing moves nothing', async ({ page }) => {
      test.setTimeout(120_000);
      await ready(page);
      await page.goto(origin + viewOf('@3'));
      await expect(page.locator('#mv-log [data-bubble="user"]').first()).toContainText('the gamma window');
      await expect(toggle(page)).toHaveCount(0);
      const before = await headerBoxes(page, 'header');
      let id = '';
      const shift = await cls(page, async () => {
        id = page_(`Gamma page ${vp.name} ${stamp}`, SC);
        await expect(toggle(page)).toHaveAttribute('data-preview-count', '1', { timeout: 25_000 });
      });
      expect(shift, 'layout shift when the button appeared').toBeLessThan(0.001);
      const after = await headerBoxes(page, 'header');
      expectOneRow(after, `mirror ${vp.name}, the button arrived`);
      expect(after.header.h, 'the header kept its height').toBeCloseTo(before.header.h, 1);
      fix('rmpage', id);
      // POSITIVE CONTROL: the same measurement sees a shift that is there (a
      // line pushed in above the header by script, not by input; inside the
      // conversation scroll anchoring would hide it).
      const control = await cls(page, async () => {
        await page.evaluate(() => {
          const d = document.createElement('div');
          d.style.height = '60px';
          d.style.flex = 'none';
          document.body.prepend(d);
        });
      });
      expect(control, 'the CLS measurement sees an unprovoked shift').toBeGreaterThan(0.001);
    });

    test('an agent session: one row, no overlap, 44px targets, with its Open button; Chat history in its ⋯ menu, by keyboard', async ({ page }) => {
      await ready(page);
      await page.goto(origin + `/agents/${agentName}`);
      await expect(toggle(page)).toHaveAttribute('data-preview-count', '2');
      expectOneRow(await headerBoxes(page, 'header[data-folded-header]'), `agent ${vp.name}`);
      // Chat history moved to the ⋯ menu (New chat stays in the row).
      await expect(page.locator('header [data-header-tools] button[aria-label="New chat session"]')).toBeVisible();
      await expect(page.locator('header [data-header-tools] label[aria-label="Chat history"]')).toHaveCount(0);
      await chatHistoryByKeyboard(page);
    });

    test('a gateway agent’s two-row header: its first row one line, no overlap, 44px targets, with its Open button', async ({ page }) => {
      await ready(page);
      await page.goto(origin + `/agents/${otherAgentName}`);
      await expect(toggle(page)).toHaveAttribute('data-preview-count', '1');
      const m = await page.evaluate(() => {
        const row = document.querySelector('header > div > div') as HTMLElement;
        const t = row.querySelector('h1')!.getBoundingClientRect();
        const cs = Array.from(row.querySelectorAll(':scope button, :scope a, :scope label[role="button"]'))
          .filter((el) => !el.closest('h1') && !el.closest('[popover]') && el.getBoundingClientRect().width > 0)
          .map((el) => { const r = el.getBoundingClientRect(); return { what: el.getAttribute('aria-label') || el.id, x: r.x, y: r.y, w: r.width, h: r.height }; });
        return { t: { x: t.x, y: t.y, w: t.width, h: t.height }, cs, vw: window.innerWidth, wide: document.documentElement.scrollWidth };
      });
      expect(m.wide).toBeLessThanOrEqual(m.vw);
      const mids = m.cs.map((c) => c.y + c.h / 2);
      expect(Math.max(...mids) - Math.min(...mids), 'one line').toBeLessThanOrEqual(1);
      for (const c of m.cs) {
        expect(Math.round(c.w), `${c.what} width`).toBeGreaterThanOrEqual(44);
        expect(Math.round(c.h), `${c.what} height`).toBeGreaterThanOrEqual(44);
        expect(c.x >= m.t.x + m.t.w - 0.5 || c.x + c.w <= m.t.x + 0.5, `the title collides with ${c.what}`).toBe(true);
      }
      expect(m.t.w, 'the title keeps some room').toBeGreaterThan(40);
      await chatHistoryByKeyboard(page);
    });
  });
}

// chatHistoryByKeyboard opens an agent page's ⋯ menu from its trigger with
// Enter, Tabs to its Chat history item and activates it with Enter: the
// session drawer opens, and the menu closes.
async function chatHistoryByKeyboard(page: Page): Promise<void> {
  const trigger = page.locator('#agent-actions-open');
  const menu = page.locator('#agent-actions');
  const item = menu.locator('[data-chat-history]');
  await expect(page.locator('#session-drawer')).not.toBeChecked();
  await trigger.focus();
  await page.keyboard.press('Enter');
  await expect(menu).toBeVisible();
  await expect(item).toHaveText('Chat history');
  // Tab through the menu until Chat history has focus (a menu's first
  // focusable items may come before it).
  for (let i = 0; i < 6 && !(await item.evaluate((el) => el === document.activeElement)); i++) await page.keyboard.press('Tab');
  await expect(item, 'Tab reaches Chat history').toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.locator('#session-drawer'), 'Enter on Chat history opens the drawer').toBeChecked();
  await expect(menu).toBeHidden();
}

// page_ makes a page for session sid on the rig's host, now.
function page_(title: string, sid: string): string {
  return fix('page', OWNER, title, '0', rig.host, sid, '1', '0', '0');
}
