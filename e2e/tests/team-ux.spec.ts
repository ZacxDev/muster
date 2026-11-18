import { execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import path from 'node:path';
import { Browser, BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, e2eRoot, gotrueSecret, repoRoot, teamUxBrowserPort, teamUxGotruePort, teamUxMainPort, versionA } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { landedHome } from '../lib/nav';

// A TEAM MANAGER WITH A STALE SIGN-IN, on the click paths a live check of
// v0.232.0 found broken, against a gotrue-mode muster and its own real
// GoTrue: on /teams/<id>/plugins and /teams/<id>/hosts a tap on Mint (and
// Revoke) asks "Confirm it's you" in place — the dialog the session composer
// uses — and once the password is confirmed the SAME form submits, what was
// typed in it kept, and nothing reached the server before. Before, the tap
// went straight to a 403 page saying "Confirm it's you, then try again" with
// no way to: only signing out and in got past it.
//
// What only a browser shows is here: the form script holding the submit
// (StepUpFormScript). Which pages and forms are gated is the Go seam's
// (stepup_ledger_test.go, stepup_forms_seam_pg_test.go). Every value is
// invented fixture data.
//
// 🔴 REGRESSION TEST: red at ff1295f8 (no dialog on either team page).

const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
const project = `muster-e2e-gotrue-${teamUxGotruePort}`;
const composeEnv = {
  ...process.env,
  MUSTER_E2E_GOTRUE_PROJECT: project,
  MUSTER_E2E_GOTRUE_PORT: String(teamUxGotruePort),
  MUSTER_E2E_GOTRUE_SITE: `http://localhost:${teamUxBrowserPort}`,
};
const browserURL = `http://localhost:${teamUxBrowserPort}`;
const gotrueURL = `http://127.0.0.1:${teamUxGotruePort}`;
const stamp = Date.now().toString(36);
const adminEmail = `teamux-admin-${stamp}@example.com`;
const adminPW = 'e2e-teamux-password-not-a-real-one';
const zone = 'https://*.viewer.example.net';

let server: Running | undefined;
let teamID = '';

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

const serverEnv = () => ({ ...process.env, DATABASE_URL: dsn, MUSTER_PUBLIC_URL: browserURL, MUSTER_GOTRUE_URL: gotrueURL, MUSTER_GOTRUE_JWT_SECRET: gotrueSecret });



async function signInAs(browser: Browser, email: string, pw: string): Promise<{ ctx: BrowserContext; page: Page }> {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await page.goto(browserURL + '/login');
  await page.fill('#muster-email', email);
  await page.fill('#muster-password', pw);
  await Promise.all([page.waitForURL(landedHome(browserURL)), page.click('button[type=submit]')]);
  return { ctx, page };
}

// makeStale re-signs the access cookie as the same session signed in 13 hours
// ago (mirror-stepup.spec.ts explains why this IS the token GoTrue would mint).
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

// posts records every POST the page sends to a path under prefix, with its status.
function posts(page: Page, prefix: string): { path: string; status: number }[] {
  const seen: { path: string; status: number }[] = [];
  page.on('response', (r) => {
    const u = new URL(r.url());
    if (r.request().method() === 'POST' && u.pathname.startsWith(prefix)) seen.push({ path: u.pathname, status: r.status() });
  });
  return seen;
}

// confirm answers the open "Confirm it's you" dialog with the password and
// waits for the form it held to land.
async function confirm(page: Page, why: string) {
  const dialog = page.locator('#term-confirm');
  await expect(dialog, 'a stale tap asks in place').toBeVisible();
  await expect(page.locator('#term-confirm-why')).toContainText(why);
  await page.fill('#term-confirm-password', adminPW);
  await page.click('#term-confirm-submit');
}

test.beforeAll(async () => {
  test.setTimeout(300_000);
  execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
  execFileSync('go', ['run', './e2e/seed', '--accounts-only'], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn } });
  server = await startServer(versionA, teamUxMainPort, {
    MUSTER_AUTH_MODE: 'gotrue',
    MUSTER_DIRECT_CLIENTS: '1', // the browser reaches muster directly
    MUSTER_GOTRUE_URL: gotrueURL,
    MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
    MUSTER_BROWSER_PORT: String(teamUxBrowserPort),
    MUSTER_PUBLIC_URL: browserURL,
    MUSTER_PLUGINS: '1',
    MUSTER_PLUGIN_ORIGINS: zone,
    MUSTER_MIRROR: '1',
  });
});

