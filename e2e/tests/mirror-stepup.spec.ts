import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page, Route } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import {
  binDir, dsn, e2eRoot, gotrueSecret, repoRoot, stepupBrowserPort, stepupGotruePort, stepupMainPort, versionA,
} from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { Authenticator, authenticator, credentialIds, removeCredential, setUserVerified } from '../lib/webauthn';
import { landedHome } from '../lib/nav';

// STEP-UP for typing into a mirrored session, end to end against the REAL
// GoTrue image (e2e/gotrue/compose.yml, the deployment's pin), a gotrue-mode
// muster with the mirror plugin on, and a REAL muster-mirror (`-tags
// e2emirror`: only tmux is played) started -interactive.
//
// The Interactive switch attaches rw at once on a FRESH sign-in (within the
// step-up window — 12 hours, the server's default, which this spec runs with —
// read from the access token's `amr`), and on a STALE one asks "Confirm it's
// you" first: a PASSKEY (a WebAuthn assertion with user verification, verified
// by GoTrue and bound by muster to the signed-in user) or the password again.
// The server refuses a stale rw attach or send whatever the page believes.
//
// This GoTrue runs with passkeys ON (MUSTER_E2E_GOTRUE_PASSKEYS), so the
// dialog's passkey path is the real one; the first three tests never register a
// passkey, and so also pin that an account WITHOUT one is offered the password
// alone.
//
// 🔴 HOW A STALE SESSION IS MADE WITHOUT WAITING 12 HOURS: the access cookie
// is re-signed with the e2e JWT secret (which muster and this GoTrue share)
// with its `amr` timestamp moved 13 hours back and everything else — sub,
// session_id, iss, aud — the real token's. That is byte-for-byte the token
// GoTrue mints for a session signed in 13 hours ago (measured: a refresh
// carries amr forward unchanged), so muster's verifier takes it as such. Its
// exp is pushed out so muster does not refresh it back into a fresh one
// mid-test (GOTRUE_JWT_EXP is 5 s here).

const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
const project = `muster-e2e-gotrue-${stepupGotruePort}`;
const composeEnv = {
  ...process.env,
  MUSTER_E2E_GOTRUE_PROJECT: project,
  MUSTER_E2E_GOTRUE_PORT: String(stepupGotruePort),
  MUSTER_E2E_GOTRUE_SITE: `http://localhost:${stepupBrowserPort}`,
  MUSTER_E2E_GOTRUE_PASSKEYS: 'true',
};
const browserURL = `http://localhost:${stepupBrowserPort}`;
const machineURL = `http://localhost:${stepupMainPort}`;
const gotrueURL = `http://127.0.0.1:${stepupGotruePort}`;
const host = 'stepup-' + Date.now().toString(36);
const mirrorBin = path.join(binDir, 'muster-mirror-e2e-stepup');
const WIN = '120x30';
const adminEmail = `stepup-admin-${Date.now()}@example.com`;
const adminPW = 'e2e-stepup-password-not-a-real-one';

let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';
// `main` is played as a Claude Code pane (cmd/muster-mirror/e2emirror.go) whose
// transcript is this file, so the session also has a RENDERED view; a message
// sent there is appended to <transcript>.pasted by the fake host.
const transcriptDir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-e2e-stepup-cc-'));
const transcript = path.join(transcriptDir, 'e2e-stepup-0001.jsonl');
fs.writeFileSync(
  transcript,
  JSON.stringify({ type: 'user', promptId: 'p', message: { role: 'user', content: 'hello there' } }) + '\n' +
    JSON.stringify({ type: 'assistant', message: { content: [{ type: 'text', text: 'Hi.' }] } }) + '\n',
);

test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(300_000);
  execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
  const health = await fetch(gotrueURL + '/health');
  expect(((await health.json()) as { version?: string }).version).toBe('v2.197.0');
  execFileSync('go', ['run', './e2e/seed', '--accounts-only'], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn } });
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionA, stepupMainPort, {
    MUSTER_AUTH_MODE: 'gotrue',
    MUSTER_GOTRUE_URL: gotrueURL,
    MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
    MUSTER_BROWSER_PORT: String(stepupBrowserPort),
    MUSTER_PUBLIC_URL: browserURL,
    MUSTER_MIRROR: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
});

test.afterAll(async () => {
  if (mirrorProc && mirrorProc.exitCode === null) {
    const done = new Promise((r) => mirrorProc!.once('exit', r));
    mirrorProc.kill('SIGTERM');
    await done;
  }
  await server?.stop();
  execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
  fs.rmSync(transcriptDir, { recursive: true, force: true });
});

const serverEnv = () => ({ ...process.env, DATABASE_URL: dsn, MUSTER_PUBLIC_URL: browserURL, MUSTER_GOTRUE_URL: gotrueURL, MUSTER_GOTRUE_JWT_SECRET: gotrueSecret });

// claimAndSignIn claims the admin (user 1, the owner of every mirror token the
// server binary mints) and leaves the page signed in.
// adminClaimed: this run has claimed the admin (the file's first test does).
// A test run on its own claims it itself (adminSignIn).
let adminClaimed = false;
async function claimAndSignIn(page: Page) {
  adminClaimed = true;
  const link = execFileSync(serverBinary(versionA), ['invite', '--claim-admin'], { env: serverEnv() }).toString().trim();
  await page.goto(link);
  await page.fill('#claim-email', adminEmail);
  await page.fill('#claim-password', adminPW);
  await page.fill('#claim-password2', adminPW);
  await Promise.all([page.waitForURL(landedHome(browserURL)), page.click('button[type=submit]')]);
}

function startMirror(extra: NodeJS.ProcessEnv = {}): ChildProcess {
  const token = execFileSync(serverBinary(versionA), ['mirror', 'token', 'create', host], { env: serverEnv() }).toString().trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  const p = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: machineURL,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_E2E_SESSIONS: 'main',
      MUSTER_MIRROR_E2E_WINDOW: WIN,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_CLAUDE: 'main=' + transcript,
      ...extra,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  p.stdout?.on('data', (d) => (mirrorLog += d));
  p.stderr?.on('data', (d) => (mirrorLog += d));
  return p;
}

const termPath = `/mirror/terminal?host=${encodeURIComponent(host)}&session=main`;
const screen = (page: Page) => page.locator('#term .xterm-rows');
const dialog = (page: Page) => page.locator('#term-confirm');

function b64urlJSON(part: string): Record<string, unknown> {
  return JSON.parse(Buffer.from(part, 'base64url').toString('utf8'));
}

function sign(payload: Record<string, unknown>): string {
  const h = Buffer.from(JSON.stringify({ alg: 'HS256', typ: 'JWT' })).toString('base64url');
  const p = Buffer.from(JSON.stringify(payload)).toString('base64url');
  const s = crypto.createHmac('sha256', gotrueSecret).update(h + '.' + p).digest('base64url');
  return `${h}.${p}.${s}`;
}

async function accessToken(ctx: BrowserContext): Promise<string> {
  const c = (await ctx.cookies(browserURL)).find((k) => k.name === 'muster_at');
  expect(c, 'a session cookie').toBeTruthy();
  return c!.value;
}

function amrOf(token: string): number {
  const amr = b64urlJSON(token.split('.')[1]).amr as { timestamp: number }[];
  return Math.max(...amr.map((e) => e.timestamp));
}

// How long ago makeStale's session "signed in": an hour past the 12 hour
// window. (10 minutes — stale under the old five-minute window — is FRESH now,
// which the window test below pins.)
const STALE_SECONDS = 13 * 3600;

// makeStale replaces the access cookie with the same session signed in `ago`
// seconds ago — by default 13 hours, an hour past the window (see the header).
async function makeStale(ctx: BrowserContext, ago = STALE_SECONDS) {
  const real = await accessToken(ctx);
  const claims = b64urlJSON(real.split('.')[1]);
  const now = Math.floor(Date.now() / 1000);
  claims.amr = [{ method: 'password', timestamp: now - ago }];
  claims.exp = now + 600;
  await ctx.addCookies([{ name: 'muster_at', value: sign(claims), url: browserURL, httpOnly: true, secure: true, sameSite: 'Lax' }]);
}

test('live keys on a mirrored session need a fresh sign-in, and a key pressed on a stale one asks for the password (real GoTrue, 412px)', async ({ page, context }) => {
  test.setTimeout(180_000);
  // The page's own sockets, to close one as a dropped network would (part 3).
  await page.addInitScript(() => {
    const C = window.WebSocket;
    const socks: WebSocket[] = ((window as any).__socks = []);
    const N = function (this: unknown, url: string | URL, p?: string | string[]) {
      const o = p === undefined ? new C(url) : new C(url, p);
      socks.push(o);
      return o;
    } as unknown as typeof WebSocket;
    N.prototype = C.prototype;
    (window as any).WebSocket = N;
  });
  await claimAndSignIn(page);
  mirrorProc = startMirror();
  // Every terminal socket the page OPENS, by mode — so "the page asked before
  // trying" is distinguishable from "it tried, the server refused, then it
  // asked".
  const rwOpened: string[] = [];
  page.on('websocket', (ws) => {
    if (ws.url().includes('/mirror/terminal/ws') && ws.url().includes('mode=rw')) rwOpened.push(ws.url());
  });

  // The card announces the host is interactive.
  await expect.poll(async () => (await (await page.request.get(browserURL + '/ui/agents')).text()).includes(`Mirrored · ${host}`), {
    message: `the host's card (muster-mirror log:\n${mirrorLog})`, timeout: 20_000,
  }).toBe(true);
  await page.goto(browserURL + '/agents');
  const card = page.locator('#agents-list article[data-mirror-card]').filter({ has: page.locator(`[data-mirror-badge][aria-label="Mirrored · ${host}"]`) });
  // What the host announced is a filter field now, not a badge on the card.
  await expect(card).toHaveAttribute('data-f-mode', 'interactive');
  await expect(card.locator('[data-mirror-interactive]')).toHaveCount(0);

  // 1. FRESH (just signed in): the page attaches rw at once — live keys are
  // always on, there is no switch — at the session's own size (a phone does
  // not shrink it).
  await page.goto(browserURL + termPath);
  await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=rw size=' + WIN);
  await expect(page.locator('#term')).toHaveAttribute('data-remote', WIN);
  expect(Number(await page.locator('#term').getAttribute('data-fresh-for'))).toBeGreaterThan(200_000);
  await expect(page.locator('#term-mode'), 'no Live keys switch').toHaveCount(0);
  await expect(page.locator('#term-status')).toHaveText('Connected');
  await expect(dialog(page)).toBeHidden();

  // Within the window: a reload comes back rw, without asking.
  await page.reload();
  await expect(screen(page)).toContainText('mode=rw');
  await expect(page.locator('#term-status')).toHaveText('Connected');
  await expect(dialog(page)).toBeHidden();

  // 2. STALE: the page loads read-only and says what a key will do; the key
  // pressed asks first, before any rw attach.
  await makeStale(context);
  const rwBeforeStale = rwOpened.length;
  await page.reload();
  await expect(screen(page)).toContainText('mode=ro');
  await expect(page.locator('#term-status')).toHaveText("Connected · confirm it's you to type");
  expect(await page.locator('#term').getAttribute('data-fresh-for')).toBe('0');
  expect(rwOpened.length, 'a stale page opens no rw socket at load').toBe(rwBeforeStale);
  await expect(page.locator('#term-keys [data-key="up"]'), 'the keys are on all the same').toBeEnabled();
  await page.locator('#term-keys [data-key="up"]').click();
  await expect(dialog(page)).toBeVisible();
  expect(rwOpened.length, 'a stale page asks BEFORE trying rw (no rw socket opened)').toBe(rwBeforeStale);
  await expect(page.getByRole('heading', { name: "Confirm it's you" })).toBeVisible();
  const box = (await dialog(page).boundingBox())!;
  expect(box.x + box.width, 'the dialog fits 412px').toBeLessThanOrEqual(412);

  //    A wrong password is refused, and nothing attached.
  await page.fill('#term-confirm-password', 'not the password');
  await page.click('#term-confirm-submit');
  await expect(page.locator('#term-confirm-error')).toHaveText('That password is not right.');
  expect(rwOpened.length, 'a refused password attached nothing').toBe(rwBeforeStale);
  const staleToken = await accessToken(context);
  expect(Date.now() / 1000 - amrOf(staleToken), 'still the stale session').toBeGreaterThan(STALE_SECONDS - 100);

  //    The right one: a NEW sign-in (amr is now), then rw.
  await page.fill('#term-confirm-password', adminPW);
  await page.click('#term-confirm-submit');
  await expect(dialog(page)).toBeHidden();
  await expect(screen(page)).toContainText('mode=rw size=' + WIN);
  await expect(page.locator('#term-status')).toHaveText('Connected');
  // The key that asked was typed at a terminal that could not take it: it is
  // not replayed into the session.
  await expect(screen(page)).not.toContainText('in=');
  const freshToken = await accessToken(context);
  expect(freshToken).not.toBe(staleToken);
  expect(Math.abs(Date.now() / 1000 - amrOf(freshToken)), 'the confirm signed in just now').toBeLessThan(60);

  // 3. THE SERVER DECIDES, NOT THE PAGE: the page was rendered fresh and is
  // attached rw; the cookie goes stale under it, and the socket drops (as a
  // network would). The page reconnects by itself, believing it fresh: its rw
  // attach is refused (403), it attaches read-only, tries rw once more (the
  // first refusal could have been the network) and, refused again, stays
  // read-only and says a key will ask — no dialog it was not asked for.
  await page.reload();
  await expect(screen(page)).toContainText('mode=rw');
  expect(Number(await page.locator('#term').getAttribute('data-fresh-for'))).toBeGreaterThan(0);
  await makeStale(context);
  const rwBeforeRefusal = rwOpened.length;
  await page.evaluate(() => {
    const live = (window as any).__socks.filter((s: WebSocket) => s.url.includes('/mirror/terminal/ws') && s.readyState === 1);
    live.forEach((s: WebSocket) => s.close());
  });
  await expect(page.locator('#term-status'), 'the refused page settles read-only and says why').toHaveText("Connected · confirm it's you to type", { timeout: 15_000 });
  await expect(screen(page)).toContainText('mode=ro');
  expect(rwOpened.length - rwBeforeRefusal, 'the page (believing it fresh) DID try rw — twice, the second after a read-only attach proved the network').toBe(2);
  await expect(dialog(page), 'no dialog it was not asked for').toBeHidden();
  await page.locator('#term-keys [data-key="up"]').click();
  await expect(dialog(page), 'the next key asks').toBeVisible();
  const status = await page.evaluate(async (u) => {
    const r = await fetch(u + '&mode=rw');
    return r.status;
  }, browserURL + `/mirror/terminal/ws?host=${encodeURIComponent(host)}&session=main`);
  expect(status, 'a stale rw attach is 403 at the server').toBe(403);
});

