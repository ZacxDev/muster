import fs from 'node:fs';
import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { ocAskMediaMCPPort, ocAskMediaPort, ocAskMediaS3Port } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';
import { FakeS3, startFakeS3 } from '../lib/fakes3';

// A MIRRORED OPENCODE SESSION'S ASK CARRYING A FILE (internal/mirrorhost
// mcpproxy_attach.go; docs/attention.md "Attachments and context"), end to
// end: a REAL muster-mirror (-tags e2emirror: its fakes are tmux and the
// opencode server, cmd/muster-mirror/e2eopencode.go) serving its loopback MCP
// endpoint, a REAL e2eterm server whose upload store is a fake S3 this spec
// plays, and a real browser.
//
//   opencode's MCP entry calls the host's endpoint as opencode does — the
//   secret and X-Muster-Client: opencode, no pane, the session in
//   params._meta["ai.opencode/sessionID"] — with attention_ask{attachments:
//   ["shots/screen.png"], context}. The host binds the call to the window its
//   launcher record binds to that session, reads the session's directory from
//   the (fake) opencode server, uploads the file from there, and forwards the
//   call with its id; the ask's page shows the image (loaded, through the
//   authorised route) and the why. A path outside that directory files no ask.
//
// 🔴 RED AT origin/main: there the host refused the call ("attachments from an
// opencode session are not supported yet"), so no ask was filed.

const stamp = Date.now().toString(36);
const ACCESS = 'e2e-ocaskmedia-s3-access-not-real';
const rig = new MirrorRig(ocAskMediaPort, 'e2e-ocaskm', {
  MUSTER_FOCUS_INTERVAL: '0',
  MUSTER_PUBLIC_URL: `http://localhost:${ocAskMediaPort}`,
  MUSTER_S3_ENDPOINT: `http://127.0.0.1:${ocAskMediaS3Port}`,
  MUSTER_S3_BUCKET: 'muster-e2e',
  MUSTER_S3_ACCESS_KEY: ACCESS,
  MUSTER_S3_SECRET_KEY: 'e2e-ocaskmedia-s3-secret-not-real',
  MUSTER_UPLOAD_SIGNING_KEY: 'e2e-ocaskmedia-signing-key-not-a-real-one-0123456789',
});
const sid = 'ses_0e2eocaskmedia01';
const localMCP = `http://127.0.0.1:${ocAskMediaMCPPort}/mcp`;
const t0 = Math.floor(Date.now() / 1000);
let s3: FakeS3 | undefined;
let secretFile = '';

// A real 2×2 PNG.
const PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8AAQv8ZYAwAQ84H+VjtZqAAAAAASUVORK5CYII=',
  'base64',
);

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async ({ browser }) => {
  test.setTimeout(180_000);
  s3 = await startFakeS3(ocAskMediaS3Port, ACCESS, 'muster-e2e');
  await rig.start();
  const dir = fs.mkdtempSync(path.join(rig.dir, 'opencode-'));
  // The session's directory, as the opencode server records it: the file is
  // in it; a sibling file is not.
  const sessionDir = fs.mkdtempSync(path.join(rig.dir, 'session-'));
  fs.mkdirSync(path.join(sessionDir, 'shots'));
  fs.writeFileSync(path.join(sessionDir, 'shots', 'screen.png'), PNG);
  fs.writeFileSync(path.join(rig.dir, 'outside.png'), PNG);
  const ops = [
    { op: 'session', id: sid, title: 'Asking fixture session', directory: sessionDir },
    { op: 'bind', session: 'oc', window: '@2', id: sid },
  ];
  fs.writeFileSync(path.join(dir, 'ops.jsonl'), ops.map((o) => JSON.stringify(o)).join('\n') + '\n');
  const windows = path.join(rig.dir, 'windows');
  writeFileAtomic(windows, `plain|@1|0|1|zsh|${t0 - 50}|\n`);
  const ocWindows = path.join(rig.dir, 'opencode-windows');
  writeFileAtomic(ocWindows, `oc|@2|0|1|${t0 - 10}|OC | Asking fixture session\n`);
  secretFile = path.join(rig.dir, 'mcp-secret');
  await rig.startHost({
    MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
    MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE: ocWindows,
    MUSTER_MIRROR_E2E_OPENCODE_DIR: dir,
    MUSTER_MIRROR_E2E_REFRESH: '200ms',
    MUSTER_MIRROR_MCP_LISTEN: `127.0.0.1:${ocAskMediaMCPPort}`,
    MUSTER_MIRROR_MCP_SECRET_FILE: secretFile,
  });
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await rig.login(page);
  await rig.waitListed(page, 'session=oc');
  await ctx.close();
});

