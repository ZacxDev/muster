import { execFileSync } from 'node:child_process';
import { BrowserContext, Page, Route } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, pluginChipPort, repoRoot } from '../lib/env';
import { cc, entryMatches, mirroredEntries, MirrorRig } from '../lib/mirrorhost';

// THE SESSION-VIEW CHIP (docs/plugins.md#the-session-view-chip), on the
// operator's click path: a REAL muster-server with the mirror plugin, plugins
// and MUSTER_PLUGIN_ORIGINS on, a REAL muster-mirror (`-tags e2emirror`) with a
// Claude Code window (@1) and a shell (@2), and a viewer plugin minted through
// the real /plugins form.
//
// ⚠ WHAT IS PLAYED: the plugin's ENTRY PAGE. Its URL is https under an
// invented zone, so the browser's requests to it are answered by page.route
// here — which, like a real plugin's entry route, REDEEMS the code with the
// plugin's own token (POST /plugin/v1/handoffs/redeem on the real server) and
// draws what it learned. So a landing proves the whole hand-off: the chip
// minted at click time, the browser went to the entry URL with muster_code and
// muster_plugin, and the code was good for this owner and this window.
//
// 🔴 ONLY THIS SPEC'S SERVER HAS MUSTER_PLUGIN_ORIGINS SET, and afterAll
// removes the plugins it made (pluginfix rmplugin), so no other spec on the
// shared database draws a chip.
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

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
  const now = Math.floor(Date.now() / 1000);
  const tr = rig.transcript(cc.prompt('the prompt of the chip window') + cc.reply('Answered.'), 'one');
  const windows = rig.transcript(`work|@1|1|1|claude|${now - 60}|${tr}\nwork|@2|2|0|zsh|${now - 120}|\n`, 'windows');
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
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

test('a viewer plugin chip on both session views: Open lands on the entry page in a new tab with no opener, Show here frames it sandboxed, the view swap still frames this origin, and a revoked token draws none', async ({ page, context }) => {
  test.setTimeout(120_000);
  await ready(page);
  const { token, id } = await mintViewer(page, 'Example viewer');
  const landings = await playEntry(context, token);

  // The Rendered view: one chip, and the CSP frames the plugin's origin and
  // this one.
  const resp = await page.goto(origin + viewOf('@1'));
  expect(resp!.headers()['content-security-policy']).toContain(`frame-src 'self' ${ZONE}; `);
  await expect(page.locator('#mv-log [data-bubble="user"]').first()).toContainText('the prompt of the chip window');
  const chip = page.locator(`#plugin-chips [data-plugin-chip="${id}"]`);
  await expect(chip).toHaveCount(1);
  await expect(page.locator('#plugin-chips [data-plugin-chip]')).toHaveCount(1);
  await expect(chip).toContainText('Example viewer');
  const open = chip.locator('[data-plugin-open]');
  await expect(open).toHaveAttribute('target', '_blank');
  await expect(open).toHaveAttribute('rel', 'noopener noreferrer');
  // Nothing is minted to draw it.
  expect(landings, 'no landing before a click').toHaveLength(0);

  // OPEN: a new tab, minted at the click, on the entry page with muster_code
  // and muster_plugin; the plugin redeems it for THIS window; no opener.
  const [tab] = await Promise.all([context.waitForEvent('page'), open.click()]);
  await expect(tab.locator('#fake-entry')).toBeVisible();
  const landed = new URL(tab.url());
  expect(landed.origin + landed.pathname).toBe(ENTRY);
  expect(landed.searchParams.get('muster_plugin')).toBe(String(id));
  expect(landed.searchParams.get('muster_code') ?? '').toMatch(/^.{20,}$/);
  await expect(tab.locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
  await expect(tab.locator('#fake-opener')).toHaveText('opener:none');
  await expect(tab.locator('#fake-referrer')).toHaveText('referrer:none');
  expect(landings).toHaveLength(1);
  expect(landings[0].referer, 'no Referer reaches the plugin').toBeUndefined();
  await tab.close();

  // SHOW HERE: the dialog's frame, with exactly the sandbox set and no
  // top-navigation, lands on the entry page (CSP frame-src lets it load).
  await chip.locator('[data-plugin-show]').click();
  const frameEl = page.locator('#plugin-frame iframe');
  await expect(page.locator('#plugin-frame')).toBeVisible();
  await expect(frameEl).toHaveAttribute('sandbox', SANDBOX);
  await expect(frameEl).toHaveAttribute('allow', '');
  await expect(frameEl).toHaveAttribute('referrerpolicy', 'no-referrer');
  expect(await frameEl.getAttribute('sandbox')).not.toContain('allow-top-navigation');
  const framed = page.frameLocator('#plugin-frame iframe');
  await expect(framed.locator('#fake-redeem')).toHaveText(`200 ${rig.host} @1`);
  expect(landings).toHaveLength(2);
  expect(landings[1].url.searchParams.get('muster_code'), 'a fresh code per click').not.toBe(landings[0].url.searchParams.get('muster_code'));
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
  await page.locator('#plugin-frame [data-plugin-close]').click();
  await expect(page.locator('#plugin-frame')).toBeHidden();

  // POSITIVE CONTROL for 'self' in frame-src: the Rendered | Terminal switch
  // still opens this origin's other view in its frame, on this page.
  await page.locator('#view-terminal').click();
  await expect(page.locator('iframe[data-muster-view]')).toBeVisible();
  await expect(page.frameLocator('iframe[data-muster-view]').locator('#term')).toHaveCount(1);
  // The Terminal view draws the chip too, aimed at the same window.
  await expect(page.frameLocator('iframe[data-muster-view]').locator(`#plugin-chips[data-window="@1"] [data-plugin-chip="${id}"]`)).toHaveCount(1);

  // A window with NO rendered view opens on /mirror/terminal: the chip is
  // there, aimed at that window, and its Open lands for it.
  const t = await page.goto(origin + viewOf('@2'));
  expect(new URL(page.url()).pathname).toBe('/mirror/terminal');
  expect(t!.headers()['content-security-policy']).toContain(`frame-src 'self' ${ZONE}; `);
  const tchip = page.locator(`#plugin-chips[data-window="@2"] [data-plugin-chip="${id}"]`);
  await expect(tchip).toHaveCount(1);
  const [tab2] = await Promise.all([context.waitForEvent('page'), tchip.locator('[data-plugin-open]').click()]);
  await expect(tab2.locator('#fake-redeem')).toHaveText(`200 ${rig.host} @2`);
  await tab2.close();

  // Any other page: no frame-src.
  const other = await page.goto(origin + '/tasks');
  expect(other!.headers()['content-security-policy']).not.toContain('frame-src');

  // REVOKED: the chip is gone and the views' CSP has no frame-src again.
  const rv = await page.request.post(origin + `/plugins/${id}/revoke`, { headers: { Origin: origin }, maxRedirects: 0 });
  expect(rv.status(), await rv.text()).toBe(303);
  for (const path of [viewOf('@1'), termOf('@2')]) {
    const r = await page.goto(origin + path);
    expect(r!.headers()['content-security-policy'], path).not.toContain('frame-src');
    await expect(page.locator('#plugin-chips'), path).toHaveCount(0);
  }
});
