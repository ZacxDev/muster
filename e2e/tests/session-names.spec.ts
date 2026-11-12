import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { dsn, repoRoot, sessionNamesPort } from '../lib/env';
import { cc, MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';
import { revealBox } from '../lib/composer';

// A SESSION'S STABLE NAME (docs/focus.md#a-sessions-name), in a real browser
// against a real muster-server (MUSTER_MIRROR=1, its own port, the focus pass
// off) and a real muster-mirror over the tunnel (the `-tags e2emirror` fake),
// on a phone (390×844, touch) and a desktop (1280×800).
//
// Focus is written by e2e/mkchat exactly as the refresh writes it: the name
// bar is judged by focus.Nameable over the tail the spec hands it, in the same
// upsert. Each viewport has a window (and so a conversation) of its own in the
// tmux session the host labels with the SYNTHETIC codename "walnut".
//
//   1. a greeting freezes no name: the row is called by its codename;
//   2. the first good focus title becomes the name — on the sidebar row
//      (with the live focus as its second line), in the Rendered view's
//      header and its <title> — and the codename and host leave the row's
//      text for its tooltip;
//   3. a later focus moves the live line and NOT the name;
//   4. Rename… in the session's ⋯ menu renames it; the name survives a
//      reload and a later nameable focus (never overwritten);
//   5. a session whose focus moved on is OFFERED a rename in a one-row
//      banner;
//   6. Keep / Rename are answered in place — no reload;
//   7. on a Terminal page too: its ⋯ Rename… renames its own header and tab,
//      and its framed Rendered view's Rename retitles the top tab.

const rig = new MirrorRig(sessionNamesPort, 'e2e-names');
const origin = rig.origin;
const host = rig.host;
const stamp = Date.now().toString(36);
const agentName = 'e2e-names-' + stamp;
const windowsFile = path.join(rig.dir, 'windows');
const labelsFile = path.join(rig.dir, 'labels.json');
const now = Math.floor(Date.now() / 1000);
const sids = { phone: 'names-phone-' + stamp, desktop: 'names-desktop-' + stamp };
const windows = { phone: 1, desktop: 2 };
// A second window per viewport for the drift offer's layout (5.), and a third
// for its in-place answers (6.), each its own conversation.
const driftSids = { phone: 'drift-phone-' + stamp, desktop: 'drift-desktop-' + stamp };
const driftWindows = { phone: 3, desktop: 4 };
const inPlaceSids = { phone: 'inplace-phone-' + stamp, desktop: 'inplace-desktop-' + stamp };
const inPlaceWindows = { phone: 5, desktop: 6 };
// A fourth for the Terminal page (7.): its own dialog, and its framed
// Rendered view's offer.
const termSids = { phone: 'term-phone-' + stamp, desktop: 'term-desktop-' + stamp };
const termWindows = { phone: 7, desktop: 8 };
const termView = (w: number) => `/mirror/terminal?host=${host}&session=namesx&window=%40${w}`;
// The drift test's second offer: long enough to wrap on a phone, short enough
// to be a focus title (focus.TitleCapRunes, 60).
const longOffer = 'Wiring the Stripe refund flow for partial capture retries';
const view = (w: number) => `/mirror/view?host=${host}&session=namesx&window=%40${w}`;

const REAL_TAIL = [
  { role: 'user', text: 'Migrate the billing tables to the v2 schema' },
  { role: 'assistant', text: 'Reading the current schema first.' },
];
const GREETING_TAIL = [
  { role: 'user', text: 'hi' },
  { role: 'assistant', text: 'Hello! How can I help?' },
];

type FocusSpec = {
  subject: string;
  title: string;
  description?: string;
  tail?: { role: string; text: string }[];
  historyTitles?: string[];
  historyAgeMinutes?: number;
};

function mkchat(env: NodeJS.ProcessEnv, input = '') {
  execFileSync('go', ['run', './e2e/mkchat'], { cwd: repoRoot, input, env: { ...process.env, DATABASE_URL: dsn, MUSTER_E2E_AGENT: agentName, ...env } });
}
const writeFocus = (specs: FocusSpec[]) => mkchat({ MUSTER_E2E_FOCUS_ONLY: '1', MUSTER_E2E_FOCUS: JSON.stringify(specs) });

test.beforeAll(async () => {
  await rig.start();
  mkchat({}, JSON.stringify([{ role: 'user', content: 'Hold the fixtures.' }]));
  const rows: string[] = [];
  for (const k of ['phone', 'desktop'] as const) {
    for (const [sid, w] of [[sids[k], windows[k]], [driftSids[k], driftWindows[k]], [inPlaceSids[k], inPlaceWindows[k]], [termSids[k], termWindows[k]]] as const) {
      const tr = path.join(rig.dir, sid + '.jsonl');
      fs.writeFileSync(tr, cc.prompt('Migrate the billing tables') + cc.reply('Reading the schema.'));
      rows.push(`namesx|@${w}|${w}|${w === 1 ? 1 : 0}|claude|${now - 60 * w}|${tr}`);
    }
  }
  writeFileAtomic(windowsFile, rows.join('\n') + '\n');
  fs.writeFileSync(labelsFile, JSON.stringify({ namesx: 'walnut' }));
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile, MUSTER_MIRROR_LABELS_FILE: labelsFile, MUSTER_MIRROR_INTERACTIVE: '1' });
});

