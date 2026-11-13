import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { hookToken, updatePort, versionA, versionB, versionC } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { waitControlled } from '../lib/sw';
import { openMore } from '../lib/nav';

// A deploy, end to end: build A serves the page, build B replaces it on the
// same origin, and the open page offers the update without taking it.
//
// 🔴 TWO REAL BUILDS, NOT A ROUTED SCRIPT. Playwright cannot route() a service
// worker's update fetch, and the property under test is that DIFFERENT BUILDS
// serve different worker bytes — so the spec runs both binaries global-setup
// built, on a port of its own (a fresh origin, so no other spec's worker is
// involved).

let server: Running | undefined;
test.afterEach(async () => {
  await server?.stop();
  server = undefined;
});

// Asks the CONTROLLING worker which build it is. It times out to null: a
// message posted to a controller that is being replaced can go unanswered (it
// turns redundant mid-question), and a poll that hangs on one call never asks
// again.
const controllerBuild = `(async () => {
  const c = navigator.serviceWorker.controller;
  if (!c) return null;
  return await new Promise((resolve) => {
    const ch = new MessageChannel();
    const t = setTimeout(() => resolve(null), 1000);
    ch.port1.onmessage = (e) => { clearTimeout(t); resolve(e.data.build); };
    c.postMessage({ type: 'GET_VERSION' }, [ch.port2]);
  });
})()`;

// Every cache on the origin, and what its "/" entry is. `shell` reads the BODY:
// the authenticated document renders the update toast, the login page does
// not, so a login page stored under "/" reads as shell:false even at 200.
const shellCaches = `(async () => {
  const out = {};
  for (const k of await caches.keys()) {
    const r = await (await caches.open(k)).match('/');
    out[k] = r ? { status: r.status, type: r.type, redirected: r.redirected, shell: (await r.text()).includes('id="sw-update-toast"') } : null;
  }
  return out;
})()`;
const goodShell = { status: 200, type: 'basic', redirected: false, shell: true };

// A deploy: the same origin starts answering with build B.
async function deployB(): Promise<void> {
  await server!.stop();
  server = await startServer(versionB, updatePort);
}

// The waiting worker's state, or null when nothing waits.
const waitingState = `(async () => {
  const reg = await navigator.serviceWorker.getRegistration();
  return reg && reg.waiting ? reg.waiting.state : null;
})()`;

const checkForUpdate = async () => {
  const reg = await navigator.serviceWorker.getRegistration();
  await reg!.update();
};

// Signs the browser out UNDER the open page, without the page reloading itself.
//
// 🔴 THE PAGE IS STILL A SIGNED-IN DOCUMENT THAT POLLS. Its htmx fragments ask
// on timers, on SSE (re)open and on muster:resync, and the app's auth gate
// (components.go, htmx:beforeSwap) turns any 401 into location.reload(). With
// the cookies cleared, one such request landing before the test's own steps
// reloads the page: onto /login while a server answers (no toast), or — when
// its 401 lands while deployB has the port closed — onto
// chrome-error://chromewebdata/, where navigator.serviceWorker is undefined
// and checkForUpdate throws "Cannot read properties of undefined (reading
// 'getRegistration')". That was this spec's flake (CI, 655 ms).
//
// So the page's own XHR/fetch traffic is aborted from here on (an abort is a
// sendError, which only toasts), and a resync is provoked at once so the
// hazard is exercised on every run rather than when a timer happens to land
// in the window. Navigations, the worker's own fetches (page.route never sees
// them) and the update check are untouched.
//
// ⚠ THIS GUARD DELIBERATELY MASKS A REAL PRODUCT PATH. A signed-out tab's 401
// goes to the auth gate's location.reload() (internal/ui/components.go,
// htmx:beforeSwap), and during a deploy's restart window that reload lands on
// chrome-error:// — a dead tab. These tests are about the worker's update, not
// that path, so they keep it out; NO spec covers it. It is tracked as a
// separate product defect.
async function signOutUnderPage(page: Page): Promise<void> {
  let stopped = 0;
  await page.route('**/*', (route) => {
    const type = route.request().resourceType();
    if (type === 'xhr' || type === 'fetch') {
      stopped++;
      return route.abort();
    }
    return route.continue();
  });
  const url = page.url();
  await page.context().clearCookies();
  await page.evaluate(() => document.body.dispatchEvent(new CustomEvent('muster:resync')));
  await expect.poll(() => stopped, { message: 'CONTROL: the page really asked for its fragments after the sign-out' }).toBeGreaterThan(0);
  await deployB();
  expect(page.url(), 'the sign-out did not reload the page').toBe(url);
}

test('a new build is offered, waits for Reload, and Reload takes it exactly once', async ({ page }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.addInitScript(() => {
    sessionStorage.setItem('loads', String(Number(sessionStorage.getItem('loads') || '0') + 1));
  });
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  expect(await page.evaluate(controllerBuild)).toBe(versionA);
  // A first install offers nothing.
  await expect(page.locator('#sw-update-toast')).toBeHidden();

  // Deploy: same origin, new build.
  await server.stop();
  server = await startServer(versionB, updatePort);
  await page.evaluate(async () => {
    const reg = await navigator.serviceWorker.getRegistration();
    await reg!.update();
  });

  await expect(page.locator('#sw-update-toast')).toBeVisible();
  // 🔴 The new worker WAITS: the page is still controlled by build A, and the
  // registration still holds B as an installed, waiting worker. (A worker that
  // called skipWaiting() at install would be activating, not waiting.)
  expect(await page.evaluate(waitingState)).toBe('installed');
  expect(await page.evaluate(controllerBuild)).toBe(versionA);

  const loadsBefore = Number(await page.evaluate(() => sessionStorage.getItem('loads')));
  // 🔴 THE LOAD EVENT IS ARMED BEFORE THE CLICK. Polling the controller and
  // then waitForLoadState('load') raced: the controller turns B in the OLD
  // document (controllerchange fires before location.reload() navigates), the
  // old document is already "loaded", and the next evaluate then ran into the
  // navigation — "Execution context was destroyed". Waiting for the NEW
  // document's load event cannot see the old one.
  const reloaded = page.waitForEvent('load', { timeout: 15_000 });
  await page.locator('[data-sw-reload]').click();
  await reloaded;
  expect(await page.evaluate(controllerBuild)).toBe(versionB);
  // No sleep to "let a second reload happen": once the page is controlled by B
  // with NOTHING waiting, no reload source is left (the fallback timer died
  // with the old document, and no further controllerchange can come).
  expect(await page.evaluate(waitingState)).toBeNull();
  expect(Number(await page.evaluate(() => sessionStorage.getItem('loads'))), 'exactly one reload').toBe(loadsBefore + 1);
  await expect(page.locator('#sw-update-toast')).toBeHidden();
  // Per-build cache naming: the new build's shell cache replaced the old one.
  await expect.poll(() => page.evaluate(shellCaches)).toEqual({ ['muster-shell-' + versionB]: goodShell });
});