test.afterAll(async () => {
  await rig.stop();
  await s3?.close();
});

type ToolResult = { result?: { isError?: boolean; content: { text: string }[] }; error?: unknown };

// ocAsk is opencode's attention_ask through the host's endpoint, as opencode
// sends it.
async function ocAsk(args: Record<string, unknown>): Promise<ToolResult> {
  const secret = fs.readFileSync(secretFile, 'utf8').trim();
  const r = await fetch(localMCP, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream', Authorization: 'Bearer ' + secret, 'X-Muster-Client': 'opencode' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name: 'attention_ask', arguments: args, _meta: { 'ai.opencode/sessionID': sid } } }),
  });
  expect(r.status, 'the host endpoint answered').toBe(200);
  return (await r.json()) as ToolResult;
}

test('an opencode session\'s ask carries a file from its session\'s directory, and its page shows it and the why', async ({ page }) => {
  test.setTimeout(90_000);
  const why = `Checking the fixture screen (${stamp}) before I ship it`;
  const asked = await ocAsk({ question: `Is this the right screen? (${stamp})`, context: why, attachments: ['shots/screen.png'] });
  expect(asked.error, JSON.stringify(asked.error)).toBeUndefined();
  expect(asked.result!.isError, asked.result!.content[0].text + '\nmuster-mirror log:\n' + rig.log).toBeFalsy();
  const out = JSON.parse(asked.result!.content[0].text) as { id: number; attachments: number };
  expect(out.attachments).toBe(1);
  expect(s3!.objects.size, 'the file is in the upload store').toBe(1);

  // Outside the session's directory: refused, nothing stored, no ask.
  const escape = await ocAsk({ question: `Escape? (${stamp})`, attachments: ['../outside.png'] });
  expect(escape.result!.isError).toBe(true);
  expect(escape.result!.content[0].text).toContain('the path is outside your workspace');
  expect(s3!.objects.size, 'a refused path stored nothing').toBe(1);

  await page.setViewportSize({ width: 1280, height: 800 });
  await rig.login(page);
  await page.goto(`${rig.origin}/attention/asks/${out.id}`);
  const thumb = page.locator(`img[src^="/attention/asks/${out.id}/attachments/"]`);
  await expect(thumb).toHaveCount(1);
  await expect.poll(() => thumb.evaluate((i: HTMLImageElement) => i.complete && i.naturalWidth), { message: 'the thumbnail loads' }).toBe(2);
  await expect(page.locator('body')).toContainText(why);
  const src = (await thumb.getAttribute('src'))!;
  const file = await page.request.get(rig.origin + src);
  expect(file.status()).toBe(200);
  expect(file.headers()['content-type']).toBe('image/png');
  expect(Buffer.compare(await file.body(), PNG)).toBe(0);
  // The escaping ask was never filed: the asks list holds only the one.
  await page.goto(`${rig.origin}/attention`);
  await expect(page.locator('body')).toContainText(`Is this the right screen? (${stamp})`);
  await expect(page.locator('body')).not.toContainText(`Escape? (${stamp})`);
});
