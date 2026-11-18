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
<p id="cj-out"></p><p id="probe-cookie"></p><p id="probe-fetch"></p><p id="probe-nocors"></p>
</div>
<script>
(function(){
  var c=document.getElementById('probe-cookie');
  try { var v=document.cookie; c.textContent='read:'+v.length; } catch(e) { c.textContent='threw:'+e.name; }
  var f=document.getElementById('probe-fetch');
  try {
    fetch('/pages',{credentials:'include',redirect:'manual'}).then(function(r){ f.textContent='status:'+r.status; },function(e){ f.textContent='blocked:'+e.name; });
  } catch(e) { f.textContent='blocked:'+e.name; }
  // A no-cors request needs no CORS answer, so only the page's connect-src
  // can refuse it: this probe sees the CSP layer on its own.
  var n=document.getElementById('probe-nocors');
  try {
    fetch('/health',{mode:'no-cors'}).then(function(r){ n.textContent='status:'+r.status; },function(e){ n.textContent='blocked:'+e.name; });
  } catch(e) { n.textContent='blocked:'+e.name; }
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
    let nocors = '';
    try {
      const r = await fetch('/health', { mode: 'no-cors' });
      nocors = 'status:' + r.status;
    } catch (e) {
      nocors = 'blocked:' + (e as Error).name;
    }
    return { cookie, fetched, nocors };
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
  // The ask flow is an owner who did NOT let their agents share without
  // asking (Account → Pages): the leg's database is shared, so it is checked
  // here, not assumed.
  await expect(page.locator('#pages-agent-share')).toHaveAttribute('data-state', 'off');

  // 1. The agent publishes; the page is private.
  const created = await call('page_create', { title: 'Checkout launch ' + stamp, blocks: BLOCKS });
  expect(created.isError, JSON.stringify(created.body)).toBe(false);
  const pageID = created.body.page_id as number;
  // One name for the owner's view (round 0's D2): preview_url, kept for
  // compatibility; no url beside it.
  expect(created.body.url).toBeUndefined();
  expect(created.body.preview_url).toBe(`${origin}/pages/${pageID}/view`);

  // 2. It asks for a link; the owner taps Approve link on the Asks page.
  const req = await call('page_share_request', { page_id: pageID, reason: 'to collect the launch date' });
  expect(req.isError, JSON.stringify(req.body)).toBe(false);
  const askID = req.body.ask_id as number;
  // The url is in the request's result, once (only its hash is stored); it
  // opens nothing until the owner approves.
  const url = req.body.url as string;
  expect(url, JSON.stringify(req.body)).toMatch(new RegExp(`^${origin}/s/[A-Za-z0-9_-]{43}$`));
  expect((await page.request.get(url, { maxRedirects: 0 })).status()).toBe(404);
  await page.goto(origin + '/attention');
  const card = page.locator(`#ask-${askID}`);
  await expect(card).toContainText('asks to share your page');
  await expect(card).toContainText(`/pages/${pageID}/view`);
  await Promise.all([page.waitForURL(origin + '/attention'), card.getByRole('button', { name: 'Approve link', exact: true }).click()]);
  // (Its push to the owner's phone is muster's own Web Push, which the
  // router never carries — goNotify skips attention events there. The Go
  // test TestAPageFromCreationToAStrangersResponse asserts that push.)

  // page_get shows its state, never its url.
  const got = await call('page_get', { page_id: pageID });
  expect((got.body.links as { state: string }[])[0].state).toBe('live');
  expect(JSON.stringify(got.body)).not.toContain(url.slice(-43));

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
  await expect(opFrame.locator('#probe-nocors')).toHaveText(/^blocked:TypeError$/);
  // …and opened DIRECTLY, outside the shell's frame, it is not served at all
  // (Sec-Fetch-Dest: document): none of its script runs.
  const direct = await op.goto(url + '/content');
  expect(direct?.status()).toBe(404);
  await expect(op.locator('#probe-cookie')).toHaveCount(0);
  // The positive control: the SAME probe at muster's own top level, same
  // context, reads cookies without throwing and is answered 200.
  await op.goto(origin + '/pages');
  expect(await probe(op)).toEqual({ cookie: expect.stringMatching(/^read:\d+$/), fetched: 'status:200', nocors: 'status:200' });
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
    [`/pages/${pageID}/view`, 'iframe#page'],
  ] as const) {
    await page.addInitScript(clsInit);
    await page.goto(origin + url);
    await settle(page);
    await expect(page.locator(ready).first(), `${url} drew its content`).toBeVisible();
    const r = await readCls(page);
    expect(r.total, `${url}: ${JSON.stringify(r.shifts)}`).toBeLessThan(0.01);
  }
});

