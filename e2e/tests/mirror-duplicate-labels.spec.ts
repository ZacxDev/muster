import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Locator, Page } from '@playwright/test';
import { test, expect, takeNew } from '../lib/fixtures';
import { binDir, dsn, dupLabelsPort, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { writeFileAtomic } from '../lib/atomic';

// SAME LABEL, TWO HOSTS. A host labels its windows "<codename>:<index>", so two
// machines that each run a `mango` session both list "mango:1". Measured on
// 0.36.0: the Agents page drew two cards both titled `mango:1` — the host only
// in a small grey chip — and the operator opened the wrong one. The pages were
// titled the same too (`mango:1 · Mirrored · muster`).
//
// Here, against a REAL muster-server (MUSTER_MIRROR=1) and two REAL
// muster-mirror processes (the `-tags e2emirror` per-window fake, which plays
// tmux only): each card's title carries its host when — and only while — the
// label is not unique, and the page each card opens is titled exactly as the
// card (header and <title>).
//
// 🔴 A REGRESSION TEST: on the commit before it, both cards' titles read
// `mango:1` and the "two distinct titles" assertion fails.

const origin = `http://localhost:${dupLabelsPort}`;
const stamp = Date.now().toString(36);
const hostA = 'e2e-dupa-' + stamp;
const hostB = 'e2e-dupb-' + stamp;
// A third host whose name CONTAINS host A's: what a substring match cannot tell
// apart (the second test).
const hostC = hostA + 'book';
const mirrorBin = path.join(binDir, 'muster-mirror-e2e-dup');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mirror-dup-labels-'));
const transcript = path.join(dir, 'cccccccc-0001.jsonl');
let server: Running | undefined;
const procs: ChildProcess[] = [];
let mirrorLog = '';

interface Win {
  session: string;
  id: string;
  index: number;
  active: boolean;
  command: string;
  activity: number;
  claude?: string;
}
const now = Math.floor(Date.now() / 1000);
// Host A: mango:1 runs Claude Code (its card opens the RENDERED view), and an
// unlabelled kiwi:2 that no other host has.
const aMango: Win = { session: 'mango', id: '@1', index: 1, active: true, command: 'claude', activity: now - 60, claude: transcript };
const aKiwi: Win = { session: 'kiwi', id: '@3', index: 2, active: true, command: 'zsh', activity: now - 600 };
// Host B: mango:1 is a shell (its card opens the TERMINAL).
const bMango: Win = { session: 'mango', id: '@5', index: 1, active: true, command: 'zsh', activity: now - 120 };
// Host C: one window.
const cFig: Win = { session: 'fig', id: '@2', index: 1, active: true, command: 'zsh', activity: now - 300 };

const windowsFile = (host: string) => path.join(dir, host + '.windows');
function writeWindows(host: string, ws: Win[]) {
  const f = windowsFile(host);
  writeFileAtomic(f, ws.map((w) => [w.session, w.id, w.index, w.active ? 1 : 0, w.command, w.activity, w.claude ?? ''].join('|')).join('\n') + '\n');
}

function mint(host: string): string {
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  return token;
}

function startHost(host: string, token: string) {
  const p = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_LABELS_FILE: path.join(dir, 'labels.json'),
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile(host),
      MUSTER_MIRROR_E2E_PASTE_FILE: path.join(dir, host + '.pasted'),
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  p.stdout?.on('data', (d) => (mirrorLog += d));
  p.stderr?.on('data', (d) => (mirrorLog += d));
  procs.push(p);
}

let tokenB = '';
let tokenC = '';
test.beforeAll(async () => {
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, dupLabelsPort, {
    MUSTER_MIRROR: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  fs.writeFileSync(transcript, JSON.stringify({ type: 'user', promptId: 'p-1', message: { role: 'user', content: 'the prompt on host A' } }) + '\n');
  // BOTH hosts call their `mango` session by the same codename.
  writeFileAtomic(path.join(dir, 'labels.json'), JSON.stringify({ mango: 'mango' }));
  writeWindows(hostA, [aMango, aKiwi]);
  writeWindows(hostB, [bMango]);
  startHost(hostA, mint(hostA));
  tokenB = mint(hostB); // host B connects later, inside the test
  writeWindows(hostC, [cFig]);
  tokenC = mint(hostC); // host C connects in the second test
});

test.afterAll(async () => {
  for (const p of procs) {
    if (p.exitCode === null && p.signalCode === null) {
      const done = new Promise((r) => p.once('exit', r));
      p.kill('SIGTERM');
      await done;
    }
  }
  await server?.stop();
  fs.rmSync(dir, { recursive: true, force: true });
  fs.rmSync(mirrorBin, { force: true });
});

const cardsOf = (page: Page, host: string) => page.locator(`#agents-list article[data-mirror-card]:has([data-mirror-badge][aria-label="Mirrored · ${host}"])`);
const mangoCard = (page: Page, host: string) => cardsOf(page, host).filter({ has: page.locator('[data-mirror-label]', { hasText: /^mango:1$/ }) });

// The TITLE as shown: the name element's text, minus the raw tmux session name
// a window's PAGE draws small beside a label (it is not part of the title; a
// card draws none).
async function shownTitle(scope: Locator): Promise<string> {
  return scope.locator('[data-mirror-name]').first().evaluate((el) => {
    const c = el.cloneNode(true) as HTMLElement;
    c.querySelector('[data-mirror-session]')?.remove();
    return (c.textContent ?? '').trim();
  });
}

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

test('two hosts with the same label: two distinct titles, live, and each page is titled as its card', async ({ page }) => {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  await page.goto(origin + '/agents');

  // Host A alone: its label is unique, so its title is unchanged.
  await expect(cardsOf(page, hostA), `host A's cards (muster-mirror log:\n${mirrorLog})`).toHaveCount(2, { timeout: 20_000 });
  const a = mangoCard(page, hostA);
  await expect.poll(() => shownTitle(a)).toBe('mango:1');
  await expect(a.locator('[data-mirror-qualifier]')).toHaveCount(0);

  // Host B connects, listing "mango:1" too. With NO reload, both cards now
  // carry their host in the TITLE; the unique kiwi:2 does not.
  await page.evaluate(() => ((window as unknown as { __noReload: number }).__noReload = 1));
  startHost(hostB, tokenB);
  const b = mangoCard(page, hostB);
  await expect(b, `host B's card (log:\n${mirrorLog})`).toHaveCount(1, { timeout: 20_000 });
  // Its arrival lands among cards in view, so it waits behind the "N new ↑"
  // pill (agents_filter.go hold) — whether it does depends on how much of the
  // list fits above the fold, which the filter bar's height decides. Take it.
  await takeNew(page);
  await expect.poll(() => shownTitle(a), { timeout: 20_000 }).toBe(`mango:1 · ${hostA}`);
  await expect.poll(() => shownTitle(b)).toBe(`mango:1 · ${hostB}`);
  const titles = [await shownTitle(a), await shownTitle(b)];
  expect(new Set(titles).size, `two DISTINCT titles: ${titles}`).toBe(2);
  const kiwi = cardsOf(page, hostA).filter({ hasNot: page.locator('[data-mirror-label]') });
  await expect.poll(() => shownTitle(kiwi)).toBe('kiwi:2');
  expect(await page.evaluate(() => (window as unknown as { __noReload?: number }).__noReload), 'no reload happened').toBe(1);

  // The mirrored mark is visible on the phone and names its host; the host
  // itself is visible text in the footer.
  for (const [c, h] of [[a, hostA], [b, hostB]] as const) {
    const badge = c.locator('[data-mirror-badge]');
    await expect(badge).toBeVisible();
    await expect(badge).toHaveAttribute('aria-label', `Mirrored · ${h}`);
    await expect(c.locator('[data-mirror-host]')).toHaveText(h);
    const box = (await c.boundingBox())!;
    expect(box.x + box.width, 'the card fits the phone').toBeLessThanOrEqual(390);
  }
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'no sideways scroll').toBeLessThanOrEqual(cw);

  // The session switcher's search (which replaced the list's search box)
  // finds both by their codename, each titled apart by its host.
  await page.locator('#switcher-search-open').click();
  await page.locator('#switcher-search-input').fill('mango');
  const found = await page.locator('#switcher-search-results [role="option"] > span:first-child').allTextContents();
  expect(found, 'two mango windows, told apart by their hosts').toEqual(expect.arrayContaining([`mango:1 · ${hostA}`, `mango:1 · ${hostB}`]));
  await page.keyboard.press('Escape');

  // Each card opens a page titled EXACTLY as the card: A's (Claude Code) the
  // rendered view, B's (a shell) the terminal.
  const aTitle = await shownTitle(a);
  await a.locator('a').first().click();
  await page.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('host') === hostA);
  await expect.poll(() => shownTitle(page.locator('header h1'))).toBe(aTitle);
  await expect(page).toHaveTitle(`${aTitle} · Mirrored · muster`);
  await expect(page.locator('header [data-term-badge]')).toContainText(`Mirrored · ${hostA}`);

  await page.goto(origin + '/agents');
  const bTitle = await shownTitle(mangoCard(page, hostB));
  expect(bTitle).toBe(`mango:1 · ${hostB}`);
  await mangoCard(page, hostB).locator('a').first().click();
  await page.waitForURL((u) => u.pathname === '/mirror/terminal' && u.searchParams.get('host') === hostB);
  await expect.poll(() => shownTitle(page.locator('header h1'))).toBe(bTitle);
  await expect(page).toHaveTitle(`Terminal · ${bTitle} · muster`);
  expect(aTitle).not.toBe(bTitle);

  // The clash goes away (host B closes its mango window): A's card is plain
  // "mango:1" again, live.
  await page.goto(origin + '/agents');
  await expect.poll(() => shownTitle(mangoCard(page, hostA)), { timeout: 20_000 }).toBe(`mango:1 · ${hostA}`);
  writeWindows(hostB, [{ ...bMango, session: 'other', id: '@6' }]);
  await expect.poll(() => shownTitle(mangoCard(page, hostA)), { timeout: 20_000 }).toBe('mango:1');
  await expect(mangoCard(page, hostB)).toHaveCount(0);
});

