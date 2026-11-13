import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { BrowserContext, Frame, Page, Route } from '@playwright/test';
import { revealBox } from '../lib/composer';
import { test, expect } from '../lib/fixtures';
import { dsn, previewComposerPort, previewComposerSttPort, repoRoot } from '../lib/env';
import { cc, MirrorRig } from '../lib/mirrorhost';

// THE MESSAGE BAR STAYS USABLE WHILE THE PREVIEW PANEL IS OPEN
// (internal/ui/plugin_chip.go; operator ask: "chat and send messages while I'm
// looking at the preview"). On the operator's paths, against a REAL
// muster-server (mirror plugin, plugins and MUSTER_PLUGIN_ORIGINS on) and a
// REAL interactive muster-mirror (`-tags e2emirror`) with a Claude Code window
// (@1) and a shell (@2):
//
//   - a phone: the panel ends at the bar's top, the bar stays live (typing,
//     Send, the mic, the paperclip, the `/` menu over the panel), and the
//     keyboard raises the bar and shrinks the panel;
//   - a desktop: the conversation and the bar end at the side panel's left
//     edge, Send and the replies in view;
//   - full screen in the Android app: the bar hidden, a faint button at the
//     bottom right shows it (the panel above it) and hides it again, and Exit
//     and Back restore the panel as before;
//   - the Terminal view, the same, in its own page and in the view switch's
//     frame;
//   - the new-reply dot while the panel covers the conversation;
//   - the open panel at rest shifts nothing.
//
// The plugin's entry page is PLAYED by page.route (as plugin-chip.spec.ts's,
// which holds the hand-off itself); this spec is about the page around it.
//
// What a send reached is read off the fake host: a paste into the Claude pane
// is appended to <transcript>.pasted, into any other pane to the paste file.
const ZONE = 'https://*.viewer.example.net';
const ENTRY = 'https://w.viewer.example.net/__muster/enter';
const OWNER = '1'; // the password-mode operator

const rig = new MirrorRig(previewComposerPort, 'e2e-pvc', {
  MUSTER_PLUGINS: '1',
  MUSTER_PLUGIN_ORIGINS: ZONE,
  // Drawn, so the mic and the paperclip are in the bar: nothing answers at
  // either address, and nothing here needs an answer.
  MUSTER_STT_URL: `http://127.0.0.1:${previewComposerSttPort}`,
  MUSTER_STT_TOKEN: 'e2e-stt-token-not-a-real-one',
  MUSTER_PUBLIC_URL: `http://localhost:${previewComposerPort}`,
  MUSTER_S3_ENDPOINT: `http://127.0.0.1:${previewComposerSttPort}`,
  MUSTER_S3_BUCKET: 'muster-e2e',
  MUSTER_S3_ACCESS_KEY: 'e2e-s3-access-not-real',
  MUSTER_S3_SECRET_KEY: 'e2e-s3-secret-not-real',
  MUSTER_UPLOAD_SIGNING_KEY: 'e2e-upload-signing-key-not-a-real-one-0123456789',
});
const origin = rig.origin;
const names: string[] = [];
let tr = '';
let tr3 = '';
let pasteFile = '';
let hostEnv: NodeJS.ProcessEnv = {};
// @3's conversation: more events than one batch of the wire carries (64,
// internal/mirror/wire.go MaxBatchEvents), so its redraw after a reconnect
// arrives in several batches.
const LONG_PAIRS = 45;

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();
const q = (win: string) => `host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent(win)}`;
const viewOf = (win: string) => `/mirror/view?${q(win)}`;
const termOf = (win: string) => `/mirror/terminal?${q(win)}`;
const read = (f: string) => (fs.existsSync(f) ? fs.readFileSync(f, 'utf8') : '');
let replies = 0;
// reply plays Claude Code answering in @1: a text record appended to its
// transcript, read by the host's real tailer.
const reply = (text: string) => {
  replies++;
  fs.appendFileSync(tr, cc.reply(text));
};

// The fake microphone, so the mic records (a launch option: the whole file's).
test.use({
  storageState: { cookies: [], origins: [] },
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] },
  permissions: ['microphone'],
});

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
  const now = Math.floor(Date.now() / 1000);
  tr = rig.transcript(cc.prompt('the prompt of the preview window') + cc.reply('Answered.'), 'one');
  let long = '';
  for (let i = 1; i <= LONG_PAIRS; i++) long += cc.prompt(`long prompt ${i}`) + cc.reply(`long reply ${i}`);
  tr3 = rig.transcript(long, 'long');
  const windows = rig.transcript(`work|@1|1|1|claude|${now - 60}|${tr}\nwork|@2|2|0|zsh|${now - 120}|\nwork|@3|3|0|claude|${now - 180}|${tr3}\n`, 'windows');
  const previews = path.join(rig.dir, 'previews-played');
  fs.writeFileSync(previews, '@1 1 0\n@2 1 0\n@3 1 0\n');
  pasteFile = path.join(rig.dir, 'pasted');
  hostEnv = {
    MUSTER_MIRROR_INTERACTIVE: '1',
    MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
    MUSTER_MIRROR_E2E_REFRESH: '300ms',
    MUSTER_MIRROR_E2E_PREVIEWS_FILE: previews,
    MUSTER_MIRROR_E2E_PASTE_FILE: pasteFile,
  };
  await rig.startHost(hostEnv);
});

