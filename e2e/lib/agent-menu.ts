// A claude-code agent's page has ONE header row (internal/ui
// agent_header_folded.go — operator decision on PR #527): its Claude account,
// its failover line, its worker pool and its Terminal link are items of the ⋯
// menu (#agent-actions), not a second header row. These are the suite's ONE way
// to reach them, so a spec says "open the menu, then …" rather than each
// re-deriving the menu's ids.
import { expect, FrameLocator, Page } from '@playwright/test';

type Root = Page | FrameLocator;

// openAgentMenu opens the agent page's ⋯ menu (a tap with touch, else a click)
// and waits for it to be shown.
export async function openAgentMenu(page: Root, how: 'click' | 'tap' = 'click'): Promise<void> {
  const open = page.locator('#agent-actions-open');
  await (how === 'tap' ? open.tap() : open.click());
  await expect(page.locator('#agent-actions')).toBeVisible();
}

// agentMenu is the ⋯ menu's element.
export const agentMenu = (page: Root) => page.locator('#agent-actions');

// toTerminal opens the ⋯ menu and presses its Terminal item (#view-terminal, the
// same view-swap link the Rendered | Terminal switch draws).
export async function toTerminal(page: Root, how: 'click' | 'tap' = 'click'): Promise<void> {
  await openAgentMenu(page, how);
  const t = page.locator('#agent-actions #view-terminal');
  await (how === 'tap' ? t.tap() : t.click());
}
