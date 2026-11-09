import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { test, expect } from '../lib/fixtures';
import { chiefEndSessionFakePort, chiefEndSessionPort, dsn, repoRoot } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';

// ENDING A SESSION FROM ITS VIEW (docs/design/chief-in-agent-view.md,
// "End-session"), on the operator's click path: an opencode window's Rendered
// view, the composer flipped to "Chief", "close this agent session", the CARD
// muster raises, the owner's TAP — the authorisation — and the window gone,
// the view saying the session ended.
//
// ⚠ WHAT IS PLAYED: the model (a FAKE /v1/responses runtime is the chief: on a
// view turn whose owner line asks to close, it calls end_session_propose with
// the subject muster's header names) and tmux (the e2emirror build's
// MUSTER_MIRROR_E2E_CLOSE: a window can be closed, and an agent typed /exit
// exits with its window). Everything else is muster's own, and the HOST's own
// agent-close code: its listing check, its /exit, its wait, its answer.
// Every value is invented fixture data.
//
// 🔴 REGRESSION TEST: on its base branch the chief has no end_session_propose,
// no card is drawn, and the host is never asked to close anything.

const stamp = Date.now().toString(36);
const chiefName = 'e2e-end-chief-' + stamp;
const chiefToken = 'e2e-end-chief-token-not-a-real-one-' + stamp;

const VIEWS = {
  phone: { sid: 'ses_0e2eendphone1a2b3c4', win: '@2', title: 'Retire the old fixture loader' },
  desktop: { sid: 'ses_0e2eenddesk1a2b3c45', win: '@3', title: 'Retire the old widget parser' },
};

const rig = new MirrorRig(chiefEndSessionPort, 'e2e-cend', {
  MUSTER_FOCUS_INTERVAL: '0',
  MUSTER_AGENT_GATEWAY: 'hooks-sha256',
  MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
  MUSTER_AGENT_RUNTIME_CONFIG: '{}',
  MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
  MUSTER_AGENT_KINDS: 'gateway',
  MUSTER_AGENT_OPENROUTER_KEY: 'sk-or-v1-FAKE-e2e-not-a-real-key',
  MUSTER_E2E_AGENT_ENDPOINT: `http://127.0.0.1:${chiefEndSessionFakePort}`,
  MUSTER_PLUGINS: '1',
  MUSTER_TASK_ROUTING: '1',
  MUSTER_CHIEF_DISPATCH_OWNERS: '1',
  // The card is an attention ask, which asks for suggested replies: pointed at
  // the fake runtime (which answers 404), so nothing real is ever called.
  MUSTER_E2E_SUGGEST_URL: `http://127.0.0.1:${chiefEndSessionFakePort}/v1/chat/completions`,
});
let runtime: http.Server | undefined;
let chiefMade = false;

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

// The scripted chief: a view line asking to close → end_session_propose with
// the subject the header names.
function script(body: { input?: { type?: string; content?: string; output?: string }[] }): Out {
  const input = body.input ?? [];
  const first = input.find((i) => i.type === 'message')?.content ?? '';
  const outputs = input.filter((i) => i.type === 'function_call_output').map((i) => i.output ?? '');
  asked.push({ first, outputs });
  if (outputs.length) return { text: 'CHIEF-PROPOSED-END: ' + outputs[0].slice(0, 200) };
  const subject = /\nsubject: (\S+)/.exec(first)?.[1];
  const line = first.split('\n---\n').pop() ?? '';
  if (first.startsWith('[muster] Your owner is viewing') && subject && first.includes('end_session_propose') && /close/i.test(line)) {
    return { call: { id: 'call_end_' + asked.length, name: 'end_session_propose', args: { session: subject } } };
  }
  return { text: 'GOT: ' + first.slice(-120) };
}

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

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
  await new Promise<void>((r) => runtime!.listen(chiefEndSessionFakePort, '127.0.0.1', () => r()));
  await rig.start();
  const chiefID = fix('chief', '1', chiefName, chiefToken);
  chiefMade = true;
  const dir = fs.mkdtempSync(path.join(rig.dir, 'opencode-'));
  const ops = path.join(dir, 'ops.jsonl');
  let n = 0;
  for (const v of Object.values(VIEWS)) {
    n++;
    for (const l of [
      { op: 'session', id: v.sid, title: v.title },
      { op: 'message', session: v.sid, message: { id: `msg_0ce${n}1`, type: 'user', text: 'are we done?', files: [], agents: [], time: { created: 1700000000001 } } },
      {
        op: 'message',
        session: v.sid,
        message: {
          id: `msg_0ce${n}2`,
          type: 'assistant',
          agent: 'build',
          model: { id: 'fixture-model', providerID: 'fixture' },
          finish: 'stop',
          time: { created: 1, completed: 9 },
          content: [{ type: 'text', text: 'All done here.' }],
        },
      },
    ])
      fs.appendFileSync(ops, JSON.stringify(l) + '\n');
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
    MUSTER_MIRROR_E2E_CLOSE: '1',
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
  }
});

