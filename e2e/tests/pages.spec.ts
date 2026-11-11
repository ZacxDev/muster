import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { binDir, dsn, pagesFakeS3Port, pagesMirrorMCPPort, pagesPort, pagesRouterPort, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { clsInit, readCls, settle } from '../lib/cls';

// SHAREABLE PAGES, every hop real (internal/api pages.go, page_tools.go,
// page_share.go; docs/pages.md):
//
//   a Claude Code session in a mirrored window calls its host's LOOPBACK MCP
//   endpoint (a real muster-mirror, -tags e2emirror) → page_create publishes a
//   page with a form, an upload and custom script → page_share_request files
//   the owner's card → the owner taps Approve link on the Asks page → an
//   ANONYMOUS browser opens the link: the instance's branding, the sandboxed
//   frame; fills the form, uploads a file, and the page's own script saves
//   through muster.submit → page_responses hands all three back as fenced data
//   → the owner's notification reaches the (fake) router → republish keeps the
//   link, a rollback restores version 1, revoke makes it 404.
//
// 🔴 THE NEGATIVE SECURITY TEST runs IN THE OWNER'S OWN SIGNED-IN CONTEXT, so
// muster's cookies are right there: the page's script reads document.cookie
// (must throw — an opaque origin) and fetches a session route of muster's
// (must be refused — no network). Its POSITIVE CONTROL is the same probe run
// at the top level of muster's own origin in the same context: there it reads
// cookies without throwing and the fetch answers 200. So "refused" is a fact
// about the sandbox, not about a probe that could never succeed.
//
// A password-mode server (user 1 owns everything), uploads to a fake S3 (the
// minimal one uploads.spec.ts plays), and a fake permission router whose
// POST /api/notify the owner's notifications land on (routerServes: user 1 is
// the admin). Every value below is an obvious fake.

const origin = `http://localhost:${pagesPort}`;
const routerURL = `http://127.0.0.1:${pagesRouterPort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-pages-' + stamp;
const sid = 'dddddddd-' + stamp;
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'pages-'));
const tr = path.join(dir, sid + '.jsonl');
const fakeTMUX = '/tmp/muster-e2e-fake-tmux/default,1,0';
const mcpSecretFile = path.join(dir, 'mcp-secret');
const localMCP = `http://127.0.0.1:${pagesMirrorMCPPort}/mcp`;
const ACCESS = 'e2e-pages-s3-access-not-real';
const BRAND = 'Example Studio ' + stamp;
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';

// A real 2×2 PNG (the uploads spec's).
const PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8AAQv8ZYAwAQ84H+VjtZqAAAAAASUVORK5CYII=',
  'base64',
);

