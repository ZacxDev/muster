import { Page } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { chiefMobilePort, dsn, repoRoot, sttToken, versionA } from '../lib/env';
import { Running, startServer } from '../lib/server';
import { signIn } from '../lib/signin';
import { agentMenu, openAgentMenu } from '../lib/agent-menu';

// A CLAUDE-CODE AGENT'S PAGE ON A PHONE (an operator report: "the layout is
// unintuitive and looks broken on mobile"). The agent the chief usually is: a
// claude-code agent whose account failed over, with several long questions
// waiting on the operator. What was broken, and what each case pins:
//
//   header  the second row held a "kind Claude Code · <account>" chip and a
//           "failed over from … · rate_limited · resets …" chip, each wrapping
//           into a tall blob at 412px, so the row grew to ~5 lines and pushed
//           the view switch to the edge. It was then ONE line: the account
//           name as text and a "↻ switched" marker whose detail opened on a
//           TAP. Since PR #527 the header is ONE ROW in all (internal/ui
//           agent_header_folded.go): account, failover marker with its whole
//           line, and Terminal are in the ⋯ menu, one TAP away (a title=
//           tooltip never shows on a touch screen).
//   asks    the block of waiting questions above the composer took the screen
//           (a long question read as cut off mid-sentence, the conversation
//           left a sliver, the composer pushed under the bottom bar). Now
//           there is NO block (operator decision): the header counts them in a
//           badge that opens the Asks page filtered to them, and the composer
//           and the conversation keep the room they have with none waiting.
//
// ⚠ THE AGENT HERE IS NOT THE CHIEF (its display name is its slug): the page a
// DISPATCHED claude-code agent draws, which folds the same way the chief's does
// (e2e/tests/chief-fold-in.spec.ts pins both side by side).
//
// Every value here is invented fixture data. The agent, its failover and its
// asks are rows e2e/ccfix and e2e/pluginfix write (a failover needs a real
// runtime reporting a rate limit, which the noop driver has none of).

const origin = `http://localhost:${chiefMobilePort}`;
const stamp = Date.now().toString(36);
const NAME = `e2e-ccm-${stamp}`;
const OWNER = '1';
const ASKS = 7;
const FAILOVER = 'from alpha · rate_limited · resets Nov 6 22:00 UTC';

const go = (pkg: string, ...args: string[]) =>
  execFileSync('go', ['run', pkg, ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();
// appendToLatest stores one owner line in agent name's latest chat
// (e2e/mkchat's MUSTER_E2E_APPEND).
const appendToLatest = (name: string, text: string) =>
  execFileSync('go', ['run', './e2e/mkchat'], {
    cwd: repoRoot,
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_E2E_AGENT: name, MUSTER_E2E_APPEND: '1' },
    input: JSON.stringify([{ role: 'user', content: text }]),
  });

const question = (i: number) =>
  `Question ${i + 1}: the nightly fleet check flagged node gpu-${i}-a as not ready and marked it as a control-plane node, ` +
  `with a note to confirm before any shutdown. It was first seen four minutes before the check and has rebooted once this week; ` +
  `its zone is otherwise healthy and its metrics came back after the earlier telemetry gap. Two sessions are working in that area ` +
  `right now (one rolling out node updates, one draining a pool). Should I cordon it and page the owner, or leave it for the rollout?`;
// Every question offers two long options, as a real chief's does (a live card
// with these is what clipped at 0.148.0: the fixture had none).
// The conversation's floor: #chat-log's own min-height (7.5rem, internal/ui
// agentChatPane).
const MIN_LOG = 120;
const OPTIONS = ['agent_dispatch on pod/gateway: start a new agent on it', 'agent_dispatch on pod/claude-code: start a new agent on it'];
// The chief: a claude-code agent displayed as "chief".
const CHIEF = `e2e-ccm-chief-${stamp}`;

let server: Running | undefined;

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, chiefMobilePort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha,beta',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-alpha',
    MUSTER_AGENT_CC_TOKEN_BETA: 'sk-ant-oat01-FAKE-e2e-beta',
    // Dictation on, as on a real deployment: every answer box carries its mic
    // (nothing is ever sent to this address — no test records).
    MUSTER_STT_URL: 'http://127.0.0.1:9',
    MUSTER_STT_TOKEN: sttToken,
  });
  go('./e2e/ccfix', 'agent', OWNER, NAME, 'beta', `e2e-ccm-token-${stamp}`);
  go('./e2e/ccfix', 'failover', OWNER, NAME, 'alpha', 'rate_limited', '2026-11-06T22:00:00Z');
  for (let i = 0; i < ASKS; i++) go('./e2e/ccfix', 'ask', OWNER, NAME, question(i), ...OPTIONS);
  // The CHIEF (a claude-code agent whose display name is "chief"): its page's
  // sheet, header and layout (the describes at the end of this file).
  go('./e2e/ccfix', 'agent', OWNER, CHIEF, 'beta', `e2e-ccm-chief-token-${stamp}`, 'chief');
});

