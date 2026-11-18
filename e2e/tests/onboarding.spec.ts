import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { dsn, layoutDispatchPort, onboardingPort, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// A brand-new install's home: no agent, no mirrored host, so home has nothing
// to open and GET /agents draws the onboarding page (internal/api/nav_agents.go
// handleAgentsHome, internal/ui/onboarding.go — the retired Agents list's empty
// state). It must say how to connect a machine — plain steps, the exact
// one-prompt and the tmux one-liner each behind an ⓘ with Copy, the guide's
// link — not draw a blank page, and it must fit a phone.
//
// Against a REAL muster-server on a FRESH database (e2e/emptydb), because the
// suite's seeded database holds agents. The control is the suite's own server
// on the seeded database: the same URL there opens a session and draws no
// empty state, so the assertion below is about emptiness, not about the page.
//
// Two empty servers: one with no agent runtime, and one with the noop
// provisioner. Neither offers a dispatch (docs/design/chief-dispatch.md phase 2
// deleted "Dispatch an agent" and the "not set up" hint with the form). The
// second borrows the layout spec's dispatching port: the suite runs one spec at
// a time, and each stops its server in afterAll.

const PROMPT =
  'Set up muster for me. Download its install guide with ' +
  '`curl -fsSL -o muster-install.md https://github.com/ZacxDev/muster/releases/latest/download/INSTALL.md`, ' +
  'read that whole file and follow it step by step. ' +
  'If that download fails, stop and tell me what it printed; never work from another copy of the guide, ' +
  'such as the one on the main branch, or from memory. ' +
  'Ask me everything you need in one go up front, then do the rest yourself. ' +
  "Where the guide and the machine disagree, trust `muster doctor`, each command's `--help` and the code at " +
  'https://github.com/ZacxDev/muster over memory.';

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
    // Plain steps up front; the prompt and the command are each in an ⓘ
    // sheet (closed until tapped), never in the top layer.
    await expect(empty).toContainText('Connect your first computer');
    await expect(empty).toContainText('Start an agent on that computer');
    await expect(empty.locator('#onboarding-prompt [data-info-code]')).toHaveText(PROMPT);
    await expect(empty.locator('#onboarding-start [data-info-code]')).toHaveText('tmux new -A -s work claude');
    await expect(empty.locator('#onboarding-prompt')).toBeHidden();
    await expect(empty.locator('#onboarding-start')).toBeHidden();
    await expect(empty.locator('#onboarding-copy-prompt')).toHaveAttribute('data-copy-text', PROMPT);
    await expect(empty.locator('a[href="https://github.com/ZacxDev/muster/blob/main/INSTALL.md"]')).toHaveText('install guide');
    // No dispatch button and no "not set up" hint: the chief starts agents
    // (docs/design/chief-dispatch.md phase 2 deleted both).
    await expect(page.locator('#home-deploy')).toHaveCount(0);
    await expect(page.locator('[data-dispatch-off-hint]')).toHaveCount(0);
    // It is drawn inside the navigation, whose list is empty too.
    await expect(page.locator('#sidebar')).toHaveCount(1);
    if (vp.width < 1024) await page.locator('#sidebar-open').click();
    await expect(page.locator('#sidebar-agents-list [data-nav-agents-empty]')).toBeVisible();
    await expect(page.locator('#sidebar-agents-list a[data-nav-agent]')).toHaveCount(0);
    if (vp.width < 1024) await page.keyboard.press('Escape');

    // It fits: the page does not scroll sideways, and the steps and the
    // prompt's sheet, once open, lie inside the viewport.
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(overflow, 'horizontal page overflow in px').toBeLessThanOrEqual(0);
    for (const sel of ['#onboarding-copy-prompt', '#onboarding-prompt-open', '#onboarding-start-open']) {
      const b = await empty.locator(sel).boundingBox();
      expect(b, `${sel} is laid out`).not.toBeNull();
      expect(b!.x, sel).toBeGreaterThanOrEqual(-0.5);
      expect(b!.x + b!.width, sel).toBeLessThanOrEqual(vp.width + 0.5);
    }
    await empty.locator('#onboarding-prompt-open').click();
    const box = await empty.locator('#onboarding-prompt').boundingBox();
    expect(box, 'the prompt sheet is laid out').not.toBeNull();
    expect(box!.x).toBeGreaterThanOrEqual(-0.5);
    expect(box!.x + box!.width).toBeLessThanOrEqual(vp.width + 0.5);
    await page.keyboard.press('Escape');
  });

  test(`an empty install with an agent provisioner offers no "Dispatch an agent" on its home either (${vp.name} ${vp.width}x${vp.height})`, async ({ page }) => {
    await page.setViewportSize({ width: vp.width, height: vp.height });
    await login(page, dorigin);
    await page.goto(dorigin + '/');
    expect(new URL(page.url()).pathname).toBe('/agents');
    await expect(page.locator('#agents-empty')).toBeVisible();
    await expect(page.locator('#home-deploy')).toHaveCount(0);
    await expect(page.locator('[data-dispatch-off-hint]')).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Dispatch an agent' })).toHaveCount(0);
  });
}

