import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { Browser, BrowserContext, Locator, Page } from '@playwright/test';
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
// 🔴 REGRESSION TEST: before PR #537 the view had no chief mode at all, and
// POST /mirror/view/chief was not a route. Before the toggle the mode was a
// two-radio "<agent> | Chief" switch above the row and the composer looked the
// same in both modes: the switch's assertions (one role="switch" checkbox,
// first in the row, its name per mode, the chief's colour, the mode working
// without script and when the chief script throws) fail there.

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
// A second host, opted in to the opencode send: its window's agent composer is
// ENABLED (the Enter-on-the-switch cases need a Send that can post).
const sendSid = 'ses_0e2eviewchiefsend9z';
let sendHost = '';
let stopSend: (() => Promise<void>) | undefined;
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
  const sendDir = fs.mkdtempSync(path.join(rig.dir, 'opencode-send-'));
  fs.writeFileSync(
    path.join(sendDir, 'ops.jsonl'),
    [
      { op: 'session', id: sendSid, title: 'Send-enabled fixture session' },
      { op: 'status', session: sendSid, status: 'idle' },
      { op: 'bind', session: 'ocs', window: '@2', id: sendSid },
    ]
      .map((o) => JSON.stringify(o))
      .join('\n') + '\n',
  );
  const sendPlain = path.join(rig.dir, 'windows-send');
  writeFileAtomic(sendPlain, `plainsend|@1|0|1|zsh|${t0 - 50}|\n`);
  const sendWindows = path.join(rig.dir, 'opencode-windows-send');
  writeFileAtomic(sendWindows, `ocs|@2|0|1|${t0 - 10}|OC | Send-enabled fixture session\n`);
  sendHost = rig.host + '-send';
  stopSend = rig.extraHost(sendHost, {
    MUSTER_MIRROR_E2E_WINDOWS_FILE: sendPlain,
    MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE: sendWindows,
    MUSTER_MIRROR_E2E_OPENCODE_DIR: sendDir,
    MUSTER_MIRROR_E2E_REFRESH: '200ms',
    MUSTER_MIRROR_INTERACTIVE: '1',
    MUSTER_MIRROR_OPENCODE_SEND: '1',
  });
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await rig.login(page);
  await rig.waitListed(page, 'session=oc');
  await rig.waitListed(page, 'session=ocs');
  // The chief's pod, on the noop driver: its row is the fixture's, and a chief
  // with no instance reads as stopped — a send to it is refused, as POST
  // /agents/{name}/send refuses one. Start it, as its page's Start does.
  const started = await page.request.post(`${rig.origin}/agents/${chiefID}/start`, { headers: { Origin: rig.origin, 'HX-Request': 'true' } });
  expect(started.status(), await started.text()).toBeLessThan(400);
  await ctx.close();
});