test.afterAll(async () => {
  try {
    go('./e2e/ccfix', 'rmagent', OWNER, NAME);
    go('./e2e/ccfix', 'rmagent', OWNER, CHIEF);
  } finally {
    await server?.stop();
  }
});

// shot saves a screenshot when MUSTER_E2E_CHIEF_SHOTS names a directory (the
// before/after images a reviewer looks at); a generator, never an assertion.
async function shot(page: Page, name: string) {
  const dir = process.env.MUSTER_E2E_CHIEF_SHOTS;
  if (dir) await page.screenshot({ path: path.join(dir, name) });
}

// headerRow is the header's ONE row (a claude-code agent's page folds its
// status row into the ⋯ menu — internal/ui agent_header_folded.go): the row the
// ⋯ trigger sits in.
const headerRow = (page: Page) => page.locator('header > div').filter({ has: page.locator('#agent-actions-open') });

// badge is the header's asks badge (ui.AskerAsksBadge).
const badge = (page: Page) => page.locator('header [data-asker-asks-link]');

async function open(page: Page) {
  await signIn(page, origin);
  await page.goto(origin + '/agents/' + NAME);
  await expect(page.locator('header #agent-actions-open')).toBeVisible();
  await expect(badge(page)).toHaveAttribute('data-count', String(ASKS));
  // The page reveals its transcript and composer once laid out.
  await expect(page.locator('#chat-form')).toBeVisible();
}

// noAsksBlock: the page draws no asks block and no ask card — the asks are the
// header's badge alone.
async function noAsksBlock(page: Page) {
  await expect(page.locator('#asker-asks, #asker-asks-chip, [data-asker-asks-hide]'), 'no asks block').toHaveCount(0);
  await expect(page.locator('main [data-attention-item]'), 'no ask card on the page').toHaveCount(0);
}

// noSideScroll: nothing on the page is wider than the viewport.
async function noSideScroll(page: Page) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect.soft(sw, 'the page does not scroll sideways').toBeLessThanOrEqual(cw);
}

async function headerIsOneLine(page: Page, how: 'click' | 'tap') {
  const row = headerRow(page);
  const box = (await row.boundingBox())!;
  // One line of the ⋯'s 44px, plus the row's py-1.5 (12px).
  expect.soft(box.height, 'the header\'s row is one line (the ⋯\'s 44px + 12px padding)').toBeLessThanOrEqual(57);
  const [sw, cw] = await row.evaluate((el) => [el.scrollWidth, el.clientWidth]);
  expect.soft(sw, 'nothing in the header\'s row overflows it').toBeLessThanOrEqual(cw);
  await expect.soft(page.locator('header'), 'no kind label').not.toContainText('Claude Code');
  // The account is named as text (no "kind Claude Code" label) — in the ⋯ menu.
  await expect.soft(page.locator('[data-cc-account]')).toBeHidden();
  await openAgentMenu(page, how);
  await expect.soft(agentMenu(page).locator('[data-cc-account]')).toHaveText('beta');
  await expect.soft(agentMenu(page), 'no kind label').not.toContainText('Claude Code');
  await page.keyboard.press('Escape');
  await expect(agentMenu(page)).toBeHidden();
}

