import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';

// MUSTER'S OWN MARK (docs/brand.md), in a real browser, on a server with no
// instance branding set (every spec but branding.spec.ts sees nothing set).
//
// The sign-in page (signed out) and the app shell draw the brand pack's
// lockup: found by its data-brand-mark attribute and its file, never by text
// (the wordmark is outlines, so there is no text to find). It must LOAD as an
// SVG (naturalWidth > 0, and the response's type), and be drawn at its size.
// The favicons and the touch icon resolve, and so does every icon the manifest
// names, each 200 with its declared type.
//
// 🔴 REGRESSION TEST: on the commit before the brand pack, the header's brand
// was two spans of text ("mu" + "ster"), the sign-in page drew no mark, and
// there was no SVG favicon: the data-brand-mark and rel=icon type=image/svg+xml
// locators below find nothing there.

const LOCKUP = '/static/icons/lockup-horizontal.svg';

// lockupLoaded is the image's natural width once it has loaded (0 if it failed).
async function lockupLoaded(page: Page, sel: string): Promise<number> {
  const img = page.locator(sel).first();
  await expect(img).toBeVisible();
  return img.evaluate(async (el: HTMLImageElement) => {
    if (!el.complete) await new Promise((r) => el.addEventListener('load', r, { once: true }));
    return el.naturalWidth;
  });
}

async function expectServed(page: Page, href: string | null, type: string, what: string) {
  expect(href, what).toBeTruthy();
  const res = await page.request.get(href!);
  expect(res.status(), `${what}: ${href}`).toBe(200);
  expect(res.headers()['content-type'], `${what}: ${href}`).toBe(type);
}

test.describe('signed out', () => {
  test.use({ storageState: { cookies: [], origins: [] } });

  test('the sign-in page draws the lockup and links the favicons', async ({ page }) => {
    await page.goto('/login');
    const mark = page.locator('[data-testid="login-mark"] img[data-brand-mark]');
    await expect(mark).toHaveCount(1);
    await expect(mark).toHaveAttribute('src', new RegExp(`^${LOCKUP}\\?v=[0-9a-f]{12}$`));
    await expect(mark).toHaveAttribute('alt', 'muster');
    expect(await lockupLoaded(page, '[data-testid="login-mark"] img[data-brand-mark]')).toBeGreaterThan(0);
    await expectServed(page, await mark.getAttribute('src'), 'image/svg+xml', 'the sign-in lockup');
    // No instance brand on a server with none.
    await expect(page.locator('[data-testid="login-brand"]')).toHaveCount(0);

    await expectServed(page, await page.locator('link[rel="icon"][type="image/svg+xml"]').getAttribute('href'),
      'image/svg+xml', 'the SVG favicon');
    await expectServed(page, await page.locator('link[rel="icon"][sizes="32x32"]').getAttribute('href'),
      'image/png', 'the 32 px favicon');
    await expectServed(page, await page.locator('link[rel="apple-touch-icon"]').getAttribute('href'),
      'image/png', 'the touch icon');
  });
});

test('the app shell draws the lockup in the header, and every manifest icon resolves', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto('/tasks');
  const header = 'header a[aria-label="muster home"] img[data-brand-mark]';
  await expect(page.locator(header)).toHaveCount(1);
  await expect(page.locator(header)).toHaveAttribute('src', new RegExp(`^${LOCKUP}\\?v=[0-9a-f]{12}$`));
  expect(await lockupLoaded(page, header)).toBeGreaterThan(0);
  // Drawn at the header's size: 28 px tall, its width from the lockup's
  // 350 × 74 aspect (about 132 px), so the file's own box is what is drawn.
  const box = await page.locator(header).boundingBox();
  expect(box!.height).toBeCloseTo(28, 0);
  expect(box!.width).toBeGreaterThan(125);
  expect(box!.width).toBeLessThan(140);
  // The sidebar draws the same image.
  await expect(page.locator('#sidebar img[data-brand-mark]')).toHaveCount(1);

  await expectServed(page, await page.locator('link[rel="icon"][type="image/svg+xml"]').getAttribute('href'),
    'image/svg+xml', 'the SVG favicon');

  // At a phone's width the header crops the same image to its mark, 28 px
  // tall and 38 px wide (the left 100 of the lockup's 350 units), so the home
  // link stays clear of the update pill's top-right corner (update.spec.ts,
  // "dragging moves the pill"). The sidebar, which has the room, keeps the
  // whole lockup.
  await page.setViewportSize({ width: 390, height: 844 });
  const phone = await page.locator(header).boundingBox();
  expect(phone!.height).toBeCloseTo(28, 0);
  expect(phone!.width).toBeCloseTo(38, 0);
  expect(await page.locator(header).evaluate((el) => getComputedStyle(el).objectFit), 'cropped, not squashed').toBe('cover');
  // The link around the cropped mark is still a 44 px touch target (layout.spec.ts).
  const link = await page.locator('header a[aria-label="muster home"]').boundingBox();
  expect(link!.width, 'a 44px target').toBeGreaterThanOrEqual(44);
  const side = await page.locator('#sidebar img[data-brand-mark]').boundingBox();
  expect(side!.width, 'the sidebar keeps the whole lockup').toBeGreaterThan(125);

  const manifest = await (await page.request.get('/manifest.webmanifest')).json();
  const icons: { src: string; type: string; sizes: string; purpose: string }[] = manifest.icons;
  expect(icons.map((i) => `${i.src} ${i.purpose}`)).toEqual([
    '/static/icons/icon-192.png any',
    '/static/icons/icon-512.png any',
    '/static/icons/maskable-192.png maskable',
    '/static/icons/maskable-512.png maskable',
  ]);
  for (const i of icons) {
    await expectServed(page, i.src, i.type, `manifest icon ${i.sizes} ${i.purpose}`);
    // The browser decodes it at the size the manifest declares.
    const [w, h] = await page.evaluate(async (src) => {
      const img = new Image();
      img.src = src;
      await img.decode();
      return [img.naturalWidth, img.naturalHeight];
    }, i.src);
    expect(`${w}x${h}`, i.src).toBe(i.sizes);
  }
});
