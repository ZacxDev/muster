import fs from 'node:fs';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { toolContextPort } from '../lib/env';
import { cc, MirrorRig } from '../lib/mirrorhost';
import { clsInit, readCls, resetCls } from '../lib/cls';

// EACH TOOL ROW SAYS HOW MUCH CONTEXT ITS CALL AND RESULT ADDED (internal/ui/
// tool_context.go, cctranscript/toolctx.go), on a mirrored Claude Code pane
// whose SYNTHETIC transcript carries message.usage:
//
//   - one call, measured: the next turn's context minus its own — "+3.3k ctx";
//   - two parallel calls: one measured growth, split by size — "~+1.5k ctx" and
//     "~+4.5k ctx", marked approximate;
//   - a call whose next turn came after a prompt: not attributable, so an
//     estimate from size — "~+181 ctx";
//   - a call with a long MCP-style tool name: the name truncates, and the badge
//     and the status stay inside the row at a phone's width;
//   - a call whose next turn has not happened: no badge at all.
//
// The badges of the parallel pair arrive LIVE (the next turn is appended while
// the page is open, the group expanded), and filling them in must not shift
// the layout. Phone 390×844 and desktop 1280×800, no horizontal scroll.

const rig = new MirrorRig(toolContextPort, 'e2e-toolctx');

// An assistant record with one block and its usage (input, cache creation,
// cache read): the context is their sum.
const rec = (o: object) => JSON.stringify(o) + '\n';
const usage = (input: number, create: number, read: number) => ({
  input_tokens: input,
  cache_creation_input_tokens: create,
  cache_read_input_tokens: read,
  output_tokens: 97,
});
const asst = (block: object, u: ReturnType<typeof usage>) => rec({ type: 'assistant', message: { model: 'claude-fixture', content: [block], usage: u } });
const use = (id: string, command: string, name = 'Bash') => ({ type: 'tool_use', id, name, input: { command } });
// A long MCP-style tool name (synthetic): at a phone's width the name must give
// way, not push the badge and the status out of the row.
const LONG = 'mcp__example_server__a_very_long_tool_name_here';
const text = (t: string) => ({ type: 'text', text: t });

// The page's first read: a call measured by the parallel pair's turn, and the
// pair, answered but with no next turn yet.
const FIRST =
  cc.prompt('measure the tools') +
  asst(text('Reading.'), usage(1203, 4511, 20017)) + // 25731
  asst(use('ta', 'cat notes.txt'), usage(1203, 4511, 20017)) +
  cc.toolResult('ta', 'n'.repeat(2600)) +
  asst(use('tp', 'ls -la'), usage(7, 3292, 25731)) + // 29030: ta grew 3299
  asst(use('tq', 'pwd -P'), usage(7, 3292, 25731)) +
  cc.toolResult('tp', 'p'.repeat(380)) +
  cc.toolResult('tq', 'q'.repeat(1180));

// Appended while the page is open.
const LATER =
  asst(text('Both done.'), usage(59, 5941, 29030)) + // 35030: the pair grew 6000
  asst(use('te', 'sleep'), usage(11, 1083, 35030)) + // 36124
  cc.toolResult('te', 'e'.repeat(702)) +
  cc.prompt('stop there') +
  asst(text('Stopped.'), usage(5, 1777, 36124)) + // 37906; after a prompt: te is estimated
  asst(use('tl', 'lookup', LONG), usage(3, 2309, 37906)) + // 40218
  cc.toolResult('tl', 'l'.repeat(900)) +
  asst(text('Next.'), usage(13, 1489, 40218)) + // 41720: tl grew 1502
  asst(use('tz', 'make'), usage(3, 2311, 41720)) +
  cc.toolResult('tz', 'z'.repeat(4409));

const SIZES = [
  { name: 'phone', viewport: { width: 390, height: 844 }, touch: true },
  { name: 'desktop', viewport: { width: 1280, height: 800 }, touch: false },
];

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(120_000);
  await rig.start();
});
test.afterAll(async () => {
  await rig.stop();
});

const row = (page: Page, target: string) => page.locator('#mv-log [data-tool-row]').filter({ has: page.locator('[data-tool-target]', { hasText: target }) });

