import { execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import path from 'node:path';
import zlib from 'node:zlib';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { startServer, Running } from '../lib/server';
import { landedHome } from '../lib/nav';
import {
  brandingBrowserPort, brandingGotruePort, brandingMainPort, brandingPort, dsn, e2eRoot, gotrueSecret, repoRoot, versionA,
} from '../lib/env';

// INSTANCE BRANDING (internal/api/branding.go; docs/branding.md), in a real
// browser against a real muster-server.
//
// 1. The ADMIN (password mode: the operator, user 1) uploads a logo and a name
//    on Account → Instance branding. The header draws the logo (and it LOADS:
//    naturalWidth > 0) and the name; the shell's favicon and the manifest's
//    icons are the logo's; the sign-in page, opened with no session, draws the
//    logo and name; GET /api/instance names both. An SVG upload is refused
//    and the page says why. Reset logo, then an empty name, restore every
//    default: the wordmark, muster's favicon and its manifest icons.
// 2. A MEMBER (gotrue mode, the real GoTrue) has no branding section on their
//    Account page, and the write route refuses them.
//
// 🔴 REGRESSION TEST: on origin/main there is no branding section, no
// /api/instance and no /brand/ route. The branding is the SERVER's row in the
// leg's shared database, so it is cleared before and after (pluginfix branding
// clear) and every other spec sees nothing set; this spec's servers are its
// own, so no other server cached the logo.

const origin = `http://localhost:${brandingPort}`;
let server: Running | undefined;

const clearBranding = () =>
  execFileSync('go', ['run', './e2e/pluginfix', 'branding', 'clear'], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } });

