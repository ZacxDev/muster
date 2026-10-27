import fs from 'node:fs';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { password, pinsPort } from '../lib/env';
import { MirrorRig, mkchat } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';
import { clsInit, readCls, settle } from '../lib/cls';
import { agentLink } from '../lib/agent-link';

// PINNED ENTRIES COME FIRST, AS THEIR CARDS, in a real browser, against a real
// muster-server (MUSTER_MIRROR=1, its own port) and a real muster-mirror over
// the tunnel (the `-tags e2emirror` per-window fake, as mirror-windows.spec.ts),
// at a phone's size and a desktop's.
//
// The operator asked "Add 'pinned' agents so they appear first in list"; a
// pinned entry was then drawn as a label-only chip in a row above the cards.
// That row is gone ("hide the pinned … on the main agent list page because the
// bottom bar flow supersedes it"): the session switcher's Pins view is where a
// pin is a one-tap label now (agents-switcher.spec.ts). A pinned entry is its
// CARD again, still first, and the card's own toggle unpins it. The host gives
// two sessions SYNTHETIC codenames (quince, sable) through its labels file. The
// entries pinned are the LEAST recently active (the two older windows, the last
// seeded agent), so "it moved to the top" cannot be the activity order doing
// it. Pins are server-stored, so they must survive a list refresh AND a page
// reload.
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
let pinnedAgentID = '';

const now = Math.floor(Date.now() / 1000);
// session|id|index|active|command|activity|claude-transcript
// pinold has had no activity for 30 days: it is drawn in the collapsed Idle
// section (MUSTER_AGENTS_IDLE_AFTER defaults to 14 days).
const windows = [
  `pinx|@1|1|1|zsh|${now - 60}|`,
  `pins|@2|1|1|zsh|${now - 3600}|`,
  `pinq|@3|1|1|zsh|${now - 7200}|`,
  `pinold|@4|1|1|zsh|${now - 30 * 86400}|`,
];
const href = (session: string, id: number) => `/mirror/terminal?host=${host}&session=${session}&window=%40${id}`;
const unpinnedHref = href('pinx', 1);
const sableHref = href('pins', 2);
const quinceHref = href('pinq', 3);
const oldHref = href('pinold', 4);

test.beforeAll(async () => {
  await rig.start();
  writeFileAtomic(windowsFile, windows.join('\n') + '\n');
  fs.writeFileSync(labelsFile, JSON.stringify({ pinq: 'quince', pins: 'sable' }));
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile, MUSTER_MIRROR_LABELS_FILE: labelsFile });
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
  if (signedIn) {
    await page.context().addCookies(signedIn);
    return;
  }
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  signedIn = await page.context().cookies(origin);
}

// order is every entry of the list, top to bottom as DRAWN:
// "agent-<id>" for an agent, "win@<n>" for one of THIS host's windows; other
// hosts' entries are skipped. Read by position, so it is what the eye sees
// whatever the DOM order.
async function order(page: Page): Promise<string[]> {
  return page.locator('#agents-list > article').evaluateAll((els, host) => {
    const out: { k: string; top: number; left: number }[] = [];
    for (const e of els) {
      const r = e.getBoundingClientRect();
      if (!r.width && !r.height) continue;
      if (e.id.startsWith('agent-')) {
        out.push({ k: e.id, top: Math.round(r.top), left: r.left });
        continue;
      }
      const a = e.querySelector('a[href*="window="]') as HTMLAnchorElement | null;
      if (!a || !a.href.includes('host=' + host)) continue;
      out.push({ k: 'win@' + new URL(a.href).searchParams.get('window')!.slice(1), top: Math.round(r.top), left: r.left });
    }
    out.sort((a, b) => a.top - b.top || a.left - b.left);
    return out.map((x) => x.k);
  }, host);
}

const pinnedCard = (page: Page, h: string) => page.locator(`#agents-list > article[data-pinned][data-mirror-card]:has(a[href="${h}"])`);
const card = (page: Page, h: string) => page.locator(`#agents-list > article[data-mirror-card]:has(a[href="${h}"])`);

// pinVia pins (or unpins) through the route, as ANOTHER tab would: this page
// hears nothing of it until its list refreshes.
async function pinVia(page: Page, on: boolean, form: Record<string, string>) {
  const r = await page.request.post(origin + (on ? '/agents/pins' : '/agents/pins/delete'), { form, headers: { Origin: origin } });
  expect(r.status(), `${on ? 'pin' : 'unpin'} ${JSON.stringify(form)}`).toBe(200);
}
const winForm = (session: string, id: number, h = host) => ({ kind: 'mirror', host: h, session, window: '@' + id });

async function openList(page: Page) {
  await login(page);
  await page.goto(origin + '/agents');
  await expect(card(page, quinceHref), `the host's windows (log:\n${rig.log})`).toHaveCount(1, { timeout: 20_000 });
  await expect(card(page, sableHref)).toHaveCount(1);
  await expect(card(page, unpinnedHref)).toHaveCount(1);
}

// noOverflow: the page never scrolls sideways.
async function noOverflow(page: Page, what: string) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, `${what}: document scrollWidth`).toBeLessThanOrEqual(cw);
}

