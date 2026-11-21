import { execFileSync } from 'node:child_process';
import path from 'node:path';
import type { Browser, BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { baseURL, dsn, e2eRoot, gotrueSecret, qrBrowserPort, qrGotruePort, qrMainPort, repoRoot, versionA, versionTerm } from '../lib/env';
import { gotrueSkewEnv } from '../lib/gotrue';
import { serverBinary, startServer, Running } from '../lib/server';
import { signIn } from '../lib/signin';

// Sign in with the muster app (docs/auth.md#sign-in-with-the-muster-app), end
// to end on the shared password-mode server with two browser contexts: a
// signed-OUT desktop (its own empty context, desktop-sized) asks to sign in
// and shows a QR code; the "phone" (the suite's signed-in session, which is a
// known device because global-setup signed it in through the form, phone-sized)
// opens the QR's link and approves or denies. The desktop's status line
// follows the request and, approved, lands signed in.
//
// The QR's link is read from the code's data-qr-url (the same URL the SVG
// encodes, and the visible fallback link). The status lines are the server's
// constants (internal/api signin_app_page.go), pinned whole.

const WAITING = "Scan this code with your phone's camera. Your phone must be signed in to muster.";
const SCANNED = 'Scanned. Approve the sign-in on your phone.';
const DENIED = 'The sign-in was denied on your phone.';
const EXPIRED = 'This code has expired.';
const APPROVED_PHONE = 'Approved. The other browser is signing in now.';
const DENIED_PHONE = 'Denied. Nothing was signed in.';

async function desktop(browser: Browser): Promise<{ ctx: BrowserContext; page: Page }> {
  const ctx = await browser.newContext({ baseURL, viewport: { width: 1280, height: 800 }, storageState: { cookies: [], origins: [] } });
  return { ctx, page: await ctx.newPage() };
}

// startCode opens /login on the desktop, chooses the app option and returns
// the link the QR carries once it is drawn.
async function startCode(page: Page, origin: string = baseURL): Promise<string> {
  await page.goto(origin + '/login');
  await page.getByRole('button', { name: 'Sign in with the muster app' }).click();
  const code = page.locator('#qr-signin-code');
  await expect(code.locator('svg')).toBeVisible();
  await expect(page.locator('#qr-signin-status')).toHaveText(WAITING);
  const url = await code.getAttribute('data-qr-url');
  expect(url, 'the QR names the phone page with the code in its fragment').toMatch(/\/auth\/qr#mqr_[A-Za-z0-9_-]{43}$/);
  await expect(page.locator('#qr-signin-link')).toHaveAttribute('href', url!);
  return url!;
}

test('approve on the phone: the desktop follows along and lands signed in', async ({ browser, page }) => {
  await signIn(page, baseURL);
  const desk = await desktop(browser);
  try {
    const link = await startCode(desk.page);

    // The phone opens the link: the page posts the fragment and shows the
    // request's details.
    await page.goto(link);
    await expect(page.getByRole('heading', { name: 'Sign in on another device?' })).toBeVisible();
    expect(page.url(), 'the code left the address bar').not.toContain('#');
    await expect(page.locator('#qr-signin-phone-agent')).toContainText('Chrome');
    await expect(desk.page.locator('#qr-signin-status')).toHaveText(SCANNED);

    await page.getByRole('button', { name: 'Approve' }).click();
    await expect(page.locator('#qr-signin-phone-result')).toHaveText(APPROVED_PHONE);

    // The desktop redeems and is sent on, signed in.
    await desk.page.waitForURL((u) => !u.pathname.startsWith('/login'), { timeout: 15_000 });
    const cookies = await desk.ctx.cookies(baseURL);
    expect(cookies.find((c) => c.name === 'muster_session'), 'the desktop holds a session').toBeTruthy();
    expect(cookies.find((c) => c.name === 'muster_device'), 'an app sign-in does not mark a known device').toBeFalsy();
    const again = await desk.ctx.request.get(baseURL + '/login', { maxRedirects: 0 });
    expect(again.status(), 'GET /login now redirects a signed-in browser').toBe(303);

    // The same link a second time is spent. (From another page first: from
    // /auth/qr itself, a link that differs only by its fragment does not load.)
    await page.goto('about:blank');
    await page.goto(link);
    await expect(page.getByText('This code is invalid, already used or expired. Start again on the other screen.')).toBeVisible();
  } finally {
    await desk.ctx.close();
  }
});

test('deny on the phone: the desktop says so and offers a new code', async ({ browser, page }) => {
  await signIn(page, baseURL);
  const desk = await desktop(browser);
  try {
    const link = await startCode(desk.page);
    await page.goto(link);
    await page.getByRole('button', { name: 'Deny' }).click();
    await expect(page.locator('#qr-signin-phone-result')).toHaveText(DENIED_PHONE);
    await expect(desk.page.locator('#qr-signin-status')).toHaveText(DENIED);
    const fresh = desk.page.getByRole('button', { name: 'Get a new code' });
    await expect(fresh).toBeVisible();
    expect((await desk.ctx.cookies(baseURL)).find((c) => c.name === 'muster_session'), 'a denial signs nothing in').toBeFalsy();

    await fresh.click();
    await expect(desk.page.locator('#qr-signin-status')).toHaveText(WAITING);
    const next = await desk.page.locator('#qr-signin-code').getAttribute('data-qr-url');
    expect(next, 'a new code, not the denied one').not.toBe(link);
    await expect(fresh).toBeHidden();
  } finally {
    await desk.ctx.close();
  }
});

// ⚠ THE EXPIRY HERE IS THE PAGE'S HALF ONLY. A code lives two minutes on the
// server; waiting that out would cost the leg two minutes, so the server's
// expiry is pinned by the Go tests (TestAppSignInExpires, TestExpiry), and
// this test plays the status poll's "expired" answer to check what the page
// does with it: the line, the button, and that the button starts a real new
// request.
test('an expired code: the desktop says so and a new code works', async ({ browser, page }) => {
  await signIn(page, baseURL);
  const desk = await desktop(browser);
  try {
    let expire = true;
    await desk.page.route('**/login/app/status', async (route) => {
      if (!expire) {
        await route.continue();
        return;
      }
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ state: 'expired', interval: 2 }) });
    });
    // The first poll waits the 2 s interval, so the waiting line is read
    // before the played answer lands.
    const first = await startCode(desk.page);
    await expect(desk.page.locator('#qr-signin-status')).toHaveText(EXPIRED);
    await expect(desk.page.locator('#qr-signin-code')).toHaveAttribute('data-state', 'expired');
    expire = false;
    await desk.page.getByRole('button', { name: 'Get a new code' }).click();
    await expect(desk.page.locator('#qr-signin-status')).toHaveText(WAITING);
    const link = await desk.page.locator('#qr-signin-code').getAttribute('data-qr-url');
    expect(link).not.toBe(first);

    // The new code is live: the phone can open it.
    await page.goto(link!);
    await expect(page.getByRole('heading', { name: 'Sign in on another device?' })).toBeVisible();
    await expect(desk.page.locator('#qr-signin-status')).toHaveText(SCANNED);
  } finally {
    await desk.ctx.close();
  }
});

