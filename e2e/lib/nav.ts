import { FrameLocator, Page, expect } from '@playwright/test';

// The sidebar's pages (Tasks, Agents, Connectors, Asks, Install, the theme,
// Account, Admin) are inside its collapsed "More" (internal/ui/nav_agents.go
// navMore), shut by default. openMore expands it the way a person does — a
// tap on its header — and waits until its body is shown. A More this device
// already left open is left as it is. The sidebar must be on screen (opened on
// a phone, the rail on a desktop).
export async function openMore(d: Page | FrameLocator): Promise<void> {
  const more = d.locator('#sidebar-more');
  if (!(await more.evaluate((e) => (e as HTMLDetailsElement).open))) {
    await d.locator('#sidebar-more-summary').click();
  }
  await expect(more).toHaveJSProperty('open', true);
  await expect(d.locator('#sidebar-more-body')).toBeVisible();
}

// navMoreKey is where a device keeps whether More is open (localStorage).
export const navMoreKey = 'muster.nav.more';

// landedHome matches where a sign-in (or a claim, an invite) lands on the
// server at base: it redirects to `/`, and `/` opens the chief agent's chat —
// or, with no chief, the Agents list (internal/api/nav_agents.go). `/` itself
// never commits, so a wait for exactly `/` would never end.
export const landedHome = (base: string) => (u: URL) =>
  u.origin === new URL(base).origin && (u.pathname === '/agents' || u.pathname.startsWith('/agents/'));
