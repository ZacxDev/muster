import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { binDir, dsn, dupLabelsPort, repoRoot, versionTerm } from '../lib/env';
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
// tmux only): each entry's title carries its host when — and only while — the
// label is not unique, and the page each entry opens is titled exactly as the
// entry (header and <title>). The entries were the Agents page's cards; they
// are the sidebar's rows now (whose title is the whole label).
//
// 🔴 A REGRESSION TEST: on the commit before it, both entries' titles read
// `mango:1` and the "two distinct titles" assertion fails.

const origin = `http://localhost:${dupLabelsPort}`;
const stamp = Date.now().toString(36);
const hostA = 'e2e-dupa-' + stamp;
const hostB = 'e2e-dupb-' + stamp;
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

// A host's rows in the sidebar's list. The trailing '&' makes the host match
// whole.
const cardsOf = (page: Page, host: string) => page.locator(`#sidebar-agents-list a[data-nav-agent="mirror"][href*="host=${encodeURIComponent(host)}&"]`);
const sessionRow = (page: Page, host: string, session: string) => page.locator(`#sidebar-agents-list a[data-nav-agent="mirror"][href*="host=${encodeURIComponent(host)}&session=${session}&"]`);
const mangoCard = (page: Page, host: string) => sessionRow(page, host, 'mango');
// A row's title as shown: its text, which is its whole label (and its title).
const rowTitle = async (row: Locator): Promise<string> => {
  const [text, title] = await row.evaluate((e) => [(e.textContent ?? '').trim(), e.getAttribute('title') ?? '']);
  return text === title ? text : `text ${JSON.stringify(text)} ≠ title ${JSON.stringify(title)}`;
};

// openSidebar opens the sidebar's drawer (the phone) on the board, a page of
// its own: its list loads on that open, and refreshes itself while open.
async function openSidebar(page: Page) {
  await page.goto(origin + '/tasks');
  await page.locator('#sidebar-open').tap();
  await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });
}

// The TITLE as shown on a window's own PAGE: the name element's text, minus
// the raw tmux session name the page draws small beside a label (it is not
// part of the title).
async function shownTitle(scope: Locator): Promise<string> {
  return scope.locator('[data-mirror-name]').first().evaluate((el) => {
    const c = el.cloneNode(true) as HTMLElement;
    c.querySelector('[data-mirror-session]')?.remove();
    return (c.textContent ?? '').trim();
  });
}

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

