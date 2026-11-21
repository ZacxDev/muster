import { execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import path from 'node:path';
import { Browser, BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, e2eRoot, gotrueSecret, repoRoot, serverAppBrowserPort, serverAppEdgePort, serverAppGotruePort, serverAppMainPort, serverAppStubPort, versionA } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { landedHome } from '../lib/nav';
import { APP_ENTRY, APP_ZONE, AppStub, appStubArgs } from '../lib/appstub';

// A SERVER-INSTALLED APP (docs/plugins.md#server-apps; docs/design/meetings.md
// "Core seam: server-installed app plugins"; core PR 1h.2), on the click path
// in a real Chromium against a gotrue-mode muster, its own real GoTrue, and the
// STUB app plugin (e2e/lib/appstub.ts) on a real https origin under the zone,
// whose entry route redeems each code with the ONE server token.
//
//   1. An admin installs the app on /admin/apps, with a stale sign-in: the
//      form asks "Confirm it's you" in place, then installs; the token is shown
//      once.
//   2. Two people, in two browser contexts, each see the tab — nobody
//      restarted the stub — and each opens it: the stub redeemed two codes
//      with the one token and holds two DIFFERENT subjects.
//   3. B hides it on Account → Apps: B's open frame goes within 35 s (the
//      alive poll), A's stays.
//   4. The admin uninstalls it: A's frame goes within 35 s.
//
// Every value is invented fixture data.

const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
const project = `muster-e2e-gotrue-${serverAppGotruePort}`;
const composeEnv = {
  ...process.env,
  MUSTER_E2E_GOTRUE_PROJECT: project,
  MUSTER_E2E_GOTRUE_PORT: String(serverAppGotruePort),
  MUSTER_E2E_GOTRUE_SITE: `http://localhost:${serverAppBrowserPort}`,
};
const browserURL = `http://localhost:${serverAppBrowserPort}`;
const mainURL = `http://localhost:${serverAppMainPort}`;
const gotrueURL = `http://127.0.0.1:${serverAppGotruePort}`;
const stamp = Date.now().toString(36);
const adminEmail = `srvapp-admin-${stamp}@example.com`;
const adminPW = 'e2e-srvapp-admin-password-not-real';
const memberPW = 'e2e-srvapp-member-password-not-real';
const LABEL = 'Granite Room';
// The alive poll's period is 30 s; the frame goes within one period.
const GONE_MS = 35_000;

let server: Running | undefined;
const stub = new AppStub(mainURL, serverAppStubPort, serverAppEdgePort, false);
let appID = '';

test.use({ storageState: { cookies: [], origins: [] }, ignoreHTTPSErrors: true, launchOptions: { args: appStubArgs(serverAppEdgePort) } });
test.describe.configure({ mode: 'serial' });

const serverEnv = () => ({ ...process.env, DATABASE_URL: dsn, MUSTER_PUBLIC_URL: browserURL, MUSTER_GOTRUE_URL: gotrueURL, MUSTER_GOTRUE_JWT_SECRET: gotrueSecret });

function serviceRoleJWT(): string {
  const now = Math.floor(Date.now() / 1000);
  const b64url = (v: string) => Buffer.from(v).toString('base64url');
  const head = b64url(JSON.stringify({ alg: 'HS256', typ: 'JWT' }));
  const body = b64url(JSON.stringify({ role: 'service_role', aud: 'authenticated', iss: 'muster-auth', iat: now, exp: now + 60 }));
  const sig = crypto.createHmac('sha256', gotrueSecret).update(`${head}.${body}`).digest('base64url');
  return `${head}.${body}.${sig}`;
}

// makeMember is a GoTrue user bound to a new muster member row.
async function makeMember(email: string): Promise<number> {
  const r = await fetch(gotrueURL + '/admin/users', {
    method: 'POST',
    headers: { Authorization: `Bearer ${serviceRoleJWT()}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password: memberPW, email_confirm: true }),
  });
  const body = (await r.json()) as { id?: string };
  expect(r.status, JSON.stringify(body)).toBeLessThan(300);
  const out = execFileSync('go', ['run', './e2e/mkmember'], {
    cwd: repoRoot,
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_E2E_SUB: body.id!, MUSTER_E2E_EMAIL: email },
  });
  return Number(out.toString().trim());
}

async function signInAs(browser: Browser, email: string, pw: string): Promise<{ ctx: BrowserContext; page: Page }> {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, ignoreHTTPSErrors: true, storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await page.goto(browserURL + '/login');
  await page.fill('#muster-email', email);
  await page.fill('#muster-password', pw);
  await Promise.all([page.waitForURL(landedHome(browserURL)), page.click('button[type=submit]')]);
  return { ctx, page };
}

// makeStale re-signs the access cookie as the same session signed in 13 hours
// ago (team-ux.spec.ts and mirror-stepup.spec.ts explain why this IS the token
// GoTrue would mint).
async function makeStale(ctx: BrowserContext) {
  const c = (await ctx.cookies(browserURL)).find((k) => k.name === 'muster_at');
  expect(c, 'a session cookie').toBeTruthy();
  const claims = JSON.parse(Buffer.from(c!.value.split('.')[1], 'base64url').toString('utf8'));
  const now = Math.floor(Date.now() / 1000);
  claims.amr = [{ method: 'password', timestamp: now - 13 * 3600 }];
  claims.exp = now + 600;
  const h = Buffer.from(JSON.stringify({ alg: 'HS256', typ: 'JWT' })).toString('base64url');
  const p = Buffer.from(JSON.stringify(claims)).toString('base64url');
  const s = crypto.createHmac('sha256', gotrueSecret).update(h + '.' + p).digest('base64url');
  await ctx.addCookies([{ name: 'muster_at', value: `${h}.${p}.${s}`, url: browserURL, httpOnly: true, secure: true, sameSite: 'Lax' }]);
}

// openTab opens the app's page and waits for the stub's page in the frame.
async function openTab(page: Page) {
  await page.goto(`${browserURL}/apps/${appID}`);
  await expect(page.locator(`[data-app-frame][src^="${APP_ENTRY}?muster_code="]`), 'the frame loaded the plugin').toBeAttached({ timeout: 15_000 });
  await expect(page.frameLocator('[data-app-frame]').locator('#stub-app'), "the stub's page is in the frame").toBeVisible({ timeout: 15_000 });
}

test.beforeAll(async () => {
  test.setTimeout(300_000);
  execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
  execFileSync('go', ['run', './e2e/seed', '--accounts-only'], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn } });
  await stub.start();
  server = await startServer(versionA, serverAppMainPort, {
    MUSTER_AUTH_MODE: 'gotrue',
    MUSTER_DIRECT_CLIENTS: '1',
    MUSTER_GOTRUE_URL: gotrueURL,
    MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
    MUSTER_BROWSER_PORT: String(serverAppBrowserPort),
    MUSTER_PUBLIC_URL: browserURL,
    MUSTER_PLUGINS: '1',
    MUSTER_PLUGIN_ORIGINS: APP_ZONE,
  });
});

test.afterAll(async () => {
  try {
    await server?.stop();
    await stub.stop();
    execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
  } finally {
    // An uninstalled app is revoked, never live; the accounts reset removes
    // this spec's people. Uninstall any app a failed run left live.
    if (appID) {
      try {
        execFileSync(serverBinary(versionA), ['app', 'uninstall', appID], { env: { ...serverEnv(), MUSTER_PLUGIN_ORIGINS: APP_ZONE } });
      } catch {
        /* already uninstalled by the spec */
      }
    }
    execFileSync('go', ['run', './e2e/seed', '--accounts-only'], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn } });
  }
});

test('an admin installs the app on /admin/apps, confirming a stale sign-in in place', async ({ page, browser }) => {
  test.setTimeout(120_000);
  const link = execFileSync(serverBinary(versionA), ['invite', '--claim-admin'], { env: serverEnv() }).toString().trim();
  await page.goto(link);
  await page.fill('#claim-email', adminEmail);
  await page.fill('#claim-password', adminPW);
  await page.fill('#claim-password2', adminPW);
  await Promise.all([page.waitForURL(landedHome(browserURL)), page.click('button[type=submit]')]);

  const { ctx, page: admin } = await signInAs(browser, adminEmail, adminPW);
  try {
    await makeStale(ctx);
    await admin.goto(browserURL + '/admin/apps');
    await expect(admin.locator('#admin-apps-none')).toBeVisible();
    await admin.fill('#name', `srvapp-${stamp}`);
    await admin.fill('#app_label', LABEL);
    await admin.selectOption('#origin', APP_ZONE);
    await admin.fill('#entry_url', APP_ENTRY);
    await admin.check('#tasks');
    await admin.click('#admin-app-install');
    // The stale sign-in asks in place; the held form submits once confirmed.
    await expect(admin.locator('#term-confirm'), 'a stale install asks "Confirm it\'s you" in place').toBeVisible();
    await admin.fill('#term-confirm-password', adminPW);
    await admin.click('#term-confirm-submit');
    const token = await admin.locator('#minted-server-app-token').inputValue({ timeout: 15_000 });
    expect(token, 'the token is shown once').toMatch(/^mpl_[A-Za-z0-9_-]{43}$/);
    const self = await fetch(mainURL + '/plugin/v1/self', { headers: { Authorization: `Bearer ${token}` } });
    const body = (await self.json()) as { plugin_id: number; scope: string; owner_id: unknown };
    expect(body.scope).toBe('server');
    expect(body.owner_id).toBeNull();
    appID = String(body.plugin_id);
    stub.tokens.set(appID, token);
    await expect(admin.locator(`[data-server-app="${appID}"]`)).toBeVisible();
  } finally {
    await ctx.close();
  }
});

test('two people each get the tab and two different subjects; B hides it and B\'s frame goes; the admin uninstalls and A\'s goes', async ({ browser }) => {
  test.setTimeout(240_000);
  expect(appID, 'the app from the first test').toMatch(/^\d+$/);
  const emailA = `srvapp-a-${stamp}@example.com`;
  const emailB = `srvapp-b-${stamp}@example.com`;
  await makeMember(emailA);
  await makeMember(emailB);
  const a = await signInAs(browser, emailA, memberPW);
  const b = await signInAs(browser, emailB, memberPW);
  const admin = await signInAs(browser, adminEmail, adminPW);
  try {
    // The tab, for each person, with no restart of the stub.
    for (const who of [a, b]) {
      await who.page.goto(browserURL + '/tasks');
      await expect(who.page.locator(`[data-app-tab="${appID}"]`).first(), 'the server app\'s tab is drawn').toBeAttached();
    }
    const before = stub.entries.length;
    await openTab(a.page);
    await openTab(b.page);
    const redeems = stub.entries.slice(before).filter((e) => e.plugin === appID);
    expect(redeems.map((e) => e.status), 'two redeems with the one server token').toEqual([200, 200]);
    const subjects = redeems.map((e) => (e.body as { owner?: { subject?: string } }).owner?.subject ?? '');
    for (const s of subjects) expect(s).toMatch(/^[A-Za-z0-9_-]{22}$/);
    expect(subjects[0], 'A and B are two different subjects').not.toBe(subjects[1]);
    // No New tab for a server app.
    await expect(a.page.locator('[data-app-newtab]')).toHaveCount(0);

    // B hides it: B's open frame goes within one alive poll; A's stays.
    // B's REAL Hide button on Account → Apps, in a second tab of B's own
    // browser, so the browser's own headers (Origin, Sec-Fetch-Site) are the
    // ones the server checks. The app page is brought back to the front at
    // once: a background tab's alive poll rests (visibilitychange restarts it).
    const acct = await b.ctx.newPage();
    await acct.goto(browserURL + '/account/apps');
    await expect(acct.locator(`[data-account-app="${appID}"]`)).toContainText(LABEL);
    await Promise.all([acct.waitForURL(/\/account\/apps\?saved=1$/), acct.click(`[data-app-hide="${appID}"]`)]);
    await expect(acct.locator(`[data-account-app="${appID}"]`)).toHaveAttribute('data-app-state', 'hidden');
    await acct.close();
    await b.page.bringToFront();
    await expect(b.page.locator('[data-app-frame]'), "B's frame goes after the hide").toHaveCount(0, { timeout: GONE_MS });
    await expect(b.page.locator('#app-status')).toHaveText('This app was removed.');
    await expect(a.page.locator('[data-app-frame]'), "A's frame stays").toHaveCount(1);

    // The admin uninstalls it: A's frame goes within one alive poll.
    await admin.page.goto(browserURL + '/admin/apps');
    await Promise.all([admin.page.waitForURL(browserURL + '/admin/apps'), admin.page.click(`[data-uninstall="${appID}"]`)]);
    await expect(admin.page.locator('#admin-apps-none')).toBeVisible();
    await expect(a.page.locator('[data-app-frame]'), "A's frame goes after the uninstall").toHaveCount(0, { timeout: GONE_MS });
    await expect(a.page.locator('#app-status')).toHaveText('This app was removed.');
    const after = await fetch(mainURL + '/plugin/v1/self', { headers: { Authorization: `Bearer ${stub.tokens.get(appID)}` } });
    expect(after.status, 'the token is refused after the uninstall').toBe(401);
  } finally {
    await a.ctx.close();
    await b.ctx.close();
    await admin.ctx.close();
  }
});