test.describe('at a desktop width (1280px)', () => {
  test.use({ viewport: { width: 1280, height: 900 }, hasTouch: false, isMobile: false });

  test('a stale desktop session is asked to confirm at its first key before it attaches rw (real GoTrue)', async ({ page, context }) => {
    test.setTimeout(120_000);
    // Signed in through /login this time (the 412px test claimed the account).
    await page.goto(browserURL + '/login');
    await page.fill('#muster-email', adminEmail);
    await page.fill('#muster-password', adminPW);
    await Promise.all([page.waitForURL(landedHome(browserURL)), page.click('button[type=submit]')]);
    if (!mirrorProc || mirrorProc.exitCode !== null) mirrorProc = startMirror();

    await makeStale(context);
    await page.goto(browserURL + termPath);
    await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro');
    // 1280px covers the 120x30 session: the desktop fits as before.
    await expect(page.locator('#term')).toHaveAttribute('data-overflow', '0');
    await expect(page.locator('#term-status')).toHaveText("Connected · confirm it's you to type");
    await page.locator('#term').click();
    await page.keyboard.type('x');
    await expect(dialog(page)).toBeVisible();
    await page.fill('#term-confirm-password', adminPW);
    await page.click('#term-confirm-submit');
    await expect(dialog(page)).toBeHidden();
    await expect(screen(page)).toContainText('mode=rw');
    await expect(page.locator('#term-status')).toHaveText('Connected');
    await expect(screen(page), 'the key that asked is not replayed').not.toContainText('in=78');
    // Typing reaches the host.
    await page.locator('#term').click();
    await page.keyboard.type('ok');
    await expect(screen(page)).toContainText(/in=6F6B|in=6F/);
  });
});

// ---------------------------------------------------------------------------
// SENDING FROM THE RENDERED VIEW, on a phone (390×844, touch), by the real tap
// path: the Agents list → the Claude session's card → the rendered view → type
// → Send. With real GoTrue, so the stale refusal is the SERVER's own (in
// mirrored.spec.ts it is played, because a password-mode session cannot be
// aged in a browser).
//
// ⚠ AN INVARIANT GUARD, NOT A REGRESSION TEST: it was written while
// investigating a report that messages could not be sent from the rendered
// view, and it PASSES on the code that report was made against (4208deb). It
// pins that the composer is there by default — no Interactive switch, which
// lives only on the Terminal page — and that a stale sign-in is asked to
// confirm inline and the SAME text is then sent.
// ---------------------------------------------------------------------------
test.describe('the rendered view on a phone (390×844)', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('a message is typed and sent from the rendered view: at once when the sign-in is fresh, after an inline confirm when it is stale (real GoTrue)', async ({ page, context }) => {
    test.setTimeout(120_000);
    // Signed in through /login: like the desktop test above, this relies on the
    // 412px test having claimed the account (the file's tests run in order).
    await page.goto(browserURL + '/login');
    await page.fill('#muster-email', adminEmail);
    await page.fill('#muster-password', adminPW);
    await Promise.all([page.waitForURL(landedHome(browserURL)), page.click('button[type=submit]')]);
    if (!mirrorProc || mirrorProc.exitCode !== null) mirrorProc = startMirror();
    const pasted = () => (fs.existsSync(transcript + '.pasted') ? fs.readFileSync(transcript + '.pasted', 'utf8') : '');
    const sends: number[] = [];
    page.on('response', (r) => {
      if (new URL(r.url()).pathname === '/mirror/view/send') sends.push(r.status());
    });

    // The card of a Claude session opens the rendered view.
    await page.goto(browserURL + '/agents');
    const card = page.locator('#agents-list article[data-mirror-card]').filter({ has: page.locator(`[data-mirror-badge][aria-label="Mirrored · ${host}"]`) });
    await expect(card.locator('[data-mirror-claude]'), `main is a Claude pane (muster-mirror log:\n${mirrorLog})`).toBeVisible({ timeout: 20_000 });
    await Promise.all([page.waitForURL((u) => u.pathname === '/mirror/view'), card.locator('a').first().tap()]);
    await expect(page.locator('#mv-log [data-bubble="assistant"]').first()).toContainText('Hi.');

    // The composer is THERE, enabled and on screen, with nothing to switch on.
    const input = page.locator('#mv-input');
    const send = page.locator('#mv-send');
    await expect(input).toBeEnabled();
    await expect(send).toBeEnabled();
    await expect(page.locator('#term-mode')).toHaveCount(0);
    for (const el of [input, send]) {
      const b = (await el.boundingBox())!;
      expect(b.y + b.height, 'the composer is above the bottom of a 390×844 screen').toBeLessThanOrEqual(844);
      expect(b.x + b.width, 'the composer fits 390px').toBeLessThanOrEqual(390);
      expect(b.height, 'a 44px touch target').toBeGreaterThanOrEqual(44);
    }

    // 1. FRESH: typed, tapped, pasted into the Claude pane; the composer clears
    // and the prompt comes back through the transcript.
    const before = pasted();
    await input.tap();
    await page.keyboard.type('first from the phone');
    await send.tap();
    await expect.poll(pasted).toBe(before + '%9 first from the phone\n');
    await expect(input).toHaveValue('');
    await expect(page.locator('#mv-log [data-bubble="user"]').last()).toContainText('first from the phone');
    await expect(dialog(page)).toBeHidden();
    expect(sends, 'one send, accepted').toEqual([204]);

    // 2. STALE: the server refuses (403 + X-Muster-Step-Up), the page asks to
    // confirm, and NOTHING is pasted and nothing typed is lost meanwhile.
    await makeStale(context);
    await input.tap();
    await page.keyboard.type('second, after the confirm');
    await send.tap();
    await expect(dialog(page)).toBeVisible();
    expect(sends, 'the stale send was refused by the server').toEqual([204, 403]);
    await expect(input).toHaveValue('second, after the confirm');
    expect(pasted()).toBe(before + '%9 first from the phone\n');
    const box = (await dialog(page).boundingBox())!;
    expect(box.x + box.width, 'the dialog fits 390px').toBeLessThanOrEqual(390);

    await page.fill('#term-confirm-password', 'not the password');
    await page.locator('#term-confirm-submit').tap();
    await expect(page.locator('#term-confirm-error')).toHaveText('That password is not right.');
    expect(pasted(), 'a wrong password sends nothing').toBe(before + '%9 first from the phone\n');
    await expect(input).toHaveValue('second, after the confirm');

    await page.fill('#term-confirm-password', adminPW);
    await page.locator('#term-confirm-submit').tap();
    await expect(dialog(page)).toBeHidden();
    await expect.poll(pasted).toBe(before + '%9 first from the phone\n%9 second, after the confirm\n');
    await expect(input).toHaveValue('');
    expect(sends, 'the SAME text was sent again after the confirm, once').toEqual([204, 403, 204]);
    expect(Math.abs(Date.now() / 1000 - amrOf(await accessToken(context))), 'the confirm signed in just now').toBeLessThan(60);
    await expect(page.locator('#mv-error')).toBeHidden();
    const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(cw);
  });
});

// ---------------------------------------------------------------------------
// THE 12 HOUR WINDOW and CONFIRMING WITH A PASSKEY, with real GoTrue and
// Chromium's virtual authenticator (real signatures, verified by GoTrue).
//
// 🔴 REGRESSION TESTS: on the commit before the feature the window is five
// minutes (so a sign-in from ten minutes ago is refused, and the first test
// fails at its first send) and the dialog has no passkey button (so the others
// fail at `#term-confirm-passkey`).
// ---------------------------------------------------------------------------

const viewPath = `/mirror/view?host=${encodeURIComponent(host)}&session=main`;
const pasted = () => (fs.existsSync(transcript + '.pasted') ? fs.readFileSync(transcript + '.pasted', 'utf8') : '');
const passkeyBtn = (page: Page) => page.locator('#term-confirm-passkey');
// Why the dialog did NOT offer a passkey (stepUpScript's unavailable): the
// dialog's data-passkey-unavailable, and the muted line that shows it.
const passkeyNote = (page: Page) => page.locator('#term-confirm-passkey-note');
const passwordToggle = (page: Page) => page.locator('#term-confirm-password-toggle');

// usePassword opens the password BEHIND the offered passkey: the dialog shows
// the passkey alone and keeps the password in a collapse, shut
// (stepup_confirm.go's confirmDialog). Waits for the offer first, so the
// collapse is in its settled state when it is opened.
async function usePassword(page: Page) {
  await expect(passkeyBtn(page)).toBeVisible();
  await expect(page.locator('#term-confirm-password'), 'the password is behind its collapse while a passkey is offered').toBeHidden();
  await passwordToggle(page).click();
  await expect(page.locator('#term-confirm-password'), 'opened, the password takes focus').toBeFocused();
}
const subOf = (token: string) => String(b64urlJSON(token.split('.')[1]).sub);

