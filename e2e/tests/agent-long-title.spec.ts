import { execFileSync } from 'node:child_process';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, repoRoot } from '../lib/env';

// A LONG TITLE MUST NOT BREAK THE AGENT PAGE (operator report: "A dispatched
// cluster agent … had a very long title and it completely breaks the UI").
// A dispatched agent is named after its task, and a task title has no length
// limit. The header drew the name whole (break-all), so a long one became a
// wall of lines over the chat. Now the header shows at most 60 characters on
// one line and a "…" button that opens the whole title in a popover.
//
// Synthetic titles only: one 600-character token with no break opportunity,
// and one made of long words and markdown on a task-linked agent (whose title
// is the task-detail button, so the "…" must sit beside it, not inside it).
// The agent rows are written straight into the database, as a dispatch writes
// them, and removed afterwards: the shared server's other specs count agents.

const stamp = Date.now().toString(36);
const token = (n: string) => 'e2e-long-title-token-not-a-real-one-' + n + '-' + stamp;
const unbroken = 'x'.repeat(600);
const wordy =
  '#803 · **Refactor** the `importerRetryBackoffControllerConfiguration` so ' +
  'supercalifragilisticexpialidocious '.repeat(12) + '_drains_ on restart';
// What the page draws for `wordy` once textplain strips the markdown.
const wordyPlain =
  '#803 · Refactor the importerRetryBackoffControllerConfiguration so ' +
  'supercalifragilisticexpialidocious '.repeat(12) + 'drains on restart';

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

const agents: { name: string; id: string; title: string; full: string; linked: boolean }[] = [
  { name: 'e2e-long-title-a-' + stamp, id: '', title: unbroken, full: unbroken, linked: false },
  { name: 'e2e-long-title-b-' + stamp, id: '', title: wordy, full: wordyPlain, linked: true },
];

test.beforeAll(async ({ api }) => {
  for (const a of agents) {
    a.id = fix('agent', '1', a.name, token(a.name));
    fix('label', a.id, a.title);
    if (a.linked) {
      const t = await api.createTask('Long-title spec task ' + stamp, 'A synthetic task for the long-title spec.');
      fix('linknote', a.id, String(t.id));
    }
  }
});

test.afterAll(() => {
  for (const a of agents) if (a.id) fix('rmagent', '1', a.name);
});

// Every box the header and the composer draw is inside the viewport, and the
// document is not wider than it.
async function assertFits(page: Page, label: string) {
  const m = await page.evaluate(() => {
    const vw = window.innerWidth, vh = window.innerHeight;
    const boxes = (sel: string) =>
      Array.from(document.querySelectorAll<HTMLElement>(sel))
        .filter((el) => el.offsetParent !== null)
        .map((el) => {
          const r = el.getBoundingClientRect();
          return { what: el.id || el.getAttribute('aria-label') || el.tagName, left: r.left, right: r.right, top: r.top, bottom: r.bottom };
        });
    const h1 = document.querySelector('header h1')!.getBoundingClientRect();
    return {
      vw, vh,
      docScroll: document.documentElement.scrollWidth, docClient: document.documentElement.clientWidth,
      bodyScroll: document.body.scrollWidth, bodyClient: document.body.clientWidth,
      header: boxes('header button, header a, header select, header input'),
      composer: boxes('#chat-input, #chat-send'),
      h1Height: h1.height,
    };
  });
  expect(m.docScroll, `${label}: the document scrolls sideways`).toBeLessThanOrEqual(m.docClient);
  expect(m.bodyScroll, `${label}: the body scrolls sideways`).toBeLessThanOrEqual(m.bodyClient);
  expect(m.header.length, `${label}: positive control — the header has controls to measure`).toBeGreaterThan(2);
  expect(m.composer.length, `${label}: positive control — the composer is drawn`).toBe(2);
  for (const b of [...m.header, ...m.composer]) {
    expect(b.left, `${label}: ${b.what} starts off screen`).toBeGreaterThanOrEqual(-0.5);
    expect(b.right, `${label}: ${b.what} runs off the right edge`).toBeLessThanOrEqual(m.vw + 0.5);
    expect(b.bottom, `${label}: ${b.what} is below the screen`).toBeLessThanOrEqual(m.vh + 0.5);
  }
  // ONE line: a wrapped title is the wall of text the report was about.
  expect(m.h1Height, `${label}: the title wraps onto more than one line`).toBeLessThanOrEqual(48);
}

for (const vp of [
  { width: 390, height: 844, mobile: true },
  { width: 1280, height: 800, mobile: false },
]) {
  test.describe(`at ${vp.width}x${vp.height}`, () => {
    test.use({ viewport: { width: vp.width, height: vp.height }, hasTouch: vp.mobile, isMobile: vp.mobile });

    for (const which of [0, 1]) {
      test(`a long ${which === 0 ? 'unbroken' : 'task-linked markdown'} title fits one line and opens whole from its "…"`, async ({ page }) => {
        const a = agents[which];
        await page.goto('/agents/' + a.name);
        await expect(page.locator('#chat-input')).toBeVisible();
        await assertFits(page, a.name);

        // The header shows the first 60 characters, not the whole title.
        const shown = (await page.locator('header h1').innerText()).trim();
        expect(shown).toBe(a.full.slice(0, 60).trimEnd());
        await expect(page).toHaveTitle(a.full.slice(0, 60).trimEnd() + '… · muster');

        const more = page.getByRole('button', { name: 'Show the full title' });
        const pop = page.locator('#agent-title-full');
        await expect(more).toBeVisible();
        const mb = (await more.boundingBox())!;
        expect(mb.height).toBeGreaterThanOrEqual(44);
        expect(mb.width).toBeGreaterThanOrEqual(44);
        await expect(more).toHaveAttribute('aria-expanded', 'false');
        await expect(pop).toBeHidden();

        // Tap/click opens it, with the whole title, inside the viewport.
        await more.click();
        await expect(pop).toBeVisible();
        await expect(more).toHaveAttribute('aria-expanded', 'true');
        await expect(pop).toHaveText(a.full.trim());
        const pb = (await pop.boundingBox())!;
        expect(pb.x).toBeGreaterThanOrEqual(0);
        expect(pb.x + pb.width).toBeLessThanOrEqual(vp.width);
        expect(pb.y).toBeGreaterThanOrEqual(0);
        expect(pb.y + pb.height).toBeLessThanOrEqual(vp.height);
        // The open popover widens nothing either.
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);

        // Escape closes it.
        await page.keyboard.press('Escape');
        await expect(pop).toBeHidden();
        await expect(more).toHaveAttribute('aria-expanded', 'false');

        // The keyboard opens it, and the button again closes it.
        await more.focus();
        await page.keyboard.press('Enter');
        await expect(pop).toBeVisible();
        await more.click();
        await expect(pop).toBeHidden();

        // A tap outside closes it.
        await more.click();
        await expect(pop).toBeVisible();
        const out = { x: vp.width - 10, y: Math.round(vp.height * 0.6) };
        const ob = (await pop.boundingBox())!;
        expect(out.x < ob.x || out.x > ob.x + ob.width || out.y < ob.y || out.y > ob.y + ob.height, 'the outside tap lands on the popover').toBe(true);
        await page.mouse.click(out.x, out.y);
        await expect(pop).toBeHidden();

        if (a.linked) {
          // The title is still the task-detail button, and the "…" did not
          // become part of it: tapping the words opens the task modal.
          await page.getByRole('button', { name: 'View task details' }).click();
          await expect(page.locator('#task-modal')).not.toBeEmpty();
        }
      });
    }
  });
}
