import fs from 'node:fs';
import { Browser, BrowserContext, Page, WebSocketRoute } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { reconnectPort } from '../lib/env';
import { launchWithBfcache } from '../lib/bfcache';
import { cc, mkchat, MirrorRig } from '../lib/mirrorhost';

// LIVE CONNECTIONS COME BACK BY THEMSELVES (internal/ui/live.go). Operator ask:
// "Autoreconnect when returning or reopening a stale page (ex returning to app
// after it went to sleep). Also make live terminal keys always enabled and hide
// the button for it."
//
// Against a REAL muster-server (beating every 500 ms: MUSTER_LIVE_HEARTBEAT, so
// a silent socket is stale after about a second) and a REAL muster-mirror built
// with `-tags e2emirror`, whose window work @1 runs Claude Code over a transcript
// this spec appends to. An agent pod's terminal is the e2eterm fake
// (cmd/muster-server/e2eterm.go), which clears the screen and prints
// `FAKE-TUI mode=…` on every attach and `in=<HEX>` for every input.
//
// 🔴 "ASLEEP" IS BUILT FROM TWO THINGS A SLEEPING PHONE DOES, BOTH REAL TO THE
// PAGE: its socket goes SILENT while still looking open (routeWebSocket stops
// relaying in both directions — the server's heartbeats included — and closes
// nothing), and the page is HIDDEN (document.hidden / visibilityState, with
// the visibilitychange a browser fires; headless Chromium cannot hide a page
// itself). Waking is the page shown again. Nothing taps anything.
//
// 🔴 SOCKETS ARE COUNTED IN THE PAGE (instrument below): every socket the
// document makes, and its readyState now. Playwright's own websocket event
// does not see a socket a RESTORED document opens (bfcache-restore.spec).
//
// 🔴 THE REASON A RECONNECT GIVES IS READ FROM THE PAGE'S OWN TELEMETRY CALLS.
// window.musterOpen (faro.go) is absent while telemetry is off, as it is here;
// the instrument installs a recorder in its place, so each open's view and
// attributes (reconnect, reconnect_reason) are what the page passed to it.
test.use({ storageState: { cookies: [], origins: [] } });

const instrument = `(() => {
  window.__socks = [];
  const C = window.WebSocket;
  const N = function (url, p) {
    const o = p === undefined ? new C(url) : new C(url, p);
    window.__socks.push({ url: String(url), o });
    return o;
  };
  N.prototype = C.prototype;
  for (const k of ['CONNECTING', 'OPEN', 'CLOSING', 'CLOSED']) N[k] = C[k];
  window.WebSocket = N;
  window.__opens = [];
  const h = { tp: () => '', mark() {}, markAt() {}, attr() {}, flag() {}, count() {}, end() {}, disconnected() {} };
  window.musterOpen = {
    begin(view, attrs) { window.__opens.push({ view, attrs: Object.assign({}, attrs || {}) }); return h; },
    disconnected() {}, quiet(fn) { setTimeout(fn, 0); }, attach() {},
  };
  let hidden = false;
  Object.defineProperty(Document.prototype, 'hidden', { configurable: true, get() { return hidden; } });
  Object.defineProperty(Document.prototype, 'visibilityState', { configurable: true, get() { return hidden ? 'hidden' : 'visible'; } });
  window.__setHidden = (v) => { hidden = v; document.dispatchEvent(new Event('visibilitychange')); };
})()`;

const rig = new MirrorRig(reconnectPort, 'e2e-recon', {
  MUSTER_LIVE_HEARTBEAT: '500ms',
  MUSTER_AGENT_KINDS: 'gateway,claude-code',
  MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
  MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
  MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-reconnect-alpha',
  // A chat wire, so an agent's chat socket is served (nothing listens behind
  // it, as in agent-composer.spec: no turn is sent here).
  MUSTER_AGENT_GATEWAY: 'hooks-sha256',
  MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
  MUSTER_AGENT_RUNTIME_CONFIG: '{}',
  MUSTER_AGENT_RUNTIME_INSTALL: 'true',
});
let trPath = '';
const termAgent = 'e2e-recon-term-' + Date.now().toString(36);
const chatAgent = 'e2e-recon-chat-' + Date.now().toString(36);

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
  trPath = rig.transcript(cc.prompt('the prompt of the window that sleeps') + cc.reply('The first answer.'), 'recon');
  const now = Math.floor(Date.now() / 1000);
  const windows = rig.transcript(`work|@1|1|1|claude|${now - 60}|${trPath}\n`, 'windows');
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms', MUSTER_MIRROR_INTERACTIVE: '1' });
  mkchat(termAgent, [], { MUSTER_E2E_KIND: 'claude-code', MUSTER_E2E_CC_ACCOUNT: 'alpha' });
  mkchat(chatAgent, [
    { role: 'user', content: 'Synthetic question for the reconnect spec.' },
    { role: 'assistant', content: 'Synthetic answer for the reconnect spec.' },
  ]);
});
test.afterAll(async () => {
  mkchat(termAgent, null);
  mkchat(chatAgent, null);
  await rig.stop();
});