// F1: an htmx in-app navigation swaps the body — the toast included, which
// comes back from the server hidden. The update is still waiting, so the toast
// must still be offered after the swap.
test('the update toast survives an in-app navigation while the update waits', async ({ page }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  const r = await page.request.get(origin + '/api/tasks', { headers: { Authorization: `Bearer ${hookToken}` } });
  expect(r.status()).toBe(200);
  const tasks = (await r.json()) as { id: number }[];
  expect(tasks.length).toBeGreaterThan(0);
  await page.addInitScript(() => {
    sessionStorage.setItem('loads', String(Number(sessionStorage.getItem('loads') || '0') + 1));
  });
  await page.goto(`${origin}/tasks/${tasks[0].id}`);
  await waitControlled(page);
  await deployB();
  await page.evaluate(checkForUpdate);
  const toast = page.locator('#sw-update-toast');
  await expect(toast).toBeVisible();

  const loadsBefore = Number(await page.evaluate(() => sessionStorage.getItem('loads')));
  await page.click('#sidebar-open');
  await openMore(page);
  // The task page's Tasks link (the retired Agents tab was this hop; this
  // server has no connector store, so Tasks is the shell's one tab): off the
  // shell, an in-app boosted swap.
  await page.locator('#sidebar a[data-tab="tasks"]').click();
  await expect(page).toHaveURL(origin + '/tasks');
  await expect(page.locator('#sidebar a[data-tab="tasks"][aria-current="page"]')).toHaveCount(1);
  // 🔴 The navigation was a swap, not a document load: a full load would
  // re-offer the update from the registration-time check and prove nothing.
  expect(Number(await page.evaluate(() => sessionStorage.getItem('loads'))), 'no full reload').toBe(loadsBefore);
  await expect(toast).toBeVisible();
});

// F2: a deploy installed while the page has NO session. The new worker's
// precache of "/" is refused (401 to a worker fetch), and that must not cost
// the offline shell the previous build cached.
test('an update installed while signed out keeps the offline shell', async ({ page }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  // Per-build naming, and a real shell, from a signed-in install.
  await expect.poll(() => page.evaluate(shellCaches)).toEqual({ ['muster-shell-' + versionA]: goodShell });

  await signOutUnderPage(page);
  // Positive control on the premise: signed out, "/" really is refused.
  expect((await page.request.get(origin + '/', { maxRedirects: 0 })).status()).not.toBe(200);
  await page.evaluate(checkForUpdate);
  await expect(page.locator('#sw-update-toast')).toBeVisible();
  await page.locator('[data-sw-reload]').click();
  await expect
    .poll(() => page.evaluate(controllerBuild).catch(() => null), { timeout: 15_000 })
    .toBe(versionB);
  // The new build's cache holds a usable shell, carried forward from A.
  await expect.poll(() => page.evaluate(shellCaches).catch(() => null)).toEqual({ ['muster-shell-' + versionB]: goodShell });
});

// 🔴 A REGRESSION TEST (#126 audit F1): the carried shell keeps its stylesheet.
// A deploy installed while signed out carries the older build's "/" forward —
// and that document links the OLDER build's /static/app.css?v=<old hash>, a key
// only the older cache held. Activate deleted that cache, the offline fallback
// matches the exact URL, and the offline shell rendered unstyled.
//
// Both e2e builds compile the same app.css, so the older build's hash is made
// here: build A's worker is handed a "/" that links ?v=0ld0ld0ld0ld (its own
// precache fetch, routed), and the page then asks for that stylesheet through
// the worker, which caches it under that URL — the cache an older release
// leaves behind.
test('an update installed while signed out keeps the offline shell styled', async ({ page, context }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  const oldCSS = '/static/app.css?v=0ld0ld0ld0ld';
  let routed = 0;
  // The worker fetches its shell as "/?shell=1" (sw.js SHELL_FETCH): match "/" by path.
  const root = (u: URL) => u.origin === origin && u.pathname === '/';
  await context.route(root, async (route) => {
    if (!route.request().serviceWorker()) return route.continue();
    const res = await route.fetch();
    const html = await res.text();
    routed++;
    return route.fulfill({ response: res, body: html.replace(/\/static\/app\.css\?v=[0-9a-f]+/, oldCSS) });
  });
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  await expect.poll(() => page.evaluate(shellCaches)).toEqual({ ['muster-shell-' + versionA]: goodShell });
  expect(routed, "control: the worker's precache of / was the routed one").toBeGreaterThan(0);
  await context.unroute(root);
  // A's shell links the old stylesheet, and A's cache holds it under that URL.
  const links = await page.evaluate(async ([build, old]) => {
    await fetch(old);
    const doc = await (await caches.open('muster-shell-' + build)).match('/');
    return (await doc!.text()).includes('href="' + old + '"');
  }, [versionA, oldCSS]);
  expect(links, "A's shell links the old stylesheet").toBe(true);
  // ⚠ POLLED, NOT READ ONCE: the worker answers the fetch first and writes
  // the cache after (sw.js's app.css handler does not wait for its put), so a
  // read right after the fetch can land before the write. That was this
  // spec's flake: {cached: false} in 547 ms on CI, green on re-run.
  await expect
    .poll(
      () => page.evaluate(async ([build, old]) => !!(await (await caches.open('muster-shell-' + build)).match(old)), [versionA, oldCSS]),
      { message: "A's cache holds the old stylesheet under its URL" },
    )
    .toBe(true);

  await signOutUnderPage(page);
  await page.evaluate(checkForUpdate);
  await expect(page.locator('#sw-update-toast')).toBeVisible();
  await page.locator('[data-sw-reload]').click();
  await expect
    .poll(() => page.evaluate(controllerBuild).catch(() => null), { timeout: 15_000 })
    .toBe(versionB);
  await expect.poll(() => page.evaluate(shellCaches).catch(() => null)).toEqual({ ['muster-shell-' + versionB]: goodShell });

  // Offline: the carried "/" (linking the OLD stylesheet) is the page, and its
  // stylesheet loads — from the carried entry, not the network. The browser's
  // HTTP cache is cleared first: it also holds that stylesheet (the fetch
  // above).
  // And the SERVER is stopped: the worker's own fetch is not reliably cut by
  // the page's offline emulation (measured: with it alone, the no-carry mutant
  // still read styled), so the network answer is taken away for real.
  const cdp = await context.newCDPSession(page);
  await cdp.send('Network.clearBrowserCache');
  await server!.stop();
  server = undefined;
  await context.setOffline(true);
  await page.goto(origin + '/agents');
  const sheet = await page.evaluate(() => {
    const l = document.querySelector('link[rel="stylesheet"][href*="/static/app.css"]') as HTMLLinkElement | null;
    let rules = -1;
    try { rules = l?.sheet ? l.sheet.cssRules.length : 0; } catch { rules = -2; }
    return { href: l?.getAttribute('href') ?? null, rules };
  });
  expect(sheet.href, 'the carried shell links the older stylesheet').toBe(oldCSS);
  expect(sheet.rules, 'the offline shell is styled').toBeGreaterThan(0);
  await context.setOffline(false);
});

// 🔴 A BOARD-DRAWN "/" NEVER BECOMES THE NEW SHELL (sw.js boardless(), #273).
// The real "/" draws the signed-in user's board. During a rolling deploy an
// OLDER worker (one that fetched "/" itself) can store that copy from a new
// pod; the new worker's activate would then carry it forward into a shell that
// outlives sign-out. And a new worker whose own shell fetch were ever answered
// with the board must not store it either. Both cases need the worker's own
// check: the server half (?shell=1) cannot see them.
const seededTitle = 'Rotate the staging webhook secret'; // e2e/seed
const shellOf = (build: string) => `(async () => {
  const r = await (await caches.open('muster-shell-${build}')).match('/');
  if (!r) return null;
  const h = await r.text();
  return { drawn: h.includes('data-tasks-drawn'), seeded: h.includes(${JSON.stringify(seededTitle)}), shell: h.includes('id="sw-update-toast"') };
})()`;

