import { execFileSync } from 'node:child_process';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { startServer, Running } from '../lib/server';
import { dsn, instanceThemePort, repoRoot, versionA } from '../lib/env';

// THE SERVER'S THEME (internal/api/theme.go; docs/ui-kit.md#the-theme), in a
// real browser against a real muster-server, at a phone's size and a desktop's.
//
// The admin (password mode: the operator) picks an accent on Account → Theme.
// The live preview draws it before anything is saved; an unreadable accent is
// shown failing in the preview and refused at save with the reason on the
// page; a readable one is saved, and muster's OWN pages then draw it — the
// theme stylesheet is linked, the root's --mu-accent is the new channels, and
// a real accent-filled control (the Account page's Sign out button) paints in
// it. Back to the default preset, no theme stylesheet is linked and the
// button paints muster's brass again.
//
// 🔴 REGRESSION TEST: on origin/main there is no Theme section and no
// /ui-kit/v1/theme.css, so the first expectation past sign-in fails. The theme
// is the server's row in the leg's shared database, so it is cleared before
// and after (pluginfix branding clear), and this spec's server is its own.

const origin = `http://localhost:${instanceThemePort}`;
let server: Running | undefined;

const clearBranding = () =>
  execFileSync('go', ['run', './e2e/pluginfix', 'branding', 'clear'], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } });

// The fixture accent: no preset's, and distinct from brass (228 181 86).
const ACCENT = '#7FD0FF';
const ACCENT_CHANNELS = '127 208 255';
const ACCENT_RGB = 'rgb(127, 208, 255)';
const BRASS_RGB = 'rgb(228, 181, 86)';

const rootAccent = (page: Page) =>
  page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--mu-accent').trim());

// A real accent-filled control. toHaveCSS retries, so a colour transition
// (the kit's --mu-dur-fast) settles before it is read.
const signOut = (page: Page) => page.locator('form[action="/logout"] button[type="submit"]').first();

async function saveTheme(page: Page, preset: string, accent: string, expectUrl: string) {
  await page.goto(origin + '/account#theme');
  await page.selectOption('#theme-preset', preset);
  await page.fill('#theme-accent', accent);
  await Promise.all([page.waitForURL(origin + expectUrl), page.click('#theme-save')]);
}

test.beforeAll(async () => {
  clearBranding();
  server = await startServer(versionA, instanceThemePort, { MUSTER_PUBLIC_URL: origin });
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

    test("an admin's accent reaches muster's own pages, and an unreadable one is refused", async ({ page }) => {
      test.setTimeout(90_000);
      await signIn(page, origin);

      // The default: no theme stylesheet, brass everywhere.
      await page.goto(origin + '/account');
      await expect(page.locator('link[data-theme-css]')).toHaveCount(0);
      expect(await rootAccent(page)).toBe('228 181 86');
      await expect(signOut(page)).toHaveCSS('background-color', BRASS_RGB);

      const section = page.locator('[data-testid="theme"]');
      await expect(section).toBeVisible();
      const preview = section.locator('[data-testid="theme-preview"]');

      // An unreadable accent: the live preview says so before any save…
      await page.fill('#theme-accent', '#3A2F6B');
      await expect(preview).toHaveAttribute('data-theme-ok', 'false');
      await expect(preview.getByText('accent #3A2F6B on bg', { exact: false })).toBeVisible();
      // …and the save is refused, with the reason on the page, nothing changed.
      await saveTheme(page, 'brass', '#3A2F6B', '/account?theme_error=theme_contrast#theme');
      await expect(section.getByText('too hard to read')).toBeVisible();
      await expect(page.locator('link[data-theme-css]')).toHaveCount(0);

      // A readable accent: the preview draws it live, unsaved.
      await page.fill('#theme-accent', ACCENT);
      await expect(preview).toHaveAttribute('data-accent', ACCENT);
      await expect(preview).toHaveAttribute('data-theme-ok', 'true');
      await expect(preview.locator('.mu-btn--primary')).toHaveCSS('background-color', ACCENT_RGB);
      // The page itself has not changed yet.
      await expect(signOut(page)).toHaveCSS('background-color', BRASS_RGB);

      await saveTheme(page, 'brass', ACCENT, '/account?theme=saved#theme');
      await expect(section.getByText('Theme saved.')).toBeVisible();

      // muster's own pages now draw it: the board and the Account page.
      for (const path of ['/tasks', '/account']) {
        await page.goto(origin + path);
        await expect(page.locator('link[data-theme-css]')).toHaveCount(1);
        expect(await rootAccent(page)).toBe(ACCENT_CHANNELS);
      }
      await expect(signOut(page)).toHaveCSS('background-color', ACCENT_RGB);
      const instance = await (await page.request.get(origin + '/api/instance')).json();
      expect(instance.theme.accent).toBe(ACCENT);

      // Back to the default preset: nothing linked, brass again.
      await saveTheme(page, 'brass', '', '/account?theme=saved#theme');
      await expect(page.locator('link[data-theme-css]')).toHaveCount(0);
      expect(await rootAccent(page)).toBe('228 181 86');
      await expect(signOut(page)).toHaveCSS('background-color', BRASS_RGB);
    });
  });
}
