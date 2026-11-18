import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { hostNamesPort } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';

// THE OPERATOR'S OWN HOSTS, in a real browser against a real muster-server
// (mirror plugin on) configured the way the operator's deployment is — two
// hosts given icons by name:
//
//	MUSTER_HOST_ICONS=laptop=laptop,tower=server
//
// — and four REAL muster-mirror processes over the tunnel (the `-tags
// e2emirror` fake plays tmux only), plus the seeded dispatched agents on the
// host "cluster". (`tower` stands in for the operator's own server.)
//
// 1. NAMED ICONS (internal/ui/host_icons.go, HostPreferences.Icons): `laptop`
//    draws the laptop glyph though it was registered AFTER tower (by rank
//    alone it drew another), `tower` the server glyph, and no other host draws
//    either — on the sidebar's rows, a window's own page and the switcher.
// 2. THE HOST IS SPELLED ON EVERY ENTRY: a row's icon names its host, and the
//    host is the row's last search word ("cluster" for a dispatched agent).
//
// (The host tabs' ORDER — All, tower, laptop, the rest A–Z, cluster last — has
// no successor: the tabs went with the Agents list, and the sidebar is one
// list ordered by who waits and by activity.)
//
// 🔴 REGRESSION TEST: on the commit before named icons, the laptop host drew
// the icon of its registration rank.

const rig = new MirrorRig(hostNamesPort, 'e2e-names-zeta', { MUSTER_HOST_ICONS: 'laptop=laptop,tower=server' });
const zeta = rig.host;
const mint = rig.host.replace('e2e-names-zeta-', 'e2e-names-mint-');
const HOSTS = ['tower', 'laptop', mint, zeta];
const t0 = Math.floor(Date.now() / 1000);
// session|id|index|active|command|activity|claude-transcript
const line = (session: string, ago: number) => `${session}|@1|1|1|zsh|${t0 - ago}|`;

test.beforeAll(async () => {
  test.setTimeout(180_000);
  // Registration order: zeta (the rig's own), tower, mint, laptop.
  await rig.start();
  const files: Record<string, string> = {};
  for (const [h, session, ago] of [
    [zeta, 'zsession', 100],
    ['tower', 'wsession', 200],
    [mint, 'msession', 300],
    ['laptop', 'lsession', 400],
  ] as const) {
    files[h] = rig.transcript('', 'windows-' + session);
    writeFileAtomic(files[h], line(session, ago) + '\n');
  }
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: files[zeta] });
  for (const h of ['tower', mint, 'laptop']) rig.extraHost(h, { MUSTER_MIRROR_E2E_WINDOWS_FILE: files[h] });
});

test.afterAll(async () => {
  await rig.stop();
});

test.use({ storageState: { cookies: [], origins: [] } });

const rowsOn = (page: Page, host: string) =>
  host === 'cluster' ? page.locator('#sidebar-agents-list a[data-nav-agent="agent"]') : page.locator(`#sidebar-agents-list a[data-nav-agent="mirror"][href*="host=${host}&"]`);
const rowIcons = (page: Page, host: string) => rowsOn(page, host).locator('[data-host-icon]').evaluateAll((els) => els.map((e) => e.getAttribute('data-host-icon')));

// openList signs in, opens a page with the sidebar (the account page) with
// its list shown — the rail on a desktop, the drawer on a phone — once every
// host's window and the cluster's agents are rows.
async function openList(page: Page) {
  await rig.login(page);
  for (const s of ['zsession', 'wsession', 'msession', 'lsession']) await rig.waitListed(page, s);
  await page.goto(rig.origin + '/account');
  if ((page.viewportSize()?.width ?? 0) < 1024) await page.locator('#sidebar-open').click();
  await expect
    .poll(async () => [...(await Promise.all(HOSTS.map((h) => rowsOn(page, h).count()))), (await rowsOn(page, 'cluster').count()) > 0], {
      timeout: 30_000,
      message: `every host's window and the cluster's agents are listed (log:\n${rig.log})`,
    })
    .toEqual([1, 1, 1, 1, true]);
}

for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('laptop draws the laptop glyph and tower the server glyph, on rows, switcher entries and pages; no other host draws either', async ({ page }) => {
      await openList(page);
      expect(await rowIcons(page, 'laptop'), 'laptop, registered after tower, draws a laptop').toEqual(['laptop']);
      expect(await rowIcons(page, 'tower')).toEqual(['server']);
      for (const h of [mint, zeta]) {
        const [k] = await rowIcons(page, h);
        expect(['laptop', 'server', 'cluster'], `${h} draws ${k}`).not.toContain(k);
      }
      expect(new Set(await rowIcons(page, 'cluster'))).toEqual(new Set(['cluster']));
      // The window's own page leads with the same glyph.
      await page.goto(`${rig.origin}/mirror/terminal?host=laptop&session=lsession`);
      await expect(page.locator('[data-term-badge] [data-host-icon]')).toHaveAttribute('data-host-icon', 'laptop');
      await page.goto(`${rig.origin}/mirror/terminal?host=tower&session=wsession`);
      await expect(page.locator('[data-term-badge] [data-host-icon]')).toHaveAttribute('data-host-icon', 'server');
      // …and so do its entries in a session page's switcher (a seeded agent's
      // chat: the shared database's agents are this server's too), the All view.
      await page.addInitScript(() => { try { localStorage.setItem('muster.switcher.view', 'all'); } catch (e) {} });
      await page.goto(rig.origin + '/agents/notes-tidy');
      const all = page.locator('#session-switcher [data-switcher-list="all"]');
      await expect(all.locator('a[data-switcher-entry="mirror"][href*="host=laptop&"] [data-host-icon]')).toHaveAttribute('data-host-icon', 'laptop');
      await expect(all.locator('a[data-switcher-entry="mirror"][href*="host=tower&"] [data-host-icon]')).toHaveAttribute('data-host-icon', 'server');
      await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
    });

    // THE HOST IS SPELLED ON EVERY ENTRY: a mirrored row's icon names its host
    // in its tooltip, and the host is the last word the palette finds the row
    // by; a dispatched agent's host is "cluster". (The card's host chip, and
    // the footer it replaced, went with the list.)
    test("every row names its host: in its icon's tooltip, and as its last search word; an agent's is the cluster", async ({ page }) => {
      await openList(page);
      for (const h of HOSTS) {
        const r = rowsOn(page, h).first();
        await expect(r.locator('[data-host-icon]'), h).toHaveAttribute('title', 'Host: ' + h);
        const words = ((await r.getAttribute('data-switcher-words')) || '').split(' ');
        expect(words[words.length - 1], `${h}'s last word`).toBe(h.toLowerCase());
      }
      const agentWords = ((await rowsOn(page, 'cluster').first().getAttribute('data-switcher-words')) || '').split(' ');
      expect(agentWords[agentWords.length - 1], "an agent's last word").toBe('cluster');
      await expect(rowsOn(page, 'cluster').first().locator('[data-host-icon="cluster"]')).toHaveAttribute('title', 'Host: cluster');
      const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
      expect(sw, 'no sideways page scroll').toBeLessThanOrEqual(cw);
    });
  });
}
