import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { chiefInViewFakePort, chiefInViewPort, dsn, repoRoot } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';

// ASKING THE CHIEF FROM A SESSION'S VIEW (docs/design/chief-in-agent-view.md,
// phase 1), on the operator's click path: an opencode window's Rendered view
// (its agent composer DISABLED — the host did not opt in to the opencode send),
// the composer flipped to "Chief", a line typed and sent, and the chief's reply
// in the strip above the composer.
//
// ⚠ WHAT IS PLAYED: the model. A FAKE /v1/responses runtime (the e2eterm
// build's MUSTER_E2E_AGENT_ENDPOINT points every agent's endpoint at it) is the
// chief: on a turn that carries muster's "[muster] Your owner is viewing" header
// it calls session_context with the subject the HEADER names — muster's binding,
// not the page's — and answers with what that tool returned. Everything else is
// muster's own: the door, the stored line, the header, the tool, the real host
// reading the fake opencode server, and the strip. The chief's pod is a fixture
// row (e2e/pluginfix chief). Every value is invented fixture data.
//
// 🔴 REGRESSION TEST: on origin/main the view has no "Chief" switch at all
// (#mv-to), and POST /mirror/view/chief is not a route.

const stamp = Date.now().toString(36);
const chiefName = 'e2e-view-chief-' + stamp;
const chiefToken = 'e2e-view-chief-token-not-a-real-one-' + stamp;
const sid = 'ses_0e2eviewchief1a2b3c';
const KICKOFF_LINE = '/resume — continue the fixture loader work. Canonical handoff (read first): /tmp/e2e/handoff.md';

const rig = new MirrorRig(chiefInViewPort, 'e2e-cview', {
  MUSTER_FOCUS_INTERVAL: '0',
  MUSTER_AGENT_GATEWAY: 'hooks-sha256',
  MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
  MUSTER_AGENT_RUNTIME_CONFIG: '{}',
  MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
  MUSTER_AGENT_KINDS: 'gateway',
  MUSTER_AGENT_OPENROUTER_KEY: 'sk-or-v1-FAKE-e2e-not-a-real-key',
  MUSTER_E2E_AGENT_ENDPOINT: `http://127.0.0.1:${chiefInViewFakePort}`,
});
let runtime: http.Server | undefined;
let chiefMade = false;
let ops = '';

// What the fake chief was asked, for the assertions and a failure's log.
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

function script(body: { input?: { type?: string; content?: string; call_id?: string; output?: string }[] }): Out {
  const input = body.input ?? [];
  const first = input.find((i) => i.type === 'message')?.content ?? '';
  const outputs = input.filter((i) => i.type === 'function_call_output').map((i) => i.output ?? '');
  asked.push({ first, outputs });
  if (outputs.length) {
    // session_context's answer came back: say what it named.
    let r: { error?: string; subject?: string; runtime?: string; tail?: { ref: string; blocks?: { first_line: string }[] }[] } = {};
    try {
      r = JSON.parse(outputs[0]);
    } catch {}
    if (r.error) return { text: 'CHIEF-TOOL-ERROR: ' + r.error };
    const kick = (r.tail ?? []).find((m) => (m.blocks ?? []).length > 0);
    return { text: `CHIEF-SAW ${r.subject} (${r.runtime}) kickoff ${kick?.ref} block 0: ${kick?.blocks?.[0].first_line}` };
  }
  // The header names the subject; the chief reads it with the tool.
  const subject = /\nsubject: (\S+)/.exec(first)?.[1];
  if (first.startsWith('[muster] Your owner is viewing') && subject) {
    return { call: { id: 'call_ctx_' + asked.length, name: 'session_context', args: { subject } } };
  }
  return { text: 'GOT: ' + first.slice(-120) };
}

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

function op(o: Record<string, unknown>) {
  fs.appendFileSync(ops, JSON.stringify(o) + '\n');
}

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
  await new Promise<void>((r) => runtime!.listen(chiefInViewFakePort, '127.0.0.1', () => r()));
  await rig.start();
  const chiefID = fix('chief', '1', chiefName, chiefToken);
  chiefMade = true;
  const dir = fs.mkdtempSync(path.join(rig.dir, 'opencode-'));
  ops = path.join(dir, 'ops.jsonl');
  op({ op: 'session', id: sid, title: 'Fix the flaky fixture loader' });
  op({ op: 'message', session: sid, message: { id: 'msg_0cv01', type: 'user', text: 'write the handoff', files: [], agents: [], time: { created: 1700000000001 } } });
  op({
    op: 'message',
    session: sid,
    message: {
      id: 'msg_0cv02',
      type: 'assistant',
      agent: 'build',
      model: { id: 'fixture-model', providerID: 'fixture' },
      finish: 'stop',
      time: { created: 1, completed: 9 },
      content: [{ type: 'text', text: 'Handoff written. The kickoff:\n\n```\n' + KICKOFF_LINE + '\nRun the next rank.\n```\n' }],
    },
  });
  const t0 = Math.floor(Date.now() / 1000);
  const windows = path.join(rig.dir, 'windows');
  writeFileAtomic(windows, `plain|@1|0|1|zsh|${t0 - 50}|\n`);
  const ocWindows = path.join(rig.dir, 'opencode-windows');
  writeFileAtomic(ocWindows, `oc|@2|0|1|${t0 - 10}|OC | Fix the flaky fixture loader\n`);
  await rig.startHost({
    MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
    MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE: ocWindows,
    MUSTER_MIRROR_E2E_OPENCODE_DIR: dir,
    MUSTER_MIRROR_E2E_REFRESH: '200ms',
    // Interactive, but not opted in to the opencode send: the agent composer is
    // disabled by the runtime — the chief mode must work anyway.
    MUSTER_MIRROR_INTERACTIVE: '1',
  });
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await rig.login(page);
  await rig.waitListed(page, 'session=oc');
  // The chief's pod, on the noop driver: its row is the fixture's, and a chief
  // with no instance reads as stopped — a send to it is refused, as POST
  // /agents/{name}/send refuses one. Start it, as its page's Start does.
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
  console.log('strip:', (await page.locator('#mv-chief-log').innerText().catch(() => '(none)')).slice(-1500));
  console.log('muster-mirror log (tail):', rig.log.split('\n').slice(-30).join('\n'));
  console.log('server log (tail):', (rig.server?.log() ?? '').split('\n').slice(-40).join('\n'));
});

