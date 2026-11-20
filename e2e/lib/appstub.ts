// The STUB APP PLUGIN the app tab's specs frame (plugin-app.spec.ts, and
// cls.spec.ts's measurement of /apps/{id}): a tiny HTTP server on loopback,
// reached by the browser as https://w.<zone> through the previews specs' own
// TLS edge (e2e/previewfix edge) and Chromium's --host-resolver-rules, so the
// frame's origin is a real https origin under MUSTER_PLUGIN_ORIGINS with the
// default port, as ValidViewerLink requires of an entry URL.
//
// 🔴 NO page.route / context.route IN A CONTEXT THAT USES IT. Measured on
// Chromium 153 (Playwright 1.63): with any route registered on the context,
// ignoreHTTPSErrors no longer covers a SUBFRAME's certificate, and the frame
// loads chrome-error://chromewebdata/ instead of the stub. A real server on
// the far side of the edge is the only way the frame reaches the stub.
//
// What it serves (by the request's Host):
//   - w.<zone> GET /enter?muster_code=…&muster_plugin=… — the plugin's entry
//     route: REDEEMS the code with the plugin's own token on the real muster
//     server (POST /plugin/v1/handoffs/redeem) and records the answer, then
//     serves the stub page;
//   - w.<zone> GET /probe?run=… — the same stub page with no code (the
//     negative controls' framings, below, load this);
//   - control.<zone> GET /frame?case=… — a TEST-ONLY framing page: the stub
//     in an iframe carrying the production sandbox, with the `allow`
//     attribute and the Permissions-Policy header under test (FRAMINGS). It
//     is the stub's own origin's sibling, never a muster route;
//   - POST /report — what the stub page reports (JSON), kept in `reports`.
//
// The stub page (media on) asks for getUserMedia({audio, video}) and
// getDisplayMedia(), reads document.featurePolicy.allowsFeature for camera,
// microphone and display-capture, and reports each result; then it reports a
// heartbeat with its tracks' states every second, and `pagehide` (a beacon)
// when its document goes. Every name and value here is invented for the suite.
import { spawn, ChildProcess, execFileSync } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { binDir, repoRoot } from './env';

export const APP_ZONE_HOST = 'app.example.test';
export const APP_ZONE = `https://*.${APP_ZONE_HOST}`;
export const APP_ORIGIN = `https://w.${APP_ZONE_HOST}`;
export const APP_ENTRY = `${APP_ORIGIN}/enter`;
export const CONTROL_ORIGIN = `https://control.${APP_ZONE_HOST}`;

// The production frame's sandbox (internal/ui PluginChipSandbox), spelled
// here as the design spells it.
export const APP_SANDBOX = 'allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals';
const O = APP_ORIGIN;
const fullAllow = `camera ${O}; microphone ${O}; display-capture ${O}; fullscreen ${O}; autoplay ${O}; clipboard-write ${O}`;
const fullPolicy =
  `microphone=(self "${O}"), camera=(self "${O}"), display-capture=(self "${O}"), ` +
  `fullscreen=(self "${O}"), autoplay=(self "${O}"), clipboard-write=(self "${O}")`;

// The test-only framings. Each is LITERAL, never derived from what muster
// sends, so a regression in muster cannot move a control with it.
//   - full: the design's header and allow, from a page that is not muster's.
//     The instrument's positive control: the control origin CAN grant all
//     three, so a refusal below is the case's, not the origin's.
//   - noallow: no allow attribute at all.
//   - nodc: display-capture left out of allow.
//   - noself: the first revision's header (spike S5): camera and
//     display-capture delegated to the frame's origin WITHOUT self.
export const FRAMINGS: Record<string, { policy: string; allow: string | null }> = {
  full: { policy: fullPolicy, allow: fullAllow },
  noallow: { policy: fullPolicy, allow: null },
  nodc: { policy: fullPolicy, allow: `camera ${O}; microphone ${O}; fullscreen ${O}; autoplay ${O}; clipboard-write ${O}` },
  noself: {
    policy:
      `microphone=(self "${O}"), camera=("${O}"), display-capture=("${O}"), ` +
      `fullscreen=(self "${O}"), autoplay=(self "${O}"), clipboard-write=(self "${O}")`,
    allow: fullAllow,
  },
};

