import { test, expect } from '../lib/fixtures';
import { Page } from '@playwright/test';
import { openDispatchSheet } from '../lib/dispatch';
import { layoutDispatchPort, password, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// Phone-width layout at the narrowest common Android width (360px).
test.use({ viewport: { width: 360, height: 740 }, hasTouch: true, isMobile: true });

// 🔴 THE DISPATCH SURFACES ARE MEASURED ON A SERVER OF THEIR OWN. The shared
// server has no agent provisioner — the docker compose install's shape — so it
// draws no "+" on the Agents bar, no Dispatch on a task card and no dispatch
// form. This one runs the noop provisioner, so the controls and the sheet a
// dispatching deployment draws are still audited.
const dorigin = `http://localhost:${layoutDispatchPort}`;
let dserver: Running | undefined;

test.beforeAll(async () => {
  dserver = await startServer(versionA, layoutDispatchPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
});
test.afterAll(async () => {
  await dserver?.stop();
});

// signInDispatch leaves this page signed in on the dispatching server's
// /agents. The shared server's session cookie is scoped to the host, not the
// port, and both servers share the suite's password, so it is usually already
// accepted here; the form is filled only when the server asks for it.
async function signInDispatch(page: Page) {
  await page.goto(dorigin + '/agents');
  if (new URL(page.url()).pathname.startsWith('/login')) {
    await page.fill('#muster-password', password);
    await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
    await page.goto(dorigin + '/agents');
  }
  expect(new URL(page.url()).pathname, 'signed in on the dispatching server').toBe('/agents');
}

const pages = ['/tasks', '/agents', '/tasks/1', '/tasks/999999', '/agents/docs-sweeper'];

async function settle(page: Page) {
  await page.waitForLoadState('networkidle').catch(() => undefined);
  await page.waitForTimeout(300);
}

test('no main page scrolls sideways at 360px', async ({ page }) => {
  for (const p of pages) {
    await page.goto(p);
    await settle(page);
    const [scroll, client] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(scroll, `${p}: scrollWidth ${scroll} > clientWidth ${client}`).toBeLessThanOrEqual(client);
  }
});

// Every VISIBLE control a thumb is meant to hit is at least 44×44 CSS px.
// The board card's stretched link is excluded: its hit area is the whole card
// (a ::after overlay), not the anchor's own box.
const controlsJS = `(() => {
  const vis = (e) => { const s = getComputedStyle(e); const r = e.getBoundingClientRect();
    return s.visibility !== 'hidden' && s.display !== 'none' && r.width > 0 && r.height > 0 &&
      r.right > 0 && r.left < innerWidth && !e.closest('[aria-hidden="true"]') && !e.closest('.sr-only'); };
  const out = [];
  for (const e of document.querySelectorAll('a[href], button, select, input:not([type=hidden]):not([type=checkbox]):not([type=radio]), textarea, summary')) {
    if (!vis(e) || e.classList.contains('card-link')) continue;
    const r = e.getBoundingClientRect();
    if (r.width < 44 || r.height < 44) out.push(e.tagName.toLowerCase() + (e.id ? '#' + e.id : '') + ' ' + Math.round(r.width) + 'x' + Math.round(r.height) + ' ' + JSON.stringify((e.getAttribute('aria-label') || e.textContent || '').trim().slice(0, 30)));
  }
  return out;
})()`;

const fieldsJS = `(() => [...document.querySelectorAll('input:not([type=hidden]):not([type=checkbox]):not([type=radio]), select, textarea')]
  .filter((e) => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0 && getComputedStyle(e).visibility !== 'hidden'; })
  .map((e) => ({ name: e.name || e.id || e.tagName, px: parseFloat(getComputedStyle(e).fontSize) }))
  .filter((f) => f.px < 16))()`;

test('every visible control is a 44px target and every field is 16px text', async ({ page }) => {
  // A control audit that matched nothing would report zero violations for any
  // page; require it to have measured a real number of controls.
  let measured = 0;
  const check = async (label: string) => {
    await settle(page);
    measured += await page.evaluate(() => document.querySelectorAll('a[href], button, select, textarea, input').length);
    expect(await page.evaluate(controlsJS), `${label}: controls under 44px`).toEqual([]);
    expect(await page.evaluate(fieldsJS), `${label}: fields under 16px (mobile browsers zoom on focus)`).toEqual([]);
  };
  for (const p of pages) {
    await page.goto(p);
    await check(p);
  }
  await page.goto('/tasks');
  await settle(page);
  await page.click('#fab-tasks');
  await expect(page.locator('#task-modal textarea[name="body"]')).toBeVisible();
  await check('new-task sheet');
  // The dispatching server: its Tasks board (a card's Dispatch button), its
  // Agents list (the bar's "+") and the dispatch sheet.
  await signInDispatch(page);
  await check('agents (dispatching server)');
  await expect(page.locator('#switcher-deploy'), 'the dispatching server draws the +').toBeVisible();
  await page.goto(dorigin + '/tasks');
  await check('tasks (dispatching server)');
  await expect(page.locator('button[aria-label="Dispatch agent for this task"]').first(), 'the dispatching server draws a card Dispatch').toBeVisible();
  await page.goto(dorigin + '/agents');
  await settle(page);
  await openDispatchSheet(page);
  await expect(page.locator('#agent-modal form')).toBeVisible();
  await check('dispatch sheet');
  expect(measured, 'the audit measured real controls').toBeGreaterThan(100);
});

test('a sheet keeps its primary action on screen with Advanced open', async ({ page }) => {
  await page.setViewportSize({ width: 360, height: 640 });
  // The Agents tab has no FAB any more: its sheet is opened as the FAB did
  // (openDispatchSheet).
  for (const [path, open, label] of [
    ['/tasks', (p: typeof page) => p.click('#fab-tasks'), 'Save task'],
    // On the dispatching server: the shared one draws no dispatch sheet.
    [dorigin + '/agents', openDispatchSheet, 'Dispatch'],
  ] as const) {
    if (path.startsWith(dorigin)) await signInDispatch(page);
    await page.goto(path);
    await settle(page);
    await open(page);
    const sheet = page.locator('.fixed:not(.hidden) form').first();
    await expect(sheet).toBeVisible();
    await sheet.locator('summary').first().click();
    await page.waitForTimeout(300);
    const box = await page.getByRole('button', { name: label, exact: true }).boundingBox();
    expect(box, `${label} has a box`).toBeTruthy();
    expect(box!.y + box!.height, `${label} bottom edge is inside the 640px viewport`).toBeLessThanOrEqual(640);
    expect(box!.y).toBeGreaterThanOrEqual(0);
  }
});

// The shared server is the docker compose install's shape: no agent
// provisioner, so a dispatch could only answer 503. It must offer none — no
// "+" on the Agents bar, no Dispatch on a task card — and the dispatch sheet,
// if a stale page asks for it, explains what is missing instead of drawing a
// form. Red at origin/main: all three were drawn. The dispatching server
// (above) is the control: it draws the + and the card's Dispatch.
test('a server with no agent provisioner offers no dispatch, and the sheet explains why', async ({ page }) => {
  await page.goto('/agents');
  await settle(page);
  await expect(page.locator('#agents-switcher-dock'), 'the bar the + would sit in is drawn').toBeVisible();
  await expect(page.locator('#switcher-deploy')).toHaveCount(0);
  await page.goto('/tasks');
  await settle(page);
  await expect(page.locator('[id^="task-"][data-task-status]').first(), 'the board drew task cards').toBeVisible();
  await expect(page.locator('button[aria-label="Dispatch agent for this task"]')).toHaveCount(0);
  const sheet = await page.request.get('/ui/agents/new');
  expect(sheet.status()).toBe(200);
  const body = await sheet.text();
  expect(body).toContain('data-dispatch-unavailable');
  expect(body).toContain('Agent dispatch is not set up');
  expect(body).not.toContain('hx-post="/agents"');
});