test.afterAll(async () => {
  await rig.stop();
  mkchat({ MUSTER_E2E_DELETE: '1', MUSTER_E2E_FOCUS: JSON.stringify([...Object.values(sids), ...Object.values(driftSids), ...Object.values(inPlaceSids), ...Object.values(termSids)].map((s) => ({ subject: 'mirror:' + s, title: 'x' }))) });
});

// rowOf is the sidebar row of window w (its <a>), from the live sidebar.
const rowOf = (page: Page, w: number): Locator => page.locator(`#sidebar-agents-list a[href*="window=%40${w}"]`);

// closeSidebar closes an overlay sidebar and WAITS for it to be closed: its
// backdrop takes taps while it is open (measured: a ⋯ tap right after the
// close landed on the backdrop).
async function closeSidebar(page: Page) {
  const root = page.locator('#sidebar-root');
  if ((await root.getAttribute('data-open')) !== 'true') return;
  if (await page.locator('#sidebar-close').isVisible()) await page.locator('#sidebar-close').click();
  else await page.keyboard.press('Escape');
  await expect(root).not.toHaveAttribute('data-open', 'true');
}

async function openSidebar(page: Page) {
  const open = page.locator('#sidebar-open');
  if (await open.isVisible()) await open.click();
}

// BANNER HEIGHTS (operator: the stacked three-row card was too tall). On a
// desktop the offer is ONE row: its 44px buttons are its height, plus its
// border. On a phone it may take two rows at most, never more.
const ONE_LINE_MAX = 48;
const TWO_LINES_MAX = 2 * 44 + 4;

