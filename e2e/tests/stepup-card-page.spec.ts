import { execFileSync } from 'node:child_process';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { dsn, repoRoot, stepupCardPagePort, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// A STEP-UP CONFIRMATION CARD'S PAGE (internal/api host_stepup_card.go), in a
// real browser against a real muster-server, at a phone (390×844, touch): the
// page a card's notification opens shows WHY it is asked and a PREVIEW of what
// it does — the title (the draft's id in it), the why, who asked, and the post
// as a mock of it on X: X's logo, its text as paragraphs with real line breaks
// (markup in it drawn as text), its image below — and folds the boilerplate
// (the machine, the session, the call in words) into a Details toggle that is
// CLOSED until tapped. No face shows a media type, size or hash, or a spelled
// escape. Nothing scrolls sideways.
//
// 🔴 RED AT origin/main: there a card has no parts (no attention_asks.step_up,
// no data-stepup-*), and its page prints the whole question — host, session,
// the call's raw JSON and the hash — as one paragraph.
//
// 🔴 RED AT asks-ui-followup (c8473fefb): the post was a "Post on X" pill over
// one bare box of text, with no logo, and the title had no id.
//
// The card is a fixture row (e2e/stepupfix): what is under test is how the
// page DRAWS a card; the Go tests drive how the gate files one. Its draft has
// no row here, so the spec serves the draft's one medium itself (a 1×1 PNG).

const origin = `http://localhost:${stepupCardPagePort}`;
let server: Running | undefined;

const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgaGD4DwAChAGAkdXSbQAAAABJRU5ErkJggg==', 'base64');
const serveMedium = (page: import('@playwright/test').Page) =>
  page.route('**/plugins/drafts/41/media/0', (route) => route.fulfill({ status: 200, contentType: 'image/png', body: png }));

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/stepupfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  server = await startServer(versionA, stepupCardPagePort, { MUSTER_FOCUS_INTERVAL: '0' });
});

test.afterAll(async () => {
  try {
    fix('clear', '1');
  } finally {
    await server?.stop();
  }
});

