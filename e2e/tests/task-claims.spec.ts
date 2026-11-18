import { request as pwRequest } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { baseURL, hookToken } from '../lib/env';

// A CLAIMED TASK ON THE BOARD (docs/tasks.md#claims), at a phone and a desktop
// width, each set explicitly: a session picks a task up through the machine
// route (`muster task status <id> in_progress` from inside a session sends its
// session id), and the board's card says the task is worked, and by whom. A
// task set in progress with NO session is not claimed and carries no chip (the
// control), and setting the claimed task back to open drops the chip.
//
// 🔴 REGRESSION TEST: on origin/main the status write records no claim and the
// card has no [data-task-claimed] chip.
//
// Every title and session id is invented.

const stamp = Date.now().toString(36);
const sessionID = '5e55a1d0-0000-4000-8000-' + stamp.padStart(12, '0').slice(-12);

async function machine() {
  return pwRequest.newContext({ baseURL, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
}

async function setStatus(id: number, status: string, session?: string) {
  const ctx = await machine();
  const r = await ctx.patch(`/api/tasks/${id}/status`, {
    data: { status },
    headers: session ? { 'X-Muster-Session-Id': session } : {},
  });
  expect(r.status(), await r.text()).toBeLessThan(300);
  await ctx.dispose();
}

for (const vp of [
  { name: 'phone', viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true },
  { name: 'desktop', viewport: { width: 1280, height: 800 }, isMobile: false, hasTouch: false },
]) {
  test.describe(`at ${vp.name} width`, () => {
    test.use({ viewport: vp.viewport, isMobile: vp.isMobile, hasTouch: vp.hasTouch });

    test('a task a session claimed shows as worked, with its claimant; unclaimed shows nothing', async ({ page, api }) => {
      const claimed = await api.createTask(`Claimed press calibration ${vp.name} ${stamp}`, 'A synthetic task for the claims spec.');
      const control = await api.createTask(`Unclaimed press calibration ${vp.name} ${stamp}`, 'A synthetic task for the claims spec.');
      await setStatus(claimed.id, 'in_progress', sessionID);
      await setStatus(control.id, 'in_progress');

      await page.goto('/tasks');
      const card = page.locator(`#tasks-list #task-${claimed.id}`);
      await expect(card).toBeVisible();
      const chip = card.locator('[data-task-claimed]');
      await expect(chip).toHaveAttribute('data-task-claimed', 'session');
      await expect(chip).toHaveText('⚑ worked by session ' + sessionID.slice(0, 8));
      await expect(chip).toBeVisible();
      // The chip sits inside the card, not past its edge, at this width.
      const [c, k] = [await card.boundingBox(), await chip.boundingBox()];
      expect(c && k && k.x >= c.x && k.x + k.width <= c.x + c.width + 0.5, `chip ${JSON.stringify(k)} inside card ${JSON.stringify(c)}`).toBe(true);
      // The control: in progress with no session is no claim.
      const other = page.locator(`#tasks-list #task-${control.id}`);
      await expect(other).toBeVisible();
      await expect(other.locator('[data-task-claimed]')).toHaveCount(0);

      // The unclaim: back to open, and the board drops the chip.
      await setStatus(claimed.id, 'open');
      await page.goto('/tasks');
      await expect(page.locator(`#tasks-list #task-${claimed.id}`)).toBeVisible();
      await expect(page.locator(`#tasks-list #task-${claimed.id} [data-task-claimed]`)).toHaveCount(0);
    });
  });
}
