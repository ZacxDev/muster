// signIn: the suite's ONE way to sign a page's context in to a password-mode
// server through the sign-in form.
//
// 🔴 PASSWORD SIGN-IN IS THROTTLED: loginRateBurst attempts per loginRateWindow
// (10 a minute, internal/api/login.go), in ONE server-wide bucket. A spec that
// drove the form for every test crossed it — the 11th POST /login got a 429,
// the page stayed on /login, and waitForURL timed out at 60 s. Whether it did
// was a race on the runner's speed, so it passed locally and broke main's CI.
// So the form is driven ONCE per server per worker, and every later call hands
// the context the session cookies that sign-in earned.
//
// A cached session is handed on unchecked: the session key is derived from the
// password (internal/api auth.go), every server the suite starts uses the
// suite's, and password sessions last 30 days and cannot be revoked — so a
// server restarted on the same port still accepts it. With nothing cached the
// context's own cookies are checked first (GET /login redirects a signed-in
// caller, login.go handleLoginPage): a context that already holds the suite's
// session from another of its servers needs no form. A restarted WORKER
// (Playwright starts a new one after a failure) starts with an empty cache.
//
// ⚠ WHERE THE PAGE IS AFTERWARDS DEPENDS ON THE PATH: after the form it is on
// the origin's home (where the sign-in redirects), after a reused session it
// has not moved. So a caller navigates to what it tests. Landing every path on
// home was tried and reverted: home opens the default session's view, and its
// sockets broke five tests that count sockets and streams (mirror-windows,
// mirrored, view-swap, hermes-view).
//
// A test that needs a session of its OWN — one it signs out, or a sign-in whose
// issue time a step-up check reads — passes { fresh: true }. That still goes
// through here, so the budget (lib/server.ts signInBudget) counts it.
import type { BrowserContext, Page } from '@playwright/test';
import fs from 'node:fs';
import { authState, baseURL, password as suitePassword } from './env';
import { noteFormSignIn } from './server';

type Cookies = Awaited<ReturnType<BrowserContext['cookies']>>;

// The password-mode session cookie (internal/api auth.go sessionCookieName).
const sessionCookie = 'muster_session';
const sessions = new Map<string, Cookies>();

// signedIn reports whether the context's cookies are a session this server
// accepts. maxRedirects 0: the redirect itself is the answer.
async function signedIn(page: Page, origin: string): Promise<boolean> {
  const r = await page.context().request.get(origin + '/login', { maxRedirects: 0 });
  return r.status() >= 300 && r.status() < 400;
}

export interface SignInOptions {
  // Drive the form even when a session is cached: the caller needs one of its own.
  fresh?: boolean;
}

export async function signIn(page: Page, origin: string = baseURL, opts: SignInOptions = {}): Promise<void> {
  if (!opts.fresh) {
    let cached = sessions.get(origin);
    // The shared server's session is the one global-setup saved: a worker
    // never needs the form for it.
    if (!cached && origin === baseURL && fs.existsSync(authState)) {
      cached = (JSON.parse(fs.readFileSync(authState, 'utf8')) as { cookies: Cookies }).cookies;
    }
    if (cached) {
      await page.context().addCookies(cached);
      sessions.set(origin, cached);
      return;
    }
    if (await signedIn(page, origin)) {
      // Already signed in (the context's own cookies: a session is signed with
      // a key derived from the password, so the suite's session from another
      // of its servers is good here too). /login would only redirect.
      sessions.set(origin, await page.context().cookies(origin));
      return;
    }
  } else {
    // A session the context already holds would send /login straight on.
    await page.context().clearCookies({ name: sessionCookie });
  }
  await page.goto(origin + '/login');
  await page.fill('#muster-password', suitePassword);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  noteFormSignIn(origin);
  sessions.set(origin, await page.context().cookies(origin));
}
