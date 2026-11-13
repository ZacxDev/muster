import http from 'node:http';
import type { AddressInfo } from 'node:net';
import zlib from 'node:zlib';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { baseURL, port, serverSwitcherStubPort } from '../lib/env';

// THE SERVER SWITCHER (web/static/servers.js, internal/ui/server_switcher.go;
// docs/design/web-server-switcher.md), in a real browser against the real
// shared muster, at a phone's width and a desktop's.
//
// Three origins:
//   A    — the shared server, as every spec reaches it (localhost).
//   A2   — the SAME server reached as 127.0.0.1: another origin to the
//          browser, so a read of its /api/instance from A is a real
//          cross-origin read through muster's own CORS header and CSP.
//   STUB — a fake second server (node:http, here) whose name, logo and etag
//          the spec sets, and which records every request it gets.
//
// 🔴 REGRESSION TEST: on origin/main the sidebar has no #server-switcher, the
// instance document has no Access-Control-Allow-Origin and the CSP's
// connect-src is 'self' alone.

const A = baseURL;
const A2 = `http://127.0.0.1:${port}`;
const STUB = `http://127.0.0.1:${serverSwitcherStubPort}`;
const hostOf = (o: string) => new URL(o).host;

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
  name: 'Team Stub' as string | null,
  etag: 'e1' as string | null,
  seen: [] as Seen[],
  server: undefined as http.Server | undefined,
};
const icons: Record<string, Buffer> = { e1: png(32, 32, [200, 40, 40]), e2: png(32, 32, [40, 40, 200]) };

test.beforeAll(async () => {
  stub.server = http.createServer((req, res) => {
    stub.seen.push({ method: req.method ?? '', url: req.url ?? '', headers: req.headers });
    const u = new URL(req.url ?? '/', STUB);
    const cors = { 'Access-Control-Allow-Origin': '*', 'Cache-Control': 'no-store' };
    if (u.pathname === '/api/instance') {
      const e = stub.etag;
      res.writeHead(200, { ...cors, 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ name: stub.name, logo: e ? { url: `/brand/logo.png?v=${e}`, icon192: `/brand/icon-192.png?v=${e}`, icon512: `/brand/icon-512.png?v=${e}`, etag: e } : null }));
      return;
    }
    if (u.pathname === '/brand/icon-192.png' && icons[u.searchParams.get('v') ?? '']) {
      res.writeHead(200, { ...cors, 'Content-Type': 'image/png' });
      res.end(icons[u.searchParams.get('v')!]);
      return;
    }
    if (u.pathname === '/') {
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
      res.end('<!doctype html><title>stub</title><h1 id="stub-home">the other server</h1>');
      return;
    }
    res.writeHead(404, cors);
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

for (const vp of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 } } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('shows the server in view; adds a server, refusing bad addresses; reads its name and logo; renames; removes', async ({ page }) => {
      const consoleErrors: string[] = [];
      page.on('console', (m) => { if (m.type() === 'error') consoleErrors.push(m.text()); });
      stub.name = 'Team Stub';
      stub.etag = 'e1';
      stub.seen = [];
      await fresh(page);

      // The server in view: one entry, current, labelled by its name or host.
      const self = await (await page.request.get(A + '/api/instance')).json();
      const selfLabel = self.name ?? hostOf(A);
      await expect(page.locator('#server-switcher-toggle [data-server-label]')).toHaveText(selfLabel);
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
      // Its name and logo, read from its own /api/instance and icon.
      await expect(s.locator('[data-server-label]')).toHaveText('Team Stub');
      await expect(s.locator('[data-server-host]')).toHaveText(hostOf(STUB));
      await expect(s.locator('[data-server-logo]')).toBeVisible();
      expect(await s.locator('[data-server-logo]').evaluate((i) => (i as HTMLImageElement).src.startsWith('data:image/png;base64,') && (i as HTMLImageElement).naturalWidth === 32)).toBe(true);
      // Its colour is painted.
      expect(await s.locator('[data-server-dot]').evaluate((d) => (d as HTMLElement).style.backgroundColor)).toMatch(/^rgb\(/);
      const href = await s.getAttribute('href');
      expect(href).toBe(`${STUB}/#muster-from=${encodeURIComponent(A)}`);

      // 🔴 What reached the other server: its two public paths, no cookie,
      // no authorization, no referrer, nothing of the pasted path or token.
      expect(stub.seen.map((r) => r.url)).toEqual(['/api/instance', '/brand/icon-192.png?v=e1']);
      for (const r of stub.seen) {
        expect(r.headers.cookie, r.url).toBeUndefined();
        expect(r.headers.authorization, r.url).toBeUndefined();
        expect(r.headers.referer, r.url).toBeUndefined();
        expect(r.headers.origin, r.url).toBe(A);
      }
      const kept = JSON.stringify(await stored(page));
      expect(kept).not.toContain('mst_');
      expect(kept).not.toContain('/tasks');

      // The same server again: refused as a duplicate.
      await addServer(page, hostOf(STUB).replace(/^/, 'http://') + '/');
      await expect(page.locator('#server-add-error')).toHaveText(/already on the list/);
      await page.locator('#server-add [data-dialog-close]').click();

      // The etag: the same one is not fetched again; a new one is.
      const markStale = () => page.evaluate(() => {
        const v = JSON.parse(localStorage.getItem('muster-servers')!);
        for (const s of v.servers) s.checkedAt = 0;
        localStorage.setItem('muster-servers', JSON.stringify(v));
      });
      const reopen = async () => {
        await page.locator('#server-switcher-toggle').click();
        await expect(page.locator('#server-switcher-toggle')).toHaveAttribute('aria-expanded', 'false');
        await page.locator('#server-switcher-toggle').click();
      };
      const firstLogo = await s.locator('[data-server-logo]').getAttribute('src');
      await markStale();
      await reopen();
      await expect.poll(() => stub.seen.length).toBe(3);
      expect(stub.seen[2].url).toBe('/api/instance');
      // That read is written back (checkedAt stamped) before the next one.
      await expect.poll(async () => (await stored(page)).servers[1].checkedAt).toBeGreaterThan(0);
      expect(stub.seen.length, 'the same etag: the icon is not fetched again').toBe(3);
      stub.etag = 'e2';
      stub.name = 'Team Stub Renamed';
      await markStale();
      await reopen();
      await expect(s.locator('[data-server-label]')).toHaveText('Team Stub Renamed');
      await expect.poll(() => stub.seen.map((r) => r.url).slice(3)).toEqual(['/api/instance', '/brand/icon-192.png?v=e2']);
      await expect.poll(() => s.locator('[data-server-logo]').getAttribute('src')).not.toBe(firstLogo);

      // Rename, then clear the label: the name shows again.
      await page.locator('#server-manage-open').click();
      const manage = page.locator('#server-manage');
      await expect(manage).toHaveJSProperty('open', true);
      const row = manage.locator(`li[data-server-origin="${STUB}"]`);
      const selfRow = manage.locator(`li[data-server-origin="${A}"]`);
      await expect(selfRow.locator('[data-server-remove]')).toBeHidden();
      await row.locator('input').fill('Work');
      await row.locator('input').press('Enter');
      await expect(s.locator('[data-server-label]')).toHaveText('Work');
      await row.locator('input').fill('');
      await row.locator('input').press('Enter');
      await expect(s.locator('[data-server-label]')).toHaveText('Team Stub Renamed');

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

      expect(consoleErrors.filter((e) => /Content Security Policy|content security policy/i.test(e))).toEqual([]);
    });
  });
}