// A w×h RGBA PNG whose pixels are a gradient, built here so the spec carries no
// binary fixture.
const crcTable = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();
function crc32(b: Buffer): number {
  let c = 0xffffffff;
  for (const x of b) c = crcTable[(c ^ x) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}
function chunk(type: string, data: Buffer): Buffer {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const td = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(td));
  return Buffer.concat([len, td, crc]);
}
function makePNG(w: number, h: number): Buffer {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(w, 0);
  ihdr.writeUInt32BE(h, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 6; // RGBA
  const raw = Buffer.alloc(h * (1 + 4 * w));
  for (let y = 0; y < h; y++) {
    const row = y * (1 + 4 * w);
    for (let x = 0; x < w; x++) {
      const p = row + 1 + 4 * x;
      raw[p] = (x * 5) & 0xff;
      raw[p + 1] = (y * 9) & 0xff;
      raw[p + 2] = 160;
      raw[p + 3] = 255;
    }
  }
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', zlib.deflateSync(raw)),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

// loaded: the <img> really decoded an image (not a broken-image box).
const loaded = (page: Page, sel: string) =>
  page.locator(sel).first().evaluate((el) => (el as HTMLImageElement).complete && (el as HTMLImageElement).naturalWidth > 0);

const iconHref = (page: Page) => page.locator('link[rel="icon"]').getAttribute('href');

test.describe('the admin', () => {
  test.use({ viewport: { width: 1280, height: 800 } });

  test.beforeAll(async () => {
    clearBranding();
    server = await startServer(versionA, brandingPort, { MUSTER_PUBLIC_URL: origin });
  });

  test.afterAll(async () => {
    await server?.stop();
    clearBranding();
  });

  test('uploads a logo and a name: header, favicon, manifest and sign-in page show them; reset restores the defaults', async ({ page, browser }) => {
    test.setTimeout(90_000);
    await signIn(page, origin);

    // Nothing set: the wordmark, muster's icons.
    await page.goto(origin + '/tasks');
    await expect(page.locator('header [data-brand-logo]')).toHaveCount(0);
    expect(await iconHref(page)).toContain('/static/icons/icon-192.png');

    await page.goto(origin + '/account');
    const section = page.locator('[data-testid="branding"]');
    await expect(section).toBeVisible();
    await expect(section.locator('[data-testid="branding-default"]')).toBeVisible();

    // An SVG, named and typed as a PNG, is refused by its bytes, and the page says why.
    await page.setInputFiles('#branding-logo', {
      name: 'logo.png', mimeType: 'image/png',
      buffer: Buffer.from('<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>'),
    });
    await Promise.all([page.waitForURL(origin + '/account?branding_error=svg_refused#branding'), page.click('#branding-save')]);
    await expect(section.getByText('SVG is not accepted')).toBeVisible();

    // The real upload.
    await page.fill('#branding-name', 'Team Example');
    await page.setInputFiles('#branding-logo', { name: 'logo.png', mimeType: 'image/png', buffer: makePNG(300, 120) });
    await Promise.all([page.waitForURL(origin + '/account?branding=saved#branding'), page.click('#branding-save')]);
    await expect(section.getByText('Branding saved.')).toBeVisible();
    await expect(section.locator('[data-testid="branding-preview"] img')).toHaveCount(2);
    expect(await loaded(page, '[data-testid="branding-preview"] img')).toBe(true);

    const instance = await (await page.request.get(origin + '/api/instance')).json();
    expect(instance.name).toBe('Team Example');
    const etag: string = instance.logo.etag;
    expect(etag).toMatch(/^[0-9a-f]{16}$/);
    expect(instance.logo.url).toBe('/brand/logo.png?v=' + etag);
    expect(instance.logo.icon192).toBe('/brand/icon-192.png?v=' + etag);
    expect(instance.logo.icon512).toBe('/brand/icon-512.png?v=' + etag);

    // The header (and the sidebar) draw the logo — loaded — and the name.
    await page.goto(origin + '/tasks');
    const logo = page.locator('header [data-brand-logo]').first();
    await expect(logo).toBeVisible();
    expect(await logo.getAttribute('src')).toBe('/brand/logo.png?v=' + etag);
    expect(await loaded(page, 'header [data-brand-logo]')).toBe(true);
    await expect(page.locator('header [data-brand-name]').first()).toHaveText('Team Example');
    await expect(page.locator('#sidebar [data-brand-logo]')).toHaveCount(1);
    // The favicon and touch icon are the logo's.
    expect(await iconHref(page)).toBe('/brand/icon-192.png?v=' + etag);
    expect(await page.locator('link[rel="apple-touch-icon"]').getAttribute('href')).toBe('/brand/icon-192.png?v=' + etag);
    // The manifest's icons are the logo's.
    const manifest = await (await page.request.get(origin + '/manifest.webmanifest')).json();
    expect(manifest.icons.map((i: { src: string }) => i.src)).toEqual(['/brand/icon-192.png?v=' + etag, '/brand/icon-512.png?v=' + etag]);
    const icon = await page.request.get(origin + '/brand/icon-512.png?v=' + etag);
    expect(icon.status()).toBe(200);
    expect(icon.headers()['content-type']).toBe('image/png');
    expect(icon.headers()['cache-control']).toBe('public, max-age=2592000, immutable');
    // An unversioned URL must be revalidated, so a CDN never serves an old logo.
    const bare = await page.request.get(origin + '/brand/icon-512.png');
    expect(bare.status()).toBe(200);
    expect(bare.headers()['cache-control']).toBe('no-cache');

    // The sign-in page, with no session at all, draws the logo and the name.
    const anon = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    const lp = await anon.newPage();
    await lp.goto(origin + '/login');
    await expect(lp.locator('[data-testid="login-brand"] [data-brand-name]')).toHaveText('Team Example');
    expect(await loaded(lp, '[data-testid="login-brand"] [data-brand-logo]')).toBe(true);
    await anon.close();

    // Reset the logo: the name stays, the icons go back to muster's.
    await page.goto(origin + '/account');
    await Promise.all([page.waitForURL(origin + '/account?branding=reset#branding'), page.click('#branding-reset')]);
    await expect(section.locator('[data-testid="branding-default"]')).toBeVisible();
    await page.goto(origin + '/tasks');
    await expect(page.locator('header [data-brand-logo]')).toHaveCount(0);
    await expect(page.locator('header [data-brand-name]').first()).toHaveText('Team Example');
    expect(await iconHref(page)).toContain('/static/icons/icon-192.png');
    expect((await page.request.get(origin + '/brand/logo.png')).status()).toBe(404);

    // Clear the name: the wordmark is back, and the manifest is muster's own.
    await page.goto(origin + '/account');
    await page.fill('#branding-name', '');
    await Promise.all([page.waitForURL(origin + '/account?branding=saved#branding'), page.click('#branding-save')]);
    await page.goto(origin + '/tasks');
    await expect(page.locator('header [data-brand-name]')).toHaveCount(0);
    await expect(page.locator('header a[aria-label="muster home"]').first()).toHaveText('muster');
    const after = await (await page.request.get(origin + '/manifest.webmanifest')).json();
    expect(after.icons.map((i: { src: string }) => i.src)).toContain('/static/icons/maskable-512.png');
    expect(await (await page.request.get(origin + '/api/instance')).json()).toEqual({ name: null, logo: null });
  });
});

// A member against the real GoTrue: its own compose project and ports, so it
// shares neither with gotrue-login, byo-credentials or invites.
test.describe('a member', () => {
  const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
  const project = `muster-e2e-branding-${brandingGotruePort}`;
  const browserURL = `http://localhost:${brandingBrowserPort}`;
  const gotrueURL = `http://127.0.0.1:${brandingGotruePort}`;
  const composeEnv = {
    ...process.env,
    MUSTER_E2E_GOTRUE_PROJECT: project,
    MUSTER_E2E_GOTRUE_PORT: String(brandingGotruePort),
    MUSTER_E2E_GOTRUE_SITE: browserURL,
  };
  let gtServer: Running | undefined;

  test.use({ viewport: { width: 412, height: 915 }, storageState: { cookies: [], origins: [] } });

  test.beforeAll(async () => {
    test.setTimeout(240_000);
    execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
    gtServer = await startServer(versionA, brandingMainPort, {
      MUSTER_AUTH_MODE: 'gotrue',
      MUSTER_GOTRUE_URL: gotrueURL,
      MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
      MUSTER_BROWSER_PORT: String(brandingBrowserPort),
      MUSTER_PUBLIC_URL: browserURL,
    });
  });

  test.afterAll(async () => {
    await gtServer?.stop();
    execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
  });

  // A 60 s service_role token for GoTrue's admin API (byo-credentials.spec.ts).
  function serviceRoleJWT(): string {
    const now = Math.floor(Date.now() / 1000);
    const b64 = (v: string) => Buffer.from(v).toString('base64url');
    const head = b64(JSON.stringify({ alg: 'HS256', typ: 'JWT' }));
    const body = b64(JSON.stringify({ role: 'service_role', aud: 'authenticated', iss: 'muster-auth', iat: now, exp: now + 60 }));
    const sig = crypto.createHmac('sha256', gotrueSecret).update(`${head}.${body}`).digest('base64url');
    return `${head}.${body}.${sig}`;
  }

  test('sees no branding section, and the write route refuses them', async ({ page }) => {
    test.setTimeout(90_000);
    const email = `branding-member-${Date.now()}@example.com`;
    const password = 'e2e-member-password-not-a-real-one';
    const r = await fetch(gotrueURL + '/admin/users', {
      method: 'POST',
      headers: { Authorization: `Bearer ${serviceRoleJWT()}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ email, password, email_confirm: true }),
    });
    const created = (await r.json()) as { id?: string };
    expect(r.status, JSON.stringify(created)).toBeLessThan(300);
    execFileSync('go', ['run', './e2e/mkmember'], {
      cwd: repoRoot,
      env: { ...process.env, DATABASE_URL: dsn, MUSTER_E2E_SUB: created.id!, MUSTER_E2E_EMAIL: email },
    });

    await page.goto(browserURL + '/login');
    await page.fill('#muster-email', email);
    await page.fill('#muster-password', password);
    await Promise.all([page.waitForURL(landedHome(browserURL)), page.click('button[type=submit]')]);

    await page.goto(browserURL + '/account');
    // The control: this IS the member's Account page.
    await expect(page.getByRole('heading', { name: 'Account', exact: true })).toBeVisible();
    await expect(page.locator('[data-testid="branding"]')).toHaveCount(0);
    await expect(page.getByText('Instance branding')).toHaveCount(0);

    // The route refuses the member's own session from muster's own origin.
    const put = await page.request.fetch(browserURL + '/api/instance/branding', {
      method: 'PUT',
      headers: { Origin: browserURL },
      multipart: { name: 'Hijacked' },
    });
    expect(put.status()).toBe(403);
    expect((await (await page.request.get(browserURL + '/api/instance')).json()).name).toBeNull();
  });
});