test.afterAll(async () => {
  for (const n of names) fix('rmplugin', OWNER, n);
  await rig.stop();
});

// ready signs in, mints a viewer plugin through the real /plugins form (once
// per server), plays its entry page in this context, and waits until the
// host's report has reached muster for both windows.
let token = '';
async function ready(page: Page, context: BrowserContext): Promise<void> {
  await rig.login(page);
  await rig.waitListed(page, 'data-mirror-claude');
  if (!token) {
    const name = 'e2e-pvc-' + Date.now().toString(36);
    names.push(name);
    const form = new URLSearchParams({ name, label: 'Example viewer', data_scope: 'example', origin: ZONE, entry_url: ENTRY });
    form.append('capability', 'viewer');
    const r = await page.request.post(origin + '/plugins', {
      headers: { Origin: origin, 'Content-Type': 'application/x-www-form-urlencoded' },
      data: form.toString(),
    });
    const html = await r.text();
    expect(r.status(), html.slice(0, 400)).toBe(200);
    token = /id="minted-plugin-token" readonly value="(mpl_[A-Za-z0-9_-]+)"/.exec(html)?.[1] ?? '';
    expect(token, 'a plugin token was minted').toBeTruthy();
  }
  await context.route('https://*.viewer.example.net/**', async (route: Route) => {
    const code = new URL(route.request().url()).searchParams.get('muster_code') ?? '';
    const res = await fetch(origin + '/plugin/v1/handoffs/redeem', {
      method: 'POST',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ code }),
    });
    await route.fulfill({
      status: 200,
      headers: { 'Content-Type': 'text/html; charset=utf-8' },
      body: `<!doctype html><title>Fake plugin</title><body style="margin:0;background:#123"><h1 id="fake-redeem">${res.status}</h1></body>`,
    });
  });
  for (const w of ['@1', '@2']) {
    await expect
      .poll(async () => (await (await page.request.get(origin + `/ui/mirror/preview-button?${q(w)}`)).text()).includes('id="preview-toggle"'), {
        message: `the host's report for ${w} reached muster (muster-mirror log:\n${rig.log})`,
        timeout: 20_000,
      })
      .toBe(true);
  }
}

type Doc = Page | Frame;

// openRendered opens @1's Rendered view and waits until its bar can send.
async function openRendered(page: Page): Promise<void> {
  await page.goto(origin + viewOf('@1'));
  await expect(page.locator('#mv-input'), `the rendered view (log:\n${rig.log})`).toBeEnabled({ timeout: 20_000 });
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 20_000 });
}

// openPanel opens the panel from whichever Preview button is on screen and
// waits until the plugin's page has landed in it.
async function openPanel(doc: Doc): Promise<void> {
  await doc.locator('[data-preview-toggle]:visible').click();
  await expect(doc.locator('#preview-panel')).toBeVisible();
  await expect(doc.frameLocator('#preview-panel iframe').locator('#fake-redeem')).toHaveText('200');
}

// rect is an element's box, unrounded.
const rect = (doc: Doc, sel: string) =>
  doc.evaluate((s) => {
    const r = document.querySelector(s)!.getBoundingClientRect();
    return { x: r.x, y: r.y, w: r.width, h: r.height, right: r.right, bottom: r.bottom };
  }, sel);

// onTop is the id (or data-attribute name) of what a tap at sel's centre
// lands on: the element itself when nothing covers it.
const onTop = (doc: Doc, sel: string) =>
  doc.evaluate((s) => {
    const el = document.querySelector(s) as HTMLElement;
    const r = el.getBoundingClientRect();
    const hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
    return hit === el || (hit !== null && el.contains(hit)) ? 'itself' : (hit?.closest('[id]')?.id ?? hit?.tagName ?? 'nothing');
  }, sel);

// inert is whether sel is inside an inert subtree.
const inert = (doc: Doc, sel: string) => doc.evaluate((s) => !!document.querySelector(s)!.closest('[inert]'), sel);

// panelOnBar polls until the panel's bottom edge is the bar's top edge (±1px)
// and returns both.
async function panelOnBar(doc: Doc, why: string): Promise<{ panel: Awaited<ReturnType<typeof rect>>; bar: Awaited<ReturnType<typeof rect>> }> {
  await expect
    .poll(async () => Math.abs((await rect(doc, '#preview-panel')).bottom - (await rect(doc, '#mv-compose')).y), { message: `${why}: the panel ends at the bar's top` })
    .toBeLessThanOrEqual(1);
  return { panel: await rect(doc, '#preview-panel'), bar: await rect(doc, '#mv-compose') };
}

