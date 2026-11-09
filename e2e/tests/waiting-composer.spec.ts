import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, repoRoot, waitingComposerConnectPort, waitingComposerPort } from '../lib/env';
import { cc, MirrorRig, mkchat } from '../lib/mirrorhost';

// THREE PHONE-FIRST ASKS, on a real muster-server and a real muster-mirror
// (`-tags e2emirror`, tmux faked by files) and an agent asking over the real
// /mcp door:
//
//   1. the ☰ button that opens the sidebar carries a badge counting what waits
//      on the operator — the sidebar rows' own "waiting" (listEntry.waiting) —
//      shown when an ask arrives and gone when it is answered; on the Rendered
//      view its first count is drawn with the page, with no read of its own;
//   2. the command palette (the bar's magnifier) marks a waiting session with
//      the SIDEBAR's own mark, and no other;
//   3. the paperclip is on screen while the message box is collapsed, and
//      tapping it opens the file chooser without focusing the box (no
//      keyboard).
//
// Each at a phone (390×844, touch) and a desktop (1440×900). The ⋯ after Send
// is agents-switcher.spec's and session-tags.spec's (they own that row); the
// upload itself is uploads.spec's (the paperclip's change is CSS only).
//
// 🔴 FIXTURES ARE PAIRWISE DISTINCT: one agent waits (an open ask), one agent
// does not (a plain chat), and two mirrored windows do not — so a mark drawn
// on every row, on none, or on the wrong kind of entry fails.

test.use({ storageState: { cookies: [], origins: [] } });

const connectURL = `http://127.0.0.1:${waitingComposerConnectPort}`;
const stamp = String(Date.now());
const ASKER = `e2e-wc-asker-${stamp}`;
const ASKER_TOKEN = `e2e-wc-asker-token-not-a-real-one-${stamp}`;
const QUIET = `e2e-wc-quiet-${stamp}`;

const rig = new MirrorRig(
  waitingComposerPort,
  'e2e-wc',
  {
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x5c).toString('base64'),
    MUSTER_CONNECT_PORT: String(waitingComposerConnectPort),
    MUSTER_AGENT_CONNECT_URL: connectURL,
    // Attachments ON, so the composer draws the paperclip — against an S3 that
    // is never reached (port 1 refuses): nothing here uploads, the boot check
    // only warns (cmd/muster-server's checkUploadBucket).
    MUSTER_PUBLIC_URL: `http://localhost:${waitingComposerPort}`,
    MUSTER_S3_ENDPOINT: 'http://127.0.0.1:1',
    MUSTER_S3_BUCKET: 'muster-e2e',
    MUSTER_S3_ACCESS_KEY: 'e2e-wc-s3-access-not-real',
    MUSTER_S3_SECRET_KEY: 'e2e-wc-s3-secret-not-real',
    MUSTER_UPLOAD_SIGNING_KEY: 'e2e-wc-upload-signing-key-not-a-real-one-0123456789',
  },
  { decimalHost: true },
);
const origin = rig.origin;
// The windows: plum is the page's own session (never a palette result), kiwi
// another one that waits on nothing.
const WINDOWS: [string, string][] = [
  ['plum', '@1'],
  ['kiwi', '@2'],
];
const viewOf = (s: string) => `${origin}${rig.viewPath(s)}&window=${encodeURIComponent(WINDOWS.find((w) => w[0] === s)![1])}`;
let askerMade = false;

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.beforeAll(async () => {
  test.setTimeout(240_000);
  await rig.start();
  const now = Math.floor(Date.now() / 1000);
  const lines = WINDOWS.map(([s, w], i) => `${s}|${w}|1|1|claude|${now - 60 * (i + 1)}|${rig.transcript(cc.prompt(`the prompt of ${s}`) + cc.reply(`Answered in ${s}.`), 'w' + i)}`);
  await rig.startHost({ MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_E2E_WINDOWS_FILE: rig.transcript(lines.join('\n') + '\n', 'windows'), MUSTER_MIRROR_E2E_REFRESH: '300ms' });
  fix('agent', '1', ASKER, ASKER_TOKEN);
  askerMade = true;
  mkchat(QUIET, [{ role: 'user', content: 'hello' }]);
});

