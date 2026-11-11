import { Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { clsInit, readCls, settle, ClsResult } from '../lib/cls';
import { tailClampPort } from '../lib/env';
import { cc, entryMatches, mirroredEntries, MirrorRig, mkchat } from '../lib/mirrorhost';
import { signIn } from '../lib/signin';

// AN AGENT'S LONG REPLY IS CLAMPED FROM ITS TAIL (bubble_clamp.go): the reply's
// LAST lines show, the fade and "Show more" are at its TOP, and opening it grows
// the earlier text upward with the end held where it was on screen. The
// reader's own long message is still clamped head-first. On every surface that
// draws a reply bubble: the mirrored Rendered view, the agent chat page and the
// chief panel — at a phone with touch (390×844) and at a desktop (1280×800),
// each set here, not inherited.
//
// 🔴 REGRESSION TEST: before this change every bubble was head-first (its first
// twelve lines, "Show more" under it) and the chief panel was not clamped at
// all. Every case below that reads a reply's lines fails there.
//
// Every message is invented fixture data.

const rig = new MirrorRig(tailClampPort, 'e2e-tail');
const origin = rig.origin;
const stamp = Date.now().toString(36);
const AGENT = `e2e-tail-${stamp}`;
const CHIEF = `e2e-tail-chief-${stamp}`;
const CLS_AGENT = `e2e-tail-cls-${stamp}`;
const SHORT = `e2e-tail-short-${stamp}`;

// lines is n paragraphs "<tag> line <i>": far over fifteen lines at any width.
const lines = (n: number, tag: string) => Array.from({ length: n }, (_, i) => `${tag} line ${i + 1}`).join('\n\n');
const first = (tag: string) => `${tag} line 1`;
const last = (tag: string, n: number) => `${tag} line ${n}`;
const N = 40;

const viewports = [
  { name: 'phone 390×844', touch: true, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', touch: false, use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
];

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(240_000);
  await rig.start();
  for (const a of [AGENT, CHIEF, CLS_AGENT, SHORT]) mkchat(a, null);
  // The agent chat page: earlier short turns (more than a screen of them, so
  // the log scrolls), then a long reply LAST (on screen at the end the page
  // opens at).
  const earlier: object[] = [];
  for (let i = 0; i < 8; i++) earlier.push({ role: 'user', content: `Earlier ask ${i}.` }, { role: 'assistant', content: `Earlier answer ${i}.` });
  mkchat(AGENT, [...earlier, { role: 'user', content: 'Check the fixture loader.' }, { role: 'assistant', content: lines(N, 'agentreply') }]);
  // …and one whose whole transcript fits on screen.
  mkchat(SHORT, [
    { role: 'user', content: 'Check the fixture loader.' },
    { role: 'assistant', content: lines(N, 'shortreply') },
  ]);
  // The chief: a long USER message, then a long reply last.
  mkchat(CHIEF, [
    { role: 'user', content: lines(30, 'chiefask') },
    { role: 'assistant', content: lines(N, 'chiefreply') },
  ], { MUSTER_E2E_DISPLAY_NAME: 'chief' });
  // The layout-shift case: a long transcript of long replies.
  const msgs: object[] = [];
  for (let i = 0; i < 40; i++) {
    msgs.push({ role: 'user', content: `Step ${i + 1}: check module-${i}.` });
    msgs.push({ role: 'assistant', content: lines(N, `cls${i}`) });
  }
  mkchat(CLS_AGENT, msgs);
});

test.afterEach(async () => {
  await rig.stopHost();
});

test.afterAll(async () => {
  try {
    for (const a of [AGENT, CHIEF, CLS_AGENT, SHORT]) mkchat(a, null);
  } finally {
    await rig.stop();
  }
});

// --- geometry ----------------------------------------------------------------

