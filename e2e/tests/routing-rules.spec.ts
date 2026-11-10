import http from 'node:http';
import { execFileSync } from 'node:child_process';
import { request as pwRequest, APIRequestContext } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, repoRoot, routingRulesFakePort, routingRulesPort } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';

// ROUTING RULES AND THE USAGE PAGE (docs/plugins.md#routing-rules,
// docs/usage.md), on the operator's click path at a phone and a desktop width:
//
//   1. a plugin message reaches the chief (one turn); the chief proposes a
//      settle rule for its sender (routing_rule_propose);
//   2. the owner taps Approve rule on the card on the Attention view;
//   3. a second message from that sender is settled by muster with NO chief
//      turn, and the chief's chat shows it as a rule hit ("muster: rule #N");
//   4. the Rules page lists the rule with its hit, and Disable stops it;
//   5. the Usage page renders the totals and the plugin's split.
//
// ⚠ WHAT IS PLAYED: the model (a FAKE /v1/responses runtime is the chief: on a
// plugin message from a sender with "propose" in it, it calls
// routing_rule_propose for that message; on any other it answers "noted"; it
// reports usage on every turn). Everything else is muster's own: the door, the
// deliverer, the tool, the card, the tap, the dispatch, the pages. Every value
// is invented fixture data.
//
// 🔴 REGRESSION TEST: on origin/main the chief has no routing_rule_propose, no
// card is raised, the second message is a second chief turn, and /rules and
// /usage are 404.

const stamp = Date.now().toString(36);
const chiefName = 'e2e-rules-chief-' + stamp;
const chiefToken = 'e2e-rules-chief-token-not-a-real-one-' + stamp;

const rig = new MirrorRig(routingRulesPort, 'e2e-rules', {
  MUSTER_FOCUS_INTERVAL: '0',
  MUSTER_AGENT_GATEWAY: 'hooks-sha256',
  MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
  MUSTER_AGENT_RUNTIME_CONFIG: '{}',
  MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
  MUSTER_AGENT_KINDS: 'gateway',
  MUSTER_AGENT_OPENROUTER_KEY: 'sk-or-v1-FAKE-e2e-not-a-real-key',
  MUSTER_E2E_AGENT_ENDPOINT: `http://127.0.0.1:${routingRulesFakePort}`,
  MUSTER_PLUGINS: '1',
  // The task triage (task_triage.go): a plugin's task, assigned and routed to
  // the chief, that muster puts on the board itself.
  MUSTER_TASK_ROUTING: '1',
  MUSTER_PLUGIN_TASK_NAMESPACES: 'tracker',
  // The card is an attention ask, which asks for suggested replies: pointed at
  // the fake runtime (which answers 404), so nothing real is ever called.
  MUSTER_E2E_SUGGEST_URL: `http://127.0.0.1:${routingRulesFakePort}/v1/chat/completions`,
});
let runtime: http.Server | undefined;
let chiefMade = false;
// pluginTurns: every chief turn the runtime was asked for a plugin message
// (the first request of a turn: no function_call_output in its input).
const pluginTurns: string[] = [];
// requests: every /v1/responses request the runtime got, any turn.
let requests = 0;
const asked: string[] = [];

const sse = (type: string, data: Record<string, unknown>) => `event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`;
const USAGE = { input_tokens: 40, output_tokens: 12, cache_read_input_tokens: 900, cache_creation_input_tokens: 48 };

function answer(res: http.ServerResponse, out: { text?: string; call?: { id: string; name: string; args: Record<string, unknown> } }) {
  res.setHeader('Content-Type', 'text/event-stream');
  const output: unknown[] = [];
  if (out.text) {
    res.write(sse('response.output_text.delta', { delta: out.text, output_index: 0 }));
    output.push({ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: out.text }] });
  }
  if (out.call) output.push({ type: 'function_call', call_id: out.call.id, name: out.call.name, arguments: JSON.stringify(out.call.args) });
  res.end(sse('response.completed', { response: { status: 'completed', output, usage: USAGE } }));
}

