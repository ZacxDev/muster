import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { Locator, Page } from '@playwright/test';
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
// same in both modes: the toggle's assertions (one button, first in the row,
// aria-pressed, the chief's colour) fail there.

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
  await expect(page.locator('#mv-compose')).toHaveAttribute('data-to', 'chief');
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
      await expect(sw).toBeVisible();
      await expect(sw).toHaveJSProperty('tagName', 'BUTTON');
      await expect(sw).toHaveAttribute('type', 'button');
      await expect(sw).toHaveAttribute('aria-pressed', 'false');
      await expect(sw).toHaveAttribute('aria-label', 'Sending to opencode — switch to the chief');
      await expect(page.locator('#mv-to-agent, #mv-compose input[type="radio"]')).toHaveCount(0);
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
      await expect(sw).toHaveAttribute('aria-pressed', 'true');
      await expect(sw).toHaveAttribute('aria-label', 'Sending to the chief — switch to opencode');
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
      await expect(sw).toHaveAttribute('aria-pressed', 'true');
      await expect(input).toBeEnabled();
      await expect(strip(page)).toContainText(`CHIEF-SAW opencode:${sid}`, { timeout: 20_000 });
      await expectChiefLook(page, agentLook);
      // Back to the agent: disabled again, the strip gone, the form posting to
      // the agent, and the composer exactly as it was.
      await press(sw, vp.use.hasTouch);
      await expect(sw).toHaveAttribute('aria-pressed', 'false');
      await expect(sw).toHaveAttribute('aria-label', 'Sending to opencode — switch to the chief');
      await expect(input).toBeDisabled();
      await expect(page.locator('#mv-chief')).toBeHidden();
      await expect(page.locator('#mv-form')).not.toHaveAttribute('action', /\/mirror\/view\/chief/);
      await expectOneSend(page, '#mv-send');
      await expect.poll(() => look(page)).toEqual(agentLook);
      // …and that, too, is remembered.
      await page.reload();
      await expect(sw).toHaveAttribute('aria-pressed', 'false');
      await expect(page.locator('#mv-compose')).toHaveAttribute('data-to', 'agent');
      await expect.poll(() => look(page)).toEqual(agentLook);
    });
  });
}

// WITHOUT SCRIPT the toggle cannot toggle anything: it is hidden, and a
// <noscript> checkbox drawn in its place (#mv-to-ns) puts the composer in chief
// mode by the stylesheet alone — the chief's Send (whose formaction is the
// chief's door) shown, the agent's hidden, the chief's colour on the composer.
test('without script the no-script checkbox switches the Send and the colour', async ({ browser }) => {
  const js = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  await rig.login(await js.newPage());
  const cookies = await js.cookies();
  await js.close();
  const ctx = await browser.newContext({ javaScriptEnabled: false, viewport: { width: 390, height: 844 }, storageState: { cookies, origins: [] } });
  try {
    const page = await ctx.newPage();
    await page.goto(rig.origin + view());
    await expect(page.locator('#mv-to')).toBeHidden();
    const ns = page.locator('#mv-to-ns');
    await expect(ns).toBeVisible();
    await expectOneSend(page, '#mv-send');
    const edge = () => page.locator('#mv-compose').evaluate((e) => getComputedStyle(e).borderTopColor);
    const before = await edge();
    await ns.click();
    await expect(page.locator('#mv-to-chief')).toBeChecked();
    await expectOneSend(page, '#mv-send-chief');
    await expect(page.locator('#mv-send-chief')).toHaveAttribute('formaction', /^\/mirror\/view\/chief\?/);
    expect(await edge()).not.toBe(before);
  } finally {
    await ctx.close();
  }
});
