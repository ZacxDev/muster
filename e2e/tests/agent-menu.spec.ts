import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, repoRoot } from '../lib/env';

// THE AGENT'S ⋯ MENU, ON ITS OWN PAGE (internal/ui/agent_actions.go
// agentActionsMenu, at the start of the header's second row, beside the
// status strip — the row's right end is left as it was, where the update pill
// slides). The Agents list's
// cards carried this menu until the list page was retired; what it guarded
// moved with it:
//
// 🔴 THE ⋯ MENU MUST BE FULLY ON SCREEN AND ON TOP — ASSERTED BY HIT-TESTING,
// NOT BY VISIBILITY. The operator report: "Agent card three dots context menu is
// cut off by card bounds". The menu used to be an absolute dropdown inside the
// card, and the card is overflow-hidden, so the part below the card's edge was
// clipped. Playwright's toBeVisible() is TRUE for a clipped element (it has a
// box), so it cannot see this bug; what can is asking the page which element
// is at each item's centre. A clipped item answers with whatever is under it
// instead of itself. On the page the trigger is near the header's left (the
// menu opens under it, left edges aligned), inside a sticky, blurred header —
// at a 412px phone and at a desktop width. The Delete step is opened
// too, because it is what GROWS the menu.
//
// And what the menu DOES (the card's actions, moved): Rename opens its form
// and Save renames the agent (the page reloads with the new name); the chief
// has no Delete. Start/Stop and a real Delete need an agent runtime, which the
// shared server has none of: agent-cards.spec.ts drives them on its own server.

const stamp = Date.now().toString(36);
const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

const PAGE = '/agents/docs-sweeper';
const trigger = (page: Page) => page.locator('#agent-actions-open');
const menu = (page: Page) => page.locator('#agent-actions');
const delSummary = (page: Page) => menu(page).locator('summary[data-agent-action="delete"]');
const confirmDel = (page: Page) => menu(page).locator('[data-agent-action="confirm-delete"]');

async function open(page: Page) {
  await page.goto(PAGE);
  await expect(trigger(page)).toBeVisible();
}

// hitReport reports, for every visible menu control, whether the element at
// its centre is the control itself (or inside it), and whether its box is
// inside the viewport.
async function hitReport(page: Page, labels: string[]) {
  return page.evaluate((labels) => {
    const m = document.getElementById('agent-actions')!;
    const out: string[] = [];
    let found = 0;
    for (const el of m.querySelectorAll('button, summary') as NodeListOf<HTMLElement>) {
      const label = (el.textContent || '').trim();
      if (!labels.includes(label)) continue;
      const r = el.getBoundingClientRect();
      if (r.width === 0 || r.height === 0) continue;
      found++;
      const inView = r.left >= 0 && r.top >= 0 && r.right <= document.documentElement.clientWidth && r.bottom <= window.innerHeight;
      const x = r.left + r.width / 2, y = r.top + r.height / 2;
      const hit = document.elementFromPoint(x, y);
      const ok = !!hit && (hit === el || el.contains(hit));
      if (!inView || !ok) {
        out.push(`${label}: inView=${inView} box=${Math.round(r.left)},${Math.round(r.top)},${Math.round(r.right)},${Math.round(r.bottom)} hit=${hit ? hit.tagName.toLowerCase() + (hit.id ? '#' + hit.id : '') + '.' + String(hit.className).slice(0, 40) : 'null'}`);
      }
    }
    return { found, problems: out };
  }, labels);
}

// placement is where the open menu is relative to its trigger: 'closed',
// 'beside' (left edges aligned — data-menu-align="start" — just below or
// above it) or 'stranded: …'.
const placementJS = () => {
  const m = document.getElementById('agent-actions');
  if (!m || !m.matches(':popover-open')) return 'closed';
  const t = document.getElementById('agent-actions-open')!.getBoundingClientRect();
  const r = m.getBoundingClientRect();
  const beside = Math.abs(r.left - t.left) <= 1 && (Math.abs(r.top - t.bottom) <= 8 || Math.abs(t.top - r.bottom) <= 8);
  return beside ? 'beside' : `stranded: menu ${Math.round(r.left)},${Math.round(r.top)} trigger ${Math.round(t.left)},${Math.round(t.top)}`;
};

