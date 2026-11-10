import fs from 'node:fs';
import path from 'node:path';
import { Page, Route } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { openCodeSendPort, password } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';
import { revealBox } from '../lib/composer';

// SENDING TO OPENCODE THROUGH ITS API (docs/design/opencode-first-class.md),
// ON A REAL SERVER (the e2eterm binary, mirror plugin on, a 1 s send-queue
// idle) AND REAL muster-mirror processes whose fakes are tmux and the opencode
// server (cmd/muster-mirror/e2eopencode.go). The opted-in host runs with
// -interactive and -opencode-send; its window @2 is LAUNCHER-BOUND (a real
// launcher record, read by the real resolver), @3 is only title-matched. A
// second host is -interactive WITHOUT -opencode-send. A message the fake
// opencode server admits becomes the session's user message, so the view's
// own row for it is the proof that it landed — and how many times. Every value
// is invented fixture data.
//
// 🔴 REGRESSION TEST. On origin/main every opencode composer is disabled
// ("Sending to opencode isn't supported yet"), with no queue.

const rig = new MirrorRig(openCodeSendPort, 'e2e-ocsend', { MUSTER_SEND_QUEUE_IDLE: '1s' });
const bound = 'ses_0e2esend1a2b3c4d';
const titled = 'ses_0e2esend5e6f7a8b';
// The opencode parity windows (docs/design/opencode-first-class.md): @4 a
// hand-opened pane whose CUT title a subagent's session also starts with
// (ambiguous: never sent to), @5 a session with
// a step's token counts and two quick failed turns, @6 a session that asks.
const cut = 'ses_0e2esendcut00001';
const ctxSession = 'ses_0e2esendctx00001';
const askSession = 'ses_0e2esendask00001';
const t0 = Math.floor(Date.now() / 1000);
let ops = '';
let ocDir = '';
let ocWindows = '';
let offHost = '';
let stopOff: (() => Promise<void>) | undefined;

function op(o: Record<string, unknown>) {
  fs.appendFileSync(ops, JSON.stringify(o) + '\n');
}

function windowsLine(boundTitle: string) {
  // sess|id|index|active|activity|pane title
  return (
    `oc|@2|0|1|${t0 - 10}|${boundTitle}\noc|@3|1|0|${t0 - 20}|OC | Title-matched fixture session\n` +
    `oc|@4|2|0|${t0 - 30}|OC | A long hand-opened fixture session ti…\n` +
    `oc|@5|3|0|${t0 - 40}|OC | Context fixture session\n` +
    `oc|@6|4|0|${t0 - 50}|OC | Asking fixture session\n`
  );
}

