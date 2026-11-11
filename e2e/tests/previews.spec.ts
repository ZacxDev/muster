import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import { Browser, Frame, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import {
  binDir,
  dsn,
  previewsAppPort,
  previewsEdgePort,
  previewsGatewayPort,
  previewsMeshPort,
  previewsPort,
  previewsSiblingPort,
  repoRoot,
} from '../lib/env';
import { cc, entryMatches, mirroredEntries, MirrorRig } from '../lib/mirrorhost';

// PREVIEWS, END TO END (docs/design/previews.md), on the owner's click path in
// a real Chromium. Everything on the path is the real binary:
//
//   muster-server (mirror plugin, plugins, MUSTER_PLUGIN_ORIGINS) with a viewer
//   plugin minted through the real /plugins form → the session view's Preview
//   button (shown because the host REPORTS this window's offers to muster) and
//   its side panel → the REAL gateway (cmd/muster-preview), whose plugin token is that
//   plugin's and whose session page redeems the panel's hand-off with muster →
//   the REAL muster-mirror (`-tags e2emirror`: only tmux is played) running
//   `-preview`, whose tunnel to the gateway carries the CONNECT streams, whose
//   /proc walk finds the dev app's listeners and offers them → the dev app
//   (e2e/previewfix app): a page, a WebSocket echo, an event stream.
//
// ⚠ WHAT IS PLAYED, AND WHY EACH IS SAFE TO PLAY:
//   - tmux. The fake host's window @1 names the dev app's pid as its pane root
//     (MUSTER_MIRROR_E2E_PANE_PIDS), so the real listener walk puts the app's
//     listeners "in" @1. The walk, the allowlist and the uid check are real.
//   - DNS and the edge. Chromium resolves every *.preview.example.net name to
//     the TLS edge on loopback (--host-resolver-rules, the port included, so
//     the page's ORIGIN keeps the default port and matches the gateway's
//     https://p-<id>.<zone>). The edge (e2e/previewfix edge) terminates TLS
//     with a self-signed certificate (ignoreHTTPSErrors) and passes the
//     request through unchanged, as a real edge does: the gateway sees the
//     browser's own Host, Origin, cookies and Sec-Fetch-* headers. HTTPS is
//     not optional here — the owner cookie is __Host-mp, which a browser
//     accepts only Secure over https, and Partitioned needs Secure too.
//
// 🔴 THE EDGE'S LOG IS HOW THIS SPEC READS WHAT THE GATEWAY WAS SENT: one line
// per request with the Sec-Fetch-* values, the Origin, the NAMES of the cookies
// and the gateway's status. The app's own log says what reached the app. The
// real-browser facts the gateway's unit tests could only assume are asserted
// from those two logs, and printed (FACTS) for the record.
const ZONE_HOST = 'preview.example.net';
const ZONE = `https://*.${ZONE_HOST}`;
const ENTRY = `https://w.${ZONE_HOST}/__muster/enter`;
const OWNER = '1'; // the password-mode operator (as plugin-chip.spec's)
const A = previewsAppPort;
const B = previewsSiblingPort;
const previewHost = /^https:\/\/p-([a-z2-7]{26})\.preview\.example\.net\//;

test.use({
  storageState: { cookies: [], origins: [] },
  ignoreHTTPSErrors: true,
  launchOptions: { args: [`--host-resolver-rules=MAP *.${ZONE_HOST} 127.0.0.1:${previewsEdgePort}`] },
});

const rig = new MirrorRig(previewsPort, 'e2e-previews', { MUSTER_PLUGINS: '1', MUSTER_PLUGIN_ORIGINS: ZONE });
const origin = rig.origin;
const names: string[] = [];
const procs: { name: string; p: ChildProcess; log: () => string }[] = [];
const edgeLog = path.join(rig.dir, 'edge.jsonl');
const appLog = path.join(rig.dir, 'app.jsonl');
let pluginId = 0;

const gatewayBin = path.join(binDir, 'muster-preview');
const fixBin = path.join(binDir, 'previewfix');

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

const view = `/mirror/view?host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent('@1')}`;
// @2: a shell window with no listener in it — nothing to preview.
const quietTerm = `/mirror/terminal?host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent('@2')}`;
const buttonSrc = (win: string) => `/ui/mirror/preview-button?host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent(win)}`;

type EdgeRec = {
  host: string;
  method: string;
  path: string;
  sec_fetch_site: string;
  sec_fetch_mode: string;
  sec_fetch_dest: string;
  sec_fetch_user: string;
  origin: string;
  upgrade: string;
  cookies: string[];
  status: number;
  code: string;
};
type AppRec = { port: number; method: string; path: string };

function lines<T>(file: string): T[] {
  if (!fs.existsSync(file)) return [];
  return fs
    .readFileSync(file, 'utf8')
    .split('\n')
    .filter((l) => l.trim())
    .map((l) => JSON.parse(l) as T);
}
const edge = () => lines<EdgeRec>(edgeLog);
const appHits = (port: number, p: string) => lines<AppRec>(appLog).filter((r) => r.port === port && r.path === p).length;
const onPreview = (id: string) => (r: EdgeRec) => r.host === `p-${id}.${ZONE_HOST}`;
const allLogs = () => procs.map((x) => `--- ${x.name}:\n${x.log()}`).join('\n') + `\n--- muster-mirror:\n${rig.log}`;

// start runs a fixture process and resolves once it prints `ready` on stdout
// (or at once when ready is empty); it is stopped in afterAll.
function start(name: string, bin: string, args: string[], env: NodeJS.ProcessEnv, ready = ''): Promise<ChildProcess> {
  const p = spawn(bin, args, { env: { ...process.env, ...env }, stdio: ['ignore', 'pipe', 'pipe'] });
  let log = '';
  p.stdout?.on('data', (d) => (log += d));
  p.stderr?.on('data', (d) => (log += d));
  procs.push({ name, p, log: () => log });
  if (!ready) return Promise.resolve(p);
  return new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error(`${name} never said ${ready}:\n${log}`)), 20_000);
    p.stdout?.on('data', () => {
      if (log.includes(ready)) {
        clearTimeout(t);
        resolve(p);
      }
    });
    p.once('exit', (c) => {
      clearTimeout(t);
      reject(new Error(`${name} exited ${c}:\n${log}`));
    });
  });
}

