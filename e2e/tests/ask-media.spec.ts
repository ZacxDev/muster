import { execFileSync } from 'node:child_process';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { askMediaConnectPort, askMediaPort, askMediaS3Port, dsn, repoRoot, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { FakeS3, startFakeS3 } from '../lib/fakes3';

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

let server: Running | undefined;
let s3: FakeS3 | undefined;
let agentMade = false;
const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  s3 = await startFakeS3(askMediaS3Port, ACCESS, 'muster-e2e');
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
    await s3?.close();
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
      // On the page the image is drawn LARGE (at most 20rem tall, its own
      // aspect), not as the feed's 56px thumbnail.
      await expect(pageThumb).toHaveClass(/(^| )max-h-80( |$)/);
      await expect(page.locator('#ask-page [data-ask-media-image]')).not.toHaveClass(/(^| )h-14( |$)/);

      // The same URL with no session is not the file.
      const anon = await fetch(origin + fileURL, { redirect: 'manual' });
      expect(anon.status, 'no session, no file').not.toBe(200);
      expect(Buffer.from(await anon.arrayBuffer()).includes(PNG.subarray(0, 8))).toBe(false);
    });
  });
}
