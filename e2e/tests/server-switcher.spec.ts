import http from 'node:http';
import type { AddressInfo } from 'node:net';
import zlib from 'node:zlib';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { authState, baseURL, password, port, serverSwitcherStubPort } from '../lib/env';
import { noteFormSignIn } from '../lib/server';

// THE SERVER SWITCHER (web/static/servers.js, internal/ui/server_switcher.go;
// docs/design/web-server-switcher.md), in a real browser against the real
// shared muster, at a phone's width and a desktop's.
//
// The operator's decision on review, "Logo via <img>, no CORS": the page READS
// nothing on another origin (no fetch, no XHR); another server's logo is a
// plain <img> of its /brand/icon-192.png, and its label is the person's own or
// its host.
//
// Three origins:
//   A    — the shared server, as every spec reaches it (localhost).
//   A2   — the SAME server reached as 127.0.0.1: another origin to the
//          browser, so A's CSP and A2's headers are muster's own.
//   STUB — a fake second server (node:http, here) with no CORS header at all,
//          whose logo the spec turns on and off, and which records every
//          request it gets.
//
// 🔴 REGRESSION TEST: on origin/main the sidebar has no #server-switcher and
// img-src is 'self' data: alone. Against this PR's first revision (f191b0f8)
// the "reads nothing" checks are red: it fetched /api/instance and the icon.

const A = baseURL;
const A2 = `http://127.0.0.1:${port}`;
const STUB = `http://127.0.0.1:${serverSwitcherStubPort}`;
const hostOf = (o: string) => new URL(o).host;
const LOGO = '/brand/icon-192.png';