async function signIn(page: Page, email = adminEmail, pw = adminPW) {
  await page.goto(browserURL + '/login');
  await page.fill('#muster-email', email);
  await page.fill('#muster-password', pw);
  await page.click('button[type=submit]');
  // A sign-in that does not land says WHY (the page's own words), rather than
  // timing out on a URL.
  await page.waitForURL(landedHome(browserURL), { timeout: 20_000 }).catch(async () => {
    throw new Error(`sign-in as ${email} did not land on /: ${page.url()} says: ${(await page.locator('body').innerText()).slice(0, 400)}`);
  });
  if (!mirrorProc || mirrorProc.exitCode !== null) mirrorProc = startMirror();
}

async function signOut(page: Page) {
  await page.goto(browserURL + '/account');
  await Promise.all([page.waitForURL(/\/login$/), page.getByRole('button', { name: 'Sign out', exact: true }).click()]);
}

// addPasskey registers a passkey for the signed-in account on this page's
// authenticator. It needs a sign-in from the last FIVE minutes — that window
// did not lengthen — so every caller has just signed in.
async function addPasskey(page: Page) {
  await page.goto(browserURL + '/account/passkeys');
  await expect(page.locator('#passkey-add')).toBeVisible();
  const before = await page.locator('#passkey-list [data-passkey-id]').count();
  await page.locator('#passkey-add').click();
  await expect(page.locator('#passkey-list [data-passkey-id]'), 'the new passkey is listed').toHaveCount(before + 1);
}

// sendFrom opens the rendered view and returns its composer plus a live list of
// the send route's answers.
async function openView(page: Page) {
  const sends: number[] = [];
  page.on('response', (r) => {
    if (new URL(r.url()).pathname === '/mirror/view/send') sends.push(r.status());
  });
  await page.goto(browserURL + viewPath);
  await expect(page.locator('#mv-log [data-bubble="assistant"]').first()).toContainText('Hi.', { timeout: 20_000 });
  await expect(page.locator('#mv-input')).toBeEnabled();
  return { input: page.locator('#mv-input'), send: page.locator('#mv-send'), sends };
}

test.describe('the window and the passkey confirm on a phone (390×844)', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('a sign-in from ten minutes ago still sends; one from over twelve hours ago is asked — by password alone when the account has no passkey (real GoTrue)', async ({ page, context }) => {
    test.setTimeout(120_000);
    await signIn(page);
    const { input, send, sends } = await openView(page);
    const before = pasted();

    // Ten minutes: stale under the old five-minute window, FRESH under twelve hours.
    await makeStale(context, 600);
    await input.tap();
    await page.keyboard.type('ten minutes in');
    await send.tap();
    await expect.poll(pasted).toBe(before + '%9 ten minutes in\n');
    await expect(input, 'cleared once the 204 arrived').toHaveValue('');
    expect(sends, 'sent at once, no step-up').toEqual([204]);
    await expect(dialog(page)).toBeHidden();

    // Eleven hours fifty: still inside the window.
    await makeStale(context, 12 * 3600 - 600);
    await input.tap();
    await page.keyboard.type('nearly twelve hours in');
    await send.tap();
    await expect.poll(pasted).toBe(before + '%9 ten minutes in\n%9 nearly twelve hours in\n');
    await expect(input).toHaveValue('');
    expect(sends).toEqual([204, 204]);
    await page.goto(browserURL + termPath);
    const freshFor = Number(await page.locator('#term').getAttribute('data-fresh-for'));
    expect(freshFor, 'about ten minutes of freshness left').toBeGreaterThan(8 * 60_000);
    expect(freshFor).toBeLessThanOrEqual(10 * 60_000);

    // Twelve hours and ten minutes: asked, and the dialog names the window.
    const v = await openView(page);
    await makeStale(context, 12 * 3600 + 600);
    await v.input.tap();
    await page.keyboard.type('past the window');
    // Listened for BEFORE the send: the dialog asks for the options the moment
    // it opens, and a listener registered after the dialog's own checks missed
    // an answer that had already arrived and waited out the test (measured: the
    // options came back 35 ms after the 403).
    const options = page.waitForResponse((r) => r.url().endsWith('/auth/confirm/passkey/options'), { timeout: 20_000 });
    // The options answer is HELD a moment: this browser can run WebAuthn, so
    // the passkey button shows at the open — inert until the answer. A tap in
    // that moment says nothing (no "A passkey cannot be used right now"):
    // there is no way to reach that message for an account with no passkey.
    let releaseOptions!: () => void;
    const gate = new Promise<void>((r) => { releaseOptions = r; });
    await page.route('**/auth/confirm/passkey/options', async (r) => { await gate; await r.continue(); }, { times: 1 });
    await v.send.tap();
    await expect(dialog(page)).toBeVisible();
    expect(v.sends, 'refused by the server').toEqual([403]);
    await expect(dialog(page)).toContainText('needs a sign-in from the last 12 hours.');
    await expect(dialog(page)).not.toContainText('5 minutes');
    await expect(passkeyBtn(page)).toHaveAttribute('aria-disabled', 'true');
    await passkeyBtn(page).dispatchEvent('click');
    await expect(page.locator('#term-confirm-error'), 'a tap while checking says nothing').toBeHidden();
    releaseOptions();
    // No passkey is registered for this account: the dialog becomes
    // password-only although this GoTrue has passkeys on and this browser
    // supports WebAuthn.
    await options.catch(() => undefined);
    await page.unrouteAll({ behavior: 'wait' });
    await expect(page.locator('#term-confirm-error'), 'and nothing after the answer either').toBeHidden();
    await expect(passkeyBtn(page)).toBeHidden();
    await expect(page.locator('#term-confirm-submit')).toHaveText('Confirm');
    // 🔴 With no passkey the password is the only option: shown with no tap,
    // and nothing to collapse it behind.
    await expect(page.locator('#term-confirm-password'), 'the password is shown open').toBeVisible();
    await expect(passwordToggle(page), 'no collapse summary when the password is all there is').toBeHidden();
    // Recorded as the account having none — and, that being the ordinary
    // password-only case, nothing about it on screen.
    await expect(dialog(page)).toHaveAttribute('data-passkey-unavailable', 'not-available');
    await expect(passkeyNote(page)).toBeHidden();
    await page.fill('#term-confirm-password', adminPW);
    await page.locator('#term-confirm-submit').tap();
    await expect(dialog(page)).toBeHidden();
    await expect.poll(pasted).toBe(before + '%9 ten minutes in\n%9 nearly twelve hours in\n%9 past the window\n');
    await expect(v.input).toHaveValue('');
    expect(v.sends).toEqual([403, 204]);
  });

  test('a stale send is confirmed with a PASSKEY and the same text is delivered; a UV=false assertion and another account’s passkey are refused; the password still works (real GoTrue, virtual authenticator)', async ({ page, context }) => {
    test.setTimeout(300_000);
    const auth: Authenticator = await authenticator(page);
    await signIn(page);
    await addPasskey(page);
    const adminCreds = await credentialIds(auth);
    expect(adminCreds.length, 'the authenticator holds the admin’s passkey').toBe(1);
    const adminSub = subOf(await accessToken(context));

    // 1. STALE → Send → the dialog offers the PASSKEY as the primary action →
    //    confirmed → the SAME text is delivered.
    let v = await openView(page);
    let before = pasted();
    await makeStale(context);
    await v.input.tap();
    await page.keyboard.type('sent after a passkey');
    await v.send.tap();
    await expect(dialog(page)).toBeVisible();
    expect(v.sends, 'the stale send was refused by the server').toEqual([403]);
    await expect(passkeyBtn(page)).toBeVisible();
    await expect(passkeyBtn(page), 'the passkey is the default action').toBeFocused();
    // 🔴 THE PASSKEY IS THE ONE OPTION SHOWN: the password is in a collapse,
    // shut, whose summary is the way in (opened in part 3).
    await expect(page.locator('#term-confirm-password'), 'the password is collapsed behind the passkey').toBeHidden();
    await expect(page.locator('#term-confirm-submit')).toBeHidden();
    await expect(passwordToggle(page)).toBeVisible();
    await expect(passwordToggle(page)).toHaveText('Use your password instead');
    expect(await page.locator('#term-confirm-password-box').evaluate((d) => (d as HTMLDetailsElement).open), 'the collapse is shut').toBe(false);
    expect((await passwordToggle(page).boundingBox())!.height, 'the summary is a 44px touch target').toBeGreaterThanOrEqual(44);
    await expect(page.locator('#term-confirm-cancel'), 'Cancel stays outside the collapse').toBeVisible();
    await expect(dialog(page)).toContainText('Confirm with your passkey, or use your password.');
    const box = (await dialog(page).boundingBox())!;
    expect(box.x + box.width, 'the dialog fits 390px').toBeLessThanOrEqual(390);
    expect(box.y + box.height, 'and is on screen').toBeLessThanOrEqual(844);
    expect((await passkeyBtn(page).boundingBox())!.height, 'a 44px touch target').toBeGreaterThanOrEqual(44);
    await expect(v.input, 'nothing typed is lost while confirming').toHaveValue('sent after a passkey');
    expect(pasted(), 'nothing is pasted while the confirm is open').toBe(before);
    const staleToken = await accessToken(context);

    await passkeyBtn(page).tap();
    await expect(dialog(page)).toBeHidden();
    await expect.poll(pasted).toBe(before + '%9 sent after a passkey\n');
    await expect(v.input).toHaveValue('');
    expect(v.sends, 'the SAME text was sent again after the confirm, once').toEqual([403, 204]);
    const freshToken = await accessToken(context);
    expect(freshToken).not.toBe(staleToken);
    expect(subOf(freshToken), 'the new session is the SAME user').toBe(adminSub);
    expect(Math.abs(Date.now() / 1000 - amrOf(freshToken)), 'the passkey confirm signed in just now').toBeLessThan(60);
    const methods = (b64urlJSON(freshToken.split('.')[1]).amr as { method: string }[]).map((e) => e.method);
    test.info().annotations.push({ type: 'amr-after-passkey-confirm', description: methods.join(',') });
    expect(methods, 'not a password sign-in').not.toContain('password');
    await expect(page.locator('#mv-note')).toBeHidden();

    // 2. 🔴 UV=false. The options are rewritten in flight to userVerification
    //    "discouraged" and the authenticator stops verifying, so the browser
    //    makes a correctly SIGNED assertion with UV=0 — which GoTrue (UV
    //    "preferred") would accept. muster refuses it; nothing is sent.
    before = pasted();
    await setUserVerified(auth, false);
    await page.route('**/auth/confirm/passkey/options', async (route) => {
      const resp = await route.fetch();
      const body = await resp.json();
      if (body.options) body.options.userVerification = 'discouraged';
      await route.fulfill({ response: resp, json: body });
    });
    v = await openView(page);
    await makeStale(context);
    await v.input.tap();
    await page.keyboard.type('must not be sent without verification');
    await v.send.tap();
    await expect(passkeyBtn(page)).toBeVisible();
    const verify = page.waitForResponse((r) => r.url().endsWith('/auth/confirm/passkey/verify'));
    await passkeyBtn(page).tap();
    expect((await verify).status(), 'muster refused the UV=0 assertion').toBe(401);
    await expect(page.locator('#term-confirm-error')).toContainText('did not verify you');
    await expect(dialog(page)).toBeVisible();
    expect(pasted(), 'a UV=0 assertion sends nothing').toBe(before);
    await expect(v.input).toHaveValue('must not be sent without verification');
    expect(Date.now() / 1000 - amrOf(await accessToken(context)), 'still the stale session').toBeGreaterThan(STALE_SECONDS - 100);
    await page.unrouteAll({ behavior: 'wait' });

    // 3. THE PASSWORD FALLBACK, in the same dialog, sends the same text — opened
    //    from its collapse (the passkey is still offered: muster's refusal kept
    //    its challenge).
    await setUserVerified(auth, true);
    await usePassword(page);
    await expect(page.locator('#term-confirm-submit')).toHaveText('Confirm with password');
    await page.fill('#term-confirm-password', 'not the password');
    await page.locator('#term-confirm-submit').tap();
    await expect(page.locator('#term-confirm-error')).toHaveText('That password is not right.');
    expect(pasted(), 'a wrong password sends nothing').toBe(before);
    await page.fill('#term-confirm-password', adminPW);
    await page.locator('#term-confirm-submit').tap();
    await expect(dialog(page)).toBeHidden();
    await expect.poll(pasted).toBe(before + '%9 must not be sent without verification\n');
    await expect(v.input).toHaveValue('');

    // 4. 🔴 ANOTHER ACCOUNT'S PASSKEY. A member joins and registers a passkey on
    //    this same authenticator; the admin's own credential is then removed
    //    from it, so the only passkey this browser can present is the MEMBER's
    //    — perfectly valid, user-verified, and GoTrue (usernameless) accepts it.
    //    Presented on the ADMIN's session, muster refuses it: nothing is sent,
    //    and the browser is still the admin.
    await page.goto(browserURL + '/admin/invites');
    await page.getByRole('button', { name: 'Create link' }).click();
    const link = await page.locator('#once-link').inputValue();
    await signOut(page);
    const memberEmail = `stepup-member-${Date.now()}@example.com`;
    const memberPW = 'e2e-stepup-member-password-not-real';
    await page.goto(link);
    await page.fill('#invite-email', memberEmail);
    await page.fill('#invite-password', memberPW);
    await page.fill('#invite-password2', memberPW);
    await Promise.all([page.waitForURL(browserURL + '/account/passkeys?welcome=1'), page.getByRole('button', { name: 'Create account' }).click()]);
    await page.locator('#passkey-add').click();
    await expect(page.locator('#passkey-list [data-passkey-id]')).toHaveCount(1);
    await signOut(page);
    await removeCredential(auth, adminCreds[0]);
    expect((await credentialIds(auth)).length, 'the authenticator now holds ONLY the member’s passkey').toBe(1);

    await signIn(page);
    v = await openView(page);
    before = pasted();
    await makeStale(context);
    await v.input.tap();
    await page.keyboard.type('must not be sent as somebody else');
    await v.send.tap();
    await expect(passkeyBtn(page), 'the admin HAS a passkey, so one is offered').toBeVisible();
    const verify2 = page.waitForResponse((r) => r.url().endsWith('/auth/confirm/passkey/verify'));
    await passkeyBtn(page).tap();
    expect((await verify2).status(), 'muster refused another account’s passkey').toBe(403);
    await expect(page.locator('#term-confirm-error')).toContainText('belongs to a different account');
    await expect(dialog(page)).toBeVisible();
    expect(pasted(), 'another account’s passkey sends nothing').toBe(before);
    await expect(v.input).toHaveValue('must not be sent as somebody else');
    const after = await accessToken(context);
    expect(subOf(after), '🔴 the browser is STILL the admin, not the member').toBe(adminSub);
    expect(Date.now() / 1000 - amrOf(after), 'and still stale').toBeGreaterThan(STALE_SECONDS - 100);
    await page.locator('#term-confirm-cancel').tap();
    const account = await (await page.request.get(browserURL + '/account')).text();
    expect(account.includes(adminEmail) && !account.includes(memberEmail), 'the account page is the admin’s').toBe(true);
    const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(cw);
  });
});

