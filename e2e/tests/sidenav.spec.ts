import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { sidenavPort } from '../lib/env';
import { cc, MirrorRig } from '../lib/mirrorhost';

// THE SIDE NAV ON EVERY SIGNED-IN PAGE (internal/ui/nav_layout.go), opened by
// its own toggle in a real browser, at a phone's size and a desktop's:
//
//   - the server-authored pages (/attention, /account, /plugins, /flows) carry
//     it, mark the entry they belong to, and keep that mark after the page's
//     script has run (no fallback onto Tasks);
//   - the mirrored Rendered view and the terminal carry it as an OVERLAY at
//     both sizes — its hamburger opens it, and nothing on the page moves;
//   - the view-swap frame (the other view, opened over the page) carries NONE:
//     the page under it already has one.
//
// Its own server: the e2eterm binary with the mirror plugin AND the plugin
// interface on (/plugins is served only then), and one per-window mirrored host
// whose two windows run Claude Code over a transcript this spec writes.

test.use({ storageState: { cookies: [], origins: [] } });

const rig = new MirrorRig(sidenavPort, 'e2e-nav', { MUSTER_PLUGINS: '1' });
const origin = rig.origin;
const q = (win: string) => `host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent(win)}`;
const viewOf = (win: string) => `/mirror/view?${q(win)}`;
const termOf = (win: string) => `/mirror/terminal?${q(win)}`;

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
  const one = rig.transcript(cc.prompt('the prompt of window ONE') + cc.reply('Window one answered.'), 'one');
  const two = rig.transcript(cc.prompt('the prompt of window TWO') + cc.reply('Window two answered.'), 'two');
  const now = Math.floor(Date.now() / 1000);
  const windows = rig.transcript([`work|@1|1|1|claude|${now - 60}|${one}`, `work|@2|2|0|claude|${now - 120}|${two}`].join('\n') + '\n', 'windows');
  await rig.startHost({ MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
});

test.afterAll(async () => {
  await rig.stop();
});

const root = (page: Page) => page.locator('#sidebar-root');
const panel = (page: Page) => page.locator('#sidebar');
const toggle = (page: Page) => page.locator('#sidebar-open');

// openByToggle opens the nav with its hamburger and checks it is on screen,
// then closes it with its own close control.
async function openByToggle(page: Page): Promise<void> {
  await expect(root(page)).toHaveAttribute('data-open', 'false');
  await expect(panel(page), 'closed, the panel is off screen').not.toBeInViewport();
  await toggle(page).click();
  await expect(root(page)).toHaveAttribute('data-open', 'true');
  await expect(panel(page)).toBeInViewport({ ratio: 0.99 });
  await expect(toggle(page)).toHaveAttribute('aria-expanded', 'true');
  await page.locator('#sidebar-close').click();
  await expect(root(page)).toHaveAttribute('data-open', 'false');
  await expect(panel(page)).not.toBeInViewport();
}

// marks asserts which nav link the page marks as its own — the accent and
// aria-current — after the page's own script has run, and that no other link
// (Tasks in particular, the script's old fallback) is marked.
async function marks(page: Page, sel: string | null): Promise<void> {
  // The script has run: it binds the hamburger.
  await expect(toggle(page)).toHaveAttribute('data-cg-bound', '1');
  const current = panel(page).locator('[aria-current="page"]');
  const accented = panel(page).locator('a.bg-accent\\/15');
  if (sel === null) {
    await expect(current).toHaveCount(0);
    await expect(accented).toHaveCount(0);
    return;
  }
  const own = panel(page).locator(sel);
  await expect(own).toHaveCount(1);
  await expect(current).toHaveCount(1);
  await expect(own).toHaveAttribute('aria-current', 'page');
  await expect(accented).toHaveCount(1);
  await expect(own).toHaveClass(/(^|\s)bg-accent\/15(\s|$)/);
}

// The pages internal/api writes, and the entry each belongs to (a selector
// inside the nav), or null for a page no entry names.
const authored: { path: string; entry: string | null; content: string }[] = [
  { path: '/attention', entry: '#sidebar-attention', content: '[data-testid="attention-page"]' },
  { path: '/account', entry: '#sidebar-account', content: 'main h1:has-text("Account")' },
  { path: '/plugins', entry: null, content: 'main h1' },
  { path: '/flows', entry: null, content: '[data-testid="flows-page"]' },
];

