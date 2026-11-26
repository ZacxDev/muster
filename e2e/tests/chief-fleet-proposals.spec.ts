import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { chiefProposeConnectPort, chiefProposeFakePort, chiefProposeGitHubPort, chiefProposePort, dsn, repoRoot } from '../lib/env';
import { MirrorRig, cc } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';
import { clsInit, readCls, resetCls, settle } from '../lib/cls';

// FLEET PROPOSALS (docs/chief.md "Fleet proposals"), on the operator's click
// path, at a phone (390×844, touch) and a desktop (1280×800):
//
//   - with MUSTER_FLEET_ORCHESTRATION=propose, the chief's session_instruct
//     (called over its own MCP door, as a claude-code chief calls it) files a
//     card in the digest's Proposed group showing EXACTLY what will be sent —
//     muster's fixed text after its attribution line, or the chief's own;
//   - Approve queues it ("Queued…") and the card leaves; Edit opens the text,
//     and "Send as mine" queues the owner's words; Reject sends nothing;
//   - a card whose stop ended expires: it leaves the group on the next draw;
//   - Approve all, after a confirm listing every card, sends each that is
//     still on its stop and skips one whose stop ended after it was drawn:
//     "2 sent, 1 skipped (moved on).";
//   - opening the panel on a card with a Proposed group shifts nothing;
//   - the batched push's tap (ui.FleetProposalsURL, the url
//     TestTheFleetPushNamesTheNewestProposalAndOpensIt pins on the push) lands
//     with the panel open on the Proposed group — on a phone its sheet —
//     scrolled to the newest proposal, highlighted, its Approve in view, each
//     card saying when it expires, and nothing shifted; a tap whose proposal
//     no longer waits (decided, expired) says so.
//
// ⚠ WHAT IS PLAYED: tmux (the e2emirror host's windows file), GitHub (a fake
// App: one merged pull request), the model (a fake runtime that must hear
// nothing), and each window's STOP (e2e/pluginfix episode, as the pass would
// open it after the stall threshold, which a browser spec cannot wait out).
// The chief's calls are the test's (no model runs). What is filed, its text,
// the re-read at the tap and the expiry are muster's own. (muster's OWN
// handoff proposals wait out the stall threshold, which a browser spec cannot:
// TestFlagOffListsNoToolDrawsNoCardWritesNothing drives them.) Every value is
// invented fixture data.

const stamp = Date.now().toString(36);
const chiefName = 'e2e-fprop-chief-' + stamp;
const chiefToken = 'e2e-fprop-chief-token-not-a-real-one-' + stamp;
const REPO = 'e2e-quill/ledger';
const MERGED_PR = 31;
const OPEN_PR = 32;
const CI_HEAD = 'd00dfeedd00dfeedd00dfeedd00dfeedd00dfeed';
const ANSWER = 'Keep the old flag for one release, then drop it.';
const MERGED_HEAD = 'c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00';
const HANDOFF = `Your PR #${MERGED_PR} is merged and nothing is in flight. Write your handoff (what is done, what is left, where), then stop.`;
const CONTINUE = 'Yes, continue with what you proposed. Ask with attention_ask before anything destructive, outward or outside this task.';
// What the chief proposes on each window (one class per window: at most one
// open proposal per session and class).
const CLASS: Record<string, string> = { answer: 'composed', approve: 'continue', edit: 'handoff', reject: 'stop_note', gone: 'continue', bulkGone: 'continue', bulkA: 'handoff', bulkB: 'continue' };
const prefix = `fprop${stamp}`.replace(/[^0-9A-Za-z]/g, '');

// Seven finished windows per viewport, one per step.
const ROLES = ['approve', 'edit', 'reject', 'gone', 'bulkGone', 'bulkA', 'bulkB', 'answer'] as const;
type Role = (typeof ROLES)[number];
const windowsOf = (vp: string, base: number) =>
  Object.fromEntries(ROLES.map((r, i) => [r, { sid: `${prefix}${vp}x${i}`, win: `@${base + i}` }])) as Record<Role, { sid: string; win: string }>;
