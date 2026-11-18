import path from 'node:path';
import { Page } from '@playwright/test';
import { revealBox } from '../lib/composer';
import { test, expect } from '../lib/fixtures';
import { featuresPort } from '../lib/env';
import { cc, MirrorRig } from '../lib/mirrorhost';
import { openMore } from '../lib/nav';
import { pageWords, setupJargon } from '../lib/plainwords';

// THE FEATURE REGISTRY IN A REAL BROWSER (internal/features, internal/api
// features.go): a REAL muster-server with a mirrored host (the `-tags
// e2emirror` muster-mirror) and NO speech-to-text service, NO S3 and NO
// connector key — so dictation, attachments and the Connectors tab are off.
//
//   - the Rendered view's composer draws a MUTED mic and paperclip where the
//     real ones would be, and the mic's hint opens the Features page with the
//     dictation row's ⓘ sheet open, which says Off and holds the .env lines;
//   - the Features page lists the registry: mirrored sessions On (core),
//     suggested replies Off ("needs MUSTER_AGENT_OPENROUTER_KEY" — off, not
//     misconfigured, on a keyless install), dictation Off with the Compose
//     profile line, the chief Off with its Compose .env block (the chief
//     profile, MUSTER_CHIEF_RUNTIME=compose), agent chat Off with "needs
//     Kubernetes", officers On with their reason, and NOTHING misconfigured
//     (the default install's doctor passes) — every setup detail in a row's
//     ⓘ sheet (the e2e user is an admin, so none is withheld);
//   - the nav's More carries a muted, labelled Connectors entry (linking to its
//     row's sheet), and no /plugins or /flows link.
//
// At a phone's size and a desktop's.
test.use({ storageState: { cookies: [], origins: [] } });

const rig = new MirrorRig(featuresPort, 'e2e-feat');
const origin = rig.origin;

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
  const one = rig.transcript(cc.prompt('a prompt') + cc.reply('An answer.'), 'one');
  const now = Math.floor(Date.now() / 1000);
  const windows = rig.transcript(`work|@1|1|1|claude|${now - 60}|${one}\n`, 'windows');
  await rig.startHost({ MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
});

test.afterAll(async () => {
  await rig.stop();
});

const row = (page: Page, id: string) => page.locator(`section[data-feature="${id}"]`);

for (const size of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 } } },
]) {
  test.describe(size.name, () => {
    test.use(size.use);

    test('the composer shows muted dictation and attachment hints that open their Features sheets', async ({ page }) => {
      await rig.login(page);
      await rig.waitListed(page, 'data-mirror-claude');
      await page.goto(origin + `/mirror/view?host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent('@1')}`);
      // On a phone the box (and the paperclip inside it) is collapsed behind
      // "Type a message" (lib/composer.ts); the mic's hint is beside it either way.
      await expect(page.locator('#mv-dictate-off')).toBeVisible();
      await revealBox(page);
      // The hints, in the real controls' places — and not the real controls.
      await expect(page.locator('#mv-dictate-off')).toBeVisible();
      await expect(page.locator('#mv-attach-off')).toBeVisible();
      await expect(page.locator('#mv-dictate')).toHaveCount(0);
      await expect(page.locator('#mv-attach')).toHaveCount(0);
      await expect(page.locator('#mv-dictate-off')).toHaveAttribute('aria-label', /Dictation is off on this server/);

      await page.locator('#mv-dictate-off').click();
      await page.waitForURL((u) => u.pathname === '/account/features' && u.hash === '#dictation-setup');
      const dictation = row(page, 'dictation');
      await expect(dictation).toHaveAttribute('data-state', 'off');
      await expect(dictation.locator('[data-feature-state]')).toHaveText('Off');
      // The hint opened the row's ⓘ sheet, holding the .env lines.
      const sheet = page.locator('#dictation-setup');
      await expect(sheet).toBeVisible();
      await expect(sheet.locator('[data-info-code]')).toContainText('COMPOSE_PROFILES=stt');
      await expect(sheet.locator('[data-info-code]')).toContainText('MUSTER_STT_TOKEN=');
      await page.keyboard.press('Escape');
      await expect(sheet).toBeHidden();
      await expect(dictation).toBeInViewport();

      // The page never scrolls sideways at a phone's width.
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      expect(overflow, 'the Features page scrolls sideways').toBeLessThanOrEqual(0);
    });

    test('the Features page lists every state, and the nav shows what is off', async ({ page }) => {
      await rig.login(page);
      await page.goto(origin + '/account/features');
      await expect(row(page, 'mirror')).toHaveAttribute('data-state', 'on');
      await expect(row(page, 'suggested-replies')).toHaveAttribute('data-state', 'off');
      // The setup is in each row's ⓘ sheet (closed: read as text, not seen).
      await expect(row(page, 'suggested-replies').locator('[data-feature-reason]')).toContainText('needs MUSTER_AGENT_OPENROUTER_KEY');
      await expect(page.locator('section[data-feature][data-state="misconfigured"]')).toHaveCount(0);
      await expect(page.locator('[data-features-counts]')).toContainText('0 need setup');
      const chief = row(page, 'chief');
      await expect(chief).toHaveAttribute('data-state', 'off');
      await expect(chief.locator('[data-feature-kubernetes]')).toHaveCount(0);
      await expect(chief.locator('[data-info-code]')).toContainText('COMPOSE_PROFILES=chief');
      await expect(chief.locator('[data-info-code]')).toContainText('MUSTER_CHIEF_RUNTIME=compose');
      // A Kubernetes-only feature that is off here (no MUSTER_AGENT_GATEWAY;
      // dispatch itself is on, over the rig's noop provisioner): its sheet
      // says the Compose stack cannot run it, and gives the lines for a
      // Kubernetes deployment.
      const gateway = row(page, 'agent-gateway');
      await expect(gateway).toHaveAttribute('data-state', 'off');
      await expect(gateway.locator('[data-feature-kubernetes]')).toContainText('needs Kubernetes (in-cluster dispatch)');
      await expect(gateway.locator('[data-info-sheet]')).toContainText('In a Kubernetes deployment, set:');
      // Officers run in the Compose stack too (as host-spawned sessions,
      // docs/design/officers-on-hosts.md), and with a database they are ON
      // whether or not MUSTER_OFFICERS declares any: an officer can be
      // declared in the database (migration 0136). No Kubernetes hint.
      const officers = row(page, 'officers');
      await expect(officers).toHaveAttribute('data-state', 'on');
      await expect(officers.locator('[data-feature-kubernetes]')).toHaveCount(0);
      await expect(officers.locator('[data-feature-reason]')).toHaveText('MUSTER_OFFICERS declares none; officers declared in the database run');
      await expect(officers.locator('[data-feature-description]')).toContainText('as host-spawned sessions on one of your machines');
      await expect(row(page, 'attachments')).toHaveAttribute('data-state', 'off');
      await expect(row(page, 'connectors')).toHaveAttribute('data-state', 'off');

      // The nav: open it on a phone (the rail is always there on a desktop).
      if (size.name === 'phone') {
        await page.locator('#sidebar-open').click();
        await expect(page.locator('#sidebar-root')).toHaveAttribute('data-open', 'true');
        await expect(page.locator('#sidebar')).toBeInViewport({ ratio: 0.99 });
      }
      await openMore(page);
      const off = page.locator('#sidebar-connectors-off');
      await expect(off).toBeVisible();
      await expect(off).toHaveAttribute('href', '/account/features#connectors-setup');
      await expect(page.locator('#sidebar a[href="/flows"], #sidebar a[href="/plugins"]')).toHaveCount(0);
      await expect(page.locator('#sidebar-features')).toHaveAttribute('aria-current', 'page');
    });
  });
}