export type Track = { kind: string; readyState: string };
export type MediaResult = { ok: true; tracks: Track[] } | { ok: false; error: string };
export type Report =
  | { run: string; event: 'result'; allows: Record<string, boolean | null>; um: MediaResult; dm: MediaResult; at: number }
  | { run: string; event: 'beat' | 'pagehide'; tracks: Track[]; at: number }
  | { run: string; event: 'ended'; kind: string; at: number };
export type Entry = { run: string; code: string; plugin: string; status: number; body: any; at: number };

// The chromium flags the stub's specs launch with: the fake devices and UI
// (so a GRANTED request needs no prompt), the screen picked automatically, and
// every name in the zone sent to the edge.
export function appStubArgs(edgePort: number): string[] {
  return [
    '--use-fake-device-for-media-stream',
    '--use-fake-ui-for-media-stream',
    '--auto-select-desktop-capture-source=Entire screen',
    `--host-resolver-rules=MAP *.${APP_ZONE_HOST} 127.0.0.1:${edgePort}`,
  ];
}

function page(run: string, redeem: string, media: boolean): string {
  const script = !media
    ? ''
    : `<script>
(function () {
  var run = ${JSON.stringify(run)};
  var tracks = [];
  function send(o) { o.run = run; return fetch('/report', { method: 'POST', body: JSON.stringify(o), keepalive: true }).catch(function () {}); }
  function states() { return tracks.map(function (t) { return { kind: t.kind, readyState: t.readyState }; }); }
  function keep(s) { s.getTracks().forEach(function (t) { tracks.push(t); t.addEventListener('ended', function () { send({ event: 'ended', kind: t.kind }); }); }); }
  async function ask(fn) {
    try { var s = await fn(); keep(s); return { ok: true, tracks: s.getTracks().map(function (t) { return { kind: t.kind, readyState: t.readyState }; }) }; }
    catch (e) { return { ok: false, error: (e && e.name) || String(e) }; }
  }
  (async function () {
    var allows = {};
    ['camera', 'microphone', 'display-capture'].forEach(function (f) {
      allows[f] = document.featurePolicy ? document.featurePolicy.allowsFeature(f) : null;
    });
    var um = await ask(function () { return navigator.mediaDevices.getUserMedia({ audio: true, video: true }); });
    var dm = await ask(function () { return navigator.mediaDevices.getDisplayMedia({ video: true }); });
    document.getElementById('stub-result').textContent = JSON.stringify({ allows: allows, um: um, dm: dm });
    await send({ event: 'result', allows: allows, um: um, dm: dm });
    setInterval(function () { send({ event: 'beat', tracks: states() }); }, 1000);
    addEventListener('pagehide', function () {
      navigator.sendBeacon('/report', JSON.stringify({ run: run, event: 'pagehide', tracks: states() }));
    });
  })();
})();
</script>`;
  return `<!doctype html><html><head><meta charset="utf-8"><title>Stub app</title></head>
<body><h1 id="stub-app">stub app</h1><p id="stub-redeem">${redeem}</p><p id="stub-result"></p>${script}</body></html>`;
}

function framing(name: string, run: string): { headers: Record<string, string>; body: string } {
  const f = FRAMINGS[name];
  const allow = f.allow === null ? '' : ` allow="${f.allow}"`;
  return {
    headers: { 'Content-Type': 'text/html; charset=utf-8', 'Permissions-Policy': f.policy, 'Cache-Control': 'no-store' },
    body: `<!doctype html><html><head><meta charset="utf-8"><title>Framing ${name}</title></head><body style="margin:0">
<iframe id="framed" sandbox="${APP_SANDBOX}"${allow} referrerpolicy="no-referrer" style="width:100%;height:90vh;border:0"
 src="${APP_ORIGIN}/probe?run=${encodeURIComponent(run)}"></iframe></body></html>`,
  };
}

export class AppStub {
  readonly entries: Entry[] = [];
  readonly reports: Report[] = [];
  // tokens are the app plugins' tokens by plugin id, set as each is minted:
  // the entry route redeems a code with its muster_plugin's token.
  readonly tokens = new Map<string, string>();
  private server?: http.Server;
  private edge?: ChildProcess;
  private edgeLog = '';
  private seq = 0;
  // dir holds the edge's log: made by start(), removed by stop().
  private dir = '';

  constructor(
    readonly musterOrigin: string,
    readonly stubPort: number,
    readonly edgePort: number,
    readonly media = true,
  ) {}

