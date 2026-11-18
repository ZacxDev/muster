import { defineConfig, devices } from '@playwright/test';

// The local quickstart in real browsers: sign-in on http://localhost against
// the zero-config Compose stack, in Chromium AND WebKit (Safari's engine).
//
// NOT part of the main suite (its own directory, its own config, a file name
// that is not *.spec.ts, so e2e/legs.json never sees it): it needs the stack
// tests/compose-up.sh brings up, and it is run by that script with
// COMPOSE_UP_BROWSERS=1, which passes MUSTER_LOCAL_URL and
// MUSTER_LOCAL_PASSWORD. Run by hand the same way:
//
//   MUSTER_LOCAL_URL=http://localhost:8105 MUSTER_LOCAL_PASSWORD=… \
//     npx --prefix e2e playwright test -c e2e/localhost/playwright.config.ts
//
// (after `npx --prefix e2e playwright install chromium webkit`).
export default defineConfig({
  testDir: '.',
  testMatch: /signin\.local\.ts$/,
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 60_000,
  reporter: [['list']],
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    { name: 'webkit', use: { ...devices['Desktop Safari'] } },
  ],
});
