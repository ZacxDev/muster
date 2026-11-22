import { execFileSync } from 'node:child_process';
import http from 'node:http';
import { Page, Request } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { askPageConnectPort, askPageFakePort, askPagePort, dsn, repoRoot, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { turnSuggestionsOn } from '../lib/suggest';

// THE ASK PAGE A NOTIFICATION OPENS (internal/api attention_ask_page.go), in a
// real browser against a real muster-server (the e2eterm build, its suggestion
// model pointed at a FAKE chat-completions endpoint this spec runs — never a
// real model), the asker a real agent over the real /mcp connect door:
//
//   an agent asks (attention_ask, with options) → the notification's tap URL,
//   /attention/open/ask-<id>, lands on /attention/asks/<id> → the question,
//   its options as full-width buttons, the suggested replies as chips (made
//   by the fake model in the background, arriving LIVE on a page that was
//   already open), the answer box open beside the options — the feed card's
//   own answer form (askAnswerFormHTML) — at an explicit phone viewport (360
//   and 412 wide) and a desktop one (1280×800) — no sideways scroll; a chip
//   FILLS the box and sends nothing; Answer comes back to the page showing the
//   answer, and the agent's wait returns it.
//
// 🔴 RED AT origin/main: there the tap URL redirects to /agents/<name>, and
// /attention/asks/<id> is a 404.

const origin = `http://localhost:${askPagePort}`;
const connectURL = `http://127.0.0.1:${askPageConnectPort}`;
const stamp = Date.now().toString(36);
const agentName = 'e2e-askpage-' + stamp;
const agentToken = 'e2e-askpage-agent-token-not-a-real-one-' + stamp;
const sugA = 'Wait until Monday, after the backup is verified';
const sugB = 'Deploy now, but only after the smoke tests';

// The fake model: every request counted; each answered (once `hold` is
// released) with two grounded suggestions for the spec's question.
let calls = 0;
let hold: Promise<void> | undefined;
const fake = http.createServer((req, res) => {
  let body = '';
  req.on('data', (d) => (body += d));
  req.on('end', async () => {
    calls++;
    if (hold) await hold;
    const content = JSON.stringify({
      suggestions: [
        { text: sugA, kind: 'decide', cites: ['R'], quote: 'wait until Monday' },
        { text: sugB, kind: 'decide', cites: ['R'], quote: 'Deploy the importer to production now' },
      ],
    });
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ choices: [{ message: { content } }], usage: { prompt_tokens: 300, completion_tokens: 40 } }));
  });
});

let server: Running | undefined;
let agentMade = false;
const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await new Promise<void>((r) => fake.listen(askPageFakePort, '127.0.0.1', () => r()));
  server = await startServer(versionTerm, askPagePort, {
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x3e).toString('base64'),
    MUSTER_CONNECT_PORT: String(askPageConnectPort),
    MUSTER_AGENT_CONNECT_URL: connectURL,
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_E2E_SUGGEST_URL: `http://127.0.0.1:${askPageFakePort}/chat`,
  });
  fix('agent', '1', agentName, agentToken);
  agentMade = true;
});

test.afterAll(async () => {
  try {
    if (agentMade) fix('cleanup', '1', agentName);
  } finally {
    await server?.stop();
    await new Promise((r) => fake.close(r));
  }
});

async function mcpCall(name: string, args: Record<string, unknown>): Promise<string> {
  const r = await fetch(connectURL + '/mcp', {
    method: 'POST',
    headers: { 'X-Muster-Token': agentToken, 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name, arguments: args } }),
  });
  expect(r.status, 'the /mcp door answered').toBe(200);
  const j = (await r.json()) as { result?: { content: { text: string }[]; isError?: boolean }; error?: unknown };
  expect(j.error, JSON.stringify(j.error)).toBeUndefined();
  expect(j.result!.isError, j.result!.content[0].text).toBeFalsy();
  return j.result!.content[0].text;
}

async function ask(question: string): Promise<number> {
  const text = await mcpCall('attention_ask', { question, options: ['deploy now', 'wait until Monday'] });
  return (JSON.parse(text) as { id: number }).id;
}