// gotrue mode, against the REAL GoTrue image (e2e/gotrue/compose.yml, the
// deployment's pin): the session the desktop gets is one GoTrue issued for the
// APPROVER — muster mints a magic link for them server-side and exchanges it
// (internal/api signin_app.go issueGoTrueSessionFor). The phone is the admin,
// claimed in its own context; the claim marks it a known device.
test.describe('gotrue mode', () => {
  const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
  const browserURL = `http://localhost:${qrBrowserPort}`;
  const gotrueURL = `http://127.0.0.1:${qrGotruePort}`;
  const composeEnv = {
    ...process.env,
    MUSTER_E2E_GOTRUE_PROJECT: `muster-e2e-gotrue-${qrGotruePort}`,
    MUSTER_E2E_GOTRUE_PORT: String(qrGotruePort),
    MUSTER_E2E_GOTRUE_SITE: browserURL,
  };
  const adminEmail = `qr-admin-${Date.now()}@example.com`;
  const adminPW = 'e2e-admin-password-not-a-real-one';
  let server: Running | undefined;

  test.use({ storageState: { cookies: [], origins: [] } });

  test.beforeAll(async () => {
    test.setTimeout(240_000);
    execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
    const health = await fetch(gotrueURL + '/health');
    expect(((await health.json()) as { version?: string }).version).toBe('v2.197.0');
    // User 1 unclaimed, so this spec's GoTrue can own it (the passkeys and
    // invites specs do the same in this leg).
    execFileSync('go', ['run', './e2e/seed', '--accounts-only'], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn } });
    server = await startServer(versionTerm, qrMainPort, {
      ...gotrueSkewEnv,
      MUSTER_AUTH_MODE: 'gotrue',
      MUSTER_DIRECT_CLIENTS: '1',
      MUSTER_GOTRUE_URL: gotrueURL,
      MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
      MUSTER_BROWSER_PORT: String(qrBrowserPort),
      MUSTER_PUBLIC_URL: browserURL,
    });
  });

  test.afterAll(async () => {
    await server?.stop();
    execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
  });

  const subOf = (at: string) => String(JSON.parse(Buffer.from(at.split('.')[1], 'base64url').toString()).sub);

  test('a phone signed in as the admin approves; the desktop gets a GoTrue session for the admin', async ({ browser }) => {
    test.setTimeout(120_000);
    const phoneCtx = await browser.newContext({ viewport: { width: 390, height: 844 }, storageState: { cookies: [], origins: [] } });
    const desk = await browser.newContext({ viewport: { width: 1280, height: 800 }, storageState: { cookies: [], origins: [] } });
    try {
      const phone = await phoneCtx.newPage();
      const claim = execFileSync(serverBinary(versionA), ['invite', '--claim-admin'], {
        env: { ...process.env, DATABASE_URL: dsn, MUSTER_PUBLIC_URL: browserURL, MUSTER_GOTRUE_URL: gotrueURL, MUSTER_GOTRUE_JWT_SECRET: gotrueSecret },
      }).toString().trim();
      await phone.goto(claim);
      await phone.fill('#claim-email', adminEmail);
      await phone.fill('#claim-password', adminPW);
      await phone.fill('#claim-password2', adminPW);
      await Promise.all([phone.waitForURL((u) => !u.pathname.startsWith('/auth/claim')), phone.click('button[type=submit]')]);
      const phoneCookies = await phoneCtx.cookies(browserURL);
      expect(phoneCookies.find((c) => c.name === 'muster_device'), 'the claim marked the phone a known device').toBeTruthy();
      const phoneSub = subOf(phoneCookies.find((c) => c.name === 'muster_at')!.value);

      const deskPage = await desk.newPage();
      const link = await startCode(deskPage, browserURL);
      expect(link.startsWith(browserURL + '/auth/qr#')).toBe(true);
      await phone.goto(link);
      await expect(phone.getByRole('heading', { name: 'Sign in on another device?' })).toBeVisible();
      await expect(deskPage.locator('#qr-signin-status')).toHaveText(SCANNED);
      await phone.getByRole('button', { name: 'Approve' }).click();
      await expect(phone.locator('#qr-signin-phone-result')).toHaveText(APPROVED_PHONE);

      await deskPage.waitForURL((u) => !u.pathname.startsWith('/login'), { timeout: 20_000 });
      const deskCookies = await desk.cookies(browserURL);
      const at = deskCookies.find((c) => c.name === 'muster_at');
      expect(at && deskCookies.find((c) => c.name === 'muster_rt'), 'GoTrue issued the desktop a session pair').toBeTruthy();
      expect(subOf(at!.value), 'for the approver').toBe(phoneSub);
      expect(deskCookies.find((c) => c.name === 'muster_device'), 'an app sign-in does not mark a known device').toBeFalsy();
      await deskPage.goto(browserURL + '/account');
      await expect(deskPage.locator('#account-who')).toContainText(adminEmail);
    } finally {
      await desk.close();
      await phoneCtx.close();
    }
  });
});