// A PLAIN FEATURES PAGE, ON A PHONE (operator rule: no variable, Compose
// profile, flag or command in muster's top layer). Each row reads as a plain
// name, one plain sentence and On / Off; for an ADMIN (the e2e user) the ⓘ
// beside it opens a sheet that fits the phone and holds the exact lines, with
// Copy. (A member's page is features-member.spec.ts.) With
// MUSTER_E2E_SHOTS_DIR set it saves the phone screenshots there (never in CI).
//
// 🔴 REGRESSION TEST for the jargon audit's features theme: red at
// origin/main, whose rows drew the .env lines, the reasons and the docs path
// for anyone, and had no ⓘ.
test.describe('phone 390×844 touch: the Features page reads plainly', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
  const shots = process.env.MUSTER_E2E_SHOTS_DIR ?? '';
  const shot = async (page: Page, name: string) => {
    if (shots) await page.screenshot({ path: path.join(shots, name + '.png'), fullPage: name.endsWith('-list') });
  };

  test('an admin reads plain rows, and the ⓘ opens the exact setting with Copy', async ({ page }) => {
    await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin });
    await rig.login(page);
    await page.goto(origin + '/account/features');
    await expect(row(page, 'dictation')).toBeVisible();
    await shot(page, 'features-admin-list');
    // Nothing a person reads without opening an ⓘ spells setup.
    const words = await pageWords(page);
    expect(words.length, 'CONTROL: the page has words to read').toBeGreaterThan(1000);
    expect(words, 'the top layer spells no variable, command or profile').not.toMatch(setupJargon);
    const rows = page.locator('section[data-feature]');
    const n = await rows.count();
    expect(n, 'the page lists the registry').toBeGreaterThan(30);
    await expect(page.locator('section[data-feature] [data-feature-oneline]')).toHaveCount(n);
    const dictation = row(page, 'dictation');
    await expect(dictation.locator('[data-feature-name]')).toHaveText('Voice typing');
    await expect(dictation.locator('[data-feature-oneline]')).toHaveText('Speak into any message box and your words are typed for you.');
    await expect(dictation.locator('[data-feature-state]')).toHaveText('Off');
    await expect(dictation.locator('[data-feature-next]')).toHaveText('To turn it on, open its ⓘ.');
    await expect(row(page, 'mirror').locator('[data-feature-state]')).toHaveText('On');

    const info = page.locator('#dictation-setup-open');
    await expect(info).toHaveAccessibleName('How to turn on Voice typing');
    await info.tap();
    const sheet = page.locator('#dictation-setup');
    await expect(sheet).toBeVisible();
    await expect(sheet).toHaveAttribute('role', 'dialog');
    await expect(sheet).toHaveAccessibleName('Turn on Voice typing');
    const sb = (await sheet.boundingBox())!;
    expect(sb.x >= -0.5 && sb.x + sb.width <= 390.5, 'the sheet fits the phone').toBe(true);
    await shot(page, 'features-admin-sheet');
    const copy = sheet.locator('[data-info-copy]');
    await copy.tap();
    await expect(copy).toHaveText('Copied');
    expect(await page.evaluate(() => navigator.clipboard.readText()), 'Copy puts the exact .env lines on the clipboard').toBe(
      'COMPOSE_PROFILES=stt\nMUSTER_STT_TOKEN=<a random secret>',
    );
    await page.keyboard.press('Escape');
    await expect(sheet).toBeHidden();
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow, 'the Features page scrolls sideways').toBeLessThanOrEqual(0);
  });
});