// composerOnScreen: the composer is on screen and the conversation keeps real
// room.
async function composerOnScreen(page: Page, minLog: number) {
  const vh = page.viewportSize()!.height;
  const compose = (await page.locator('#chat-form').boundingBox())!;
  const log = (await page.locator('#chat-log').boundingBox())!;
  expect.soft(compose.y + compose.height, 'the composer is on screen').toBeLessThanOrEqual(vh + 1);
  expect.soft(log.height, 'the conversation keeps room').toBeGreaterThanOrEqual(minLog);
}

test.describe('on a phone', () => {
  test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, permissions: ['microphone'], storageState: { cookies: [], origins: [] } });

  test('the header is one line and the failover detail opens on a tap', async ({ page }) => {
    await open(page);
    await shot(page, 'chief-phone.png');
    await noSideScroll(page);
    await headerIsOneLine(page, 'tap');

    // The failover: a marker and its whole line in the ⋯ menu, one TAP away.
    const marker = page.locator('[data-failover]');
    const detail = page.locator('[data-failover-detail]');
    await expect(marker).toBeHidden();
    await expect(detail).toBeHidden();
    await openAgentMenu(page, 'tap');
    await expect(agentMenu(page).locator('[data-failover]')).toBeVisible();
    await expect(marker).toHaveText('↻Account switched');
    await expect(detail).toBeVisible();
    await expect(detail).toContainText(FAILOVER);
    const d = (await detail.boundingBox())!;
    expect.soft(d.x, 'the detail is inside the viewport (left)').toBeGreaterThanOrEqual(0);
    expect.soft(d.x + d.width, 'the detail is inside the viewport (right)').toBeLessThanOrEqual(412);
    expect.soft((await headerRow(page).boundingBox())!.height, 'opening it does not grow the row').toBeLessThanOrEqual(57);
    await shot(page, 'chief-phone-failover.png');
    await page.locator('#agent-actions-open').tap();
    await expect(detail).toBeHidden();
  });

  test('seven questions waiting draw no block: the header counts them and the composer stays on screen', async ({ page }) => {
    await open(page);
    await noSideScroll(page);
    await noAsksBlock(page);
    await expect(badge(page)).toHaveText(String(ASKS));
    await composerOnScreen(page, MIN_LOG);
  });

  test('the Asks queue draws the same compact answer form', async ({ page }) => {
    await signIn(page, origin);
    await theQueueDrawsTheCompactForm(page, 'attention-phone.png');
  });

  // The answer box is the Asks card's now (the session page has none).
  test('the answer box grows as it is typed into, up to its cap', async ({ page }) => {
    await signIn(page, origin);
    await page.goto(origin + '/attention');
    const box = page.locator('[data-attention-item]').filter({ hasText: 'Question 1:' }).locator('textarea[name="answer"]');
    await box.scrollIntoViewIfNeeded();
    const h0 = (await box.boundingBox())!.height;
    await box.fill('one\ntwo\nthree');
    await expect.poll(async () => (await box.boundingBox())!.height, 'three lines are taller than one').toBeGreaterThan(h0 + 20);
    await box.fill(Array.from({ length: 30 }, (_, i) => `line ${i}`).join('\n'));
    const capped = (await box.boundingBox())!.height;
    expect(capped, 'capped at 8rem').toBeLessThanOrEqual(128 + 1);
    await box.fill('');
    await expect.poll(async () => (await box.boundingBox())!.height, 'and back to one line').toBeLessThanOrEqual(h0 + 1);
  });
});

