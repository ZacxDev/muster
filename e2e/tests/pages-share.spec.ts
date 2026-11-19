import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import type { Browser, BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { binDir, dsn, pagesShareConnectPort, pagesShareMirrorMCPPort, pagesSharePort, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { withMCPSession } from '../lib/localmcp';

// SHAREABLE PAGES, INSTANT SHARE (docs/pages.md#sharing, #in-the-chat), every
// hop real, on a server with a link key (MUSTER_CONNECTOR_ENCRYPTION_KEY),
// whose owner turns ON "Let my agents share pages without asking" on the
// Account page (a per-owner opt-in; pages.spec.ts is an owner who did not):
//
//   a Claude Code session in a mirrored window publishes a page through its
//   host's loopback MCP endpoint (page_create) → the call and its result land
//   in the session's transcript, as the CLI writes them → the mirrored view
//   draws the PAGE CARD → the owner taps Share:
//     - on a desktop browser with no share sheet, the page's link is on the
//       clipboard and "Link copied" shows; a second Share copies the SAME link;
//     - on a phone, navigator.share is called with the page's title and that
//       same link, SYNCHRONOUSLY inside the tap (the address is fetched on
//       pointerdown, before the click — what iOS Safari's share sheet needs);
//   → a signed-out browser opens the link and sees the page → the owner turns
//   the opt-in on, on Account → the agent's own page_share_request is live at
//   once and returns that same link.
//
// Every value below is an obvious fake.

const origin = `http://localhost:${pagesSharePort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-pshare-' + stamp;
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'pagesshare-'));
const tr = path.join(dir, 'eeeeeeee-' + stamp + '.jsonl');
const fakeTMUX = '/tmp/muster-e2e-fake-tmux/default,1,0';
const mcpSecretFile = path.join(dir, 'mcp-secret');
const localMCP = `http://127.0.0.1:${pagesShareMirrorMCPPort}/mcp`;
const TITLE = 'Launch notes ' + stamp;
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';

const rec = (o: object) => JSON.stringify(o) + '\n';

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(240_000);
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, pagesSharePort, {
    MUSTER_MIRROR: '1',
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_PAGES: '1',
    MUSTER_PUBLIC_URL: origin,
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x6b).toString('base64'),
    // A link key is the connector key, and that one needs its listener.
    MUSTER_CONNECT_PORT: String(pagesShareConnectPort),
    MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${pagesShareConnectPort}`,
  });
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  fs.writeFileSync(tr, rec({ type: 'user', message: { role: 'user', content: 'make a page for the client' } }));
  const windows = path.join(dir, 'windows');
  fs.writeFileSync(windows, `work|@1|0|1|claude|${Math.floor(Date.now() / 1000) - 60}|${tr}\n`);
  mirrorProc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
      MUSTER_MIRROR_MCP_LISTEN: `127.0.0.1:${pagesShareMirrorMCPPort}`,
      MUSTER_MIRROR_MCP_SECRET_FILE: mcpSecretFile,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  mirrorProc.stdout?.on('data', (d) => (mirrorLog += d));
  mirrorProc.stderr?.on('data', (d) => (mirrorLog += d));
});

test.afterAll(async ({ browser }) => {
  // 🔴 THE OPT-IN IS THE OWNER'S ROW IN THE LEG'S SHARED DATABASE (user 1's,
  // every spec's owner): turned off again here, or a spec after this one —
  // pages.spec.ts, the ask flow — would find its agent sharing at once.
  if (server) {
    const ctx = await browser.newContext({ baseURL: origin });
    try {
      const page = await ctx.newPage();
      await signIn(page, origin);
      const r = await page.request.post(origin + '/account/pages/agent-share', { form: { on: '0' }, headers: { Origin: origin }, maxRedirects: 0 });
      expect(r.status(), 'the opt-in was turned off again').toBe(303);
    } finally {
      await ctx.close();
    }
  }
  if (mirrorProc && mirrorProc.exitCode === null) {
    const done = new Promise((r) => mirrorProc!.once('exit', r));
    mirrorProc.kill('SIGTERM');
    await done;
  }
  await server?.stop();
  fs.rmSync(dir, { recursive: true, force: true });
});

type ToolResult = { isError: boolean; text: string; body: Record<string, unknown> };

// call is one tools/call through the host's loopback endpoint as the pane's
// session (pages.spec.ts's).
async function call(name: string, args: Record<string, unknown>): Promise<ToolResult> {
  let last = '';
  for (let i = 0; i < 60; i++) {
    if (fs.existsSync(mcpSecretFile)) {
      const secret = fs.readFileSync(mcpSecretFile, 'utf8').trim();
      const r = await fetch(localMCP, {
        method: 'POST',
        headers: await withMCPSession(localMCP, { 'Content-Type': 'application/json', Authorization: 'Bearer ' + secret, 'X-Muster-Pane': '%e2e-work-@1', 'X-Muster-Tmux': fakeTMUX }),
        body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name, arguments: args } }),
      }).catch((e) => ({ status: 0, text: async () => String(e) }) as unknown as Response);
      last = await r.text();
      if (r.status === 200) {
        const j = JSON.parse(last) as { result?: { isError?: boolean; content: { text: string }[] }; error?: unknown };
        const text = j.result?.content?.[0]?.text ?? '';
        if (!(j.result?.isError && /lists no window running Claude session|no Claude Code session runs in pane/.test(text))) {
          expect(j.error, last).toBeUndefined();
          return { isError: !!j.result?.isError, text, body: JSON.parse(text) as Record<string, unknown> };
        }
      }
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`the host's MCP endpoint never answered ${name}: ${last}\n${mirrorLog}`);
}

// The page the agent publishes, and the transcript lines its call leaves: the
// call (mcp__muster__page_create, as the CLI names an MCP tool) and its result.
let pageID = 0;
async function publish(): Promise<void> {
  if (pageID) return;
  const created = await call('page_create', { title: TITLE, blocks: [{ type: 'markdown', text: '# Launch notes\n\nShipped today.' }] });
  expect(created.isError, created.text).toBe(false);
  pageID = created.body.page_id as number;
  const id = 'toolu_pshare_' + stamp;
  fs.appendFileSync(
    tr,
    rec({ type: 'assistant', message: { role: 'assistant', content: [{ type: 'tool_use', id, name: 'mcp__muster__page_create', input: { title: TITLE } }] } }) +
      rec({ type: 'user', message: { role: 'user', content: [{ type: 'tool_result', tool_use_id: id, content: [{ type: 'text', text: created.text }] }] } }),
  );
}

// openCard signs page in, opens the session's mirrored view and returns the
// page card the view drew.
async function openCard(page: Page) {
  await signIn(page, origin);
  await page.goto(origin + `/mirror/view?host=${encodeURIComponent(host)}&session=work&window=%401`);
  const card = page.locator(`[data-page-card][data-page-id="${pageID}"]`);
  await expect(card, `the page card is drawn (mirror log:\n${mirrorLog})`).toBeVisible({ timeout: 20_000 });
  await expect(card.locator('[data-page-card-title]')).toHaveText(TITLE);
  await expect(card.getByRole('link', { name: `Open “${TITLE}”` })).toHaveAttribute('href', `/pages/${pageID}/view`);
  return card;
}

const linkRE = new RegExp(`^${origin}/s/[A-Za-z0-9_-]{43}$`);
let sharedURL = '';

// strangerSees opens url in a fresh, signed-out context and sees the page.
async function strangerSees(browser: Browser, url: string) {
  const stranger = await browser.newContext({ viewport: { width: 390, height: 844 } });
  try {
    const sp = await stranger.newPage();
    const res = await sp.goto(url);
    expect(res?.status()).toBe(200);
    await expect(sp.locator('[data-page-title]')).toHaveText(TITLE);
    await expect(sp.frameLocator('iframe#page').locator('h1')).toHaveText('Launch notes');
  } finally {
    await stranger.close();
  }
}

test('desktop: Share on the page card copies the page link, the same one twice, and a stranger opens it', async ({ browser }) => {
  test.setTimeout(120_000);
  await publish();
  let ctx: BrowserContext | undefined;
  try {
    ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, permissions: ['clipboard-read', 'clipboard-write'], baseURL: origin });
    // A desktop browser with no share sheet: the copy path.
    await ctx.addInitScript(() => {
      try {
        delete (Navigator.prototype as unknown as { share?: unknown }).share;
      } catch {
        /* none to delete */
      }
    });
    const page = await ctx.newPage();
    const card = await openCard(page);
    const share = card.getByRole('button', { name: `Share “${TITLE}”` });
    await share.click();
    await expect(page.locator('[data-page-toast]').filter({ hasText: 'Link copied' })).toBeVisible();
    const first = await page.evaluate(() => navigator.clipboard.readText());
    expect(first).toMatch(linkRE);
    // A second Share: the SAME link (the page's one reusable link).
    await page.evaluate(() => navigator.clipboard.writeText('cleared'));
    await share.click();
    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(first);
    sharedURL = first;
    await strangerSees(browser, first);
  } finally {
    await ctx?.close();
  }
});