async function until(what: string, ok: () => Promise<boolean>): Promise<void> {
  const deadline = Date.now() + 20_000;
  while (!(await ok().catch(() => false))) {
    if (Date.now() > deadline) throw new Error(`${what} never came up:\n${allLogs()}`);
    await new Promise((r) => setTimeout(r, 150));
  }
}

const tcpOpen = (port: number) =>
  new Promise<boolean>((resolve) => {
    const s = net.connect(port, '127.0.0.1');
    s.once('connect', () => (s.destroy(), resolve(true)));
    s.once('error', () => resolve(false));
  });

// mintViewer creates a viewer-only plugin through the real /plugins form,
// with the gateway's zone and entry URL, and returns its token.
async function mintViewer(page: Page): Promise<string> {
  const name = 'e2e-previews-' + Date.now().toString(36);
  names.push(name);
  const form = new URLSearchParams({ name, label: 'Previews', data_scope: 'example', origin: ZONE, entry_url: ENTRY });
  form.append('capability', 'viewer');
  const r = await page.request.post(origin + '/plugins', {
    headers: { Origin: origin, 'Content-Type': 'application/x-www-form-urlencoded' },
    data: form.toString(),
  });
  const html = await r.text();
  expect(r.status(), html.slice(0, 400)).toBe(200);
  const token = /id="minted-plugin-token" readonly value="(mpl_[A-Za-z0-9_-]+)"/.exec(html)?.[1];
  expect(token, 'a plugin token was minted').toBeTruthy();
  const self = await page.request.get(origin + '/plugin/v1/self', { headers: { Authorization: `Bearer ${token}` } });
  const body = (await self.json()) as { plugin_id: number; capabilities: string[] };
  expect(body.capabilities).toEqual(['viewer']);
  pluginId = body.plugin_id;
  return token!;
}

