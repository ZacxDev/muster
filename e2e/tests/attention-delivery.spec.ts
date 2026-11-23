import http from 'node:http';
import { execFileSync } from 'node:child_process';
import { Frame, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { attentionDeliveryConnectPort, attentionDeliveryFakePort, attentionDeliveryPort, dsn, repoRoot, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';

// AN ANSWER IS DELIVERED TO THE CHIEF THAT ASKED (internal/api/attention_delivery.go),
// the operator's click path, every muster hop real:
//
//   the chief asks over the real /mcp door (attention_ask, with options) and
//   does NOT wait → /attention lists it → the operator taps an option → muster
//   pushes the answer into the chief's default session as a turn, through the
//   real agent gateway, to a FAKE RUNTIME this spec runs (the e2eterm build's
//   MUSTER_E2E_AGENT_ENDPOINT points every agent endpoint at it) → the item's
//   Answered entry says "Delivered to chief" → the chief panel shows the
//   delivered answer and the chief's reply.
//
// ⚠ WHAT IS PLAYED: the runtime (it answers every /v1/responses turn with one
// fixed sentence and records what it was sent) and the chief's pod (a fixture
// row, e2e/pluginfix chief). The ask, the answer form, the delivery, the gateway
// request, the stored turn and the panel are muster's own.

const origin = `http://localhost:${attentionDeliveryPort}`;
const connectURL = `http://127.0.0.1:${attentionDeliveryConnectPort}`;
const stamp = Date.now().toString(36);
const chiefName = 'e2e-deliv-chief-' + stamp;
const chiefToken = 'e2e-deliv-chief-token-not-a-real-one-' + stamp;
const REPLY = 'On it: starting a new agent on the task.';

let server: Running | undefined;
let runtime: http.Server | undefined;
let chiefMade = false;
const turns: string[] = [];

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

// sse is one Server-Sent Event of the /v1/responses stream.
const sse = (type: string, data: Record<string, unknown>) => `event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`;

test.beforeAll(async () => {
  test.setTimeout(180_000);
  runtime = http.createServer((req, res) => {
    let body = '';
    req.on('data', (d) => (body += d));
    req.on('end', () => {
      if (req.method !== 'POST' || !(req.url || '').endsWith('/v1/responses')) {
        res.statusCode = 404;
        return res.end();
      }
      // The turn's user message, as the gateway sent it (/v1/responses `input`).
      const input = (JSON.parse(body) as { input?: { content?: string }[] }).input ?? [];
      turns.push(input.map((m) => m.content ?? '').join('\n'));
      res.setHeader('Content-Type', 'text/event-stream');
      res.write(sse('response.output_text.delta', { delta: REPLY, output_index: 0 }));
      res.end(
        sse('response.completed', {
          response: { status: 'completed', output: [{ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: REPLY }] }] },
        }),
      );
    });
  });
  await new Promise<void>((r) => runtime!.listen(attentionDeliveryFakePort, '127.0.0.1', () => r()));
  server = await startServer(versionTerm, attentionDeliveryPort, {
    MUSTER_FOCUS_INTERVAL: '0',
    // Plugins on, so the page has its Drafts tab beside the Answered tab
    // (asks_drafts.go). No plugin is ever created.
    MUSTER_PLUGINS: '1',
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x4e).toString('base64'),
    MUSTER_CONNECT_PORT: String(attentionDeliveryConnectPort),
    MUSTER_AGENT_CONNECT_URL: connectURL,
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_GATEWAY: 'hooks-sha256',
    MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
    // The runtime bundle the gateway needs at boot (nothing is provisioned).
    MUSTER_AGENT_RUNTIME_CONFIG: '{}',
    MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
    MUSTER_E2E_AGENT_ENDPOINT: `http://127.0.0.1:${attentionDeliveryFakePort}`,
  });
  fix('chief', '1', chiefName, chiefToken);
  chiefMade = true;
});