test.describe('phone 390×844', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('a step-up card shows why and the post, with the boilerplate folded into Details', async ({ page }, testInfo) => {
    const id = fix('card', '1');
    await signIn(page, origin);
    await serveMedium(page);
    await page.goto(`${origin}/attention/asks/${id}`);
    const card = page.locator('[data-stepup-card]');
    await expect(card).toBeVisible();

    await expect(card.locator('[data-stepup-title]')).toHaveText('Post to X? · Draft #41');
    await expect(card.locator('[data-stepup-why]')).toHaveText('Marketing: rebrand announcement');
    await expect(card.locator('[data-stepup-asker]')).toHaveText('Asked by work:1');
    // The post, as X shows it: X's logo and name, what it is, the text, the image.
    const post = card.locator('[data-stepup-post]');
    await expect(post).toBeVisible();
    await expect(post.locator('[data-stepup-logo] svg[data-venue-logo="x"]')).toBeVisible();
    await expect(post.locator('[data-stepup-venue]')).toHaveText('X');
    await expect(post.locator('[data-stepup-post-kind]')).toHaveText('New post');
    const paras = post.locator('[data-stepup-preview-text] p');
    await expect(paras).toHaveText(['Big news: acme is now acme-two.\nRead more on the blog. <b>not bold</b>', 'A second paragraph.']);
    // A line break is DRAWN as one (white-space: pre-wrap keeps it, and the
    // rendered text holds it), never spelled.
    expect(await paras.first().evaluate((p) => getComputedStyle(p).whiteSpace)).toBe('pre-wrap');
    expect(await paras.first().innerText()).toBe('Big news: acme is now acme-two.\nRead more on the blog. <b>not bold</b>');
    // The officer's markup is text: no element was made of it.
    await expect(post.locator('[data-stepup-preview-text] b')).toHaveCount(0);
    const img = post.locator('[data-stepup-media] img[data-stepup-image]');
    await expect(img).toBeVisible();
    expect(await img.evaluate((i) => (i as HTMLImageElement).naturalWidth), 'the image loaded').toBe(1);
    const box = await post.boundingBox();
    const ibox = await img.boundingBox();
    expect(ibox!.y, 'the image is below the text').toBeGreaterThan((await paras.last().boundingBox())!.y);
    expect(ibox!.width, 'the image spans the post').toBeGreaterThan(box!.width - 40);

    // Details: present, CLOSED, its text hidden until tapped.
    const details = card.locator('[data-stepup-details]');
    await expect(details).toHaveCount(1);
    expect(await details.evaluate((d) => (d as HTMLDetailsElement).open), 'Details is closed by default').toBe(false);
    const detailsText = card.locator('[data-stepup-details-text]');
    await expect(detailsText).toBeHidden();

    // The visible card says nothing of the boilerplate: no JSON, no host, no
    // hash, no media type, no spelled escape.
    const visible = await card.innerText();
    for (const never of ['{', 'plugin_draft_decide', 'host-a', '0a0a0a', 'image/png', 'sha', '\\n', 'Post on X', 'paired phone', 'any process there']) {
      expect(visible, `the card's face shows ${never}`).not.toContain(never);
    }
    await expect(page.locator('[data-ask-question]')).toHaveCount(0);

    // 🔴 THE BROWSER CANNOT ANSWER IT (errHostDecisionNeedsPhone): no
    // Confirm/Refuse, no answer box, no mic — one line says where it is
    // answered — and it is headed by its action, never "step-up is asking".
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('Post to X? · Draft #41');
    await expect(page.locator('[data-ask-stepup-phone]')).toHaveText('Confirm this on your phone.');
    await expect(page.locator('[data-ask-option]')).toHaveCount(0);
    await expect(page.locator('#ask-input, #ask-send')).toHaveCount(0);
    expect(await page.locator('body').innerText(), 'the internal asker label is on screen').not.toContain('step-up');

    const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
    expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(iw);
    const shut = testInfo.outputPath('stepup-card-phone.png');
    await page.screenshot({ path: shut, fullPage: true });
    await testInfo.attach('stepup-card-phone.png', { path: shut, contentType: 'image/png' });

    // Tapped open: the record is all there, and the chevron has turned.
    const chevron = card.locator('[data-stepup-details-toggle] .asks-chevron');
    await expect(chevron).toBeVisible();
    expect(await chevron.evaluate((e) => getComputedStyle(e).transform), 'closed: the chevron points down').toBe('none');
    await card.locator('[data-stepup-details-toggle]').tap();
    await expect(detailsText).toBeVisible();
    await expect.poll(() => chevron.evaluate((e) => getComputedStyle(e).transform), { message: 'open: the chevron has turned' }).not.toBe('none');
    await expect(detailsText).toContainText('A session on host-a (work:1 @ host-a)');
    await expect(detailsText).toContainText('The call: plugin_draft_decide, with id 41, decision “approve”.');
    for (const never of ['{', 'sha', '0a0a0a', 'image/png']) {
      expect(await detailsText.innerText(), `Details shows ${never}`).not.toContain(never);
    }
    const opened = testInfo.outputPath('stepup-card-phone-details.png');
    await page.screenshot({ path: opened, fullPage: true });
    await testInfo.attach('stepup-card-phone-details.png', { path: opened, contentType: 'image/png' });
  });
});

