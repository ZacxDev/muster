import { request as pwRequest, CDPSession, Page, WebSocket as PWWebSocket } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { hookToken, overflowPort, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { openDispatchSheet } from '../lib/dispatch';

// A terminal whose far side is WIDER than the page (internal/termsize): the
// session keeps its size, the page renders it in a scrolling, pinch-zoomable
// view with "Fit width", and a phone never sends a size that would shrink it.
//
// The far side is the e2eterm fake pod with a 120x30 session window
// (MUSTER_E2E_TERM_WINDOW): it answers muster's window-size query as tmux would,
// prints `size=CxR` for every size that reaches the exec, and draws a ruler that
// ends exactly at the client's last column with RIGHT-EDGE. Everything between
// — agentprovision's no-shrink rule, the in-band report, the socket bridge, the
// page — is the shipped code.
//
// 🔴 120 COLUMNS ON PURPOSE: wider than 412px (≈52 columns) and narrower than
// 1280px (≈160), so the same server answers both the phone case (overflow) and
// the desktop case (fit, unchanged).

const origin = `http://localhost:${overflowPort}`;
const WIN = { cols: 120, rows: 30 };
let server: Running | undefined;

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  server = await startServer(versionTerm, overflowPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-overflow-alpha',
    MUSTER_E2E_TERM_WINDOW: `${WIN.cols}x${WIN.rows}`,
  });
});
test.afterAll(async () => {
  await server?.stop();
});

async function login(page: Page) {
  await signIn(page, origin);
}

async function agentNames(): Promise<Set<string>> {
  const ctx = await pwRequest.newContext({ baseURL: origin, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
  const r = await ctx.get('/api/agents');
  expect(r.status(), await r.text()).toBe(200);
  const rows = (await r.json()) as { name: string }[];
  await ctx.dispose();
  return new Set(rows.map((a) => a.name));
}

// A claude-code agent created through the API's own door (the hook token), so
// the dispatch sheet's layout at each width is not part of this spec.
async function newAgent(page: Page, note: string): Promise<string> {
  const before = await agentNames();
  await page.goto(origin + '/tasks');
  await openDispatchSheet(page);
  const sheet = page.locator('#agent-modal form');
  await sheet.locator('[data-kind-option="claude-code"]').click();
  await sheet.locator('textarea[name="note_text"]').fill(note);
  const resp = page.waitForResponse((r) => r.url().endsWith('/agents') && r.request().method() === 'POST');
  await sheet.locator('button[value="save"]').click();
  expect((await resp).status()).toBe(200);
  const created = [...(await agentNames())].filter((n) => !before.has(n));
  expect(created.length, 'exactly one agent created').toBe(1);
  // The sheet's answer sends the page to the new agent's own page.
  await page.waitForURL((u) => u.pathname === '/agents/' + created[0]);
  return created[0];
}

// Every {"type":"resize"} the page SENT on the terminal socket.
function recordResizes(page: Page): string[] {
  const sent: string[] = [];
  page.on('websocket', (ws: PWWebSocket) => {
    if (!ws.url().includes('/terminal/ws')) return;
    ws.on('framesent', (f) => {
      if (typeof f.payload === 'string' && f.payload.includes('"resize"')) sent.push(f.payload);
    });
  });
  return sent;
}

const screen = (page: Page) => page.locator('#term .xterm-rows');
const sizeLines = async (page: Page) => [...(await screen(page).innerText()).matchAll(/size=(\d+)x(\d+)/g)].map((m) => `${m[1]}x${m[2]}`);

// The on-screen rectangle of the RIGHT-EDGE marker (the session's last
// columns), measured on the text itself.
async function markerRect(page: Page) {
  return page.evaluate(() => {
    const rows = document.querySelector('#term .xterm-rows')!;
    const walker = document.createTreeWalker(rows, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const i = (n.textContent || '').indexOf('RIGHT-EDGE');
      if (i < 0) continue;
      const r = document.createRange();
      r.setStart(n, i);
      r.setEnd(n, i + 'RIGHT-EDGE'.length);
      const b = r.getBoundingClientRect();
      return { left: b.left, right: b.right };
    }
    return null;
  });
}

// pinch is a real two-finger gesture (CDP touch events) centred on the
// terminal, the fingers moving from `from` to `to` px either side of centre.
// (Input.synthesizePinchGesture was measured NOT to zoom in headless Chromium;
// raw touch points do, through the browser's own gesture recogniser.)
async function pinch(cdp: CDPSession, from: number, to: number) {
  const cx = 206, cy = 420;
  const pts = (d: number) => [{ x: cx - d, y: cy, id: 0 }, { x: cx + d, y: cy, id: 1 }];
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: pts(from) });
  const step = from < to ? 10 : -10;
  for (let d = from; from < to ? d <= to : d >= to; d += step) {
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: pts(d) });
  }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
}

