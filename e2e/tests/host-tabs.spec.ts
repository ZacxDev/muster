import { Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { hostTabsPort } from '../lib/env';
import { MirrorRig, mkchat } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';
import { agentLink } from '../lib/agent-link';

// FINDING AN ENTRY BY ITS HOST — the retired Agents list's host tabs'
// successor (internal/ui/nav_agents.go: "the host tabs became a word: the
// palette finds an entry by its host's name") — in a real browser, against a
// real muster-server with the mirror plugin on and REAL muster-mirror
// processes over the tunnel (the `-tags e2emirror` fake plays tmux only): host
// A (this rig's own), host B and host AB (connected beside it) and, later,
// host C — plus a dispatched agent of this spec's own (e2e/mkchat), which is
// on the host "cluster" with the seeded pod agents.
//
// A host's name in the palette finds exactly its own windows (never a host
// whose name merely shares a prefix — host AB's begins with host A's tag),
// "cluster" finds the dispatched agents, a host that connects is found live,
// and a host that goes away is marked offline on the sidebar's rows and back.
// (The tabs' strip, its counts, its remembered choice and its keyboard moves
// have no successor: the sidebar is one list.)
//
// And THE FOCUS TITLE IS STORED AND SHOWN WHOLE WHERE IT IS READ: this spec's
// agent carries a 120-character focus title, which the agent page's focus
// history shows untruncated and wrapped; never sideways overflow.
//
// 🔴 REGRESSION TEST: on the commit before, the focus title was stored cut to
// 80 characters ending in "…" (internal/focus Clean).

const rig = new MirrorRig(hostTabsPort, 'e2e-tabs-a', { MUSTER_FOCUS_INTERVAL: '0' });
const hostA = rig.host;
const hostB = rig.host.replace('e2e-tabs-a-', 'e2e-tabs-b-');
const hostC = rig.host.replace('e2e-tabs-a-', 'e2e-tabs-c-');
// Sorts between host A and host B ('-' < 'b').
const hostAB = rig.host.replace('e2e-tabs-a-', 'e2e-tabs-ab-');
const agentName = 'e2e-tabs-' + Date.now().toString(36);
// 120 characters: past the old 80-character clamp, under the 300 storage guard.
const LONG = 'Untangling the reconnect backoff so a flapping tunnel stops resetting its retry window, then proving it with a soak test';

const t0 = Math.floor(Date.now() / 1000);
// session|id|index|active|command|activity|claude-transcript
const line = (session: string, id: string, ago: number) => `${session}|${id}|1|1|zsh|${t0 - ago}|`;
let filesA = '';
let filesB = '';
let filesC = '';
let filesAB = '';
const writeTo = (f: string, lines: string[]) => {
  writeFileAtomic(f, lines.length ? lines.join('\n') + '\n' : '');
};
// 🔴 HOST A'S "alpha" WINDOW IS NAMED FOR THIS RUN, because a search for it WAS
// asserted against the WHOLE list (the list's search box is gone; the name is
// kept unique all the same). That list is the shared e2e database's: every
// spec's agents are cards on it too (host "cluster"), and the box finds a card
// by any text on it — its claude-code account included, and other specs put
// agents on the account "alpha" (e2e/heldfix's row; kinds.spec.ts saves some).
// Searched as a plain "alpha", each such agent still on the database is one
// more visible card: two of them make the 3-where-1-is-wanted a full run once
// counted, and seeding two reproduces it every time. No other spec can spell
// this run's tag.
const alpha = 'alpha' + rig.host.slice(rig.host.lastIndexOf('-') + 1);
// Each of A and B has a window called "zebrafish", which the search finds on both.
const windowsA = () => [line(alpha, '@1', 100), line('zebrafish', '@2', 200)];
const windowsB = () => [line('beta', '@1', 150), line('zebrafish', '@2', 250)];

let stopB: () => Promise<void> = async () => {};

test.beforeAll(async () => {
  test.setTimeout(180_000);
  expect(LONG.length, 'the fixture title is 120 characters').toBe(120);
  mkchat(agentName, [{ role: 'user', content: 'Fix the tunnel backoff.' }], {
    MUSTER_E2E_FOCUS: JSON.stringify([{ subject: 'self', title: LONG, description: 'The retry window resets too early. One soak left.' }]),
  });
  await rig.start();
  filesA = rig.transcript('', 'windows-a');
  filesB = rig.transcript('', 'windows-b');
  filesC = rig.transcript('', 'windows-c');
  filesAB = rig.transcript('', 'windows-ab');
  writeTo(filesA, windowsA());
  writeTo(filesB, windowsB());
  // Nine days: short of the sidebar's Idle group's 14 days, so its row is in
  // the list itself (the palette finds an idle row too; that is not what this
  // spec is about).
  writeTo(filesC, [line('citrus', '@1', 9 * 86400)]);
  writeTo(filesAB, [line('abacus', '@1', 0)]);
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: filesA });
  stopB = rig.extraHost(hostB, { MUSTER_MIRROR_E2E_WINDOWS_FILE: filesB });
  rig.extraHost(hostAB, { MUSTER_MIRROR_E2E_WINDOWS_FILE: filesAB });
});