// 🔴 A SERVER WITH NO LINK KEY (this spec's) MINTS ON THE TAP ONLY. There each
// Share makes a new one-off link, so a finger going down (or a tab landing) on
// Share must make nothing: a press that turns into a scroll, or a tab-through,
// would otherwise leave live links nobody holds, counted against the page's
// open-link cap. The control: the tap itself makes exactly one.
test('with no link key, a press or a focus on Share makes no link; the tap makes one', async ({ page }) => {
  await signIn(page, origin);
  const created = await call('page_create', { title: 'No prefetch ' + stamp, blocks: [{ type: 'markdown', text: 'x' }] });
  const pageID = created.body.page_id as number;
  const links = async () => ((await call('page_get', { page_id: pageID })).body.links as unknown[]).length;
  await page.goto(origin + '/pages');
  const share = page.locator(`[data-page-share][data-page-id="${pageID}"]`);
  await share.dispatchEvent('pointerdown');
  await share.focus();
  await page.waitForTimeout(1500);
  expect(await links(), 'a press and a focus made a link').toBe(0);
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  await share.click();
  await expect(page.locator('[data-page-toast]').first()).toBeVisible();
  await expect.poll(links).toBe(1);
});

// --- THE OWNER'S OWN VIEW, THE SLIM HEADER AND THE VERSION CHIP --------------
// (internal/pages render.go Shell/ShellScript/sdkScript; docs/pages.md "The
// page itself"), at a phone (touch) and a desktop:
//
//   the header is one row of at most 44px, visible on load, its long title
//   cut with an ellipsis (the whole title in its title attribute); the chip
//   opens the version popover (a bottom sheet on the phone, 44px targets),
//   Escape closes it and gives the chip its focus back; a version in it loads
//   ?v=N; the owner's own submit there says "Sent" and is recorded as theirs,
//   at that version; the header hides on a scroll
//   down, returns on a scroll up and at the top, without the frame moving or
//   any layout shift; a scroll op a page's script posts to the shell, one
//   from another window, and a non-numeric y on the private port itself are
//   all ignored (the port's positive control: the same scroll unmangled
//   hides it); a link's viewer gets the same header without the chip.
//
// MUSTER_E2E_PAGES_SHOTS=<dir> also saves screenshots of each state there.

const VIEW_TITLE = 'Quarterly launch plan: ' + 'every milestone, owner and date spelled out in full, '.repeat(3) + stamp;
const LONG = Array.from({ length: 60 }, (_, i) => `Paragraph ${i + 1}: the plan, in more words than fit on one screen.`).join('\n\n');
// The page's own script (round 0's Q1): it sends on load, untouched — through
// muster.submit and by submitting a built-in form itself — and sends again
// when its own button is tapped. In the owner's view only the tapped one may
// record; a link's records all of them, as it always has.
const SELF_SEND = `<p id="auto-out">waiting</p><button id="cj" type="button">Send my pick</button><p id="cj-out"></p>
<script>
muster.submit({auto:true}).then(function(r){document.getElementById('auto-out').textContent='saved:'+r.id;},
  function(e){document.getElementById('auto-out').textContent='refused:'+e.message;});
document.querySelector('[data-muster-component="auto"] form').requestSubmit();
document.getElementById('cj').addEventListener('click',function(){
  muster.submit({picked:'tapped'}).then(function(r){document.getElementById('cj-out').textContent='saved:'+r.id;},
    function(e){document.getElementById('cj-out').textContent='refused:'+e.message;});
});
</script>`;
const viewBlocks = (v: string) => [
  { type: 'question', id: 'pick', label: 'Your pick (' + v + ')', submit_label: 'Send pick' },
  { type: 'question', id: 'auto', label: 'Sent by the page itself' },
  { type: 'html', html: SELF_SEND },
  { type: 'markdown', text: '# ' + v + '\n\n' + LONG },
];
// Round 0's D1: a page that replaces its own document (document.open erases
// the SDK's listeners) and then listens for the shell's port. Measured before
// the fix: it got the port, at both sizes. Now the port is held before any
// script of the page's runs, so it never arrives here.
const TAKE_PORT = `<p id="x">start</p><script>setTimeout(function(){document.open();document.write('<!doctype html><p id="x">replaced</p>` +
  `<script>addEventListener("message",function(e){if(e.ports&&e.ports.length){var p=e.ports[0];document.getElementById("x").textContent="got-port";` +
  `setInterval(function(){p.postMessage({muster:1,op:"scroll",y:9000+Math.random()*900})},50)}},true)<\\/script>');document.close();},0)</script>`;