// A HOST TAB MATCHES EXACTLY. Host C's name contains host A's ("…book"), so a
// substring match on host A shows C's window too — which is how choosing host
// "mac" once showed "macbook". The tab filters on the whole host field.
//
// 🔴 A REGRESSION TEST (ported from the host dropdown the tabs replaced): with
// the tab's term made a substring (internal/ui/agents_filter.go, `exact: true`
// dropped), C's card stays visible under A's tab.
const hostTab = (page: Page, h: string) => page.locator(`#agents-hosts [role="tab"][data-host="${h}"]`);
test('a host tab matches exactly', async ({ page }) => {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  startCOnce();
  await page.goto(origin + '/agents');
  await expect(cardsOf(page, hostC), `host C's card (log:\n${mirrorLog})`).toHaveCount(1, { timeout: 20_000 });
  await expect(cardsOf(page, hostA)).toHaveCount(2);
  await takeNew(page);
  const visible = (h: string) => cardsOf(page, h).filter({ visible: true });
  await expect(hostTab(page, hostC)).toHaveCount(1, { timeout: 20_000 });

  // Chosen: host A's two cards, and NOT host C's.
  await hostTab(page, hostA).tap();
  await expect(visible(hostA)).toHaveCount(2);
  await expect(visible(hostC), 'host C contains host A but is not host A').toHaveCount(0);
  await expect(hostTab(page, hostA).locator('[data-tab-count]')).toHaveText('2');
  // Positive control: choosing C shows C, so the hiding above is the match and
  // not a card that never rendered.
  await hostTab(page, hostC).tap();
  await expect(visible(hostC)).toHaveCount(1);
  await expect(visible(hostA)).toHaveCount(0);

  // The chosen tab survives a reload, still exact.
  await hostTab(page, hostA).tap();
  await page.reload();
  await expect(hostTab(page, hostA)).toHaveAttribute('aria-selected', 'true', { timeout: 20_000 });
  await expect(visible(hostC)).toHaveCount(0);
  await expect(visible(hostA)).toHaveCount(2);
  await hostTab(page, '').tap();
});

