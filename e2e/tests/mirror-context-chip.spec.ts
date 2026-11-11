import fs from 'node:fs';
import path from 'node:path';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { contextChipPort } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';

// THE MIRRORED VIEW'S CONTEXT CHIP (internal/ui/mirror_view.go's
// mirrorContextChip and contextView), ON A REAL SERVER (the e2eterm binary,
// mirror plugin on) AND A REAL muster-mirror whose fakes are tmux and the
// opencode server (cmd/muster-mirror/e2eopencode.go). The fake server reports
// a session on a LONG provider-prefixed model id with a large window; its
// steps fill the window past 70% and then 90%. Every value is invented
// fixture data.
//
// 🔴 REGRESSION TEST. On origin/main the chip was `shrink-0` with the whole
// "<provider>/<model> · 29k / 1.8M context" in it, and at 390px it ran out of
// the header (the document scrolled sideways); it had no detail on tap and no
// tint.

const rig = new MirrorRig(contextChipPort, 'e2e-ctxchip');
const sid = 'ses_0e2ectxchip00001';
const model = 'someprovider/some-very-long-model-name-v9-experimental';
const shortName = 'some-very-long-model-name-v9-experimental';
// The window, and three steps' context (input + cache read + cache write):
// 2%, 70.6% and 92.4% of it. None is a threshold or a round number.
const windowSize = 1_843_200;
const t0 = Math.floor(Date.now() / 1000);
let ops = '';

function op(o: Record<string, unknown>) {
  fs.appendFileSync(ops, JSON.stringify(o) + '\n');
}

let step = 0;
function assistantStep(input: number, read: number) {
  step++;
  op({ op: 'message', session: sid, message: { id: `msg_0e2echip${step}a`, type: 'user', text: `fill step ${step}`, time: { created: 1700000000000 + step * 10 } } });
  op({
    op: 'message',
    session: sid,
    message: {
      id: `msg_0e2echip${step}b`,
      type: 'assistant',
      agent: 'build',
      model: { id: model, providerID: 'fixture-router' },
      finish: 'stop',
      time: { created: 1700000000001 + step * 10, completed: 1700000000005 + step * 10 },
      tokens: { input, output: 313, reasoning: 0, cache: { read, write: 0 } },
      content: [{ type: 'text', text: `Step ${step} done.` }],
    },
  });
}

test.beforeAll(async ({ browser }) => {
  test.setTimeout(180_000);
  await rig.start();
  const dir = fs.mkdtempSync(path.join(rig.dir, 'opencode-'));
  ops = path.join(dir, 'ops.jsonl');
  // A long title too, so the header's title is clipped (its button's 44px
  // hit area then spans the chip's row: mirrorTitleHitClass).
  op({ op: 'session', id: sid, title: 'A deliberately long fixture session title that cannot fit a phone header' });
  op({ op: 'models', models: [{ provider: 'fixture-router', id: model, context: windowSize }] });
  assistantStep(20_011, 9_203); // 29,214
  const windows = path.join(rig.dir, 'windows');
  writeFileAtomic(windows, `plain|@1|0|1|zsh|${t0 - 50}|\n`);
  const ocWindows = path.join(rig.dir, 'opencode-windows');
  writeFileAtomic(ocWindows, `oc|@2|0|1|${t0 - 10}|OC | A deliberately long fixture session title that cannot fit a phone header\n`);
  await rig.startHost({
    MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
    MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE: ocWindows,
    MUSTER_MIRROR_E2E_OPENCODE_DIR: dir,
    MUSTER_MIRROR_E2E_REFRESH: '200ms',
  });
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await rig.login(page);
  await rig.waitListed(page, 'session=oc');
  await ctx.close();
});

test.afterAll(async () => {
  await rig.stop();
});

test.use({ storageState: { cookies: [], origins: [] }, hasTouch: true });

const view = () => `${rig.viewPath('oc')}&window=${encodeURIComponent('@2')}`;
const widths = [
  { name: 'phone', width: 390, height: 844 },
  { name: 'desktop', width: 1280, height: 800 },
];