const viewPath = () => `/mirror/view?host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent('@1')}`;
const termPath = () => `/mirror/terminal?host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent('@1')}`;
const isView = (u: URL) => u.pathname === '/mirror/view/ws' && !u.search.includes('warm=1');

// --- the instruments ----------------------------------------------------------

// sockets is the readyState of every socket the document made whose URL holds
// part (a snapshot warm's excluded): [] when none.
const sockets = (page: Page, part: string): Promise<number[]> =>
  page.evaluate(
    (part) => (window as any).__socks.filter((s: any) => s.url.includes(part) && !s.url.includes('warm=1')).map((s: any) => s.o.readyState),
    part,
  );
// live is how many of them are open or opening.
const live = async (page: Page, part: string) => (await sockets(page, part)).filter((r) => r <= 1).length;
const made = async (page: Page, part: string) => (await sockets(page, part)).length;
// opens is every open the page measured for view.
const opens = (page: Page, view: string): Promise<{ view: string; attrs: any }[]> =>
  page.evaluate((view) => (window as any).__opens.filter((o: any) => o.view === view), view);
const hide = (page: Page, h: boolean) => page.evaluate((h) => (window as any).__setHidden(h), h);

// Relay passes a page's sockets through to the server; silence() makes every
// socket open NOW go quiet both ways — open on both ends, saying nothing, as a
// socket a sleeping phone left behind — and drop() closes them from the
// server's side. A socket the page opens later is relayed normally.
interface Relay {
  silence(): void;
  drop(): void;
  routed(): number;
}
async function relay(page: Page, match: (u: URL) => boolean): Promise<Relay> {
  const routes: { ws: WebSocketRoute; quiet: boolean }[] = [];
  await page.routeWebSocket(match, (ws) => {
    const r = { ws, quiet: false };
    routes.push(r);
    const server = ws.connectToServer();
    server.onMessage((m) => {
      if (!r.quiet) ws.send(m);
    });
    ws.onMessage((m) => {
      if (!r.quiet) server.send(m);
    });
  });
  return {
    silence: () => routes.forEach((r) => (r.quiet = true)),
    drop: () => routes.forEach((r) => r.ws.close({ code: 4000, reason: 'e2e drop' })),
    routed: () => routes.length,
  };
}

async function signedIn(ctx: BrowserContext): Promise<Page> {
  await ctx.addInitScript(instrument);
  const page = await ctx.newPage();
  await rig.login(page);
  await rig.waitListed(page, 'data-mirror-claude');
  return page;
}

// beats waits long enough for the page to have heard the server's heartbeat
// (every 500 ms here): only a socket that has heard one is judged stale.
const beats = (page: Page) => page.waitForTimeout(1500);

const sizes = [
  { name: 'phone', viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true },
  { name: 'desktop', viewport: { width: 1280, height: 800 }, isMobile: false, hasTouch: false },
];

