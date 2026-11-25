import { execFileSync } from 'node:child_process';
import type { FrameLocator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { startServer, Running } from '../lib/server';
import { dsn, pagesThemeWidthConnectPort, pagesThemeWidthPort, repoRoot, versionA } from '../lib/env';

// A PAGE IN THE SERVER'S THEME, FULL WIDTH (internal/pages render.go Content
// and contentCSS, internal/api pages.go pageThemeCSS; docs/pages.md "Styling:
// theme and width"), in a real browser against a real muster-server.
//
// A page with a markdown block and an html block styled only with the
// content's tokens (e2e/pagesfix themed) is opened as a link's viewer, first
// on the default theme (the control: its button paints muster's brass), then
// after the admin saves a theme of their own on Account → Theme: the link's
// view AND the owner's own view then draw the instance's palette — the
// content's ground is the theme's --bg (never white), the html card's
// var(--s1) its surface, the button's var(--accent) the admin's accent — and
// the shell's header is drawn in it too. The content box (.mp) is the frame's
// full width less its two 16px gutters, at a desktop's size and a phone's.
//
// 🔴 REGRESSION TEST: on origin/main the content never inlines the theme (the
// button stays brass after the save) and .mp is capped at 44rem (704px), so
// the first themed expectation and the desktop width both fail. The theme is
// the server's row in the leg's shared database, so it is cleared before and
// after (pluginfix branding clear), and this spec's server is its own.
//
// Every value below is an obvious fake.

const origin = `http://localhost:${pagesThemeWidthPort}`;
const KEY = Buffer.alloc(32, 0x5c).toString('base64');
let server: Running | undefined;
let token = '';
let pageID = 0;

const fix = (cmd: 'themed' | 'themed-clear') =>
  execFileSync('go', ['run', './e2e/pagesfix', cmd, '1'], {
    cwd: repoRoot,
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_CONNECTOR_ENCRYPTION_KEY: KEY },
  }).toString();
const clearBranding = () =>
  execFileSync('go', ['run', './e2e/pluginfix', 'branding', 'clear'], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } });

// The fixture theme: harbor with an accent of the admin's own, distinct from
// brass (228 181 86) and from harbor's own (110 176 245).
const PRESET = 'harbor';
const ACCENT = '#5AA9F0';
const ACCENT_RGB = 'rgb(90, 169, 240)';
const BRASS_RGB = 'rgb(228, 181, 86)';
const WHITE = 'rgb(255, 255, 255)';

const rgb = (channels: string) => `rgb(${channels.split(' ').join(', ')})`;

// The theme's own colour, as /ui-kit/v1/theme.css (the server's theme block)
// declares it: "--mu-<name>: R G B;".
async function themeColour(page: Page, name: string): Promise<string> {
  const css = await (await page.request.get(origin + '/ui-kit/v1/theme.css')).text();
  const m = css.match(new RegExp(`--mu-${name}: (\\d+ \\d+ \\d+);`));
  expect(m, `theme.css declares --mu-${name}`).not.toBeNull();
  return rgb(m![1]);
}

async function saveTheme(page: Page, preset: string, accent: string) {
  await page.goto(origin + '/account#theme');
  await page.selectOption('#theme-preset', preset);
  await page.fill('#theme-accent', accent);
  await Promise.all([page.waitForURL(origin + '/account?theme=saved#theme'), page.click('#theme-save')]);
}

// The content box against the frame: .mp's width and the frame document's
// own client width (the viewport inside the frame, less any scrollbar).
async function measure(frame: FrameLocator) {
  return frame.locator('main.mp').evaluate((m) => {
    const r = m.getBoundingClientRect();
    const card = (m.querySelector('[data-testid="card"]') as HTMLElement).getBoundingClientRect();
    return { mp: r.width, left: r.left, client: document.documentElement.clientWidth, card: card.width, scroll: document.documentElement.scrollWidth };
  });
}

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  clearBranding();
  server = await startServer(versionA, pagesThemeWidthPort, {
    MUSTER_PAGES: '1',
    MUSTER_PUBLIC_URL: origin,
    MUSTER_CONNECTOR_ENCRYPTION_KEY: KEY,
    // A link key is the connector key, and that one needs its listener.
    MUSTER_CONNECT_PORT: String(pagesThemeWidthConnectPort),
    MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${pagesThemeWidthConnectPort}`,
  });
  const out = JSON.parse(fix('themed')) as { page: number; token: string };
  token = out.token;
  pageID = out.page;
  expect(token.length, 'the fixture made a live link').toBeGreaterThan(20);
});

test.afterAll(async () => {
  if (server) fix('themed-clear');
  await server?.stop();
  clearBranding();
});

for (const [name, viewport] of [
  ['desktop', { width: 1440, height: 900 }],
  ['phone', { width: 390, height: 844 }],
] as const) {
  test.describe(`at ${name} size (${viewport.width}x${viewport.height})`, () => {
    test.use({ viewport });

    test("a page is drawn in the instance's theme, full width, on its link and in its owner's view", async ({ page }) => {
      test.setTimeout(90_000);

      // The control: on the default theme the link's button paints brass.
      await page.goto(`${origin}/s/${token}`);
      let frame = page.frameLocator('iframe#page');
      await expect(frame.locator('main h1')).toHaveText('Theme and width');
      await expect(frame.getByTestId('go')).toHaveCSS('background-color', BRASS_RGB);

      await signIn(page, origin);
      await saveTheme(page, PRESET, ACCENT);
      const bg = await themeColour(page, 'bg');
      const s1 = await themeColour(page, 's1');
      expect(bg, "the theme's ground is not white").not.toBe(WHITE);

      for (const [view, path] of [
        ["the link's view", `/s/${token}`],
        ["the owner's view", `/pages/${pageID}/view`],
      ] as const) {
        await page.goto(origin + path);
        frame = page.frameLocator('iframe#page');
        await expect(frame.locator('main h1'), view).toHaveText('Theme and width');
        await expect(frame.locator('body'), `${view}: the content's ground`).toHaveCSS('background-color', bg);
        await expect(frame.getByTestId('card'), `${view}: the html card's var(--s1)`).toHaveCSS('background-color', s1);
        await expect(frame.getByTestId('go'), `${view}: the html button's var(--accent)`).toHaveCSS('background-color', ACCENT_RGB);
        // The shell is drawn in the theme too: its ground and its accent.
        await expect(page.locator('body'), `${view}: the shell's ground`).toHaveCSS('background-color', bg);
        expect(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--mu-accent').trim()), `${view}: the shell's accent`)
          .toBe('90 169 240');

        // Full width: .mp is the frame less its two 16px gutters.
        const m = await measure(frame);
        expect(m.client, `${view}: the frame is about the window wide`).toBeGreaterThanOrEqual(viewport.width - 20);
        expect(Math.abs(m.mp - m.client), `${view}: .mp (${m.mp}px) is the frame's width (${m.client}px)`).toBeLessThanOrEqual(1);
        expect(Math.abs(m.card - (m.client - 32)), `${view}: the html card (${m.card}px) is the frame less 32px (${m.client - 32}px)`).toBeLessThanOrEqual(1);
        expect(m.left, `${view}: .mp starts at the frame's edge`).toBeLessThanOrEqual(1);
        expect(m.scroll, `${view}: the content scrolls sideways`).toBeLessThanOrEqual(m.client);
      }

      // Back to the default through the server itself (a row cleared under
      // it would stay cached for its branding TTL), for the next size's control.
      await saveTheme(page, 'brass', '');
    });
  });
}