test.describe('the passkey confirm at a desktop width (1280px)', () => {
  test.use({ viewport: { width: 1280, height: 900 }, hasTouch: false, isMobile: false });

  test('live keys, and a send from the Terminal composer, are confirmed with a passkey (real GoTrue, virtual authenticator)', async ({ page, context }) => {
    test.setTimeout(180_000);
    await authenticator(page);
    await signIn(page);
    await addPasskey(page); // this browser's own passkey for the admin
    const adminSub = subOf(await accessToken(context));

    // The interactive attach: stale → a key asks → passkey → rw.
    const termSockets: string[] = [];
    page.on('websocket', (ws) => {
      if (ws.url().includes('/mirror/terminal/ws')) termSockets.push(ws.url());
    });
    await makeStale(context);
    await page.goto(browserURL + termPath);
    await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro');
    await page.locator('#term-keys [data-key="up"]').click();
    await expect(dialog(page)).toBeVisible();
    await expect(dialog(page)).toContainText('needs a sign-in from the last 12 hours.');
    await expect(passkeyBtn(page)).toBeVisible();
    await expect(passkeyBtn(page)).toBeFocused();
    // Offered: no reason recorded, none shown.
    await expect(dialog(page)).not.toHaveAttribute('data-passkey-unavailable', /.*/);
    await expect(passkeyNote(page)).toBeHidden();
    await passkeyBtn(page).click();
    await expect(dialog(page)).toBeHidden();
    await expect(screen(page)).toContainText('mode=rw');
    await expect(page.locator('#term-status')).toHaveText('Connected');
    let token = await accessToken(context);
    expect(subOf(token)).toBe(adminSub);
    expect(Math.abs(Date.now() / 1000 - amrOf(token)), 'the passkey confirm signed in just now').toBeLessThan(60);
    await page.locator('#term').click();
    await page.keyboard.type('ok');
    await expect(screen(page)).toContainText(/in=6F6B|in=6F/);

    // The Terminal view's composer (main's active pane is its Claude pane, %9):
    // stale → Send → passkey → the same text is delivered.
    const socketsBeforeSend = termSockets.length;
    const sends: number[] = [];
    page.on('response', (r) => {
      if (new URL(r.url()).pathname === '/mirror/terminal/send') sends.push(r.status());
    });
    const before = pasted();
    await makeStale(context);
    await page.locator('#mv-input').fill('from the desktop terminal');
    await page.locator('#mv-send').click();
    await expect(dialog(page)).toBeVisible();
    expect(sends).toEqual([403]);
    await expect(passkeyBtn(page)).toBeVisible();
    await passkeyBtn(page).click();
    await expect(dialog(page)).toBeHidden();
    await expect.poll(pasted).toBe(before + '%9 from the desktop terminal\n');
    await expect(page.locator('#mv-input')).toHaveValue('');
    expect(sends).toEqual([403, 204]);
    // The send went by the composer's route: the terminal's attach is the one
    // it was, untouched by it.
    expect(termSockets.length, 'the send opened no terminal socket').toBe(socketsBeforeSend);
    await expect(page.locator('#term-status')).toHaveText('Connected');
    token = await accessToken(context);
    expect(Math.abs(Date.now() / 1000 - amrOf(token))).toBeLessThan(60);
  });
});