// expectCompactOffer: the rename offer is one row on a desktop (≤ 2 on a
// phone): the label, the offered name, "Rename" and "Keep" all inside the
// banner, the name truncated with an ellipsis (its whole text in its tooltip)
// rather than wrapping, the banner and the page with no sideways overflow;
// "Keep" is a text link (no fill, no ring) and both are 44px targets.
// Returns the banner's height, which the spec reports.
async function expectCompactOffer(page: Page, banner: Locator, phone: boolean, offer: string): Promise<number> {
  // The height FIRST, from the banner alone: it is the operator's complaint,
  // and it is measurable on any banner (the stacked one too).
  const box = (await banner.boundingBox())!;
  expect(box.height, `the banner is ${phone ? 'at most two rows' : 'one row'} (${box.height}px)`).toBeLessThanOrEqual(phone ? TWO_LINES_MAX : ONE_LINE_MAX);
  const name = banner.locator('[data-session-drift-offer]');
  const accept = (await banner.locator('[data-session-drift-accept]').boundingBox())!;
  const keep = (await banner.locator('[data-session-drift-dismiss]').boundingBox())!;
  const named = (await name.boundingBox())!;
  if (!phone) {
    // One row: the name and both buttons share a line (their boxes overlap
    // vertically), Rename before Keep.
    for (const [what, b] of [['Rename', accept], ['Keep', keep]] as const) {
      expect(b.y, `${what} is on the name's row`).toBeLessThan(named.y + named.height);
      expect(b.y + b.height, `${what} is on the name's row`).toBeGreaterThan(named.y);
    }
    expect(accept.x + accept.width, 'Rename is before Keep').toBeLessThanOrEqual(keep.x + 0.5);
  }
  for (const [what, b] of [['the name', named], ['Rename', accept], ['Keep', keep]] as const) {
    expect(b.x, `${what} starts inside the banner`).toBeGreaterThanOrEqual(box.x - 0.5);
    expect(b.x + b.width, `${what} ends inside the banner`).toBeLessThanOrEqual(box.x + box.width + 0.5);
    expect(b.y + b.height, `${what} ends inside the banner`).toBeLessThanOrEqual(box.y + box.height + 0.5);
  }
  for (const b of [accept, keep]) expect(b.height, 'a 44px target').toBeGreaterThanOrEqual(44);
  await expect(name, 'the whole name is the tooltip').toHaveAttribute('title', offer);
  const cut = await name.evaluate((el) => ({ ws: getComputedStyle(el).whiteSpace, to: getComputedStyle(el).textOverflow }));
  expect(cut, 'the name truncates with an ellipsis rather than wrapping').toEqual({ ws: 'nowrap', to: 'ellipsis' });
  const o = await banner.evaluate((el) => ({ sw: el.scrollWidth, cw: el.clientWidth, dw: document.documentElement.scrollWidth, iw: window.innerWidth }));
  expect(o.sw, 'the banner does not overflow sideways').toBeLessThanOrEqual(o.cw);
  expect(o.dw, 'the page does not scroll sideways').toBeLessThanOrEqual(o.iw);
  const look = await banner.locator('[data-session-drift-dismiss]').evaluate((el) => {
    const c = getComputedStyle(el);
    return { bg: c.backgroundColor, shadow: c.boxShadow, line: c.textDecorationLine };
  });
  expect(look.bg, 'Keep has no fill').toBe('rgba(0, 0, 0, 0)');
  expect(look.shadow, 'Keep has no ring').toBe('none');
  expect(look.line, 'not underlined at rest').toBe('none');
  return box.height;
}

// NO RELOAD (operator: accepting or keeping the offer reloaded the whole
// page). armNoReload marks the document and starts counting main-frame
// navigations, and types a draft into the composer; expectNoReload then
// checks the SAME document is still there (the marker), nothing navigated, and
// the draft is as typed. The marker is the discriminating check: the composer
// keeps a draft across a reload on its own (it is stored), so the draft alone
// could not see one.
async function markPage(page: Page) {
  const navs: string[] = [];
  page.on('framenavigated', (f) => { if (f === page.mainFrame()) navs.push(f.url()); });
  await page.evaluate(() => { (window as unknown as { __driftMarker: string }).__driftMarker = 'still-here'; });
  return navs;
}

async function expectSamePage(page: Page, navs: string[], what: string) {
  expect(await page.evaluate(() => (window as unknown as { __driftMarker?: string }).__driftMarker), `${what}: the page was not reloaded`).toBe('still-here');
  expect(navs, `${what}: nothing navigated the page`).toEqual([]);
}

// focusedId is the id of the element focus is on (a rename moves it to the
// page's title, not <body>).
const focusedId = (page: Page) => page.evaluate(() => document.activeElement?.id ?? '');

async function armNoReload(page: Page, draft: string) {
  const navs = await markPage(page);
  // A touch screen's composer starts collapsed (internal/ui/composer_reveal.go).
  await revealBox(page);
  await page.locator('#mv-input').fill(draft);
  return navs;
}

async function expectNoReload(page: Page, navs: string[], draft: string, what: string) {
  await expectSamePage(page, navs, what);
  await expect(page.locator('#mv-input'), `${what}: the typed draft is kept`).toHaveValue(draft);
}