// rootFlags is which of the panel's layout attributes <html> carries.
const rootFlags = (doc: Doc) =>
  doc.evaluate(() => ['data-preview-bar', 'data-preview-nobar', 'data-preview-side'].filter((a) => document.documentElement.hasAttribute(a)));

const pasted = () => read(tr + '.pasted');

// sendFrom types text into the bar and presses Send.
async function sendFrom(doc: Doc, text: string, how: 'tap' | 'click'): Promise<void> {
  await doc.locator('#mv-input').fill(text);
  const send = doc.locator('#mv-send');
  if (how === 'tap') await send.tap();
  else await send.click();
}

// shiftsAtRest is the layout shift summed over `ms` with no input on the
// page, then — the positive control, so a zero is not an observer wired to
// nothing — after an unprovoked change made by script.
async function shiftsAtRest(page: Page, ms: number): Promise<{ rest: number; control: number }> {
  return page.evaluate(
    (ms) =>
      new Promise<{ rest: number; control: number }>((done) => {
        let sum = 0;
        const po = new PerformanceObserver((l) => {
          for (const e of l.getEntries() as unknown as { value: number; hadRecentInput: boolean }[]) if (!e.hadRecentInput) sum += e.value;
        });
        po.observe({ type: 'layout-shift' });
        setTimeout(() => {
          const rest = sum;
          (document.querySelector('header') as HTMLElement).style.paddingTop = '60px';
          setTimeout(() => {
            po.disconnect();
            (document.querySelector('header') as HTMLElement).style.paddingTop = '';
            done({ rest, control: sum - rest });
          }, 500);
        }, ms);
      }),
    ms,
  );
}

