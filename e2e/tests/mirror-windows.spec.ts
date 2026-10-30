import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { binDir, dsn, repoRoot, versionTerm, windowsPort } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { writeFileAtomic } from '../lib/atomic';

// EVERY WINDOW IS MIRRORED, in a real browser, against a REAL muster-server
// (MUSTER_MIRROR=1) and a REAL muster-mirror dialling it over the tunnel. Only
// tmux is played: the `-tags e2emirror` binary's per-window fake
// (cmd/muster-mirror/e2emirror.go, MUSTER_MIRROR_E2E_WINDOWS_FILE), whose
// terminal prints `FAKE-MIRROR session=<s> window=<id> …` and which records
// every paste WITH THE PANE IT REACHED (`%e2e-<session>-<window id>`).
//
// The host is the operator's own shape: session `scratch2` (codename Gold) has
// three windows — a shell at index 1, Claude Code at index 8 (its ACTIVE
// window), Claude Code at index 11 — and session `solo` has one, opencode. With
// one entry per session that host showed two entries and reached two windows;
// the other two were unreachable. Each window is a row of the sidebar's list of
// agents (the retired Agents list's card).
//
// Each test runs at a phone's size (390×844, touch) and at a desktop's.

const origin = `http://localhost:${windowsPort}`;
const host = 'e2e-win-' + Date.now().toString(36);
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mirror-windows-'));
const windowsFile = path.join(dir, 'windows');
const pasteFile = path.join(dir, 'pasted');
const trEight = path.join(dir, 'aaaaaaaa-0008.jsonl');
const trEleven = path.join(dir, 'bbbbbbbb-0011.jsonl');
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';

const prompt = (text: string) => JSON.stringify({ type: 'user', promptId: 'p-' + text.length, message: { role: 'user', content: text } }) + '\n';

interface Win {
  session: string;
  id: string;
  index: number;
  active: boolean;
  command: string;
  activity: number;
  claude?: string;
  // Why the window has no rendered view, as the host says it (the windows
  // file's 11th field; mirrorhost's NoView lines).
  noView?: string;
}
const now = Math.floor(Date.now() / 1000);
const shellWin: Win = { session: 'scratch2', id: '@4', index: 1, active: false, command: 'zsh', activity: now - 3600 };
const eightWin: Win = { session: 'scratch2', id: '@7', index: 8, active: true, command: 'claude', activity: now - 1800, claude: trEight };
// The most recently active window is NOT its session's active one.
const elevenWin: Win = { session: 'scratch2', id: '@9', index: 11, active: false, command: 'claude', activity: now - 120, claude: trEleven };
const soloWin: Win = { session: 'solo', id: '@2', index: 3, active: true, command: 'opencode', activity: now - 900 };

function writeWindows(ws: Win[]) {
  writeFileAtomic(
    windowsFile,
    ws
      .map((w) => {
        const f: (string | number)[] = [w.session, w.id, w.index, w.active ? 1 : 0, w.command, w.activity, w.claude ?? ''];
        // Fields 8–10 (split transcript, cwd, colour) are empty; the 11th is the reason.
        if (w.noView) f.push('', '', '', w.noView);
        return f.join('|');
      })
      .join('\n') + '\n',
  );
}

const q = (w: Win) => `host=${encodeURIComponent(host)}&session=${encodeURIComponent(w.session)}&window=${encodeURIComponent(w.id)}`;
const viewPath = (w: Win) => '/mirror/view?' + q(w);
const termPath = (w: Win) => '/mirror/terminal?' + q(w);
// This host's rows in the sidebar's list, in its order; one row by the page it opens.
const cards = (page: Page) => page.locator(`#sidebar-agents-list a[data-nav-agent="mirror"][href*="host=${encodeURIComponent(host)}&"]`);
const cardOf = (page: Page, href: string) => page.locator(`#sidebar-agents-list a[data-nav-agent="mirror"][href="${href}"]`);
const names = (page: Page) => cards(page).evaluateAll((els) => els.map((e) => e.getAttribute('title')));
const screen = (page: Page) => page.locator('#term .xterm-rows');
const read = (f: string) => (fs.existsSync(f) ? fs.readFileSync(f, 'utf8') : '');

