import { execFileSync } from 'node:child_process';
import type { Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { startServer, Running } from '../lib/server';
import { dsn, repoRoot, uiKitParityPort, versionA } from '../lib/env';

// THE UI KIT DRAWS WHAT MUSTER DRAWS (docs/ui-kit.md#matching-musters-own),
// at a phone's size and a desktop's.
//
// muster's own components are read off a real muster page (the Account page,
// with a logo set so the branding section's secondary button is there). The
// kit's equivalents are drawn the way a plugin draws them: on a page of
// ANOTHER origin that links only /ui-kit/v1/kit.css and kit.js from muster,
// with none of muster's stylesheet. Each pair's computed font size, line
// height, weight, padding, corner radii, border, ring, colours and height must
// be equal; a change on EITHER side fails here.
//
// The same kit page proves the icons need no CORS: kit.js, loaded from
// muster onto a page of another origin, puts the sprite in that page.

const origin = `http://localhost:${uiKitParityPort}`;
let server: Running | undefined;

const clearBranding = () =>
  execFileSync('go', ['run', './e2e/pluginfix', 'branding', 'clear'], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } });

// An 8×8 opaque PNG: the logo that makes the branding section draw its
// "Reset logo" button.
const LOGO = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAYAAADED76LAAAAEklEQVR4nGM4kWL0Hx9mGBkKAPQrl0FTjoORAAAAAElFTkSuQmCC', 'base64');

const PROPS = [
  'font-family', 'font-size', 'line-height', 'font-weight',
  'padding-top', 'padding-right', 'padding-bottom', 'padding-left',
  'border-top-left-radius', 'border-top-right-radius', 'border-bottom-right-radius', 'border-bottom-left-radius',
  'border-top', 'border-right', 'border-bottom', 'border-left',
  'color', 'background-color', 'height', 'ring',
] as const;

type Style = Record<string, string>;

// computed reads PROPS off one element. A zero-width border is "none" (muster's
// preflight writes `0 solid`, a plugin page's default is `0 none`: the same
// nothing). `ring` is the box-shadow's visible layers only (Tailwind writes
// two transparent zero layers around its ring).
async function computed(el: Locator, skip: string[] = []): Promise<Style> {
  const s = await el.evaluate((node, props) => {
    const cs = getComputedStyle(node as Element);
    const out: Record<string, string> = {};
    for (const p of props) {
      if (p === 'ring') {
        const layers = cs.boxShadow === 'none' ? [] : cs.boxShadow.split(/,(?![^(]*\))/).map((x) => x.trim());
        out.ring = layers.filter((l) => !/^rgba\(0, 0, 0, 0\)/.test(l) && !/ 0px 0px 0px 0px/.test(l)).join(', ') || 'none';
      } else if (p.startsWith('border-') && !p.endsWith('radius')) {
        const side = p.slice('border-'.length);
        const w = cs.getPropertyValue(`border-${side}-width`);
        out[p] = w === '0px' ? 'none' : `${w} ${cs.getPropertyValue(`border-${side}-style`)} ${cs.getPropertyValue(`border-${side}-color`)}`;
      } else {
        out[p] = cs.getPropertyValue(p);
      }
    }
    return out;
  }, PROPS as unknown as string[]);
  for (const k of skip) delete s[k];
  return s;
}

// The kit page carries the CSP docs/ui-kit.md recommends to a plugin (muster's
// origin in style-src, script-src and img-src, and NO 'unsafe-inline'), so a
// kit that writes an inline style — or anything else that policy refuses —
// logs a violation here.
const KIT_PAGE = (o: string) => `<!doctype html><html><head><meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'self'; style-src 'self' ${o}; script-src 'self' ${o}; img-src 'self' ${o}">
<link rel="stylesheet" href="${o}/ui-kit/v1/kit.css">
<script src="${o}/ui-kit/v1/kit.js"></script>
</head><body class="mu-root">
<button type="button" id="k-primary" class="mu-btn mu-btn--primary">Sign out</button>
<button type="button" id="k-secondary" class="mu-btn mu-btn--secondary">Reset logo</button>
<input id="k-input" class="mu-input" type="text" value="Team Example">
<section id="k-card" class="mu-card"><h2 class="mu-card__title">Card</h2><p class="mu-card__body">Body</p></section>
<button type="button" id="k-icon" class="mu-icon-btn" aria-label="Menu"><svg class="mu-icon" aria-hidden="true"><use href="#mu-more"/></svg></button>
</body></html>`;