  nextRun(prefix: string): string {
    return `${prefix}-${++this.seq}-${Date.now().toString(36)}`;
  }

  reportsOf(run: string): Report[] {
    return this.reports.filter((r) => r.run === run);
  }

  result(run: string): Extract<Report, { event: 'result' }> | undefined {
    return this.reports.find((r) => r.run === run && r.event === 'result') as Extract<Report, { event: 'result' }> | undefined;
  }

  get log(): string {
    return this.edgeLog;
  }

  async start(): Promise<void> {
    this.dir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-e2e-appstub-'));
    this.server = http.createServer((req, res) => void this.handle(req, res));
    await new Promise<void>((resolve) => this.server!.listen(this.stubPort, '127.0.0.1', resolve));
    const fixBin = path.join(binDir, 'previewfix');
    fs.mkdirSync(binDir, { recursive: true });
    execFileSync('go', ['build', '-o', fixBin, './e2e/previewfix'], { cwd: repoRoot, stdio: 'inherit' });
    this.edge = spawn(fixBin, ['edge', '-listen', `127.0.0.1:${this.edgePort}`, '-upstream', `127.0.0.1:${this.stubPort}`, '-log', path.join(this.dir, 'edge.jsonl')], {
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    this.edge.stdout?.on('data', (d) => (this.edgeLog += d));
    this.edge.stderr?.on('data', (d) => (this.edgeLog += d));
    const deadline = Date.now() + 20_000;
    while (!(await tcpOpen(this.edgePort))) {
      if (this.edge.exitCode !== null) throw new Error(`previewfix edge exited ${this.edge.exitCode}:\n${this.edgeLog}`);
      if (Date.now() > deadline) throw new Error(`previewfix edge did not listen on ${this.edgePort}:\n${this.edgeLog}`);
      await new Promise((r) => setTimeout(r, 100));
    }
  }

  async stop(): Promise<void> {
    if (this.edge && this.edge.exitCode === null) {
      const done = new Promise((r) => this.edge!.once('exit', r));
      this.edge.kill('SIGTERM');
      await done;
    }
    await new Promise<void>((resolve) => (this.server ? this.server.close(() => resolve()) : resolve()));
    if (this.dir) fs.rmSync(this.dir, { recursive: true, force: true });
    this.dir = '';
  }

  private async handle(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
    const host = (req.headers.host ?? '').toLowerCase();
    const url = new URL(req.url ?? '/', `https://${host}`);
    const html = (body: string, headers: Record<string, string> = {}) => {
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store', ...headers });
      res.end(body);
    };
    if (req.method === 'POST' && url.pathname === '/report') {
      let body = '';
      for await (const c of req) body += c;
      try {
        this.reports.push({ ...JSON.parse(body), at: Date.now() });
      } catch {
        /* a torn beacon is not a report */
      }
      res.writeHead(204).end();
      return;
    }
    if (host === `w.${APP_ZONE_HOST}` && url.pathname === '/enter') {
      const run = this.nextRun('entry');
      const code = url.searchParams.get('muster_code') ?? '';
      const plugin = url.searchParams.get('muster_plugin') ?? '';
      const r = await fetch(this.musterOrigin + '/plugin/v1/handoffs/redeem', {
        method: 'POST',
        headers: { Authorization: `Bearer ${this.tokens.get(plugin) ?? ''}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ code }),
      });
      const body = await r.json().catch(() => ({}));
      this.entries.push({ run, code, plugin, status: r.status, body, at: Date.now() });
      html(page(run, `${r.status} ${body?.purpose ?? ''}`, this.media));
      return;
    }
    if (host === `w.${APP_ZONE_HOST}` && url.pathname === '/probe') {
      html(page(url.searchParams.get('run') ?? 'probe', 'no code', this.media));
      return;
    }
    if (host === `control.${APP_ZONE_HOST}` && url.pathname === '/frame') {
      const name = url.searchParams.get('case') ?? '';
      if (!FRAMINGS[name]) {
        res.writeHead(404).end('no such framing');
        return;
      }
      const f = framing(name, url.searchParams.get('run') ?? name);
      res.writeHead(200, f.headers);
      res.end(f.body);
      return;
    }
    res.writeHead(404).end('not the stub');
  }
}

function tcpOpen(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const s = net.connect(port, '127.0.0.1');
    s.once('connect', () => {
      s.destroy();
      resolve(true);
    });
    s.once('error', () => resolve(false));
  });
}
