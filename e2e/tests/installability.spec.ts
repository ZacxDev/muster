import { chromium, BrowserContext, Page } from '@playwright/test';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { authState, baseURL, stateDir } from '../lib/env';

// Chrome's own installability verdict, read over CDP.
//
// 🔴 THE INSTRUMENT IS PROVEN TO GO RED FIRST. A zero from
// Page.getInstallabilityErrors is indistinguishable from a harness that cannot
// see installability at all (an incognito context reports `in-incognito`; a
// headless build without the web-app components may report nothing for
// anything). So each run first feeds Chrome two deliberately broken manifests
// and requires a non-zero answer, and only then believes the zero for the real
// one. The pair is printed.
//
// ⚠ A PERSISTENT (non-incognito) context, in full Chromium rather than the
// headless shell: Playwright's default contexts are off-the-record, and Chrome
// refuses installability there by design.

let context: BrowserContext;
let userDataDir: string;

test.beforeAll(async () => {
  if (fs.existsSync(`${stateDir}/skip`)) return; // the test itself skips, via the fixture
  userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-e2e-install-'));
  context = await chromium.launchPersistentContext(userDataDir, {
    channel: 'chromium',
    headless: true,
    baseURL,
    viewport: { width: 390, height: 844 },
  });
  const state = JSON.parse(fs.readFileSync(authState, 'utf8'));
  await context.addCookies(state.cookies);
});

test.afterAll(async () => {
  await context?.close();
  if (userDataDir) fs.rmSync(userDataDir, { recursive: true, force: true });
});

async function verdict(page: Page) {
  const cdp = await page.context().newCDPSession(page);
  const manifest = (await cdp.send('Page.getAppManifest')) as {
    errors: { message: string }[];
    manifest?: { id?: string; shareTarget?: { action: string; method: string }; shortcuts?: { url: string }[] };
  };
  const inst = (await cdp.send('Page.getInstallabilityErrors')) as { installabilityErrors: { errorId: string }[] };
  return { manifest, errors: inst.installabilityErrors.map((e) => e.errorId) };
}

const realManifest = async () => (await fetch(baseURL + '/manifest.webmanifest')).json();

test('Chrome finds the manifest valid and the app installable — and the check can fail', async () => {
  // Control 1: a manifest that does not parse.
  const p1 = await context.newPage();
  await p1.route('**/manifest.webmanifest', (r) => r.fulfill({ contentType: 'application/manifest+json', body: '{"name": "muster",' }));
  await p1.goto('/tasks');
  const broken = await verdict(p1);
  await p1.close();

  // Control 2: a valid manifest whose icons 404.
  const m = await realManifest();
  m.icons = m.icons.map((i: { src: string }) => ({ ...i, src: '/static/icons/definitely-missing.png' }));
  const p2 = await context.newPage();
  await p2.route('**/manifest.webmanifest', (r) => r.fulfill({ contentType: 'application/manifest+json', body: JSON.stringify(m) }));
  await p2.goto('/tasks');
  const iconless = await verdict(p2);
  await p2.close();

  // The real thing.
  const p3 = await context.newPage();
  await p3.goto('/tasks');
  const real = await verdict(p3);
  await p3.close();

  console.log(
    `installability pair: parse-broken manifest → ${broken.manifest.errors.length} parse error(s) / ${broken.errors.join(',') || 'none'}; ` +
      `icon-404 manifest → ${iconless.errors.join(',') || 'none'}; real → parse ${real.manifest.errors.length} / ${real.errors.join(',') || 'none'}`,
  );
  expect(broken.manifest.errors.length + broken.errors.length, 'control: a manifest that does not parse must be reported').toBeGreaterThan(0);
  expect(iconless.errors.length, 'control: a manifest whose icons 404 must be reported').toBeGreaterThan(0);

  expect(real.manifest.errors).toEqual([]);
  expect(real.errors).toEqual([]);
  expect(real.manifest.manifest?.shareTarget?.action).toMatch(/\/share$/);
  // CDP reports the method as its enum spelling ("kGet").
  expect((real.manifest.manifest?.shareTarget?.method ?? '').replace(/^k/, '').toUpperCase()).toBe('GET');
  expect((real.manifest.manifest?.shortcuts ?? []).map((s) => new URL(s.url).pathname + new URL(s.url).search)).toEqual(['/tasks?new=1', '/tasks', '/agents']);
});