async function noDocumentScroll(page: Page) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'the DOCUMENT never scrolls sideways — only the terminal view does').toBeLessThanOrEqual(cw);
}

test.describe('at a phone’s width (412px)', () => {
  test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true });

  test('a wider session renders at its own size, scrolls to its right edge, pinch-zooms and fits width — and is never resized', async ({ page }) => {
    await login(page);
    const name = await newAgent(page, 'e2e overflow: phone');
    const resizes = recordResizes(page);
    await page.goto(origin + '/agents/' + name + '/terminal');
    await expect(screen(page)).toContainText('FAKE-TUI mode=rw');

    // The far side kept its window and told the page; the exec got 120x30,
    // not the phone's ~52 columns.
    await expect(page.locator('#term')).toHaveAttribute('data-remote', `${WIN.cols}x${WIN.rows}`);
    await expect.poll(() => sizeLines(page)).toEqual([`${WIN.cols}x${WIN.rows}`]);
    await expect(page.locator('#term')).toHaveAttribute('data-overflow', '1');
    await expect(page.locator('#term-fitw')).toBeVisible();
    await noDocumentScroll(page);

    // The marker starts OFF the right of the screen…
    await expect.poll(async () => (await markerRect(page))?.left ?? -1).toBeGreaterThan(412);
    // …and the view scrolls all the way to it.
    const scrolled = await page.locator('#term-scroll').evaluate((el) => {
      el.scrollLeft = el.scrollWidth;
      return { left: el.scrollLeft, max: el.scrollWidth - el.clientWidth };
    });
    expect(scrolled.max, 'the view is scrollable sideways').toBeGreaterThan(300);
    expect(scrolled.left, 'scrolled to the far end').toBe(scrolled.max);
    const atEdge = (await markerRect(page))!;
    expect(atEdge.right, 'RIGHT-EDGE is on screen once scrolled').toBeLessThanOrEqual(412);
    expect(atEdge.left).toBeGreaterThanOrEqual(0);
    await page.locator('#term-scroll').evaluate((el) => (el.scrollLeft = 0));

    // PINCH. Positive control first: the page really is zoomed afterwards,
    // and visualViewport really fired — or "no resize" would prove nothing.
    await page.evaluate(() => {
      (window as unknown as { __vv: number }).__vv = 0;
      window.visualViewport!.addEventListener('resize', () => ((window as unknown as { __vv: number }).__vv += 1));
    });
    const layoutsBefore = await page.locator('#term').getAttribute('data-layouts');
    const cdp = await page.context().newCDPSession(page);
    await pinch(cdp, 30, 140); // fingers apart: zoom in, on the terminal itself
    await expect.poll(() => page.evaluate(() => window.visualViewport!.scale), { message: 'the pinch zoomed the page' }).toBeGreaterThan(1.5);
    expect(await page.evaluate(() => (window as unknown as { __vv: number }).__vv), 'visualViewport fired resize during the pinch').toBeGreaterThan(0);
    await page.waitForTimeout(500);
    expect(resizes, 'resize messages sent by a pinch').toEqual([]);
    expect(await page.locator('#term').getAttribute('data-layouts'), 'the pinch did not re-lay out (no refit on zoom)').toBe(layoutsBefore);
    expect(await sizeLines(page)).toEqual([`${WIN.cols}x${WIN.rows}`]);
    await pinch(cdp, 140, 10); // fingers together: back out
    await expect.poll(() => page.evaluate(() => window.visualViewport!.scale)).toBeLessThan(1.1);
    expect(resizes, 'resize messages sent by pinching back out').toEqual([]);

    // FIT WIDTH: the whole width on screen, nothing scrolled, nothing sent.
    await page.locator('#term-fitw').click();
    await expect(page.locator('#term-fitw')).toHaveAttribute('aria-pressed', 'true');
    await expect.poll(async () => (await markerRect(page))?.right ?? 9999, { message: 'RIGHT-EDGE inside 412px under Fit width' }).toBeLessThanOrEqual(412);
    expect(await page.locator('#term-scroll').evaluate((el) => el.scrollWidth - el.clientWidth), 'nothing left to scroll sideways').toBeLessThanOrEqual(1);
    await page.locator('#term-fitw').click();
    await expect(page.locator('#term-fitw')).toHaveAttribute('aria-pressed', 'false');
    await expect.poll(async () => (await markerRect(page))?.left ?? -1).toBeGreaterThan(412);
    expect(resizes, 'resize messages sent by Fit width').toEqual([]);

    // A rotation-sized change is a REAL layout change, but the page still does
    // not cover the session, so it still sends nothing.
    await page.setViewportSize({ width: 600, height: 915 });
    await page.waitForTimeout(400);
    expect(resizes, 'a still-narrower-than-the-session viewport sends nothing').toEqual([]);
    await noDocumentScroll(page);
  });

  test('interactive on a phone does not shrink the session either', async ({ page }) => {
    await login(page);
    const name = await newAgent(page, 'e2e overflow: phone rw');
    const resizes = recordResizes(page);
    await page.goto(origin + '/agents/' + name + '/terminal');
    // Live keys are always on: the page attaches interactively at once.
    await expect(screen(page)).toContainText('FAKE-TUI mode=rw');
    await expect(page.locator('#term-mode'), 'no Live keys switch').toHaveCount(0);
    await expect(page.locator('#term')).toHaveAttribute('data-remote', `${WIN.cols}x${WIN.rows}`);
    await expect.poll(() => sizeLines(page)).toEqual([`${WIN.cols}x${WIN.rows}`]);
    await page.waitForTimeout(300);
    expect(resizes).toEqual([]);
  });
});

