import { execFileSync } from 'node:child_process';
import { test, expect } from '../lib/fixtures';
import type { BrowserContext, CDPSession, Cookie, Page, Worker } from '@playwright/test';
import { dsn, password, pushPort, repoRoot, versionA } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { waitControlled } from '../lib/sw';

// muster-native Web Push, the browser half: the Enable flow subscribes with the
// SERVER's VAPID key and the device lands on /account; a subscription made
// with an old key is replaced (key rotation); an iPhone Safari tab is told to
// install to the Home Screen rather than "not supported".
//
// pushManager and Notification are STUBBED (in the style of badge.spec.ts): a
// real subscribe would reach a vendor push service, and headless Chromium has
// none. What is real is everything muster does — the key route, the subscribe
// route, the store, the account page — on a server with a real keypair.

const origin = `http://localhost:${pushPort}`;
let server: Running | undefined;

test.use({ viewport: { width: 412, height: 915 }, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  const out = execFileSync(serverBinary(versionA), ['vapid', 'generate'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
  const env: Record<string, string> = { MUSTER_VAPID_SUBJECT: 'mailto:e2e@example.com' };
  for (const line of out.trim().split('\n')) {
    const i = line.indexOf('=');
    env[line.slice(0, i)] = line.slice(i + 1);
  }
  server = await startServer(versionA, pushPort, env);
});
test.afterAll(async () => {
  await server?.stop();
});

interface StubState {
  perm: NotificationPermission;
  current: { endpoint: string } | null;
  subscribeKeys: number[][];
  unsubscribed: number;
}

// stubPush installs a pushManager and Notification permission the page script
// drives. `existingKey`, when set, starts the page with a subscription made
// with THAT key (the pre-rotation state).
async function stubPush(page: Page, opts: { existingKey?: number[]; perm?: NotificationPermission } = {}) {
  await page.addInitScript((o) => {
    const state = { perm: o.perm ?? 'default', current: null as unknown, subscribeKeys: [] as number[][], unsubscribed: 0 };
    (window as unknown as { __push: unknown }).__push = state;
    class FakeSub {
      endpoint: string;
      options: { applicationServerKey: ArrayBuffer };
      constructor(endpoint: string, key: Uint8Array) {
        this.endpoint = endpoint;
        this.options = { applicationServerKey: key.buffer.slice(0) as ArrayBuffer };
      }
      toJSON() {
        return { endpoint: this.endpoint, keys: { p256dh: 'BStubClientKeyForE2E', auth: 'stubauthsecret' } };
      }
      async unsubscribe() {
        state.unsubscribed++;
        state.current = null;
        return true;
      }
    }
    if (o.existingKey) {
      state.current = new FakeSub('https://fcm.googleapis.com/fcm/send/e2e-stale-' + Math.random().toString(36).slice(2), new Uint8Array(o.existingKey));
    }
    const pm = {
      async getSubscription() {
        return state.current;
      },
      async subscribe(sopts: { applicationServerKey: Uint8Array }) {
        const k = new Uint8Array(sopts.applicationServerKey);
        state.subscribeKeys.push(Array.from(k));
        state.current = new FakeSub('https://fcm.googleapis.com/fcm/send/e2e-' + Math.random().toString(36).slice(2), k);
        return state.current;
      },
    };
    Object.defineProperty(ServiceWorkerRegistration.prototype, 'pushManager', { configurable: true, get: () => pm });
    Object.defineProperty(Notification, 'permission', { configurable: true, get: () => state.perm });
    Notification.requestPermission = async () => (state.perm = 'granted');
  }, opts);
}

async function signIn(page: Page) {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

async function serverKey(page: Page): Promise<number[]> {
  const r = await page.request.get(origin + '/api/push/vapid-public-key');
  expect(r.status()).toBe(200);
  const key = ((await r.json()) as { key: string }).key;
  const b64 = key.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (key.length % 4)) % 4);
  return Array.from(Buffer.from(b64, 'base64'));
}

const pushState = (page: Page) => page.evaluate(() => (window as unknown as { __push: StubState }).__push);

// Inside the Android app's WebView (the app injects window.musterApp) the app
// owns notifications: the page draws no Enable and no "not supported" line,
// on a server that serves push to a browser that could subscribe. The control
// is the test below, where the same page without the bridge shows Enable.
test('inside the Android app shell the page offers no Enable', async ({ page }) => {
  await stubPush(page);
  await page.addInitScript(() => { (window as unknown as { musterApp: { postMessage(m: string): void } }).musterApp = { postMessage: () => {} }; });
  await signIn(page);
  await page.goto(origin + '/tasks');
  await expect(page.locator('#push-status')).toBeHidden();
  await expect(page.locator('#enable-push')).toBeHidden();
  await expect(page.locator('#push-on')).toBeHidden();
});

// Account → Devices notices a paired phone's push registration BY ITSELF. On
// the Android emulator the app registers its push endpoint a few seconds after
// pairing, while the page is open, and the page used to show "push not
// registered" with no test button until a manual reload. Here: pair from the
// app shell (the bridge stub), redeem the code as the app, reload as the app
// does once it has paired (webView.reload() on Pairing.Outcome.Paired), then
// register the phone's push: the SAME page — no further reload or navigation —
// shows it registered with "Send a test notification"; and once nothing is
// pending the list stops re-fetching.
//
// ⚠ THE REGISTRATION IS e2e/devicefix, NOT PUT /api/device/push: that route
// keeps an endpoint only after a VAPID test push to an https push service on
// port 443 succeeds, which this harness has none of. devicefix makes the same
// store write (push.PGStore.ClaimNative); the route itself is internal/api's
// TestTheDeviceListNoticesAPhonesPushRegistration.
test('Account → Devices shows a paired phone and its push registration without a reload', async ({ page, request }) => {
  await page.addInitScript(() => {
    const w = window as unknown as { __appMsgs: string[]; musterApp: { postMessage(m: string): void } };
    w.__appMsgs = [];
    w.musterApp = { postMessage: (m: string) => w.__appMsgs.push(m) };
  });
  await signIn(page);
  // The operator's phones are shared with the other specs on this database. A
  // phone another spec left behind, freshly paired and unregistered, would keep
  // the list polling, so every one is removed first, through the page's own
  // Remove route.
  const listed = await (await page.request.get(origin + '/account')).text();
  for (const [, id] of listed.matchAll(/data-testid="paired-device" data-device-id="(\d+)"/g)) {
    const removed = await page.request.post(origin + `/account/devices/${id}/remove`, { headers: { Origin: origin }, maxRedirects: 0 });
    expect(removed.status(), `removing leftover phone ${id}`).toBe(303);
  }
  await page.goto(origin + '/account');
  const section = page.getByTestId('paired-devices');
  await expect(section.getByTestId('paired-device')).toHaveCount(0);
  await section.getByRole('button', { name: 'Pair this phone' }).click();
  await expect(page.locator('#pair-status')).toHaveText('The app is finishing the pairing.');
  const msgs = await page.evaluate(() => (window as unknown as { __appMsgs: string[] }).__appMsgs);
  const code = JSON.parse(msgs[0]).code as string;

  // The app's side: redeem the code, then reload the WebView as the app does.
  const name = `e2e live phone ${Date.now()}`;
  const paired = await request.post(origin + '/api/device/pair', { data: { code, name, app_version: '0.0.2' } });
  expect(paired.status(), await paired.text()).toBe(201);
  const body = (await paired.json()) as { token: string; device: { id: number } };
  try {
    await page.reload();
    let navigations = 0;
    page.on('framenavigated', (f) => {
      if (f === page.mainFrame()) navigations++;
    });
    const refetches: string[] = [];
    page.on('request', (r) => {
      if (new URL(r.url()).pathname === '/account/devices/live') refetches.push(r.url());
    });
    const row = section.getByTestId('paired-device').filter({ hasText: name });
    await expect(row).toContainText('push not registered');
    // The Notifications section lists the phone by the label the claim gives
    // it. (Its test button may already be there for another device of the
    // operator's, so the phone's own entry is what is asserted.)
    const notifications = page.getByTestId('push-devices');
    const pushEntry = notifications.getByTestId('push-device').filter({ hasText: 'muster app: ' + name });
    await expect(pushEntry).toHaveCount(0);

    // The app registers its push endpoint. The page shows it with no further
    // reload.
    execFileSync('go', ['run', './e2e/devicefix', 'push', String(body.device.id), 'https://fcm.googleapis.com/fcm/send/e2e-live-' + Date.now()], {
      cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn }, stdio: ['ignore', 'inherit', 'inherit'],
    });
    await expect(row).toContainText('push registered');
    await expect(row).not.toContainText('push not registered');
    await expect(pushEntry).toBeVisible();
    await expect(notifications.getByRole('button', { name: 'Send a test notification' })).toBeVisible();
    expect(navigations, 'the page was not reloaded or navigated').toBe(0);
    expect(refetches.length, 'the list re-fetched itself').toBeGreaterThan(0);

    // Nothing is pending now: the list stops polling. Its interval is 3s, so
    // 4.5s without a re-fetch is a missed tick, not a slow one.
    const settled = refetches.length;
    await page.waitForTimeout(4500);
    expect(refetches.length, 'no re-fetch once nothing is pending').toBe(settled);
  } finally {
    // The app unpairs, pass or fail: the shared database keeps no phone of
    // this spec's.
    expect((await request.delete(origin + '/api/device', { headers: { Authorization: 'Bearer ' + body.token } })).status()).toBe(204);
  }
});