// askFree is an ask with no options: its answer box is the only way to answer.
async function askFree(question: string): Promise<number> {
  const text = await mcpCall('attention_ask', { question });
  return (JSON.parse(text) as { id: number }).id;
}

// askMulti is a MULTI-select ask (migration 0175): its options are CHECKBOXES.
// Seeded through the real /mcp door, as a multi-select asker files it.
async function askMulti(question: string, options: string[]): Promise<number> {
  const text = await mcpCall('attention_ask', { question, options, multi: true });
  return (JSON.parse(text) as { id: number }).id;
}

// posts records every POST the page makes.
function posts(page: Page): string[] {
  const seen: string[] = [];
  page.on('request', (r: Request) => {
    if (r.method() === 'POST') seen.push(new URL(r.url()).pathname);
  });
  return seen;
}

// answered resolves on the answer form's POST being redirected back to the page.
const answered = (page: Page, id: number) =>
  page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === `/attention/asks/${id}/answer` && r.status() === 303);

async function noSidewaysScroll(page: Page) {
  const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
  expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(iw);
}

const question = (vp: string) => `Deploy the importer to production now, or wait until Monday? (${vp})`;

for (const vp of [
  { name: 'phone 360×740', live: false, use: { viewport: { width: 360, height: 740 }, hasTouch: true, isMobile: true } },
  { name: 'phone 412×915', live: true, use: { viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);
    test('the tap lands on a phone-first ask page: options, live chips that fill, the answer box, no sideways scroll', async ({ page }) => {
      test.setTimeout(90_000);
      let release = () => {};
      hold = vp.live ? new Promise<void>((r) => (release = r)) : undefined;
      const before = calls;
      await signIn(page, origin);
      await turnSuggestionsOn(page, origin); // off until the user turns them on
      const id = await ask(question(vp.name));
      if (!vp.live) await expect.poll(() => calls - before, { timeout: 15_000 }).toBe(1);

      // The notification's own tap URL.
      await page.goto(origin + `/attention/open/ask-${id}`);
      await page.waitForURL(origin + `/attention/asks/${id}`);
      await expect(page.getByRole('heading', { level: 1 })).toHaveText(`${agentName} is asking`);
      await expect(page.locator('[data-ask-question]')).toHaveText(question(vp.name));
      await expect(page.getByRole('link', { name: 'Back to Asks' })).toHaveAttribute('href', '/attention');
      await expect(page.locator('[data-ask-context]')).toHaveAttribute('href', `/agents/${agentName}`);
      const options = page.locator('#ask-answer [data-attention-option]');
      await expect(options).toHaveCount(2);
      const vw = page.viewportSize()!;
      // The page is the feed's card, expanded (asks_feed.go), with the card's
      // answer form: its options laid out as the card lays them out (two
      // short ones share a row, askOptionsShort), inside the card's content
      // box, each a 44px target.
      const cardBox = (await page.locator('[data-asks-page-card]').boundingBox())!;
      expect(cardBox.width, 'the card spans the column').toBeGreaterThan(vw.width - 48);
      const ob = await Promise.all((await options.all()).map(async (o) => (await o.boundingBox())!));
      for (const b of ob) {
        expect(b.height, 'an option is at least a 44px target').toBeGreaterThanOrEqual(44);
        expect(b.x, 'an option is inside the card').toBeGreaterThanOrEqual(cardBox.x);
        expect(b.x + b.width, 'an option is inside the card').toBeLessThanOrEqual(cardBox.x + cardBox.width);
      }
      expect(ob[0].y, 'two short options share a row, as on the card').toBeCloseTo(ob[1].y, 0);
      expect(ob[0].width + ob[1].width, 'the pair spans the card').toBeGreaterThan(cardBox.width - 48);

      // The chips: already there, or — the model held — arriving LIVE.
      const chips = page.locator('#ask-live [data-asks-suggestion]');
      if (vp.live) {
        await expect(chips, 'no chip while the model is still working').toHaveCount(0);
        release();
      }
      await expect(chips).toHaveCount(2, { timeout: 15_000 });
      await expect(chips.first()).toHaveText(sugA);
      await expect(page.getByRole('group', { name: 'AI-suggested replies' })).toBeVisible();
      for (const c of await chips.all()) expect((await c.boundingBox())!.height).toBeGreaterThanOrEqual(44);
      expect(calls - before, 'one model call for the ask').toBe(1);

      // The answer box: in the card, under the options, as on the feed's card
      // — open, never behind an "Other answer…" toggle — 16px text (no iOS
      // zoom), and nothing scrolls sideways.
      const box = page.locator('#ask-input');
      await expect(box, 'beside options the box is open, as on the card').toBeVisible();
      await expect(page.locator('[data-ask-other]')).toHaveCount(0);
      const bb = (await box.boundingBox())!;
      expect(bb.x, 'the box is inside the card').toBeGreaterThanOrEqual(cardBox.x);
      expect(bb.x + bb.width).toBeLessThanOrEqual(cardBox.x + cardBox.width);
      expect(await box.evaluate((e) => getComputedStyle(e).fontSize)).toBe('16px');
      await noSidewaysScroll(page);

      // Answer is DISABLED while the box is blank (a blank answer used to
      // round-trip to a server notice); a chip's fill enables it.
      const send = page.locator('#ask-send');
      await expect(send).toBeDisabled();
      // Pressed anyway (aria-disabled keeps it focusable): nothing is posted,
      // the box takes focus.
      const blank = posts(page);
      await send.click({ force: true });
      await page.waitForTimeout(300);
      expect(blank, 'a blank Answer posted nothing').toEqual([]);
      await expect(box).toBeFocused();
      await box.fill('   ');
      await expect(send, 'whitespace is still blank').toBeDisabled();
      await box.fill('');
      // A chip FILLS the box and sends nothing.
      const sent = posts(page);
      await chips.nth(1).tap();
      await expect(box).toHaveValue(sugB);
      await expect(send).toBeEnabled();
      await expect(box).toBeFocused();
      await page.waitForTimeout(300);
      expect(sent, 'a chip sent nothing').toEqual([]);

      // Answer: back on the page, answered; the agent's wait returns it.
      await box.fill(sugB + ' — and post in the channel');
      // The page posts and comes back to ITSELF, so the URL alone cannot say
      // the answer was sent: wait for the POST's redirect.
      await Promise.all([answered(page, id), page.locator('#ask-send').tap()]);
      await page.waitForURL(origin + `/attention/asks/${id}`);
      expect(sent).toEqual([`/attention/asks/${id}/answer`]);
      await expect(page.locator('[data-ask-closed]')).toBeVisible();
      // Settled, it is headed by its state, not "… is asking".
      await expect(page.getByRole('heading', { level: 1 })).toHaveText('Answered');
      // …so the badge row names who asked (an open page's heading does).
      await expect(page.locator('[data-ask-source]')).toHaveText(agentName);
      await expect(page.locator('[data-ask-source]')).toBeVisible();
      await expect(page.locator('[data-ask-answer]')).toHaveText('Your answer: ' + sugB + ' — and post in the channel');
      await expect(page.locator('#ask-answer, #ask-input'), 'no answer form on an answered ask').toHaveCount(0);
      const waited = JSON.parse(await mcpCall('attention_wait', { id, wait_seconds: 0 })) as { status: string; answer: string };
      expect(waited).toMatchObject({ status: 'answered', answer: sugB + ' — and post in the channel' });
      await noSidewaysScroll(page);
    });
  });
}

