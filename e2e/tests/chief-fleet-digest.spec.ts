import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { chiefFleetConnectPort, chiefFleetFakePort, chiefFleetGitHubPort, chiefFleetPort, dsn, repoRoot } from '../lib/env';
import { MirrorRig, cc } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';

// THE FLEET DIGEST CARD (docs/chief.md "Fleet digest"), on the operator's
// click path, at a phone (390×844, touch) and a desktop (1280×800):
//
//   - the card is pinned at the top of the chief panel, ABOVE the chat that is
//     open, and stays there when the chat under it changes;
//   - its groups come from muster's own state of each session: a prompt on
//     screen and the chief's own open card are Needs you; a stall with its pull
//     request open is Waiting on checks; one whose pull request merged is Done; a
//     stall with none is Stalled; a busy window is Working;
//   - desktop: a group opens inline; a phone: one row of chips, a chip opens a
//     bottom sheet;
//   - a session's name opens its drill-in, whose Open session goes to that
//     session's rendered view;
//   - "Ask the chief about this" opens the Fleet side chat with a draft in the
//     box and SENDS NOTHING: the fake runtime hears no turn, and the chat holds
//     no message.
//
// ⚠ WHAT IS PLAYED: tmux (the e2emirror host's windows file, each Claude window
// a transcript and a status file written as Claude Code writes them), GitHub
// (a fake App answering two pull requests), and the model (a fake runtime that
// counts what it is asked — it must be asked nothing), and the three idle
// windows' STOPS: the session-state pass (on, MUSTER_SESSION_STATE=1) opens a
// stop only after a window has been idle for the stall threshold, ten minutes
// with the chief's nudges off, so the fixture writes them as the pass would
// (e2e/pluginfix episode) and the pass then keeps them. Which group a stop
// lands in — its pull request's state from the fake GitHub, the prompt on
// screen read live — is muster's own. Every value is invented fixture data.

const stamp = Date.now().toString(36);
const chiefName = 'e2e-fleet-chief-' + stamp;
const chiefToken = 'e2e-fleet-chief-token-not-a-real-one-' + stamp;
const REPO = 'e2e-acme/widgets';
const OPEN_PR = 7;
const MERGED_PR = 8;
const MERGED_HEAD = 'a1b2c3d4e5f60718293a4b5c6d7e8f9012345678';
const CARD_Q = 'Ship the widget release tonight? ' + stamp;

// One Claude session per window, each named for its work (never a codename).
const sid = (n: number) => `fleet${stamp}x${n}`.replace(/[^0-9A-Za-z]/g, '');
const W = {
  waiting: { sid: sid(1), win: '@2', label: 'release:1', ask: 'Which schema should the migration use?' },
  ci: { sid: sid(2), win: '@3', label: 'parser:2', reply: 'Opened the parser pull request; CI is running.' },
  done: { sid: sid(3), win: '@4', label: 'loader:3', reply: 'The loader fix is merged.' },
  done2: { sid: sid(6), win: '@7', label: 'cache:6', reply: 'The cache fix is merged.' },
  stalled: { sid: sid(4), win: '@5', label: 'docs:4', reply: 'Drafted the docs outline.' },
  working: { sid: sid(5), win: '@6', label: 'tests:5', reply: 'Running the test suite now.' },
};

const APP_ID = '424249';
const APP_ACCOUNT = { login: 'e2e-acme', id: 4349, type: 'User' };
const APP_KEY = crypto.generateKeyPairSync('rsa', { modulusLength: 2048 }).privateKey.export({ type: 'pkcs1', format: 'pem' }).toString();