test('Enable subscribes with the server key and the device appears on the account page', async ({ page }) => {
  await stubPush(page);
  await signIn(page);
  const key = await serverKey(page);
  expect(key.length, 'an uncompressed P-256 public key').toBe(65);

  await page.goto(origin + '/tasks');
  const enable = page.locator('#enable-push');
  await expect(enable).toBeVisible();
  const saved = page.waitForResponse((r) => r.url().endsWith('/api/push/subscribe') && r.request().method() === 'POST');
  await enable.click();
  expect((await saved).status()).toBe(200);
  await expect(page.locator('#push-on')).toBeVisible();
  await expect(enable).toBeHidden();
  const st = await pushState(page);
  expect(st.subscribeKeys).toEqual([key]);

  await page.goto(origin + '/account');
  await expect(page.locator('[data-testid="push-device"]')).toHaveCount(1);
  await expect(page.locator('[data-testid="push-preferences"] input[type=checkbox]:checked')).toHaveCount(8); // every push.Events toggle, on by default (plugin: PR C; attention: the attention queue)
});

test('a subscription made with an old VAPID key is replaced with one on the current key', async ({ page }) => {
  const stale = Array.from({ length: 65 }, (_, i) => (i === 0 ? 4 : (i * 7) % 256));
  await stubPush(page, { existingKey: stale, perm: 'granted' });
  await signIn(page);
  const key = await serverKey(page);
  const resubscribed = page.waitForResponse((r) => r.url().endsWith('/api/push/subscribe') && r.request().method() === 'POST');
  await page.goto(origin + '/tasks');
  expect((await resubscribed).status()).toBe(200);
  await expect.poll(async () => (await pushState(page)).unsubscribed).toBe(1);
  const st = await pushState(page);
  expect(st.subscribeKeys, 'resubscribed exactly once, with the server key').toEqual([key]);
  await expect(page.locator('#push-on')).toBeVisible();
});