for (const size of sizes) {
  test.describe(`at ${size.name} size`, () => {
    let ctx: BrowserContext;
    test.beforeEach(async ({ browser }) => {
      ctx = await browser.newContext({
        baseURL: rig.origin,
        viewport: size.viewport,
        isMobile: size.isMobile,
        hasTouch: size.hasTouch,
        storageState: { cookies: [], origins: [] },
      });
    });
    test.afterEach(async () => {
      await ctx.close();
    });

    test('the Rendered view, asleep with a dead socket, comes back by itself when shown: current, once, on one socket', async () => {
      const page = await signedIn(ctx);
      const r = await relay(page, isView);
      await page.goto(rig.origin + viewPath());
      await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
      await expect(page.locator('#mv-log')).toContainText('The first answer.');
      await beats(page);
      const before = await page.locator('#mv-log [data-bubble]').count();
      expect(await made(page, '/mirror/view/ws'), 'one socket before sleeping').toBe(1);

      // Asleep: the socket goes silent, the page is hidden, and the
      // conversation moves on meanwhile.
      r.silence();
      await hide(page, true);
      const marker = 'A reply written while the page slept ' + Date.now();
      fs.appendFileSync(trPath, cc.reply(marker));
      await page.waitForTimeout(2500); // well past the 1.2 s stale bound
      expect(await made(page, '/mirror/view/ws'), 'a hidden page opens nothing').toBe(1);
      await expect(page.locator('#mv-log')).not.toContainText(marker);

      // Woken: no tap.
      await hide(page, false);
      await expect(page.locator('#mv-log')).toContainText(marker, { timeout: 10_000 });
      await expect(page.locator('#mv-status')).toHaveText('Live');
      expect(await page.locator('#mv-log [data-bubble]').count(), 'the earlier bubbles once each, plus the reply').toBe(before + 1);
      expect(await page.locator('#mv-log').getByText('The first answer.').count()).toBe(1);
      await expect.poll(() => live(page, '/mirror/view/ws'), { message: 'exactly one live socket' }).toBe(1);
      expect(await made(page, '/mirror/view/ws'), 'one new socket, not more').toBe(2);
      const o = await opens(page, 'mirror_view');
      expect(o.map((x) => x.attrs.reconnect_reason).filter(Boolean), 'the reconnect says why').toEqual(['visibility']);
      expect(o[0].attrs.reconnect, 'the first open is not a reconnect').toBeUndefined();
    });

    test('a socket that dies silently while the page is shown is replaced by itself', async () => {
      const page = await signedIn(ctx);
      const r = await relay(page, isView);
      await page.goto(rig.origin + viewPath());
      await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
      await beats(page);
      r.silence();
      const marker = 'A reply written while the socket was dead ' + Date.now();
      fs.appendFileSync(trPath, cc.reply(marker));
      await expect(page.locator('#mv-log')).toContainText(marker, { timeout: 10_000 });
      await expect.poll(() => live(page, '/mirror/view/ws')).toBe(1);
      expect(await page.locator('#mv-log').getByText(marker).count(), 'drawn once').toBe(1);
      expect((await opens(page, 'mirror_view')).map((x) => x.attrs.reconnect_reason).filter(Boolean)).toEqual(['stale']);
    });

    test('offline, the view waits without hammering; online, it reconnects by itself', async () => {
      const page = await signedIn(ctx);
      const r = await relay(page, isView);
      await page.goto(rig.origin + viewPath());
      await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
      await ctx.setOffline(true);
      r.drop();
      await expect(page.locator('#mv-status')).not.toHaveText('Live', { timeout: 5_000 });
      await page.waitForTimeout(3000);
      expect(await made(page, '/mirror/view/ws'), 'no attempt while offline').toBe(1);
      const marker = 'A reply written while the network was gone ' + Date.now();
      fs.appendFileSync(trPath, cc.reply(marker));
      await ctx.setOffline(false);
      await expect(page.locator('#mv-log')).toContainText(marker, { timeout: 10_000 });
      await expect.poll(() => live(page, '/mirror/view/ws')).toBe(1);
      expect((await opens(page, 'mirror_view')).map((x) => x.attrs.reconnect_reason).filter(Boolean)).toEqual(['online']);
    });

    test("an agent's terminal takes keys from the start, with no switch, and reconnects by itself after sleep", async () => {
      const page = await signedIn(ctx);
      const r = await relay(page, (u) => u.pathname.endsWith('/terminal/ws'));
      await page.goto(rig.origin + '/agents/' + termAgent + '/terminal');
      const screen = page.locator('#term .xterm-rows');
      await expect(screen).toContainText('FAKE-TUI mode=rw', { timeout: 15_000 });
      await expect(page.locator('#term-mode'), 'no Live keys switch').toHaveCount(0);
      await expect(page.locator('#term-status')).toHaveText('Connected');
      await expect(page.locator('#term-keys [data-key="up"]')).toBeEnabled();
      await page.locator('#term').click();
      await page.keyboard.type('ok');
      await expect(screen).toContainText(/in=6F6B|in=6F/);
      await page.locator('#term-keys [data-key="up"]').click();
      await expect(screen).toContainText('in=1B5B41');
      await beats(page);

      r.silence();
      await hide(page, true);
      await page.waitForTimeout(2500);
      expect(await made(page, '/terminal/ws'), 'a hidden page opens nothing').toBe(1);
      await hide(page, false);
      // A fresh attach: the far side cleared the screen and drew it again.
      await expect(screen).not.toContainText('in=1B5B41', { timeout: 10_000 });
      await expect(screen).toContainText('FAKE-TUI mode=rw');
      await expect(page.locator('#term-status')).toHaveText('Connected');
      await expect.poll(() => live(page, '/terminal/ws')).toBe(1);
      expect(await made(page, '/terminal/ws')).toBe(2);
      await expect(page.locator('#term-reconnect'), 'no tap needed: the button stays hidden').toBeHidden();
      const o = await opens(page, 'agent_terminal');
      expect(o.map((x) => x.attrs.reconnect_reason).filter(Boolean)).toEqual(['visibility']);
      expect(o.every((x) => x.attrs.mode === 'rw'), 'every attach with live keys').toBe(true);
      // And the keys still go straight in.
      await page.locator('#term').click();
      await page.keyboard.type('z');
      await expect(screen).toContainText('in=7A');
    });

    test("a mirrored session's terminal opens with live keys and no switch", async () => {
      const page = await signedIn(ctx);
      await page.goto(rig.origin + termPath());
      const screen = page.locator('#term .xterm-rows');
      await expect(screen).toContainText('mode=rw', { timeout: 15_000 });
      await expect(page.locator('#term-mode')).toHaveCount(0);
      await expect(page.locator('#term-status')).toHaveText('Connected');
      await expect(page.locator('#term-keys [data-key="enter"]')).toBeEnabled();
      // What live keys reach is still said (InteractiveNote).
      await expect(page.locator('#term-status')).toHaveAttribute('title', /tmux client on/);
    });

    test("an agent's chat socket, asleep and dead, is replaced when the page is shown", async () => {
      const page = await signedIn(ctx);
      const r = await relay(page, (u) => u.pathname === '/agents/' + chatAgent + '/ws');
      await page.goto(rig.origin + '/agents/' + chatAgent);
      await expect(page.locator('#chat-log')).toContainText('Synthetic answer for the reconnect spec.', { timeout: 15_000 });
      await expect.poll(() => live(page, '/agents/' + chatAgent + '/ws')).toBe(1);
      await beats(page);
      r.silence();
      await hide(page, true);
      await page.waitForTimeout(2500);
      expect(await made(page, '/agents/' + chatAgent + '/ws')).toBe(1);
      await hide(page, false);
      await expect.poll(() => made(page, '/agents/' + chatAgent + '/ws'), { timeout: 10_000 }).toBe(2);
      await expect.poll(() => live(page, '/agents/' + chatAgent + '/ws')).toBe(1);
      expect((await opens(page, 'agent')).map((x) => x.attrs.reconnect_reason).filter(Boolean)).toEqual(['visibility']);
    });
  });
}

