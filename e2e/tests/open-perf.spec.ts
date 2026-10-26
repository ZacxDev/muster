import fs from 'node:fs';
import http from 'node:http';
import { AddressInfo } from 'node:net';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { openPerfPort } from '../lib/env';
import { MirrorRig, cc } from '../lib/mirrorhost';
import { Profile, THROTTLED, throttle } from '../lib/throttle';

// HOW LONG OPENING A MIRRORED VIEW TAKES IN THE BROWSER, measured from outside
// the page's own code, so the same numbers are read from any build.
//
// One fake host whose `main` window holds a long Claude Code conversation
// (100 prompts, each with two tool calls, and 100 markdown replies), served
// with the browser telemetry ON (Faro relayed to this spec's own receiver),
// so the page's head is the one a deployment serves. Each case is a fresh
// browser context:
//
//   - a COLD open: nothing stored, the whole conversation arrives in the
//     socket's first batch;
//   - the conversation then goes on while it is watched (120 more tool calls
//     and replies, each its own small batch), so the snapshot the page keeps
//     is tens of batches, as an operator's is;
//   - three WARM opens: the snapshot is read, painted and resumed from.
//
// What is read, per open, from an init script that wraps WebSocket and reads
// the navigation entry (never the page's telemetry, which the base build does
// not have): the document's responseEnd, DOMContentLoaded and load; when the
// view's socket was created and its first batch received; and `visible`, the
// first animation frame in which the log holds the whole conversation.
//
// 🔴 THE BOUND IS ON GAP 2: DOCUMENT LOADED → SOCKET CREATED, warm, throttled.
// The page used to wait for DOMContentLoaded, then read the snapshot, then
// paint all of it (a forced layout per batch), and only then create the
// socket. The bound is the median of three warm opens ≤ GAP2_BOUND_MS; the
// measured values before and after are in the pull request that added it.
//
// ⚠ Playwright's trace recorder is OFF here: it snapshots the DOM in the page
// on every action, and that is main-thread time inside the open (measured:
// 236 ms of a throttled open). For the same reason nothing polls a locator
// while an open runs.
test.use({ storageState: { cookies: [], origins: [] }, trace: 'off' });

const GAP2_BOUND_MS = 60;
const WARM_OPENS = 3;

const sentences = (i: number, n: number) =>
  Array.from({ length: n }, (_, j) => `Sentence ${j + 1} about module-${i}: the value was checked against the expected default and matched after the last migration ran.`).join(' ');

// The conversation: every value is invented fixture data.
function conversation(): string {
  let out = '';
  for (let i = 0; i < 200; i++) {
    if (i % 2 === 0) {
      out += cc.prompt(`Step ${i / 2 + 1}: ${sentences(i, 1)}`);
      out += cc.toolUse(`tu-${i}-a`, 'Read', { file_path: `/src/module-${i}.go` });
      out += cc.toolResult(`tu-${i}-a`, sentences(i, 2));
      out += cc.toolUse(`tu-${i}-b`, 'Bash', { command: `make test-${i}` });
      out += cc.toolResult(`tu-${i}-b`, sentences(i, 1));
    } else {
      out += cc.reply(`## Module ${i}\n\nChecked **module-${i}**. ${sentences(i, 2)}\n\n- one \`key_a\`\n- two \`key_b\`\n\n\`\`\`sh\nmake build-${i}\n\`\`\`\n\n${sentences(i, 3)}`);
    }
  }
  return out;
}