test.describe('at a desktop width (1280px)', () => {
  test.use({ viewport: { width: 1280, height: 900 } });

  test('a page wider than the session fits its container and grows it, as before; narrowing scrolls instead of shrinking', async ({ page }) => {
    await login(page);
    const name = await newAgent(page, 'e2e overflow: desktop');
    const resizes = recordResizes(page);
    await page.goto(origin + '/agents/' + name + '/terminal');
    await expect(screen(page)).toContainText('FAKE-TUI mode=rw');

    // The page covers 120x30, so it asks for its own fit and gets it.
    await expect.poll(async () => (await page.locator('#term').getAttribute('data-remote')) ?? '').not.toBe('');
    const fitted = (await page.locator('#term').getAttribute('data-remote'))!;
    const [fc, fr] = fitted.split('x').map(Number);
    expect(fc, 'the desktop fit is wider than the session').toBeGreaterThan(WIN.cols);
    expect(fr).toBeGreaterThanOrEqual(WIN.rows);
    await expect.poll(() => sizeLines(page)).toEqual([fitted]);
    await expect(page.locator('#term')).toHaveAttribute('data-overflow', '0');
    await expect(page.locator('#term-fitw')).toBeHidden();
    expect(await page.locator('#term-scroll').evaluate((el) => el.scrollWidth - el.clientWidth), 'no sideways scroll on a desktop that fits').toBeLessThanOrEqual(1);
    const edge = (await markerRect(page))!;
    expect(edge.right, 'the ruler (the client width) ends inside the page').toBeLessThanOrEqual(1280);

    // Wider: the page grows the session, as it always did.
    await page.setViewportSize({ width: 1600, height: 900 });
    await expect.poll(() => resizes.length, { message: 'a wider desktop sends its new size' }).toBeGreaterThan(0);
    await expect.poll(async () => (await sizeLines(page)).length).toBe(2);
    const grown = (await sizeLines(page))[1];
    expect(Number(grown.split('x')[0])).toBeGreaterThan(fc);

    // Narrower than what it grew: the session is not shrunk — the view scrolls.
    const sentBefore = resizes.length;
    await page.setViewportSize({ width: 1100, height: 900 });
    await expect(page.locator('#term')).toHaveAttribute('data-overflow', '1');
    await expect(page.locator('#term-fitw')).toBeVisible();
    await page.waitForTimeout(400);
    expect(resizes.length, 'narrowing below the session sends nothing').toBe(sentBefore);
    expect(await sizeLines(page)).toEqual([fitted, grown]);
    await noDocumentScroll(page);
    // Fit width on a desktop, too: the whole session width in view.
    await page.locator('#term-fitw').click();
    await expect.poll(async () => (await markerRect(page))?.right ?? 9999).toBeLessThanOrEqual(1100);
    expect(resizes.length, 'Fit width sends nothing').toBe(sentBefore);
  });
});