function script(body: { input?: { type?: string; content?: string; output?: string }[] }) {
  const input = body.input ?? [];
  const first = input.find((i) => i.type === 'message')?.content ?? '';
  const outputs = input.filter((i) => i.type === 'function_call_output').map((i) => i.output ?? '');
  requests++;
  asked.push(first.slice(0, 300));
  if (outputs.length > 0) return { text: 'CHIEF-PROPOSED: ' + outputs[0].slice(0, 300) };
  const id = /message_id (\d+)/.exec(first)?.[1];
  if (!first.startsWith('Plugin ') || !id) return { text: 'ok' };
  pluginTurns.push(first.slice(0, 200));
  if (!first.includes('propose-')) return { text: 'noted' };
  return {
    call: { id: 'call_rule_' + pluginTurns.length, name: 'routing_rule_propose',
      args: { message_id: Number(id), action: 'settle', reason: 'a weekly digest: nothing to do', match_sender: true } },
  };
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
  await new Promise<void>((r) => runtime!.listen(routingRulesFakePort, '127.0.0.1', () => r()));
  await rig.start();
  const chiefID = fix('chief', '1', chiefName, chiefToken);
  chiefMade = true;
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await rig.login(page);
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
  console.log('fake chief asked (last 6):', JSON.stringify(asked.slice(-6), null, 1).slice(0, 3000));
  console.log('page:', page.url());
  console.log('server log (tail):', (rig.server?.log() ?? '').split('\n').slice(-60).join('\n'));
});

