import fs from 'node:fs';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { password, pinsPort } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';

// PINNED ENTRIES COME FIRST — AS LABEL-ONLY CHIPS — in a real browser, against
// a real muster-server (MUSTER_MIRROR=1, its own port) and a real muster-mirror
// over the tunnel (the `-tags e2emirror` per-window fake, as
// mirror-windows.spec.ts), at a phone's size and a desktop's.
//
// The operator asks: "Add 'pinned' agents so they appear first in list", then
// "Pinned agents should show only the 'pretty Tmux' label (for example mango,
// vapor, wheat) not the full agent card" (internal/ui/pinned_chips.go). The
// host gives two sessions SYNTHETIC codenames (quince, sable) through its
// labels file, so their windows are "quince:1" and "sable:1" and their chips
// read "quince" and "sable". The entries pinned are the LEAST recently active
// (the two older windows, the last seeded agent), so "it moved to the top"
// cannot be the activity order doing it. Pins are server-stored, so they must
// survive a list refresh AND a page reload.
//
// THE CHOICE THIS PINS: a chip is filtered exactly as its card would be — the
// search box and the host tabs hide it — and is unpinned from the pinned row's
// "Edit pins" toggle, which reveals a × on each chip.
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
const windows = [`pinx|@1|1|1|zsh|${now - 60}|`, `pins|@2|1|1|zsh|${now - 3600}|`, `pinq|@3|1|1|zsh|${now - 7200}|`];
const href = (session: string, id: number) => `/mirror/terminal?host=${host}&session=${session}&window=%40${id}`;
const unpinnedHref = href('pinx', 1);
const sableHref = href('pins', 2);
const quinceHref = href('pinq', 3);

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

// order is every entry of the list, top to bottom as DRAWN (chips and cards):
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

