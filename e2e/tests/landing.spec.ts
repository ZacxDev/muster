import { test, expect, expectShellShows, navTabKey } from '../lib/fixtures';
import { openMore } from '../lib/nav';

// `/` is where every sign-in, claim and invite redirect lands. It opens the
// chief agent's chat — or, with no chief (this shared server has none), the
// Agents list (internal/api/nav_agents.go redirectHome).
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
// 412 px, the width it was measured at.
test.use({ viewport: { width: 412, height: 915 } });

const shellAtRoot = '/?shell=1';
const taskCards = '[data-tasks-flat] > [id^="task-"]';

test('/ opens the Agents list when there is no chief', async ({ page }) => {
  await page.goto('/');
  expect(new URL(page.url()).pathname, '/ redirects home').toBe('/agents');
  await expectShellShows(page, 'agents', 'Agents', '/ with no chief');
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

// INVARIANT GUARD, not regression coverage: this held before the fix too. It is
// here so the fix cannot be "ignore the saved tab".
test('the shell at / still restores a saved tab the shell does have', async ({ page }) => {
  await page.addInitScript((k) => localStorage.setItem(k, 'agents'), navTabKey);
  await page.goto(shellAtRoot);
  await expectShellShows(page, 'agents', 'Agents', '/ with agents saved');
  expect(new URL(page.url()).pathname).toBe('/agents');
});

test('from the shell at /, the sidebar switches tab and comes back to the board', async ({ page }) => {
  await page.addInitScript((k) => localStorage.removeItem(k), navTabKey);
  await page.goto(shellAtRoot);
  await expectShellShows(page, 'tasks', 'Tasks', '/ before switching');
  await page.click('#sidebar-open');
  await openMore(page);
  await page.click('.sidebar-tab[data-tab="agents"]');
  await expectShellShows(page, 'agents', 'Agents', 'after choosing Agents from /');
  expect(new URL(page.url()).pathname).toBe('/agents');
  await page.goBack();
  await expectShellShows(page, 'tasks', 'Tasks', 'Back, to /');
  expect(new URL(page.url()).pathname).toBe('/');
});