// ---------------------------------------------------------------------------
// ONE PASSKEY CHALLENGE PER DIALOG OPEN (stepup_confirm.go's stepUpScript).
// Seen live: several /auth/confirm/passkey/options calls in a row and no
// /verify. Two ways the code made them: every ask() refetched (and hid the
// passkey button) even with the dialog already open, and the terminal sent an
// ask per key pressed on a stale attach; and every failed tap fetched a
// replacement challenge, a cancelled navigator.credentials.get included —
// whose challenge never reached the server and was still good.
//
// The /options calls are counted in the page, at the fetch the script makes
// (synchronous, so exact the moment the script returns), and cross-checked
// against the browser's own request events.
//
// 🔴 REGRESSION TESTS: before the fix the first fails at the ask count (4) and
// the options count (4), the second at the options count after three cancelled
// taps (4) and at the telemetry events (none).
// ---------------------------------------------------------------------------
test.describe('one passkey challenge per dialog open (1280px)', () => {
  test.use({ viewport: { width: 1280, height: 900 }, hasTouch: false, isMobile: false });

  const optionsPath = '/auth/confirm/passkey/options';
  type W = Window & { __options?: number; __asks?: number; __events?: [string, Record<string, unknown>][]; __reject?: number };

  // countOptions counts, in the page, every /options fetch the page makes.
  async function countOptions(page: Page) {
    await page.addInitScript((p) => {
      const w = window as unknown as W;
      w.__options = 0;
      const orig = window.fetch.bind(window);
      window.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
        if (String(input instanceof Request ? input.url : input).split('?')[0].endsWith(p)) w.__options!++;
        return orig(input, init);
      };
    }, optionsPath);
    const net: string[] = [];
    page.on('request', (r) => {
      if (new URL(r.url()).pathname === optionsPath) net.push(r.url());
    });
    return { inPage: () => page.evaluate(() => (window as unknown as W).__options!), net };
  }

  // ownAddress gives this test its own client address: muster's browser
  // listener reads CF-Connecting-IP (clientIP) and forwards it to GoTrue, which
  // buckets its passkey rate limit on it. The tests above already spend most of
  // 127.0.0.1's budget; a test that adds many passkey calls takes its own
  // (documentation range) address instead of starving the tests after it.
  async function ownAddress(context: BrowserContext, ip: string) {
    await context.setExtraHTTPHeaders({ 'CF-Connecting-IP': ip });
  }

  type Ev = [string, Record<string, unknown>];
  // recordTelemetry makes window.cgTrack a recorder (telemetry is off in e2e:
  // no Faro, so nothing else defines it).
  async function recordTelemetry(page: Page) {
    await page.addInitScript(() => {
      const w = window as unknown as W & { cgTrack: (n: string, a: Record<string, unknown>) => void };
      w.__events = [];
      w.cgTrack = (n, a) => { w.__events!.push([n, JSON.parse(JSON.stringify(a))]); };
    });
    return () => page.evaluate(() => (window as unknown as W).__events!) as Promise<Ev[]>;
  }
  // taps is the per-tap events with open_id taken off: the stepup.passkey shape
  // as it was before the fetch events were added (open_id is checked apart).
  const taps = (evs: Ev[]) => evs.filter(([n]) => n === 'stepup.passkey').map(([n, a]) => {
    const { open_id: _id, ...rest } = a;
    return [n, rest];
  });
  const fetches = (evs: Ev[]) => evs.filter(([n]) => n === 'stepup.passkey_options');

  async function staleTerminal(page: Page, context: BrowserContext) {
    await makeStale(context);
    await page.goto(browserURL + termPath);
    await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro');
  }

  test('keys pressed before the modal takes over ask once, a re-ask while open fetches nothing, and one challenge confirms (real GoTrue, virtual authenticator)', async ({ page, context }) => {
    test.setTimeout(180_000);
    await authenticator(page);
    await signIn(page);
    await addPasskey(page);
    const options = await countOptions(page);
    await staleTerminal(page, context);
    expect(await options.inPage(), 'nothing is fetched on page load').toBe(0);
    // Every ask the page makes, counted at musterStepUp.ask (the terminal's
    // stepUp is this same object, and calls .ask at call time).
    await page.evaluate(() => {
      const w = window as unknown as W & { musterStepUp: { ask: (...a: unknown[]) => void } };
      const s = w.musterStepUp, orig = s.ask;
      w.__asks = 0;
      s.ask = function (...a: unknown[]) { w.__asks!++; return orig.apply(this, a); };
    });

    // 1. FOUR key-bar presses in one task — inputs that reach the terminal
    //    before the modal has the keyboard.
    await page.evaluate(() => {
      const b = document.querySelector<HTMLButtonElement>('#term-keys [data-key="up"]')!;
      for (let i = 0; i < 4; i++) b.click();
    });
    await expect(dialog(page)).toBeVisible();
    await expect(passkeyBtn(page)).toBeVisible();
    expect(await page.evaluate(() => (window as unknown as W).__asks), 'the terminal asked once for four keys').toBe(1);
    // POSITIVE CONTROL (the counter sees a fetch): the open's own, exactly one.
    expect(await options.inPage(), 'one options call for the open').toBe(1);

    // 2. A RE-ASK WHILE OPEN (another caller, the view switch's frame): the
    //    offer stands, synchronously, and nothing is fetched.
    const offered = await page.evaluate(() => {
      const s = (window as unknown as { musterStepUp: { ask: (w: string, cb: null) => void } }).musterStepUp;
      s.ask('', null);
      s.ask('', null);
      return document.getElementById('term-confirm')!.getAttribute('data-passkey-offered');
    });
    expect(offered, 'a re-ask while open leaves the passkey offered').toBe('1');
    expect(await options.inPage(), 'a re-ask while open fetches no challenge').toBe(1);
    await expect(passkeyBtn(page)).toBeVisible();

    // 3. POSITIVE CONTROL: that one challenge confirms, and the attach goes rw.
    const verify = page.waitForResponse((r) => new URL(r.url()).pathname === '/auth/confirm/passkey/verify');
    await passkeyBtn(page).click();
    expect((await verify).status()).toBe(200);
    await expect(dialog(page)).toBeHidden();
    await expect(screen(page)).toContainText('mode=rw');
    expect(await options.inPage()).toBe(1);
    expect(options.net.length, 'the browser saw the same one request').toBe(1);
  });

  test('cancelled taps reuse the open’s challenge, a refused /verify fetches one replacement, and each tap sends one telemetry event (real GoTrue, virtual authenticator)', async ({ page, context }) => {
    test.setTimeout(180_000);
    await authenticator(page);
    await signIn(page);
    await addPasskey(page);
    const options = await countOptions(page);
    const events = await recordTelemetry(page);
    await staleTerminal(page, context);
    // navigator.credentials.get rejects as a person's cancel does, __reject
    // times, then is the virtual authenticator's again.
    await page.evaluate(() => {
      const w = window as unknown as W;
      const c = navigator.credentials, orig = c.get.bind(c);
      w.__reject = 3;
      Object.defineProperty(c, 'get', {
        configurable: true,
        value: (o?: CredentialRequestOptions) => (w.__reject!-- > 0
          ? Promise.reject(new DOMException('The operation either timed out or was not allowed.', 'NotAllowedError'))
          : orig(o)),
      });
    });
    await page.locator('#term-keys [data-key="up"]').click();
    await expect(passkeyBtn(page)).toBeVisible();
    expect(await options.inPage()).toBe(1);

    // 1. THREE CANCELLED TAPS: no /verify, and no new challenge.
    const verifies: number[] = [];
    page.on('response', (r) => {
      if (new URL(r.url()).pathname === '/auth/confirm/passkey/verify') verifies.push(r.status());
    });
    for (let i = 0; i < 3; i++) {
      await passkeyBtn(page).click();
      await expect(page.locator('#term-confirm-error')).toContainText('The passkey was cancelled.');
      await expect(passkeyBtn(page)).toBeEnabled();
    }
    expect(await options.inPage(), 'three cancelled taps fetched no challenge').toBe(1);
    expect(verifies).toEqual([]);

    // 2. THE SAME CHALLENGE, three cancels later, is still good: real GoTrue
    //    verifies it, and the attach goes rw.
    await passkeyBtn(page).click();
    await expect(dialog(page)).toBeHidden();
    await expect(screen(page)).toContainText('mode=rw');
    expect(verifies, 'the cancelled-and-reused challenge verified').toEqual([200]);
    expect(await options.inPage()).toBe(1);
    // ONE EVENT PER TAP, with how each one ended — and nothing else in it.
    const cancel = { options_trigger: 'open', options_status: 200, get: 'NotAllowedError', verify: '' };
    const first = await events();
    expect(taps(first)).toEqual([
      ['stepup.passkey', { attempt: 1, ...cancel }],
      ['stepup.passkey', { attempt: 2, ...cancel }],
      ['stepup.passkey', { attempt: 3, ...cancel }],
      ['stepup.passkey', { attempt: 4, options_trigger: 'open', options_status: 200, get: 'ok', verify: 200 }],
    ]);
    // ...and ONE fetch event, the open's, for four taps; all five share an id.
    expect(fetches(first).map(([n, a]) => [n, a.trigger, a.status, a.taps])).toEqual([['stepup.passkey_options', 'open', 200, 0]]);
    expect(new Set(first.map(([, a]) => a.open_id)).size, 'one open, one id').toBe(1);

    // 3. A NEW OPEN fetches again (one call). A /verify refused (answered
    //    here, 401) has spent THAT challenge, so exactly one replacement is
    //    fetched — before the next tap, which it then confirms.
    await makeStale(context);
    await page.goto(browserURL + termPath);
    await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro');
    await page.locator('#term-keys [data-key="up"]').click();
    await expect(passkeyBtn(page)).toBeVisible();
    expect(await options.inPage(), 'a new document, a new open: one call').toBe(1);
    await page.route('**/auth/confirm/passkey/verify', (r) => r.fulfill({ status: 401, json: { error: 'That passkey was not accepted.' } }), { times: 1 });
    await passkeyBtn(page).click();
    await expect(page.locator('#term-confirm-error')).toContainText('That passkey was not accepted.');
    await expect.poll(options.inPage, { message: 'one replacement after a refused /verify' }).toBe(2);
    await page.unrouteAll({ behavior: 'wait' });
    await expect(passkeyBtn(page)).toBeEnabled();
    await passkeyBtn(page).click();
    await expect(dialog(page)).toBeHidden();
    await expect(screen(page)).toContainText('mode=rw');
    expect(verifies, 'the browser saw every /verify answer').toEqual([200, 401, 200]);
    expect(await options.inPage()).toBe(2);
    expect(options.net.length, 'the browser saw 1 + 2 options requests').toBe(3);

    // This document's events (the first document's left with it).
    const second = await events();
    expect(taps(second)).toEqual([
      ['stepup.passkey', { attempt: 1, options_trigger: 'open', options_status: 200, get: 'ok', verify: 401 }],
      ['stepup.passkey', { attempt: 2, options_trigger: 'retry', options_status: 200, get: 'ok', verify: 200 }],
    ]);
    // The refused /verify's replacement is a retry fetch, made after tap 1.
    expect(fetches(second).map(([n, a]) => [n, a.trigger, a.status, a.taps])).toEqual([
      ['stepup.passkey_options', 'open', 200, 0],
      ['stepup.passkey_options', 'retry', 200, 1],
    ]);
    expect(new Set(second.map(([, a]) => a.open_id)).size).toBe(1);
    expect(second[0][1].open_id, 'a new document, a new open: a new id').not.toBe(first[0][1].open_id);
  });

  // 🔴 ONE stepup.passkey_options EVENT PER /options FETCH, the tapless ones
  // included: a burst of fetches with no tap — what was seen live — is visible
  // in the telemetry only through these (the per-tap event never fires).
  // REGRESSION TEST: before these events existed it fails at the first
  // assertion on them (no fetch event at all).
  test('telemetry: one stepup.passkey_options event per /options fetch (open, stale, retry), none for a re-ask, and one id per open (real GoTrue, virtual authenticator)', async ({ page, context }) => {
    test.setTimeout(180_000);
    await authenticator(page);
    await signIn(page);
    await addPasskey(page);
    const options = await countOptions(page);
    const events = await recordTelemetry(page);
    await staleTerminal(page, context);
    await page.evaluate(() => {
      const w = window as unknown as W;
      const c = navigator.credentials, orig = c.get.bind(c);
      w.__reject = 1;
      Object.defineProperty(c, 'get', {
        configurable: true,
        value: (o?: CredentialRequestOptions) => (w.__reject!-- > 0
          ? Promise.reject(new DOMException('The operation either timed out or was not allowed.', 'NotAllowedError'))
          : orig(o)),
      });
    });

    // 1. AN OPEN WITH NO TAP: exactly one fetch event (open, 200, 0 taps),
    //    sent when the options answer, and no tap event. (The passkey is
    //    offered from the open's first frame, BEFORE that answer, so its being
    //    visible says nothing about the event: the event itself is awaited.)
    await page.locator('#term-keys [data-key="up"]').click();
    await expect(passkeyBtn(page)).toBeVisible();
    await expect.poll(async () => (await events()).length, { message: 'the open\'s fetch sent its event' }).toBe(1);
    const open1 = await events();
    expect(open1.map(([n, a]) => [n, a.trigger, a.status, a.taps])).toEqual([['stepup.passkey_options', 'open', 200, 0]]);
    const id1 = open1[0][1].open_id as string;
    expect(id1, 'a random per-open id: 16 hex digits').toMatch(/^[0-9a-f]{16}$/);
    expect(Object.keys(open1[0][1]).sort(), 'the fetch event carries these fields and nothing else').toEqual(['open_id', 'status', 'taps', 'trigger']);

    // 2. RE-ASKS WHILE OPEN (more keys, other callers): no fetch, no event.
    //    Checked again at the end, after later awaits, so a late one shows.
    //    (The modal takes the pointer, so the key is pressed in the page — as a
    //    key that reaches the terminal under it.)
    await page.evaluate(() => {
      document.querySelector<HTMLButtonElement>('#term-keys [data-key="up"]')!.click();
      const s = (window as unknown as { musterStepUp: { ask: (w: string, cb: null) => void } }).musterStepUp;
      s.ask('', null);
      s.ask('', null);
    });
    expect(await options.inPage()).toBe(1);
    expect(await events(), 'a re-ask while open sends nothing').toHaveLength(1);

    // 3. A STALE CHALLENGE: the page's clock moves past mpkMaxAge (240 s), so
    //    tap 1 fetches again (trigger stale, taps 1); that tap is cancelled,
    //    so the next one keeps the stale fetch's challenge.
    await page.evaluate(() => { const n = Date.now.bind(Date); Date.now = () => n() + 250_000; });
    await passkeyBtn(page).click();
    await expect(page.locator('#term-confirm-error')).toContainText('The passkey was cancelled.');
    await expect(passkeyBtn(page)).toBeEnabled();
    expect(await options.inPage(), 'the stale tap fetched once').toBe(2);

    // 4. A REFUSED /verify (401): its replacement is a retry fetch, after tap 2.
    await page.route('**/auth/confirm/passkey/verify', (r) => r.fulfill({ status: 401, json: { error: 'That passkey was not accepted.' } }), { times: 1 });
    await passkeyBtn(page).click();
    await expect(page.locator('#term-confirm-error')).toContainText('That passkey was not accepted.');
    await expect.poll(async () => fetches(await events()).length, { message: 'the retry fetch sent its event' }).toBe(3);
    await page.unrouteAll({ behavior: 'wait' });
    await expect(passkeyBtn(page)).toBeEnabled();

    // 5. Tap 3 uses the retry challenge and confirms against real GoTrue.
    await passkeyBtn(page).click();
    await expect(dialog(page)).toBeHidden();
    await expect(screen(page)).toContainText('mode=rw');
    const all1 = await events();
    expect(all1.map(([n, a]) => n === 'stepup.passkey'
      ? [n, a.attempt, a.options_trigger, a.options_status, a.get, a.verify]
      : [n, a.trigger, a.status, a.taps])).toEqual([
      ['stepup.passkey_options', 'open', 200, 0],
      ['stepup.passkey_options', 'stale', 200, 1],
      ['stepup.passkey', 1, 'stale', 200, 'NotAllowedError', ''],
      ['stepup.passkey', 2, 'stale', 200, 'ok', 401],
      ['stepup.passkey_options', 'retry', 200, 2],
      ['stepup.passkey', 3, 'retry', 200, 'ok', 200],
    ]);
    expect(all1.map(([, a]) => a.open_id), 'every event of one open has its id').toEqual(all1.map(() => id1));
    expect(await options.inPage()).toBe(3);

    // 6. A NEW OPEN in the same document, closed with no tap: one fetch event,
    //    a NEW id, and no tap event.
    await page.evaluate(() => (window as unknown as { musterStepUp: { ask: (w: string, cb: null) => void } }).musterStepUp.ask('', null));
    await expect(passkeyBtn(page)).toBeVisible();
    // The passkey shows at the open's first frame, before /options answers:
    // its event is awaited, not inferred from the button.
    await expect.poll(async () => (await events()).length, { message: 'the new open\'s fetch sent its event' }).toBe(all1.length + 1);
    await page.locator('#term-confirm-cancel').click();
    await expect(dialog(page)).toBeHidden();
    const all = await events();
    expect(all.slice(all1.length).map(([n, a]) => [n, a.trigger, a.status, a.taps])).toEqual([['stepup.passkey_options', 'open', 200, 0]]);
    const id2 = all[all1.length][1].open_id as string;
    expect(id2).toMatch(/^[0-9a-f]{16}$/);
    expect(id2, 'a new open, a new id').not.toBe(id1);
    expect(await options.inPage()).toBe(4);
    expect(options.net.length, 'the browser saw the same four requests').toBe(4);
  });

  // 🔴 A /verify MUSTER REFUSED BEFORE GoTrue SAW THE CHALLENGE KEEPS IT. The
  // server says so (challenge_unspent: true, stepUpRefuseUnspent) because the
  // status cannot: GoTrue's own refusals are 401 and 429 too. Open A: muster's
  // limiter (a 429 answered here with the body the server writes, pinned by
  // TestARefusalBeforeGoTrueKeepsTheChallenge) and a REAL no-UV refusal (the
  // UV bit cleared in flight, so muster refuses it before GoTrue) fetch
  // nothing, and the third tap verifies on the SAME challenge. Open B: a REAL
  // GoTrue refusal (an unknown challenge id) fetches exactly one replacement;
  // GoTrue's own 429 (no field) fetches nothing until the next tap.
  // REGRESSION TEST: before the field was read, the first refusal fetched a
  // replacement at once — this fails at its first options count.
  test('a /verify muster refused before GoTrue (its limiter’s 429, no user verification) keeps the challenge — no fetch, the next tap verifies on it; a GoTrue refusal fetches one replacement, GoTrue’s 429 none until the next tap (real GoTrue, virtual authenticator)', async ({ page, context }) => {
    test.setTimeout(180_000);
    await ownAddress(context, '198.51.100.41');
    await authenticator(page);
    await signIn(page);
    await addPasskey(page);
    const options = await countOptions(page);
    const events = await recordTelemetry(page);
    await staleTerminal(page, context);
    const verifyPath = '/auth/confirm/passkey/verify';
    // The challenge each tap's /verify carried (as the PAGE sent it) and its answer.
    const sent: { challenge: string; status: number }[] = [];
    page.on('response', (r) => {
      if (new URL(r.url()).pathname === verifyPath) sent.push({ challenge: String(r.request().postDataJSON().challenge_id), status: r.status() });
    });
    // tapped waits for tap n's stepup.passkey event: sent in the handler's
    // finally, just BEFORE any replacement fetch is started (synchronously), so
    // the options count read after it is final for that tap.
    const tapped = (n: number) => expect.poll(async () => taps(await events()).length).toBe(n);
    const noUV = async (r: Route) => {
      const b = r.request().postDataJSON();
      const ad = Buffer.from(b.credential.response.authenticatorData, 'base64url');
      ad[32] &= ~0x04; // the UV flag
      b.credential.response.authenticatorData = ad.toString('base64url');
      await r.continue({ postData: JSON.stringify(b) });
    };

    // OPEN A.
    await page.locator('#term-keys [data-key="up"]').click();
    await expect(passkeyBtn(page)).toBeVisible();
    expect(await options.inPage()).toBe(1);

    // A1. muster's limiter.
    await page.route('**' + verifyPath, (r) => r.fulfill({ status: 429, json: { error: 'Too many attempts. Wait a few minutes and try again.', challenge_unspent: true } }), { times: 1 });
    await passkeyBtn(page).click();
    await tapped(1);
    await expect(page.locator('#term-confirm-error')).toContainText('Too many attempts.');
    expect(await options.inPage(), 'muster’s 429 fetched no replacement').toBe(1);
    await page.unrouteAll({ behavior: 'wait' });

    // A2. A real no-UV refusal from muster.
    await page.route('**' + verifyPath, noUV, { times: 1 });
    await expect(passkeyBtn(page)).toBeEnabled();
    await passkeyBtn(page).click();
    await tapped(2);
    await expect(page.locator('#term-confirm-error')).toContainText('did not verify you');
    expect(await options.inPage(), 'muster’s no-UV 401 fetched no replacement').toBe(1);
    await page.unrouteAll({ behavior: 'wait' });

    // A3. The same challenge, twice refused by muster, verifies against real GoTrue.
    await expect(passkeyBtn(page)).toBeEnabled();
    await passkeyBtn(page).click();
    await expect(dialog(page)).toBeHidden();
    await expect(screen(page)).toContainText('mode=rw');
    expect(sent.map((s) => s.status)).toEqual([429, 401, 200]);
    expect(new Set(sent.map((s) => s.challenge)).size, 'all three taps sent the open’s one challenge').toBe(1);
    expect(await options.inPage()).toBe(1);
    expect(options.net.length).toBe(1);
    expect(taps(await events()).map(([, a]) => [a.options_trigger, a.verify])).toEqual([['open', 429], ['open', 401], ['open', 200]]);

    // OPEN B (a new document).
    await makeStale(context);
    await page.goto(browserURL + termPath);
    await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro');
    sent.length = 0;
    await page.locator('#term-keys [data-key="up"]').click();
    await expect(passkeyBtn(page)).toBeVisible();
    expect(await options.inPage()).toBe(1);

    // B1. A real GoTrue refusal: the id is swapped in flight for one GoTrue
    //     never issued. Exactly one replacement, fetched at once.
    await page.route('**' + verifyPath, async (r) => {
      const b = r.request().postDataJSON();
      b.challenge_id = '00000000-0000-4000-8000-000000000000';
      await r.continue({ postData: JSON.stringify(b) });
    }, { times: 1 });
    await passkeyBtn(page).click();
    await tapped(1);
    await expect(page.locator('#term-confirm-error')).toContainText('That passkey was not accepted.');
    expect(await options.inPage(), 'a GoTrue refusal fetched one replacement').toBe(2);
    await page.unrouteAll({ behavior: 'wait' });

    // B2. GoTrue's own 429 (no field): nothing fetched now.
    await page.route('**' + verifyPath, (r) => r.fulfill({ status: 429, json: { error: 'Too many attempts. Wait a few minutes and try again.' } }), { times: 1 });
    await expect(passkeyBtn(page)).toBeEnabled();
    await passkeyBtn(page).click();
    await tapped(2);
    await expect(page.locator('#term-confirm-error')).toContainText('Too many attempts.');
    expect(await options.inPage(), 'GoTrue’s 429 fetched nothing').toBe(2);
    await page.unrouteAll({ behavior: 'wait' });

    // B3. The next tap fetches (retry) and verifies.
    await expect(passkeyBtn(page)).toBeEnabled();
    await passkeyBtn(page).click();
    await expect(dialog(page)).toBeHidden();
    await expect(screen(page)).toContainText('mode=rw');
    expect(sent.map((s) => s.status)).toEqual([401, 429, 200]);
    expect(new Set(sent.map((s) => s.challenge)).size, 'three taps, three challenges').toBe(3);
    expect(await options.inPage()).toBe(3);
    // B1's replacement was fetched after tap 1; B3's inside tap 3 (taps counts it).
    expect(fetches(await events()).map(([, a]) => [a.trigger, a.status, a.taps])).toEqual([['open', 200, 0], ['retry', 200, 1], ['retry', 200, 3]]);
  });

  type Run = Window & { __ran: string[]; musterStepUp: { ask: (w: string, cb: ((ff: number) => void) | null) => void } };
  const ran = (page: Page) => page.evaluate(() => (window as unknown as Run).__ran);

  // ONE TEST, FOUR STEPS: each passkey open spends GoTrue passkey budget
  // (ownAddress), and one sign-in and one registration serve all four.
  //
  // 1-2. Callbacks chained by a re-ask while the dialog is open (callers all
  //    waiting on one confirm) each run EXACTLY ONCE, in the order they asked,
  //    after a passkey confirm (1) and a password confirm (2). Step 1 is an
  //    INVARIANT GUARD (the chain already did this; keep-only-first and
  //    keep-only-last each turn it red). Step 2 is a REGRESSION TEST: one that
  //    throws no longer skips the ones after it (before, `prev(ff); cb(ff)`
  //    stopped at the throw). They are also the positive controls for 3-4: a
  //    confirm that succeeds still runs its callbacks.
  // 3-4. 🔴 ESCAPE IS A CANCEL. Closing the dialog without confirming drops the
  //    waiting action, so a passkey /verify (3) or a password POST (4) still in
  //    flight does not run it when it answers. REGRESSION TEST: before, only
  //    Cancel dropped them, and step 3 ran the action.
  test('callbacks chained while open each run once, in order, after a confirm, and one that throws does not stop the next; Escape during an in-flight /verify or password POST runs none (real GoTrue, virtual authenticator)', async ({ page, context }) => {
    test.setTimeout(180_000);
    await ownAddress(context, '198.51.100.42');
    await authenticator(page);
    await signIn(page);
    await addPasskey(page);
    const events = await recordTelemetry(page);
    const errors: string[] = [];
    page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
    await staleTerminal(page, context);

    // 1. a opens the dialog, b chains while it is open, a null caller drops nothing.
    await page.evaluate(() => {
      const w = window as unknown as Run;
      w.__ran = [];
      w.musterStepUp.ask('', () => { w.__ran.push('a'); });
      w.musterStepUp.ask('', () => { w.__ran.push('b'); });
      w.musterStepUp.ask('', null);
    });
    await expect(passkeyBtn(page)).toBeVisible();
    await passkeyBtn(page).click();
    await expect(dialog(page)).toBeHidden();
    await expect.poll(() => ran(page)).toEqual(['a', 'b']);

    // 2. x throws; c and d still run, in order — confirmed by password.
    await page.evaluate(() => {
      const w = window as unknown as Run;
      w.musterStepUp.ask('', () => { w.__ran.push('x'); throw new Error('boom'); });
      w.musterStepUp.ask('', () => { w.__ran.push('c'); });
      w.musterStepUp.ask('', () => { w.__ran.push('d'); });
    });
    await expect(dialog(page)).toBeVisible();
    await usePassword(page);
    await page.fill('#term-confirm-password', adminPW);
    await page.locator('#term-confirm-submit').click();
    await expect(dialog(page)).toBeHidden();
    await expect.poll(() => ran(page)).toEqual(['a', 'b', 'x', 'c', 'd']);
    expect(errors.some((e) => e.includes('a confirmed action failed')), 'the throw was logged').toBe(true);

    // hold routes glob's next request until the returned release() is called,
    // then sends it on.
    const hold = async (glob: string) => {
      let release!: () => void;
      const gate = new Promise<void>((r) => { release = r; });
      await page.route(glob, async (r) => { await gate; await r.continue(); }, { times: 1 });
      return () => release();
    };
    const isPath = (p: string) => (r: { url: () => string }) => new URL(r.url()).pathname === p;

    // 3. Escape while a passkey /verify is in flight.
    await page.evaluate(() => {
      const w = window as unknown as Run;
      w.__ran = [];
      w.musterStepUp.ask('', () => { w.__ran.push('passkey'); });
    });
    await expect(passkeyBtn(page)).toBeVisible();
    const tapsBefore = taps(await events()).length;
    let release = await hold('**/auth/confirm/passkey/verify');
    let request = page.waitForRequest(isPath('/auth/confirm/passkey/verify'));
    let answer = page.waitForResponse(isPath('/auth/confirm/passkey/verify'));
    await passkeyBtn(page).click();
    await request;
    await page.keyboard.press('Escape');
    await expect(dialog(page)).toBeHidden();
    release();
    expect((await answer).status(), 'the verify itself succeeded').toBe(200);
    await expect.poll(async () => taps(await events()).length, { message: 'the tap’s handler finished' }).toBe(tapsBefore + 1);
    expect(await ran(page), 'an escaped passkey confirm ran nothing').toEqual([]);
    await page.unrouteAll({ behavior: 'wait' });

    // 4. Escape while a password POST is in flight.
    await page.evaluate(() => {
      const w = window as unknown as Run;
      w.musterStepUp.ask('', () => { w.__ran.push('password'); });
    });
    await expect(dialog(page)).toBeVisible();
    await usePassword(page);
    await page.fill('#term-confirm-password', adminPW);
    release = await hold('**/auth/confirm');
    request = page.waitForRequest(isPath('/auth/confirm'));
    answer = page.waitForResponse(isPath('/auth/confirm'));
    await page.locator('#term-confirm-submit').click();
    await request;
    await page.keyboard.press('Escape');
    await expect(dialog(page)).toBeHidden();
    release();
    expect((await answer).status(), 'the password confirm itself succeeded').toBe(200);
    // finish clears the password field: the answer has been handled.
    await expect(page.locator('#term-confirm-password')).toHaveValue('');
    expect(await ran(page), 'an escaped password confirm ran nothing').toEqual([]);
    await page.unrouteAll({ behavior: 'wait' });

    // 5. 🔴 SHUT FROM THE FIRST FRAME (operator decision): on a device that
    //    can use a passkey the password's collapse is shut, its summary shown
    //    and focus on the passkey button — read in the SAME task as the open
    //    and again at the next frame, so no flash of the password can hide
    //    between them — and it STAYS shut while /options is held (slow),
    //    with the summary the way to the password. REGRESSION TEST: red at
    //    1ba3b4b4, which drew the password open, focused, until the answer.
    const releaseOptions = await hold('**/auth/confirm/passkey/options');
    const held = page.waitForRequest(isPath('/auth/confirm/passkey/options'));
    const first = await page.evaluate(() => new Promise((res) => {
      const read = () => ({
        open: (document.getElementById('term-confirm-password-box') as HTMLDetailsElement).open,
        focus: document.activeElement ? document.activeElement.id : '',
        summary: !document.getElementById('term-confirm-password-toggle')!.hidden,
        // checkVisibility, not client rects: a shut <details> keeps its
        // content's boxes (content-visibility), so only this says "hidden".
        passkey: document.getElementById('term-confirm-passkey')!.checkVisibility(),
        waiting: document.getElementById('term-confirm-passkey')!.getAttribute('aria-disabled') === 'true',
        password: document.getElementById('term-confirm-password')!.checkVisibility(),
      });
      (window as unknown as Run).musterStepUp.ask('', null);
      const sync = read();
      requestAnimationFrame(() => res({ sync, frame: read() }));
    }));
    const shut = { open: false, focus: 'term-confirm-passkey', summary: true, passkey: true, waiting: true, password: false };
    expect(first, 'shut, the passkey focused, at the open and at its first frame').toEqual({ sync: shut, frame: shut });
    await held;
    await expect(passkeyBtn(page)).toBeVisible();
    await expect(page.locator('#term-confirm-password'), 'still shut while the options are slow').toBeHidden();
    await expect(passkeyBtn(page)).toBeFocused();
    // 🔴 INERT UNTIL THE OPTIONS ANSWER: a tap now would await the fetch
    //    inside its gesture (iOS: NotAllowedError, read as "cancelled"). So it
    //    is aria-disabled, says it is checking, and a click (dispatched: the
    //    page's own click, which Playwright's would wait out) reaches no
    //    WebAuthn call and sends no /verify. REGRESSION TEST: red at ac6fdfc3,
    //    where the button was live during the wait.
    await expect(passkeyBtn(page)).toHaveAttribute('aria-disabled', 'true');
    await expect(passkeyBtn(page)).toHaveText('Checking for your passkey…');
    await page.evaluate(() => {
      const w = window as unknown as { __gets: number };
      w.__gets = 0;
      const c = navigator.credentials, orig = c.get.bind(c);
      Object.defineProperty(c, 'get', { configurable: true, value: (o?: CredentialRequestOptions) => { w.__gets++; return orig(o); } });
    });
    let earlyVerify = 0;
    const onVerify = (r: { url: () => string }) => { if (new URL(r.url()).pathname === '/auth/confirm/passkey/verify') earlyVerify++; };
    page.on('request', onVerify);
    await passkeyBtn(page).dispatchEvent('click');
    await page.evaluate(() => new Promise((r) => setTimeout(r, 300)));
    expect(await page.evaluate(() => (window as unknown as { __gets: number }).__gets), 'no WebAuthn call while the options are in flight').toBe(0);
    expect(earlyVerify, 'and no /verify').toBe(0);
    await expect(page.locator('#term-confirm-error'), 'and no error said').toBeHidden();
    page.off('request', onVerify);
    // The summary is the way in, answer or not.
    await passwordToggle(page).click();
    await expect(page.locator('#term-confirm-password')).toBeVisible();
    await expect(page.locator('#term-confirm-password'), 'opened, the password takes focus').toBeFocused();
    const answered = page.waitForResponse(isPath('/auth/confirm/passkey/options'));
    releaseOptions();
    expect((await answered).status()).toBe(200);
    await expect(passkeyBtn(page), 'the passkey is still offered').toBeVisible();
    await expect(passkeyBtn(page), 'and live once the options answered').not.toHaveAttribute('aria-disabled', /.*/);
    await expect(passkeyBtn(page)).toHaveText('Confirm with passkey');
    await expect(page.locator('#term-confirm-password'), 'and the password the person opened stays open').toBeVisible();
    await page.keyboard.press('Escape');
    await expect(dialog(page)).toBeHidden();
    await page.unrouteAll({ behavior: 'wait' });

    // 6. A PASSKEY-CAPABLE BROWSER WHOSE /options FAILS (500): the password
    //    opens, as the only option, with no summary, and takes focus.
    await page.route('**/auth/confirm/passkey/options', (r) => r.fulfill({ status: 500, json: { error: 'boom' } }), { times: 1 });
    await page.evaluate(() => { (window as unknown as Run).musterStepUp.ask('', null); });
    await expect(dialog(page)).toBeVisible();
    await expect(dialog(page)).toHaveAttribute('data-passkey-unavailable', 'options-failed:500');
    await expect(page.locator('#term-confirm-password'), 'a failed options call opens the password').toBeVisible();
    await expect(page.locator('#term-confirm-password')).toBeFocused();
    await expect(passwordToggle(page), 'with no summary: it is the only option').toBeHidden();
    await expect(passkeyBtn(page)).toBeHidden();
    await page.keyboard.press('Escape');
    await expect(dialog(page)).toBeHidden();
    await page.unrouteAll({ behavior: 'wait' });
  });
});