test.afterAll(async () => {
  try {
    mkchat(QUIET, null);
    if (askerMade) fix('cleanup', '1', ASKER);
  } finally {
    await rig.stop();
  }
});

async function mcp(name: string, args: Record<string, unknown>): Promise<string> {
  const r = await fetch(connectURL + '/mcp', {
    method: 'POST',
    headers: { 'X-Muster-Token': ASKER_TOKEN, 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name, arguments: args } }),
  });
  expect(r.status, 'the /mcp door answered').toBe(200);
  const j = (await r.json()) as { result?: { content: { text: string }[]; isError?: boolean }; error?: unknown };
  expect(j.error, JSON.stringify(j.error)).toBeUndefined();
  expect(j.result!.isError, j.result!.content[0].text).toBeFalsy();
  return j.result!.content[0].text;
}

// ask raises an open ask from ASKER (the agent then waits on the operator).
async function ask(question: string): Promise<number> {
  const id = (JSON.parse(await mcp('attention_ask', { question, options: ['yes', 'no'] })) as { id: number }).id;
  expect(id).toBeGreaterThan(0);
  return id;
}

// answer answers it the operator's way: the Asks page, its option tapped — in a
// page of its own, so the page under test sees the change arrive.
async function answer(page: Page, id: number) {
  const other = await page.context().newPage();
  await other.goto(origin + '/attention');
  await Promise.all([other.waitForURL(origin + '/attention'), other.locator(`#ask-${id}`).getByRole('button', { name: 'no' }).click()]);
  await expect(other.locator(`#ask-${id}`)).toHaveCount(0);
  await other.close();
}

// waitingRows is how many of the sidebar's rows are marked waiting, as the
// server draws them now (GET /ui/nav/agents): what the badge must count.
async function waitingRows(page: Page): Promise<number> {
  const html = await (await page.request.get(origin + '/ui/nav/agents')).text();
  return (html.match(/data-nav-wait=""/g) || []).length;
}

// The sidebar's mark for ASKER, as the server draws it on its row.
async function askerRowMark(page: Page): Promise<string> {
  const html = await (await page.request.get(origin + '/ui/nav/agents')).text();
  const row = html.slice(html.indexOf(`href="/agents/${ASKER}`));
  const m = row.slice(0, row.indexOf('</a>')).match(/<span data-nav-attention=""[\s\S]*?<\/span><\/span>/);
  return m ? m[0] : '';
}

const menuBtn = (page: Page) => page.locator('#sidebar-open');
const badge = (page: Page) => page.locator('#nav-waiting-badge');

async function noSidewaysScroll(page: Page, w: number) {
  const sw = await page.evaluate(() => document.documentElement.scrollWidth);
  expect(sw, 'no horizontal overflow').toBeLessThanOrEqual(w);
}