// --- the fake S3 (uploads.spec.ts's, minus multipart: every file here is tiny) ---
const objects = new Map<string, Buffer>();
const xmlErr = (res: http.ServerResponse, status: number, code: string) =>
  res.writeHead(status, { 'Content-Type': 'application/xml' }).end(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>${code}</Code><Message>${code}</Message></Error>`);
function unchunk(b: Buffer): Buffer {
  const out: Buffer[] = [];
  let at = 0;
  for (;;) {
    const eol = b.indexOf('\r\n', at);
    if (eol < 0) break;
    const size = parseInt(b.subarray(at, eol).toString('latin1').split(';')[0], 16);
    if (!size) break;
    out.push(b.subarray(eol + 2, eol + 2 + size));
    at = eol + 2 + size + 2;
  }
  return Buffer.concat(out);
}
const objHeaders = (b: Buffer) => ({
  'Content-Length': String(b.length),
  ETag: '"' + crypto.createHash('md5').update(b).digest('hex') + '"',
  'Last-Modified': new Date().toUTCString(),
  'Content-Type': 'application/octet-stream',
});
const s3 = http.createServer((req, res) => {
  const chunks: Buffer[] = [];
  req.on('data', (c) => chunks.push(c));
  req.on('end', () => {
    let body = Buffer.concat(chunks);
    const [rawPath, query = ''] = (req.url ?? '').split('?');
    const p = decodeURIComponent(rawPath);
    if (!(req.headers.authorization ?? '').startsWith(`AWS4-HMAC-SHA256 Credential=${ACCESS}/`)) return xmlErr(res, 403, 'AccessDenied');
    if (!p.startsWith('/muster-e2e')) return xmlErr(res, 404, 'NoSuchBucket');
    const key = p.replace(/^\/muster-e2e\/?/, '');
    if (key === '') {
      if (req.method === 'HEAD') return void res.writeHead(200).end();
      if (query.startsWith('lifecycle')) return xmlErr(res, 404, 'NoSuchLifecycleConfiguration');
      if (query.startsWith('location')) return void res.writeHead(200, { 'Content-Type': 'application/xml' }).end('<LocationConstraint>us-east-1</LocationConstraint>');
      return xmlErr(res, 400, 'NotImplemented');
    }
    if (req.method === 'PUT') {
      if (String(req.headers['x-amz-content-sha256'] ?? '').startsWith('STREAMING')) body = unchunk(body);
      objects.set(key, body);
      return void res.writeHead(200, { ETag: objHeaders(body).ETag }).end();
    }
    const b = objects.get(key);
    if (req.method === 'HEAD') return b ? void res.writeHead(200, objHeaders(b)).end() : void res.writeHead(404).end();
    if (req.method === 'GET') return b ? void res.writeHead(200, objHeaders(b)).end(b) : xmlErr(res, 404, 'NoSuchKey');
    if (req.method === 'DELETE') {
      objects.delete(key);
      return void res.writeHead(204).end();
    }
    res.writeHead(405).end();
  });
});

// --- the fake permission router: it records POST /api/notify ---------------------
type Notification = { type: string; title?: string; body?: string; tag?: string; data?: Record<string, string> };
const notified: Notification[] = [];
const router = http.createServer((req, res) => {
  const chunks: Buffer[] = [];
  req.on('data', (c) => chunks.push(c));
  req.on('end', () => {
    res.setHeader('Content-Type', 'application/json');
    if (req.method === 'POST' && (req.url ?? '').startsWith('/api/notify')) {
      try {
        notified.push(JSON.parse(Buffer.concat(chunks).toString('utf8')) as Notification);
      } catch {
        /* a body muster never sends */
      }
    }
    res.end('{}');
  });
});

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.beforeAll(async () => {
  test.setTimeout(240_000);
  await new Promise<void>((r) => s3.listen(pagesFakeS3Port, '127.0.0.1', () => r()));
  await new Promise<void>((r) => router.listen(pagesRouterPort, '127.0.0.1', () => r()));
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, pagesPort, {
    MUSTER_MIRROR: '1',
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_PAGES: '1',
    MUSTER_PUBLIC_URL: origin,
    MUSTER_S3_ENDPOINT: `http://127.0.0.1:${pagesFakeS3Port}`,
    MUSTER_S3_BUCKET: 'muster-e2e',
    MUSTER_S3_ACCESS_KEY: ACCESS,
    MUSTER_S3_SECRET_KEY: 'e2e-pages-s3-secret-not-real',
    MUSTER_UPLOAD_SIGNING_KEY: 'e2e-pages-upload-signing-key-not-a-real-one-0123456789',
    // A router instead of the standalone declaration (the two are exclusive):
    // the owner's notifications land on its POST /api/notify.
    MUSTER_STANDALONE: '',
    MUSTER_ROUTER_URL: routerURL,
    MUSTER_ROUTER_TOKEN: 'e2e-pages-router-token-not-a-real-one',
  });
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  fs.writeFileSync(tr, JSON.stringify({ type: 'user', message: { role: 'user', content: 'make a page for the client' } }) + '\n');
  const windows = path.join(dir, 'windows');
  fs.writeFileSync(windows, `work|@1|0|1|claude|${Math.floor(Date.now() / 1000) - 60}|${tr}\n`);
  mirrorProc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
      MUSTER_MIRROR_MCP_LISTEN: `127.0.0.1:${pagesMirrorMCPPort}`,
      MUSTER_MIRROR_MCP_SECRET_FILE: mcpSecretFile,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  mirrorProc.stdout?.on('data', (d) => (mirrorLog += d));
  mirrorProc.stderr?.on('data', (d) => (mirrorLog += d));
});