// probe records the open's moments (performance.now()) on window.__open. The
// bubble count that makes the conversation `visible` is read from
// sessionStorage, set by the spec before the navigation.
const probe = `(() => {
  const T = window.__open = { ws: null, frame: null, visible: null, want: 0 };
  const WS = window.WebSocket;
  function W(url, p) {
    const s = p === undefined ? new WS(url) : new WS(url, p);
    if (/\\/mirror\\/view\\/ws/.test(String(url)) && !/warm=1/.test(String(url)) && T.ws === null) {
      T.ws = performance.now();
      s.addEventListener('message', (ev) => {
        if (T.frame !== null) return;
        try { if (JSON.parse(ev.data).type !== 'batch') return; } catch (e) { return; }
        T.frame = performance.now();
      });
    }
    return s;
  }
  W.prototype = WS.prototype; W.CONNECTING = 0; W.OPEN = 1; W.CLOSING = 2; W.CLOSED = 3;
  window.WebSocket = W;
  try { T.want = Number(sessionStorage.getItem('e2e-open-want') || 0); } catch (e) {}
  if (!T.want || location.pathname !== '/mirror/view') return;
  const f = () => {
    const log = document.getElementById('mv-log');
    if (log && log.querySelectorAll('[data-bubble]').length >= T.want) { T.visible = performance.now(); return; }
    requestAnimationFrame(f);
  };
  requestAnimationFrame(f);
})()`;

interface Open {
  responseEnd: number;
  dcl: number;
  load: number;
  ws: number;
  frame: number;
  visible: number;
  gap2: number;
}

async function receiver(): Promise<{ url: string; close: () => Promise<void> }> {
  const srv = http.createServer((req, res) => {
    req.on('data', () => {});
    req.on('end', () => {
      res.writeHead(202, { 'Content-Type': 'application/json' });
      res.end('{}');
    });
  });
  await new Promise<void>((r) => srv.listen(0, '127.0.0.1', () => r()));
  const { port } = srv.address() as AddressInfo;
  return { url: `http://127.0.0.1:${port}`, close: () => new Promise((r) => srv.close(() => r())) };
}

let rig: MirrorRig;
let faro: Awaited<ReturnType<typeof receiver>>;
let state: object = {};

const bubbles = (page: Page) => page.locator('#mv-log [data-bubble]').count();

// settledCount waits until the log's bubble count holds still for a second.
async function settledCount(page: Page): Promise<number> {
  let last = -1;
  await expect
    .poll(
      async () => {
        const n = await bubbles(page);
        const same = n > 0 && n === last;
        last = n;
        return same;
      },
      { timeout: 60_000, intervals: [1000] },
    )
    .toBe(true);
  return last;
}

test.beforeAll(async ({ browser }) => {
  test.setTimeout(180_000);
  faro = await receiver();
  rig = new MirrorRig(openPerfPort, 'e2e-openperf', { MUSTER_FARO_UPSTREAM: faro.url + '/collect', MUSTER_FARO_TRACING: '1' });
  await rig.start();
  await rig.startHost({ MUSTER_MIRROR_E2E_SESSIONS: 'main', MUSTER_MIRROR_E2E_CLAUDE: 'main=' + rig.transcript(conversation()) });
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await rig.login(page);
  await rig.waitListed(page, 'data-mirror-claude');
  state = await ctx.storageState();
  await ctx.close();
});
test.afterAll(async () => {
  await rig?.stop();
  await faro?.close();
});

// openOnce navigates to the view and reads the probe once the whole
// conversation (want bubbles) is visible and the document has loaded.
async function openOnce(page: Page, want: number): Promise<Open> {
  await page.goto(rig.origin + '/readyz');
  await page.evaluate((w) => sessionStorage.setItem('e2e-open-want', String(w)), want);
  await page.goto(rig.origin + rig.viewPath('main'), { waitUntil: 'commit' });
  await page.waitForFunction(() => (window as any).__open?.visible != null && (window as any).__open?.frame != null && document.readyState === 'complete', null, { polling: 100, timeout: 30_000 });
  return page.evaluate(() => {
    const n = performance.getEntriesByType('navigation')[0] as PerformanceNavigationTiming;
    const o = (window as any).__open;
    const r = (x: number) => Math.round(x);
    return { responseEnd: r(n.responseEnd), dcl: r(n.domContentLoadedEventStart), load: r(n.loadEventStart), ws: r(o.ws), frame: r(o.frame), visible: r(o.visible), gap2: r(o.ws - n.loadEventStart) };
  });
}

