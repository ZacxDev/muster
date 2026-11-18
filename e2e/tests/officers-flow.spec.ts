import { execFileSync } from 'node:child_process';
import { Page, Response } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { authState, baseURL, dsn, hookToken, repoRoot } from '../lib/env';

// THE WHOLE OFFICER FLOW (officers redesign O-F), on the shared server — a
// database, so the built-in officer store is on; no Kubernetes agent plane and
// no mirror hub, so an officer created here is declared but runs nowhere, and
// every item it is given STAYS in its queue, which is what lets the Run now
// dedupe be read back.
//
// officers.spec.ts already walks a Blank officer through the wizard, the list,
// pause/resume and delete, keeps a host officer's machine through an Edit, and
// checks each template's starting values. This file does NOT repeat those: it
// walks one officer, started from a non-blank template, through EVERY step a
// person takes — each wizard step changed, Run now twice, Live, pause, resume,
// an edit, a schedule change, a refused and an accepted delete — and after each
// step reads the SERVER's state back through the same JSON admin API the CLI
// uses (the hook token acts as the operator, the signed-in user), never only
// the words the page drew. Then the Session minder template, and the refusal
// of its permission on a team's officer.
//
// At a desktop (1280×800) and a phone (390×844), as officers.spec.ts runs: the
// viewport set here per size, never inherited from the config, and no page in
// the flow scrolls sideways at either.

const sizes = [
  { name: 'desktop', viewport: { width: 1280, height: 800 } },
  { name: 'phone', viewport: { width: 390, height: 844 } },
];

// --- the server's state, read as the CLI reads it ---------------------------------

async function api(path: string, init: RequestInit = {}): Promise<globalThis.Response> {
  return fetch(`${baseURL}${path}`, { ...init, headers: { Authorization: `Bearer ${hookToken}`, ...(init.headers ?? {}) } });
}

async function apiJSON(path: string): Promise<any> {
  const r = await api(path);
  expect(r.status, `GET ${path}`).toBe(200);
  return r.json();
}

const enc = encodeURIComponent;

// declared is officer id as GET /api/officers/declared/{id} answers it, or
// null when that answers 404.
async function declared(id: string): Promise<any | null> {
  const r = await api(`/api/officers/declared/${enc(id)}`);
  if (r.status === 404) return null;
  expect(r.status, `GET declared ${id}`).toBe(200);
  return r.json();
}

// officerType is the type officer id's def names (its charter and policy).
async function officerType(id: string): Promise<{ charter: string; caps: string[] }> {
  const { defs } = await apiJSON('/api/officers/defs');
  const def = (defs as any[]).find((d) => d.officer === id);
  expect(def, `officer ${id} has a def`).toBeTruthy();
  const t = await apiJSON(`/api/officers/types/${enc(def.type_key)}`);
  // A form posts a textarea's line breaks as CRLF (the HTML form encoding),
  // and the server stores what was posted: read back as the person typed it.
  // (builtin.TypeInfo: policy is an object, absent when {}.)
  return { charter: String(t.charter).replace(/\r\n/g, '\n'), caps: t.policy?.capabilities ?? [] };
}

async function items(id: string): Promise<any[]> {
  return (await apiJSON(`/api/officers/defs/${enc(id)}/items`)).items;
}

async function schedules(id: string): Promise<any[]> {
  return (await apiJSON(`/api/officers/defs/${enc(id)}/schedules`)).schedules;
}

// mineOnServer is every officer id the Officers list draws in "yours": the
// declared ones (env and database) and each def neither declares.
async function mineOnServer(): Promise<Set<string>> {
  const ids = new Set<string>();
  for (const o of (await apiJSON('/api/officers/declared')).officers as any[]) ids.add(o.id);
  for (const d of (await apiJSON('/api/officers/defs')).defs as any[]) ids.add(d.officer);
  return ids;
}

async function removeOfficer(id: string) {
  await api(`/api/officers/declared/${enc(id)}`, { method: 'DELETE' });
}

// --- the page -----------------------------------------------------------------------

async function noSidewaysScroll(page: Page) {
  const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(over, 'the page scrolls sideways').toBeLessThanOrEqual(0);
}