test.describe('on an iPhone Safari tab', () => {
  test.use({
    userAgent:
      'Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1',
  });
  test('the page says to install to the Home Screen, not that push is unsupported', async ({ page }) => {
    // A Safari tab has no PushManager: only an installed web app does.
    await page.addInitScript(() => {
      delete (window as unknown as { PushManager?: unknown }).PushManager;
    });
    await signIn(page);
    await page.goto(origin + '/tasks');
    const status = page.locator('#push-status');
    await expect(status).toContainText('Add to Home Screen');
    await expect(status).not.toContainText('not supported');
    await expect(page.locator('#enable-push')).toBeHidden();
  });
});

// The ATTENTION notification, the service worker's half: a new item is shown
// with its per-item tag and a tap target on /attention, and the "resolved"
// message carrying the same tag closes it. The payloads are EXACTLY what the
// server sends — internal/api's TestTheAttentionPayloadsAreWhatTheWorkerReads
// pins the same two JSON documents — delivered to the REAL worker's push
// handler through the browser's own push path (CDP
// ServiceWorker.deliverPushMessage), since headless Chromium has no vendor push
// service.
//
// ⚠ THE NOTIFICATION SINK IS STUBBED, in the worker (as pushManager is stubbed
// in the page above): headless Chromium refuses showNotification outright
// ("No notification permission has been granted", whatever permission the
// context is given — measured). So registration.showNotification and
// getNotifications are replaced by a recorder; what is real is the push event,
// the worker's handler and its branch on the payload.
test('an attention notification is shown with its tag and link, and its resolved message closes it', async ({ page, context }) => {
  await signIn(page);
  const registered = context.serviceWorkers().length ? Promise.resolve(context.serviceWorkers()[0]) : context.waitForEvent('serviceworker');
  await page.goto(origin + '/tasks');
  const worker = await registered;
  await page.evaluate(async () => {
    await navigator.serviceWorker.ready;
  });
  await worker.evaluate(() => {
    type Note = { title: string; body: string; tag: string; url: string; open: boolean; close: () => void };
    const sink: Note[] = [];
    (self as unknown as { __notes: Note[] }).__notes = sink;
    const reg = (self as unknown as { registration: ServiceWorkerRegistration }).registration;
    reg.showNotification = async (title: string, o?: NotificationOptions) => {
      const n: Note = { title, body: o?.body ?? '', tag: o?.tag ?? '', url: (o?.data || {}).url, open: true, close: () => (n.open = false) };
      sink.push(n);
    };
    reg.getNotifications = (async (f?: { tag?: string }) =>
      sink.filter((n) => n.open && (!f?.tag || n.tag === f.tag))) as unknown as typeof reg.getNotifications;
  });
  const cdp = await context.newCDPSession(page);
  const regs: { registrationId: string; scopeURL: string; isDeleted?: boolean }[] = [];
  cdp.on('ServiceWorker.workerRegistrationUpdated', (e) => regs.push(...e.registrations));
  await cdp.send('ServiceWorker.enable');
  await expect.poll(() => regs.find((r) => r.scopeURL === origin + '/' && !r.isDeleted)?.registrationId ?? '').not.toBe('');
  const registrationId = regs.find((r) => r.scopeURL === origin + '/' && !r.isDeleted)!.registrationId;
  const shown = () =>
    worker.evaluate(() =>
      (self as unknown as { __notes: { title: string; body: string; tag: string; url: string; open: boolean }[] }).__notes.map((n) => ({
        title: n.title, body: n.body, tag: n.tag, url: n.url, open: n.open,
      })),
    );
  const deliver = (data: string) => cdp.send('ServiceWorker.deliverPushMessage', { origin, registrationId, data });

  await deliver(
    '{"type":"attention","id":"ask-41","title":"worker-7 is asking","body":"Deploy the importer now? More detail below.","tag":"attention-ask-41","data":{"url":"/attention/open/ask-41"}}',
  );
  const note = { title: 'worker-7 is asking', body: 'Deploy the importer now? More detail below.', tag: 'attention-ask-41', url: '/attention/open/ask-41' };
  await expect.poll(shown).toEqual([{ ...note, open: true }]);
  await deliver('{"type":"resolved","id":"ask-41","tag":"attention-ask-41"}');
  await expect.poll(shown).toEqual([{ ...note, open: false }]);
});