for (const vp of [
  { name: 'phone 390×844', w: 390, phone: true, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', w: 1440, phone: false, use: { viewport: { width: 1440, height: 900 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);
    const tap = async (page: Page, sel: string) => (vp.phone ? page.locator(sel).tap() : page.locator(sel).click());

    test('the ☰ badge counts what waits: shown when an ask arrives, gone when it is answered, live', async ({ page }) => {
      test.setTimeout(120_000);
      await rig.login(page);
      await rig.waitListed(page, 'kiwi');
      const base = await waitingRows(page);
      // A page whose ☰ is on screen at this size: the shell on a phone (the
      // rail is a drawer there), the Rendered view and then a Terminal view on
      // a desktop (their sidebar is an overlay at every width).
      // 🔴 ONLY THE NEW DOCUMENT'S READS COUNT. The ask makes the page ON
      // SCREEN refresh its badge (its events trigger), and that read can be
      // issued after page.reload()/goto() has started but before the new
      // document commits — the OLD page's read, aborted by the navigation
      // (CI, run 42550480035 attempt 1: one read, status -1, 21 ms after the
      // reload's document request). So each navigation under test arms
      // `fresh`, and the main frame's next commit (framenavigated, delivered
      // in order with the request events: the old page's reads before it, the
      // new document's after) clears what the old page read. Cleared at the
      // commit, never later, so a read the new page makes on load is counted.
      const reads: string[] = [];
      let fresh = false;
      page.on('request', (r) => {
        if (new URL(r.url()).pathname === '/ui/nav/waiting') reads.push(r.url());
      });
      page.on('framenavigated', (f) => {
        if (fresh && f === page.mainFrame()) {
          fresh = false;
          reads.length = 0;
        }
      });
      await page.goto(vp.phone ? origin + '/tasks' : viewOf('plum'));
      await expect(menuBtn(page)).toBeVisible();
      const box = (await menuBtn(page).boundingBox())!;
      expect(box.width, 'the ☰ is a 44px target').toBeGreaterThanOrEqual(44);
      expect(box.height, 'the ☰ is a 44px target').toBeGreaterThanOrEqual(44);
      await expect(badge(page)).toHaveAttribute('data-count', String(base));
      if (base === 0) await expect(badge(page), 'nothing waits: no badge').toBeHidden();

      const id = await ask(`Ship the ${stamp} build?`);
      expect(await waitingRows(page), 'POSITIVE CONTROL: the sidebar marks one more row waiting').toBe(base + 1);
      if (vp.phone) {
        // The shell holds the /events stream: the badge follows it, no reload.
        await expect(badge(page)).toHaveAttribute('data-count', String(base + 1), { timeout: 15_000 });
      } else {
        // The Rendered view's next load draws the count WITH the page (from
        // the switcher's Waiting view) — no read of its own. (Afterwards its
        // asks block's asks:changed refreshes it, on a queue change and on the
        // block's minute's tick.)
        fresh = true;
        await page.reload();
        expect(fresh, 'the reload committed (its reads are counted from there)').toBe(false);
        await expect(badge(page)).toHaveAttribute('data-count', String(base + 1));
        await page.waitForTimeout(500);
        expect(reads, 'the Rendered view drew the count with the page, no /ui/nav/waiting read').toEqual([]);
        // A Terminal view: also drawn with the page; nothing else on it
        // refreshes the badge but the drawer's open and its minute's poll.
        fresh = true;
        await page.goto(origin + rig.terminalPath('plum') + '&window=%401');
        expect(fresh, 'the Terminal view committed (its reads are counted from there)').toBe(false);
        await expect(menuBtn(page)).toBeVisible();
        await expect(badge(page)).toHaveAttribute('data-count', String(base + 1));
        await page.waitForTimeout(500);
        expect(reads, 'the Terminal view drew the count with the page, no /ui/nav/waiting read').toEqual([]);
      }
      await expect(badge(page)).toBeVisible();
      // On the button's corner, inside the screen.
      // 🔴 RETRIED, NOT READ ONCE: the badge is #nav-waiting-slot's innerHTML,
      // and the ask fires several refreshes (sse attention_queue.changed,
      // agent.changed, agents:changed, each its own /ui/nav/waiting read). Any
      // one swap replaces the element, so a boundingBox read after the
      // visibility check can land on a moment it is not laid out and return
      // null (CI, run 42513100375 attempt 1: "Cannot read properties of null
      // (reading 'x')" at 953ms). The geometry is asserted on one read of both
      // boxes, retried until it holds.
      await expect(async () => {
        const b = await badge(page).boundingBox();
        const m = await menuBtn(page).boundingBox();
        expect(b && m, 'both boxes are laid out').toBeTruthy();
        expect(b!.x + b!.width, 'the badge is on the ☰').toBeLessThanOrEqual(m!.x + m!.width + 1);
        expect(b!.y).toBeGreaterThanOrEqual(m!.y - 1);
      }).toPass({ timeout: 5_000 });
      await expect(menuBtn(page), 'the badge describes the button').toHaveAccessibleDescription(`${base + 1} waiting on you`);
      await noSidewaysScroll(page, vp.w);

      await answer(page, id);
      expect(await waitingRows(page)).toBe(base);
      if (vp.phone) {
        await expect(badge(page), 'answered: the badge follows, live').toHaveAttribute('data-count', String(base), { timeout: 15_000 });
      } else {
        // CONTROL: on the Terminal view nothing refreshes the badge by itself
        // within the minute — it still says what the page drew …
        await page.waitForTimeout(1500);
        await expect(badge(page), 'nothing but the open refreshes the Terminal view’s badge').toHaveAttribute('data-count', String(base + 1));
        expect(reads, 'no read before the open').toEqual([]);
        // … and opening the sidebar refreshes it (with the rows it opens on).
        await tap(page, '#sidebar-open');
        await expect(badge(page), 'answered: the open refreshes it').toHaveAttribute('data-count', String(base), { timeout: 15_000 });
        expect(reads.length, 'the open is a /ui/nav/waiting read').toBeGreaterThanOrEqual(1);
      }
      if (base === 0) await expect(badge(page)).toBeHidden();
    });

    test('the palette marks a waiting session with the sidebar’s mark, and no other', async ({ page }) => {
      test.setTimeout(120_000);
      await rig.login(page);
      await rig.waitListed(page, 'kiwi');
      const id = await ask(`Rotate the ${stamp} key?`);
      try {
        const want = await askerRowMark(page);
        expect(want, 'POSITIVE CONTROL: the sidebar draws the asker’s mark').toContain('data-kind="ask"');
        await page.goto(viewOf('plum'));
        await tap(page, '#switcher-search-open');
        await expect(page.locator('#switcher-search')).toBeVisible();
        const opts = page.locator('#switcher-search-results [role="option"]');
        const row = (href: string) => page.locator(`#switcher-search-results [role="option"][data-href^="${href}"]`);
        const asker = row(`/agents/${ASKER}`);
        const quiet = row(`/agents/${QUIET}`);
        const kiwi = opts.filter({ hasText: 'kiwi' });
        await expect(asker).toHaveCount(1);
        await expect(quiet).toHaveCount(1);
        await expect(kiwi).toHaveCount(1);
        // The waiting one: the sidebar's own mark, byte for byte.
        await expect(asker.locator('[data-palette-wait] [data-nav-attention][data-kind="ask"]')).toBeVisible();
        // (Both as the browser serialises them: the server escapes ' as &#39;.)
        const parsed = await page.evaluate((w) => { const t = document.createElement('template'); t.innerHTML = w; return t.innerHTML; }, want);
        expect(await asker.locator('[data-palette-wait]').innerHTML()).toBe(parsed);
        // The others: none.
        await expect(quiet.locator('[data-palette-wait]')).toHaveCount(0);
        await expect(kiwi.locator('[data-palette-wait]')).toHaveCount(0);
        // The label is still the row's first line, and the mark inside the box.
        expect(await asker.evaluate((li) => (li.firstElementChild as HTMLElement).textContent)).toBe(ASKER);
        // Retried on one read of both boxes, as the ☰ badge's above: the mark is
        // drawn from the switcher's live entries, which the same events refresh.
        await expect(async () => {
          const r = await asker.boundingBox();
          const k = await asker.locator('[data-palette-wait]').boundingBox();
          expect(r && k, 'both boxes are laid out').toBeTruthy();
          expect(k!.x + k!.width).toBeLessThanOrEqual(r!.x + r!.width);
        }).toPass({ timeout: 5_000 });
        await noSidewaysScroll(page, vp.w);
      } finally {
        await answer(page, id);
      }
    });

    test('the paperclip is on screen with the box at rest; a tap opens the file chooser without focusing the box', async ({ page }) => {
      test.setTimeout(90_000);
      await rig.login(page);
      await rig.waitListed(page, 'kiwi');
      await page.goto(viewOf('plum'));
      const input = page.locator('#mv-input');
      const clip = page.locator('#mv-attach');
      await expect(input).toHaveCount(1);
      if (vp.phone) {
        // At rest on a touch screen the box is collapsed behind "Type a message".
        await expect(page.locator('#mv-text')).toBeVisible();
        await expect(input).toBeHidden();
        const c = (await clip.boundingBox())!;
        expect(c.width, 'a 44px target at rest').toBeGreaterThanOrEqual(44);
        expect(c.height, 'a 44px target at rest').toBeGreaterThanOrEqual(44);
      }
      await expect(clip, 'the paperclip is on screen without opening the box').toBeVisible();
      await expect(input).not.toBeFocused();
      const [chooser] = await Promise.all([page.waitForEvent('filechooser'), tap(page, '#mv-attach')]);
      expect(chooser.isMultiple()).toBe(true);
      await expect(input, 'the tap does not focus the box (no keyboard)').not.toBeFocused();
      if (vp.phone) await expect(input, 'the box stays collapsed').toBeHidden();
      await noSidewaysScroll(page, vp.w);
    });
  });
}