// A MULTI-SELECT ASK'S ONE SEND COUNTS A TICKED BOX (migration 0175): a tick
// with an empty free-text box is a complete answer. (RED once: the page's own
// composer read its form's DESCENDANTS for a tick while the boxes sat outside
// it; the page now draws the card's form, the boxes inside it.)
test.describe('multi-select ask, phone 390×844', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
  test('a ticked box with an empty free-text box answers from Send', async ({ page }) => {
    test.setTimeout(90_000);
    hold = undefined;
    await signIn(page, origin);
    const id = await askMulti(`Which checks should the release run? (${stamp})`, ['lint', 'typecheck', 'e2e']);
    await page.goto(origin + `/attention/asks/${id}`);
    const box = page.locator('#ask-input'), send = page.locator('#ask-send');
    // A multi ask's free-text row is OPEN, its optional note under it.
    await expect(box).toBeVisible();
    await expect(box, 'the free-text box starts empty').toHaveValue('');
    // Tick ONE box and leave the free text empty: the pick alone is the answer.
    await page.locator('[data-attention-option]').first().check();
    await expect(page.locator('[data-attention-option]').first()).toBeChecked();
    await expect(box).toHaveValue('');
    // Send must count the tick as an answer: the tap posts the ticked label
    // and the page comes back settled (forced, so a Send wrongly drawn
    // aria-disabled still reaches the submit guard that must let it through).
    const sent = posts(page);
    await Promise.all([
      page
        .waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === `/attention/asks/${id}/answer`, { timeout: 5_000 })
        .catch(() => null),
      send.click({ force: true }),
    ]);
    expect(sent, 'Send posted the ticked box').toEqual([`/attention/asks/${id}/answer`]);
    await page.waitForURL(origin + `/attention/asks/${id}`);
    await expect(page.locator('[data-ask-closed]')).toBeVisible();
    await expect(page.locator('[data-ask-answer]')).toHaveText('Your answer: lint');
    const waited = JSON.parse(await mcpCall('attention_wait', { id, wait_seconds: 0 })) as { status: string; answer: string };
    expect(waited).toMatchObject({ status: 'answered', answer: 'lint' });
  });
});

