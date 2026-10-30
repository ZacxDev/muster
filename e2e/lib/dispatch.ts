import { Page, expect } from '@playwright/test';

// openDispatchSheet opens the task-less dispatch sheet (the Model, Repository,
// task picker and ad-hoc task form, GET /ui/agents/new) the way a person does:
// the "+" beside the sidebar's Agents label (internal/ui/nav_agents.go
// navDeployButton — the retired Agents list's bar had it). The sidebar is on
// every signed-in page; on a phone it is a drawer, opened first. A page with
// no sidebar yet (a fresh tab) goes to /tasks first.
export async function openDispatchSheet(page: Page): Promise<void> {
  if ((await page.locator('#sidebar').count()) === 0) await page.goto('/tasks');
  const plus = page.locator('#sidebar-deploy');
  if (!(await plus.isVisible())) await page.locator('#sidebar-open').click();
  await expect(plus).toBeVisible();
  await plus.click();
  await expect(page.locator('#agent-modal form')).toBeVisible();
}