test.describe('on a desktop', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false, storageState: { cookies: [], origins: [] } });

  test('the same page holds its layout at 1280px', async ({ page }) => {
    await open(page);
    await shot(page, 'chief-desktop.png');
    await noSideScroll(page);
    await headerIsOneLine(page, 'click');
    await openAgentMenu(page, 'click');
    await expect(agentMenu(page).locator('[data-failover]')).toBeVisible();
    await expect(page.locator('[data-failover-detail]')).toContainText(FAILOVER);
    await page.locator('#agent-actions-open').click();
    await expect(page.locator('[data-failover-detail]')).toBeHidden();
    await noAsksBlock(page);
    await expect(badge(page)).toHaveText(String(ASKS));
    await composerOnScreen(page, MIN_LOG);
    await theQueueDrawsTheCompactForm(page, 'attention-desktop.png');
  });
});

// wordsAreWhole: neither button of a card's answer row clips its own words.
async function wordsAreWhole(card: ReturnType<Page['locator']>) {
  for (const name of ['Answer', 'Dismiss without answering']) {
    const [sw, cw] = await card.getByRole('button', { name, exact: true }).evaluate((el) => [el.scrollWidth, el.clientWidth]);
    expect.soft(sw, `"${name}" is drawn whole`).toBeLessThanOrEqual(cw);
  }
}

// theQueueDrawsTheCompactForm: the Asks queue (/attention) draws its card
// feed's answer form (internal/api askFeedAnswerFormHTML): the options, then a
// one-line box beside its mic and Answer, Dismiss the small icon in the card's
// footer, nothing wider than the page.
async function theQueueDrawsTheCompactForm(page: Page, name: string) {
  await page.goto(origin + '/attention');
  const card = page.locator('[data-attention-item]').filter({ hasText: 'Question 1:' });
  await expect(card).toHaveCount(1);
  await card.scrollIntoViewIfNeeded();
  await expect(card.locator('textarea[name="answer"]'), 'the box is on the card').toBeVisible();
  await shot(page, name);
  const ans = (await card.getByRole('button', { name: 'Answer', exact: true }).boundingBox())!;
  const dis = (await card.getByRole('button', { name: 'Dismiss without answering' }).boundingBox())!;
  expect.soft(dis.y, 'Dismiss is in the footer, not the head').toBeGreaterThan((await card.locator('[data-attention-text]').boundingBox())!.y);
  expect.soft(ans.width, 'Answer is drawn').toBeGreaterThan(0);
  await wordsAreWhole(card);
  const box = (await card.locator('textarea[name="answer"]').boundingBox())!;
  expect.soft(box.height, 'the answer box starts one line').toBeLessThanOrEqual(46);
  await noSideScroll(page);
}

// --- THE CHIEF'S PAGE (operator ask, chief-header-chats-sheet) ------------------
//
//   header   the same single list icon a mirrored session has (no ＋, no clock,
//            no target), and the chat's context chip under the title.
//   chats    that icon's sheet has three tabs — Chats | Your messages | Focus
//            history — and Chats is where the primary and side chats are
//            switched, made and archived; no chat bar over the transcript.
//   layout   with questions waiting, the page draws NO asks block (operator
//            decision: it took too much of the screen and pushed the composer
//            under the bottom bar): the composer keeps the place and the
//            height it has with none waiting, above the bottom bar (the
//            session switcher) and inside the viewport, and the header counts
//            the questions. Measured at 0, 1 and 3 questions and on a live
//            arrival, at four sizes — a phone, the same phone's visible area
//            under its browser's toolbars, the phone held sideways, and a
//            desktop.

// The chief's asks, by id, so a case can set how many wait.
const chiefAsks: string[] = [];
// (Not "Question N:", which the Asks-queue case finds its card by.)
const chiefQuestion = (i: number) => `Chief ask #${i + 1} — ` + question(i).slice(question(i).indexOf(':') + 2);
function setChiefAsks(n: number) {
  while (chiefAsks.length < n) chiefAsks.push(go('./e2e/ccfix', 'ask', OWNER, CHIEF, chiefQuestion(chiefAsks.length), ...OPTIONS));
  while (chiefAsks.length > n) go('./e2e/pluginfix', 'rmask', OWNER, chiefAsks.pop()!);
}

