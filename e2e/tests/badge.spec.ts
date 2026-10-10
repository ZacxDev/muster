import { test, expect } from '../lib/fixtures';

// Desktop/ChromeOS app badge: the ready-for-review count, kept live by the
// task.changed SSE event. navigator.setAppBadge is stubbed to record calls.
test('the app badge tracks the ready-for-review count through live updates', async ({ page, api }) => {
  await page.addInitScript(() => {
    const calls: number[] = [];
    (window as unknown as { __badge: number[] }).__badge = calls;
    Object.defineProperty(navigator, 'setAppBadge', { configurable: true, value: (n: number) => (calls.push(n), Promise.resolve()) });
    Object.defineProperty(navigator, 'clearAppBadge', { configurable: true, value: () => (calls.push(0), Promise.resolve()) });
  });
  const reviewCount = async () => (await api.tasks()).filter((t) => t.status === 'ready_for_review').length;
  const last = () => page.evaluate(() => {
    const c = (window as unknown as { __badge: number[] }).__badge;
    return c.length ? c[c.length - 1] : null;
  });

  const start = await reviewCount();
  expect(start, 'the seed has a task in review').toBeGreaterThan(0);
  await page.goto('/tasks');
  await expect.poll(last).toBe(start);

  const open = (await api.tasks()).find((t) => t.status === 'open');
  expect(open, 'an open task to move').toBeTruthy();
  await api.setStatus(open!.id, 'ready_for_review');
  await expect.poll(last, { timeout: 15_000 }).toBe(start + 1);

  await api.setStatus(open!.id, 'open');
  await expect.poll(last, { timeout: 15_000 }).toBe(start);
});
