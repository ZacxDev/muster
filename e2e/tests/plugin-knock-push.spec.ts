import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import { createDecipheriv, createECDH, createHmac, randomBytes, ECDH } from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { request as pwRequest, APIRequestContext } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { binDir, knockPushPort, knockPushSinkPort, repoRoot, stateDir, versionA } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// THE KNOCK PUSH SPEC (docs/design/meetings.md, "The knock push spec"; core PR
// 1e): an `app` plugin's knock reaches the owner's PAIRED PHONE as payload v2,
// end to end through muster's real push sender, and decrypts to muster's own
// text.
//
// There is no meetings plugin yet, so the "plugin" is this spec holding an
// `app` plugin token and calling POST/DELETE /plugin/v1/notify itself; the
// owner's "not looking at the meeting" (the plugin's half) has nothing to
// test here.
//
// 🔴 THE PHONE IS REAL AS FAR AS MUSTER CAN TELL. It is paired through the
// device routes (a pairing code from /account/devices/pair, redeemed on
// POST /api/device/pair), and registers its push endpoint on PUT
// /api/device/push — whose own VAPID test push must reach the endpoint and be
// answered 2xx before muster keeps it. The endpoint is UnifiedPush-shaped,
// https://push.example.test/up/<topic>, on the default port, on a host listed
// in MUSTER_PUSH_EXTRA_HOSTS (the only hosts a paired phone may name), served
// over TLS by e2e/pushsink with a certificate from a test CA that muster
// trusts through SSL_CERT_FILE. The spec holds the phone's P-256 key pair and
// auth secret and decrypts every body itself (RFC 8291 / RFC 8188 aes128gcm):
// the sink never sees a key.
//
// ⚠ HOW MUSTER REACHES :443 WITHOUT ROOT: through HTTPS_PROXY, an HTTP CONNECT
// tunnel the sink runs that accepts exactly push.example.test:443 (see
// e2e/pushsink). The TLS handshake runs end to end inside the tunnel, so the
// host name, the port and the chain are checked as they would be against a
// listener on 443. The sink records the CONNECT target of every send, and
// this spec asserts it.

const origin = `http://localhost:${knockPushPort}`;
const pushHost = 'push.example.test';
const appOrigin = 'https://*.meet.example.test';
const appEntry = 'https://lobby.meet.example.test/__muster/enter';
// The tab label — the push's title. It shares no word with either body
// sentence, so a title that came from anywhere else would not match.
const tabLabel = 'Lantern Room';
const guestName = 'Ada Lovelace';
const topic = 'upE2Eknock' + Date.now().toString(36);
const endpoint = `https://${pushHost}/up/${topic}`;

const sinkDir = path.join(stateDir, 'pushsink');
const caFile = path.join(sinkDir, 'ca.pem');
const recordFile = path.join(sinkDir, 'received.jsonl');

let server: Running | undefined;
let sink: ChildProcess | undefined;
let bearer: APIRequestContext | undefined;
let deviceToken = '';

// The phone: its ECDH key pair and auth secret, as the app generates them.
const phoneKey: ECDH = createECDH('prime256v1');
const phonePub = phoneKey.generateKeys();
const phoneAuth = randomBytes(16);
const b64url = (b: Buffer) => b.toString('base64url');

interface Received {
  path?: string;
  content_encoding?: string;
  authorization?: boolean;
  body?: string;
  connect?: string;
  sni?: string;
  refused?: string;
}

function received(): Received[] {
  if (!fs.existsSync(recordFile)) return [];
  // Only whole lines: the sink may be part-way through writing the last one.
  const text = fs.readFileSync(recordFile, 'utf8');
  return text.slice(0, text.lastIndexOf('\n') + 1).split('\n').filter(Boolean).map((l) => JSON.parse(l) as Received);
}

const toPhone = () => received().filter((r) => r.path === `/up/${topic}`);