// THE OPERATOR'S PATH: a multi-select ask's card offers a note box, and its
// details page must too — the page draws the card's answer form. On the page:
// tick a box, type a note, Send, and the stored answer is "<option> — note:
// <text>". The top bar puts the menu LEFT of the title (a phone; on a desktop
// the side nav shows and the menu hides), and the breadcrumbs "Asks › Ask #N"
// take you back to the asks list. RED at the base: the page drew its own
// composer, pinned to the bottom, with the boxes and the note outside the
// answer form (joined by form=), no breadcrumbs, and the menu right of the
// title.
for (const vp of [
  { name: 'phone 390×844', phone: true, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', phone: false, use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`the card's answer form on the page, ${vp.name}`, () => {
    test.use(vp.use);
    test('tick, note, Send records "<option> — note: <text>"; the menu is left of the title; the breadcrumbs go back to Asks', async ({ page }) => {
      test.setTimeout(90_000);
      hold = undefined;
      await signIn(page, origin);
      const id = await askMulti(`Which checks before the release? (${vp.name} ${stamp})`, ['lint', 'typecheck', 'e2e']);

      // The card on the Asks list offers the note; so does the page.
      await page.goto(origin + '/attention');
      const card = page.locator(`#ask-${id}`);
      await expect(card.locator('textarea[name="note"]'), 'the card offers the note').toBeVisible();
      await page.goto(origin + `/attention/asks/${id}`);
      const form = page.locator('#ask-answer');
      await expect(form.locator('input[type="checkbox"][name="option"]')).toHaveCount(3);
      const note = form.getByLabel('Note (optional)');
      await expect(note, 'the page offers the note, shown').toBeVisible();
      await expect(page.locator('[data-ask-other]')).toHaveCount(0);

      // The top bar: the menu at the left, before the back arrow and the title.
      const title = (await page.getByRole('heading', { level: 1 }).boundingBox())!;
      const back = (await page.locator('#app-back').boundingBox())!;
      if (vp.phone) {
        const menu = (await page.locator('#sidebar-open').boundingBox())!;
        expect(menu.x + menu.width, 'the menu is left of the back arrow').toBeLessThanOrEqual(back.x + 1);
        expect(back.x + back.width, 'the back arrow is left of the title').toBeLessThanOrEqual(title.x + 1);
        expect(menu.x, 'the menu is at the left edge').toBeLessThan(vp.use.viewport.width / 4);
      } else {
        await expect(page.locator('#sidebar-open'), 'no hamburger on a desktop').toBeHidden();
        await expect(page.locator('#sidebar')).toBeVisible();
        expect(back.x + back.width).toBeLessThanOrEqual(title.x + 1);
      }

      // The breadcrumbs: Asks › Ask #N, the ask the current page.
      const crumbs = page.getByRole('navigation', { name: 'Breadcrumb' });
      await expect(crumbs).toHaveText(`Asks›Ask #${id}`);
      await expect(crumbs.locator('[aria-current="page"]')).toHaveText(`Ask #${id}`);
      await noSidewaysScroll(page);

      // Tick one box, type a note, Send.
      await form.locator('input[type="checkbox"][value="typecheck"]').check();
      await note.fill('run it twice');
      const sent = posts(page);
      await Promise.all([answered(page, id), page.locator('#ask-send').click()]);
      expect(sent).toEqual([`/attention/asks/${id}/answer`]);
      await page.waitForURL(origin + `/attention/asks/${id}`);
      await expect(page.locator('[data-ask-answer]')).toHaveText('Your answer: typecheck — note: run it twice');
      const waited = JSON.parse(await mcpCall('attention_wait', { id, wait_seconds: 0 })) as { status: string; answer: string };
      expect(waited).toMatchObject({ status: 'answered', answer: 'typecheck — note: run it twice' });

      // The breadcrumbs take you back to the asks list.
      await page.getByRole('navigation', { name: 'Breadcrumb' }).getByRole('link', { name: 'Asks' }).click();
      await page.waitForURL(origin + '/attention');
      await expect(page.locator('#attention-items')).toBeVisible();
    });
  });
}

