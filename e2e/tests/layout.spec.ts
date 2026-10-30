import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { Page } from '@playwright/test';
import { openDispatchSheet } from '../lib/dispatch';
import { layoutDispatchPort, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// Phone-width layout at the narrowest common Android width (360px).
test.use({ viewport: { width: 360, height: 740 }, hasTouch: true, isMobile: true });

// 🔴 THE DISPATCH SURFACES ARE MEASURED ON A SERVER OF THEIR OWN. The shared
// server has no agent provisioner — the docker compose install's shape — so it
// draws no "+" beside the sidebar's Agents label, no Dispatch on a task card
// and no dispatch form. This one runs the noop provisioner, so the controls and the sheet a
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
// /tasks. (Not /agents: the Agents list page is retired, and /agents is home's
// redirect to some session there.) The shared server's session cookie is scoped
// to the host, not the port, and both servers share the suite's password, so
// lib/signin.ts usually finds it already accepted and drives no form.
async function signInDispatch(page: Page) {
  await signIn(page, dorigin);
  await page.goto(dorigin + '/tasks');
  expect(new URL(page.url()).pathname, 'signed in on the dispatching server').toBe('/tasks');
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
  // sidebar (the "+" beside the Agents label, in the drawer) and the dispatch
  // sheet.
  await signInDispatch(page);
  await check('tasks (dispatching server)');
  await expect(page.locator('button[aria-label="Dispatch agent for this task"]').first(), 'the dispatching server draws a card Dispatch').toBeVisible();
  await page.click('#sidebar-open');
  await expect(page.locator('#sidebar-deploy'), 'the dispatching server draws the +').toBeVisible();
  await check('sidebar drawer (dispatching server)');
  await openDispatchSheet(page);
  await expect(page.locator('#agent-modal form')).toBeVisible();
  await check('dispatch sheet');
  expect(measured, 'the audit measured real controls').toBeGreaterThan(100);
});

test('a sheet keeps its primary action on screen with Advanced open', async ({ page }) => {
  await page.setViewportSize({ width: 360, height: 640 });
  // The dispatch sheet is opened from the sidebar's "+" (openDispatchSheet).
  for (const [path, open, label] of [
    ['/tasks', (p: typeof page) => p.click('#fab-tasks'), 'Save task'],
    // On the dispatching server: the shared one draws no dispatch sheet.
    [dorigin + '/tasks', openDispatchSheet, 'Dispatch'],
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
// "+" beside the sidebar's Agents label (the hint that says why is drawn in its
// place), no Dispatch on a task card — and the dispatch sheet,
// if a stale page asks for it, explains what is missing instead of drawing a
// form. Red at origin/main: all three were drawn. The dispatching server
// (above) is the control: it draws the + and the card's Dispatch.
test('a server with no agent provisioner offers no dispatch, and the sheet explains why', async ({ page }) => {
  await page.goto('/tasks');
  await settle(page);
  await page.click('#sidebar-open');
  await expect(page.locator('#sidebar [data-dispatch-off-hint]'), 'the head row the + would sit in is drawn, with its hint').toBeVisible();
  await expect(page.locator('#sidebar-deploy')).toHaveCount(0);
  await expect(page.locator('[id^="task-"][data-task-status]').first(), 'the board drew task cards').toBeVisible();
  await expect(page.locator('button[aria-label="Dispatch agent for this task"]')).toHaveCount(0);
  const sheet = await page.request.get('/ui/agents/new');
  expect(sheet.status()).toBe(200);
  const body = await sheet.text();
  expect(body).toContain('data-dispatch-unavailable');
  expect(body).toContain('Agent dispatch is not set up');
  expect(body).not.toContain('hx-post="/agents"');
});
