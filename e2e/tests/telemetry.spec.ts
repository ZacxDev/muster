import http from 'node:http';
import { AddressInfo } from 'node:net';
import { test, expect } from '../lib/fixtures';
import { telemetryPort } from '../lib/env';
import { MirrorRig, cc } from '../lib/mirrorhost';

// OPENING A SESSION, TRACED (docs/telemetry.md), against a REAL muster-server
// and a REAL muster-mirror (the e2emirror build: only tmux is faked), with
// tracing on in both and the browser's telemetry relayed same-origin.
//
// The receivers are this spec's own fakes: an OTLP/HTTP endpoint that keeps
// every export it is sent (both binaries export to it), and a Faro receiver
// behind muster's relay (MUSTER_FARO_UPSTREAM) that keeps every batch. The
// spec asserts on what ARRIVED there, not on what the page meant to send.

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

interface Post {
  path: string;
  body: Buffer;
}

// receiver is a loopback HTTP server that keeps every POST.
async function receiver(status: number): Promise<{ url: string; posts: Post[]; close: () => Promise<void> }> {
  const posts: Post[] = [];
  const srv = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => {
      posts.push({ path: req.url ?? '', body: Buffer.concat(chunks) });
      res.writeHead(status, { 'Content-Type': 'application/json' });
      res.end('{}');
    });
  });
  await new Promise<void>((r) => srv.listen(0, '127.0.0.1', () => r()));
  const { port } = srv.address() as AddressInfo;
  return { url: `http://127.0.0.1:${port}`, posts, close: () => new Promise((r) => srv.close(() => r())) };
}

let otlp: Awaited<ReturnType<typeof receiver>>;
let faro: Awaited<ReturnType<typeof receiver>>;
let rig: MirrorRig;

test.beforeAll(async () => {
  otlp = await receiver(200);
  faro = await receiver(202);
  rig = new MirrorRig(telemetryPort, 'e2e-otel', {
    OTEL_EXPORTER_OTLP_ENDPOINT: otlp.url,
    MUSTER_FARO_UPSTREAM: faro.url + '/collect',
    MUSTER_FARO_TRACING: '1',
  });
  await rig.start();
});

test.afterAll(async () => {
  await rig.stop();
  await otlp.close();
  await faro.close();
});

async function startHost(): Promise<void> {
  const tr = rig.transcript(cc.prompt('Synthetic question about the weather.') + cc.reply('Synthetic answer: it is sunny.'));
  await rig.startHost({ MUSTER_MIRROR_E2E_SESSIONS: 'main', MUSTER_MIRROR_E2E_CLAUDE: 'main=' + tr, OTEL_EXPORTER_OTLP_ENDPOINT: otlp.url });
}

// faroItems is every item of a kind (events, measurements) the relay
// delivered to the fake Faro receiver.
function faroItems(kind: 'events' | 'measurements' | 'exceptions' | 'logs'): Record<string, any>[] {
  const out: Record<string, any>[] = [];
  for (const p of faro.posts) {
    try {
      const b = JSON.parse(p.body.toString('utf8'));
      for (const it of b[kind] ?? []) out.push(it);
    } catch {
      // not a Faro batch
    }
  }
  return out;
}

// exportedBy is the OTLP exports that name service s (an OTLP/protobuf body
// carries its resource's strings as raw UTF-8).
function exportedBy(s: string): Buffer[] {
  return otlp.posts.filter((p) => p.path === '/v1/traces' && p.body.includes(Buffer.from(s))).map((p) => p.body);
}

test('the document names its server trace, and the open is one trace from the page to the host', async ({ page }) => {
  await startHost();
  await rig.login(page);
  await rig.waitListed(page, 'main');

  const resp = await page.goto(rig.origin + rig.viewPath('main'));
  expect(resp, 'the view page answered').toBeTruthy();
  // The document's own server span, for the page to join (Server-Timing),
  // and the server's phases beside it.
  const timing = (await resp!.headerValue('server-timing')) ?? '';
  const tp = /traceparent;desc="(00-([0-9a-f]{32})-[0-9a-f]{16}-0[01])"/.exec(timing);
  expect(tp, `the document's Server-Timing: ${timing}`).not.toBeNull();
  const traceId = tp![2];
  expect(timing).toMatch(/\bentry;dur=[\d.]+/);
  expect(timing).toMatch(/\btotal;dur=[\d.]+/);

  await expect(page.locator('#mv-log')).toContainText('Synthetic answer: it is sunny.');

  // The page's open: a measurement the relay delivered, ok, carrying the
  // marks of the open and the document's trace id.
  await expect
    .poll(() => faroItems('measurements').filter((m) => m.type === 'muster_open' && m.context?.view === 'mirror_view'), {
      message: 'the open measurement reached the Faro receiver through the relay',
      timeout: 15_000,
    })
    .not.toHaveLength(0);
  const open = faroItems('measurements').find((m) => m.type === 'muster_open' && m.context?.view === 'mirror_view')!;
  expect(open.context.outcome).toBe('ok');
  expect(open.context.trace_id).toBe(traceId);
  for (const k of ['stream_connected_ms', 'first_paint_ms', 'usable_ms', 'total_ms']) {
    expect(typeof open.values[k], `the open measured ${k}`).toBe('number');
  }

  // Both binaries exported spans of THAT trace: the server's, and the
  // host's — the host learned the trace only from the watch frame, which
  // learned it from the socket's ?tp=, which the page took from the document.
  const id = Buffer.from(traceId, 'hex');
  await expect
    .poll(() => exportedBy('muster-mirror').some((b) => b.includes(id)), {
      message: "the host exported a span in the document's trace",
      timeout: 20_000,
    })
    .toBe(true);
  expect(exportedBy('muster-server').some((b) => b.includes(id)), "the server exported a span in the document's trace").toBe(true);

  // 🔴 NO QUERY STRING LEFT THE PAGE: the view's URL names its host and
  // session in the query, and no Faro batch carries it.
  for (const p of faro.posts) {
    expect(p.body.toString('utf8')).not.toContain('session=main');
  }
});