const rig = new MirrorRig(chiefFleetPort, 'e2e-cfleet', {
  MUSTER_FOCUS_INTERVAL: '0',
  MUSTER_AGENT_GATEWAY: 'hooks-sha256',
  MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
  MUSTER_AGENT_RUNTIME_CONFIG: '{}',
  MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
  MUSTER_AGENT_KINDS: 'gateway',
  MUSTER_AGENT_OPENROUTER_KEY: 'sk-or-v1-FAKE-e2e-not-a-real-key',
  MUSTER_E2E_AGENT_ENDPOINT: `http://127.0.0.1:${chiefFleetFakePort}`,
  MUSTER_E2E_SUGGEST_URL: `http://127.0.0.1:${chiefFleetFakePort}/v1/chat/completions`,
  // Session state: each window's stop is an episode (the digest's states).
  MUSTER_SESSION_STATE: '1',
  // The digest itself is opt-in (MUSTER_FLEET_DIGEST).
  MUSTER_FLEET_DIGEST: '1',
  // The pull requests muster records sessions creating (session_prs) are
  // the PR-mail feature's store.
  MUSTER_PLUGINS: '1',
  MUSTER_SESSION_ROUTING: '1',
  MUSTER_PR_MAIL_SENDERS: 'e2e-review/pr-check',
  // The GitHub App: each pull request's state.
  MUSTER_GITHUB_APP_ID: APP_ID,
  MUSTER_GITHUB_APP_PRIVATE_KEY: APP_KEY,
  MUSTER_GITHUB_APP_ACCOUNTS: `${APP_ACCOUNT.login}:${APP_ACCOUNT.id}`,
  MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x5d).toString('base64'),
  MUSTER_CONNECT_PORT: String(chiefFleetConnectPort),
  MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${chiefFleetConnectPort}`,
  MUSTER_E2E_GITHUB_API: `http://127.0.0.1:${chiefFleetGitHubPort}`,
});

let runtime: http.Server | undefined;
let github: http.Server | undefined;
let chiefMade = false;
let cardID = '';
// Every request the fake runtime got: a chief turn would be one.
const turns: string[] = [];
const prReads: string[] = [];

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

const shotDir = process.env.MUSTER_E2E_FLEET_SHOTS || '';

test.beforeAll(async ({ browser }) => {
  test.setTimeout(240_000);
  runtime = http.createServer((req, res) => {
    let body = '';
    req.on('data', (d) => (body += d));
    req.on('end', () => {
      turns.push(`${req.method} ${req.url} ${body.slice(0, 200)}`);
      res.statusCode = 404;
      res.end();
    });
  });
  await new Promise<void>((r) => runtime!.listen(chiefFleetFakePort, '127.0.0.1', () => r()));
  github = http.createServer((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    const u = new URL(req.url || '/', 'http://x');
    if (u.pathname === '/app/installations') return res.end(JSON.stringify([{ id: 79, account: APP_ACCOUNT }]));
    if (u.pathname === '/app/installations/79/access_tokens') {
      return res.end(JSON.stringify({ token: 'ghs_e2e_fleet_not_a_real_token', expires_at: new Date(Date.now() + 3600_000).toISOString() }));
    }
    if (u.pathname === `/repos/${REPO}/pulls/${OPEN_PR}`) {
      prReads.push(u.pathname);
      return res.end(JSON.stringify({ number: OPEN_PR, state: 'open', merged: false, head: { sha: 'b'.repeat(40) } }));
    }
    if (u.pathname === `/repos/${REPO}/pulls/${MERGED_PR}`) {
      prReads.push(u.pathname);
      return res.end(JSON.stringify({ number: MERGED_PR, state: 'closed', merged: true, head: { sha: MERGED_HEAD }, closed_at: '2000-01-02T03:04:05Z' }));
    }
    res.statusCode = 404;
    res.end('{}');
  });
  await new Promise<void>((r) => github!.listen(chiefFleetGitHubPort, '127.0.0.1', () => r()));
  await rig.start();
  fix('chief', '1', chiefName, chiefToken);
  chiefMade = true;
  cardID = fix('ownercard', '1', chiefName, CARD_Q, 'Yes', 'Not tonight');

  // The windows: each a Claude Code transcript (the owner's prompt, the
  // session's reply) and its status as the CLI writes it.
  const longAgo = Date.now() - 60 * 60 * 1000;
  const lines: string[] = [];
  let i = 0;
  for (const [key, w] of Object.entries(W)) {
    i++;
    const file = path.join(rig.dir, `${w.sid}.jsonl`);
    const reply = 'reply' in w ? w.reply : 'I need your answer before I go on.';
    fs.writeFileSync(file, cc.prompt('Work on the ' + w.label.split(':')[0] + ' task.') + cc.reply(reply));
    const status = key === 'waiting' ? 'waiting' : key === 'working' ? 'busy' : 'idle';
    writeFileAtomic(file + '.status', JSON.stringify({ status, statusUpdatedAt: key === 'working' ? Date.now() : longAgo }));
    lines.push(`work|${w.win}|${i}|${i === 1 ? 1 : 0}|claude|${Math.floor(Date.now() / 1000) - 30}|${file}`);
  }
  const windows = path.join(rig.dir, 'windows');
  writeFileAtomic(windows, lines.join('\n') + '\n');
  fix('prsession', '1', W.ci.sid, REPO, String(OPEN_PR));
  fix('prsession', '1', W.done.sid, REPO, String(MERGED_PR));
  fix('prsession', '1', W.done2.sid, REPO, String(MERGED_PR));
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms', MUSTER_MIRROR_INTERACTIVE: '1' });
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await rig.login(page);
  await rig.waitListed(page, 'data-mirror-claude');
  for (const w of [W.ci, W.done, W.done2, W.stalled]) fix('episode', '1', w.sid, 'stalled', rig.host, w.win);
  let last = '';
  await expect
    .poll(
      async () => {
        last = await (await page.request.get(rig.origin + '/ui/chief/fleet')).text();
        return ['needs_you', 'waiting_ci', 'done', 'stalled'].map((g) => chipCount(last, g)).join(',');
      },
      { message: 'every group drawn from the fleet', timeout: 60_000, intervals: [1_000] },
    )
    .toBe('2,1,2,1')
    .catch((e) => {
      throw new Error(`${e.message}\ncard:\n${last.slice(0, 6000)}\nserver log (tail):\n${(rig.server?.log() ?? '').split('\n').slice(-80).join('\n')}`);
    });
  await ctx.close();
});