// THE PAGE TOOL'S ROW SAYS WHAT IT DID (jargon audit, the Pages filming
// blocker): the row above the page card reads "Made a page", not the raw
// mcp__muster__page_create; the raw name is in the opened row's ⓘ. On
// origin/main the row spells the raw name.
test('phone: the page tool row above the card says "Made a page"; its ⓘ names the exact step', async ({ browser }, testInfo) => {
  test.setTimeout(120_000);
  await publish();
  const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, baseURL: origin });
  try {
    const page = await ctx.newPage();
    await openCard(page);
    const log = page.locator('#mv-log');
    await page.screenshot({ path: testInfo.outputPath('page-tool-row.png') });
    const row = log.locator('[data-tool-row]').filter({ has: page.locator('[data-tool-label]') });
    await expect(row).toHaveCount(1);
    await expect(row.locator('summary [data-tool-label]')).toHaveText('Made a page');
    expect(await log.innerText(), "the chat's visible text spells the raw tool name").not.toContain('mcp__muster__');
    await row.locator('summary').click();
    await row.getByRole('button', { name: "The step's exact name" }).click();
    const sheet = page.locator('[data-info-sheet]:popover-open');
    await expect(sheet).toBeVisible();
    await expect(sheet.locator('[data-info-code]')).toHaveText('mcp__muster__page_create');
    await page.screenshot({ path: testInfo.outputPath('page-tool-row-info.png') });
    const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(sw, 'nothing overflows 390px').toBeLessThanOrEqual(cw);
  } finally {
    await ctx.close();
  }
});