test('a disconnect is reported with its reason, by the page and by the server', async ({ page }) => {
  await startHost();
  await rig.login(page);
  await rig.waitListed(page, 'main');
  await page.goto(rig.origin + rig.viewPath('main'));
  await expect(page.locator('#mv-log')).toContainText('Synthetic answer: it is sunny.');
  const before = faroItems('events').filter((e) => e.name === 'muster.session_disconnected').length;

  // The host goes away while the view is live: the server ends the stream
  // with its reason, and the page reports why it disconnected.
  await rig.stopHost();
  await expect(page.locator('#mv-status')).toHaveText(/Disconnected/);
  await expect
    .poll(() => faroItems('events').filter((e) => e.name === 'muster.session_disconnected').slice(before), {
      message: 'the disconnect reached the Faro receiver through the relay',
      timeout: 15_000,
    })
    .not.toHaveLength(0);
  const ev = faroItems('events').filter((e) => e.name === 'muster.session_disconnected').slice(before)[0];
  expect(ev.attributes.view).toBe('mirror_view');
  expect(ev.attributes.reason).toBe('host_disconnected');
  expect(ev.attributes.phase).toBe('live');

  // The server counted the same end, by the same closed word.
  const metrics = await (await page.request.get(rig.origin + '/metrics')).text();
  expect(metrics).toMatch(/muster_view_disconnects_total\{reason="host_disconnected",view="mirror_view"\} [1-9]/);
});

// 🔴 NO FREE TEXT REACHES THE RECEIVER. A console line and a thrown error can
// carry anything the page holds (pane text, a prompt, a title), so the
// console is not captured and an exception keeps its shape, never its text.
// The sentinel is logged at every console level, thrown, and rejected, on a
// live view; it is in no batch the relay delivered.
//
// The positive controls: the relay DID deliver this page's events (a marker
// pushed after the sentinel), and the thrown error DID arrive as an exception
// — so a sentinel-free receiver is the scrub working, not a dead pipe.
test('a console line or a thrown error never carries its text to the receiver', async ({ page }) => {
  const sentinel = 'SENTINEL' + 'faro9d2';
  await startHost();
  await rig.login(page);
  await rig.waitListed(page, 'main');
  await page.goto(rig.origin + rig.viewPath('main'));
  await expect(page.locator('#mv-log')).toContainText('Synthetic answer: it is sunny.');
  const exceptionsBefore = faroItems('exceptions').length;

  await page.evaluate((s) => {
    console.log('pane text: ' + s);
    console.info('prompt: ' + s);
    console.warn('title: ' + s);
    console.error('error: ' + s);
    setTimeout(() => {
      throw new Error('thrown with ' + s);
    }, 0);
    void Promise.reject(new Error('rejected with ' + s));
  }, sentinel);
  // The marker, after the sentinel's every source has fired.
  await page.waitForTimeout(300);
  await page.evaluate(() => (window as any).cgTrack('muster.e2e_marker', { step: 'after' }));

  await expect
    .poll(() => faroItems('events').some((e) => e.name === 'muster.e2e_marker'), {
      message: 'POSITIVE CONTROL: the relay delivered the marker event',
      timeout: 15_000,
    })
    .toBe(true);
  await expect
    .poll(() => faroItems('exceptions').length, { message: 'POSITIVE CONTROL: the thrown error arrived as an exception', timeout: 15_000 })
    .toBeGreaterThan(exceptionsBefore);

  for (const p of faro.posts) {
    expect(p.body.toString('utf8'), 'a batch the relay delivered').not.toContain(sentinel);
  }
  expect(faroItems('logs'), 'no console line was sent').toHaveLength(0);
  for (const ex of faroItems('exceptions').slice(exceptionsBefore)) {
    expect(ex.value).toBe('[scrubbed]');
  }
});