test.beforeAll(async ({ browser }) => {
  test.setTimeout(180_000);
  await rig.start();
  const dir = fs.mkdtempSync(path.join(rig.dir, 'opencode-'));
  ocDir = dir;
  ops = path.join(dir, 'ops.jsonl');
  op({ op: 'session', id: bound, title: 'Bound fixture session' });
  op({ op: 'session', id: titled, title: 'Title-matched fixture session' });
  op({ op: 'session', id: cut, title: 'A long hand-opened fixture session title, past the cut' });
  op({ op: 'session', id: 'ses_0e2esendcutkid01', title: 'A long hand-opened fixture session ti (@general subagent)', parent: cut });
  op({ op: 'session', id: ctxSession, title: 'Context fixture session' });
  op({ op: 'session', id: askSession, title: 'Asking fixture session' });
  // The session @3's pane switches to in the queued-then-switched case.
  op({ op: 'session', id: 'ses_0e2esendswitch01', title: 'Switched fixture session' });
  // @5: a completed step's usage on fixture-model (input 3517 + cache read
  // 41203 + cache write 2089 = 46809 in context; output 611 is not), whose
  // window GET /api/model says is 187500. Its failed turns are added by the
  // test that reads them (the queue's breaker holds for a minute from when
  // muster first sees the window's status).
  op({ op: 'models', models: [{ provider: 'fixture-provider', id: 'fixture-model', context: 187500 }] });
  op({ op: 'message', session: ctxSession, message: { id: 'msg_0e2ectx01', type: 'user', text: 'measure the fixture', time: { created: 1700000000001 } } });
  op({
    op: 'message',
    session: ctxSession,
    message: {
      id: 'msg_0e2ectx02',
      type: 'assistant',
      agent: 'build',
      model: { id: 'fixture-model', providerID: 'fixture-provider' },
      finish: 'stop',
      time: { created: 1700000000002, completed: 1700000000900 },
      tokens: { input: 3517, output: 611, reasoning: 97, cache: { read: 41203, write: 2089 } },
      content: [{ type: 'text', text: 'Measured.' }],
    },
  });
  // No status op: a session that has never run is idle WITH a status time
  // (mirrorhost's OpenCodeSource), so it takes a send before its first turn.
  op({ op: 'bind', session: 'oc', window: '@2', id: bound });
  const windows = path.join(rig.dir, 'windows');
  writeFileAtomic(windows, `plain|@1|0|1|zsh|${t0 - 50}|\n`);
  ocWindows = path.join(rig.dir, 'opencode-windows');
  writeFileAtomic(ocWindows, windowsLine('OC | Bound fixture session'));
  await rig.startHost({
    MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
    MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE: ocWindows,
    MUSTER_MIRROR_E2E_OPENCODE_DIR: dir,
    MUSTER_MIRROR_E2E_REFRESH: '200ms',
    MUSTER_MIRROR_INTERACTIVE: '1',
    MUSTER_MIRROR_OPENCODE_SEND: '1',
  });

  // The host that did NOT opt in: the same bound window, its own fake server.
  const offDir = fs.mkdtempSync(path.join(rig.dir, 'opencode-off-'));
  fs.writeFileSync(
    path.join(offDir, 'ops.jsonl'),
    [
      { op: 'session', id: bound, title: 'Bound fixture session' },
      { op: 'status', session: bound, status: 'idle' },
      { op: 'bind', session: 'off', window: '@2', id: bound },
    ]
      .map((o) => JSON.stringify(o))
      .join('\n') + '\n',
  );
  const offWindows = path.join(rig.dir, 'opencode-windows-off');
  writeFileAtomic(offWindows, `off|@2|0|1|${t0 - 10}|OC | Bound fixture session\n`);
  const offPlain = path.join(rig.dir, 'windows-off');
  writeFileAtomic(offPlain, `plainoff|@1|0|1|zsh|${t0 - 50}|\n`);
  offHost = rig.host + '-off';
  stopOff = rig.extraHost(offHost, {
    MUSTER_MIRROR_E2E_WINDOWS_FILE: offPlain,
    MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE: offWindows,
    MUSTER_MIRROR_E2E_OPENCODE_DIR: offDir,
    MUSTER_MIRROR_E2E_REFRESH: '200ms',
    MUSTER_MIRROR_INTERACTIVE: '1',
  });

  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await rig.login(page);
  await rig.waitListed(page, 'session=oc');
  await ctx.close();
});

test.afterAll(async () => {
  if (stopOff) await stopOff();
  await rig.stop();
});

test.use({ storageState: { cookies: [], origins: [] } });

const viewOf = (window: string, host = rig.host, session = 'oc') =>
  `/mirror/view?host=${encodeURIComponent(host)}&session=${encodeURIComponent(session)}&window=${encodeURIComponent(window)}`;
const pendingOf = (page: Page, text: string) => page.locator('#mv-log [data-pending-send]', { hasText: text });
const rowsOf = (page: Page, text: string) => page.locator('#mv-log [data-bubble="user"]', { hasText: text });

async function openBound(page: Page) {
  await rig.login(page);
  await page.goto(rig.origin + viewOf('@2'));
  await expect(page.locator('#mv-log [data-notice-row="opencode"]').first(), `the bound notice (mirror log:\n${rig.log})`).toContainText(
    'started by muster-mirror opencode: ' + bound,
    { timeout: 20_000 },
  );
  await revealBox(page);
  await expect(page.locator('#mv-input')).toBeEnabled();
}

async function send(page: Page, text: string) {
  await page.locator('#mv-input').fill(text);
  await page.locator('#mv-send').click();
}

test('a launcher-bound opencode window sends from the Rendered view: pending at once, then the session’s own row, once', async ({ page }) => {
  await openBound(page);
  await expect(page.locator('#mv-readonly')).toHaveCount(0);
  await expect(page.locator('#mv-input')).toHaveAttribute('placeholder', 'Message opencode…');
  const text = `hello opencode ${Date.now()}`;
  await send(page, text);
  // Drawn at once, pending; the box clears.
  await expect(pendingOf(page, text)).toHaveCount(1);
  await expect(page.locator('#mv-input')).toHaveValue('');
  // The session's own row replaces it: exactly one, no bubble left.
  await expect(rowsOf(page, text)).toHaveCount(1, { timeout: 20_000 });
  await expect(page.locator('#mv-log [data-pending-send]')).toHaveCount(0, { timeout: 10_000 });
  // And stays one across a reload (the fake admitted it once).
  await page.reload();
  await expect(rowsOf(page, text)).toHaveCount(1, { timeout: 20_000 });
});