// line is where the paragraph reading exactly text sits, against what of its
// bubble is on screen (the bubble's box, cut by its log's): shown = wholly
// inside it, hidden = wholly outside it.
type Line = { top: number; shown: boolean; hidden: boolean };
const line = (bubble: Locator, text: string): Promise<Line> =>
  bubble.evaluate((b, text) => {
    const el = Array.from(b.querySelectorAll('p, li')).find((e) => (e.textContent || '').trim() === text);
    if (!el) throw new Error('no line ' + JSON.stringify(text) + ' in the bubble');
    const r = el.getBoundingClientRect();
    const br = b.getBoundingClientRect();
    const lr = b.closest('#mv-log, #chat-log')!.getBoundingClientRect();
    const top = Math.max(br.top, lr.top);
    const bottom = Math.min(br.bottom, lr.bottom);
    return { top: r.top, shown: r.top >= top - 1 && r.bottom <= bottom + 1, hidden: r.bottom <= top + 1 || r.top >= bottom - 1 };
  }, text);

// settled waits until the bubble is no longer animating and its height holds
// for two frames.
async function settled(bubble: Locator) {
  await expect(bubble).not.toHaveAttribute('data-clamp', 'moving');
  await expect
    .poll(() =>
      bubble.evaluate(
        (b) =>
          new Promise<boolean>((r) => {
            const h = b.getBoundingClientRect().height;
            requestAnimationFrame(() => requestAnimationFrame(() => r(Math.abs(b.getBoundingClientRect().height - h) < 0.5)));
          }),
      ),
    )
    .toBe(true);
}

const press = (touch: boolean, l: Locator) => (touch ? l.tap() : l.click());
const fromBottom = (log: Locator) => log.evaluate((l) => l.scrollHeight - l.scrollTop - l.clientHeight);

// THE TAIL-FIRST CASE, on whichever surface: reply is a long assistant bubble
// on screen, drawn closed. It shows its last line and not its first, "Show
// more" is ABOVE it; opening it keeps that last line at the same screen y and
// makes the first line reachable by scrolling up; "Show less", still above it,
// closes it to the end again.
async function tailCase(touch: boolean, reply: Locator, tag: string) {
  await expect(reply).toHaveAttribute('data-clamp-from', 'tail');
  await expect(reply).toHaveAttribute('data-clamp', 'closed');
  await reply.evaluate((b) => b.scrollIntoView({ block: 'end' }));
  expect(await line(reply, last(tag, N)), 'the reply shows its LAST line').toMatchObject({ shown: true });
  expect(await line(reply, first(tag)), 'and not its first').toMatchObject({ hidden: true });

  // "Show more" is at the top: the control right before the bubble, above it.
  const toggle = reply.locator('xpath=preceding-sibling::*[1]');
  await expect(toggle).toHaveAttribute('data-clamp-toggle', 'tail');
  await expect(toggle).toHaveText('Show more');
  const tb = (await toggle.boundingBox())!;
  const rb = (await reply.boundingBox())!;
  expect(tb.y + tb.height / 2, '"Show more" is above the bubble').toBeLessThan(rb.y);
  expect(tb.height, 'a 44px thumb target').toBeGreaterThanOrEqual(44);

  // Open it: the last line does not move; the earlier text opened upward.
  // The control is brought on screen FIRST, so the tap's own scroll-into-view
  // cannot move the log between the two measurements.
  await toggle.scrollIntoViewIfNeeded();
  const before = await line(reply, last(tag, N));
  expect(before.shown, 'the last line is on screen with its control').toBe(true);
  await press(touch, toggle);
  await expect(reply).toHaveAttribute('data-clamp', 'open');
  await settled(reply);
  const after = await line(reply, last(tag, N));
  expect(Math.abs(after.top - before.top), `the last line stayed put (was ${before.top}, now ${after.top})`).toBeLessThanOrEqual(2);
  const [h, sh] = await reply.evaluate((e) => [e.clientHeight, e.scrollHeight]);
  expect(Math.abs(h - sh), 'drawn at its full height').toBeLessThanOrEqual(1);
  await expect(toggle).toHaveText('Show less');
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');

  // The first line is reachable by scrolling the log UP to it.
  const up = await reply.evaluate((b, text) => {
    const el = Array.from(b.querySelectorAll('p, li')).find((e) => (e.textContent || '').trim() === text)!;
    const l = b.closest('#mv-log, #chat-log') as HTMLElement;
    const before = l.scrollTop;
    l.scrollTop += el.getBoundingClientRect().top - l.getBoundingClientRect().top - 60;
    return before - l.scrollTop;
  }, first(tag));
  expect(up, 'the log scrolled UP to reach the first line').toBeGreaterThan(0);
  expect(await line(reply, first(tag)), 'the first line, scrolled to').toMatchObject({ shown: true });
  expect(await reply.evaluate((b) => b.scrollTop), 'nothing of it is scrolled away inside the bubble').toBe(0);

  // "Show less", still at the top, closes it to its end again.
  const tb2 = (await toggle.boundingBox())!;
  expect(tb2.y + tb2.height / 2, '"Show less" is above the bubble').toBeLessThan((await reply.boundingBox())!.y);
  await press(touch, toggle);
  await expect(reply).toHaveAttribute('data-clamp', 'closed');
  await settled(reply);
  await expect(toggle).toHaveText('Show more');
  await reply.evaluate((b) => b.scrollIntoView({ block: 'end' }));
  expect(await line(reply, last(tag, N)), 'closed again: the last line').toMatchObject({ shown: true });
  expect(await line(reply, first(tag)), 'closed again: not the first').toMatchObject({ hidden: true });
}

