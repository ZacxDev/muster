import { execFileSync } from 'node:child_process';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { dsn, repoRoot, stepupCardPagePort, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// A STEP-UP CONFIRMATION CARD'S PAGE (internal/api host_stepup_card.go), in a
// real browser against a real muster-server, at a phone (390×844, touch): the
// page a card's notification opens shows WHY it is asked and a PREVIEW of what
// it does — the title, the why, who asked, the venue and the post's exact text
// (markup in it drawn as text) — and folds the boilerplate (the machine, the
// session, the call's JSON, the hash) into a Details toggle that is CLOSED
// until tapped. Nothing scrolls sideways.
//
// 🔴 RED AT origin/main: there a card has no parts (no attention_asks.step_up,
// no data-stepup-*), and its page prints the whole question — host, session,
// the call's raw JSON and the hash — as one paragraph.
//
// The card is a fixture row (e2e/stepupfix): what is under test is how the
// page DRAWS a card; the Go tests drive how the gate files one.

const origin = `http://localhost:${stepupCardPagePort}`;
let server: Running | undefined;

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/stepupfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, stepupCardPagePort, { MUSTER_FOCUS_INTERVAL: '0' });
});

test.afterAll(async () => {
  try {
    fix('clear', '1');
  } finally {
    await server?.stop();
  }
});

test.describe('phone 390×844', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('a step-up card shows why and the post, with the boilerplate folded into Details', async ({ page }, testInfo) => {
    const id = fix('card', '1');
    await signIn(page, origin);
    await page.goto(`${origin}/attention/asks/${id}`);
    const card = page.locator('[data-stepup-card]');
    await expect(card).toBeVisible();

    await expect(card.locator('[data-stepup-title]')).toHaveText('Post to X?');
    await expect(card.locator('[data-stepup-why]')).toHaveText('Marketing: rebrand announcement');
    await expect(card.locator('[data-stepup-asker]')).toHaveText('Asked by work:1');
    await expect(card.locator('[data-stepup-venue]')).toHaveText('X');
    const preview = card.locator('[data-stepup-preview-text]');
    await expect(preview).toBeVisible();
    await expect(preview).toHaveText('Big news: acme is now acme-two.\nRead more on the blog. <b>not bold</b>');
    // The officer's markup is text: no element was made of it.
    await expect(preview.locator('b')).toHaveCount(0);

    // Details: present, CLOSED, its text hidden until tapped.
    const details = card.locator('[data-stepup-details]');
    await expect(details).toHaveCount(1);
    expect(await details.evaluate((d) => (d as HTMLDetailsElement).open), 'Details is closed by default').toBe(false);
    const detailsText = card.locator('[data-stepup-details-text]');
    await expect(detailsText).toBeHidden();

    // The visible card says nothing of the boilerplate: no JSON, no host, no hash.
    const visible = await card.innerText();
    for (const never of ['{', 'plugin_draft_decide', 'host-a', '0123456789ab', 'paired phone', 'any process there']) {
      expect(visible, `the card's face shows ${never}`).not.toContain(never);
    }
    await expect(page.locator('[data-ask-question]')).toHaveCount(0);

    const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
    expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(iw);
    const shut = testInfo.outputPath('stepup-card-phone.png');
    await page.screenshot({ path: shut, fullPage: true });
    await testInfo.attach('stepup-card-phone.png', { path: shut, contentType: 'image/png' });

    // Tapped open: the record is all there.
    await card.locator('[data-stepup-details-toggle]').tap();
    await expect(detailsText).toBeVisible();
    await expect(detailsText).toContainText('A session on host-a (work:1 @ host-a)');
    await expect(detailsText).toContainText('plugin_draft_decide {"id":41,"decision":"approve"}');
    await expect(detailsText).toContainText('[0123456789ab]');
    const opened = testInfo.outputPath('stepup-card-phone-details.png');
    await page.screenshot({ path: opened, fullPage: true });
    await testInfo.attach('stepup-card-phone-details.png', { path: opened, contentType: 'image/png' });
  });
});