test.afterAll(async () => {
  try {
    if (chiefMade) fix('cleanup', '1', chiefName);
  } finally {
    await server?.stop();
    await new Promise<void>((r) => (runtime ? runtime.close(() => r()) : r()));
  }
});

async function mcpCall(name: string, args: Record<string, unknown>): Promise<{ text: string; isError: boolean }> {
  const r = await fetch(connectURL + '/mcp', {
    method: 'POST',
    headers: { 'X-Muster-Token': chiefToken, 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name, arguments: args } }),
  });
  expect(r.status, 'the /mcp door answered').toBe(200);
  const j = (await r.json()) as { result?: { content: { text: string }[]; isError?: boolean }; error?: unknown };
  expect(j.error, JSON.stringify(j.error)).toBeUndefined();
  return { text: j.result!.content[0].text, isError: !!j.result!.isError };
}

async function openChiefPanel(page: Page) {
  await page.evaluate(() => {
    const b = document.createElement('button');
    b.setAttribute('data-chief-panel-toggle', '');
    b.style.cssText = 'position:fixed;left:-9999px;top:0';
    document.body.appendChild(b);
    b.click();
  });
  await expect(page.locator('#chief-panel')).toHaveAttribute('data-chief-panel-open', '1');
}

test('the operator answers the chief on /attention and the chief\'s session receives the answer as a turn, once', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page, origin);

  // The chief asks and does not wait: its tool result says it need not.
  const asked = await mcpCall('attention_ask', { question: 'Who should take task 42?', options: ['forward it', 'dispatch a new agent'] });
  expect(asked.isError, asked.text).toBe(false);
  const { id, next } = JSON.parse(asked.text) as { id: number; next: string };
  expect(next).toContain('you do not need to wait');

  await page.goto(origin + '/attention');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Asks');
  await expect(page.getByRole('tab', { name: 'Open', exact: true })).toHaveAttribute('aria-selected', 'true');
  await expect(page.getByRole('tab', { name: 'Drafts', exact: true })).toHaveAttribute('aria-selected', 'false');
  const item = page.locator(`#ask-${id}`);
  await expect(item.locator('[data-attention-text]')).toHaveText('Who should take task 42?');
  await Promise.all([page.waitForURL(origin + '/attention'), item.getByRole('button', { name: 'dispatch a new agent' }).click()]);
  await expect(page.locator(`#ask-${id}`)).toHaveCount(0);

  // Where the answer went, live on the Answered tab (after the waiter's grace).
  await page.getByRole('tab', { name: 'Answered', exact: true }).click();
  await expect(page).toHaveURL(origin + '/attention?tab=answered');
  const answered = page.locator(`#answered-${id}`);
  await expect(answered.locator('[data-attention-answer-text]')).toHaveText('Your answer: dispatch a new agent');
  await expect(answered).toHaveAttribute('data-delivery', 'delivered', { timeout: 30_000 });
  await expect(answered.locator('[data-attention-delivery]')).toHaveText('Delivered to chief as a new message in its session.');
  // The Answered card is the Answered tab's: visible there, and hidden with it.
  await expect(page.locator('#asks-tabs-panel-answered').locator(`#answered-${id}`)).toBeVisible();
  await page.getByRole('tab', { name: 'Drafts', exact: true }).click();
  await expect(answered).toBeHidden();
  await expect(page.locator('#asks-drafts-pending [data-drafts-empty]')).toBeVisible();
  await expect(page).toHaveURL(origin + '/attention?tab=drafts');

  // The runtime got exactly one turn carrying it, fenced.
  const carrying = turns.filter((b) => b.includes(`your owner answered attention #${id}`));
  expect(carrying, `turns the runtime received: ${turns.length}`).toHaveLength(1);
  expect(carrying[0]).toContain('<<<attention-answer untrusted nonce=');
  expect(carrying[0]).toContain('dispatch a new agent');

  // The chief panel shows the delivered answer and the chief's reply.
  await page.goto(origin + '/tasks');
  await openChiefPanel(page);
  const log = page.locator('#chief-panel #chat-log');
  await expect(log).toContainText(`your owner answered attention #${id}`, { timeout: 15_000 });
  await expect(log).toContainText(REPLY);
});