// AN OVER-LONG ANSWER (a long dictation fills the box from script, past its
// maxlength) is refused with its own notice, never "Type an answer or pick an
// option.", and the open page names nobody in its badge row (its heading
// does). RED before the fix: the notice was the blank answer's.
test.describe('over-long answer, phone 390×844', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
  test('an answer over 8000 characters says so', async ({ page }) => {
    test.setTimeout(90_000);
    hold = undefined;
    await signIn(page, origin);
    const id = await askFree(question('over-long'));
    await page.goto(origin + `/attention/asks/${id}`);
    await expect(page.locator('[data-ask-source]')).toHaveCount(0);
    const box = page.locator('#ask-input');
    await box.evaluate((el: HTMLTextAreaElement) => {
      el.value = 'a'.repeat(8001);
      el.dispatchEvent(new Event('input', { bubbles: true }));
    });
    await Promise.all([
      page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === `/attention/asks/${id}/answer`),
      page.locator('#ask-send').tap(),
    ]);
    await expect(page.getByText('Not recorded: the answer is longer than 8000 characters. Shorten it and send it again.')).toBeVisible();
    await expect(page.getByText('Type an answer or pick an option.')).toHaveCount(0);
    const waited = JSON.parse(await mcpCall('attention_wait', { id, wait_seconds: 0 })) as { status: string };
    expect(waited.status, 'nothing was recorded').toBe('pending');
  });
});

// An ask answered ELSEWHERE while its page is open: the page follows live — it
// shows the answer, and neither the answer box nor Dismiss stays on screen
// (Dismiss sits outside the live region). RED at #313's round-0 head, where
// Dismiss stayed.
test.describe('answered elsewhere, phone 390×844', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
  test('the open page closes live: no answer box, no Dismiss', async ({ page }) => {
    test.setTimeout(90_000);
    hold = undefined;
    await signIn(page, origin);
    await turnSuggestionsOn(page, origin); // off until the user turns them on
    const id = await ask(question('elsewhere'));
    await page.goto(origin + `/attention/asks/${id}`);
    await expect(page.locator('[data-ask-dismiss]')).toBeVisible();
    // Another device answers it (the queue's own route, as the browser would).
    const r = await page.request.post(origin + `/attention/asks/${id}/answer`, {
      form: { option: 'wait until Monday' },
      headers: { Origin: origin },
      maxRedirects: 0,
    });
    expect(r.status(), "the other device's answer was recorded").toBe(303);
    await expect(page.locator('[data-ask-closed]')).toBeVisible({ timeout: 15_000 });
    await expect(page.locator('#ask-input'), 'no answer box on a closed ask').toBeHidden();
    await expect(page.locator('#ask-send')).toBeHidden();
    await expect(page.locator('#ask-answer [data-attention-option]').first(), 'no option on a closed ask').toBeHidden();
    await expect(page.locator('[data-ask-dismiss]'), 'no Dismiss on a closed ask').toBeHidden();
  });
});

