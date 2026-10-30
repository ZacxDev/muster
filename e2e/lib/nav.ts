import { FrameLocator, Page, expect } from '@playwright/test';

// The sidebar's pages (Tasks, Connectors, Asks, Install, Account, Admin) are inside its collapsed "More" (internal/ui/nav_agents.go
// navMore), shut on every load. openMore expands it the way a person does — a
// tap on its header — and waits until its body is shown. A More already open
// in this document (an in-page tab switch keeps it) is left as it is. The
// sidebar must be on screen (opened on a phone, the rail on a desktop).
export async function openMore(d: Page | FrameLocator): Promise<void> {
  const more = d.locator('#sidebar-more');
  if (!(await more.evaluate((e) => (e as HTMLDetailsElement).open))) {
    await d.locator('#sidebar-more-summary').click();
  }
  await expect(more).toHaveJSProperty('open', true);
  await expect(d.locator('#sidebar-more-body')).toBeVisible();
}

// landedHome matches where a sign-in (or a claim, an invite) lands on the
// server at base: it redirects to `/`, and `/` opens the chief agent's chat —
// or, with no chief, the session opened last (an agent's page or a mirrored
// window's view), or with nothing to open the onboarding page at /agents
// (internal/api/nav_agents.go homeTarget). `/` itself never commits, so a wait
// for exactly `/` would never end.
export const landedHome = (base: string) => (u: URL) =>
  u.origin === new URL(base).origin &&
  (u.pathname === '/agents' || u.pathname.startsWith('/agents/') || u.pathname.startsWith('/mirror/'));