test('phone: Share opens the native share sheet with the title and the same link', async ({ browser }) => {
  test.setTimeout(120_000);
  await publish();
  let ctx: BrowserContext | undefined;
  try {
    ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, baseURL: origin });
    // The share sheet, stubbed: what the page asked it to share, and whether
    // it was asked INSIDE the tap's click dispatch (inClick: set by a capture
    // listener on window before any other, cleared by a bubble listener on
    // window after the page's own) — not after an await. A call that the
    // user CANCELS (AbortError) must say nothing and copy nothing.
    await ctx.addInitScript(() => {
      const w = window as unknown as { __shared: unknown[]; __cancelNext: boolean; __inClick: boolean };
      w.__shared = [];
      w.__cancelNext = false;
      w.__inClick = false;
      window.addEventListener('click', () => (w.__inClick = true), true);
      window.addEventListener('click', () => (w.__inClick = false));
      Object.defineProperty(Navigator.prototype, 'share', {
        configurable: true,
        value: (d: Record<string, unknown>) => {
          w.__shared.push({ ...d, inClick: w.__inClick });
          if (w.__cancelNext) {
            w.__cancelNext = false;
            return Promise.reject(new DOMException('cancelled', 'AbortError'));
          }
          return Promise.resolve();
        },
      });
      Object.defineProperty(Navigator.prototype, 'canShare', { configurable: true, value: () => true });
    });
    const page = await ctx.newPage();
    const card = await openCard(page);
    const share = card.getByRole('button', { name: `Share “${TITLE}”` });
    // The card holds no address; the finger going down fetches it, so it is
    // on the button BEFORE the click.
    await expect(share).not.toHaveAttribute('data-share-url', /./);
    await share.dispatchEvent('pointerdown');
    await expect(share).toHaveAttribute('data-share-url', linkRE);
    await share.tap();
    await expect.poll(() => page.evaluate(() => (window as unknown as { __shared: unknown[] }).__shared.length)).toBe(1);
    const got = await page.evaluate(() => (window as unknown as { __shared: { title: string; url: string; inClick: boolean }[] }).__shared[0]);
    expect(got.title).toBe(TITLE);
    expect(got.url).toMatch(linkRE);
    expect(got.inClick, 'navigator.share was called inside the click, with no await before it').toBe(true);
    if (sharedURL) expect(got.url, 'the same link the desktop copied').toBe(sharedURL);
    // Cancelled: silence (no toast).
    await page.evaluate(() => ((window as unknown as { __cancelNext: boolean }).__cancelNext = true));
    await share.tap();
    await expect.poll(() => page.evaluate(() => (window as unknown as { __shared: unknown[] }).__shared.length)).toBe(2);
    await page.waitForTimeout(300);
    await expect(page.locator('[data-page-toast]')).toHaveCount(0);
    await strangerSees(browser, got.url);
    sharedURL = got.url;
  } finally {
    await ctx?.close();
  }
});