// decrypt is RFC 8291's aes128gcm for the phone's keys: one record, its header
// the salt, the record size, and the sender's ephemeral public key.
function decrypt(body: Buffer): string {
  const salt = body.subarray(0, 16);
  const idlen = body[20];
  const senderPub = body.subarray(21, 21 + idlen);
  const ct = body.subarray(21 + idlen);
  const hmac = (k: Buffer, d: Buffer) => createHmac('sha256', k).update(d).digest();
  const shared = phoneKey.computeSecret(senderPub);
  const ikm = hmac(hmac(phoneAuth, shared), Buffer.concat([Buffer.from('WebPush: info\0'), phonePub, senderPub, Buffer.from([1])]));
  const prk = hmac(salt, ikm);
  const cek = hmac(prk, Buffer.from('Content-Encoding: aes128gcm\0\x01', 'latin1')).subarray(0, 16);
  const nonce = hmac(prk, Buffer.from('Content-Encoding: nonce\0\x01', 'latin1')).subarray(0, 12);
  const d = createDecipheriv('aes-128-gcm', cek, nonce);
  d.setAuthTag(ct.subarray(ct.length - 16));
  const pt = Buffer.concat([d.update(ct.subarray(0, ct.length - 16)), d.final()]);
  let end = pt.length - 1;
  while (end >= 0 && pt[end] === 0) end--;
  if (pt[end] !== 2) throw new Error('the record does not end in the last-record delimiter');
  return pt.subarray(0, end).toString('utf8');
}

// nthToPhone waits for the phone's (n+1)th push and returns it decrypted,
// after checking how it was sent.
async function nthToPhone(n: number): Promise<Record<string, unknown>> {
  await expect.poll(() => toPhone().length, { timeout: 15_000, message: `push #${n + 1} to the phone` }).toBeGreaterThan(n);
  const r = toPhone()[n];
  expect(r.connect, 'muster dialled the push host on the default port').toBe(`${pushHost}:443`);
  expect(r.sni).toBe(pushHost);
  expect(r.content_encoding).toBe('aes128gcm');
  expect(r.authorization, 'a VAPID Authorization header').toBe(true);
  const body = Buffer.from(r.body ?? '', 'base64');
  expect(body.length, 'every native push is one 4096-byte record').toBe(4096);
  return JSON.parse(decrypt(body)) as Record<string, unknown>;
}

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  fs.mkdirSync(sinkDir, { recursive: true });
  fs.rmSync(recordFile, { force: true });
  const sinkBin = path.join(binDir, 'pushsink');
  execFileSync('go', ['build', '-o', sinkBin, './e2e/pushsink'], { cwd: repoRoot, stdio: 'inherit' });
  sink = spawn(sinkBin, ['-host', pushHost, '-proxy', `127.0.0.1:${knockPushSinkPort}`, '-ca-out', caFile, '-record', recordFile], {
    stdio: ['ignore', 'pipe', 'inherit'],
  });
  await new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('pushsink did not report ready')), 20_000);
    sink!.once('exit', (c) => reject(new Error(`pushsink exited ${c}`)));
    sink!.stdout!.on('data', (d) => {
      if (String(d).includes('pushsink: ready')) {
        clearTimeout(timer);
        resolve();
      }
    });
  });

  const out = execFileSync(serverBinary(versionA), ['vapid', 'generate'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
  const env: Record<string, string> = {
    MUSTER_VAPID_SUBJECT: 'mailto:e2e@example.com',
    MUSTER_PUSH_EXTRA_HOSTS: pushHost,
    MUSTER_PLUGINS: '1',
    MUSTER_PLUGIN_ORIGINS: appOrigin,
    // The test CA replaces the system bundle FILE, so the sink's chain is
    // verified against it. (Go still reads its default certificate
    // directories: an empty SSL_CERT_DIR is ignored, so none is set. The only
    // TLS this server makes is to the sink, so that changes nothing here.)
    SSL_CERT_FILE: caFile,
    HTTPS_PROXY: `http://127.0.0.1:${knockPushSinkPort}`,
    https_proxy: `http://127.0.0.1:${knockPushSinkPort}`,
    NO_PROXY: '',
    no_proxy: '',
  };
  for (const line of out.trim().split('\n')) {
    const i = line.indexOf('=');
    env[line.slice(0, i)] = line.slice(i + 1);
  }
  server = await startServer(versionA, knockPushPort, env);
  bearer = await pwRequest.newContext({ baseURL: origin });
});

test.afterAll(async () => {
  try {
    // The phone unpairs, pass or fail: the shared database keeps no phone of this spec's.
    if (deviceToken && bearer) await bearer.delete('/api/device', { headers: { Authorization: 'Bearer ' + deviceToken } });
    await bearer?.dispose();
  } finally {
    await server?.stop();
    if (sink && sink.exitCode === null) {
      const done = new Promise((r) => sink!.once('exit', r));
      sink.kill('SIGTERM');
      await done;
    }
  }
});

