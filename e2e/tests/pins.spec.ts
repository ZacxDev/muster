import fs from 'node:fs';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { hookToken, pinsPort } from '../lib/env';
import { cc, MirrorRig, mkchat } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';
import { clsInit, readCls, settle } from '../lib/cls';

// PINNED ENTRIES, in a real browser, against a real muster-server
// (MUSTER_MIRROR=1, its own port) and a real muster-mirror over the tunnel
// (the `-tags e2emirror` per-window fake, as mirror-windows.spec.ts), at a
// phone's size and a desktop's.
//
// The operator asked "Add 'pinned' agents so they appear first in list". The
// Agents list page — whose cards carried a pin toggle and drew the pinned ones
// first — is retired: a session is pinned from its own page, by the session
// switcher's ⋯ menu (#switcher-more-btn, its item #switcher-pin: "open it,
// then pin"), and the pinned
// entries are the bar's Pins view and lead its All view. The host gives two
// sessions SYNTHETIC codenames (quince, sable) through its labels file. The
// entries pinned are the LEAST recently active (the two older windows, a
// seeded agent), so "it leads All" cannot be the activity order doing it. Pins
// are server-stored, so they must survive a reload and reach another page.
//
// And a pin keeps an entry out of the sidebar's collapsed Idle group (a pinned
// entry is never idle: InIdleSection).
//
// 🔴 THE AGENT PIN IS ON A SEEDED AGENT, WHICH EVERY SERVER IN THE SUITE LISTS
// (one database, one operator). The test unpins it again, and afterAll unpins
// it once more through the route, so no later spec sees it pinned.

// The server, its mirror token and the fake host are e2e/lib/mirrorhost.ts's.
const rig = new MirrorRig(pinsPort, 'e2e-pin');
const origin = rig.origin;
const host = rig.host;
const windowsFile = path.join(rig.dir, 'windows');
const labelsFile = path.join(rig.dir, 'labels.json');
const SEEDED = 'notes-tidy';
let pinnedAgentID = '';

const now = Math.floor(Date.now() / 1000);
// session|id|index|active|command|activity|claude-transcript
// pinx, pins and pinq are Claude Code windows (each opens its Rendered view,
// whose bar carries the pin toggle). pinold has had no activity for 30 days:
// it is drawn in the sidebar's collapsed Idle group (MUSTER_AGENTS_IDLE_AFTER
// defaults to 14 days). pinsh is a plain shell: terminal only.
const windows = (tr: (s: string) => string) => [
  `pinx|@1|1|1|claude|${now - 60}|${tr('pinx')}`,
  `pins|@2|1|1|claude|${now - 3600}|${tr('pins')}`,
  `pinq|@3|1|1|claude|${now - 7200}|${tr('pinq')}`,
  `pinold|@4|1|1|zsh|${now - 30 * 86400}|`,
  `pinsh|@5|1|1|zsh|${now - 600}|`,
];
const view = (session: string, id: number) => `/mirror/view?host=${host}&session=${session}&window=%40${id}`;
const term = (session: string, id: number) => `/mirror/terminal?host=${host}&session=${session}&window=%40${id}`;
const unpinnedHref = view('pinx', 1);
const sableHref = view('pins', 2);
const quinceHref = view('pinq', 3);
const oldHref = term('pinold', 4);
const shellHref = term('pinsh', 5);

test.beforeAll(async () => {
  await rig.start();
  writeFileAtomic(windowsFile, windows((s) => rig.transcript(cc.prompt('the prompt of ' + s) + cc.reply('Answered in ' + s + '.'), s)).join('\n') + '\n');
  fs.writeFileSync(labelsFile, JSON.stringify({ pinq: 'quince', pins: 'sable' }));
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile, MUSTER_MIRROR_LABELS_FILE: labelsFile });
  const r = await fetch(origin + '/api/agents', { headers: { Authorization: `Bearer ${hookToken}` } });
  const a = ((await r.json()) as { id: number; name: string }[]).find((x) => x.name === SEEDED);
  expect(a, `${SEEDED} is listed`).toBeTruthy();
  pinnedAgentID = String(a!.id);
});