// TAPPING an attention notification (operator ask: "go straight to the
// session"): the notification brings the OPEN muster tab forward and moves it
// to its tap route, GET /attention/open/<key>, which redirects to wherever the
// item opens now (internal/api attention_push_target.go). Each payload reaches
// the real worker exactly as the server sends it (internal/api's
// TestTheSessionPushPayloadIsWhatTheWorkerReads pins the session one), and the
// worker's real notificationclick handler and clients lookup do the rest.
//
// ⚠ WHAT THIS DOES NOT COVER: the session-page branch of the tap route.
// This server mirrors no host and raised no ask, so both keys resolve to
// openTarget's not-found fallback (the item on the queue, /attention#<key>),
// and pushTargetFor never runs. Its rule is guarded by the Go tests in
// internal/api (attention_push_target_test.go, _pg_test.go); attention.spec.ts
// and attention-delivery.spec.ts follow the route to a live session page.
//
// ⚠ STUBBED, AND WHY: headless Chromium shows no notification (see above), so
// the click is a synthetic notificationclick carrying the notification the
// worker recorded. The browser lets a worker focus a window or open one only
// inside a TRUSTED notificationclick, so WindowClient.focus is replaced by a
// counter and Clients.openWindow by a recorder, and waitUntil (which refuses an
// untrusted event) by a collector. The navigation that moves the tab is real.
async function tapHarness(page: Page, context: BrowserContext, worker: Worker) {
  await worker.evaluate(() => {
    type Note = { title: string; tag: string; data: Record<string, string> };
    const scope = self as unknown as {
      __notes: Note[];
      __focused: number;
      __windows: string[];
      __calls: string[];
      __click: (tag: string) => Promise<void>;
      registration: ServiceWorkerRegistration;
      dispatchEvent: (e: Event) => boolean;
    };
    scope.__notes = [];
    scope.__focused = 0;
    scope.__windows = [];
    scope.__calls = [];
    scope.registration.showNotification = async (title: string, o?: NotificationOptions) => {
      scope.__notes.push({ title, tag: o?.tag ?? '', data: (o?.data || {}) as Record<string, string> });
    };
    (WindowClient.prototype as unknown as { focus: () => Promise<WindowClient> }).focus = async function (this: WindowClient) {
      scope.__focused++;
      scope.__calls.push('focus');
      return this;
    };
    (Clients.prototype as unknown as { openWindow: (u: string) => Promise<null> }).openWindow = async (u: string) => {
      scope.__windows.push(new URL(u, self.location.origin).pathname);
      scope.__calls.push('openWindow');
      return null;
    };
    scope.__click = async (tag: string) => {
      const n = scope.__notes.find((x) => x.tag === tag);
      if (!n) throw new Error('no notification tagged ' + tag);
      const ev = new ExtendableEvent('notificationclick');
      const waits: Promise<unknown>[] = [];
      Object.defineProperty(ev, 'notification', { value: { tag: n.tag, data: n.data, close() {} } });
      Object.defineProperty(ev, 'action', { value: '' });
      Object.defineProperty(ev, 'waitUntil', { value: (p: Promise<unknown>) => waits.push(p) });
      scope.dispatchEvent(ev);
      await Promise.all(waits);
    };
  });
  const cdp = await context.newCDPSession(page);
  const regs: { registrationId: string; scopeURL: string; isDeleted?: boolean }[] = [];
  cdp.on('ServiceWorker.workerRegistrationUpdated', (e) => regs.push(...e.registrations));
  await cdp.send('ServiceWorker.enable');
  await expect.poll(() => regs.find((r) => r.scopeURL === origin + '/' && !r.isDeleted)?.registrationId ?? '').not.toBe('');
  const registrationId = regs.find((r) => r.scopeURL === origin + '/' && !r.isDeleted)!.registrationId;
  const deliver = (data: string) => cdp.send('ServiceWorker.deliverPushMessage', { origin, registrationId, data });
  // The tab follows the tap route; record what it asked for.
  const opened: string[] = [];
  page.on('request', (r) => {
    if (r.isNavigationRequest() && r.frame() === page.mainFrame()) opened.push(new URL(r.url()).pathname);
  });
  return {
    cdp,
    opened,
    // deliverAndTap pushes key's notification (payload, the server's exact
    // document, or a minimal one) and taps it once the worker has shown it.
    deliverAndTap: async (key: string, payload?: string) => {
      await deliver(
        payload ??
          JSON.stringify({ type: 'attention', id: key, title: 'muster is asking', body: 'A plugin message needs you.', tag: 'attention-' + key, data: { url: '/attention/open/' + key } }),
      );
      await expect
        .poll(() => worker.evaluate((t) => (self as unknown as { __notes: { tag: string }[] }).__notes.some((n) => n.tag === t), 'attention-' + key))
        .toBe(true);
      await worker.evaluate((t) => (self as unknown as { __click: (t: string) => Promise<void> }).__click(t), 'attention-' + key);
    },
    focused: () => worker.evaluate(() => (self as unknown as { __focused: number }).__focused),
    windows: () => worker.evaluate(() => (self as unknown as { __windows: string[] }).__windows),
    // calls: focus() and openWindow() in the order the worker made them.
    calls: () => worker.evaluate(() => (self as unknown as { __calls: string[] }).__calls),
    // controlledURLs / allURLs: the window clients the worker controls, and all
    // of them including the uncontrolled ones.
    controlledURLs: () => worker.evaluate(async () => (await (self as unknown as { clients: Clients }).clients.matchAll({ type: 'window' })).map((c) => new URL(c.url).pathname)),
    allURLs: () =>
      worker.evaluate(async () =>
        (await (self as unknown as { clients: Clients }).clients.matchAll({ type: 'window', includeUncontrolled: true })).map((c) => new URL(c.url).pathname),
      ),
  };
}