test('two hosts with the same label: two distinct titles, live, and each page is titled as its row', async ({ page }) => {
  await signIn(page, origin);
  await expect
    .poll(async () => (await (await page.request.get(origin + '/ui/nav/agents')).text()).split(`host=${hostA}&amp;`).length - 1, { message: `host A's rows (muster-mirror log:\n${mirrorLog})`, timeout: 20_000 })
    .toBe(2);
  await openSidebar(page);

  // Host A alone: its label is unique, so its title is unchanged.
  await expect(cardsOf(page, hostA), `host A's rows (muster-mirror log:\n${mirrorLog})`).toHaveCount(2, { timeout: 20_000 });
  const a = mangoCard(page, hostA);
  await expect.poll(() => rowTitle(a)).toBe('mango:1');

  // Host B connects, listing "mango:1" too. With NO reload, both rows now
  // carry their host in the TITLE; the unique kiwi:2 does not.
  await page.evaluate(() => ((window as unknown as { __noReload: number }).__noReload = 1));
  startHost(hostB, tokenB);
  const b = mangoCard(page, hostB);
  await expect(b, `host B's row (log:\n${mirrorLog})`).toHaveCount(1, { timeout: 20_000 });
  await expect.poll(() => rowTitle(a), { timeout: 20_000 }).toBe(`mango:1 · ${hostA}`);
  await expect.poll(() => rowTitle(b)).toBe(`mango:1 · ${hostB}`);
  const titles = [await rowTitle(a), await rowTitle(b)];
  expect(new Set(titles).size, `two DISTINCT titles: ${titles}`).toBe(2);
  const kiwi = sessionRow(page, hostA, 'kiwi');
  await expect.poll(() => rowTitle(kiwi)).toBe('kiwi:2');
  expect(await page.evaluate(() => (window as unknown as { __noReload?: number }).__noReload), 'no reload happened').toBe(1);

  // Each row fits the phone, and names its host (the palette's last word).
  for (const [c, h] of [[a, hostA], [b, hostB]] as const) {
    expect((await c.getAttribute('data-switcher-words'))!.split(' ').pop(), 'the row names its host').toBe(h);
    const box = (await c.boundingBox())!;
    expect(box.x + box.width, 'the row fits the phone').toBeLessThanOrEqual(390);
  }
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'no sideways scroll').toBeLessThanOrEqual(cw);

  // The command palette (the sidebar's magnifier) finds both by their
  // codename, each titled apart by its host.
  await page.locator('#sidebar-search').tap();
  await page.locator('#switcher-search-input').fill('mango');
  const found = await page.locator('#switcher-search-results [role="option"] > span:first-child').allTextContents();
  expect(found, 'two mango windows, told apart by their hosts').toEqual(expect.arrayContaining([`mango:1 · ${hostA}`, `mango:1 · ${hostB}`]));
  await page.keyboard.press('Escape');

  // Each row opens a page titled EXACTLY as the row: A's (Claude Code) the
  // rendered view, B's (a shell) the terminal.
  const aTitle = await rowTitle(a);
  await a.tap();
  await page.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('host') === hostA);
  await expect.poll(() => shownTitle(page.locator('header h1'))).toBe(aTitle);
  await expect(page).toHaveTitle(`${aTitle} · Mirrored · muster`);
  await expect(page.locator('header [data-term-badge]')).toContainText(`Mirrored · ${hostA}`);

  await openSidebar(page);
  const bTitle = await rowTitle(mangoCard(page, hostB));
  expect(bTitle).toBe(`mango:1 · ${hostB}`);
  await mangoCard(page, hostB).tap();
  await page.waitForURL((u) => u.pathname === '/mirror/terminal' && u.searchParams.get('host') === hostB);
  await expect.poll(() => shownTitle(page.locator('header h1'))).toBe(bTitle);
  await expect(page).toHaveTitle(`Terminal · ${bTitle} · muster`);
  expect(aTitle).not.toBe(bTitle);

  // The clash goes away (host B closes its mango window): A's row is plain
  // "mango:1" again, live.
  await openSidebar(page);
  await expect.poll(() => rowTitle(mangoCard(page, hostA)), { timeout: 20_000 }).toBe(`mango:1 · ${hostA}`);
  writeWindows(hostB, [{ ...bMango, session: 'other', id: '@6' }]);
  await expect.poll(() => rowTitle(mangoCard(page, hostA)), { timeout: 20_000 }).toBe('mango:1');
  await expect(mangoCard(page, hostB)).toHaveCount(0);
});

// (Two tests here pinned the retired Agents list's HOST TABS: that a tab
// matched its host exactly — host C's name contains host A's — and that a list
// refresh never undid the chosen tab. The tabs are gone; the palette finds an
// entry by its host's name instead.)

// ONE HOST, ONE CODENAME, TWO SESSIONS — and no raw session name on the row.
// The entry does not draw the raw tmux session beside a codename (an operator
// ask: the resolved title only), so what tells two "violet:1" windows of ONE
// host apart is their qualifier: the tmux address (mirror.qualify), part of the
// title. A codename no other window shares carries no qualifier, and its row
// spells no session name at all.
//
// 🔴 Red before: each labelled card (the entry then) also drew its session
// name (`scratch7`, `scratch9`) muted beside the title, so the "no raw session
// text" assertions fail there.
test('two sessions of one host given one codename: two distinct titles, and no raw session name on a labelled row', async ({ page }) => {
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
  await signIn(page, origin);
  await expect
    .poll(async () => (await (await page.request.get(origin + '/ui/nav/agents')).text()).split(`host=${hostD}&amp;`).length - 1, { message: `host D's rows (log:\n${mirrorLog})`, timeout: 20_000 })
    .toBe(3);
  await openSidebar(page);
  await expect(cardsOf(page, hostD), `host D's rows (log:\n${mirrorLog})`).toHaveCount(3, { timeout: 20_000 });
  const violet = [sessionRow(page, hostD, 'scratch7'), sessionRow(page, hostD, 'scratch8')];
  const titles = (await Promise.all(violet.map(rowTitle))).sort();
  expect(titles, 'each carries its tmux address as its qualifier').toEqual(['violet:1 · scratch7:1', 'violet:1 · scratch8:1']);
  const amber = sessionRow(page, hostD, 'scratch9');
  expect(await rowTitle(amber)).toBe('amber:1');
  expect(await amber.innerText(), 'a unique codename\'s row spells no session name').not.toContain('scratch9');
  // The violet rows spell their session ONLY in the qualifier, once each.
  for (const c of violet) {
    const text = await c.innerText();
    expect((text.match(/scratch[78]/g) ?? []).length, `one mention, the qualifier's: ${text}`).toBe(1);
  }
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'no sideways scroll').toBeLessThanOrEqual(cw);
});