test.beforeAll(async ({ browser }: { browser: Browser }) => {
  test.setTimeout(240_000);
  fs.mkdirSync(binDir, { recursive: true });
  execFileSync('go', ['build', '-o', gatewayBin, './cmd/muster-preview'], { cwd: repoRoot, stdio: 'inherit' });
  execFileSync('go', ['build', '-o', fixBin, './e2e/previewfix'], { cwd: repoRoot, stdio: 'inherit' });
  await rig.start();

  // The dev app: two listeners in one process, both on the host's allowlist.
  const app = await start('previewfix app', fixBin, ['app', '-listen', `127.0.0.1:${A},127.0.0.1:${B}`, '-log', appLog], {}, 'listening');

  // The viewer plugin, minted by the signed-in owner; its token is the
  // gateway's.
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await rig.login(page);
  const token = await mintViewer(page);
  await ctx.close();

  const state = fs.mkdtempSync(path.join(rig.dir, 'gw-'));
  const tokens = path.join(rig.dir, 'plugin-tokens');
  fs.writeFileSync(tokens, token + '\n', { mode: 0o600 });
  const gwEnv = {
    MUSTER_URL: origin,
    MUSTER_PUBLIC_URL: origin,
    PLUGIN_TOKENS_FILE: tokens,
    PREVIEW_DOMAIN: ZONE_HOST,
    PREVIEW_LISTEN: `127.0.0.1:${previewsGatewayPort}`,
    PREVIEW_MESH_LISTEN: `127.0.0.1:${previewsMeshPort}`,
    STATE_DIR: state,
    PREVIEW_SIGNING_KEY: 'e2e-preview-signing-key-not-a-real-one-0123456789',
  };
  // The host's preview token, minted on the gateway's CLI as an operator would.
  const hostToken = execFileSync(gatewayBin, ['host-token', 'create', rig.host], { env: { ...process.env, ...gwEnv } })
    .toString()
    .split('\n')[0]
    .trim();
  expect(hostToken.startsWith('mpt_'), 'a preview host token was minted').toBe(true);
  const hostTokenFile = path.join(rig.dir, 'preview-token');
  fs.writeFileSync(hostTokenFile, hostToken + '\n', { mode: 0o600 });

  await start('muster-preview', gatewayBin, ['serve'], gwEnv);
  await until('the gateway', async () => (await fetch(`http://127.0.0.1:${previewsMeshPort}/healthz`)).ok);
  await start('previewfix edge', fixBin, ['edge', '-listen', `127.0.0.1:${previewsEdgePort}`, '-upstream', `127.0.0.1:${previewsGatewayPort}`, '-log', edgeLog], {});
  await until('the edge', () => tcpOpen(previewsEdgePort));

  const previewsJSON = path.join(rig.dir, 'previews.json');
  fs.writeFileSync(previewsJSON, JSON.stringify({ ports: [String(A), String(B)], offer: true }), { mode: 0o600 });
  const now = Math.floor(Date.now() / 1000);
  const tr = rig.transcript(cc.prompt('start the dev server') + cc.reply('It is listening.'), 'one');
  const windows = rig.transcript(`work|@1|1|1|claude|${now - 60}|${tr}\nwork|@2|2|0|zsh|${now - 120}|\n`, 'windows');
  await rig.startHost({
    MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
    MUSTER_MIRROR_E2E_REFRESH: '300ms',
    MUSTER_MIRROR_E2E_PANE_PIDS: `@1=${app.pid}`,
    MUSTER_MIRROR_PREVIEW: '1',
    MUSTER_MIRROR_PREVIEW_URLS: `ws://127.0.0.1:${previewsMeshPort}/v1/host`,
    MUSTER_MIRROR_PREVIEW_TOKEN_FILE: hostTokenFile,
    MUSTER_MIRROR_PREVIEW_FILE: previewsJSON,
  });
});

// stopProc sends SIGTERM and waits up to 5 s for the exit, then SIGKILLs.
async function stopProc(p: ChildProcess): Promise<void> {
  if (p.exitCode !== null || p.signalCode !== null) return;
  const done = new Promise<boolean>((r) => p.once('exit', () => r(true)));
  p.kill('SIGTERM');
  const exited = await Promise.race([done, new Promise<boolean>((r) => setTimeout(() => r(false), 5_000))]);
  if (!exited) {
    p.kill('SIGKILL');
    await done;
  }
}

test.afterAll(async () => {
  try {
    for (const { p } of procs.reverse()) await stopProc(p);
    for (const n of names) fix('rmplugin', OWNER, n);
  } finally {
    await rig.stop();
  }
});

