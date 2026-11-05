import { Page } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { chiefMobilePort, dsn, repoRoot, sttToken, versionA } from '../lib/env';
import { Running, startServer } from '../lib/server';
import { signIn } from '../lib/signin';

// A CLAUDE-CODE AGENT'S PAGE ON A PHONE (an operator report: "the layout is
// unintuitive and looks broken on mobile"). The agent the chief usually is: a
// claude-code agent whose account failed over, with several long questions
// waiting on the operator. What was broken, and what each case pins:
//
//   header  the second row held a "kind Claude Code · <account>" chip and a
//           "failed over from … · rate_limited · resets …" chip, each wrapping
//           into a tall blob at 412px, so the row grew to ~5 lines and pushed
//           the view switch to the edge. It is ONE line now: the account name
//           as text and a "↻ switched" marker whose detail opens on a TAP (a
//           popover; a title= tooltip never shows on a touch screen).
//   asks    the heading wrapped beside a bordered "Hide until next ask"
//           button, a long question ran past the block's scroll box so it read
//           as cut off mid-sentence, and the block left the conversation a
//           sliver. Now: a one-line heading and a text button, each question's
//           body clamped with its "Show more" INSIDE its own card, and past the
//           first question the rest behind one "Show N more" — so the composer
//           and some of the conversation stay on screen.
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
// The conversation's floor while the asks block shows one REAL card. Operator
// decision (fix/asks-block-real-card): the block grows to fit its one card and
// the conversation's scroller shrinks — never the card cut by an inner scroll
// edge. It was 200 for the fixture's smaller card; the real one leaves 158 at
// 412×915 and 150 at 1280×800 — a strip of conversation, not none.
const MIN_LOG = 120;
const OPTIONS = ['agent_dispatch on pod/gateway: start a new agent on it', 'agent_dispatch on pod/claude-code: start a new agent on it'];

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
});