test('a knock reaches the paired phone as muster\'s text, is capped per ref, and is withdrawn', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page, origin);
  const session = page.request;

  // --- Pair the phone through the device routes, and register its endpoint.
  const code = await session.post(origin + '/account/devices/pair', { headers: { Origin: origin } });
  expect(code.status(), await code.text()).toBe(200);
  const paired = await bearer!.post('/api/device/pair', {
    data: { code: ((await code.json()) as { code: string }).code, name: `e2e knock phone ${Date.now()}`, app_version: '0.0.2' },
  });
  expect(paired.status(), await paired.text()).toBe(201);
  deviceToken = ((await paired.json()) as { token: string }).token;
  const dev = { Authorization: 'Bearer ' + deviceToken };
  const reg = await bearer!.put('/api/device/push', { headers: dev, data: { endpoint, p256dh: b64url(phonePub), auth: b64url(phoneAuth) } });
  expect(reg.status(), await reg.text()).toBe(200);
  // POSITIVE CONTROL of the sink AND the decryption: the registration's own
  // test push arrived, under the default port, and decrypts.
  expect(await nthToPhone(0)).toEqual({ v: 2, type: 'test' });

  // --- The plugin: an app token minted on /plugins.
  const name = `e2e-knock-${Date.now()}`;
  const minted = await session.post(origin + '/plugins', {
    headers: { Origin: origin },
    form: { name, label: 'Example meetings', data_scope: 'example', capability: 'app', origin: appOrigin, entry_url: appEntry, app_label: tabLabel, app_icon: 'video' },
  });
  const html = await minted.text();
  expect(minted.status(), html.slice(0, 400)).toBe(200);
  const tok = /id="minted-plugin-token"[^>]*>(mpl_[A-Za-z0-9_-]+)</.exec(html)?.[1];
  expect(tok, 'the minted token is shown once').toBeTruthy();
  const plug = { Authorization: 'Bearer ' + tok };
  const self = (await (await bearer!.get('/plugin/v1/self', { headers: plug })).json()) as { plugin_id: number };
  const id = self.plugin_id;
  const ref = 'mtg-' + Date.now().toString(36);
  const tag = `app-${id}-${ref}`;

  try {
    // --- The knock: muster's text, the tab label as title, the tap URL.
    const first = await bearer!.post('/plugin/v1/notify', { headers: plug, data: { kind: 'knock', ref, count: 1 } });
    expect(first.status(), await first.text()).toBe(202);
    const knock = await nthToPhone(1);
    expect(knock).toEqual({
      v: 2, type: 'app', id: tag, tag,
      title: tabLabel, body: 'Someone is waiting to join your meeting', url: `/apps/${id}?ref=${ref}`,
    });

    // A guest's name the plugin tries to send is refused, never delivered.
    const named = await bearer!.post('/plugin/v1/notify', { headers: plug, data: { kind: 'knock', ref: ref + 'x', count: 1, name: guestName } });
    expect(named.status()).toBe(400);

    // --- A second knock on the same ref within 30 s: refused, nothing sent.
    const second = await bearer!.post('/plugin/v1/notify', { headers: plug, data: { kind: 'knock', ref, count: 2 } });
    expect(second.status(), await second.text()).toBe(429);
    expect(Number(second.headers()['retry-after']), 'Retry-After').toBeGreaterThan(0);
    expect(server!.log()).toContain(`push: dropped tag=${tag} event=app owner=`);

    // --- The withdrawal: payload v2's silent resolved under the same tag.
    const del = await bearer!.delete(`/plugin/v1/notify/${ref}`, { headers: plug });
    expect(del.status(), await del.text()).toBe(202);
    const resolved = await nthToPhone(2);
    expect(resolved).toEqual({ v: 2, type: 'resolved', id: tag, tag });

    // The capped knock never arrived: the withdrawal is the very next push
    // (nthToPhone(2) above), and nothing came after it. The other checks:
    // no push carries the guest's name, and the proxy refused nothing.
    await page.waitForTimeout(1_500);
    expect(toPhone().length, 'pushes to the phone: the probe, the knock, the withdrawal').toBe(3);
    for (let i = 0; i < 3; i++) expect(JSON.stringify(await nthToPhone(i))).not.toContain('Ada');
    expect(received().filter((r) => r.refused), 'CONNECTs the sink refused').toEqual([]);
  } finally {
    await session.post(origin + `/plugins/${id}/revoke`, { headers: { Origin: origin }, maxRedirects: 0 });
  }
});
