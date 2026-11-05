import { Page } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { dsn, repoRoot } from '../lib/env';

// THE SWITCHER ROW FADES AT AN EDGE THAT HAS MORE (internal/ui/switcher.go,
// switcherFadeJS; an operator report: the bar's last pill read "mango:1 ·
// lap", cut mid-word by the row's edge, which looked broken). Operator
// decision: the row keeps scrolling sideways with every title whole — so the
// side with more to scroll to fades out instead. Pinned here, on the shared
// server at a phone's width with touch:
//   - a row with more on its right: the right edge fades, the left does not;
//   - scrolled to its end: the left edge fades, the right does not;
//   - the same row when everything fits (a very wide window): no fade at all;
//   - and the scroll offset is the reader's — the fade never moves it.
//
// The sessions are this spec's own agents (e2e/pluginfix agent + label), with
// long multi-word labels so the Recent row cannot hold them at 412px; they are
// removed afterwards. Every value is invented fixture data.

test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true });

const stamp = Date.now().toString(36);
const OWNER = '1';
const LABELS = ['nightly billing reconcile · west', 'release notes drafting · laptop', 'fleet health sweep · tower'];
const names = LABELS.map((_, i) => `e2e-fade-${stamp}-${i}`);
const VIEWER = `e2e-fade-${stamp}-v`;

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.beforeAll(async () => {
  test.setTimeout(120_000);
  names.forEach((n, i) => fix('label', fix('agent', OWNER, n, `e2e-fade-token-${stamp}-${i}`), LABELS[i]));
  fix('agent', OWNER, VIEWER, `e2e-fade-token-${stamp}-v`);
});

test.afterAll(async () => {
  for (const n of [...names, VIEWER]) fix('rmagent', OWNER, n);
});

async function openAgent(page: Page, name: string) {
  await page.goto('/agents/' + name);
  await page.waitForURL((u) => u.pathname === '/agents/' + name && u.searchParams.has('session'));
}

const row = (page: Page) => page.locator('#session-switcher [data-switcher-list="recent"]');

// edges reads the row's fade state: its attributes, the mask the stylesheet
// draws for them, and its scroll geometry.
const edges = (page: Page) =>
  row(page).evaluate((r) => ({
    start: r.hasAttribute('data-fade-start'),
    end: r.hasAttribute('data-fade-end'),
    mask: getComputedStyle(r).maskImage || getComputedStyle(r).getPropertyValue('-webkit-mask-image'),
    left: r.scrollLeft,
    max: r.scrollWidth - r.clientWidth,
  }));

test('the switcher row fades only at an edge with more to scroll to', async ({ page }) => {
  for (const n of names) await openAgent(page, n);
  // The Recent view, whatever an earlier spec stored as this browser's view.
  await page.addInitScript(() => {
    try {
      localStorage.removeItem('muster.switcher.view');
    } catch (e) {}
  });
  await openAgent(page, VIEWER);
  await expect(row(page)).toBeVisible();
  for (const l of LABELS) await expect(row(page).locator(`[data-switcher-entry][title="${l}"]`)).toHaveCount(1);

  // More on the right (the positive control: the row really overflows).
  const a = await edges(page);
  expect(a.max, 'the Recent row holds more than fits at 412px').toBeGreaterThan(1);
  expect(a.left).toBe(0);
  expect.soft(a.end, 'more on the right: the right edge fades').toBe(true);
  expect.soft(a.start, 'at the start: the left edge does not').toBe(false);
  expect.soft(a.mask, 'the stylesheet masks the row').not.toBe('none');
  const dir = process.env.MUSTER_E2E_CHIEF_SHOTS;
  if (dir) await page.screenshot({ path: path.join(dir, 'chief-dock-fade.png') });

  // Scrolled to the end: the left fades, the right does not; the offset is
  // the reader's (the fade only reads it).
  await row(page).evaluate((r) => r.scrollTo({ left: r.scrollWidth }));
  await expect.poll(async () => (await edges(page)).start, 'scrolled: the left edge fades').toBe(true);
  const b = await edges(page);
  expect.soft(b.end, 'at the end: the right edge does not fade').toBe(false);
  expect.soft(Math.abs(b.left - b.max), 'the row stays where it was scrolled to').toBeLessThanOrEqual(1);
  await page.waitForTimeout(300);
  expect.soft(Math.abs((await edges(page)).left - b.max), 'and is not moved afterwards').toBeLessThanOrEqual(1);

  // Everything fits (a window far wider than the row's pills): no fade at all.
  await page.setViewportSize({ width: 4000, height: 915 });
  await expect.poll(async () => (await edges(page)).max, 'the row fits').toBeLessThanOrEqual(1);
  await expect.poll(async () => { const e = await edges(page); return [e.start, e.end, e.mask]; }, 'no fade when it fits').toEqual([false, false, 'none']);
});