// ANSWERED FROM THE CHIEF'S HEADER BADGE (attention_asker.go): the chief's page
// draws no asks block — its header counts the chief's open asks, live (a new
// ask raises it and an ask answered elsewhere lowers it, with no reload, and
// the shell's Asks badge follows too: the badge's refresh is what relays the
// queue's event to the sidebar on this page). A tap on the count opens the
// Asks page filtered to the chief's asks; the operator taps an option there,
// the card leaves, and the chief's session receives the answer as a turn —
// once.
// THE CATCH-UP: an ask filed while the page's stream is down (its event
// missed) still reaches the Open button, by the asks badge's own catch-up
// (askerAsksRefresh, once a minute), which the button follows
// (htmx:afterSwap on the badge, ui slotTrigger). The stream is held down from
// the page's load (/events refused), so no event can carry it; at 390×844,
// where the button is the only count drawn. RED with the slot's trigger
// reduced to "": the button kept 1.
test('the Open button catches up with an ask whose event the page missed (390×844)', async ({ page }) => {
  test.setTimeout(150_000);
  await signIn(page, origin);
  await page.route('**/events', (r) => r.abort());
  await page.goto(origin + '/agents/' + chiefName);
  await expect(page.locator('#chat-form')).toBeVisible();
  const open = page.locator('header #preview-toggle');
  // No open ask yet draws no button (nothing to count); one is drawn bare
  // or counted otherwise.
  const drawn = (await open.count()) > 0;
  const before = drawn ? Number((await open.getAttribute('data-open-asks')) ?? '0') : 0;
  const asked = await mcpCall('attention_ask', { question: `Missed while asleep (${stamp})?` });
  const id = (JSON.parse(asked.text) as { id: number }).id;
  // No event reaches the page: nothing moves at once.
  await page.waitForTimeout(3_000);
  if (!drawn) await expect(open).toHaveCount(0);
  else await expect(open).toHaveAttribute('data-open-asks', String(before));
  // The badge's catch-up (within its minute) re-reads, and the button with it.
  await expect(open, 'the Open button caught up').toHaveAttribute('data-open-asks', String(before + 1), { timeout: 75_000 });
  const r = await page.request.post(origin + `/attention/asks/${id}/answer`, { form: { answer: 'caught up' }, headers: { Origin: origin }, maxRedirects: 0 });
  expect(r.status()).toBe(303);
});