test('a send opencode does not confirm is Not confirmed, and its Send again lands at most once', async ({ page }) => {
  test.setTimeout(120_000);
  await openBound(page);
  // The server admits at once and answers far later than the host waits.
  op({ op: 'delay', ms: 30_000 });
  const text = `maybe landed ${Date.now()}`;
  await send(page, text);
  const p = pendingOf(page, text);
  await expect(p, `Not confirmed (mirror log:\n${rig.log})`).toHaveAttribute('data-pending-send', 'unknown', { timeout: 40_000 });
  // It DID land (the fake admitted it before the late answer).
  await expect(rowsOf(page, text)).toHaveCount(1, { timeout: 20_000 });
  op({ op: 'delay', ms: 0 });
  await page.locator('#mv-send-again').click();
  // Send again posts the SAME message id: answered with the first admission.
  // Its row arrived BEFORE the Send again, so no new row settles the bubble:
  // a confirmed send whose row never arrives is taken off after 30 s
  // (docs/sending-and-step-up.md, "A sent message shows at once").
  await expect(pendingOf(page, text)).not.toHaveAttribute('data-pending-send', 'unknown', { timeout: 20_000 });
  await expect(page.locator('#mv-log [data-pending-send]')).toHaveCount(0, { timeout: 40_000 });
  await page.reload();
  await expect(rowsOf(page, text), 'landed exactly once').toHaveCount(1, { timeout: 20_000 });
});

test('a message queued while opencode works is sent once it is idle', async ({ page }) => {
  test.setTimeout(60_000);
  await openBound(page);
  op({ op: 'status', session: bound, status: 'busy' });
  await expect(page.locator('#mv-working')).toBeVisible({ timeout: 20_000 });
  const text = `queued for opencode ${Date.now()}`;
  await page.locator('#mv-input').fill(text);
  await page.locator('#mv-send-more').click();
  await page.locator('#mv-queue-send').click();
  await expect(page.locator('#mv-note')).toHaveText('Queued.', { timeout: 10_000 });
  await expect(page.locator('#mv-queue [data-queue-id]', { hasText: text })).toHaveCount(1);
  // Not sent while busy.
  await page.waitForTimeout(2_500);
  await expect(rowsOf(page, text)).toHaveCount(0);
  op({ op: 'status', session: bound, status: 'idle' });
  await expect(rowsOf(page, text), 'delivered once idle').toHaveCount(1, { timeout: 30_000 });
});

test('each refusal says why: a slash command, a hand-opened pane a subagent shares the title of, a host that did not opt in', async ({ page }) => {
  await openBound(page);
  const text = '/review the fixture branch';
  await send(page, text);
  const p = pendingOf(page, text);
  await expect(p).toHaveAttribute('data-pending-send', 'failed', { timeout: 20_000 });
  await expect(p).toContainText('that is an opencode slash command');
  await expect(rowsOf(page, text)).toHaveCount(0);

  // A hand-opened pane is sent to only when its binding is verified: @4's cut
  // title is also the start of a subagent's session's, so it stays refused.
  await page.goto(rig.origin + viewOf('@4'));
  await expect(page.locator('#mv-input')).toBeDisabled({ timeout: 20_000 });
  await expect(page.locator('#mv-readonly')).toContainText("a subagent's opencode session has the same title as this pane");
  await expect(page.locator('#mv-queue, #mv-queue-send')).toHaveCount(0);

  await page.goto(rig.origin + viewOf('@2', offHost, 'off'));
  await expect(page.locator('#mv-input')).toBeDisabled({ timeout: 20_000 });
  await expect(page.locator('#mv-readonly')).toContainText('does not send to opencode (it needs -interactive and -opencode-send)');
});