test.afterAll(async () => {
  await rig.stop();
  mkchat(agentName, null);
});

test.use({ storageState: { cookies: [], origins: [] } });

const pal = (page: Page) => page.locator('#switcher-search');
const input = (page: Page) => page.locator('#switcher-search-input');
const options = (page: Page) => page.locator('#switcher-search-results [role="option"]');
// found is what the palette lists for q, as its options' links.
async function found(page: Page, q: string): Promise<string[]> {
  await input(page).fill(q);
  return options(page).evaluateAll((els) => els.map((e) => e.getAttribute('data-href') || ''));
}
const onHost = (hrefs: string[], h: string) => hrefs.filter((x) => new URLSearchParams(x.replace(/^[^?]*\?/, '')).get('host') === h);
const rowsOn = (page: Page, h: string) => page.locator(`#sidebar-agents-list a[data-nav-agent="mirror"][href*="host=${h}&"]`);

// openList signs in, waits until both hosts and the agent are listed, and
// opens a page with no bar (the account page) with the sidebar's list shown —
// the rail on a desktop, the drawer on a phone — which the palette reads.
async function openList(page: Page) {
  await rig.login(page);
  await rig.waitListed(page, 'beta');
  await rig.waitListed(page, alpha);
  await rig.waitListed(page, 'abacus');
  await page.goto(rig.origin + '/account');
  if ((page.viewportSize()?.width ?? 0) < 1024) await page.locator('#sidebar-open').click();
  await expect
    .poll(async () => [await rowsOn(page, hostA).count(), await rowsOn(page, hostB).count(), await page.locator(`#sidebar-agents-list a[data-nav-agent="agent"]:is(${agentLink(agentName).replace(/a\[/g, '[')})`).count()], {
      timeout: 30_000,
      message: `both hosts' windows and the agent are listed (log:\n${rig.log})`,
    })
    .toEqual([2, 2, 1]);
}

async function openPalette(page: Page) {
  await page.locator('#sidebar-search').click();
  await expect(pal(page)).toBeVisible();
}

async function noPageOverflow(page: Page) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'no sideways page scroll').toBeLessThanOrEqual(cw);
}