test.afterAll(async () => {
  try {
    go('./e2e/ccfix', 'rmagent', OWNER, NAME);
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

// headerRow is the header's second row: the one the view switch sits in.
const headerRow = (page: Page) => page.locator('header > div > div').filter({ has: page.locator('#view-terminal') });

async function open(page: Page) {
  await signIn(page, origin);
  await page.goto(origin + '/agents/' + NAME);
  await expect(page.locator('#view-terminal')).toBeVisible();
  await expect(page.locator('#asker-asks [data-attention-item]')).toHaveCount(ASKS);
  // The page reveals its transcript and composer once laid out.
  await expect(page.locator('#chat-form')).toBeVisible();
}

// noSideScroll: nothing on the page is wider than the viewport.
async function noSideScroll(page: Page) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect.soft(sw, 'the page does not scroll sideways').toBeLessThanOrEqual(cw);
}

async function headerIsOneLine(page: Page) {
  const row = headerRow(page);
  const box = (await row.boundingBox())!;
  expect.soft(box.height, 'the header\'s second row is one line (the ⋯\'s 44px)').toBeLessThanOrEqual(48);
  const [sw, cw] = await row.evaluate((el) => [el.scrollWidth, el.clientWidth]);
  expect.soft(sw, 'nothing in the header\'s second row overflows it').toBeLessThanOrEqual(cw);
  // The account is named as text (no "kind Claude Code" label: the Terminal
  // switch already says what kind of agent this is).
  await expect.soft(page.locator('header [data-cc-account]')).toHaveText('beta');
  await expect.soft(page.locator('header'), 'no kind label').not.toContainText('Claude Code');
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
    await headerIsOneLine(page);

    // The failover: a one-line marker in the row, its detail one TAP away.
    const marker = page.locator('header [data-failover]');
    await expect(marker).toBeVisible();
    await expect(marker).toHaveText('↻switched');
    const detail = page.locator('[data-failover-detail]');
    await expect(detail).toBeHidden();
    await marker.tap();
    await expect(detail).toBeVisible();
    await expect(detail).toContainText(FAILOVER);
    const d = (await detail.boundingBox())!;
    expect.soft(d.x, 'the detail is inside the viewport (left)').toBeGreaterThanOrEqual(0);
    expect.soft(d.x + d.width, 'the detail is inside the viewport (right)').toBeLessThanOrEqual(412);
    expect.soft((await headerRow(page).boundingBox())!.height, 'opening it does not grow the row').toBeLessThanOrEqual(48);
    await shot(page, 'chief-phone-failover.png');
    await marker.tap();
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

    // One card fits the block WHOLE: no scrolling inside the block, and the
    // card's border and the "Show N more" under it are inside the block's box
    // (the compact answer form: a one-line box, Answer and Dismiss on one row).
    const [bsh, bch] = await block.evaluate((el) => [el.scrollHeight, el.clientHeight]);
    expect.soft(bsh, 'the asks block does not scroll inside').toBeLessThanOrEqual(bch + 1);
    const bb = (await block.boundingBox())!;
    const card = (await block.locator('[data-attention-item]').first().boundingBox())!;
    expect.soft(card.y + card.height, 'the card\'s bottom border is inside the block').toBeLessThanOrEqual(bb.y + bb.height + 0.5);
    const moreBox = (await block.locator('[data-asks-more]').boundingBox())!;
    expect.soft(moreBox.y + moreBox.height, '"Show N more" is inside the block').toBeLessThanOrEqual(bb.y + bb.height + 0.5);
    // Answer and Dismiss share one row.
    const first0 = block.locator('[data-attention-item]').first();
    const ans = (await first0.getByRole('button', { name: 'Answer', exact: true }).boundingBox())!;
    const dis = (await first0.getByRole('button', { name: 'Dismiss without answering' }).boundingBox())!;
    expect.soft(Math.abs(ans.y - dis.y), 'Answer and Dismiss are on one row').toBeLessThanOrEqual(2);
    await wordsAreWhole(first0);
    // THE REAL CARD (what clipped live): two long options and the answer box's
    // mic are in it — the positive control that this card is production's size.
    await expect(first0.locator('[data-attention-option]')).toHaveCount(OPTIONS.length);
    await expect(first0.locator('[id^="answer-"][id$="-dictate"]')).toBeVisible();
    // Answer and Dismiss are wholly on screen and nothing covers them (the
    // block's own scroll edge, the conversation, the composer), and the card's
    // bottom border is on screen too.
    const vp = page.viewportSize()!;
    for (const [name, b] of [['Answer', ans], ['Dismiss', dis]] as const) {
      expect.soft(b.y >= 0 && b.y + b.height <= vp.height && b.x >= 0 && b.x + b.width <= vp.width, `${name} is wholly inside the viewport`).toBe(true);
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
    await headerIsOneLine(page);
    const marker = page.locator('header [data-failover]');
    await marker.click();
    await expect(page.locator('[data-failover-detail]')).toContainText(FAILOVER);
    await marker.click();
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

// theQueueDrawsTheCompactForm: the Asks queue (/attention) draws the SAME
// answer form as the session page's block (internal/api attentionItemsHTMLTo):
// a one-line box, Answer and Dismiss on one row, nothing wider than the page.
async function theQueueDrawsTheCompactForm(page: Page, name: string) {
  await page.goto(origin + '/attention');
  const card = page.locator('[data-attention-item]').filter({ hasText: 'Question 1:' });
  await expect(card).toHaveCount(1);
  await card.scrollIntoViewIfNeeded();
  await shot(page, name);
  const ans = (await card.getByRole('button', { name: 'Answer', exact: true }).boundingBox())!;
  const dis = (await card.getByRole('button', { name: 'Dismiss without answering' }).boundingBox())!;
  expect.soft(Math.abs(ans.y - dis.y), 'Answer and Dismiss are on one row').toBeLessThanOrEqual(2);
  await wordsAreWhole(card);
  const box = (await card.locator('textarea[name="answer"]').boundingBox())!;
  expect.soft(box.height, 'the answer box starts one line').toBeLessThanOrEqual(46);
  await noSideScroll(page);
}