test.afterEach(async ({ page }, info) => {
  if (info.status === info.expectedStatus) return;
  console.log('fake chief asked (last 4):', JSON.stringify(asked.slice(-4), null, 1).slice(0, 4000));
  console.log('page:', page.url());
  console.log('region:', (await page.locator('#mv-succession').evaluate((e) => e.outerHTML).catch(() => '(none)')).slice(-1500));
  console.log('muster-mirror log (tail):', rig.log.split('\n').slice(-30).join('\n'));
  console.log('server log (tail):', (rig.server?.log() ?? '').split('\n').slice(-60).join('\n'));
});

// listed is whether the host lists window win of session oc now (the
// sidebar's read of the hub).
async function listed(page: import('@playwright/test').Page, win: string): Promise<boolean> {
  const html = await (await page.request.get(rig.origin + '/ui/nav/agents')).text();
  return html.includes(`window=${encodeURIComponent(win)}`) && html.includes('session=oc');
}

for (const vp of [
  { name: 'phone 390×844', key: 'phone' as const, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', key: 'desktop' as const, use: { viewport: { width: 1440, height: 900 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`on a ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('ask the chief to close this session → the card → one tap → the window is gone and the view says it ended', async ({ page }, info) => {
      test.setTimeout(120_000);
      const v = VIEWS[vp.key];
      await rig.login(page);
      await page.goto(rig.origin + `${rig.viewPath('oc')}&window=${encodeURIComponent(v.win)}`);
      await expect(page.locator('#mv-log')).toContainText('All done here.', { timeout: 20_000 });
      expect(await listed(page, v.win), 'the window is listed before').toBe(true);

      await page.locator('#mv-to').getByText('Chief', { exact: true }).click();
      const input = page.locator('#mv-input');
      await expect(input).toBeEnabled();
      await input.fill(`E2E-END-${vp.key}-${stamp}: close this agent session`);
      await input.press('Enter');

      // THE CARD, on this view — and nothing has closed.
      const card = page.locator('#mv-proposal-card[data-proposal-kind="end_session"]');
      await expect(card).toBeVisible({ timeout: 30_000 });
      await expect(card).toContainText('End this session?');
      await expect(card).toContainText('asks opencode to exit');
      await page.waitForTimeout(500);
      expect(await listed(page, v.win), 'the proposal closed nothing').toBe(true);
      expect(rig.log).not.toContain('CLOSED oc:' + v.win);
      await page.screenshot({ path: info.outputPath(`end-card-${vp.key}.png`) });

      // THE TAP.
      const btn = page.locator('[data-proposal-choice="End it"]');
      await (vp.use.hasTouch ? btn.tap() : btn.click());
      await expect(page.locator('#mv-succession')).toContainText('Session ended.', { timeout: 30_000 });
      await expect(page.locator('#mv-succession')).toContainText('the agent exited and its window closed');
      await expect.poll(() => listed(page, v.win), { timeout: 15_000, message: 'the window is gone from the listing' }).toBe(false);
      expect(rig.log).toContain(`CLOSED oc:${v.win} on muster's request (graceful: true)`);
      await page.screenshot({ path: info.outputPath(`end-ended-${vp.key}.png`) });
    });
  });
}