test.afterAll(async () => {
  try {
    if (cardID) fix('rmask', '1', cardID);
    fix('rmprsessions', '1', 'fleet' + stamp.replace(/[^0-9A-Za-z]/g, ''));
    fix('rmepisodes', '1', 'fleet' + stamp.replace(/[^0-9A-Za-z]/g, ''));
    if (chiefMade) fix('rmagent', '1', chiefName);
  } finally {
    await rig.stop();
    await new Promise<void>((r) => (runtime ? runtime.close(() => r()) : r()));
    await new Promise<void>((r) => (github ? github.close(() => r()) : r()));
  }
});

test.afterEach(async ({ page }, info) => {
  if (info.status === info.expectedStatus) return;
  console.log('runtime asked:', JSON.stringify(turns.slice(-5)));
  console.log('PR reads:', JSON.stringify(prReads.slice(-5)));
  console.log('card:', (await page.locator('#fleet-digest').evaluate((e) => e.outerHTML).catch(() => '(none)')).slice(0, 4000));
  console.log('muster-mirror log (tail):', rig.log.split('\n').slice(-20).join('\n'));
  console.log('server log (tail):', (rig.server?.log() ?? '').split('\n').slice(-60).join('\n'));
});

// chipCount is a group's count on the card's phone chips, from its markup.
function chipCount(html: string, group: string): string {
  const m = new RegExp(`data-fleet-chip="${group}"[^>]*><span>[^<]*</span><span data-fleet-count=""[^>]*>([0-9]+)<`).exec(html);
  return m ? m[1] : '?';
}

// openPanel opens the chief panel over /tasks (it has no launcher there; the
// toggle is delegated on the document, so a launcher's markup clicked by
// script opens it), and waits for the card and the chat.
async function openPanel(page: Page) {
  await page.goto(rig.origin + '/tasks');
  await page.evaluate(() => {
    const b = document.createElement('button');
    b.setAttribute('data-chief-panel-toggle', '');
    b.style.cssText = 'position:fixed;left:-9999px;top:0';
    document.body.appendChild(b);
    b.click();
  });
  await expect(page.locator('#chief-panel')).toHaveAttribute('data-chief-panel-open', '1');
  await expect(page.locator('#fleet-digest [data-fleet-digest]')).toHaveAttribute('data-fleet-state', 'ready', { timeout: 20_000 });
  await expect(page.locator('#chat-log')).toBeVisible({ timeout: 20_000 });
}

// above asserts the card sits wholly above the open chat.
async function above(page: Page) {
  const card = await page.locator('#fleet-digest').boundingBox();
  const chat = await page.locator('#chief-panel-body').boundingBox();
  expect(card && chat, 'both are laid out').toBeTruthy();
  expect(card!.y + card!.height, 'the card ends above the chat').toBeLessThanOrEqual(chat!.y + 1);
}

const shot = async (page: Page, info: { outputPath: (n: string) => string }, name: string) => {
  const file = info.outputPath(name);
  await page.screenshot({ path: file });
  if (shotDir) {
    fs.mkdirSync(shotDir, { recursive: true });
    fs.copyFileSync(file, path.join(shotDir, name));
  }
};