const PHONE = windowsOf('p', 10);
const DESK = windowsOf('d', 30);
// One window per viewport whose open pull request's CI finished after it
// stopped: muster proposes ci_result (its stop is opened in that viewport's
// test, so the other's card is not in its bulk approve).
const CIW = { p: { sid: `${prefix}pci`, win: '@50' }, d: { sid: `${prefix}dci`, win: '@51' } };
// Three windows per viewport for the push's tap: proposed oldest to newest;
// the middle one's stop ends before a tap (it expires).
const ARRIVALS = ['old', 'gone', 'fresh'] as const;
type Arrival = (typeof ARRIVALS)[number];
const arrivalsOf = (vp: string, base: number) =>
  Object.fromEntries(ARRIVALS.map((r, i) => [r, { sid: `${prefix}${vp}a${i}`, win: `@${base + i}` }])) as Record<Arrival, { sid: string; win: string }>;
const ARRIVE = { p: arrivalsOf('p', 60), d: arrivalsOf('d', 70) };
const ARRIVE_CLASS: Record<Arrival, string> = { old: 'continue', gone: 'handoff', fresh: 'stop_note' };

const APP_ID = '515151';
const APP_ACCOUNT = { login: 'e2e-quill', id: 5151, type: 'User' };
const APP_KEY = crypto.generateKeyPairSync('rsa', { modulusLength: 2048 }).privateKey.export({ type: 'pkcs1', format: 'pem' }).toString();

const rig = new MirrorRig(chiefProposePort, 'e2e-cprop', {
  MUSTER_FOCUS_INTERVAL: '0',
  MUSTER_AGENT_GATEWAY: 'hooks-sha256',
  MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
  MUSTER_AGENT_RUNTIME_CONFIG: '{}',
  MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
  MUSTER_AGENT_KINDS: 'gateway',
  MUSTER_AGENT_OPENROUTER_KEY: 'sk-or-v1-FAKE-e2e-not-a-real-key',
  MUSTER_E2E_AGENT_ENDPOINT: `http://127.0.0.1:${chiefProposeFakePort}`,
  MUSTER_E2E_SUGGEST_URL: `http://127.0.0.1:${chiefProposeFakePort}/v1/chat/completions`,
  MUSTER_SESSION_STATE: '1',
  MUSTER_FLEET_DIGEST: '1',
  MUSTER_FLEET_ORCHESTRATION: 'propose',
  // The ask log: a composed answer answers a question the session HOLDS
  // (its open plain-text ask-log row).
  MUSTER_ASK_LOG: '1',
  MUSTER_PLUGINS: '1',
  MUSTER_SESSION_ROUTING: '1',
  MUSTER_PR_MAIL_SENDERS: 'e2e-review/pr-check',
  MUSTER_GITHUB_APP_ID: APP_ID,
  MUSTER_GITHUB_APP_PRIVATE_KEY: APP_KEY,
  MUSTER_GITHUB_APP_ACCOUNTS: `${APP_ACCOUNT.login}:${APP_ACCOUNT.id}`,
  MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x6e).toString('base64'),
  MUSTER_CONNECT_PORT: String(chiefProposeConnectPort),
  MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${chiefProposeConnectPort}`,
  MUSTER_E2E_GITHUB_API: `http://127.0.0.1:${chiefProposeGitHubPort}`,
});