let signedIn: Awaited<ReturnType<BrowserContext['cookies']>> | undefined;

async function unpinRoute(body: string) {
  if (!signedIn) return;
  const cookie = signedIn.map((c) => `${c.name}=${c.value}`).join('; ');
  await fetch(origin + '/agents/pins/delete', {
    method: 'POST',
    headers: { Cookie: cookie, Origin: origin, 'Content-Type': 'application/x-www-form-urlencoded', 'HX-Request': 'true' },
    body,
  }).catch(() => undefined);
}

test.afterAll(async () => {
  // Belt and braces: the seeded agent must not stay pinned for later specs.
  if (pinnedAgentID) await unpinRoute(`kind=agent&agent_id=${pinnedAgentID}`);
  await rig.stop();
});

async function login(page: Page) {
  await signIn(page, origin);
  signedIn = await page.context().cookies(origin);
}

// pinToggle is the ⋯ menu's Pin / Unpin item (hidden until the ⋯ opens);
// moreBtn the ⋯ that opens it.
const pinToggle = (page: Page) => page.locator('#switcher-pin');
const moreBtn = (page: Page) => page.locator('#session-switcher #switcher-more-btn');
const viewRow = (page: Page, key: string) => page.locator(`#session-switcher [data-switcher-list="${key}"]`);
// rowHrefs is a bar view's entries as their links ("current" for the page's
// own, which is not one), each agent link without its ?session= query.
const rowHrefs = (page: Page, key: string) =>
  viewRow(page, key)
    .locator('[data-switcher-entry]')
    .evaluateAll((els) => els.map((e) => {
      const h = e.getAttribute('href');
      if (!h) return 'current';
      return h.startsWith('/agents/') ? h.split('?')[0] : h;
    }));

// pinVia pins (or unpins) through the route, as ANOTHER tab would.
async function pinVia(page: Page, on: boolean, form: Record<string, string>) {
  const r = await page.request.post(origin + (on ? '/agents/pins' : '/agents/pins/delete'), { form, headers: { Origin: origin } });
  expect(r.status(), `${on ? 'pin' : 'unpin'} ${JSON.stringify(form)}`).toBe(200);
}
const winForm = (session: string, id: number, h = host) => ({ kind: 'mirror', host: h, session, window: '@' + id });

// open goes to a session page (a window's terminal or an agent's chat) once
// the host's windows are listed, with the bar's All view shown.
async function open(page: Page, url: string) {
  await login(page);
  await rig.waitListed(page, 'pinq');
  await page.addInitScript(() => { try { localStorage.setItem('muster.switcher.view', 'all'); } catch (e) {} });
  await page.goto(origin + url);
  await expect(moreBtn(page)).toBeVisible({ timeout: 20_000 });
  await expect(pinToggle(page)).toBeAttached();
}

// pinHere opens the bar's ⋯ menu, taps the page's own Pin (or Unpin) in it and
// waits until it says pinned (or not) and the menu has closed.
async function pinHere(page: Page, on: boolean, touch: boolean) {
  const t = pinToggle(page);
  await expect(t).toHaveAttribute('data-pinned', on ? 'false' : 'true');
  await expect(t, 'the pin is in the ⋯ menu, not the bar').toBeHidden();
  await (touch ? moreBtn(page).tap() : moreBtn(page).click());
  await expect(t).toBeVisible();
  await expect(t).toHaveAccessibleName(on ? 'Pin this session' : 'Unpin this session');
  await (touch ? t.tap() : t.click());
  await expect(t).toHaveAttribute('data-pinned', on ? 'true' : 'false');
  await expect(page.locator('#switcher-more'), 'the menu closes once used').toBeHidden();
}