// box is an element's place on the page, for "nothing moved".
async function box(page: Page, sel: string) {
  const b = await page.locator(sel).boundingBox();
  expect(b, `${sel} is drawn`).not.toBeNull();
  return b!;
}

for (const size of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 } } },
]) {
  test.describe(size.name, () => {
    test.use(size.use);
    const desktop = size.name === 'desktop';

    test('the server-authored pages carry the nav, opened by its toggle, marking their own entry', async ({ page }) => {
      await rig.login(page);
      for (const p of authored) {
        await page.goto(origin + p.path);
        await expect(page.locator(p.content).first(), `${p.path} drew its own content`).toBeVisible();
        await marks(page, p.entry);
        if (desktop) {
          // A desktop's rail: in view without a toggle, which is hidden.
          await expect(panel(page), `${p.path}: the rail is in view`).toBeInViewport({ ratio: 0.99 });
          await expect(toggle(page)).toBeHidden();
          // The content column clears the rail.
          const rail = await box(page, '#sidebar');
          const main = await box(page, 'main');
          expect(main.x, `${p.path}: the content starts right of the rail`).toBeGreaterThanOrEqual(rail.x + rail.width);
        } else {
          await openByToggle(page);
        }
      }
      // A link in it leaves the page: to the board.
      if (!desktop) await toggle(page).click();
      await panel(page).locator('a[data-tab="tasks"]').click();
      await expect.poll(() => new URL(page.url()).pathname).toBe('/tasks');
      await expect(page.locator('#panel-tasks')).toBeVisible();
    });

    test('the mirrored Rendered view and terminal carry it as an overlay; the view-swap frame carries none', async ({ page }) => {
      await rig.login(page);
      await expect.poll(() => rig.log.includes('connected to '), { message: `the host connected (log:\n${rig.log})`, timeout: 20_000 }).toBe(true);
      await expect
        .poll(async () => ((await (await page.request.get(origin + '/ui/agents')).text()).match(/data-mirror-claude/g) || []).length, {
          message: `both windows listed as Claude Code (log:\n${rig.log})`,
          timeout: 20_000,
        })
        .toBe(2);

      await page.goto(origin + viewOf('@1'));
      await expect(page.locator('#mv-log [data-bubble="user"]').first()).toContainText('window ONE');
      await expect(page.locator('#mv-status')).toHaveText('Live');
      await expect(toggle(page), 'the hamburger is shown at this size too').toBeVisible();
      // A mirrored window hangs off the Agents list, as an agent's page does.
      await marks(page, 'a[data-tab="agents"]');
      const before = { log: await box(page, '#mv-log'), compose: await box(page, '#mv-compose') };
      await openByToggle(page);
      // Nothing on the page moved for the nav being there, opened or closed.
      expect(await box(page, '#mv-log')).toEqual(before.log);
      expect(await box(page, '#mv-compose')).toEqual(before.compose);
      expect(before.compose.x, 'the pinned composer spans the window: no rail beside it').toBe(0);

      // The terminal, opened in the frame over this page: no nav in it.
      await page.locator('#view-terminal').click();
      const frame = page.locator('iframe[data-muster-view]');
      await expect(frame).toBeVisible();
      const framed = page.frameLocator('iframe[data-muster-view]');
      await expect(framed.locator('#term-status')).toHaveText('Connected');
      // POSITIVE CONTROL for the counts below: the frame's own header is there.
      await expect(framed.locator('#view-rendered')).toHaveCount(1);
      await expect(framed.locator('#sidebar-root')).toHaveCount(0);
      await expect(framed.locator('#sidebar-open')).toHaveCount(0);
      await expect(framed.locator('nav[aria-label="Main menu"]')).toHaveCount(0);

      // The terminal as its own page: the nav, as an overlay.
      await page.goto(origin + termOf('@1'));
      await expect(page.locator('#term-status')).toHaveText('Connected');
      await openByToggle(page);
      // …and the Rendered view in ITS frame: none.
      await page.locator('#view-rendered').click();
      await expect(frame).toBeVisible();
      await expect(framed.locator('#mv-status')).toHaveText('Live');
      await expect(framed.locator('#view-terminal')).toHaveCount(1);
      await expect(framed.locator('#sidebar-root')).toHaveCount(0);
      await expect(framed.locator('#sidebar-open')).toHaveCount(0);
    });
  });
}
