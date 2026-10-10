import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { repoRoot } from '../lib/env';

// Generates the manifest's install-sheet screenshots from the SEEDED fixture
// board (no real data): 360×640 CSS px at 3× = 1080×1920, the size the
// manifest declares (TestManifestImagesShipAtTheirDeclaredSize checks the
// files). Run with `npm run capture`; never part of the test run.
//
// ⚠ ONE THING IS HIDDEN, AND IT IS SAID HERE: the push-permission status line.
// A headless browser has notifications blocked, so the page shows "Notifications
// are blocked…" — true of the harness, not of the app a person installs.
test.use({ viewport: { width: 360, height: 640 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true });

for (const [route, file, ready] of [
  ['/tasks', 'screenshot-tasks-narrow.png', '#tasks-list article'],
  ['/agents', 'screenshot-agents-narrow.png', '#agents-list article'],
] as const) {
  test(`capture ${file}`, async ({ page }) => {
    await page.goto(route);
    await page.addStyleTag({ content: '#push-status{display:none!important}' });
    await expect(page.locator(ready).first()).toBeVisible();
    await page.waitForTimeout(800);
    await page.screenshot({ path: path.join(repoRoot, 'web/static/icons', file) });
  });
}