let runtime: http.Server | undefined;
let github: http.Server | undefined;
let chiefMade = false;
const turns: string[] = [];

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

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
  await new Promise<void>((r) => runtime!.listen(chiefProposeFakePort, '127.0.0.1', () => r()));
  github = http.createServer((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    const u = new URL(req.url || '/', 'http://x');
    if (u.pathname === '/app/installations') return res.end(JSON.stringify([{ id: 83, account: APP_ACCOUNT }]));
    if (u.pathname === '/app/installations/83/access_tokens') {
      return res.end(JSON.stringify({ token: 'ghs_e2e_fprop_not_a_real_token', expires_at: new Date(Date.now() + 3600_000).toISOString() }));
    }
    if (u.pathname === `/repos/${REPO}/pulls/${OPEN_PR}`) {
      return res.end(JSON.stringify({ number: OPEN_PR, state: 'open', merged: false, head: { sha: CI_HEAD } }));
    }
    if (u.pathname === `/repos/${REPO}/commits/${CI_HEAD}/status`) return res.end(JSON.stringify({ statuses: [] }));
    if (u.pathname === `/repos/${REPO}/commits/${CI_HEAD}/check-runs`) {
      return res.end(JSON.stringify({ check_runs: [{ id: 501, name: 'unit', status: 'completed', conclusion: 'success', completed_at: new Date().toISOString() }] }));
    }
    if (u.pathname === `/repos/${REPO}/pulls/${MERGED_PR}`) {
      return res.end(JSON.stringify({ number: MERGED_PR, state: 'closed', merged: true, head: { sha: MERGED_HEAD }, closed_at: '2000-01-02T03:04:05Z' }));
    }
    res.statusCode = 404;
    res.end('{}');
  });
  await new Promise<void>((r) => github!.listen(chiefProposeGitHubPort, '127.0.0.1', () => r()));
  await rig.start();
  fix('chief', '1', chiefName, chiefToken);
  chiefMade = true;

  const longAgo = Date.now() - 60 * 60 * 1000;
  const lines: string[] = [];
  let i = 0;
  for (const w of [...Object.values(PHONE), ...Object.values(DESK), CIW.p, CIW.d, ...Object.values(ARRIVE.p), ...Object.values(ARRIVE.d)]) {
    i++;
    const file = path.join(rig.dir, `${w.sid}.jsonl`);
    fs.writeFileSync(file, cc.prompt('Fix ledger bug ' + i + '.') + cc.reply('Ledger fix ' + i + ' is merged.'));
    writeFileAtomic(file + '.status', JSON.stringify({ status: 'idle', statusUpdatedAt: longAgo }));
    lines.push(`work|${w.win}|${i}|${i === 1 ? 1 : 0}|claude|${Math.floor(Date.now() / 1000) - 30}|${file}`);
    fix('prsession', '1', w.sid, REPO, String(w === CIW.p || w === CIW.d ? OPEN_PR : MERGED_PR));
  }
  const windows = path.join(rig.dir, 'windows');
  writeFileAtomic(windows, lines.join('\n') + '\n');
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms', MUSTER_MIRROR_INTERACTIVE: '1' });
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await rig.login(page);
  await rig.waitListed(page, 'data-mirror-claude');
  await ctx.close();
  // Every window's stop, as the pass would open it; the pass then observes
  // each (its idle streak), which a proposal is filed on.
  const all = [...Object.values(PHONE), ...Object.values(DESK), ...Object.values(ARRIVE.p), ...Object.values(ARRIVE.d)];
  for (const w of all) fix('episode', '1', w.sid, 'stalled', rig.host, w.win);
  // The question each answer card answers: an open plain-text ask in the
  // answer window's stop.
  for (const set of [PHONE, DESK]) fix('plainask', '1', set.answer.sid, rig.host, set.answer.win, 'Keep the old flag, or drop it now?');
  await expect
    .poll(() => all.filter((w) => fix('episodestreak', '1', w.sid) !== '').length, {
      message: 'the session-state pass observed every stop', timeout: 90_000, intervals: [3_000],
    })
    .toBe(all.length);
});

test.afterAll(async () => {
  try {
    fix('rmfleetactions', '1', prefix);
    fix('rmasks', '1', prefix);
    fix('rmprsessions', '1', prefix);
    fix('rmepisodes', '1', prefix);
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
  console.log('card:', (await page.locator('#fleet-digest').evaluate((e) => e.outerHTML).catch(() => '(none)')).slice(0, 6000));
  console.log('server log (tail):', (rig.server?.log() ?? '').split('\n').slice(-80).join('\n'));
});

// chiefCall calls one of the chief's tools over its MCP door (the connect
// listener, the chief's own token), as a claude-code chief does.
async function chiefCall(name: string, args: Record<string, unknown>): Promise<Record<string, unknown>> {
  const r = await fetch(`http://127.0.0.1:${chiefProposeConnectPort}/mcp`, {
    method: 'POST',
    headers: { 'X-Muster-Token': chiefToken, 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name, arguments: args } }),
  });
  expect(r.status, 'the /mcp door answered').toBe(200);
  const j = (await r.json()) as { result: { content: { text: string }[]; isError?: boolean } };
  return JSON.parse(j.result.content[0].text) as Record<string, unknown>;
}