async function signIn(page: Page) {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}
// startCOnce connects host C the first time a test needs it (so a test run
// alone still has it).
let cStarted = false;
function startCOnce() {
  if (cStarted) return;
  cStarted = true;
  startHost(hostC, tokenC);
}

// A LIST REFRESH NEVER UNDOES A CHOSEN TAB: the tabs are rebuilt on every
// refresh, which must not reset the chosen one. (Its sibling — a refresh never
// moves what is being typed in the search box — went with the box.)
async function openRefreshable(page: Page) {
  await signIn(page);
  startCOnce();
  await page.goto(origin + '/agents');
  await expect(cardsOf(page, hostC), `host C's card (log:\n${mirrorLog})`).toHaveCount(1, { timeout: 20_000 });
  await takeNew(page);
  const visible = (h: string) => cardsOf(page, h).filter({ visible: true });
  await expect(hostTab(page, hostC)).toHaveCount(1, { timeout: 20_000 });
  const kiwiTitle = () => shownTitle(cardsOf(page, hostA).filter({ hasNot: page.locator('[data-mirror-label]') }));
  await page.evaluate(() => ((window as unknown as { __noReload: number }).__noReload = 1));
  return { visible, kiwiTitle };
}

test('a refresh never undoes a chosen tab', async ({ page }) => {
  const { visible, kiwiTitle } = await openRefreshable(page);
  await hostTab(page, hostA).tap();
  await expect(visible(hostA)).toHaveCount(2);
  await expect(visible(hostC)).toHaveCount(0);
  writeWindows(hostA, [aMango, { ...aKiwi, index: 5 }]);
  await expect.poll(kiwiTitle, { message: `the list refreshed (log:\n${mirrorLog})`, timeout: 20_000 }).toBe('kiwi:5');
  await expect(hostTab(page, hostA), 'the refresh did not undo the tab').toHaveAttribute('aria-selected', 'true');
  await expect(visible(hostA)).toHaveCount(2);
  await expect(visible(hostC)).toHaveCount(0);
  expect(await page.evaluate(() => (window as unknown as { __noReload?: number }).__noReload), 'no reload happened').toBe(1);
  await hostTab(page, '').tap();
  writeWindows(hostA, [aMango, aKiwi]);
});

