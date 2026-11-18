import { execFileSync } from 'node:child_process';
import { test, expect } from '../lib/fixtures';
import { dsn, repoRoot } from '../lib/env';

// A HELD FIRST TURN, in a real browser on the shared server: a claude-code agent
// whose repository cannot be checked out until its owner approves the agent's
// github connector request. The kickoff deliverer holds its first turn, unsent and
// unstamped, until then (internal/agentkickoff, agents.DecideFirstTurn), and the
// agent's page says so — "waiting for your github approval", a notice above the
// transcript (#agent-notices, internal/ui/agent_actions.go agentNotices; the
// retired Agents list's card said it) — with a link to Connectors, where the
// request is. The click path is the point: the owner's one move.
//
// The row is the fixture e2e/heldfix writes (the state the deliverer leaves).

const held = 'held-kelp-tern';
const heldfix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/heldfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } });

test.beforeAll(() => {
  heldfix('create', held);
});
test.afterAll(() => {
  heldfix('delete', held);
});

for (const vp of [
  { label: 'phone', viewport: { width: 390, height: 844 }, touch: true },
  { label: 'desktop', viewport: { width: 1280, height: 900 }, touch: false },
]) {
  test.describe(vp.label, () => {
    test.use({ viewport: vp.viewport, hasTouch: vp.touch, isMobile: vp.touch });

    // 🔴 A REGRESSION TEST: on the commit before the notice, a held agent said
    // only "kickoff owed", and nothing linked the owner to the approval it is
    // waiting on.
    test('a held kickoff says it waits for the github approval and links to Connectors', async ({ page }) => {
      await page.goto('/agents/' + held);
      await expect(page.locator('#agent-strip [data-kickoff-owed]')).toBeVisible();
      const line = page.locator('#agent-notices [data-kickoff-awaiting-grant]');
      await expect(line).toBeVisible();
      await expect(line).toContainText('Waiting for your github approval');
      // Only the fixed line crosses: not the recorded detail.
      await expect(line).not.toContainText('could not be cloned');

      // An agent that is not held has no such line (the control: the locator
      // CAN miss, on a page that drew its notices slot and its strip).
      const other = await page.context().newPage();
      await other.goto('/agents/docs-sweeper');
      await expect(other.locator('#agent-strip')).toBeVisible();
      await expect(other.locator('#agent-notices')).toHaveCount(1);
      await expect(other.locator('[data-kickoff-awaiting-grant]')).toHaveCount(0);
      await other.close();

      const link = line.getByRole('link', { name: 'Open Connectors' });
      if (vp.touch) await link.tap();
      else await link.click();
      await page.waitForURL((u) => u.pathname === '/connectors');
    });
  });
}