// ---------------------------------------------------------------------------
// THE TERMINAL OPENED THROUGH THE RENDERED | TERMINAL SWITCH, on a phone: it
// is a same-origin frame over the rendered view (view_swap.go). The operator's
// report, in the Android app: a passkey was offered on the rendered view, and
// on the terminal opened from it only the password (no options call at all) —
// the frame's dialog judged a passkey unusable there, for a reason that is not
// measured (stepup_confirm.go).
//
// The frame here is made to lack WebAuthn (PublicKeyCredential deleted in
// every subframe, never in the top document): one way to make the frame's
// answer to "can a passkey be used here?" NO, which reproduces the report's
// signature (password only, no options call) on the code before the fix.
//
// 🔴 A REGRESSION TEST: before the framed view handed its ask to the top
// window, the frame opened its OWN dialog, password-only — this fails at the
// top window's dialog being visible.
// ---------------------------------------------------------------------------
test.describe('the terminal in the view switch’s frame, with no WebAuthn in the frame (390×844)', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('a key and a composer send in the framed terminal are confirmed with a PASSKEY in the top window’s dialog (real GoTrue, virtual authenticator)', async ({ page, context }) => {
    test.setTimeout(180_000);
    await authenticator(page);
    await signIn(page);
    await addPasskey(page);
    await page.addInitScript(() => {
      if (window.top !== window) delete (window as { PublicKeyCredential?: unknown }).PublicKeyCredential;
    });
    const options: string[] = [];
    page.on('request', (r) => {
      if (new URL(r.url()).pathname === '/auth/confirm/passkey/options') options.push(r.frame() === page.mainFrame() ? 'top' : 'frame');
    });

    await makeStale(context);
    await openView(page);
    await page.locator('#view-terminal').tap();
    const frameEl = page.locator('iframe[data-muster-view]');
    await expect(frameEl).toBeVisible();
    const t = page.frameLocator('iframe[data-muster-view]');
    await expect(t.locator('#term .xterm-rows')).toContainText('FAKE-MIRROR session=main mode=ro');
    // CONTROL: the frame has no WebAuthn; the top document has it.
    const frame = (await (await frameEl.elementHandle())!.contentFrame())!;
    expect(await frame.evaluate(() => typeof window.PublicKeyCredential), 'no WebAuthn in the frame').toBe('undefined');
    expect(await page.evaluate(() => typeof window.PublicKeyCredential), 'WebAuthn in the top document').toBe('function');

    // 1. A key: stale → asked → in the TOP window's dialog, with the passkey
    //    offered → confirmed → the frame attaches rw.
    await t.locator('#term-keys [data-key="up"]').tap();
    await expect(dialog(page), 'asked in the top window').toBeVisible();
    await expect(t.locator('#term-confirm'), 'not in the frame’s own dialog').toBeHidden();
    await expect(passkeyBtn(page), 'the passkey is offered').toBeVisible();
    await expect(passkeyBtn(page)).toBeFocused();
    await expect(dialog(page), 'offered: no reason recorded').not.toHaveAttribute('data-passkey-unavailable', /.*/);
    await expect(passkeyNote(page), 'and none shown').toBeHidden();
    const box = (await dialog(page).boundingBox())!;
    expect(box.x + box.width, 'the dialog fits 390px').toBeLessThanOrEqual(390);
    await passkeyBtn(page).tap();
    await expect(dialog(page)).toBeHidden();
    await expect(t.locator('#term .xterm-rows')).toContainText('mode=rw');
    await expect(t.locator('#term-status')).toHaveText('Connected');
    let token = await accessToken(context);
    expect(Math.abs(Date.now() / 1000 - amrOf(token)), 'the passkey confirm signed in just now').toBeLessThan(60);
    expect(options, 'the passkey options were asked for by the top window').toEqual(['top']);

    //    Hidden behind the rendered view and shown again, the frame keeps its
    //    one rw attach exactly as it is: no new socket, no dialog.
    const rwLines = async () => ((await t.locator('#term .xterm-rows').innerText()).match(/mode=rw/g) || []).length;
    const rwBefore = await rwLines();
    const termWS: string[] = [];
    page.on('websocket', (ws) => {
      if (ws.url().includes('/mirror/terminal/ws')) termWS.push(ws.url());
    });
    await t.locator('#view-rendered').tap();
    await expect(frameEl).toBeHidden();
    await page.locator('#view-terminal').tap();
    await expect(frameEl).toBeVisible();
    await page.waitForTimeout(1000);
    await expect(t.locator('#term-status')).toHaveText('Connected');
    expect(termWS, 'hiding and showing opened no terminal socket').toEqual([]);
    expect(await rwLines(), 'still the one rw attach: nothing re-attached').toBe(rwBefore);
    await expect(t.locator('#term .xterm-rows')).toContainText('mode=rw');
    await expect(dialog(page)).toBeHidden();

    // 2. The framed terminal's composer: stale → Send → 403 → the top window's
    //    dialog, passkey → the same text is delivered once.
    const sends: number[] = [];
    page.on('response', (r) => {
      if (new URL(r.url()).pathname === '/mirror/terminal/send') sends.push(r.status());
    });
    const before = pasted();
    await makeStale(context);
    await t.locator('#mv-input').fill('from the framed terminal');
    await t.locator('#mv-send').tap();
    await expect(dialog(page)).toBeVisible();
    expect(sends).toEqual([403]);
    await expect(passkeyBtn(page)).toBeVisible();
    await passkeyBtn(page).tap();
    await expect(dialog(page)).toBeHidden();
    await expect.poll(pasted).toBe(before + '%9 from the framed terminal\n');
    await expect(t.locator('#mv-input')).toHaveValue('');
    expect(sends).toEqual([403, 204]);
    expect(options).toEqual(['top', 'top']);
    token = await accessToken(context);
    expect(Math.abs(Date.now() / 1000 - amrOf(token))).toBeLessThan(60);

    // The dialog was made usable (the swap had made it inert with the rest of
    // the page under the frame); closed, it is out of reach again, and the frame
    // is still the view on screen.
    expect(await page.evaluate(() => document.documentElement.getAttribute('data-view'))).toBe('terminal');
    expect(await dialog(page).evaluate((el) => !!el.closest('[inert]')), 'the closed dialog is inert again').toBe(true);
    expect(await page.locator('#mv-log').evaluate((el) => !!el.closest('[inert]')), 'the rendered view under the frame is inert').toBe(true);

    // 3. When the dialog that ASKS (the top window's) has no WebAuthn either,
    //    it says why it offers the password only: on the dialog, and in one
    //    muted line on screen. The password still confirms, and both are gone
    //    once it closes.
    //    (Asked by a send: the frame's terminal still believes its sign-in
    //    fresh — it is, by the page's measure — so a key would just type.)
    await page.evaluate(() => { delete (window as { PublicKeyCredential?: unknown }).PublicKeyCredential; });
    await makeStale(context);
    const before3 = pasted();
    await t.locator('#mv-input').fill('third from the framed terminal');
    await t.locator('#mv-send').tap();
    await expect(dialog(page)).toBeVisible();
    await expect(dialog(page)).toHaveAttribute('data-passkey-unavailable', 'no-publickeycredential');
    await expect(passkeyNote(page)).toBeVisible();
    await expect(passkeyNote(page)).toHaveText('Passkey unavailable here (no-publickeycredential)');
    await expect(passkeyBtn(page)).toBeHidden();
    await expect(page.locator('#term-confirm-password'), 'no usable passkey: the password is shown open').toBeVisible();
    await expect(passwordToggle(page)).toBeHidden();
    expect(options, 'no options asked for where no ceremony can run').toEqual(['top', 'top']);
    await page.fill('#term-confirm-password', adminPW);
    await page.locator('#term-confirm-submit').click();
    await expect(dialog(page)).toBeHidden();
    await expect.poll(pasted).toBe(before3 + '%9 third from the framed terminal\n');
    await expect(dialog(page)).not.toHaveAttribute('data-passkey-unavailable', /.*/);
    await expect(passkeyNote(page)).toBeHidden();
  });
});

