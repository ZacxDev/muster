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
// A cached session is PROBED before it is trusted: GET /login answers a redirect
// to a signed-in caller and the form to anyone else (internal/api login.go
// handleLoginPage). So a server restarted under the same port with another
// secret, or a session the server no longer accepts, is signed in again rather
// than handed on. A restarted WORKER (Playwright starts a new one after a
// failure) starts with an empty cache and signs in again on its first call.
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
const key = (origin: string, pw: string) => `${origin}\n${pw}`;

// signedIn reports whether the context's cookies are a session this server
// accepts. maxRedirects 0: the redirect itself is the answer.
async function signedIn(page: Page, origin: string): Promise<boolean> {
  const r = await page.context().request.get(origin + '/login', { maxRedirects: 0 });
  return r.status() >= 300 && r.status() < 400;
}

// land leaves the page where the form's sign-in leaves it: the origin's home
// (the form redirects to /, with no ?next=). 🔴 EVERY PATH ENDS THERE, so a
// test cannot pass only because it happens to be its file's first: one that
// used whatever page signing in left it on worked after the form and timed out
// after a reused session, which left the page blank (claude-code-polish's
// desktop 1a, the first run of this helper).
async function land(page: Page, origin: string): Promise<void> {
  await page.goto(origin + '/');
}

export interface SignInOptions {
  // The server's operator password, when it is not the suite's.
  password?: string;
  // Drive the form even when a session is cached: the caller needs one of its own.
  fresh?: boolean;
}

export async function signIn(page: Page, origin: string = baseURL, opts: SignInOptions = {}): Promise<void> {
  const pw = opts.password ?? suitePassword;
  const k = key(origin, pw);
  if (!opts.fresh) {
    let cached = sessions.get(k);
    // The shared server's session is the one global-setup saved: a worker
    // never needs the form for it.
    if (!cached && origin === baseURL && pw === suitePassword && fs.existsSync(authState)) {
      cached = (JSON.parse(fs.readFileSync(authState, 'utf8')) as { cookies: Cookies }).cookies;
    }
    if (cached) {
      await page.context().addCookies(cached);
      if (await signedIn(page, origin)) {
        sessions.set(k, cached);
        return land(page, origin);
      }
      sessions.delete(k);
    } else if (await signedIn(page, origin)) {
      // Already signed in (the context's own cookies: a session is signed with
      // a key derived from the password, so the suite's session from another
      // of its servers is good here too). /login would only redirect.
      sessions.set(k, await page.context().cookies(origin));
      return land(page, origin);
    }
  } else {
    // A session the context already holds would send /login straight on.
    await page.context().clearCookies({ name: sessionCookie });
  }
  await page.goto(origin + '/login');
  await page.fill('#muster-password', pw);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  noteFormSignIn(origin);
  sessions.set(k, await page.context().cookies(origin));
}
