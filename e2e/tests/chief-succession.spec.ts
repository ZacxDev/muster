import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { test, expect } from '../lib/fixtures';
import { chiefSuccessionFakePort, chiefSuccessionGitHubPort, chiefSuccessionPort, dsn, repoRoot } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';

// A SUCCESSION FROM A SESSION'S VIEW (docs/design/chief-in-agent-view.md,
// phase 2), on the operator's click path: an opencode window's Rendered view,
// the composer flipped to "Chief", a line asking for a new session from this
// one's kickoff, the CARD muster raises (the exact kickoff and the settings),
// the owner's TAP — the authorisation — and the view jumping, with no click,
// to the successor once its window runs. Back returns to the predecessor,
// now superseded.
//
// ⚠ WHAT IS PLAYED: the model (a FAKE /v1/responses runtime is the chief: on a
// view turn it calls session_context with the subject muster's header names,
// then succession_propose naming the kickoff by session_context's ref and
// block — it never writes the kickoff), the host's agent spawn (the e2emirror
// build's MUSTER_MIRROR_E2E_SPAWN_RUNTIMES: it records the request and lists a
// window tagged with the dispatch), and the GitHub App (its repository list).
// Everything else is muster's own: the door, the binding, the copy, the card,
// the tap, the reservation, the reconciler, lineage.changed and the follow.
// Every value is invented fixture data.
//
// 🔴 REGRESSION TEST: on origin/main the chief has no succession_propose, the
// view draws no card, and nothing follows.

const stamp = Date.now().toString(36);
const chiefName = 'e2e-succ-chief-' + stamp;
const chiefToken = 'e2e-succ-chief-token-not-a-real-one-' + stamp;
const REPO = 'e2e-acme/widgets';
const MODEL = 'qwen/qwen3-coder';
const KICKOFF = '/resume — continue the fixture loader work. Canonical handoff (read first): /tmp/e2e/handoff.md\nRun rank 2 next.';

// One predecessor per viewport, so the two runs share nothing but the rig.
const VIEWS = {
  phone: { sid: 'ses_0e2esuccphone1a2b3c', win: '@2', title: 'Fix the flaky fixture loader' },
  desktop: { sid: 'ses_0e2esuccdesk1a2b3c4', win: '@3', title: 'Tidy the widget parser' },
};

const APP_ID = '424243';
const APP_ACCOUNT = { login: 'e2e-acme', id: 4343, type: 'User' };
const APP_KEY = crypto.generateKeyPairSync('rsa', { modulusLength: 2048 }).privateKey.export({ type: 'pkcs1', format: 'pem' }).toString();
const connectPort = chiefSuccessionGitHubPort + 1;

const rig = new MirrorRig(chiefSuccessionPort, 'e2e-csucc', {
  MUSTER_FOCUS_INTERVAL: '0',
  MUSTER_AGENT_GATEWAY: 'hooks-sha256',
  MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
  MUSTER_AGENT_RUNTIME_CONFIG: '{}',
  MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
  MUSTER_AGENT_KINDS: 'gateway',
  MUSTER_AGENT_OPENROUTER_KEY: 'sk-or-v1-FAKE-e2e-not-a-real-key',
  MUSTER_E2E_AGENT_ENDPOINT: `http://127.0.0.1:${chiefSuccessionFakePort}`,
  // The chief's dispatch: task routing (with the plugin interface) and the
  // operator opted in — succession_propose is offered beside agent_dispatch.
  MUSTER_PLUGINS: '1',
  MUSTER_TASK_ROUTING: '1',
  MUSTER_CHIEF_DISPATCH_OWNERS: '1',
  // The GitHub App: the connected repositories a start is checked against.
  MUSTER_GITHUB_APP_ID: APP_ID,
  MUSTER_GITHUB_APP_PRIVATE_KEY: APP_KEY,
  MUSTER_GITHUB_APP_ACCOUNTS: `${APP_ACCOUNT.login}:${APP_ACCOUNT.id}`,
  MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x5c).toString('base64'),
  MUSTER_CONNECT_PORT: String(connectPort),
  MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${connectPort}`,
  MUSTER_E2E_GITHUB_API: `http://127.0.0.1:${chiefSuccessionGitHubPort}`,
  // The card is an attention ask, which asks for suggested replies: pointed at
  // the fake runtime (which answers 404), so nothing real is ever called.
  MUSTER_E2E_SUGGEST_URL: `http://127.0.0.1:${chiefSuccessionFakePort}/v1/chat/completions`,
});
let runtime: http.Server | undefined;
let github: http.Server | undefined;
let chiefMade = false;
let spawnLog = '';