// ready signs in, waits until the host lists the Claude window as one and
// reports something to preview in it (the button's own poll answers it), and
// opens its Rendered view, whose header then draws the Preview button.
async function ready(page: Page): Promise<void> {
  await rig.login(page);
  await expect
    .poll(
      async () =>
        mirroredEntries(await (await page.request.get(origin + '/ui/nav/agents')).text()).filter(
          (e) => e.state === 'online' && e.markup.includes(`host=${encodeURIComponent(rig.host)}&`) && entryMatches(e.markup, 'data-mirror-claude'),
        ).length,
      { message: `the Claude window listed\n${allLogs()}`, timeout: 20_000 },
    )
    .toBe(1);
  await expect
    .poll(async () => (await (await page.request.get(origin + buttonSrc('@1'))).text()).includes('id="preview-toggle"'), {
      message: `the host reported @1's offers to muster\n${allLogs()}`,
      timeout: 30_000,
    })
    .toBe(true);
  await page.goto(origin + view);
  await expect(page.locator('#preview-toggle')).toHaveCount(1);
  await expect(page.locator('#preview-panel')).toHaveAttribute('data-plugin', String(pluginId));
}

// showSessionPage opens the Preview button's panel on the gateway's session
// page.
async function showSessionPage(page: Page) {
  await page.locator('#preview-toggle').click();
  await expect(page.locator('#preview-panel')).toBeVisible();
  await expect(page.locator('#preview-toggle')).toHaveAttribute('aria-expanded', 'true');
  const framed = page.frameLocator('#preview-panel iframe');
  await expect(framed.locator('h1'), allLogs()).toHaveText('Previews');
  return framed;
}

// rowOf is the session page's row for a port (an offer or an open preview).
const rowOf = (scope: { locator: Page['locator'] }, port: number) => scope.locator('#list li', { hasText: `· :${port}` });

// previewFrame is the page's frame now showing a preview, and its id.
async function previewFrame(page: Page): Promise<{ frame: Frame; id: string }> {
  let found: Frame | undefined;
  await expect
    .poll(() => (found = page.frames().find((f) => previewHost.test(f.url()))) !== undefined, { message: 'a frame on a preview host', timeout: 15_000 })
    .toBe(true);
  return { frame: found!, id: previewHost.exec(found!.url())![1] };
}

const facts: Record<string, unknown> = {};