// 🔴 A SESSION TALLER THAN THE PAGE SHOWS ITS BOTTOM (ui/terminal.go,
// atBottom). The far side's prompt and status line are its LAST rows; drawn
// from the top they were off screen on every load — a pod's window, once grown
// to a full desktop height, stays that tall (termsize never shrinks it), so the
// page drew it in overflow at scrollTop 0 for good. Now the overflow view
// starts at its bottom and goes back there on every redraw, unless the person
// scrolled it away themselves; and the session switcher stepping aside on a
// very short screen and coming back (a relayout each way) leaves it there.
// The 120x30 window is taller than a 1280px-wide page 470–580px tall, and
// narrower than it, so the overflow here is vertical only.
async function lastRow(page: Page) {
  return page.evaluate(() => {
    const sc = document.getElementById('term-scroll')!;
    const rows = document.querySelector('#term .xterm-rows')!;
    const a = sc.getBoundingClientRect(), b = (rows.lastElementChild as HTMLElement).getBoundingClientRect();
    return { shown: b.top >= a.top - 1 && b.bottom <= a.bottom + 1, rows: rows.children.length, top: sc.scrollTop, max: sc.scrollHeight - sc.clientHeight };
  });
}

test.describe('a session taller than the page (1280×560)', () => {
  test.use({ viewport: { width: 1280, height: 560 } });

  test('it loads showing its last row, and stays at the bottom through a relayout unless the person scrolled away', async ({ page }) => {
    await login(page);
    const name = await newAgent(page, 'e2e overflow: taller');
    const resizes = recordResizes(page);
    await page.goto(origin + '/agents/' + name + '/terminal');
    await expect(screen(page)).toContainText('FAKE-TUI mode=rw');
    await expect(page.locator('#term')).toHaveAttribute('data-remote', `${WIN.cols}x${WIN.rows}`);
    await expect(page.locator('#term')).toHaveAttribute('data-overflow', '1');
    await expect(page.locator('#session-switcher')).toBeVisible();
    // The view really overflows downward (or "at its bottom" proves nothing),
    // and it is at its bottom: the session's last row is on screen.
    await expect.poll(async () => (await lastRow(page)).max, { message: 'the view scrolls vertically' }).toBeGreaterThan(50);
    await expect.poll(async () => (await lastRow(page)).shown, { message: 'the last row is on screen on load' }).toBe(true);
    const r0 = await lastRow(page);
    expect(r0.rows, 'the session is drawn at its own height').toBe(WIN.rows);
    expect(r0.top, 'scrolled to the bottom').toBeGreaterThanOrEqual(r0.max - 1);

    // The person scrolls up: a relayout keeps their place. (A scroll the page
    // did not make itself — set on the element here: xterm takes a desktop
    // wheel over its own rows, and a phone's pan is a touch scroll of the same
    // element, which the page sees as the same scroll event.)
    const scrollTo = (y: number) => page.locator('#term-scroll').evaluate((el, v) => { el.scrollTop = v; }, y);
    await page.waitForTimeout(300);
    await scrollTo(0);
    await expect(page.locator('#term')).toHaveAttribute('data-at-bottom', '0');
    const layouts = await page.locator('#term').getAttribute('data-layouts');
    await page.setViewportSize({ width: 1280, height: 580 });
    await expect.poll(() => page.locator('#term').getAttribute('data-layouts'), { message: 'the viewport change re-laid out' }).not.toBe(layouts);
    await page.waitForTimeout(500);
    const kept = await lastRow(page);
    expect(kept.top, 'a relayout did not take the person back down').toBeLessThan(5);
    expect(kept.shown).toBe(false);

    // Scrolled back to the bottom: held there again through the next relayout.
    await page.waitForTimeout(300);
    await scrollTo(100_000);
    await expect(page.locator('#term')).toHaveAttribute('data-at-bottom', '1');
    await page.setViewportSize({ width: 1280, height: 560 });
    await page.waitForTimeout(500);
    expect((await lastRow(page)).shown, 'the last row after a relayout at the bottom').toBe(true);
    expect(resizes, 'a session taller than the page is never resized').toEqual([]);
  });

  test('the session switcher stepping aside on a very short screen and coming back leaves the view at its bottom', async ({ page }) => {
    await login(page);
    const name = await newAgent(page, 'e2e overflow: switcher');
    await page.goto(origin + '/agents/' + name + '/terminal');
    await expect(screen(page)).toContainText('FAKE-TUI mode=rw');
    await expect(page.locator('#term')).toHaveAttribute('data-overflow', '1');
    const bar = page.locator('#session-switcher');
    await expect(bar).toBeVisible();
    await expect.poll(async () => (await lastRow(page)).shown).toBe(true);
    // 470px tall, a short screen (≤ 30rem): the bar steps aside, its room given back.
    await page.setViewportSize({ width: 1280, height: 470 });
    await expect(bar).toBeHidden();
    await expect.poll(async () => (await lastRow(page)).shown, { message: 'the last row with the bar hidden' }).toBe(true);
    // 520px: the bar is back, the terminal's box 45px shorter — still at the bottom.
    await page.setViewportSize({ width: 1280, height: 520 });
    await expect(bar).toBeVisible();
    await expect(page.locator('#term')).toHaveAttribute('data-overflow', '1');
    await expect.poll(async () => (await lastRow(page)).shown, { message: 'the last row once the bar is back' }).toBe(true);
  });

  test('a page that stops covering the session it grew enters overflow at the bottom', async ({ page }) => {
    await login(page);
    const name = await newAgent(page, 'e2e overflow: grew');
    await page.setViewportSize({ width: 1280, height: 1000 });
    await page.goto(origin + '/agents/' + name + '/terminal');
    await expect(screen(page)).toContainText('FAKE-TUI mode=rw');
    // Tall enough to cover 120x30: it fits and grows the session to its size.
    await expect(page.locator('#term')).toHaveAttribute('data-overflow', '0');
    await expect.poll(async () => Number(((await page.locator('#term').getAttribute('data-remote')) ?? '0x0').split('x')[1])).toBeGreaterThan(WIN.rows);
    const grown = Number((await page.locator('#term').getAttribute('data-remote'))!.split('x')[1]);
    // Shorter again: the session is not shrunk, the page draws it in
    // overflow — from its bottom.
    await page.setViewportSize({ width: 1280, height: 700 });
    await expect(page.locator('#term')).toHaveAttribute('data-overflow', '1');
    await expect.poll(async () => (await lastRow(page)).shown, { message: 'the last row once in overflow' }).toBe(true);
    expect((await lastRow(page)).rows).toBe(grown);
  });
});
