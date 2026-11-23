import { execFileSync } from 'node:child_process';
import { Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { dsn, hookToken, officerPageRedesignConnectPort, officerPageRedesignPort, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// THE OFFICER PAGE REDESIGN (the operator), in a real browser against
// a real muster-server, at a phone (390×844, touch) and a desktop (1440×900):
//
//   - the page never scrolls sideways; it draws no Shadow / Live button and
//     no machine-user warning;
//   - the spend bar reads "$8.50 of $10 today", amber, with a Raise button a
//     thumb can hit (≥44px), whose tap raises TODAY's limit to $20;
//   - the Instructions section: open it, change them, Save (Version 2), then
//     Save and refresh (it is not running here, so its next start is new);
//   - an ask the officer raised: its answer box is on a line of its own, at
//     least 90% of the card's inner width, with Answer BELOW it — on the
//     officer's page and on the Asks page.
//
// The officer is MUSTER_OFFICERS's (a builtin officer with a $10 daily
// budget: a DB-declared one has none), its type and def put through the admin
// API; its spend and its ask are the fixture's (e2e/officerfix spend, ask),
// because the harness runs no officer instance.
//
// 🔴 RED at origin/main: no #officer-spend, no #officer-charter, a Shadow /
// Live button, and the answer box shares its row with Answer.

const origin = `http://localhost:${officerPageRedesignPort}`;
const stamp = Date.now().toString(36);
const vps = [
  { name: 'phone 390×844', key: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', key: 'desktop', use: { viewport: { width: 1440, height: 900 } } },
];
const typeOf = (key: string) => `e2erd${key}${stamp}`;
const idOf = (key: string) => `${typeOf(key)}:main`;
let server: Running | undefined;

const CHARTER_1 = 'Read the triage queue each morning and label every item.';
const CHARTER_2 = 'Read the triage queue each morning; label every item; ask before closing one.';
const QUESTION = 'Which repository should it read first this week?';

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/officerfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

async function api(method: string, path: string, body?: unknown) {
  const r = await fetch(`${origin}${path}`, {
    method,
    headers: { Authorization: `Bearer ${hookToken}`, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  return r;
}

async function noSidewaysScroll(page: Page) {
  const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
  expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(iw);
}

async function shoot(page: Page, testInfo: { outputPath: (n: string) => string; attach: (n: string, o: { path: string; contentType: string }) => Promise<void> }, name: string) {
  const path = testInfo.outputPath(name);
  try {
    await page.screenshot({ path, fullPage: true });
  } catch (e) {
    if (!(e instanceof Error) || !e.message.includes('Unable to capture screenshot')) throw e;
    await page.waitForTimeout(250);
    await page.screenshot({ path, fullPage: true });
  }
  await testInfo.attach(name, { path, contentType: 'image/png' });
}

// answerBoxOnItsOwnLine: in the card holding box, the box is ≥90% of the
// card's inner width and Answer sits wholly below it.
async function answerBoxOnItsOwnLine(box: Locator, where: string) {
  await expect(box).toBeVisible();
  const m = await box.evaluate((ta) => {
    const card = (ta.closest('li') ?? ta.closest('article')) as HTMLElement;
    const cs = getComputedStyle(card);
    const inner = card.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
    const form = ta.closest('form') as HTMLFormElement;
    const answer = Array.from(form.querySelectorAll('button[type=submit]')).find((b) => (b.textContent ?? '').trim() === 'Answer') as HTMLElement;
    const t = ta.getBoundingClientRect();
    const a = answer.getBoundingClientRect();
    return { box: t.width, inner, boxBottom: t.bottom, answerTop: a.top, answerH: a.height };
  });
  expect(m.box / m.inner, `${where}: the answer box is ${m.box}px of a ${m.inner}px card`).toBeGreaterThanOrEqual(0.9);
  expect(m.answerTop, `${where}: Answer is below the box`).toBeGreaterThanOrEqual(m.boxBottom);
  expect(m.answerH, `${where}: Answer is a 44px target`).toBeGreaterThanOrEqual(44);
}

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  // A pod officer (a host one may not have a daily budget): it needs the
  // connect listener and a provisioner, and the noop one starts nothing.
  const roster = vps.map((v) => ({ id: idOf(v.key), backend: 'builtin', daily_budget_usd: 10 }));
  server = await startServer(versionA, officerPageRedesignPort, {
    MUSTER_OFFICERS: JSON.stringify(roster),
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x52).toString('base64'),
    MUSTER_CONNECT_PORT: String(officerPageRedesignConnectPort),
    MUSTER_AGENT_CONNECT_URL: `http://localhost:${officerPageRedesignConnectPort}`,
    // A provisioner that starts nothing: the officer is declared, never run.
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    // Officer instances are claude-code agents; the kinds need gateway too,
    // and a gateway's three boot values (agent-composer.spec.ts's): none is
    // used, since no instance starts.
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-officer-redesign',
    MUSTER_AGENT_GATEWAY: 'hooks-sha256',
    MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
    MUSTER_AGENT_RUNTIME_CONFIG: '{}',
    MUSTER_AGENT_RUNTIME_INSTALL: 'true',
  });
});

test.afterAll(async () => {
  await server?.stop();
});

for (const vp of vps) {
  test.describe(vp.name, () => {
    test.use(vp.use);
    const id = idOf(vp.key);
    const type = typeOf(vp.key);

    test('spend bar and Raise, Instructions Save and Save and refresh, the answer box on its own line', async ({ page }, testInfo) => {
      const t = await api('PUT', `/api/officers/types/${type}`, { name: `Triage ${vp.key}`, charter: CHARTER_1 });
      expect(t.status, `the type is put: ${await t.text()}`).toBeLessThan(300);
      const d = await api('PUT', `/api/officers/defs/${encodeURIComponent(id)}`, { enabled: true });
      expect(d.status, `the def is put: ${await d.text()}`).toBeLessThan(300);
      fix('spend', '1', id, '8.5');
      fix('ask', '1', id, QUESTION);
      await signIn(page, origin);
      await page.goto(`${origin}/officers/${encodeURIComponent(id)}`);

      // No Shadow / Live button, no machine-user warning, nothing sideways.
      await expect(page.locator('#officer-head')).toBeVisible();
      await expect(page.locator('form[action$="/mode"]')).toHaveCount(0);
      await expect(page.locator('[data-host-risk]')).toHaveCount(0);
      await noSidewaysScroll(page);

      // The spend bar: $8.50 of $10, amber, Raise reachable.
      const spend = page.locator('#officer-spend');
      await expect(spend).toHaveAttribute('data-spend-level', 'near');
      await expect(spend.locator('[data-run-spend]')).toHaveText('$8.50 of $10 today');
      await expect(spend.locator('[role=progressbar]')).toHaveAttribute('data-progress-tone', 'warn');
      const raise = spend.locator('[data-cap-raise-submit]');
      await expect(raise).toHaveText("Raise today's limit to $20");
      expect((await raise.boundingBox())!.height, 'Raise is a 44px target').toBeGreaterThanOrEqual(44);
      // The last 7 days fold into its details.
      await expect(page.locator('#officer-usage')).toHaveCount(0);
      await spend.locator('[data-spend-details] summary').click();
      await expect(spend.locator('[data-spend-details]')).toContainText('Last 7 days');

      // The officer's ask: the answer box on its own line.
      const ask = page.locator('#officer-needs-you [data-needs-asks] li', { hasText: QUESTION });
      await answerBoxOnItsOwnLine(ask.locator('textarea[name=answer]'), "the officer's page");
      await shoot(page, testInfo, `officer-page-${vp.key}.png`);

      // Instructions: open, change, Save.
      const charter = page.locator('#officer-charter');
      await expect(charter.locator('[data-charter-version-chip]')).toHaveText('Version 1');
      await charter.locator('[data-charter-body] summary').click();
      const input = charter.locator('[data-charter-input]');
      await expect(input).toHaveValue(CHARTER_1);
      const iw = await input.evaluate((el) => (el as HTMLElement).getBoundingClientRect().width);
      const cw = await charter.evaluate((el) => (el as HTMLElement).clientWidth);
      expect(iw / cw, 'the editor spans the section').toBeGreaterThan(0.8);
      await noSidewaysScroll(page);
      await shoot(page, testInfo, `officer-charter-${vp.key}.png`);
      await input.fill(CHARTER_2);
      await charter.locator('[data-charter-save]').click();
      await page.waitForURL((u) => u.searchParams.get('notice') === 'charter-saved');
      await expect(page.locator('#officer-charter [data-charter-version-chip]')).toHaveText('Version 2');
      const stored = await (await api('GET', `/api/officers/types/${type}`)).json();
      expect(stored.charter, 'the saved charter').toBe(CHARTER_2);
      expect(stored.charter_version, 'one version per change').toBe(2);

      // Save and refresh: not running here, so its next start is new.
      await page.locator('#officer-charter [data-charter-body] summary').click();
      const save2 = page.locator('#officer-charter [data-charter-save-refresh]');
      expect((await save2.boundingBox())!.height, 'Save and refresh is a 44px target').toBeGreaterThanOrEqual(44);
      await save2.click();
      await page.waitForURL((u) => u.searchParams.get('notice') === 'refreshed-idle');
      expect((await (await api('GET', `/api/officers/types/${type}`)).json()).charter_version, 'unchanged text: no new version').toBe(2);

      // Raise: today's limit is $20, the bar plain again.
      await page.locator('#officer-spend [data-cap-raise-submit]').click();
      await page.waitForURL((u) => u.searchParams.get('notice') === 'cap-raised');
      await expect(page.locator('#officer-spend [data-run-spend]')).toHaveText('$8.50 of $20 today');
      await expect(page.locator('#officer-spend')).toHaveAttribute('data-spend-level', 'under');
      await expect(page.locator('#officer-spend [data-cap-raise-submit]')).toHaveCount(0);
      await noSidewaysScroll(page);

      // The Asks page: the same form, the box on its own line too.
      await page.goto(`${origin}/attention`);
      const card = page.locator('li', { hasText: QUESTION }).first();
      await answerBoxOnItsOwnLine(card.locator('textarea[name=answer]'), 'the Asks page');
      await noSidewaysScroll(page);
      await shoot(page, testInfo, `asks-page-${vp.key}.png`);
    });
  });
}