for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);
    const press = (l: Locator) => (vp.use.hasTouch ? l.tap() : l.click());

    test("the agent page's focus history shows the 120-character title whole", async ({ page }) => {
      await rig.login(page);
      await page.goto(rig.origin + '/agents/' + agentName);
      await press(page.locator('#focus-history-open'));
      const dialog = page.locator('dialog#focus-history');
      await expect(dialog).toBeVisible();
      const first = dialog.locator('[data-focus-history-title]').first();
      await expect(first).toHaveText(LONG);
      expect(await first.evaluate((e) => e.scrollWidth <= e.clientWidth + 1), 'the history title does not overflow sideways').toBe(true);
      const db = (await dialog.boundingBox())!;
      expect(db.x + db.width).toBeLessThanOrEqual(vp.use.viewport.width);
      await page.keyboard.press('Escape');
    });

    // THE HOST TABS BECAME A WORD (internal/ui/nav_agents.go: "the host tabs
    // became a word: the palette finds an entry by its host's name"). A host's
    // name finds exactly its own windows — never a host whose name merely
    // shares a prefix with it (the tabs' exactness rule, kept: host AB's name
    // begins with host A's tag) — and "cluster" finds the dispatched agents.
    test("the palette finds a host's entries by the host's name, and only that host's", async ({ page }) => {
      await openList(page);
      await openPalette(page);
      const a = await found(page, hostA);
      expect(onHost(a, hostA).length, "host A's two windows").toBe(2);
      expect(a.length, "only host A's").toBe(2);
      const b = await found(page, hostB);
      expect(onHost(b, hostB).length, "host B's two windows").toBe(2);
      expect(b.length, "only host B's").toBe(2);
      const ab = await found(page, hostAB);
      expect(ab.length, "host AB's one window").toBe(1);
      expect(onHost(ab, hostAB).length).toBe(1);
      // The cluster: the dispatched agent, and none of the mirrored windows.
      const cl = await found(page, 'cluster');
      expect(cl.some((h) => h.startsWith('/agents/' + agentName)), 'the agent is on the cluster').toBe(true);
      expect(cl.filter((h) => h.startsWith('/mirror/')), 'no window is on the cluster').toEqual([]);
      // "zebrafish" is a window on both A and B: the name alone finds both.
      const z = await found(page, 'zebrafish');
      expect([onHost(z, hostA).length, onHost(z, hostB).length]).toEqual([1, 1]);
      await noPageOverflow(page);
      // Enter opens the find.
      await input(page).fill('beta');
      await Promise.all([
        page.waitForURL((u) => u.pathname.startsWith('/mirror/') && u.searchParams.get('host') === hostB && u.searchParams.get('session') === 'beta'),
        page.keyboard.press('Enter'),
      ]);
    });
  });
}

// A HOST THAT CONNECTS IS FOUND BY ITS NAME, LIVE, and a host that goes away
// says so: its rows are marked offline (the tabs' "its tab is gone"), and come
// back online when it returns — no reload.
test.describe('phone 390×844, hosts coming and going', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('a host that connects is found by its name, live; a host that goes away is marked offline, and back', async ({ page }) => {
    test.setTimeout(90_000);
    await openList(page);
    await page.evaluate(() => ((window as unknown as { __noReload: number }).__noReload = 1));
    await openPalette(page);
    expect(await found(page, hostC), 'host C is not connected yet').toEqual([]);
    await page.keyboard.press('Escape');
    const stopC = rig.extraHost(hostC, { MUSTER_MIRROR_E2E_WINDOWS_FILE: filesC });
    try {
      // The sidebar's list follows (the list's own refresh event).
      await expect
        .poll(async () => {
          await page.evaluate(() => document.body.dispatchEvent(new Event('muster:resync', { bubbles: true })));
          return rowsOn(page, hostC).count();
        }, { timeout: 30_000, message: `host C connected (log:\n${rig.log})` })
        .toBe(1);
      await openPalette(page);
      const c = await found(page, hostC);
      expect(c.length, "host C's one window").toBe(1);
      expect(onHost(c, hostC).length).toBe(1);
      await page.keyboard.press('Escape');

      // Host B goes away (its muster-mirror stops): its rows say offline.
      await stopB();
      await expect
        .poll(async () => {
          await page.evaluate(() => document.body.dispatchEvent(new Event('muster:resync', { bubbles: true })));
          return rowsOn(page, hostB).locator('[data-nav-offline]').count();
        }, { timeout: 30_000, message: `host B disconnected (log:\n${rig.log})` })
        .toBe(2);
      await expect(rowsOn(page, hostA).locator('[data-nav-offline]'), 'host A is still online').toHaveCount(0);
      // Back: online again.
      stopB = rig.extraHost(hostB, { MUSTER_MIRROR_E2E_WINDOWS_FILE: filesB });
      await expect
        .poll(async () => {
          await page.evaluate(() => document.body.dispatchEvent(new Event('muster:resync', { bubbles: true })));
          return rowsOn(page, hostB).locator('[data-nav-offline]').count();
        }, { timeout: 30_000, message: `host B reconnected (log:\n${rig.log})` })
        .toBe(0);
      expect(await page.evaluate(() => (window as unknown as { __noReload?: number }).__noReload), 'no reload happened').toBe(1);
    } finally {
      await stopC();
    }
  });
});