// THE FEED MATCHES THE PAGE (the asks audit): a step-up card on the Asks feed
// draws no Confirm/Refuse, no answer box and no mic — only the paired phone
// answers it — and says "Confirm this on your phone." as a plain status line,
// on the feed and on the page alike: no border, no fill, nothing that reads
// as tappable. RED before: the feed drew Confirm, Refuse and the box, and the
// page drew the line in a bordered pill.
for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`the feed, ${vp.name}`, () => {
    test.use(vp.use);
    test('a step-up card on the feed has no answer controls, and its phone line is plain text', async ({ page }) => {
      const id = fix('card', '1');
      await signIn(page, origin);
      await serveMedium(page);
      await page.goto(`${origin}/attention`);
      const card = page.locator(`#ask-${id}`);
      await expect(card).toBeVisible();
      // The feed draws the card's title (with the id) and a compact mock of
      // the post: X's logo, its text, its image as a thumbnail.
      await expect(card.locator('[data-stepup-title]')).toHaveText('Post to X? · Draft #41');
      await expect(card.locator('[data-stepup-post] svg[data-venue-logo="x"]')).toBeVisible();
      await expect(card.locator('[data-stepup-preview-text]')).toHaveText(
        'Big news: acme is now acme-two.\nRead more on the blog. <b>not bold</b>\n\nA second paragraph.');
      await expect(card.locator('[data-stepup-media] img')).toBeVisible();
      const face = await card.innerText();
      for (const never of ['{', 'image/png', 'sha', '0a0a0a', '\\n']) {
        expect(face, `the feed card shows ${never}`).not.toContain(never);
      }
      const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
      expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(iw);
      await expect(card.locator('[data-attention-option]')).toHaveCount(0);
      await expect(card.getByRole('button', { name: 'Confirm', exact: true })).toHaveCount(0);
      await expect(card.getByRole('button', { name: 'Refuse', exact: true })).toHaveCount(0);
      await expect(card.locator('textarea, [data-asks-send], button[data-dictate]')).toHaveCount(0);
      const line = card.locator('[data-ask-stepup-phone]');
      await expect(line).toHaveText('Confirm this on your phone.');
      await expect(card.getByRole('button', { name: 'Dismiss without answering' }), 'Dismiss stays').toBeVisible();
      const plain = (el: Element) => {
        const st = getComputedStyle(el);
        return { tag: el.tagName, border: st.borderTopWidth, shadow: st.boxShadow, bg: st.backgroundColor, weight: st.fontWeight };
      };
      for (const where of [line, null]) {
        if (where === null) {
          await page.goto(`${origin}/attention/asks/${id}`);
        }
        const l = where ?? page.locator('[data-ask-stepup-phone]');
        await expect(l).toHaveText('Confirm this on your phone.');
        const st = await l.evaluate(plain);
        expect(st, 'a status line, not a control').toEqual({ tag: 'P', border: '0px', shadow: 'none', bg: 'rgba(0, 0, 0, 0)', weight: '400' });
      }
    });
  });
}

