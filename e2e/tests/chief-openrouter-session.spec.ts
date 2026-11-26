import http from 'node:http';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { openSessions, sessionsTab } from '../lib/chief-sessions';
import { signIn } from '../lib/signin';
import { chiefOpenRouterCatalogPort, chiefOpenRouterConnectPort, chiefOpenRouterPort, versionTerm } from '../lib/env';
import { mkchat } from '../lib/mirrorhost';
import { startServer, Running } from '../lib/server';

// A CHIEF SIDE CHAT ON ANY OPENROUTER MODEL (the opencode runtime,
// docs/chief.md "OpenRouter sessions on opencode"), in a real browser, against
// a real muster-server with the spend door on: the New session picker and the
// Sessions drawer's New session menu offer OpenRouter with a search of the catalogue; a model is
// found by part of its name, picked with a tap, and the session it starts is
// on that model — the thread list and its Sessions row say so — and stored as
// it (a reload draws the same). A near miss starts nothing.
//
// The chief is a claude-code agent of this spec's own (e2e/mkchat), on a server
// of its own over the noop driver, its OpenRouter catalogue a fake on loopback
// (MUSTER_E2E_OPENROUTER_MODELS). Nothing is provisioned and no turn is sent:
// what is under test is the pick, the stored session and what is drawn.

const origin = `http://localhost:${chiefOpenRouterPort}`;
const chief = 'e2e-openrouter-chief-' + Date.now().toString(36);

let server: Running | undefined;
let catalog: http.Server | undefined;

test.use({ storageState: { cookies: [], origins: [] }, viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

test.beforeAll(async () => {
  test.setTimeout(120_000);
  catalog = http.createServer((_req, res) => {
    res.setHeader('Content-Type', 'application/json');
    res.end(
      JSON.stringify({
        data: [
          { id: 'deepseek/deepseek-v4.1-flash', name: 'DeepSeek: DeepSeek V4.1 Flash', context_length: 128000, pricing: { prompt: '0.00000015', completion: '0.0000006' } },
          { id: 'qwen/qwen3-coder', name: 'Qwen: Qwen3 Coder', context_length: 262144, pricing: { prompt: '0.0000002', completion: '0.0000008' } },
          { id: 'vendor-z/other-model', name: 'Vendor Z: Other', context_length: 32000, pricing: { prompt: '0.000001', completion: '0.000002' } },
        ],
      }),
    );
  });
  await new Promise<void>((r) => catalog!.listen(chiefOpenRouterCatalogPort, '127.0.0.1', () => r()));
  server = await startServer(versionTerm, chiefOpenRouterPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-openrouter',
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x6f).toString('base64'),
    MUSTER_CONNECT_PORT: String(chiefOpenRouterConnectPort),
    MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${chiefOpenRouterConnectPort}`,
    MUSTER_CHIEF_SESSION_RUNTIMES: 'claude-code,opencode',
    MUSTER_CHIEF_SESSIONS_OPENROUTER: '1',
    MUSTER_CHIEF_SESSIONS_OPENROUTER_KEY: 'sk-or-v1-FAKE-e2e-chief-sessions-key',
    MUSTER_E2E_OPENROUTER_MODELS: `http://127.0.0.1:${chiefOpenRouterCatalogPort}/models`,
  });
  mkchat(chief, null);
  mkchat(chief, [{ title: 'the original', hoursAgo: 2, messages: [{ role: 'user', content: 'hello chief' }] }], {
    MUSTER_E2E_DISPLAY_NAME: 'chief',
    MUSTER_E2E_THREADS: '1',
    MUSTER_E2E_KIND: 'claude-code',
    MUSTER_E2E_CC_ACCOUNT: 'alpha',
  });
});

test.afterAll(async () => {
  mkchat(chief, null);
  await server?.stop();
  await new Promise<void>((r) => (catalog ? catalog.close(() => r()) : r()));
});

// openPanel opens the chief panel (its toggle is delegated on the document).
async function openPanel(page: Page) {
  if ((await page.locator('#chief-panel').getAttribute('data-chief-panel-open')) !== '1') {
    await page.evaluate(() => {
      const b = document.createElement('button');
      b.setAttribute('data-chief-panel-toggle', '');
      b.style.cssText = 'position:fixed;left:-9999px;top:0';
      document.body.appendChild(b);
      b.click();
    });
    await expect(page.locator('#chief-panel')).toHaveAttribute('data-chief-panel-open', '1');
  }
  await expect(page.locator('#chief-thread-search')).toBeAttached({ timeout: 15_000 });
}

// openThreads opens the chief panel and then its thread list (which covers
// the bar naming the chat on screen). The same steps as chief-runtimes.spec.ts.
async function openThreads(page: Page) {
  await openPanel(page);
  await page.locator('label[for="chief-threads"]').first().click();
  await expect(page.locator('#chief-thread-search')).toBeVisible();
}