test('the owner opens a window\'s dev server from muster: the Preview button\'s panel offers it, renders it with a working WebSocket and event stream, Close cuts the frame off at its next request, and New tab works', async ({ page, context }) => {
  test.setTimeout(150_000);
  await ready(page);

  // THE PANEL: its frame lands on the gateway's session page (the
  // hand-off redeemed with muster), which offers both of the app's listeners
  // in this window under the listening process's name.
  const framed = await showSessionPage(page);
  await expect(framed.locator('.sub')).toHaveText(`${rig.host} · work (@1)`);
  await expect(rowOf(framed, A), allLogs()).toHaveAttribute('data-kind', 'offer', { timeout: 30_000 });
  await expect(rowOf(framed, A)).toContainText(`previewfix · :${A}`);
  await expect(rowOf(framed, B)).toHaveAttribute('data-kind', 'offer');

  // OPEN, in the frame: the app's page renders inside muster's frame, its
  // WebSocket echo round-trips and its event stream delivers.
  const before = edge().length;
  await rowOf(framed, A).locator('button[data-how="here"]').click();
  // The frame's owner cookie came back on /__muster/check: Chromium accepted
  // the SameSite=None; Partitioned cookie the entry set inside muster's frame
  // (a refused one is the "blocks embedded previews" page, frame_blocked).
  let checked: EdgeRec | undefined;
  await expect
    .poll(() => (checked = edge().slice(before).find((r) => r.path === '/__muster/check')) !== undefined, { message: 'the frame reached check' })
    .toBe(true);
  expect([checked!.status, checked!.code], 'check admitted the frame\'s cookie').toEqual([200, '']);
  await expect(framed.locator('#app'), allLogs()).toHaveText(`preview app on ${A}`);
  await expect(framed.locator('#ws')).toHaveText('ws: echo ok');
  await expect(framed.locator('#sse')).toHaveText(`sse: hello from ${A}`);
  const { frame, id } = await previewFrame(page);
  expect(new URL(frame.url()).pathname).toBe('/');
  expect(appHits(A, '/ws')).toBe(1);
  expect(appHits(A, '/sse')).toBeGreaterThanOrEqual(1);

  // What the gateway was sent on the Show here chain, by the real browser.
  const p = edge().filter(onPreview(id));
  const at = (pathname: string) => p.find((r) => r.path === pathname);
  const enter = at('/__muster/enter')!;
  const check = at('/__muster/check')!;
  const root = at('/')!;
  const ws = at('/ws')!;
  const sse = at('/sse')!;
  for (const [n, r] of Object.entries({ enter, check, root, ws, sse })) expect(r, `the gateway was sent ${n}`).toBeTruthy();
  expect([enter.sec_fetch_site, enter.sec_fetch_mode, enter.sec_fetch_dest, enter.status]).toEqual(['same-site', 'navigate', 'iframe', 303]);
  expect([check.sec_fetch_site, check.sec_fetch_mode, check.sec_fetch_dest, check.status]).toEqual(['same-site', 'navigate', 'iframe', 200]);
  expect(check.cookies, 'the frame sent the owner cookie back on check: the Partitioned cookie was accepted').toContain('__Host-mp');
  expect([root.sec_fetch_site, root.sec_fetch_mode, root.sec_fetch_dest, root.status]).toEqual(['same-origin', 'navigate', 'iframe', 200]);
  // The upgrade is admitted on its Origin (Chromium sends it no Sec-Fetch-Site:
  // the gateway's WebSocket rule is the Origin one, samesite.go).
  expect([ws.upgrade.toLowerCase(), ws.origin, ws.status]).toEqual(['websocket', `https://p-${id}.${ZONE_HOST}`, 101]);
  expect([sse.sec_fetch_site, sse.status]).toEqual(['same-origin', 200]);
  const frameCookie = (await context.cookies(`https://p-${id}.${ZONE_HOST}/`)).find((c) => c.name === '__Host-mp');
  expect(frameCookie, 'the frame\'s owner cookie is in the jar').toBeTruthy();
  expect([frameCookie!.sameSite, frameCookie!.secure, frameCookie!.httpOnly]).toEqual(['None', true, true]);
  expect(frameCookie!.partitionKey, 'the frame\'s owner cookie is partitioned under muster').toBeTruthy();
  facts.showHere = { enter, check, root, ws, sse, cookie: { sameSite: frameCookie!.sameSite, partitionKey: frameCookie!.partitionKey } };

  // CLOSE, on the session page — in a tab of its own, from the panel's New
  // tab on a second muster tab: this tab's panel now shows the preview, and
  // stays open to be refused below.
  const second = await context.newPage();
  await second.goto(origin + view);
  await second.locator('#preview-toggle').click();
  await expect(second.locator('#preview-panel')).toBeVisible();
  const [tab] = await Promise.all([context.waitForEvent('page'), second.locator('#preview-panel [data-preview-newtab]').click()]);
  await expect(tab.locator('h1')).toHaveText('Previews');
  await expect(rowOf(tab, A)).toHaveAttribute('data-kind', 'preview');
  await rowOf(tab, A).locator(`button[data-close="${id}"]`).click();
  await expect(rowOf(tab, A), 'the closed preview\'s port is offered again').toHaveAttribute('data-kind', 'offer', { timeout: 15_000 });

  // The frame's next request is refused: 404, and it never reaches the app.
  const pings = appHits(A, '/api/ping');
  const status = await frame.evaluate(() => fetch('/api/ping', { cache: 'no-store' }).then((r) => r.status));
  expect(status, 'the closed preview answers 404').toBe(404);
  expect(appHits(A, '/api/ping'), 'nothing reached the app after Close').toBe(pings);
  expect(edge().filter(onPreview(id)).filter((r) => r.path === '/api/ping').map((r) => r.status)).toEqual([404]);

  // NEW TAB: the offer opens a new preview top-level, with a document-mode
  // (SameSite=Strict, unpartitioned) cookie, and works there too.
  const [preview] = await Promise.all([context.waitForEvent('page'), rowOf(tab, A).locator('button[data-how="tab"]').click()]);
  await expect(preview.locator('#app')).toHaveText(`preview app on ${A}`);
  await expect(preview.locator('#ws')).toHaveText('ws: echo ok');
  await expect(preview.locator('#sse')).toHaveText(`sse: hello from ${A}`);
  const id2 = previewHost.exec(preview.url())![1];
  expect(id2, 'a reopened offer is a new preview').not.toBe(id);
  const enter2 = edge().filter(onPreview(id2)).find((r) => r.path === '/__muster/enter')!;
  expect([enter2.sec_fetch_site, enter2.sec_fetch_mode, enter2.sec_fetch_dest, enter2.status]).toEqual(['same-site', 'navigate', 'document', 303]);
  const tabCookie = (await context.cookies(`https://p-${id2}.${ZONE_HOST}/`)).find((c) => c.name === '__Host-mp');
  expect([tabCookie?.sameSite, tabCookie?.partitionKey]).toEqual(['Strict', undefined]);
  facts.newTab = { enter: enter2, root: edge().filter(onPreview(id2)).find((r) => r.path === '/'), cookie: { sameSite: tabCookie?.sameSite } };

  // Leave no preview open: Close id2 on the session page, so the next test
  // finds both ports offered, as it does when it runs alone.
  await expect(rowOf(tab, A)).toHaveAttribute('data-kind', 'preview', { timeout: 15_000 });
  await rowOf(tab, A).locator(`button[data-close="${id2}"]`).click();
  await expect(rowOf(tab, A), 'the New tab preview was closed').toHaveAttribute('data-kind', 'offer', { timeout: 15_000 });
  await expect(rowOf(tab, B)).toHaveAttribute('data-kind', 'offer');
  await preview.close();
  await tab.close();
  await second.close();
});

