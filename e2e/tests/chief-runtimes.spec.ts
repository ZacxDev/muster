import { Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { chiefRuntimesPort, versionTerm } from '../lib/env';
import { mkchat } from '../lib/mirrorhost';
import { startServer, Running } from '../lib/server';
import { bar, openSessions, sessionsTab } from '../lib/chief-sessions';

// THE CHIEF'S SESSIONS ON MORE THAN ONE RUNTIME (internal/api/session_runtime.go),
// in a real browser, against a real muster-server: the Sessions tab's "+ New
// session" menu offers Claude Code's models, and a session started there is
// listed with its model; one chat is the PRIMARY (where plugin messages and
// `chief ask` go), and a row's ⋯ "Make primary" moves it. (These were the chief
// panel's thread list's — its "New session" picker and "Make default" — until
// the list was folded into the Sessions tab.)
//
// The chief is a claude-code agent of this spec's own (e2e/mkchat), on a server
// of its own over the noop driver. Its MUSTER_CHIEF_SESSION_RUNTIMES still names
// the retired `gateway` beside claude-code, as a deployment from before its
// removal does: the server boots, ignores it, and offers claude-code alone.
// Nothing is provisioned and no turn is sent: what is under test is the picker,
// the stored session and the list.

const origin = `http://localhost:${chiefRuntimesPort}`;
const chief = 'e2e-runtimes-chief-' + Date.now().toString(36);

let server: Running | undefined;

test.use({ storageState: { cookies: [], origins: [] }, viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

test.beforeAll(async () => {
  test.setTimeout(120_000);
  server = await startServer(versionTerm, chiefRuntimesPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-runtimes',
    // The retired `gateway` entry: ignored with one warning, never refused.
    MUSTER_CHIEF_SESSION_RUNTIMES: 'claude-code,gateway',
  });
  mkchat(chief, null);
  mkchat(chief, [{ title: 'the original', hoursAgo: 2, messages: [{ role: 'user', content: 'hello chief' }] }], {
    MUSTER_E2E_DISPLAY_NAME: 'chief',
    MUSTER_E2E_THREADS: '1',
    MUSTER_E2E_KIND: 'claude-code',
    MUSTER_E2E_CC_ACCOUNT: 'alpha',
  });
});

test.afterAll(async () => {
  mkchat(chief, null);
  await server?.stop();
});

// openPanelTab opens the chief panel (its toggle is delegated on the document)
// and then its sheet's Sessions tab.
async function openPanelTab(page: Page, hasTouch = true) {
  if ((await page.locator('#chief-panel').getAttribute('data-chief-panel-open')) !== '1') {
    await page.evaluate(() => {
      const b = document.createElement('button');
      b.setAttribute('data-chief-panel-toggle', '');
      b.style.cssText = 'position:fixed;left:-9999px;top:0';
      document.body.appendChild(b);
      b.click();
    });
    await expect(page.locator('#chief-panel')).toHaveAttribute('data-chief-panel-open', '1');
  }
  await expect(page.locator('#chief-panel-index-open')).toBeVisible({ timeout: 15_000 });
  await openSessions(sessionsTab(page, 'panel'), hasTouch);
}

// row is a chat's row in the panel's Sessions tab, by its title.
const row = (page: Page, title: string): Locator =>
  sessionsTab(page, 'panel').locator('[data-chief-chip]').filter({ has: page.locator(`[data-chief-chat-row][title="${title}"]`) });

test('a session is started on a picked Claude model from the panel\'s Sessions tab, listed with it, and can be made the primary', async ({ page }) => {
  await signIn(page, origin);
  await page.goto(origin + '/tasks');
  await openPanelTab(page);
  const tab = sessionsTab(page, 'panel');
  const menu = page.locator('#chief-panel-new-chat-menu');

  // The "+" menu: Default and the Claude models; the retired gateway entry
  // draws nothing, and there is no OpenRouter search on this server.
  await tab.locator('button[data-chief-side-chat]').tap();
  await expect(menu).toBeVisible();
  await expect(menu.getByRole('menuitem')).toHaveText(['Default', 'Opus', 'Sonnet', 'Haiku']);
  await expect(menu.locator('[data-chief-new-chat-openrouter]')).toHaveCount(0);
  await page.keyboard.press('Escape');
  // The original chat is the primary, and names no model of its own.
  const original = row(page, 'the original');
  await expect(original).toHaveAttribute('data-chief-chip-kind', 'primary');
  await expect(original.locator('[data-chief-chip-model]')).toHaveCount(0);

  // Sonnet, then Haiku.
  //
  // 🔴 WAIT FOR THE START'S OWN ANSWER — THE ACTIVE SESSION MOVING OFF THE ONE
  // SHOWN BEFORE IT — not for "some session is active": the original already
  // is, so that wait would pass before the answer landed.
  const active = page.locator('[data-chief-active-session]');
  const made: Record<string, string> = {};
  for (const model of ['Sonnet', 'Haiku']) {
    const was = await active.getAttribute('data-chief-active-session');
    await openPanelTab(page);
    await tab.locator('button[data-chief-side-chat]').tap();
    await expect(menu).toBeVisible();
    await menu.getByRole('menuitem', { name: model }).tap();
    await expect(active).not.toHaveAttribute('data-chief-active-session', was ?? '');
    await expect(active).not.toHaveAttribute('data-chief-active-session', '');
    made[model] = (await active.getAttribute('data-chief-active-session'))!;
  }
  await openPanelTab(page);
  await expect(tab.locator(`[data-chief-chip="${made.Haiku}"] [data-chief-chip-model]`)).toHaveText('haiku');
  await expect(tab.locator(`[data-chief-chip="${made.Sonnet}"] [data-chief-chip-model]`)).toHaveText('sonnet');
  await expect(tab.locator('[data-chief-chip-kind="primary"]')).toHaveCount(1);
  await expect(original).toHaveAttribute('data-chief-chip-kind', 'primary');

  // Make the haiku chat the primary from its ⋯: the ★ moves to it.
  await tab.locator(`[data-chief-chip="${made.Haiku}"] button[aria-haspopup="menu"]`).tap();
  await page.locator(`#chief-panel-chat-menu-${made.Haiku}`).getByRole('menuitem', { name: 'Make primary' }).tap();
  await expect(tab.locator(`[data-chief-chip="${made.Haiku}"]`)).toHaveAttribute('data-chief-chip-kind', 'primary');
  await expect(tab.locator('[data-chief-chip-kind="primary"]')).toHaveCount(1);
  await expect(original.locator('button[aria-haspopup="menu"]')).toHaveCount(1);

  // It is stored, not only drawn: a fresh load of the panel draws the same.
  await page.reload();
  await openPanelTab(page);
  await expect(tab.locator(`[data-chief-chip="${made.Haiku}"]`)).toHaveAttribute('data-chief-chip-kind', 'primary');
});

test('a bad model is refused and starts no session', async ({ page }) => {
  await signIn(page, origin);
  const chats = async () => ((await (await page.request.get(origin + '/ui/chief/panel')).text()).match(/<li data-chief-chip=/g) || []).length;
  const count = await chats();
  // The session-create route (the side chat route's parent), with a model the store refuses.
  const side = /hx-post="(\/agents\/\d+\/sessions)\/side\?surface=chief-panel"/.exec(await (await page.request.get(origin + '/ui/chief/panel')).text());
  expect(side, 'the panel names the side chat route').not.toBeNull();
  // A bad Claude model, and the retired gateway runtime with a model it once took.
  for (const form of [{ runtime: 'claude-code', model: 'not a model' }, { runtime: 'gateway', model: 'openrouter/vendor-x/model-y' }]) {
    const r = await page.request.post(origin + side![1], { form, headers: { Origin: origin } });
    expect(r.status()).toBe(400);
  }
  expect(await chats()).toBe(count);
});

// "Continue in a new session" (internal/api chief_continue.go): on a SIDE chat
// the Sessions tab offers it — on the chief page and in the panel (it was under
// the panel's thread list, the panel only) — with the "+" menu's runtime and
// models, posting to that chat's continue route the way each surface answers.
// Nothing is submitted here (no turn is sent on this server); the route itself
// is covered by internal/api chief_continue_pg_test.go.
for (const vp of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`Continue on a ${vp.name}`, () => {
    test.use(vp.use);
    test('a side chat offers Continue in a new session in the Sessions tab, on the page and in the panel', async ({ page }) => {
      const tap = (l: Locator) => (vp.use.hasTouch ? l.tap() : l.click());
      await signIn(page, origin);
      // The page, on a side chat.
      await page.goto(origin + '/agents/' + chief);
      const tab = sessionsTab(page, 'page');
      await openSessions(tab, vp.use.hasTouch);
      const side = tab.locator('[data-chief-chip-kind="side"]').first();
      const sid = (await side.getAttribute('data-chief-chip'))!;
      expect(sid, 'the spec above left a side chat').toBeTruthy();
      await tap(side.locator('[data-chief-chat-row]'));
      await page.waitForURL((u) => new URL(u.toString()).searchParams.get('session') === sid);
      await openSessions(tab, vp.use.hasTouch);
      const group = tab.locator('[data-chief-continue]');
      await expect(group).toHaveCount(1);
      await tap(group.locator('summary'));
      const form = group.locator('form[data-chief-continue-form="claude-code"]');
      await expect(form).toBeVisible();
      await expect(form).toHaveAttribute('hx-post', new RegExp(`/sessions/${sid}/continue$`));
      await expect(form).toHaveAttribute('hx-swap', 'none');
      await expect(form.locator('select[name="model"] option')).toHaveText(['opus', 'sonnet', 'haiku']);
      await expect(form.getByRole('button', { name: 'Continue' })).toBeVisible();
      // The primary has none.
      await page.keyboard.press('Escape');
      // The panel (it opens on the chat viewed last: this side chat).
      await page.goto(origin + '/tasks');
      await openPanelTab(page, vp.use.hasTouch);
      await expect(page.locator('[data-chief-active-session]')).toHaveAttribute('data-chief-active-session', sid);
      const pgroup = sessionsTab(page, 'panel').locator('[data-chief-continue]');
      await tap(pgroup.locator('summary'));
      const pform = pgroup.locator('form[data-chief-continue-form="claude-code"]');
      await expect(pform).toBeVisible();
      await expect(pform).toHaveAttribute('hx-post', new RegExp(`/sessions/${sid}/continue\\?surface=chief-panel$`));
      await expect(pform).toHaveAttribute('hx-target', '#chief-panel-body');
    });
  });
}

// THE SIDE CHAT "+" MODEL MENU (internal/ui chiefNewChatChip): with the
// runtimes gate on, "+ New session" in the chief page's Sessions tab opens a
// small menu
// (Default, Opus, Sonnet, Haiku), not a modal. Sonnet opens a new side chat on
// sonnet: its row is current and tagged "sonnet" (so is the bar naming the chat
// on screen), the cursor is in its message
// box, and the tag is the STORED model (a reload draws it again). The gate-off
// "+" (a chat at once) is chief-ux.spec.ts's.
test('"+" opens a model menu; a chat picked on Sonnet lands focused with a sonnet tag that survives a reload', async ({ page }) => {
  const strip = () => sessionsTab(page, 'page');
  const chipIds = () => strip().locator('[data-chief-chip]').evaluateAll((els) => els.map((e) => e.getAttribute('data-chief-chip') ?? ''));
  const menu = page.locator('#chief-new-chat-menu');
  // pick taps "+", then the entry named name, and returns the chat it landed on.
  const pick = async (name: string) => {
    const url0 = page.url();
    const plus = strip().locator('button[data-chief-side-chat]');
    await openSessions(strip(), true);
    await expect(plus).toHaveAccessibleName('New session');
    await plus.tap();
    await expect(menu).toBeVisible();
    await expect(menu.getByRole('menuitem')).toHaveText(['Default', 'Opus', 'Sonnet', 'Haiku']);
    // A menu, not a navigation: nothing is opened yet.
    expect(page.url()).toBe(url0);
    await menu.getByRole('menuitem', { name }).tap();
    await page.waitForURL((u) => u.toString() !== url0);
    return new URL(page.url()).searchParams.get('session')!;
  };
  await signIn(page, origin);
  await page.goto(origin + '/agents/' + chief);
  await expect(bar(page, 'page')).toBeVisible();

  // Default first: the specs above leave an EMPTY sonnet chat as the newest
  // side chat, which "+" reuses on the same model (internal/api openSideChat).
  // A Default chat is on another model, so it is new, untagged, and makes the
  // Sonnet pick below a new chat too.
  let before = await chipIds();
  const def = await pick('Default');
  expect(before, 'Default opened a NEW chat').not.toContain(def);
  await expect(strip().locator(`[data-chief-chat-row="${def}"]`)).toHaveAttribute('aria-current', 'true');
  await expect(strip().locator(`[data-chief-chip="${def}"] [data-chief-chip-model]`)).toHaveCount(0);

  before = await chipIds();
  const sid = await pick('Sonnet');
  expect(before, 'Sonnet opened a NEW chat').not.toContain(sid);
  await expect(strip().locator(`[data-chief-chat-row="${sid}"]`)).toHaveAttribute('aria-current', 'true');
  await expect(strip().locator(`[data-chief-chip="${sid}"] [data-chief-chip-model]`)).toHaveText('sonnet');
  await expect(bar(page, 'page').locator('[data-chief-chip-model]'), 'the bar names the chat on screen, with its model').toHaveText('sonnet');
  await expect(page.locator('#chat-input'), "the cursor is in the new chat's message box").toBeFocused();
  // The Default chat, on the chief's own model, is still untagged beside it.
  await expect(strip().locator(`[data-chief-chip="${def}"] [data-chief-chip-model]`)).toHaveCount(0);

  await page.reload();
  await expect(strip().locator(`[data-chief-chip="${sid}"] [data-chief-chip-model]`)).toHaveText('sonnet');
  await expect(strip().locator(`[data-chief-chat-row="${sid}"]`)).toHaveAttribute('aria-current', 'true');
});
