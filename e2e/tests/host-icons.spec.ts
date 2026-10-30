import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { hostIconsPort } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';

// HOST ICONS AND SCROLL POSITIONS, ON A REAL SERVER WITH TWO MIRRORED HOSTS
// (the e2eterm binary, mirror plugin on, two real muster-mirror processes;
// only tmux is faked), plus the seeded dispatched agents, whose host is
// "cluster". The Agents list page this measured is retired: its rows are the
// sidebar's, and the session switcher's bar is on session pages.
//
// 1. HOST ICONS (internal/ui/host_icons.go). Every listed entry draws its
//    host's icon beside its name — the host in its tooltip — and two hosts
//    draw two DIFFERENT icons, the cluster a third. The same host draws the
//    same icon on its sidebar rows, its session-switcher entries (the Recent
//    view) and its window's own page.
//
// 2. SCROLL POSITIONS SURVIVE A REFRESH. The sidebar's list (which scrolls
//    inside itself) keeps its offset across its background refreshes, which
//    swap its rows whole; and the switcher's Recent row keeps its sideways
//    offset across the swap that carries its entries out of band (the pin
//    toggle's answer, switcher.go switcherRefresh). (The list page's offset
//    across a reload and a Back, through a held load, has no successor: the
//    sidebar's list is a lazy partial, drawn shut on a phone.)
//
// 🔴 REGRESSION TESTS. On the commit before the icons: no entry carries
// [data-host-icon].

const rig = new MirrorRig(hostIconsPort, 'e2e-icons-a');
const hostA = rig.host;
const hostB = rig.host.replace('e2e-icons-a-', 'e2e-icons-b-');
const t0 = Math.floor(Date.now() / 1000);
// session|id|index|active|command|activity|claude-transcript — 14 windows a
// host, so the list runs several screens on a phone.
const line = (session: string, id: string, ago: number) => `${session}|${id}|1|1|zsh|${t0 - ago}|`;
const windowsA = Array.from({ length: 14 }, (_, i) => line(`alpha${i}`, `@${i + 1}`, 100 + i));
const windowsB = Array.from({ length: 14 }, (_, i) => line(`beta${i}`, `@${i + 1}`, 150 + i));

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
  const fa = rig.transcript('', 'windows-a');
  const fb = rig.transcript('', 'windows-b');
  writeFileAtomic(fa, windowsA.join('\n') + '\n');
  writeFileAtomic(fb, windowsB.join('\n') + '\n');
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: fa });
  rig.extraHost(hostB, { MUSTER_MIRROR_E2E_WINDOWS_FILE: fb });
});

test.afterAll(async () => {
  await rig.stop();
});

