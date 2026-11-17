import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { baseURL, hookToken } from '../lib/env';

// THE OFFICERS PAGES (officers redesign O-C), on the shared server — which has
// a database, so the built-in officer store is on, and no Kubernetes agent
// plane, so the wizard must say pods are not available and why.
//
// At a desktop (1280×800) and a phone (390×844) each, the person: opens
// Officers from the nav, starts the New-officer wizard, steps through it
// (Blank → name and instructions → where it runs → when it works → what it may
// do → review), creates it in Shadow, finds it in the list, opens its page,
// pauses and resumes it, and deletes it. The viewport is set here, per size,
// never inherited from the config.

const sizes = [
  { name: 'desktop', viewport: { width: 1280, height: 800 } },
  { name: 'phone', viewport: { width: 390, height: 844 } },
];

// removeOfficer deletes id through the admin API (the hook token acts as the
// operator, the signed-in user): leftovers of an earlier, failed run.
async function removeOfficer(id: string) {
  await fetch(`${baseURL}/api/officers/declared/${encodeURIComponent(id)}`, {
    method: 'DELETE', headers: { Authorization: `Bearer ${hookToken}` },
  });
}

async function noSidewaysScroll(page: Page) {
  const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(over, 'the page scrolls sideways').toBeLessThanOrEqual(0);
}

for (const size of sizes) {
  test.describe(`the officers pages at ${size.name} size`, () => {
    test.use({ viewport: size.viewport });
    const name = `E2E scout ${size.name}`;
    const id = `e2e-scout-${size.name}:main`;
    test.beforeEach(async () => { await removeOfficer(id); });
    test.afterEach(async () => { await removeOfficer(id); });

    test('create one in the wizard (Shadow), list it, pause and resume it, delete it', async ({ page }) => {
      // The nav entry, inside More.
      await page.goto('/officers');
      await expect(page.locator('#officer-new')).toBeVisible();
      await expect(page.locator('#officers-runtimes [data-runtime="pod"]')).toHaveAttribute('data-available', 'false');
      await noSidewaysScroll(page);
      await expect(page.locator('#sidebar-officers')).toHaveAttribute('aria-current', 'page');

      await page.click('#officer-new');
      await page.waitForURL(/\/officers\/new$/);
      const step = (n: number) => page.locator(`fieldset[data-step="${n}"]`);
      const next = page.locator('[data-wizard-next]');
      // 1. Start from: Blank, the one template.
      await expect(step(1)).toBeVisible();
      await expect(step(2)).toBeHidden();
      await expect(step(1).locator('input[name=template][value=blank]')).toBeChecked();
      await next.click();
      // 2. Name and instructions: Next refuses an empty name.
      await expect(step(2)).toBeVisible();
      await next.click();
      await expect(step(2)).toBeVisible();
      await page.fill('#officer-name', name);
      await page.fill('#officer-charter', 'Read the queue and summarise each item. Never post anything.');
      await next.click();
      // 3. Where it runs: auto is the default, and the pod choice says why not.
      await expect(step(3)).toBeVisible();
      await expect(step(3).locator('input[name=runtime][value=auto]')).toBeChecked();
      await expect(step(3).locator('input[name=runtime][value=pod]')).toBeDisabled();
      await expect(page.locator('#runtime-pod-why')).toContainText('Not available here');
      await next.click();
      // 4. When it works: a choice is required (F5).
      await expect(step(4)).toBeVisible();
      await next.click();
      await expect(step(4)).toBeVisible();
      await step(4).locator('input[name=wake][value=manual]').check();
      await next.click();
      // 5. What it may do: nothing extra by default.
      await expect(step(5)).toBeVisible();
      await expect(step(5).locator('input[name=cap]:checked')).toHaveCount(0);
      await next.click();
      // 6. Review: the answers, and Start in Shadow chosen.
      await expect(step(6)).toBeVisible();
      await expect(page.locator('#wizard-review')).toContainText(name);
      await expect(page.locator('#wizard-review')).toContainText('Only when you give it work');
      await expect(step(6).locator('input[name=mode][value=shadow]')).toBeChecked();
      await noSidewaysScroll(page);
      await page.click('#officer-submit');

      // Its page.
      await page.waitForURL((u) => u.pathname === `/officers/${id}` && u.searchParams.get('notice') === 'created');
      await expect(page.locator('#officer-notice')).toBeVisible();
      await expect(page.locator('#officer-head h1')).toHaveText(name);
      await expect(page.locator('#officer-actions button', { hasText: 'Switch to Live' })).toBeVisible();
      await expect(page.locator('#officer-schedules-none')).toBeVisible();
      await noSidewaysScroll(page);

      // The list.
      await page.goto('/officers');
      const row = page.locator(`#officers-mine [data-officer="${id}"]`);
      await expect(row).toBeVisible();
      await expect(row.locator('a')).toHaveText(name);
      await row.locator('a').click();
      await page.waitForURL((u) => u.pathname === `/officers/${id}`);

      // Pause, then resume.
      await page.click('#officer-actions button:has-text("Pause")');
      await page.waitForURL((u) => u.searchParams.get('notice') === 'paused');
      await expect(page.locator('#officer-actions button', { hasText: 'Resume' })).toBeVisible();
      await page.click('#officer-actions button:has-text("Resume")');
      await page.waitForURL((u) => u.searchParams.get('notice') === 'resumed');
      await expect(page.locator('#officer-actions button', { hasText: 'Pause' })).toBeVisible();

      // Delete: only with its id typed.
      await page.fill('#officer-delete-confirm', id);
      await page.click('#officer-delete button[type=submit]');
      await page.waitForURL((u) => u.pathname === '/officers' && u.searchParams.get('notice') === 'deleted');
      await expect(page.locator(`[data-officer="${id}"]`)).toHaveCount(0);
    });
  });
}