// The test's hook on the SDK's port: it runs before the SDK (an init script),
// so the SDK captures THIS postMessage, and window.__mutate (set by the test)
// can corrupt what it sends — how a non-numeric y reaches the shell's port.
const portHook = `(() => { const pm = MessagePort.prototype.postMessage;
  MessagePort.prototype.postMessage = function (m, ...r) { if (window.__mutate) m = window.__mutate(m); return pm.call(this, m, ...r); }; })()`;
const shots = process.env.MUSTER_E2E_PAGES_SHOTS ?? '';
const shot = async (p: Page, name: string) => {
  if (shots) await p.screenshot({ path: path.join(shots, name + '.png') });
};

for (const vp of [
  { name: 'phone', viewport: { width: 390, height: 844 }, touch: true },
  { name: 'desktop', viewport: { width: 1280, height: 800 }, touch: false },
]) {
  test(`the owner's view at ${vp.name}: a slim header that hides on scroll, the version chip, and what the owner sends is recorded`, async ({ browser }) => {
    test.setTimeout(150_000);
    const created = await call('page_create', { title: VIEW_TITLE, blocks: viewBlocks('version one') });
    expect(created.isError, JSON.stringify(created.body)).toBe(false);
    const pageID = created.body.page_id as number;
    await call('page_update', { page_id: pageID, blocks: viewBlocks('version two') });
    const ctx = await browser.newContext({ viewport: vp.viewport, hasTouch: vp.touch, isMobile: vp.touch });
    const stranger = await browser.newContext({ viewport: vp.viewport, hasTouch: vp.touch, isMobile: vp.touch });
    try {
      await ctx.addInitScript(portHook);
      const page = await ctx.newPage();
      await page.addInitScript(clsInit);
      await signIn(page, origin);
      await page.goto(origin + `/pages/${pageID}/view`);
      const header = page.locator('header[data-page-brand]');
      const frameEl = page.locator('iframe#page');
      const frame = page.frameLocator('iframe#page');
      const inFrame = (fn: () => void) => frame.locator('body').evaluate(fn);
      await expect(frame.locator('h1')).toHaveText('version two');

      // 1. Visible on load; one row of at most 44px; the title cut short.
      await expect(header).not.toHaveAttribute('data-hidden');
      const hb = (await header.boundingBox())!;
      expect(hb.y).toBe(0);
      expect(hb.height, 'the header is one slim row').toBeLessThanOrEqual(44);
      const title = page.locator('[data-page-title]');
      await expect(title).toHaveAttribute('title', VIEW_TITLE);
      const t = await title.evaluate((e) => ({ cut: e.scrollWidth > e.clientWidth, overflow: getComputedStyle(e).textOverflow, height: e.getBoundingClientRect().height }));
      expect(t.cut, 'the title is cut short').toBe(true);
      expect(t.overflow).toBe('ellipsis');
      expect(t.height, 'the title is one line').toBeLessThan(24);
      // The page's own script sent on load, untouched: refused — by
      // muster.submit, and by submitting a built-in form — and nothing is
      // recorded (step 4 counts the rows).
      await expect(frame.locator('#auto-out')).toHaveText('refused:Tap to send');
      await expect(frame.locator('[data-muster-component="auto"] [data-muster-status]')).toHaveText('Tap to send');
      await settle(page);
      expect((await readCls(page)).total, 'the owner\'s view does not shift on load').toBeLessThan(0.01);
      await shot(page, `owner-${vp.name}-closed`);

      // 2. The chip opens the popover; Escape closes it.
      const chip = page.locator('[data-version-chip]');
      const pop = page.locator('#page-versions');
      await expect(chip).toHaveText('v2 ▾');
      await expect(chip).toHaveAttribute('aria-expanded', 'false');
      await chip.click();
      await expect(chip).toHaveAttribute('aria-expanded', 'true');
      await expect(pop).toBeVisible();
      await expect(pop.locator('[data-version-link]')).toHaveText([/^Version 2current · \d{4}-\d\d-\d\d \d\d:\d\d UTC$/, /^Version 1\d{4}-\d\d-\d\d \d\d:\d\d UTC$/]);
      await expect(pop.locator('[data-page-share]')).toBeVisible();
      await expect(pop.locator('[data-page-details]')).toHaveAttribute('href', `/pages/${pageID}`);
      await expect(pop.locator('[data-version-link="2"]')).toBeFocused();
      for (const el of await pop.locator('a, button').all()) {
        expect((await el.boundingBox())!.height, 'a popover target is at least 44px').toBeGreaterThanOrEqual(44);
      }
      if (vp.touch) {
        const pb = (await pop.boundingBox())!;
        expect(Math.round(pb.y + pb.height), 'on a phone the popover is a bottom sheet').toBe(vp.viewport.height);
        expect(Math.round(pb.width)).toBe(vp.viewport.width);
      }
      await shot(page, `owner-${vp.name}-open`);
      await page.keyboard.press('Escape');
      await expect(pop).toBeHidden();
      await expect(chip).toHaveAttribute('aria-expanded', 'false');
      await expect(chip).toBeFocused();

      // 3. Selecting a version loads it.
      await chip.click();
      await Promise.all([page.waitForURL(origin + `/pages/${pageID}/view?v=1`), pop.locator('[data-version-link="1"]').click()]);
      await expect(frame.locator('h1')).toHaveText('version one');
      await expect(chip).toHaveText('v1 ▾');

      // 4. The owner submits from it: "Sent", recorded as theirs, at version 1.
      const pick = frame.locator('[data-muster-component="pick"]');
      await pick.locator('input[name="answer"]').fill('the ' + vp.name + ' pick');
      await pick.getByRole('button', { name: 'Send pick' }).click();
      await expect(pick.locator('[data-muster-status]')).toHaveText('Sent. Thank you.');
      const read = await call('page_responses', { page_id: pageID });
      expect(read.body.count, JSON.stringify(read.body)).toBe(1);
      const fenced = read.body.responses as string;
      expect(fenced).toContain('"answer":"the ' + vp.name + ' pick"');
      expect(fenced).toContain('"by_owner":true');
      expect(fenced).toContain('"version":1');
      expect(fenced).not.toContain('"link_id"');
      expect(fenced, 'a send on load was recorded').not.toContain('"auto"');
      // The page's own script, after a real tap on its own button: recorded.
      await expect(frame.locator('#auto-out')).toHaveText('refused:Tap to send');
      await frame.locator('#cj').click();
      await expect(frame.locator('#cj-out')).toHaveText(/^saved:\d+$/);
      const tapped = await call('page_responses', { page_id: pageID });
      expect(tapped.body.count, JSON.stringify(tapped.body)).toBe(2);
      expect(tapped.body.responses as string).toContain('"picked":"tapped"');
      expect(tapped.body.responses as string).not.toContain('"auto"');

      // 5. The header hides on a scroll down, returns on a scroll up and at
      // the top; the frame never moves, and nothing shifts.
      await page.goto(origin + `/pages/${pageID}/view`);
      await expect(frame.locator('h1')).toHaveText('version two');
      await settle(page);
      const fb = await frameEl.boundingBox();
      await page.mouse.move(vp.viewport.width / 2, vp.viewport.height / 2);
      await page.mouse.wheel(0, 900);
      await expect(header).toHaveAttribute('data-hidden', '');
      await expect.poll(() => header.evaluate((h) => h.getBoundingClientRect().bottom), 'the header slid away').toBeLessThanOrEqual(0);
      expect(await frameEl.boundingBox(), 'the frame did not move').toEqual(fb);
      await page.mouse.wheel(0, -150);
      await expect(header).not.toHaveAttribute('data-hidden');
      await page.mouse.wheel(0, 900);
      await expect(header).toHaveAttribute('data-hidden', '');
      await inFrame(() => window.scrollTo(0, 0));
      await expect(header).not.toHaveAttribute('data-hidden');
      expect((await readCls(page)).total, 'hiding and showing the header shifts nothing').toBeLessThan(0.01);

      // 6. Forged reports are ignored: a scroll op the page's own script
      // posts to the shell, and one from another window (the shell itself).
      await inFrame(() => parent.postMessage({ muster: 1, op: 'scroll', y: 5000 }, '*'));
      await page.evaluate(() => window.postMessage({ muster: 1, op: 'scroll', y: 5000 }, '*'));
      await page.waitForTimeout(400);
      await expect(header).not.toHaveAttribute('data-hidden');
      // A non-numeric y on the port itself: ignored. The control, the same
      // scroll with y left a number: it hides.
      await inFrame(() => { (window as unknown as { __mutate: unknown }).__mutate = (m: { y: number }) => ({ ...m, y: String(m.y) }); });
      await inFrame(() => window.scrollTo(0, 1200));
      await page.waitForTimeout(400);
      await expect(header, 'a non-numeric y moved the header').not.toHaveAttribute('data-hidden');
      await inFrame(() => { (window as unknown as { __mutate: unknown }).__mutate = null; window.scrollTo(0, 1400); });
      await expect(header, 'control: the same scroll, y a number, hides it').toHaveAttribute('data-hidden', '');
      await inFrame(() => window.scrollTo(0, 0));
      await expect(header).not.toHaveAttribute('data-hidden');

      // 7. A link's viewer: the same header, no chip.
      const shared = await page.request.post(origin + `/pages/${pageID}/share`, { headers: { Origin: origin, Accept: 'application/json' } });
      expect(shared.status()).toBe(200);
      const link = (await shared.json()).url as string;
      const sp = await stranger.newPage();
      await sp.addInitScript(clsInit);
      await sp.goto(link);
      await expect(sp.frameLocator('iframe#page').locator('h1')).toHaveText('version two');
      const sh = (await sp.locator('header[data-page-brand]').boundingBox())!;
      expect(sh.height).toBeLessThanOrEqual(44);
      await expect(sp.locator('[data-version-chip]')).toHaveCount(0);
      await expect(sp.locator('#page-versions')).toHaveCount(0);
      // A link's viewer needs no tap: the page's send on load records, as it
      // always has.
      await expect(sp.frameLocator('iframe#page').locator('#auto-out')).toHaveText(/^saved:\d+$/);
      await settle(sp);
      expect((await readCls(sp)).total).toBeLessThan(0.01);
      await shot(sp, `link-${vp.name}`);
      await sp.mouse.move(vp.viewport.width / 2, vp.viewport.height / 2);
      await sp.mouse.wheel(0, 900);
      await expect(sp.locator('header[data-page-brand]')).toHaveAttribute('data-hidden', '');
      await sp.mouse.wheel(0, -150);
      await expect(sp.locator('header[data-page-brand]')).not.toHaveAttribute('data-hidden');
    } finally {
      await ctx.close();
      await stranger.close();
    }
  });
}

