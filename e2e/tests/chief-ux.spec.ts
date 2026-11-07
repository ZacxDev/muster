import http from 'node:http';
import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { chiefUxConnectPort, chiefUxFakePort, chiefUxPort, dsn, hookToken, repoRoot, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';

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

type Out = { text?: string; call?: { id: string; name: string; args: Record<string, unknown> } };

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
  res.end(sse('response.completed', { response: { status: 'completed', output } }));
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
