// The sign-in page while a sign-in is in flight, and what replaced the
// sign-in's Clear-Site-Data (internal/api signin_progress.go, login.go
// clearSiteCaches / clearCachesHTML).
//
// The sign-in posts as a plain form navigation, so the browser keeps the
// sign-in page on screen until the app's page arrives. It used to stay
// unchanged for all of it — and the sign-in's 303 carried Clear-Site-Data:
// "cache", which the browser waits on before following the redirect, for a
// time that grows with the size of the browser's whole disk cache (measured
// in Chromium: 350–500 ms at 20,000 entries, 10.7 s on the first clear after
// a start). These tests hold the POST (page.route) so the in-flight state can
// be read, at a phone width and a desktop one.
//
// Its own password-mode server: the refused and accepted sign-ins here are
// judged by a server of their own, inside its own budget (lib/server.ts).
import type { Browser, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { launchWithBfcache } from '../lib/bfcache';
import { password, signinProgressPort, versionA } from '../lib/env';
import { landedHome } from '../lib/nav';
import { startServer, Running } from '../lib/server';

const origin = `http://localhost:${signinProgressPort}`;
let server: Running | undefined;
let browser: Browser;
// Stated on every context: the runner applies the suite's signed-in
// storageState (lib/fixtures.ts) to a browser a spec launches itself too.
const signedOut = { cookies: [], origins: [] };

test.beforeAll(async ({ playwright }) => {
  server = await startServer(versionA, signinProgressPort);
  browser = await launchWithBfcache(playwright.chromium);
});
test.afterAll(async () => {
  await browser?.close();
  await server?.stop();
});

// holdSignIns delays every POST /login by holdMs before letting it through, so
// the page spends a known time with its sign-in in flight.
const holdMs = 1500;
function holdSignIns(page: Page): () => number {
  let n = 0;
  void page.route('**/login', (route) => {
    if (route.request().method() !== 'POST') return void route.continue();
    n++;
    setTimeout(() => void route.continue().catch(() => {}), holdMs);
  });
  return () => n;
}

// 🔴 THE IN-FLIGHT FORM IS READ BY THE PAGE ITSELF, NOT BY PLAYWRIGHT. While
// the form's navigation is pending, Playwright's locators AND page.evaluate
// wait for it and resolve only on the next page (measured: toBeDisabled
// timed out "Received: undefined"; an evaluate threw "Target page … closed"),
// so neither can see the page the person is looking at. This recorder runs in
// every document of the context: on a submit it snapshots the form once its
// handlers have run, then every 100 ms until the page goes, into
// sessionStorage (which the same tab's next page can read). It also taps the
// button once more, as an impatient person would — a tap that must post
// nothing.
const recorder = `(() => {
  const key = '__e2eSignIn';
  const log = (e) => {
    try {
      const a = JSON.parse(sessionStorage.getItem(key) || '[]');
      a.push(e);
      sessionStorage.setItem(key, JSON.stringify(a));
    } catch (x) {}
  };
  const snap = () => {
    const form = document.querySelector('form[action="/login"]');
    const button = form && form.querySelector('button[type=submit]');
    const st = document.getElementById('signin-status');
    const spin = button && button.querySelector('svg.animate-spin');
    return {
      disabled: button ? button.disabled : null,
      button: button ? button.textContent.trim() : null,
      spinner: !!spin && spin.getBoundingClientRect().width > 0,
      busy: form ? form.getAttribute('aria-busy') : null,
      status: st ? { role: st.getAttribute('role'), live: st.getAttribute('aria-live'), text: st.textContent } : null,
    };
  };
  window.__e2eFormState = snap;
  let tapped = false;
  addEventListener('submit', () => {
    log({ kind: 'submit' });
    setTimeout(() => {
      log(Object.assign({ kind: 'after' }, snap()));
      const b = document.querySelector('form[action="/login"] button[type=submit]');
      if (b && !tapped) { tapped = true; b.click(); }
    }, 50);
    setInterval(() => log(Object.assign({ kind: 'tick' }, snap())), 100);
  }, true);
})()`;

interface Entry {
  kind: string;
  [k: string]: unknown;
}
const signInLog = (page: Page): Promise<Entry[]> =>
  page.evaluate(() => {
    const a = JSON.parse(sessionStorage.getItem('__e2eSignIn') || '[]');
    sessionStorage.removeItem('__e2eSignIn');
    return a;
  });

const inFlight = {
  disabled: true,
  button: 'Signing in…',
  spinner: true,
  busy: 'true',
  status: { role: 'status', live: 'polite', text: 'Signing in…' },
};
const idle = {
  disabled: false,
  button: 'Sign in',
  spinner: false,
  busy: null,
  status: { role: 'status', live: 'polite', text: '' },
};

// expectHeldInFlight: one submit, and from just after it until the page went
// (holdMs of it at the least), the form said it was signing in — every
// snapshot, not one.
function expectHeldInFlight(entries: Entry[], why: string) {
  expect(entries.filter((e) => e.kind === 'submit').length, `${why}: exactly one submit (the second tap posted nothing)`).toBe(1);
  const snaps = entries.filter((e) => e.kind !== 'submit').map(({ kind: _k, ...rest }) => rest);
  expect(snaps.length, `${why}: the page was read through the hold (${JSON.stringify(entries)})`).toBeGreaterThanOrEqual(Math.floor(holdMs / 100) - 3);
  for (const s of snaps) expect(s, `${why}: the form while the sign-in was in flight`).toEqual(inFlight);
}

for (const vp of [
  { name: 'phone', viewport: { width: 390, height: 844 } },
  { name: 'desktop', viewport: { width: 1280, height: 800 } },
]) {
  test(`a sign-in shows it is in progress, clears it on a refused password, and lands on the app (${vp.name})`, async () => {
    const ctx = await browser.newContext({ baseURL: origin, viewport: vp.viewport, storageState: signedOut });
    await ctx.addInitScript(recorder);
    const page = await ctx.newPage();
    try {
      await page.goto('/login');
      expect(await page.evaluate(() => (window as any).__e2eFormState()), 'the page as drawn').toEqual(idle);
      const posted = holdSignIns(page);

      // A refused password: in flight for the whole hold, then the server's
      // answer — a new page saying why, with the form back.
      await page.fill('#muster-password', 'not-the-password-at-all');
      await Promise.all([page.waitForURL(/\/login$/), page.click('form button[type=submit]')]);
      await expect(page.getByRole('alert')).toHaveText('That password is not right.', { timeout: 15_000 });
      expectHeldInFlight(await signInLog(page), 'a refused sign-in');
      expect(posted(), 'one POST: the second tap posted nothing').toBe(1);
      expect(await page.evaluate(() => (window as any).__e2eFormState()), 'after a refused password').toEqual(idle);

      // The right password: in flight for the whole hold, then the app, from a
      // redirect that carries no Clear-Site-Data.
      const responses: { status: number; clearSiteData: string | null }[] = [];
      page.on('response', async (r) => {
        if (r.request().method() === 'POST' && new URL(r.url()).pathname === '/login') {
          // allHeaders: the raw headers (headers() leaves Clear-Site-Data out).
          responses.push({ status: r.status(), clearSiteData: (await r.allHeaders())['clear-site-data'] ?? null });
        }
      });
      await page.fill('#muster-password', password);
      await Promise.all([page.waitForURL(landedHome(origin)), page.click('form button[type=submit]')]);
      expectHeldInFlight(await signInLog(page), 'the accepted sign-in');
      expect(posted(), 'one more POST').toBe(2);
      await expect.poll(() => responses.length).toBe(1);
      expect(responses, 'the sign-in answered with a redirect that carries no Clear-Site-Data').toEqual([{ status: 303, clearSiteData: null }]);
    } finally {
      await ctx.close();
    }
  });
}

// 🔴 WHAT THE SIGN-IN'S Clear-Site-Data PROTECTED, STILL PROTECTED. A session
// that ended with no sign-out (it expired, or was ended on another device)
// leaves its pages in this browser's caches; the sign-in page drops them, in
// the background, before anyone signs in on it. Both caches are read: the
// HTTP cache (a fetch with cache: 'only-if-cached' answers only from it) and
// the back/forward cache (a page restored from it keeps its window object).
// Each with a positive control taken before the sign-in page, so a probe that
// cannot see the cache does not pass for a cleared one.
test('the sign-in page drops the pages an ended session left in the caches', async () => {
  const ctx = await browser.newContext({ baseURL: origin, viewport: { width: 390, height: 844 }, storageState: signedOut });
  const page = await ctx.newPage();
  const notRestored: string[] = [];
  const cdp = await ctx.newCDPSession(page);
  await cdp.send('Page.enable');
  cdp.on('Page.backForwardCacheNotUsed', (e: any) => notRestored.push(JSON.stringify((e.notRestoredExplanations || []).map((x: any) => x.reason))));
  const kept = () => page.evaluate(() => (window as any).__e2eKept ?? 'reloaded').catch(() => 'navigating');
  // workerSettled: a first visit's service worker claims its clients when it
  // activates, which evicts a page sitting in the back/forward cache
  // (bfcache-restore.spec.ts measured it); a fresh context waits for it.
  const workerSettled = () =>
    page.evaluate(async () => {
      if (!navigator.serviceWorker) return;
      await navigator.serviceWorker.ready;
      if (navigator.serviceWorker.controller) return;
      await new Promise((r) => {
        navigator.serviceWorker.addEventListener('controllerchange', r, { once: true });
        setTimeout(r, 3000);
      });
    }).catch(() => {});
  const cached = (path: string) =>
    page.evaluate(async (p) => {
      try {
        return (await fetch(p, { cache: 'only-if-cached', mode: 'same-origin' })).status;
      } catch {
        return 'not cached';
      }
    }, path);
  try {
    await page.goto('/login');
    await page.fill('#muster-password', password);
    await Promise.all([page.waitForURL(landedHome(origin)), page.click('form button[type=submit]')]);
    const home = new URL(page.url()).pathname + new URL(page.url()).search;

    // Positive control: Back restores the page from the back/forward cache
    // (its window marker survives), and the HTTP cache holds it.
    await workerSettled();
    await page.evaluate(() => ((window as any).__e2eKept = 'kept'));
    await page.goto('/static/icons/favicon.svg');
    await page.goBack({ waitUntil: 'commit' });
    await expect.poll(kept, {
      message: `CONTROL: Back restores the page from the back/forward cache (not restored because: ${notRestored.join(' ')})`,
    }).toBe('kept');
    expect(await cached(home), 'CONTROL: the HTTP cache holds the page').toBe(200);

    // The session ends with no sign-out, and the browser opens the sign-in page.
    await page.goto('/static/icons/favicon.svg');
    await ctx.clearCookies();
    const cleared = page.waitForResponse((r) => new URL(r.url()).pathname === '/auth/clear-caches', { timeout: 10_000 });
    await page.goto('/login');
    const r = await cleared;
    expect(r.status(), 'the sign-in page asked for the caches to be cleared').toBe(204);
    expect((await r.allHeaders())['clear-site-data'], 'with Clear-Site-Data').toBe('"cache"');
    await expect.poll(() => cached(home), { message: 'the HTTP cache no longer holds the ended session\'s page' }).toBe('not cached');

    // Back twice, past the icon, to the ended session's page: not restored.
    await page.goBack({ waitUntil: 'commit' });
    await page.goBack({ waitUntil: 'commit' });
    await expect.poll(kept, { message: 'the ended session\'s page was not restored from the back/forward cache' }).toBe('reloaded');
  } finally {
    await ctx.close();
  }
});