// propose files one viewport's proposals through the chief's session_instruct,
// then waits for its cards.
async function propose(page: Page, set: Record<Role, { sid: string; win: string }>) {
  for (const r of ROLES) {
    const out =
      r === 'answer'
        ? await chiefCall('session_answer', { subject: `mirror:${set[r].sid}`, answer_kind: 'composed', text: ANSWER, reason: 'it asked which way' })
        : await chiefCall('session_instruct', { subject: `mirror:${set[r].sid}`, class: CLASS[r], reason: `the ${r} step` });
    expect(out.state, `session_instruct ${r}: ${JSON.stringify(out)}`).toBe('proposed');
  }
  let last = '';
  await expect
    .poll(
      async () => {
        last = await (await page.request.get(rig.origin + '/ui/chief/fleet')).text();
        return Object.values(set).filter((w) => last.includes(`data-fleet-proposal-subject="mirror:${w.sid}"`)).length;
      },
      { message: 'a card for every proposal', timeout: 20_000, intervals: [1_000] },
    )
    .toBe(ROLES.length)
    .catch((e) => {
      throw new Error(`${e.message}\ncard:\n${last.slice(0, 6000)}\nserver log (tail):\n${(rig.server?.log() ?? '').split('\n').slice(-80).join('\n')}`);
    });
}

// ciCard opens one viewport's CI window's stop, then waits for muster's own
// ci_result card on it (the pass: within a pass or two).
async function ciCard(page: Page, w: { sid: string; win: string }) {
  fix('episode', '1', w.sid, 'stalled', rig.host, w.win);
  let last = '';
  await expect
    .poll(
      async () => {
        last = await (await page.request.get(rig.origin + '/ui/chief/fleet')).text();
        return last.includes(`data-fleet-proposal-subject="mirror:${w.sid}"`);
      },
      { message: 'muster proposes ci_result', timeout: 100_000, intervals: [2_000] },
    )
    .toBe(true)
    .catch((e) => {
      throw new Error(`${e.message}\ncard:\n${last.slice(0, 6000)}\nserver log (tail):\n${(rig.server?.log() ?? '').split('\n').slice(-80).join('\n')}`);
    });
}

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
}

