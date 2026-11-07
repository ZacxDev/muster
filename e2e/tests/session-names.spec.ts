import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { dsn, repoRoot, sessionNamesPort } from '../lib/env';
import { cc, MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';

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
//      reload and a later nameable focus (never overwritten).

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
// A second window per viewport for the drift offer (5.), its own conversation.
const driftSids = { phone: 'drift-phone-' + stamp, desktop: 'drift-desktop-' + stamp };
const driftWindows = { phone: 3, desktop: 4 };
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
    for (const [sid, w] of [[sids[k], windows[k]], [driftSids[k], driftWindows[k]]] as const) {
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
  mkchat({ MUSTER_E2E_DELETE: '1', MUSTER_E2E_FOCUS: JSON.stringify([...Object.values(sids), ...Object.values(driftSids)].map((s) => ({ subject: 'mirror:' + s, title: 'x' }))) });
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
      await Promise.all([page.waitForEvent('load'), dialog.getByRole('button', { name: 'Save', exact: true }).click()]);
      await expect(page.locator('header [data-session-name="manual"]')).toHaveText(`Payments ${vp.key}`);
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
    // never made; "Keep the name" holds until the focus changes; accepting
    // renames it (a manual name), and the offer is gone.
    test('a session whose work moved on is offered a rename, which it can keep or accept', async ({ page }) => {
      const dw = driftWindows[vp.key];
      const dsub = 'mirror:' + driftSids[vp.key];
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
      const keep = banner.locator('[data-session-drift-dismiss]');
      const accept = banner.locator('[data-session-drift-accept]');
      for (const b of [keep, accept]) expect((await b.boundingBox())!.height, 'a 44px target').toBeGreaterThanOrEqual(44);
      // Keep the name: gone, and still gone after a reload.
      await Promise.all([page.waitForEvent('load'), keep.click()]);
      await expect(banner).toHaveCount(0);
      await page.reload();
      await expect(banner).toHaveCount(0);
      // A new focus title releases the dismissal.
      writeFocus([{ subject: dsub, title: 'Wiring the Stripe refund flow', tail: REAL_TAIL }]);
      await page.goto(origin + view(dw));
      await expect(banner).toHaveAttribute('data-session-drift', 'Wiring the Stripe refund flow');
      // Accept: one tap renames it; the name is the person's now.
      await Promise.all([page.waitForEvent('load'), accept.click()]);
      await expect(page.locator('header [data-session-name="manual"]')).toHaveText('Wiring the Stripe refund flow');
      await expect(banner).toHaveCount(0);
    });
  });
}