for (const s of SIZES) {
  test.describe(s.name, () => {
    test.use({ viewport: s.viewport, hasTouch: s.touch, isMobile: s.touch });

    test(`${s.name}: each tool row shows the context it added, measured or marked "~"`, async ({ page }, info) => {
      test.setTimeout(90_000);
      const tr = rig.transcript(FIRST, 'ctx-' + s.name);
      await rig.startHost({ MUSTER_MIRROR_E2E_SESSIONS: 'main', MUSTER_MIRROR_E2E_CLAUDE: 'main=' + tr });
      await page.addInitScript(clsInit);
      await rig.login(page);
      await rig.waitListed(page, 'data-mirror-claude');
      await page.goto(rig.origin + rig.viewPath('main'));

      // ta, tp, tq are one run: a "3 tool calls" group. Open it.
      const group = page.locator('#mv-log [data-tool-group]').first();
      await expect(group.locator('summary [data-tool-group-count]')).toHaveText('3 tool calls', { timeout: 15_000 });
      await group.locator(':scope > summary').click();
      await expect(row(page, 'cat notes.txt')).toBeVisible();

      // Measured: 1203+4511+20017 = 25731 → 7+3292+25731 = 29030, +3299.
      const ta = row(page, 'cat notes.txt').locator('[data-tool-ctx]');
      await expect(ta).toHaveText('+3.3k ctx');
      await expect(ta).toHaveAttribute('data-tool-ctx', 'measured');
      await expect(ta).toHaveAttribute('data-tool-ctx-tokens', '3299');
      // The pair: answered, but its next turn has not been written — no badge.
      for (const t of ['ls -la', 'pwd -P']) await expect(row(page, t).locator('[data-tool-ctx]')).toBeHidden();

      // The next turns arrive while the group is open; the badges fill in place.
      //
      // 🔴 MEASURED AWAY FROM THE BOTTOM. A log stuck to its bottom scrolls with
      // every append, and a row that grew under that scroll was measured to
      // report NO layout-shift entry at all (a badge mutated taller than its row
      // passed). So the three rows are expanded — their args and results make
      // the log overflow — and the log is scrolled to its top, where an append
      // does not scroll it: a badge that changed a row's height moves every row
      // below it on screen, and is counted (checked by that same mutation).
      for (const t of ['cat notes.txt', 'ls -la', 'pwd -P']) await row(page, t).locator(':scope > summary').click();
      await expect(row(page, 'pwd -P')).toHaveAttribute('open', '');
      await page.evaluate(() => (document.getElementById('mv-log')!.scrollTop = 0));
      const scroll = await page.evaluate(() => {
        const l = document.getElementById('mv-log')!;
        return { top: l.scrollTop, overflow: l.scrollHeight - l.clientHeight };
      });
      expect(scroll.top, 'the log is at its top').toBe(0);
      expect(scroll.overflow, 'the log overflows, so its top is not its bottom').toBeGreaterThan(80);
      await page.waitForTimeout(600); // past the taps' 500 ms input window
      await resetCls(page);
      fs.appendFileSync(tr, LATER);
      // 6000 split by bytes: ls -la 20+380 = 400, pwd -P 20+1180 = 1200.
      const tp = row(page, 'ls -la').locator('[data-tool-ctx]');
      const tq = row(page, 'pwd -P').locator('[data-tool-ctx]');
      await expect(tp).toHaveText('~+1.5k ctx', { timeout: 15_000 });
      await expect(tq).toHaveText('~+4.5k ctx');
      for (const b of [tp, tq]) await expect(b).toHaveAttribute('data-tool-ctx', 'split');
      await expect(tq).toHaveAttribute('title', /a share of 6,000 measured for 2 parallel calls/);
      // After a prompt: an estimate, (19 + 702) / 4 → 181.
      const te = row(page, 'sleep').locator('[data-tool-ctx]');
      await expect(te).toHaveText('~+181 ctx');
      await expect(te).toHaveAttribute('data-tool-ctx', 'estimate');
      // The long-named call: measured, 41720 − 40218 = 1502.
      const tl = row(page, 'lookup').locator('[data-tool-ctx]');
      await expect(tl).toHaveText('+1.5k ctx');
      await expect(tl).toHaveAttribute('data-tool-ctx', 'measured');
      // The last call's next turn has not happened: no badge.
      await expect(row(page, 'make')).toBeVisible();
      await expect(row(page, 'make').locator('[data-tool-ctx]')).toBeHidden();

      // Filling the badges in moved nothing.
      const cls = await readCls(page);
      expect(cls.unsupported, 'layout-shift observed').toBeUndefined();
      expect(cls.total, `layout shift while the badges filled in: ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.001);
      expect(await page.evaluate(() => document.getElementById('mv-log')!.scrollTop), 'the append did not scroll the log').toBe(0);

      // Every badge sits inside its row, beside the status, and the page does
      // not scroll sideways.
      for (const t of ['cat notes.txt', 'ls -la', 'pwd -P', 'sleep', 'lookup']) {
        const r = (await row(page, t).boundingBox())!;
        const b = (await row(page, t).locator('[data-tool-ctx]').boundingBox())!;
        const st = (await row(page, t).locator('summary [aria-label^="status:"]').boundingBox())!;
        expect(b.x + b.width, `${t}: the badge is inside its row`).toBeLessThanOrEqual(r.x + r.width);
        expect(b.x + b.width, `${t}: the badge sits before the status`).toBeLessThanOrEqual(st.x);
        expect(st.x - (b.x + b.width), `${t}: the status sits beside the badge`).toBeLessThan(16);
        expect(r.x + r.width, `${t}: the row fits the viewport`).toBeLessThanOrEqual(s.viewport.width);
        expect(st.x + st.width, `${t}: the status stays inside its row`).toBeLessThanOrEqual(r.x + r.width);
        expect(b.x, `${t}: the badge starts inside its row`).toBeGreaterThanOrEqual(r.x);
      }
      const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
      expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(cw);

      // The screenshot: the rows closed again, the whole conversation in view.
      for (const t of ['cat notes.txt', 'ls -la', 'pwd -P']) await row(page, t).locator(':scope > summary').click();
      await page.evaluate(() => (document.getElementById('mv-log')!.scrollTop = 1e6));
      await page.waitForTimeout(300);
      await page.screenshot({ path: info.outputPath(`tool-context-${s.name}.png`) });
    });
  });
}