test('a send that needs a fresh sign-in is not delivered until it is confirmed, then lands once', async ({ page }) => {
  await openBound(page);
  // The server's stale answer for the first post (as optimistic-send.spec
  // does): the hub's own stale gate is pinned in Go (mirror's
  // TestAnOpenCodeSendIsRefusedBeforeTheHost, "a stale sign-in").
  const refused = new Set<string>();
  await page.route(
    (u) => u.pathname === '/mirror/view/send',
    async (r: Route) => {
      const nonce = new URLSearchParams(r.request().postData() || '').get('nonce') || '';
      if (!refused.has(nonce)) {
        refused.add(nonce);
        return r.fulfill({ status: 403, headers: { 'X-Muster-Step-Up': '1', 'Content-Type': 'text/plain' }, body: 'confirm it is you' });
      }
      return r.continue();
    },
  );
  const text = `after a step-up ${Date.now()}`;
  await send(page, text);
  await expect(pendingOf(page, text)).toHaveAttribute('data-pending-send', 'stepup');
  await page.waitForTimeout(1_000);
  await expect(rowsOf(page, text), 'nothing lands before the confirm').toHaveCount(0);
  await page.locator('#term-confirm-password').fill(password);
  await page.locator('#term-confirm-submit').click();
  await expect(rowsOf(page, text)).toHaveCount(1, { timeout: 20_000 });
});

test('a bound pane whose title shows another session warns, and its send still lands in the bound session', async ({ page }) => {
  await openBound(page);
  writeFileAtomic(ocWindows, windowsLine('OC | Title-matched fixture session'));
  // The note is drawn when the page loads, not live: wait for the host's next
  // listing (200 ms) to reach the server — read from the served page, since a
  // reload of a live view does not return — then open the view once.
  const warning = "This pane's title shows a different opencode session (Title-matched fixture session)";
  await expect
    .poll(async () => (await (await page.request.get(rig.origin + viewOf('@2'))).text()).includes(warning.replace("'", '&#39;')), {
      timeout: 20_000,
      intervals: [250],
    })
    .toBe(true);
  await page.goto(rig.origin + viewOf('@2'));
  await expect(page.locator('#mv-host-note')).toContainText(warning);
  await revealBox(page);
  const text = `despite the title ${Date.now()}`;
  await send(page, text);
  // The view is the BOUND session's; the row arrives in it.
  await expect(rowsOf(page, text)).toHaveCount(1, { timeout: 20_000 });
  writeFileAtomic(ocWindows, windowsLine('OC | Bound fixture session'));
});

// ---------------------------------------------------------------------------
// OPENCODE PARITY (docs/design/opencode-first-class.md): each at a phone's
// viewport AND a desktop's, set explicitly — the suite's default is the
// phone's, which alone would hide a desktop-only break.
//
// 🔴 REGRESSION TESTS. On origin/main: a hand-opened pane's composer is
// disabled; an opencode view has no context readout and no failed-turn hold;
// an opencode question has no card in the view (only the terminal answers it).

const viewports = [
  { name: 'phone', width: 390, height: 844 },
  { name: 'desktop', width: 1280, height: 800 },
];