test.afterAll(async () => {
  try {
    // The branding is the SERVER's row in the leg's shared database.
    fix('branding', 'clear');
  } finally {
    if (mirrorProc && mirrorProc.exitCode === null) {
      const done = new Promise((r) => mirrorProc!.once('exit', r));
      mirrorProc.kill('SIGTERM');
      await done;
    }
    await server?.stop();
    await new Promise((r) => s3.close(r));
    await new Promise((r) => router.close(r));
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

type ToolResult = { isError: boolean; body: Record<string, unknown> };

// call is one tools/call through the host's loopback endpoint as the pane's
// session (mcp-dispatch.spec.ts's): it waits for the secret and the listing.
async function call(name: string, args: Record<string, unknown>): Promise<ToolResult> {
  let last = '';
  for (let i = 0; i < 60; i++) {
    if (fs.existsSync(mcpSecretFile)) {
      const secret = fs.readFileSync(mcpSecretFile, 'utf8').trim();
      const r = await fetch(localMCP, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + secret, 'X-Muster-Pane': '%e2e-work-@1', 'X-Muster-Tmux': fakeTMUX },
        body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name, arguments: args } }),
      }).catch((e) => ({ status: 0, text: async () => String(e) }) as unknown as Response);
      last = await r.text();
      if (r.status === 200) {
        const j = JSON.parse(last) as { result?: { isError?: boolean; content: { text: string }[] }; error?: unknown };
        const text = j.result?.content?.[0]?.text ?? '';
        if (!(j.result?.isError && /lists no window running Claude session|no Claude Code session runs in pane/.test(text))) {
          expect(j.error, last).toBeUndefined();
          return { isError: !!j.result?.isError, body: JSON.parse(text) as Record<string, unknown> };
        }
      }
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`the host's MCP endpoint never answered ${name}: ${last}\n${mirrorLog}`);
}

// The page's own script: a custom submit, and the two probes.
const CUSTOM_HTML = `<div id="custom">
<button id="cj" type="button">Send my pick</button>
<p id="cj-out"></p><p id="probe-cookie"></p><p id="probe-fetch"></p>
</div>
<script>
(function(){
  var c=document.getElementById('probe-cookie');
  try { var v=document.cookie; c.textContent='read:'+v.length; } catch(e) { c.textContent='threw:'+e.name; }
  var f=document.getElementById('probe-fetch');
  try {
    fetch('/pages',{credentials:'include',redirect:'manual'}).then(function(r){ f.textContent='status:'+r.status; },function(e){ f.textContent='blocked:'+e.name; });
  } catch(e) { f.textContent='blocked:'+e.name; }
  document.getElementById('cj').addEventListener('click',function(){
    muster.submit({picked:'May 8',from:'custom script'}).then(function(r){ document.getElementById('cj-out').textContent='saved:'+r.id; },
      function(e){ document.getElementById('cj-out').textContent='failed:'+e.message; });
  });
})();
</script>`;

const BLOCKS = [
  { type: 'markdown', text: '# Pick your launch date\n\nWe built the new **checkout** flow.' },
  { type: 'form', id: 'contact', label: 'Your details', fields: [
    { name: 'email', label: 'Email', type: 'email', required: true },
    { name: 'company', label: 'Company', type: 'text' },
  ], submit_label: 'Send details' },
  { type: 'upload', id: 'logo', label: 'Your logo', submit_label: 'Upload logo' },
  { type: 'html', html: CUSTOM_HTML },
];

// probe runs the page script's two probes at the top level of `page`.
const probe = (page: Page) =>
  page.evaluate(async () => {
    let cookie = '';
    try {
      cookie = 'read:' + document.cookie.length;
    } catch (e) {
      cookie = 'threw:' + (e as Error).name;
    }
    let fetched = '';
    try {
      const r = await fetch('/pages', { credentials: 'include', redirect: 'manual' });
      fetched = 'status:' + r.status;
    } catch (e) {
      fetched = 'blocked:' + (e as Error).name;
    }
    return { cookie, fetched };
  });