// signedInWorker signs in, opens /tasks and returns the origin's worker once it
// controls the tab.
//
// 🔴 ONE SIGN-IN FOR ALL THE TAP TESTS. Password-mode sign-in is throttled
// server-wide (internal/api login.go: loginRateBurst per loginRateWindow, 10 a
// minute), and this file runs in seconds. When each tap test signed in on its
// own, the file made 11 sign-ins and the 11th got a 429 ("Too many attempts").
// So the first test signs in and the rest reuse its session cookie.
let tapSession: Cookie[] | undefined;
async function signedInWorker(page: Page, context: BrowserContext): Promise<Worker> {
  if (tapSession) {
    await context.addCookies(tapSession);
  } else {
    await signIn(page);
    tapSession = await context.cookies();
  }
  const registered = context.serviceWorkers().length ? Promise.resolve(context.serviceWorkers()[0]) : context.waitForEvent('serviceworker');
  await page.goto(origin + '/tasks');
  const worker = await registered;
  await waitControlled(page);
  return worker;
}

// hardReload reloads the tab the way shift-reload does. 🔴 A HARD RELOAD
// BYPASSES THE SERVICE WORKER, so the new document is UNCONTROLLED, and it
// stays so: hx-boost moves the tab without another full navigation, and
// nothing claims it later. This is the state a tap must still work in.
async function hardReload(page: Page, cdp: CDPSession): Promise<void> {
  const loaded = page.waitForEvent('load');
  await cdp.send('Page.reload', { ignoreCache: true });
  await loaded;
}