test.beforeAll(async () => {
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, windowsPort, {
    MUSTER_MIRROR: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  fs.writeFileSync(trEight, prompt('the prompt of window EIGHT'));
  fs.writeFileSync(trEleven, prompt('the prompt of window ELEVEN'));
  writeFileAtomic(path.join(dir, 'labels.json'), JSON.stringify({ scratch2: 'Gold' }));
  writeWindows([shellWin, eightWin, elevenWin, soloWin]);
  mirrorProc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_LABELS_FILE: path.join(dir, 'labels.json'),
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile,
      MUSTER_MIRROR_E2E_PASTE_FILE: pasteFile,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  mirrorProc.stdout?.on('data', (d) => (mirrorLog += d));
  mirrorProc.stderr?.on('data', (d) => (mirrorLog += d));
});

test.afterAll(async () => {
  if (mirrorProc && mirrorProc.exitCode === null) {
    const done = new Promise((r) => mirrorProc!.once('exit', r));
    mirrorProc.kill('SIGTERM');
    await done;
  }
  await server?.stop();
  fs.rmSync(dir, { recursive: true, force: true });
});

// login signs in (lib/signin.ts) and keeps the session for a context the test
// makes itself.
let signedIn: Awaited<ReturnType<BrowserContext['cookies']>> | undefined;
async function login(page: Page) {
  await signIn(page, origin);
  signedIn = await page.context().cookies(origin);
}

async function noHorizontalScroll(page: Page) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'document scrollWidth').toBeLessThanOrEqual(cw);
}

// openList opens the sidebar the way a person does, on a page of its own (the
// board) — on a phone its hamburger opens the drawer, whose list loads on that
// open; at a desktop width it is the rail — and waits for this host's rows.
// The partial is polled first, so the page opens on a list that already
// holds them.
async function openList(page: Page, want: number) {
  await login(page);
  await expect
    .poll(async () => (await (await page.request.get(origin + '/ui/nav/agents')).text()).split(`host=${encodeURIComponent(host)}&amp;`).length - 1, {
      message: `this host's rows (muster-mirror log:\n${mirrorLog})`,
      timeout: 20_000,
    })
    .toBe(want);
  await page.goto(origin + '/tasks');
  const open = page.locator('#sidebar-open');
  if (await open.isVisible()) await open.click();
  await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });
  await expect(cards(page), `this host's rows (muster-mirror log:\n${mirrorLog})`).toHaveCount(want, { timeout: 20_000 });
}

