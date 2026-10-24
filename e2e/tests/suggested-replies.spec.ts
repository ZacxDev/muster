import fs from 'node:fs';
import http from 'node:http';
import { Page, Request } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { suggestFakePort, suggestPort } from '../lib/env';
import { clsInit, readCls, resetCls } from '../lib/cls';
import { cc, MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';

// SUGGESTED REPLIES' CHIP ROW (docs/suggested-replies.md), in a real browser
// against a real muster-server (the e2eterm build, its model pointed at a FAKE
// chat-completions endpoint this spec runs — never a real model) and a real
// muster-mirror (the `-tags e2emirror` per-window fake) whose Claude window's
// status is <transcript>.status, written here the way Claude Code writes its
// sessions file ({status, statusUpdatedAt}).
//
//   1. A turn end (busy → idle, the turn's reply on the stream) brings up to 3
//      chips, labelled AI, 44px tall, within ui.SuggestCutoff (8 s).
//   2. A tap FILLS the box and sends NOTHING (no send request, nothing pasted)
//      — the positive control is Send itself, which does reach the host; the
//      report that follows carries numbers, never the text.
//   3. A tap below a draft appends on a new line.
//   4. × hides the row for that turn (and the set stays hidden on a reload).
//   5. No chips while the session is busy, nor at a permission prompt — and
//      the fake model is not even asked.
//   6. CLS < 0.01 while the row is reserved and filled, at 390×844 and
//      1280×800, against a positive control (the same row forced into flow)
//      that the same instrument does see.

const reply = 'Two ways: (a) checkpoint every 500 rows in the jobs table, (b) idempotent re-run keyed on batch id. Which do you prefer?';
const chipA = 'Go with (a), checkpoint every 500 rows';
const chipB = '(b), the re-run keyed on batch id';

// The fake model: every request is counted and kept; each answers two grounded
// picks for `reply` (the server's filter must keep both).
let calls: string[] = [];
const fake = http.createServer((req, res) => {
  let body = '';
  req.on('data', (d) => (body += d));
  req.on('end', () => {
    calls.push(body);
    const content = JSON.stringify({
      suggestions: [
        { text: chipA, kind: 'pick', cites: ['R'], quote: '(a) checkpoint every 500 rows' },
        { text: chipB, kind: 'pick', cites: ['R'], quote: '(b) idempotent re-run keyed on batch id' },
      ],
    });
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ choices: [{ message: { content } }], usage: { prompt_tokens: 800, completion_tokens: 60 } }));
  });
});

const rig = new MirrorRig(suggestPort, 'e2e-sugg', { MUSTER_E2E_SUGGEST_URL: `http://127.0.0.1:${suggestFakePort}/chat` });
let tr = '';
const windowsFile = () => rig.dir + '/host.windows';
// setStatus is Claude Code writing its status, with NOW as its time.
let lastTok = 0;
function setStatus(status: string) {
  lastTok = Math.max(Date.now(), lastTok + 1);
  writeFileAtomic(tr + '.status', JSON.stringify({ status, statusUpdatedAt: lastTok }));
}
const pasted = () => (fs.existsSync(tr + '.pasted') ? fs.readFileSync(tr + '.pasted', 'utf8') : '');
const viewPath = () => rig.viewPath('work') + '&window=%401';
const row = (page: Page) => page.locator('#mv-suggest');
const chips = (page: Page) => page.locator('#mv-suggest [data-suggest-chip]');

test.beforeAll(async () => {
  await new Promise<void>((r) => fake.listen(suggestFakePort, '127.0.0.1', () => r()));
  await rig.start();
  tr = rig.transcript(cc.prompt('make the export job resumable'), 'ssssssss-0001');
  setStatus('busy');
  writeFileAtomic(windowsFile(), `work|@1|0|1|claude|${Math.floor(Date.now() / 1000) - 60}|${tr}\n`);
  await rig.startHost({ MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile(), MUSTER_MIRROR_E2E_REFRESH: '250ms' });
});

test.afterAll(async () => {
  await rig.stop();
  await new Promise((r) => fake.close(r));
});

test.use({ storageState: { cookies: [], origins: [] } });

async function openView(page: Page) {
  await rig.login(page);
  await page.goto(rig.origin + viewPath());
  await expect(page.locator('#mv-input'), `the rendered view (muster-mirror log:\n${rig.log})`).toBeEnabled({ timeout: 20_000 });
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 20_000 });
}

