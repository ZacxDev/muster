// Builds both server versions, migrates and seeds the e2e database, starts the
// shared server, and saves a signed-in browser state. Returns the teardown.
import { chromium, FullConfig } from '@playwright/test';
import fs from 'node:fs';
import { authState, baseURL, dsn, port, requireFull, stateDir, versionA, versionB, versionC, versionTerm } from './lib/env';
import { buildServer, expectSignInBudget, goRun, startServer } from './lib/server';
import { signIn } from './lib/signin';

export default async function globalSetup(_config: FullConfig) {
  fs.mkdirSync(stateDir, { recursive: true });
  const skipFile = `${stateDir}/skip`;
  fs.rmSync(skipFile, { force: true });
  if (!dsn) {
    if (requireFull) {
      throw new Error('MUSTER_E2E_DATABASE_URL is unset and MUSTER_E2E_REQUIRE_FULL=1: refusing to report a suite that ran nothing');
    }
    // Every spec checks this file and skips — loudly, in the verdict's count.
    fs.writeFileSync(skipFile, 'no MUSTER_E2E_DATABASE_URL');
    return async () => {};
  }

  buildServer(versionA);
  buildServer(versionB);
  buildServer(versionC);
  buildServer(versionTerm, ['e2eterm']);
  goRun('./cmd/muster-migrate', { DATABASE_URL: dsn });
  goRun('./e2e/seed', { DATABASE_URL: dsn });

  const server = await startServer(versionA, port);

  const browser = await chromium.launch();
  const page = await browser.newPage();
  // fresh: a previous run's auth.json would still be accepted (the session is
  // signed with a key derived from the suite's password), but its issue time is
  // that run's, and this one's specs should not inherit it.
  await signIn(page, baseURL, { fresh: true });
  await page.context().storageState({ path: authState });
  await browser.close();

  return async () => {
    await server.stop();
    // The shared server's sign-ins happen in the workers, but its log is here
    // (lib/server.ts signInBudget).
    expectSignInBudget();
  };
}