// THE HEAD-FIRST CASE: the reader's own long message still shows its first
// line, with "Show more" UNDER it.
async function headCase(mine: Locator, tag: string, n: number) {
  await expect(mine).not.toHaveAttribute('data-clamp-from', /.*/);
  await expect(mine).toHaveAttribute('data-clamp', 'closed');
  await mine.evaluate((b) => b.scrollIntoView({ block: 'start' }));
  expect(await line(mine, first(tag)), 'the user message shows its FIRST line').toMatchObject({ shown: true });
  expect(await line(mine, last(tag, n)), 'and not its last').toMatchObject({ hidden: true });
  const toggle = mine.locator('xpath=following-sibling::*[1]');
  await expect(toggle).toHaveAttribute('data-clamp-toggle', '');
  await expect(toggle).toHaveText('Show more');
}

// --- the mirrored view -------------------------------------------------------

async function openView(page: Page, transcript: string) {
  await rig.startHost({ MUSTER_MIRROR_E2E_SESSIONS: 'main', MUSTER_MIRROR_E2E_CLAUDE: 'main=' + rig.transcript(transcript) });
  await signIn(page, origin);
  await expect
    .poll(
      async () => {
        if (!rig.log.includes('muster-mirror: connected to ')) return 'muster-mirror not connected yet';
        const rows = mirroredEntries(await (await page.request.get(origin + '/ui/nav/agents')).text()).filter((e) => entryMatches(e.markup, 'data-mirror-claude'));
        if (!rows.length) return 'main not reported as a Claude pane';
        if (rows.some((e) => e.state === 'offline')) return 'the host is listed offline';
        return 'ready';
      },
      { message: `this test's muster-mirror connected (log:\n${rig.log})`, timeout: 20_000 },
    )
    .toBe('ready');
  await page.goto(origin + rig.viewPath('main'));
  expect(new URL(page.url()).pathname, 'the rendered view opened, not the terminal').toBe('/mirror/view');
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
}

// The agent chat page.
async function openAgent(page: Page, name: string) {
  await signIn(page, origin);
  await page.goto(origin + '/agents/' + name);
  await expect(page.locator('#chat-log [data-bubble]').first()).toBeVisible();
}

// The chief panel, opened on the tasks page by its delegated toggle.
async function openChiefPanel(page: Page) {
  await signIn(page, origin);
  await page.goto(origin + '/tasks');
  await page.evaluate(() => {
    const b = document.createElement('button');
    b.setAttribute('data-chief-panel-toggle', '');
    b.style.cssText = 'position:fixed;left:-9999px;top:0';
    document.body.appendChild(b);
    b.click();
  });
  await expect(page.locator('#chief-panel')).toHaveAttribute('data-chief-panel-open', '1');
  await expect(page.locator('#chief-panel #chat-log [data-bubble="assistant"]')).toHaveCount(1, { timeout: 15_000 });
}

