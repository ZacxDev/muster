import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { baseURL, hookToken } from '../lib/env';

// THE OFFICERS PAGES (officers redesign O-C), on the shared server — which has
// a database, so the built-in officer store is on, and no Kubernetes agent
// plane, so the wizard must say pods are not available and why.
//
// At a desktop (1280×800) and a phone (390×844) each, the person: opens
// Officers from the nav, starts the New-officer wizard, steps through it
// (start from → name and instructions → where it runs → when it works → what
// it may do → review), creates it in Shadow,
// finds it in the list, opens its page, pauses and resumes it, and deletes it,
// the list then saying so. And a host officer whose machine this server does
// not see (it has no mirror hub) keeps that machine through an Edit saved in
// the browser — red before round 1, where the host choice was drawn disabled
// and the save moved it to auto. The viewport is set here, per size, never
// inherited from the config.

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
      // 1. Start from (officers O-D: the template catalogue): Blank chosen,
      // the no-JS button removed by the script. Next with Blank still chosen
      // goes on without redrawing the form.
      await expect(step(1)).toBeVisible();
      await expect(step(2)).toBeHidden();
      await expect(page.locator('#wizard-progress li').first()).toHaveText('1. Start from');
      await expect(step(1).locator('input[name=template][value=blank]')).toBeChecked();
      await expect(page.locator('#officer-template-pick')).toHaveCount(0);
      await next.click();
      // 2. Name and instructions: Next refuses an empty name.
      await expect(step(2)).toBeVisible();
      await expect(step(3)).toBeHidden();
      await expect(page.locator('#officer-name')).toHaveValue('');
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
      await expect(page.locator('#officer-notice')).toBeVisible();
      await expect(page.locator(`[data-officer="${id}"]`)).toHaveCount(0);
    });

    test('an Edit keeps a host officer on a machine this server does not see', async ({ page }) => {
      const hid = `e2e-hostkeep-${size.name}:main`;
      await removeOfficer(hid);
      try {
        const made = await fetch(`${baseURL}/api/officers/declared`, {
          method: 'POST', headers: { Authorization: `Bearer ${hookToken}`, 'Content-Type': 'application/json' },
          body: JSON.stringify({ id: hid, name: 'E2E host keeper', charter: 'Summarise the queue. Never post.', runtime: 'host', host: 'e2e-laptop', mode: 'shadow' }),
        });
        expect(made.status, 'the host officer is declared').toBe(201);
        await page.goto(`/officers/${hid}/edit`);
        const hostChoice = page.locator('input[name=runtime][value=host]');
        await expect(hostChoice).toBeChecked();
        await expect(hostChoice).toBeEnabled();
        await expect(page.locator('#officer-host')).toHaveValue('e2e-laptop');
        await noSidewaysScroll(page);
        await page.fill('#officer-charter', 'Summarise the queue, then stop. Never post.');
        await page.click('#officer-submit');
        await page.waitForURL((u) => u.pathname === `/officers/${hid}` && u.searchParams.get('notice') === 'saved');
        const got = await (await fetch(`${baseURL}/api/officers/declared/${encodeURIComponent(hid)}`, { headers: { Authorization: `Bearer ${hookToken}` } })).json();
        expect(got.runtime, 'the saved runtime').toBe('host');
        expect(got.host, 'the saved machine').toBe('e2e-laptop');
        expect(got.mode, 'the mode is not changed by the save').toBe('shadow');
      } finally {
        await removeOfficer(hid);
      }
    });
  });
}

// THE TEMPLATES (officers O-D), at a desktop and a phone: choosing one on
// "Start from" and pressing Next redraws the wizard at "Name and
// instructions" with that template's starting values — which the person can
// still change — and nothing is created by choosing.
const templates = [
  { key: 'session-minder', name: 'Session minder', charter: 'session.stalled', wake: 'manual', preset: null, cap: 'session_minder' },
  { key: 'marketing', name: 'Marketing', charter: 'Your venue is manual', wake: 'schedule', preset: 'weekly', cap: null },
  { key: 'qa', name: 'QA', charter: 'You test; you never fix.', wake: 'schedule', preset: 'weekdays', cap: null },
];
for (const size of sizes) {
  test.describe(`the officer templates at ${size.name} size`, () => {
    test.use({ viewport: size.viewport });
    for (const tpl of templates) {
      test(`starting from ${tpl.key} fills in the wizard`, async ({ page }) => {
        await page.goto('/officers/new');
        const step = (n: number) => page.locator(`fieldset[data-step="${n}"]`);
        await expect(step(1)).toBeVisible();
        await step(1).locator(`input[name=template][value="${tpl.key}"]`).check();
        await page.locator('[data-wizard-next]').click();
        // The form is redrawn (a POST answering 200) at step 2.
        await expect(step(2)).toBeVisible();
        await expect(page.locator('#officer-form')).toHaveAttribute('data-template', tpl.key);
        await expect(page.locator('#officer-name')).toHaveValue(tpl.name);
        await expect(page.locator('#officer-charter')).toHaveValue(new RegExp(tpl.charter.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
        await expect(page.locator(`input[name=wake][value=${tpl.wake}]`)).toBeChecked();
        if (tpl.preset) await expect(page.locator('#officer-preset')).toHaveValue(tpl.preset);
        await expect(page.locator('input[name=cap]:checked')).toHaveCount(tpl.cap ? 1 : 0);
        if (tpl.cap) await expect(page.locator(`input[name=cap][value=${tpl.cap}]`)).toBeChecked();
        await expect(page.locator('#officer-repo')).toHaveValue('');
        await expect(page.locator('input[name=mode][value=shadow]')).toBeChecked();
        // Back shows the template chosen.
        await page.locator('[data-wizard-back]').click();
        await expect(step(1).locator(`input[name=template][value="${tpl.key}"]`)).toBeChecked();
        await noSidewaysScroll(page);
      });
    }
  });
}
