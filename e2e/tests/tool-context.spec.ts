import fs from 'node:fs';
import path from 'node:path';
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
//     estimate — its turn's output plus its result at the calibrated rate —
//     "~+417 ctx";
//   - a call with a long MCP-style tool name: the name truncates, and the badge
//     and the status stay inside the row at a phone's width;
//   - a call whose next turn has not happened: no badge at all;
//   - a subagent's card and a question card, badged like a row;
//   - a call still running when the page's socket drops: the socket that
//     resumes badges it when its next turn arrives.
//
// The badges of the parallel pair arrive LIVE (the next turn is appended while
// the page is open, the group expanded), and filling them in must not shift
// the layout. Phone 390×844, desktop 1280×800 and a narrow 320×700, no
// horizontal scroll.

const rig = new MirrorRig(toolContextPort, 'e2e-toolctx');

// An assistant record with one block and its usage (input, cache creation,
// cache read): the context is their sum.
const rec = (o: object) => JSON.stringify(o) + '\n';
const usage = (input: number, create: number, read: number, output = 97) => ({
  input_tokens: input,
  cache_creation_input_tokens: create,
  cache_read_input_tokens: read,
  output_tokens: output,
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

// The cards and the reconnect: a subagent's call, a question, and a call still
// running when the socket drops.
const QUESTIONS = { questions: [{ question: 'Which colour?', header: 'Colour', multiSelect: false, options: [{ label: 'Teal', description: 'calm' }, { label: 'Amber', description: 'warm' }] }] };
const CARDS =
  cc.prompt('delegate it') +
  asst(text('Delegating.'), usage(101, 2203, 30017, 211)) + // 32321
  asst({ type: 'tool_use', id: 'sa', name: 'Agent', input: { description: 'Survey the module', prompt: 'p', subagent_type: 'Explore' } }, usage(101, 2203, 30017, 211)) +
  // A background agent: its card's summary keeps moving after the reconnect.
  rec({ type: 'user', promptId: 'p', toolUseResult: { isAsync: true, status: 'async_launched', agentId: 'a0ctx1' },
    message: { content: [{ type: 'tool_result', tool_use_id: 'sa', content: 's'.repeat(2000) }] } }) +
  asst({ type: 'tool_use', id: 'qa', name: 'AskUserQuestion', input: QUESTIONS }, usage(7, 4093, 32321, 157)) + // 36421: sa grew 4100
  cc.toolResult('qa', 'User has answered your questions: "Which colour?"="Teal".') +
  asst(text('Teal.'), usage(3, 517, 36421, 23)) + // 36941: qa grew 520
  asst(use('tm', 'make'), usage(5, 1109, 36941, 61)); // 38055: still running
// The agent's own transcript (beside the parent's, where Claude Code writes it).
const subRow = (ago: number, message: object, type = 'assistant') =>
  rec({ type, isSidechain: true, agentId: 'a0ctx1', timestamp: new Date(Date.now() - ago * 1000).toISOString(), message });
const subTool = (ago: number, name: string, input: object) =>
  subRow(ago, { role: 'assistant', stop_reason: 'tool_use', content: [{ type: 'tool_use', id: 'x' + ago, name, input }] });
const CARDS_LATER = cc.toolResult('tm', 'm'.repeat(3109)) + asst(text('Built.'), usage(9, 2169, 38055, 41)); // 40233: tm grew 2178

const SIZES = [
  { name: 'phone', viewport: { width: 390, height: 844 }, touch: true },
  { name: 'desktop', viewport: { width: 1280, height: 800 }, touch: false },
  // A narrow phone: a question card's label must stay inside the card.
  { name: 'narrow', viewport: { width: 320, height: 700 }, touch: true },
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
      // After a prompt: an estimate, the turn's output 97 + 702 / 2.2 (319.1 → 320) = 417.
      const te = row(page, 'sleep').locator('[data-tool-ctx]');
      await expect(te).toHaveText('~+417 ctx');
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

    test(`${s.name}: a subagent card, a question card and a call running across a reconnect are badged`, async ({ page }, info) => {
      test.setTimeout(90_000);
      const tr = rig.transcript(CARDS, 'cards-' + s.name);
      const subDir = path.join(tr.replace(/\.jsonl$/, ''), 'subagents');
      fs.mkdirSync(subDir, { recursive: true });
      fs.writeFileSync(path.join(subDir, 'agent-a0ctx1.meta.json'),
        JSON.stringify({ agentType: 'Explore', description: 'Survey the module', toolUseId: 'sa', spawnDepth: 1, requestShape: 'background' }));
      const subFile = path.join(subDir, 'agent-a0ctx1.jsonl');
      fs.writeFileSync(subFile, subRow(90, { role: 'user', content: 'survey it' }, 'user') + subTool(70, 'Grep', { pattern: 'TODO' }));
      await rig.startHost({ MUSTER_MIRROR_E2E_SESSIONS: 'main', MUSTER_MIRROR_E2E_CLAUDE: 'main=' + tr });
      const sockets: { close: () => Promise<void> }[] = [];
      await page.routeWebSocket(/\/mirror\/view\/ws/, (ws) => {
        ws.connectToServer();
        sockets.push(ws);
      });
      await rig.login(page);
      await rig.waitListed(page, 'data-mirror-claude');
      await page.goto(rig.origin + rig.viewPath('main'));
      const log = page.locator('#mv-log');

      // The subagent's card: 32321 → 36421, +4100, measured.
      const card = log.locator('[data-subagent-card]');
      const cardBadge = card.locator('[data-tool-ctx]');
      await expect(cardBadge).toHaveText('+4.1k ctx', { timeout: 15_000 });
      await expect(cardBadge).toHaveAttribute('data-tool-ctx', 'measured');
      // The question card: 36421 → 36941, +520, measured.
      const ask = log.locator('[data-ask]');
      await expect(ask.locator('[data-tool-ctx]')).toHaveText('+520 ctx');
      // The running call: no badge yet.
      await expect(row(page, 'make')).toBeVisible();
      await expect(row(page, 'make').locator('[data-tool-ctx]')).toBeHidden();

      // The socket drops; the page reconnects on its own and resumes.
      await sockets[0].close();
      await expect.poll(() => sockets.length, { message: 'the page reconnected', timeout: 15_000 }).toBe(2);
      await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
      // The agent moves: the NEW socket redraws its card's summary, which it
      // never badged itself — the page keeps the badge it has.
      fs.appendFileSync(subFile, subTool(0, 'Read', { file_path: '/src/module.go' }));
      await expect(card.locator('[data-subagent-last]')).toHaveText('Read: /src/module.go', { timeout: 15_000 });
      await expect(cardBadge).toHaveText('+4.1k ctx');
      // The call's result and its next turn arrive over the NEW socket: 38055 → 40233, +2178.
      fs.appendFileSync(tr, CARDS_LATER);
      const tm = row(page, 'make').locator('[data-tool-ctx]');
      await expect(tm).toHaveText('+2.2k ctx', { timeout: 15_000 });
      await expect(tm).toHaveAttribute('data-tool-ctx', 'measured');
      await expect(tm).toHaveAttribute('data-tool-ctx-tokens', '2178');
      // The cards kept theirs.
      await expect(cardBadge).toHaveText('+4.1k ctx');
      await expect(ask.locator('[data-tool-ctx]')).toHaveText('+520 ctx');

      // Each badge sits inside its card or row, and the page does not scroll sideways.
      for (const [el, what] of [[card, 'subagent card'], [ask, 'question card'], [row(page, 'make'), 'row']] as const) {
        const r = (await el.boundingBox())!;
        const b = (await el.locator('[data-tool-ctx]').first().boundingBox())!;
        expect(b.x, `${what}: the badge starts inside it`).toBeGreaterThanOrEqual(r.x);
        expect(b.x + b.width, `${what}: the badge ends inside it`).toBeLessThanOrEqual(r.x + r.width);
      }
      const label = (await ask.locator('[data-ask-label]').boundingBox())!;
      const ab = (await ask.boundingBox())!;
      expect(label.x + label.width, 'the question card\'s label stays inside the card').toBeLessThanOrEqual(ab.x + ab.width);
      const pill = (await card.locator('[data-subagent-status]').boundingBox())!;
      const cb = (await card.boundingBox())!;
      expect(pill.x + pill.width, 'the status pill stays inside the card').toBeLessThanOrEqual(cb.x + cb.width);
      const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
      expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(cw);
      await page.screenshot({ path: info.outputPath(`tool-context-cards-${s.name}.png`) });
    });
  });
}