test.describe('desktop 1280×800', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });
  test('the ask page keeps the side nav and a centred column; an option answers at once', async ({ page }) => {
    test.setTimeout(90_000);
    hold = undefined;
    await signIn(page, origin);
    await turnSuggestionsOn(page, origin); // off until the user turns them on
    const id = await ask(question('desktop'));
    await page.goto(origin + `/attention/open/ask-${id}`);
    await page.waitForURL(origin + `/attention/asks/${id}`);
    await expect(page.locator('#sidebar'), 'the side nav is shown on a desktop').toBeVisible();
    await expect(page.locator('#sidebar-open'), 'no hamburger on a desktop').toBeHidden();
    const main = (await page.locator('#app-main').boundingBox())!;
    expect(main.x, 'the content is beside the side nav').toBeGreaterThanOrEqual(280);
    const q = (await page.locator('[data-ask-question]').boundingBox())!;
    expect(q.width, 'the question reads in a column, not the full width').toBeLessThanOrEqual(680);
    const form = (await page.locator('#ask-answer').boundingBox())!;
    expect(form.width, 'the answer form reads in the column too').toBeLessThanOrEqual(680);
    await noSidewaysScroll(page);
    await expect(page.locator('#ask-live [data-asks-suggestion]')).toHaveCount(2, { timeout: 15_000 });
    await Promise.all([answered(page, id), page.getByRole('button', { name: 'wait until Monday', exact: true }).click()]);
    await page.waitForURL(origin + `/attention/asks/${id}`);
    await expect(page.locator('[data-ask-answer]')).toHaveText('Your answer: wait until Monday');
    // The queue page itself is unchanged: the ask is no longer open there.
    await page.goto(origin + '/attention');
    await expect(page.locator(`#ask-${id}`)).toHaveCount(0);
  });
});

