import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Browser, Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { binDir, dsn, hookToken, listUxPort, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { mkchat } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';
import { agentLink } from '../lib/agent-link';

// THE AGENTS LIST UX BATCH, WHERE IT IS NOW (the Agents list page is retired:
// its entries are the sidebar's rows, and the session switcher's bar is on
// every session page), in a real browser at a phone (390×844, touch) and a
// desktop (1280×800), against a real muster-server (its own port, the e2eterm
// binary, mirror plugin on) and a real muster-mirror over the tunnel (the
// `-tags e2emirror` fake plays tmux only):
//
//   1. ONE LIST, FLAT ACROSS HOSTS: the sidebar draws whoever waits on the
//      operator first, then the rest most recently active first — mirrored
//      windows and dispatched agents interleaved by activity, each with its
//      host's icon.
//   2. A RECENCY DOT on the operator's tmux idle-fade scale, one colour per
//      bucket (bright green, aqua, orange, grey), on each row.
//   3. A LONG TITLE keeps its row one line, whole in its tooltip.
//   4. WAITING ON YOU: an agent's attention_ask, a Claude window stopped at a
//      prompt, a held AskUserQuestion registered by the real `muster-mirror
//      ask` hook — and NOT an agent with only a reply draft. The sidebar marks
//      exactly those, and the bar's Waiting view holds exactly those. (The
//      list's preset chips — Waiting on you, Idle >15m / >1h / >1d — are gone
//      with it; the sidebar's Idle group is agents-idle.spec.ts.)
//   5. MARK COMPLETE on an agent's page goes back OUT of the agent, with
//      JavaScript and without it.

const origin = `http://localhost:${listUxPort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-lux-' + stamp;
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lux-'));
let server: Running | undefined;
let token = '';
const procs: ChildProcess[] = [];
let mirrorLog = '';

// The two agents on the list: one fresh with a LONG name, one three days idle.
const freshAgent = 'lux-fresh-' + stamp;
const oldAgent = 'lux-old-' + stamp;
// An agent waiting on an attention_ask, and one whose only attention item is a
// reply draft (which waits on nothing).
const askAgent = 'lux-ask-' + stamp;
const draftAgent = 'lux-draft-' + stamp;
const draftPlugin = 'lux-draft-' + stamp;
const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();
const LONG = 'Untangle the reconnect backoff so a flapping tunnel stops resetting its retry window every time';
// The old agent's focus title, past the same clamp.
const FOCUS = 'Sweeping the stale notes into the archive bucket and re-linking every task that still points at them';
// The agents whose task each test completes: one per viewport and path.
const doneAgent = (vp: string, js: boolean) => `lux-done-${vp}-${js ? 'js' : 'nojs'}-${stamp}`;
const doneTask = (vp: string, js: boolean) => `Lux task ${vp} ${js ? 'js' : 'nojs'} ${stamp}`;
const VPS = ['phone', 'desktop'];

// The windows: Claude windows luxnow (1 min, waiting at a prompt) and luxq (45
// min, holding an AskUserQuestion), and shells luxorange (3 h) and luxancient
// (2 days). session|id|index|active|command|activity|transcript
const now = Math.floor(Date.now() / 1000);
const sidNow = 'luxnow-' + stamp;
const sidQ = 'luxq-' + stamp;
const trNow = path.join(dir, sidNow + '.jsonl');
const trQ = path.join(dir, sidQ + '.jsonl');
const prompt = (t: string) => JSON.stringify({ type: 'user', promptId: 'p', origin: { kind: 'human' }, message: { role: 'user', content: t } }) + '\n';

// The expected relative order of this spec's cards, newest first.
const ORDER = ['luxnow', askAgent, draftAgent, freshAgent, 'luxq', 'luxorange', 'luxancient', oldAgent];
// What the "Waiting on you" chip shows: the window at a prompt, the asking
// agent and the window holding a question — not the draft's agent.
const WAITING = ['luxnow', askAgent, 'luxq'];

const question = {
  question: 'Which region should the replica go to?',
  header: 'Region',
  options: [
    { label: 'North', description: 'Closer to the users' },
    { label: 'South', description: 'Cheaper' },
  ],
  multiSelect: false,
};

test.beforeAll(async () => {
  test.setTimeout(240_000);
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, listUxPort, {
    MUSTER_MIRROR: '1',
    MUSTER_FOCUS_INTERVAL: '0',
    // Reply drafts are an attention item only with the plugin interface on.
    MUSTER_PLUGINS: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  expect(LONG.length, 'the fixture title is past the 60-character clamp').toBeGreaterThan(60);

  mkchat(freshAgent, [{ role: 'user', content: 'Fix the tunnel backoff.' }], { MUSTER_E2E_DISPLAY_NAME: LONG, MUSTER_E2E_ACTIVE_MINUTES_AGO: '5' });
  mkchat(oldAgent, [{ role: 'user', content: 'Tidy the notes.' }], {
    MUSTER_E2E_ACTIVE_MINUTES_AGO: String(3 * 24 * 60),
    MUSTER_E2E_FOCUS: JSON.stringify([{ subject: 'self', title: FOCUS, description: 'Two buckets left.' }]),
  });
  mkchat(askAgent, [{ role: 'user', content: 'Roll the fleet?' }], { MUSTER_E2E_ACTIVE_MINUTES_AGO: '2' });
  mkchat(draftAgent, [{ role: 'user', content: 'Answer the mail.' }], { MUSTER_E2E_ACTIVE_MINUTES_AGO: '3' });
  fix('ask', '1', askAgent, 'Roll the fleet now?');
  fix('draft', '1', draftAgent, draftPlugin);
  for (const vp of VPS) {
    for (const js of [true, false]) {
      mkchat(doneAgent(vp, js), [{ role: 'user', content: 'Ship the fix.' }], {
        MUSTER_E2E_TASK: doneTask(vp, js),
        MUSTER_E2E_ACTIVE_MINUTES_AGO: String(5 * 24 * 60),
      });
    }
  }

  fs.writeFileSync(trNow, prompt('roll the fleet'));
  writeFileAtomic(trNow + '.status', JSON.stringify({ status: 'waiting', statusUpdatedAt: Date.now() - 30_000 }));
  fs.writeFileSync(trQ, prompt('set the replica up'));
  writeFileAtomic(trQ + '.status', JSON.stringify({ status: 'idle', statusUpdatedAt: Date.now() - 60_000 }));
  const windows = path.join(dir, 'windows');
  writeFileAtomic(
    windows,
    [
      `luxnow|@1|1|1|claude|${now - 60}|${trNow}`,
      `luxq|@2|1|1|claude|${now - 45 * 60}|${trQ}`,
      `luxorange|@3|1|1|zsh|${now - 3 * 3600}|`,
      `luxancient|@4|1|1|zsh|${now - 2 * 86400}|`,
    ].join('\n') + '\n',
  );
  const p = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
      MUSTER_MIRROR_E2E_REFRESH: '300ms',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  p.stdout?.on('data', (d) => (mirrorLog += d));
  p.stderr?.on('data', (d) => (mirrorLog += d));
  procs.push(p);
});

// askQuestion registers luxq's AskUserQuestion through the REAL hook, as
// Claude Code would run it (the payload on stdin, the host's token).
async function askQuestion() {
  writeFileAtomic(trQ + '.picker.json', JSON.stringify({ toolUseId: 'toolu_lux_' + stamp, questions: [question] }));
  const h = spawn(mirrorBin, ['ask'], {
    env: { ...process.env, MUSTER_MIRROR_URL: origin, MUSTER_MIRROR_ALLOW_HTTP: '1', MUSTER_MIRROR_TOKEN: token },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  procs.push(h);
  let err = '';
  h.stderr?.on('data', (d) => (err += d));
  h.stdin?.end(
    JSON.stringify({
      session_id: sidQ,
      transcript_path: trQ,
      cwd: dir,
      permission_mode: 'default',
      hook_event_name: 'PreToolUse',
      tool_name: 'AskUserQuestion',
      tool_input: { questions: [question] },
      tool_use_id: 'toolu_lux_' + stamp,
    }),
  );
  const code = await new Promise((r) => h.once('exit', r));
  expect(code, `the ask hook ran (${err}; host log:\n${mirrorLog})`).toBe(0);
}
let asked = false;

test.afterAll(async () => {
  for (const p of procs) {
    if (p.exitCode === null && p.signalCode === null) {
      const done = new Promise((r) => p.once('exit', r));
      p.kill('SIGTERM');
      await done;
    }
  }
  await server?.stop();
  mkchat(freshAgent, null);
  mkchat(oldAgent, null);
  fix('rmplugin', '1', draftPlugin);
  mkchat(askAgent, null);
  mkchat(draftAgent, null);
  for (const vp of VPS) for (const js of [true, false]) mkchat(doneAgent(vp, js), null, { MUSTER_E2E_TASK: doneTask(vp, js) });
  fs.rmSync(dir, { recursive: true, force: true });
});

test.use({ storageState: { cookies: [], origins: [] } });

// row is one of this spec's sidebar rows: a window by its tmux session, an
// agent by its page.
function row(page: Page, key: string): Locator {
  if (key.startsWith('lux-')) return page.locator(`#sidebar-agents-list a[data-nav-agent="agent"]:is(${agentLink(key).replace(/a\[/g, '[')})`);
  return page.locator(`#sidebar-agents-list a[data-nav-agent="mirror"][href*="host=${host}&session=${key}&"]`);
}

