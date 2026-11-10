import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { Page } from '@playwright/test';
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
// 🔴 ONE NAMED EXCEPTION: the message composers' buttons are 40×40 (operator
// decision, internal/ui/mirror_compose.go) — every control
// of a mirrored composer's button row, and the agent chat's Send and mic by
// id. They are held to 40, not waived.
const controlsJS = `(() => {
  const vis = (e) => { const s = getComputedStyle(e); const r = e.getBoundingClientRect();
    return s.visibility !== 'hidden' && s.display !== 'none' && r.width > 0 && r.height > 0 &&
      r.right > 0 && r.left < innerWidth && !e.closest('[aria-hidden="true"]') && !e.closest('.sr-only'); };
  const composer = new Set(['chat-send', 'chat-dictate', 'chat-dictate-off']);
  const out = [];
  for (const e of document.querySelectorAll('a[href], button, select, input:not([type=hidden]):not([type=checkbox]):not([type=radio]), textarea, summary')) {
    if (!vis(e) || e.classList.contains('card-link')) continue;
    const r = e.getBoundingClientRect();
    const min = (e.closest('#mv-button-row') && !e.closest('[popover]')) || composer.has(e.id) ? 40 : 44;
    if (r.width < min || r.height < min) out.push(e.tagName.toLowerCase() + (e.id ? '#' + e.id : '') + ' ' + Math.round(r.width) + 'x' + Math.round(r.height) + ' ' + JSON.stringify((e.getAttribute('aria-label') || e.textContent || '').trim().slice(0, 30)));
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
  // The switcher dock (internal/ui/switcher_dock.go), open: in this file's
  // phone context (the touch layout, asserted) the bar's view button is its
  // handle; its grip, its close button, its tabs and its rows are measured with
  // everything else on the page. Two other agents visited first, so Recent
  // has rows.
  await page.goto('/agents/release-helper');
  await page.goto('/agents/notes-tidy');
  await page.goto('/agents/docs-sweeper');
  await settle(page);
  expect(await page.evaluate(() => matchMedia('(pointer: coarse)').matches), 'the touch layout').toBe(true);
  await page.locator('#switcher-toggle').tap();
  await expect.poll(() => page.evaluate(() => (document.getElementById('switcher-dock') as HTMLDialogElement).open)).toBe(true);
  expect(await page.locator('#switcher-dock #dock-list a[data-dock-row]').count(), 'the dock has rows to measure').toBeGreaterThan(0);
  await check('switcher dock (open)');
  await page.keyboard.press('Escape');
  await page.goto('/tasks');
  await settle(page);
  await page.click('#fab-tasks');
  await expect(page.locator('#task-modal textarea[name="body"]')).toBeVisible();
  await check('new-task sheet');
  // The dispatching server (an agent provisioner wired — the shape that drew
  // the dispatch "+", the card's Dispatch and the dispatch sheet until
  // docs/design/chief-dispatch.md phase 2 deleted them): its Tasks board and
  // its sidebar drawer.
  await signInDispatch(page);
  await check('tasks (dispatching server)');
  await page.click('#sidebar-open');
  await check('sidebar drawer (dispatching server)');
  expect(measured, 'the audit measured real controls').toBeGreaterThan(100);
});

test('a sheet keeps its primary action on screen with Advanced open', async ({ page }) => {
  await page.setViewportSize({ width: 360, height: 640 });
  for (const [path, open, label] of [
    ['/tasks', (p: typeof page) => p.click('#fab-tasks'), 'Save task'],
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

// docs/design/chief-dispatch.md phase 2 deleted the dispatch form with nothing
// in its place (D8): no server offers a dispatch, whether or not it has an
// agent provisioner — no "+" beside the sidebar's Agents label and no "not set
// up" hint there, no Dispatch on a task card — and the sheet's route is gone.
// The shared server is the docker compose install's shape (no provisioner);
// the dispatching server is the one that used to draw all three.
test('no server offers a dispatch, with or without an agent provisioner', async ({ page }) => {
  for (const [label, origin] of [
    ['no provisioner', ''],
    ['provisioner wired', dorigin],
  ] as const) {
    if (origin) await signInDispatch(page);
    await page.goto(origin + '/tasks');
    await settle(page);
    await expect(page.locator('[id^="task-"][data-task-status]').first(), `${label}: the board drew task cards`).toBeVisible();
    await expect(page.locator('button[aria-label="Dispatch agent for this task"]'), `${label}: no card Dispatch`).toHaveCount(0);
    await page.click('#sidebar-open');
    await expect(page.locator('#sidebar-agents-label'), `${label}: the head row is drawn`).toBeVisible();
    await expect(page.locator('#sidebar-deploy'), `${label}: no "+"`).toHaveCount(0);
    await expect(page.locator('#sidebar [data-dispatch-off-hint]'), `${label}: no "not set up" hint`).toHaveCount(0);
    const sheet = await page.request.get(origin + '/ui/agents/new');
    expect(sheet.status(), `${label}: GET /ui/agents/new`).toBe(404);
  }
});
