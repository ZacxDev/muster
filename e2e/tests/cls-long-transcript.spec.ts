import { Browser, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { authState, baseURL, clsLongMirrorPort } from '../lib/env';
import { clsInit, readCls, settle, ClsResult } from '../lib/cls';
import { cc, MirrorRig, mkchat } from '../lib/mirrorhost';
import { Profile, THROTTLED, throttle } from '../lib/throttle';

// A LONG TRANSCRIPT SHIFTS LESS THAN 0.01, at a phone and a desktop, on an
// unthrottled loopback AND under the throttled profile (lib/throttle.ts).
//
// cls.spec.ts measures every page, but its agent page is the seeded
// docs-sweeper: seven messages, which fit on one screen. An operator's agent
// holds up to chatHistoryCap (200, internal/api/agents.go) messages, and the
// page opens scrolled to the end of them. This spec opens agents of its own
// (e2e/mkchat) holding exactly that many, in two variants:
//
//   - markdown: assistant replies with a heading, emphasis, a list, inline code
//     and a fenced block, long enough for the bubble clamp (bubble_clamp.go);
//   - plain: the same lengths in plain sentences, so a shift that is not the
//     markdown conversion is still seen.
//
// 🔴 REGRESSION TEST. Before the fix, the transcript was converted, clamped and
// scrolled to its end only at DOMContentLoaded, which waits for the DEFERRED
// vendor scripts; whenever they landed after the document had parsed, the raw
// transcript was painted first, scrolled to the top, and then every bubble on
// screen moved (agentChatScript's window.__cgLayoutLog says how). Unthrottled
// that is a race this harness usually wins; the throttled profile loses it
// every time, so the throttled cases are the deterministic red.
//
// 🔴 THE PHONE'S NUMBER IS total + excluded, NOT total. Under Playwright's
// mobile emulation (isMobile + hasTouch) Chromium reports a shift in the first
// moments after load with hadRecentInput=true though nothing was input —
// measured: a block injected 100 ms after load was all `excluded`, the same
// block 3 s after load all `total`; at 390px without the emulation, both
// `total`. This spec gives the page no input at all, so every excluded shift is
// that artefact, and it is counted. (cls.spec.ts's phone measurements count
// total only, and so do not see a shift in that window.)
//
// Every message is invented fixture data.
const BAR = 0.01;
const CAP = 200;
// Each case is opened RUNS times, each in a fresh context (a cold HTTP cache),
// because the defect is a race with the first paint.
const RUNS = 3;
const stamp = Date.now().toString(36);
type Kind = 'markdown' | 'plain';
const agents: Record<Kind, string> = { markdown: `cls-long-md-${stamp}`, plain: `cls-long-plain-${stamp}` };

const sentences = (i: number, n: number) =>
  Array.from({ length: n }, (_, j) => `Sentence ${j + 1} about module-${i}: the value was checked against the expected default and matched after the last migration ran.`).join(' ');

function transcript(kind: Kind): { role: string; content: string }[] {
  const out: { role: string; content: string }[] = [];
  for (let i = 0; i < CAP; i++) {
    if (i % 2 === 0) {
      out.push({ role: 'user', content: `Step ${i / 2 + 1}: ${sentences(i, 1)}` });
    } else if (kind === 'markdown') {
      out.push({
        role: 'assistant',
        content: `## Module ${i}\n\nChecked **module-${i}**. ${sentences(i, 2)}\n\n- one \`key_a\`\n- two \`key_b\`\n- three\n\n\`\`\`sh\nmake build-${i}\nmake test-${i}\n\`\`\`\n\n${sentences(i, 3)}`,
      });
    } else {
      out.push({ role: 'assistant', content: `${sentences(i, 2)}\n\n${sentences(i, 3)}` });
    }
  }
  return out;
}

const viewports = [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
];
const profiles: { name: string; p: Profile | null }[] = [
  { name: 'unthrottled', p: null },
  { name: THROTTLED.name, p: THROTTLED },
];

// moved is every shift measured, input-excluded or not (see the header).
const moved = (r: ClsResult) => +(r.total + r.excluded).toFixed(5);
const report = (label: string, r: ClsResult) => `${label}: ${moved(r)} (total ${r.total}, excluded ${r.excluded}) — ${JSON.stringify(r.shifts)}`;

// open loads url in a fresh signed-in context under profile p and lets it
// settle. The caller closes the context.
async function open(browser: Browser, use: object, p: Profile | null, url: string, o: { origin?: string; init?: string; state?: string | object } = {}) {
  const origin = o.origin ?? baseURL;
  const ctx = await browser.newContext({ ...use, baseURL: origin, storageState: (o.state ?? authState) as never });
  const page = await ctx.newPage();
  await throttle(page, p);
  await page.addInitScript(clsInit);
  if (o.init) await page.addInitScript(o.init);
  await page.goto(origin + url);
  await settle(page);
  return { ctx, page };
}

async function atEnd(page: Page, log: string): Promise<boolean> {
  return page.locator(log).evaluate((l) => l.scrollHeight - l.scrollTop - l.clientHeight < 48);
}

test.describe('a 200-message agent transcript', () => {
  test.beforeAll(() => {
    for (const k of ['markdown', 'plain'] as const) {
      mkchat(agents[k], null);
      mkchat(agents[k], transcript(k));
    }
  });
  test.afterAll(() => {
    for (const k of ['markdown', 'plain'] as const) mkchat(agents[k], null);
  });

  for (const vp of viewports) {
    for (const pr of profiles) {
      for (const kind of ['markdown', 'plain'] as const) {
        test(`${vp.name}, ${pr.name}, ${kind}: opening the agent shifts less than ${BAR}, ${RUNS} times`, async ({ browser }) => {
          test.setTimeout(120_000);
          const seen: number[] = [];
          const bad: string[] = [];
          for (let i = 0; i < RUNS; i++) {
            const { ctx, page } = await open(browser, vp.use, pr.p, `/agents/${agents[kind]}`);
            try {
              const r = await readCls(page);
              // What was measured is the drawn transcript, opened at its end.
              await expect(page.locator('#chat-log [data-bubble]')).toHaveCount(CAP);
              await expect(page.locator('#chat-log [data-md-done]'), 'every bubble was converted').toHaveCount(CAP);
              expect(await atEnd(page, '#chat-log'), 'the log opened at its end').toBe(true);
              seen.push(moved(r));
              if (moved(r) >= BAR) bad.push(report(`run ${i + 1}`, r));
            } finally {
              await ctx.close();
            }
          }
          console.log(`[cls-long ${vp.name}, ${pr.name}] ${kind}: ${seen.join(' ')}`);
          expect(bad).toEqual([]);
        });
      }
    }

    // THE MECHANISM, WITHOUT THE RACE. The deferred vendor scripts are held
    // unanswered, so the document parses and paints with DOMContentLoaded (and
    // start()) still waiting on them — the window the defect painted in. In it,
    // the transcript must already be in its final layout: every bubble
    // converted, the log at its end (with #chat-form, parsed after the layout
    // call, under it), and nothing moved. Then the scripts are let through, and
    // the whole open still moves less than the bar.
    for (const kind of ['markdown', 'plain'] as const) {
      test(`${vp.name}, ${kind}: before the deferred scripts arrive, the transcript is already laid out`, async ({ browser }) => {
        const ctx = await browser.newContext({ ...vp.use, baseURL, storageState: authState });
        try {
          const page = await ctx.newPage();
          let release: () => void = () => {};
          const gate = new Promise<void>((r) => (release = r));
          let held = 0;
          await page.route('**/static/vendor/*.js*', async (route) => {
            held++;
            await gate;
            await route.continue().catch(() => {});
          });
          await page.addInitScript(clsInit);
          await page.goto(`/agents/${agents[kind]}`, { waitUntil: 'commit' });
          await expect.poll(() => page.evaluate(() => document.readyState), { message: 'the document parsed' }).toBe('interactive');
          // A few frames painted while the scripts are held.
          await page.evaluate(() => new Promise<void>((r) => { let n = 0; const f = () => (++n < 10 ? requestAnimationFrame(f) : r()); requestAnimationFrame(f); }));
          expect(held, 'both deferred vendor scripts are being held').toBe(2);
          expect(await page.evaluate(() => !!(window as any).htmx), 'control: htmx has not run').toBe(false);
          await expect(page.locator('#chat-log [data-md-done]'), 'every bubble converted before start()').toHaveCount(CAP);
          expect(await atEnd(page, '#chat-log'), 'the log is at its end before start()').toBe(true);
          const before = await readCls(page);
          release();
          await expect.poll(() => page.evaluate(() => !!(window as any).__cgChat), { message: 'start() ran once the scripts landed' }).toBe(true);
          await settle(page);
          const after = await readCls(page);
          console.log(`[cls-long ${vp.name}] ${kind}, scripts held: ${moved(before)}, after they land: ${moved(after)}`);
          expect(moved(before), report('while the scripts were held', before)).toBeLessThan(BAR);
          expect(moved(after), report('the whole open', after)).toBeLessThan(BAR);
          expect(await atEnd(page, '#chat-log'), 'still at its end').toBe(true);
        } finally {
          await ctx.close();
        }
      });
    }

    // The positive control for the counting: a shift the page did not ask for,
    // 100 ms after load (unthrottled, the window in which the phone's emulation
    // marks a shift as input), on this same page, MUST be measured.
    test(`${vp.name}: positive control: an unprovoked shift just after load is measured`, async ({ browser }) => {
      const init = `addEventListener('load', () => setTimeout(() => {
        const l = document.getElementById('chat-log');
        const d = document.createElement('div');
        d.style.height = '160px';
        l.parentElement.insertBefore(d, l);
      }, 100));`;
      const { ctx, page } = await open(browser, vp.use, null, `/agents/${agents.markdown}`, { init });
      try {
        const r = await readCls(page);
        console.log(`[cls-long ${vp.name}] positive control: ${report('injected', r)}`);
        expect(moved(r), report('injected shift', r)).toBeGreaterThan(BAR);
      } finally {
        await ctx.close();
      }
    });
  }
});

// --- THE MIRRORED (tmux) RENDERED VIEW, with a long Claude Code transcript. Its
// transcript is not in the document: it arrives over /mirror/view/ws from the
// host after load (mirror_view.go), as server-rendered markdown. One fake host,
// whose `main` session is a Claude Code pane over CAP records — the markdown
// variant's prompts and replies.
const rig = new MirrorRig(clsLongMirrorPort, 'e2e-clslong');
const view = () => `/mirror/view?host=${encodeURIComponent(rig.host)}&session=main`;

test.describe('a 200-message mirrored transcript', () => {
  let state: object = {};
  test.beforeAll(async ({ browser }) => {
    test.setTimeout(180_000);
    await rig.start();
    const body = transcript('markdown').map((m) => (m.role === 'user' ? cc.prompt(m.content) : cc.reply(m.content))).join('');
    await rig.startHost({ MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_E2E_SESSIONS: 'main', MUSTER_MIRROR_E2E_CLAUDE: 'main=' + rig.transcript(body) });
    // Signed in to THIS server: an empty state, or the suite's default (the
    // shared server's cookie) comes along and /login is never drawn.
    const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    const page = await ctx.newPage();
    await rig.login(page);
    await expect
      .poll(async () => (await (await page.request.get(rig.origin + '/ui/agents')).text()).includes('data-mirror-claude'), {
        message: `main reported as a Claude pane (muster-mirror log:\n${rig.log})`,
        timeout: 20_000,
      })
      .toBe(true);
    state = await ctx.storageState();
    await ctx.close();
  });
  test.afterAll(async () => {
    await rig.stop();
  });

  for (const vp of viewports) {
    for (const pr of profiles) {
      test(`${vp.name}, ${pr.name}: opening the rendered view shifts less than ${BAR}`, async ({ browser }) => {
        test.setTimeout(120_000);
        const { ctx, page } = await open(browser, vp.use, pr.p, view(), { origin: rig.origin, state });
        try {
          // The transcript arrives after load: wait for all of it, then settle.
          await expect(page.locator('#mv-log [data-bubble]'), 'the view drew the whole transcript').toHaveCount(CAP, { timeout: 30_000 });
          await settle(page);
          const r = await readCls(page);
          expect(await atEnd(page, '#mv-log'), 'the view opened at its end').toBe(true);
          console.log(`[cls-long mirror ${vp.name}, ${pr.name}] ${moved(r)}`);
          expect(moved(r), report('mirrored view', r)).toBeLessThan(BAR);
        } finally {
          await ctx.close();
        }
      });
    }

    // The positive control on THIS page: a block put above the log, unasked,
    // once the transcript has landed, MUST be measured — so the zeros above are
    // not a page the instrument cannot see.
    test(`${vp.name}: positive control: an unprovoked shift on the rendered view is measured`, async ({ browser }) => {
      const { ctx, page } = await open(browser, vp.use, null, view(), { origin: rig.origin, state });
      try {
        await expect(page.locator('#mv-log [data-bubble]')).toHaveCount(CAP, { timeout: 30_000 });
        await page.evaluate(() => {
          const l = document.getElementById('mv-log')!;
          const d = document.createElement('div');
          d.style.height = '160px';
          l.parentElement!.insertBefore(d, l);
        });
        await settle(page, 500);
        const r = await readCls(page);
        console.log(`[cls-long mirror ${vp.name}] positive control: ${report('injected', r)}`);
        expect(moved(r), report('injected shift', r)).toBeGreaterThan(BAR);
      } finally {
        await ctx.close();
      }
    });
  }
});