test('tapping an attention notification follows its tap route in the open tab', async ({ page, context }) => {
  const worker = await signedInWorker(page, context);
  const tap = await tapHarness(page, context, worker);

  // A waiting mirrored window's notification. The window is gone by the tap
  // (this server mirrors no host), so its route lands on its queue item.
  await tap.deliverAndTap(
    'session-0011223344556677',
    '{"type":"attention","id":"session-0011223344556677","title":"build @ ws is waiting","body":"Waiting on you: a permission prompt or a question in the session.",' +
      '"tag":"attention-session-0011223344556677","data":{"url":"/attention/open/session-0011223344556677"}}',
  );
  await page.waitForURL(origin + '/attention#session-0011223344556677');
  expect(tap.opened, 'the tab went through the tap route').toContain('/attention/open/session-0011223344556677');
  expect(await tap.focused(), 'the open tab is the one brought forward').toBe(1);
  expect(context.pages(), 'no second tab is opened').toHaveLength(1);

  // A question muster raised itself (no session): its item on the queue.
  await tap.deliverAndTap(
    'ask-71',
    '{"type":"attention","id":"ask-71","title":"muster is asking","body":"A plugin message needs you.\\nOptions: No action needed","tag":"attention-ask-71","data":{"url":"/attention/open/ask-71"}}',
  );
  await page.waitForURL(origin + '/attention#ask-71');
  expect(tap.opened).toContain('/attention/open/ask-71');
  expect(await tap.focused()).toBe(2);
  expect(context.pages()).toHaveLength(1);
  expect(await tap.windows(), 'no window is opened').toEqual([]);
});