// ONE HOST, ONE CODENAME, TWO SESSIONS — and no raw session name on the card.
// The card no longer draws the raw tmux session beside a codename (an operator
// ask: the resolved title only), so what tells two "violet:1" windows of ONE
// host apart is their qualifier: the tmux address (mirror.qualify), part of the
// title. A codename no other window shares carries no qualifier, and its card
// spells no session name at all.
//
// 🔴 Red before: each labelled card also drew its session name (`scratch7`,
// `scratch9`) muted beside the title, so the "no raw session text" assertions
// fail there.
test('two sessions of one host given one codename: two distinct titles, and no raw session name on a labelled card', async ({ page }) => {
  const hostD = 'e2e-dupd-' + stamp;
  // The codenames are written before host D starts; hosts A–C have none of
  // these sessions, so their titles are unchanged.
  writeFileAtomic(path.join(dir, 'labels.json'), JSON.stringify({ mango: 'mango', scratch7: 'violet', scratch8: 'violet', scratch9: 'amber' }));
  writeWindows(hostD, [
    { session: 'scratch7', id: '@1', index: 1, active: true, command: 'zsh', activity: now - 30 },
    { session: 'scratch8', id: '@2', index: 1, active: true, command: 'zsh', activity: now - 40 },
    { session: 'scratch9', id: '@3', index: 1, active: true, command: 'zsh', activity: now - 50 },
  ]);
  startHost(hostD, mint(hostD));
  await signIn(page);
  await page.goto(origin + '/agents');
  await expect(cardsOf(page, hostD), `host D's cards (log:\n${mirrorLog})`).toHaveCount(3, { timeout: 20_000 });
  await takeNew(page);
  const violet = cardsOf(page, hostD).filter({ has: page.locator('[data-mirror-label]', { hasText: /^violet:1$/ }) });
  await expect(violet).toHaveCount(2);
  const titles = (await violet.evaluateAll((els) => els.map((e) => (e.querySelector('[data-mirror-name]')?.textContent ?? '').trim()))).sort();
  expect(titles, 'each carries its tmux address as its qualifier').toEqual(['violet:1 · scratch7:1', 'violet:1 · scratch8:1']);
  const amber = cardsOf(page, hostD).filter({ has: page.locator('[data-mirror-label]', { hasText: /^amber:1$/ }) });
  await expect(amber.locator('[data-mirror-name]')).toHaveText('amber:1');
  await expect(amber.locator('[data-mirror-qualifier]')).toHaveCount(0);
  for (const c of [amber, violet.first(), violet.last()]) await expect(c.locator('[data-mirror-session]')).toHaveCount(0);
  expect(await amber.innerText(), 'a unique codename\'s card spells no session name').not.toContain('scratch9');
  // The violet cards spell their session ONLY in the qualifier, once each.
  for (const c of [violet.first(), violet.last()]) {
    const text = await c.innerText();
    expect((text.match(/scratch[78]/g) ?? []).length, `one mention, the qualifier's: ${text}`).toBe(1);
  }
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'no sideways scroll').toBeLessThanOrEqual(cw);
});