for (const vp of [
  { name: 'phone 390×844', key: 'p', phone: true, set: PHONE, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', key: 'd', phone: false, set: DESK, use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`on a ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('approve, edit, reject, expiry and approve all, each on its card', async ({ page }) => {
      test.setTimeout(240_000);
      const tap = async (loc: ReturnType<Page['locator']>) => (vp.phone ? loc.tap() : loc.click());
      await rig.login(page);
      await propose(page, vp.set);
      const ciw = vp.phone ? CIW.p : CIW.d;
      await ciCard(page, ciw);
      await page.addInitScript(clsInit);
      await openPanel(page);
      const card = page.locator('#fleet-digest [data-fleet-digest]');
      // The group: a bottom sheet on a phone, inline on a desktop.
      const group = () => (vp.phone ? page.locator('[data-fleet-sheet="proposed"]') : card.locator('details[data-fleet-group="proposed"]'));
      if (vp.phone) {
        await tap(card.locator('[data-fleet-chip="proposed"]'));
      } else {
        await tap(card.locator('details[data-fleet-group="proposed"] summary'));
      }
      await expect(group()).toBeVisible();
      const of = (r: Role) => group().locator(`[data-fleet-proposal-subject="mirror:${vp.set[r].sid}"]`);

      // Each card shows its verb and EXACTLY what will be sent.
      const first = of('approve');
      await expect(first.locator('[data-fleet-verb]')).toHaveText('Continue');
      await expect(first.locator('[data-fleet-text]')).toHaveText(new RegExp(`^\\[Chief, approved by .+ at \\d\\d:\\d\\d UTC\\]\\s+${CONTINUE.replace(/[.()]/g, '\\$&')}$`));
      if (vp.phone) {
        // Tap targets: full width, 44 px tall.
        for (const b of ['[data-fleet-approve]', '[data-fleet-reject]', '[data-fleet-edit]']) {
          const box = await first.locator(b).boundingBox();
          expect(Math.round(box!.height), `${b} is 44px tall`).toBeGreaterThanOrEqual(44);
        }
      }

      // Approve.
      await tap(first.locator('[data-fleet-approve]'));
      await expect(card.locator('[data-fleet-note]')).toHaveText('Queued: it is pasted once the session is idle.');
      await expect(of('approve')).toHaveCount(0);

      // An answer card (the chief's own words) and muster's ci_result card:
      // each shows exactly what is sent, and Approve queues it.
      const ans = of('answer');
      await expect(ans.locator('[data-fleet-verb]')).toHaveText("Answer (the chief's words)");
      await expect(ans.locator('[data-fleet-text]')).toContainText(ANSWER);
      await tap(ans.locator('[data-fleet-approve]'));
      await expect(card.locator('[data-fleet-note]')).toHaveText('Queued: it is pasted once the session is idle.');
      await expect(of('answer')).toHaveCount(0);
      const ci = group().locator(`[data-fleet-proposal-subject="mirror:${ciw.sid}"]`);
      await expect(ci.locator('[data-fleet-verb]')).toHaveText('Tell it CI finished');
      await expect(ci.locator('[data-fleet-text]')).toContainText(`CI finished on #${OPEN_PR} at ${CI_HEAD.slice(0, 12)}: ok=1 bad=0. Continue.`);
      await tap(ci.locator('[data-fleet-approve]'));
      await expect(card.locator('[data-fleet-note]')).toHaveText('Queued: it is pasted once the session is idle.');
      await expect(ci).toHaveCount(0);

      // Edit: the box, the owner's own words, sent as theirs.
      const ed = of('edit');
      await tap(ed.locator('[data-fleet-edit]'));
      const box = ed.locator('textarea[name="text"]');
      await expect(box).toBeVisible();
      await expect(box).toHaveValue(HANDOFF);
      await expect(ed.locator('[data-fleet-verb]')).toHaveText('Write handoff');
      await box.fill(`Write the handoff, then wait for me ${stamp}.`);
      await tap(ed.locator('button[type="submit"]'));
      await expect(card.locator('[data-fleet-note]')).toHaveText('Queued: it is pasted once the session is idle.');
      await expect(of('edit')).toHaveCount(0);

      // Expiry: a stop that ended leaves the group on the next draw (the
      // Reject below redraws the card).
      await expect(of('gone')).toHaveCount(1);
      fix('endepisode', '1', vp.set.gone.sid);

      // Reject.
      await tap(of('reject').locator('[data-fleet-reject]'));
      await expect(card.locator('[data-fleet-note]')).toHaveText('Rejected.');
      await expect(of('reject')).toHaveCount(0);
      await expect(of('gone'), 'the card whose stop ended expired').toHaveCount(0);

      // Approve all: after the confirm listing each card; one stop ends after
      // the card was drawn, and is skipped.
      fix('endepisode', '1', vp.set.bulkGone.sid);
      let asked = '';
      page.once('dialog', async (d) => {
        asked = d.message();
        await d.accept();
      });
      await tap(group().locator('[data-fleet-bulk="approve"] button[type="submit"]:visible'));
      await expect(card.locator('[data-fleet-note]')).toHaveText('2 sent, 1 skipped (moved on).');
      expect(asked).toContain('Write handoff');
      expect(asked).toContain('Continue');
      for (const r of ['bulkGone', 'bulkA', 'bulkB'] as Role[]) await expect(of(r)).toHaveCount(0);
      expect(turns.length, 'no chief turn, no model call').toBe(0);
    });

    test('opening the panel on a card with a Proposed group shifts less than 0.01', async ({ page }) => {
      test.setTimeout(60_000);
      await rig.login(page);
      await page.addInitScript(clsInit);
      await page.goto(rig.origin + '/tasks');
      await settle(page);
      await resetCls(page);
      await page.evaluate(() => {
        const b = document.createElement('button');
        b.setAttribute('data-chief-panel-toggle', '');
        b.style.cssText = 'position:fixed;left:-9999px;top:0';
        document.body.appendChild(b);
        b.click();
      });
      await expect(page.locator('#fleet-digest [data-fleet-digest]')).toHaveAttribute('data-fleet-state', 'ready', { timeout: 20_000 });
      await expect(page.locator('#fleet-digest [data-fleet-chip="proposed"]')).toHaveCount(1);
      await settle(page);
      const r = await readCls(page);
      expect(r.unsupported, 'the browser reports layout-shift entries').toBeUndefined();
      expect(r.total, JSON.stringify(r.shifts)).toBeLessThan(0.01);
      await page.evaluate(() => { try { localStorage.removeItem('muster.chief.v1.open.chief-panel'); } catch (e) {} });
    });

    test("the push's tap opens the Proposed group on the newest proposal, and says when one no longer waits", async ({ page }) => {
      test.setTimeout(120_000);
      const tap = async (loc: ReturnType<Page['locator']>) => (vp.phone ? loc.tap() : loc.click());
      await rig.login(page);
      const set = ARRIVE[vp.key as 'p' | 'd'];
      const ids = {} as Record<Arrival, number>;
      for (const r of ARRIVALS) {
        const out = await chiefCall('session_instruct', { subject: `mirror:${set[r].sid}`, class: ARRIVE_CLASS[r], reason: `the ${r} arrival` });
        expect(out.state, `session_instruct ${r}: ${JSON.stringify(out)}`).toBe('proposed');
        ids[r] = out.action_id as number;
      }
      const card = page.locator('#fleet-digest [data-fleet-digest]');
      const group = () => (vp.phone ? page.locator('[data-fleet-sheet="proposed"]') : card.locator('details[data-fleet-group="proposed"]'));
      const of = (r: Arrival) => group().locator(`[data-fleet-proposal-subject="mirror:${set[r].sid}"]`);
      // The push's url for proposal id (ui.FleetProposalsURL; the Go tests pin
      // the push and the Attention notice to it).
      const arrive = async (id: number) => {
        await page.goto(rig.origin + `/tasks?chief=open&fleet=proposed&proposal=${id}`);
        await expect(page.locator('#chief-panel')).toHaveAttribute('data-chief-panel-open', '1', { timeout: 20_000 });
        await expect(card).toHaveAttribute('data-fleet-state', 'ready');
        await expect(group(), 'the Proposed group is open on arrival').toBeVisible();
      };

      await page.addInitScript(clsInit);
      await arrive(ids.fresh);
      // The parameters left the address bar: a reload does not reopen it.
      await expect(page).toHaveURL(rig.origin + '/tasks');
      if (vp.phone) {
        await expect(card.locator('[data-fleet-chip="proposed"]')).toHaveAttribute('aria-expanded', 'true');
      } else {
        await expect(group()).toHaveAttribute('open', '');
      }
      // The newest, highlighted, scrolled into view, its Approve right there.
      const hl = group().locator('[data-fleet-highlight]');
      await expect(hl).toHaveCount(1);
      await expect(hl).toHaveAttribute('data-fleet-proposal-subject', `mirror:${set.fresh.sid}`);
      await expect(hl.locator('[data-fleet-verb]')).toHaveText('Stop and wait');
      await expect(of('old')).toHaveCount(1);
      await expect(hl.locator('[data-fleet-approve]')).toBeInViewport();
      // Each card says when it expires, in the reader's own clock (with its
      // date when that is not today: 12 h on can be tomorrow) — never the
      // server's UTC fallback.
      for (const r of ARRIVALS) {
        await expect(of(r).locator('[data-fleet-expires]')).toHaveText(/^expires in 1[01]h \((?:[A-Z][a-z]{2} \d{1,2}, )?\d{1,2}:\d\d(\s?[AP]M)?\)$/);
      }
      await settle(page);
      const shift = await readCls(page);
      expect(shift.unsupported, 'the browser reports layout-shift entries').toBeUndefined();
      console.log(`[cls ${vp.name}] arriving from the push: ${shift.total}`);
      expect(shift.total, 'arriving from the push: ' + JSON.stringify(shift.shifts)).toBeLessThan(0.01);

      // Approve is reachable and works; the oldest is rejected.
      await tap(hl.locator('[data-fleet-approve]'));
      await expect(card.locator('[data-fleet-note]')).toHaveText('Queued: it is pasted once the session is idle.');
      await tap(of('old').locator('[data-fleet-reject]'));
      await expect(card.locator('[data-fleet-note]')).toHaveText('Rejected.');

      // A tap whose proposal expired (its stop ended): the line says so, and
      // nothing waits.
      fix('endepisode', '1', set.gone.sid);
      await arrive(ids.gone);
      await expect(group().locator('[data-fleet-gone]')).toHaveText(/^No longer needed: the session moved on at \d{1,2}:\d\d(\s?[AP]M)?\.$/);
      await expect(group().locator('[data-fleet-proposal]')).toHaveCount(0);
      await expect(group()).toContainText('None.');
      // The one already approved: decided, and said so.
      await arrive(ids.fresh);
      await expect(group().locator('[data-fleet-gone]')).toHaveText(/^That proposal was already approved at \d{1,2}:\d\d(\s?[AP]M)?\.$/);
      await page.evaluate(() => { try { localStorage.removeItem('muster.chief.v1.open.chief-panel'); } catch (e) {} });
    });
  });
}