for (const vp of [
  { key: 'phone' as const, name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { key: 'desktop' as const, name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });
    const w = windows[vp.key];
    const subject = 'mirror:' + sids[vp.key];

    test('the first good focus title names the session, a later one moves only the live line, and a rename sticks', async ({ page }) => {
      await signIn(page, origin);
      await rig.waitListed(page, `walnut:${w}`);

      // 1. A greeting freezes nothing: the row is still called by its codename.
      writeFocus([{ subject, title: 'Greeting the user', tail: GREETING_TAIL }]);
      await page.goto(origin + view(w));
      await openSidebar(page);
      await expect(rowOf(page, w).locator('[data-nav-name]')).toHaveText(`walnut:${w}`);

      // 2. The first good title becomes the name.
      writeFocus([{ subject, title: 'Billing migration', tail: REAL_TAIL }]);
      await page.goto(origin + view(w));
      await expect(page.locator('header [data-session-name="auto"]')).toHaveText('Billing migration');
      await expect(page).toHaveTitle(/^Billing migration/);
      await openSidebar(page);
      const row = rowOf(page, w);
      await expect(row.locator('[data-nav-name]')).toHaveText('Billing migration');
      await expect(row.locator('[data-nav-focus]')).toContainText('Billing migration');
      // The technical ids are off the row's text, and in its tooltip.
      const text = (await row.innerText()).toLowerCase();
      expect(text, 'the codename is not on the row').not.toContain('walnut');
      expect(text, 'the host is not on the row').not.toContain(host.toLowerCase());
      await expect(row).toHaveAttribute('title', new RegExp(`^Billing migration · walnut:${w} · tmux namesx:${w} @ ${host}$`));

      // 3. A later title moves the live line, not the name.
      writeFocus([{ subject, title: 'Fixing the Stripe webhook retry', tail: [...REAL_TAIL, { role: 'user', text: 'now the webhooks' }, { role: 'assistant', text: 'Looking.' }] }]);
      await page.goto(origin + view(w));
      await expect(page.locator('header [data-session-name="auto"]')).toHaveText('Billing migration');
      await openSidebar(page);
      await expect(rowOf(page, w).locator('[data-nav-name]')).toHaveText('Billing migration');
      await expect(rowOf(page, w).locator('[data-nav-focus]')).toContainText('Fixing the Stripe webhook retry');
      await closeSidebar(page);

      // 4. Rename… in the session's ⋯ menu.
      await page.locator('#switcher-more-btn').first().click();
      await page.locator('#switcher-rename').click();
      const dialog = page.locator('#session-rename');
      await expect(dialog).toBeVisible();
      const input = dialog.locator('input[name="name"]');
      await expect(input, 'prefilled with what it is called now').toHaveValue('Billing migration');
      await input.fill(`Payments ${vp.key}`);
      // IN PLACE, as the offer's Rename: the dialog closes, the header and the
      // tab read the name, focus is on the title, and nothing reloaded.
      const navs = await markPage(page);
      await dialog.getByRole('button', { name: 'Save', exact: true }).click();
      await expect(dialog).toBeHidden();
      await expect(page.locator('header [data-session-name="manual"]')).toHaveText(`Payments ${vp.key}`);
      await expect(page).toHaveTitle(`Payments ${vp.key} · Mirrored · muster`);
      await expectSamePage(page, navs, 'the dialog Save');
      await expect.poll(() => focusedId(page), 'focus is on the title').toBe('mv-heading');
      page.removeAllListeners('framenavigated');
      await page.reload();
      await expect(page.locator('header [data-session-name="manual"]'), 'the name survives a reload').toHaveText(`Payments ${vp.key}`);
      // A later focus that would freeze a name changes nothing.
      writeFocus([{ subject, title: 'Shipping the invoice exporter', tail: [...REAL_TAIL, { role: 'user', text: 'and the exporter' }, { role: 'assistant', text: 'On it.' }] }]);
      await page.reload();
      await expect(page.locator('header [data-session-name="manual"]'), 'never overwritten').toHaveText(`Payments ${vp.key}`);
      await expect(page).toHaveTitle(new RegExp(`^Payments ${vp.key}`));
      await openSidebar(page);
      await expect(rowOf(page, w).locator('[data-nav-name]')).toHaveText(`Payments ${vp.key}`);
      await expect(rowOf(page, w).locator('[data-nav-focus]')).toContainText('Shipping the invoice exporter');

      // 5. The palette still finds it by its codename, and calls it by its name.
      await page.goto(origin + '/tasks');
      await openSidebar(page);
      await page.locator('#sidebar-search').click();
      const options = page.locator('#switcher-search-results [role="option"]');
      await page.locator('#switcher-search-input').fill(`walnut:${w}`);
      await expect(options).toHaveCount(1);
      await expect(options.first().locator('span').first()).toHaveText(`Payments ${vp.key}`);
    });

    // DRIFT: the focus has been elsewhere for a while — a rename is OFFERED,
    // never made, in ONE compact row (two at most on a phone), a long offer
    // cut with an ellipsis.
    test('a session whose work moved on is offered a rename in one compact row', async ({ page }, testInfo) => {
      const dw = driftWindows[vp.key];
      const dsub = 'mirror:' + driftSids[vp.key];
      const phone = vp.key === 'phone';
      await signIn(page, origin);
      await rig.waitListed(page, `walnut:${dw}`);
      writeFocus([{ subject: dsub, title: 'Billing migration', tail: REAL_TAIL }]);
      const banner = page.locator('#session-drift');
      // CONTROL: named, on-topic — no offer.
      await page.goto(origin + view(dw));
      await expect(page.locator('header [data-session-name="auto"]')).toHaveText('Billing migration');
      await expect(banner).toHaveCount(0);
      // Three changes about something else, the oldest an hour ago.
      writeFocus([{ subject: dsub, title: 'Fixing the Stripe webhook retry', tail: REAL_TAIL,
        historyTitles: ['Tracing the webhook retry queue', 'Reading the Stripe webhook docs'], historyAgeMinutes: 60 }]);
      await page.goto(origin + view(dw));
      await expect(banner).toHaveAttribute('data-session-drift', 'Fixing the Stripe webhook retry');
      await expect(page.locator('header [data-session-name="auto"]'), 'offered, not renamed').toHaveText('Billing migration');
      await banner.screenshot({ path: testInfo.outputPath(`drift-banner-${vp.key}.png`) });
      const short = await expectCompactOffer(page, banner, phone, 'Fixing the Stripe webhook retry');
      const keep = banner.locator('[data-session-drift-dismiss]');
      const accept = banner.locator('[data-session-drift-accept]');
      await expect(banner.locator('[data-session-drift-offer]')).toHaveText('Fixing the Stripe webhook retry');
      await expect(accept).toHaveText('Rename');
      await expect(accept).toHaveAccessibleName('Rename to “Fixing the Stripe webhook retry”');
      await expect(keep).toHaveText('Keep');
      await expect(keep).toHaveAccessibleName('Keep the current name');
      if (!phone) {
        // A link: underlined on hover.
        await keep.hover();
        await expect.poll(() => keep.evaluate((el) => getComputedStyle(el).textDecorationLine)).toBe('underline');
      }
      // A LONG offer (57 characters), the newest of three diverged titles: it
      // is cut inside the banner, still one row on a desktop, nothing sideways.
      writeFocus([{ subject: dsub, title: longOffer, tail: REAL_TAIL }]);
      await page.goto(origin + view(dw));
      await expect(banner).toHaveAttribute('data-session-drift', longOffer);
      await banner.screenshot({ path: testInfo.outputPath(`drift-banner-long-${vp.key}.png`) });
      const long = await expectCompactOffer(page, banner, phone, longOffer);
      // CONTROL that the long case IS long on a phone: the name is cut (its
      // text is wider than its box), so the no-overflow checks above saw a
      // truncated name, not one that happened to fit. A desktop has the room
      // to draw it whole, and does.
      expect(await banner.locator('[data-session-drift-offer]').evaluate((el) => el.scrollWidth > el.clientWidth),
        phone ? 'the long name is truncated on a phone' : 'the long name fits on a desktop').toBe(phone);
      testInfo.annotations.push({ type: 'banner height', description: `${vp.key}: ${short}px short, ${long}px long` });
    });

    // IN PLACE (operator: accepting or keeping the offer reloaded the whole
    // page). "Keep" and "Rename" change the page without reloading it — the
    // draft, the stream and the scroll stay; "Keep" holds until the focus
    // changes; "Rename" renames it (a manual name) and every name on the page
    // follows. Only the offer's buttons are found by what main drew too
    // ([data-session-drift-dismiss|accept]), so the no-reload check is what
    // a reload fails first.
    test('the offer is kept or accepted in place, with no reload', async ({ page }) => {
      const iw = inPlaceWindows[vp.key];
      const isub = 'mirror:' + inPlaceSids[vp.key];
      await signIn(page, origin);
      await rig.waitListed(page, `walnut:${iw}`);
      writeFocus([{ subject: isub, title: 'Billing migration', tail: REAL_TAIL }]);
      writeFocus([{ subject: isub, title: 'Fixing the Stripe webhook retry', tail: REAL_TAIL,
        historyTitles: ['Tracing the webhook retry queue', 'Reading the Stripe webhook docs'], historyAgeMinutes: 60 }]);
      const banner = page.locator('#session-drift');
      const status = page.locator('#session-drift-status');
      const headerName = page.locator('header h1 [data-mirror-name]');
      await page.goto(origin + view(iw));
      await expect(banner).toHaveAttribute('data-session-drift', 'Fixing the Stripe webhook retry');
      await expect(headerName).toHaveText('Billing migration');
      // Keep: gone IN PLACE, and still gone after a reload.
      let navs = await armNoReload(page, `draft kept ${vp.key}`);
      await banner.locator('[data-session-drift-dismiss]').click();
      await expect(banner).toHaveCount(0);
      await expectNoReload(page, navs, `draft kept ${vp.key}`, 'Keep');
      await expect(status, 'the change is announced').toHaveText('Kept the name.');
      await expect.poll(() => focusedId(page), 'Keep: focus is on the title, not <body>').toBe('mv-heading');
      await expect(headerName, 'Keep renames nothing').toHaveText('Billing migration');
      page.removeAllListeners('framenavigated');
      await page.reload();
      await expect(banner, 'the dismissal is stored').toHaveCount(0);
      // A new focus title releases the dismissal.
      writeFocus([{ subject: isub, title: longOffer, tail: REAL_TAIL }]);
      await page.goto(origin + view(iw));
      await expect(banner).toHaveAttribute('data-session-drift', longOffer);
      // Rename: one tap renames it IN PLACE; the name is the person's now,
      // on every surface the page draws it — and it survives a reload.
      navs = await armNoReload(page, `draft renamed ${vp.key}`);
      await banner.locator('[data-session-drift-accept]').click();
      await expect(banner).toHaveCount(0);
      await expectNoReload(page, navs, `draft renamed ${vp.key}`, 'Rename');
      await expect(page.locator('header [data-session-name="manual"]')).toHaveText(longOffer);
      await expect(page).toHaveTitle(`${longOffer} · Mirrored · muster`);
      await expect(page.locator('#mv-title-full'), 'the full title follows').toContainText(longOffer);
      await expect(status, 'the change is announced').toHaveText(`Renamed to “${longOffer}”.`);
      await expect.poll(() => focusedId(page), 'Rename: focus is on the title, not <body>').toBe('mv-heading');
      // The ⋯ menu's Rename… now opens prefilled with the NEW name.
      await page.locator('#switcher-more-btn').first().click();
      await page.locator('#switcher-rename').click();
      await expect(page.locator('#session-rename input[name="name"]')).toHaveValue(longOffer);
      await page.keyboard.press('Escape');
      page.removeAllListeners('framenavigated');
      await page.reload();
      await expect(page.locator('header [data-session-name="manual"]'), 'the rename is stored').toHaveText(longOffer);
      await expect(banner).toHaveCount(0);
    });

    // A TERMINAL PAGE has the ⋯ menu's Rename… too, but no #mv-title: its own
    // heading and its own tab title ("Terminal · … · muster") follow, in place.
    // Its Rendered view opens in a FRAME over it (view_swap.go): a Rename
    // there retitles the TOP tab, which shows the frame. A name with a literal
    // "&amp;" titles the tab exactly so (htmx's own <title> handling would
    // decode it to "&").
    test('a Terminal page renames in place, and so does its framed Rendered view', async ({ page }) => {
      const tw = termWindows[vp.key];
      const tsub = 'mirror:' + termSids[vp.key];
      await signIn(page, origin);
      await rig.waitListed(page, `walnut:${tw}`);
      writeFocus([{ subject: tsub, title: 'Billing migration', tail: REAL_TAIL }]);
      writeFocus([{ subject: tsub, title: 'Fixing the Stripe webhook retry', tail: REAL_TAIL,
        historyTitles: ['Tracing the webhook retry queue', 'Reading the Stripe webhook docs'], historyAgeMinutes: 60 }]);
      // The framed Rendered view's offer.
      await page.goto(origin + termView(tw));
      await expect(page).toHaveTitle('Terminal · Billing migration · muster');
      await page.locator('#view-rendered').click();
      const frameEl = page.locator('iframe[data-muster-view]');
      await expect(frameEl).toBeVisible();
      const framed = page.frameLocator('iframe[data-muster-view]');
      await expect(framed.locator('#session-drift')).toHaveAttribute('data-session-drift', 'Fixing the Stripe webhook retry');
      // Armed once the frame is open: the switch itself moves the address
      // (history.replaceState), which Playwright counts as a navigation.
      let navs = await markPage(page);
      await expect(page, 'control: the top tab shows the frame').toHaveTitle('Billing migration · Mirrored · muster');
      await framed.locator('[data-session-drift-accept]').click();
      await expect(framed.locator('#session-drift')).toHaveCount(0);
      await expect(framed.locator('header [data-session-name="manual"]')).toHaveText('Fixing the Stripe webhook retry');
      await expect(page, 'the top tab follows the framed rename').toHaveTitle('Fixing the Stripe webhook retry · Mirrored · muster');
      await expectSamePage(page, navs, 'the framed Rename');
      page.removeAllListeners('framenavigated');

      // The Terminal page's own ⋯ Rename…, with a name that holds a literal
      // "&amp;" once plained: typed "R&amp;amp;D", it is called "R&amp;D"
      // (the plaining decodes one entity), and its tab must read so — htmx's
      // <title> handling would decode it again, to "R&D".
      const typed = `R&amp;amp;D ${vp.key}`;
      const name = `R&amp;D ${vp.key}`;
      await page.goto(origin + termView(tw));
      navs = await markPage(page);
      await page.locator('#switcher-more-btn').first().click();
      await page.locator('#switcher-rename').click();
      const dialog = page.locator('#session-rename');
      await expect(dialog).toBeVisible();
      await dialog.locator('input[name="name"]').fill(typed);
      await dialog.getByRole('button', { name: 'Save', exact: true }).click();
      await expect(dialog).toBeHidden();
      await expect(page.locator('#term-title [data-session-name="manual"]')).toHaveText(name);
      await expect.poll(() => page.evaluate(() => document.title), 'the tab reads the name exactly').toBe(`Terminal · ${name} · muster`);
      await expectSamePage(page, navs, "the Terminal page's dialog Save");
      await expect.poll(() => focusedId(page), 'focus is on the title').toBe('term-title');
      page.removeAllListeners('framenavigated');
      await page.reload();
      expect(await page.evaluate(() => document.title), 'a reload titles the tab the same').toBe(`Terminal · ${name} · muster`);
      await expect(page.locator('#term-title [data-session-name="manual"]')).toHaveText(name);
    });
  });
}
