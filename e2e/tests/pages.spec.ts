import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import zlib from 'node:zlib';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import type { Browser, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { binDir, dsn, pagesFakeS3Port, pagesMirrorMCPPort, pagesPort, pagesRouterPort, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { FakeS3, startFakeS3 } from '../lib/fakes3';
import { clsInit, readCls, settle } from '../lib/cls';
import { withMCPSession } from '../lib/localmcp';
import { touchDrag } from '../lib/touch';

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
// The mirrored session's working directory: where page_asset_upload's files
// are read from (MUSTER_MIRROR_E2E_CLAUDE_CWD plays its process's cwd).
const workDir = path.join(dir, 'work');
const localMCP = `http://127.0.0.1:${pagesMirrorMCPPort}/mcp`;
const ACCESS = 'e2e-pages-s3-access-not-real';
const BRAND = 'Example Studio ' + stamp;
let server: Running | undefined;
let s3: FakeS3 | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';

// A real 2×2 PNG (the uploads spec's).
const PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8AAQv8ZYAwAQ84H+VjtZqAAAAAASUVORK5CYII=',
  'base64',
);

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
  s3 = await startFakeS3(pagesFakeS3Port, ACCESS, 'muster-e2e');
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
  fs.mkdirSync(workDir, { recursive: true });
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
      MUSTER_MIRROR_E2E_CLAUDE_CWD: workDir,
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
    // So is the owner's (user 1's) preferences row, where the test below
    // turned "Let my agents share pages without asking" off: cleared, every
    // later spec sees the default (on) again.
    fix('notifypref', '1', 'clear');
  } finally {
    if (mirrorProc && mirrorProc.exitCode === null) {
      const done = new Promise((r) => mirrorProc!.once('exit', r));
      mirrorProc.kill('SIGTERM');
      await done;
    }
    await server?.stop();
    await s3?.close();
    await new Promise((r) => router.close(r));
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

type ToolResult = { isError: boolean; body: Record<string, unknown> };

// approveLink taps Approve link on a share card on the Asks page, which must
// already be open, and returns once the answer is recorded. The card's form
// posts through htmx (200 with the card settled in place, no navigation:
// asks_feed.go asksCardRequest, asks_done.go), so
// the page never leaves /attention and a waitForURL there settles at once;
// the answer's own response is the signal that the link is live.
async function approveLink(p: Page, askID: number) {
  const posted = p.waitForResponse((r) => new URL(r.url()).pathname === `/attention/asks/${askID}/answer` && r.request().method() === 'POST');
  await p.locator(`#ask-${askID}`).getByRole('button', { name: 'Approve link', exact: true }).click();
  expect((await posted).status(), 'the approval is taken (200, the settled card)').toBe(200);
}

// call is one tools/call through the host's loopback endpoint as the pane's
// session (mcp-dispatch.spec.ts's): it waits for the secret and the listing.
async function call(name: string, args: Record<string, unknown>): Promise<ToolResult> {
  let last = '';
  for (let i = 0; i < 60; i++) {
    if (fs.existsSync(mcpSecretFile)) {
      const secret = fs.readFileSync(mcpSecretFile, 'utf8').trim();
      const r = await fetch(localMCP, {
        method: 'POST',
        headers: await withMCPSession(localMCP, { 'Content-Type': 'application/json', Authorization: 'Bearer ' + secret, 'X-Muster-Pane': '%e2e-work-@1', 'X-Muster-Tmux': fakeTMUX }),
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
  // The ask flow is an owner who turned OFF "Let my agents share pages
  // without asking" (Account → Pages; on is every user's default): the owner
  // turns it off here, through the switch, and that is checked, not assumed.
  if ((await page.locator('#pages-agent-share').getAttribute('data-state')) === 'on') {
    await page.locator('#pages-agent-share-toggle').click();
  }
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
  // The page by its title, in words; the page one tap away on the card,
  // never an address in its text.
  await expect(card).toContainText(`Share the page “Checkout launch ${stamp}”?`);
  await expect(card).not.toContainText('/pages/', { useInnerText: true });
  await expect(card.getByRole('link', { name: 'Open the page', exact: true })).toHaveAttribute('href', `/pages/${pageID}/view`);
  await approveLink(page, askID);
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
  // Each response is labelled from the page's own form, never raw JSON.
  const details = page.locator('[data-response-kind="submit"]').filter({ hasText: 'Your details' });
  await expect(details.locator('dt', { hasText: 'Email' })).toBeVisible();
  await expect(details.locator('[data-response-value]').first()).toHaveText('client@example.com');
  await expect(page.locator('[data-testid="page-responses"]')).not.toContainText('"email"', { useInnerText: true });
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
  // (An older version's ⋯ holds "Make current".)
  await page.getByRole('button', { name: 'More for version 1' }).click();
  await Promise.all([page.waitForURL(origin + `/pages/${pageID}`), page.locator('[data-version="1"] [data-version-current]').click()]);
  await sp.goto(url);
  await expect(frame.locator('h1')).toHaveText('Pick your launch date');

  // 10. Revoke: the link 404s at once.
  await Promise.all([page.waitForURL(origin + `/pages/${pageID}`), page.locator('[data-link-state="live"] [data-link-revoke]').click()]);
  const gone = await sp.goto(url);
  expect(gone?.status()).toBe(404);
  await stranger.close();
});

// --- A PAGE'S IMAGE AND VIDEO (internal/pages asset.go; docs/pages.md "Images
// and video") — #833's closing condition, through a mirrored session's REAL
// path: the session's page_asset_upload names two files in its working
// directory, a 2 MB+ PNG and a 2 MB+ WebM; the host's real muster-mirror reads
// them there and sends them to muster (never inside the MCP body); a version
// uses both refs; and both render in the OWNER's view and through a share
// link a stranger opens: the image decoded (naturalWidth > 0), the video's
// metadata loaded (readyState >= 1). The frame's sandbox is unchanged: the
// shell fetches each asset and hands it to the frame as a Blob.

// crc32 is PNG's chunk checksum.
const crcTable = Array.from({ length: 256 }, (_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});
function crc32(b: Buffer): number {
  let c = 0xffffffff;
  for (const x of b) c = crcTable[(c ^ x) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}
function pngChunk(type: string, data: Buffer): Buffer {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const td = Buffer.concat([Buffer.from(type, 'latin1'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(td));
  return Buffer.concat([len, td, crc]);
}
// noisePNG is a w×h truecolour PNG of random pixels: random bytes do not
// compress, so it is about 3·w·h bytes — a real, decodable image that big.
function noisePNG(w: number, h: number): Buffer {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(w, 0);
  ihdr.writeUInt32BE(h, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // truecolour
  const raw = Buffer.alloc((w * 3 + 1) * h);
  for (let y = 0; y < h; y++) crypto.randomFillSync(raw, y * (w * 3 + 1) + 1, w * 3); // filter byte 0, then noise
  return Buffer.concat([Buffer.from('89504e470d0a1a0a', 'hex'), pngChunk('IHDR', ihdr), pngChunk('IDAT', zlib.deflateSync(raw)), pngChunk('IEND', Buffer.alloc(0))]);
}

// recordWebM records a canvas of noise in the browser (MediaRecorder, VP8)
// until the recording passes minBytes: a real WebM Chromium can play back.
async function recordWebM(p: Page, minBytes: number): Promise<Buffer> {
  const b64 = await p.evaluate(async (min) => {
    const c = document.createElement('canvas');
    c.width = 640;
    c.height = 360;
    const g = c.getContext('2d')!;
    const img = g.createImageData(c.width, c.height);
    // window.crypto, spelled so: this function's text runs in the page, where
    // the spec's own crypto import does not exist.
    const rnd = window.crypto;
    const draw = () => {
      for (let i = 0; i < img.data.length; i += 65536) rnd.getRandomValues(new Uint8Array(img.data.buffer, i, Math.min(65536, img.data.length - i)));
      for (let i = 3; i < img.data.length; i += 4) img.data[i] = 255;
      g.putImageData(img, 0, 0);
    };
    draw();
    const rec = new MediaRecorder(c.captureStream(30), { mimeType: 'video/webm;codecs=vp8', videoBitsPerSecond: 8_000_000 });
    const parts: Blob[] = [];
    let size = 0;
    const done = new Promise<void>((res) => (rec.onstop = () => res()));
    rec.ondataavailable = (e) => {
      parts.push(e.data);
      size += e.data.size;
      if (size >= min && rec.state === 'recording') rec.stop();
    };
    const tick = setInterval(draw, 33);
    rec.start(250);
    setTimeout(() => rec.state === 'recording' && rec.stop(), 30_000);
    await done;
    clearInterval(tick);
    const buf = new Uint8Array(await new Blob(parts, { type: 'video/webm' }).arrayBuffer());
    let s = '';
    for (let i = 0; i < buf.length; i += 32768) s += String.fromCharCode(...buf.subarray(i, i + 32768));
    return btoa(s);
  }, minBytes);
  return Buffer.from(b64, 'base64');
}

const TWO_MB = 2 * 1024 * 1024;

// rendered waits for the frame's image to decode and its video's metadata to
// load, as the browser reports them.
async function expectRendered(p: Page, where: string) {
  const frame = p.frameLocator('iframe#page');
  const still = frame.locator('.mp-markdown img');
  const reel = frame.locator('video#reel');
  await expect(still, `${where}: the image`).toHaveCount(1);
  await expect(reel, `${where}: the video`).toHaveCount(1);
  await expect.poll(() => still.evaluate((i: HTMLImageElement) => i.complete && i.naturalWidth), { message: `${where}: the image decodes`, timeout: 30_000 }).toBeGreaterThan(0);
  await expect.poll(() => reel.evaluate((v: HTMLVideoElement) => v.readyState), { message: `${where}: the video's metadata loads`, timeout: 30_000 }).toBeGreaterThanOrEqual(1);
  // Each src is a blob: URL the frame made of the shell's Blob — never the
  // asset: reference, never a network address.
  expect(await still.getAttribute('src')).toMatch(/^blob:/);
  expect(await reel.getAttribute('src')).toMatch(/^blob:/);
}

test('a mirrored session attaches a 2 MB image and a 2 MB video from its working directory, and both render for the owner and through a link', async ({ page, browser }) => {
  test.setTimeout(180_000);
  await signIn(page, origin);
  await page.goto(origin + '/pages');
  const still = noisePNG(900, 900);
  const reel = await recordWebM(page, TWO_MB + 256 * 1024);
  expect(still.length, 'the image is 2 MB or more').toBeGreaterThanOrEqual(TWO_MB);
  expect(reel.length, 'the video is 2 MB or more').toBeGreaterThanOrEqual(TWO_MB);
  fs.writeFileSync(path.join(workDir, 'review-still.png'), still);
  fs.writeFileSync(path.join(workDir, 'review-reel.webm'), reel);

  const created = await call('page_create', { title: 'Creative review ' + stamp, blocks: [{ type: 'markdown', text: 'Media coming.' }] });
  expect(created.isError, JSON.stringify(created.body)).toBe(false);
  const pageID = created.body.page_id as number;
  const refs: Record<string, string> = {};
  for (const [file, type] of [['review-still.png', 'image/png'], ['review-reel.webm', 'video/webm']] as const) {
    const up = await call('page_asset_upload', { page_id: pageID, path: file });
    expect(up.isError, JSON.stringify(up.body)).toBe(false);
    expect(up.body.type).toBe(type);
    expect(up.body.size).toBe(fs.statSync(path.join(workDir, file)).size);
    refs[file] = up.body.ref as string;
    expect(refs[file]).toMatch(/^asset:[A-Za-z0-9_-]{22}$/);
  }
  const upd = await call('page_update', {
    page_id: pageID,
    blocks: [
      { type: 'markdown', text: '# The options\n\n![The still](' + refs['review-still.png'] + ')' },
      { type: 'html', html: `<video id="reel" src="${refs['review-reel.webm']}" muted playsinline controls></video>` },
    ],
  });
  expect(upd.isError, JSON.stringify(upd.body)).toBe(false);

  // The owner's own view.
  await page.goto(origin + `/pages/${pageID}/view`);
  await expectRendered(page, "the owner's view");

  // A link, approved by the owner on Asks, opened by a stranger.
  const req = await call('page_share_request', { page_id: pageID, reason: 'to pick a creative' });
  expect(req.isError, JSON.stringify(req.body)).toBe(false);
  const url = req.body.url as string;
  await page.goto(origin + '/attention');
  await approveLink(page, req.body.ask_id as number);
  const stranger = await browser.newContext({ viewport: { width: 390, height: 844 } });
  const sp = await stranger.newPage();
  expect((await sp.goto(url))?.status()).toBe(200);
  await expectRendered(sp, 'the share link');
  await stranger.close();
});

// RENDER FIRST, FILL IN (asset.go): the page's text is on screen while an
// asset is still on its way — its element a placeholder box marked
// data-muster-asset="loading" — and the asset then renders in place. The
// asset's response is HELD (the shell's fetch, on muster's own origin, is the
// one that goes through this route) until the text has been seen.
test('a page renders at once and fills in an asset when it arrives', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page, origin);
  fs.writeFileSync(path.join(workDir, 'slow-still.png'), noisePNG(400, 300));
  const created = await call('page_create', { title: 'Slow media ' + stamp, blocks: [{ type: 'markdown', text: 'Media coming.' }] });
  expect(created.isError, JSON.stringify(created.body)).toBe(false);
  const pageID = created.body.page_id as number;
  const up = await call('page_asset_upload', { page_id: pageID, path: 'slow-still.png' });
  expect(up.isError, JSON.stringify(up.body)).toBe(false);
  const upd = await call('page_update', {
    page_id: pageID,
    blocks: [{ type: 'markdown', text: '# Visible before the media\n\n![The still](' + (up.body.ref as string) + ')' }],
  });
  expect(upd.isError, JSON.stringify(upd.body)).toBe(false);

  let release!: () => void;
  const held = new Promise<void>((r) => (release = r));
  let asked = 0;
  await page.route('**/assets/**', async (route) => {
    asked++;
    await held;
    await route.continue();
  });
  await page.goto(origin + `/pages/${pageID}/view`);
  const frame = page.frameLocator('iframe#page');
  // The text, while the asset is held: the page did not wait for it.
  await expect(frame.locator('h1')).toHaveText('Visible before the media');
  await expect.poll(() => asked, 'the shell asked for the asset').toBe(1);
  const still = frame.locator('.mp-markdown img');
  await expect(still).toHaveAttribute('data-muster-asset', 'loading');
  expect(await still.getAttribute('src')).toMatch(/^data:image\/gif;base64,.*#muster-asset-[A-Za-z0-9_-]{22}$/);
  // The placeholder is a box, not a 1px dot: no jump to nothing.
  expect((await still.boundingBox())!.height).toBeGreaterThan(50);
  // Released: it renders in place.
  release();
  await expect.poll(() => still.evaluate((i: HTMLImageElement) => i.complete && i.naturalWidth), { timeout: 30_000 }).toBe(400);
  expect(await still.getAttribute('src')).toMatch(/^blob:/);
  await expect(still).not.toHaveAttribute('data-muster-asset', /.*/);
  await page.unroute('**/assets/**');
});

// The owner's two Pages screens settle without layout shift (cls.spec.ts
// names them as measured here: they need pages on and an agent-made page).
test('the Pages screens do not shift on load', async ({ page }) => {
  await signIn(page, origin);
  const created = await call('page_create', { title: 'Layout probe ' + stamp, blocks: [{ type: 'markdown', text: 'probe' }] });
  const pageID = created.body.page_id as number;
  for (const [url, ready] of [
    ['/pages', '[data-testid="pages"] li'],
    [`/pages/${pageID}`, '[data-page-title]'],
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

// A PAGE OF ~400 KB THROUGH THE HOST'S MCP ENDPOINT (#833): a page_update of
// 400,000 bytes goes Claude Code → this host's REAL muster-mirror → muster over
// HTTP and comes back with a page version; one byte past the request bound
// (mirror.MaxMCPRequestBytes, 1088 KiB = 1,114,112 bytes) is refused by the
// mirror with a 413 that names it and the limit — never JSON-RPC -32700.
//
// ⚠ AN INVARIANT GUARD, NOT #833'S REGRESSION TEST: both binaries here are
// built from this tree, and since #733 they agree on the bound, so this went
// green at origin/main too. #833's -32700 came from an OLD muster behind a
// current mirror (internal/mirrorhost's mcpproxy_oldmuster_test.go has the
// red-then-green for that); this pins the seam a real mirror and a real muster
// cross, which #733's tests reach only in process.
test('a 400,000-byte page_update through the host MCP endpoint returns a version; one past the bound is a named 413', async () => {
  const created = await call('page_create', { title: 'Big page ' + stamp, blocks: [{ type: 'markdown', text: 'v1' }] });
  expect(created.isError, JSON.stringify(created.body)).toBe(false);
  const pageID = created.body.page_id as number;
  const secret = fs.readFileSync(mcpSecretFile, 'utf8').trim();
  // sized is a page_update padded to exactly n bytes: two html blocks (a block
  // holds at most 256 KiB) of base64-shaped text, as inline images are sent.
  const sized = (n: number) => {
    const mk = (pad: number) => {
      const a = Math.floor(pad / 2);
      const blocks = [a, pad - a].map((k) => ({ type: 'html', html: `<img alt="" src="data:image/webp;base64,${'Q'.repeat(k)}">` }));
      return JSON.stringify({ jsonrpc: '2.0', id: 41, method: 'tools/call', params: { name: 'page_update', arguments: { page_id: pageID, blocks } } });
    };
    const body = mk(n - mk(0).length);
    expect(body.length, 'fixture size').toBe(n);
    return body;
  };
  const headers = await withMCPSession(localMCP, { 'Content-Type': 'application/json', Authorization: 'Bearer ' + secret, 'X-Muster-Pane': '%e2e-work-@1', 'X-Muster-Tmux': fakeTMUX });
  const post = (body: string) => fetch(localMCP, { method: 'POST', headers, body });

  const ok = await post(sized(400_000));
  const okText = await ok.text();
  expect(ok.status, okText.slice(0, 300)).toBe(200);
  const rep = JSON.parse(okText) as { error?: unknown; result?: { isError?: boolean; content: { text: string }[] } };
  expect(rep.error, okText.slice(0, 300)).toBeUndefined();
  expect(rep.result?.isError, okText.slice(0, 300)).toBeFalsy();
  const out = JSON.parse(rep.result!.content[0].text) as { page_id: number; version: number };
  expect(out).toMatchObject({ page_id: pageID, version: 2 });

  const over = await post(sized(1_114_113));
  const overText = await over.text();
  expect(over.status, overText.slice(0, 300)).toBe(413);
  expect(overText).not.toContain('-32700');
  expect(overText).toContain('muster-mirror on this host refused this MCP request: it is larger than 1088 KiB');
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
  // The row's Share is in its ⋯.
  await page.locator(`li[data-page-id="${pageID}"] [data-more-menu-open]`).click();
  const share = page.locator(`[data-page-share][data-page-id="${pageID}"]`);
  await share.dispatchEvent('pointerdown');
  await share.focus();
  await page.waitForTimeout(1500);
  expect(await links(), 'a press and a focus made a link').toBe(0);
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  await share.click();
  await expect(page.locator('[data-page-toast]').first()).toBeVisible();
  await expect.poll(links).toBe(1);
  // That one-off link is not the page's reusable link, and Revoke link / New
  // link act on that one only: the row's ⋯ still offers neither after the
  // Share (red at 776f0ca57, whose script showed both).
  await page.locator(`li[data-page-id="${pageID}"] [data-more-menu-open]`).click();
  const menu = page.locator(`#page-menu-${pageID}`);
  await expect(menu.getByRole('link', { name: 'Details' })).toBeVisible();
  await expect(menu.getByRole('button', { name: 'Revoke link' })).toHaveCount(0);
  await expect(menu.getByRole('button', { name: 'New link' })).toHaveCount(0);
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
// The page's own script: it sends on load, untouched — through muster.submit
// and by submitting a built-in form itself — and sends again when its own
// button is tapped. In the owner's view each waits for the owner's Send in
// muster's own "Send as you?" bar (one at a time); a link's records all of
// them, as it always has.
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
// The owner's confirm: muster's "Send as you?" bar in the shell.
const confirmBar = (p: Page) => p.locator('[data-send-confirm]');
// answerConfirm waits for the bar to show want, then — past the half second
// in which a press is ignored — taps Send (go) or Cancel.
async function answerConfirm(p: Page, want: string | RegExp, go: boolean) {
  const bar = confirmBar(p);
  await expect(bar).toBeVisible();
  await expect(bar.locator('[data-send-summary]')).toHaveText(want);
  await p.waitForTimeout(600);
  await bar.locator(go ? '[data-send-go]' : '[data-send-cancel]').click();
  await expect(bar).toBeHidden();
}
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
      // The owner's title is a button the header's height (its popover's,
      // a 40px target), so "one line" is counted from its text's own boxes.
      const t = await title.evaluate((e) => {
        const r = document.createRange();
        r.selectNodeContents(e);
        const lines = () => new Set(Array.from(r.getClientRects()).map((b) => Math.round(b.top))).size;
        const out = { cut: e.scrollWidth > e.clientWidth, overflow: getComputedStyle(e).textOverflow, lines: lines(), wrapped: 0 };
        // CONTROL: let it wrap, and the same count sees several lines.
        (e as HTMLElement).style.whiteSpace = 'normal';
        out.wrapped = lines();
        (e as HTMLElement).style.whiteSpace = '';
        return out;
      });
      expect(t.cut, 'the title is cut short').toBe(true);
      expect(t.overflow).toBe('ellipsis');
      expect(t.lines, 'the title is one line').toBe(1);
      expect(t.wrapped, 'CONTROL: the count sees a wrapped title').toBeGreaterThan(1);
      // The page's own script sent on load, untouched: muster's bar asks the
      // owner (the second send, while it waits, is refused), and nothing is
      // recorded; Cancel answers the page "Not sent".
      await expect(confirmBar(page)).toBeVisible();
      await expect(confirmBar(page).locator('[data-send-what]')).toHaveText('To “custom”');
      await expect(frame.locator('[data-muster-component="auto"] [data-muster-status]')).toHaveText('Another send is waiting for your answer.');
      expect((await call('page_responses', { page_id: pageID })).body.count, 'a send waiting for the owner was recorded').toBe(0);
      await shot(page, `owner-${vp.name}-confirm`);
      await answerConfirm(page, '"auto": true', false);
      await expect(frame.locator('#auto-out')).toHaveText('refused:Not sent');
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
      await expect(pop.locator('[data-version-link]')).toHaveText([/^Version 2current · today at \d{1,2}:\d{2}\s[AP]M$/, /^Version 1today at \d{1,2}:\d{2}\s[AP]M$/]);
      // Share is the header's own now (its end), not the popover's.
      await expect(pop.locator('[data-page-share]')).toHaveCount(0);
      await expect(header.locator('[data-page-share]')).toHaveCount(1);
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

      // 4. The owner submits from it, confirms in muster's bar: "Sent",
      // recorded as theirs, at version 1. (The page's send on load asks
      // again first: cancelled.)
      await answerConfirm(page, '"auto": true', false);
      const pick = frame.locator('[data-muster-component="pick"]');
      await pick.locator('input[name="answer"]').fill('the ' + vp.name + ' pick');
      await pick.getByRole('button', { name: 'Send pick' }).click();
      await answerConfirm(page, '"answer": "the ' + vp.name + ' pick"', true);
      await expect(pick.locator('[data-muster-status]')).toHaveText('Sent. Thank you.');
      const read = await call('page_responses', { page_id: pageID });
      expect(read.body.count, JSON.stringify(read.body)).toBe(1);
      const fenced = read.body.responses as string;
      expect(fenced).toContain('"answer":"the ' + vp.name + ' pick"');
      expect(fenced).toContain('"by_owner":true');
      expect(fenced).toContain('"version":1');
      expect(fenced).not.toContain('"link_id"');
      expect(fenced, 'a send on load was recorded').not.toContain('"auto"');
      // The page's own script's send, confirmed by the owner: recorded.
      await expect(frame.locator('#auto-out')).toHaveText('refused:Not sent');
      await frame.locator('#cj').click();
      await answerConfirm(page, '"picked": "tapped"', true);
      await expect(frame.locator('#cj-out')).toHaveText(/^saved:\d+$/);
      const tapped = await call('page_responses', { page_id: pageID });
      expect(tapped.body.count, JSON.stringify(tapped.body)).toBe(2);
      expect(tapped.body.responses as string).toContain('"picked":"tapped"');
      expect(tapped.body.responses as string).not.toContain('"auto"');

      // 5. The header hides on a scroll down, returns on a scroll up and at
      // the top; the frame never moves, and nothing shifts.
      await page.goto(origin + `/pages/${pageID}/view`);
      await expect(frame.locator('h1')).toHaveText('version two');
      await answerConfirm(page, '"auto": true', false);
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

// --- ROUNDS 1 AND 2 ON #802: THE OWNER'S CONFIRM ------------------------------
//
// After round 2 (the operator's decision) nothing from the owner's view is
// recorded until the owner taps Send in muster's own "Send as you?" bar, in
// the shell, which the page's script cannot reach. Every forge of rounds 1 and
// 2 — which beat the earlier tap check in the page — now only opens the bar
// (or is refused while it waits), and leaves no row.

// Round 1's forges: setters on Object.prototype, a getter on act, a scripted
// requestSubmit() of a built-in form — all on load, untouched.
const FORGE = `<p id="f-data">-</p><p id="f-act">-</p><p id="f-file">-</p><p id="f-req">-</p>
<script>
function said(id){return function(r){document.getElementById(id).textContent='saved:'+r.id;};}
function no(id){return function(e){document.getElementById(id).textContent='refused:'+e.message;};}
function setter(k){Object.defineProperty(Object.prototype,k,{configurable:true,set:function(v){
  Object.defineProperty(this,k,{value:v,enumerable:true,writable:true,configurable:true});this.act=true;}});}
setter('data');
muster.submit({s:'forge'}).then(said('f-data'),no('f-data'));
delete Object.prototype.data;
Object.defineProperty(Object.prototype,'act',{configurable:true,get:function(){return true;},set:function(){}});
muster.submit({s:'forge-act'}).then(said('f-act'),no('f-act'));
delete Object.prototype.act;
setter('file');setter('name');
muster.upload(new File(['forged'],'forged.txt',{type:'text/plain'}),{component:'fu'}).then(said('f-file'),no('f-file'));
delete Object.prototype.file;delete Object.prototype.name;
setter('data');
document.querySelector('[data-muster-component="fq"] form').requestSubmit();
delete Object.prototype.data;
document.getElementById('f-req').textContent='done';
</script>`;
// Round 2's p2, verbatim: Array species makes the built-in upload's list a
// page-built Proxy that answers act = true.
const P2_SPECIES = "<p>p2</p><script>\nsetTimeout(()=>{\n const inp=document.querySelector('[data-muster-component=up1] input[type=file]');\n const dt=new DataTransfer(); dt.items.add(new File(['hello'],'evil-p2.txt')); inp.files=dt.files;\n class C { constructor(n){ const t=[]; return new Proxy(t,{get(o,k){ if(typeof k==='string'&&/^\\d+$/.test(k)&&o[k]){ const r=o[k]; const f={}; for(const kk of Object.keys(r)) f[kk]=r[kk]; Object.defineProperty(f,'act',{get(){return true},set(){},enumerable:true}); return f;} return Reflect.get(o,k);} }); } }\n C[Symbol.species]=C;\n Object.defineProperty(Array.prototype,'constructor',{get(){return C},configurable:true});\n inp.form.requestSubmit();\n window.p2done=1;\n},300);\n</script>\n";
const FORGE_BLOCKS = [
  { type: 'question', id: 'fq', label: 'Forged question' },
  { type: 'upload', id: 'fu', label: 'Forged file' },
  { type: 'upload', id: 'up1', label: 'Species upload' },
  { type: 'html', html: FORGE },
  { type: 'html', html: P2_SPECIES },
];

// A page that, while the owner's bar is open, dispatches clicks, Enter and
// Space, and focuses its parent and itself, every 50 ms.
const HOSTILE = `<p id="h-out">-</p><button id="hb" type="button">x</button><script>
muster.submit({hostile:1}).then(function(r){document.getElementById('h-out').textContent='saved:'+r.id;},function(e){document.getElementById('h-out').textContent='refused:'+e.message;});
setInterval(function(){
  document.getElementById('hb').click();
  document.body.dispatchEvent(new MouseEvent('click',{bubbles:true}));
  ['Enter',' '].forEach(function(k){document.dispatchEvent(new KeyboardEvent('keydown',{key:k,bubbles:true}));document.dispatchEvent(new KeyboardEvent('keyup',{key:k,bubbles:true}));});
  try{window.parent.focus();}catch(e){}
  window.focus();
},50);
</script>`;

// What the owner confirms is exactly what is recorded: a form and a two-file
// upload, through the built-in components.
const EXACT_BLOCKS = [
  { type: 'form', id: 'contact', label: 'Your details', fields: [
    { name: 'email', label: 'Email', type: 'email', required: true },
    { name: 'company', label: 'Company', type: 'text' },
  ], submit_label: 'Send details' },
  { type: 'upload', id: 'files', label: 'Your files', multiple: true, submit_label: 'Upload files' },
];

// A page that sets itself up on DOMContentLoaded (document and window) and on
// window.onload: each must run exactly once.
const READY = `<p id="dcl">0</p><p id="dclw">0</p><p id="onl">0</p><script>
function bump(id){var e=document.getElementById(id);e.textContent=String(+e.textContent+1);}
document.addEventListener('DOMContentLoaded',function(){bump('dcl');});
window.addEventListener('DOMContentLoaded',function(){bump('dclw');});
window.onload=function(){bump('onl');};
</script>`;

// Pages that try to keep the header hidden: round 1's (scroll down, then stop
// their own scrolling), and round 2's p6 (then re-dispatch scroll events) and
// p8 (then scroll an invisible inner element), verbatim.
const STUCK = `<div style="height:4000px">tall</div><script>setTimeout(function(){window.scrollTo(0,800);
setTimeout(function(){document.documentElement.style.overflow='hidden';},300);},300);</script>`;
const P6_REDISPATCH = "<div style=\"height:5000px\">tall</div><script>\nsetTimeout(()=>{document.scrollingElement.scrollTop=800;setTimeout(()=>{document.documentElement.style.overflow='hidden';window.locked=1;\n setInterval(()=>window.dispatchEvent(new Event('scroll')),1000);},300);},500);\n</script>\n";
const P8_INNER = "<div style=\"height:5000px\">tall<div id=\"x\" style=\"height:10px;overflow:auto;opacity:0\"><div style=\"height:100px\"></div></div></div><script>\nsetTimeout(()=>{document.scrollingElement.scrollTop=800;setTimeout(()=>{document.documentElement.style.overflow=\"hidden\";let t=0;setInterval(()=>{document.getElementById(\"x\").scrollTop=(t^=1)*50},1000);},300);},500);\n</script>";

const ROUND_VIEWPORTS = [
  { name: 'phone', viewport: { width: 390, height: 844 }, touch: true },
  { name: 'desktop', viewport: { width: 1280, height: 800 }, touch: false },
];
// asOwner opens a signed-in context at vp, runs fn with its page, and closes it.
async function asOwner(browser: Browser, vp: (typeof ROUND_VIEWPORTS)[number], fn: (page: Page) => Promise<void>) {
  const ctx = await browser.newContext({ viewport: vp.viewport, hasTouch: vp.touch, isMobile: vp.touch });
  try {
    const page = await ctx.newPage();
    await signIn(page, origin);
    await fn(page);
  } finally {
    await ctx.close();
  }
}
const rowsOf = async (id: number) => (await call('page_responses', { page_id: id })).body.count as number;

for (const vp of ROUND_VIEWPORTS) {
  test(`at ${vp.name}, no forge of rounds 1 and 2 records anything as the owner: each only opens muster's confirm`, async ({ browser }) => {
    await asOwner(browser, vp, async (page) => {
      const frame = page.frameLocator('iframe#page');
      const forged = await call('page_create', { title: 'Forge ' + vp.name + ' ' + stamp, blocks: FORGE_BLOCKS });
      const id = forged.body.page_id as number;
      await page.goto(origin + `/pages/${id}/view`);
      await expect(frame.locator('#f-req')).toHaveText('done');
      await expect.poll(() => frame.locator('body').evaluate(() => (window as unknown as { p2done?: number }).p2done)).toBe(1);
      // The first send opened the bar; every other one was refused while it
      // waits. None is recorded.
      await expect(confirmBar(page)).toBeVisible();
      await expect(confirmBar(page).locator('[data-send-summary]')).toHaveText('"s": "forge"');
      for (const f of ['#f-act', '#f-file']) await expect(frame.locator(f)).toHaveText('refused:Another send is waiting for your answer.');
      await expect(frame.locator('[data-muster-component="fq"] [data-muster-status]')).toHaveText('Another send is waiting for your answer.');
      await expect(frame.locator('[data-muster-component="up1"] [data-muster-status]')).toHaveText('Another send is waiting for your answer.');
      await page.waitForTimeout(1000);
      expect(await rowsOf(id), 'a forge recorded a row').toBe(0);
      await answerConfirm(page, '"s": "forge"', false);
      await expect(frame.locator('#f-data')).toHaveText('refused:Not sent');
      expect(await rowsOf(id), 'Cancel recorded a row').toBe(0);
    });
  });

  test(`at ${vp.name}, a page's script dispatching clicks, keys and focus cannot answer muster's confirm`, async ({ browser }) => {
    await asOwner(browser, vp, async (page) => {
      const frame = page.frameLocator('iframe#page');
      const h = await call('page_create', { title: 'Hostile ' + vp.name + ' ' + stamp, blocks: [{ type: 'html', html: HOSTILE }] });
      const id = h.body.page_id as number;
      await page.goto(origin + `/pages/${id}/view`);
      await expect(confirmBar(page)).toBeVisible();
      await page.waitForTimeout(2500);
      await expect(confirmBar(page), 'the page answered the bar').toBeVisible();
      await expect(frame.locator('#h-out')).toHaveText('-');
      expect(await rowsOf(id), 'the page confirmed its own send').toBe(0);
      // An Enter of the owner's own does not send either (Send is never
      // focused; the page keeps taking focus back anyway), and Cancel does not.
      await page.keyboard.press('Enter');
      await page.waitForTimeout(300);
      expect(await rowsOf(id)).toBe(0);
      await answerConfirm(page, '"hostile": 1', false);
      await expect(frame.locator('#h-out')).toHaveText('refused:Not sent');
      expect(await rowsOf(id)).toBe(0);
    });
  });

  test(`at ${vp.name}, the owner's Send records exactly what muster showed, Cancel nothing, and a link's viewer needs no confirm`, async ({ browser }) => {
    await asOwner(browser, vp, async (page) => {
      const frame = page.frameLocator('iframe#page');
      const ex = await call('page_create', { title: 'Exact ' + vp.name + ' ' + stamp, blocks: EXACT_BLOCKS });
      const id = ex.body.page_id as number;
      await page.goto(origin + `/pages/${id}/view`);
      const contact = frame.locator('[data-muster-component="contact"]');
      // Cancel: nothing.
      await contact.locator('input[name="email"]').fill('no@example.com');
      await contact.getByRole('button', { name: 'Send details' }).click();
      await answerConfirm(page, '"email": "no@example.com"', false);
      await expect(contact.locator('[data-muster-status]')).toHaveText('Not sent');
      expect(await rowsOf(id)).toBe(0);
      // A press of Send in the bar's first half second is ignored.
      await contact.locator('input[name="email"]').fill('me@example.com');
      await contact.locator('input[name="company"]').fill('Example Co');
      await contact.getByRole('button', { name: 'Send details' }).click();
      await expect(confirmBar(page)).toBeVisible();
      await confirmBar(page).locator('[data-send-go]').click();
      await expect(confirmBar(page), 'a press in the first 500 ms sent').toBeVisible();
      expect(await rowsOf(id)).toBe(0);
      // Send: exactly what was shown.
      await expect(confirmBar(page).locator('[data-send-what]')).toHaveText('To “contact”');
      await answerConfirm(page, '"email": "me@example.com"\n"company": "Example Co"', true);
      await expect(contact.locator('[data-muster-status]')).toHaveText('Sent. Thank you.');
      // Two files, one confirm, names and sizes shown.
      const files = frame.locator('[data-muster-component="files"]');
      await files.locator('input[type="file"]').setInputFiles([
        { name: 'one.txt', mimeType: 'text/plain', buffer: Buffer.from('hello') },
        { name: 'two.txt', mimeType: 'text/plain', buffer: Buffer.from('second') },
      ]);
      await files.getByRole('button', { name: 'Upload files' }).click();
      await expect(confirmBar(page).locator('[data-send-what]')).toHaveText('2 files to “files”');
      await answerConfirm(page, '"one.txt" (5 B)\n"two.txt" (6 B)', true);
      await expect(files.locator('[data-muster-status]')).toHaveText('Sent. Thank you.');
      const read = await call('page_responses', { page_id: id });
      expect(read.body.count, JSON.stringify(read.body)).toBe(3);
      const fenced = read.body.responses as string;
      expect(fenced).toContain('"email":"me@example.com"');
      expect(fenced).toContain('"company":"Example Co"');
      expect(fenced).toContain('"name":"one.txt"');
      expect(fenced).toContain('"name":"two.txt"');
      // (The preface names "by_owner":true too: count the rows' own.)
      expect(fenced.match(/"by_owner":true,"component"/g)?.length).toBe(3);
      // A link's viewer: no confirm, recorded at once.
      const shared = await page.request.post(origin + `/pages/${id}/share`, { headers: { Origin: origin, Accept: 'application/json' } });
      const stranger = await browser.newContext({ viewport: vp.viewport, hasTouch: vp.touch, isMobile: vp.touch });
      try {
        const sp = await stranger.newPage();
        await sp.goto((await shared.json()).url as string);
        const sc = sp.frameLocator('iframe#page').locator('[data-muster-component="contact"]');
        await sc.locator('input[name="email"]').fill('viewer@example.com');
        await sc.getByRole('button', { name: 'Send details' }).click();
        await expect(sc.locator('[data-muster-status]')).toHaveText('Sent. Thank you.');
        await expect(confirmBar(sp)).toHaveCount(0);
      } finally {
        await stranger.close();
      }
      expect(await rowsOf(id)).toBe(4);
    });
  });

  test(`at ${vp.name}, a page's DOMContentLoaded and load handlers run once, for the owner and a link's viewer`, async ({ browser }) => {
    await asOwner(browser, vp, async (page) => {
      const frame = page.frameLocator('iframe#page');
      const ready = await call('page_create', { title: 'Ready ' + vp.name + ' ' + stamp, blocks: [{ type: 'html', html: READY }] });
      const readyID = ready.body.page_id as number;
      await page.goto(origin + `/pages/${readyID}/view`);
      for (const [sel, what] of [['#dcl', 'document DOMContentLoaded'], ['#dclw', 'window DOMContentLoaded'], ['#onl', 'window.onload']]) {
        await expect(frame.locator(sel), 'the owner: ' + what).toHaveText('1');
      }
      const shared = await page.request.post(origin + `/pages/${readyID}/share`, { headers: { Origin: origin, Accept: 'application/json' } });
      const stranger = await browser.newContext({ viewport: vp.viewport, hasTouch: vp.touch, isMobile: vp.touch });
      try {
        const sp = await stranger.newPage();
        await sp.goto((await shared.json()).url as string);
        const sf = sp.frameLocator('iframe#page');
        for (const [sel, what] of [['#dcl', 'document DOMContentLoaded'], ['#dclw', 'window DOMContentLoaded'], ['#onl', 'window.onload']]) {
          await expect(sf.locator(sel), 'a link viewer: ' + what).toHaveText('1');
        }
      } finally {
        await stranger.close();
      }
      await page.waitForTimeout(800);
      for (const sel of ['#dcl', '#dclw', '#onl']) await expect(frame.locator(sel), sel + ' ran more than once').toHaveText('1');
    });
  });

  for (const [label, html, locked] of [
    ['round 1: stops its own scrolling', STUCK, `document.documentElement.style.overflow==='hidden'`],
    ['round 2 p6: re-dispatches scroll', P6_REDISPATCH, `window.locked===1`],
    ['round 2 p8: scrolls an inner element', P8_INNER, `document.documentElement.style.overflow==='hidden'`],
  ] as const) {
    test(`at ${vp.name}, a page cannot keep the header hidden (${label})`, async ({ browser }) => {
      await asOwner(browser, vp, async (page) => {
        const frame = page.frameLocator('iframe#page');
        const header = page.locator('header[data-page-brand]');
        const stuck = await call('page_create', { title: 'Stuck ' + vp.name + ' ' + stamp, blocks: [{ type: 'html', html }] });
        await page.goto(origin + `/pages/${stuck.body.page_id as number}/view`);
        // Only once the page has stopped its own scrolling: a scroll up
        // before that is a real one, and reports itself.
        await expect.poll(() => frame.locator('body').evaluate((_, c) => new Function('return ' + c)() as boolean, locked)).toBe(true);
        await expect(header, 'the page hid the header').toHaveAttribute('data-hidden', '');
        await page.mouse.move(vp.viewport.width / 2, vp.viewport.height / 2);
        await page.mouse.wheel(0, -400);
        await page.keyboard.press('PageUp');
        await expect(header, 'the header came back on its own').not.toHaveAttribute('data-hidden', { timeout: 5000 });
        // The control: an ordinary scroll down hides it, and it comes back by
        // the same rule once the scrolling stops.
        const long = await call('page_create', { title: 'Long ' + vp.name + ' ' + stamp, blocks: [{ type: 'markdown', text: LONG }] });
        await page.goto(origin + `/pages/${long.body.page_id as number}/view`);
        await expect(frame.locator('p').first()).toBeVisible();
        await page.mouse.move(vp.viewport.width / 2, vp.viewport.height / 2);
        await page.mouse.wheel(0, 900);
        await expect(header).toHaveAttribute('data-hidden', '');
        await expect(header, 'control: back once the scrolling stops').not.toHaveAttribute('data-hidden', { timeout: 5000 });
      });
    });
  }
}

// --- ROUND 3 ON #802 ----------------------------------------------------------

// A page whose own button lies under where muster's Send appears: the first
// tap sends (the bar opens over it), the rest of a burst lands on Send.
const UNDER_SEND = `<button id="under" type="button" style="position:fixed;left:0;right:0;bottom:0;height:260px;z-index:9;font-size:20px">Press me</button>
<p id="u-out">-</p><script>
document.getElementById('under').addEventListener('click',function(){
  muster.submit({burst:1}).then(function(r){document.getElementById('u-out').textContent='saved:'+r.id;},
    function(e){document.getElementById('u-out').textContent='refused:'+e.message;});
});</script>`;

// Fields that tried to hide or fake what the owner reads.
const TRICKY = `<p id="t-out">-</p><script>
window.sendIt=function(v){muster.submit(v).then(function(r){document.getElementById('t-out').textContent='saved:'+r.id;},
  function(e){document.getElementById('t-out').textContent='refused:'+e.message;});};
</script>`;

// A link viewer's page sending shapes JSON normalises (they were recorded so
// before the owner's confirm, and must still be).
const SHAPES = `<p id="s-out">-</p><script>
Promise.all([muster.submit({a:1,b:undefined}),muster.submit({when:new Date(0)}),muster.submit({x:NaN})])
  .then(function(){document.getElementById('s-out').textContent='sent';},function(e){document.getElementById('s-out').textContent='refused:'+e.message;});
</script>`;

// Round 3's a7: a page that scrolls itself down, then 1px every 2.5s,
// forever — every report "further down".
const A7_CREEP = `<div style="height:6000px">tall</div><script>
setTimeout(function(){window.scrollTo(0,800);window.crept=1;setInterval(function(){window.scrollBy(0,1);},2500);},500);
</script>`;

const pageButton = (p: Page) => p.frameLocator('iframe#page');

for (const vp of ROUND_VIEWPORTS) {
  test(`at ${vp.name}, a burst of taps cannot reach past the confirm's quiet window (round 3, 🟡-1)`, async ({ browser }) => {
    await asOwner(browser, vp, async (page) => {
      const frame = pageButton(page);
      const pg = await call('page_create', { title: 'Burst ' + vp.name + ' ' + stamp, blocks: [{ type: 'html', html: UNDER_SEND }] });
      const id = pg.body.page_id as number;
      await page.goto(origin + `/pages/${id}/view`);
      await expect(frame.locator('#under')).toBeVisible();
      // Where Send appears: open the bar once, read it, cancel.
      await frame.locator('#under').click();
      const go = confirmBar(page).locator('[data-send-go]');
      await expect(go).toBeVisible();
      const b = (await go.boundingBox())!;
      const x = b.x + b.width / 2, y = b.y + b.height / 2;
      await answerConfirm(page, /burst/, false);
      await expect(frame.locator('#u-out')).toHaveText('refused:Not sent');
      // The burst: the first tap is the page's button (the bar opens over
      // it), the rest land on Send.
      const tap = vp.touch ? () => page.touchscreen.tap(x, y) : () => page.mouse.click(x, y);
      // Taps 250 ms apart on a phone, 183 on a desktop: each lands inside
      // the window the one before it restarted.
      const gaps = vp.touch ? [0, 250, 250, 250] : [0, 183, 183, 182];
      for (const g of gaps) {
        if (g) await page.waitForTimeout(g);
        await tap();
      }
      await page.waitForTimeout(300);
      await expect(confirmBar(page), 'a burst answered the bar').toBeVisible();
      expect(await rowsOf(id), 'a burst recorded the send').toBe(0);
      // The control: a tap after a quiet half second sends.
      await page.waitForTimeout(700);
      await tap();
      await expect(confirmBar(page)).toBeHidden();
      await expect(frame.locator('#u-out')).toHaveText(/^saved:\d+$/);
      expect(await rowsOf(id)).toBe(1);
    });
  });

  test(`at ${vp.name}, the confirm shows every field, escaped, or refuses (round 3, 🟡-2)`, async ({ browser }) => {
    await asOwner(browser, vp, async (page) => {
      const frame = pageButton(page);
      const pg = await call('page_create', { title: 'Tricky ' + vp.name + ' ' + stamp, blocks: [{ type: 'html', html: TRICKY }] });
      const id = pg.body.page_id as number;
      await page.goto(origin + `/pages/${id}/view`);
      await expect(frame.locator('#t-out')).toHaveText('-');
      const summary = confirmBar(page).locator('[data-send-summary]');
      // A long first field does not push the second out of sight.
      await frame.locator('body').evaluate(() => (window as unknown as { sendIt: (v: unknown) => void }).sendIt({ pad: 'x'.repeat(700), approve_deploy: 'yes' }));
      await expect(summary, 'a field was cut out of sight').toContainText('approve_deploy');
      await expect(summary).toContainText('"approve_deploy": "yes"');
      await expect(summary).toContainText('"pad": "' + 'x'.repeat(700) + '"');
      await expect(confirmBar(page)).toHaveAccessibleDescription(/"approve_deploy": "yes"/);
      await answerConfirm(page, /approve_deploy/, false);
      // A newline cannot fake a second field; a direction override shows as
      // its escape.
      await frame.locator('body').evaluate(() => (window as unknown as { sendIt: (v: unknown) => void }).sendIt({ decision: 'reject\nreason: ok', who: '‮evil' }));
      await expect(summary).toHaveText('"decision": "reject\\nreason: ok"\n"who": "\\u202eevil"');
      expect(await summary.evaluate((e) => e.textContent!.split('\n').length), 'the newline made a line').toBe(2);
      await answerConfirm(page, /decision/, false);
      // Too much to show whole: refused, nothing shown in part.
      await frame.locator('body').evaluate(() => (window as unknown as { sendIt: (v: unknown) => void }).sendIt({ s: '‮'.repeat(11000) }));
      await expect(frame.locator('#t-out')).toHaveText('refused:This is too much to show you before sending; nothing was sent.');
      await expect(confirmBar(page)).toBeHidden();
      expect(await rowsOf(id)).toBe(0);
    });
  });

  test(`at ${vp.name}, a link's viewer still records undefined, a Date and NaN as JSON does (round 3, 🟡-3)`, async ({ browser }) => {
    await asOwner(browser, vp, async (page) => {
      const pg = await call('page_create', { title: 'Shapes ' + vp.name + ' ' + stamp, blocks: [{ type: 'html', html: SHAPES }] });
      const id = pg.body.page_id as number;
      const shared = await page.request.post(origin + `/pages/${id}/share`, { headers: { Origin: origin, Accept: 'application/json' } });
      const stranger = await browser.newContext({ viewport: vp.viewport, hasTouch: vp.touch, isMobile: vp.touch });
      try {
        const sp = await stranger.newPage();
        await sp.goto((await shared.json()).url as string);
        await expect(sp.frameLocator('iframe#page').locator('#s-out')).toHaveText('sent');
      } finally {
        await stranger.close();
      }
      const read = await call('page_responses', { page_id: id });
      expect(read.body.count, JSON.stringify(read.body)).toBe(3);
      const fenced = read.body.responses as string;
      expect(fenced).toContain('"data":{"value":{"a":1}}');
      expect(fenced).toContain('"data":{"value":{"when":"1970-01-01T00:00:00.000Z"}}');
      expect(fenced).toContain('"data":{"value":{"x":null}}');
    });
  });

  test(`at ${vp.name}, a page creeping down by script cannot hold the header hidden (round 3, 🟡-4)`, async ({ browser }) => {
    test.setTimeout(90_000);
    await asOwner(browser, vp, async (page) => {
      const frame = pageButton(page);
      const header = page.locator('header[data-page-brand]');
      const pg = await call('page_create', { title: 'Creep ' + vp.name + ' ' + stamp, blocks: [{ type: 'html', html: A7_CREEP }] });
      await page.goto(origin + `/pages/${pg.body.page_id as number}/view`);
      await expect.poll(() => frame.locator('body').evaluate(() => (window as unknown as { crept?: number }).crept)).toBe(1);
      await expect(header, 'the page hid the header').toHaveAttribute('data-hidden', '');
      // Back within the 8s cap, whatever the creep reports…
      await expect(header, 'the header came back within its cap').not.toHaveAttribute('data-hidden', { timeout: 9500 });
      // …and the creep (1px at a time) does not hide it again.
      await page.waitForTimeout(6000);
      await expect(header, 'the creep hid it again').not.toHaveAttribute('data-hidden');
      // The control: a real scroll down hides it again.
      await page.mouse.move(vp.viewport.width / 2, vp.viewport.height / 2);
      await page.mouse.wheel(0, 900);
      await expect(header).toHaveAttribute('data-hidden', '');
    });
  });

  test(`at ${vp.name}, the confirm gives up on its own before the page's wait does (round 3, 🟢-2)`, async ({ browser }) => {
    await asOwner(browser, vp, async (page) => {
      await page.clock.install();
      const frame = pageButton(page);
      const pg = await call('page_create', { title: 'Waits ' + vp.name + ' ' + stamp, blocks: [{ type: 'html', html: TRICKY }] });
      const id = pg.body.page_id as number;
      await page.goto(origin + `/pages/${id}/view`);
      await expect(frame.locator('#t-out')).toHaveText('-');
      await frame.locator('body').evaluate(() => (window as unknown as { sendIt: (v: unknown) => void }).sendIt({ late: 1 }));
      await expect(confirmBar(page)).toBeVisible();
      await page.clock.fastForward('10:01');
      await expect(confirmBar(page), 'the bar still waits after 10 minutes').toBeHidden();
      await expect(frame.locator('#t-out')).toHaveText('refused:Not sent');
      expect(await rowsOf(id)).toBe(0);
    });
  });
}

// --- THE SESSION THAT MADE A PAGE -------------------------------------------
// The operator: "when I'm looking at a page, I want to be able to see the
// session that created [it], so I can hit one tap [and] jump to that
// session." A page made by this spec's mirrored Claude Code session (its MCP
// call) records the session; the owner's header has its button; one tap
// lands on that session's rendered view, at its current window.
for (const vp of ROUND_VIEWPORTS) {
  test(`at ${vp.name}, one tap on the page's session button lands on the session that made it`, async ({ browser }) => {
    const made = await call('page_create', { title: 'From my session ' + vp.name + ' ' + stamp, blocks: [{ type: 'markdown', text: 'made here' }] });
    expect(made.isError, JSON.stringify(made.body)).toBe(false);
    const id = made.body.page_id as number;
    await asOwner(browser, vp, async (page) => {
      await page.goto(origin + `/pages/${id}/view`);
      const sess = page.locator('header [data-page-session]');
      await expect(sess).toBeVisible();
      const href = await sess.getAttribute('href');
      expect(href, 'the button links the mirrored window of the session').toMatch(new RegExp(`^/mirror/view\\?host=${host}&session=[^&]+&window=%40\\d+$`));
      const box = (await sess.boundingBox())!;
      expect(box.height, 'the button fits the slim header').toBeLessThanOrEqual(32);
      // The tap targets of the header's two buttons (their ::after) are 44px
      // tall, and stay INSIDE the 40px header: the whole header's height at
      // their middle is the button, and the page just below it is the page.
      for (const sel of ['[data-page-session]', '[data-version-chip]']) {
        const b = (await page.locator('header ' + sel).boundingBox())!;
        const at = (y: number) => page.evaluate(([px, py, s]) => !!document.elementFromPoint(px as number, py as number)?.closest(s as string), [b.x + b.width / 2, y, sel]);
        for (const y of [1, 20, 39]) expect(await at(y), `${sel}'s target at y=${y}`).toBe(true);
        // y=40.25: the page's first quarter-pixel row, just below the header.
        expect.soft(await at(40.25), `${sel}'s target overhangs the header onto the page`).toBe(false);
        expect(await page.locator('header ' + sel).evaluate((e) => getComputedStyle(e, '::after').height), `${sel}'s target height`).toBe('44px');
      }
      await shot(page, `owner-${vp.name}-session`);
      if (vp.touch) await sess.tap();
      else await sess.click();
      await page.waitForURL((u) => u.pathname === '/mirror/view');
      expect(page.url()).toBe(origin + href);
      await expect(page.locator('body')).not.toContainText('not found');
    });
    // A link's viewer: no button.
    await asOwner(browser, vp, async (page) => {
      const shared = await page.request.post(origin + `/pages/${id}/share`, { headers: { Origin: origin, Accept: 'application/json' } });
      const stranger = await browser.newContext({ viewport: vp.viewport, hasTouch: vp.touch, isMobile: vp.touch });
      try {
        const sp = await stranger.newPage();
        await sp.goto((await shared.json()).url as string);
        await expect(sp.frameLocator('iframe#page').locator('main')).toContainText('made here');
        await expect(sp.locator('[data-page-session]')).toHaveCount(0);
      } finally {
        await stranger.close();
      }
    });
  });
}

// THE PAGE IS IN THE APP'S THEME (internal/pages render.go Content and Shell,
// content_theme.css): the page's own top bar is the app header's ground, and
// in the frame a markdown page's ground is the app's background, its
// table cells are ruled in the palette's line colour, and its links are the
// app's accent, for the owner and for a link's viewer.
//
// The expected colours are the palette's, written out from web/css/tokens.css,
// and cross-checked against the app's own document in the same browser. The
// app has ONE theme (dark: every document carries class="dark" and the theme
// head forces it), so the page is checked in that one; a link's viewer whose
// device prefers light still gets it.
//
// MUSTER_E2E_PAGES_SHOTS=<dir> saves the owner's view there.
const THEME = {
  dark: { bg: 'rgb(17, 15, 28)', accent: 'rgb(228, 181, 86)', line: 'rgb(47, 42, 72)', text: 'rgb(238, 235, 247)' },
} as const;
const THEMED_MD =
  '## Prices\n\nEach plan, per seat:\n\n| Plan | Price | Notes |\n| --- | --- | --- |\n| Basic | 4 | [what is in it](https://example.com/basic) |\n| Team | 9 | everything in Basic |\n';

for (const vp of ROUND_VIEWPORTS) {
  test(`at ${vp.name}, a markdown page is drawn in the app's theme: its ground, its table's rules and its links`, async ({ browser }) => {
    const made = await call('page_create', { title: 'Themed ' + vp.name + ' ' + stamp, blocks: [{ type: 'markdown', text: THEMED_MD }] });
    expect(made.isError, JSON.stringify(made.body)).toBe(false);
    const id = made.body.page_id as number;
    const look = (f: ReturnType<Page['frameLocator']>) =>
      f.locator('table').evaluate((tbl) => {
        const td = tbl.querySelector('td')!;
        const a = tbl.querySelector('a')!;
        const c = getComputedStyle(td);
        return {
          bg: getComputedStyle(document.body).backgroundColor,
          border: [c.borderTopWidth, c.borderTopStyle, c.borderTopColor, c.borderLeftWidth, c.borderLeftStyle],
          link: getComputedStyle(a).color,
        };
      });
    await asOwner(browser, vp, async (page) => {
      // Control: the app's own ground and header, from the app's own document.
      await page.goto(origin + '/pages');
      const appDark = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
      const appHeader = await page.locator('header').first().evaluate((h) => getComputedStyle(h).backgroundColor);
      expect(appHeader, "control: the app's header is drawn on its dark ground").toBe(THEME.dark.bg);
      expect(appDark, "control: the app's dark ground is the palette's").toBe(THEME.dark.bg);

      await page.goto(origin + `/pages/${id}/view`);
      const frame = page.frameLocator('iframe#page');
      await expect(frame.locator('td').first()).toHaveText('Basic');
      await shot(page, `theme-${vp.name}-dark`);
      // The page's own top bar is the app's header: its ground and its rule.
      const bar = await page.locator('header[data-page-brand]').evaluate((h) => {
        const c = getComputedStyle(h);
        return { bg: c.backgroundColor, rule: c.borderBottomColor, text: getComputedStyle(h.querySelector('[data-page-title]')!).color };
      });
      expect(bar.bg, "the page's top bar is the app header's ground, not white").toBe(appHeader);
      expect(bar.rule, "the top bar's rule is the palette's line").toBe(THEME.dark.line);
      expect(bar.text, "the top bar's title is the palette's text").toBe(THEME.dark.text);
      const dark = await look(frame);
      expect(dark.bg, "the frame's ground is the app's dark background").toBe(THEME.dark.bg);
      expect(dark.border, 'a table cell is ruled: 1px solid, in the line colour').toEqual(['1px', 'solid', THEME.dark.line, '1px', 'solid']);
      expect(dark.link, "a link is the app's accent").toBe(THEME.dark.accent);

      // A link's viewer, on a device that prefers light, sees the same document.
      const shared = await page.request.post(origin + `/pages/${id}/share`, { headers: { Origin: origin, Accept: 'application/json' } });
      const stranger = await browser.newContext({ viewport: vp.viewport, hasTouch: vp.touch, isMobile: vp.touch, colorScheme: 'light' });
      try {
        const sp = await stranger.newPage();
        await sp.goto((await shared.json()).url as string);
        const sf = sp.frameLocator('iframe#page');
        await expect(sf.locator('td').first()).toHaveText('Basic');
        expect(await look(sf), "a link's viewer on a light-scheme device: the same theme").toEqual(dark);
      } finally {
        await stranger.close();
      }
    });
  });
}

// --- THE GALLERY BLOCK (internal/pages gallery.go; docs/pages.md#gallery) ---
// A mirrored session attaches six real PNGs (each a different width, so which
// one is on screen is measurable) and publishes a gallery of them. Every
// gesture is a real one: touches are CDP touch events through the browser's
// own gesture handling (lib/touch.ts), keys are keys, Back is the browser's
// history traversal of the TOP window (what a phone's Back and an edge swipe
// do; in the Android app, ShellBack's goBack()).

const GALLERY_N = 6;
const galleryWidth = (i: number) => 120 + i * 10;
let galleryRefs: string[] = [];

// galleryPage makes a page and attaches the six images to it (galleryRefs:
// their refs, in order), then publishes a gallery of them — captions 1a..2c in
// groups "Round 1" and "Round 2" — with pick (none: a gallery to look at).
async function galleryPage(title: string, pick: Record<string, unknown> | null, extra: Record<string, unknown>[] = []): Promise<number> {
  const created = await call('page_create', { title: title + ' ' + stamp, blocks: [{ type: 'markdown', text: 'Images coming.' }] });
  expect(created.isError, JSON.stringify(created.body)).toBe(false);
  const id = created.body.page_id as number;
  const refs: string[] = [];
  for (let i = 0; i < GALLERY_N; i++) {
    const file = `gallery-${i}.png`;
    if (!fs.existsSync(path.join(workDir, file))) fs.writeFileSync(path.join(workDir, file), noisePNG(galleryWidth(i), 90));
    const up = await call('page_asset_upload', { page_id: id, path: file });
    expect(up.isError, JSON.stringify(up.body)).toBe(false);
    refs.push(up.body.ref as string);
  }
  galleryRefs = refs;
  const items = refs.map((asset, i) => ({ asset, caption: `${Math.floor(i / 3) + 1}${'abc'[i % 3]}`, group: `Round ${Math.floor(i / 3) + 1}` }));
  const block: Record<string, unknown> = { type: 'gallery', id: 'logos', label: 'Pick the logos', items };
  if (pick) block.pick = pick;
  const upd = await call('page_update', { page_id: id, blocks: [{ type: 'markdown', text: '# Logo review' }, block, ...extra] });
  expect(upd.isError, JSON.stringify(upd.body)).toBe(false);
  return id;
}

type Touch = { x: number; y: number; id?: number };
async function touches(page: Page, steps: { type: 'touchStart' | 'touchMove' | 'touchEnd'; pts: Touch[] }[], gap = 16) {
  const cdp = await page.context().newCDPSession(page);
  for (const s of steps) {
    await cdp.send('Input.dispatchTouchEvent', { type: s.type, touchPoints: s.pts.map((p, i) => ({ x: p.x, y: p.y, id: p.id ?? i })) });
    await page.waitForTimeout(gap);
  }
  await cdp.detach();
}
const tapAt = (page: Page, p: Touch) => touches(page, [{ type: 'touchStart', pts: [p] }, { type: 'touchEnd', pts: [] }], 30);
async function doubleTap(page: Page, p: Touch) {
  await tapAt(page, p);
  await page.waitForTimeout(60);
  await tapAt(page, p);
}
// pinch spreads two fingers from 40px apart to 240px apart around c.
async function pinch(page: Page, c: Touch) {
  const steps: { type: 'touchStart' | 'touchMove' | 'touchEnd'; pts: Touch[] }[] = [{ type: 'touchStart', pts: [{ x: c.x - 20, y: c.y }, { x: c.x + 20, y: c.y }] }];
  for (let i = 1; i <= 10; i++) steps.push({ type: 'touchMove', pts: [{ x: c.x - 20 - i * 10, y: c.y }, { x: c.x + 20 + i * 10, y: c.y }] });
  steps.push({ type: 'touchEnd', pts: [] });
  await touches(page, steps);
}

// viewerEntry is 1 while the shell's history entry for an open viewer is the
// current one, else 0.
const viewerEntry = (page: Page) => page.evaluate(() => (history.state as { musterViewer?: number } | null)?.musterViewer ?? 0);

// viewerOf is the gallery's full-size viewer in the frame, and what it shows.
function viewerOf(page: Page) {
  const frame = page.frameLocator('iframe#page');
  const dlg = frame.locator('[data-muster-viewer="logos"]');
  const track = dlg.locator('.mp-viewer-track');
  return {
    frame,
    dlg,
    track,
    count: dlg.locator('.mp-viewer-count'),
    cap: dlg.locator('.mp-viewer-cap'),
    // The slide the track is scrolled to: its index, its image's width as
    // decoded, and its zoom.
    shown: () =>
      track.evaluate((t: HTMLElement) => {
        const i = Math.round(t.scrollLeft / t.clientWidth);
        const im = t.children[i]?.querySelector('img') as HTMLImageElement | null;
        return { i, w: im && im.complete ? im.naturalWidth : 0, zoom: Number(im?.getAttribute('data-muster-zoom') ?? '1') };
      }),
    // drawn is how the current slide's picture is DRAWN: its natural size
    // scaled to fit its img's box (object-fit:contain letterboxes it in the
    // box; an img box of exactly the natural size draws it at 1x), beside the
    // track's size, and the zoom transform's offset and scale.
    drawn: () =>
      track.evaluate((t: HTMLElement) => {
        const i = Math.round(t.scrollLeft / t.clientWidth);
        const im = t.children[i]?.querySelector('img') as HTMLImageElement;
        const nw = im.naturalWidth, nh = im.naturalHeight, bw = im.clientWidth, bh = im.clientHeight;
        const s = nw && nh ? Math.min(bw / nw, bh / nh) : 0;
        const m = new DOMMatrix(getComputedStyle(im).transform === 'none' ? '' : getComputedStyle(im).transform);
        return { nw, nh, w: nw * s, h: nh * s, tw: t.clientWidth, th: t.clientHeight, x: m.m41, y: m.m42, z: m.a };
      }),
    // rest waits for the track to come to rest on a slide (a swipe's snap
    // finished): a finger lands on a still image, as a person's does.
    rest: () =>
      expect
        .poll(
          () =>
            track.evaluate(async (t: HTMLElement) => {
              const a = t.scrollLeft;
              await new Promise((r) => setTimeout(r, 120));
              return a === t.scrollLeft && Math.abs(a - Math.round(a / t.clientWidth) * t.clientWidth) < 1;
            }),
          { message: 'the track comes to rest on a slide' },
        )
        .toBe(true),
  };
}

test('a gallery on a phone: tap opens full size, a swipe moves on, pinch and double-tap zoom and stop the swipe, Back closes it and stays, a second Back leaves', async ({ browser }) => {
  test.setTimeout(150_000);
  const id = await galleryPage('Gallery phone', { multiple: true });
  await asOwner(browser, ROUND_VIEWPORTS[0], async (page) => {
    // Somewhere to go back to: the Pages screen, then the page.
    await page.goto(origin + '/pages');
    await page.goto(origin + `/pages/${id}/view`);
    const v = viewerOf(page);
    const open1 = v.frame.getByRole('button', { name: 'Open 1a full size' });
    await expect(open1).toBeVisible();
    await expect.poll(() => open1.locator('img').evaluate((i: HTMLImageElement) => i.complete && i.naturalWidth), { timeout: 30_000 }).toBe(galleryWidth(0));
    await expect(v.frame.locator('.mp-gallery-group')).toHaveText(['Round 1', 'Round 2']);
    await shot(page, 'gallery-phone-grid');
    const before = await page.evaluate(() => history.length);

    // Tap the first thumbnail: the viewer, image 1, its counter and caption.
    await open1.tap();
    await expect(v.dlg).toBeVisible();
    await expect(v.count).toHaveText('1 / 6');
    await expect(v.cap).toHaveText('1a');
    await expect.poll(v.shown).toEqual({ i: 0, w: galleryWidth(0), zoom: 1 });
    await expect(v.dlg.getByRole('button', { name: 'Close full size' })).toBeFocused();
    // A small image (120x90 on a 390-wide phone) is ENLARGED to fill the
    // viewer's width or height, not drawn at its own size in the middle.
    {
      const d = await v.drawn();
      expect(d.nw, 'the first image is decoded').toBe(galleryWidth(0));
      expect(d.w, `drawn ${d.w}x${d.h} in a ${d.tw}x${d.th} viewer: larger than its natural ${d.nw}`).toBeGreaterThan(d.nw * 2);
      expect(Math.abs(d.w - d.tw) <= 2 || Math.abs(d.h - d.th) <= 2, `drawn ${d.w}x${d.h} fills the ${d.tw}x${d.th} viewer's width or height`).toBe(true);
    }
    await shot(page, 'gallery-phone-viewer');

    // A swipe left: image 2 (snapped, its neighbour already loaded).
    const tb = (await page.locator('iframe#page').boundingBox())!;
    const box = (await v.track.boundingBox())!;
    const mid = { x: tb.x + box.x + box.width / 2, y: tb.y + box.y + box.height / 2 };
    await touchDrag(page, { x: mid.x + 120, y: mid.y }, { x: mid.x - 160, y: mid.y });
    await v.rest();
    await expect(v.count).toHaveText('2 / 6');
    await expect(v.cap).toHaveText('1b');
    await expect.poll(v.shown).toEqual({ i: 1, w: galleryWidth(1), zoom: 1 });

    // Double-tap zooms in; a swipe then pans, it does not move on.
    await doubleTap(page, mid);
    await expect.poll(async () => (await v.shown()).zoom, 'a double-tap zooms in').toBeGreaterThan(1);
    await expect(v.dlg).toHaveClass(/mp-zoomed/);
    await shot(page, 'gallery-phone-zoomed');
    await touchDrag(page, { x: mid.x + 120, y: mid.y }, { x: mid.x - 160, y: mid.y });
    await page.waitForTimeout(400);
    await expect(v.count, 'no swipe while zoomed').toHaveText('2 / 6');
    expect((await v.shown()).i).toBe(1);
    // A pan stops at the PICTURE's edges, not the viewer's: dragged far down,
    // the zoomed picture moves only as far as its drawn height overhangs the
    // track (none at all when it is shorter than the track).
    await touchDrag(page, { x: mid.x, y: mid.y - 150 }, { x: mid.x, y: mid.y + 250 });
    await page.waitForTimeout(200);
    {
      const d = await v.drawn();
      const room = Math.max(0, (d.z * d.h - d.th) / 2);
      expect(d.z, 'still zoomed').toBeGreaterThan(1);
      expect(Math.abs(d.y), `panned ${d.y}px down; the ${d.z}x picture (${d.z * d.h}px high in a ${d.th}px track) has ${room}px of room`).toBeLessThanOrEqual(room + 1);
    }
    // A second double-tap zooms back out; the swipe works again.
    await doubleTap(page, mid);
    await expect.poll(async () => (await v.shown()).zoom).toBe(1);

    // Pinch: two fingers apart zoom in, and again no swipe.
    await pinch(page, mid);
    await expect.poll(async () => (await v.shown()).zoom, 'a pinch zooms in').toBeGreaterThan(2);
    await touchDrag(page, { x: mid.x + 120, y: mid.y }, { x: mid.x - 160, y: mid.y });
    await page.waitForTimeout(400);
    await expect(v.count, 'no swipe while pinched in').toHaveText('2 / 6');
    await doubleTap(page, mid);
    await expect.poll(async () => (await v.shown()).zoom).toBe(1);
    await touchDrag(page, { x: mid.x + 120, y: mid.y }, { x: mid.x - 160, y: mid.y });
    await v.rest();
    await expect(v.count, 'zoomed out, the swipe moves on again').toHaveText('3 / 6');

    // Back closes the viewer and stays on the page.
    expect(await viewerEntry(page), 'opening pushed the viewer entry').toBe(1);
    expect(await page.evaluate(() => history.length), 'opening pushed one entry').toBe(before + 1);
    await page.goBack({ waitUntil: 'commit' }).catch(() => null);
    await expect(v.dlg).toBeHidden();
    expect(page.url()).toBe(origin + `/pages/${id}/view`);
    await expect(open1, 'focus goes back to the thumbnail it opened from').toBeFocused();
    // A second Back leaves the page.
    await Promise.all([page.waitForURL(origin + '/pages'), page.goBack()]);
  });
});

test('a gallery leaves no history entry behind: opens closed by ×, Esc, a tap beside the image and Back, then one Back leaves', async ({ browser }) => {
  const id = await galleryPage('Gallery history', null);
  await asOwner(browser, ROUND_VIEWPORTS[1], async (page) => {
    await page.goto(origin + '/pages');
    await page.goto(origin + `/pages/${id}/view`);
    const v = viewerOf(page);
    const open2 = v.frame.getByRole('button', { name: 'Open 1b full size' });
    await expect(open2).toBeVisible();
    const before = await page.evaluate(() => history.length);
    const closes: [string, () => Promise<void>][] = [
      ['×', () => v.dlg.getByRole('button', { name: 'Close full size' }).click()],
      ['Esc', () => page.keyboard.press('Escape')],
      ['a tap beside the image', () => v.track.click({ position: { x: 20, y: 20 } })],
      // Back closes it too, and stays on the page.
      ['Back', async () => void (await page.goBack({ waitUntil: 'commit' }).catch(() => null))],
    ];
    for (const [how, close] of closes) {
      await open2.click();
      await expect(v.dlg).toBeVisible();
      await expect.poll(() => viewerEntry(page), `${how}: the shell pushed its entry`).toBe(1);
      expect(await page.evaluate(() => history.length), `${how}: one entry while open`).toBe(before + 1);
      await close();
      await expect(v.dlg, `${how} closes it`).toBeHidden();
      expect(page.url(), `${how}: still on the page`).toBe(origin + `/pages/${id}/view`);
      await expect(open2, `${how}: focus returns to the thumbnail`).toBeFocused();
      await expect.poll(() => viewerEntry(page), `${how}: its entry is popped`).toBe(0);
    }
    expect(await page.evaluate(() => history.length), 'no entry piled up').toBeLessThanOrEqual(before + 1);
    // The next Back is the page's own: it leaves.
    await Promise.all([page.waitForURL(origin + '/pages'), page.goBack()]);
  });
});

test('a gallery survives a slow history pop: reopened while the close is still popping, it stays open with ONE entry; Forward into a closed viewer\'s entry and reopening adds none', async ({ browser }) => {
  test.setTimeout(90_000);
  const id = await galleryPage('Gallery slow pop', null);
  await asOwner(browser, ROUND_VIEWPORTS[1], async (page) => {
    await page.goto(origin + '/pages');
    await page.goto(origin + `/pages/${id}/view`);
    const v = viewerOf(page);
    const open2 = v.frame.getByRole('button', { name: 'Open 1b full size' });
    const close = () => v.dlg.getByRole('button', { name: 'Close full size' }).click();
    await expect(open2).toBeVisible();
    const before = await page.evaluate(() => history.length);
    // A slow traversal, as on a cold or loaded phone: the shell's history.back()
    // lands 1.5 s after it is called (a back() is queued, and its target is
    // counted when it runs, so this is the same race).
    await page.evaluate(() => {
      const back = history.back.bind(history);
      (history as unknown as { back: () => void }).back = () => void setTimeout(back, 1500);
    });
    await open2.click();
    await expect.poll(() => viewerEntry(page)).toBe(1);
    await close();
    await expect(v.dlg).toBeHidden();
    // Reopened while that pop is still on its way.
    await open2.click();
    await expect(v.dlg).toBeVisible();
    // Past the pop, and past any one-second guess at it.
    await page.waitForTimeout(2500);
    await expect(v.dlg, 'the late pop does not close the reopened viewer').toBeVisible();
    expect(await viewerEntry(page), "the viewer's entry is current while it is open").toBe(1);
    expect(await page.evaluate(() => history.length), 'one entry while open').toBe(before + 1);
    await close();
    await expect(v.dlg).toBeHidden();
    await expect.poll(() => viewerEntry(page), { message: 'the close pops its entry', timeout: 5000 }).toBe(0);
    await page.evaluate(() => void delete (history as unknown as { back?: unknown }).back);

    // Back closes it; Forward lands on that closed viewer's entry; opening
    // takes the entry over rather than pushing another.
    await open2.click();
    await expect.poll(() => viewerEntry(page)).toBe(1);
    await page.goBack({ waitUntil: 'commit' }).catch(() => null);
    await expect(v.dlg).toBeHidden();
    await page.goForward({ waitUntil: 'commit' }).catch(() => null);
    await expect.poll(() => viewerEntry(page), 'Forward is on the old viewer entry').toBe(1);
    await expect(v.dlg, 'Forward does not open the viewer').toBeHidden();
    await open2.click();
    await expect(v.dlg).toBeVisible();
    expect(await page.evaluate(() => history.length), 'reopening on that entry adds none').toBe(before + 1);
    await close();
    await expect.poll(() => viewerEntry(page)).toBe(0);
    // The next Back is the page's own: it leaves.
    await Promise.all([page.waitForURL(origin + '/pages'), page.goBack()]);
  });
});

// FORGED_GALLERIES is an html block's markup and script posing as 20 hidden
// picking galleries: the SDK's gallery script takes every gallery-shaped
// section, so a tap on each fake Pick makes it send a keep for an id the page
// does not have.
const FORGED_GALLERIES =
  Array.from({ length: 20 }, (_, i) =>
    `<section hidden data-muster-kind="gallery" data-muster-component="forged${i}" data-muster-pick="several"><ul><li data-muster-item="0"><button type="button" class="mp-gallery-pick">P</button></li></ul></section>`,
  ).join('') +
  `<p id="forged-out"></p><script>document.addEventListener('DOMContentLoaded',function(){var b=document.querySelectorAll('section[data-muster-component^="forged"] .mp-gallery-pick');for(var i=0;i<b.length;i++)b[i].click();document.getElementById('forged-out').textContent='clicked:'+b.length;});</script>`;

test('a page script forging gallery sections keeps nothing: only the version\'s own pick gallery is kept in the shell\'s sessionStorage', async ({ browser }) => {
  const id = await galleryPage('Gallery forged keeps', { multiple: true }, [{ type: 'html', html: FORGED_GALLERIES }]);
  await asOwner(browser, ROUND_VIEWPORTS[0], async (page) => {
    await page.goto(origin + `/pages/${id}/view`);
    const v = viewerOf(page);
    await expect(v.frame.locator('#forged-out')).toHaveText('clicked:20');
    // The real gallery's pick IS kept (the positive control: keeps work and
    // this read sees them). The port is in order, so once it is stored every
    // forged keep sent before it has been answered.
    await v.frame.locator('.mp-gallery-grid').getByRole('button', { name: 'Pick 1a' }).tap();
    const keys = () => page.evaluate(() => Object.keys(sessionStorage).filter((k) => k.startsWith('muster-picks:')));
    await expect.poll(async () => (await keys()).filter((k) => k.endsWith(':logos')).length, 'the real pick is kept').toBe(1);
    const all = await keys();
    expect(all.filter((k) => k.includes('forged')), 'no key for a forged gallery: ' + JSON.stringify(all)).toEqual([]);
    expect(all.length).toBe(1);
  });
});

test('a gallery on a desktop: the arrows and ← / → move through every item, the dialog keeps focus, Esc closes', async ({ browser }) => {
  const id = await galleryPage('Gallery desktop', null);
  await asOwner(browser, ROUND_VIEWPORTS[1], async (page) => {
    await page.goto(origin + `/pages/${id}/view`);
    const v = viewerOf(page);
    await v.frame.getByRole('button', { name: 'Open 2c full size' }).click();
    await expect(v.count).toHaveText('6 / 6');
    await page.keyboard.press('ArrowLeft');
    await expect(v.count).toHaveText('5 / 6');
    await expect.poll(v.shown).toEqual({ i: 4, w: galleryWidth(4), zoom: 1 });
    await page.keyboard.press('ArrowRight');
    await expect(v.count).toHaveText('6 / 6');
    // At the end, → stays.
    await page.keyboard.press('ArrowRight');
    await page.waitForTimeout(300);
    await expect(v.count).toHaveText('6 / 6');
    await v.dlg.getByRole('button', { name: 'Previous image' }).click();
    await expect(v.count).toHaveText('5 / 6');
    await v.dlg.getByRole('button', { name: 'Next image' }).click();
    await expect(v.count).toHaveText('6 / 6');
    await expect.poll(v.shown).toEqual({ i: 5, w: galleryWidth(5), zoom: 1 });
    // Tab cycles inside the dialog.
    for (let i = 0; i < 6; i++) {
      await page.keyboard.press('Tab');
      expect(await v.frame.locator('body').evaluate(() => !!document.activeElement?.closest('[data-muster-viewer]')), 'focus stays in the dialog').toBe(true);
    }
    // A double-click zooms on a desktop (on the image, once the track is at rest).
    await v.rest();
    await v.track.dblclick();
    await expect.poll(async () => (await v.shown()).zoom).toBeGreaterThan(1);
    await page.keyboard.press('Escape');
    await expect(v.dlg).toBeHidden();
    await expect(v.frame.getByRole('button', { name: 'Open 2c full size' })).toBeFocused();
  });
});

test('gallery picks: grid and viewer toggle the same picks, a reload keeps them, Save sends ONCE as the owner, a link viewer double-tapping Save records one', async ({ browser }) => {
  test.setTimeout(150_000);
  const id = await galleryPage('Gallery picks', { multiple: true, required: true, submit_label: 'Save picks' });
  await asOwner(browser, ROUND_VIEWPORTS[0], async (page) => {
    await page.goto(origin + `/pages/${id}/view`);
    const v = viewerOf(page);
    const item = (n: number) => v.frame.locator(`li[data-muster-item="${n}"]`);
    const pickBtn = (name: string) => v.frame.locator('.mp-gallery-grid').getByRole('button', { name: 'Pick ' + name });
    const bar = v.frame.locator('.mp-gallery-bar');
    await expect(bar.locator('[data-muster-picked]')).toHaveText('0 picked');
    // Required: an empty Save asks for a pick and sends nothing.
    await bar.getByRole('button', { name: 'Save picks' }).tap();
    await expect(bar.locator('[data-muster-status]')).toHaveText('Pick at least one first.');
    await expect(confirmBar(page)).toBeHidden();

    // Pick 1a in the grid: its badge, its button pressed.
    await pickBtn('1a').tap();
    await expect(pickBtn('1a')).toHaveAttribute('aria-pressed', 'true');
    await expect(item(0).locator('.mp-gallery-badge')).toBeVisible();
    await expect(item(1).locator('.mp-gallery-badge')).toBeHidden();
    await expect(bar.locator('[data-muster-picked]')).toHaveText('1 picked');
    // The viewer shows the same pick, and picks 1b: the grid follows.
    await v.frame.getByRole('button', { name: 'Open 1a full size' }).tap();
    const vpick = v.dlg.locator('.mp-viewer-pick');
    await expect(vpick).toHaveAttribute('aria-label', 'Pick 1a');
    await expect(vpick).toHaveAttribute('aria-pressed', 'true');
    await page.keyboard.press('ArrowRight');
    await v.rest();
    await expect(vpick).toHaveAttribute('aria-label', 'Pick 1b');
    await expect(vpick).toHaveAttribute('aria-pressed', 'false');
    await vpick.tap();
    await expect(vpick).toHaveAttribute('aria-pressed', 'true');
    await shot(page, 'gallery-phone-viewer-picked');
    await v.dlg.getByRole('button', { name: 'Close full size' }).tap();
    await expect(pickBtn('1b')).toHaveAttribute('aria-pressed', 'true');
    await expect(bar.locator('[data-muster-picked]')).toHaveText('2 picked');
    expect(await rowsOf(id), 'a pick sends nothing').toBe(0);
    await expect.poll(() => viewerEntry(page), "the viewer's entry is popped before the reload").toBe(0);

    // A reload keeps the unsaved picks.
    await page.reload();
    await expect(pickBtn('1a')).toHaveAttribute('aria-pressed', 'true');
    await expect(pickBtn('1b')).toHaveAttribute('aria-pressed', 'true');
    await expect(pickBtn('1c')).toHaveAttribute('aria-pressed', 'false');
    await expect(bar.locator('[data-muster-picked]')).toHaveText('2 picked');
    await shot(page, 'gallery-phone-picked-grid');

    // Save, twice in a row: ONE confirm, ONE response, the owner's.
    const save = bar.getByRole('button', { name: 'Save picks' });
    await save.dblclick();
    await answerConfirm(page, `"picks": [{"index":0,"asset":"${galleryRefs[0]}","caption":"1a"},{"index":1,"asset":"${galleryRefs[1]}","caption":"1b"}]`, true);
    await expect(bar.locator('[data-muster-status]')).toHaveText('Saved.');
    await page.waitForTimeout(500);
    await expect(confirmBar(page), 'no second confirm').toBeHidden();
    const read = await call('page_responses', { page_id: id });
    expect(read.body.count, JSON.stringify(read.body)).toBe(1);
    const fenced = read.body.responses as string;
    expect(fenced).toContain(`"by_owner":true,"component":"logos","data":{"picks":[{"asset":"${galleryRefs[0]}","index":0,"caption":"1a"},{"asset":"${galleryRefs[1]}","index":1,"caption":"1b"}]}`);
    // A change after saving says so, and can be saved again.
    await pickBtn('1a').tap();
    await expect(bar.locator('[data-muster-status]')).toHaveText('Changed since you saved: Save again to send these picks.');
    await pickBtn('1a').tap();
    await expect(bar.locator('[data-muster-status]')).toHaveText('Saved.');

    // A link's viewer has no confirm: a double-tap of Save must still record
    // ONE response (the gallery's own guard, not the owner's bar).
    const shared = await page.request.post(origin + `/pages/${id}/share`, { headers: { Origin: origin, Accept: 'application/json' } });
    const stranger = await browser.newContext({ viewport: ROUND_VIEWPORTS[0].viewport, hasTouch: true, isMobile: true });
    try {
      const sp = await stranger.newPage();
      await sp.goto((await shared.json()).url as string);
      const sv = viewerOf(sp);
      await sv.frame.locator('.mp-gallery-grid').getByRole('button', { name: 'Pick 2b' }).tap();
      const sbar = sv.frame.locator('.mp-gallery-bar');
      await sbar.getByRole('button', { name: 'Save picks' }).dblclick();
      await expect(sbar.locator('[data-muster-status]')).toHaveText('Saved.');
      await sp.waitForTimeout(1000);
    } finally {
      await stranger.close();
    }
    const after = await call('page_responses', { page_id: id });
    expect(after.body.count, 'a double-tap of Save is one response: ' + JSON.stringify(after.body)).toBe(2);
    expect(after.body.responses as string).toContain(`"by_owner":false,"component":"logos","data":{"picks":[{"asset":"${galleryRefs[4]}","index":4,"caption":"2b"}]}`);
  });
});

test('a single-pick gallery: a new pick replaces the last, in the grid and the viewer', async ({ browser }) => {
  const id = await galleryPage('Gallery single', { multiple: false });
  await asOwner(browser, ROUND_VIEWPORTS[0], async (page) => {
    await page.goto(origin + `/pages/${id}/view`);
    const v = viewerOf(page);
    const pickBtn = (name: string) => v.frame.locator('.mp-gallery-grid').getByRole('button', { name: 'Pick ' + name });
    const count = v.frame.locator('.mp-gallery-bar [data-muster-picked]');
    await pickBtn('1a').tap();
    await pickBtn('2a').tap();
    await expect(pickBtn('2a')).toHaveAttribute('aria-pressed', 'true');
    await expect(pickBtn('1a')).toHaveAttribute('aria-pressed', 'false');
    await expect(count).toHaveText('1 picked');
    await v.frame.getByRole('button', { name: 'Open 1c full size' }).tap();
    await v.dlg.locator('.mp-viewer-pick').tap();
    await v.dlg.getByRole('button', { name: 'Close full size' }).tap();
    await expect(pickBtn('1c')).toHaveAttribute('aria-pressed', 'true');
    await expect(pickBtn('2a')).toHaveAttribute('aria-pressed', 'false');
    await expect(count).toHaveText('1 picked');
    // Tapping the one pick again clears it.
    await pickBtn('1c').tap();
    await expect(count).toHaveText('0 picked');
  });
});

