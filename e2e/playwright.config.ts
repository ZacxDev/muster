import { defineConfig, devices } from '@playwright/test';
import { baseURL } from './lib/env';

// One worker, in order: every spec shares one server and one seeded database,
// and several of them write to it (a share creates a task, the badge spec flips
// statuses). Parallel workers would race those writes.
//
// The `capture` project is the manifest-screenshot generator, not a test. It
// runs only when MUSTER_E2E_CAPTURE=1 (`npm run capture`), so a CI run never
// rewrites a committed image.
const capture = process.env.MUSTER_E2E_CAPTURE === '1';

export default defineConfig({
  testDir: '.',
  globalSetup: './global-setup.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 60_000,
  reporter: [['list'], ['json', { outputFile: 'test-results/results.json' }]],
  use: {
    baseURL,
    trace: 'retain-on-failure',
  },
  projects: capture
    ? [{ name: 'capture', testMatch: /capture\/.*\.spec\.ts/, use: { ...devices['Desktop Chrome'] } }]
    : [
        {
          name: 'chromium',
          testMatch: /tests\/.*\.spec\.ts/,
          use: {
            ...devices['Desktop Chrome'],
            // Phone-class viewport by default (Pixel/Galaxy width); specs that
            // need another size set their own.
            viewport: { width: 390, height: 844 },
          },
        },
      ],
});