for (const vp of viewports) {
  test(`a hand-opened pane verified by its title sends, and the message reaches its own session (${vp.name})`, async ({ page }) => {
    await page.setViewportSize({ width: vp.width, height: vp.height });
    await rig.login(page);
    await page.goto(rig.origin + viewOf('@3'));
    await expect(page.locator('#mv-log [data-notice-row="opencode"]').first(), `the verified notice (mirror log:\n${rig.log})`).toContainText(
      'matched by title, verified: Title-matched fixture session',
      { timeout: 20_000 },
    );
    await revealBox(page);
    await expect(page.locator('#mv-input')).toBeEnabled();
    await expect(page.locator('#mv-readonly')).toHaveCount(0);
    const text = `hand-opened ${vp.name} ${Date.now()}`;
    await send(page, text);
    await expect(rowsOf(page, text), 'the row in the session the pane shows').toHaveCount(1, { timeout: 20_000 });
    // And in no other session: the launcher-bound one's view never shows it.
    await page.goto(rig.origin + viewOf('@2'));
    await expect(page.locator('#mv-log [data-notice-row="opencode"]').first()).toContainText('started by muster-mirror opencode', { timeout: 20_000 });
    await expect(rowsOf(page, text)).toHaveCount(0);
  });

  test(`a hand-opened pane whose binding is ambiguous stays refused, saying why (${vp.name})`, async ({ page }) => {
    await page.setViewportSize({ width: vp.width, height: vp.height });
    await rig.login(page);
    await page.goto(rig.origin + viewOf('@4'));
    await expect(page.locator('#mv-log [data-notice-row="opencode"]').first()).toContainText('matched by title: A long hand-opened', {
      timeout: 20_000,
    });
    await expect(page.locator('#mv-input')).toBeDisabled();
    await expect(page.locator('#mv-readonly')).toContainText("a subagent's opencode session has the same title as this pane (or one that starts the same way)");
  });

  test(`an opencode view shows its model, context readout and failed turns (${vp.name})`, async ({ page }) => {
    await page.setViewportSize({ width: vp.width, height: vp.height });
    // Two turns that each failed at once with no reply (2345 ms and 1234 ms
    // after their prompts), then a fresh idle — so the breaker's minute
    // starts now.
    const base = vp.name === 'phone' ? 1700000100000 : 1700000300000;
    for (const [i, at, after] of [
      [1, base, 2345],
      [2, base + 50000, 1234],
    ]) {
      op({ op: 'message', session: ctxSession, message: { id: `msg_0e2ef${vp.name}u${i}`, type: 'user', text: `try ${i}`, time: { created: at } } });
      op({
        op: 'message',
        session: ctxSession,
        message: {
          id: `msg_0e2ef${vp.name}i${i}`,
          type: 'idle',
          outcome: 'failed',
          error: { name: 'APIError', data: { message: 'fixture provider overloaded' } },
          time: { created: at + after },
        },
      });
    }
    op({ op: 'status', session: ctxSession, status: 'busy' });
    op({ op: 'status', session: ctxSession, status: 'idle' });
    await rig.login(page);
    await page.goto(rig.origin + viewOf('@5'));
    // 46809 of 187500, on fixture-model — worked out from the fake's numbers.
    const ctx = page.locator('#mv-context');
    await expect(ctx, `the readout (mirror log:\n${rig.log})`).toHaveText('fixture-model · 47k / 188k context', { timeout: 20_000 });
    await expect(ctx).toBeVisible();
    await expect(ctx).toHaveAttribute('title', /input \+ cache read \+ cache write/);
    // The failed turns: their rows, and the queue holding instead of sending
    // into the same failure (its circuit breaker, as for Claude Code).
    const errors = page.locator('#mv-log [data-mirror-error]', { hasText: 'fixture provider overloaded' });
    await expect(errors.first()).toBeVisible({ timeout: 20_000 });
    expect(await errors.count()).toBeGreaterThanOrEqual(2);
    await revealBox(page);
    const text = `held for opencode ${vp.name} ${Date.now()}`;
    await page.locator('#mv-input').fill(text);
    await page.locator('#mv-send-more').click();
    await page.locator('#mv-queue-send').click();
    await expect(page.locator('#mv-queue [data-queue-id]', { hasText: text })).toHaveCount(1, { timeout: 10_000 });
    await expect(page.locator('#mv-queue-waiting')).toContainText(/Held — the last \d+ messages each failed at once \(api error\)/, { timeout: 20_000 });
    await page.waitForTimeout(2_500);
    await expect(rowsOf(page, text), 'held: not sent').toHaveCount(0);
  });

  test(`an opencode question is answered from the view, and the fake receives exactly that answer for that session (${vp.name})`, async ({ page }) => {
    await page.setViewportSize({ width: vp.width, height: vp.height });
    const form = `frm_0e2eask${vp.name}0001`;
    const field = (key: string, title: string, description: string, type: string, labels: string[]) => ({
      key,
      title,
      description,
      type,
      options: labels.map((l) => ({ value: l, label: l })),
      custom: true,
    });
    const frame = {
      id: `evt_0e2eask${vp.name}`,
      type: 'form.created',
      location: { directory: '/srv/fixture' },
      data: {
        form: {
          id: form,
          sessionID: askSession,
          title: 'Questions',
          metadata: { kind: 'question' },
          fields: [
            field('q0', 'Database', 'Which database should the fixture use?', 'string', ['Postgres (Recommended)', 'SQLite']),
            field('q1', 'Extras', 'Which extras?', 'multiselect', ['cache', 'metrics']),
          ],
        },
      },
    };
    op({ op: 'frame', frame: JSON.stringify(frame) });
    await rig.login(page);
    await page.goto(rig.origin + viewOf('@6'));
    const card = page.locator(`[data-ask-live][data-ask-tool="${form}"]`);
    await expect(card, `the card (mirror log:\n${rig.log})`).toBeVisible({ timeout: 20_000 });
    await expect(card).toContainText('opencode is asking');
    await expect(card).toContainText('Which database should the fixture use?');
    await expect(card).toContainText('Which extras?');
    // Options are answered by their place in the form, never a label.
    await card.locator('input[name="a0"][value="1"]').check();
    await card.locator('input[name="a1"][value="1"]').check();
    const answered = page.waitForResponse((r) => r.url().endsWith('/mirror/view/ask') && r.request().method() === 'POST');
    await card.locator('[data-ask-submit]').click();
    const resp = await answered;
    expect(resp.status(), 'the answer was relayed').toBe(200);
    expect(await resp.text()).toContain('Answered here — opencode has it.');
    // The window stops waiting, and its next poll takes the card away.
    await expect(page.locator(`[data-ask-live][data-ask-tool="${form}"]`)).toHaveCount(0, { timeout: 20_000 });
    // The fake server's own record of the reply: exactly this form, in exactly
    // this session, with exactly these values — once.
    const replies = () => {
      try {
        return JSON.parse(fs.readFileSync(path.join(ocDir, 'replies.json'), 'utf8')) as { Path: string; Body: string }[];
      } catch {
        return [];
      }
    };
    await expect.poll(() => replies().filter((r) => r.Path.includes(form)).length, { timeout: 20_000 }).toBe(1);
    const mine = replies().filter((r) => r.Path.includes(form));
    expect(mine[0].Path).toBe(`/api/session/${askSession}/form/${form}/reply`);
    expect(JSON.parse(mine[0].Body)).toEqual({ answer: { q0: 'SQLite', q1: ['metrics'] } });
  });
}