// submitTo clicks the submit button of the form whose action ends in suffix
// (inside scope) and answers the POST's response, once the page it redirects
// to has loaded. The response is waited on, not the URL: a second tap lands
// on the URL the first already shows.
async function submitTo(page: Page, scope: string, suffix: string): Promise<Response> {
  const posted = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname.endsWith(suffix));
  await page.locator(`${scope} form[action$="${suffix}"] button[type=submit]`).click();
  const resp = await posted;
  await page.waitForLoadState('load');
  return resp;
}

const notice = (page: Page) => new URL(page.url()).searchParams.get('notice');

const step = (page: Page, n: number) => page.locator(`fieldset[data-step="${n}"]`);

for (const size of sizes) {
  test.describe(`the officer flow at ${size.name} size`, () => {
    test.use({ viewport: size.viewport });
    const name = `E2E flow ${size.name}`;
    const id = `e2e-flow-${size.name}:main`;
    const minderID = `e2e-minder-${size.name}:main`;
    test.beforeEach(async () => { await removeOfficer(id); await removeOfficer(minderID); });
    test.afterEach(async () => { await removeOfficer(id); await removeOfficer(minderID); });

    test('from the QA template through every step, Run now, Live, pause, edit, schedule and delete — each read back from the server', async ({ page }) => {
      test.setTimeout(120_000);
      // 1. The list: its empty state exactly when the server holds none of
      // the person's officers, and the panel of where one can run.
      await page.goto('/officers');
      const before = await mineOnServer();
      await expect(page.locator('#officers-mine [data-officer]')).toHaveCount(before.size);
      await expect(page.locator('#officers-empty')).toHaveCount(before.size === 0 ? 1 : 0);
      const where = page.locator('#officers-runtimes');
      await expect(where).toBeVisible();
      await expect(where.locator('[data-runtime="pod"]')).toHaveAttribute('data-available', 'false');
      await expect(where.locator('[data-runtime="host"]')).toHaveAttribute('data-available', 'false');
      await expect(where.locator('#officers-runtimes-auto')).toBeVisible();
      await noSidewaysScroll(page);
      await page.click('#officer-new');
      await page.waitForURL(/\/officers\/new$/);

      // 2. Start from QA: the form is redrawn at step 2 with its values.
      const next = page.locator('[data-wizard-next]');
      await expect(step(page, 1)).toBeVisible();
      await step(page, 1).locator('input[name=template][value="qa"]').check();
      await next.click();
      await expect(page.locator('#officer-form')).toHaveAttribute('data-template', 'qa');
      await expect(step(page, 2)).toBeVisible();
      await noSidewaysScroll(page);
      //  Name and instructions: both changed.
      const charter = `${await page.locator('#officer-charter').inputValue()}\n\nE2E flow ${size.name}: test only the officers pages.`;
      await page.fill('#officer-name', name);
      await page.fill('#officer-charter', charter);
      await next.click();
      //  Where it runs: auto (the only choice here), and a repository typed.
      await expect(step(page, 3)).toBeVisible();
      await expect(step(page, 3).locator('input[name=runtime][value=auto]')).toBeChecked();
      await page.fill('#officer-repo', 'example-org/example-repo');
      await noSidewaysScroll(page);
      await next.click();
      //  When it works: the template's weekdays, changed to every day.
      await expect(step(page, 4)).toBeVisible();
      await expect(step(page, 4).locator('input[name=wake][value=schedule]')).toBeChecked();
      await expect(page.locator('#officer-preset')).toHaveValue('weekdays');
      await page.selectOption('#officer-preset', 'daily');
      await next.click();
      //  What it may do: nothing by default; contacts ticked.
      await expect(step(page, 5)).toBeVisible();
      await expect(step(page, 5).locator('input[name=cap]:checked')).toHaveCount(0);
      await step(page, 5).locator('input[name=cap][value=contacts]').check();
      await noSidewaysScroll(page);
      await next.click();
      //  Review: the answers; Live chosen, then back to Shadow.
      await expect(step(page, 6)).toBeVisible();
      const review = page.locator('#wizard-review');
      await expect(review).toContainText(name);
      await expect(review).toContainText('example-org/example-repo');
      await expect(review).toContainText('Every day at 09:00 UTC');
      await step(page, 6).locator('input[name=mode][value=live]').check();
      await step(page, 6).locator('input[name=mode][value=shadow]').check();
      await noSidewaysScroll(page);
      await page.click('#officer-submit');

      // 3. Its page — and the server holds every value chosen.
      await page.waitForURL((u) => u.pathname === `/officers/${id}` && u.searchParams.get('notice') === 'created');
      await expect(page.locator('#officer-head')).toHaveAttribute('data-officer', id);
      const made = await declared(id);
      expect(made, 'the officer exists on the server').not.toBeNull();
      expect(made.mode, 'created in Shadow').toBe('shadow');
      expect(made.paused, 'created switched on').toBe(false);
      expect(made.name, 'the name typed').toBe(name);
      expect(made.runtime, 'where it runs').toBe('auto');
      expect(made.repo, 'the repository typed').toBe('example-org/example-repo');
      const t0 = await officerType(id);
      expect(t0.charter, 'the instructions typed').toBe(charter);
      expect(t0.caps, 'the permission ticked').toEqual(['contacts']);
      const s0 = await schedules(id);
      expect(s0.map((s) => [s.name, s.cron, s.enabled]), 'the schedule chosen').toEqual([['pass', '0 9 * * *', true]]);
      await expect(page.locator('#officer-actions form[action$="/mode"] input[name=mode]')).toHaveValue('live');
      await expect(page.locator('#officer-schedules [data-schedule="pass"]')).toBeVisible();
      await expect(page.locator('#officer-queue-empty')).toBeVisible();
      await noSidewaysScroll(page);

      // 4. Run now: one manual.run item; a second tap while it waits adds none.
      const run1 = await submitTo(page, '#officer-run', '/run');
      expect(run1.status(), 'Run now redirects').toBe(303);
      const q1 = (await items(id)).filter((it) => it.kind === 'manual.run');
      expect(q1.length, 'one Run now queues exactly one manual.run item').toBe(1);
      expect(q1[0].status, 'the item waits').toBe('open');
      await expect.poll(() => notice(page)).toBe('queued');
      await expect(page.locator('#officer-queue [data-item]')).toHaveCount(1);
      await submitTo(page, '#officer-run', '/run');
      const q2 = (await items(id)).filter((it) => it.kind === 'manual.run');
      expect(q2.map((it) => it.id), 'a second Run now while the first waits adds no item').toEqual([q1[0].id]);
      await expect.poll(() => notice(page)).toBe('already-queued');
      await expect(page.locator('#officer-queue [data-item]')).toHaveCount(1);

      // 5. Live; then pause (still Live) and resume.
      await submitTo(page, '#officer-actions', '/mode');
      expect((await declared(id)).mode, 'Switch to Live makes it live on the server').toBe('live');
      await expect.poll(() => notice(page)).toBe('mode-live');
      await expect(page.locator('#officer-actions form[action$="/mode"] input[name=mode]')).toHaveValue('shadow');
      await submitTo(page, '#officer-run', '/pause');
      const paused = await declared(id);
      expect(paused.paused, 'Pause switches it off on the server').toBe(true);
      expect(paused.mode, 'a pause keeps it Live').toBe('live');
      await expect(page.locator('#officer-run form[action$="/resume"]')).toBeVisible();
      await submitTo(page, '#officer-run', '/resume');
      const resumed = await declared(id);
      expect(resumed.paused, 'Resume switches it back on').toBe(false);
      expect(resumed.mode, 'a resume keeps it Live').toBe('live');
      await expect(page.locator('#officer-run form[action$="/pause"]')).toBeVisible();

      // 6. Edit: only its own steps (no "Start from", no "When it works", no
      // stepping); the instructions changed and saved.
      await page.click('#officer-edit');
      await page.waitForURL((u) => u.pathname === `/officers/${id}/edit`);
      await expect(page.locator('#officer-form')).toHaveAttribute('data-wizard', 'false');
      const drawn = await page.locator('#officer-form fieldset[data-step]').evaluateAll((els) => els.map((e) => (e as HTMLElement).dataset.step));
      expect(drawn, 'the edit form draws only its steps').toEqual(['2', '3', '5', '6']);
      for (const n of [2, 3, 5, 6]) await expect(step(page, n)).toBeVisible();
      await expect(page.locator('#wizard-progress')).toHaveCount(0);
      await expect(page.locator('#officer-name')).toHaveValue(name);
      await expect(page.locator('#officer-repo')).toHaveValue('example-org/example-repo');
      await expect(page.locator('input[name=cap][value=contacts]')).toBeChecked();
      await expect(page.locator('input[name=mode][value=live]')).toBeChecked();
      await noSidewaysScroll(page);
      const charter2 = `Read the newest change and report what you tested (${size.name}). Never push.`;
      await page.fill('#officer-charter', charter2);
      await page.click('#officer-submit');
      await page.waitForURL((u) => u.pathname === `/officers/${id}` && u.searchParams.get('notice') === 'saved');
      const t1 = await officerType(id);
      expect(t1.charter, 'the edited instructions are saved').toBe(charter2);
      expect(t1.caps, 'an edit keeps the permission').toEqual(['contacts']);
      const edited = await declared(id);
      expect(edited.mode, 'an edit that leaves the mode keeps it Live').toBe('live');
      expect(edited.repo, 'an edit keeps the repository').toBe('example-org/example-repo');
      //  Its schedule (on its page: the edit form has no "When it works"):
      //  the wizard's "pass" replaced, every day -> every hour.
      await page.selectOption('#schedule-preset', 'hourly');
      await page.fill('#schedule-name', 'pass');
      await submitTo(page, '#officer-schedules', '/schedules');
      await expect.poll(() => notice(page)).toBe('schedule-saved');
      const s1 = await schedules(id);
      expect(s1.map((s) => [s.name, s.cron, s.enabled]), 'the schedule is replaced, not added').toEqual([['pass', '0 * * * *', true]]);
      await expect(page.locator('#officer-schedules [data-schedule]')).toHaveCount(1);
      await noSidewaysScroll(page);

      // 7. Delete: refused without its id typed; done with it — gone from the
      // server, then from the list.
      await page.fill('#officer-delete-confirm', 'not-its-id');
      const refused = await submitTo(page, '#officer-delete', '/delete');
      expect(refused.status(), 'a delete without its id typed is refused').toBe(400);
      expect(await declared(id), 'a refused delete leaves it').not.toBeNull();
      await page.fill('#officer-delete-confirm', id);
      await submitTo(page, '#officer-delete', '/delete');
      expect(await declared(id), 'Delete removes it from the server').toBeNull();
      expect((await apiJSON('/api/officers/defs')).defs.map((d: any) => d.officer), 'Delete removes its def').not.toContain(id);
      await expect.poll(() => new URL(page.url()).pathname).toBe('/officers');
      await expect.poll(() => notice(page)).toBe('deleted');
      await expect(page.locator(`[data-officer="${id}"]`)).toHaveCount(0);
      await expect(page.locator('#officers-mine [data-officer]')).toHaveCount(before.size);
      await noSidewaysScroll(page);
    });

    test('the Session minder template creates an officer with the session_minder permission', async ({ page }) => {
      await page.goto('/officers/new');
      await step(page, 1).locator('input[name=template][value="session-minder"]').check();
      const next = page.locator('[data-wizard-next]');
      await next.click();
      await expect(page.locator('#officer-form')).toHaveAttribute('data-template', 'session-minder');
      await page.fill('#officer-name', `E2E minder ${size.name}`);
      for (const n of [2, 3, 4, 5]) {
        await expect(step(page, n)).toBeVisible();
        await next.click();
      }
      await expect(step(page, 6)).toBeVisible();
      await page.click('#officer-submit');
      await page.waitForURL((u) => u.pathname === `/officers/${minderID}` && u.searchParams.get('notice') === 'created');
      const t = await officerType(minderID);
      expect(t.caps, 'the session_minder permission is set').toEqual(['session_minder']);
      expect((await declared(minderID)).mode, 'created in Shadow').toBe('shadow');
      expect(await schedules(minderID), 'Session minder works only when given work').toEqual([]);
      await noSidewaysScroll(page);
    });
  });
}