for (const vp of [
  { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });
    test.beforeEach(() => {
      writeWindows([shellWin, eightWin, elevenWin, soloWin]);
      fs.rmSync(pasteFile, { force: true });
      for (const t of [trEight, trEleven]) fs.rmSync(t + '.pasted', { force: true });
    });

    // 🔴 A REGRESSION TEST for "I only see a handful": on the commit before
    // per-window registration this host draws TWO entries (one per session),
    // so the count of four fails there — and the two windows that are not
    // their session's active one have no entry to open at all.
    test('every window is a row of its own, named <codename>:<index>, most recently active first', async ({ page }) => {
      await openList(page, 4);
      const eleven = cardOf(page, viewPath(elevenWin));
      const eight = cardOf(page, viewPath(eightWin));
      const shell = cardOf(page, termPath(shellWin));
      const solo = cardOf(page, termPath(soloWin));

      // Named by codename and the window's OWN index (the row's text, and
      // its title: the whole label); a session with no codename is
      // <session>:<index>.
      for (const [c, label] of [[eleven, 'Gold:11'], [eight, 'Gold:8'], [shell, 'Gold:1'], [solo, 'solo:3']] as const) {
        await expect(c).toHaveText(label);
        await expect(c).toHaveAttribute('title', label);
      }
      // 🔴 The raw tmux session name is NOT drawn on a labelled row (an
      // operator ask: the resolved title only) — not beside the name, not
      // anywhere in its text. The palette still finds the row by it, and by
      // the host's name (the row's search words).
      for (const c of [eleven, eight, shell]) {
        expect(await c.innerText(), 'no raw session name on the row').not.toContain('scratch2');
        expect((await c.getAttribute('data-switcher-words'))!.split(' '), 'the palette finds it by its tmux session').toContain('scratch2');
      }
      for (const c of [eleven, eight, shell, solo]) {
        expect((await c.getAttribute('data-switcher-words'))!.split(' ').pop(), 'the palette finds it by its host').toBe(host.toLowerCase());
      }
      // What a row opens: a Claude Code window its Rendered view, any other
      // window its terminal (the cardOf hrefs above, each found once).

      // One flat list, most recently active first — two windows of ONE session
      // sit either side of another session's.
      expect(await names(page)).toEqual(['Gold:11', 'solo:3', 'Gold:8', 'Gold:1']);

      await noHorizontalScroll(page);
      // ⚠ THE LIST REFRESHES ITSELF UNDER THE TEST (the agents' SSE events,
      // nav_agents.go's trigger), and each refresh swaps every row for a new
      // element. Any step that resolves a row and then acts on that element
      // in a SECOND call can catch it mid-swap: boundingBox read null
      // (muster#357's CI, run 41014171994, both attempts), and with the
      // box polled, scrollIntoViewIfNeeded run alone ahead of the poll threw
      // "Element is not attached to the DOM" (run 41093948598). So scrolling
      // and measuring are ONE retried block that finds the row again on each
      // try. The row is scrolled to first because its place in the list is
      // not this file's to fix: agents left by earlier specs in the same leg
      // sort above it when they are newer, below the drawer's fold. (The
      // other reads of a row above — innerText, getAttribute, evaluateAll,
      // the toHave* assertions — each resolve and read in one call.)
      for (const c of [eleven, solo]) {
        await expect(async () => {
          await c.scrollIntoViewIfNeeded({ timeout: 2_000 });
          const b = await c.boundingBox({ timeout: 2_000 });
          expect(b, 'the row has a box').not.toBeNull();
          expect(b!.x + b!.width, 'the row fits the viewport').toBeLessThanOrEqual(vp.use.viewport.width);
        }).toPass({ timeout: 15_000 });
      }

      // Live, with no reload: a window is renumbered (its row follows, same
      // element), and another closes (its row goes).
      const id = await eleven.getAttribute('id');
      writeWindows([shellWin, eightWin, { ...elevenWin, index: 2 }, soloWin]);
      await expect(eleven, `the row follows the renumbering (log:\n${mirrorLog})`).toHaveText('Gold:2', { timeout: 20_000 });
      expect(await eleven.getAttribute('id'), 'a renumbered window is the SAME row: its address is the window id, not the index').toBe(id);
      writeWindows([eightWin, { ...elevenWin, index: 2 }, soloWin]);
      await expect(cards(page)).toHaveCount(3, { timeout: 20_000 });
      await expect(shell).toHaveCount(0);
    });

    // The window that is NOT its session's active one: its rendered view is
    // ITS transcript, and a message sent from it reaches ITS pane.
    test('a non-active Claude Code window opens its own rendered view, and Send reaches that window', async ({ page }) => {
      await openList(page, 4);
      await cardOf(page, viewPath(elevenWin)).click();
      await page.waitForURL((u) => u.pathname === '/mirror/view');
      expect(new URL(page.url()).searchParams.get('window'), 'opened by the window id').toBe('@9');
      await expect(page.locator('header h1')).toContainText('Gold:11');
      await expect(page.locator('header h1 [data-mirror-session]')).toHaveText('scratch2');

      const log = page.locator('#mv-log');
      await expect(log.locator('[data-bubble="user"]').first()).toContainText('the prompt of window ELEVEN');
      await expect(log).not.toContainText('window EIGHT');

      await page.locator('#mv-input').fill('a message for window eleven');
      await page.locator('#mv-send').click();
      await expect(page.locator('#mv-input')).toHaveValue('');
      await expect.poll(() => read(trEleven + '.pasted'), { message: `pasted into window @9's pane (log:\n${mirrorLog})` }).toBe('%e2e-scratch2-@9 a message for window eleven\n');
      await expect(log.locator('[data-bubble="user"]').last()).toContainText('a message for window eleven');
      expect(read(trEight + '.pasted'), 'nothing reached the session’s ACTIVE window').toBe('');
      expect(read(pasteFile), 'nothing reached a non-Claude pane').toBe('');
      await expect(page.locator('#mv-note')).toBeHidden();

      // Its Terminal is this window's too.
      // (It opens in a frame over the view: view_swap.go.)
      await page.locator('#view-terminal').click();
      await page.waitForURL((u) => u.pathname === '/mirror/terminal');
      expect(new URL(page.url()).searchParams.get('window')).toBe('@9');
      await expect(page.frameLocator('iframe[data-muster-view]').locator('#term .xterm-rows')).toContainText('FAKE-MIRROR session=scratch2 window=@9 mode=rw');
      await noHorizontalScroll(page);
    });

    test('a non-active window that is not Claude Code opens its own terminal, and Send reaches that window', async ({ page }) => {
      await openList(page, 4);
      await cardOf(page, termPath(shellWin)).click();
      await page.waitForURL((u) => u.pathname === '/mirror/terminal');
      await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch2 window=@4 mode=rw');
      await expect(page.locator('#term-status')).toHaveText('Connected');
      await expect(page.locator('header h1')).toContainText('Gold:1');
      await expect(page.locator('#view-rendered'), 'a shell has no rendered view').toHaveCount(0);

      await page.locator('#mv-input').fill('ls -la');
      await page.locator('#mv-send').click();
      await expect(page.locator('#mv-input')).toHaveValue('');
      await expect.poll(() => read(pasteFile), { message: `pasted into window @4's pane (log:\n${mirrorLog})` }).toBe('%e2e-scratch2-@4 ls -la\n');
      for (const t of [trEight, trEleven]) expect(read(t + '.pasted'), 'nothing reached a Claude window').toBe('');
      await noHorizontalScroll(page);
    });

    // ⚠ THE LIST'S SEARCH BOX IS GONE (operator ask: the session switcher's
    // search palette supersedes it — agents-switcher.spec.ts), and with it
    // this spec's tests of its words, field tokens and ?q=. What they also
    // pinned about the LIST itself is kept here.

    // 🔴 THE LIST REFRESHES ITSELF and opens no socket per row: a window is
    // renumbered (a row's text changes) and a new one appears, and nothing
    // reloads. (The retired Agents list held an arrival behind an "N new ↑"
    // pill under a host tab; the sidebar has neither, and draws it at once.)
    test('the sidebar refreshes itself and opens no socket per row', async ({ page }) => {
      const sockets: string[] = [];
      page.on('websocket', (ws) => sockets.push(ws.url()));
      await openList(page, 4);
      const sorted = async () => (await names(page)).sort();
      expect(await sorted()).toEqual(['Gold:1', 'Gold:11', 'Gold:8', 'solo:3']);
      const marked = await page.evaluate(() => ((window as unknown as { __noReload: number }).__noReload = 1));
      expect(marked).toBe(1);
      const twelve: Win = { session: 'scratch2', id: '@12', index: 12, active: false, command: 'zsh', activity: now - 30 };
      writeWindows([shellWin, eightWin, { ...elevenWin, index: 2 }, soloWin, twelve]);
      await expect(cardOf(page, viewPath(elevenWin)), `the list refreshed (log:\n${mirrorLog})`).toHaveText('Gold:2', { timeout: 20_000 });
      await expect(cardOf(page, termPath(twelve))).toBeVisible({ timeout: 20_000 });
      expect(await sorted()).toEqual(['Gold:1', 'Gold:12', 'Gold:2', 'Gold:8', 'solo:3']);
      expect(await page.evaluate(() => (window as unknown as { __noReload?: number }).__noReload), 'no reload happened').toBe(1);

      // A hundred rows must not mean a hundred connections: the page opened
      // no socket at all for its mirrored rows. (Read here, before a row is
      // opened: its page has a socket of its own.) Nor any warm socket: the
      // board does not warm (web/static/mirror-snap.js warmable()).
      expect(sockets.filter((u) => u.includes('/mirror/')), 'no per-row socket').toEqual([]);
      await noHorizontalScroll(page);
    });

    // (A test here pinned the retired Agents list's HOST TABS — All, "cluster",
    // this host, each with its count, kept across a reload. The tabs are gone:
    // the palette finds an entry by its host's name instead, pinned above on
    // each row's search words and by switcher-search.spec.ts.)

    // THE COMPOSER WITHOUT ITS SCRIPT. A browser context with JavaScript OFF: the
    // page's composer script does not exist, so what is left is the form itself.
    //
    // 🔴 A REGRESSION TEST: on the commit before this the composer was a form with
    // no method and no action — a browser submitting it GETs the page it is on
    // with `?text=…`, and nothing is sent.
    test('with no script at all, a message is typed, submitted by the browser, delivered, and the page comes back saying so', async ({ page, browser }) => {
      await login(page);
      await expect.poll(async () => (await page.request.get(origin + '/ui/nav/agents')).text(), { timeout: 20_000 }).toContain(`host=${encodeURIComponent(host)}&amp;`);
      const ctx = await browser.newContext({ ...vp.use, javaScriptEnabled: false });
      await ctx.addCookies(signedIn!);
      const plain = await ctx.newPage();
      try {
        // The Terminal view of a shell window (not its session's active one).
        await plain.goto(origin + termPath(shellWin));
        // The control that this page ran no script: the page's own script is
        // what builds the terminal, and there is none. (page.evaluate is no
        // witness — it runs in a context with script disabled all the same.)
        await expect(plain.locator('#mv-input')).toBeVisible();
        await expect(plain.locator('#term .xterm-rows'), 'this page ran no script').toHaveCount(0);
        await plain.locator('#mv-input').fill('ls -la /srv');
        await plain.locator('#mv-send').click();
        await plain.waitForURL((u) => u.pathname === '/mirror/terminal' && u.searchParams.get('notice') === 'sent', { timeout: 20_000 });
        expect(new URL(plain.url()).searchParams.get('window'), 'back on the same window').toBe('@4');
        await expect(plain.locator('#mv-note')).toBeVisible();
        await expect(plain.locator('#mv-note')).toHaveText('Sent.');
        expect(read(pasteFile)).toBe('%e2e-scratch2-@4 ls -la /srv\n');

        // The Rendered view of a Claude Code window: the same form, the Claude pane.
        await plain.goto(origin + viewPath(elevenWin));
        await plain.locator('#mv-input').fill('continue, please');
        await plain.locator('#mv-send').click();
        await plain.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('notice') === 'sent', { timeout: 20_000 });
        await expect(plain.locator('#mv-note')).toHaveText('Sent.');
        expect(read(trEleven + '.pasted')).toBe('%e2e-scratch2-@9 continue, please\n');
        expect(read(trEight + '.pasted'), 'nothing reached the active window').toBe('');

        // Nothing typed: said on the page it comes back to, and nothing is sent.
        await plain.locator('#mv-send').click();
        await plain.waitForURL((u) => u.searchParams.get('notice') === 'empty');
        await expect(plain.locator('#mv-note')).toHaveText('Type a message first.');
        expect(read(trEleven + '.pasted')).toBe('%e2e-scratch2-@9 continue, please\n');
        expect(read(pasteFile)).toBe('%e2e-scratch2-@4 ls -la /srv\n');
      } finally {
        await ctx.close();
      }
    });

    // THE COMPOSER WHEN ITS SCRIPT'S WAY FAILS. Send must never be a silent
    // no-op: when the fetch gets no answer the browser submits the form itself.
    test('a send whose request gets no answer is handed to the browser, and a message is never delivered twice', async ({ page }) => {
      await openList(page, 4);
      await page.goto(origin + termPath(shellWin));
      await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch2 window=@4');
      const isFetch = (h: Record<string, string>) => h['x-requested-with'] === 'XMLHttpRequest';

      // 1. The request never reaches muster (a sign-in redirect a fetch cannot
      // follow, a proxy's error): the script's fetch is aborted, the browser's
      // own submission is let through. Delivered once, and the page says so.
      let aborted = 0;
      await page.route('**/mirror/terminal/send?*', async (route) => {
        if (isFetch(route.request().headers())) {
          aborted++;
          await route.abort('failed');
          return;
        }
        await route.continue();
      });
      await page.locator('#mv-input').fill('echo first');
      // Armed BEFORE the click: the page it lands on takes ?notice= out of its
      // address as soon as its script runs, so a wait started after the click
      // can miss it and wait for a navigation that never comes (the same race
      // as agent-composer.spec.ts's form send).
      await Promise.all([page.waitForURL((u) => u.searchParams.get('notice') === 'sent'), page.locator('#mv-send').click()]);
      await expect(page.locator('#mv-note')).toHaveText('Sent.');
      expect(aborted, 'the script tried its own way first').toBe(1);
      expect(read(pasteFile)).toBe('%e2e-scratch2-@4 echo first\n');
      // The notice is taken out of the address once shown: a reload does not repeat it.
      await expect.poll(() => new URL(page.url()).searchParams.get('notice')).toBeNull();
      await page.unroute('**/mirror/terminal/send?*');

      // 2. The request REACHES muster and is delivered, and only the answer is
      // lost. The browser posts the form again — the same message — and it must
      // not be pasted a second time.
      let delivered = 0;
      await page.route('**/mirror/terminal/send?*', async (route) => {
        if (isFetch(route.request().headers())) {
          const res = await route.fetch();
          expect(res.status(), 'the first post was delivered').toBe(204);
          delivered++;
          await route.abort('failed'); // …and the page never hears
          return;
        }
        await route.continue();
      });
      await page.locator('#mv-input').fill('echo second');
      await Promise.all([page.waitForURL((u) => u.searchParams.get('notice') === 'sent'), page.locator('#mv-send').click()]);
      expect(delivered).toBe(1);
      expect(read(pasteFile), 'the message whose answer was lost was delivered ONCE').toBe('%e2e-scratch2-@4 echo first\n%e2e-scratch2-@4 echo second\n');
      await page.unroute('**/mirror/terminal/send?*');

      // 3. Control: with nothing in the way the script sends without leaving the
      // page, and the NEXT message is a new message (its nonce was renewed).
      const before = page.url();
      await page.locator('#mv-input').fill('echo third');
      await page.locator('#mv-send').click();
      await expect(page.locator('#mv-input')).toHaveValue('');
      await page.locator('#mv-input').fill('echo third');
      await page.locator('#mv-send').click();
      await expect.poll(() => read(pasteFile)).toBe(
        '%e2e-scratch2-@4 echo first\n%e2e-scratch2-@4 echo second\n%e2e-scratch2-@4 echo third\n%e2e-scratch2-@4 echo third\n',
      );
      // The send's answer empties the box; typing before it has would be wiped.
      await expect(page.locator('#mv-input')).toHaveValue('');
      expect(page.url(), 'the script sends without navigating').toBe(before);

      // 3b. Text typed while a send is still out is not thrown away when its
      // answer arrives.
      let release: () => void = () => {};
      const held = new Promise<void>((r) => (release = r));
      await page.route('**/mirror/terminal/send?*', async (route) => {
        await held;
        await route.continue();
      });
      await page.locator('#mv-input').fill('echo fourth');
      await page.locator('#mv-send').click();
      await expect(page.locator('#mv-send')).toBeDisabled();
      await page.locator('#mv-input').fill('typed meanwhile');
      release();
      await expect(page.locator('#mv-send')).toBeEnabled();
      expect(read(pasteFile)).toContain('%e2e-scratch2-@4 echo fourth\n');
      await expect(page.locator('#mv-input')).toHaveValue('typed meanwhile');
      await page.unroute('**/mirror/terminal/send?*');

      // 4. Offline is the one failure the script keeps: it says so and keeps the text.
      await page.context().setOffline(true);
      await page.locator('#mv-input').fill('while offline');
      await page.locator('#mv-send').click();
      await expect(page.locator('#mv-note')).toHaveText('Not sent: you are offline.');
      await expect(page.locator('#mv-input')).toHaveValue('while offline');
      expect(page.url()).toBe(before);
      await page.context().setOffline(false);
    });

    // THE UPDATE TOAST AND THE COMPOSER. "muster has been updated — Reload" is a
    // fixed overlay at the TOP of every page (it was near the bottom until the
    // operator reported it blocking the UI); it must never sit on the message
    // box or on Send.
    test('the update toast does not cover the composer', async ({ page }) => {
      await openList(page, 4);
      for (const p of [termPath(shellWin), viewPath(elevenWin)]) {
        await page.goto(origin + p);
        await expect(page.locator('#mv-send')).toBeVisible();
        const boxes = await page.evaluate(() => {
          const toast = document.getElementById('sw-update-toast')!;
          toast.hidden = false;
          const r = (e: Element) => e.getBoundingClientRect();
          const t = r(toast);
          const over = (e: Element) => {
            const b = r(e);
            return !(t.right <= b.left || t.left >= b.right || t.bottom <= b.top || t.top >= b.bottom);
          };
          return { height: t.height, input: over(document.getElementById('mv-input')!), send: over(document.getElementById('mv-send')!) };
        });
        expect(boxes.height, 'CONTROL: the toast was really laid out').toBeGreaterThan(20);
        expect(boxes, `on ${p}`).toMatchObject({ input: false, send: false });
      }
    });

    // 🔴 A WINDOW THAT OPENS IN THE TERMINAL SAYS WHY, in one line under the
    // header, instead of silently falling back — the silent fallback read as
    // "muster only has a terminal" to a person whose Claude Code window had no
    // transcript yet. The shell window says no Claude Code was found (red at
    // origin/main: no line at all); a Claude Code window whose host says why
    // it has no view says the host's reason in its Terminal view, which its
    // sidebar row opens; a window WITH a rendered view draws the switch and no
    // line.
    // It lists a fifth window; the next beforeEach removes it again.
    test('a terminal-only window says why in one line; a window with a rendered view does not', async ({ page }) => {
      const why = 'Claude Code has not written this conversation yet. The Rendered view appears after its first message.';
      const freshWin: Win = { session: 'fresh', id: '@5', index: 1, active: true, command: 'claude', activity: now - 60, noView: why };
      const vw = page.viewportSize()!.width;
      const lineFits = async () => {
        const box = await page.locator('#term-only').boundingBox();
        expect(box, 'the terminal-only line is drawn').not.toBeNull();
        expect(box!.x).toBeGreaterThanOrEqual(0);
        expect(box!.x + box!.width).toBeLessThanOrEqual(vw + 0.5);
        await noHorizontalScroll(page);
      };

      // The shell window: no Claude Code here.
      await openList(page, 4);
      await page.goto(origin + termPath(shellWin));
      await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch2 window=@4');
      await expect(page.locator('#term-only')).toHaveText('Terminal only. muster found no Claude Code conversation in this window.');
      await expect(page.locator('#view-rendered')).toHaveCount(0);
      await lineFits();

      // Control: a Claude Code window has its rendered view — the switch, no line.
      await page.goto(origin + termPath(eightWin));
      await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch2 window=@7');
      await expect(page.locator('#view-rendered')).toBeVisible();
      await expect(page.locator('#term-only')).toHaveCount(0);

      // A Claude Code window with no transcript yet: its row opens its
      // Terminal view, which gives the host's reason.
      writeWindows([shellWin, eightWin, elevenWin, soloWin, freshWin]);
      await openList(page, 5);
      const card = cardOf(page, termPath(freshWin));
      await expect(card).toBeVisible();
      await card.click();
      await page.waitForURL((u) => u.pathname === '/mirror/terminal' && u.searchParams.get('session') === 'fresh');
      await expect(screen(page)).toContainText('FAKE-MIRROR session=fresh window=@5');
      await expect(page.locator('#term-only')).toHaveText('Terminal only. ' + why);
      await expect(page.locator('#term-only [data-terminal-only-reason]')).toHaveText(why);
      await expect(page.locator('#view-rendered')).toHaveCount(0);
      await lineFits();
    });

    // The URLs that existed before windows were addressable.
    test('an old link naming only the session lands on its active window', async ({ page }) => {
      await login(page);
      const old = `host=${encodeURIComponent(host)}&session=scratch2`;
      await expect
        .poll(async () => (await page.request.get(origin + '/ui/nav/agents')).text(), { timeout: 20_000 })
        .toContain(`host=${encodeURIComponent(host)}&amp;`);
      await page.goto(origin + '/mirror/view?' + old);
      await page.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('window') === '@7');
      await expect(page.locator('header h1')).toContainText('Gold:8');
      await expect(page.locator('#mv-log [data-bubble="user"]').first()).toContainText('the prompt of window EIGHT');
      await page.goto(origin + '/mirror/terminal?' + old);
      await page.waitForURL((u) => u.pathname === '/mirror/terminal' && u.searchParams.get('window') === '@7');
      await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch2 window=@7 mode=rw');
    });
  });
}