// refreshChatLog swaps #chat-log in full, as a task:changed refresh does with
// a changed transcript (see mirror-chat.spec.ts's copy for why the version is
// dropped first), and resolves after the swap's own mutation callbacks.
async function refreshChatLog(page: Page) {
  await page.evaluate(
    () =>
      new Promise<void>((resolve) => {
        const on = (e: Event) => {
          if ((e as CustomEvent).detail?.target?.id !== 'chat-log') return;
          document.body.removeEventListener('htmx:afterSwap', on);
          requestAnimationFrame(() => resolve());
        };
        document.body.addEventListener('htmx:afterSwap', on);
        document.getElementById('chat-log')!.removeAttribute('data-chat-version');
        document.body.dispatchEvent(new Event('task:changed'));
      }),
  );
}

for (const vp of viewports) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('mirrored view: a long reply shows its end, opens upward in place and closes; a long prompt shows its start', async ({ page }) => {
      await openView(page, cc.prompt('a short question') + cc.reply(lines(N, 'mvreply')) + cc.prompt(lines(30, 'mvask')));
      const log = page.locator('#mv-log');
      const reply = log.locator('[data-bubble="assistant"]').first();
      await expect(reply).toContainText(last('mvreply', N));
      await headCase(log.locator('[data-bubble="user"]').last(), 'mvask', 30);
      await tailCase(vp.touch, reply, 'mvreply');
    });

    // 🔴 THE END STAYS PUT ALSO WHERE STICK-TO-BOTTOM CANNOT HOLD IT: the reply
    // mid-log, with more under it and the log scrolled well above its end.
    test('mirrored view: a reply mid-log opens with its end held in place', async ({ page }) => {
      let tail = '';
      for (let i = 0; i < 12; i++) tail += cc.prompt(`later ${i}`) + cc.reply(`later reply ${i}`);
      await openView(page, cc.prompt('first') + cc.reply(lines(N, 'midreply')) + tail);
      const log = page.locator('#mv-log');
      const reply = log.locator('[data-bubble="assistant"]').first();
      await expect(reply).toHaveAttribute('data-clamp', 'closed');
      await expect(log.locator('[data-bubble="assistant"]')).toHaveCount(13);
      // The reply, its control included, a little below the log's top: its
      // end is mid-log, with more of the log under it.
      const toggle = reply.locator('xpath=preceding-sibling::*[1]');
      await toggle.evaluate((t) => {
        const l = t.closest('#mv-log') as HTMLElement;
        l.scrollTop += t.getBoundingClientRect().top - l.getBoundingClientRect().top - 16;
      });
      expect(await fromBottom(log), 'the log is well above its end').toBeGreaterThan(120);
      const before = await line(reply, last('midreply', N));
      expect(before.shown, 'the last line is on screen').toBe(true);
      await press(vp.touch, toggle);
      await expect(reply).toHaveAttribute('data-clamp', 'open');
      await settled(reply);
      const after = await line(reply, last('midreply', N));
      expect(Math.abs(after.top - before.top), `the last line stayed put (was ${before.top}, now ${after.top})`).toBeLessThanOrEqual(2);
    });

    test('agent chat page: a long reply shows its end, opens upward in place and closes', async ({ page }) => {
      await openAgent(page, AGENT);
      const reply = page.locator('#chat-log [data-bubble="assistant"]').last();
      await tailCase(vp.touch, reply, 'agentreply');
    });

    // ⚠ THE BOUNDARY: a transcript that does not fill the log leaves nothing
    // to scroll, so the end CANNOT stay put — it moves down into the empty
    // space under it. It must still end on screen.
    test('agent chat page: a reply in a transcript that fits on screen opens with its end still on screen', async ({ page }) => {
      await openAgent(page, SHORT);
      const log = page.locator('#chat-log');
      const reply = log.locator('[data-bubble="assistant"]').last();
      await expect(reply).toHaveAttribute('data-clamp', 'closed');
      expect(await log.evaluate((l) => l.scrollHeight - l.clientHeight), 'control: the log does not scroll').toBeLessThanOrEqual(1);
      expect(await line(reply, last('shortreply', N))).toMatchObject({ shown: true });
      await press(vp.touch, reply.locator('xpath=preceding-sibling::*[1]'));
      await expect(reply).toHaveAttribute('data-clamp', 'open');
      await settled(reply);
      expect(await line(reply, last('shortreply', N)), 'the last line is still on screen').toMatchObject({ shown: true });
      expect(await fromBottom(log), 'the log is at its end').toBeLessThan(2);
    });

    test('agent chat page: an opened reply stays open across a chat-log refresh, its control still on top', async ({ page }) => {
      await openAgent(page, AGENT);
      const log = page.locator('#chat-log');
      const reply = log.locator('[data-bubble="assistant"]').last();
      const id = await reply.getAttribute('data-msg-id');
      expect(id).toMatch(/^[1-9][0-9]*$/);
      await expect(reply).toHaveAttribute('data-clamp', 'closed');
      await press(vp.touch, reply.locator('xpath=preceding-sibling::*[1]'));
      await expect(reply).toHaveAttribute('data-clamp', 'open');
      await settled(reply);
      await reply.evaluate((e) => ((e as any).__old = true));
      await refreshChatLog(page);
      const fresh = log.locator(`[data-msg-id="${id}"]`);
      await expect(fresh).toHaveCount(1);
      expect(await fresh.evaluate((e) => !(e as any).__old), 'the bubble was redrawn').toBe(true);
      await expect(fresh).toHaveAttribute('data-clamp', 'open');
      const toggle = fresh.locator('xpath=preceding-sibling::*[1]');
      await expect(toggle).toHaveAttribute('data-clamp-toggle', 'tail');
      await expect(toggle).toHaveText('Show less');
      // Closed again with it, and that is remembered across the next refresh.
      await press(vp.touch, toggle);
      await expect(fresh).toHaveAttribute('data-clamp', 'closed');
      await settled(fresh);
      await refreshChatLog(page);
      await expect(log.locator(`[data-msg-id="${id}"]`)).toHaveAttribute('data-clamp', 'closed');
    });

    test('chief panel: a long chief reply shows its end, opens upward in place and closes; a long owner message shows its start', async ({ page }) => {
      await openChiefPanel(page);
      const log = page.locator('#chief-panel #chat-log');
      await expect(log).toContainText(last('chiefreply', N));
      await headCase(log.locator('[data-bubble="user"]').last(), 'chiefask', 30);
      await tailCase(vp.touch, log.locator('[data-bubble="assistant"]').last(), 'chiefreply');
    });

    // A long transcript of tail-clamped replies opens at its end with nothing
    // moved: each reply is scrolled to its end in the same task that clamped
    // it, before the first paint.
    test('a long transcript of tail-clamped replies opens with no layout shift', async ({ page }) => {
      await signIn(page, origin);
      await page.addInitScript(clsInit);
      await page.goto(origin + '/agents/' + CLS_AGENT);
      await settle(page);
      const r: ClsResult = await readCls(page);
      const log = page.locator('#chat-log');
      await expect(log.locator('[data-bubble="assistant"][data-clamp="closed"]')).toHaveCount(40);
      expect(await fromBottom(log), 'opened at its end').toBeLessThan(48);
      const reply = log.locator('[data-bubble="assistant"]').last();
      expect(await line(reply, last('cls39', N)), 'the newest reply shows its last line').toMatchObject({ shown: true });
      expect(await line(reply, first('cls39')), 'and not its first').toMatchObject({ hidden: true });
      const moved = +(r.total + r.excluded).toFixed(5);
      expect(moved, `layout shift: ${JSON.stringify(r.shifts)} excluded: ${JSON.stringify(r.excludedShifts)}`).toBeLessThan(0.01);
    });
  });
}