// rowKey names a sidebar row the way row() finds it ('' for another spec's).
const rowKeysJS = (els: Element[], h: string) =>
  els.map((a) => {
    const href = a.getAttribute('href') || '';
    if (a.getAttribute('data-nav-agent') === 'agent') return href.split('?')[0].slice('/agents/'.length);
    const q = new URLSearchParams(href.replace(/^[^?]*\?/, ''));
    return q.get('host') === h ? q.get('session') || '' : '';
  });

// The sidebar's order (navOrder): whoever waits on the operator first — the
// window at a prompt, the asking agent, the window holding a question, by
// activity among themselves — then the rest by activity, windows and agents
// interleaved. The reply draft waits on nothing.
const NAV_ORDER = ['luxnow', askAgent, 'luxq', draftAgent, freshAgent, 'luxorange', 'luxancient', oldAgent];

// openList signs in, opens a page with the sidebar and shows its list (the
// drawer opened on a phone), and waits until every row of this spec is drawn.
async function openList(page: Page, at = '/account') {
  await signIn(page, origin);
  if (!asked) {
    await askQuestion();
    asked = true;
  }
  await page.goto(origin + at);
  if ((page.viewportSize()?.width ?? 0) < 1024) {
    await page.locator('#sidebar-open').click();
    await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });
  }
  await expect
    .poll(
      async () => {
        let n = 0;
        for (const k of NAV_ORDER) n += await row(page, k).count();
        if (n < NAV_ORDER.length) await page.evaluate(() => document.body.dispatchEvent(new Event('muster:resync', { bubbles: true })));
        return n;
      },
      { timeout: 30_000, message: `every row of the spec is listed (log:\n${mirrorLog})` },
    )
    .toBe(NAV_ORDER.length);
}

