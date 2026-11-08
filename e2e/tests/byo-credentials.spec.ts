import { execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import path from 'node:path';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, e2eRoot, gotrueSecret, port, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { createAgent, postAgents } from '../lib/seed';
import { landedHome } from '../lib/nav';

// ccAccount is the agent page header's Claude account (the second row's plain
// account name, data-cc-account; the Terminal switch beside it is what says
// the kind).
const ccAccount = (page: Page, account: string) => page.locator(`header [data-cc-account="${account}"]`);


// Bring-your-own credentials, end to end at 412 px against the REAL GoTrue
// (e2e/gotrue/compose.yml) and a gotrue-mode muster with the claude-code kind
// enabled over the noop driver and an ENVIRONMENT pool (the admin's).
//
// A MEMBER signs in, is refused an agent of either kind with what to add, adds a
// (fake-shaped) Claude setup token in Account → Credentials, and an agent
// created as them (POST /api/agents with the member's own agent-create token —
// the route that replaced the deleted dispatch form, docs/design/chief-dispatch.md
// phase 2) then runs Claude Code on THAT account — and never the admin's pool.
// A stopped create provisions nothing (noop), so no token is ever used; every
// token below is an obvious fake that never leaves this machine.

// Own ports, own compose project: this spec must not share GoTrue or a
// listener with gotrue-login.spec.ts, which may run in the same leg.
const mainPort = port + 20;
const browserPort = port + 21;
const gotruePort = Number(process.env.MUSTER_E2E_BYO_GOTRUE_PORT ?? port + 22);
const connectPortBYO = port + 23;
const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
const project = `muster-e2e-byo-${gotruePort}`;
const composeEnv = {
  ...process.env,
  MUSTER_E2E_GOTRUE_PROJECT: project,
  MUSTER_E2E_GOTRUE_PORT: String(gotruePort),
  MUSTER_E2E_GOTRUE_SITE: `http://localhost:${browserPort}`,
};
const browserURL = `http://localhost:${browserPort}`;
const gotrueURL = `http://127.0.0.1:${gotruePort}`;
const ADMIN_POOL = 'adminpool-alpha';
const MEMBER_ACCOUNT = 'e2e-personal';
const MEMBER_TOKEN = 'sk-ant-oat01-FAKE-e2e-member-token-0123456789';

let server: Running | undefined;

test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(240_000);
  execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
  server = await startServer(versionA, mainPort, {
    MUSTER_AUTH_MODE: 'gotrue',
    MUSTER_GOTRUE_URL: gotrueURL,
    MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
    MUSTER_BROWSER_PORT: String(browserPort),
    MUSTER_PUBLIC_URL: browserURL,
    // The credential store's key is derived from the connector key, and the
    // connector store refuses to boot without the URL agents are told to use.
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x3c).toString('base64'),
    MUSTER_CONNECT_PORT: String(connectPortBYO),
    MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${connectPortBYO}`,
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_OPENROUTER_KEY: 'sk-or-v1-FAKE-e2e-admin-env-key-000000',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: ADMIN_POOL,
    MUSTER_AGENT_CC_TOKEN_ADMINPOOL_ALPHA: 'sk-ant-oat01-FAKE-e2e-admin-pool-0000000000',
  });
});

test.afterAll(async () => {
  await server?.stop();
  execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
});

function b64url(v: string | Buffer): string {
  return Buffer.from(v).toString('base64url');
}

// A 60 s service_role token for GoTrue's admin API, signed with the shared
// secret (exactly what muster's own GoTrue client mints).
function serviceRoleJWT(): string {
  const now = Math.floor(Date.now() / 1000);
  const head = b64url(JSON.stringify({ alg: 'HS256', typ: 'JWT' }));
  const body = b64url(JSON.stringify({ role: 'service_role', aud: 'authenticated', iss: 'muster-auth', iat: now, exp: now + 60 }));
  const sig = crypto.createHmac('sha256', gotrueSecret).update(`${head}.${body}`).digest('base64url');
  return `${head}.${body}.${sig}`;
}

// A member: a GoTrue user plus a muster users row (role member) bound to it —
// what a redeemed invite produces.
async function makeMember(email: string, password: string): Promise<number> {
  const r = await fetch(gotrueURL + '/admin/users', {
    method: 'POST',
    headers: { Authorization: `Bearer ${serviceRoleJWT()}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password, email_confirm: true }),
  });
  const body = (await r.json()) as { id?: string };
  expect(r.status, JSON.stringify(body)).toBeLessThan(300);
  const out = execFileSync('go', ['run', './e2e/mkmember'], {
    cwd: repoRoot,
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_E2E_SUB: body.id!, MUSTER_E2E_EMAIL: email },
  });
  return Number(out.toString().trim());
}