test('New session: search OpenRouter, pick a model, and the session is on it — listed, in the Sessions tab, stored', async ({ page }) => {
  await signIn(page, origin);
  await page.goto(origin + '/tasks');
  await openThreads(page);

  const picker = page.locator('[data-session-picker]');
  await expect(picker.locator('form[data-new-session="claude-code"]')).toHaveCount(1);
  const oc = picker.locator('form[data-new-session="opencode"]');
  await expect(oc).toHaveCount(1);
  await expect(oc.locator('select')).toHaveCount(0);

  // Part of the name, not the id: the search ranks fuzzily.
  const field = oc.locator('[data-openrouter-model-search]');
  await field.click();
  await field.pressSequentially('deepsee');
  const pick = oc.locator('button[data-openrouter-model-pick="openrouter/deepseek/deepseek-v4.1-flash"]');
  await expect(pick).toBeVisible({ timeout: 10_000 });
  await expect(pick).toContainText('DeepSeek: DeepSeek V4.1 Flash');
  await expect(pick).toContainText('$0.15 in, $0.60 out per M');
  await expect(oc.locator('button[data-openrouter-model-pick="openrouter/vendor-z/other-model"]')).toHaveCount(0);
  await pick.click();
  await expect(field).toHaveValue('openrouter/deepseek/deepseek-v4.1-flash');

  const active = page.locator('[data-chief-active-session]');
  const was = await active.getAttribute('data-chief-active-session');
  await oc.getByRole('button', { name: 'Start' }).click();
  await expect(active).not.toHaveAttribute('data-chief-active-session', was ?? '');
  await expect(active).not.toHaveAttribute('data-chief-active-session', '');

  // The Sessions row (and the bar) for the chat the panel is now on names the model.
  const tag = page.locator('[data-chief-chip-model="openrouter/deepseek/deepseek-v4.1-flash"]').first();
  await expect(tag).toHaveText('deepseek-v4.1-flash');
  await expect(tag).toHaveAttribute('title', 'Model: deepseek/deepseek-v4.1-flash');

  await openThreads(page);
  const line = page.locator('#chief-thread-results [data-session-runtime="opencode"]');
  await expect(line).toHaveCount(1);
  await expect(line).toHaveText('OpenRouter · deepseek/deepseek-v4.1-flash');

  // Stored, not only drawn.
  await page.reload();
  await openThreads(page);
  await expect(page.locator('#chief-thread-results [data-session-runtime="opencode"]')).toHaveText('OpenRouter · deepseek/deepseek-v4.1-flash');
});

test('"+": an OpenRouter model picked in the Sessions tab\'s New session menu opens a side chat on it', async ({ page }) => {
  await signIn(page, origin);
  await page.goto(origin + '/tasks');
  // The panel only: its thread list would cover the sheet.
  await openPanel(page);
  await openSessions(sessionsTab(page, 'panel'));
  const plus = page.locator('#chief-panel button[popovertarget="chief-panel-new-chat-menu"]');
  await plus.click();
  const menu = page.locator('#chief-panel-new-chat-menu');
  await expect(menu).toBeVisible();
  const form = menu.locator('form[data-chief-new-chat-openrouter]');
  const field = form.locator('[data-openrouter-model-search]');
  await field.click();
  await field.pressSequentially('qwen3');
  const pick = form.locator('button[data-openrouter-model-pick="openrouter/qwen/qwen3-coder"]');
  await expect(pick).toBeVisible({ timeout: 10_000 });
  await pick.click();
  await expect(field).toHaveValue('openrouter/qwen/qwen3-coder');
  const active = page.locator('[data-chief-active-session]');
  const was = await active.getAttribute('data-chief-active-session');
  await form.getByRole('button', { name: 'Start' }).click();
  await expect(active).not.toHaveAttribute('data-chief-active-session', was ?? '');
  await expect(page.locator('[data-chief-chip-model="openrouter/qwen/qwen3-coder"]').first()).toHaveText('qwen3-coder');
});

test('a near-miss OpenRouter model is refused and starts no session', async ({ page }) => {
  await signIn(page, origin);
  const count = async () =>
    ((await (await page.request.get(origin + '/ui/chief/threads/search?q=')).text()).match(/data-chief-thread=/g) || []).length;
  const before = await count();
  const panel = await page.request.get(origin + '/ui/chief/panel');
  const post = /hx-post="(\/agents\/\d+\/sessions)\?surface=chief-panel"/.exec(await panel.text());
  expect(post, 'the panel names the session-create route').not.toBeNull();
  for (const model of ['deepseek/deepseek-v4', 'not/in-the-catalogue', '']) {
    const r = await page.request.post(origin + post![1], { form: { runtime: 'opencode', model }, headers: { Origin: origin } });
    expect(r.status(), `model ${JSON.stringify(model)}`).toBe(400);
  }
  const side = /hx-post="(\/agents\/\d+\/sessions\/side)/.exec(await panel.text());
  expect(side, 'the panel names the side chat route').not.toBeNull();
  const r = await page.request.post(origin + side![1], { form: { runtime: 'opencode', model: 'deepseek/deepseek-v4' }, headers: { Origin: origin } });
  expect(r.status()).toBe(400);
  expect(await count()).toBe(before);
});
