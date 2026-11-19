import http from 'node:http';
import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { chiefUxConnectPort, chiefUxFakePort, chiefUxPort, dsn, hookToken, repoRoot, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { clsInit, readCls, settle } from '../lib/cls';
import path from 'node:path';

// THE CHIEF'S CHAT, AS AN OPERATOR REPORTED IT (the four complaints, one each):
//
//   1. "we reloaded the page, and Chief's working indicator disappeared. It did
//      respond." — the page draws "working…" from the server's own mark, so a
//      reload mid-turn shows it, and it goes when the reply is stored. The turn
//      is never cancelled by the reload.
//   2. "it asked me if I wanted to use gateway or Claude Code, which is
//      confusing" — a dispatch with no model is refused with the choices in the
//      owner's words, and the chief asks with them.
//   3. "it should use the Ask User Question tool so that I can just tap an
//      option" — ask_owner draws a question card; a tap (or an Other answer) is
//      sent as the owner's next message, and the card shows it answered, after
//      a reload too.
//   4. "when it dispatches an agent, there should be an enriched card … so I can
//      tap on that card to view the agent" — the start is drawn as the agent's
//      card, its state follows the agent, and a tap opens the agent's page.
//
// ⚠ WHAT IS PLAYED: the model. A FAKE /v1/responses runtime (the e2eterm build's
// MUSTER_E2E_AGENT_ENDPOINT points every agent's endpoint at it) answers each
// turn by a script keyed on the owner's words, and calls muster's tools exactly
// as a gateway chief would — ask_owner, agent_dispatch — so every tool, every
// refusal, the stored transcript, the cards, the socket and the started agent's
// first turn are muster's own. The chief's pod is a fixture row.

const origin = `http://localhost:${chiefUxPort}`;
const connectURL = `http://127.0.0.1:${chiefUxConnectPort}`;
const stamp = Date.now().toString(36);
const chiefName = 'e2e-ux-chief-' + stamp;
const chiefToken = 'e2e-ux-chief-token-not-a-real-one-' + stamp;
const KICKOFF = 'E2E-KICKOFF-' + stamp;

let server: Running | undefined;
let runtime: http.Server | undefined;
let chiefMade = false;
const tasksMade: number[] = [];
const agentsMade = new Set<string>();

// --- the fake runtime ----------------------------------------------------------

// A gate the test opens: a held turn waits on it.
class Gate {
  private waiters: (() => void)[] = [];
  private open = false;
  entered = 0;
  wait(): Promise<void> {
    this.entered++;
    if (this.open) return Promise.resolve();
    return new Promise((r) => this.waiters.push(r));
  }
  release() {
    this.open = true;
    for (const w of this.waiters.splice(0)) w();
  }
}
const gates = new Map<string, Gate>();
const gate = (k: string) => {
  if (!gates.has(k)) gates.set(k, new Gate());
  return gates.get(k)!;
};
// What the runtime was asked and what it answered, for the assertions.
const requests: { first: string; outputs: { call_id: string; output: string }[]; toolChoice?: unknown }[] = [];

const sse = (type: string, data: Record<string, unknown>) => `event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`;

type Out = {
  text?: string;
  call?: { id: string; name: string; args: Record<string, unknown> };
  // The turn's tokens, in ccd's (Anthropic) split: what the chief chat's
  // context chip reads back from the turn's usage row.
  usage?: { input_tokens: number; output_tokens: number; cache_read_input_tokens: number; cache_creation_input_tokens: number };
};

function answer(res: http.ServerResponse, out: Out) {
  res.setHeader('Content-Type', 'text/event-stream');
  const output: unknown[] = [];
  if (out.text) {
    res.write(sse('response.output_text.delta', { delta: out.text, output_index: 0 }));
    output.push({ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: out.text }] });
  }
  if (out.call) {
    output.push({ type: 'function_call', call_id: out.call.id, name: out.call.name, arguments: JSON.stringify(out.call.args) });
  }
  res.end(sse('response.completed', { response: { status: 'completed', output, ...(out.usage ? { usage: out.usage } : {}) } }));
}

// The OpenRouter models the fake chief offers beside Claude.
const MODELS = ['deepseek/deepseek-v3', 'qwen/qwen3-coder'];

async function script(body: { input?: { type?: string; content?: string; call_id?: string; output?: string }[]; tool_choice?: unknown }): Promise<Out> {
  const input = body.input ?? [];
  const first = input.find((i) => i.type === 'message')?.content ?? '';
  const outputs = input.filter((i) => i.type === 'function_call_output').map((i) => ({ call_id: i.call_id ?? '', output: i.output ?? '' }));
  requests.push({ first, outputs, toolChoice: body.tool_choice });

  // A round of tool outputs coming back.
  if (outputs.length) {
    const o = outputs[0];
    if (o.call_id.startsWith('call_ask')) {
      // ask_owner ended the turn and its output came back. This runtime tries to
      // GO ON with another tool: muster must not run it (the loop's own gate).
      return { call: { id: 'call_after_ask', name: 'agent_dispatch', args: { target: 'pod/gateway', prompt: 'went on', request_key: 'k-after-' + requests.length, owner_asked: true } } };
    }
    if (o.call_id === 'call_nomodel') {
      // The refusal of a dispatch with no target: ask the owner, offering its
      // choices in their words — Claude, or an OpenRouter model.
      let r: { error?: string; targets?: { choices?: { target: string; label: string }[] } } = {};
      try {
        r = JSON.parse(o.output);
      } catch {}
      const choices = r.targets?.choices ?? [];
      if (!r.error || !choices.length) return { text: 'NO-CHOICES: ' + o.output.slice(0, 200) };
      const options: { label: string; description: string }[] = [];
      for (const c of choices) {
        if (c.target === 'pod/claude-code') options.push({ label: 'Claude', description: c.label });
        if (c.target === 'pod/gateway') for (const m of MODELS) options.push({ label: m, description: c.label.replace('<model>', m) });
      }
      return { call: { id: 'call_ask_target', name: 'ask_owner', args: { question: 'Where should I run it: Claude, or an OpenRouter model?', options } } };
    }
    if (o.call_id.startsWith('call_dispatch')) return { text: o.output.includes('"agent_id"') ? 'Started it.' : 'It was refused.' };
    return { text: 'ok' };
  }

  // A new turn, by the owner's words.
  const kick = new RegExp(KICKOFF + ' run-(\\w+)').exec(first);
  if (kick) {
    // The started agent's first turn: held until the test lets it finish (one
    // gate per run, so a repeated run never finds an earlier run's gate open).
    await gate('kickoff-' + kick[1]).wait();
    return { text: 'Kickoff done: looked at the task.' };
  }
  // A reply naming one of the owner's sessions by the marker the chief's tools
  // teach it: muster draws it as a session card (ui/session_card.go).
  const named = /CARD-ME agent:(\d+)/.exec(first);
  if (named) return { text: `Ask [[session:agent:${named[1]}]] about the retry logging.` };
  if (first.includes('SIDE-USAGE')) {
    // 330,000 + 30,000 + 2,345 = 362,345 in context (output is not counted):
    // past the 350k the ⋯ menu's memory line waits for.
    return {
      text: 'GOT: ' + first.slice(-200),
      usage: { input_tokens: 330_000, output_tokens: 777, cache_read_input_tokens: 30_000, cache_creation_input_tokens: 2_345 },
    };
  }
  const hold = /HOLD-ME-(\w+)/.exec(first);
  if (hold) {
    await gate(hold[1]).wait();
    return { text: 'HELD-REPLY-' + hold[1] };
  }
  if (first.includes('ASK-ME')) {
    return {
      call: {
        id: 'call_ask_' + requests.length,
        name: 'ask_owner',
        args: { question: 'Which flavour of report do you want?', options: [{ label: 'Short summary', description: 'A paragraph' }, { label: 'Full report' }] },
      },
    };
  }
  if (first.includes('dispatch something')) return { call: { id: 'call_nomodel', name: 'agent_dispatch', args: {} } };
  const disp = /DISPATCH-TASK-(\d+)/.exec(first);
  if (disp) {
    return { call: { id: 'call_dispatch_ok', name: 'agent_dispatch', args: { target: 'pod/gateway', model: 'openrouter/' + MODELS[0], task_id: Number(disp[1]) } } };
  }
  if (first.includes('MIRRORED-PLEASE')) {
    return { call: { id: 'call_dispatch_mirrored', name: 'agent_dispatch', args: { target: 'mirrored', prompt: 'do it', request_key: 'k-' + stamp, owner_asked: true } } };
  }
  return { text: 'GOT: ' + first.slice(-200) };
}