test.use({ storageState: { cookies: [], origins: [] }, viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

const rowsOn = (page: Page, host: string) =>
  host === 'cluster' ? page.locator('#sidebar-agents-list a[data-nav-agent="agent"]') : page.locator(`#sidebar-agents-list a[data-nav-agent="mirror"][href*="host=${host}&"]`);
const iconKeys = (page: Page, host: string) => rowsOn(page, host).locator('[data-host-icon]').evaluateAll((els) => els.map((e) => e.getAttribute('data-host-icon')));
const strip = (page: Page) => page.locator('#session-switcher [data-switcher-list="recent"]');
// The bar is read on a session page: a seeded agent's chat (the shared
// database's agents are this server's too).
const BAR_PAGE = '/agents/notes-tidy';

// open signs in, visits six of host A's windows (so the switcher's Recent row
// has six entries, wider than a phone) and opens a session page with the
// Recent view shown and the sidebar's list open, every row drawn.
async function open(page: Page) {
  await rig.login(page);
  await page.addInitScript(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
  await rig.waitListed(page, 'alpha13');
  await rig.waitListed(page, 'beta13');
  for (let i = 0; i < 6; i++) await page.goto(rig.origin + rig.terminalPath(`alpha${i}`));
  await page.goto(rig.origin + BAR_PAGE);
  // The six visits lead the Recent row (after the page's own, which is not a
  // link), newest first. (It holds up to eight, and the shared database's
  // other visits may follow them.)
  await expect
    .poll(() => strip(page).locator('a[data-switcher-entry]').evaluateAll((els) => els.slice(0, 6).map((e) => new URL((e as HTMLAnchorElement).href).searchParams.get('session'))))
    .toEqual(['alpha5', 'alpha4', 'alpha3', 'alpha2', 'alpha1', 'alpha0']);
  await page.locator('#sidebar-open').click();
  await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });
  await drawn(page);
}

// drawn waits until both hosts' 14 windows and the seeded agents are rows.
async function drawn(page: Page) {
  await expect
    .poll(async () => [await rowsOn(page, hostA).count(), await rowsOn(page, hostB).count(), (await rowsOn(page, 'cluster').count()) > 0], {
      timeout: 30_000,
      message: `both hosts' windows and the cluster's agents are listed (log:\n${rig.log})`,
    })
    .toEqual([14, 14, true]);
}

test.describe('host icons', () => {
  test('two hosts draw two different icons, the cluster a third; each host the same one on every row, entry and page', async ({ page }) => {
    await open(page);
    const a = await iconKeys(page, hostA);
    const b = await iconKeys(page, hostB);
    const c = await iconKeys(page, 'cluster');
    expect(a, 'every row of host A draws an icon').toHaveLength(14);
    expect(b, 'every row of host B draws an icon').toHaveLength(14);
    expect(new Set(a).size, "one icon for all of host A's rows").toBe(1);
    expect(new Set(b).size, "one icon for all of host B's rows").toBe(1);
    expect(new Set(c), "the dispatched agents draw the cluster's icon").toEqual(new Set(['cluster']));
    expect(a[0], 'two hosts, two icons').not.toBe(b[0]);
    expect([a[0], b[0]], "no host draws the cluster's icon").not.toContain('cluster');

    // The host is the tooltip, and the box is the reserved 16px square beside
    // the name (decorative on a row: the row's own name is its label).
    const mark = rowsOn(page, hostA).first().locator('[data-host-icon]');
    await expect(mark).toHaveAttribute('title', `Host: ${hostA}`);
    const box = (await mark.boundingBox())!;
    expect([Math.round(box.width), Math.round(box.height)], 'a 16px box').toEqual([16, 16]);
    await expect(rowsOn(page, 'cluster').locator('[data-host-icon="cluster"]').first()).toHaveAttribute('title', 'Host: cluster');

    // The same icon everywhere host A appears: its switcher entries and its
    // window's own page.
    const entries = strip(page).locator(`a[data-switcher-entry="mirror"][href*="host=${encodeURIComponent(hostA)}"] [data-host-icon]`);
    expect(await entries.count(), "control: host A's visited windows are in the row").toBeGreaterThanOrEqual(6);
    expect(new Set(await entries.evaluateAll((els) => els.map((e) => e.getAttribute('data-host-icon'))))).toEqual(new Set([a[0]]));
    const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(sw, 'no sideways page scroll at 390px').toBeLessThanOrEqual(cw);

    // (rig.terminalPath names host A; this is host B's window.)
    await page.goto(`${rig.origin}/mirror/terminal?host=${encodeURIComponent(hostB)}&session=beta2`);
    await expect(page.locator('[data-term-badge] [data-host-icon]')).toHaveAttribute('data-host-icon', b[0]!);
  });
});

// scroll is the sidebar list's offset (#sidebar-agents scrolls inside itself)
// and the Recent row's sideways offset.
const pos = (page: Page) =>
  page.evaluate(() => ({
    y: Math.round((document.getElementById('sidebar-agents') as HTMLElement).scrollTop),
    x: Math.round((document.querySelector('#session-switcher [data-switcher-list="recent"]') as HTMLElement | null)?.scrollLeft ?? -1),
  }));

// scrollBoth puts the sidebar's list at 300 and the Recent row at 200 — and
// proves both CAN be (a list or a row too short to scroll would read 0 for any
// code).
async function scrollBoth(page: Page) {
  await strip(page).evaluate((e) => (e.scrollLeft = 200));
  await page.locator('#sidebar-agents').evaluate((e) => (e.scrollTop = 300));
  await expect.poll(() => pos(page), 'control: the list and the row both scroll that far').toEqual({ y: 300, x: 200 });
}

test.describe('scroll positions', () => {
  // The Agents list page kept its page offset through its refreshes; its rows
  // are the sidebar's now, a list that scrolls inside itself and is swapped
  // whole on every refresh — which must leave the reader where they were.
  test("a background refresh of the sidebar's list keeps its offset, and the Recent row's", async ({ page }) => {
    await open(page);
    for (const ev of ['muster:resync', 'agents:changed']) {
      await scrollBoth(page);
      const got = page.waitForResponse((r) => new URL(r.url()).pathname === '/ui/nav/agents');
      await page.evaluate(() => ((document.querySelector('#sidebar-agents-list a[data-nav-agent]') as unknown as { __old: boolean }).__old = true));
      await page.evaluate((name) => document.body.dispatchEvent(new CustomEvent(name)), ev);
      await got;
      // Control: the refresh really swapped the rows in.
      await expect
        .poll(() => page.evaluate(() => !(document.querySelector('#sidebar-agents-list a[data-nav-agent]') as unknown as { __old?: boolean }).__old), `control: ${ev} re-rendered the list`)
        .toBe(true);
      await page.waitForTimeout(300);
      expect(await pos(page), `after ${ev}`).toEqual({ y: 300, x: 200 });
    }
  });

  // 🔴 THE OUT-OF-BAND COPY MUST LAND (the strip's once did not: sent as
  // hx-swap-oob="morph:outerHTML", which htmx 2 reads as swap style "morph"
  // into the element matching the SELECTOR "outerHTML" — nothing). The bar's
  // rows arrive out of band with the pin toggle's answer (switcher_pin.go). Here
  // a window is opened WITHOUT navigating this page (a request on its session),
  // and the toggle's answer must bring it into the Recent row — while keeping
  // the row's offset. (Tapped by script, so nothing else scrolls.)
  test("the pin toggle's answer changes what the Recent row shows with no navigation, and keeps its offset", async ({ page }) => {
    await open(page);
    await page.locator('#sidebar-close').click();
    const hrefs = () => strip(page).locator('a[data-switcher-entry]').evaluateAll((els) => els.map((e) => e.getAttribute('href')));
    const before = await hrefs();
    await strip(page).evaluate((e) => (e.scrollLeft = 200));
    await expect.poll(async () => (await pos(page)).x, 'control: the row scrolls that far').toBe(200);
    // Opened in "another tab": the page itself stays where it is.
    const r = await page.request.get(rig.origin + rig.terminalPath('alpha9'));
    expect(r.status(), 'control: the visit was recorded by opening the window').toBe(200);
    const visited = new URL(r.url()).pathname + new URL(r.url()).search;
    expect(before, 'control: the visited window was not in the Recent row yet').not.toContain(visited);
    const pin = page.locator('#switcher-pin');
    const was = await pin.getAttribute('aria-pressed');
    try {
      await page.evaluate(() => (document.getElementById('switcher-pin') as HTMLElement).click());
      await expect(pin).not.toHaveAttribute('aria-pressed', was!);
      await expect.poll(async () => (await hrefs())[0], 'the swap put the visited window first in the Recent row').toBe(visited);
      await page.waitForTimeout(300);
      expect((await pos(page)).x, 'the row kept its offset').toBe(200);
    } finally {
      if ((await pin.getAttribute('aria-pressed')) !== was) await page.evaluate(() => (document.getElementById('switcher-pin') as HTMLElement).click());
      await expect(pin).toHaveAttribute('aria-pressed', was!);
    }
  });
});