// THE STEP-UP WITH NO SCRIPT. A browser context with JavaScript off has no
// confirm dialog: a stale send, submitted by the browser itself, is answered
// with a plain page — the password, and the message that was waiting — and
// confirming there delivers it.
//
// 🔴 A REGRESSION TEST: on the commit before this the composer had no method and
// no action (the browser would GET the page with `?text=…`), and a stale POST
// was a bare 403 text page with nothing to do on it.
test.describe('with no script at all (390×844)', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('a stale send is confirmed on a plain page with the password, and the same text is then delivered once (real GoTrue)', async ({ page, context, browser }) => {
    test.setTimeout(120_000);
    await signIn(page);
    const viewPath = `/mirror/view?host=${encodeURIComponent(host)}&session=main`;
    await expect.poll(async () => (await (await page.request.get(browserURL + '/ui/agents')).text()).includes('data-mirror-claude'), {
      message: `main is a Claude pane (muster-mirror log:\n${mirrorLog})`, timeout: 20_000,
    }).toBe(true);
    const pasted = () => (fs.existsSync(transcript + '.pasted') ? fs.readFileSync(transcript + '.pasted', 'utf8') : '');
    const before = pasted();

    // The signed-in session, made stale, in a context that runs no script.
    await makeStale(context);
    const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, javaScriptEnabled: false });
    await ctx.addCookies(await context.cookies(browserURL));
    const plain = await ctx.newPage();
    try {
      await plain.goto(browserURL + viewPath);
      await plain.locator('#mv-input').fill('typed with no script, before the confirm');
      await plain.locator('#mv-send').click();

      // Not delivered: the plain confirm page, in the send route's place.
      await expect(plain.locator('#stepup-form')).toBeVisible();
      await expect(plain.locator('#stepup-form')).toContainText('from the last 12 hours');
      expect(pasted(), 'nothing is delivered before the confirm').toBe(before);
      const box = (await plain.locator('#stepup-form').boundingBox())!;
      expect(box.x + box.width, 'the confirm page fits 390px').toBeLessThanOrEqual(390);
      for (const id of ['#stepup-password', '#stepup-submit', '#stepup-cancel']) {
        expect((await plain.locator(id).boundingBox())!.height, `${id} is a 44px target`).toBeGreaterThanOrEqual(44);
      }

      // A wrong password: the page again, saying so, and still nothing delivered.
      await plain.locator('#stepup-password').fill('not the password');
      await plain.locator('#stepup-submit').click();
      await expect(plain.locator('#stepup-error')).toHaveText('That password is not right.');
      expect(pasted()).toBe(before);

      // The right one: delivered, and back on the view, which says so.
      await plain.locator('#stepup-password').fill(adminPW);
      await plain.locator('#stepup-submit').click();
      await plain.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('notice') === 'sent');
      await expect(plain.locator('#mv-note')).toHaveText('Sent.');
      expect(pasted(), 'the pending message was delivered once').toBe(before + '%9 typed with no script, before the confirm\n');

      // That confirm signed this browser in afresh: the next send needs none.
      await plain.locator('#mv-input').fill('and the next one');
      await plain.locator('#mv-send').click();
      await plain.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('notice') === 'sent');
      expect(pasted()).toBe(before + '%9 typed with no script, before the confirm\n%9 and the next one\n');
    } finally {
      await ctx.close();
    }
  });
});