// turn runs one turn on the open view: busy, the reply, then idle — and
// returns when the status flip was written.
async function turn(page: Page, text = reply) {
  setStatus('busy');
  await page.waitForTimeout(1200); // the host's re-resolve (3 × 300 ms) sees busy
  fs.appendFileSync(tr, cc.reply(text));
  await expect(page.locator('#mv-log [data-bubble="assistant"]').last()).toContainText(text.slice(0, 20));
  setStatus('idle');
  return Date.now();
}

// sendRequests records every request that could deliver text to the session.
function sendRequests(page: Page): string[] {
  const seen: string[] = [];
  page.on('request', (r: Request) => {
    const p = new URL(r.url()).pathname;
    if (r.method() === 'POST' && (p === '/mirror/view/send' || p.startsWith('/send-queue') || p === '/mirror/terminal/send')) seen.push(p);
  });
  return seen;
}

test.describe.serial('phone 390×844', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('a turn end brings AI-labelled chips within the cutoff; a tap fills and never sends; Send is the positive control', async ({ page }) => {
    const sends = sendRequests(page);
    const reports: string[] = [];
    page.on('request', (r) => {
      if (new URL(r.url()).pathname === '/mirror/view/suggest/event') reports.push(r.postData() || '');
    });
    await openView(page);
    await expect(row(page), 'no row while nothing is suggested').toBeHidden();
    const before = calls.length;
    const flipped = await turn(page);
    await expect(chips(page).first()).toBeVisible({ timeout: 8_000 });
    const tookMs = Date.now() - flipped;
    expect(tookMs, 'the chips are on screen within the 8 s cutoff of the status flip').toBeLessThan(8_000);
    expect(calls.length - before, 'one call to the (fake) model for the turn').toBe(1);
    expect(calls[calls.length - 1], 'the model was given the reply').toContain('Which do you prefer?');
    await expect(row(page)).toHaveAttribute('role', 'group');
    await expect(row(page)).toHaveAttribute('aria-label', 'AI-suggested replies');
    await expect(row(page).getByText('AI', { exact: true })).toBeVisible();
    await expect(chips(page)).toHaveCount(2);
    await expect(chips(page).first()).toHaveAttribute('aria-label', 'AI-suggested reply: ' + chipA);
    for (const b of await chips(page).all()) {
      expect((await b.boundingBox())!.height, 'a chip is a 44px target').toBeGreaterThanOrEqual(44);
    }
    // The row sits directly above the composer.
    const rb = (await row(page).boundingBox())!, cb = (await page.locator('#mv-compose').boundingBox())!;
    expect(Math.abs(rb.y + rb.height - cb.y), 'the row ends where the composer starts').toBeLessThanOrEqual(1);

    // A tap fills — and sends nothing.
    await chips(page).first().tap();
    await expect(page.locator('#mv-input')).toHaveValue(chipA);
    await expect(page.locator('#mv-input')).toBeFocused();
    await page.waitForTimeout(1500);
    expect(sends, 'a tap made no send request').toEqual([]);
    expect(pasted(), 'nothing reached the pane').toBe('');

    // The positive control: Send (edited) does reach the host.
    await page.locator('#mv-input').fill(chipA + ', and keep it a flag');
    await page.locator('#mv-send').tap();
    await expect.poll(pasted, { timeout: 10_000 }).toContain('keep it a flag');
    expect(sends, 'Send made the send request').toEqual(['/mirror/view/send']);
    await expect(row(page), 'a send takes the row away').toBeHidden();
    // The reports: shown, filled, sent — numbers and the fingerprint only.
    await expect.poll(() => reports.length).toBeGreaterThanOrEqual(3);
    const evs = reports.map((b) => new URLSearchParams(b));
    expect(evs.map((e) => e.get('event'))).toEqual(['shown', 'filled', 'sent']);
    expect(evs[2].get('dist'), 'the edit distance of ", and keep it a flag"').toBe(String(', and keep it a flag'.length));
    for (const e of evs) {
      expect([...e.keys()].sort().filter((k) => !['host', 'session', 'window', 'fingerprint', 'event', 'idx', 'dist'].includes(k)), 'no other field').toEqual([]);
      for (const v of e.values()) expect(v.includes('checkpoint'), 'no text in a report').toBe(false);
    }
  });

  test('a tap below a draft appends on a new line; × hides the row for the turn, also after a reload', async ({ page }) => {
    await openView(page);
    await turn(page, reply + ' ');
    await expect(chips(page).first()).toBeVisible({ timeout: 8_000 });
    await page.locator('#mv-input').fill('my draft so far');
    await chips(page).nth(1).tap();
    await expect(page.locator('#mv-input')).toHaveValue('my draft so far\n' + chipB);
    await page.locator('#mv-input').fill('');

    await page.locator('#mv-suggest-x').tap();
    await expect(row(page)).toBeHidden();
    await page.waitForTimeout(1500);
    await expect(row(page), 'still hidden').toBeHidden();
    // A reload re-reads the same set (no call) and keeps it hidden.
    const before = calls.length;
    await page.reload();
    await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 20_000 });
    await page.waitForTimeout(2500);
    await expect(row(page), 'the dismissed set stays hidden on this device').toBeHidden();
    expect(calls.length, 'a reload of the same turn calls nothing').toBe(before);
  });

  test('no chips while busy, nor at a permission prompt — and the model is not asked', async ({ page }) => {
    await openView(page);
    setStatus('busy');
    await page.waitForTimeout(1200);
    const before = calls.length;
    fs.appendFileSync(tr, cc.reply(reply + ' (third)'));
    await page.waitForTimeout(3000);
    await expect(row(page), 'busy: no row').toBeHidden();
    // A permission prompt: the turn's status is WAITING.
    setStatus('waiting');
    await page.waitForTimeout(3000);
    await expect(row(page), 'waiting: no row').toBeHidden();
    expect(calls.length, 'no call while busy or waiting').toBe(before);
    // Positive control on the same page: idle brings the chips.
    setStatus('idle');
    await expect(chips(page).first()).toBeVisible({ timeout: 8_000 });
    expect(calls.length - before).toBe(1);
  });
});