test('an agent publishes a page, the owner approves its link, a stranger answers it, and the agent reads the answers', async ({ page, browser }) => {
  test.setTimeout(180_000);

  // The instance's branding, set by the admin on Account (the stranger's shell
  // draws it).
  await signIn(page, origin);
  await page.goto(origin + '/account#branding');
  await page.fill('#branding-name', BRAND);
  await page.setInputFiles('#branding-logo', { name: 'logo.png', mimeType: 'image/png', buffer: PNG });
  await Promise.all([page.waitForURL(origin + '/account?branding=saved#branding'), page.click('#branding-save')]);

  // 1. The agent publishes; the page is private.
  const created = await call('page_create', { title: 'Checkout launch ' + stamp, blocks: BLOCKS });
  expect(created.isError, JSON.stringify(created.body)).toBe(false);
  const pageID = created.body.page_id as number;
  expect(created.body.preview_url).toBe(`${origin}/pages/${pageID}/preview`);

  // 2. It asks for a link; the owner taps Approve link on the Asks page.
  const req = await call('page_share_request', { page_id: pageID, reason: 'to collect the launch date' });
  expect(req.isError, JSON.stringify(req.body)).toBe(false);
  const askID = req.body.ask_id as number;
  expect((await call('page_get', { page_id: pageID })).body.links).toEqual([expect.not.objectContaining({ url: expect.anything() })]);
  await page.goto(origin + '/attention');
  const card = page.locator(`#ask-${askID}`);
  await expect(card).toContainText('asks to share your page');
  await expect(card).toContainText(`/pages/${pageID}/preview`);
  await Promise.all([page.waitForURL(origin + '/attention'), card.getByRole('button', { name: 'Approve link', exact: true }).click()]);
  // The share request reached the owner's phone path too (the router's fan-out).
  await expect.poll(() => notified.some((n) => n.tag === `attention-ask-${askID}`), 'the share request was notified').toBe(true);

  const got = await call('page_get', { page_id: pageID });
  const url = ((got.body.links as { url?: string }[])[0] ?? {}).url ?? '';
  expect(url, JSON.stringify(got.body)).toMatch(new RegExp(`^${origin}/s/[A-Za-z0-9_-]{43}$`));

  // 3. A stranger: no cookie at all.
  const stranger = await browser.newContext({ viewport: { width: 390, height: 844 } });
  const sp = await stranger.newPage();
  await sp.addInitScript(clsInit);
  const res = await sp.goto(url);
  expect(res?.status()).toBe(200);
  expect(res?.headers()['referrer-policy']).toBe('no-referrer');
  await expect(sp.locator('[data-brand-name]')).toHaveText(BRAND);
  await expect.poll(() => sp.locator('[data-brand-logo]').evaluate((i: HTMLImageElement) => i.complete && i.naturalWidth > 0)).toBe(true);
  await expect(sp.locator('[data-page-title]')).toHaveText('Checkout launch ' + stamp);
  // No muster chrome: no sidebar, no nav, no wordmark.
  await expect(sp.locator('nav, [data-sidebar], #sidenav')).toHaveCount(0);
  const frame = sp.frameLocator('iframe#page');
  await expect(frame.locator('h1')).toHaveText('Pick your launch date');
  // The shell's layout settles (it is a page a stranger opens).
  await settle(sp);
  expect((await readCls(sp)).total, 'the shell does not shift').toBeLessThan(0.01);

  // The form, through the built-in component.
  const contact = frame.locator('[data-muster-component="contact"]');
  await contact.locator('input[name="email"]').fill('client@example.com');
  await contact.locator('input[name="company"]').fill('Example Co');
  await contact.getByRole('button', { name: 'Send details' }).click();
  await expect(contact.locator('[data-muster-status]')).toHaveText('Sent. Thank you.');
  // The upload.
  const logo = frame.locator('[data-muster-component="logo"]');
  await logo.locator('input[type="file"]').setInputFiles({ name: 'client-logo.png', mimeType: 'image/png', buffer: PNG });
  await logo.getByRole('button', { name: 'Upload logo' }).click();
  await expect(logo.locator('[data-muster-status]')).toHaveText('Sent. Thank you.');
  // The page's own script, through muster.submit.
  await frame.locator('#cj').click();
  await expect(frame.locator('#cj-out')).toHaveText(/^saved:\d+$/);

  // 4. The agent reads all three, fenced.
  const read = await call('page_responses', { page_id: pageID });
  expect(read.isError, JSON.stringify(read.body)).toBe(false);
  expect(read.body.count).toBe(3);
  const fenced = read.body.responses as string;
  expect(fenced).toContain('<<<page-responses untrusted nonce=');
  expect(fenced).toContain('"email":"client@example.com"');
  expect(fenced).toContain('"name":"client-logo.png"');
  expect(fenced).toContain('"picked":"May 8"');

  // 5. The owner was notified once (one a minute per page), naming the page.
  await expect.poll(() => notified.filter((n) => n.tag === `page-${pageID}-response`).length, 'the owner was notified').toBe(1);
  const note = notified.find((n) => n.tag === `page-${pageID}-response`)!;
  expect(note.title).toContain('Checkout launch');
  expect(JSON.stringify(note)).not.toContain('client@example.com');

  // 6. 🔴 THE SANDBOX, in the OWNER's signed-in context (its cookies present).
  const op = await page.context().newPage();
  await op.goto(url);
  const opFrame = op.frameLocator('iframe#page');
  await expect(opFrame.locator('#probe-cookie')).toHaveText('threw:SecurityError');
  await expect(opFrame.locator('#probe-fetch')).toHaveText(/^blocked:TypeError$/);
  // …and opened DIRECTLY (no shell, no frame attribute): the header alone.
  await op.goto(url + '/content');
  await expect(op.locator('#probe-cookie')).toHaveText('threw:SecurityError');
  await expect(op.locator('#probe-fetch')).toHaveText(/^blocked:TypeError$/);
  // The positive control: the SAME probe at muster's own top level, same
  // context, reads cookies without throwing and is answered 200.
  await op.goto(origin + '/pages');
  expect(await probe(op)).toEqual({ cookie: expect.stringMatching(/^read:\d+$/), fetched: 'status:200' });
  await op.close();

  // 7. The owner's Pages screen: the responses and the file to download.
  await page.goto(origin + `/pages/${pageID}`);
  await expect(page.locator('[data-response-kind="submit"]')).toHaveCount(2);
  const dl = page.locator('[data-response-download]');
  await expect(dl).toHaveCount(1);
  const href = await dl.getAttribute('href');
  const file = await page.request.get(origin + href!);
  expect(file.status()).toBe(200);
  expect(file.headers()['content-security-policy']).toContain('sandbox');

  // 8. Republish: the SAME link shows version 2.
  const up = await call('page_update', { page_id: pageID, blocks: [{ type: 'markdown', text: 'Thanks — this is version two.' }] });
  expect(up.body.version).toBe(2);
  await sp.goto(url);
  await expect(frame.locator('main')).toContainText('this is version two');

  // 9. Rollback on the Pages screen: version 1 again.
  await page.goto(origin + `/pages/${pageID}`);
  await Promise.all([page.waitForURL(origin + `/pages/${pageID}`), page.locator('[data-version="1"] [data-version-current]').click()]);
  await sp.goto(url);
  await expect(frame.locator('h1')).toHaveText('Pick your launch date');

  // 10. Revoke: the link 404s at once.
  await Promise.all([page.waitForURL(origin + `/pages/${pageID}`), page.locator('[data-link-state="live"] [data-link-revoke]').click()]);
  const gone = await sp.goto(url);
  expect(gone?.status()).toBe(404);
  await stranger.close();
});

// The owner's two Pages screens settle without layout shift (cls.spec.ts
// names them as measured here: they need pages on and an agent-made page).
test('the Pages screens do not shift on load', async ({ page }) => {
  await signIn(page, origin);
  const created = await call('page_create', { title: 'Layout probe ' + stamp, blocks: [{ type: 'markdown', text: 'probe' }] });
  const pageID = created.body.page_id as number;
  for (const [url, ready] of [
    ['/pages', '[data-testid="pages"] li'],
    [`/pages/${pageID}`, '[data-testid="page-versions"] li'],
    [`/pages/${pageID}/preview`, 'iframe#page'],
  ] as const) {
    await page.addInitScript(clsInit);
    await page.goto(origin + url);
    await settle(page);
    await expect(page.locator(ready).first(), `${url} drew its content`).toBeVisible();
    const r = await readCls(page);
    expect(r.total, `${url}: ${JSON.stringify(r.shifts)}`).toBeLessThan(0.01);
  }
});
