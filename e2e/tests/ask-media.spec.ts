import { execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import http from 'node:http';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { askMediaConnectPort, askMediaPort, askMediaS3Port, dsn, repoRoot, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';

// AN AGENT'S ASK CARRYING AN IMAGE (internal/api ask_media.go,
// docs/attention.md "Attachments and context"), in a real browser against a
// real muster-server whose upload store is a FAKE S3 played by this file:
//
//   the agent's pod sends a PNG to POST /agent/ask-attachments on the connect
//   door with its own token (what `ccd ask-attach` does) → attention_ask over
//   /mcp with attachment_ids and a context → on /attention the ask's feed
//   card shows the thumbnail (loaded, through the authorised route) and the
//   why, at 390×844 and 1280×800; tapping the thumbnail opens the full-size
//   image in a new tab, served image/png to the signed-in session, without
//   toggling the card; /attention/asks/<id> (the same card, expanded) shows
//   them too — and the same URL without a session is refused.
//
// 🔴 RED AT origin/main: there attention_ask has no attachment_ids (the call
// answers a tool error for the unknown argument) and no ask draws media.

const origin = `http://localhost:${askMediaPort}`;
const connectURL = `http://127.0.0.1:${askMediaConnectPort}`;
const stamp = Date.now().toString(36);
const agentName = 'e2e-askmedia-' + stamp;
const agentToken = 'e2e-askmedia-agent-token-not-a-real-one-' + stamp;
const ACCESS = 'e2e-askmedia-s3-access-not-real';

// A real 2×2 PNG.
const PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8AAQv8ZYAwAQ84H+VjtZqAAAAAASUVORK5CYII=',
  'base64',
);

// --- the fake S3 (uploads.spec.ts's, cut to what one small file needs) --------
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

let server: Running | undefined;
let agentMade = false;
const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await new Promise<void>((r) => s3.listen(askMediaS3Port, '127.0.0.1', () => r()));
  server = await startServer(versionTerm, askMediaPort, {
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_PUBLIC_URL: origin,
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x4d).toString('base64'),
    MUSTER_CONNECT_PORT: String(askMediaConnectPort),
    MUSTER_AGENT_CONNECT_URL: connectURL,
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_S3_ENDPOINT: `http://127.0.0.1:${askMediaS3Port}`,
    MUSTER_S3_BUCKET: 'muster-e2e',
    MUSTER_S3_ACCESS_KEY: ACCESS,
    MUSTER_S3_SECRET_KEY: 'e2e-askmedia-s3-secret-not-real',
    MUSTER_UPLOAD_SIGNING_KEY: 'e2e-askmedia-signing-key-not-a-real-one-0123456789',
  });
  fix('agent', '1', agentName, agentToken);
  agentMade = true;
});

test.afterAll(async () => {
  try {
    if (agentMade) fix('cleanup', '1', agentName);
  } finally {
    await server?.stop();
    await new Promise((r) => s3.close(r));
  }
});

async function mcpCall(name: string, args: Record<string, unknown>): Promise<string> {
  const r = await fetch(connectURL + '/mcp', {
    method: 'POST',
    headers: { 'X-Muster-Token': agentToken, 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name, arguments: args } }),
  });
  expect(r.status, 'the /mcp door answered').toBe(200);
  const j = (await r.json()) as { result?: { content: { text: string }[]; isError?: boolean }; error?: unknown };
  expect(j.error, JSON.stringify(j.error)).toBeUndefined();
  expect(j.result!.isError, j.result!.content[0].text).toBeFalsy();
  return j.result!.content[0].text;
}

// fileAsk is a new ask by the agent carrying one PNG (sent as the pod's
// `ccd ask-attach` would) and a context: its id, the file's route, the why.
async function fileAsk(tag: string): Promise<{ id: number; fileURL: string; why: string }> {
  const up = await fetch(connectURL + '/agent/ask-attachments?filename=screen.png', {
    method: 'POST',
    headers: { 'X-Muster-Token': agentToken, 'Content-Type': 'application/octet-stream' },
    body: PNG,
  });
  expect(up.status, await up.clone().text()).toBe(201);
  const rec = (await up.json()) as { id: string; type: string };
  expect(rec.type).toBe('image/png');
  const why = `Checking the new header (${tag}) before I ship it`;
  const text = await mcpCall('attention_ask', { question: `Does the header look right? (${tag})`, options: ['ship it', 'fix it'], attachment_ids: [rec.id], context: why });
  const id = (JSON.parse(text) as { id: number }).id;
  return { id, fileURL: `/attention/asks/${id}/attachments/${rec.id}`, why };
}

// THE CARD FEED (#768): the ask's Open card carries the thumbnail and the why
// in its slots, at a phone and a desktop width; tapping the thumbnail opens the
// full-size image and does NOT toggle the card (the card's script ignores a
// tap on media); the ask's own page — the same card, expanded — carries them
// too.
for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 } } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);
    test('an ask with an image shows its thumbnail on its feed card and its page; a tap opens it full size and leaves the card as it was', async ({ page, context }) => {
      test.setTimeout(90_000);
      const { id, fileURL, why } = await fileAsk(vp.name);
      await signIn(page, origin);
      await page.goto(origin + '/attention');
      const card = page.locator(`#ask-${id}`);
      const thumb = card.locator('[data-ask-media-image] img');
      await expect(thumb).toBeVisible();
      await expect(thumb).toHaveAttribute('src', fileURL);
      await expect.poll(() => thumb.evaluate((e: HTMLImageElement) => e.complete && e.naturalWidth), { timeout: 10_000 }).toBe(2);
      await expect(card.locator('[data-ask-why="agent"]')).toContainText(why);
      await expect(card.locator('[data-ask-media-image]')).toHaveCount(1);

      // A tap on the thumbnail opens the full-size image in a new tab, and
      // the card is not toggled by it.
      const before = await card.getAttribute('data-expanded');
      const [full] = await Promise.all([context.waitForEvent('page'), card.locator('[data-ask-media-image]').click()]);
      await full.waitForLoadState();
      expect(new URL(full.url()).pathname).toBe(fileURL);
      const served = await full.request.get(origin + fileURL);
      expect(served.status()).toBe(200);
      expect(served.headers()['content-type']).toBe('image/png');
      expect(Buffer.compare(await served.body(), PNG)).toBe(0);
      await full.close();
      await page.waitForTimeout(300);
      expect(await card.getAttribute('data-expanded'), 'a tap on the thumbnail toggled the card').toBe(before);

      // The ask's own page: the same card, expanded, with the same media.
      await page.goto(origin + `/attention/asks/${id}`);
      const pageThumb = page.locator('#ask-page [data-ask-media-image] img');
      await expect(pageThumb).toBeVisible();
      await expect(pageThumb).toHaveAttribute('src', fileURL);
      await expect.poll(() => pageThumb.evaluate((e: HTMLImageElement) => e.complete && e.naturalWidth), { timeout: 10_000 }).toBe(2);
      await expect(page.locator('#ask-page [data-ask-why="agent"]')).toContainText(why);
      await expect(page.locator('#ask-page [data-ask-media-image]')).toHaveCount(1);

      // The same URL with no session is not the file.
      const anon = await fetch(origin + fileURL, { redirect: 'manual' });
      expect(anon.status, 'no session, no file').not.toBe(200);
      expect(Buffer.from(await anon.arrayBuffer()).includes(PNG.subarray(0, 8))).toBe(false);
    });
  });
}