// measureTurn measures CLS from just before a turn's status flip until its
// chips are shown and settled; inFlow forces the row into the composer's flow
// (the positive control).
async function measureTurn(page: Page, inFlow: boolean, salt: string) {
  await page.addInitScript(clsInit);
  if (inFlow) await page.addInitScript(() => {
    document.addEventListener('DOMContentLoaded', () => {
      const st = document.createElement('style');
      st.textContent = '#mv-suggest{position:static!important}';
      document.head.appendChild(st);
    });
  });
  await openView(page);
  // The view opens on an idle session whose last turn has a set: the chips
  // come with the page (the open is a turn end too, served from the cache).
  await expect(chips(page).first(), 'the previous turn\'s chips at the open').toBeVisible({ timeout: 8_000 });
  await page.waitForTimeout(1000);
  const atOpen = await readCls(page);
  await resetCls(page);
  await turn(page, reply + salt);
  await expect(chips(page).first()).toBeVisible({ timeout: 8_000 });
  await page.waitForTimeout(1500);
  const r = await readCls(page);
  expect(r.unsupported, 'layout-shift entries are observable here').toBeUndefined();
  return { open: atOpen, turn: r };
}

for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`layout shift, ${vp.name}`, () => {
    test.use(vp.use);
    test('the row coming and filling shifts nothing (< 0.01), and the instrument sees the in-flow control', async ({ page, browser }) => {
      const { open, turn: r } = await measureTurn(page, false, ' [' + vp.name + ']');
      console.log(`CLS ${vp.name}: open with chips ${open.total} ${JSON.stringify(open.shifts)}; turn end → chips ${r.total} ${JSON.stringify(r.shifts)}`);
      // The positive control, on a fresh page: the same flow, the row in flow.
      const ctx = await browser.newContext({ ...vp.use, storageState: { cookies: [], origins: [] } });
      const p2 = await ctx.newPage();
      const c = await measureTurn(p2, true, ' [' + vp.name + ' control]');
      console.log(`CLS ${vp.name}: positive control (row in flow): open ${c.open.total} ${JSON.stringify(c.open.shifts)}; turn ${c.turn.total} ${JSON.stringify(c.turn.shifts)}`);
      await ctx.close();
      expect(c.turn.total, 'the positive control: the instrument sees the in-flow row shift the composer').toBeGreaterThan(0);
      expect(open.total, `CLS of the open with chips (${JSON.stringify(open.shifts)})`).toBeLessThan(0.01);
      expect(r.total, `CLS of the turn end (${JSON.stringify(r.shifts)})`).toBeLessThan(0.01);
    });
  });
}