// 🔴 A MESSAGE QUEUED FOR ONE SESSION IS NEVER DELIVERED TO ANOTHER: queued
// on @3 while its session works, then the hand-opened pane switches to
// another session (its title moves) and goes idle. The row fails, saying the
// pane shows another session now — and lands in neither session.
// REGRESSION TEST: at 2afa5ab8 it was delivered to the session switched to.
test('a message queued for a hand-opened pane is refused, not delivered, after the pane switches session', async ({ page }) => {
  test.setTimeout(90_000);
  await page.setViewportSize({ width: 390, height: 844 });
  await rig.login(page);
  await page.goto(rig.origin + viewOf('@3'));
  await expect(page.locator('#mv-log [data-notice-row="opencode"]').first()).toContainText('matched by title, verified: Title-matched fixture session', {
    timeout: 20_000,
  });
  op({ op: 'status', session: titled, status: 'busy' });
  await expect(page.locator('#mv-working')).toBeVisible({ timeout: 20_000 });
  await revealBox(page);
  const text = `queued then switched ${Date.now()}`;
  await page.locator('#mv-input').fill(text);
  await page.locator('#mv-send-more').click();
  await page.locator('#mv-queue-send').click();
  const row = page.locator('#mv-queue [data-queue-id]', { hasText: text });
  await expect(row).toHaveCount(1, { timeout: 10_000 });
  // The pane now shows another session, and is idle.
  writeFileAtomic(ocWindows, windowsLine('OC | Bound fixture session').replace('OC | Title-matched fixture session', 'OC | Switched fixture session'));
  op({ op: 'status', session: titled, status: 'idle' });
  // The session switched to runs a turn and ends it: a status newer than the
  // one the row waited behind, so the queue tries to deliver it.
  op({ op: 'status', session: 'ses_0e2esendswitch01', status: 'busy' });
  op({ op: 'status', session: 'ses_0e2esendswitch01', status: 'idle' });
  try {
    await expect
      .poll(async () => (await page.request.get(rig.origin + viewOf('@3'))).text(), { timeout: 30_000, intervals: [500] })
      .toContain('shows another session now than the one this message was written for');
    await page.goto(rig.origin + viewOf('@3'));
    await expect(page.locator('#mv-queue [data-queue-id]', { hasText: text })).toHaveAttribute('data-queue-state', 'failed');
    // Delivered to neither: the switched-to session's view has no row for it.
    await expect(page.locator('#mv-log [data-notice-row="opencode"]').first()).toContainText('Switched fixture session', { timeout: 20_000 });
    await expect(rowsOf(page, text)).toHaveCount(0);
  } finally {
    writeFileAtomic(ocWindows, windowsLine('OC | Bound fixture session'));
  }
});