const asked: { first: string; outputs: string[] }[] = [];
const sse = (type: string, data: Record<string, unknown>) => `event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`;
type Out = { text?: string; call?: { id: string; name: string; args: Record<string, unknown> } };

function answer(res: http.ServerResponse, out: Out) {
  res.setHeader('Content-Type', 'text/event-stream');
  const output: unknown[] = [];
  if (out.text) {
    res.write(sse('response.output_text.delta', { delta: out.text, output_index: 0 }));
    output.push({ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: out.text }] });
  }
  if (out.call) output.push({ type: 'function_call', call_id: out.call.id, name: out.call.name, arguments: JSON.stringify(out.call.args) });
  res.end(sse('response.completed', { response: { status: 'completed', output } }));
}

// The scripted chief: the header names the subject → session_context; its
// tail's kickoff (the message with a fenced block) → succession_propose by
// ref and block, with the model and repository the owner's line names.
function script(body: { input?: { type?: string; content?: string; output?: string }[] }): Out {
  const input = body.input ?? [];
  const first = input.find((i) => i.type === 'message')?.content ?? '';
  const outputs = input.filter((i) => i.type === 'function_call_output').map((i) => i.output ?? '');
  asked.push({ first, outputs });
  if (outputs.length === 1) {
    // session_context's answer: the subject it names (the one muster bound)
    // and its tail's kickoff.
    let r: { error?: string; subject?: string; tail?: { ref: string; blocks?: unknown[] }[] } = {};
    try {
      r = JSON.parse(outputs[0]);
    } catch {}
    if (r.error) return { text: 'CHIEF-TOOL-ERROR: ' + r.error };
    const kick = (r.tail ?? []).filter((m) => (m.blocks ?? []).length > 0).pop();
    return {
      call: {
        id: 'call_prop_' + asked.length,
        name: 'succession_propose',
        args: { successor_of: r.subject, succession: 'replace', prompt_from: { ref: kick?.ref, block: 0 }, model: MODEL, repo: REPO },
      },
    };
  }
  if (outputs.length >= 2) return { text: 'CHIEF-PROPOSED: ' + outputs[1].slice(0, 200) };
  const subject = /\nsubject: (\S+)/.exec(first)?.[1];
  if (!first.startsWith('[muster] Your owner is viewing') || !subject || !first.includes('succession_propose')) {
    return { text: 'GOT: ' + first.slice(-120) };
  }
  return { call: { id: 'call_ctx_' + asked.length, name: 'session_context', args: { subject } } };
}

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

const spawns = (): { dispatch: number; runtime: string; repo: string; model: string; prompt: string }[] =>
  fs.existsSync(spawnLog) ? fs.readFileSync(spawnLog, 'utf8').trim().split('\n').filter(Boolean).map((l) => JSON.parse(l)) : [];