// --- fixtures --------------------------------------------------------------------

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

async function api(method: string, path: string, body?: unknown) {
  const r = await fetch(origin + path, {
    method,
    headers: { Authorization: `Bearer ${hookToken}`, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  return r;
}

test.beforeAll(async () => {
  test.setTimeout(180_000);
  runtime = http.createServer((req, res) => {
    let body = '';
    req.on('data', (d) => (body += d));
    req.on('end', async () => {
      // The model catalogue every model is checked against (internal/api
      // model_validate.go): the spec's own models, never the real OpenRouter.
      if (req.method === 'GET' && (req.url || '') === '/models') {
        res.setHeader('Content-Type', 'application/json');
        return res.end(JSON.stringify({ data: MODELS.map((id) => ({ id, name: id })) }));
      }
      if (req.method !== 'POST' || !(req.url || '').endsWith('/v1/responses')) {
        res.statusCode = 404;
        return res.end();
      }
      try {
        answer(res, await script(JSON.parse(body)));
      } catch (e) {
        res.statusCode = 500;
        res.end(String(e));
      }
    });
  });
  await new Promise<void>((r) => runtime!.listen(chiefUxFakePort, '127.0.0.1', () => r()));
  server = await startServer(versionTerm, chiefUxPort, {
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_PLUGINS: '1',
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x5a).toString('base64'),
    MUSTER_CONNECT_PORT: String(chiefUxConnectPort),
    MUSTER_AGENT_CONNECT_URL: connectURL,
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_GATEWAY: 'hooks-sha256',
    MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
    MUSTER_AGENT_RUNTIME_CONFIG: '{}',
    MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-alpha',
    MUSTER_AGENT_OPENROUTER_KEY: 'sk-or-v1-FAKE-e2e-not-a-real-key',
    MUSTER_TASK_ROUTING: '1',
    MUSTER_CHIEF_DISPATCH_OWNERS: '1',
    MUSTER_E2E_AGENT_ENDPOINT: `http://127.0.0.1:${chiefUxFakePort}`,
    MUSTER_E2E_OPENROUTER_MODELS: `http://127.0.0.1:${chiefUxFakePort}/models`,
  });
  fix('chief', '1', chiefName, chiefToken);
  chiefMade = true;
});

test.afterAll(async () => {
  try {
    for (const name of agentsMade) fix('rmagent', '1', name);
    for (const id of tasksMade) await api('DELETE', `/api/tasks/${id}`);
    if (chiefMade) fix('rmagent', '1', chiefName);
  } finally {
    await server?.stop();
    await new Promise<void>((r) => (runtime ? runtime.close(() => r()) : r()));
  }
});

// --- helpers -----------------------------------------------------------------------

// On a failure: what the fake runtime was asked last, and what the chat shows —
// the two sides a broken turn could be on.
test.afterEach(async ({ page }, info) => {
  if (info.status === info.expectedStatus) return;
  console.log('runtime requests (last 6):', JSON.stringify(requests.slice(-6), null, 1).slice(0, 4000));
  console.log('form session:', await page.locator('#chief-panel #chat-form').getAttribute('data-session').catch(() => '?'),
    'log src:', await page.locator('#chief-panel #chat-log').getAttribute('hx-get').catch(() => '?'));
  console.log('chat log:', (await page.locator('#chief-panel #chat-log').innerText().catch(() => '(none)')).slice(-2000));
  console.log('server log (tail):', (server?.log() ?? '').split('\n').slice(-40).join('\n'));
});

async function openChiefPanel(page: Page) {
  // The panel remembers it was open (its localStorage): a reloaded page restores
  // it, and a toggle then would CLOSE it.
  await page.waitForLoadState('load');
  if ((await page.locator('#chief-panel').getAttribute('data-chief-panel-open')) === '1') {
    await expect(page.locator('#chief-panel #chat-form')).toBeVisible();
    return;
  }
  await page.evaluate(() => {
    const b = document.createElement('button');
    b.setAttribute('data-chief-panel-toggle', '');
    b.style.cssText = 'position:fixed;left:-9999px;top:0';
    document.body.appendChild(b);
    b.click();
  });
  await expect(page.locator('#chief-panel')).toHaveAttribute('data-chief-panel-open', '1');
  await expect(page.locator('#chief-panel #chat-form')).toBeVisible();
}

const panel = (page: Page) => page.locator('#chief-panel');
const log = (page: Page) => panel(page).locator('#chat-log');
const working = (page: Page) => panel(page).locator('#chat-working');

// send types text into the chief's composer and sends it (the socket's path).
async function send(page: Page, text: string) {
  const input = panel(page).locator('#chat-input');
  await input.fill(text);
  await input.press('Enter');
}

async function openChief(page: Page) {
  await signIn(page, origin);
  await page.goto(origin + '/tasks');
  await openChiefPanel(page);
}

for (const vp of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`on a ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('"working…" survives a reload mid-turn and goes when the reply lands', async ({ page }) => {
      test.setTimeout(90_000);
      const key = vp.name + stamp;
      await openChief(page);
      await send(page, `How is the fleet? HOLD-ME-${key}`);
      await expect(working(page)).toBeVisible();
      await expect.poll(() => gate(key).entered, { timeout: 15_000 }).toBe(1);

      // The operator's reload, mid-turn: the page is drawn fresh, its socket gone.
      await page.reload();
      await openChiefPanel(page);
      await expect(working(page), 'the reloaded panel shows the turn still running').toBeVisible();
      await expect(working(page)).toHaveAttribute('data-turn-in-flight', '');
      await expect(log(page)).not.toContainText(`HELD-REPLY-${key}`);

      // The turn finishes (it was never cancelled): the reply lands and "working…" goes.
      gate(key).release();
      await expect(log(page)).toContainText(`HELD-REPLY-${key}`, { timeout: 20_000 });
      await expect(working(page)).toBeHidden();
      // And a reload now draws neither the indicator nor a lost reply.
      await page.reload();
      await openChiefPanel(page);
      await expect(log(page)).toContainText(`HELD-REPLY-${key}`);
      await expect(working(page)).toBeHidden();
    });

    test('an ask_owner card: tap an option, it is sent as the next message, answered after a reload', async ({ page }) => {
      test.setTimeout(90_000);
      await openChief(page);
      // The thread is shared across this file's tests: count what is there.
      const cardsBefore = await log(page).locator('[data-dispatch-card]').count();
      await send(page, `Write me a report ASK-ME (${vp.name})`);
      const card = log(page).locator('[data-owner-ask]').last();
      await expect(card).toHaveAttribute('data-owner-ask-state', 'waiting', { timeout: 20_000 });
      // A region named by its question, with a real button per option.
      await expect(log(page).getByRole('region', { name: 'Which flavour of report do you want?' }).last()).toBeVisible();
      await expect(card.getByRole('button', { name: /Short summary/ })).toBeVisible();
      await expect(card.getByRole('button', { name: /Full report/ })).toBeVisible();
      await expect(card.getByLabel('Other')).toBeVisible();
      // The turn ENDED at the question: the tool the runtime asked for after it
      // was never run, so no output for it ever came back to the runtime.
      expect(requests.some((r) => r.outputs.some((o) => o.call_id === 'call_after_ask'))).toBe(false);
      await expect(log(page).locator('[data-dispatch-card]')).toHaveCount(cardsBefore);
      const before = requests.length;

      if (vp.name === 'desktop') {
        // Keyboard: focus the option and press Enter.
        await card.getByRole('button', { name: /Full report/ }).focus();
        await page.keyboard.press('Enter');
      } else {
        await card.getByRole('button', { name: /Full report/ }).tap();
      }
      // Sent as the owner's next message: the runtime got it as a new turn…
      await expect.poll(() => requests.slice(before).some((r) => r.first === 'Full report'), { timeout: 15_000 }).toBe(true);
      await expect(log(page)).toContainText('GOT: Full report', { timeout: 20_000 });
      // …and the card shows it answered.
      await expect(card).toHaveAttribute('data-owner-ask-state', 'answered');
      await expect(card.locator('[data-owner-ask-answer]')).toHaveText('Your answer: Full report');
      await expect(card.getByRole('button', { name: /Short summary/ })).toHaveCount(0);

      await page.reload();
      await openChiefPanel(page);
      const again = log(page).locator('[data-owner-ask]').last();
      await expect(again).toHaveAttribute('data-owner-ask-state', 'answered');
      await expect(again.locator('[data-owner-ask-chosen]')).toContainText('Full report');
    });

    test('an ask_owner card takes a free "Other" answer, and a waiting card survives a reload', async ({ page }) => {
      test.setTimeout(90_000);
      await openChief(page);
      await send(page, `Another report ASK-ME (${vp.name}, other)`);
      const card = log(page).locator('[data-owner-ask]').last();
      await expect(card).toHaveAttribute('data-owner-ask-state', 'waiting', { timeout: 20_000 });
      // Pending across a reload: the card is drawn from the stored transcript.
      await page.reload();
      await openChiefPanel(page);
      const waiting = log(page).locator('[data-owner-ask]').last();
      await expect(waiting).toHaveAttribute('data-owner-ask-state', 'waiting');
      const answer = `Just the failing tests, please (${vp.name})`;
      await waiting.getByLabel('Other').fill(answer);
      await waiting.getByRole('button', { name: 'Send' }).click();
      await expect(log(page)).toContainText('GOT: ' + answer, { timeout: 20_000 });
      await expect(waiting).toHaveAttribute('data-owner-ask-state', 'answered');
      await expect(waiting.locator('[data-owner-ask-answer]')).toHaveText('Your answer: ' + answer);
    });

    test('tapping an option keeps a half-typed draft in the composer', async ({ page }) => {
      test.setTimeout(90_000);
      await openChief(page);
      await send(page, `Draft check ASK-ME (${vp.name})`);
      const card = log(page).locator('[data-owner-ask]').last();
      await expect(card).toHaveAttribute('data-owner-ask-state', 'waiting', { timeout: 20_000 });
      const draft = `half-typed, not sent yet (${vp.name})`;
      const input = panel(page).locator('#chat-input');
      await input.fill(draft);
      await card.getByRole('button', { name: /Short summary/ }).click();
      await expect(log(page)).toContainText('GOT: Short summary', { timeout: 20_000 });
      await expect(card).toHaveAttribute('data-owner-ask-state', 'answered');
      // The answer went; the draft did not.
      await expect(input).toHaveValue(draft);
      expect(requests.some((r) => r.first.includes(draft))).toBe(false);
      // And it is still the saved draft: a reload brings it back.
      await page.reload();
      await openChiefPanel(page);
      await expect(panel(page).locator('#chat-input')).toHaveValue(draft);
    });

    test('"dispatch" with no model asks with a card offering Claude and OpenRouter models', async ({ page }) => {
      test.setTimeout(90_000);
      await openChief(page);
      await send(page, `Please dispatch something to fix the flaky test (${vp.name})`);
      const card = log(page).locator('[data-owner-ask]').last();
      await expect(card).toHaveAttribute('data-owner-ask-state', 'waiting', { timeout: 20_000 });
      await expect(card.locator('[data-owner-ask-question]')).toHaveText('Where should I run it: Claude, or an OpenRouter model?');
      // One button per choice the refusal listed, in the owner's words (the
      // deployment's order of kinds): Claude, and each OpenRouter model offered.
      await expect(card.locator('[data-owner-ask-pick]')).toHaveCount(3);
      const pick = (label: string) => card.locator(`[data-owner-ask-pick="${label}"]`);
      await expect(pick('Claude')).toContainText('Claude Code (your Claude account)');
      await expect(pick(MODELS[0])).toContainText(`Gateway (OpenRouter model: ${MODELS[0]})`);
      await expect(pick(MODELS[1])).toContainText(`Gateway (OpenRouter model: ${MODELS[1]})`);
      // No agent was started without the owner's choice.
      await expect(log(page).locator('[data-dispatch-card][data-dispatch-agent]')).toHaveCount(0);
      await expect(log(page)).not.toContainText('NO-CHOICES');
    });

    test('a refused dispatch is one compact line saying why', async ({ page }) => {
      test.setTimeout(90_000);
      await openChief(page);
      await send(page, `Start it MIRRORED-PLEASE (${vp.name})`);
      const refused = log(page).locator('[data-dispatch-card][data-dispatch-state="refused"]').last();
      await expect(refused).toBeVisible({ timeout: 20_000 });
      // No host here runs -agent-spawn, so a mirrored start is refused before
      // anything is asked (agentDispatchNoHost); no cluster agent stands in.
      await expect(refused.locator('[data-dispatch-refused]')).toContainText('no host can start agents now');
      await expect(refused.locator('[data-dispatch-refused]')).toContainText('nothing was asked or queued');
      await expect(refused.locator('a')).toHaveCount(0);
      // Stored, so a reload draws it the same.
      await expect(log(page)).toContainText('It was refused.', { timeout: 20_000 });
      await page.reload();
      await openChiefPanel(page);
      await expect(log(page).locator('[data-dispatch-card][data-dispatch-state="refused"]').last()).toBeVisible();
    });
  });
}

// The started agent's card, at a phone's width. Each run removes the agent and
// the task it made, so the chief's live budget is spent once per run, not once
// per file (the daily cap still bounds a --repeat-each to its size).
test.describe('a dispatched agent', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

  test('is a live card: its state follows the agent, and a tap opens its page', async ({ page }) => {
    test.setTimeout(150_000);
    const run = Date.now().toString(36);
    const made = await api('POST', '/api/tasks', { body: `${KICKOFF} run-${run}: make the export test stop flaking` });
    expect(made.status, await made.clone().text()).toBeLessThan(300);
    const task = (await made.json()) as { id: number };
    tasksMade.push(task.id);

    await openChief(page);
    await send(page, `Go ahead DISPATCH-TASK-${task.id}`);
    // THIS run's card, by its task link — never .last(): the thread is shared
    // with the file's other tests (and with earlier runs of this one).
    const card = log(page).locator('[data-dispatch-card][data-dispatch-agent]').filter({ has: page.locator(`[data-dispatch-task="${task.id}"]`) });
    // The live stream draws the card as soon as the result arrives.
    await expect(card).toBeVisible({ timeout: 20_000 });
    const open = card.locator('[data-dispatch-open]');
    const href = (await open.getAttribute('href'))!;
    const name = /\/agents\/([^?]+)/.exec(href)![1];
    agentsMade.add(name);
    await expect(card.locator('[data-dispatch-runtime]')).toHaveText(`Gateway · openrouter/${MODELS[0]}`);
    await expect(card.locator(`[data-dispatch-task="${task.id}"]`)).toHaveAttribute('href', `/tasks/${task.id}`);

    // Its first turn runs (the runtime holds it): the card says working, live.
    await expect.poll(() => gate('kickoff-' + run).entered, { timeout: 60_000 }).toBe(1);
    await expect(card).toHaveAttribute('data-dispatch-state', 'working', { timeout: 30_000 });
    // The turn ends: the card says it waits for its owner — no reload.
    gate('kickoff-' + run).release();
    await expect(card).toHaveAttribute('data-dispatch-state', 'waiting', { timeout: 30_000 });
    // 🔴 TWO OF ITS AGENT'S EVENTS BACK TO BACK: the LATER one decides the card.
    // CI caught the card stuck on "Starting" while the agent worked: the
    // status promoter's agent.changed and the first turn's stamp land in the
    // same moment, the second while the card's refresh for the first is still
    // in flight. The refresh replaced the card (outerHTML), so a request htmx
    // queued behind it fired from the REMOVED card and drew nothing (now the
    // card is refreshed by a wrapper that stays: ui.liveCard). Played
    // deterministically here: the first refresh is held and answers a stale
    // "starting"; the second answers the real state; the card must end on it.
    // No wait for the chief's note that re-renders this log just before: a card
    // that log redraws must hear the first event however close it lands (it is
    // listening in the swap's own task — #chat-log's settle:0ms, agents_detail.go).
    let calls = 0;
    await page.route('**/ui/dispatch-card?agent=*', async (route) => {
      const n = ++calls;
      const res = await route.fetch();
      let body = await res.text();
      if (n === 1) {
        await new Promise((r) => setTimeout(r, 1500));
        body = body.replace(/data-dispatch-state="[a-z]+"/, 'data-dispatch-state="starting"');
      }
      await route.fulfill({ response: res, body });
    });
    await page.evaluate((agent) => {
      const fire = () => document.body.dispatchEvent(new CustomEvent('sse:agent.changed', { detail: { data: JSON.stringify({ id: agent }) } }));
      fire();
      setTimeout(fire, 300);
    }, name);
    await expect.poll(() => calls, { timeout: 10_000 }).toBeGreaterThanOrEqual(1);
    await page.waitForTimeout(2500); // past the held first answer
    await expect(card).toHaveAttribute('data-dispatch-state', 'waiting');
    await page.unroute('**/ui/dispatch-card?agent=*');
    // 🔴 A STALE RESPONSE THAT ARRIVES AFTER A NEWER RENDER IS DISCARDED (the
    // card's version guard, ui.dispatchCardGuardJS). Two refreshes into the same
    // card from two DIFFERENT elements (htmx serializes requests per source, so
    // one source would queue the second behind the first and never race): the
    // first is held and answers a stale "starting" — with the version the server
    // gave it, older because it was rendered first — and lands AFTER the second.
    // The card keeps the newer state. (Without the guard: "starting".)
    let fetched = 0;
    await page.route('**/ui/dispatch-card?agent=*', async (route) => {
      const n = ++fetched;
      const res = await route.fetch();
      let body = await res.text();
      if (n === 1) {
        await new Promise((r) => setTimeout(r, 1200));
        body = body.replace(/data-dispatch-state="[a-z]+"/, 'data-dispatch-state="starting"');
      }
      await route.fulfill({ response: res, body });
    });
    const agentId = await card.getAttribute('data-dispatch-agent');
    await page.evaluate((id) => {
      const w = document.querySelector(`[data-dispatch-live="${id}"]`)!;
      const go = () => {
        const src = document.body.appendChild(document.createElement('div'));
        (window as any).htmx.ajax('GET', '/ui/dispatch-card?agent=' + id, { source: src, target: w, swap: 'innerHTML', select: '[data-dispatch-card]' });
      };
      go();
      setTimeout(go, 200);
    }, agentId);
    await expect.poll(() => fetched, { timeout: 10_000 }).toBe(2);
    await page.waitForTimeout(2000); // past the held stale answer
    await expect(card).toHaveAttribute('data-dispatch-state', 'waiting');
    await page.unroute('**/ui/dispatch-card?agent=*');
    // The card's link is the agent's page (its list link: its latest session).
    await expect(card.locator('[data-dispatch-open]')).toHaveAttribute('href', new RegExp(`^/agents/${name}\\?session=\\d+&from=list$`));
    await card.locator('[data-dispatch-open]').tap();
    await page.waitForURL((u) => u.pathname === '/agents/' + name);
    await expect(page.locator('#chat-form')).toBeVisible();
    // This run's agent and task go, so the next run starts from the same budget.
    fix('rmagent', '1', name);
    agentsMade.delete(name);
    expect((await api('DELETE', `/api/tasks/${task.id}`)).status).toBeLessThan(300);
    tasksMade.splice(tasksMade.indexOf(task.id), 1);
  });
});

// --- the chief's PAGE: "working…" that holds, and primary + side chats ---------
//
// THE OPERATOR'S REPORT: in a fresh chief chat, "ping" drew "working…", and a
// #chat-log refetch moments later re-rendered the log with the row HIDDEN while
// the chief was still working; the reply came later out of nowhere. Two halves:
// the server's count of the turn (internal/api countSessionTurn — now counted
// from the moment the socket reads the message) and the page's own (the row is
// no longer hidden when its socket drops mid-turn; a reconnect redraws it from
// the server; a turn started anywhere else tells every open chat).

const pageLog = (page: Page) => page.locator('#chat-log');
const pageWorking = (page: Page) => page.locator('#chat-working');
// The chief page's chats: the strip of chips under its header (internal/ui
// chief_chats.go), which names the chat the page is on (data-chief-chat-kind).
// In the panel the same strip is over the panel's transcript.
const chats = (page: Page) => page.locator('body > [data-chief-strip-row] [data-chief-chat-strip]');
const panelChats = (page: Page) => page.locator('#chief-panel [data-chief-chat-strip]');
const sessionOf = (u: string) => new URL(u).searchParams.get('session');

// shot saves a screenshot when MUSTER_E2E_CHIEF_SHOTS names a directory (the
// before/after images a reviewer looks at); a generator, never an assertion.
async function shot(page: Page, name: string) {
  const dir = process.env.MUSTER_E2E_CHIEF_SHOTS;
  if (dir) await page.screenshot({ path: path.join(dir, name) });
}

// stripIds is the chat ids of a strip's chips, in order (hidden ones too).
const stripIds = (strip: ReturnType<Page['locator']>) =>
  strip.locator('[data-chief-chip]').evaluateAll((els) => els.map((e) => e.getAttribute('data-chief-chip') ?? ''));
// currentId is the chat id of a strip's current chip.
const currentId = async (strip: ReturnType<Page['locator']>) => {
  await expect(strip.locator('[aria-current="true"]')).toHaveCount(1);
  return (await strip.locator('[aria-current="true"]').getAttribute('data-chief-chat-row'))!;
};

// noSheet: nothing modal is involved — no open <dialog>, no visible
// role="dialog", no open popover.
async function noSheet(page: Page, scope = 'page') {
  await expect(page.locator('dialog[open]'), `${scope}: no open dialog`).toHaveCount(0);
  if (scope === 'page') await expect(page.locator('[role="dialog"]:visible'), `${scope}: no visible role=dialog`).toHaveCount(0);
  expect(await page.evaluate(() => document.querySelectorAll(':popover-open').length), `${scope}: no open popover`).toBe(0);
}

// stripLayout: the strip is ONE row under the header (no wrap, no overlap with
// the header or its context chip), every target is at least 44px, and the page
// does not scroll sideways.
async function stripLayout(page: Page, strip: ReturnType<Page['locator']>, label: string, under?: ReturnType<Page['locator']>) {
  const sb = (await strip.boundingBox())!;
  expect.soft(sb.height, `${label}: the strip is one 52px row`).toBeLessThanOrEqual(52.5);
  if (under) {
    const hb = (await under.boundingBox())!;
    expect.soft(sb.y, `${label}: the strip starts below the header`).toBeGreaterThanOrEqual(hb.y + hb.height - 0.5);
    const ctx = await under.locator('#chief-context').boundingBox();
    if (ctx) expect.soft(ctx.y + ctx.height, `${label}: the context chip is in the header, above the strip`).toBeLessThanOrEqual(sb.y + 0.5);
  }
  const boxes = await strip
    .locator('[data-chief-chip]:not([hidden]) > *:not([popover]), button[data-chief-side-chat], button[data-chief-archived]')
    .evaluateAll((els) => els.map((e) => { const r = e.getBoundingClientRect(); return { top: r.top, h: r.height, w: r.width, what: e.outerHTML.slice(0, 60) }; }));
  expect(boxes.length, `${label}: the strip has targets`).toBeGreaterThan(2);
  for (const b of boxes) {
    expect.soft(Math.abs(b.top - boxes[0].top), `${label}: ${b.what} is on the strip's one row`).toBeLessThanOrEqual(1);
    expect.soft(b.h, `${label}: ${b.what} is a 44px target`).toBeGreaterThanOrEqual(44);
    expect.soft(b.w, `${label}: ${b.what} is a 44px target`).toBeGreaterThanOrEqual(44);
  }
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect.soft(sw, `${label}: the page does not scroll sideways`).toBeLessThanOrEqual(cw);
}

// currentInView: the current chip is wholly inside its strip's scroller.
async function currentInView(strip: ReturnType<Page['locator']>, label: string) {
  const r = await strip.evaluate((s) => {
    const sc = s.querySelector('[data-chief-chip-scroller]')!;
    const cur = sc.querySelector('[aria-current="true"]')!.closest('[data-chief-chip]')!;
    const a = sc.getBoundingClientRect(), b = cur.getBoundingClientRect();
    return { left: b.left - a.left, right: a.right - b.right, overflow: sc.scrollWidth - sc.clientWidth };
  });
  expect.soft(r.left, `${label}: the current chip's left edge is in view`).toBeGreaterThanOrEqual(-0.5);
  expect.soft(r.right, `${label}: the current chip's right edge is in view`).toBeGreaterThanOrEqual(-0.5);
  return r.overflow;
}

async function openChiefPage(page: Page) {
  await signIn(page, origin);
  await page.goto(origin + '/agents/' + chiefName);
  await expect(page.locator('#chat-form')).toBeVisible();
  // The chat's socket is open: a send goes over it, not the form's fallback.
  await expect.poll(() => page.evaluate(() => (window as any).__cgChat?.ws?.readyState ?? -1), { timeout: 15_000 }).toBe(1);
}

async function sendOnPage(page: Page, text: string) {
  const input = page.locator('#chat-input');
  await input.fill(text);
  await input.press('Enter');
}

for (const vp of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`the chief page on a ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('"working…" holds across a socket drop mid-turn, and goes when the reply lands', async ({ page }) => {
      test.setTimeout(90_000);
      const key = 'drop' + vp.name + stamp;
      await openChiefPage(page);
      await expect(chats(page)).toHaveAttribute('data-chief-chat-kind', 'primary');
      await sendOnPage(page, `ping HOLD-ME-${key}`);
      await expect(pageWorking(page)).toBeVisible();
      await expect.poll(() => gate(key).entered, { timeout: 15_000 }).toBe(1);
      // The socket drops mid-turn (a network blip, a sleeping phone): the turn
      // runs on, detached, on the server.
      await page.evaluate(() => {
        (window as any).__droppedWS = (window as any).__cgChat.ws;
        (window as any).__cgChat.ws.close();
      });
      // A new socket opens…
      await expect
        .poll(() => page.evaluate(() => {
          const c = (window as any).__cgChat;
          return !!c && !!c.ws && c.ws !== (window as any).__droppedWS && c.ws.readyState === 1;
        }), { timeout: 15_000 })
        .toBe(true);
      // …and the row is the SERVER's: drawn showing, from its count of the turn.
      await expect(pageWorking(page), 'the row is redrawn from the server while the turn runs').toHaveAttribute('data-turn-in-flight', '', { timeout: 10_000 });
      await expect(pageWorking(page)).toBeVisible();
      await expect(pageLog(page)).not.toContainText(`HELD-REPLY-${key}`);
      gate(key).release();
      await expect(pageLog(page)).toContainText(`HELD-REPLY-${key}`, { timeout: 20_000 });
      await expect(pageWorking(page)).toBeHidden();
    });

    test('a turn started elsewhere — `muster chief ask` — shows "working…" on the open chat', async ({ page }) => {
      test.setTimeout(90_000);
      const key = 'ask' + vp.name + stamp;
      await openChiefPage(page);
      await expect(chats(page)).toHaveAttribute('data-chief-chat-kind', 'primary');
      await expect(pageWorking(page)).toBeHidden();
      // The CLI's door, not this page: the turn runs in the primary chat.
      const asked = api('POST', `/api/agents/${chiefName}/messages`, { message: `from the CLI HOLD-ME-${key}` });
      await expect.poll(() => gate(key).entered, { timeout: 15_000 }).toBe(1);
      await expect(pageWorking(page), 'the open chat shows the turn it did not start').toBeVisible({ timeout: 10_000 });
      gate(key).release();
      expect((await asked).status).toBe(200);
      await expect(pageLog(page)).toContainText(`HELD-REPLY-${key}`, { timeout: 20_000 });
      await expect(pageWorking(page)).toBeHidden();
    });

    test('a session the chief names is a card: its name, a status dot, no raw id; a tap opens it', async ({ page }) => {
      test.setTimeout(90_000);
      const name = `e2e-ux-card-${vp.name.replace(/\W+/g, '-')}-${stamp}`;
      const id = fix('agent', '1', name, `e2e-ux-card-token-${stamp}-${vp.name.length}`);
      agentsMade.add(name);
      await openChiefPage(page);
      await expect.poll(() => page.evaluate(() => (window as any).__cgChat?.ws?.readyState ?? -1), { timeout: 15_000 }).toBe(1);
      await sendOnPage(page, `who has it? CARD-ME agent:${id}`);
      const reply = pageLog(page).locator('[data-bubble="assistant"]').filter({ hasText: 'about the retry logging' }).last();
      await expect(reply).toBeVisible({ timeout: 20_000 });
      const card = reply.locator('[data-session-card]');
      await expect(card).toHaveCount(1);
      await expect(card.locator('[data-session-card-title]')).toHaveText(name);
      await expect(card.locator('[data-session-status]')).toHaveCount(1);
      await expect(reply).not.toContainText('[[session:');
      await expect(reply).not.toContainText(`agent:${id}`);
      await card.locator('[data-session-card-open]').click();
      await page.waitForURL((u) => u.pathname === '/agents/' + name);
      await expect(page.locator('#chat-form')).toBeVisible();
      fix('rmagent', '1', name);
      agentsMade.delete(name);
    });

    // + LANDS WHERE YOU CAN TYPE (the operator: "the chief new side chat button
    // doesn't work" — the server opened the chat and the page moved to it, but
    // nothing on screen said so). Red at main: no strip, no current chip, and
    // the composer was not focused.
    test('the chip strip: + lands in the message box, an empty chat is reused, a used one is not, a chip switches', async ({ page }) => {
      test.setTimeout(150_000);
      const tap = (l: ReturnType<Page['locator']>) => (vp.use.hasTouch ? l.tap() : l.click());
      await page.addInitScript(clsInit);
      await openChiefPage(page);
      await expect(chats(page)).toHaveAttribute('data-chief-chat-kind', 'primary');
      const primary = await currentId(chats(page));
      // The header keeps the one list icon, whose sheet holds no chats now.
      await expect(page.locator('header #chat-index-open')).toBeVisible();
      await expect(page.locator('#chat-index [role="tab"][data-tab="chats"], #chat-index [data-chief-chats]')).toHaveCount(0);
      await expect(page.locator('[data-chief-chat-bar]')).toHaveCount(0);
      await expect(chats(page).locator(`[data-chief-chip="${primary}"]`)).toHaveAttribute('data-chief-chip-kind', 'primary');
      await expect(chats(page).locator(`[data-chief-chat-row="${primary}"]`)).toContainText('★Primary');
      const before = await stripIds(chats(page));
      await shot(page, `after-page-primary-${vp.name}.png`);

      // 1. + from the primary: a NEW chat, its chip current, the cursor in its box.
      const plus = chats(page).locator('button[data-chief-side-chat]');
      await expect(plus).toHaveAccessibleName('New side chat');
      const url0 = page.url();
      await tap(plus);
      await page.waitForURL((u) => u.toString() !== url0);
      const s1 = sessionOf(page.url())!;
      expect(before, '+ opened a NEW chat').not.toContain(s1);
      await expect(chats(page).locator(`[data-chief-chat-row="${s1}"]`)).toHaveAttribute('aria-current', 'true');
      await expect(chats(page).locator(`[data-chief-chat-row="${s1}"]`)).toHaveText('New chat');
      await expect(chats(page)).toHaveAttribute('data-chief-chat-kind', 'side');
      await expect(page.locator('#chat-input'), 'the cursor is in the new chat\'s message box').toBeFocused();
      expect(await stripIds(chats(page)), 'the new chip is the newest side chat, right after the primary').toEqual([before[0], s1, ...before.slice(1)]);
      await noSheet(page);
      // No layout shift landing on it (the strip is drawn with the page).
      await settle(page);
      const landed = await readCls(page);
      expect(landed.unsupported).toBeUndefined();
      expect(landed.total, `layout shift landing on a new chat: ${JSON.stringify(landed.shifts)}`).toBeLessThan(0.01);
      await shot(page, `after-page-plus-${vp.name}.png`);
      // Only "+"'s answer asks for the focus: the address it landed on no
      // longer carries the ask, so a reload — or a plain load of ?session= —
      // leaves the cursor alone.
      expect(new URL(page.url()).searchParams.has('focus'), 'the focus ask is dropped from the address').toBe(false);
      const quiet = async (label: string) => {
        await expect(page.locator('#chat-form')).toBeVisible();
        await page.waitForLoadState('load');
        await page.waitForTimeout(500);
        await expect(page.locator('#chat-input'), label).not.toBeFocused();
      };
      await page.reload();
      await quiet('a reload does not take the focus');
      await page.goto(origin + '/agents/' + chiefName + '?session=' + s1);
      await quiet('a plain load of ?session=N does not take the focus');

      // 2. + again while that chat is empty: the SAME chat, no second chip,
      // still current, the cursor in its box (HX-Redirect to this very page).
      const ids1 = await stripIds(chats(page));
      await page.locator('#chat-input').blur();
      const reloaded = page.waitForEvent('load');
      await tap(plus);
      await reloaded;
      expect(sessionOf(page.url()), 'the empty chat is reused').toBe(s1);
      expect(await stripIds(chats(page)), 'no second chip').toEqual(ids1);
      await expect(chats(page).locator(`[data-chief-chat-row="${s1}"]`)).toHaveAttribute('aria-current', 'true');
      await expect(page.locator('#chat-input'), 'a reused chat lands in its message box too').toBeFocused();
      await noSheet(page);

      // 3. Once it has a line, + makes a NEW chat. The turn reports its tokens:
      // the ⋯ menu says how much memory the chat uses when it ends, no reload.
      await expect.poll(() => page.evaluate(() => (window as any).__cgChat?.ws?.readyState ?? -1), { timeout: 15_000 }).toBe(1);
      // No token chip in the header (operator decision, 2026-11-19).
      await expect(page.locator('header [data-chief-context-row], header #chief-context-chip')).toHaveCount(0);
      const memory = page.locator('#agent-actions [data-chief-memory]');
      await expect(memory, 'no memory line before a turn reported its tokens').toHaveCount(0);
      // A turn in the side chat that reports 362,345 tokens of context: the ⋯
      // menu says it in plain words when the turn ends, no reload — and the
      // header still carries no chip.
      await sendOnPage(page, `side question SIDE-USAGE (${vp.name})`);
      await expect(pageLog(page)).toContainText(`GOT: side question SIDE-USAGE (${vp.name})`, { timeout: 20_000 });
      await expect(memory).toHaveCount(1, { timeout: 10_000 });
      await page.locator('#agent-actions-open').click();
      await expect(memory).toBeVisible();
      await expect(memory).toHaveText('Memory used: 362k');
      await page.keyboard.press('Escape');
      await expect(page.locator('header [data-chief-context-row], header #chief-context-chip')).toHaveCount(0);
      await tap(plus);
      await page.waitForURL((u) => sessionOf(u.toString()) !== s1);
      const s2 = sessionOf(page.url())!;
      expect(ids1, 'a used chat is not reused').not.toContain(s2);
      await expect(chats(page).locator(`[data-chief-chat-row="${s2}"]`)).toHaveAttribute('aria-current', 'true');
      await expect(page.locator('#chat-input')).toBeFocused();
      const ids2 = await stripIds(chats(page));
      expect(ids2.slice(0, 3), 'primary, the new chat, then the used one').toEqual([primary, s2, s1]);
      await stripLayout(page, chats(page), `${vp.name} on a new chat`, page.locator('header'));
      await shot(page, `after-page-second-plus-${vp.name}.png`);

      // 4. A chip switches: the transcript and the current marker move.
      await tap(chats(page).locator(`[data-chief-chat-row="${s1}"]`));
      await page.waitForURL((u) => sessionOf(u.toString()) === s1);
      await expect(pageLog(page)).toContainText(`side question SIDE-USAGE (${vp.name})`);
      await expect(chats(page).locator(`[data-chief-chat-row="${s1}"]`)).toHaveAttribute('aria-current', 'true');
      await expect(chats(page).locator('[aria-current="true"]')).toHaveCount(1);
      // On a phone the chips overflow and scroll sideways; the current one —
      // the third chip — is scrolled into view.
      const overflow = await currentInView(chats(page), `${vp.name} on the third chip`);
      if (vp.use.hasTouch) expect(overflow, 'the phone\'s strip overflows (so the scroll into view is exercised)').toBeGreaterThan(0);
      await stripLayout(page, chats(page), `${vp.name} on the third chip`, page.locator('header'));
      await shot(page, `after-page-switched-${vp.name}.png`);

      // 8. The unread dot: a turn held in s1, the operator leaves for the
      // primary, the reply lands in s1 — its chip carries the dot.
      const key = 'unread' + vp.name + stamp;
      await expect.poll(() => page.evaluate(() => (window as any).__cgChat?.ws?.readyState ?? -1), { timeout: 15_000 }).toBe(1);
      await sendOnPage(page, `while you are away HOLD-ME-${key}`);
      await expect.poll(() => gate(key).entered, { timeout: 15_000 }).toBe(1);
      await tap(chats(page).locator(`[data-chief-chat-row="${primary}"]`));
      await page.waitForURL((u) => sessionOf(u.toString()) === primary);
      // (A side chat an earlier run left unread keeps its own dot: count them.)
      await expect(chats(page).locator(`[data-chief-chip="${s1}"] [data-chief-unread]`)).toHaveCount(0);
      const dotsBefore = await chats(page).locator('[data-chief-unread]').count();
      gate(key).release();
      await expect
        .poll(async () => {
          await page.reload();
          return chats(page).locator(`[data-chief-chip="${s1}"] [data-chief-unread]`).count();
        }, { timeout: 20_000, message: 'the side chat\'s chip shows an unread dot' })
        .toBe(1);
      await expect(chats(page).locator('[data-chief-unread]'), 'one more dot: that chip\'s').toHaveCount(dotsBefore + 1);
      await expect(chats(page).locator(`[data-chief-chip="${primary}"] [data-chief-unread]`), 'none on the chat you are on').toHaveCount(0);

      // The instrument: a shift nobody asked for (a block above the strip,
      // no input) reads above the bar, so the zero above is a measurement.
      await settle(page);
      await page.evaluate(() => {
        const d = document.createElement('div');
        d.style.height = '160px';
        const row = document.querySelector('[data-chief-strip-row]')!;
        row.parentElement!.insertBefore(d, row);
      });
      await settle(page, 500);
      const control = await readCls(page);
      expect(control.total, 'positive control: an injected shift above the strip is measured').toBeGreaterThan(0.01);
    });

    // 5. Every chip's ⋯ by keyboard. Red at main: no chips.
    test('a chip\'s ⋯ menu by keyboard: archive (inline Archived), unarchive, make primary, fresh primary\'s confirm, Escape', async ({ page }) => {
      test.setTimeout(120_000);
      await openChiefPage(page);
      const primary = await currentId(chats(page));
      // The empty side chat the test above left (its chip is still "New chat").
      const side = (await chats(page).locator('[data-chief-chip-kind="side"]').filter({ hasText: 'New chat' }).first().getAttribute('data-chief-chip'))!;
      expect(side).toBeTruthy();
      const menuOf = (sid: string) => chats(page).locator(`[data-chief-chip="${sid}"] button[aria-haspopup="menu"]`);
      const menu = (sid: string) => page.locator(`#chief-chat-menu-${sid}`);
      // openByKeyboard: focus the chip's ⋯, Enter, and the menu is open.
      const openByKeyboard = async (sid: string) => {
        await menuOf(sid).focus();
        await expect(menuOf(sid)).toBeFocused();
        await page.keyboard.press('Enter');
        await expect(menu(sid)).toBeVisible();
        await expect(menuOf(sid)).toHaveAttribute('aria-expanded', 'true');
      };
      await chats(page).locator(`[data-chief-chat-row="${side}"]`).click();
      await page.waitForURL((u) => sessionOf(u.toString()) === side);

      // Archive the open chat: you land on the primary, and it is behind
      // "Archived (n)", which expands inline.
      const toggle = chats(page).locator('button[data-chief-archived]');
      const archivedBefore = (await toggle.count()) ? Number(/\((\d+)\)/.exec((await toggle.textContent())!)![1]) : 0;
      await openByKeyboard(side);
      await page.keyboard.press('Tab');
      await expect(menu(side).getByRole('menuitem', { name: 'Make primary' })).toBeFocused();
      await page.keyboard.press('Tab');
      await expect(menu(side).getByRole('menuitem', { name: 'Archive' })).toBeFocused();
      const toPrimary = page.waitForURL((u) => sessionOf(u.toString()) === primary);
      await page.keyboard.press('Enter');
      await toPrimary;
      await expect(chats(page)).toHaveAttribute('data-chief-chat-kind', 'primary');
      await expect(toggle).toHaveText(`Archived (${archivedBefore + 1})`);
      await expect(chats(page).locator('[data-chief-archived-chip]')).toHaveCount(archivedBefore + 1);
      const archivedChip = chats(page).locator(`[data-chief-chip="${side}"]`);
      await expect(archivedChip).toHaveAttribute('data-chief-chip-kind', 'archived');
      await expect(archivedChip).toBeHidden();
      await toggle.focus();
      await page.keyboard.press('Enter');
      await expect(toggle).toHaveAttribute('aria-expanded', 'true');
      await expect(archivedChip, 'Archived expands inline').toBeVisible();
      await noSheet(page);
      await stripLayout(page, chats(page), `${vp.name} with Archived open`, page.locator('header'));
      await shot(page, `after-page-archived-open-${vp.name}.png`);

      // Unarchive it from its ⋯: it is back among the side chats, and the page
      // stays on the primary.
      await openByKeyboard(side);
      await page.keyboard.press('Tab');
      await expect(menu(side).getByRole('menuitem', { name: 'Unarchive' })).toBeFocused();
      let reloaded = page.waitForEvent('load');
      await page.keyboard.press('Enter');
      await reloaded;
      expect(sessionOf(page.url())).toBe(primary);
      await expect(chats(page).locator(`[data-chief-chip="${side}"]`)).toHaveAttribute('data-chief-chip-kind', 'side');
      await expect(chats(page).locator(`[data-chief-chip="${side}"]`)).toBeVisible();

      // Make primary moves the ★ (and the chip to the front); the page stays.
      await openByKeyboard(side);
      await page.keyboard.press('Tab');
      await expect(menu(side).getByRole('menuitem', { name: 'Make primary' })).toBeFocused();
      reloaded = page.waitForEvent('load');
      await page.keyboard.press('Enter');
      await reloaded;
      await expect(chats(page).locator(`[data-chief-chip="${side}"]`)).toHaveAttribute('data-chief-chip-kind', 'primary');
      await expect(chats(page).locator(`[data-chief-chat-row="${side}"]`)).toContainText('★');
      await expect(chats(page).locator(`[data-chief-chip="${primary}"]`)).toHaveAttribute('data-chief-chip-kind', 'side');
      expect((await stripIds(chats(page)))[0], 'the primary is first').toBe(side);
      // And back.
      await openByKeyboard(primary);
      await page.keyboard.press('Tab');
      await expect(menu(primary).getByRole('menuitem', { name: 'Make primary' })).toBeFocused();
      reloaded = page.waitForEvent('load');
      await page.keyboard.press('Enter');
      await reloaded;
      await expect(chats(page).locator(`[data-chief-chip="${primary}"]`)).toHaveAttribute('data-chief-chip-kind', 'primary');

      // Fresh primary, on the primary's ⋯, asks its confirm (dismissed:
      // nothing happens); Escape then closes the menu.
      const here = page.url();
      await openByKeyboard(primary);
      await page.keyboard.press('Tab');
      await expect(menu(primary).getByRole('menuitem', { name: 'Fresh primary' })).toBeFocused();
      const asked: string[] = [];
      page.once('dialog', (d) => { asked.push(d.type() + ':' + d.message()); void d.dismiss(); });
      await page.keyboard.press('Enter');
      await expect.poll(() => asked.length, { message: 'Fresh primary asks its confirm' }).toBe(1);
      expect(asked[0]).toMatch(/^confirm:Start a fresh primary chat\?/);
      await page.waitForTimeout(300);
      expect(page.url(), 'a dismissed confirm leaves the page where it was').toBe(here);
      await page.keyboard.press('Escape');
      await expect(menu(primary)).toBeHidden();
      await expect(menuOf(primary)).toHaveAttribute('aria-expanded', 'false');

      // Put the empty side chat away again, so a later + opens a new one.
      await menuOf(side).click();
      await menu(side).getByRole('menuitem', { name: 'Archive' }).click();
      await expect(chats(page).locator(`[data-chief-chip="${side}"]`)).toHaveAttribute('data-chief-chip-kind', 'archived');
    });

    // The chief PANEL: the same strip, every answer into the panel body, the
    // URL the grid's. Red at main: no strip in the panel.
    test('the panel draws the same strip: +, a chip and archive stay in the grid', async ({ page }) => {
      test.setTimeout(90_000);
      const tap = (l: ReturnType<Page['locator']>) => (vp.use.hasTouch ? l.tap() : l.click());
      await openChief(page);
      const strip = panelChats(page);
      await expect(strip).toBeVisible();
      await expect(panel(page).locator('[data-chief-chat-bar]')).toHaveCount(0);
      const grid = page.url();
      const start = await currentId(strip);
      const primary = (await strip.locator('[data-chief-chip-kind="primary"]').getAttribute('data-chief-chip'))!;
      const before = await stripIds(strip);
      const active = panel(page).locator('[data-chief-active-session]');
      await shot(page, `after-panel-${vp.name}.png`);

      await tap(strip.locator('button[data-chief-side-chat]'));
      await expect(active).not.toHaveAttribute('data-chief-active-session', start);
      const p1 = (await active.getAttribute('data-chief-active-session'))!;
      expect(before, '+ in the panel opened a NEW chat').not.toContain(p1);
      await expect(strip.locator(`[data-chief-chat-row="${p1}"]`)).toHaveAttribute('aria-current', 'true');
      await expect(panel(page).locator('#chat-input'), 'the cursor is in the panel\'s message box').toBeFocused();
      expect(page.url(), 'the grid stays').toBe(grid);
      await noSheet(page, 'panel');
      await expect(panel(page).locator('#chief-threads')).not.toBeChecked();
      await stripLayout(page, strip, `${vp.name} panel`);
      await shot(page, `after-panel-plus-${vp.name}.png`);

      // Escape on a chip's ⋯ closes the MENU only: the panel stays open, on
      // the same chat (the panel's Escape used to shut the whole panel).
      const p1Menu = strip.locator(`[data-chief-chip="${p1}"] button[aria-haspopup="menu"]`);
      await expect(p1Menu).toHaveAccessibleName('Actions for New chat');
      await p1Menu.focus();
      await expect(p1Menu).toBeFocused();
      await page.keyboard.press('Enter');
      await expect(page.locator(`#chief-panel-chat-menu-${p1}`)).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(page.locator(`#chief-panel-chat-menu-${p1}`)).toBeHidden();
      await page.waitForTimeout(300);
      await expect(panel(page), 'Escape on the menu leaves the panel open').toHaveAttribute('data-chief-panel-open', '1');
      await expect(active).toHaveAttribute('data-chief-active-session', p1);
      await expect(panel(page).locator('#chat-form')).toBeVisible();

      // A chip switches the panel's chat.
      await tap(strip.locator(`[data-chief-chat-row="${primary}"]`));
      await expect(active).toHaveAttribute('data-chief-active-session', primary);
      await expect(strip.locator(`[data-chief-chat-row="${primary}"]`)).toHaveAttribute('aria-current', 'true');
      expect(page.url()).toBe(grid);

      // Archive p1 from its chip's ⋯: the panel stays on the primary, p1 is
      // behind Archived (n), which expands inline.
      await tap(strip.locator(`[data-chief-chip="${p1}"] button[aria-haspopup="menu"]`));
      const m = page.locator(`#chief-panel-chat-menu-${p1}`);
      await expect(m).toBeVisible();
      await m.getByRole('menuitem', { name: 'Archive' }).click();
      await expect(strip.locator(`[data-chief-chip="${p1}"]`)).toHaveAttribute('data-chief-chip-kind', 'archived');
      await expect(active).toHaveAttribute('data-chief-active-session', primary);
      await expect(strip.locator(`[data-chief-chip="${p1}"]`)).toBeHidden();
      await tap(strip.locator('button[data-chief-archived]'));
      await expect(strip.locator(`[data-chief-chip="${p1}"]`)).toBeVisible();
      expect(page.url(), 'the grid stays').toBe(grid);
      const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
      expect(sw).toBeLessThanOrEqual(cw);
    });
  });
}