const chip = (page: Page, h: string) => page.locator(`#agents-list > article[data-pinned-chip]:has(a[href="${h}"])`);
const card = (page: Page, h: string) => page.locator(`#agents-list > article[data-mirror-card]:has(a[href="${h}"])`);

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

    test('pinned entries become label-only chips at the top, filter like cards, open their session, and unpin', async ({ page }) => {
      await openList(page);
      const before = await order(page);
      const agentsBefore = before.filter((x) => x.startsWith('agent-'));
      expect(agentsBefore.length, 'the seeded agents are listed').toBeGreaterThanOrEqual(2);
      expect(before.filter((x) => x.startsWith('win@')), 'the windows by activity, unpinned').toEqual(['win@1', 'win@2', 'win@3']);
      expect(await page.locator('#agents-list [data-pinned]').count(), 'nothing pinned at the start').toBe(0);
      await expect(page.locator('#agents-pins-edit'), 'no pinned row with nothing pinned').toHaveCount(0);
      await noOverflow(page, 'unpinned');

      // Pin through the real controls: the LEAST recently active agent's ⋯ menu…
      const last = agentsBefore[agentsBefore.length - 1];
      pinnedAgentID = last.replace('agent-', '');
      // The name its card shows: the first of its titles (display name, name).
      const agentName = ((await page.locator(`#${last}`).getAttribute('data-rank-title')) ?? '').split('\n').find((t) => t)!;
      await page.locator(`#${last} [aria-label="Agent actions"]`).click();
      await page.locator(`#agent-menu-${pinnedAgentID}`).getByRole('button', { name: 'Pin to top' }).click();
      await expect(page.locator(`#${last}[data-pinned-chip="agent"]`), 'the pinned agent is a chip').toHaveCount(1);
      // …and the two older windows' pin toggles (44px targets).
      for (const h of [quinceHref, sableHref]) {
        const toggle = card(page, h).locator('[data-pin-toggle]');
        await expect(toggle).toHaveAttribute('aria-pressed', 'false');
        expect((await toggle.boundingBox())!.width, 'the toggle is a 44px target').toBeGreaterThanOrEqual(44);
        await toggle.click();
        await expect(chip(page, h)).toHaveCount(1);
      }

      // THE CHIPS: label only — the codename without its ":1", the agent's name
      // — and no card for a pinned entry.
      await expect(chip(page, quinceHref)).toHaveText('quince');
      await expect(chip(page, sableHref)).toHaveText('sable');
      await expect(page.locator(`#${last}`)).toHaveText(agentName);
      for (const h of [quinceHref, sableHref]) await expect(card(page, h), 'no card beside the chip').toHaveCount(0);
      await expect(page.locator('#agents-list > article[data-pinned-chip] :is([data-card-slot], [data-card-host], [data-card-focus], [data-host-icon])')).toHaveCount(0);

      // First, by activity among themselves (the agent, seeded this run, is
      // newer than both windows), then the rest in the ONE flat order they had.
      const want = [last, 'win@2', 'win@3', ...before.filter((x) => x !== last && x !== 'win@2' && x !== 'win@3')];
      await expect.poll(() => order(page), { message: 'the chips, then the rest in their order' }).toEqual(want);

      // Geometry: 44px tall, side by side in rows ABOVE every card, the page
      // never scrolling sideways.
      const chips = page.locator('#agents-list > article[data-pinned-chip]');
      const boxes = await chips.evaluateAll((els) => els.map((e) => e.getBoundingClientRect().toJSON()));
      for (const b of boxes) {
        expect(b.height, 'a chip is a 44px target').toBeGreaterThanOrEqual(44);
        expect(b.right, 'a chip stays inside the viewport').toBeLessThanOrEqual(vp.use.viewport.width);
      }
      // Side by side: the "Edit pins" toggle and at least the first chip share
      // a row (the chips wrap onto more rows only as they need to).
      const head = (await page.locator('#agents-pins-head').boundingBox())!;
      expect(Math.round(boxes[0].top), 'the chips sit beside the Edit toggle, not under it').toBe(Math.round(head.y));
      expect(Math.max(...boxes.map((b) => b.height)), 'a chip is not stretched to a card\'s height').toBeLessThan(60);
      const firstCard = await page.locator('#agents-list > article:not([data-pinned-chip])').first().boundingBox();
      expect(firstCard!.y, 'the cards start below the chips').toBeGreaterThanOrEqual(Math.max(...boxes.map((b) => b.bottom)));
      await noOverflow(page, 'pinned');

      // A list refresh (what an SSE event or a lifecycle action does) keeps it.
      await page.evaluate(() => (window as any).htmx.trigger(document.body, 'agents:changed'));
      await page.waitForTimeout(500);
      expect(await order(page), 'after a list refresh').toEqual(want);
      // And a reload: the pins are the server's, not the page's.
      await page.reload();
      await expect.poll(() => order(page), { message: 'after a reload', timeout: 20_000 }).toEqual(want);

      // The host's tab still leads with its icon, though the host's first drawn
      // entry is now a chip, which draws none (syncHosts takes the icon from
      // the first entry that has one).
      await expect(page.locator(`#agents-hosts [role="tab"][data-host="${host}"] [data-host-icon]`)).toHaveCount(1);

      // FILTERS: a chip is filtered as its card is. This host's name is on the
      // windows and not on the agent…
      await page.fill('#agents-search', host);
      await expect(page.locator(`#${last}`)).toBeHidden();
      await expect(chip(page, quinceHref)).toBeVisible();
      // …a word no entry carries hides every chip, and the Edit toggle with them…
      await page.fill('#agents-search', 'zzz-no-such-entry');
      await expect(chips.first()).toBeHidden();
      await expect(page.locator('#agents-pins-edit')).toBeHidden();
      await page.fill('#agents-search', '');
      await expect(page.locator('#agents-pins-edit')).toBeVisible();
      // …and the host tabs: this host's tab hides the agent's chip, the
      // cluster's hides the windows'.
      await page.locator(`#agents-hosts [role="tab"][data-host="${host}"]`).click();
      await expect(page.locator(`#${last}`)).toBeHidden();
      await expect(chip(page, sableHref)).toBeVisible();
      await page.locator('#agents-hosts [role="tab"][data-host="cluster"]').click();
      await expect(chip(page, sableHref)).toBeHidden();
      await expect(page.locator(`#${last}`)).toBeVisible();
      await page.locator('#agents-hosts [role="tab"][data-host=""]').click();
      await expect(chips).toHaveCount(3);
      for (const c of await chips.all()) await expect(c).toBeVisible();

      // A tap opens the session — the card's own href.
      await chip(page, quinceHref).locator('a').click();
      await page.waitForURL((u) => u.pathname + u.search === quinceHref);
      expect(new URL(page.url()).searchParams.get('session')).toBe('pinq');
      await page.goto(origin + '/agents');
      await expect(chip(page, quinceHref)).toHaveCount(1, { timeout: 20_000 });

      // UNPIN: the × is hidden until "Edit pins" is pressed.
      const x = (label: string) => page.getByRole('button', { name: `Unpin ${label}`, exact: true });
      await expect(x('quince')).toBeHidden();
      const edit = page.locator('#agents-pins-edit');
      expect((await edit.boundingBox())!.width, 'Edit pins is a 44px target').toBeGreaterThanOrEqual(44);
      await edit.click();
      await expect(edit).toHaveAttribute('aria-pressed', 'true');
      await expect(x('quince')).toBeVisible();
      await noOverflow(page, 'editing pins');
      await x('quince').click();
      await expect(card(page, quinceHref), 'unpinned: a card again').toHaveCount(1);
      await expect(chip(page, quinceHref)).toHaveCount(0);
      // The edit state survives the list's re-render.
      await expect(edit).toHaveAttribute('aria-pressed', 'true');
      await x('sable').click();
      await expect(card(page, sableHref)).toHaveCount(1);
      await x(agentName).click();
      await expect(page.locator(`#${last}[data-pinned-chip]`)).toHaveCount(0);
      await expect.poll(() => order(page), { message: 'after unpinning all three' }).toEqual(before);
      expect(await page.locator('#agents-list [data-pinned]').count()).toBe(0);
      await expect(page.locator('#agents-pins-edit')).toHaveCount(0);
    });
  });
}