for (const vp of [
  { name: 'phone 412x915', width: 412, height: 915 },
  { name: 'desktop 1280x800', width: 1280, height: 800 },
]) {
  test.describe(vp.name, () => {
    test.use({ viewport: { width: vp.width, height: vp.height } });

    test("the agent page's ⋯ menu is fully visible, on top and beside its trigger", async ({ page }) => {
      await open(page);
      // The trigger is on the header's SECOND row, at its start beside the
      // status strip: the first row is the title's.
      const first = (await page.locator('[data-agent-title]').boundingBox())!;
      const t = (await trigger(page).boundingBox())!;
      expect(t.y, 'the ⋯ is below the title row').toBeGreaterThanOrEqual(first.y + first.height - 1);
      expect([Math.round(t.width), Math.round(t.height)], 'a 44px target').toEqual([44, 44]);
      await trigger(page).click();
      // Polled, not sampled once: placement runs in the browser's rendering
      // steps, and a probe in the same task as the click can read the frame
      // before it. A clipped menu never becomes hittable, so a poll cannot turn
      // a clipped one green.
      const settled = (labels: string[], n: number) =>
        expect.poll(async () => {
          const r = await hitReport(page, labels);
          return r.found === n ? r.problems : [`found ${r.found} items, want ${n}`];
        }, { timeout: 3000 });
      // Rename, the lifecycle item and Delete: all reachable where they are drawn.
      await settled(['Rename', 'Start', 'Stop', 'Delete'], 3).toEqual([]);
      await expect.poll(() => page.evaluate(placementJS)).toBe('beside');

      // The two-step delete GROWS the menu; the confirm must be reachable too.
      await delSummary(page).click();
      await settled(['Rename', 'Start', 'Stop', 'Delete', 'Confirm delete'], 4).toEqual([]);
    });

    // 🔴 THE HEADER RE-RENDERS UNDER AN OPEN MENU THROUGH ITS OWN hx-trigger.
    // agents:changed (a lifecycle action's answer), the status poll's
    // agents:awaiting and sse:agent.changed each re-fetch the strip. The ⋯
    // menu is NOT part of that answer: it is drawn once with the page (its
    // Start/Stop follows the strip's status by a stylesheet rule), so an open
    // menu — an armed "Confirm delete" included — is never replaced under the
    // reader, before or after it closes. The refresh is fired by the strip's
    // own trigger event, the real path.
    test('a header refresh under an open menu keeps it open beside its trigger, and never replaces it', async ({ page }) => {
      await open(page);
      await trigger(page).click();
      await expect.poll(() => page.evaluate(placementJS)).toBe('beside');
      await delSummary(page).click();
      await page.evaluate(() => {
        (document.getElementById('agent-actions') as unknown as { __old: boolean }).__old = true;
        (document.getElementById('agent-status') as unknown as { __old: boolean }).__old = true;
      });
      const refreshed = page.waitForResponse((r) => /\/ui\/agents\/\d+\/header$/.test(new URL(r.url()).pathname));
      await page.evaluate(() => document.body.dispatchEvent(new CustomEvent('agents:changed', { bubbles: true })));
      await refreshed;
      await page.waitForTimeout(300);
      expect(await page.evaluate(() => !!(document.getElementById('agent-actions') as unknown as { __old?: boolean }).__old), 'the open menu was not replaced').toBe(true);
      expect(await page.evaluate(placementJS), 'still open, beside its trigger').toBe('beside');
      await expect(confirmDel(page), 'the armed Confirm delete is still there').toBeVisible();
      // POSITIVE CONTROL: the refresh did redraw the strip around it.
      expect(await page.evaluate(() => !!(document.getElementById('agent-status') as unknown as { __old?: boolean } | null)?.__old), 'the strip was redrawn').toBe(false);
      await page.keyboard.press('Escape');
      await trigger(page).click();
      await expect.poll(() => page.evaluate(placementJS), { timeout: 3000 }).toBe('beside');
      await expect(delSummary(page)).toBeVisible();
    });

    // 🔴 NO FRAME WITH THE MENU SHOWN AWAY FROM ITS TRIGGER — NOT EVEN THE FIRST.
    // The popover opens synchronously on the click, but the `toggle` event that
    // places it is a later task, and a rendering update runs between the two:
    // measured (on the card), a requestAnimationFrame callback saw the menu open
    // and painted at 0,0 before it jumped to its trigger. So the first frame
    // after the click is sampled from INSIDE the page: by then the menu must be
    // not yet open, not yet visible, or already beside its trigger.
    test('the menu is never shown unplaced, even on its first frame', async ({ page }) => {
      await open(page);
      const arm = () =>
        page.evaluate(() => {
          const w = window as unknown as { __firstFrame: string };
          w.__firstFrame = 'no frame sampled';
          const t = document.getElementById('agent-actions-open')!;
          t.addEventListener(
            'click',
            () => {
              requestAnimationFrame(() => {
                const m = document.getElementById('agent-actions')!;
                if (!m.matches(':popover-open')) return void (w.__firstFrame = 'not open yet');
                if (getComputedStyle(m).visibility !== 'visible') return void (w.__firstFrame = 'open, not visible yet');
                const tr = t.getBoundingClientRect();
                const r = m.getBoundingClientRect();
                const beside = Math.abs(r.left - tr.left) <= 1 && (Math.abs(r.top - tr.bottom) <= 8 || Math.abs(tr.top - r.bottom) <= 8);
                w.__firstFrame = beside ? 'beside' : `SHOWN UNPLACED at ${Math.round(r.left)},${Math.round(r.top)}`;
              });
            },
            { once: true },
          );
        });
      await arm();
      await trigger(page).click();
      await expect(delSummary(page)).toBeVisible();
      const read = () => page.evaluate(() => (window as unknown as { __firstFrame: string }).__firstFrame);
      const ok = ['not open yet', 'open, not visible yet', 'beside'];
      expect(ok, 'first open').toContain(await read());

      // And on a REOPEN after the trigger has moved: a menu that kept its last
      // position would be shown for a frame where the trigger USED to be.
      await page.keyboard.press('Escape');
      await page.evaluate(() => {
        (document.querySelector('header') as HTMLElement).style.paddingTop = '120px';
      });
      await arm();
      await trigger(page).click();
      await expect(delSummary(page)).toBeVisible();
      expect(ok, 'reopen after the trigger moved').toContain(await read());
    });

    // Closing the menu resets it: reopening starts at step 1 (no armed "Confirm
    // delete" waiting from last time).
    test('reopening the menu starts at step 1 of the delete', async ({ page }) => {
      await open(page);
      await trigger(page).click();
      await delSummary(page).click();
      await expect(confirmDel(page)).toBeVisible();
      await page.keyboard.press('Escape');
      await trigger(page).click();
      await expect(delSummary(page)).toBeVisible();
      await expect(confirmDel(page)).toBeHidden();
    });

    // Start/Stop post and re-fetch the header; the menu must not linger over
    // the result. The request is answered here (as the server does: an empty
    // 200 with agents:changed), so no seeded agent is started or stopped.
    test('a menu action closes the menu', async ({ page }) => {
      let posted = 0;
      await page.route('**/agents/*/st*', (r) => {
        posted++;
        return r.fulfill({ status: 200, headers: { 'HX-Trigger': 'agents:changed' }, body: '' });
      });
      await open(page);
      await trigger(page).click();
      // The lifecycle item the status calls for (both are drawn; the
      // stylesheet shows one).
      const action = menu(page).locator('[data-agent-action="Start"]:visible, [data-agent-action="Stop"]:visible');
      await expect(action).toBeVisible();
      await action.click();
      await expect(action).toBeHidden();
      expect(posted, 'the action posted').toBe(1);
    });

    test('Escape and an outside tap close the menu', async ({ page }) => {
      await open(page);
      const item = delSummary(page);
      await trigger(page).click();
      await expect(item).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(item).toBeHidden();
      await trigger(page).click();
      await expect(item).toBeVisible();
      await page.mouse.click(5, vp.height / 2);
      await expect(item).toBeHidden();
    });

    // RENAME, moved from the card: the menu's Rename opens the form, Save posts
    // the display name and reloads the page, whose title then reads it.
    test('Rename opens its form, and Save renames the agent', async ({ page }) => {
      const name = `e2e-menu-rename-${vp.width}-${stamp}`;
      const id = fix('agent', '1', name, `e2e-menu-rename-token-not-a-real-one-${vp.width}-${stamp}`);
      try {
        await page.goto('/agents/' + name);
        await expect(page.locator('[data-agent-title] h1')).toHaveAttribute('title', name);
        await trigger(page).click();
        await menu(page).locator('[data-agent-action="rename"]').click();
        const form = page.locator('#agent-rename');
        await expect(form).toBeVisible();
        const input = form.locator('input[name="name"]');
        await expect(input).toHaveValue(name);
        await input.fill('Renamed by the menu ' + stamp);
        await page.evaluate(() => ((window as unknown as { __noReload: number }).__noReload = 1));
        const posted = page.waitForResponse((r) => r.url().endsWith(`/agents/${id}/name`) && r.request().method() === 'POST');
        const reloaded = page.waitForEvent('framenavigated', (f) => f === page.mainFrame());
        await form.getByRole('button', { name: 'Save', exact: true }).click();
        expect((await posted).status()).toBe(200);
        // The answer reloads the page: the marker is gone, the title is new.
        await reloaded;
        await page.waitForLoadState('load');
        expect(await page.evaluate(() => (window as unknown as { __noReload?: number }).__noReload ?? 0), 'the page reloaded').toBe(0);
        await expect(page.locator('[data-agent-title] h1')).toHaveAttribute('title', 'Renamed by the menu ' + stamp);
        await expect(page).toHaveTitle('Renamed by the menu ' + stamp + ' · muster');
      } finally {
        fix('rmagent', '1', name);
      }
    });

    // The chief cannot be deleted from its menu (the server refuses it too):
    // no Delete, while an ordinary agent's menu has one (the control).
    test("the chief's menu has no Delete", async ({ page }) => {
      const name = `e2e-menu-chief-${vp.width}-${stamp}`;
      fix('chief', '1', name, `e2e-menu-chief-token-not-a-real-one-${vp.width}-${stamp}`);
      try {
        await page.goto('/agents/' + name);
        await trigger(page).click();
        await expect(menu(page).locator('[data-agent-action="rename"]')).toBeVisible();
        await expect(delSummary(page)).toHaveCount(0);
        await open(page);
        await trigger(page).click();
        await expect(delSummary(page), 'control: an ordinary agent has Delete').toBeVisible();
      } finally {
        fix('rmagent', '1', name);
      }
    });
  });
}