for (const vp of [
  { name: 'phone 390×844', key: 'phone', phone: true, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', key: 'desktop', phone: false, use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`on a ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    // 🔴 AUDIT ROUND 1 🟡2: a panel restored open re-dispatches the body's open
    // event on every settle while the body has not loaded, and a card on that
    // event refetched ITSELF (a marks write and a whole fleet read each time)
    // until the body landed — for ever, if it never did. The body is held back
    // here so that window is long. The card is fetched once, with no look; a
    // real open (the panel tapped open) fetches it once more, with one.
    test('restored open: the card is fetched once, without a look; a real open fetches it once more, with one', async ({ page }) => {
      test.setTimeout(60_000);
      await rig.login(page);
      await page.goto(rig.origin + '/tasks');
      // 🔴 THE STORED OPEN STATE IS SEEDED FOR THE RELOADED DOCUMENT ONLY. Set
      // in the page BEFORE the reload (as this test once did), the old
      // document saw it too: restore() runs on every htmx settle and applies
      // the stored state — the cross-tab restore the panel is built for — so a
      // settle in the old document between the write and its unload opened
      // ITS panel and fetched the card, and that fetch was counted here as the
      // reloaded page's second (measured: the extra request left 15 ms before
      // the reload's navigation committed, while the reloaded document itself
      // made exactly one). An init script writes it before the new document's
      // first script runs, and only once.
      await page.addInitScript(() => {
        if (sessionStorage.getItem('e2e-fleet-restore-seeded')) return;
        sessionStorage.setItem('e2e-fleet-restore-seeded', '1');
        localStorage.setItem('muster.chief.v1.open.chief-panel', '1');
      });
      const fetches: string[] = [];
      page.on('request', (r) => {
        const u = new URL(r.url());
        if (u.pathname === '/ui/chief/fleet') fetches.push(u.search);
      });
      await page.route('**/ui/chief/panel*', async (route) => {
        await new Promise((r) => setTimeout(r, 3000));
        await route.continue().catch(() => {});
      });
      await page.reload();
      await expect(page.locator('#fleet-digest [data-fleet-digest]')).toHaveAttribute('data-fleet-state', 'ready', { timeout: 20_000 });
      await expect(page.locator('#chat-log')).toBeVisible({ timeout: 20_000 });
      await page.waitForTimeout(1500);
      expect(fetches, 'one fetch on a restored-open load, and no look').toEqual(['']);
      await page.unroute('**/ui/chief/panel*');
      await page.locator('[data-chief-panel-close]').click();
      await expect(page.locator('#chief-panel')).toHaveAttribute('data-chief-panel-open', '0');
      await page.evaluate(() => {
        const b = document.createElement('button');
        b.setAttribute('data-chief-panel-toggle', '');
        b.style.cssText = 'position:fixed;left:-9999px;top:0';
        document.body.appendChild(b);
        b.click();
      });
      await expect.poll(() => fetches.length, { timeout: 10_000 }).toBe(2);
      expect(fetches[1], 'a real open is a look').toBe('?look=1');
      await page.evaluate(() => localStorage.removeItem('muster.chief.v1.open.chief-panel'));
    });

    test('the card above the chat → a group → a drill-in → the Fleet chat with a draft, nothing sent → Open session', async ({ page }, info) => {
      test.setTimeout(120_000);
      const tap = async (loc: ReturnType<Page['locator']>) => (vp.phone ? loc.tap() : loc.click());
      await rig.login(page);
      await openPanel(page);
      const card = page.locator('#fleet-digest [data-fleet-digest]');
      await above(page);

      // The counts, the same on the chips and the desktop groups: Needs you
      // is the prompt on screen and the chief's own open card. (Working also
      // holds the shared database's seeded agents, so it is checked by its
      // row: the busy window is in it.)
      // Each viewport opens ITS OWN Done session, and opening a session ends its
      // stop (the owner viewed it): the phone runs first and leaves one.
      const mine = vp.phone ? W.done : W.done2;
      const want: Record<string, string> = { needs_you: '2', waiting_ci: '1', done: vp.phone ? '2' : '1', stalled: '1' };
      await expect(card.locator(`[data-fleet-group="working"] [data-fleet-row="mirror:${W.working.sid}"]`)).toHaveCount(1);
      await expect(card.locator(`[data-fleet-group="needs_you"] [data-fleet-row="mirror:${W.waiting.sid}"]`)).toHaveCount(1);
      await expect(card.locator(`[data-fleet-group="needs_you"] [data-fleet-ask="${cardID}"]`)).toHaveCount(1);
      for (const [g, n] of Object.entries(want)) {
        await expect(card.locator(`[data-fleet-chip="${g}"] [data-fleet-count]`)).toHaveText(n);
        await expect(card.locator(`[data-fleet-group="${g}"] [data-fleet-count]`).first()).toHaveText(n);
      }
      await shot(page, info, `fleet-card-${vp.key}.png`);

      let drill: ReturnType<Page['locator']>;
      if (vp.phone) {
        // One row of chips; the desktop groups are not shown.
        await expect(card.locator('[data-fleet-chip="done"]')).toBeVisible();
        await expect(card.locator('details[data-fleet-group]').first()).toBeHidden();
        const chips = await card.locator('[data-fleet-chip]').evaluateAll((els) => els.map((e) => Math.round(e.getBoundingClientRect().top)));
        expect(new Set(chips).size, 'the chips are one row').toBe(1);
        // A chip opens its group's bottom sheet, at the screen's bottom.
        await tap(card.locator('[data-fleet-chip="done"]'));
        const sheet = page.locator('[data-fleet-sheet="done"]');
        await expect(sheet).toBeVisible();
        const box = await sheet.boundingBox();
        expect(Math.round(box!.y + box!.height), 'the sheet sits on the screen bottom').toBe(844);
        await expect(sheet).toContainText(`${REPO}#${MERGED_PR} merged`);
        await shot(page, info, `fleet-sheet-${vp.key}.png`);
        drill = sheet;
      } else {
        await expect(card.locator('[data-fleet-chip]').first()).toBeHidden();
        const group = card.locator('details[data-fleet-group="done"]');
        await tap(group.locator('summary'));
        await expect(group).toHaveAttribute('open', '');
        await expect(group).toContainText(`${REPO}#${MERGED_PR} merged`);
        await above(page);
        drill = group;
      }

      // The drill-in, under the session's row.
      const row = drill.locator(`[data-fleet-row="mirror:${mine.sid}"]`);
      await tap(row.locator('[data-fleet-drill]'));
      const inner = row.locator(`[data-fleet-drill-in="mirror:${mine.sid}"]`);
      await expect(inner).toBeVisible({ timeout: 15_000 });
      await expect(inner).toContainText(`${REPO}#${MERGED_PR} · merged · ${MERGED_HEAD.slice(0, 12)}`);
      await expect(inner).toContainText(mine.reply);
      await expect(inner).toContainText('stalled');
      const open = inner.locator('[data-fleet-open-session]');
      const href = await open.getAttribute('href');
      expect(href).toBe(`/mirror/view?host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent(mine.win)}`);
      await shot(page, info, `fleet-drill-${vp.key}.png`);

      // "Ask the chief about this": the Fleet chat, the draft in the box,
      // nothing sent.
      const asked = turns.length;
      await tap(inner.locator('[data-fleet-ask-chief]'));
      const input = page.locator('#chat-input');
      await expect(input).toHaveValue(new RegExp(`^About mirror:${mine.sid} \\(“.+”\\), stalled: $`), { timeout: 15_000 });
      await expect(page.locator('[data-fleet-sheet]:not([hidden])')).toHaveCount(0);
      await expect(card).toBeVisible();
      await above(page);
      // The open chat is a side chat called Fleet, not the primary.
      const active = await page.locator('#chief-panel-body [data-chief-active-session]').getAttribute('data-chief-active-session');
      const threads = await (await page.request.get(rig.origin + '/ui/chief/threads/search?q=Fleet&session=' + active)).text();
      expect(threads).toContain(`data-chief-thread="${active}"`);
      await page.waitForTimeout(1500);
      expect(turns.length, 'the runtime heard no turn').toBe(asked);
      await expect(page.locator('#chat-log [data-bubble]')).toHaveCount(0);
      await shot(page, info, `fleet-chat-${vp.key}.png`);

      // Open session: the session's rendered view. On a phone the sheet the
      // drill-in is in was shut by the chat; its chip opens it again, drill-in
      // and all.
      if (vp.phone) {
        await tap(card.locator('[data-fleet-chip="done"]'));
        await expect(page.locator('[data-fleet-sheet="done"]')).toBeVisible();
      }
      await expect(open).toBeVisible();
      await tap(open);
      await expect(page).toHaveURL(new RegExp(`/mirror/view\\?.*window=${encodeURIComponent(mine.win)}`), { timeout: 20_000 });
      await expect(page.locator('#mv-log')).toContainText(mine.reply, { timeout: 20_000 });
    });
  });
}
