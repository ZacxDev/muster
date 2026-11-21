import { test, expect } from '@playwright/test';
import http from 'node:http';
import type { AddressInfo } from 'node:net';

// Sign-in on http://localhost with the Compose stack's default cookie policy
// (MUSTER_SECURE_COOKIES=1, with the loopback-page exception in
// internal/api passwordCookieSecure), in each browser project.

const url = process.env.MUSTER_LOCAL_URL ?? '';
const password = process.env.MUSTER_LOCAL_PASSWORD ?? '';

// 🔴 A MISSING STACK FAILS, it does not skip: a skipped browser test is a green
// run that checked nothing.
test.beforeAll(() => {
  if (!url || !password) throw new Error('MUSTER_LOCAL_URL and MUSTER_LOCAL_PASSWORD must be set (tests/compose-up.sh sets them)');
  if (!/^http:\/\/localhost:\d+$/.test(url)) throw new Error(`MUSTER_LOCAL_URL must be http://localhost:<port>, got ${url}`);
});

// THE MEASUREMENT this policy rests on, recorded every run: does this browser
// keep a Secure cookie set over plain http://localhost? A bare server, no
// muster. It is reported, not asserted — the sign-in below must work either
// way — and the line names the browser so the CI log answers the question.
test('measure: a Secure cookie set over http://localhost', async ({ page, browserName }) => {
  const srv = http.createServer((req, res) => {
    if (req.url === '/set') {
      res.setHeader('Set-Cookie', ['probe_secure=1; Path=/; Secure', 'probe_plain=1; Path=/']);
      res.end('set');
      return;
    }
    res.end(`cookie=[${req.headers.cookie ?? ''}]`);
  });
  await new Promise<void>((r) => srv.listen(0, '127.0.0.1', () => r()));
  const port = (srv.address() as AddressInfo).port;
  try {
    await page.goto(`http://localhost:${port}/set`);
    await page.goto(`http://localhost:${port}/echo`);
    const body = (await page.textContent('body')) ?? '';
    // The plain cookie is the positive control: it must come back.
    expect(body, 'positive control: a plain cookie did not round-trip').toContain('probe_plain=1');
    const kept = body.includes('probe_secure=1');
    console.log(`MEASURED [${browserName}]: a Secure cookie set over http://localhost is ${kept ? 'KEPT' : 'DROPPED'}`);
    test.info().annotations.push({ type: 'measured', description: `${browserName}: Secure cookie on http://localhost ${kept ? 'kept' : 'dropped'}` });
  } finally {
    srv.close();
  }
});

// The session-tier route the check reads: JSON, and 401 without a session, so
// "the browser sent its cookie back" is one status code (the board page is a
// whole app, which is not what this test is about).
const signedInProbe = '/api/push/vapid-public-key';

test('sign in on http://localhost and stay signed in', async ({ page, context }) => {
  await page.goto(`${url}/login`);
  await page.locator('#muster-password').fill(password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page).not.toHaveURL(/\/login/);
  const session = (await context.cookies(url)).find((c) => c.name === 'muster_session');
  expect(session, 'no session cookie was kept').toBeTruthy();
  // The cookie is SENT back by the browser itself, on a fresh navigation.
  const res = await page.goto(`${url}${signedInProbe}`);
  expect(page.url(), 'the browser did not send its session cookie back (sent to sign in)').toBe(`${url}${signedInProbe}`);
  expect(res?.status()).toBe(200);
  expect(await res?.text()).toContain('"key"');
});

// NEGATIVE CONTROL: a wrong password stays on the sign-in page, so the test
// above cannot pass by landing anywhere at all.
test('a wrong password does not sign in', async ({ page }) => {
  await page.goto(`${url}/login`);
  await page.locator('#muster-password').fill(`not-${password}`);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByText('That password is not right.')).toBeVisible();
  // The same probe as above, from a browser that holds no session: sent to
  // sign in (a navigation without a session is redirected to /login).
  await page.goto(`${url}${signedInProbe}`);
  await expect(page).toHaveURL(/\/login/);
});