test.afterAll(async () => {
  try {
    if (stopSend) await stopSend();
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

// The switch's FIXED name; its checked state is the mode, and its description
// names the current target and the action.
const SWITCH_NAME = 'Send to the chief';
const AGENT_DESC = 'Sending to opencode — switch to the chief';
const CHIEF_DESC = 'Sending to the chief — switch to opencode';

// expectOneSwitch: the mode is ONE control — a checkbox with the switch role,
// inside #mv-to — and none of the old ones (radios, a button, a <noscript>
// duplicate) is drawn.
async function expectOneSwitch(page: Page) {
  await expect(page.getByRole('switch')).toHaveCount(1);
  await expect(page.locator('#mv-to #mv-to-chief[type="checkbox"][role="switch"]')).toHaveCount(1);
  await expect(page.locator('#mv-compose input[type="radio"], #mv-to-agent, #mv-to-ns, button#mv-to, #mv-form noscript')).toHaveCount(0);
}

// press is the operator's press on a control: a tap on a touch screen.
const press = (l: Locator, touch: boolean) => (touch ? l.tap() : l.click());

// expectToggleFirst: the toggle is the composer row's FIRST control — left of
// every other shown control of the form (the paperclip, the input, the mic,
// Send, the ⋯) — and a 44px target.
async function expectToggleFirst(page: Page) {
  const boxes = await page.locator('#mv-form').evaluate((f) =>
    Array.from(f.querySelectorAll('button, a, textarea, label'))
      .filter((e) => (e as HTMLElement).offsetParent !== null && e.getBoundingClientRect().width > 0)
      .map((e) => {
        const r = e.getBoundingClientRect();
        return { id: e.id, x: r.x, r: r.right, w: r.width, h: r.height };
      }),
  );
  expect(boxes.length, JSON.stringify(boxes)).toBeGreaterThan(2);
  const [first, ...rest] = [...boxes].sort((a, b) => a.x - b.x);
  expect(first.id, JSON.stringify(boxes)).toBe('mv-to');
  for (const b of rest) expect(b.x, `${b.id} starts after the toggle ends`).toBeGreaterThanOrEqual(first.r);
  expect(first.w).toBeGreaterThanOrEqual(44);
  expect(first.h).toBeGreaterThanOrEqual(44);
}

// expectOneSend: exactly one submit button of the form is shown — id.
async function expectOneSend(page: Page, id: string) {
  const shown = page.locator('#mv-form button[type="submit"]:visible');
  await expect(shown).toHaveCount(1);
  await expect(shown).toHaveId(id.slice(1));
}

// look is the composer's mode-bearing computed style: its top edge, the box's
// fill and ring, the toggle's fill, and the shown Send's fill.
type Look = { edge: string; box: string; ring: string; toggle: string; send: string };
function look(page: Page): Promise<Look> {
  return page.evaluate(() => {
    const cs = (sel: string) => getComputedStyle(document.querySelector(sel) as Element);
    const send = Array.from(document.querySelectorAll('#mv-form button[type="submit"]')).find((b) => (b as HTMLElement).offsetParent !== null);
    return {
      edge: cs('#mv-compose').borderTopColor,
      box: cs('#mv-input').backgroundColor,
      ring: cs('#mv-input').boxShadow,
      toggle: cs('#mv-to').backgroundColor,
      send: send ? getComputedStyle(send).backgroundColor : '',
    };
  });
}

// expectChiefLook: chief mode is UNMISTAKABLE — the composer's edge, the box's
// fill and ring, the toggle and Send all differ from agent mode's, and each is
// the theme's chief colour (the review status token pair).
async function expectChiefLook(page: Page, agent: Look) {
  const tok = await page.evaluate(() => {
    const v = (n: string) => `rgb(${getComputedStyle(document.documentElement).getPropertyValue(n).trim().split(/\s+/).join(', ')})`;
    return { fg: v('--mu-st-review-fg'), bg: v('--mu-st-review-bg') };
  });
  await expect(page.locator('#mv-to-chief')).toBeChecked();
  // The toggle's fill eases in (its `transition`): read once it has settled.
  await expect.poll(async () => (await look(page)).toggle).toBe(tok.bg);
  const chief = await look(page);
  expect(chief.edge).toBe(tok.fg);
  expect(chief.box).toBe(tok.bg);
  expect(chief.ring).toContain(tok.fg);
  expect(chief.toggle).toBe(tok.bg);
  expect(chief.send).toBe(tok.fg);
  for (const k of ['edge', 'box', 'ring', 'toggle', 'send'] as const) {
    expect(chief[k], `chief mode's ${k} differs from agent mode's`).not.toBe(agent[k]);
  }
}

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
      // …and the toggle is there, on the agent: ONE button, the row's FIRST
      // control, a 44px target, named by its current target and the action.
      const sw = page.locator('#mv-to');
      const cb = page.getByRole('switch');
      await expect(sw).toBeVisible();
      await expectOneSwitch(page);
      await expect(cb).not.toBeChecked();
      await expect(cb).toHaveAccessibleName(SWITCH_NAME);
      await expect(cb).toHaveAccessibleDescription(AGENT_DESC);
      await expect(page.locator('#mv-chief')).toBeHidden();
      await expectToggleFirst(page);
      const agentLook = await look(page);
      await expectOneSend(page, '#mv-send');
      // The toggle and the row fit the screen: no sideways scroll.
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await page.screenshot({ path: info.outputPath(`agent-mode-${vp.key}.png`) });

      // Flip to the chief: the toggle pressed, the box enabled, says where it
      // goes, the form posts to the chief, the strip opens, and the composer
      // wears the chief's colour.
      await press(sw, vp.use.hasTouch);
      await expect(cb).toBeChecked();
      await expect(cb).toHaveAccessibleName(SWITCH_NAME);
      await expect(cb).toHaveAccessibleDescription(CHIEF_DESC);
      await expect(input).toBeEnabled();
      await expect(input).toHaveAttribute('placeholder', 'Ask the chief about this session…');
      await expect(page.locator('#mv-form')).toHaveAttribute('action', /^\/mirror\/view\/chief\?/);
      await expect(page.locator('#mv-chief')).toBeVisible();
      await expect(page.locator('#mv-send-chief')).toBeVisible();
      await expect(page.locator('#mv-send')).toBeHidden();
      await expectOneSend(page, '#mv-send-chief');
      await expectChiefLook(page, agentLook);
      await expectToggleFirst(page);
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
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

      // A reload lands in the mode last used for this window, the reply still
      // there, the composer still in the chief's colour.
      await page.reload();
      await expect(cb).toBeChecked();
      await expect(cb).toHaveAccessibleName(SWITCH_NAME);
      await expect(cb).toHaveAccessibleDescription(CHIEF_DESC);
      await expect(input).toBeEnabled();
      await expect(strip(page)).toContainText(`CHIEF-SAW opencode:${sid}`, { timeout: 20_000 });
      await expectChiefLook(page, agentLook);
      // Back to the agent: disabled again, the strip gone, the form posting to
      // the agent, and the composer exactly as it was.
      await press(sw, vp.use.hasTouch);
      await expect(cb).not.toBeChecked();
      await expect(cb).toHaveAccessibleName(SWITCH_NAME);
      await expect(cb).toHaveAccessibleDescription(AGENT_DESC);
      await expect(input).toBeDisabled();
      await expect(page.locator('#mv-chief')).toBeHidden();
      await expect(page.locator('#mv-form')).not.toHaveAttribute('action', /\/mirror\/view\/chief/);
      await expectOneSend(page, '#mv-send');
      await expect.poll(() => look(page)).toEqual(agentLook);
      // …and that, too, is remembered.
      await page.reload();
      await expect(cb).not.toBeChecked();
      await expect.poll(() => look(page)).toEqual(agentLook);
    });
  });
}