test('a sibling preview\'s hidden iframe and WebSocket to another preview are refused (cross_site) and never reach the victim\'s app, though the browser sent the victim\'s cookie',async ({ page }) => {
  test.setTimeout(120_000);
  await ready(page);

  // The VICTIM: the app on A, opened in muster's frame (its cookie is the
  // frame's, partitioned under muster).
  // Both ports are OFFERS here, alone (-g) or after the owner test, which
  // closes every preview it opened: this test opens fresh previews either way.
  let framed = await showSessionPage(page);
  await expect(rowOf(framed, A), allLogs()).toHaveAttribute('data-kind', 'offer', { timeout: 30_000 });
  await expect(rowOf(framed, B)).toHaveAttribute('data-kind', 'offer');
  await rowOf(framed, A).locator('button[data-how="here"]').click();
  await expect(framed.locator('#app')).toHaveText(`preview app on ${A}`);
  const victim = await previewFrame(page);

  // POSITIVE CONTROL: the victim's own page reaches /secret, and the app's log
  // counts it — so a zero below is the gateway refusing, not a log that cannot
  // see the path.
  const before = appHits(A, '/secret');
  expect(await victim.frame.evaluate(() => fetch('/secret').then((r) => r.status))).toBe(200);
  await expect.poll(() => appHits(A, '/secret')).toBe(before + 1);

  // The SIBLING: the app on B, opened in the same frame (same partition), whose
  // page loads the victim's /secret in a hidden iframe.
  await page.locator('#preview-panel [data-preview-close]').click();
  await expect(page.locator('#preview-panel')).toBeHidden();
  framed = await showSessionPage(page);
  await expect(rowOf(framed, A)).toHaveAttribute('data-kind', 'preview', { timeout: 30_000 });
  await expect(rowOf(framed, B)).toHaveAttribute('data-kind', 'offer');
  await rowOf(framed, B).locator('button[data-how="here"]').click();
  await expect(framed.locator('#app')).toHaveText(`preview app on ${B}`);
  const sibling = await previewFrame(page);
  expect(sibling.id).not.toBe(victim.id);
  const target = `https://p-${victim.id}.${ZONE_HOST}/secret`;
  const seen = edge().length; // the control's own /secret is before this
  const victimWS = appHits(A, '/ws'); // the victim page's own echo socket
  await sibling.frame.evaluate((t) => location.assign('/attack?victim=' + encodeURIComponent(t)), target);
  await expect(framed.locator('#attacker')).toHaveText(`sibling preview on ${B}`);

  let refused: EdgeRec | undefined;
  await expect
    .poll(() => (refused = edge().slice(seen).filter(onPreview(victim.id)).find((r) => r.path === '/secret')) !== undefined, {
      message: 'the hidden iframe\'s request reached the gateway',
      timeout: 15_000,
    })
    .toBe(true);
  expect([refused!.status, refused!.code], 'the gateway refused the sibling\'s frame').toEqual([403, 'cross_site']);
  expect([refused!.sec_fetch_site, refused!.sec_fetch_mode, refused!.sec_fetch_dest]).toEqual(['same-site', 'navigate', 'iframe']);
  expect(refused!.cookies, 'the browser DID send the victim\'s cookie: only the gateway stood in the way').toContain('__Host-mp');

  // The same page's WebSocket to the victim's /ws: Chromium sends a WebSocket
  // handshake no Sec-Fetch-*, so only the gateway's Origin rule (the upgrade
  // must carry the victim's own preview Origin) refuses it.
  let wsRefused: EdgeRec | undefined;
  await expect
    .poll(() => (wsRefused = edge().slice(seen).filter(onPreview(victim.id)).find((r) => r.path === '/ws')) !== undefined, {
      message: 'the sibling\'s WebSocket reached the gateway',
      timeout: 15_000,
    })
    .toBe(true);
  expect([wsRefused!.upgrade.toLowerCase(), wsRefused!.origin], 'a WebSocket upgrade from the sibling\'s origin').toEqual([
    'websocket',
    `https://p-${sibling.id}.${ZONE_HOST}`,
  ]);
  expect([wsRefused!.status, wsRefused!.code], 'the gateway refused the sibling\'s WebSocket').toEqual([403, 'cross_site']);
  await expect(framed.locator('#wsattack')).toHaveText('wsattack: error');
  await page.waitForTimeout(500);
  expect(appHits(A, '/secret'), 'the victim\'s app never saw the sibling\'s request').toBe(before + 1);
  expect(appHits(A, '/ws'), 'the victim\'s app never saw the sibling\'s WebSocket').toBe(victimWS);
  facts.sibling = refused;
  facts.siblingWebSocket = wsRefused;
  console.log('FACTS (what the gateway was sent by Chromium):\n' + JSON.stringify(facts, null, 2));
});

