import crypto from 'node:crypto';
import http from 'node:http';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { pluginAutostartConnectPort, pluginAutostartFakePort, pluginAutostartPort, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';

// A PLUGIN'S AUTO-START, TURNED ON AND OFF ON ITS /plugins CARD (docs/plugins.md
// #auto-start; internal/api plugin_autostart.go), at 390px, against a real
// muster-server that can start agents for the operator: the noop agent driver
// behind an agent gateway, task routing on with the operator in MUSTER_CHIEF_DISPATCH_OWNERS, and a fake
// GitHub App (the e2eterm build's MUSTER_E2E_GITHUB_API) listing two invented
// repositories. Synthetic data only.
//
//   mint a plugin → its card says auto-start is off → open it: the agent kind,
//   the repository (the App's), the daily limit; the Turn on form is a step-up
//   form → turn it on → the card says it is on, with the repository and the
//   limit → turn it off → off again.
//
// What a task of the plugin then does — the start, the fence, the push and the
// Stop, the cap, the budget, a gated task — is the Go tests'
// (internal/api/plugin_autostart_pg_test.go): the noop driver starts nothing a
// browser could see.

const origin = `http://localhost:${pluginAutostartPort}`;
const fake = `http://127.0.0.1:${pluginAutostartFakePort}`;
const REPOS = ['example-org/app', 'example-org/other'];
const APP_ID = '515151';
const APP_ACCOUNT = { login: 'e2e-autostart', id: 5151, type: 'User' };
const APP_KEY = crypto.generateKeyPairSync('rsa', { modulusLength: 2048 }).privateKey.export({ type: 'pkcs1', format: 'pem' }).toString();

let server: Running | undefined;
let upstream: http.Server | undefined;

test.use({ storageState: { cookies: [], origins: [] }, viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

test.beforeAll(async () => {
  test.setTimeout(120_000);
  upstream = http.createServer((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    const u = new URL(req.url || '/', fake);
    if (u.pathname === '/app/installations') return res.end(JSON.stringify([{ id: 88, account: APP_ACCOUNT }]));
    if (u.pathname === '/app/installations/88/access_tokens') {
      return res.end(JSON.stringify({ token: 'ghs_e2e_autostart_not_a_real_token', expires_at: new Date(Date.now() + 3600_000).toISOString() }));
    }
    if (u.pathname === '/installation/repositories') {
      return res.end(JSON.stringify({ total_count: REPOS.length, repositories: REPOS.map((full_name) => ({ full_name, private: false })) }));
    }
    res.end('{}');
  });
  await new Promise<void>((r) => upstream!.listen(pluginAutostartFakePort, '127.0.0.1', () => r()));
  server = await startServer(versionTerm, pluginAutostartPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    // An agent gateway: without one no first turn can be delivered, and
    // auto-start (agent_dispatch's deployment check) is rightly unavailable.
    MUSTER_AGENT_GATEWAY: 'hooks-sha256',
    MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
    MUSTER_AGENT_RUNTIME_CONFIG: '{}',
    MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
    MUSTER_AGENT_OPENROUTER_KEY: 'sk-or-e2e-autostart-not-a-real-key',
    MUSTER_GITHUB_APP_ID: APP_ID,
    MUSTER_GITHUB_APP_PRIVATE_KEY: APP_KEY,
    MUSTER_GITHUB_APP_ACCOUNTS: `${APP_ACCOUNT.login}:${APP_ACCOUNT.id}`,
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x5a).toString('base64'),
    MUSTER_CONNECT_PORT: String(pluginAutostartConnectPort),
    MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${pluginAutostartConnectPort}`,
    MUSTER_E2E_GITHUB_API: fake,
    MUSTER_PLUGINS: '1',
    MUSTER_PLUGIN_TASK_NAMESPACES: 'tracker',
    MUSTER_TASK_ROUTING: '1',
    MUSTER_CHIEF_DISPATCH_OWNERS: '1',
  });
});

test.afterAll(async () => {
  await server?.stop();
  await new Promise<void>((r) => (upstream ? upstream.close(() => r()) : r()));
});

test("the owner turns a plugin's auto-start on and off on its card (390px)", async ({ page }) => {
  await signIn(page, origin);
  await page.goto(origin + '/plugins');
  const name = 'example-auto-' + Date.now().toString(36);
  await page.fill('#plugin-name', name);
  await page.fill('#plugin-label', 'Example auto');
  await page.click('[data-testid="plugin-create"] button[type="submit"]');
  await expect(page.getByTestId('minted-plugin-token')).toBeVisible();

  const card = page.locator('[data-plugin-id]', { hasText: name });
  const off = card.locator('[data-plugin-autostart="off"]');
  await expect(off, 'a new plugin starts with auto-start off').toBeVisible();
  await off.locator('summary').click();
  const form = off.locator('form[data-plugin-autostart-on]');
  await expect(form, 'Turn on asks for a fresh sign-in').toHaveAttribute('data-step-up-form', '');
  const repo = form.locator('select[name="repo"]');
  await expect(repo.locator('option')).toHaveText(REPOS);
  await repo.selectOption('example-org/other');
  await form.locator('input[name="daily_cap"]').fill('2');
  // The sign-in is seconds old, so the step-up passes without the dialog.
  await form.getByRole('button', { name: 'Turn on auto-start' }).click();

  const on = card.locator('[data-plugin-autostart="on"]');
  await expect(on).toBeVisible();
  await expect(on).toContainText('example-org/other');
  await expect(on).toContainText('at most 2 a day');
  await on.getByRole('button', { name: 'Turn off auto-start' }).click();
  await expect(card.locator('[data-plugin-autostart="off"]')).toBeVisible();
  await expect(card.locator('[data-plugin-autostart="on"]')).toHaveCount(0);
});
