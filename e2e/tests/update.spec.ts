import { test, expect } from '../lib/fixtures';
import { updatePort, versionA, versionB } from '../lib/env';
import { startServer, Running } from '../lib/server';

// A deploy, end to end: build A serves the page, build B replaces it on the
// same origin, and the open page offers the update without taking it.
//
// 🔴 TWO REAL BUILDS, NOT A ROUTED SCRIPT. Playwright cannot route() a service
// worker's update fetch, and the property under test is that DIFFERENT BUILDS
// serve different worker bytes — so the spec runs both binaries global-setup
// built, on a port of its own (a fresh origin, so no other spec's worker is
// involved).

let server: Running | undefined;
test.afterEach(async () => {
  await server?.stop();
  server = undefined;
});

// Asks the CONTROLLING worker which build it is. It times out to null: a
// message posted to a controller that is being replaced can go unanswered (it
// turns redundant mid-question), and a poll that hangs on one call never asks
// again.
const controllerBuild = `(async () => {
  const c = navigator.serviceWorker.controller;
  if (!c) return null;
  return await new Promise((resolve) => {
    const ch = new MessageChannel();
    const t = setTimeout(() => resolve(null), 1000);
    ch.port1.onmessage = (e) => { clearTimeout(t); resolve(e.data.build); };
    c.postMessage({ type: 'GET_VERSION' }, [ch.port2]);
  });
})()`;

test('a new build is offered, waits for Reload, and Reload takes it exactly once', async ({ page }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.addInitScript(() => {
    sessionStorage.setItem('loads', String(Number(sessionStorage.getItem('loads') || '0') + 1));
  });
  await page.goto(origin + '/tasks');
  await page.evaluate(async () => {
    await navigator.serviceWorker.ready;
    if (!navigator.serviceWorker.controller) {
      await new Promise((r) => navigator.serviceWorker.addEventListener('controllerchange', r, { once: true }));
    }
  });
  expect(await page.evaluate(controllerBuild)).toBe(versionA);
  // A first install offers nothing.
  await expect(page.locator('#sw-update-toast')).toBeHidden();

  // Deploy: same origin, new build.
  await server.stop();
  server = await startServer(versionB, updatePort);
  await page.evaluate(async () => {
    const reg = await navigator.serviceWorker.getRegistration();
    await reg!.update();
  });

  await expect(page.locator('#sw-update-toast')).toBeVisible();
  // 🔴 The new worker WAITS: the page is still controlled by build A. (A worker
  // that called skipWaiting() at install would already be in control here.)
  await page.waitForTimeout(500);
  expect(await page.evaluate(controllerBuild)).toBe(versionA);

  const loadsBefore = Number(await page.evaluate(() => sessionStorage.getItem('loads')));
  await page.locator('[data-sw-reload]').click();
  // The reload this click causes destroys the page's context mid-evaluate;
  // that is the expected event, so a destroyed context reads as "not yet".
  await expect
    .poll(() => page.evaluate(controllerBuild).catch(() => null), { timeout: 15_000 })
    .toBe(versionB);
  await page.waitForLoadState('load');
  await page.waitForTimeout(1500);
  expect(Number(await page.evaluate(() => sessionStorage.getItem('loads'))), 'exactly one reload').toBe(loadsBefore + 1);
  await expect(page.locator('#sw-update-toast')).toBeHidden();
});