// ---------------------------------------------------------------------------
// 🔴 ONLY A KEY THE PERSON PRESSED ASKS "CONFIRM IT'S YOU". A terminal answers
// some of what the far side writes by ITSELF: tmux queries a client's Device
// Attributes and cursor position as it attaches, and with focus reporting on
// the terminal reports focus. xterm.js hands those answers to the same onData
// a keystroke takes. Live keys always on (#299) made every input on a stale
// attach ask to confirm — so the dialog opened with nothing pressed, even in
// a background tab (live 0.95.0). The host here writes those queries on every
// attach (MUSTER_MIRROR_E2E_QUERIES); a stale sign-in must then show NO dialog
// for 3 s after the terminal opens — on its own page and in the view switch's
// frame — and a real key must still open it.
// ---------------------------------------------------------------------------
for (const size of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`a stale terminal that answers the host's queries (${size.name})`, () => {
    test.use(size.use);

    test('no dialog opens until a key is pressed — on the terminal page and in the view switch’s frame (real GoTrue)', async ({ page, context }) => {
      test.setTimeout(180_000);
      // The terminal's own answers, as it gives them (instrument): every
      // onData of the page's xterm, wrapped as the emulator is defined.
      await page.addInitScript(() => {
        const w = window as any;
        w.__onData = [];
        let T: any;
        Object.defineProperty(w, 'Terminal', {
          configurable: true,
          get: () => T,
          set: (v: any) => {
            T = class extends v {
              constructor(o: any) {
                super(o);
                (this as any).onData((d: string) => w.__onData.push(d));
              }
            };
          },
        });
      });
      // Self-sufficient: run alone (a filter, --last-failed), it claims the
      // admin itself.
      if (adminClaimed) await signIn(page);
      else await claimAndSignIn(page);
      if (mirrorProc && mirrorProc.exitCode === null) {
        const done = new Promise((r) => mirrorProc!.once('exit', r));
        mirrorProc.kill('SIGTERM');
        await done;
      }
      mirrorProc = startMirror({ MUSTER_MIRROR_E2E_QUERIES: '1' });
      try {
        // The restarted host has connected and listed its session.
        await expect
          .poll(async () => (await page.request.get(browserURL + termPath)).status(), { message: `the host's session is listed (log:\n${mirrorLog})`, timeout: 20_000 })
          .toBe(200);
        await makeStale(context);

        // 1. The terminal page, opened and left alone.
        await page.goto(browserURL + termPath);
        await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro', { timeout: 20_000 });
        await expect(page.locator('#term-status')).toHaveText("Connected · confirm it's you to type");
        // CONTROL: the terminal DID answer the host by itself.
        await expect.poll(() => page.evaluate(() => (window as any).__onData.length), { message: 'the terminal answered the host’s queries' }).toBeGreaterThan(0);
        const answers = await page.evaluate(() => (window as any).__onData as string[]);
        console.log(`[${test.info().title}] the terminal's own answers on a read-only attach: ${JSON.stringify(answers)}`);
        expect(answers.some((d) => d.startsWith('\x1b[?') || d.startsWith('\x1b[>')), `answers seen: ${JSON.stringify(answers)}`).toBe(true);
        await page.waitForTimeout(3000);
        await expect(dialog(page), 'no dialog with nothing pressed').not.toBeVisible();
        // A tap or click on the terminal focuses it (a focus report, with
        // reporting on) and still asks nothing; a letter typed does.
        await page.locator('#term').click();
        await page.waitForTimeout(500);
        await expect(dialog(page), 'focusing the terminal asks nothing').not.toBeVisible();
        // Text committed through an input method (a phone keyboard's swipe or
        // voice, a CJK IME) is the person's too. xterm sends it from a timer
        // its compositionend queues, after the event — so it must still count.
        const cdp = await page.context().newCDPSession(page);
        await cdp.send('Input.imeSetComposition', { text: 'ls', selectionStart: 2, selectionEnd: 2 });
        await cdp.send('Input.insertText', { text: 'ls' });
        await expect(dialog(page), 'text committed by an input method asks').toBeVisible();
        await page.locator('#term-confirm-cancel').click();
        await expect(dialog(page)).not.toBeVisible();
        await page.keyboard.type('x');
        await expect(dialog(page), 'a letter typed asks').toBeVisible();
        await page.locator('#term-confirm-cancel').click();
        await expect(dialog(page)).not.toBeVisible();

        // 2. The view switch: Rendered, then Terminal in its frame.
        await page.goto(browserURL + viewPath);
        await expect(page.locator('#mv-log [data-bubble="assistant"]').first()).toContainText('Hi.', { timeout: 20_000 });
        await page.locator('#view-terminal').click();
        const t = page.frameLocator('iframe[data-muster-view]');
        await expect(t.locator('#term .xterm-rows')).toContainText('FAKE-MIRROR session=main mode=ro', { timeout: 20_000 });
        await page.waitForTimeout(3000);
        await expect(dialog(page), 'no dialog in the top window').not.toBeVisible();
        await expect(t.locator('#term-confirm'), 'no dialog in the frame').not.toBeVisible();

        // 3. A real key DOES ask (the top window's dialog, for a framed view).
        await t.locator('#term-keys [data-key="enter"]').click();
        await expect(dialog(page), 'a key pressed asks').toBeVisible();
      } finally {
        if (mirrorProc && mirrorProc.exitCode === null) {
          const done = new Promise((r) => mirrorProc!.once('exit', r));
          mirrorProc.kill('SIGTERM');
          await done;
        }
        mirrorProc = startMirror();
      }
    });
  });
}
