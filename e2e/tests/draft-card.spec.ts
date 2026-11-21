import { execFileSync } from 'node:child_process';
import { Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { draftCardPort, dsn, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// THE ASKS PAGE'S REPLY-DRAFT CARD KEEPS ITS ACTIONS READABLE (#689), in a real
// browser against a real muster-server (plugins on), at a phone (390×844,
// touch) and a desktop (1440×900): a pending draft whose reply is long and
// multi-line, with its expiry line, on the Reply drafts tab. Approve and send
// must show its whole label on one line, inside the button; so must Reject;
// the reason box must be usable; and nothing scrolls sideways.
//
// 🔴 RED AT origin/main (bdfacef2): the action row put the Approve form at
// `flex-1` (a ZERO basis) beside the Reject form at its max-content width —
// the reason box's intrinsic width plus the Reject button — so Approve got
// what was left: the same 24 px sliver at 390 and at 1440, because the Asks
// page is one max-w-sm column and the card is 352 px wide at every viewport.
// The two widths are measured to show the fix holds at both, not because the
// bug depended on the viewport.
//
// The draft is a fixture row (e2e/pluginfix draft): what is under test is how
// the page DRAWS it, not how a draft is made (plugins.spec.ts drives that).

const origin = `http://localhost:${draftCardPort}`;
const stamp = Date.now().toString(36);
const agentName = 'e2e-draftcard-' + stamp;
const agentToken = 'e2e-draftcard-agent-token-not-a-real-one-' + stamp;
const plugins: string[] = [];
let agentMade = false;
let server: Running | undefined;

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

// A long, multi-line reply (synthetic).
const BODY = [
  'Thanks for the note. Yes, the release is still planned for this week.',
  '',
  'The migration runs first, then the rollout to every region, one at a time, with a pause between each.',
  'If anything looks off we hold the rest and write back here before going on.',
].join('\n');

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, draftCardPort, {
    // Reply drafts are drawn only with the plugin interface on.
    MUSTER_PLUGINS: '1',
    MUSTER_FOCUS_INTERVAL: '0',
  });
  fix('agent', '1', agentName, agentToken);
  agentMade = true;
});

test.afterAll(async () => {
  try {
    for (const p of plugins) fix('rmplugin', '1', p);
    if (agentMade) fix('rmagent', '1', agentName);
  } finally {
    await server?.stop();
  }
});

// labelFit measures a button's label against the button: the label's width
// set on ONE line in the button's own font, the button's content box, the
// lines the label actually takes, and whether the button's content overflows.
async function labelFit(button: Locator) {
  return button.evaluate((el) => {
    const cs = getComputedStyle(el);
    const ctx = document.createElement('canvas').getContext('2d')!;
    ctx.font = `${cs.fontStyle} ${cs.fontWeight} ${cs.fontSize} ${cs.fontFamily}`;
    const text = (el.textContent ?? '').trim();
    const range = document.createRange();
    range.selectNodeContents(el);
    const lines = new Set(Array.from(range.getClientRects(), (r) => Math.round(r.top))).size;
    return {
      text,
      textWidth: ctx.measureText(text).width,
      contentWidth: el.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight),
      lines,
      scrollWidth: el.scrollWidth,
      clientWidth: el.clientWidth,
    };
  });
}

async function expectFullLabel(button: Locator, label: string) {
  await expect(button).toBeVisible();
  const m = await labelFit(button);
  expect(m.text).toBe(label);
  expect(m.textWidth, `${label}: the measured label is not empty`).toBeGreaterThan(20);
  expect(m.contentWidth, `${label}: the button is at least as wide as its label (${JSON.stringify(m)})`).toBeGreaterThanOrEqual(m.textWidth - 0.5);
  expect(m.lines, `${label}: the label is on one line (${JSON.stringify(m)})`).toBe(1);
  expect(m.scrollWidth, `${label}: nothing overflows the button (${JSON.stringify(m)})`).toBeLessThanOrEqual(m.clientWidth);
}

async function noSidewaysScroll(page: Page) {
  const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
  expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(iw);
}