async function openChiefPage(page: Page) {
  await signIn(page, origin);
  await page.goto(origin + '/agents/' + CHIEF);
  await expect(page.locator('#chat-form')).toBeVisible();
  await expect(page.locator('#session-switcher')).toBeVisible();
}

// composerClearsTheBar: the composer is wholly on screen and wholly above the
// session switcher (bounding boxes, and what a tap at the box's centre hits),
// and the conversation keeps its floor.
async function composerClearsTheBar(page: Page, label: string) {
  const vp = page.viewportSize()!;
  const form = (await page.locator('#chat-form').boundingBox())!;
  const bar = (await page.locator('#session-switcher').boundingBox())!;
  const log = (await page.locator('#chat-log').boundingBox())!;
  expect.soft(form.y + form.height, `${label}: the composer's bottom is at or above the bottom bar's top`).toBeLessThanOrEqual(bar.y + 0.5);
  expect.soft(form.y >= 0 && form.y + form.height <= vp.height + 0.5, `${label}: the composer is wholly inside the viewport`).toBe(true);
  expect.soft(bar.y + bar.height, `${label}: the bottom bar is inside the viewport`).toBeLessThanOrEqual(vp.height + 0.5);
  expect.soft(log.height, `${label}: the conversation keeps its floor`).toBeGreaterThanOrEqual(MIN_LOG - 0.5);
  const hit = await page.locator('#chat-input').evaluate((el) => {
    const r = el.getBoundingClientRect();
    const at = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    return !!at && (at === el || el.contains(at));
  });
  expect.soft(hit, `${label}: a tap on the message box lands on it (the bar does not cover it)`).toBe(true);
}

// layoutOf is where the composer and the conversation are, and how tall.
async function layoutOf(page: Page) {
  const form = (await page.locator('#chat-form').boundingBox())!;
  const log = (await page.locator('#chat-log').boundingBox())!;
  return { formY: form.y, formH: form.height, logY: log.y, logH: log.height };
}

// sameLayout: the composer and the conversation are where, and as tall as,
// they were with no question waiting (to a pixel).
function sameLayout(label: string, got: Awaited<ReturnType<typeof layoutOf>>, want: Awaited<ReturnType<typeof layoutOf>>) {
  for (const k of ['formY', 'formH', 'logY', 'logH'] as const) {
    expect.soft(Math.abs(got[k] - want[k]), `${label}: ${k} is ${got[k]}, ${want[k]} with none waiting`).toBeLessThanOrEqual(1);
  }
}