test('a board-drawn "/" an older worker stored is not carried into the new shell', async ({ page, context }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  await expect.poll(() => page.evaluate(shellCaches)).toEqual({ ['muster-shell-' + versionA]: goodShell });
  // The older worker's cache now holds the BOARD copy of "/": the page's own
  // same-origin fetch (type basic, 200, the session cookie), as a pre-#273
  // worker's install fetch would have stored it. (The board document is
  // /tasks now — `/` itself opens the chief's chat — stored under "/".)
  const poisoned = await page.evaluate(async ([build, title]) => {
    const res = await fetch('/tasks', { credentials: 'same-origin' });
    const html = await res.clone().text();
    await (await caches.open('muster-shell-' + build)).put('/', res);
    return { type: res.type, status: res.status, drawn: html.includes('data-tasks-drawn'), seeded: html.includes(title) };
  }, [versionA, seededTitle]);
  expect(poisoned, "control: the older cache holds the board copy").toEqual({ type: 'basic', status: 200, drawn: true, seeded: true });
  // The new worker's own shell fetch fails, so "/" can only come from the
  // older cache: activate's carry-forward is the path under test.
  let refused = 0;
  await context.route((u) => u.origin === origin && u.pathname === '/', (route) => {
    if (!route.request().serviceWorker()) return route.continue();
    refused++;
    return route.fulfill({ status: 503, body: 'unavailable' });
  });
  await deployB();
  await page.evaluate(checkForUpdate);
  await expect(page.locator('#sw-update-toast')).toBeVisible();
  expect(refused, "control: the new worker's own shell fetch was the refused one").toBeGreaterThan(0);
  await page.locator('[data-sw-reload]').click();
  await expect
    .poll(() => page.evaluate(controllerBuild).catch(() => null), { timeout: 15_000 })
    .toBe(versionB);
  // Activate has run: the older cache is gone, and with it the only source.
  await expect.poll(() => page.evaluate(() => caches.keys()).catch(() => null)).toEqual(['muster-shell-' + versionB]);
  expect(await page.evaluate(shellOf(versionB)), 'the new shell carried the board copy forward').toBeNull();
});

test('a board-drawn answer to the new worker\'s shell fetch is not installed', async ({ page, context }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  await expect.poll(() => page.evaluate(shellCaches)).toEqual({ ['muster-shell-' + versionA]: goodShell });
  // The new worker asks for its shell and is answered with the BOARD copy of
  // "/" (the real board document, /tasks, fetched with the request's own cookie).
  let boarded = 0;
  await context.route((u) => u.origin === origin && u.pathname === '/', async (route) => {
    if (!route.request().serviceWorker()) return route.continue();
    const res = await route.fetch({ url: origin + '/tasks' });
    const html = await res.text();
    if (html.includes('data-tasks-drawn')) boarded++;
    return route.fulfill({ response: res, body: html });
  });
  await deployB();
  await page.evaluate(checkForUpdate);
  await expect(page.locator('#sw-update-toast')).toBeVisible();
  expect(boarded, "control: the new worker's shell fetch was answered with the board").toBeGreaterThan(0);
  await page.locator('[data-sw-reload]').click();
  await expect
    .poll(() => page.evaluate(controllerBuild).catch(() => null), { timeout: 15_000 })
    .toBe(versionB);
  await expect.poll(() => page.evaluate(() => caches.keys()).catch(() => null)).toEqual(['muster-shell-' + versionB]);
  // The board answer was refused; "/" is the older build's board-less shell,
  // carried forward.
  expect(await page.evaluate(shellOf(versionB)), 'the new shell is the board copy').toEqual({ drawn: false, seeded: false, shell: true });
});

// F2, the edge's shape: behind a forward-auth edge an expired session's "/" is
// a REDIRECT to a login page that answers 200. A followed redirect must not be
// cached as the shell. The route answers only the WORKER's fetch of "/", so the
// page itself stays signed in.
//
// ⚠ THE STAND-IN LOGIN PAGE IS /manifest.webmanifest: a real same-origin 200
// that is not the app document. Measured: the request a worker makes when it
// follows a route-fulfilled redirect is NOT routed again: a routed stand-in
// page counted 0 hits and "/" was simply not cached, even with the redirect
// handling mutated away — a test blind to the thing it names. Redirecting to a
// path the real server answers 200 is what made that mutant go red.
// muster's own /login would not do either: signed in, it redirects to the app.
test('a redirected "/" is never cached as the shell', async ({ page, context }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  await expect.poll(() => page.evaluate(shellCaches)).toEqual({ ['muster-shell-' + versionA]: goodShell });

  let redirected = 0;
  // The worker fetches its shell as "/?shell=1" (sw.js SHELL_FETCH): match "/" by path.
  await context.route((u) => u.origin === origin && u.pathname === '/', (route) => {
    if (!route.request().serviceWorker()) return route.continue();
    redirected++;
    return route.fulfill({ status: 302, headers: { location: origin + '/manifest.webmanifest' } });
  });
  await deployB();
  await page.evaluate(checkForUpdate);
  await expect(page.locator('#sw-update-toast')).toBeVisible();
  // Positive control: the route really answered the new worker's precache.
  expect(redirected).toBeGreaterThan(0);
  await page.locator('[data-sw-reload]').click();
  await expect
    .poll(() => page.evaluate(controllerBuild).catch(() => null), { timeout: 15_000 })
    .toBe(versionB);
  await expect.poll(() => page.evaluate(shellCaches).catch(() => null)).toEqual({ ['muster-shell-' + versionB]: goodShell });
});

// N1: an installed app resumes rather than navigates, so the resume is the
// update check. The first resume after a page load runs one — loading the page
// does not start the throttle window.
test('the first resume after a load checks for an update', async ({ page }) => {
  await page.clock.install();
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  await deployB();
  await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await expect(page.locator('#sw-update-toast')).toBeVisible();
});

// N1, the throttle: after a check that ran, resumes are ignored for a minute,
// and an ignored resume does not push the next check back.
//
// ⚠ COUNTED, NOT SLEPT ON. This used to prove "throttled" by waiting 1–3 s and
// seeing no toast — a sleep standing in for an absence. The resume handler
// calls reg.update() synchronously, so wrapping update() and reading the count
// right after the dispatch answers "did this resume check?" exactly.
test('a resume checks for an update at most once a minute', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(() => {
    const w = window as unknown as { __updates: number; __lastUpdate: Promise<unknown> };
    w.__updates = 0;
    w.__lastUpdate = Promise.resolve();
    const orig = ServiceWorkerRegistration.prototype.update;
    ServiceWorkerRegistration.prototype.update = function (this: ServiceWorkerRegistration) {
      w.__updates++;
      const p = orig.call(this);
      w.__lastUpdate = p.catch(() => undefined);
      return p;
    };
  });
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  const toast = page.locator('#sw-update-toast');
  // Dispatches a resume and returns how many checks have run, once the last
  // one has settled.
  const resume = () =>
    page.evaluate(async () => {
      const w = window as unknown as { __updates: number; __lastUpdate: Promise<unknown> };
      document.dispatchEvent(new Event('visibilitychange'));
      await w.__lastUpdate;
      return w.__updates;
    });

  expect(await resume(), 'the first resume checks (against A: nothing new)').toBe(1);
  await deployB();
  expect(await resume(), 'under a minute since that check: throttled').toBe(1);
  await page.clock.fastForward(40_000);
  expect(await resume(), 'still under a minute: throttled, and must NOT reset the clock').toBe(1);
  await expect(toast).toBeHidden();
  await page.clock.fastForward(25_000);
  expect(await resume(), 'over a minute since the last check that RAN').toBe(2);
  await expect(toast).toBeVisible();
});

