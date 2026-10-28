import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, repoRoot } from '../lib/env';
import { openMore } from '../lib/nav';

// THE CHIEF IS OPTIONAL AND OFF UNLESS YOU HAVE ONE (internal/api/chief_toggle.go),
// on the shared server — which, like the public `docker compose` install, has
// no agent provisioner — at a phone's size (390×844, touch) and a desktop's
// (1280×800):
//
//   1. a fresh user (no chief agent, no choice): `/` lands on the Agents list,
//      the sidebar has no chief row, and Account → Chief says the chief is not
//      available on this install, with no switch;
//   2. a user who already has a chief (this spec's own row, e2e/pluginfix):
//      it is on with no choice made — `/` opens it and the sidebar lists it
//      first; Account → Chief turns it OFF (`/` → the Agents list, no chief
//      row) and back ON (`/` → the chief again, first row again).
//
// 🔴 REGRESSION TESTS: on origin/main the Account page has no Chief section
// and POST /account/chief does not exist.
//
// The chief row and the switch's stored choice are deleted afterwards, so no
// later spec on the shared database sees a chief or a choice.

const stamp = Date.now().toString(36);
const CHIEF = `e2e-toggle-chief-${stamp}`;

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

const onAgents = /\/agents$/;
const onChief = new RegExp(`/agents/${CHIEF}(\\?|$)`);
const rows = (page: Page) => page.locator('#sidebar-agents-list a[data-nav-agent]');

// showSidebar puts the agent list on screen: the phone's drawer is opened (its
// list loads on each open); a desktop's rail is already there.
async function showSidebar(page: Page, phone: boolean) {
  if (phone) {
    await page.locator('#sidebar-open').click();
    await expect(page.locator('#sidebar-root')).toHaveAttribute('data-open', 'true');
  }
  await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });
}

// expectNoChiefRow: the list has loaded (at least one seeded agent row) and
// none of its rows is a chief, or this spec's chief.
async function expectNoChiefRow(page: Page) {
  await expect(rows(page).first()).toBeVisible();
  await expect(page.locator('#sidebar-agents-list a[data-nav-chief]')).toHaveCount(0);
  await expect(page.locator(`#sidebar-agents-list a[href^="/agents/${CHIEF}"]`)).toHaveCount(0);
}

async function expectChiefFirst(page: Page) {
  await expect(rows(page).first()).toHaveAttribute('href', new RegExp(`^/agents/${CHIEF}`));
  await expect(rows(page).first()).toHaveAttribute('data-nav-chief', '');
}

// openAccount reaches Account the way a person does: the sidebar's More.
async function openAccount(page: Page, phone: boolean) {
  await showSidebar(page, phone);
  await openMore(page);
  await page.locator('#sidebar-account').click();
  await expect(page).toHaveURL(/\/account$/);
}

const chiefState = (page: Page) => page.locator('#chief-state');

async function expectNoSidewaysScroll(page: Page) {
  const w = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth }));
  expect(w.sw, `the page is ${w.sw}px wide in a ${w.cw}px viewport`).toBeLessThanOrEqual(w.cw);
}

test.beforeAll(() => {
  test.setTimeout(120_000);
  fix('chiefpref', '1', 'clear');
});

test.afterAll(() => {
  fix('rmagent', '1', CHIEF);
  fix('chiefpref', '1', 'clear');
});

for (const [name, size, phone] of [
  ['phone', { width: 390, height: 844 }, true],
  ['desktop', { width: 1280, height: 800 }, false],
] as const) {
  test.describe(name, () => {
    test.use(phone ? { viewport: size, hasTouch: true, isMobile: true } : { viewport: size });

    test('a fresh user has no chief: home is the Agents list, no chief row, and Account says it is not available here', async ({ page }) => {
      fix('rmagent', '1', CHIEF);
      await page.goto('/');
      await expect(page, '`/` lands on the Agents list').toHaveURL(onAgents);
      await showSidebar(page, phone);
      await expectNoChiefRow(page);

      await page.goto('/account');
      const section = page.locator('[data-testid="chief-settings"]');
      await expect(section).toBeVisible();
      await expect(chiefState(page)).toHaveAttribute('data-state', 'unavailable');
      await expect(section).toContainText('Not available on this install');
      await expect(section.locator('#chief-toggle'), 'no switch that cannot work').toHaveCount(0);
      await expectNoSidewaysScroll(page);
    });

    test('an existing chief stays on; Account turns it off and on again', async ({ page }) => {
      fix('rmagent', '1', CHIEF);
      fix('chief', '1', CHIEF, `e2e-toggle-chief-token-not-a-real-one-${stamp}`);
      try {
        // On, with no choice made: home opens it, the sidebar lists it first.
        await page.goto('/');
        await expect(page, '`/` opens the existing chief').toHaveURL(onChief);
        await showSidebar(page, phone);
        await expectChiefFirst(page);

        // Off, from Account → Chief.
        await page.goto('/agents');
        await openAccount(page, phone);
        await expect(chiefState(page)).toHaveAttribute('data-state', 'on');
        await expect(page.locator('#chief-toggle')).toHaveText('Turn the chief off');
        const box = await page.locator('#chief-toggle').boundingBox();
        expect(box?.height ?? 0, 'the switch is a 44px target').toBeGreaterThanOrEqual(44);
        await expectNoSidewaysScroll(page);
        await page.locator('#chief-toggle').click();
        await expect(chiefState(page)).toHaveAttribute('data-state', 'off');
        await expect(page.locator('[data-testid="chief-settings"] [role="status"]')).toHaveText('Saved.');
        await expect(page.locator('#chief-toggle')).toHaveText('Turn the chief on');

        await page.goto('/');
        await expect(page, 'off: `/` lands on the Agents list').toHaveURL(onAgents);
        await showSidebar(page, phone);
        await expectNoChiefRow(page);

        // On again: home and the sidebar have it back.
        await page.goto('/account');
        await page.locator('#chief-toggle').click();
        await expect(chiefState(page)).toHaveAttribute('data-state', 'on');
        await page.goto('/');
        await expect(page, 'on again: `/` opens the chief').toHaveURL(onChief);
        await showSidebar(page, phone);
        await expectChiefFirst(page);
      } finally {
        fix('rmagent', '1', CHIEF);
        fix('chiefpref', '1', 'clear');
      }
    });
  });
}