// A LONG X THREAD, AND LOOK-ALIKE LETTERS (the step-up preview audit), on the
// page and the feed, at a phone and a desktop.
//
// The thread: 20 posts, of which the card holds the first 9 whole ones. It
// says the WHOLE count (Thread · 20 posts, 1/20 … 9/20, "+19 more posts in
// this thread"), no post ends in a cut "---8<-", and the excerpt line is drawn
// OUTSIDE the post's mock. 🔴 RED at 037fe5724: the card was cut before it was
// split, so it read "Thread · 9 posts", its last post ended "---8<-…" with the
// excerpt warning inside it, and the feed said "+8 more posts" and no more.
//
// The look-alikes: "pаypal.com" (a Cyrillic а) and a struck-through word
// (U+0336) are highlighted, one line under the post says so, and the
// pure-Cyrillic word, "café" and the emoji are drawn as they are, unmarked.
// 🔴 RED at 037fe5724: nothing was marked, and "pаypal.com" read as
// "paypal.com".
const shotsTo = process.env.MUSTER_STEPUP_SHOTS ?? '';
for (const vp of [
  { name: 'phone 390×844', key: 'mobile', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', key: 'desktop', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`the preview audit, ${vp.name}`, () => {
    test.use(vp.use);
    const shoot = async (page: import('@playwright/test').Page, testInfo: import('@playwright/test').TestInfo, name: string) => {
      const file = testInfo.outputPath(`${name}-${vp.key}.png`);
      await page.screenshot({ path: file, fullPage: true });
      await testInfo.attach(`${name}-${vp.key}.png`, { path: file, contentType: 'image/png' });
      if (shotsTo) {
        (await import('node:fs')).copyFileSync(file, `${shotsTo}/${name}-${vp.key}.png`);
      }
    };

    test('a long X thread counts the whole thread, and says it is an excerpt outside the post', async ({ page }, testInfo) => {
      const id = fix('thread', '1');
      await signIn(page, origin);
      await serveMedium(page);
      await page.goto(`${origin}/attention/asks/${id}`);
      const card = page.locator('[data-stepup-card]');
      const post = card.locator('[data-stepup-post]');
      await expect(post.locator('[data-stepup-post-kind]')).toHaveText('Thread · 20 posts');
      await expect(post.locator('[data-stepup-part-n]')).toHaveText(['1/20', '2/20', '3/20', '4/20', '5/20', '6/20', '7/20', '8/20', '9/20']);
      const postText = await post.innerText();
      for (const never of ['8<', 'EXCERPT', 'Refuse unless']) {
        expect(postText, `the post's mock carries ${never}`).not.toContain(never);
      }
      const excerpt = card.locator('[data-stepup-excerpt]');
      await expect(excerpt).toHaveText("⚠ AN EXCERPT: the first 9 of the thread's 20 posts. Refuse unless you have read the rest in muster's web UI.");
      await expect(post.locator('[data-stepup-excerpt]')).toHaveCount(0);
      const pbox = (await post.boundingBox())!;
      expect((await excerpt.boundingBox())!.y, 'the excerpt line is under the post').toBeGreaterThanOrEqual(pbox.y + pbox.height);
      await excerpt.scrollIntoViewIfNeeded();
      await shoot(page, testInfo, 'audit-thread-page');

      await page.goto(`${origin}/attention`);
      const fcard = page.locator(`#ask-${id}`);
      await expect(fcard.locator('[data-stepup-post-kind]')).toHaveText('Thread · 20 posts');
      await expect(fcard.locator('[data-stepup-thread-more]')).toHaveText('+19 more posts in this thread');
      const fpost = fcard.locator('[data-stepup-post]');
      await expect(fpost.locator('[data-stepup-excerpt]')).toHaveCount(0);
      await expect(fcard.locator('[data-stepup-excerpt]')).toHaveCount(1);
      await expect(fcard.locator('[data-stepup-excerpt]')).toBeVisible();
      const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
      expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(iw);
      await fcard.scrollIntoViewIfNeeded();
      await shoot(page, testInfo, 'audit-thread-feed');
    });

    test('look-alike letters are highlighted and said, ordinary text is not', async ({ page }, testInfo) => {
      const id = fix('lookalike', '1');
      await signIn(page, origin);
      await serveMedium(page);
      for (const where of ['page', 'feed']) {
        await page.goto(where === 'page' ? `${origin}/attention/asks/${id}` : `${origin}/attention`);
        const root = where === 'page' ? page.locator('[data-stepup-card]') : page.locator(`#ask-${id}`);
        const marks = root.locator('[data-stepup-post] mark[data-stepup-lookalike]');
        await expect(marks).toHaveText(['pаypal.com/refund', 'S̶A̶L̶E̶']);
        await expect(marks.first()).toBeVisible();
        const note = root.locator('[data-stepup-lookalike-note]');
        await expect(note).toHaveText('Contains look-alike letters — check the highlighted words.');
        await expect(root.locator('[data-stepup-post] [data-stepup-lookalike-note]')).toHaveCount(0);
        // The marked word reads differently from its neighbours: a tinted
        // background behind it.
        expect(await marks.first().evaluate((m) => getComputedStyle(m).backgroundColor), 'the mark is tinted').not.toBe('rgba(0, 0, 0, 0)');
        await root.scrollIntoViewIfNeeded();
        await shoot(page, testInfo, `audit-lookalike-${where}`);
      }
    });
  });
}