// THE PAGE'S OWN SCREEN, ON A PHONE: no address in its top layer — Copy link
// puts the page's exact link on the clipboard, and the ⓘ beside it shows that
// same link (with its own Copy). A stranger opens what was copied.
test('phone: the page screen’s Copy link copies the exact link, and its ⓘ shows it', async ({ browser }) => {
  test.setTimeout(120_000);
  await publish();
  let ctx: BrowserContext | undefined;
  try {
    ctx = await browser.newContext({
      viewport: { width: 390, height: 844 },
      isMobile: true,
      hasTouch: true,
      permissions: ['clipboard-read', 'clipboard-write'],
      baseURL: origin,
    });
    // A phone's share sheet (stubbed: Copy link is drawn only where one exists).
    await ctx.addInitScript(() => {
      Object.defineProperty(Navigator.prototype, 'share', { configurable: true, value: () => Promise.resolve() });
    });
    const page = await ctx.newPage();
    await signIn(page, origin);
    // The page starts with NO link: its ⓘ is hidden until a tap makes one.
    const revoked = await page.request.post(origin + `/pages/${pageID}/share/revoke`, { headers: { Origin: origin, Accept: 'application/json' } });
    expect(revoked.status()).toBe(200);
    await page.goto(origin + `/pages/${pageID}`);
    const box = page.locator('[data-page-share-box]');
    await expect(box).toBeVisible();
    const info = page.getByRole('button', { name: "This page's link" });
    await expect(info).toBeHidden();
    // The top layer spells no address: the only place it is is the ⓘ.
    await expect(box.locator('input')).toHaveCount(0);
    await page.evaluate(() => navigator.clipboard.writeText('cleared'));
    await page.getByRole('button', { name: `Copy the link to “${TITLE}”` }).tap();
    await expect(page.locator('[data-page-toast]').filter({ hasText: 'Link copied' })).toBeVisible();
    const copied = await page.evaluate(() => navigator.clipboard.readText());
    expect(copied).toMatch(linkRE);
    // The page's one link, as Share's own request answers it now.
    const share = await page.request.post(origin + `/pages/${pageID}/share`, { headers: { Origin: origin, Accept: 'application/json' } });
    expect((await share.json()).url, 'the same link Share hands out').toBe(copied);
    await expect(page.locator('[data-testid="page-detail"]')).not.toContainText('/s/', { useInnerText: true });
    // The ⓘ appeared with that link, with no reload, and shows exactly it.
    const sheet = page.locator(`#page-link-${pageID}`);
    await expect(sheet).toBeHidden();
    await info.tap();
    await expect(sheet).toBeVisible();
    await expect(sheet.locator('[data-info-code]')).toHaveText(copied);
    // Its Copy copies the code as it reads (no second copy in an attribute).
    await page.evaluate(() => navigator.clipboard.writeText('cleared'));
    const copy = sheet.locator('[data-info-copy]');
    await copy.tap();
    await expect(copy).toHaveText('Copied');
    expect(await page.evaluate(() => navigator.clipboard.readText()), 'the ⓘ copies the link').toBe(copied);
    await strangerSees(browser, copied);
    sharedURL = copied;
  } finally {
    await ctx?.close();
  }
});

test('an owner who opts in on Account: the agent’s share request is live at once and returns the page’s same link', async ({ browser }) => {
  await publish();
  // Before the opt-in the agent is asked to wait for the owner.
  const asked = await call('page_share_request', { page_id: pageID });
  expect(asked.body.status, asked.text).toBe('pending');
  // The owner turns it on, on the Account page.
  const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, baseURL: origin });
  try {
    const page = await ctx.newPage();
    await signIn(page, origin);
    await page.goto(origin + '/account#pages-agent-share');
    const sw = page.locator('#pages-agent-share');
    await expect(sw).toHaveAttribute('data-state', 'off');
    await page.locator('#pages-agent-share-toggle').click();
    await expect(page.locator('#pages-agent-share')).toHaveAttribute('data-state', 'on');
  } finally {
    await ctx.close();
  }
  const req = await call('page_share_request', { page_id: pageID, reason: 'for the client' });
  expect(req.isError, req.text).toBe(false);
  expect(req.body.status).toBe('live');
  expect(req.body.ask_id).toBeUndefined();
  expect(req.body.url).toMatch(linkRE);
  if (sharedURL) expect(req.body.url, "the owner's Share link").toBe(sharedURL);
  const again = await call('page_share_request', { page_id: pageID });
  expect(again.body.url).toBe(req.body.url);
});