// order is this spec's rows in the order the sidebar draws them.
async function order(page: Page): Promise<string[]> {
  const keys = await page.locator('#sidebar-agents-list a[data-nav-agent]').evaluateAll(rowKeysJS, host);
  return keys.filter((k) => NAV_ORDER.includes(k));
}

const dotOf = (c: Locator) => c.locator('[data-recency]');

for (const vp of [
  { name: 'phone', title: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', title: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.title, () => {
    test.use(vp.use);
    const press = (l: Locator) => (vp.use.hasTouch ? l.tap() : l.click());

    test('the sidebar: who waits first, then the rest by activity, windows and agents interleaved; each with its host', async ({ page }) => {
      await openList(page);
      await expect.poll(() => order(page)).toEqual(NAV_ORDER);
      // The waiting ones are marked; the draft's agent is not.
      for (const k of WAITING) await expect(row(page, k), `${k} waits`).toHaveAttribute('data-nav-wait', '');
      await expect(row(page, draftAgent)).not.toHaveAttribute('data-nav-wait', '');
      // The host is still on each row: the window's host icon (its host in the
      // tooltip), the agent's cluster mark.
      await expect(row(page, 'luxorange').locator('[data-host-icon]')).toHaveAttribute('title', 'Host: ' + host);
      await expect(row(page, freshAgent).locator('[data-host-icon]').first()).toBeVisible();
    });

    test('each row draws a recency dot in its tmux idle-fade colour', async ({ page }) => {
      await openList(page);
      const want: Record<string, [string, string]> = {
        luxnow: ['0', 'rgb(184, 187, 38)'],
        [freshAgent]: ['0', 'rgb(184, 187, 38)'],
        luxq: ['2', 'rgb(104, 157, 106)'],
        luxorange: ['4', 'rgb(214, 93, 14)'],
        luxancient: ['7', 'rgb(102, 92, 84)'],
        [oldAgent]: ['7', 'rgb(102, 92, 84)'],
      };
      for (const [k, [bucket, colour]] of Object.entries(want)) {
        const dot = dotOf(row(page, k));
        await expect(dot, k).toHaveAttribute('data-recency', bucket);
        await expect(dot, k).toHaveCSS('background-color', colour);
        const box = (await dot.boundingBox())!;
        expect(Math.round(box.width), `${k}: the dot is 8px`).toBe(8);
      }
    });

    // The card's clamp and its "…" popover went with the list; a row is one
    // line that clips, its whole title in its tooltip and its accessible text,
    // and it never widens the sidebar or the page. A tap opens the agent.
    test("a long title keeps one line on its row, whole in its tooltip; a tap opens the agent", async ({ page }) => {
      await openList(page);
      const r = row(page, freshAgent);
      await expect(r).toHaveAttribute('title', LONG);
      await expect(r).toContainText(LONG);
      const box = (await r.boundingBox())!;
      expect(box.height, 'one 44px row').toBeLessThan(60);
      expect(box.height).toBeGreaterThanOrEqual(44);
      const clipped = await r.evaluate((a) => {
        const t = [...a.querySelectorAll('span')].find((s) => s.classList.contains('truncate')) as HTMLElement;
        return t.scrollWidth > t.clientWidth;
      });
      expect(clipped, 'instrument check: the title is long enough to clip here').toBe(true);
      const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
      expect(sw).toBeLessThanOrEqual(cw);
      const panel = await page.evaluate(() => (document.getElementById('sidebar') as HTMLElement).scrollWidth - (document.getElementById('sidebar') as HTMLElement).clientWidth);
      expect(panel, 'the sidebar does not scroll sideways').toBeLessThanOrEqual(0);
      await Promise.all([page.waitForURL((u) => u.pathname === `/agents/${freshAgent}`), press(r)]);
    });

    // THE BOTTOM BAR'S WAITING VIEW (operator ask: a view "that uses the same
    // filter as the waiting for you filter"): picked from the bar's view menu,
    // its row holds EXACTLY the entries the sidebar marks waiting — a session
    // at a prompt, an ask, a picker; not the reply draft — in the sidebar's
    // order. (The list's "Waiting on you" chip this was compared against is
    // retired; the sidebar's mark reads the same predicate, listEntry.waiting.)
    // Compared whole, every marked row against every pill, so a seeded entry
    // would count on both sides. Read on a session page (an agent's chat) that
    // is not itself waiting.
    test("the bottom bar's Waiting view holds exactly what the sidebar marks waiting", async ({ page }) => {
      await openList(page, `/agents/${oldAgent}`);
      // A link, normalised: an agent's page without its query.
      const key = (h: string) => (h.startsWith('/agents/') ? h.split('?')[0] : h);
      const marked = (
        await page.locator('#sidebar-agents-list a[data-nav-agent][data-nav-wait]').evaluateAll((els) => els.map((a) => a.getAttribute('href') || ''))
      ).map(key);
      expect(marked.length, "instrument check: the sidebar marks the spec's waiting rows").toBeGreaterThanOrEqual(WAITING.length);
      if (vp.use.viewport.width < 1024) await page.locator('#sidebar-close').click();
      // The view, from the bar's menu.
      await press(page.locator('#switcher-toggle'));
      const pick = page.locator('#switcher-views [data-switcher-pick="waiting"]');
      await expect(pick).toContainText('Waiting on you');
      await press(pick);
      await expect(page.locator('#switcher-toggle')).toHaveText('Waiting');
      const viewRow = page.locator('#session-switcher [data-switcher-list="waiting"]');
      await expect(viewRow).toBeVisible();
      const pills = (await viewRow.locator('a[data-switcher-entry]').evaluateAll((els) => els.map((e) => e.getAttribute('href') || ''))).map(key);
      expect(pills, "the Waiting view is the sidebar's waiting set, in its order").toEqual(marked);
      await expect(page.locator('#switcher-views [data-switcher-count="waiting"]')).toHaveText(String(marked.length));
      // The draft's agent is listed but not waiting: in neither.
      expect(pills.some((h) => h.includes(draftAgent)), 'the reply draft is not waiting').toBe(false);
      await page.evaluate(() => { try { localStorage.removeItem('muster.switcher.view'); } catch (e) {} });
    });

    // MARK COMPLETE LEAVES THE AGENT (operator ask: "after marking a task
    // complete, it should just redirect back out of the agent instead of to
    // another page"). It went to the Agents list; that page is retired, and
    // home (`/`) now opens a session — which must not be the agent just left.
    test('Mark complete on an agent page goes back out of the agent (with JavaScript)', async ({ page }) => {
      await signIn(page, origin);
      await page.goto(origin + `/agents/${doneAgent(vp.name, true)}`);
      const btn = page.getByRole('button', { name: 'Mark complete' });
      await expect(btn).toBeVisible({ timeout: 20_000 });
      await Promise.all([page.waitForURL((u) => u.pathname !== `/agents/${doneAgent(vp.name, true)}`, { timeout: 20_000 }), press(btn)]);
      await expectTaskComplete(doneTask(vp.name, true));
    });

    test('Mark complete on an agent page goes back out of the agent (without JavaScript)', async ({ browser }) => {
      await noJS(browser, vp.use, async (page) => {
        await page.goto(origin + `/agents/${doneAgent(vp.name, false)}`);
        const btn = page.getByRole('button', { name: 'Mark complete' });
        await expect(btn).toBeVisible();
        await Promise.all([page.waitForURL((u) => u.pathname !== `/agents/${doneAgent(vp.name, false)}`, { timeout: 15_000 }), btn.click()]);
      });
      await expectTaskComplete(doneTask(vp.name, false));
    });
  });
}

// noJS runs fn in a signed-in context with JavaScript OFF.
async function noJS(browser: Browser, use: object, fn: (page: Page) => Promise<void>) {
  const ctx = await browser.newContext({ ...use, javaScriptEnabled: false, baseURL: origin });
  try {
    const p = await ctx.newPage();
    await signIn(p, origin);
    await fn(p);
  } finally {
    await ctx.close();
  }
}

// expectTaskComplete reads the task back through the machine API.
async function expectTaskComplete(title: string) {
  const r = await fetch(origin + '/api/tasks', { headers: { Authorization: `Bearer ${hookToken}` } });
  expect(r.status).toBe(200);
  const tasks = (await r.json()) as { title: string; status: string }[];
  const t = tasks.find((x) => x.title === title);
  expect(t, `the task ${title} exists`).toBeTruthy();
  expect(t!.status).toBe('complete');
}
