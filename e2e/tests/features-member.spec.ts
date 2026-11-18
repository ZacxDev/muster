import { execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import path from 'node:path';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, e2eRoot, featuresMemberBrowserPort, featuresMemberGotruePort, featuresMemberMainPort, gotrueSecret, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { landedHome } from '../lib/nav';
import { pageWords, setupJargon } from '../lib/plainwords';

// A MEMBER'S FEATURES PAGE, ON A PHONE, against the REAL GoTrue
// (e2e/gotrue/compose.yml) and a gotrue-mode muster with no optional feature
// set up (operator rule: muster's own chrome shows no variable, Compose
// profile, flag or command in its top layer — and a feature's SETUP is an
// admin's). The member reads each feature as a plain name, one plain sentence
// and On / Off; every feature that is not on says "Ask your admin to turn this
// on."; the page's HTML carries no MUSTER_ variable, no COMPOSE_PROFILES, no
// "docker compose" and no ⓘ code row at all (absent, not hidden); and the
// anchor every "how to turn it on" hint links to opens the feature's sheet,
// which says to ask their admin. (The admin's page, the control, is
// features.spec.ts.) With MUSTER_E2E_SHOTS_DIR set it saves the screenshots
// there (never in CI).
//
// 🔴 REGRESSION TEST: red at origin/main, which drew every row's .env lines for
// anyone signed in.

const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
const project = `muster-e2e-features-member-${featuresMemberGotruePort}`;
const browserURL = `http://localhost:${featuresMemberBrowserPort}`;
const gotrueURL = `http://127.0.0.1:${featuresMemberGotruePort}`;
const composeEnv = {
  ...process.env,
  MUSTER_E2E_GOTRUE_PROJECT: project,
  MUSTER_E2E_GOTRUE_PORT: String(featuresMemberGotruePort),
  MUSTER_E2E_GOTRUE_SITE: browserURL,
};

let server: Running | undefined;

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(240_000);
  execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
  server = await startServer(versionA, featuresMemberMainPort, {
    MUSTER_AUTH_MODE: 'gotrue',
    MUSTER_DIRECT_CLIENTS: '1', // the browser reaches muster directly
    MUSTER_GOTRUE_URL: gotrueURL,
    MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
    MUSTER_BROWSER_PORT: String(featuresMemberBrowserPort),
    MUSTER_PUBLIC_URL: browserURL,
  });
});

test.afterAll(async () => {
  await server?.stop();
  execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
});

// A 60 s service_role token for GoTrue's admin API, signed with the shared
// secret (exactly what muster's own GoTrue client mints).
function serviceRoleJWT(): string {
  const now = Math.floor(Date.now() / 1000);
  const b64url = (v: string) => Buffer.from(v).toString('base64url');
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

const row = (page: Page, id: string) => page.locator(`section[data-feature="${id}"]`);
const shots = process.env.MUSTER_E2E_SHOTS_DIR ?? '';
const shot = async (page: Page, name: string, fullPage = false) => {
  if (shots) await page.screenshot({ path: path.join(shots, name + '.png'), fullPage });
};

test('a member reads plain rows, is told to ask their admin, and is sent no setup (real GoTrue, 390px)', async ({ page }) => {
  test.setTimeout(120_000);
  const email = `features-member-${Date.now()}@example.com`;
  const password = 'e2e-member-password-not-a-real-one';
  expect(await makeMember(email, password)).toBeGreaterThan(1);
  await page.goto(browserURL + '/login');
  await page.fill('#muster-email', email);
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL(landedHome(browserURL)), page.click('button[type=submit]')]);

  await page.goto(browserURL + '/account/features');
  await expect(row(page, 'dictation')).toBeVisible();
  await shot(page, 'features-member-list', true);
  const words = await pageWords(page);
  expect(words.length, 'CONTROL: the page has words to read').toBeGreaterThan(1000);
  expect(words, 'the top layer spells no variable, command or profile').not.toMatch(setupJargon);
  const rows = page.locator('section[data-feature]');
  const n = await rows.count();
  expect(n, 'the page lists the registry').toBeGreaterThan(30);
  await expect(page.locator('section[data-feature] [data-feature-oneline]')).toHaveCount(n);

  // Every feature that is not on says to ask the admin — except one that is
  // off because another does its job (the shared password, on a server where
  // everyone has an account), which says so instead. No row tells a member
  // to open an ⓘ for a setting they will not be shown.
  const askable = await page.locator('section[data-feature]:not([data-state="on"]):not(:has([data-feature-replaced]))').count();
  expect(askable, 'CONTROL: this server has features that are off').toBeGreaterThan(5);
  await expect(page.locator('section[data-feature] [data-feature-ask]')).toHaveCount(askable);
  await expect(page.locator('[data-feature-next]')).toHaveCount(0);
  const sharedPassword = row(page, 'password-sign-in');
  await expect(sharedPassword.locator('[data-feature-replaced]')).toHaveText('Not used: everyone signs in with their own account.');
  await expect(sharedPassword.locator('[data-feature-ask]')).toHaveCount(0);
  const dictation = row(page, 'dictation');
  await expect(dictation.locator('[data-feature-name]')).toHaveText('Voice typing');
  await expect(dictation.locator('[data-feature-state]')).toHaveText('Off');
  await expect(dictation.locator('[data-feature-ask]')).toHaveText('Ask your admin to turn this on.');

  // The setup is absent from what the server sent, not hidden in it.
  const html = (await page.content()).replace(/<script\b[^>]*>[\s\S]*?<\/script>/g, '');
  for (const needle of ['MUSTER_', 'COMPOSE_PROFILES', 'docker compose', 'data-info-code']) {
    expect(html, `a member's Features page carries ${needle}`).not.toContain(needle);
  }

  // A "how to turn it on" hint on another page opens the feature's sheet: the
  // Account page's suggested-replies note (off: this server has no model
  // key), tapped as a member would. For a member the sheet says to ask
  // their admin.
  await page.goto(browserURL + '/account');
  const hint = page.locator('a[href="/account/features#suggested-replies-setup"]');
  await expect(hint).toBeVisible();
  await hint.tap();
  await page.waitForURL((u) => u.pathname === '/account/features' && u.hash === '#suggested-replies-setup');
  const sheet = page.locator('#suggested-replies-setup');
  await expect(sheet).toBeVisible();
  await expect(sheet).toHaveAttribute('role', 'dialog');
  await expect(sheet.locator('[data-info-withheld]')).toHaveText('Ask your admin to turn this on.');
  await expect(sheet.locator('[data-info-code], [data-info-copy]')).toHaveCount(0);
  const sb = (await sheet.boundingBox())!;
  expect(sb.x >= -0.5 && sb.x + sb.width <= 390.5, 'the sheet fits the phone').toBe(true);
  await shot(page, 'features-member-sheet');
  await page.keyboard.press('Escape');
  await expect(sheet).toBeHidden();
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  expect(overflow, 'the Features page scrolls sideways').toBeLessThanOrEqual(0);
});
