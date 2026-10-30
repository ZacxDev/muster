import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { dsn, layoutDispatchPort, onboardingPort, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// A brand-new install's home: no agent, no mirrored host, so home has nothing
// to open and GET /agents draws the onboarding page (internal/api/nav_agents.go
// handleAgentsHome, internal/ui/onboarding.go — the retired Agents list's empty
// state). It must say how to connect a machine — the tmux one-liner, the exact
// one-prompt, the guide's link — not draw a blank page, and it must fit a phone.
//
// Against a REAL muster-server on a FRESH database (e2e/emptydb), because the
// suite's seeded database holds agents. The control is the suite's own server
// on the seeded database: the same URL there opens a session and draws no
// empty state, so the assertion below is about emptiness, not about the page.
//
// Two empty servers: one with no agent runtime (it says dispatch is not set
// up), and one with the noop provisioner (it offers "Dispatch an agent", which
// a phone reaches without opening the drawer). The second borrows the layout
// spec's dispatching port: the suite runs one spec at a time, and each stops
// its server in afterAll.

const PROMPT =
  'Set up muster for me: read https://raw.githubusercontent.com/ZacxDev/muster/main/INSTALL.md ' +
  'and follow it step by step. Ask me everything you need in one go up front, then do the rest yourself.';

const origin = `http://localhost:${onboardingPort}`;
const dorigin = `http://localhost:${layoutDispatchPort}`;
let server: Running | undefined;
let dserver: Running | undefined;

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  const emptyDSN = execFileSync('go', ['run', './e2e/emptydb'], {
    cwd: repoRoot,
    env: { ...process.env, DATABASE_URL: dsn },
    encoding: 'utf8',
  }).trim();
  expect(emptyDSN).toContain('_empty_e2e');
  server = await startServer(versionA, onboardingPort, { DATABASE_URL: emptyDSN, MUSTER_MIRROR: '1' });
  dserver = await startServer(versionA, layoutDispatchPort, {
    DATABASE_URL: emptyDSN,
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
});
test.afterAll(async () => {
  await server?.stop();
  await dserver?.stop();
});

async function login(page: Page, at: string) {
  await signIn(page, at);
}

for (const vp of [
  { name: 'phone', width: 390, height: 844 },
  { name: 'desktop', width: 1280, height: 800 },
]) {
  test(`an empty install's home says how to connect a machine (${vp.name} ${vp.width}x${vp.height})`, async ({ page }) => {
    await page.setViewportSize({ width: vp.width, height: vp.height });
    await login(page, origin);
    // Home, as a sign-in lands: `/` has nothing to open and goes to /agents,
    // which draws the onboarding page rather than redirecting again.
    await page.goto(origin + '/');
    expect(new URL(page.url()).pathname, '/ with nothing to open').toBe('/agents');

    const empty = page.locator('main #agents-empty');
    await expect(empty).toBeVisible();
    await expect(empty).toContainText('No agents yet');
    await expect(empty.locator('#onboarding-prompt')).toHaveText(PROMPT);
    await expect(empty).toContainText('tmux new -A -s work claude');
    await expect(empty.locator('a[href="https://github.com/ZacxDev/muster/blob/main/INSTALL.md"]')).toBeVisible();
    // No agent runtime: no dispatch button, and the hint that says why.
    await expect(page.locator('#home-deploy')).toHaveCount(0);
    await expect(empty.locator('[data-dispatch-off-hint]')).toBeVisible();
    // It is drawn inside the navigation, whose list is empty too.
    await expect(page.locator('#sidebar')).toHaveCount(1);
    if (vp.width < 1024) await page.locator('#sidebar-open').click();
    await expect(page.locator('#sidebar-agents-list [data-nav-agents-empty]')).toBeVisible();
    await expect(page.locator('#sidebar-agents-list a[data-nav-agent]')).toHaveCount(0);
    if (vp.width < 1024) await page.keyboard.press('Escape');

    // It fits: the page does not scroll sideways, and the prompt wraps inside
    // the viewport rather than running off it.
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(overflow, 'horizontal page overflow in px').toBeLessThanOrEqual(0);
    const box = await empty.locator('#onboarding-prompt').boundingBox();
    expect(box, 'the prompt is laid out').not.toBeNull();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(vp.width);
  });

  test(`an empty install with dispatch configured offers "Dispatch an agent" on its home (${vp.name} ${vp.width}x${vp.height})`, async ({ page }) => {
    await page.setViewportSize({ width: vp.width, height: vp.height });
    await login(page, dorigin);
    await page.goto(dorigin + '/');
    expect(new URL(page.url()).pathname).toBe('/agents');
    await expect(page.locator('#agents-empty')).toBeVisible();
    await expect(page.locator('#agents-empty [data-dispatch-off-hint]')).toHaveCount(0);
    const btn = page.locator('#home-deploy');
    await expect(btn).toBeVisible();
    await expect(btn).toHaveText('Dispatch an agent');
    // Reachable without opening the drawer, and it opens the dispatch sheet.
    await btn.click();
    await expect(page.locator('#agent-modal form')).toBeVisible();
  });
}

test('the control: the seeded server opens a session at the same URL and draws no empty state', async ({ page, baseURL }) => {
  await login(page, baseURL!);
  await page.goto(baseURL! + '/agents');
  expect(new URL(page.url()).pathname, '/agents with agents to open').not.toBe('/agents');
  expect(await (await page.request.get(baseURL! + '/ui/nav/agents')).text(), 'the sidebar lists entries').toContain('data-nav-agent=');
  await expect(page.locator('#agents-empty')).toHaveCount(0);
});