// 🔴 A REGRESSION TEST (operator report: "the reload button is showing but
// doesn't reload on click"). Two clients of one origin — a second tab, or the
// installed app beside a browser tab — both offer the update. The first one's
// Reload activates B for EVERY client. The second page is then an old document
// under the new worker, with its toast still up; its Reload used to post
// SKIP_WAITING to the worker it captured (now the ACTIVE one, so a no-op), wait
// for a controllerchange that never came, and disable itself: a dead tap.
// It must load the new build instead. Run on the real input of each viewport:
// a touch tap on the phone, a mouse click on the desktop.
const viewports = [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true }, tap: true },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false }, tap: false },
];
for (const vp of viewports) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('another client took the update: Reload here still lands on the new build', async ({ context }) => {
      server = await startServer(versionA, updatePort);
      const origin = `http://localhost:${updatePort}`;
      await context.addInitScript(() => {
        sessionStorage.setItem('loads', String(Number(sessionStorage.getItem('loads') || '0') + 1));
      });
      const first = await context.newPage();
      const second = await context.newPage();
      await first.goto(origin + '/tasks');
      await waitControlled(first);
      await second.goto(origin + '/agents/docs-sweeper');
      await waitControlled(second);
      await deployB();
      await first.evaluate(checkForUpdate);
      await expect(first.locator('#sw-update-toast')).toBeVisible();
      await expect(second.locator('#sw-update-toast')).toBeVisible();

      // The first client takes the update.
      await first.locator('[data-sw-reload]').click();
      await expect.poll(() => first.evaluate(controllerBuild).catch(() => null), { timeout: 15_000 }).toBe(versionB);
      // Premise: the second page is now controlled by B, nothing waits, and its
      // toast is still offering a reload (it IS still build A's document).
      await expect.poll(() => second.evaluate(controllerBuild)).toBe(versionB);
      expect(await second.evaluate(waitingState)).toBeNull();
      await expect(second.locator('#sw-update-toast')).toBeVisible();

      const before = Number(await second.evaluate(() => sessionStorage.getItem('loads')));
      // 🔴 THE PAGE CLOCK IS PAUSED, so Reload's timed fallback cannot fire:
      // the reload below must come from Reload seeing that nothing waits. (The
      // fallback has a test of its own, next.)
      await second.clock.install();
      await second.clock.pauseAt(Date.now() + 1000);
      const reloaded = second.waitForEvent('load', { timeout: 10_000 });
      const button = second.locator('[data-sw-reload]');
      if (vp.tap) await button.tap();
      else await button.click();
      await reloaded;
      expect(Number(await second.evaluate(() => sessionStorage.getItem('loads'))), 'the tap reloaded the page').toBe(before + 1);
      expect(await second.evaluate(controllerBuild)).toBe(versionB);
      await expect(second.locator('#sw-update-toast')).toBeHidden();
    });
  });
}

// The fallback: a waiting worker that is told SKIP_WAITING but never takes
// control (here the message is dropped, so no controllerchange can come) must
// not leave Reload dead. The page clock is paused, so it is the fallback timer
// — and nothing else — that reloads, and only once the clock passes it.
test('Reload still reloads when the waiting worker never takes control', async ({ page }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.addInitScript(() => {
    sessionStorage.setItem('loads', String(Number(sessionStorage.getItem('loads') || '0') + 1));
    const orig = ServiceWorker.prototype.postMessage;
    ServiceWorker.prototype.postMessage = function (this: ServiceWorker, msg: unknown, ...rest: unknown[]) {
      if (msg && (msg as { type?: string }).type === 'SKIP_WAITING') {
        (window as unknown as { __skipDropped: number }).__skipDropped = ((window as unknown as { __skipDropped: number }).__skipDropped || 0) + 1;
        return;
      }
      return (orig as (...a: unknown[]) => void).call(this, msg, ...rest);
    };
  });
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  await deployB();
  await page.evaluate(checkForUpdate);
  await expect(page.locator('#sw-update-toast')).toBeVisible();
  expect(await page.evaluate(waitingState)).toBe('installed');

  await page.clock.install();
  await page.clock.pauseAt(Date.now() + 1000);
  const before = Number(await page.evaluate(() => sessionStorage.getItem('loads')));
  await page.locator('[data-sw-reload]').click();
  // CONTROL: Reload really asked the waiting worker (and the ask was dropped).
  await expect.poll(() => page.evaluate(() => (window as unknown as { __skipDropped?: number }).__skipDropped ?? 0)).toBe(1);
  expect(Number(await page.evaluate(() => sessionStorage.getItem('loads'))), 'nothing reloads while the clock stands still').toBe(before);
  const reloaded = page.waitForEvent('load', { timeout: 10_000 });
  await page.clock.runFor(3_000);
  await reloaded;
  expect(Number(await page.evaluate(() => sessionStorage.getItem('loads'))), 'the fallback reloaded once').toBe(before + 1);
});

// PLACEMENT (operator report: "it's blocking the UI, move it higher up"; the
// operator then chose "a compact pill in the top corner"). The pill sits in
// the top-right corner and covers NO control — on every page,
// at a phone in both orientations and on a desktop, and with the sidebar open.
// Offered for real (a deploy), so it is placed by the code that places it in
// production.
//
// 🔴 "COVERS" IS ASKED OF THE BROWSER, NOT COMPUTED FROM BOXES. Every
// interactive element whose box intersects the pill is probed with
// elementFromPoint across the intersection: if the pill (or its child) is what
// a tap there would hit, the control is covered. An open sidebar that slides
// OVER the pill therefore passes — its links are what the tap reaches — and a
// pill stacked over them fails. (This used to check only fixed/sticky elements
// and the composer, while its title said "covers no control".)
const coverageJS = `(() => {
  const t = document.getElementById('sw-update-toast');
  const tb = t.getBoundingClientRect();
  const header = document.querySelector('header');
  const sel = 'a, button, input, select, textarea, [role=button], [tabindex]';
  const covered = [];
  let intersecting = 0;
  for (const e of document.querySelectorAll(sel)) {
    if (e === t || t.contains(e) || e.contains(t)) continue;
    const b = e.getBoundingClientRect();
    const l = Math.max(b.left, tb.left), r = Math.min(b.right, tb.right);
    const top = Math.max(b.top, tb.top), bot = Math.min(b.bottom, tb.bottom);
    if (r - l < 1 || bot - top < 1) continue;
    intersecting++;
    let hitPill = false;
    for (const fx of [0.1, 0.5, 0.9]) for (const fy of [0.1, 0.5, 0.9]) {
      const h = document.elementFromPoint(l + (r - l) * fx, top + (bot - top) * fy);
      if (h && (h === t || t.contains(h))) hitPill = true;
    }
    if (hitPill) covered.push(e.tagName.toLowerCase() + (e.id ? '#' + e.id : '') + ' "' + (e.textContent || '').trim().slice(0, 24) + '"');
  }
  return { top: tb.top, right: tb.right, width: tb.width, height: tb.height, vw: innerWidth, vh: innerHeight,
    headerBottom: header ? header.getBoundingClientRect().bottom : 0, intersecting, covered };
})()`;