for (const vp of [
  { name: 'phone 390×844', key: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', key: 'desktop', use: { viewport: { width: 1440, height: 900 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`on a ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('the chief proposes a rule → one tap → a matching message skips the chief → disable → usage', async ({ page }, info) => {
      test.setTimeout(150_000);
      const tap = async (sel: string) => (vp.use.hasTouch ? page.locator(sel).first().tap() : page.locator(sel).first().click());
      const pluginName = `rules-${vp.key}-${stamp}`;
      const sender = `propose-${vp.key}@digest.example.test`;
      await rig.login(page);

      // A plugin of this viewport's own, minted on /plugins.
      await page.goto(rig.origin + '/plugins');
      await page.fill('#plugin-name', pluginName);
      await page.fill('#plugin-label', `Digest ${vp.key}`);
      await page.fill('#plugin-scope', pluginName);
      await page.getByRole('button', { name: 'Create plugin token' }).click();
      const token = await page.locator('#minted-plugin-token').inputValue();
      expect(token).toMatch(/^mpl_[A-Za-z0-9_-]{43}$/);
      const plugin: APIRequestContext = await pwRequest.newContext({ baseURL: rig.origin, extraHTTPHeaders: { Authorization: `Bearer ${token}` } });
      const post = async (n: number) => {
        const r = await plugin.post('/plugin/v1/messages', {
          data: { external_id: `e2e-rules-${vp.key}-${n}-${stamp}`, sender, subject: 'Weekly digest', body: 'Three widgets shipped this week.', sender_verified: true },
        });
        expect(r.status(), await r.text()).toBe(202);
        return ((await r.json()) as { id: number }).id;
      };

      // 1. The first message: the chief's turn, which proposes the rule.
      const turns0 = pluginTurns.length;
      await post(1);
      await expect.poll(() => pluginTurns.length, { timeout: 30_000 }).toBe(turns0 + 1);

      // 2. The card on the Attention view; the owner's tap.
      await page.goto(rig.origin + '/attention');
      const card = page.locator('li[data-attention-item]').filter({ hasText: 'Your chief proposes a routing rule' }).filter({ hasText: sender });
      await expect(card).toBeVisible({ timeout: 30_000 });
      await expect(card).toContainText('settle it: "a weekly digest: nothing to do"');
      await card.scrollIntoViewIfNeeded();
      await page.screenshot({ path: info.outputPath(`rule-card-${vp.key}.png`) });
      const approve = card.locator('button[data-attention-option][value="Approve rule"]');
      await (vp.use.hasTouch ? approve.tap() : approve.click());
      await expect(page.locator('li[data-attention-item]').filter({ hasText: sender }).locator('button[data-attention-option]')).toHaveCount(0, { timeout: 15_000 });

      // The Rules page: the rule, active, no hits yet.
      await page.goto(rig.origin + '/rules');
      const row = page.locator('[data-rule-id]').filter({ hasText: pluginName });
      await expect(row).toHaveAttribute('data-rule-state', 'active');
      await expect(row.locator('[data-rule-match]')).toContainText(`sender is "${sender}" (verified sender)`);
      const ruleID = await row.getAttribute('data-rule-id');

      // 3. A second message from that sender: settled by muster, NO chief turn.
      const turns1 = pluginTurns.length;
      const second = await post(2);
      await expect.poll(async () => {
        await page.goto(rig.origin + '/rules');
        return page.locator('[data-rule-id]').filter({ hasText: pluginName }).locator('[data-rule-hits]').getAttribute('data-rule-hits');
      }, { timeout: 30_000 }).toBe('1');
      await page.waitForTimeout(1000);
      expect(pluginTurns.length, 'a message the rule handled cost no chief turn').toBe(turns1);
      await page.screenshot({ path: info.outputPath(`rules-hit-${vp.key}.png`) });

      // The chief's chat: the hit, as a relay row naming the rule.
      await page.goto(rig.origin + `/agents/${chiefName}`);
      const relay = page.locator('[data-relay-row]').filter({ hasText: `muster: rule #${ruleID}` });
      await expect(relay).toHaveCount(1, { timeout: 20_000 });
      await relay.locator('summary').click();
      await expect(relay).toContainText(`plugin message_id ${second} was settled by muster: rule #${ruleID} — a weekly digest: nothing to do`);
      await relay.scrollIntoViewIfNeeded();
      await page.screenshot({ path: info.outputPath(`chief-chat-hit-${vp.key}.png`) });

      // 4. Disable it on the Rules page: the next matching message is the chief's.
      await page.goto(rig.origin + '/rules');
      await tap(`[data-rule-id="${ruleID}"] [data-rule-disable]`);
      await expect(page.locator(`[data-rule-id="${ruleID}"]`)).toHaveAttribute('data-rule-state', 'disabled');
      await expect(page.locator(`[data-rule-id="${ruleID}"] [data-rule-enable]`)).toBeVisible();
      const turns2 = pluginTurns.length;
      await post(3);
      await expect.poll(() => pluginTurns.length, { timeout: 30_000 }).toBe(turns2 + 1);

      // 5. The Usage page: the totals, and this plugin's three messages — two
      // by a chief turn, one by the rule.
      await page.goto(rig.origin + '/usage');
      const totals = page.locator('[data-testid="usage-totals"]');
      await expect(totals).toBeVisible();
      expect(Number(await totals.getAttribute('data-by-rule'))).toBeGreaterThanOrEqual(1);
      expect(Number(await totals.getAttribute('data-turns'))).toBeGreaterThanOrEqual(2);
      expect(Number(await totals.getAttribute('data-tokens'))).toBeGreaterThan(0);
      const src = page.locator(`[data-usage-source="${pluginName}"]`);
      await expect(src).toContainText(`${pluginName} · 3`);
      await expect(src).toContainText('1 by a rule · 0 by muster · 2 by a chief turn');
      await expect(page.locator(`[data-usage-rule="${ruleID}"]`)).toContainText('1 hit in this window (1 in all)');
      // No horizontal page scroll at this width.
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      expect(overflow, 'the usage page scrolls sideways').toBeLessThanOrEqual(0);
      await page.screenshot({ path: info.outputPath(`usage-${vp.key}.png`), fullPage: true });

      // 6. THE TASK TRIAGE: an assigned task nobody is on reaches the board
      // as the chief's own "Task #N needs you" card, with NO chief turn.
      const before = requests;
      const title = `Rotate the ${vp.key} build key ${stamp}`;
      const filed = await plugin.post('/plugin/v1/tasks', {
        data: { key: `tracker:e2e-${vp.key}-${stamp}`, create: { title, body: 'Rotate it before Friday.' },
          comment: 'upstream: assigned to you', route_to_chief: true, notice: 'assigned' },
      });
      expect(filed.status(), await filed.text()).toBe(201);
      const taskID = ((await filed.json()) as { id: number }).id;
      await page.goto(rig.origin + '/attention');
      const board = page.locator('li[data-attention-item]').filter({ hasText: `Task #${taskID} needs you: ${title}` });
      await expect(board).toBeVisible({ timeout: 30_000 });
      await board.scrollIntoViewIfNeeded();
      await page.screenshot({ path: info.outputPath(`task-board-${vp.key}.png`) });
      await page.waitForTimeout(1000);
      expect(requests, 'the board card cost no chief turn').toBe(before);
      // Routed by muster: not left on the queue as a task to route.
      await page.reload();
      await expect(page.locator('li[data-attention-item][data-kind="task"]').filter({ hasText: `Task #${taskID}:` })).toHaveCount(0);
      await page.goto(rig.origin + '/usage');
      await expect(page.locator(`[data-usage-source="${pluginName}"]`)).toContainText('1 by muster');
      await plugin.dispose();
    });
  });
}