// AN ANSWER THAT FAILS TO POST (the asks audit, #12): a server error or no
// network is said in one line under the answer form; the page, the question
// and the typed text stay, and the same tap sends it once the server is back.
// An option's failed post is said the same way. RED before: the form was a
// plain post, so a 500 replaced the whole page with the server's error.
//
// Beside options the answer box is OPEN, as on the feed's card (it was once
// collapsed behind "Other answer…", which hid a multi-select ask's note).
for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`failed posts, ${vp.name}`, () => {
    test.use(vp.use);
    test('an answer that fails to post says so under the form, keeps the text, and the same tap retries it', async ({ page }) => {
      test.setTimeout(90_000);
      hold = undefined;
      await signIn(page, origin);
      const q = `Which branch should the importer ship from? (${vp.name})`;
      const id = await askFree(q);
      let mode: 'error' | 'offline' | 'through' = 'error';
      let tries = 0;
      await page.route(`**/attention/asks/${id}/answer`, async (r) => {
        tries++;
        if (mode === 'error') return r.fulfill({ status: 500, contentType: 'text/plain', body: 'internal error' });
        if (mode === 'offline') return r.abort('internetdisconnected');
        return r.continue();
      });
      await page.goto(origin + `/attention/asks/${id}`);
      const box = page.locator('#ask-input'), send = page.locator('#ask-send'), err = page.locator('#ask-error');
      await expect(page.locator('[data-ask-other]'), 'no options: the box is open').toHaveCount(0);
      await expect(box).toBeVisible();
      await expect(err).toBeHidden();
      await box.fill('Cut it from main');
      await send.click();
      await expect(err).toBeVisible();
      await expect(err).toHaveText("Your answer wasn't sent: muster couldn't record it. Try again.");
      await expect(err).toHaveAttribute('role', 'alert');
      expect(tries, 'the answer was posted once').toBe(1);
      await expect(page).toHaveURL(origin + `/attention/asks/${id}`);
      await expect(page.locator('[data-ask-question]'), 'the page is still the question').toHaveText(q);
      await expect(box, 'the typed text is kept').toHaveValue('Cut it from main');
      await expect(send).toBeEnabled();
      // Under the form: below the box.
      const eb = (await err.boundingBox())!, bb = (await box.boundingBox())!;
      expect(eb.y, 'the error line is under the answer form').toBeGreaterThanOrEqual(bb.y + bb.height - 1);

      mode = 'offline';
      await send.click();
      await expect(err).toHaveText("Your answer wasn't sent: muster couldn't be reached. Check your connection and try again.");
      await expect(box).toHaveValue('Cut it from main');

      // The server is back: the same tap answers.
      mode = 'through';
      await Promise.all([answered(page, id), send.click()]);
      await expect(page.locator('[data-ask-answer]')).toHaveText('Your answer: Cut it from main');
      await expect(err).toHaveCount(0);
      expect(tries).toBe(3);
    });

    test('an option that fails to post says so too; the box beside the options answers', async ({ page }) => {
      test.setTimeout(90_000);
      hold = undefined;
      await signIn(page, origin);
      const id = await ask(question('failed option ' + vp.name));
      let fail = true;
      await page.route(`**/attention/asks/${id}/answer`, (r) => (fail ? r.fulfill({ status: 502, body: 'bad gateway' }) : r.continue()));
      await page.goto(origin + `/attention/asks/${id}`);
      const box = page.locator('#ask-input');
      await expect(box, 'beside options the box is open').toBeVisible();
      await expect(page.locator('[data-ask-other]')).toHaveCount(0);
      await page.getByRole('button', { name: 'wait until Monday', exact: true }).click();
      await expect(page.locator('#ask-error')).toHaveText("Your answer wasn't sent: muster couldn't record it. Try again.");
      await expect(page.locator('#ask-answer [data-attention-option]')).toHaveCount(2);
      // Typed text answers instead.
      fail = false;
      await box.fill('Monday, after the backup');
      await Promise.all([answered(page, id), page.locator('#ask-send').click()]);
      await expect(page.locator('[data-ask-answer]')).toHaveText('Your answer: Monday, after the backup');
    });
  });
}