// --- back/forward restore ---------------------------------------------------------

test.describe('restored from the back/forward cache', () => {
  let bf: Browser;
  test.beforeAll(async ({ playwright }) => {
    bf = await launchWithBfcache(playwright.chromium);
  });
  test.afterAll(async () => {
    await bf?.close();
  });

  test('the Rendered view reconnects by itself, says so, and holds one socket', async () => {
    const ctx = await bf.newContext({ baseURL: rig.origin, viewport: { width: 390, height: 844 }, storageState: { cookies: [], origins: [] } });
    try {
      await ctx.addInitScript(`addEventListener('pageshow', (e) => { if (e.persisted) window.__restored = (window.__restored || 0) + 1; })`);
      const page = await signedIn(ctx);
      await page.goto(rig.origin + viewPath());
      await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
      await page.waitForTimeout(1500);
      // The service worker controls the page before it leaves: one that claims
      // it while it sits in the cache evicts it (bfcache-restore.spec).
      await page.evaluate(async () => {
        if (!navigator.serviceWorker) return;
        await navigator.serviceWorker.ready;
        if (navigator.serviceWorker.controller) return;
        await new Promise((r) => {
          navigator.serviceWorker.addEventListener('controllerchange', r, { once: true });
          setTimeout(r, 3000);
        });
      }).catch(() => {});
      await page.goto(rig.origin + '/tasks');
      const marker = 'A reply written while the view was in the cache ' + Date.now();
      fs.appendFileSync(trPath, cc.reply(marker));
      await page.goBack({ waitUntil: 'commit', timeout: 10_000 }).catch(() => {});
      await expect.poll(() => page.evaluate(() => (window as any).__restored || 0), { message: 'Back restored the view (pageshow persisted)' }).toBe(1);
      await expect(page.locator('#mv-log')).toContainText(marker, { timeout: 10_000 });
      await expect(page.locator('#mv-status')).toHaveText('Live');
      await page.waitForTimeout(1500);
      await expect.poll(() => live(page, '/mirror/view/ws')).toBe(1);
      expect((await opens(page, 'mirror_view')).map((x) => x.attrs.reconnect_reason).filter(Boolean)).toEqual(['pageshow']);
    } finally {
      await ctx.close();
    }
  });
});