test('nothing to preview, no Preview button: a window with no listener draws none on the real host\'s report, the sidebar marks only the window that has one, and Close unloads the panel', async ({ page }) => {
  test.setTimeout(90_000);
  await ready(page);

  // The sidebar (overlay on this page): @1 is marked, @2 is not.
  const nav = await (await page.request.get(origin + '/ui/nav/agents')).text();
  const rows = mirroredEntries(nav).filter((e) => e.markup.includes(`host=${encodeURIComponent(rig.host)}&`));
  const row = (win: string) => rows.find((e) => e.markup.includes(`window=${encodeURIComponent(win)}`));
  expect(row('@1')?.markup, 'CONTROL: the window with offers is marked').toMatch(/data-nav-preview="[1-9]/);
  expect(row('@2'), 'the quiet window is listed').toBeTruthy();
  expect(row('@2')!.markup, 'the quiet window is not marked').not.toContain('data-nav-preview');

  // @2's Terminal view: the slot and panel are there (the owner has a live
  // viewer plugin), the button is not; its poll answers nothing.
  const resp = await page.goto(origin + quietTerm);
  expect(new URL(page.url()).pathname).toBe('/mirror/terminal');
  expect(resp!.headers()['content-security-policy']).toContain(`frame-src 'self' ${ZONE}; `);
  await expect(page.locator('#preview-slot')).toHaveCount(1);
  await expect(page.locator('#preview-toggle')).toHaveCount(0);
  expect((await (await page.request.get(origin + buttonSrc('@2'))).text()).trim()).toBe('');

  // @1 (the control): the button; the panel opens and Close unloads it.
  await page.goto(origin + view);
  await showSessionPage(page);
  await page.keyboard.press('Escape');
  await expect(page.locator('#preview-panel')).toBeHidden();
  await expect(page.locator('#preview-panel iframe')).toHaveAttribute('src', 'about:blank');
  await expect(page.locator('#preview-toggle')).toHaveAttribute('aria-expanded', 'false');
});