const placements = [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true }, sidebar: true, sidebarReachesPill: true },
  // At 844 wide the 18rem sidebar ends far left of a right-anchored pill.
  { name: 'phone landscape 844×390', use: { viewport: { width: 844, height: 390 }, hasTouch: true, isMobile: true }, sidebar: true, sidebarReachesPill: false },
  // The desktop sidebar is always open (lg:), so every page above already had it.
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false }, sidebar: false, sidebarReachesPill: false },
];
for (const vp of placements) {
  test.describe(`placement ${vp.name}`, () => {
    test.use(vp.use);

    test('the update pill sits in the top-right corner and covers no control', async ({ page }) => {
      server = await startServer(versionA, updatePort);
      const origin = `http://localhost:${updatePort}`;
      await page.goto(origin + '/tasks');
      await waitControlled(page);
      await deployB();
      await page.evaluate(checkForUpdate);
      const pill = page.locator('#sw-update-toast');
      let sidebarIntersections = 0;
      // (`/` and /agents are not listed: with no chief they redirect to an
      // agent's page here, which is. The retired Agents list's place in this
      // list is another sidebar page, Account.)
      for (const path of ['/tasks', '/account', '/agents/docs-sweeper']) {
        // A full load re-offers the waiting update from the registration check.
        await page.goto(origin + path);
        await expect(pill).toBeVisible();
        await page.waitForLoadState('networkidle');
        const r = await page.evaluate(coverageJS);
        expect(r.height, `CONTROL on ${path}: the pill was really laid out`).toBeGreaterThan(20);
        expect(r.width, `on ${path}: a compact pill, not a bar`).toBeLessThan(220);
        expect(r.right, `on ${path}: anchored to the right edge`).toBeGreaterThan(r.vw - 32);
        // In the top corner: under the safe-area inset (0 here), in the top
        // half. Whether that is IN the header's empty right end or below the
        // header is the placement's choice; covering a control never is.
        expect(r.top, `on ${path}: under the safe-area inset`).toBeGreaterThanOrEqual(0);
        expect(r.top + r.height, `on ${path}: in the top half`).toBeLessThan(r.vh / 2);
        console.log(`placement ${vp.name} ${path}: top=${r.top} headerBottom=${r.headerBottom} width=${r.width}`);
        expect(r.covered, `on ${path}: covers no control`).toEqual([]);
        if (vp.sidebar) {
          await page.click('#sidebar-open');
          await expect(page.locator('#sidebar-more-summary')).toBeVisible();
          // Let the slide-in finish: the probe reads where things ARE. It waits
          // until NOTHING in the sidebar is animating, settling each round.
          // 🔴 Not Promise.all over `finished`: the panel slides under the
          // pointer the click left behind, so a header button's hover colour
          // transition starts and is then replaced mid-slide — a CANCELLED
          // transition, whose `finished` rejects with AbortError ("The user
          // aborted a request"; CI runs 41001388651 and 41089311557). A
          // cancelled transition has stopped moving, which is all this wait is for.
          await page.locator('#sidebar').evaluate(async (el) => {
            for (let running; (running = el.getAnimations({ subtree: true })).length; ) {
              await Promise.allSettled(running.map((a) => a.finished));
            }
          });
          const s = await page.evaluate(coverageJS);
          sidebarIntersections += s.intersecting;
          expect(s.covered, `on ${path} with the sidebar open: covers no control`).toEqual([]);
          await page.keyboard.press('Escape');
        }
      }
      if (vp.sidebarReachesPill) {
        // CONTROL: the open sidebar really overlaps the pill somewhere, so the
        // "slides over it" case was exercised rather than vacuously passed.
        expect(sidebarIntersections, 'CONTROL: the open sidebar overlapped the pill').toBeGreaterThan(0);
      }
      // The chat page really had its composer.
      await expect(page.locator('#chat-input')).toBeVisible();
    });
  });
}

test.describe('the pill, on a phone', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  // Showing the pill moves nothing: it is placed while invisible, then shown.
  test('offering the update causes no layout shift', async ({ page }) => {
    server = await startServer(versionA, updatePort);
    const origin = `http://localhost:${updatePort}`;
    await page.goto(origin + '/tasks');
    await waitControlled(page);
    await deployB();
    await page.evaluate(checkForUpdate);
    const pill = page.locator('#sw-update-toast');
    await expect(pill).toBeVisible();
    await page.waitForLoadState('networkidle');
    const shift = await page.evaluate(async () => {
      const t = document.getElementById('sw-update-toast')!;
      t.hidden = true;
      let total = 0;
      const po = new PerformanceObserver((l) => { for (const e of l.getEntries()) total += (e as unknown as { value: number }).value; });
      po.observe({ type: 'layout-shift', buffered: false });
      // htmx:load is what re-offers a waiting update after a swap.
      document.body.dispatchEvent(new Event('htmx:load', { bubbles: true }));
      await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
      await new Promise((r) => setTimeout(r, 0));
      po.disconnect();
      return { total, shown: !t.hidden };
    });
    expect(shift.shown, 'CONTROL: the pill was offered again').toBe(true);
    expect(shift.total).toBe(0);
  });

  // Dismissible, and a dismissal is for THAT update: an in-app navigation does
  // not re-offer it, but a NEWER build (a third deploy) is offered again.
  test('a dismissed update stays dismissed, and a newer build is offered again', async ({ page }) => {
    server = await startServer(versionA, updatePort);
    const origin = `http://localhost:${updatePort}`;
    await page.goto(origin + '/tasks');
    await waitControlled(page);
    await deployB();
    await page.evaluate(checkForUpdate);
    const pill = page.locator('#sw-update-toast');
    await expect(pill).toBeVisible();
    await page.locator('[data-sw-dismiss]').tap();
    await expect(pill).toBeHidden();
    await page.evaluate(() => document.body.dispatchEvent(new Event('htmx:load', { bubbles: true })));
    await expect(pill, 'the dismissed update is not re-offered by an in-app navigation').toBeHidden();

    // A third build: B is replaced in the waiting slot by C, which is offered.
    await server!.stop();
    server = await startServer(versionC, updatePort);
    await page.evaluate(checkForUpdate);
    await expect(pill, 'a newer build is offered after a dismissal').toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// THE PILL'S VERSIONS, ITS RELOAD ICON, AND DRAGGING IT (operator, 2026-11-12).
// Run at a phone (a finger: CDP touch events, so the pointer events are
// pointerType "touch") and at a desktop (a mouse), each viewport set here.
// ---------------------------------------------------------------------------

const POS_KEY = 'muster.updatePill.pos'; // pwaScript's storage key
const savedPos = (page: Page) => page.evaluate((k) => localStorage.getItem(k), POS_KEY);

// Offers B over A for real: two builds, a waiting worker.
async function offerB(page: Page): Promise<string> {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  await deployB();
  await page.evaluate(checkForUpdate);
  await expect(page.locator('#sw-update-toast')).toBeVisible();
  await page.waitForLoadState('networkidle');
  return origin;
}

type Pt = { x: number; y: number };
// press: a press at `from`, moved through `steps` points to `to`, released —
// with the viewport's real input (a finger on the phone, a mouse on the
// desktop).
async function press(page: Page, touch: boolean, from: Pt, to: Pt, steps = 8): Promise<void> {
  if (!touch) {
    await page.mouse.move(from.x, from.y);
    await page.mouse.down();
    await page.mouse.move(to.x, to.y, { steps });
    await page.mouse.up();
    return;
  }
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: from.x, y: from.y }] });
  for (let i = 1; i <= steps; i++) {
    const x = from.x + ((to.x - from.x) * i) / steps, y = from.y + ((to.y - from.y) * i) / steps;
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y }] });
  }
  // Held still before lifting, as a finger that places something does: a
  // lift at full speed ends in a FLING, and Chromium spends the next tap on
  // stopping it (measured: a tap on × right after a fast far press did not
  // dismiss).
  await page.waitForTimeout(200);
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: to.x, y: to.y }] });
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await cdp.detach();
}
const pillBox = async (page: Page) => (await page.locator('#sw-update-toast').boundingBox())!;
// A point on the pill's BODY: its left padding, clear of the text and buttons.
const bodyPoint = (b: { x: number; y: number; height: number }): Pt => ({ x: b.x + 5, y: b.y + b.height / 2 });
const centre = async (page: Page, sel: string): Promise<Pt> => {
  const b = (await page.locator(sel).boundingBox())!;
  return { x: b.x + b.width / 2, y: b.y + b.height / 2 };
};

