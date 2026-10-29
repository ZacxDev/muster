import { execFileSync } from 'node:child_process';
import { test, expect } from '../lib/fixtures';
import { dsn, repoRoot } from '../lib/env';

// TITLES SHOW AS THE TEXT A READER SEES, NOT AS MARKDOWN (operator report:
// "Titles sometimes include markdown, strip it out"). Every title sink goes
// through internal/textplain.Title at render time; this is the click path:
//
//   - an agent dispatched from a task is named after it ("#<id> · <title>"),
//     and a task title routinely carries markdown. Its detail page's <title> and header
//     and its sidebar row must read as plain text — with the "#<id>" and the
//     snake_case identifier intact (the previous stripper removed every # and _).
//     The Agents list page is retired, so its card's sinks moved: the sidebar's
//     row (its text and its title attribute, the whole label) and the agent's
//     page header title;
//   - a task created with a markdown title: its /tasks/{id} <title> and <h1>.
//
// The stored values stay as written (the title is rendered plain, not
// rewritten), so the agent row's display_name is set straight in the database,
// as a dispatch writes it, and removed afterwards: the shared server's other
// specs count its agents.

const stamp = Date.now().toString(36);
const agentName = 'e2e-plain-title-' + stamp;
const token = 'e2e-plain-title-token-not-a-real-one-' + stamp;
const markdownName = '#41 · **Fix** [the login](https://example.com/pr/41) for `snake_case_names` ~~today~~';
const plainName = '#41 · Fix the login for snake_case_names today';
let agentID = '';

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

test.beforeAll(() => {
  agentID = fix('agent', '1', agentName, token);
  fix('label', agentID, markdownName);
});

test.afterAll(() => {
  if (agentID) fix('rmagent', '1', agentName);
});

test('a task-derived agent name with markdown reads plain on its sidebar row and in its page title', async ({ page }) => {
  await page.goto('/agents/' + agentName);
  await expect(page).toHaveTitle(plainName + ' · muster');
  // The page's own header title: the whole plain name (its title attribute)
  // and none of the markers in what it draws.
  const title = page.locator('[data-agent-title] h1');
  await expect(title).toHaveAttribute('title', plainName);
  // The sidebar's row (a drawer on a phone): the whole plain label as its
  // title, and none of the markers in its text.
  await page.locator('#sidebar-open').click();
  const row = page.locator(`#sidebar-agents-list a#nav-agent-${agentID}`);
  await expect(row).toBeVisible();
  await expect(row).toHaveAttribute('title', plainName);
  await expect(row).toContainText(plainName);
  for (const el of [row, title]) {
    const shown = await el.innerText();
    for (const marker of ['**', '](', '`', '~~']) expect(shown, `${await el.evaluate((e) => e.id || e.tagName)} shows ${marker}`).not.toContain(marker);
  }
});

test('a task with a markdown title reads plain in its detail page title and heading', async ({ page, api }) => {
  const t = await api.createTask('**Ship** `v2` for [C#](https://example.com/c) and a*b ' + stamp, 'A synthetic task for the plain-title spec.');
  await page.goto('/tasks/' + t.id);
  const plain = 'Ship v2 for C# and a*b ' + stamp;
  await expect(page).toHaveTitle(plain + ' · muster');
  await expect(page.locator('h1[data-task-title]')).toHaveText(plain);
});