test('the control: the seeded server opens a session at the same URL and draws no empty state', async ({ page, baseURL }) => {
  await login(page, baseURL!);
  await page.goto(baseURL! + '/agents');
  expect(new URL(page.url()).pathname, '/agents with agents to open').not.toBe('/agents');
  expect(await (await page.request.get(baseURL! + '/ui/nav/agents')).text(), 'the sidebar lists entries').toContain('data-nav-agent=');
  await expect(page.locator('#agents-empty')).toHaveCount(0);
});

// THE FIRST-RUN SCREEN IN PLAIN WORDS (operator rule: no command, URL, flag or
// file name in muster's top layer), on a phone: the admin's first screen says
// what to do in steps — nothing visible spells "tmux", "curl", a URL or
// "INSTALL.md" — and each step's ⓘ (a 44px button, named for a screen reader)
// opens on a tap and on Enter, its sheet holding the exact prompt or command,
// which Copy puts on the clipboard. The step's own "Copy setup instructions"
// copies the prompt without opening anything.
//
// 🔴 REGRESSION TEST for the jargon audit's F101/F102/F103/F104: red at
// origin/main, whose first screen printed the tmux command and the whole curl
// prompt and had no ⓘ. With MUSTER_E2E_SHOTS_DIR set it saves the phone
// screenshots there (never in CI).
test.describe('phone 390×844 touch: the first-run screen in plain words', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });
  const shots = process.env.MUSTER_E2E_SHOTS_DIR ?? '';
  const shot = async (page: Page, name: string) => {
    if (shots) await page.screenshot({ path: path.join(shots, name + '.png') });
  };
  const jargon = /\btmux\b|\bcurl\b|https?:\/\/|\.md\b|\s--?[a-z]/i;

  test('no command in the top layer; each ⓘ opens on tap and on Enter and copies the exact text', async ({ page }) => {
    test.setTimeout(90_000);
    await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin });
    await login(page, origin);
    await page.goto(origin + '/');
    expect(new URL(page.url()).pathname).toBe('/agents');
    const empty = page.locator('main #agents-empty');
    await expect(empty).toBeVisible();
    await shot(page, 'first-run');
    // CONTROL: the scan reads the page's words (its heading is among them).
    const visible = await empty.innerText();
    expect(visible).toContain('No agents yet');
    expect(visible, 'nothing visible on the first-run screen spells a command, URL or file').not.toMatch(jargon);
    expect(visible).toContain('Connect your first computer');

    for (const c of [
      { id: 'onboarding-prompt', name: 'Show the setup instructions', heading: 'Setup instructions', code: PROMPT },
      { id: 'onboarding-start', name: 'How to start an agent', heading: 'Start an agent', code: 'tmux new -A -s work claude' },
    ]) {
      const info = page.locator(`#${c.id}-open`);
      await expect(info).toHaveAccessibleName(c.name);
      const ib = (await info.boundingBox())!;
      expect(ib.height, 'a 44px target').toBeGreaterThanOrEqual(44);
      expect(ib.x + ib.width, 'the ⓘ is on screen').toBeLessThanOrEqual(390.5);
      const sheet = page.locator('#' + c.id);
      await expect(sheet).toBeHidden();
      await info.tap();
      await expect(sheet).toBeVisible();
      await expect(sheet).toHaveAttribute('role', 'dialog');
      await expect(sheet).toHaveAccessibleName(c.heading);
      await expect(sheet.locator('[data-info-code]')).toHaveText(c.code);
      const sb = (await sheet.boundingBox())!;
      expect(sb.x >= -0.5 && sb.x + sb.width <= 390.5, 'the sheet fits the phone').toBe(true);
      await shot(page, `first-run-${c.id}`);
      const copy = sheet.locator('[data-info-copy]');
      await copy.tap();
      await expect(copy).toHaveText('Copied');
      expect(await page.evaluate(() => navigator.clipboard.readText()), 'Copy puts the exact text on the clipboard').toBe(c.code);
      await page.keyboard.press('Escape');
      await expect(sheet).toBeHidden();
      // By keyboard: the ⓘ is a button — Enter opens it, Escape closes it.
      await info.focus();
      await page.keyboard.press('Enter');
      await expect(sheet).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(sheet).toBeHidden();
    }

    // The step's own Copy: the prompt, without opening the sheet.
    await page.evaluate(() => navigator.clipboard.writeText('stale'));
    const direct = empty.locator('#onboarding-copy-prompt');
    await direct.tap();
    await expect(direct).toHaveText('Copied');
    expect(await page.evaluate(() => navigator.clipboard.readText()), 'Copy setup instructions copies the exact prompt').toBe(PROMPT);
    await expect(page.locator('#onboarding-prompt')).toBeHidden();
  });
});