const view = () => `${rig.viewPath('oc')}&window=${encodeURIComponent('@2')}`;
const strip = (page: Page) => page.locator('#mv-chief-log');

for (const vp of [
  { name: 'phone 390×844', key: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', key: 'desktop', use: { viewport: { width: 1440, height: 900 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`on a ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('flip the composer to Chief on an opencode view, send, and the reply about THIS session lands in the strip', async ({ page }, info) => {
      test.setTimeout(120_000);
      await rig.login(page);
      await page.goto(rig.origin + view());
      await expect(page).toHaveURL(/\/mirror\/view\?/);
      // The session's own transcript is drawn.
      await expect(page.locator('#mv-log')).toContainText('Handoff written.', { timeout: 20_000 });
      // The agent composer is disabled (this host does not send to opencode)…
      const input = page.locator('#mv-input');
      await expect(input).toBeDisabled();
      // …and the switch is there, on the agent.
      const sw = page.locator('#mv-to');
      await expect(sw).toBeVisible();
      await expect(page.locator('#mv-to-agent')).toBeChecked();
      await expect(page.locator('#mv-chief')).toBeHidden();

      // Flip to Chief: the box is enabled, says where it goes, and the strip opens.
      await sw.getByText('Chief', { exact: true }).click();
      await expect(page.locator('#mv-to-chief')).toBeChecked();
      await expect(input).toBeEnabled();
      await expect(input).toHaveAttribute('placeholder', 'Ask the chief about this session…');
      await expect(page.locator('#mv-chief')).toBeVisible();
      await expect(page.locator('#mv-send-chief')).toBeVisible();
      await expect(page.locator('#mv-send')).toBeHidden();
      await expect(strip(page).locator('[data-chief-strip-empty]').or(strip(page).locator('[data-bubble]').first())).toBeVisible();

      const line = `E2E-ASK-${vp.key}-${stamp}: copy the kickoff out of this session's tail`;
      await input.fill(line);
      await input.press('Enter');
      // The owner's line is in the strip, and NOT drawn into the session's
      // own conversation (it is not the session's).
      await expect(strip(page).locator('[data-bubble="user"]').last()).toContainText(line, { timeout: 20_000 });
      await expect(page.locator('#mv-log')).not.toContainText(line);
      // The chief's reply, from session_context on the subject muster bound:
      // this window's opencode session, its kickoff block named by ref.
      await expect(strip(page)).toContainText(`CHIEF-SAW opencode:${sid} (opencode) kickoff m:`, { timeout: 30_000 });
      await expect(strip(page)).toContainText('block 0: ' + KICKOFF_LINE);
      // The chief was handed the line behind muster's header naming this session.
      const turn = asked.find((a) => a.first.endsWith('\n---\n' + line));
      expect(turn, 'the chief got the line as a turn').toBeTruthy();
      expect(turn!.first).toContain(`subject: opencode:${sid}   host: ${rig.host}   window: oc:0`);

      // The strip sits between the transcript and the box, within the screen.
      const s = await page.locator('#mv-chief').boundingBox();
      const box = await input.boundingBox();
      const vh = vp.use.viewport.height;
      expect(s && box && s.y + s.height <= box.y + 1, 'the strip is above the input').toBeTruthy();
      expect(s!.y >= 0 && box!.y + box!.height <= vh, 'the strip and the input are on screen').toBeTruthy();
      await page.screenshot({ path: info.outputPath(`chief-in-view-${vp.key}.png`) });

      // A reload lands in the mode last used for this window, the reply still there.
      await page.reload();
      await expect(page.locator('#mv-to-chief')).toBeChecked();
      await expect(input).toBeEnabled();
      await expect(strip(page)).toContainText(`CHIEF-SAW opencode:${sid}`, { timeout: 20_000 });
      // Back to the agent: disabled again, the strip gone.
      await sw.getByText('opencode', { exact: true }).click();
      await expect(input).toBeDisabled();
      await expect(page.locator('#mv-chief')).toBeHidden();
    });
  });
}