test.beforeAll(async ({ browser }) => {
  test.setTimeout(240_000);
  runtime = http.createServer((req, res) => {
    let body = '';
    req.on('data', (d) => (body += d));
    req.on('end', () => {
      if (req.method !== 'POST' || !(req.url || '').endsWith('/v1/responses')) {
        res.statusCode = 404;
        return res.end();
      }
      try {
        answer(res, script(JSON.parse(body)));
      } catch (e) {
        res.statusCode = 500;
        res.end(String(e));
      }
    });
  });
  await new Promise<void>((r) => runtime!.listen(chiefSuccessionFakePort, '127.0.0.1', () => r()));
  github = http.createServer((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    const u = new URL(req.url || '/', 'http://x');
    if (u.pathname === '/app/installations') return res.end(JSON.stringify([{ id: 78, account: APP_ACCOUNT }]));
    if (u.pathname === '/app/installations/78/access_tokens') {
      return res.end(JSON.stringify({ token: 'ghs_e2e_succession_not_a_real_token', expires_at: new Date(Date.now() + 3600_000).toISOString() }));
    }
    if (u.pathname === '/installation/repositories') return res.end(JSON.stringify({ total_count: 1, repositories: [{ full_name: REPO, private: true }] }));
    res.end('{}');
  });
  await new Promise<void>((r) => github!.listen(chiefSuccessionGitHubPort, '127.0.0.1', () => r()));
  await rig.start();
  const chiefID = fix('chief', '1', chiefName, chiefToken);
  chiefMade = true;
  const dir = fs.mkdtempSync(path.join(rig.dir, 'opencode-'));
  const ops = path.join(dir, 'ops.jsonl');
  spawnLog = path.join(rig.dir, 'spawns.jsonl');
  let n = 0;
  for (const v of Object.values(VIEWS)) {
    n++;
    const lines = [
      { op: 'session', id: v.sid, title: v.title },
      { op: 'message', session: v.sid, message: { id: `msg_0cs${n}1`, type: 'user', text: 'write the handoff', files: [], agents: [], time: { created: 1700000000001 } } },
      {
        op: 'message',
        session: v.sid,
        message: {
          id: `msg_0cs${n}2`,
          type: 'assistant',
          agent: 'build',
          model: { id: 'fixture-model', providerID: 'fixture' },
          finish: 'stop',
          time: { created: 1, completed: 9 },
          content: [{ type: 'text', text: 'Handoff written. The kickoff:\n\n```\n' + KICKOFF + '\n```\n' }],
        },
      },
    ];
    for (const l of lines) fs.appendFileSync(ops, JSON.stringify(l) + '\n');
  }
  const t0 = Math.floor(Date.now() / 1000);
  const windows = path.join(rig.dir, 'windows');
  writeFileAtomic(windows, `plain|@1|0|1|zsh|${t0 - 50}|\n`);
  const ocWindows = path.join(rig.dir, 'opencode-windows');
  writeFileAtomic(ocWindows, Object.values(VIEWS).map((v, i) => `oc|${v.win}|${i}|${i === 0 ? 1 : 0}|${t0 - 10}|OC | ${v.title}`).join('\n') + '\n');
  await rig.startHost({
    MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
    MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE: ocWindows,
    MUSTER_MIRROR_E2E_OPENCODE_DIR: dir,
    MUSTER_MIRROR_E2E_REFRESH: '200ms',
    MUSTER_MIRROR_INTERACTIVE: '1',
    // opencode spawns go through the opencode send's API (design 3.1).
    MUSTER_MIRROR_OPENCODE_SEND: '1',
    MUSTER_MIRROR_E2E_SPAWN_RUNTIMES: 'opencode',
    MUSTER_MIRROR_E2E_SPAWN_LOG: spawnLog,
  });
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await rig.login(page);
  await rig.waitListed(page, 'session=oc');
  const started = await page.request.post(`${rig.origin}/agents/${chiefID}/start`, { headers: { Origin: rig.origin, 'HX-Request': 'true' } });
  expect(started.status(), await started.text()).toBeLessThan(400);
  await ctx.close();
});

test.afterAll(async () => {
  try {
    if (chiefMade) fix('rmagent', '1', chiefName);
  } finally {
    await rig.stop();
    await new Promise<void>((r) => (runtime ? runtime.close(() => r()) : r()));
    await new Promise<void>((r) => (github ? github.close(() => r()) : r()));
  }
});

test.afterEach(async ({ page }, info) => {
  if (info.status === info.expectedStatus) return;
  console.log('fake chief asked (last 4):', JSON.stringify(asked.slice(-4), null, 1).slice(0, 4000));
  console.log('spawns:', JSON.stringify(spawns()).slice(0, 2000));
  console.log('page:', page.url());
  const u = new URL(page.url());
  if (u.pathname === '/mirror/view') {
    const r = await page.request.get(rig.origin + '/mirror/view/successor' + u.search).catch(() => null);
    console.log('successor read:', r ? r.status() + ' ' + (await r.text()).slice(0, 1500) : '(failed)');
    const doc = await page.request.get(page.url()).catch(() => null);
    const html = doc ? await doc.text() : '';
    const at = html.indexOf('id="mv-succession"');
    console.log('document region:', at < 0 ? '(no region)' : html.slice(at - 100, at + 600));
  }
  console.log('card:', (await page.locator('#mv-succession').evaluate((e) => e.outerHTML).catch(() => '(none)')).slice(-1500));
  console.log('muster-mirror log (tail):', rig.log.split('\n').slice(-30).join('\n'));
  console.log('server log (tail):', (rig.server?.log() ?? '').split('\n').slice(-60).join('\n'));
});