// answerAsRelease makes every WAITING worker answer GET_VERSION with v (the
// e2e builds are not releases), so the page's drawing of a release is tested.
async function answerAsRelease(page: Page, v: string): Promise<void> {
  await page.addInitScript((build) => {
    const orig = ServiceWorker.prototype.postMessage;
    ServiceWorker.prototype.postMessage = function (this: ServiceWorker, msg: unknown, ...rest: unknown[]) {
      const m = msg as { type?: string };
      if (m && m.type === 'GET_VERSION' && this !== navigator.serviceWorker.controller) {
        const ports = rest[0] as MessagePort[];
        ports[0].postMessage({ build });
        return;
      }
      return (orig as (...a: unknown[]) => void).call(this, msg, ...rest);
    };
  }, v);
}

const pillViewports = [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true }, touch: true },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false }, touch: false },
];
for (const vp of pillViewports) {
  test.describe(`the pill ${vp.name}`, () => {
    test.use(vp.use);

    // The current version is the page's (rendered by build A), the new one is
    // the WAITING worker's answer (build B's sw.js). Neither e2e build is an
    // X.Y.Z release, so neither is a link.
    test('the pill shows both versions when a worker is waiting', async ({ page }) => {
      await offerB(page);
      const pill = page.locator('#sw-update-toast');
      await expect(pill).toHaveAttribute('data-sw-current', versionA);
      await expect(pill.locator('[data-sw-versions]')).toBeVisible();
      await expect(pill.locator('[data-sw-versions]')).toHaveText(`Update ${versionA} → ${versionB}`);
      await expect(pill.locator('[data-sw-label]')).toBeHidden();
      await expect(pill.locator('a'), 'no release link for a non-release build').toHaveCount(0);
      expect(await page.evaluate(waitingState), 'the worker is still waiting').toBe('installed');
      // The wider "a → b" pill is placed like the narrow one: in the corner,
      // compact, covering no control. (The placement tests above offer the
      // update on FULL loads, which build B's server answers, so the page is
      // already B's and they see the narrow "Update available" form.)
      const r = await page.evaluate(coverageJS);
      expect(r.width, 'still a compact pill, not a bar').toBeLessThan(300);
      expect(r.right, 'anchored to the right edge').toBeGreaterThan(r.vw - 32);
      expect(r.covered, 'covers no control').toEqual([]);
    });

    // A release version links to that release's notes, and a tap opens them
    // (the drag threshold does not eat it). The worker's ANSWER is replaced
    // with a release version here — the e2e builds are not releases — so the
    // page's drawing of a release is what is tested.
    test('a release version links to its notes, and a tap on it opens them', async ({ page, context }) => {
      await answerAsRelease(page, '0.211.0');
      let notes = 0;
      await context.route('https://github.com/**', (route) => { notes++; return route.fulfill({ status: 200, contentType: 'text/html', body: '<title>notes</title>' }); });
      await offerB(page);
      const link = page.locator('#sw-update-toast a[data-sw-release]');
      await expect(link).toHaveText('0.211.0');
      await expect(link).toHaveAttribute('href', 'https://github.com/ZacxDev/muster/releases/tag/v0.211.0');
      await expect(link).toHaveAttribute('target', '_blank');
      // Only the version being moved to links; the current one is plain text.
      await expect(page.locator('#sw-update-toast a'), 'one link: the new version').toHaveCount(1);
      await expect(page.locator('#sw-update-toast [data-sw-to] a')).toHaveCount(1);
      await expect(page.locator('#sw-update-toast [data-sw-from]')).toHaveText(versionA);
      expect(await page.locator('#sw-update-toast [data-sw-from]').evaluate((e) => e.tagName)).toBe('SPAN');
      const popup = context.waitForEvent('page');
      if (vp.touch) await link.tap();
      else await link.click();
      const opened = await popup;
      await opened.waitForLoadState();
      expect(opened.url()).toBe('https://github.com/ZacxDev/muster/releases/tag/v0.211.0');
      expect(notes, 'CONTROL: the notes page was really requested').toBeGreaterThan(0);
      expect(await savedPos(page), 'a tap is not a drag').toBeNull();
    });

    // An older worker that does not answer GET_VERSION: the plain label, and
    // Reload still takes the update.
    test('the fallback label when the worker does not report its version, and Reload still works', async ({ page }) => {
      await page.addInitScript(() => {
        const orig = ServiceWorker.prototype.postMessage;
        ServiceWorker.prototype.postMessage = function (this: ServiceWorker, msg: unknown, ...rest: unknown[]) {
          const m = msg as { type?: string };
          if (m && m.type === 'GET_VERSION' && this !== navigator.serviceWorker.controller) {
            (window as unknown as { __unanswered: number }).__unanswered = ((window as unknown as { __unanswered: number }).__unanswered || 0) + 1;
            return; // an old worker: never answers
          }
          return (orig as (...a: unknown[]) => void).call(this, msg, ...rest);
        };
      });
      await offerB(page);
      const pill = page.locator('#sw-update-toast');
      expect(await page.evaluate(() => (window as unknown as { __unanswered?: number }).__unanswered ?? 0), 'CONTROL: the waiting worker was asked').toBeGreaterThan(0);
      await expect(pill.locator('[data-sw-label]')).toHaveText('Update available');
      await expect(pill.locator('[data-sw-label]')).toBeVisible();
      await expect(pill.locator('[data-sw-versions]')).toBeHidden();
      const reloaded = page.waitForEvent('load', { timeout: 15_000 });
      const reload = page.getByRole('button', { name: 'Reload to update' });
      if (vp.touch) await reload.tap();
      else await reload.click();
      await reloaded;
      expect(await page.evaluate(controllerBuild)).toBe(versionB);
    });

    test('the reload icon activates the waiting worker', async ({ page }) => {
      await page.addInitScript(() => {
        sessionStorage.setItem('loads', String(Number(sessionStorage.getItem('loads') || '0') + 1));
      });
      await offerB(page);
      const reload = page.getByRole('button', { name: 'Reload to update' });
      await expect(reload).toHaveAttribute('data-sw-reload', '');
      const box = (await reload.boundingBox())!;
      expect(box.width, 'a 44px touch target').toBeGreaterThanOrEqual(44);
      expect(box.height, 'a 44px touch target').toBeGreaterThanOrEqual(44);
      await expect(reload, 'an icon, no word').toHaveText('');
      expect(await page.evaluate(controllerBuild)).toBe(versionA);
      const before = Number(await page.evaluate(() => sessionStorage.getItem('loads')));
      const reloaded = page.waitForEvent('load', { timeout: 15_000 });
      if (vp.touch) await reload.tap();
      else await reload.click();
      await reloaded;
      expect(await page.evaluate(controllerBuild)).toBe(versionB);
      expect(await page.evaluate(waitingState)).toBeNull();
      expect(Number(await page.evaluate(() => sessionStorage.getItem('loads'))), 'exactly one reload').toBe(before + 1);
    });

    // Dragged, it stays where it was let go; that spot is remembered and the
    // NEXT update (a third build) is offered there, not in the corner.
    test('dragging moves the pill, and the next update is offered where it was left', async ({ page }) => {
      await offerB(page);
      const start = await pillBox(page);
      expect(await savedPos(page), 'nothing saved before a drag').toBeNull();
      const from = bodyPoint(start);
      const d = { x: -137, y: 211 };
      await press(page, vp.touch, from, { x: from.x + d.x, y: from.y + d.y });
      const moved = await pillBox(page);
      expect(Math.abs(moved.x - (start.x + d.x)), `followed the pointer in x: ${JSON.stringify({ start, moved })}`).toBeLessThan(1.5);
      expect(Math.abs(moved.y - (start.y + d.y)), `followed the pointer in y: ${JSON.stringify({ start, moved })}`).toBeLessThan(1.5);
      const saved = JSON.parse((await savedPos(page))!);
      expect(Math.abs(saved.left - moved.x)).toBeLessThan(1.5);
      expect(Math.abs(saved.top - moved.y)).toBeLessThan(1.5);
      await expect(page.locator('#sw-update-toast')).toBeVisible();

      // Take B, then deploy C: a new document, a new offer — at the saved spot.
      const reloaded = page.waitForEvent('load', { timeout: 15_000 });
      await page.locator('[data-sw-reload]').click();
      await reloaded;
      expect(await page.evaluate(controllerBuild)).toBe(versionB);
      await expect(page.locator('#sw-update-toast')).toBeHidden();
      await server!.stop();
      server = await startServer(versionC, updatePort);
      await page.evaluate(checkForUpdate);
      await expect(page.locator('#sw-update-toast')).toBeVisible();
      await expect(page.locator('#sw-update-toast [data-sw-versions]')).toHaveText(`Update ${versionB} → ${versionC}`);
      const again = await pillBox(page);
      expect(Math.abs(again.x - moved.x), `the next update is offered at the saved spot: ${JSON.stringify({ moved, again })}`).toBeLessThan(1.5);
      expect(Math.abs(again.y - moved.y), `the next update is offered at the saved spot: ${JSON.stringify({ moved, again })}`).toBeLessThan(1.5);
    });

    // 🔴 TAPS ARE NOT DRAGS. A press that moves less than the threshold — on
    // the body, on ×, on Reload — moves nothing and saves nothing, and a press
    // on a button is never a drag even when it moves far.
    test('a tap on the body, on × or on Reload does not start a drag', async ({ page }) => {
      await offerB(page);
      const start = await pillBox(page);
      // Under the threshold, on the body.
      const b = bodyPoint(start);
      await press(page, vp.touch, b, { x: b.x - 3, y: b.y + 3 }, 3);
      expect(await pillBox(page), 'a 3px press on the body moved the pill').toEqual(start);
      expect(await savedPos(page)).toBeNull();
      // A far press-and-move that starts on ×: a button is not a handle.
      const x = await centre(page, '[data-sw-dismiss]');
      await press(page, vp.touch, x, { x: x.x - 90, y: x.y + 120 });
      expect(await savedPos(page), 'a press on × became a drag').toBeNull();
      await expect(page.locator('#sw-update-toast')).toBeVisible();
      expect(await pillBox(page), 'a press on × moved the pill').toEqual(start);
      // A tap on ×: still a dismissal, and no drag. (A mouse click jitters
      // 2px; a finger is a plain tap — whether a jittering touch is still a
      // tap is Chromium's gesture slop, not the pill's.)
      if (vp.touch) await page.locator('[data-sw-dismiss]').tap();
      else await press(page, false, x, { x: x.x + 2, y: x.y - 2 }, 2);
      await expect(page.locator('#sw-update-toast'), 'the jittery tap on × still dismissed').toBeHidden();
      expect(await savedPos(page)).toBeNull();
      // Re-offered (a full load re-offers a waiting update), a tap on Reload:
      // still a reload, and no drag. (The reloaded document came from
      // build B's server, so it IS B's page and the pill reads "Update
      // available": a different width, the same corner.)
      await page.reload();
      await expect(page.locator('#sw-update-toast')).toBeVisible();
      await page.waitForLoadState('networkidle');
      const corner = await pillBox(page);
      expect([corner.x + corner.width, corner.y], 'no saved spot: the corner again').toEqual([start.x + start.width, start.y]);
      const r = await centre(page, '[data-sw-reload]');
      const reloaded = page.waitForEvent('load', { timeout: 15_000 });
      if (vp.touch) await page.locator('[data-sw-reload]').tap();
      else await press(page, false, r, { x: r.x - 2, y: r.y + 2 }, 2);
      await reloaded;
      expect(await page.evaluate(controllerBuild)).toBe(versionB);
      expect(await savedPos(page), 'the tap on Reload became a drag').toBeNull();
    });

    // 🔴 A NEWER BUILD WHILE THE PILL SHOWS: the label is redrawn for it, in
    // place. It used to keep "→ b" while Reload installed c.
    test('a newer build arriving while the pill shows redraws its version in place', async ({ page }) => {
      await offerB(page);
      const versions = page.locator('#sw-update-toast [data-sw-versions]');
      await expect(versions).toHaveText(`Update ${versionA} → ${versionB}`);
      const before = await pillBox(page);
      await server!.stop();
      server = await startServer(versionC, updatePort);
      await page.evaluate(checkForUpdate);
      await expect(versions, 'the label names the build Reload will install').toHaveText(`Update ${versionA} → ${versionC}`);
      const after = await pillBox(page);
      expect([after.x + after.width, after.y], 'redrawn in place, not moved').toEqual([before.x + before.width, before.y]);
      const reloaded = page.waitForEvent('load', { timeout: 15_000 });
      await page.locator('[data-sw-reload]').click();
      await reloaded;
      expect(await page.evaluate(controllerBuild)).toBe(versionC);
    });

    // A drag that starts on the release link and is let go over it (the link
    // moves with the pointer) does not open the notes; a tap afterwards does.
    test('a drag released over the release link does not open it', async ({ page, context }) => {
      await answerAsRelease(page, '0.211.0');
      let notes = 0;
      await context.route('https://github.com/**', (route) => { notes++; return route.fulfill({ status: 200, contentType: 'text/html', body: '<title>notes</title>' }); });
      let popups = 0;
      context.on('page', () => { popups++; });
      await offerB(page);
      const link = page.locator('#sw-update-toast a[data-sw-release]');
      await expect(link).toHaveText('0.211.0');
      const at = await centre(page, '#sw-update-toast a[data-sw-release]');
      const to = { x: at.x - 120, y: at.y + 150 };
      await press(page, vp.touch, at, to);
      expect(await savedPos(page), 'CONTROL: it was a drag').not.toBeNull();
      const now = await centre(page, '#sw-update-toast a[data-sw-release]');
      expect(Math.abs(now.x - to.x) < 2 && Math.abs(now.y - to.y) < 2, `CONTROL: let go over the link: ${JSON.stringify({ now, to })}`).toBe(true);
      // ⚠ CHROMIUM NEVER CLICKS THE LINK AT THE END OF A DRAG (measured: with
      // the swallow deleted this test stayed green) — the captured pointer
      // retargets a mouse click to the pill, and a moved touch is no tap. An
      // engine that delivers the gesture's click late (iOS's synthesized
      // click) is played here: a click on the link right after the release.
      await link.evaluate((a) => a.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true })));
      // Positive control, and the bound on a late open: a tap opens exactly one.
      const popup = context.waitForEvent('page');
      if (vp.touch) await link.tap();
      else await link.click();
      await (await popup).waitForLoadState();
      expect(popups, 'only the tap opened the notes, not the drag').toBe(1);
      expect(notes).toBe(1);
    });

    // 🔴 CLAMPED: dragged past an edge, the pill stops AT the edge; a smaller
    // viewport pulls it back inside; the original viewport puts it back at the
    // spot it was left at (a clamp does not overwrite the saved spot).
    test('the pill is clamped inside the viewport, while dragged and after a resize', async ({ page }) => {
      await offerB(page);
      const vw = vp.use.viewport.width, vh = vp.use.viewport.height;
      const start = await pillBox(page);
      const from = bodyPoint(start);
      // To the bottom-right corner, held by the pill's LEFT end: the pill
      // would hang a whole pill-width past the right edge. (The pointer stays
      // inside the viewport, so every move is delivered.)
      await press(page, vp.touch, from, { x: vw - 2, y: vh - 2 }, 10);
      const edge = await pillBox(page);
      const cw = await page.evaluate(() => document.documentElement.clientWidth);
      expect(edge.x + edge.width, `inside the right edge: ${JSON.stringify(edge)}`).toBeLessThanOrEqual(cw + 0.5);
      expect(edge.x + edge.width, `AT the right edge: ${JSON.stringify(edge)}`).toBeGreaterThan(cw - 1.5);
      expect(edge.y + edge.height, `AT the bottom edge: ${JSON.stringify(edge)}`).toBeGreaterThan(vh - 1.5);
      expect(edge.y + edge.height, `inside the bottom edge: ${JSON.stringify(edge)}`).toBeLessThanOrEqual(vh + 0.5);

      // The viewport shrinks past it (a rotation, a resized window).
      const small = { width: Math.max(Math.round(vw * 0.6), Math.ceil(edge.width) + 40), height: Math.round(vh * 0.55) };
      expect(edge.x + edge.width > small.width && edge.y + edge.height > small.height, 'CONTROL: the smaller viewport really cuts the pill off').toBe(true);
      await page.setViewportSize(small);
      await expect
        .poll(async () => {
          const b = await pillBox(page);
          return b.x + b.width <= small.width + 0.5 && b.y + b.height <= small.height + 0.5 && b.x >= -0.5 && b.y >= -0.5;
        }, { message: 'the pill is pulled inside the smaller viewport' })
        .toBe(true);
      await page.setViewportSize(vp.use.viewport);
      // Back in the column it was left in, placed from the saved spot again —
      // at it, or moved UP off a control it would cover there (the bottom-right
      // corner holds the chat button): never left where the clamp put it.
      await expect
        .poll(async () => { const b = await pillBox(page); return Math.round(b.x); }, { message: 'back in the column it was left in' })
        .toBe(Math.round(edge.x));
      const backAt = await pillBox(page);
      expect(backAt.y, `placed from the saved spot: ${JSON.stringify({ backAt, edge })}`).toBeLessThanOrEqual(edge.y + 0.5);
      expect((await page.evaluate(coverageJS)).covered, 'covers no control').toEqual([]);

      // To the top-left corner, held by its text (a body point well inside
      // the pill): it would hang past the left and top edges.
      const back = await pillBox(page);
      const from2 = await centre(page, '#sw-update-toast [data-sw-versions]');
      expect(from2.x - back.x, 'CONTROL: held far enough in to overshoot the left edge').toBeGreaterThan(30);
      await press(page, vp.touch, from2, { x: 1, y: 1 }, 10);
      const tl = await pillBox(page);
      expect(tl.x, `AT the left edge: ${JSON.stringify(tl)}`).toBeGreaterThanOrEqual(-0.5);
      expect(tl.x).toBeLessThan(1.5);
      expect(tl.y, `AT the top edge: ${JSON.stringify(tl)}`).toBeGreaterThanOrEqual(-0.5);
      expect(tl.y).toBeLessThan(1.5);
    });
  });
}