// postsTo records the page's POSTs, by path, from now on.
function postsTo(page: Page): string[] {
  const seen: string[] = [];
  page.on('request', (r) => {
    if (r.method() === 'POST') seen.push(new URL(r.url()).pathname);
  });
  return seen;
}
const AGENT_ROUTE = '/mirror/view/send';
const CHIEF_ROUTE = '/mirror/view/chief';
// throwChiefScript makes the chief script throw on its first lookup — and only
// it: the composer's own script and every other one run.
async function throwChiefScript(ctx: BrowserContext) {
  await ctx.addInitScript(() => {
    const get = Document.prototype.getElementById;
    Document.prototype.getElementById = function (id: string) {
      if (id === 'mv-chief-log') throw new Error('E2E-CHIEF-SCRIPT-THREW');
      return get.call(this, id);
    };
  });
}
// settle gives a stray submit time to be posted before an absence is read.
const settle = (page: Page) => page.waitForTimeout(1500);

// ENTER ON THE FOCUSED SWITCH NEVER SENDS — it toggles the switch, as Space
// does. A checkbox in a form submits the form on Enter (implicit submission)
// through its default button: before the fix that was the agent's #mv-send,
// so Enter on the switch posted the box to the pane. On a window whose agent
// composer is ENABLED (the send host), with the chief script working and with
// it thrown. Positive control: the agent's own Send does post to the agent
// route, through the same recorder.
for (const mode of ['script runs', 'chief script throws'] as const) {
  test(`Enter on the focused switch toggles it and sends nothing (${mode})`, async ({ browser }) => {
    test.setTimeout(90_000);
    const ctx = await browser.newContext({
      viewport: { width: 1440, height: 900 },
      storageState: { cookies: await signedInCookies(browser), origins: [] },
    });
    try {
      if (mode === 'chief script throws') await throwChiefScript(ctx);
      const page = await ctx.newPage();
      const errors: string[] = [];
      page.on('pageerror', (e) => errors.push(e.message));
      await page.goto(`${rig.origin}/mirror/view?host=${encodeURIComponent(sendHost)}&session=ocs&window=${encodeURIComponent('@2')}`);
      const input = page.locator('#mv-input');
      await expect(input).toBeEnabled({ timeout: 20_000 });
      await expect(input).not.toHaveAttribute('data-agent-off', /.*/);
      const cb = page.getByRole('switch');
      await expect(cb).not.toBeChecked();
      if (mode === 'chief script throws') await expect.poll(() => errors.join('|')).toContain('E2E-CHIEF-SCRIPT-THREW');
      const posts = postsTo(page);
      const line = `E2E-ENTER-SWITCH-${mode.replace(/ /g, '-')}-${stamp}`;
      await input.fill(line);
      // Agent mode: Enter on the switch flips it to the chief, posts nothing.
      await cb.focus();
      await cb.press('Enter');
      await settle(page);
      expect(posts, 'Enter on the switch in agent mode posted').toEqual([]);
      await expect(cb).toBeChecked();
      await expectOneSend(page, '#mv-send-chief');
      // Chief mode: Enter flips it back, posts nothing.
      await cb.press('Enter');
      await settle(page);
      expect(posts, 'Enter on the switch in chief mode posted').toEqual([]);
      await expect(cb).not.toBeChecked();
      await expect(input).toHaveValue(line);
      // Positive control: the agent's Send posts, and the recorder sees it.
      await page.locator('#mv-send').click();
      await expect.poll(() => posts).toContain(AGENT_ROUTE);
      expect(errors.filter((m) => !m.includes('E2E-CHIEF-SCRIPT-THREW')), 'no other script failed').toEqual([]);
    } finally {
      await ctx.close();
    }
  });
}