// 🔴 AN UNCONTROLLED TAB STILL FOLLOWS THE TAP. client.navigate() rejects for
// a client the worker does not control ("This service worker is not the
// client's active service worker"), and focusOrOpen used to swallow that: the
// tab came forward and stayed where it was, on every tap, until a full
// reload. Now the worker asks the page itself to go (sw.js askPageToOpen, the
// NAVIGATE message pwa.js answers).
test('tapping an attention notification moves an UNCONTROLLED open tab to its tap route', async ({ page, context }) => {
  const worker = await signedInWorker(page, context);
  const tap = await tapHarness(page, context, worker);
  await hardReload(page, tap.cdp);
  expect(await page.evaluate(() => navigator.serviceWorker.controller), 'CONTROL: the hard reload left the tab uncontrolled').toBeNull();
  expect(await tap.controlledURLs(), 'CONTROL: the worker controls no tab').toEqual([]);
  expect(await tap.allURLs(), 'CONTROL: the worker still sees the tab').toEqual(['/tasks']);

  await tap.deliverAndTap('ask-71');
  await expect(page, 'the same tab moved to the tap target').toHaveURL(origin + '/attention#ask-71');
  expect(tap.opened, 'the tab went through the tap route').toContain('/attention/open/ask-71');
  expect(await tap.focused(), 'the tab was brought forward').toBe(1);
  expect(await tap.windows(), 'no window is opened').toEqual([]);
  expect(context.pages(), 'no second tab is opened').toHaveLength(1);
});

// When neither navigate() nor the page will take the route (here the page's
// NAVIGATE message is dropped before pwa.js sees it), the tap opens the route
// in a new window rather than doing nothing.
test('a tap the open tab will not take opens its tap route in a new window', async ({ page, context }) => {
  const worker = await signedInWorker(page, context);
  const tap = await tapHarness(page, context, worker);
  await page.addInitScript(() => {
    (window as unknown as { __dropped: number }).__dropped = 0;
    navigator.serviceWorker?.addEventListener('message', (e) => {
      if ((e.data || {}).type !== 'NAVIGATE') return;
      (window as unknown as { __dropped: number }).__dropped++;
      e.stopImmediatePropagation();
    });
  });
  await hardReload(page, tap.cdp);
  expect(await page.evaluate(() => navigator.serviceWorker.controller), 'CONTROL: the hard reload left the tab uncontrolled').toBeNull();

  await tap.deliverAndTap('ask-71');
  expect(await page.evaluate(() => (window as unknown as { __dropped: number }).__dropped), 'CONTROL: the worker did ask the page').toBe(1);
  expect(await tap.windows(), 'the route opened in a new window').toEqual(['/attention/open/ask-71']);
  // 🔴 A trusted notificationclick grants ONE window-interaction token, and
  // Chromium's WindowClient.focus() consumes it, so an openWindow() after a
  // focus() is refused ("Not allowed to open a window"). The stubs cannot see
  // the token, so the order is what is asserted: no focus before the window.
  expect(await tap.calls(), 'openWindow is not preceded by a focus that would spend the click').toEqual(['openWindow']);
  expect(new URL(page.url()).pathname, 'the tab did not move').toBe('/tasks');
});

