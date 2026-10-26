// Service-worker helpers the specs share.
import { expect, type Page } from '@playwright/test';

const isControlled = (page: Page) =>
  page.evaluate(async () => {
    await navigator.serviceWorker.ready;
    return !!navigator.serviceWorker.controller;
  });

// waitControlled returns once the page is controlled by the origin's active
// worker. It reloads the page AT MOST ONCE to get there, and then asserts.
//
// 🔴 WAITING FOR controllerchange CANNOT PROMISE CONTROL. The old wait (await
// `ready`, then a controllerchange when there was no controller) hung until
// the test timeout whenever the page's navigation began while the first
// worker was still installing. By the time `ready` resolved, the worker was
// `activated` and its clients.claim() had already run, yet the page was
// uncontrolled: clients.matchAll listed it only with includeUncontrolled.
// Nothing was left to claim it. (Measured on push.spec, pinned to 2 loaded
// CPUs: 9 of 20 runs red at 0374a081, #283.) A navigation that starts once a
// worker is active is always handled by that worker (fetch waits for
// `activated`), so the reloaded document is controlled. If it is not, the
// assertion fails at once instead of hanging.
//
// ⚠ A spec that counts page loads must read its baseline AFTER this call.
// ⚠ Not for a spec whose subject IS the first install's claim (sw.spec's
// "controls the page"): a reload would hide a claim that never happened.
export async function waitControlled(page: Page): Promise<void> {
  if (!(await isControlled(page))) await page.reload();
  expect(await isControlled(page), 'the page is controlled by the service worker').toBe(true);
}
