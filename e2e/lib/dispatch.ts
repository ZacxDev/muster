import { Page, expect } from '@playwright/test';

// openDispatchSheet opens the task-less dispatch sheet (the Model, Repository,
// task picker and ad-hoc task form, GET /ui/agents/new) the way a person does
// on the Agents list: the "+" on its session-switcher bar (operator ask: deploy
// "In the bottom bar"; it replaced the list's "+" FAB). It goes to /agents
// first when the page is elsewhere.
export async function openDispatchSheet(page: Page): Promise<void> {
  if (new URL(page.url()).pathname !== '/agents') await page.goto('/agents');
  const plus = page.locator('#switcher-deploy');
  await expect(plus).toBeVisible();
  await plus.click();
  await expect(page.locator('#agent-modal form')).toBeVisible();
}
