import { test, expect } from '../lib/fixtures';

// The worker registers from the page, at scope "/", and takes control.
test('the service worker registers at scope / and controls the page', async ({ page, context }) => {
  const registered = context.waitForEvent('serviceworker');
  await page.goto('/tasks');
  const worker = await registered;
  expect(new URL(worker.url()).pathname).toBe('/sw.js');

  const state = await page.evaluate(async () => {
    const reg = await navigator.serviceWorker.ready;
    if (!navigator.serviceWorker.controller) {
      await new Promise((r) => navigator.serviceWorker.addEventListener('controllerchange', r, { once: true }));
    }
    const build = await new Promise<string>((resolve) => {
      const ch = new MessageChannel();
      ch.port1.onmessage = (e) => resolve(e.data.build);
      navigator.serviceWorker.controller!.postMessage({ type: 'GET_VERSION' }, [ch.port2]);
    });
    return { scope: new URL(reg.scope).pathname, controlled: !!navigator.serviceWorker.controller, build };
  });
  expect(state.scope).toBe('/');
  expect(state.controlled).toBe(true);
  // The server stamped its build into the worker it served.
  expect(state.build).toBe('e2e-a');
});