// A DISABLED AGENT COMPOSER SENDS NOTHING TO THE AGENT in agent mode, though
// its box is drawn enabled for the chief's sake — with the chief script thrown
// (Enter in the box, a press on the disabled Send) and with no script (the
// same, plus Enter on the switch). Read at the browser's request log: no POST
// at all, so none to the agent route.
for (const mode of ['chief script throws', 'script disabled'] as const) {
  test(`agent mode on a disabled agent composer posts nothing (${mode})`, async ({ browser }) => {
    test.setTimeout(90_000);
    const ctx = await browser.newContext({
      javaScriptEnabled: mode !== 'script disabled',
      viewport: { width: 1440, height: 900 },
      storageState: { cookies: await signedInCookies(browser), origins: [] },
    });
    try {
      if (mode === 'chief script throws') await throwChiefScript(ctx);
      const page = await ctx.newPage();
      const errors: string[] = [];
      page.on('pageerror', (e) => errors.push(e.message));
      await page.goto(rig.origin + view());
      if (mode === 'chief script throws') await expect.poll(() => errors.join('|')).toContain('E2E-CHIEF-SCRIPT-THREW');
      const cb = page.getByRole('switch');
      await expect(cb).not.toBeChecked();
      const input = page.locator('#mv-input');
      await expect(input).toHaveAttribute('data-agent-off', '');
      await expect(page.locator('#mv-send')).toBeDisabled();
      const posts = postsTo(page);
      await input.fill(`E2E-AGENT-OFF-${mode.replace(/ /g, '-')}-${stamp}`);
      await input.press('Enter');
      await page.locator('#mv-send').click({ force: true });
      await cb.focus();
      await cb.press('Enter');
      await settle(page);
      expect(posts, 'a POST left the page').toEqual([]);
      expect(posts).not.toContain(AGENT_ROUTE);
      if (mode === 'script disabled') await expect(cb).not.toBeChecked();
      else await expect(cb).toBeChecked();
    } finally {
      await ctx.close();
    }
  });
}

// THE MODE NEEDS NO SCRIPT. The switch is a checkbox the stylesheet follows,
// so the mode works (a) with script DISABLED — the chief's Send, whose
// formaction is the chief's door, is the one shown, and a native submit
// reaches the chief — and (b) when the chief script THROWS (the composer's own
// script still runs): Enter submits through the Send shown. On this view the
// agent composer is disabled, so its box is drawn enabled-and-marked for the
// chief's sake.
async function signedInCookies(browser: Browser) {
  const js = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  await rig.login(await js.newPage());
  const cookies = await js.cookies();
  await js.close();
  return cookies;
}

// chiefTurns counts the fake chief's turns that carry line.
const chiefTurns = (line: string) => asked.filter((a) => a.first.endsWith('\n---\n' + line)).length;

test('(a) script disabled: the switch flips the mode by itself and the chief Send reaches the chief', async ({ browser }) => {
  test.setTimeout(90_000);
  const ctx = await browser.newContext({
    javaScriptEnabled: false,
    viewport: { width: 390, height: 844 },
    storageState: { cookies: await signedInCookies(browser), origins: [] },
  });
  try {
    const page = await ctx.newPage();
    await page.goto(rig.origin + view());
    await expectOneSwitch(page);
    const cb = page.getByRole('switch');
    await expect(cb).not.toBeChecked();
    await expect(cb).toHaveAccessibleName(SWITCH_NAME);
      await expect(cb).toHaveAccessibleDescription(AGENT_DESC);
    await expectToggleFirst(page);
    const agentLook = await look(page);
    await expectOneSend(page, '#mv-send');
    await page.locator('#mv-to').click();
    await expect(cb).toBeChecked();
    await expect(cb).toHaveAccessibleName(SWITCH_NAME);
      await expect(cb).toHaveAccessibleDescription(CHIEF_DESC);
    await expectOneSend(page, '#mv-send-chief');
    await expectChiefLook(page, agentLook);
    await expect(page.locator('#mv-chief')).toBeVisible();
    const line = `E2E-NOSCRIPT-${stamp}`;
    // The disabled composer's box is drawn enabled for the chief's sake.
    await expect(page.locator('#mv-input')).toBeEnabled();
    await page.locator('#mv-input').fill(line);
    const posted = page.waitForRequest((r) => r.method() === 'POST' && new URL(r.url()).pathname === '/mirror/view/chief', { timeout: 15_000 });
    await page.locator('#mv-send-chief').click();
    await posted;
    await expect.poll(() => chiefTurns(line), { timeout: 30_000 }).toBe(1);
  } finally {
    await ctx.close();
  }
});