test.describe('a phone', () => {
  test.use({
    viewport: { width: 390, height: 844 },
    hasTouch: true,
    isMobile: true,
  });

  test('the Rendered view: the panel ends at the bar, which stays live — typing, Send, the mic, the paperclip and the / menu over the panel — the keyboard shrinks the panel, and a reply while it covers the chat sets the dot', async ({ page, context }) => {
    test.setTimeout(150_000);
    // An overlaying keyboard (iOS) is a visual viewport shorter than the
    // layout viewport: played by a visualViewport whose height the test sets
    // (installed before the page's scripts, as switcher-dock.spec's 13k).
    await page.addInitScript(() => {
      const real = window.visualViewport!;
      const fake = new EventTarget() as any;
      for (const k of ['width', 'offsetLeft', 'pageLeft', 'pageTop', 'scale']) Object.defineProperty(fake, k, { get: () => (real as any)[k] });
      Object.defineProperty(fake, 'height', { get: () => (window as any).__vvH ?? real.height });
      Object.defineProperty(fake, 'offsetTop', { get: () => 0 });
      real.addEventListener('resize', () => fake.dispatchEvent(new Event('resize')));
      Object.defineProperty(window, 'visualViewport', { value: fake, configurable: true });
    });
    await ready(page, context);
    await openRendered(page);
    const closedBar = await rect(page, '#mv-compose');
    expect(await rootFlags(page), 'nothing applies while the panel is shut').toEqual([]);

    await openPanel(page);
    // THE PANEL ENDS AT THE BAR'S TOP; the bar is where it was, in view,
    // not inert, and on top where it is tapped.
    const { panel, bar } = await panelOnBar(page, 'opened');
    expect([Math.round(panel.x), Math.round(panel.y), Math.round(panel.w)], 'the panel is the screen above the bar').toEqual([0, 0, 390]);
    expect(Math.abs(bar.bottom - 844), 'the bar is on the screen\'s bottom').toBeLessThanOrEqual(1);
    expect(Math.abs(bar.y - closedBar.y), 'the bar did not move when the panel opened').toBeLessThanOrEqual(1);
    await expect(page.locator('#mv-compose')).toBeVisible();
    expect(await inert(page, '#mv-send'), 'the bar (its Send) is not inert').toBe(false);
    for (const sel of ['#mv-text', '#mv-send', '#mv-dictate', '#mv-attach']) {
      expect(await onTop(page, sel), `${sel} is on top where it is tapped`).toBe('itself');
    }
    // What the panel covers stays inert (Tab cannot reach it).
    expect(await inert(page, '#mv-back'), 'the header\'s Home control behind the panel is inert').toBe(true);
    expect(await inert(page, '#mv-log'), 'the conversation behind the panel is inert').toBe(true);
    expect(await inert(page, '#preview-panel'), 'the panel is not').toBe(false);
    expect(await rootFlags(page)).toEqual(['data-preview-bar']);
    // AT REST the open panel shifts nothing (and the observer can see a shift).
    const s = await shiftsAtRest(page, 1500);
    expect(s.control, 'POSITIVE CONTROL: an unprovoked shift is measured').toBeGreaterThan(0);
    expect(s.rest, 'the open panel at rest shifts nothing').toBe(0);

    // TYPING AND SEND deliver, the panel staying open.
    await revealBox(page);
    expect(await onTop(page, '#mv-input'), 'the box is on top').toBe('itself');
    await panelOnBar(page, 'the box revealed (the bar grew)');
    await sendFrom(page, 'sent from under the preview', 'tap');
    await expect.poll(pasted, { message: 'the message reached the Claude pane' }).toContain('sent from under the preview');
    await expect(page.locator('#preview-panel')).toBeVisible();

    // THE / MENU opens over the panel, and is on top there.
    await page.locator('#mv-input').fill('');
    await page.locator('#mv-input').focus();
    await page.keyboard.type('/');
    const menu = page.locator('#mv-complete-box');
    await expect(menu).toBeVisible();
    const mb = await rect(page, '#mv-complete-box');
    expect(mb.y, 'the menu is above the bar, over the panel').toBeLessThan((await rect(page, '#mv-compose')).y);
    expect(await onTop(page, '#mv-complete-box'), 'the menu is over the panel').toBe('itself');
    await page.locator('#mv-complete [role="option"]').first().click();
    await expect(page.locator('#mv-input')).toHaveValue(/^\/\S+ $/);
    await page.locator('#mv-input').fill('');

    // THE MIC records from under the panel, its line over the panel.
    const mic = page.locator('#mv-dictate');
    await mic.tap();
    await expect(mic).toHaveAttribute('data-state', 'recording');
    await expect(page.locator('#mv-dictate-note')).toBeVisible();
    expect(await onTop(page, '#mv-dictate-note'), 'the mic\'s line is on top').toBe('itself');
    await panelOnBar(page, 'recording (the mic\'s line grew the bar)');
    await mic.tap();
    await expect(mic).not.toHaveAttribute('data-state', 'recording');

    // THE PAPERCLIP opens the file picker.
    await Promise.all([page.waitForEvent('filechooser'), page.locator('#mv-attach').tap()]);

    // THE KEYBOARD, overlaying the page (iOS): the bar rides on it, the panel
    // shrinks above it, no overlap.
    await page.locator('#mv-input').fill('');
    await page.evaluate(() => { (window as any).__vvH = 420; window.visualViewport!.dispatchEvent(new Event('resize')); });
    await expect.poll(async () => Math.round((await rect(page, '#mv-compose')).bottom), { message: 'the bar rides on the keyboard' }).toBe(420);
    const kb = await panelOnBar(page, 'an overlaying keyboard');
    expect(kb.panel.h, 'the panel shrank').toBeLessThan(panel.h - 250);
    await page.evaluate(() => { delete (window as any).__vvH; window.visualViewport!.dispatchEvent(new Event('resize')); });
    await expect.poll(async () => Math.round((await rect(page, '#mv-compose')).bottom)).toBe(844);
    await panelOnBar(page, 'the keyboard closed');
    // RESIZING the page (Android's interactive-widget=resizes-content): the
    // same.
    await page.setViewportSize({ width: 390, height: 500 });
    await expect.poll(async () => Math.round((await rect(page, '#mv-compose')).bottom), { message: 'the bar is on the shorter page\'s bottom' }).toBe(500);
    const resized = await panelOnBar(page, 'a resizing keyboard');
    expect(resized.panel.h).toBeLessThan(panel.h - 250);
    await page.setViewportSize({ width: 390, height: 844 });
    await panelOnBar(page, 'the page restored');

    // THE NEW-REPLY DOT: a reply while the panel covers the chat.
    const dot = page.locator('#preview-panel [data-preview-dot="header"]');
    await expect(dot).toBeHidden();
    const bubbles = page.locator('#mv-log [data-bubble="assistant"]');
    const before = await bubbles.count();
    reply('a reply while you look at the preview');
    await expect(bubbles, 'the reply arrived').toHaveCount(before + 1, { timeout: 20_000 });
    await expect(dot, 'the reply set the dot').toBeVisible();
    // Closing puts the chat in view: the dot clears; reopened, it is not back.
    await page.locator('#preview-panel [data-preview-close]').click();
    await expect(page.locator('#preview-panel')).toBeHidden();
    await expect(page.locator('#mv-log [data-bubble="assistant"]').last()).toContainText('a reply while you look at the preview');
    await openPanel(page);
    await expect(dot, 'the dot cleared when the chat was in view').toBeHidden();
    await page.locator('#preview-panel [data-preview-close]').click();

    // CLOSED: nothing inert, the layout as it was.
    expect(await rootFlags(page)).toEqual([]);
    expect(await inert(page, '#mv-back')).toBe(false);
    expect(await page.locator('#preview-panel').evaluate((p) => (p as HTMLElement).style.bottom)).toBe('');
  });

  // 🔴 AN OVERLAY NESTED IN A COVERED ELEMENT STAYS LIVE. The bar's search
  // button opens the command palette, a popover that sits inside
  // #sidebar-root (whose panel and backdrop the preview covers): it must
  // take focus and typing, while what it sits beside stays inert.
  test('the command palette opened from the bar under the panel takes focus and typing', async ({ page, context }) => {
    test.setTimeout(120_000);
    await ready(page, context);
    await openRendered(page);
    await openPanel(page);
    await panelOnBar(page, 'opened');
    const open = page.locator('#switcher-search-open');
    expect(await onTop(page, '#switcher-search-open'), 'the bar\'s search button is on top').toBe('itself');
    await open.tap();
    const input = page.locator('#switcher-search-input');
    await expect(input).toBeVisible();
    expect(await inert(page, '#switcher-search-input'), 'the palette is not inert').toBe(false);
    await expect(input, 'focus is in the palette\'s input').toBeFocused();
    await page.keyboard.type('work');
    await expect(input, 'typing reached the palette').toHaveValue('work');
    // What the panel covers in the same box stays inert.
    expect(await inert(page, '#sidebar'), 'the sidebar beside the palette stays inert').toBe(true);
    expect(await inert(page, '#mv-back'), 'the header\'s Home control stays inert').toBe(true);
    await page.keyboard.press('Escape');
    await expect(input).toBeHidden();
    await expect(page.locator('#preview-panel'), 'Escape was the palette\'s').toBeVisible();
  });

  // 🔴 A RECONNECT'S REDRAW IS NOT A REPLY. The host restarts, so the view's
  // log is reset and redrawn — in several batches, each in its own task
  // (more than 64 events). No dot; a reply after it sets one. The page's
  // incoming socket messages are recorded, so the multi-batch redraw is a
  // measured fact of this run, not an assumption.
  test('a reconnect that redraws the log in several batches sets no dot, and a new reply after it does', async ({ page, context }) => {
    test.setTimeout(150_000);
    await page.addInitScript(() => {
      const w = window as unknown as { __msgs: { reset: boolean; ops: number }[]; __socks: WebSocket[] };
      w.__msgs = [];
      w.__socks = [];
      const C = window.WebSocket;
      const N = function (this: unknown, url: string | URL, p?: string | string[]) {
        // A resume the host cannot take starts over with a Reset backfill
        // (internal/mirrorhost/transcript.go): played by the reconnect asking
        // for none.
        if ((w as any).__noResume) url = String(url).replace(/&gen=[^&]*&offset=[^&]*/, '');
        const o = p === undefined ? new C(url) : new C(url, p);
        w.__socks.push(o);
        o.addEventListener('message', (ev) => {
          try {
            const m = JSON.parse(String((ev as MessageEvent).data));
            if (m && (m.ops || m.reset)) w.__msgs.push({ reset: !!m.reset, ops: (m.ops || []).length });
          } catch (e) { /* not JSON */ }
        });
        return o;
      } as unknown as typeof WebSocket;
      N.prototype = C.prototype;
      for (const k of ['CONNECTING', 'OPEN', 'CLOSING', 'CLOSED'] as const) (N as any)[k] = (C as any)[k];
      window.WebSocket = N;
    });
    await ready(page, context);
    await page.goto(origin + viewOf('@3'));
    const bubbles = page.locator('#mv-log [data-bubble="assistant"]');
    await expect(bubbles, `@3's conversation is drawn (log:\n${rig.log})`).toHaveCount(LONG_PAIRS, { timeout: 20_000 });
    await openPanel(page);
    const dot = page.locator('#preview-panel [data-preview-dot="header"]');
    await expect(dot).toBeHidden();
    const mark = await page.evaluate(() => (window as unknown as { __msgs: unknown[] }).__msgs.length);

    await page.evaluate(() => { (window as any).__noResume = true; (window as any).__socks.forEach((s: WebSocket) => s.close()); });
    // The redraw: a reset, then more batches after it.
    await expect
      .poll(
        async () => {
          const ms = (await page.evaluate(() => (window as unknown as { __msgs: { reset: boolean; ops: number }[] }).__msgs)).slice(mark);
          // The redraw's first batch is a full one (64 events: the wire's
          // cap), so the rest of it follows in batches of their own.
          const at = ms.findIndex((m) => m.reset && m.ops >= 60);
          return at < 0 ? -1 : ms.slice(at + 1).filter((m) => m.ops > 0).length;
        },
        { message: `a reset arrived and more batches after it (log:\n${rig.log})`, timeout: 30_000 },
      )
      .toBeGreaterThan(0);
    await expect(bubbles, 'redrawn whole').toHaveCount(LONG_PAIRS, { timeout: 20_000 });
    await page.waitForTimeout(500);
    await expect(dot, 'the redraw set no dot').toBeHidden();
    await expect(page.locator('#preview-panel')).not.toHaveAttribute('data-preview-unread', '');

    // A REAL reply after it does.
    fs.appendFileSync(tr3, cc.reply('a reply after the reconnect'));
    await expect(bubbles).toHaveCount(LONG_PAIRS + 1, { timeout: 20_000 });
    await expect(dot, 'a new reply set the dot').toBeVisible();
  });

  test('the Terminal view: its own page and the view switch\'s frame keep the bar live under the panel, the panel ends at its top and follows the keyboard', async ({ page, context }) => {
    test.setTimeout(150_000);
    await ready(page, context);
    // A window with no Rendered view opens on its Terminal view.
    await page.goto(origin + viewOf('@2'));
    expect(new URL(page.url()).pathname).toBe('/mirror/terminal');
    await expect(page.locator('#mv-input')).toBeEnabled({ timeout: 20_000 });
    await openPanel(page);
    const { panel } = await panelOnBar(page, 'the Terminal view');
    expect(await inert(page, '#mv-send'), 'the bar (its Send) is not inert').toBe(false);
    expect(await inert(page, '#term-back'), 'the header\'s Back control behind the panel is inert').toBe(true);
    for (const sel of ['#mv-input', '#mv-send']) expect(await onTop(page, sel), `${sel} is on top`).toBe('itself');
    expect(await rootFlags(page)).toEqual(['data-preview-bar']);
    await sendFrom(page, 'typed into the shell beside the preview', 'tap');
    await expect.poll(() => read(pasteFile), { message: 'the paste reached the shell\'s pane' }).toContain('%e2e-work-@2 typed into the shell beside the preview');
    // The keyboard, resizing the page.
    await page.setViewportSize({ width: 390, height: 500 });
    await expect.poll(async () => Math.round((await rect(page, '#mv-compose')).bottom), { message: 'the bar is on the shorter page\'s bottom' }).toBe(500);
    expect((await panelOnBar(page, 'a resizing keyboard')).panel.h).toBeLessThan(panel.h - 250);
    await page.setViewportSize({ width: 390, height: 844 });
    await panelOnBar(page, 'the page restored');
    await page.locator('#preview-panel [data-preview-close]').click();
    expect(await rootFlags(page)).toEqual([]);

    // IN THE VIEW SWITCH'S FRAME: the Terminal view opened from @1's
    // Rendered view keeps its own panel and bar, laid out the same.
    await openRendered(page);
    await page.locator('#view-terminal').tap();
    const el = page.locator('iframe[data-muster-view]');
    await expect(el).toBeVisible();
    const frame = (await el.elementHandle())!.contentFrame();
    const f = (await frame)!;
    await expect(f.locator('#mv-input')).toBeEnabled({ timeout: 20_000 });
    await openPanel(f);
    await panelOnBar(f, 'in the frame');
    expect(await inert(f, '#mv-send'), 'the frame\'s bar is not inert').toBe(false);
    expect(await inert(f, '#term-back'), 'the frame\'s header controls are inert').toBe(true);
    for (const sel of ['#mv-input', '#mv-send']) expect(await onTop(f, sel), `${sel} is on top in the frame`).toBe('itself');
    expect(await rootFlags(f)).toEqual(['data-preview-bar']);
    await sendFrom(f, 'typed from the framed terminal', 'tap');
    // @1's active pane is its Claude pane: the paste goes where a Claude
    // paste goes.
    await expect.poll(pasted, { message: 'the framed Terminal view\'s paste reached @1' }).toContain('typed from the framed terminal');
  });
});