// A TEAM's officer (the operator its manager): the session_minder permission
// is refused at create with the error drawn on its own step, and nothing is
// written; the same officer without it is created — so the refusal is the
// permission's, not a team officer refused whole.
//
// The shared server has no GoTrue, so no /admin/teams; e2e/mkteam writes the
// rows users.CreateTeam would, and the session reads its memberships on every
// request.
function mkteam(...args: string[]): string {
  return execFileSync('go', ['run', './e2e/mkteam', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();
}

for (const size of sizes) {
  test.describe(`a team officer at ${size.name} size`, () => {
    test.use({ viewport: size.viewport });
    let team = '';
    test.beforeAll(async () => {
      test.setTimeout(120_000);
      team = mkteam('create', `E2E officer team ${size.name}`);
      expect(team, 'the team was made').toMatch(/^\d+$/);
    });
    // 🔴 A red run must not leave the team owning officer rows: officer_defs and
    // officer_types reference users(id) with no cascade, so a leftover would
    // fail `mkteam remove` here AND every later `seed --accounts-only` in the
    // leg, turning one red into several. Delete what it still owns the way a
    // user would (the page's Delete), then remove the team.
    test.afterAll(async ({ browser }) => {
      if (!team) return;
      if (mkteam('officers', team) !== '0 0') {
        const ctx = await browser.newContext({ storageState: authState, baseURL });
        try {
          const page = await ctx.newPage();
          await page.goto(`/officers/${enc(`e2e-team-minder-${size.name}:main`)}?owner=${team}`);
          await page.fill('#officer-delete-confirm', `e2e-team-minder-${size.name}:main`);
          await submitTo(page, '#officer-delete', '/delete');
        } finally {
          await ctx.close();
        }
      }
      mkteam('remove', team);
    });

    test('refuses the session_minder permission with the error on its step, and writes nothing', async ({ page }) => {
      test.setTimeout(120_000);
      const tid = `e2e-team-minder-${size.name}:main`;
      await page.goto('/officers');
      await expect(page.locator(`[data-officers-team="${team}"]`)).toBeVisible();
      await page.goto(`/officers/new?owner=${team}`);
      await step(page, 1).locator('input[name=template][value="session-minder"]').check();
      const next = page.locator('[data-wizard-next]');
      await next.click();
      await expect(page.locator('#officer-owner')).toHaveValue(team);
      await page.fill('#officer-name', `E2E team minder ${size.name}`);
      for (const n of [2, 3, 4, 5]) {
        await expect(step(page, n)).toBeVisible();
        await next.click();
      }
      await expect(step(page, 6)).toBeVisible();
      const posted = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/officers/new');
      await page.click('#officer-submit');
      expect((await posted).status(), 'the create is refused').toBe(400);
      await page.waitForLoadState('load');
      await expect(page.locator('#officer-form')).toHaveAttribute('data-first-step', '5');
      await expect(step(page, 5)).toBeVisible();
      const err = page.locator('[data-field-error="cap"]');
      await expect(err).toBeVisible();
      await expect(err).toContainText('session_minder');
      await expect(step(page, 5).locator('input[name=cap][value=session_minder]')).toBeChecked();
      expect(mkteam('officers', team), 'a refused create writes no def and no type').toBe('0 0');
      await noSidewaysScroll(page);

      // The control: the same answers without the permission are created.
      await step(page, 5).locator('input[name=cap][value=session_minder]').uncheck();
      await next.click();
      await page.click('#officer-submit');
      await page.waitForURL((u) => u.pathname === `/officers/${tid}` && u.searchParams.get('owner') === team && u.searchParams.get('notice') === 'created');
      expect(mkteam('officers', team), 'without the permission it is created').toBe('1 1');
      await page.fill('#officer-delete-confirm', tid);
      await submitTo(page, '#officer-delete', '/delete');
      expect(mkteam('officers', team), 'and deleted').toBe('0 0');
    });
  });
}
