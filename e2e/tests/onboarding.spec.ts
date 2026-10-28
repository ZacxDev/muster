import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, onboardingPort, password, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// A brand-new install's Agents list: no agent, no mirrored host. It must say
// how to connect a machine — the tmux one-liner, the exact one-prompt, the
// guide's link — not draw a blank list, and it must fit a phone.
//
// Against a REAL muster-server on a FRESH database (e2e/emptydb), because the
// suite's seeded database holds agents. The control is the suite's own server
// on the seeded database: the same page there draws cards and no empty state,
// so the assertion below is about emptiness, not about the page.

const PROMPT =
  'Set up muster for me: read https://raw.githubusercontent.com/ZacxDev/muster/main/INSTALL.md ' +
  'and follow it step by step. Ask me everything you need in one go up front, then do the rest yourself.';

const origin = `http://localhost:${onboardingPort}`;
let server: Running | undefined;

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  const emptyDSN = execFileSync('go', ['run', './e2e/emptydb'], {
    cwd: repoRoot,
    env: { ...process.env, DATABASE_URL: dsn },
    encoding: 'utf8',
  }).trim();
  expect(emptyDSN).toContain('_empty_e2e');
  server = await startServer(versionA, onboardingPort, { DATABASE_URL: emptyDSN, MUSTER_MIRROR: '1' });
});
test.afterAll(async () => {
  await server?.stop();
});

async function login(page: Page, at: string) {
  await page.goto(at + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

for (const vp of [
  { name: 'phone', width: 390, height: 844 },
  { name: 'desktop', width: 1280, height: 800 },
]) {
  test(`an empty Agents list says how to connect a machine (${vp.name} ${vp.width}x${vp.height})`, async ({ page }) => {
    await page.setViewportSize({ width: vp.width, height: vp.height });
    await login(page, origin);
    await page.goto(origin + '/agents');

    const empty = page.locator('#agents-list #agents-empty');
    await expect(empty).toBeVisible();
    await expect(empty).toContainText('No agents yet');
    await expect(empty.locator('#onboarding-prompt')).toHaveText(PROMPT);
    await expect(empty).toContainText('tmux new -A -s work claude');
    await expect(empty.locator('a[href="https://github.com/ZacxDev/muster/blob/main/INSTALL.md"]')).toBeVisible();
    // No card was drawn: this IS the empty list, not an empty state beside cards.
    await expect(page.locator('#agents-list > article')).toHaveCount(0);

    // It fits: the page does not scroll sideways, and the prompt wraps inside
    // the viewport rather than running off it.
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(overflow, 'horizontal page overflow in px').toBeLessThanOrEqual(0);
    const box = await empty.locator('#onboarding-prompt').boundingBox();
    expect(box, 'the prompt is laid out').not.toBeNull();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(vp.width);
  });
}

test('the control: the seeded server draws cards and no empty state', async ({ page, baseURL }) => {
  await login(page, baseURL!);
  await page.goto(baseURL! + '/agents');
  await expect(page.locator('#agents-list > article').first()).toBeVisible();
  await expect(page.locator('#agents-empty')).toHaveCount(0);
});