test('(b) the chief script throws: the switch still flips the mode, and Enter reaches the chief', async ({ browser }) => {
  test.setTimeout(90_000);
  const ctx = await browser.newContext({
    viewport: { width: 1440, height: 900 },
    storageState: { cookies: await signedInCookies(browser), origins: [] },
  });
  try {
    await throwChiefScript(ctx);
    const page = await ctx.newPage();
    const errors: string[] = [];
    page.on('pageerror', (e) => errors.push(e.message));
    await page.goto(rig.origin + view());
    await expect.poll(() => errors.join('|')).toContain('E2E-CHIEF-SCRIPT-THREW');
    await expectOneSwitch(page);
    const cb = page.getByRole('switch');
    await expect(cb).not.toBeChecked();
    await expectToggleFirst(page);
    const agentLook = await look(page);
    await expectOneSend(page, '#mv-send');
    await page.locator('#mv-to').click();
    await expect(cb).toBeChecked();
    await expect(cb).toHaveAccessibleName(SWITCH_NAME);
      await expect(cb).toHaveAccessibleDescription(CHIEF_DESC);
    await expectOneSend(page, '#mv-send-chief');
    await expectChiefLook(page, agentLook);
    // The chief script never set the form's action: it is still the agent's.
    await expect(page.locator('#mv-form')).not.toHaveAttribute('action', /\/mirror\/view\/chief/);
    const line = `E2E-THREW-${stamp}`;
    const input = page.locator('#mv-input');
    await expect(input).toBeEnabled();
    await input.fill(line);
    const posted = page.waitForRequest((r) => r.method() === 'POST' && new URL(r.url()).pathname === '/mirror/view/chief', { timeout: 15_000 });
    await input.press('Enter');
    await posted;
    await expect.poll(() => chiefTurns(line), { timeout: 30_000 }).toBe(1);
    expect(errors.filter((m) => !m.includes('E2E-CHIEF-SCRIPT-THREW')), 'no other script failed').toEqual([]);
  } finally {
    await ctx.close();
  }
});

// THE BROWSER'S OWN SUBMIT CARRIES A CHIEF LINE TO THE CHIEF. When the
// composer's post fails (a network error), it hands the line to the browser's
// native form submit (composer_pending.go native()); that submit must go to
// the line's own route — with the chief script thrown, the form's action was
// never moved off the agent's route. The fetch is failed here; the document
// POST that follows is read at the request log.
test('(c) a chief line whose post fails is resubmitted by the browser to the chief route, not the agent route', async ({ browser }) => {
  test.setTimeout(90_000);
  const ctx = await browser.newContext({
    viewport: { width: 1440, height: 900 },
    storageState: { cookies: await signedInCookies(browser), origins: [] },
  });
  try {
    await throwChiefScript(ctx);
    const page = await ctx.newPage();
    await page.route('**/mirror/view/chief**', (r) => (r.request().resourceType() === 'fetch' ? r.abort() : r.continue()));
    const errors: string[] = [];
    page.on('pageerror', (e) => errors.push(e.message));
    await page.goto(rig.origin + view());
    await expect.poll(() => errors.join('|')).toContain('E2E-CHIEF-SCRIPT-THREW');
    const docs: string[] = [];
    page.on('request', (r) => {
      if (r.method() === 'POST' && r.resourceType() === 'document') docs.push(new URL(r.url()).pathname);
    });
    await page.locator('#mv-to').click();
    await expect(page.getByRole('switch')).toBeChecked();
    const line = `E2E-NATIVE-${stamp}`;
    const input = page.locator('#mv-input');
    await expect(input).toBeEnabled();
    await input.fill(line);
    await input.press('Enter');
    await expect.poll(() => docs, { timeout: 15_000 }).not.toEqual([]);
    expect(docs, 'the browser resubmitted to').toEqual([CHIEF_ROUTE]);
    await expect.poll(() => chiefTurns(line), { timeout: 30_000 }).toBe(1);
  } finally {
    await ctx.close();
  }
});
