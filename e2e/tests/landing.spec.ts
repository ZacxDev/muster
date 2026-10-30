import { Page } from '@playwright/test';
import { test, expect, expectShellShows, navTabKey } from '../lib/fixtures';
import { connectPort, connectorsPort, password, versionA } from '../lib/env';
import { openMore } from '../lib/nav';
import { startServer, Running } from '../lib/server';

// `/` is where every sign-in, claim and invite redirect lands. It opens the
// chief agent's chat — or, with no chief (this shared server has none), the
// session the viewer opened LAST that is still listed (internal/api/nav_agents.go
// homeTarget; the Agents list it once opened is retired).
//
// The SHELL is still drawn at a bare `/` in one case: the service worker's
// offline copy, which it fetches as `/?shell=1` and serves to an offline open
// of `/`. The shell's script used to resolve `/` to a tab key muster does not
// have (the product it was extracted from defaulted to a `requests` tab), so
// on `/` it hid every panel and every FAB: a blank main area under a correct
// URL. A browser that had already saved a tab never showed it — restoreNav
// moved it off `/` first — which is how it survived until a fresh account.
// `/?shell=1` is that document at that path, so it is what is tested here.
//
// The shared server's shell has ONE tab (Tasks: it has no connector store), so
// the cases that need a SECOND tab to restore or switch to run against a server
// of this spec's own with the connector store on (its shell also draws
// Connectors).
//
// 412 px, the width it was measured at.
test.use({ viewport: { width: 412, height: 915 } });

const shellAtRoot = '/?shell=1';
const taskCards = '[data-tasks-flat] > [id^="task-"]';

// 🔴 A REGRESSION TEST for the retirement: on the commit before, `/` with no
// chief opened the Agents list. Two sessions, each opened in turn, so the
// landing follows the viewer's last open and is not one fixed page.
test('/ opens the session opened last when there is no chief', async ({ page }) => {
  for (const name of ['release-helper', 'docs-sweeper', 'release-helper']) {
    await page.goto('/agents/' + name);
    await expect(page.locator('[data-agent-title]')).toBeVisible();
    await page.goto('/');
    expect(new URL(page.url()).pathname, `/ after opening ${name}`).toBe('/agents/' + name);
  }
});

test('the retired Agents list URL lands where home does, its query dropped', async ({ page }) => {
  await page.goto('/agents/docs-sweeper');
  await page.goto('/agents?filter=waiting');
  const u = new URL(page.url());
  expect(u.pathname, '/agents redirects home').toBe('/agents/docs-sweeper');
  expect(u.searchParams.get('filter'), 'the old filter is dropped').toBeNull();
  await expect(page.locator('#agents-list')).toHaveCount(0);
});

test('the shell at / shows the board and its New-task button in a browser with no saved tab', async ({ page }) => {
  await page.addInitScript((k) => localStorage.removeItem(k), navTabKey);
  await page.goto(shellAtRoot);
  await expectShellShows(page, 'tasks', 'Tasks', '/ with no saved tab');
  await expect(page.locator(taskCards).first(), 'the seeded board is visible on /').toBeVisible();
  await expect(page.locator('#fab-tasks')).toBeVisible();
  expect(new URL(page.url()).pathname, '/?shell=1 is served as itself, not redirected').toBe('/');
});

test('the shell at / shows the board when the saved tab names a tab this shell does not have', async ({ page }) => {
  await page.addInitScript((k) => localStorage.setItem(k, 'requests'), navTabKey);
  await page.goto(shellAtRoot);
  await expectShellShows(page, 'tasks', 'Tasks', '/ with a saved tab that is not a tab');
  await expect(page.locator('#fab-tasks')).toBeVisible();
});

// A device that saved the Agents tab before it was retired: the same
// unknown-tab case, by the key such browsers really hold.
test('the shell at / shows the board when the saved tab is the retired Agents tab', async ({ page }) => {
  await page.addInitScript((k) => localStorage.setItem(k, 'agents'), navTabKey);
  await page.goto(shellAtRoot);
  await expectShellShows(page, 'tasks', 'Tasks', '/ with the retired agents tab saved');
  await expect(page.locator('#fab-tasks')).toBeVisible();
});

test.describe('a shell with a second tab (the connector store on)', () => {
  const origin = `http://localhost:${connectorsPort}`;
  let server: Running | undefined;
  test.use({ storageState: { cookies: [], origins: [] } });

  test.beforeAll(async () => {
    server = await startServer(versionA, connectorsPort, {
      // 32 bytes of 0x2a, base64: an obviously-synthetic key for a throwaway server.
      MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x2a).toString('base64'),
      MUSTER_CONNECT_PORT: String(connectPort),
      // The store refuses to boot without the URL agents are told to use.
      MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${connectPort}`,
    });
  });
  test.afterAll(async () => {
    await server?.stop();
  });

  async function login(page: Page) {
    await page.goto(origin + '/login');
    await page.fill('#muster-password', password);
    await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  }

  // INVARIANT GUARD, not regression coverage: this held before the fix too. It
  // is here so the fix cannot be "ignore the saved tab".
  test('the shell at / still restores a saved tab the shell does have', async ({ page }) => {
    await login(page);
    await page.addInitScript((k) => localStorage.setItem(k, 'connectors'), navTabKey);
    await page.goto(origin + shellAtRoot);
    await expectShellShows(page, 'connectors', 'Connectors', '/ with connectors saved');
    expect(new URL(page.url()).pathname).toBe('/connectors');
  });

  test('from the shell at /, the sidebar switches tab and comes back to the board', async ({ page }) => {
    await login(page);
    await page.addInitScript((k) => localStorage.removeItem(k), navTabKey);
    await page.goto(origin + shellAtRoot);
    await expectShellShows(page, 'tasks', 'Tasks', '/ before switching');
    await page.click('#sidebar-open');
    await openMore(page);
    await page.click('.sidebar-tab[data-tab="connectors"]');
    await expectShellShows(page, 'connectors', 'Connectors', 'after choosing Connectors from /');
    expect(new URL(page.url()).pathname).toBe('/connectors');
    await page.goBack();
    await expectShellShows(page, 'tasks', 'Tasks', 'Back, to /');
    expect(new URL(page.url()).pathname).toBe('/');
  });
});