for (const vp of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  // A phone's VISIBLE area under its browser's top and bottom toolbars.
  { name: 'phone under its toolbars', use: { viewport: { width: 390, height: 664 }, hasTouch: true, isMobile: true } },
  { name: 'phone held sideways', use: { viewport: { width: 844, height: 390 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`the chief's page on a ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    // 🔴 RED at origin/main: the asks block sat above the composer, taking the
    // conversation's height, and the header had no linked count.
    test('with 1 and with 3 questions waiting, no asks block: the composer keeps its place and its full height', async ({ page }) => {
      test.setTimeout(120_000);
      setChiefAsks(0);
      await openChiefPage(page);
      await expect(badge(page), 'none waiting: no count').toHaveCount(0);
      await composerClearsTheBar(page, 'none waiting');
      const none = await layoutOf(page);
      for (const n of [1, 3]) {
        setChiefAsks(n);
        await openChiefPage(page);
        await noAsksBlock(page);
        await composerClearsTheBar(page, `${n} waiting`);
        sameLayout(`${n} waiting`, await layoutOf(page), none);
        await noSideScroll(page);
        // The header counts them instead.
        await expect(badge(page)).toHaveAttribute('data-count', String(n));
        await shot(page, `chief-asks-${n}-${vp.name.replace(/ /g, '-')}.png`);
      }

      // A question arriving while the page is open: the header's count
      // follows (its live refresh), and nothing in the column moves.
      setChiefAsks(4);
      await page.evaluate(() => (window as any).htmx.trigger(document.querySelector('[data-asker-asks-refresh]'), 'sse:attention_queue.changed'));
      await expect(badge(page)).toHaveAttribute('data-count', '4');
      await noAsksBlock(page);
      sameLayout('4 waiting, live', await layoutOf(page), none);
      setChiefAsks(0);
    });
  });
}

for (const vp of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`the chief's chats on a ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    // The chats are a strip of chips under the header, not a sheet (operator:
    // "a simpler ui/ux to manage sidechats, no modal"). The header keeps its
    // one row and its one list icon, whose sheet is Your messages and Focus
    // history only. (e2e chief-ux drives the strip's +, chips and ⋯ menus.)
    test('the strip is one row under the one-row header, every target 44px, and the sheet holds no chats', async ({ page }) => {
      test.setTimeout(120_000);
      setChiefAsks(0);
      await signIn(page, origin);
      await page.goto(origin + '/agents/' + CHIEF);
      await expect(page.locator('#chat-form')).toBeVisible();
      const strip = page.locator('[data-chief-chat-strip]');
      await expect(strip).toHaveCount(1);
      await expect(strip).toHaveAttribute('data-chief-chat-kind', 'primary');

      // The header: the one list icon, no ＋ / clock / target; the context
      // row under the title; no chat bar over the transcript.
      await expect(page.locator('header #chat-index-open')).toBeVisible();
      await expect(page.locator('header').getByRole('button', { name: 'New chat session' })).toHaveCount(0);
      await expect(page.locator('header label[aria-label="Chat history"], header #focus-history-open')).toHaveCount(0);
      await expect(page.locator('header [data-chief-context-row] #chief-context')).toHaveCount(1);
      await expect(page.locator('[data-chief-chat-bar]')).toHaveCount(0);
      expect.soft((await page.locator('header').boundingBox())!.height, 'the header is the one row (+1px border)').toBeLessThanOrEqual(57);

      // The strip: under the header, one 52px row, the ★ Primary chip current
      // with its ⋯, then "+"; every target 44px.
      const hb = (await page.locator('header').boundingBox())!;
      const sb = (await strip.boundingBox())!;
      expect.soft(sb.y, 'the strip starts under the header').toBeGreaterThanOrEqual(hb.y + hb.height - 0.5);
      expect.soft(sb.height, 'the strip is one row').toBeLessThanOrEqual(52.5);
      await expect(strip.locator('[aria-current="true"]')).toContainText('★Primary');
      const targets = await strip
        .locator('[data-chief-chip]:not([hidden]) > *:not([popover]), button[data-chief-side-chat]')
        .evaluateAll((els) => els.map((e) => { const r = e.getBoundingClientRect(); return { top: r.top, h: r.height, w: r.width }; }));
      expect(targets.length, 'the primary\'s link, its ⋯, and +').toBe(3);
      for (const t of targets) {
        expect.soft(t.h, 'a 44px target').toBeGreaterThanOrEqual(44);
        expect.soft(t.w, 'a 44px target').toBeGreaterThanOrEqual(44);
        expect.soft(Math.abs(t.top - targets[0].top), 'on one row').toBeLessThanOrEqual(1);
      }
      await noSideScroll(page);
      await shot(page, `chief-chips-${vp.name}.png`);

      // The sheet: two tabs, in order, and nothing chat-related.
      await page.locator('header #chat-index-open').click();
      const sheet = page.locator('#chat-index');
      await expect(sheet).toBeVisible();
      await expect(sheet.locator('[role="tab"]')).toHaveText(['Your messages', 'What it worked on']);
      await expect(sheet.locator('[data-chief-chats], [data-chief-side-chat], [data-chief-chat-row]')).toHaveCount(0);
      await sheet.locator('[role="tab"][data-tab="focus"]').click();
      await expect(sheet.locator('#chat-index-tabs-panel-focus [data-focus-history-list]')).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(sheet).toBeHidden();
    });
  });
}