// 🔴 THE PHONE KEYBOARD. The pages declare interactive-widget=resizes-content,
// so an open keyboard shrinks the viewport and fires resize, and the pinned
// composer rises with it. A saved spot must not end up on the composer or
// Send: the pill moves off them, and the saved spot is kept for when the
// keyboard closes. (Measured: a spot clamped to the very bottom lands on the
// composer's lower padding, below its controls; the spot that meets them is
// where the risen composer's textarea will be, so that is where it is left.)
test.describe('the pill and the phone keyboard', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('a saved spot is moved off the composer when the keyboard brings the composer up under it', async ({ page }) => {
    const origin = await offerB(page);
    await page.goto(origin + '/agents/docs-sweeper');
    await expect(page.locator('#sw-update-toast')).toBeVisible();
    await page.waitForLoadState('networkidle');
    await expect(page.locator('#chat-input')).toBeVisible();
    const full = 844, kb = { width: 390, height: 508 }; // 844 less a keyboard
    // Where the textarea will be once the keyboard is up: the composer is
    // pinned to the bottom, so its distance from the bottom is kept.
    const inputTop = await page.locator('#chat-input').evaluate((e) => e.getBoundingClientRect().top);
    const target = kb.height - (full - inputTop) + 2;
    const start = await pillBox(page);
    const from = bodyPoint(start);
    await press(page, true, from, { x: from.x - 100, y: from.y + (target - start.y) });
    const saved = JSON.parse((await savedPos(page))!);
    expect(Math.abs(saved.top - target), 'CONTROL: left where the risen textarea will be').toBeLessThan(1.5);

    await page.setViewportSize(kb);
    const sel = ['textarea', 'button', 'input', 'select', '[role=button]'].map((x) => '#chat-form ' + x).join(', ');
    const hits = () =>
      page.evaluate((q) => {
        const t = document.getElementById('sw-update-toast')!.getBoundingClientRect();
        const out: string[] = [];
        let seen = 0;
        for (const e of document.querySelectorAll(q)) {
          const b = e.getBoundingClientRect();
          if (!b.width || !b.height || getComputedStyle(e).visibility === 'hidden') continue;
          seen++;
          if (b.left < t.right && t.left < b.right && b.top < t.bottom && t.top < b.bottom) out.push(e.tagName + (e.id ? '#' + e.id : '') + ' ' + JSON.stringify([b.top, b.bottom]));
        }
        return { seen, out, pill: [t.top, t.bottom], vh: innerHeight };
      }, sel);
    await expect.poll(async () => (await hits()).vh).toBe(kb.height);
    // CONTROL: the composer's textarea really rose to the saved spot.
    const risen = await page.locator('#chat-input').evaluate((e) => { const b = e.getBoundingClientRect(); return [b.top, b.bottom]; });
    expect(risen[0] < saved.top + start.height && saved.top < risen[1], `CONTROL: the saved spot is on the risen textarea: ${JSON.stringify({ risen, saved })}`).toBe(true);
    await expect
      .poll(async () => { const h = await hits(); return { seen: h.seen > 0, out: h.out }; }, { message: 'the pill covers no composer control' })
      .toEqual({ seen: true, out: [] });
    const b = await pillBox(page);
    expect(b.y + b.height, 'inside the shrunk viewport').toBeLessThanOrEqual(kb.height + 0.5);
    expect(JSON.parse((await savedPos(page))!), 'the saved spot is kept').toEqual(saved);
  });
});