// 🔴 THE SESSION A MESSAGE NAMES IS THE ONE SHOWN WHEN IT WAS PRESSED, not
// the one the view shows when it is finally posted. A message pressed on @3,
// held by a sign-in confirm while the pane switches to another session (the
// view starts over on it, and its composer's session field follows), is
// posted after the confirm still naming the session it was written for: it
// is refused as moved and lands in neither session. A Send and a Queue alike.
// REGRESSION TEST: at f726ef03 the post read the field as it stood then, and
// the message was delivered to the session switched to.
const switched = 'ses_0e2esendswitch01';
for (const vp of viewports) {
  for (const kind of ['send', 'queue'] as const) {
    test(`a ${kind} pressed before the pane switches session names the session it was written for (${vp.name})`, async ({ page }) => {
      test.setTimeout(90_000);
      await page.setViewportSize({ width: vp.width, height: vp.height });
      await rig.login(page);
      await page.goto(rig.origin + viewOf('@3'));
      const notice = page.locator('#mv-log [data-notice-row="opencode"]').first();
      await expect(notice).toContainText('matched by title, verified: Title-matched fixture session', { timeout: 20_000 });
      // The server's stale answer for the first post, as the step-up case
      // above: the message waits on the confirm.
      const route = kind === 'send' ? '/mirror/view/send' : '/send-queue';
      const refused = new Set<string>();
      await page.route(
        (u) => u.pathname === route,
        async (r: Route) => {
          const nonce = new URLSearchParams(r.request().postData() || '').get('nonce') || r.request().postData() || '';
          if (!refused.has(nonce)) {
            refused.add(nonce);
            return r.fulfill({ status: 403, headers: { 'X-Muster-Step-Up': '1', 'Content-Type': 'text/plain' }, body: 'confirm it is you' });
          }
          return r.continue();
        },
      );
      await revealBox(page);
      const text = `pressed before the switch ${kind} ${vp.name} ${Date.now()}`;
      await page.locator('#mv-input').fill(text);
      if (kind === 'send') {
        await page.locator('#mv-send').click();
        await expect(pendingOf(page, text)).toHaveAttribute('data-pending-send', 'stepup');
      } else {
        await page.locator('#mv-send-more').click();
        await page.locator('#mv-queue-send').click();
      }
      await expect(page.locator('#term-confirm-password')).toBeVisible({ timeout: 10_000 });
      // The pane switches; the view starts over on the other session.
      writeFileAtomic(ocWindows, windowsLine('OC | Bound fixture session').replace('OC | Title-matched fixture session', 'OC | Switched fixture session'));
      try {
        await expect(notice).toContainText('matched by title, verified: Switched fixture session', { timeout: 30_000 });
        await expect(page.locator(`input[name="agent_session"]`)).toHaveValue(switched);
        await page.locator('#term-confirm-password').fill(password);
        await page.locator('#term-confirm-submit').click();
        if (kind === 'send') {
          const p = pendingOf(page, text);
          await expect(p).toHaveAttribute('data-pending-send', 'failed', { timeout: 20_000 });
          await expect(p).toContainText('shows another session now than the one this message was written for');
        } else {
          // Queued, naming @3's old session; the session switched to runs a
          // turn, the queue tries the row, and the hub refuses it.
          await expect(page.locator('#mv-queue [data-queue-id]', { hasText: text })).toHaveCount(1, { timeout: 20_000 });
          op({ op: 'status', session: switched, status: 'busy' });
          op({ op: 'status', session: switched, status: 'idle' });
          const row = page.locator('#mv-queue [data-queue-id]', { hasText: text });
          await expect
            .poll(
              async () => {
                await page.goto(rig.origin + viewOf('@3'));
                return row.getAttribute('data-queue-state');
              },
              { timeout: 40_000, intervals: [1_000] },
            )
            .toBe('failed');
          await expect(row).toContainText('shows another session now than the one this message was written for');
        }
        // It never landed in the session switched to.
        await page.waitForTimeout(2_000);
        await expect(rowsOf(page, text)).toHaveCount(0);
      } finally {
        writeFileAtomic(ocWindows, windowsLine('OC | Bound fixture session'));
      }
    });
  }
}