for (const vp of [
  { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('pinned entries come first as their cards, survive a refresh and a reload, filter by host tab, open their session, and unpin from their own toggle', async ({ page }) => {
      await openList(page);
      const before = await order(page);
      const agentsBefore = before.filter((x) => x.startsWith('agent-'));
      expect(agentsBefore.length, 'the seeded agents are listed').toBeGreaterThanOrEqual(2);
      expect(before.filter((x) => x.startsWith('win@')), 'the windows by activity, unpinned').toEqual(['win@1', 'win@2', 'win@3']);
      expect(await page.locator('#agents-list [data-pinned]').count(), 'nothing pinned at the start').toBe(0);
      await expect(page.locator('[data-pinned-chip], #agents-pins-edit'), 'no pinned row').toHaveCount(0);
      await noOverflow(page, 'unpinned');

      // Pin through the real controls: the LEAST recently active agent's ⋯ menu…
      const last = agentsBefore[agentsBefore.length - 1];
      pinnedAgentID = last.replace('agent-', '');
      await page.locator(`#${last} [aria-label="Agent actions"]`).click();
      await page.locator(`#agent-menu-${pinnedAgentID}`).getByRole('button', { name: 'Pin to top' }).click();
      await expect(page.locator(`#${last}[data-pinned]`), 'the pinned agent is marked').toHaveCount(1);
      // …and the two older windows' pin toggles (44px targets).
      for (const h of [quinceHref, sableHref]) {
        const toggle = card(page, h).locator('[data-pin-toggle]');
        expect((await toggle.boundingBox())!.width, 'the toggle is a 44px target').toBeGreaterThanOrEqual(44);
        await expect(toggle).toHaveAttribute('aria-pressed', 'false');
        await toggle.click();
        await expect(pinnedCard(page, h)).toHaveCount(1);
        await expect(card(page, h).locator('[data-pin-toggle]'), 'pressed once pinned').toHaveAttribute('aria-pressed', 'true');
      }
      // Still CARDS — their body, their host chip, their footer — never chips.
      for (const h of [quinceHref, sableHref]) {
        await expect(pinnedCard(page, h).locator('[data-card-slot="body"]')).toHaveCount(1);
      }
      await expect(page.locator(`#${last} [data-card-slot="body"]`)).toHaveCount(1);
      await expect(page.locator('[data-pinned-chip], #agents-pins-edit')).toHaveCount(0);

      // First, by activity among themselves (the agent, seeded this run, is
      // newer than both windows), then the rest in the ONE flat order they had.
      const want = [last, 'win@2', 'win@3', ...before.filter((x) => x !== last && x !== 'win@2' && x !== 'win@3')];
      await expect.poll(() => order(page), { message: 'the pinned cards, then the rest in their order' }).toEqual(want);
      await noOverflow(page, 'pinned');

      // A list refresh (what an SSE event or a lifecycle action does) keeps it.
      await page.evaluate(() => (window as any).htmx.trigger(document.body, 'agents:changed'));
      await page.waitForTimeout(500);
      expect(await order(page), 'after a list refresh').toEqual(want);
      // And a reload: the pins are the server's, not the page's.
      await page.reload();
      await expect.poll(() => order(page), { message: 'after a reload', timeout: 20_000 }).toEqual(want);

      // The host tabs filter a pinned card as any card: this host's tab hides
      // the agent, the cluster's hides the windows.
      await page.locator(`#agents-hosts [role="tab"][data-host="${host}"]`).click();
      await expect(page.locator(`#${last}`)).toBeHidden();
      await expect(pinnedCard(page, sableHref)).toBeVisible();
      await page.locator('#agents-hosts [role="tab"][data-host="cluster"]').click();
      await expect(pinnedCard(page, sableHref)).toBeHidden();
      await expect(page.locator(`#${last}`)).toBeVisible();
      await page.locator('#agents-hosts [role="tab"][data-host=""]').click();
      for (const c of await page.locator('#agents-list > article[data-pinned]').all()) await expect(c).toBeVisible();

      // A tap opens the session — the card's own href.
      await pinnedCard(page, quinceHref).locator('[data-card-slot="body"]').click();
      await page.waitForURL((u) => u.pathname + u.search === quinceHref);
      expect(new URL(page.url()).searchParams.get('session')).toBe('pinq');
      await page.goto(origin + '/agents');
      await expect(pinnedCard(page, quinceHref)).toHaveCount(1, { timeout: 20_000 });

      // UNPIN from the same toggle (and the agent's ⋯ menu's "Unpin").
      await card(page, quinceHref).locator('[data-pin-toggle]').click();
      await expect(pinnedCard(page, quinceHref)).toHaveCount(0);
      await expect(card(page, quinceHref).locator('[data-pin-toggle]')).toHaveAttribute('aria-pressed', 'false');
      await card(page, sableHref).locator('[data-pin-toggle]').click();
      await expect(pinnedCard(page, sableHref)).toHaveCount(0);
      await page.locator(`#${last} [aria-label="Agent actions"]`).click();
      await page.locator(`#agent-menu-${pinnedAgentID}`).getByRole('button', { name: 'Unpin' }).click();
      await expect(page.locator(`#${last}[data-pinned]`)).toHaveCount(0);
      await expect.poll(() => order(page), { message: 'after unpinning all three' }).toEqual(before);
      expect(await page.locator('#agents-list [data-pinned]').count()).toBe(0);
    });

    // A pinned AGENT keeps its lifecycle controls: its card's ⋯ menu stops it,
    // and it stays pinned (first, marked) — a card throughout.
    test('a pinned agent card: its ⋯ menu stops it and it stays pinned', async ({ page }) => {
      const name = 'e2e-pinag-' + Date.now().toString(36);
      mkchat(name, [{ role: 'user', content: 'pinned agent' }]);
      let id = '';
      try {
        await login(page);
        await page.goto(origin + '/agents');
        const entry = page.locator(`#agents-list > article:has(${agentLink(name)})`);
        await expect(entry).toHaveCount(1, { timeout: 20_000 });
        id = (await entry.getAttribute('id'))!.replace('agent-', '');
        await pinVia(page, true, { kind: 'agent', agent_id: id });
        // Started (the noop provisioner then reports it live, as
        // agent-cards.spec.ts relies on), so there is something to Stop.
        expect((await page.request.post(`${origin}/agents/${id}/start`, { headers: { Origin: origin } })).status()).toBe(200);
        const cardEl = page.locator(`#agent-${id}[data-pinned]`);
        await expect
          .poll(async () => { await page.reload(); return cardEl.getAttribute('data-f-status'); }, { message: 'the pinned agent runs', timeout: 20_000 })
          .toBe('running');
        const more = cardEl.getByRole('button', { name: 'Agent actions' });
        await expect(more).toBeVisible();
        expect((await more.boundingBox())!.width, 'the ⋯ is a 44px target').toBeGreaterThanOrEqual(44);
        const menu = page.locator(`#agent-menu-${id}`);
        await more.click();
        await expect(menu).toBeVisible();
        await expect(menu.getByRole('button', { name: 'Unpin' })).toBeVisible();
        await expect(menu.getByRole('button', { name: 'Pin to top' })).toHaveCount(0);
        await menu.getByRole('button', { name: 'Stop', exact: true }).click();
        await expect
          .poll(async () => { await page.reload(); return cardEl.getAttribute('data-f-status'); }, { message: 'stopped from the card', timeout: 20_000 })
          .toBe('stopped');
        await expect(cardEl, 'still pinned').toHaveCount(1);
        await expect(page.locator('#agents-list > article').first(), 'and first').toHaveAttribute('data-pinned', '');
      } finally {
        if (id) await pinVia(page, false, { kind: 'agent', agent_id: id });
        mkchat(name, null);
      }
    });

    // 🔴 A HELD REFRESH MUST NOT LEAVE A NEWLY PINNED CARD IN THE IDLE SECTION.
    // Another tab pins a window this tab draws in the collapsed Idle section;
    // the background refresh brings it out of the section, first and visible.
    // (A pinned entry is never idle: InIdleSection.)
    test('another tab pins a window drawn in Idle: it comes out of the section, first and visible', async ({ page }) => {
      await openList(page);
      const old = page.locator(`#agents-list > article:has(a[href="${oldHref}"])`);
      await expect(old).toHaveAttribute('data-idle-section', '');
      await expect(old, 'the Idle section is collapsed').toBeHidden();
      try {
        await pinVia(page, true, winForm('pinold', 4));
        await page.evaluate(() => (window as any).htmx.trigger(document.body, 'muster:resync'));
        // Moving it out of the Idle section moves cards in view, so the list
        // may hold it behind the "N new ↑" pill; the tap applies it.
        const pill = page.locator('#agents-new-pill');
        await expect.poll(async () => (await pill.isVisible()) || (await pinnedCard(page, oldHref).isVisible()), { timeout: 10_000 }).toBe(true);
        if (await pill.isVisible()) await pill.click();
        await expect(pinnedCard(page, oldHref), 'the pinned card is drawn').toBeVisible();
        await expect(pinnedCard(page, oldHref)).not.toHaveAttribute('data-idle-section', '');
        await expect.poll(() => order(page), { message: 'it is first' }).toEqual(expect.arrayContaining(['win@4']));
        expect((await order(page))[0], 'the pinned window leads the list').toBe('win@4');
      } finally {
        await pinVia(page, false, winForm('pinold', 4));
      }
    });

    // Pinned cards load with no layout shift (their place is the server's, in
    // the first paint).
    test('a list with pinned cards loads with CLS < 0.01', async ({ page }) => {
      await login(page);
      await pinVia(page, true, winForm('pins', 2));
      try {
        await pinVia(page, true, winForm('pinq', 3));
        await page.addInitScript(clsInit);
        await page.goto(origin + '/agents');
        for (const h of [sableHref, quinceHref]) await expect(pinnedCard(page, h)).toBeVisible({ timeout: 20_000 });
        await settle(page);
        await noOverflow(page, 'pinned cards');
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