test('a member brings their own Claude token; an agent they create runs on it, never the admin pool (real GoTrue, 412px)', async ({ page }) => {
  test.setTimeout(120_000);
  const email = `member-${Date.now()}@example.com`;
  const password = 'e2e-member-password-not-a-real-one';
  const uid = await makeMember(email, password);
  expect(uid).toBeGreaterThan(1);
  const machineURL = `http://localhost:${mainPort}`;

  await page.goto(browserURL + '/login');
  await page.fill('#muster-email', email);
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL(landedHome(browserURL)), page.click('button[type=submit]')]);

  // 1. No credentials: both kinds are refused, each saying what to add, and
  //    the admin's pool is never offered.
  const noClaude = await postAgents(machineURL, { user: uid, kind: 'claude-code', prompt: 'e2e byo' });
  expect(noClaude.status, noClaude.text).toBe(409);
  expect(noClaude.text).toContain('Add your Claude setup token in Account → Credentials');
  const noKey = await postAgents(machineURL, { user: uid, kind: 'gateway', prompt: 'e2e byo' });
  expect(noKey.status, noKey.text).toBe(409);
  expect(noKey.text).toContain('Add your OpenRouter key in Account → Credentials');
  const pinned = await postAgents(machineURL, { user: uid, kind: 'claude-code', cc_account: ADMIN_POOL, prompt: 'e2e byo' });
  expect(pinned.status, `pinning the admin's pool: ${pinned.text}`).toBe(409);

  // 2. Account → Credentials: add the setup token.
  await page.goto(browserURL + '/account/credentials');
  await expect(page.locator('[data-credentials-claude] [data-credentials-empty]')).toBeVisible();
  await page.fill('#cred-claude-name', MEMBER_ACCOUNT);
  await page.fill('#cred-claude-token', MEMBER_TOKEN);
  await Promise.all([
    page.waitForURL(browserURL + '/account/credentials?done=claude'),
    page.locator('[data-credentials-claude] button[type=submit]').last().click(),
  ]);
  await expect(page.getByText('Claude account saved.')).toBeVisible();
  await expect(page.locator(`[data-credential-account="${MEMBER_ACCOUNT}"]`)).toBeVisible();
  const credPage = await page.content();
  expect(credPage, 'the page never shows the token back').not.toContain('FAKE-e2e-member-token');
  expect(credPage).not.toContain(ADMIN_POOL);
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'no horizontal scroll at 412px').toBeLessThanOrEqual(cw);

  // 3. A stopped Claude Code agent created as the member lands on the member's
  //    own account — its page says so in its header's second row — and never
  //    the admin's pool.
  const agent = await createAgent(machineURL, { user: uid, kind: 'claude-code', prompt: 'e2e byo: on my own account' });
  await page.goto(`${browserURL}/agents/${agent.name}`);
  await expect(ccAccount(page, MEMBER_ACCOUNT)).toHaveText(MEMBER_ACCOUNT);
  await expect(ccAccount(page, MEMBER_ACCOUNT)).toBeVisible();
  const agentsPage = await page.content();
  expect(agentsPage).not.toContain(ADMIN_POOL);
  expect(agentsPage).not.toContain('sk-ant-oat01');
});