test.describe('a desktop', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

  test('the Rendered view: the conversation and the bar end at the side panel\'s edge, Send and a reply stay in view, no dot, and closing restores the widths', async ({ page, context }) => {
    test.setTimeout(150_000);
    await ready(page, context);
    await openRendered(page);
    const widths = async () => ({ bar: await rect(page, '#mv-compose'), log: await rect(page, '#mv-log'), header: await rect(page, 'header') });
    const before = await widths();
    expect(Math.round(before.bar.right)).toBe(1280);

    await openPanel(page);
    const panel = await rect(page, '#preview-panel');
    expect([Math.round(panel.x), Math.round(panel.y), Math.round(panel.w), Math.round(panel.h)], 'a full-height side panel, min(40rem,60vw) wide').toEqual([640, 0, 640, 800]);
    await expect
      .poll(async () => Math.abs((await rect(page, '#mv-compose')).right - panel.x), { message: 'the bar ends at the panel\'s left edge' })
      .toBeLessThanOrEqual(1);
    const open = await widths();
    expect(Math.abs(open.log.right - panel.x), 'the conversation ends at the panel\'s left edge').toBeLessThanOrEqual(1);
    expect(open.header.right, 'the header too').toBeLessThanOrEqual(panel.x + 1);
    expect(await onTop(page, '#mv-send'), 'Send is not covered').toBe('itself');
    expect(await onTop(page, '#mv-input'), 'nor is the box').toBe('itself');
    expect(await inert(page, '#mv-log'), 'nothing is inert beside the panel').toBe(false);
    expect(await rootFlags(page)).toEqual(['data-preview-side']);
    await sendFrom(page, 'sent beside the side panel', 'click');
    await expect.poll(pasted).toContain('sent beside the side panel');

    // A REPLY is drawn in view beside the panel; no dot (the chat is visible).
    const bubbles = page.locator('#mv-log [data-bubble="assistant"]');
    const n = await bubbles.count();
    reply('a reply beside the side panel');
    await expect(bubbles).toHaveCount(n + 1, { timeout: 20_000 });
    const last = bubbles.last();
    await expect(last).toContainText('a reply beside the side panel');
    await expect(last).toBeInViewport();
    await last.evaluate((b) => (b.id = 'pvc-last-reply'));
    expect(await onTop(page, '#pvc-last-reply'), 'the reply is not covered').toBe('itself');
    expect((await last.boundingBox())!.x + (await last.boundingBox())!.width, 'the reply is left of the panel').toBeLessThanOrEqual(panel.x);
    await page.waitForTimeout(300);
    await expect(page.locator('#preview-panel [data-preview-dot="header"]'), 'no dot beside the page').toBeHidden();

    // AT REST the open panel shifts nothing (and the observer can see a shift).
    const s = await shiftsAtRest(page, 1500);
    expect(s.control, 'POSITIVE CONTROL: an unprovoked shift is measured').toBeGreaterThan(0);
    expect(s.rest, 'the open panel at rest shifts nothing').toBe(0);

    await page.locator('#preview-panel [data-preview-close]').click();
    expect(await rootFlags(page)).toEqual([]);
    const after = await widths();
    expect([after.bar.right, after.log.right, after.header.right].map(Math.round), 'closing restored the widths').toEqual(
      [before.bar.right, before.log.right, before.header.right].map(Math.round),
    );
  });

  test('the Terminal view: the bar ends at the side panel\'s edge, Send not covered, and closing restores it', async ({ page, context }) => {
    test.setTimeout(120_000);
    await ready(page, context);
    await page.goto(origin + termOf('@2'));
    await expect(page.locator('#mv-input')).toBeEnabled({ timeout: 20_000 });
    const before = await rect(page, '#mv-compose');
    expect(Math.round(before.right)).toBe(1280);
    await openPanel(page);
    const panel = await rect(page, '#preview-panel');
    await expect.poll(async () => Math.abs((await rect(page, '#mv-compose')).right - panel.x), { message: 'the bar ends at the panel\'s left edge' }).toBeLessThanOrEqual(1);
    expect((await rect(page, '#term-scroll')).right, 'the terminal ends at it too').toBeLessThanOrEqual(panel.x + 1);
    expect(await onTop(page, '#mv-send'), 'Send is not covered').toBe('itself');
    expect(await rootFlags(page)).toEqual(['data-preview-side']);
    await sendFrom(page, 'typed beside the side panel', 'click');
    await expect.poll(() => read(pasteFile)).toContain('%e2e-work-@2 typed beside the side panel');
    await page.locator('#preview-panel [data-preview-close]').click();
    expect(Math.round((await rect(page, '#mv-compose')).right), 'closing restored the width').toBe(1280);
  });
});