// THE IN-PLACE POST'S EDGES (the follow-up audit), at a phone:
//   - signed out (the session cookie gone) the answer route refuses the post
//     (401/403); no retry on this page can succeed, so the page is loaded
//     again and the sign-in gate takes over — never "couldn't record it. Try
//     again." for ever. A 403 (another site's post) does the same.
//   - a browser whose FormData ignores its submitter argument still posts the
//     tapped option (it is appended by hand).
//   - a page shown again mid-post (pageshow: the back-forward cache) is no
//     longer busy: the same tap posts again.
// RED before: the 401 drew the inline error; the option was dropped (the
// fallback sat in a catch that never runs) and the answer was refused as
// blank; a restored page stayed busy and ignored every tap.
test.describe('in-place post edges, phone 390×844', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('signed out, the post loads the page again and its sign-in gate takes over', async ({ page }) => {
    test.setTimeout(90_000);
    hold = undefined;
    await signIn(page, origin);
    const id = await askFree(`Signed out mid-answer? ${stamp}`);
    await page.goto(origin + `/attention/asks/${id}`);
    await page.locator('#ask-input').fill('Ship it');
    await page.context().clearCookies({ name: 'muster_session' });
    // Whether the inline error was ever drawn, kept across the navigation
    // (the page that drew it is gone by the time this test looks).
    await page.evaluate(() => {
      const el = document.getElementById('ask-error')!;
      new MutationObserver(() => {
        if (!el.hidden) sessionStorage.setItem('ask-error-shown', el.textContent || '');
      }).observe(el, { attributes: true, childList: true, characterData: true, subtree: true });
    });
    const refused = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === `/attention/asks/${id}/answer`);
    const reloaded = page.waitForRequest((r) => r.method() === 'GET' && r.isNavigationRequest() && new URL(r.url()).pathname === `/attention/asks/${id}`);
    await page.locator('#ask-send').click();
    const res = await refused;
    expect([401, 403], `the signed-out post was refused (${res.status()})`).toContain(res.status());
    await reloaded;
    await page.waitForURL((u) => u.pathname.startsWith('/login'));
    await expect(page.locator('#ask-error')).toHaveCount(0);
    expect(await page.evaluate(() => sessionStorage.getItem('ask-error-shown')), 'the refused post drew no inline error').toBeNull();
    const waited = JSON.parse(await mcpCall('attention_wait', { id, wait_seconds: 0 })) as { status: string };
    expect(waited.status, 'nothing was recorded').toBe('pending');
  });

  test('a 403 loads the page again instead of drawing the inline error', async ({ page }) => {
    test.setTimeout(90_000);
    hold = undefined;
    await signIn(page, origin);
    const id = await askFree(`Refused as cross-site? ${stamp}`);
    await page.route(`**/attention/asks/${id}/answer`, (r) => r.fulfill({ status: 403, contentType: 'application/json', body: '{"error":"forbidden"}' }));
    await page.goto(origin + `/attention/asks/${id}`);
    await page.locator('#ask-input').fill('Ship it');
    const reloaded = page.waitForRequest((r) => r.method() === 'GET' && r.isNavigationRequest() && new URL(r.url()).pathname === `/attention/asks/${id}`);
    await page.locator('#ask-send').click();
    await reloaded;
    await page.waitForLoadState('load');
    await expect(page.locator('#ask-error')).toBeHidden();
  });

  test('an option is posted where FormData ignores its submitter', async ({ page }) => {
    test.setTimeout(90_000);
    hold = undefined;
    await page.addInitScript(() => {
      const F = window.FormData;
      // An older browser: the submitter argument is ignored, never thrown on.
      const Old = function (this: unknown, f?: HTMLFormElement) {
        return new F(f);
      } as unknown as typeof FormData;
      Old.prototype = F.prototype;
      window.FormData = Old;
    });
    await signIn(page, origin);
    const id = await ask(question('old FormData'));
    await page.goto(origin + `/attention/asks/${id}`);
    let body = '';
    page.on('request', (r) => {
      if (r.method() === 'POST' && new URL(r.url()).pathname === `/attention/asks/${id}/answer`) body = r.postData() ?? '';
    });
    await Promise.all([answered(page, id), page.getByRole('button', { name: 'wait until Monday', exact: true }).click()]);
    expect(new URLSearchParams(body).get('option'), 'the tapped option was posted').toBe('wait until Monday');
    await expect(page.locator('[data-ask-answer]')).toHaveText('Your answer: wait until Monday');
  });

  test('a page shown again mid-post is usable: the same tap posts again', async ({ page }) => {
    test.setTimeout(90_000);
    hold = undefined;
    await signIn(page, origin);
    const id = await askFree(`Restored mid-post? ${stamp}`);
    let tries = 0;
    let through = false;
    await page.route(`**/attention/asks/${id}/answer`, async (r) => {
      tries++;
      if (!through) return; // held: never answered, as a page left mid-post
      return r.continue();
    });
    await page.goto(origin + `/attention/asks/${id}`);
    const form = page.locator('#ask-answer');
    await page.locator('#ask-input').fill('Ship it');
    await page.locator('#ask-send').click();
    await expect(form).toHaveAttribute('aria-busy', 'true');
    await expect.poll(() => tries).toBe(1);
    // What the back-forward cache does to a page it shows again.
    await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true })));
    await expect(form).not.toHaveAttribute('aria-busy', 'true');
    through = true;
    await Promise.all([answered(page, id), page.locator('#ask-send').click()]);
    expect(tries, 'the restored page posted again').toBe(2);
    await expect(page.locator('[data-ask-answer]')).toHaveText('Your answer: Ship it');
  });
});
