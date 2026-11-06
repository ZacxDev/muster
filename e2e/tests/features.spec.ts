import { Page } from '@playwright/test';
import { revealBox } from '../lib/composer';
import { test, expect } from '../lib/fixtures';
import { featuresPort } from '../lib/env';
import { cc, MirrorRig } from '../lib/mirrorhost';
import { openMore } from '../lib/nav';

// THE FEATURE REGISTRY IN A REAL BROWSER (internal/features, internal/api
// features.go): a REAL muster-server with a mirrored host (the `-tags
// e2emirror` muster-mirror) and NO speech-to-text service, NO S3 and NO
// connector key — so dictation, attachments and the Connectors tab are off.
//
//   - the Rendered view's composer draws a MUTED mic and paperclip where the
//     real ones would be, and the mic's hint opens the Features page at the
//     dictation row, which says Off and shows the .env lines;
//   - the Features page lists the registry: mirrored sessions On (core),
//     suggested replies Off ("needs MUSTER_AGENT_OPENROUTER_KEY" — off, not
//     misconfigured, on a keyless install), dictation Off with the Compose
//     profile line, the chief Off with its Compose .env block (the chief
//     profile, MUSTER_CHIEF_RUNTIME=compose), officers Off with "needs
//     Kubernetes" and no .env block,
//     and NOTHING misconfigured (the default install's doctor passes);
//   - the nav's More carries a muted, labelled Connectors entry (linking to its
//     row), and no /plugins or /flows link.
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

    test('the composer shows muted dictation and attachment hints that open their Features rows', async ({ page }) => {
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
      await page.waitForURL((u) => u.pathname === '/account/features' && u.hash === '#dictation');
      const dictation = row(page, 'dictation');
      await expect(dictation).toHaveAttribute('data-state', 'off');
      await expect(dictation.locator('[data-feature-state]')).toHaveText('Off');
      await expect(dictation.locator('[data-feature-enable]')).toContainText('COMPOSE_PROFILES=stt');
      await expect(dictation.locator('[data-feature-enable]')).toContainText('MUSTER_STT_TOKEN=');
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
      await expect(row(page, 'suggested-replies').locator('[data-feature-reason]')).toContainText('needs MUSTER_AGENT_OPENROUTER_KEY');
      await expect(page.locator('section[data-feature][data-state="misconfigured"]')).toHaveCount(0);
      await expect(page.locator('[data-features-counts]')).toContainText('0 misconfigured');
      const chief = row(page, 'chief');
      await expect(chief).toHaveAttribute('data-state', 'off');
      await expect(chief.locator('[data-feature-kubernetes]')).toHaveCount(0);
      await expect(chief.locator('[data-feature-enable]')).toContainText('COMPOSE_PROFILES=chief');
      await expect(chief.locator('[data-feature-enable]')).toContainText('MUSTER_CHIEF_RUNTIME=compose');
      const officers = row(page, 'officers');
      await expect(officers.locator('[data-feature-kubernetes]')).toContainText('needs Kubernetes (in-cluster dispatch)');
      await expect(officers.locator('[data-feature-enable]')).toHaveCount(0);
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
      await expect(off).toHaveAttribute('href', '/account/features#connectors');
      await expect(page.locator('#sidebar a[href="/flows"], #sidebar a[href="/plugins"]')).toHaveCount(0);
      await expect(page.locator('#sidebar-features')).toHaveAttribute('aria-current', 'page');
    });
  });
}
