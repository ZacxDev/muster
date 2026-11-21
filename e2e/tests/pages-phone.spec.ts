import { execFileSync } from 'node:child_process';
import { test, expect } from '../lib/fixtures';
import { dsn, pagesPhoneConnectPort, pagesPhonePort, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// A MARKDOWN PAGE READ ON A PHONE (internal/pages render.go markdownWithAssets
// and contentCSS; docs/pages.md#images-and-video): a link's view at 390×844 of
// a page written through the real store (e2e/pagesfix phone) with the shapes
// that made a page an agent published on a server with uploads off unreadable
// on a phone — a long paragraph ending in one unbroken run, a long inline-code
// URL, a 9-column table and a markdown image inline as a data:image/png URI:
//
//   - the image is DRAWN (it decoded: naturalWidth > 0), and no "data:image"
//     is left as text anywhere on the page;
//   - the content document is no wider than the phone (scrollWidth <=
//     clientWidth): the long run and the code span wrap;
//   - the table scrolls inside its own box instead, every column at least
//     90px wide (not crushed into one-letter columns).
//
// Every value below is an obvious fake.

const origin = `http://localhost:${pagesPhonePort}`;
const KEY = Buffer.alloc(32, 0x5b).toString('base64');
let server: Running | undefined;
let token = '';

const fix = (cmd: 'phone' | 'phone-clear') =>
  execFileSync('go', ['run', './e2e/pagesfix', cmd, '1'], {
    cwd: repoRoot,
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_CONNECTOR_ENCRYPTION_KEY: KEY },
  }).toString();

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, pagesPhonePort, {
    MUSTER_PAGES: '1',
    MUSTER_PUBLIC_URL: origin,
    MUSTER_CONNECTOR_ENCRYPTION_KEY: KEY,
    // A link key is the connector key, and that one needs its listener.
    MUSTER_CONNECT_PORT: String(pagesPhoneConnectPort),
    MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${pagesPhoneConnectPort}`,
  });
  token = (JSON.parse(fix('phone')) as { token: string }).token;
  expect(token.length, 'the fixture made a live link').toBeGreaterThan(20);
});

test.afterAll(async () => {
  if (server) fix('phone-clear');
  await server?.stop();
});

test('a markdown page fits a phone: its data: image drawn, its text wrapped, its wide table scrolling in its own box', async ({ page }) => {
  await page.goto(`${origin}/s/${token}`);
  const frame = page.frameLocator('iframe#page');
  await expect(frame.locator('main h1')).toHaveText('Phone check');

  const img = frame.locator('main img[alt="status chart"]');
  await expect(img, 'the markdown data: image is an <img>').toHaveCount(1);
  await expect.poll(() => img.evaluate((e) => (e as HTMLImageElement).naturalWidth), { message: 'the image decoded' }).toBeGreaterThan(0);
  const text = await frame.locator('main').evaluate((m) => (m as HTMLElement).innerText);
  expect(text, 'a data: URI is left as text').not.toContain('data:image');

  const doc = await frame.locator('html').evaluate((h) => ({ scroll: h.scrollWidth, client: h.clientWidth }));
  expect(doc.client, 'the frame is the phone wide').toBeGreaterThan(300);
  expect(doc.scroll, `the content document scrolls sideways (${doc.scroll} > ${doc.client})`).toBeLessThanOrEqual(doc.client);

  const box = await frame.locator('main table').evaluate((t) => {
    const w = t.parentElement as HTMLElement;
    return {
      overflowX: getComputedStyle(w).overflowX,
      scroll: w.scrollWidth,
      client: w.clientWidth,
      narrowest: Math.min(...Array.from(t.querySelectorAll('th')).map((c) => c.getBoundingClientRect().width)),
    };
  });
  expect(box.overflowX, "the table's box scrolls").toBe('auto');
  expect(box.scroll, "the 9-column table is wider than its box (it scrolls rather than being crushed)").toBeGreaterThan(box.client);
  expect(box.narrowest, 'a column is crushed').toBeGreaterThanOrEqual(90);
});
