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
//   asks    the heading wrapped beside a bordered "Hide until next ask"
//           button, a long question ran past the block's scroll box so it read
//           as cut off mid-sentence, and the block left the conversation a
//           sliver. Now: a one-line heading and a text button, each question's
//           body clamped with its "Show more" INSIDE its own card, and past the
//           first question the rest behind one "Show N more" — so the composer
//           and some of the conversation stay on screen.
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

const question = (i: number) =>
  `Question ${i + 1}: the nightly fleet check flagged node gpu-${i}-a as not ready and marked it as a control-plane node, ` +
  `with a note to confirm before any shutdown. It was first seen four minutes before the check and has rebooted once this week; ` +
  `its zone is otherwise healthy and its metrics came back after the earlier telemetry gap. Two sessions are working in that area ` +
  `right now (one rolling out node updates, one draining a pool). Should I cordon it and page the owner, or leave it for the rollout?`;
// Every question offers two long options, as a real chief's does (a live card
// with these is what clipped at 0.148.0: the fixture had none).
// The conversation's floor while the asks block shows one REAL card: #chat-log's
// own min-height (7.5rem, internal/ui agentChatPane). The block is capped at
// 40dvh on an agent's page and scrolls inside (operator decision,
// chief-header-chats-sheet, superseding fix/asks-block-real-card's "the block
// grows to fit its one card"), so the conversation keeps more than this.
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

