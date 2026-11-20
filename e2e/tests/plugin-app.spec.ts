import { execFileSync } from 'node:child_process';
import { Browser, BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, pluginAppEdgePort, pluginAppPort, pluginAppStubPort, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { signIn } from '../lib/signin';
import { openMore } from '../lib/nav';
import { APP_ENTRY, APP_ORIGIN, APP_SANDBOX, APP_ZONE, AppStub, CONTROL_ORIGIN, Report, appStubArgs } from '../lib/appstub';

// THE APP TAB (docs/plugins.md#app-tab; docs/design/meetings.md "The frame,
// exactly", "When the token is revoked"; core PR 1b), on the owner's click
// path in a real Chromium: a REAL muster-server with plugins and
// MUSTER_PLUGIN_ORIGINS on, an app plugin minted through the real /plugins
// form, and a STUB app plugin (e2e/lib/appstub.ts) on a real https origin
// under the zone, whose entry route redeems the code with the plugin's own
// token and whose page asks for the camera, the microphone and the screen and
// reports each result.
//
// 🔴 TRACKS, NOT "NO ERROR". Under the design's first header (spike S5,
// camera=("<O>") without self) getUserMedia RESOLVED with the audio track and
// no video track. So the positive asserts one live audio and one live video
// track, a getDisplayMedia that resolves to a video track, and allowsFeature
// for each — and the negative controls show the fake-UI flags are not what
// grants them: the same stub framed without `allow`, without display-capture
// in it, or under a header without self, is refused. Those framings are the
// stub's own test-only pages (control.<zone>), never a muster route.
//
// ⚠ "ITS TRACKS ENDED" IS OBSERVED AS ITS DOCUMENT GOING. Measured on
// Chromium 153: a frame removed from the page fires no `ended` on its tracks
// (the event is for an end the page did not cause), and the stub's pagehide
// still reads them `live` — the document that owns them is being destroyed,
// and they with it. So the revoke case asserts the stub's `pagehide`, then
// that its heartbeat (which reports every track's state each second) stops.
//
// 🔴 ONLY THIS SPEC'S SERVER (and cls.spec's app server) HAS THIS ZONE in
// MUSTER_PLUGIN_ORIGINS, and afterAll removes the plugins it made (pluginfix
// rmplugin), so no other spec draws this tab.
const OWNER = '1'; // the password-mode operator (as plugin-chip.spec's)

test.use({
  storageState: { cookies: [], origins: [] },
  ignoreHTTPSErrors: true,
  launchOptions: { args: appStubArgs(pluginAppEdgePort) },
});

const viewports = [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
];

let server: Running | undefined;
const origin = `http://localhost:${pluginAppPort}`;
const stub = new AppStub(origin, pluginAppStubPort, pluginAppEdgePort);
const names: string[] = [];
let appId = 0;

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

// mintApp creates an app plugin through the real /plugins form and returns
// its token and id.
async function mintApp(page: Page, label: string): Promise<{ token: string; id: number }> {
  const name = 'e2e-app-' + Date.now().toString(36) + '-' + names.length;
  names.push(name);
  const form = new URLSearchParams({ name, label, data_scope: 'example', origin: APP_ZONE, entry_url: APP_ENTRY, app_label: label, app_icon: 'video' });
  form.append('capability', 'app');
  const r = await page.request.post(origin + '/plugins', {
    headers: { Origin: origin, 'Content-Type': 'application/x-www-form-urlencoded' },
    data: form.toString(),
  });
  const html = await r.text();
  expect(r.status(), html.slice(0, 400)).toBe(200);
  const token = /id="minted-plugin-token"[^>]*>(mpl_[A-Za-z0-9_-]+)</.exec(html)?.[1];
  expect(token, 'an app plugin token was minted').toBeTruthy();
  const self = await page.request.get(origin + '/plugin/v1/self', { headers: { Authorization: `Bearer ${token}` } });
  const body = (await self.json()) as { plugin_id: number; capabilities: string[] };
  expect(body.capabilities).toEqual(['app']);
  return { token: token!, id: body.plugin_id };
}

async function signedInContext(browser: Browser, use: object = {}): Promise<{ ctx: BrowserContext; page: Page }> {
  const ctx = await browser.newContext({ ...use, ignoreHTTPSErrors: true, storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await signIn(page, origin);
  return { ctx, page };
}

// newEntry waits for the stub's entry route to be hit after `before` entries,
// and its page's result report (media on) to arrive; returns both.
async function newEntry(before: number, what: string) {
  await expect.poll(() => stub.entries.length, { message: `${what}: the frame reached the stub's entry route`, timeout: 20_000 }).toBeGreaterThan(before);
  const entry = stub.entries[stub.entries.length - 1];
  await expect.poll(() => !!stub.result(entry.run), { message: `${what}: the stub reported its media results`, timeout: 20_000 }).toBe(true);
  return { entry, result: stub.result(entry.run)! };
}

const kinds = (r: { ok: boolean; tracks?: { kind: string; readyState: string }[] }) =>
  r.ok ? r.tracks!.map((t) => `${t.kind}:${t.readyState}`).sort() : [];

test.beforeAll(async ({ browser }) => {
  test.setTimeout(120_000);
  await stub.start();
  server = await startServer(versionA, pluginAppPort, { MUSTER_PLUGINS: '1', MUSTER_PLUGIN_ORIGINS: APP_ZONE });
  const { ctx, page } = await signedInContext(browser);
  const minted = await mintApp(page, "Tom's calls");
  stub.tokens.set(String(minted.id), minted.token);
  appId = minted.id;
  await ctx.close();
});

test.afterAll(async () => {
  for (const n of names) fix('rmplugin', OWNER, n);
  await server?.stop();
  await stub.stop();
});

for (const vp of viewports) {
  test(`${vp.name}: the tab is the last section link and opens /apps/{id} with the frame the whole width below the header`, async ({ browser }) => {
    const { ctx, page } = await signedInContext(browser, vp.use);
    try {
      await page.goto(origin + '/tasks');
      const link = page.locator(`#sidebar-app-${appId}`);
      await expect(link).toHaveAttribute('href', `/apps/${appId}`);
      await expect(link).toContainText("Tom's calls");
      // The last of the section links: after every built-in one, before the
      // account links (#sidebar-prefs).
      const sections = await page.locator('#sidebar-more-body').evaluate((body) =>
        [...body.querySelectorAll('a')].filter((a) => !a.closest('#sidebar-prefs')).map((a) => a.id),
      );
      expect(sections.length, 'positive control: the built-in section links are read').toBeGreaterThan(2);
      expect(sections[sections.length - 1], `section links ${sections.join(', ')}`).toBe(`sidebar-app-${appId}`);
      await expect(link).not.toHaveAttribute('aria-current', 'page');

      // A tap on it, the way a person gets there: the sidebar opened (a
      // phone) and More expanded.
      const open = page.locator('#sidebar-open');
      if (await open.isVisible()) await open.click();
      await openMore(page);
      const before = stub.entries.length;
      await Promise.all([page.waitForURL(origin + `/apps/${appId}`), link.click()]);

      // Its own entry is current on its page.
      await expect(page.locator(`#sidebar-app-${appId}`)).toHaveAttribute('aria-current', 'page');
      const frame = page.locator('[data-app-frame]');
      await expect(frame).toHaveAttribute('src', new RegExp(`^${APP_ENTRY.replace(/[.]/g, '\\.')}\\?muster_code=[^&]+&muster_plugin=${appId}$`));
      await expect(frame).toHaveAttribute('sandbox', APP_SANDBOX);
      await newEntry(before, vp.name);
      // Full width, from the header's bottom to the viewport's.
      const vw = vp.use.viewport.width;
      const vh = vp.use.viewport.height;
      const fb = (await frame.boundingBox())!;
      const hb = (await page.locator('[data-app-header]').boundingBox())!;
      expect([Math.round(fb.x), Math.round(fb.width)], 'the frame is the whole width').toEqual([0, vw]);
      expect(Math.round(fb.y), 'the frame starts at the header').toBe(Math.round(hb.y + hb.height));
      expect(Math.round(fb.y + fb.height), 'the frame runs to the bottom').toBe(vh);
      // The sidebar is an overlay at every width: shut, it takes no width.
      const sidebar = await page.locator('#sidebar').boundingBox();
      expect(sidebar === null || sidebar.x + sidebar.width <= 0, `the sidebar is off the page (${JSON.stringify(sidebar)})`).toBe(true);
    } finally {
      await ctx.close();
    }
  });
}

test('positive: inside the frame the camera, the microphone and the screen are granted, as tracks, and the redeem answered purpose app', async ({ browser }) => {
  const { ctx, page } = await signedInContext(browser, viewports[1].use);
  try {
    const before = stub.entries.length;
    const resp = await page.goto(origin + `/apps/${appId}`);
    const { entry, result } = await newEntry(before, 'positive');
    // Soft: every claim is checked and each failing one named, so a broken
    // header or allow attribute says WHICH grant it took away.
    expect.soft(result.um, 'getUserMedia({audio, video}) resolved').toMatchObject({ ok: true });
    expect.soft(kinds(result.um), 'one LIVE audio and one LIVE video track').toEqual(['audio:live', 'video:live']);
    expect.soft(result.dm, 'getDisplayMedia() resolved').toMatchObject({ ok: true });
    expect.soft(kinds(result.dm), 'getDisplayMedia resolved to a live video track').toEqual(['video:live']);
    expect.soft(result.allows, 'featurePolicy.allowsFeature in the frame').toEqual({ camera: true, microphone: true, 'display-capture': true });
    expect.soft(entry.plugin).toBe(String(appId));
    expect.soft([entry.status, entry.body?.purpose, entry.body?.target], 'the redeem answered purpose app').toEqual([200, 'app', { app: {} }]);
    // The header this page sent (the delegation's other half), as the design
    // writes it. Last, so a broken header is first seen as the tracks it cost.
    const o = APP_ORIGIN;
    expect.soft(resp!.headers()['permissions-policy'], "the page's Permissions-Policy").toBe(
      `microphone=(self "${o}"), camera=(self "${o}"), display-capture=(self "${o}"), fullscreen=(self "${o}"), autoplay=(self "${o}"), clipboard-write=(self "${o}")`,
    );
  } finally {
    await ctx.close();
  }
});

// The negative controls: the same stub page, framed by the stub's test-only
// control page with the production sandbox and the `allow`/header under test.
test.describe('negative controls: the flags do not grant what the frame was not delegated', () => {
  async function framed(browser: Browser, name: string): Promise<Extract<Report, { event: 'result' }>> {
    const ctx = await browser.newContext({ ignoreHTTPSErrors: true, storageState: { cookies: [], origins: [] } });
    try {
      const page = await ctx.newPage();
      const run = stub.nextRun(name);
      await page.goto(`${CONTROL_ORIGIN}/frame?case=${name}&run=${encodeURIComponent(run)}`);
      await expect.poll(() => !!stub.result(run), { message: `${name}: the framed stub reported`, timeout: 20_000 }).toBe(true);
      return stub.result(run)!;
    } finally {
      await ctx.close();
    }
  }

  test("the instrument's positive control: the control page's full delegation grants all three", async ({ browser }) => {
    const r = await framed(browser, 'full');
    expect(r.allows).toEqual({ camera: true, microphone: true, 'display-capture': true });
    expect(kinds(r.um)).toEqual(['audio:live', 'video:live']);
    expect(kinds(r.dm)).toEqual(['video:live']);
  });

  test('framed without the allow attribute: denied all three', async ({ browser }) => {
    const r = await framed(browser, 'noallow');
    expect(r.allows).toEqual({ camera: false, microphone: false, 'display-capture': false });
    expect(r.um).toEqual({ ok: false, error: 'NotAllowedError' });
    expect(r.dm).toEqual({ ok: false, error: 'NotAllowedError' });
  });

  test('display-capture dropped from allow: only getDisplayMedia fails, with NotAllowedError', async ({ browser }) => {
    const r = await framed(browser, 'nodc');
    expect(r.allows).toEqual({ camera: true, microphone: true, 'display-capture': false });
    expect(kinds(r.um)).toEqual(['audio:live', 'video:live']);
    expect(r.dm).toEqual({ ok: false, error: 'NotAllowedError' });
  });

  test('a header without self (the first revision, spike S5): no video track', async ({ browser }) => {
    const r = await framed(browser, 'noself');
    expect(r.allows.camera).toBe(false);
    // The S5 shape: the call RESOLVES, with the audio and without the video.
    expect(r.um).toMatchObject({ ok: true });
    expect(kinds(r.um)).toEqual(['audio:live']);
    expect(r.dm).toEqual({ ok: false, error: 'NotAllowedError' });
  });
});

test('in the Android app (window.musterApp) no frame is drawn, the Open card is, and its tap navigates top level to a freshly minted URL', async ({ browser }) => {
  const { ctx, page } = await signedInContext(browser, viewports[0].use);
  try {
    await page.addInitScript(() => {
      (window as unknown as { musterApp: object }).musterApp = {};
    });
    const before = stub.entries.length;
    await page.goto(origin + `/apps/${appId}`);
    const card = page.locator('[data-app-card]');
    await expect(card).toBeVisible();
    await expect(card).toContainText("Tom's calls opens in your browser");
    await expect(page.locator('[data-app-frame]')).toHaveCount(0);
    await expect(page.locator('[data-app-newtab]')).toBeHidden();
    // Nothing was minted or loaded at load: the stub was not reached.
    await page.waitForTimeout(1_000);
    expect(stub.entries.length, 'the stub was reached before the tap').toBe(before);
    const codes = new Set(stub.entries.map((e) => e.code));

    await Promise.all([page.waitForURL((u) => u.href.startsWith(APP_ENTRY + '?')), card.locator('[data-app-open]').click()]);
    const top = new URL(page.url());
    expect(top.searchParams.get('muster_plugin')).toBe(String(appId));
    const code = top.searchParams.get('muster_code') ?? '';
    expect(codes.has(code), 'the code is a fresh one').toBe(false);
    await expect.poll(() => stub.entries.length, { timeout: 20_000 }).toBe(before + 1);
    const entry = stub.entries[stub.entries.length - 1];
    expect(entry.code).toBe(code);
    expect([entry.status, entry.body?.purpose], 'the tap-time code redeemed, purpose app').toEqual([200, 'app']);
    // Top level: the stub IS the page, not a frame of muster's.
    expect(page.mainFrame().url().startsWith(APP_ORIGIN + '/')).toBe(true);
  } finally {
    await ctx.close();
  }
});

// Last: it revokes a plugin of its own, so the cases above keep theirs.
test('revoking the plugin on /plugins in a second tab removes the frame within 35 s, and the stub page goes with its tracks', async ({ browser }) => {
  test.setTimeout(120_000);
  const { ctx, page } = await signedInContext(browser, viewports[1].use);
  try {
    const own = await mintApp(page, 'Revoked calls');
    stub.tokens.set(String(own.id), own.token);
    const before = stub.entries.length;
    await page.goto(origin + `/apps/${own.id}`);
    const { entry, result } = await newEntry(before, 'revoke');
    expect([entry.status, entry.body?.purpose]).toEqual([200, 'app']);
    expect(kinds(result.um)).toEqual(['audio:live', 'video:live']);
    // CONTROL: while it is live the stub's heartbeat arrives with its tracks live.
    await expect
      .poll(() => stub.reportsOf(entry.run).filter((r) => r.event === 'beat').length, { message: 'the stub beats while live', timeout: 10_000 })
      .toBeGreaterThanOrEqual(2);
    const frame = page.locator('[data-app-frame]');
    await expect(frame).toHaveCount(1);

    const second = await ctx.newPage();
    await second.goto(origin + '/plugins');
    const card = second.locator(`li[data-plugin-id="${own.id}"]`);
    await Promise.all([second.waitForLoadState('load'), card.locator(`form[action="/plugins/${own.id}/revoke"] button[type=submit]`).click()]);
    await expect(second.locator(`li[data-plugin-id="${own.id}"]`)).toContainText('revoked');
    const revokedAt = Date.now();
    // The poll runs only while the page is visible: the first tab must still be.
    expect(await page.evaluate(() => document.visibilityState)).toBe('visible');

    await expect(frame, 'the frame is still on the page 35 s after the revoke').toHaveCount(0, { timeout: 35_000 });
    const tookMs = Date.now() - revokedAt;
    await expect(page.locator('#app-status')).toHaveText('This app was removed.');
    await expect(page.locator('[data-app-newtab]')).toBeHidden();
    console.log(`[plugin-app] the frame went ${tookMs} ms after the revoke`);

    // The stub's document went: its pagehide, then no heartbeat after it.
    await expect
      .poll(() => stub.reportsOf(entry.run).some((r) => r.event === 'pagehide'), { message: 'the stub reported pagehide', timeout: 10_000 })
      .toBe(true);
    const gone = stub.reportsOf(entry.run).find((r) => r.event === 'pagehide')!;
    await page.waitForTimeout(3_000);
    const after = stub.reportsOf(entry.run).filter((r) => r.event === 'beat' && r.at > gone.at + 500);
    expect(after, 'the stub page (and its tracks) outlived the frame').toEqual([]);
  } finally {
    await ctx.close();
  }
});