// THE TWO OTHER CARRIERS OF THE PRESS-TIME SESSION (composer_pending.go): a
// Retry is the same message, so it names the session the message was
// written for, not the one the view shows when Retry is pressed; and a
// message handed to the browser's own submit (native: the page's fetch
// failed) posts that session too, not the field as the view left it. Each
// post's agent_session is read off the wire, and the message lands in neither
// session.
const postedSession = (body: string | null) => new URLSearchParams(body || '').get('agent_session');
for (const vp of viewports) {
  test(`a Retry after the pane switches session names the session it was written for (${vp.name})`, async ({ page }) => {
    test.setTimeout(90_000);
    await page.setViewportSize({ width: vp.width, height: vp.height });
    await rig.login(page);
    await page.goto(rig.origin + viewOf('@3'));
    const notice = page.locator('#mv-log [data-notice-row="opencode"]').first();
    await expect(notice).toContainText('matched by title, verified: Title-matched fixture session', { timeout: 20_000 });
    // The first post is refused (nothing delivered): the bubble fails, with Retry.
    const posted: (string | null)[] = [];
    await page.route(
      (u) => u.pathname === '/mirror/view/send',
      async (r: Route) => {
        posted.push(postedSession(r.request().postData()));
        if (posted.length === 1) return r.fulfill({ status: 409, headers: { 'Content-Type': 'text/plain' }, body: 'fixture: not now' });
        return r.continue();
      },
    );
    await revealBox(page);
    const text = `retried after the switch ${vp.name} ${Date.now()}`;
    await page.locator('#mv-input').fill(text);
    await page.locator('#mv-send').click();
    const p = pendingOf(page, text);
    await expect(p).toHaveAttribute('data-pending-send', 'failed', { timeout: 20_000 });
    await expect(p).toContainText('fixture: not now');
    writeFileAtomic(ocWindows, windowsLine('OC | Bound fixture session').replace('OC | Title-matched fixture session', 'OC | Switched fixture session'));
    try {
      await expect(notice).toContainText('matched by title, verified: Switched fixture session', { timeout: 30_000 });
      await expect(page.locator(`input[name="agent_session"]`)).toHaveValue(switched);
      await p.locator('[data-pending-retry]').click();
      await expect.poll(() => posted.length, { timeout: 20_000 }).toBe(2);
      expect(posted, 'the press and its Retry both name the session the message was written for').toEqual([titled, titled]);
      await expect(p).toHaveAttribute('data-pending-send', 'failed', { timeout: 20_000 });
      await expect(p).toContainText('shows another session now than the one this message was written for');
      await page.waitForTimeout(2_000);
      await expect(rowsOf(page, text)).toHaveCount(0);
    } finally {
      writeFileAtomic(ocWindows, windowsLine('OC | Bound fixture session'));
    }
  });

  test(`a message handed to the browser's own submit after the pane switches session names the session it was written for (${vp.name})`, async ({ page }) => {
    test.setTimeout(90_000);
    await page.setViewportSize({ width: vp.width, height: vp.height });
    await rig.login(page);
    await page.goto(rig.origin + viewOf('@3'));
    const notice = page.locator('#mv-log [data-notice-row="opencode"]').first();
    await expect(notice).toContainText('matched by title, verified: Title-matched fixture session', { timeout: 20_000 });
    // The page's own post is held, then fails as a network error would: the
    // composer hands the message to the browser's own form submit.
    const posted: { session: string | null; xhr: boolean }[] = [];
    let release: () => void = () => {};
    const held = new Promise<void>((resolve) => (release = resolve));
    await page.route(
      (u) => u.pathname === '/mirror/view/send',
      async (r: Route) => {
        const xhr = !!r.request().headers()['x-requested-with'];
        posted.push({ session: postedSession(r.request().postData()), xhr });
        if (xhr && posted.length === 1) {
          await held;
          return r.abort('failed');
        }
        return r.continue();
      },
    );
    await revealBox(page);
    const text = `handed off after the switch ${vp.name} ${Date.now()}`;
    await page.locator('#mv-input').fill(text);
    await page.locator('#mv-send').click();
    await expect.poll(() => posted.length, { timeout: 20_000 }).toBe(1);
    writeFileAtomic(ocWindows, windowsLine('OC | Bound fixture session').replace('OC | Title-matched fixture session', 'OC | Switched fixture session'));
    try {
      await expect(notice).toContainText('matched by title, verified: Switched fixture session', { timeout: 30_000 });
      await expect(page.locator(`input[name="agent_session"]`)).toHaveValue(switched);
      release();
      await expect.poll(() => posted.length, { timeout: 20_000 }).toBe(2);
      expect(posted, "the page's post and the browser's own both name the session the message was written for").toEqual([
        { session: titled, xhr: true },
        { session: titled, xhr: false },
      ]);
      await page.waitForLoadState('load');
      await page.goto(rig.origin + viewOf('@3'));
      await page.waitForTimeout(2_000);
      await expect(rowsOf(page, text)).toHaveCount(0);
    } finally {
      writeFileAtomic(ocWindows, windowsLine('OC | Bound fixture session'));
    }
  });
}