// Round 0's D1, its own test so a regression is named by it: a page that
// replaces its own document (document.open, which erases the SDK's
// listeners) and then listens for the shell's port never receives it — the
// SDK holds the port before any script of the page's runs — so its forged
// reports never hide the header. Measured before the fix (the markup in the
// body, the port sent on the frame's load): it received the port, at both
// sizes.
for (const vp of [
  { name: 'phone', viewport: { width: 390, height: 844 }, touch: true },
  { name: 'desktop', viewport: { width: 1280, height: 800 }, touch: false },
]) {
  test(`at ${vp.name}, a page that replaces its own document cannot take the shell's port`, async ({ browser }) => {
    const taker = await call('page_create', { title: 'Port taker ' + vp.name + ' ' + stamp, blocks: [{ type: 'html', html: TAKE_PORT }] });
    expect(taker.isError, JSON.stringify(taker.body)).toBe(false);
    const ctx = await browser.newContext({ viewport: vp.viewport, hasTouch: vp.touch, isMobile: vp.touch });
    try {
      const page = await ctx.newPage();
      await signIn(page, origin);
      await page.goto(origin + `/pages/${taker.body.page_id as number}/view`);
      const frame = page.frameLocator('iframe#page');
      await expect(frame.locator('#x')).toHaveText('replaced');
      await page.waitForTimeout(1500);
      await expect(frame.locator('#x'), 'the replaced document received the port').toHaveText('replaced');
      await expect(page.locator('header[data-page-brand]')).not.toHaveAttribute('data-hidden');
    } finally {
      await ctx.close();
    }
  });
}