// IN THE ANDROID APP (window.musterApp, played by an init script that records
// what is posted to it, as plugin-chip.spec.ts's): full screen hides the bar,
// and the faint button at the bottom right shows it and hides it again.
test.describe('full screen in the Android app', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('the bar is hidden, the bottom-right button shows it with the preview above and hides it again, a reply sets the dot on it, and Exit and Back restore the panel', async ({ page, context }) => {
    test.setTimeout(150_000);
    await page.addInitScript(() => {
      const w = window as unknown as { __appMsgs: string[]; musterApp: { postMessage(m: string): void } };
      w.__appMsgs = [];
      w.musterApp = { postMessage: (m: string) => w.__appMsgs.push(m) };
    });
    const back = () => page.evaluate(() => (window as unknown as { musterBack(): boolean }).musterBack());
    await ready(page, context);
    await openRendered(page);
    await openPanel(page);
    const panelEl = page.locator('#preview-panel');
    const full = panelEl.locator('[data-preview-newtab]');
    const exit = panelEl.locator('[data-preview-exit-full]');
    const toggle = panelEl.locator('[data-preview-bar-toggle]');
    await expect(toggle, 'no bar button outside full screen').toBeHidden();
    await expect(full).toHaveText('Full screen');

    // FULL SCREEN: the bar hidden, the panel the viewport, the button at the
    // bottom right — inside the screen, a 44px target, semi-transparent.
    await full.click();
    await expect(panelEl).toHaveAttribute('data-preview-full', '');
    await expect(page.locator('#mv-compose'), 'the bar is hidden').toBeHidden();
    expect(await inert(page, '#mv-send'), 'and its controls inert').toBe(true);
    const pb = await rect(page, '#preview-panel');
    expect([pb.x, pb.y, pb.w, pb.h].map(Math.round), 'the panel is the viewport').toEqual([0, 0, 390, 844]);
    await expect(toggle).toBeVisible();
    await expect(toggle).toHaveAttribute('aria-label', 'Show message bar');
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    const tb = await rect(page, '[data-preview-bar-toggle]');
    expect(tb.w, 'a 44px target').toBeGreaterThanOrEqual(44);
    expect(tb.h, 'a 44px target').toBeGreaterThanOrEqual(44);
    expect(tb.right, 'at the right, inside the screen').toBeLessThanOrEqual(390);
    expect(tb.right, 'at the right').toBeGreaterThan(390 - 24);
    expect(tb.bottom, 'at the bottom, inside the screen').toBeLessThanOrEqual(844);
    expect(tb.bottom, 'at the bottom').toBeGreaterThan(844 - 24);
    const opacity = await toggle.evaluate((b) => Number(getComputedStyle(b).opacity));
    expect(opacity, 'semi-transparent').toBeLessThan(1);
    expect(opacity, 'still visible').toBeGreaterThan(0.2);
    expect(await onTop(page, '[data-preview-bar-toggle]'), 'the button is on top of the preview').toBe('itself');
    expect(await rootFlags(page)).toEqual(['data-preview-nobar']);

    // A REPLY while full screen covers the chat: the dot is on the button
    // (the header, with its title, is hidden).
    const fullDot = panelEl.locator('[data-preview-dot="full"]');
    await expect(fullDot).toBeHidden();
    const bubbles = page.locator('#mv-log [data-bubble="assistant"]');
    const n = await bubbles.count();
    reply('a reply during full screen');
    await expect(bubbles).toHaveCount(n + 1, { timeout: 20_000 });
    await expect(fullDot, 'the reply set the dot on the bar button').toBeVisible();

    // SHOW: the bar, live, the preview above it and the button above that.
    await toggle.tap();
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect(toggle).toHaveAttribute('aria-label', 'Hide message bar');
    expect(await rootFlags(page)).toEqual(['data-preview-bar']);
    await expect(page.locator('#mv-compose')).toBeVisible();
    expect(await inert(page, '#mv-send'), 'the shown bar (its Send) is not inert').toBe(false);
    const shown = await panelOnBar(page, 'the bar shown in full screen');
    expect(Math.round(shown.panel.y), 'still full screen above the bar').toBe(0);
    expect((await rect(page, '[data-preview-bar-toggle]')).bottom, 'the button is above the bar').toBeLessThanOrEqual(shown.bar.y);
    for (const sel of ['#mv-text', '#mv-send']) expect(await onTop(page, sel), `${sel} is on top`).toBe('itself');
    await revealBox(page);
    await sendFrom(page, 'sent during full screen', 'tap');
    await expect.poll(pasted).toContain('sent during full screen');
    await expect(fullDot, 'the chat is still covered: the dot stays').toBeVisible();

    // HIDE again with the same button.
    await toggle.tap();
    await expect(toggle).toHaveAttribute('aria-label', 'Show message bar');
    expect(await rootFlags(page)).toEqual(['data-preview-nobar']);
    await expect(page.locator('#mv-compose')).toBeHidden();
    expect(Math.round((await rect(page, '#preview-panel')).h), 'the preview is the viewport again').toBe(844);

    // EXIT (the bar shown first): the panel as before full screen — the
    // header, the bar live below it — and the bar's button gone.
    await toggle.tap();
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await exit.click();
    await expect(panelEl).not.toHaveAttribute('data-preview-full', '');
    await expect(panelEl.locator('h2#preview-panel-title')).toBeVisible();
    await expect(toggle).toBeHidden();
    expect(await rootFlags(page)).toEqual(['data-preview-bar']);
    await panelOnBar(page, 'after Exit');
    // Still covering the chat on a phone: the dot is on the header now.
    await expect(panelEl.locator('[data-preview-dot="header"]')).toBeVisible();
    // Full screen again opens with the bar hidden, whatever it was.
    await full.click();
    expect(await rootFlags(page)).toEqual(['data-preview-nobar']);
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');

    // BACK: full screen (bar shown) → the panel → closed.
    await toggle.tap();
    expect(await rootFlags(page)).toEqual(['data-preview-bar']);
    expect(await back(), 'Back left full screen').toBe(true);
    await expect(panelEl).not.toHaveAttribute('data-preview-full', '');
    await expect(panelEl).toBeVisible();
    expect(await rootFlags(page)).toEqual(['data-preview-bar']);
    await panelOnBar(page, 'after Back left full screen');
    expect(await back(), 'the next Back closed the panel').toBe(true);
    await expect(panelEl).toBeHidden();
    expect(await rootFlags(page), 'closed: the layout as it was').toEqual([]);
    await expect(page.locator('#mv-compose')).toBeVisible();
    expect(await inert(page, '#mv-back')).toBe(false);
    expect(await back(), 'with nothing open Back is the shell\'s').toBe(false);
    // The chat is in view: the dot has cleared.
    await openPanel(page);
    await expect(panelEl.locator('[data-preview-dot="header"]')).toBeHidden();
    await panelEl.locator('[data-preview-close]').click();
  });
});