test.beforeAll(async () => {
  clearBranding();
  server = await startServer(versionA, uiKitParityPort, { MUSTER_PUBLIC_URL: origin });
});

test.afterAll(async () => {
  await server?.stop();
  clearBranding();
});

for (const [name, viewport] of [
  ['phone', { width: 390, height: 844 }],
  ['desktop', { width: 1280, height: 800 }],
] as const) {
  test.describe(`at ${name} size`, () => {
    test.use({ viewport });

    test("the kit's button, input, card and icon button compute as muster's own", async ({ page, context }) => {
      test.setTimeout(90_000);
      await signIn(page, origin);
      const up = await page.request.put(origin + '/api/instance/branding', {
        headers: { Origin: origin },
        multipart: { logo: { name: 'logo.png', mimeType: 'image/png', buffer: LOGO } },
      });
      expect(up.status(), await up.text()).toBe(200);

      // muster's own, on the Account page.
      await page.goto(origin + '/account');
      const own = {
        primary: page.locator('form[action="/logout"] button[type="submit"]').first(),
        secondary: page.locator('#branding-reset'),
        input: page.locator('#branding-name'),
        card: page.locator('main section.rounded-2xl').first(),
        // The sidebar's Search button (`h-11 w-11 rounded-lg text-muted`). On a
        // phone the sidebar is off screen, not removed: the button is laid out
        // and its computed style is still muster's.
        icon: page.locator('nav button[aria-label="Search"]').first(),
      };
      for (const [k, el] of Object.entries(own)) {
        await expect(el, `muster's own ${k}`).toHaveCount(1);
      }

      // The kit's, as a plugin draws them: another origin, kit.css + kit.js only.
      const kit = await context.newPage();
      const cspErrors: string[] = [];
      kit.on('console', (m) => {
        if (/Content Security Policy|Refused to/i.test(m.text())) cspErrors.push(m.text());
      });
      await kit.setViewportSize(viewport);
      await kit.setContent(KIT_PAGE(origin), { waitUntil: 'load' });
      await kit.evaluate(() => (window as unknown as { MusterKit: { ready: Promise<void> } }).MusterKit.ready);

      // The icons need no CORS: the sprite is in this page, and an icon draws.
      await expect(kit.locator('svg[data-mu-sprite] symbol#mu-mic')).toHaveCount(1);
      expect(await kit.locator('#k-icon svg').evaluate((n) => (n as SVGGraphicsElement).getBBox().width)).toBeGreaterThan(0);
      // Under the documented CSP, the kit draws without a single violation.
      expect(cspErrors, 'CSP violations on a plugin page using the kit').toEqual([]);

      const pairs: [string, Locator, Locator, string[]][] = [
        ['primary button', own.primary, kit.locator('#k-primary'), []],
        ['secondary button', own.secondary, kit.locator('#k-secondary'), []],
        ['input', own.input, kit.locator('#k-input'), []],
        // A card's height is its content's.
        ['card', own.card, kit.locator('#k-card'), ['height']],
        // An icon button's text metrics are unused: it holds only an icon.
        ['icon button', own.icon, kit.locator('#k-icon'), ['font-size', 'line-height', 'font-weight']],
      ];
      for (const [what, mine, theirs, skip] of pairs) {
        // Settle any colour transition before reading.
        await expect(theirs).toBeVisible();
        expect(await computed(theirs, skip), `${what}: the kit's (right) against muster's own (left)`)
          .toEqual(await computed(mine, skip));
      }

      const del = await page.request.delete(origin + '/api/instance/branding/logo', { headers: { Origin: origin } });
      expect(del.status()).toBe(200);
      await kit.close();
    });
  });
}