async function open(page: Page) {
  await signIn(page, origin);
  await page.goto(origin + '/agents/' + NAME);
  await expect(page.locator('header #agent-actions-open')).toBeVisible();
  await expect(page.locator('#asker-asks [data-attention-item]')).toHaveCount(ASKS);
  // The page reveals its transcript and composer once laid out.
  await expect(page.locator('#chat-form')).toBeVisible();
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

// asksLeaveTheComposer: the asks block ends above the composer, the composer
// is on screen, and the conversation keeps real room.
async function asksLeaveTheComposer(page: Page, minLog: number) {
  const vh = page.viewportSize()!.height;
  const compose = (await page.locator('#chat-form').boundingBox())!;
  const asks = (await page.locator('#asker-asks').boundingBox())!;
  const log = (await page.locator('#chat-log').boundingBox())!;
  expect.soft(compose.y + compose.height, 'the composer is on screen').toBeLessThanOrEqual(vh + 1);
  expect.soft(asks.y + asks.height, 'the asks block ends above the composer').toBeLessThanOrEqual(compose.y + 1);
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

  test('the asks are one card at a time, clamped in place, and leave the composer on screen', async ({ page }) => {
    await open(page);
    await noSideScroll(page);

    // The asks: a one-line heading with its hide beside it.
    const block = page.locator('#asker-asks');
    const heading = block.locator('[data-asker-asks-heading]');
    await expect.soft(heading).toHaveText(`${ASKS} questions waiting`);
    const hb = (await heading.boundingBox())!;
    const hide = (await block.getByRole('button', { name: 'Hide until next ask' }).boundingBox())!;
    expect.soft(hb.height, 'the heading is one line').toBeLessThanOrEqual(24);
    expect.soft(Math.abs(hb.y + hb.height / 2 - (hide.y + hide.height / 2)), 'the hide sits on the heading\'s line').toBeLessThanOrEqual(4);
    await asksLeaveTheComposer(page, MIN_LOG);

    // 🔴 THE BLOCK IS CAPPED AT 40dvh ON AN AGENT'S PAGE AND SCROLLS INSIDE
    // (operator decision, chief-header-chats-sheet: with questions waiting the
    // composer slid under the bottom bar). This replaced "one card fits the
    // block whole" — the one-card raise to 65dvh is not applied on the page,
    // so the real card scrolls inside the block, and Answer and Dismiss are
    // reached by scrolling the block, never cut off from it.
    const vh0 = page.viewportSize()!.height;
    const bb = (await block.boundingBox())!;
    expect.soft(bb.height, 'the asks block is within its 40dvh cap').toBeLessThanOrEqual(vh0 * 0.4 + 1);
    // Answer and Dismiss share one row (the card's footer).
    const first0 = block.locator('[data-attention-item]').first();
    await first0.getByRole('button', { name: 'Answer', exact: true }).scrollIntoViewIfNeeded();
    const card = (await first0.boundingBox())!;
    const ans = (await first0.getByRole('button', { name: 'Answer', exact: true }).boundingBox())!;
    const dis = (await first0.getByRole('button', { name: 'Dismiss without answering' }).boundingBox())!;
    expect.soft(Math.abs(ans.y - dis.y), 'Answer and Dismiss are on one row').toBeLessThanOrEqual(2);
    await wordsAreWhole(first0);
    // THE REAL CARD (what clipped live): two long options and the answer box's
    // mic are in it — the positive control that this card is production's size.
    await expect(first0.locator('[data-attention-option]')).toHaveCount(OPTIONS.length);
    await expect(first0.locator('[id^="answer-"][id$="-dictate"]')).toBeVisible();
    // Scrolled to, Answer and Dismiss are wholly on screen, inside the block's
    // box, and nothing covers them (the block's own scroll edge, the
    // conversation, the composer).
    const vp = page.viewportSize()!;
    for (const [name, b] of [['Answer', ans], ['Dismiss', dis]] as const) {
      expect.soft(b.y >= 0 && b.y + b.height <= vp.height && b.x >= 0 && b.x + b.width <= vp.width, `${name} is wholly inside the viewport`).toBe(true);
      expect.soft(b.y >= bb.y - 0.5 && b.y + b.height <= bb.y + bb.height + 0.5, `${name} is inside the block's box`).toBe(true);
    }
    const hits = await first0.evaluate((li) => {
      const out: Record<string, boolean> = {};
      for (const b of Array.from(li.querySelectorAll<HTMLElement>('button'))) {
        const name = b.getAttribute('aria-label') || (b.textContent || '').trim();
        if (name !== 'Answer' && name !== 'Dismiss without answering') continue;
        const r = b.getBoundingClientRect();
        const at = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
        out[name] = !!at && (at === b || b.contains(at));
      }
      return out;
    });
    expect.soft(hits, 'a tap on each lands on it (nothing covers them)').toEqual({ Answer: true, 'Dismiss without answering': true });
    expect.soft(card.y + card.height, 'the card\'s bottom border is on screen').toBeLessThanOrEqual(vp.height);

    // The first question: its body clamped, "Show more" INSIDE its card, and
    // a tap opens it in place.
    const items = block.locator('[data-attention-item]');
    const first = items.first();
    await first.locator('[data-attention-text]').scrollIntoViewIfNeeded();
    const text = first.locator('[data-attention-text]');
    const more = first.getByRole('button', { name: 'Show more' });
    await expect(more).toBeVisible();
    const [sh, ch] = await text.evaluate((el) => [el.scrollHeight, el.clientHeight]);
    expect.soft(sh, 'the body is clamped').toBeGreaterThan(ch + 1);
    const cb = (await first.boundingBox())!;
    const mb = (await more.boundingBox())!;
    expect.soft(mb.y >= cb.y && mb.y + mb.height <= cb.y + cb.height + 0.5, '"Show more" is inside its card').toBe(true);
    await more.tap();
    await expect(first.getByRole('button', { name: 'Show less' })).toBeVisible();
    const [sh2, ch2] = await text.evaluate((el) => [el.scrollHeight, el.clientHeight]);
    expect.soft(sh2, 'the body is open').toBeLessThanOrEqual(ch2 + 1);
    await first.getByRole('button', { name: 'Show less' }).tap();
    await expect(more).toBeVisible();

    // Past the first, the rest wait behind one "Show N more", which opens them.
    await expect.soft(items.nth(1)).toBeHidden();
    const rest = block.locator('[data-asks-more]');
    await expect(rest).toHaveText(`Show ${ASKS - 1} more`);
    await rest.tap();
    for (let i = 0; i < ASKS; i++) await expect(items.nth(i)).toBeVisible();
    await asksLeaveTheComposer(page, MIN_LOG);
    await noSideScroll(page);
  });

  test('the Asks queue draws the same compact answer form', async ({ page }) => {
    await signIn(page, origin);
    await theQueueDrawsTheCompactForm(page, 'attention-phone.png');
  });

  test('the answer box grows as it is typed into, up to its cap', async ({ page }) => {
    await open(page);
    const box = page.locator('#asker-asks [data-attention-item]').first().locator('textarea[name="answer"]');
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
    await expect(page.locator('#asker-asks [data-asker-asks-heading]')).toHaveText(`${ASKS} questions waiting`);
    await asksLeaveTheComposer(page, MIN_LOG);
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

// theQueueDrawsTheCompactForm: the Asks queue (/attention) draws the same
// answer form as the session page's block (internal/api attentionItemsHTMLTo)
// — except that its box sits behind its options ("Type an answer", the Asks
// page's design pass): opened, a one-line box and Answer, Dismiss the small
// icon in the card's footer, nothing wider than the page.
async function theQueueDrawsTheCompactForm(page: Page, name: string) {
  await page.goto(origin + '/attention');
  const card = page.locator('[data-attention-item]').filter({ hasText: 'Question 1:' });
  await expect(card).toHaveCount(1);
  await card.scrollIntoViewIfNeeded();
  await expect(card.locator('textarea[name="answer"]'), 'the box is behind the options').toBeHidden();
  await card.getByText('Type an answer', { exact: true }).click();
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
//   layout   with questions waiting, the composer stays above the bottom bar
//            (the session switcher) and inside the viewport: it used to slide
//            under the bar. Measured at 1 and 3 questions, at four sizes — a
//            phone, the same phone's visible area under its browser's toolbars,
//            the phone held sideways, and a desktop.

// The chief's asks, by id, so a case can set how many wait.
const chiefAsks: string[] = [];
// (Not "Question N:", which the Asks-queue case finds its card by.)
const chiefQuestion = (i: number) => `Chief ask #${i + 1} — ` + question(i).slice(question(i).indexOf(':') + 2);
function setChiefAsks(n: number) {
  while (chiefAsks.length < n) chiefAsks.push(go('./e2e/ccfix', 'ask', OWNER, CHIEF, chiefQuestion(chiefAsks.length), ...OPTIONS));
  while (chiefAsks.length > n) go('./e2e/pluginfix', 'rmask', OWNER, chiefAsks.pop()!);
}

async function openChiefPage(page: Page, asks: number) {
  await signIn(page, origin);
  await page.goto(origin + '/agents/' + CHIEF);
  await expect(page.locator('#chat-form')).toBeVisible();
  await expect(page.locator('#asker-asks [data-attention-item]')).toHaveCount(asks);
  await expect(page.locator('#session-switcher')).toBeVisible();
}

// composerClearsTheBar: the composer is wholly on screen and wholly above the
// session switcher (bounding boxes, and what a tap at the box's centre hits),
// the asks block is inside its 40dvh cap, and the conversation keeps its floor.
async function composerClearsTheBar(page: Page, label: string) {
  const vp = page.viewportSize()!;
  const form = (await page.locator('#chat-form').boundingBox())!;
  const bar = (await page.locator('#session-switcher').boundingBox())!;
  const asks = (await page.locator('#asker-asks').boundingBox())!;
  const log = (await page.locator('#chat-log').boundingBox())!;
  expect.soft(form.y + form.height, `${label}: the composer's bottom is at or above the bottom bar's top`).toBeLessThanOrEqual(bar.y + 0.5);
  expect.soft(form.y >= 0 && form.y + form.height <= vp.height + 0.5, `${label}: the composer is wholly inside the viewport`).toBe(true);
  expect.soft(bar.y + bar.height, `${label}: the bottom bar is inside the viewport`).toBeLessThanOrEqual(vp.height + 0.5);
  expect.soft(asks.y + asks.height, `${label}: the asks block ends above the composer`).toBeLessThanOrEqual(form.y + 0.5);
  expect.soft(asks.height, `${label}: the asks block is within its 40dvh cap`).toBeLessThanOrEqual(vp.height * 0.4 + 1);
  expect.soft(log.height, `${label}: the conversation keeps its floor`).toBeGreaterThanOrEqual(MIN_LOG - 0.5);
  const hit = await page.locator('#chat-input').evaluate((el) => {
    const r = el.getBoundingClientRect();
    const at = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    return !!at && (at === el || el.contains(at));
  });
  expect.soft(hit, `${label}: a tap on the message box lands on it (the bar does not cover it)`).toBe(true);
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

    test('with 1 and with 3 questions waiting, the composer stays above the bottom bar', async ({ page }) => {
      test.setTimeout(120_000);
      setChiefAsks(1);
      await openChiefPage(page, 1);
      await expect(page.locator('#asker-asks [data-asks-fold]'), 'one question: nothing to fold').toHaveCount(0);
      await composerClearsTheBar(page, '1 question');
      await shot(page, `chief-asks-1-${vp.name.replace(/ /g, '-')}.png`);

      setChiefAsks(3);
      await openChiefPage(page, 3);
      await composerClearsTheBar(page, '3 questions');
      await shot(page, `chief-asks-3-${vp.name.replace(/ /g, '-')}.png`);
      // More than one: the "3 questions waiting" header folds the block to its
      // own row, and opens it again.
      const fold = page.locator('#asker-asks [data-asks-fold]');
      await expect(fold).toHaveText('3 questions waiting');
      await expect(fold).toHaveAttribute('aria-expanded', 'true');
      await fold.click();
      await expect(fold).toHaveAttribute('aria-expanded', 'false');
      await expect(page.locator('#asker-asks')).toHaveAttribute('data-asks-folded', '');
      await expect(page.locator('#asker-asks [data-attention-item]').first()).toBeHidden();
      await composerClearsTheBar(page, '3 questions, folded');
      await fold.click();
      await expect(fold).toHaveAttribute('aria-expanded', 'true');
      await expect(page.locator('#asker-asks [data-attention-item]').first()).toBeVisible();
      await noSideScroll(page);
    });
  });
}

for (const vp of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`the chief's chats on a ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('one list icon opens Chats | Your messages | Focus history: switch, create and archive a chat', async ({ page }) => {
      test.setTimeout(120_000);
      setChiefAsks(0);
      await signIn(page, origin);
      await page.goto(origin + '/agents/' + CHIEF);
      await expect(page.locator('#chat-form')).toBeVisible();
      const sheet = page.locator('#chat-index');
      const chats = sheet.locator('[data-chief-chats]');
      const open = async () => {
        await page.locator('header #chat-index-open').click();
        await expect(sheet).toBeVisible();
        await sheet.locator('[role="tab"][data-tab="chats"]').click();
        await expect(chats).toBeVisible();
      };
      const sessionOf = (u: string) => new URL(u).searchParams.get('session');

      // The header: the one list icon, no ＋ / clock / target; the context
      // row under the title; no chat bar over the transcript.
      await expect(page.locator('header #chat-index-open')).toBeVisible();
      await expect(page.locator('header').getByRole('button', { name: 'New chat session' })).toHaveCount(0);
      await expect(page.locator('header label[aria-label="Chat history"], header #focus-history-open')).toHaveCount(0);
      await expect(page.locator('header [data-chief-context-row] #chief-context')).toHaveCount(1);
      await expect(page.locator('[data-chief-chat-bar]')).toHaveCount(0);
      expect.soft((await page.locator('header').boundingBox())!.height, 'the header is the one row (+1px border)').toBeLessThanOrEqual(57);

      // The sheet: three tabs, in order.
      await open();
      await expect(sheet.locator('[role="tab"]')).toHaveText(['Chats', 'Your messages', 'Focus history']);
      await expect(chats).toHaveAttribute('data-chief-chat-kind', 'primary');
      await expect(chats.getByRole('button', { name: 'Fresh primary' }), 'Fresh primary on the primary').toBeVisible();
      // Fresh primary keeps its confirm: dismissed, nothing happens.
      const here0 = page.url();
      page.once('dialog', (d) => d.dismiss());
      await chats.getByRole('button', { name: 'Fresh primary' }).click();
      await page.waitForTimeout(300);
      expect(page.url(), 'a dismissed confirm leaves the page where it was').toBe(here0);
      await noSideScroll(page);

      // Create a side chat.
      await chats.getByRole('button', { name: 'New side chat', exact: true }).click();
      await page.waitForURL((u) => u.toString() !== here0);
      const side = sessionOf(page.url())!;
      await expect(chats).toHaveAttribute('data-chief-chat-kind', 'side');
      await open();
      await expect(chats.getByRole('button', { name: 'Fresh primary' }), 'no Fresh primary off the primary').toHaveCount(0);
      const sideRow = chats.locator(`[data-chief-chat-row="${side}"]`);
      await expect(sideRow).toHaveAttribute('aria-current', 'page');
      await expect(sideRow.locator('[data-chief-chat-current]')).toHaveText('Current');

      // Switch to the primary: its row, first, with its badge.
      const primaryRow = chats.locator('[data-chief-chat-row]').first();
      await expect(primaryRow).toContainText('Primary');
      await primaryRow.click();
      await page.waitForURL((u) => sessionOf(u.toString()) !== side);
      await expect(chats).toHaveAttribute('data-chief-chat-kind', 'primary');

      // Archive the side chat from its row's ⋯ menu: it moves into the
      // collapsed Archived group, and the page is on the primary.
      await open();
      const archivedBefore = await chats.locator('[data-chief-archived] [data-chief-chat-row]').count();
      await chats.locator(`button[popovertarget="chief-chat-menu-${side}"]`).click();
      const menu = page.locator(`#chief-chat-menu-${side}`);
      await expect(menu).toBeVisible();
      // The answer is HX-Redirect to the primary — this page — so htmx loads it
      // again.
      const reloaded = page.waitForEvent('load');
      await menu.getByRole('menuitem', { name: 'Archive' }).click();
      await reloaded;
      await expect(page.locator(`#chat-index [data-chief-archived] [data-chief-chat-row="${side}"]`)).toHaveCount(1);
      await expect(chats).toHaveAttribute('data-chief-chat-kind', 'primary');
      await open();
      const group = chats.locator('[data-chief-archived]');
      await expect(group).not.toHaveAttribute('open', '');
      await expect(chats.locator(`[data-chief-chat-row="${side}"]`)).toBeHidden();
      await group.locator('summary').click();
      await expect(chats.locator(`[data-chief-chat-row="${side}"]`)).toBeVisible();
      expect(await chats.locator('[data-chief-archived] [data-chief-chat-row]').count()).toBe(archivedBefore + 1);

      // The other two tabs are the mirrored session's.
      await sheet.locator('[role="tab"][data-tab="messages"]').click();
      await expect(sheet.locator('#chat-index-tabs-panel-messages')).toBeVisible();
      await sheet.locator('[role="tab"][data-tab="focus"]').click();
      await expect(sheet.locator('#chat-index-tabs-panel-focus [data-focus-history-list]')).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(sheet).toBeHidden();
    });
  });
}