test.afterAll(async () => {
  try {
    await server?.stop();
    execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
  } finally {
    // The claim bound user 1 to this spec's GoTrue user, and the team is a
    // users row: the accounts reset removes both.
    execFileSync('go', ['run', './e2e/seed', '--accounts-only'], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn } });
  }
});

test.describe.configure({ mode: 'serial' });

test('the admin claims and makes a team it manages', async ({ page }) => {
  test.setTimeout(120_000);
  const link = execFileSync(serverBinary(versionA), ['invite', '--claim-admin'], { env: serverEnv() }).toString().trim();
  await page.goto(link);
  await page.fill('#claim-email', adminEmail);
  await page.fill('#claim-password', adminPW);
  await page.fill('#claim-password2', adminPW);
  await Promise.all([page.waitForURL(landedHome(browserURL)), page.click('button[type=submit]')]);
  const teamName = `Team UX ${stamp}`;
  await page.goto(browserURL + '/admin/teams');
  await page.fill('#team-name', teamName);
  await page.selectOption('#team-manager', { label: adminEmail });
  await Promise.all([page.waitForURL(browserURL + '/admin/teams'), page.click('form[action="/admin/teams"] button[type=submit]')]);
  teamID = (await page.locator('#admin-teams [data-team-id]', { has: page.locator('[data-team-name]', { hasText: teamName }) }).getAttribute('data-team-id')) ?? '';
  expect(teamID, 'the team was made').toMatch(/^\d+$/);

});

test('a manager with a stale sign-in mints and revokes on the team plugins page by confirming in place', async ({ browser }) => {
  test.setTimeout(120_000);
  expect(teamID, 'the team from the first test').toMatch(/^\d+$/);
  const { ctx, page } = await signInAs(browser, adminEmail, adminPW);
  try {
    const route = `/teams/${teamID}/plugins`;
    const sent = posts(page, route);
    await makeStale(ctx);
    await page.goto(browserURL + route);
    await expect(page.locator('#team-plugins-none')).toBeVisible();
    await page.fill('#team-plugin-name', `teamux-${stamp}`);
    await page.fill('#team-plugin-origin', zone);
    await page.fill('#team-plugin-entry', 'https://t.viewer.example.net/enter');
    await page.getByRole('button', { name: 'Mint a token' }).click();
    await confirm(page, 'Adding or revoking a team viewer plugin needs a sign-in');
    await expect(page.locator('#team-plugin-token'), 'the held mint submitted once confirmed').toBeVisible();
    await expect(page.locator('#team-plugins li[data-team-plugin]')).toHaveCount(1);
    expect(sent, 'nothing reached the server before the confirm, then ONE mint, accepted').toEqual([{ path: route, status: 200 }]);

    // Revoke, stale again.
    await makeStale(ctx);
    await page.goto(browserURL + route);
    await page.locator('#team-plugins li[data-team-plugin] button', { hasText: 'Revoke' }).click();
    await confirm(page, 'Adding or revoking a team viewer plugin needs a sign-in');
    await expect(page.locator('#team-plugins-none'), 'the held revoke submitted once confirmed').toBeVisible();
    expect(sent.filter((s) => s.path.endsWith('/revoke')).map((s) => s.status), 'ONE revoke, accepted (303)').toEqual([303]);
  } finally {
    await ctx.close();
  }
});

test('a manager with a stale sign-in mints on the team hosts page by confirming in place', async ({ browser }) => {
  test.setTimeout(120_000);
  expect(teamID, 'the team from the first test').toMatch(/^\d+$/);
  const { ctx, page } = await signInAs(browser, adminEmail, adminPW);
  try {
    const route = `/teams/${teamID}/hosts`;
    const sent = posts(page, route);
    await makeStale(ctx);
    await page.goto(browserURL + route);
    await expect(page.locator('#team-host-tokens-none')).toBeVisible();
    await page.fill('#team-host-name', 'teamux-host');
    await page.getByRole('button', { name: 'Mint a token' }).click();
    await confirm(page, 'Minting, revoking or replacing a team host needs a sign-in');
    await expect(page.locator('#team-host-token'), 'the held mint submitted once confirmed').toBeVisible();
    await expect(page.locator('#team-host-tokens li[data-token-id]')).toHaveCount(1);
    expect(sent, 'nothing reached the server before the confirm, then ONE mint, accepted').toEqual([{ path: route, status: 200 }]);
  } finally {
    await ctx.close();
  }
});