for (const vp of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`on a ${vp.name}`, () => {
    test.use(vp.use);
    test('the operator answers the chief from its page\'s asks badge and the chief receives it', async ({ page }) => {
      test.setTimeout(120_000);
      await signIn(page, origin);

      const asked = await mcpCall('attention_ask', { question: `Who takes task 43 (${vp.name})?`, options: ['forward it', 'dispatch a new agent'] });
      expect(asked.isError, asked.text).toBe(false);
      const { id } = JSON.parse(asked.text) as { id: number };

      const onChief = (u: URL) => u.pathname === '/agents/' + chiefName;
      // The notification's URL (the tap route) resolves to the ask's own page,
      // whose context link is the chief's page.
      await page.goto(origin + `/attention/open/ask-${id}`);
      await page.waitForURL(origin + `/attention/asks/${id}`);
      await page.locator('[data-ask-context]').click();
      await page.waitForURL(onChief);
      const count = page.locator('header [data-asker-asks-link]');
      await expect(count).toHaveAttribute('data-count', '1');
      // The Open button counts the same open ask — on a phone the only count
      // drawn (ui agentAsksBadge) — and follows the queue with the badge.
      const open = page.locator('header #preview-toggle');
      await expect(open).toHaveAttribute('data-open-asks', '1');
      await expect(page.locator('#asker-asks'), 'no asks block').toHaveCount(0);
      await expect(page.locator(`#ask-${id}`), 'no ask card on the chief\'s page').toHaveCount(0);

      // LIVE, both ways, with no reload: a second ask raises the count, and
      // answered somewhere else (here: the queue's own answer route, as the
      // /attention page or another device would post it) lowers it; the
      // shell's badge follows.
      let navigations = 0;
      const nav = (f: Frame) => {
        if (f === page.mainFrame()) navigations++;
      };
      page.on('framenavigated', nav);
      const shell = page.locator('#attention-badge');
      const second = await mcpCall('attention_ask', { question: `And task 44 (${vp.name})?` });
      const id2 = (JSON.parse(second.text) as { id: number }).id;
      await expect(count).toHaveAttribute('data-count', '2', { timeout: 15_000 });
      await expect(shell).toHaveAttribute('data-count', '2', { timeout: 15_000 });
      await expect(open, 'the Open button follows a new ask').toHaveAttribute('data-open-asks', '2', { timeout: 15_000 });
      const answered = await page.request.post(origin + `/attention/asks/${id2}/answer`, {
        form: { answer: 'later' },
        headers: { Origin: origin },
        maxRedirects: 0,
      });
      expect(answered.status()).toBe(303);
      await expect(count).toHaveAttribute('data-count', '1', { timeout: 15_000 });
      await expect(open, 'the Open button follows an answer').toHaveAttribute('data-open-asks', '1', { timeout: 15_000 });
      await expect(shell).toHaveAttribute('data-count', '1', { timeout: 15_000 });
      expect(navigations, 'the page was not reloaded').toBe(0);
      page.off('framenavigated', nav);

      // The tap: the Asks page filtered to the chief's asks. 🔴 On a phone the
      // badge is not drawn on an agent of the viewer's own (operator decision
      // on #999: the Open button counts the same asks, ui agentAsksBadge) —
      // it is still on the page and refreshed live, as above — so its link
      // is followed there, and the Open button's count checked in its place.
      if (vp.use.viewport.width < 640) {
        await expect(count).toBeHidden();
        await expect(page.locator('header #preview-toggle')).toHaveAttribute('data-open-asks', '1', { timeout: 15_000 });
        const href = await count.getAttribute('href');
        await Promise.all([
          page.waitForURL((u) => u.pathname === '/attention' && u.searchParams.get('from') === 'agent:' + chiefName),
          page.goto(origin + href!),
        ]);
      } else {
        await Promise.all([
          page.waitForURL((u) => u.pathname === '/attention' && u.searchParams.get('from') === 'agent:' + chiefName),
          count.click(),
        ]);
      }
      // Whose asks these are: the chief's chip, active; and ONE predicate —
      // the badge's last number (1) is the number of cards the tap lists.
      await expect(page.locator('[data-asks-chips] a[aria-current="true"][data-asks-chip="from"]')).toHaveCount(1);
      await expect(page.locator('#attention-items > li[data-asks-card]')).toHaveCount(1);
      const item = page.locator(`#attention-items #ask-${id}`);
      await expect(item.locator('[data-attention-text]')).toHaveText(`Who takes task 43 (${vp.name})?`);
      await item.getByRole('button', { name: 'dispatch a new agent' }).click();
      await expect(page.locator(`#ask-${id}`)).toHaveCount(0, { timeout: 15_000 });
      await expect
        .poll(() => turns.filter((b) => b.includes(`your owner answered attention #${id}`)).length, { timeout: 30_000 })
        .toBe(1);
      const carrying = turns.find((b) => b.includes(`your owner answered attention #${id}`))!;
      expect(carrying).toContain('dispatch a new agent');
      // Back on the chief's page, nothing is open: no count.
      await page.goto(origin + '/agents/' + chiefName);
      await expect(page.locator('#chat-form')).toBeVisible();
      await expect(count).toHaveCount(0);
    });
  });
}