for (const vp of [
  { name: 'phone 390×844', key: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', key: 'desktop', use: { viewport: { width: 1440, height: 900 } } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);
    test('a long multi-line reply draft keeps Approve and send and Reject whole and readable', async ({ page }, testInfo) => {
      const plugin = `e2e-draftcard-${vp.key}-${stamp}`;
      plugins.push(plugin);
      const id = fix('draft', '1', agentName, plugin, BODY);
      await signIn(page, origin);
      await page.goto(`${origin}/attention?tab=drafts`);
      const card = page.locator(`[data-draft-id="${id}"]`);
      await expect(card).toBeVisible();
      // The fixture is the card the issue reports: the expiry line and a
      // multi-line body (positive controls on the fixture itself). The card
      // draws the reply as its post (the step-up card's venue preview): its
      // blank line a paragraph break, its other newline a real line break,
      // never a spelled `\n`; the paragraph and newline counts are the ONLY
      // check here that the body has them, and the spelled-marker count
      // proves none is escaped.
      // The data-runes check compares the card's length with the fixture's
      // own, so it shows the whole body arrived but cannot see newlines (a
      // space in place of each would pass it).
      // The expiry is its own pill, relative ("expires in 1d"), never a UTC stamp.
      await expect(card.locator('[data-draft-expiry]')).toHaveText(/^expires in \d+[mhd]$/);
      await expect(card).not.toContainText('UTC');
      const body = card.locator('[data-draft-text] [data-stepup-preview-text]');
      await expect(body.locator('p'), 'the reply is its two paragraphs').toHaveCount(2);
      const text = (await body.textContent())!;
      expect(text.split('\n').length - 1, 'the second paragraph keeps its line break').toBe(1);
      expect(text.split('\\n').length - 1, 'no newline is spelled out as \\n').toBe(0);
      await expect(card.locator('[data-draft-text]')).toHaveAttribute('data-runes', String(Array.from(BODY).length));

      await card.scrollIntoViewIfNeeded();
      const shot = testInfo.outputPath(`draft-card-${vp.key}.png`);
      await card.screenshot({ path: shot });
      await testInfo.attach(`draft-card-${vp.key}.png`, { path: shot, contentType: 'image/png' });

      await expectFullLabel(card.locator('[data-draft-approve]'), 'Approve and send');
      await expectFullLabel(card.locator('[data-draft-reject]'), 'Reject');
      // The reason box is a usable field, not squeezed shut.
      const reason = await card.locator('[data-draft-reason-input]').boundingBox();
      expect(reason!.width, 'the reason box is wide enough to type in').toBeGreaterThanOrEqual(120);
      await noSidewaysScroll(page);
    });
  });
}

// THE DRAFTS TAB DRAWS EACH DRAFT AS ITS POST (the operator: "the other tabs
// in the Asks page still don't have the updated UI"), at a phone and a
// desktop, and with no script: a draft card is the Open card's head (the
// shared head, the draft's id chip on its top row) over the step-up card's
// venue preview — the venue's logo, the text as paragraphs, the image — its
// media facts folded under Details (closed), and its own decision. A reply
// draft keeps the message it answers, quoted and marked untrusted. Its text is
// never spelled out: an em dash and an emoji are themselves, and a
// right-to-left override is a visible flag naming it — the character itself
// is never in the page.
// 🔴 RED at the branch base: a "POST"/"REPLY DRAFT" chip, a "New post on X"
// label and the text in one plain box with every non-ASCII character spelled
// (\u{2014}, \u{1f389}, \u{202e}).
const RLO = String.fromCodePoint(0x202e);
const PREVIEW_REPLY = `Thanks — noon works 🎉\n\nPay ${RLO}gro.live later.`;
const PREVIEW_POST = 'Big news: acme is now acme-two 🎉\nRead more on the blog.\n\nA second paragraph — with a dash.';
const pngBytes = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgaGD4DwAChAGAkdXSbQAAAABJRU5ErkJggg==', 'base64');

