import { execFileSync } from 'node:child_process';
import http from 'node:http';
import { Page, Request } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { askPageConnectPort, askPageFakePort, askPagePort, dsn, password, repoRoot, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';

// THE ASK PAGE A NOTIFICATION OPENS (internal/api attention_ask_page.go), in a
// real browser against a real muster-server (the e2eterm build, its suggestion
// model pointed at a FAKE chat-completions endpoint this spec runs — never a
// real model), the asker a real agent over the real /mcp connect door:
//
//   an agent asks (attention_ask, with options) → the notification's tap URL,
//   /attention/open/ask-<id>, lands on /attention/asks/<id> → the question,
//   its options as full-width buttons, the suggested replies as chips (made
//   by the fake model in the background, arriving LIVE on a page that was
//   already open), the answer box pinned to the bottom — at an explicit phone
//   viewport (360 and 412 wide) and a desktop one (1280×800) — no sideways
//   scroll; a chip FILLS the box and sends nothing; Answer comes back to the
//   page showing the answer, and the agent's wait returns it.
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

async function signIn(page: Page) {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
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
    test('the tap lands on a phone-first ask page: options, live chips that fill, a pinned answer box, no sideways scroll', async ({ page }) => {
      test.setTimeout(90_000);
      let release = () => {};
      hold = vp.live ? new Promise<void>((r) => (release = r)) : undefined;
      const before = calls;
      await signIn(page);
      const id = await ask(question(vp.name));
      if (!vp.live) await expect.poll(() => calls - before, { timeout: 15_000 }).toBe(1);

      // The notification's own tap URL.
      await page.goto(origin + `/attention/open/ask-${id}`);
      await page.waitForURL(origin + `/attention/asks/${id}`);
      await expect(page.getByRole('heading', { level: 1 })).toHaveText(`${agentName} is asking`);
      await expect(page.locator('[data-ask-question]')).toHaveText(question(vp.name));
      await expect(page.getByRole('link', { name: 'Back to Asks' })).toHaveAttribute('href', '/attention');
      await expect(page.locator('[data-ask-context]')).toHaveAttribute('href', `/agents/${agentName}`);
      const options = page.locator('[data-ask-option]');
      await expect(options).toHaveCount(2);
      const vw = page.viewportSize()!;
      for (const o of await options.all()) {
        const b = (await o.boundingBox())!;
        expect(b.height, 'an option is at least a 44px target').toBeGreaterThanOrEqual(44);
        expect(b.width, 'an option spans the column').toBeGreaterThan(vw.width - 48);
      }

      // The chips: already there, or — the model held — arriving LIVE.
      const chips = page.locator('[data-ask-suggestion]');
      if (vp.live) {
        await expect(chips, 'no chip while the model is still working').toHaveCount(0);
        release();
      }
      await expect(chips).toHaveCount(2, { timeout: 15_000 });
      await expect(chips.first()).toHaveText(sugA);
      await expect(page.getByRole('group', { name: 'AI-suggested replies' })).toBeVisible();
      for (const c of await chips.all()) expect((await c.boundingBox())!.height).toBeGreaterThanOrEqual(44);
      expect(calls - before, 'one model call for the ask').toBe(1);

      // The answer box: pinned to the bottom of the screen, 16px text (no iOS
      // zoom), and nothing scrolls sideways.
      const compose = (await page.locator('#app-compose').boundingBox())!;
      expect(compose.y + compose.height, 'the answer box sits at the bottom of the screen').toBeGreaterThan(vw.height - 2);
      expect(compose.y + compose.height).toBeLessThanOrEqual(vw.height + 1);
      const box = page.locator('#ask-input');
      expect(await box.evaluate((e) => getComputedStyle(e).fontSize)).toBe('16px');
      await noSidewaysScroll(page);
      // Scrolling the content leaves the box where it is.
      await page.locator('#app-main').evaluate((e) => e.scrollTo(0, e.scrollHeight));
      expect((await page.locator('#app-compose').boundingBox())!.y).toBeCloseTo(compose.y, 0);

      // A chip FILLS the box and sends nothing.
      const sent = posts(page);
      await chips.nth(1).tap();
      await expect(box).toHaveValue(sugB);
      await expect(box).toBeFocused();
      await page.waitForTimeout(300);
      expect(sent, 'a chip sent nothing').toEqual([]);

      // Answer: back on the page, answered; the agent's wait returns it.
      await box.fill(sugB + ' — and post in the channel');
      // The page posts and comes back to ITSELF, so the URL alone cannot say
      // the answer was sent: wait for the POST's redirect.
      await Promise.all([answered(page, id), page.locator('[data-ask-send]').tap()]);
      await page.waitForURL(origin + `/attention/asks/${id}`);
      expect(sent).toEqual([`/attention/asks/${id}/answer`]);
      await expect(page.locator('[data-ask-closed]')).toBeVisible();
      await expect(page.locator('[data-ask-answer]')).toHaveText('Your answer: ' + sugB + ' — and post in the channel');
      await expect(page.locator('#app-compose'), 'no answer box on an answered ask').toBeHidden();
      const waited = JSON.parse(await mcpCall('attention_wait', { id, wait_seconds: 0 })) as { status: string; answer: string };
      expect(waited).toMatchObject({ status: 'answered', answer: sugB + ' — and post in the channel' });
      await noSidewaysScroll(page);
    });
  });
}

test.describe('desktop 1280×800', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });
  test('the ask page keeps the side nav and a centred column; an option answers at once', async ({ page }) => {
    test.setTimeout(90_000);
    hold = undefined;
    await signIn(page);
    const id = await ask(question('desktop'));
    await page.goto(origin + `/attention/open/ask-${id}`);
    await page.waitForURL(origin + `/attention/asks/${id}`);
    await expect(page.locator('#sidebar'), 'the side nav is shown on a desktop').toBeVisible();
    await expect(page.locator('#sidebar-open'), 'no hamburger on a desktop').toBeHidden();
    const main = (await page.locator('#app-main').boundingBox())!;
    expect(main.x, 'the content is beside the side nav').toBeGreaterThanOrEqual(280);
    const q = (await page.locator('[data-ask-question]').boundingBox())!;
    expect(q.width, 'the question reads in a column, not the full width').toBeLessThanOrEqual(680);
    const compose = (await page.locator('#app-compose').boundingBox())!;
    expect(compose.y + compose.height).toBeLessThanOrEqual(800 + 1);
    await noSidewaysScroll(page);
    await expect(page.locator('[data-ask-suggestion]')).toHaveCount(2, { timeout: 15_000 });
    await Promise.all([answered(page, id), page.getByRole('button', { name: 'wait until Monday', exact: true }).click()]);
    await page.waitForURL(origin + `/attention/asks/${id}`);
    await expect(page.locator('[data-ask-answer]')).toHaveText('Your answer: wait until Monday');
    // The queue page itself is unchanged: the ask is no longer open there.
    await page.goto(origin + '/attention');
    await expect(page.locator(`#ask-${id}`)).toHaveCount(0);
  });
});