// A w×h one-colour PNG (no binary fixture in the repo).
function png(w: number, h: number, rgb: [number, number, number]): Buffer {
  const crcT = new Uint32Array(256).map((_, n) => { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; return c >>> 0; });
  const crc = (b: Buffer) => { let c = 0xffffffff; for (const x of b) c = crcT[(c ^ x) & 0xff] ^ (c >>> 8); return (c ^ 0xffffffff) >>> 0; };
  const chunk = (t: string, d: Buffer) => {
    const len = Buffer.alloc(4); len.writeUInt32BE(d.length);
    const td = Buffer.concat([Buffer.from(t, 'ascii'), d]);
    const c = Buffer.alloc(4); c.writeUInt32BE(crc(td));
    return Buffer.concat([len, td, c]);
  };
  const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(w, 0); ihdr.writeUInt32BE(h, 4); ihdr[8] = 8; ihdr[9] = 2;
  const raw = Buffer.alloc(h * (1 + 3 * w));
  for (let y = 0; y < h; y++) for (let x = 0; x < w; x++) raw.set(rgb, y * (1 + 3 * w) + 1 + 3 * x);
  return Buffer.concat([Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}

interface Seen { method: string; url: string; headers: http.IncomingHttpHeaders }
const stub = {
  logo: true,
  seen: [] as Seen[],
  server: undefined as http.Server | undefined,
};
const icon = png(32, 32, [200, 40, 40]);

test.beforeAll(async () => {
  // 🔴 NO Access-Control-Allow-Origin anywhere: a script on A could not read
  // a byte of this server; only an <img> can draw its icon.
  stub.server = http.createServer((req, res) => {
    stub.seen.push({ method: req.method ?? '', url: req.url ?? '', headers: req.headers });
    const u = new URL(req.url ?? '/', STUB);
    if (u.pathname === LOGO && stub.logo) {
      res.writeHead(200, { 'Content-Type': 'image/png', 'Cache-Control': 'no-store' });
      res.end(icon);
      return;
    }
    if (u.pathname === '/') {
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
      res.end('<!doctype html><title>stub</title><h1 id="stub-home">the other server</h1>');
      return;
    }
    res.writeHead(404, { 'Cache-Control': 'no-store' });
    res.end();
  });
  await new Promise<void>((resolve) => stub.server!.listen(serverSwitcherStubPort, '127.0.0.1', resolve));
  expect((stub.server.address() as AddressInfo).port).toBe(serverSwitcherStubPort);
});

test.afterAll(async () => {
  await new Promise<void>((resolve) => (stub.server ? stub.server.close(() => resolve()) : resolve()));
});

const stored = (page: Page) => page.evaluate(() => JSON.parse(localStorage.getItem('muster-servers') || 'null'));

// fresh: A's /tasks with an empty list.
async function fresh(page: Page, origin = A) {
  await page.goto(origin + '/tasks');
  await page.evaluate(() => localStorage.removeItem('muster-servers'));
  await page.reload();
  await expect(page.locator('#server-switcher')).toHaveCount(1);
}

// openSwitcher: the sidebar on screen (the menu button on a phone), then the
// server row — its menu shown.
async function openSwitcher(page: Page) {
  const toggle = page.locator('#server-switcher-toggle');
  if (!(await toggle.isVisible())) {
    await page.locator('#sidebar-open').click();
    await expect(page.locator('#sidebar-root')).toHaveAttribute('data-open', 'true');
  }
  await expect(toggle).toBeVisible();
  if ((await toggle.getAttribute('aria-expanded')) !== 'true') await toggle.click();
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  await expect(page.locator('#server-switcher-menu')).toBeVisible();
}

const items = (page: Page) => page.locator('#server-switcher-list > li');
const item = (page: Page, origin: string) => page.locator(`#server-switcher-list a[data-server-origin="${origin}"]`);

async function addServer(page: Page, address: string) {
  await page.locator('#server-add-open').click();
  await expect(page.locator('#server-add')).toHaveJSProperty('open', true);
  await expect(page.locator('#server-add-input')).toBeFocused();
  await page.locator('#server-add-input').fill(address);
  await page.locator('#server-add-input').press('Enter');
}

// watch records every request the page makes, by kind: fetch/XHR to another
// origin (must stay 0), fetch/XHR to its own (the positive control: the
// listener sees fetches), and images from another origin (the control that it
// sees cross-origin requests at all).
function watch(page: Page) {
  const seen = { foreignReads: [] as string[], ownReads: 0, foreignImages: [] as string[] };
  page.on('request', (r) => {
    const t = r.resourceType();
    const own = new URL(r.url()).origin === new URL(page.url()).origin;
    if (t === 'fetch' || t === 'xhr') {
      if (own) seen.ownReads++;
      else seen.foreignReads.push(r.url());
    } else if (t === 'image' && !own && !r.url().startsWith('data:')) {
      seen.foreignImages.push(r.url());
    }
  });
  return seen;
}

for (const vp of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 } } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('shows the server in view; adds a server, refusing bad addresses; draws its logo with no read of it; renames; removes', async ({ page }) => {
      const cspErrors: string[] = [];
      page.on('console', (m) => { if (/Content Security Policy/i.test(m.text())) cspErrors.push(m.text()); });
      stub.logo = true;
      stub.seen = [];
      await fresh(page);
      const seen = watch(page);

      // The server in view: one entry, current, labelled by its own name
      // (server-rendered, same origin) or its host.
      const self = await (await page.request.get(A + '/api/instance')).json();
      await expect(page.locator('#server-switcher-toggle [data-server-label]')).toHaveText(self.name ?? hostOf(A));
      await openSwitcher(page);
      await expect(items(page)).toHaveCount(1);
      await expect(item(page, A)).toHaveAttribute('aria-current', 'true');
      await expect(item(page, A).locator('[data-server-current]')).toBeVisible();

      // Bad addresses are refused, saying why; the field is marked invalid.
      await addServer(page, 'http://b.example.net');
      await expect(page.locator('#server-add-error')).toHaveText(/must start with https:\/\//);
      await expect(page.locator('#server-add-input')).toHaveAttribute('aria-invalid', 'true');
      await page.locator('#server-add-input').fill('https://user:pw@b.example.net');
      await page.locator('#server-add-submit').click();
      await expect(page.locator('#server-add-error')).toHaveText(/user name and password/);
      await expect(page.locator('#server-add')).toHaveJSProperty('open', true);

      // The stub, pasted with a path and a token in its query: kept as its origin.
      await page.locator('#server-add-input').fill(STUB + '/tasks?token=mst_synthetic_not_a_token');
      await page.locator('#server-add-submit').click();
      await expect(page.locator('#server-add')).toHaveJSProperty('open', false);
      await expect(page.locator('#server-switcher-toggle')).toBeFocused();
      await expect(items(page)).toHaveCount(2);
      const s = item(page, STUB);
      // Its label is its host; its logo is drawn (a real decode, 32 px) from
      // its own icon path, with no CORS header anywhere on the stub.
      await expect(s.locator('[data-server-label]')).toHaveText(hostOf(STUB));
      await expect(s.locator('[data-server-host]')).toHaveText(hostOf(STUB));
      await expect(s.locator('[data-server-logo]')).toBeVisible();
      expect(await s.locator('[data-server-logo]').evaluate((i) => [(i as HTMLImageElement).src, (i as HTMLImageElement).naturalWidth, (i as HTMLImageElement).crossOrigin, (i as HTMLImageElement).referrerPolicy]))
        .toEqual([STUB + LOGO, 32, null, 'no-referrer']);
      expect(await s.locator('[data-server-dot]').evaluate((d) => (d as HTMLElement).style.backgroundColor)).toMatch(/^rgb\(/);
      expect(await s.getAttribute('href')).toBe(`${STUB}/#muster-from=${encodeURIComponent(A)}`);

      // 🔴 What reached the other server: its icon, as an IMAGE (no-cors), with
      // no cookie, no authorization, no referrer, no Origin — and nothing else.
      expect(new Set(stub.seen.map((r) => r.url))).toEqual(new Set([LOGO]));
      for (const r of stub.seen) {
        expect(r.headers['sec-fetch-dest'], r.url).toBe('image');
        expect(r.headers['sec-fetch-mode'], r.url).toBe('no-cors');
        expect(r.headers.cookie, r.url).toBeUndefined();
        expect(r.headers.authorization, r.url).toBeUndefined();
        expect(r.headers.referer, r.url).toBeUndefined();
        expect(r.headers.origin, r.url).toBeUndefined();
      }
      const kept = JSON.stringify(await stored(page));
      expect(kept).not.toContain('mst_');
      expect(kept).not.toContain('/tasks');

      // 🔴 The page read NOTHING on another origin: zero cross-origin
      // fetch/XHR — next to a same-origin read and a cross-origin image, which
      // prove the listener sees both kinds.
      await page.evaluate(() => fetch('/api/instance').then((r) => r.text()));
      expect(seen.foreignReads).toEqual([]);
      expect(seen.ownReads).toBeGreaterThan(0);
      expect(seen.foreignImages).toContain(STUB + LOGO);

      // With the menu shut, a page load asks the other server for nothing.
      stub.seen = [];
      await page.reload();
      await expect(page.locator('#server-switcher-toggle')).toHaveAttribute('aria-expanded', 'false');
      await page.waitForTimeout(300);
      expect(stub.seen).toEqual([]);

      // A server with no logo (404): the colour and the label, no broken image.
      stub.logo = false;
      await openSwitcher(page);
      await expect.poll(() => stub.seen.map((r) => r.url)).toContain(LOGO);
      await expect(s.locator('[data-server-logo]')).toBeHidden();
      await expect(s.locator('[data-server-dot]')).toBeVisible();
      await expect(s.locator('[data-server-label]')).toHaveText(hostOf(STUB));

      // The same server again: refused as a duplicate.
      await addServer(page, 'http://' + hostOf(STUB) + '/');
      await expect(page.locator('#server-add-error')).toHaveText(/already on the list/);
      await page.locator('#server-add [data-dialog-close]').click();

      // Rename, then clear the label: the host shows again.
      await page.locator('#server-manage-open').click();
      const manage = page.locator('#server-manage');
      await expect(manage).toHaveJSProperty('open', true);
      const row = manage.locator(`li[data-server-origin="${STUB}"]`);
      const selfRow = manage.locator(`li[data-server-origin="${A}"]`);
      await expect(selfRow.locator('[data-server-remove]')).toBeHidden();
      await row.locator('input').fill('Work');
      await row.locator('input').press('Enter');
      await expect(s.locator('[data-server-label]')).toHaveText('Work');
      // The keyboard stays in the row just renamed (the rows are redrawn).
      expect(await page.evaluate(() => document.activeElement?.closest('[data-server-origin]')?.getAttribute('data-server-origin'))).toBe(STUB);
      expect(await page.evaluate(() => document.activeElement?.tagName)).toBe('INPUT');
      await row.locator('input').fill('');
      await row.locator('input').press('Enter');
      await expect(s.locator('[data-server-label]')).toHaveText(hostOf(STUB));

      // Remove it: the list is this server alone again.
      await row.locator('[data-server-remove]').click();
      await expect(manage.locator('li')).toHaveCount(1);
      await manage.locator('[data-dialog-close]').click();
      await expect(manage).toHaveJSProperty('open', false);
      await expect(items(page)).toHaveCount(1);
      expect((await stored(page)).servers.map((x: { origin: string }) => x.origin)).toEqual([A]);

      // The keyboard: Escape closes the menu and gives focus back to its button.
      await page.locator('#server-switcher-toggle').focus();
      await page.keyboard.press('Escape');
      await expect(page.locator('#server-switcher-menu')).toBeHidden();
      await expect(page.locator('#server-switcher-toggle')).toHaveAttribute('aria-expanded', 'false');
      await expect(page.locator('#server-switcher-toggle')).toBeFocused();
      await page.keyboard.press('Enter');
      await expect(page.locator('#server-switcher-menu')).toBeVisible();

      // An attempted read the CSP stops is not a request event at all (the
      // mutation sweep measured it): it shows as a connect-src violation, so
      // "no read" is both lists empty.
      expect(seen.foreignReads).toEqual([]);
      expect(cspErrors).toEqual([]);
    });
  });
}