async function expectThePost(page: Page, id: string, venueLogo: string, media: number) {
  const card = page.locator(`[data-draft-id="${id}"]`);
  await expect(card).toBeVisible();
  const top = card.locator(':scope > [data-asks-top]');
  await expect(top.locator(':scope > [data-asks-head]')).toHaveCount(1);
  await expect(top.locator(':scope > [data-id-copy]')).toContainText(`Draft #${id}`);
  await expect(card.locator('[data-asks-badge]'), 'no kind chip').toHaveCount(0);
  const post = card.locator('[data-draft-text] article[data-stepup-post]');
  await expect(post).toBeVisible();
  await expect(post.locator(`[data-stepup-logo] svg[data-venue-logo="${venueLogo}"]`)).toBeVisible();
  await expect(post.locator('[data-stepup-preview-text] p')).toHaveCount(2);
  await expect(post.locator('[data-stepup-media] img')).toHaveCount(media);
  if (media > 0) {
    await expect(post.locator('[data-stepup-media] img').first()).toBeVisible();
    const details = card.locator('details[data-stepup-details]');
    await expect(details).not.toHaveAttribute('open');
    await expect(details.locator('[data-draft-medium-facts]')).toBeHidden();
  }
  // The preview sits inside the card: nothing of it overflows sideways.
  const fit = await card.evaluate((c) => {
    const cb = c.getBoundingClientRect();
    const a = c.querySelector('[data-stepup-post]')!.getBoundingClientRect();
    return { left: a.left - cb.left, right: cb.right - a.right };
  });
  expect(fit.left, 'the preview starts inside the card').toBeGreaterThanOrEqual(0);
  expect(fit.right, 'the preview ends inside the card').toBeGreaterThanOrEqual(0);
  const face = await card.innerText();
  for (const never of ['\\u{', '\\n', 'New post on', 'image/png']) {
    expect(face, `the draft card shows ${never}`).not.toContain(never);
  }
  expect(await card.innerHTML(), 'the override itself is never in the page').not.toContain(RLO);
  await expect(card.locator('[data-draft-approve]')).toBeVisible();
  await expect(card.locator('[data-draft-reject]')).toBeVisible();
  return card;
}

for (const vp of [
  { name: 'phone 390×844', key: 'phone', js: true, use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', key: 'desktop', js: true, use: { viewport: { width: 1440, height: 900 } } },
  { name: 'phone 390×844, no script', key: 'nojs', js: false, use: { viewport: { width: 390, height: 844 }, javaScriptEnabled: false } },
]) {
  test.describe(`the venue preview, ${vp.name}`, () => {
    test.use(vp.use);
    test('the Drafts tab draws each draft as its post, with its own decision', async ({ page }) => {
      const reply = `e2e-draftpost-r-${vp.key}-${stamp}`;
      const x = `e2e-draftpost-x-${vp.key}-${stamp}`;
      plugins.push(reply, x);
      const replyID = fix('draft', '1', agentName, reply, PREVIEW_REPLY);
      const postID = fix('outbound', '1', agentName, x, 'x', PREVIEW_POST, 'launch/hero-1.png');
      await page.route(/\/plugins\/drafts\/\d+\/media\/\d+$/, (route) => route.fulfill({ status: 200, contentType: 'image/png', body: pngBytes }));
      await signIn(page, origin);
      await page.goto(`${origin}/attention?tab=drafts`);

      const r = await expectThePost(page, replyID, 'other', 0);
      await expect(r.locator('[data-draft-original] [data-draft-untrusted]')).toBeVisible();
      const flag = r.locator('mark[data-stepup-hidden="U+202E"]');
      await expect(flag).toBeVisible();
      await expect(flag).toHaveText('U+202E');
      await expect(r.locator('[data-stepup-hidden-note]')).toBeVisible();
      await expect(r.locator('[data-stepup-preview-text] p').first()).toHaveText('Thanks — noon works 🎉');

      const p = await expectThePost(page, postID, 'x', 1);
      await expect(p.locator('[data-stepup-venue]')).toHaveText('X');
      await expect(p.locator('[data-stepup-post-kind]')).toHaveText('New post');
      // Details opens on a tap, with or without script (a native <details>).
      await p.locator('[data-stepup-details-toggle]').click();
      await expect(p.locator('[data-draft-medium-facts]')).toBeVisible();
      await expect(p.locator('[data-draft-medium-facts]')).toContainText('image/png');
      await noSidewaysScroll(page);
    });
  });
}