for (const vp of [
  { name: 'phone 390×844', key: 'phone' as const, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', key: 'desktop' as const, use: { viewport: { width: 1440, height: 900 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`on a ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('ask the chief from the view → the card → one tap → the successor runs and the view follows it', async ({ page }, info) => {
      test.setTimeout(150_000);
      const v = VIEWS[vp.key];
      const tap = async (sel: string) => (vp.use.hasTouch ? page.locator(sel).tap() : page.locator(sel).click());
      await rig.login(page);
      const predPath = `${rig.viewPath('oc')}&window=${encodeURIComponent(v.win)}`;
      await page.goto(rig.origin + predPath);
      await expect(page.locator('#mv-log')).toContainText('Handoff written.', { timeout: 20_000 });
      const predURL = page.url();

      await page.locator('#mv-to').getByText('Chief', { exact: true }).click();
      const input = page.locator('#mv-input');
      await expect(input).toBeEnabled();
      const line = `E2E-SUCC-${vp.key}-${stamp}: copy the kickoff out of this session and run it in a new opencode session with ${MODEL} in ${REPO}, replacing this one`;
      await input.fill(line);
      await input.press('Enter');

      // THE CARD, on this view: the exact kickoff muster copied, and the
      // settings — and NOTHING has started.
      const card = page.locator('#mv-succession-card');
      await expect(card).toBeVisible({ timeout: 30_000 });
      await expect(page.locator('#mv-succession-kickoff')).toHaveText(KICKOFF);
      await expect(card).toContainText(`host/${rig.host}/opencode`);
      await expect(card).toContainText(MODEL);
      await expect(card).toContainText(REPO);
      await expect(card).toContainText('replace');
      const before = spawns().length;
      await page.waitForTimeout(500);
      expect(spawns().length, 'the proposal started nothing').toBe(before);
      await card.scrollIntoViewIfNeeded();
      await page.screenshot({ path: info.outputPath(`succession-card-${vp.key}.png`) });

      // THE TAP: the authorisation. The view then follows the successor.
      await tap('[data-succession-choice="Start it"]');
      await expect(page.locator('#mv-follow')).toBeVisible({ timeout: 30_000 });
      await expect(page).toHaveURL(/session=agents/, { timeout: 30_000 });
      const sp = spawns();
      expect(sp.length, 'exactly one spawn, on the tap').toBe(before + 1);
      const s = sp[sp.length - 1];
      expect(s.runtime).toBe('opencode');
      expect(s.repo).toBe(REPO);
      expect(s.model).toBe('openrouter/' + MODEL);
      // 🔴 BYTE FOR BYTE: the successor's first message holds the kickoff unchanged.
      expect(s.prompt).toContain('\n---\n' + KICKOFF + '\n---');
      expect(page.url()).toContain(`window=${encodeURIComponent('@' + s.dispatch)}`);
      // The successor's header names the session it continues (by muster's
      // name for it) and links back to it.
      await expect(page.locator('#mv-predecessor')).toContainText(/Continues “.+” · replace · open previous/, { timeout: 20_000 });
      await expect(page.locator('[data-predecessor-open]')).toHaveAttribute('href', predPath);
      await expect(page.locator('#mv-log')).toContainText('/resume — continue the fixture loader work.', { timeout: 20_000 });
      await page.screenshot({ path: info.outputPath(`succession-successor-${vp.key}.png`) });

      // Back: the predecessor, superseded — and no second jump.
      await page.goBack();
      await expect(page).toHaveURL(predURL);
      const banner = page.locator('#mv-succession [data-succession-state="running"]');
      await expect(banner).toContainText('Superseded.', { timeout: 20_000 });
      await expect(banner).toContainText('Continued in');
      await page.waitForTimeout(2500);
      await expect(page).toHaveURL(predURL);
      await page.screenshot({ path: info.outputPath(`succession-superseded-${vp.key}.png`) });
    });
  });
}