// fits: nothing in the header (or the page) scrolls sideways, and the back
// arrow and the view switch are wholly on screen.
async function expectFits(page: Page, label: string) {
  const m = await page.evaluate(() => {
    const h = document.querySelector('header')!;
    const box = (sel: string) => {
      const r = document.querySelector(sel)!.getBoundingClientRect();
      return { left: r.left, right: r.right, height: r.height };
    };
    return {
      header: [h.scrollWidth, h.clientWidth],
      doc: [document.documentElement.scrollWidth, document.documentElement.clientWidth],
      vw: window.innerWidth,
      back: box('#mv-back'),
      toggle: box('header nav[aria-label="View"]'),
      chip: box('#mv-context'),
      count: box('#mv-context [data-ctx-count]'),
      name: box('#mv-context [data-ctx-name]'),
      // The first header control right of the chip's column.
      next: box('#chat-index-open'),
    };
  });
  const at = `${label} (${JSON.stringify(m)})`;
  expect(m.header[0], `${at}: the header scrolls sideways`).toBeLessThanOrEqual(m.header[1]);
  expect(m.doc[0], `${at}: the page scrolls sideways`).toBeLessThanOrEqual(m.doc[1]);
  for (const [name, b] of [['back', m.back], ['view switch', m.toggle], ['chip', m.chip], ['count', m.count]] as const) {
    expect(b.left, `${at}: the ${name} starts off screen`).toBeGreaterThanOrEqual(0);
    expect(b.right, `${at}: the ${name} ends off screen`).toBeLessThanOrEqual(m.vw);
  }
  // Nothing spills out of the chip (a name that cannot shrink pushes the
  // count past the chip's end, over whatever is next), and the chip ends
  // before the next control starts.
  expect(m.count.right, `${at}: the count spills out of the chip`).toBeLessThanOrEqual(m.chip.right + 0.5);
  expect(m.chip.right, `${at}: the chip runs into the next control`).toBeLessThanOrEqual(m.next.left);
  // ONE line: a name that cannot shrink does not only overflow — a hyphenated
  // id WRAPS at its hyphens instead, and the chip grows down the header.
  expect(m.name.height, `${at}: the model name wraps onto more than one line`).toBeLessThanOrEqual(m.count.height + 0.5);
}

test('the context chip is short, fits the header at every width, opens its detail on a tap, and tints as the window fills', async ({ page }) => {
  test.setTimeout(120_000);
  await page.setViewportSize(widths[0]);
  await rig.login(page);
  await page.goto(rig.origin + view());
  const chip = page.locator('#mv-context');
  const name = chip.locator('[data-ctx-name]');
  const count = chip.locator('[data-ctx-count]');
  const pop = page.locator('#mv-context-detail');

  // Short: the model without its provider, the counts without spaces or "context".
  await expect(chip, `the chip (mirror log:\n${rig.log})`).toHaveText(`${shortName} · 29k/1.8M`, { timeout: 20_000 });
  await expect(chip).not.toHaveAttribute('data-level');
  await expect(chip).toHaveAttribute('title', `${model} — 29,214 of 1,843,200 tokens (2%)`);

  for (const vp of widths) {
    await page.setViewportSize(vp);
    await expectFits(page, `${vp.name}, 2%`);
  }
  // At 390px the model name is what gives: clipped, the count whole.
  await page.setViewportSize(widths[0]);
  const clip = await name.evaluate((e) => [e.scrollWidth, e.clientWidth]);
  expect(clip[0], 'the model name is truncated at 390px').toBeGreaterThan(clip[1]);
  const cnt = await count.evaluate((e) => [e.scrollWidth, e.clientWidth]);
  expect(cnt[0], 'the count is never truncated').toBeLessThanOrEqual(cnt[1]);

  // A tap opens the exact readout (the whole model id and exact counts).
  await expect(pop).toBeHidden();
  await chip.tap();
  await expect(pop).toBeVisible();
  await expect(pop.locator('[data-ctx-detail]')).toHaveText(`${model} — 29,214 of 1,843,200 tokens (2%)`);
  await expect(pop.locator('[data-ctx-how]')).toContainText('input + cache read + cache write');
  await expect(chip).toHaveAttribute('aria-expanded', 'true');
  const pb = await pop.boundingBox();
  expect(pb && pb.x >= 0 && pb.x + pb.width <= widths[0].width, `the popover is on screen: ${JSON.stringify(pb)}`).toBe(true);
  await page.keyboard.press('Escape');
  await expect(pop).toBeHidden();

  // 70.6%: amber (the warning token); 92.4%: red (the error token).
  const bg = () => chip.evaluate((e) => getComputedStyle(e).backgroundColor);
  const plain = await bg();
  const token = (cls: string) =>
    page.evaluate((c) => {
      const d = document.createElement('span');
      d.className = c;
      document.body.appendChild(d);
      const v = getComputedStyle(d).backgroundColor;
      d.remove();
      return v;
    }, cls);
  const warnBg = await token('bg-st-warning-bg');
  const errBg = await token('bg-st-error-bg');
  expect(new Set([plain, warnBg, errBg]).size, 'the three tints are distinct colours').toBe(3);

  assistantStep(1_290_377, 11_000); // 1,301,377
  await expect(chip).toHaveText(`${shortName} · 1.3M/1.8M`, { timeout: 20_000 });
  await expect(chip).toHaveAttribute('data-level', 'warn');
  expect(await bg()).toBe(warnBg);
  for (const vp of widths) {
    await page.setViewportSize(vp);
    await expectFits(page, `${vp.name}, 71%`);
  }

  assistantStep(1_690_419, 12_000); // 1,702,419
  await expect(chip).toHaveText(`${shortName} · 1.7M/1.8M`, { timeout: 20_000 });
  await expect(chip).toHaveAttribute('data-level', 'danger');
  expect(await bg()).toBe(errBg);
  await page.setViewportSize(widths[1]);
  await chip.click();
  await expect(pop.locator('[data-ctx-detail]')).toHaveText(`${model} — 1,702,419 of 1,843,200 tokens (92%)`);
  await page.keyboard.press('Escape');
  for (const vp of widths) {
    await page.setViewportSize(vp);
    await expectFits(page, `${vp.name}, 92%`);
  }
});