// noOverflow: the page never scrolls sideways.
async function noOverflow(page: Page, what: string) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, `${what}: document scrollWidth`).toBeLessThanOrEqual(cw);
}

const agentPath = '/agents/' + SEEDED;
const PINNED = [agentPath, sableHref, quinceHref];

for (const vp of [
  { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('pinned entries lead All and fill Pins, survive a reload, reach another page, open their session, and unpin from their own toggle', async ({ page }) => {
      const touch = vp.use.hasTouch;
      // Read from the one unpinned window's page: it is none of the pinned.
      await open(page, unpinnedHref);
      const before = await rowHrefs(page, 'all');
      expect(before.filter((h) => PINNED.includes(h)).length, 'the three are listed').toBe(3);
      expect(before.indexOf(sableHref), 'sable is not first by activity').toBeGreaterThan(before.indexOf('current'));
      await expect(viewRow(page, 'pins')).toHaveText('Nothing pinned');
      await noOverflow(page, 'unpinned');

      // Pin through the real control, on each one's own page: open it, then pin.
      for (const h of [agentPath, sableHref, quinceHref]) {
        await open(page, h);
        const mb = (await moreBtn(page).boundingBox())!;
        expect(mb.width, 'the ⋯ is a 44px target').toBeGreaterThanOrEqual(44);
        expect(mb.height, 'the ⋯ is a 44px target').toBeGreaterThanOrEqual(44);
        await pinHere(page, true, touch);
      }
      // On another session's page: Pins holds the three, and All leads with
      // them — by activity among themselves (the seeded agent is newer than
      // both windows) — then the rest in the order they had.
      const want = [agentPath, sableHref, quinceHref, ...before.filter((x) => !PINNED.includes(x))];
      await open(page, unpinnedHref);
      await expect.poll(() => rowHrefs(page, 'all'), { message: 'the pinned entries, then the rest in their order' }).toEqual(want);
      expect((await rowHrefs(page, 'pins')).slice().sort()).toEqual(PINNED.slice().sort());
      await noOverflow(page, 'pinned');
      // And a reload: the pins are the server's, not the page's.
      await page.reload();
      await expect.poll(() => rowHrefs(page, 'all'), { message: 'after a reload', timeout: 20_000 }).toEqual(want);

      // A tap on a Pins entry opens the session.
      await page.locator('#switcher-toggle').click();
      await page.locator('#switcher-views [data-switcher-pick="pins"]').click();
      const quince = viewRow(page, 'pins').locator(`a[data-switcher-entry][href="${quinceHref}"]`);
      await expect(quince).toBeVisible();
      await Promise.all([page.waitForURL((u) => u.pathname + u.search === quinceHref), touch ? quince.tap() : quince.click()]);
      expect(new URL(page.url()).searchParams.get('session')).toBe('pinq');

      // UNPIN from the same menu, on each page.
      await expect(pinToggle(page)).toHaveAttribute('data-pinned', 'true');
      await pinHere(page, false, touch);
      for (const h of [sableHref, agentPath]) {
        await open(page, h);
        await pinHere(page, false, touch);
      }
      await open(page, unpinnedHref);
      await expect.poll(() => rowHrefs(page, 'all'), { message: 'after unpinning all three' }).toEqual(before);
      await expect(viewRow(page, 'pins')).toHaveText('Nothing pinned');
    });

    // A pinned AGENT keeps its lifecycle controls: its page's ⋯ menu starts and
    // stops it, the status strip follows, and it stays pinned throughout.
    test("a pinned agent: its page's ⋯ menu starts and stops it, and it stays pinned", async ({ page }) => {
      const name = 'e2e-pinag-' + Date.now().toString(36);
      mkchat(name, [{ role: 'user', content: 'pinned agent' }]);
      let id = '';
      try {
        await open(page, '/agents/' + name);
        const r = await fetch(origin + '/api/agents', { headers: { Authorization: `Bearer ${hookToken}` } });
        id = String(((await r.json()) as { id: number; name: string }[]).find((x) => x.name === name)!.id);
        await pinHere(page, true, false);
        const more = page.locator('#agent-actions-open');
        const menu = page.locator('#agent-actions');
        const status = page.locator('#agent-status');
        expect((await more.boundingBox())!.width, 'the ⋯ is a 44px target').toBeGreaterThanOrEqual(44);
        // Started from the menu (the noop provisioner then reports it live).
        await more.click();
        await menu.locator('[data-agent-action="Start"]').click();
        await expect(status, 'the strip follows the start').toHaveAttribute('data-agent-status', /provisioning|running/, { timeout: 20_000 });
        await expect
          .poll(async () => { await page.reload(); return status.getAttribute('data-agent-status'); }, { message: 'the pinned agent runs', timeout: 30_000 })
          .toBe('running');
        await more.click();
        await expect(menu.locator('[data-agent-action="Start"]'), 'Start is not offered while it runs').toBeHidden();
        await menu.locator('[data-agent-action="Stop"]').click();
        await expect(status, 'stopped from its menu, the strip follows').toHaveAttribute('data-agent-status', 'stopped', { timeout: 20_000 });
        await more.click();
        await expect(menu.locator('[data-agent-action="Start"]'), 'the menu offers Start again').toBeVisible();
        await page.keyboard.press('Escape');
        // Still pinned: its toggle, and the Pins view.
        await page.reload();
        await expect(pinToggle(page), 'still pinned').toHaveAttribute('data-pinned', 'true');
        await expect(viewRow(page, 'pins').locator('[data-switcher-current]')).toHaveCount(1);
      } finally {
        if (id) await pinVia(page, false, { kind: 'agent', agent_id: id });
        mkchat(name, null);
      }
    });

    // 🔴 A REFRESH MUST NOT LEAVE A NEWLY PINNED ENTRY IN THE IDLE GROUP.
    // Another tab pins a window this tab's sidebar draws in its collapsed Idle
    // group; the sidebar's next refresh brings it out of the group, visible.
    test('another tab pins a window drawn in Idle: it comes out of the sidebar\'s Idle group, visible', async ({ page }) => {
      await login(page);
      await rig.waitListed(page, 'pinold');
      await page.goto(origin + '/account');
      if (vp.use.viewport.width < 1024) await page.locator('#sidebar-open').click();
      const idle = page.locator(`#sidebar-idle a[data-nav-agent][href="${oldHref}"]`);
      const out = page.locator(`#sidebar-agents-list > ul a[data-nav-agent][href="${oldHref}"]`);
      await expect(idle, `drawn in the Idle group (log:\n${rig.log})`).toHaveCount(1, { timeout: 20_000 });
      await expect(idle, 'the Idle group is collapsed').toBeHidden();
      try {
        await pinVia(page, true, winForm('pinold', 4));
        await page.evaluate(() => (window as any).htmx.trigger(document.body, 'muster:resync'));
        await expect(out, 'the pinned window is out of the group').toBeVisible({ timeout: 10_000 });
        await expect(idle).toHaveCount(0);
      } finally {
        await pinVia(page, false, winForm('pinold', 4));
      }
    });

    // 🔴 A SHELL WINDOW CAN STILL BE PINNED. Its card's toggle pinned it while
    // the Agents list lived; with the list retired the one way to pin is the
    // bar's ⋯ menu on the session's own page ("open it, then pin"). A plain
    // shell window's only page is its terminal, which draws the session
    // switcher at its foot as every Terminal view does — so its pin is in the
    // bar's ⋯ menu, as on a Rendered view (the control), and the header has
    // none. It pins
    // and unpins, and the Pins view follows: on this page at once (the
    // answer's out-of-band rows), and on another session's page.
    test("a shell window's terminal page carries the pin in its bar's ⋯ menu, as a Rendered view does", async ({ page }) => {
      await open(page, sableHref);
      await expect(page.locator('#session-switcher #switcher-more #switcher-pin'), "control: the Rendered view's ⋯ menu has the pin").toBeAttached();
      await page.goto(origin + shellHref);
      await expect(page.locator('#term-host-note, [aria-label="Terminal input"]').first(), 'the terminal page is drawn').toBeAttached();
      const pin = page.locator('#session-switcher #switcher-more #switcher-pin');
      await expect(moreBtn(page), "the terminal bar's ⋯").toBeVisible();
      await expect(page.locator('header #switcher-pin'), 'the header has no pin of its own').toHaveCount(0);
      await expect(page.locator('#switcher-pin'), 'one pin on the page').toHaveCount(1);
      await expect(page.locator('#view-rendered, #view-terminal'), 'a terminal-only window has no view switch').toHaveCount(0);
      await expect(pin).toHaveAttribute('data-pinned', 'false');
      // The bar spans the screen here (pinned at the column's foot): its
      // shown row's width, all of what the compact bar gives the pills.
      const rowW = await page.locator('#session-switcher [data-switcher-list]:not([hidden])').evaluate((el) => el.clientWidth);
      if (vp.use.viewport.width === 390) expect(rowW, 'the pills\' row on a 390px Terminal view (182px on the base, 258px here)').toBeGreaterThanOrEqual(240);
      await moreBtn(page).click();
      await pin.click();
      await expect(page.locator('#session-switcher #switcher-pin')).toHaveAttribute('data-pinned', 'true');
      // The answer's rows, out of band: this page's own Pins row holds it now.
      await expect(page.locator('#session-switcher [data-switcher-list="pins"] [data-switcher-current]'), "this page's Pins row follows").toHaveCount(1);
      try {
        await open(page, sableHref);
        await expect(viewRow(page, 'pins').locator(`a[href="${shellHref.replace(/&/g, '&amp;')}"], a[href="${shellHref}"]`).first(), 'the Pins view holds the shell window').toBeAttached();
      } finally {
        await page.goto(origin + shellHref);
        await moreBtn(page).click();
        await page.locator('#session-switcher #switcher-pin').click();
        await expect(page.locator('#session-switcher #switcher-pin')).toHaveAttribute('data-pinned', 'false');
      }
    });

    // A session page with pinned entries in its bar loads with no layout shift
    // (their place is the server's, in the first paint).
    test('a session page with pinned entries in its bar loads with CLS < 0.01', async ({ page }) => {
      await login(page);
      await rig.waitListed(page, 'pinq');
      await pinVia(page, true, winForm('pins', 2));
      try {
        await pinVia(page, true, winForm('pinq', 3));
        await page.addInitScript(() => { try { localStorage.setItem('muster.switcher.view', 'pins'); } catch (e) {} });
        await page.addInitScript(clsInit);
        await page.goto(origin + unpinnedHref);
        for (const h of [sableHref, quinceHref]) await expect(viewRow(page, 'pins').locator(`a[href="${h}"]`)).toBeVisible({ timeout: 20_000 });
        await settle(page);
        await noOverflow(page, 'pinned entries');
        const r = await readCls(page);
        expect(r.unsupported, 'layout-shift is observable here').toBeUndefined();
        expect(r.total, `shifts: ${JSON.stringify(r.shifts)}`).toBeLessThan(0.01);
        // The positive control: a shift the page did not ask for IS counted.
        await page.waitForTimeout(600);
        await page.evaluate(() => {
          const d = document.createElement('div');
          d.style.cssText = 'height:150px;flex:none';
          document.body.prepend(d);
        });
        await page.waitForTimeout(300);
        expect((await readCls(page)).total, 'the instrument sees a shift').toBeGreaterThan(0.01);
      } finally {
        await pinVia(page, false, winForm('pins', 2));
        await pinVia(page, false, winForm('pinq', 3));
      }
    });
  });
}
