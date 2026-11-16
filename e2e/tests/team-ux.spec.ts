import { execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import path from 'node:path';
import { Browser, BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, e2eRoot, gotrueSecret, repoRoot, teamUxBrowserPort, teamUxGotruePort, teamUxMainPort, versionA } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { landedHome } from '../lib/nav';

// A TEAM'S PAGES ON THE CLICK PATHS A LIVE CHECK OF v0.232.0 FOUND BROKEN,
// against a gotrue-mode muster and its own real GoTrue:
//
//   1. A MANAGER WITH A STALE SIGN-IN, on /teams/<id>/plugins and
//      /teams/<id>/hosts, taps Mint (and Revoke): the page asks "Confirm it's
//      you" in place — the dialog the session composer uses — and once the
//      password is confirmed the SAME form submits, what was typed in it kept.
//      Before, the tap went straight to a 403 page saying "Confirm it's you,
//      then try again" with no way to: only signing out and in got past it.
//   2. A team VIEWER's copy of the team's end card on the Asks page draws no
//      End it, no Not now and no Dismiss — "their question cards are drawn
//      unanswerable" (docs/mirroring.md) — while the manager's copy of the same
//      card draws them (the control).
//
// ⚠ WHAT IS PLAYED: the team's end card is a fixture row (e2e/teamfix): the
// real one is filed by the team chief's end_session_propose, which needs an
// agent runtime this harness does not run. Everything else is the real routes.
// Every value is invented fixture data.
//
// 🔴 REGRESSION TEST: red at ff1295f8 (no dialog on either team page; the
// viewer's card drew both buttons).

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
const viewerEmail = `teamux-viewer-${stamp}@example.com`;
const viewerPW = 'e2e-teamux-viewer-password-not-a-real-one';
const zone = 'https://*.viewer.example.net';

let server: Running | undefined;
let teamID = '';

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

const serverEnv = () => ({ ...process.env, DATABASE_URL: dsn, MUSTER_PUBLIC_URL: browserURL, MUSTER_GOTRUE_URL: gotrueURL, MUSTER_GOTRUE_JWT_SECRET: gotrueSecret });

function teamfix(...args: string[]): string {
  return execFileSync('go', ['run', './e2e/teamfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();
}

// A 60 s service_role token for GoTrue's admin API (branding.spec.ts).
function serviceRoleJWT(): string {
  const now = Math.floor(Date.now() / 1000);
  const b64 = (v: string) => Buffer.from(v).toString('base64url');
  const head = b64(JSON.stringify({ alg: 'HS256', typ: 'JWT' }));
  const body = b64(JSON.stringify({ role: 'service_role', aud: 'authenticated', iss: 'muster-auth', iat: now, exp: now + 60 }));
  const sig = crypto.createHmac('sha256', gotrueSecret).update(`${head}.${body}`).digest('base64url');
  return `${head}.${body}.${sig}`;
}

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
    if (teamID) teamfix('cleanup', teamID);
  } finally {
    await server?.stop();
    execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
    // The claim bound user 1 to this spec's GoTrue user, and the team and its
    // viewer are users rows: the accounts reset removes all three.
    execFileSync('go', ['run', './e2e/seed', '--accounts-only'], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn } });
  }
});

test.describe.configure({ mode: 'serial' });

test('the admin claims, makes a team it manages, and adds a viewer', async ({ page }) => {
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

  // The viewer: a GoTrue user bound to a member account, added as a VIEWER
  // through the admin page's own form.
  const r = await fetch(gotrueURL + '/admin/users', {
    method: 'POST',
    headers: { Authorization: `Bearer ${serviceRoleJWT()}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ email: viewerEmail, password: viewerPW, email_confirm: true }),
  });
  const created = (await r.json()) as { id?: string };
  expect(r.status, JSON.stringify(created)).toBeLessThan(300);
  const viewerID = execFileSync('go', ['run', './e2e/mkmember'], {
    cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn, MUSTER_E2E_SUB: created.id!, MUSTER_E2E_EMAIL: viewerEmail },
  }).toString().trim();
  await page.goto(browserURL + '/admin/teams');
  await page.selectOption(`#add-${teamID}`, viewerID);
  await page.selectOption(`#add-role-${teamID}`, 'viewer');
  await Promise.all([page.waitForURL(browserURL + '/admin/teams'),
    page.locator(`#admin-teams [data-team-id="${teamID}"]`).getByRole('button', { name: 'Add member' }).click()]);
  await expect(page.locator(`#admin-teams [data-team-id="${teamID}"] [data-member-id="${viewerID}"][data-member-role="viewer"]`)).toHaveCount(1);
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

test("a team viewer's copy of the team's end card draws no End it, Not now or Dismiss; the manager's draws them", async ({ browser }) => {
  test.setTimeout(120_000);
  expect(teamID, 'the team from the first test').toMatch(/^\d+$/);
  const ask = teamfix('endcard', teamID);
  expect(ask).toMatch(/^\d+$/);

  const viewer = await signInAs(browser, viewerEmail, viewerPW);
  try {
    await viewer.page.goto(browserURL + '/attention');
    const card = viewer.page.locator(`#ask-${ask}`);
    await expect(card, 'the viewer sees the team card').toBeVisible();
    await expect(card).toContainText('End “e2e-team:1”?');
    await expect(card.getByRole('button', { name: 'End it' }), 'no End it for a viewer').toHaveCount(0);
    await expect(card.getByRole('button', { name: 'Not now' }), 'no Not now for a viewer').toHaveCount(0);
    await expect(card.locator('[data-attention-dismiss]'), 'no Dismiss for a viewer').toHaveCount(0);
    await expect(card.locator('textarea'), 'no answer box for a viewer').toHaveCount(0);
    await expect(card.locator('[data-attention-watch-only]')).toContainText('you are a viewer of the team');
  } finally {
    await viewer.ctx.close();
  }

  // The control: the same card, the manager's copy.
  const mgr = await signInAs(browser, adminEmail, adminPW);
  try {
    await mgr.page.goto(browserURL + '/attention');
    const card = mgr.page.locator(`#ask-${ask}`);
    await expect(card).toBeVisible();
    await expect(card.getByRole('button', { name: 'End it' })).toHaveCount(1);
    await expect(card.getByRole('button', { name: 'Not now' })).toHaveCount(1);
    await expect(card.locator('[data-attention-watch-only]')).toHaveCount(0);
  } finally {
    await mgr.ctx.close();
  }
});