test.describe('another muster (desktop)', () => {
  test.use({ viewport: { width: 1280, height: 800 } });

  test('a real cross-origin read of another muster’s instance document, and nothing else of it', async ({ page }) => {
    const cspErrors: string[] = [];
    page.on('console', (m) => { if (/Content Security Policy/i.test(m.text())) cspErrors.push(m.text()); });
    await fresh(page);
    // The CSP parsed: no source in it was refused as invalid.
    expect(cspErrors.filter((e) => /invalid|ignored/i.test(e))).toEqual([]);
    const opts = { credentials: 'omit', referrerPolicy: 'no-referrer', redirect: 'error', mode: 'cors' } as const;
    // Positive: muster's own CORS header and A's CSP let A read A2's document.
    const doc = await page.evaluate(async ([u, o]) => (await fetch(u as string, o as RequestInit)).json(), [A2 + '/api/instance', opts] as const);
    expect(Object.keys(doc).sort()).toEqual(['logo', 'name']);
    // Negative: any other path on another origin is refused by the CSP (not
    // just by CORS) — the request never leaves the page.
    const refused = await page.evaluate(async ([u, o]) => {
      const seen: string[] = [];
      document.addEventListener('securitypolicyviolation', (e) => seen.push(e.violatedDirective));
      try { await fetch(u as string, o as RequestInit); return 'fetched'; } catch { await new Promise((r) => setTimeout(r, 50)); return seen.join(',') || 'threw'; }
    }, [A2 + '/api/tasks', opts] as const);
    expect(refused).toBe('connect-src');
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
    await expect.poll(async () => ((await stored(page)).servers[1] ?? {}).checkedAt > 0).toBe(true);

    const nav = page.waitForRequest((r) => r.isNavigationRequest() && r.url().startsWith(A2 + '/'));
    await item(page, A2).click();
    const req = await nav;
    // The navigation carries no Referer and no query: the fragment is not sent.
    expect(req.headers()['referer']).toBeUndefined();
    expect(new URL(req.url()).search).toBe('');
    await page.waitForURL((u) => u.origin === A2 && !u.pathname.startsWith('/login'));

    const offer = page.locator('#server-offer');
    await expect(offer).toHaveJSProperty('open', true);
    await expect(offer.locator('[data-server-host]')).toHaveText(hostOf(A));
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