test.describe('another muster (desktop)', () => {
  test.use({ viewport: { width: 1280, height: 800 } });

  test('another muster’s data cannot be read from this page; only its icon path can be drawn', async ({ page }) => {
    await fresh(page);
    // A2's instance document and icon carry no CORS header.
    for (const p of ['/api/instance', LOGO]) {
      const r = await page.request.get(A2 + p);
      expect(r.headers()['access-control-allow-origin'], p).toBeUndefined();
    }
    // A's CSP refuses any read of another origin, /api/instance included.
    const read = (u: string) => page.evaluate(async (url) => {
      const seen: string[] = [];
      const h = (e: SecurityPolicyViolationEvent) => seen.push(e.violatedDirective);
      document.addEventListener('securitypolicyviolation', h);
      let out = 'fetched';
      try { await fetch(url, { credentials: 'omit', mode: 'cors' }); } catch { out = 'threw'; }
      await new Promise((r) => setTimeout(r, 50));
      document.removeEventListener('securitypolicyviolation', h);
      return `${out} ${seen.join(',')}`;
    }, u);
    expect(await read(A2 + '/api/instance')).toBe('threw connect-src');
    expect(await read(A2 + LOGO)).toBe('threw connect-src');
    // Drawing: the icon path loads past the CSP (the shared server has no logo
    // set, so it answers 404 — an error event, not a violation); any other
    // path on another origin is refused by img-src.
    const draw = (u: string) => page.evaluate(async (url) => {
      const seen: string[] = [];
      const h = (e: SecurityPolicyViolationEvent) => seen.push(e.violatedDirective);
      document.addEventListener('securitypolicyviolation', h);
      const img = new Image();
      img.referrerPolicy = 'no-referrer';
      const out = await new Promise<string>((res) => { img.onload = () => res('load'); img.onerror = () => res('error'); img.src = url; });
      await new Promise((r) => setTimeout(r, 50));
      document.removeEventListener('securitypolicyviolation', h);
      return `${out} ${seen.join(',')}`.trim();
    }, u);
    // Each A2 request's outcome: the server's status, or the browser's reason
    // for failing it. Playwright emits a request event even for one the CSP
    // stops, so the outcome — not the event — tells "sent" from "refused".
    const outcome: Record<string, string> = {};
    const key = (u: string) => new URL(u).pathname;
    page.on('response', (r) => { if (r.url().startsWith(A2 + '/')) outcome[key(r.url())] = String(r.status()); });
    page.on('requestfailed', (r) => { if (r.url().startsWith(A2 + '/')) outcome[key(r.url())] ??= r.failure()?.errorText ?? 'failed'; });
    expect(await draw(A2 + LOGO)).toBe('error');
    expect(await draw(A2 + '/brand/logo.png')).toBe('error img-src');
    expect(await draw(A2 + '/brand/icon-192.png.evil')).toBe('error img-src');
    await expect.poll(() => Object.keys(outcome).length).toBe(3);
    expect(outcome['/brand/logo.png']).toBe('csp');
    expect(outcome['/brand/icon-192.png.evil']).toBe('csp');
    // The icon path went out past the CSP (the shared server has no logo: a
    // 404, which the browser may also refuse to hand an <img> — but not by CSP).
    expect(outcome[LOGO]).not.toBe('csp');
  });

  test('switching opens the other server; it offers to add the server you came from', async ({ page }) => {
    // A session on A2 (the same server under another host: its own cookies).
    await signIn(page, A2);
    await fresh(page, A2);
    await fresh(page, A);
    await openSwitcher(page);
    await addServer(page, A2);
    await expect(page.locator('#server-add')).toHaveJSProperty('open', false);
    await expect(item(page, A2)).toHaveCount(1);
    await expect(item(page, A2).locator('[data-server-label]')).toHaveText(hostOf(A2));

    const nav = page.waitForRequest((r) => r.isNavigationRequest() && r.url().startsWith(A2 + '/'));
    await item(page, A2).click();
    const req = await nav;
    // The navigation carries no Referer and no query: the fragment is not sent.
    expect(req.headers()['referer']).toBeUndefined();
    expect(new URL(req.url()).search).toBe('');
    await page.waitForURL((u) => u.origin === A2 && !u.pathname.startsWith('/login'));

    const offer = page.locator('#server-offer');
    await expect(offer).toHaveJSProperty('open', true);
    await expect(offer.locator('[data-server-origin-text]')).toHaveText(A);
    await expect(page.locator('#server-offer-dismiss')).toBeFocused();
    expect(await page.evaluate(() => location.hash)).toBe('');
    await offer.locator('#server-offer-add').click();
    await expect(offer).toHaveJSProperty('open', false);
    expect((await stored(page)).servers.map((x: { origin: string }) => x.origin)).toEqual([A2, A]);

    // Back on A, the other way: no offer, A2 already knows it.
    await openSwitcher(page);
    await item(page, A).click();
    await page.waitForURL((u) => u.origin === A);
    await expect(page.locator('#server-switcher')).toHaveCount(1);
    await expect(page.locator('#server-offer')).toHaveJSProperty('open', false);
  });

  test('a crafted hand-off: neutral copy, the full origin, focus on Not now, and nothing added without a click', async ({ page }) => {
    await fresh(page);
    const crafted = 'https://b.example.net:8443';
    await page.goto('about:blank');
    await page.goto(A + '/tasks#muster-from=' + encodeURIComponent(crafted));
    const offer = page.locator('#server-offer');
    await expect(offer).toHaveJSProperty('open', true);
    await expect(offer.locator('#server-offer-title')).toHaveText('Add a server to your list?');
    await expect(offer.locator('[data-server-origin-text]')).toHaveText(crafted);
    await expect(offer).toContainText('A link asked');
    expect((await offer.innerText()).toLowerCase()).not.toMatch(/came from|switched here|switch back/);
    await expect(page.locator('#server-offer-dismiss')).toBeFocused();
    expect(await page.evaluate(() => location.hash)).toBe('');
    // Nothing is stored while it is open, and a stray Enter dismisses it.
    expect(await stored(page)).toBeNull();
    await page.keyboard.press('Enter');
    await expect(offer).toHaveJSProperty('open', false);
    expect(await stored(page)).toBeNull();
    await openSwitcher(page);
    await expect(items(page)).toHaveCount(1);
  });

  test('switching to a server you are not signed in to: the offer survives the sign-in', async ({ browser }) => {
    // A context signed in to A only (the suite's saved state is localhost's).
    const ctx = await browser.newContext({ storageState: authState, viewport: { width: 1280, height: 800 } });
    const page = await ctx.newPage();
    try {
      await fresh(page);
      await openSwitcher(page);
      await addServer(page, A2);
      await expect(item(page, A2)).toHaveCount(1);
      await item(page, A2).click();
      // Not signed in there: the sign-in page, the fragment still on it.
      await page.waitForURL((u) => u.origin === A2 && u.pathname.startsWith('/login'));
      expect(await page.evaluate(() => location.hash)).toBe('#muster-from=' + encodeURIComponent(A));
      await page.fill('#muster-password', password);
      await Promise.all([page.waitForURL((u) => u.origin === A2 && !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
      noteFormSignIn(A2);
      // The fragment is gone after the sign-in; the offer is not.
      const offer = page.locator('#server-offer');
      await expect(offer).toHaveJSProperty('open', true);
      await expect(offer.locator('[data-server-origin-text]')).toHaveText(A);
      expect(await page.evaluate((k) => sessionStorage.getItem(k), 'muster-from')).toBeNull();
      await offer.locator('#server-offer-add').click();
      expect((await stored(page)).servers.map((x: { origin: string }) => x.origin)).toEqual([A2, A]);
      // Read once: the next page offers nothing.
      await page.reload();
      await expect(page.locator('#server-switcher')).toHaveCount(1);
      await expect(page.locator('#server-offer')).toHaveJSProperty('open', false);
    } finally {
      await ctx.close();
    }
  });

  test('a hand-off naming an address that is not a server is not offered', async ({ page }) => {
    await fresh(page);
    // A full load (from another page: a hash-only change would not reload).
    await page.goto('about:blank');
    await page.goto(A + '/tasks#muster-from=' + encodeURIComponent('http://b.example.net'));
    await expect(page.locator('#server-switcher')).toHaveCount(1);
    await expect.poll(() => page.evaluate(() => location.hash)).toBe('');
    await expect(page.locator('#server-offer')).toHaveJSProperty('open', false);
  });
});