const median = (xs: number[]) => [...xs].sort((a, b) => a - b)[Math.floor(xs.length / 2)];
const line = (o: Open) =>
  `responseEnd ${o.responseEnd} · DCL ${o.dcl} · load ${o.load} · socket ${o.ws} · first batch ${o.frame} · visible ${o.visible} · gap2 (load→socket) ${o.gap2}`;

const viewports = [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
];

for (const vp of viewports) {
  for (const p of [null, THROTTLED] as (Profile | null)[]) {
    const pname = p ? 'throttled' : 'unthrottled';
    test(`${vp.name}, ${pname}: a warm open creates its socket ${p ? `at most ${GAP2_BOUND_MS} ms after load` : 'soon after load (reported only)'}`, async ({ browser }) => {
      test.setTimeout(240_000);
      const tr = rig.transcript(conversation());
      await rig.startHost({ MUSTER_MIRROR_E2E_SESSIONS: 'main', MUSTER_MIRROR_E2E_CLAUDE: 'main=' + tr });
      // How many bubbles the whole conversation is, read in a throwaway
      // context (its snapshot store is its own).
      const probeCtx = await browser.newContext({ storageState: state as never });
      const pp = await probeCtx.newPage();
      await rig.waitListed(pp, 'data-mirror-claude');
      await pp.goto(rig.origin + rig.viewPath('main'));
      const coldCount = await settledCount(pp);
      await probeCtx.close();

      const ctx = await browser.newContext({ ...vp.use, storageState: state as never });
      const page = await ctx.newPage();
      await page.addInitScript(probe);
      try {
        await throttle(page, p);
        const cold = await openOnce(page, coldCount);
        console.log(`[open-perf ${vp.name}, ${pname}] cold: ${line(cold)}`);
        // The conversation goes on while it is watched: many small batches.
        for (let i = 0; i < 120; i++) {
          fs.appendFileSync(tr, cc.toolUse(`live-${i}`, 'Bash', { command: `make step-${i}` }) + cc.toolResult(`live-${i}`, sentences(i, 1)) + cc.reply(`Live step ${i}: ${sentences(i, 1)}`));
          await page.waitForTimeout(60);
        }
        const drawn = await settledCount(page);
        expect(drawn, 'the live replies were drawn').toBeGreaterThan(coldCount);
        // The save is 800 ms after the last batch.
        await page.waitForTimeout(1500);
        const warm: Open[] = [];
        for (let i = 0; i < WARM_OPENS; i++) {
          const o = await openOnce(page, drawn);
          // CONTROL: this open painted a snapshot of tens of batches, so it
          // measured the path the bound is about.
          const snap = await page.evaluate(async () => {
            const s = (window as any).musterSnap;
            const r = await s.get(s.refOf(document.getElementById('mv-log')!.getAttribute('data-ws')));
            return r ? r.batches.length : 0;
          });
          expect(snap, 'the stored snapshot is many batches').toBeGreaterThan(20);
          expect(await bubbles(page), 'the warm open drew the whole conversation').toBe(drawn);
          console.log(`[open-perf ${vp.name}, ${pname}] warm ${i + 1} (snapshot of ${snap} batches): ${line(o)}`);
          warm.push(o);
          await page.waitForTimeout(1500);
        }
        const g2 = median(warm.map((o) => o.gap2));
        console.log(
          `[open-perf ${vp.name}, ${pname}] warm medians: gap2 ${g2} · socket ${median(warm.map((o) => o.ws))} · visible ${median(warm.map((o) => o.visible))} · first batch ${median(warm.map((o) => o.frame))}`,
        );
        if (p) expect(g2, `median load→socket over ${WARM_OPENS} warm opens`).toBeLessThanOrEqual(GAP2_BOUND_MS);
      } finally {
        await ctx.close();
      }
    });
  }
}