// With two tabs open, one controlled and one not, the tap moves the
// CONTROLLED one: it is the tab navigate() can move. Both orders, so neither
// "first tab" nor "last tab" passes by luck of the clients list's order.
// ⚠ "second tab uncontrolled" is the REGRESSION case: the old code took the
// first client Chromium lists (most recently focused first), the uncontrolled
// second tab, and failed it. "first tab uncontrolled" PASSES ON THE OLD CODE
// too; it is an INVARIANT GUARD, there to catch a "last client" choice.
for (const uncontrolled of ['first', 'second'] as const) {
  test(`with a controlled and an uncontrolled tab open, the tap moves the controlled one (${uncontrolled} tab uncontrolled)`, async ({ page, context }) => {
    const worker = await signedInWorker(page, context);
    const tap = await tapHarness(page, context, worker);
    const other = await context.newPage();
    await other.goto(origin + '/agents');
    await waitControlled(other);
    const [stale, live] = uncontrolled === 'first' ? [page, other] : [other, page];
    await hardReload(stale, await context.newCDPSession(stale));
    expect(await stale.evaluate(() => navigator.serviceWorker.controller), 'CONTROL: one tab is uncontrolled').toBeNull();
    const livePath = new URL(live.url()).pathname;
    const stalePath = new URL(stale.url()).pathname;
    expect(await tap.controlledURLs(), 'CONTROL: the worker controls the other tab only').toEqual([livePath]);
    expect((await tap.allURLs()).sort(), 'CONTROL: the worker sees both tabs').toEqual(['/agents', '/tasks']);

    await tap.deliverAndTap('ask-71');
    await expect(live, 'the controlled tab moved').toHaveURL(origin + '/attention#ask-71');
    expect(new URL(stale.url()).pathname, 'the uncontrolled tab stayed').toBe(stalePath);
    expect(await tap.windows(), 'no window is opened').toEqual([]);
  });
}

// The page follows a NAVIGATE only to its OWN origin (pwa.js). The messages
// are posted from the real worker exactly as focusOrOpen posts them, each with
// a reply port. A cross-origin and a javascript: url are refused (no reply, no
// navigation, no script). Then a same-origin, same-document url is followed
// (the positive control). Messages to one client arrive in order, so once
// that control's reply is in, the two before it have been handled, and their
// missing replies are an answer, not a wait that ran short.
test('the page follows a NAVIGATE only to its own origin', async ({ page, context }) => {
  const worker = await signedInWorker(page, context);
  const elsewhere: string[] = [];
  await page.route('https://elsewhere.example/**', (r) => {
    elsewhere.push(r.request().url());
    return r.abort();
  });
  await worker.evaluate(async (base) => {
    const scope = self as unknown as { __acks: string[]; clients: Clients };
    scope.__acks = [];
    const [tab] = await scope.clients.matchAll({ type: 'window', includeUncontrolled: true });
    const send = (name: string, url: string) => {
      const ch = new MessageChannel();
      ch.port1.onmessage = () => scope.__acks.push(name);
      tab.postMessage({ type: 'NAVIGATE', url }, [ch.port2]);
    };
    send('javascript', 'javascript:void(window.__pwned = 1)');
    send('cross-origin', 'https://elsewhere.example/attention');
    send('same-origin', base + '/tasks#followed');
  }, origin);
  await expect.poll(() => worker.evaluate(() => (self as unknown as { __acks: string[] }).__acks), { message: 'CONTROL: the same-origin url is followed' }).toContain('same-origin');
  await expect(page).toHaveURL(origin + '/tasks#followed');
  expect(await worker.evaluate(() => (self as unknown as { __acks: string[] }).__acks), 'only the same-origin url is answered').toEqual(['same-origin']);
  expect(await page.evaluate(() => (window as unknown as { __pwned?: number }).__pwned), 'the javascript: url ran nothing').toBeUndefined();
  expect(elsewhere, 'no request left for the other origin').toEqual([]);
});