// Storage that throws (a private window, blocked site data): the pill is still
// offered, still drags, and the spot lives for the page.
test.describe('the pill without storage', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

  test('dragging works when localStorage throws', async ({ page }) => {
    await page.addInitScript(() => {
      Object.defineProperty(window, 'localStorage', { configurable: true, get() { throw new DOMException('denied', 'SecurityError'); } });
    });
    await offerB(page);
    const start = await pillBox(page);
    const from = bodyPoint(start);
    await press(page, false, from, { x: from.x - 300, y: from.y + 250 });
    const moved = await pillBox(page);
    expect(Math.abs(moved.x - (start.x - 300))).toBeLessThan(1.5);
    expect(Math.abs(moved.y - (start.y + 250))).toBeLessThan(1.5);
    // A re-offer in this page (an in-app navigation's htmx:load) keeps the spot.
    await page.evaluate(() => {
      const t = document.getElementById('sw-update-toast')!;
      t.hidden = true;
      document.body.dispatchEvent(new Event('htmx:load', { bubbles: true }));
    });
    await expect(page.locator('#sw-update-toast')).toBeVisible();
    // Placed from the in-memory spot: its column, its height or the nearest
    // spot that covers no control.
    const again = await pillBox(page);
    expect(Math.round(again.x)).toBe(Math.round(moved.x));
    expect(Math.abs(again.y - moved.y), JSON.stringify({ again, moved })).toBeLessThan(80);
    expect((await page.evaluate(coverageJS)).covered).toEqual([]);
  });
});
