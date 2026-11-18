import http from 'node:http';
import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { chatMarkdownPort, chatMarkdownRuntimePort, dsn, repoRoot, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';

// THE AGENT CHAT'S MESSAGES ARE DRAWN BY THE SERVER'S ONE MARKDOWN RENDERER
// (internal/ui chatMessageBody / ChatMessageHTML; #492's D1). The page used to
// carry a JS copy of it, which disagreed with the Go one: a `_` at the end of a
// bare URL corrupted the anchor's target attribute, a `*` inside one became
// <em> in the href. Here, on a phone and a desktop, every way a message reaches
// /agents/{name} is read back as its DOM:
//
//   - the owner's own message, the moment it is sent, then as the socket's
//     "sent" echo;
//   - the streamed reply, WHILE it streams (its turn held open by the runtime);
//   - both again once stored, swapped in by the chat.reply refetch;
//   - a reply to a message sent from ANOTHER page, which this page receives
//     only through its event stream (sse:chat.reply → #chat-log refetch).
//
// Each is compared as the bubble's whole innerHTML, written out by hand, on
// inputs that carry the cases the two renderers split on and author markup
// that must stay text.
//
// ⚠ WHAT IS PLAYED: the model — a FAKE /v1/responses runtime (the e2eterm
// build's MUSTER_E2E_AGENT_ENDPOINT points every agent at it) that streams a
// fixed reply in three deltas. The agent is a fixture row. The socket, the
// rendering, the stored transcript and the refetch are muster's own.

const origin = `http://localhost:${chatMarkdownPort}`;
const stamp = Date.now().toString(36);
let server: Running | undefined;
let runtime: http.Server | undefined;
const agentsMade: string[] = [];

// A gate the test opens: a held reply waits on it, after streaming its deltas.
class Gate {
  private waiters: (() => void)[] = [];
  private open = false;
  entered = 0;
  wait(): Promise<void> {
    this.entered++;
    if (this.open) return Promise.resolve();
    return new Promise((r) => this.waiters.push(r));
  }
  release() {
    this.open = true;
    for (const w of this.waiters.splice(0)) w();
  }
}
const gates = new Map<string, Gate>();
const gate = (k: string) => {
  if (!gates.has(k)) gates.set(k, new Gate());
  return gates.get(k)!;
};

const sse = (type: string, data: Record<string, unknown>) => `event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`;

// The reply to a held message, in the deltas it streams in. Its cases: `*` in a
// bare URL, a Devanagari vowel sign (Mc) and a superscript digit before `_`,
// a tag that must stay text, and bold split across two deltas.
const HELD_REPLY = ['Here is **bo', 'ld** and https://h.example/a*b*c', ' and सूची_x_. and x²_y_. <img src=x onerror=window.__xss=2>'];
// The reply to a message from another page: a bare URL ending in `_` with a
// later `_` on the line (the JS copy drew target="<em>blank").
const SSE_REPLY = ['**sse** reply_ok https://example.com/foo_ then x_'];

// A reply in two parts with a gate between them, then a gate before its end:
// part one carries a fenced code block (drawn as a <details>) and a marker
// that must appear ONCE however the owner sends while it streams.
const TWO_A = ['MARK-ONE start\n```\nline a\n```\n'];
const TWO_B = ['tail **two**'];

async function streamTwo(res: http.ServerResponse, key: string) {
  res.setHeader('Content-Type', 'text/event-stream');
  for (const d of TWO_A) res.write(sse('response.output_text.delta', { delta: d, output_index: 0 }));
  await gate('a-' + key).wait();
  for (const d of TWO_B) res.write(sse('response.output_text.delta', { delta: d, output_index: 0 }));
  await gate('b-' + key).wait();
  res.end(sse('response.completed', { response: { status: 'completed', output: [{ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: [...TWO_A, ...TWO_B].join('') }] }] } }));
}

async function stream(res: http.ServerResponse, deltas: string[], hold?: Gate) {
  res.setHeader('Content-Type', 'text/event-stream');
  for (const d of deltas) {
    res.write(sse('response.output_text.delta', { delta: d, output_index: 0 }));
    await new Promise((r) => setTimeout(r, 30));
  }
  if (hold) await hold.wait();
  res.end(sse('response.completed', { response: { status: 'completed', output: [{ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: deltas.join('') }] }] } }));
}

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.beforeAll(async () => {
  test.setTimeout(180_000);
  runtime = http.createServer((req, res) => {
    let body = '';
    req.on('data', (d) => (body += d));
    req.on('end', () => {
      if (req.method !== 'POST' || !(req.url || '').endsWith('/v1/responses')) {
        res.statusCode = 404;
        return res.end();
      }
      const two = /MD-TWO-(\w+)/.exec(body);
      if (two) return void streamTwo(res, two[1]);
      const held = /MD-HOLD-(\w+)/.exec(body);
      if (held) return void stream(res, HELD_REPLY, gate(held[1]));
      if (body.includes('MD-SSE-')) return void stream(res, SSE_REPLY);
      return void stream(res, ['ok']);
    });
  });
  await new Promise<void>((r) => runtime!.listen(chatMarkdownRuntimePort, '127.0.0.1', () => r()));
  server = await startServer(versionTerm, chatMarkdownPort, {
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_GATEWAY: 'hooks-sha256',
    MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
    MUSTER_AGENT_RUNTIME_CONFIG: '{}',
    MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
    MUSTER_E2E_AGENT_ENDPOINT: `http://127.0.0.1:${chatMarkdownRuntimePort}`,
  });
});

test.afterAll(async () => {
  try {
    for (const name of agentsMade) fix('rmagent', '1', name);
  } finally {
    for (const g of gates.values()) g.release();
    await server?.stop();
    await new Promise<void>((r) => (runtime ? runtime.close(() => r()) : r()));
  }
});

test.afterEach(async ({ page }, info) => {
  if (info.status === info.expectedStatus) return;
  console.log('chat log:', (await page.locator('#chat-log').innerHTML().catch(() => '(none)')).slice(-3000));
  console.log('server log (tail):', (server?.log() ?? '').split('\n').slice(-40).join('\n'));
});

// markdown.go mdAnchor's attributes, as the browser serialises them.
const A = (href: string) =>
  `<a href="${href}" target="_blank" rel="noopener" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">${href}</a>`;

async function send(page: Page, text: string) {
  const input = page.locator('#chat-input');
  await input.fill(text);
  await input.press('Enter');
}

const html = (page: Page, sel: string) => page.locator(sel).last().evaluate((e) => e.innerHTML);

for (const vp of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`on a ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('a sent message, its streamed reply, the stored pair and a reply from another page are all the server\'s markdown', async ({ page }) => {
      test.setTimeout(120_000);
      const key = vp.name + stamp;
      const name = `e2e-md-${vp.name}-${stamp}`;
      fix('agent', '1', name, `e2e-md-token-not-a-real-one-${vp.name}-${stamp}`);
      agentsMade.push(name);

      await signIn(page, origin);
      await page.goto(`${origin}/agents/${name}`);
      await expect(page.locator('#chat-form[data-cg-chat]'), 'the chat is wired').toHaveCount(1);
      await page.evaluate(() => ((window as any).__xss = undefined));

      // --- the owner's message: plain text at once, then the server's html.
      const sent = `please **look** at https://example.com/foo_ then x_ and café_x_.txt <script>window.__xss=1</script> MD-HOLD-${key}`;
      const sentHTML =
        `<p class="my-1">please <strong>look</strong> at ${A('https://example.com/foo_')} then x_ and café_x_.txt ` +
        `&lt;script&gt;window.__xss=1&lt;/script&gt; MD-HOLD-${key}</p>`;
      await send(page, sent);
      const mine = '#chat-log [data-bubble="user"][data-chat-local]';
      await expect.poll(() => html(page, mine), { message: 'the sent message, as its echo drew it' }).toBe(sentHTML);

      // --- the reply WHILE it streams (the runtime holds the turn open).
      await expect.poll(() => gate(key).entered, { timeout: 15_000 }).toBe(1);
      const replyHTML =
        `<p class="my-1">Here is <strong>bold</strong> and ${A('https://h.example/a*b*c')} and सूची_x_. and x²_y_. ` +
        `&lt;img src=x onerror=window.__xss=2&gt;</p>`;
      const live = '#chat-log [data-chat-turn][data-chat-local] > div';
      await expect.poll(() => html(page, live), { message: 'the streamed reply, rendered while it streams' }).toBe(replyHTML);
      await expect(page.locator('#chat-log [data-chat-turn] em'), 'no emphasis inside a word or a URL').toHaveCount(0);

      // --- the turn ends: the chat.reply refetch swaps in the stored bubbles.
      gate(key).release();
      await expect(page.locator('#chat-log [data-chat-local]'), 'the streamed bubbles were replaced by the stored ones').toHaveCount(0, { timeout: 20_000 });
      expect(await html(page, '#chat-log [data-bubble="user"][data-md-rendered]')).toBe(sentHTML);
      expect(await html(page, '#chat-log [data-bubble="assistant"][data-md-rendered]')).toBe(replyHTML);

      // --- a reply this page did not ask for: another page sends, and this one
      // hears of it only through its event stream.
      const other = await page.context().newPage();
      await other.goto(`${origin}/agents/${name}`);
      await expect(other.locator('#chat-form[data-cg-chat]')).toHaveCount(1);
      await send(other, `MD-SSE-${key} _only_ this`);
      const sseHTML = `<p class="my-1"><strong>sse</strong> reply_ok ${A('https://example.com/foo_')} then x_</p>`;
      await expect
        .poll(() => html(page, '#chat-log [data-bubble="assistant"][data-md-rendered]'), { message: 'the reply, drawn by the refetch its event asked for', timeout: 20_000 })
        .toBe(sseHTML);
      expect(await html(page, '#chat-log [data-bubble="user"][data-md-rendered]')).toBe(`<p class="my-1">MD-SSE-${key} <em>only</em> this</p>`);
      await other.close();
      expect(await page.evaluate(() => (window as any).__xss), 'no author script or handler ran').toBeUndefined();

      // A reload draws the same.
      await page.reload();
      expect(await html(page, '#chat-log [data-bubble="assistant"][data-md-rendered]')).toBe(sseHTML);
      await expect(page.locator('#chat-log [data-md]'), 'no bubble left for a client renderer').toHaveCount(0);
      await expect(page.locator('#chat-log script, #chat-log img'), 'no author tag became an element').toHaveCount(0);
    });

    // #538 round 1 F1 + F2: a message sent (Enter) while a reply streams, and
    // a code block opened while it streams.
    //
    // F1: the reply's text appears ONCE. The server's bubble does not end when
    // the owner sends — it reads the next message only after this turn — so
    // its next html is the whole reply so far; the page must keep drawing it
    // into the bubble it was drawing, not start a new one beside the old.
    // F2: a <details> the owner opened stays open across the next html.
    test('a message sent mid-stream does not duplicate the reply, and an opened code block stays open', async ({ page }) => {
      test.setTimeout(120_000);
      const key = 'two' + vp.name + stamp;
      const name = `e2e-md2-${vp.name}-${stamp}`;
      fix('agent', '1', name, `e2e-md2-token-not-a-real-one-${vp.name}-${stamp}`);
      agentsMade.push(name);
      await signIn(page, origin);
      await page.goto(`${origin}/agents/${name}`);
      await expect(page.locator('#chat-form[data-cg-chat]')).toHaveCount(1);

      await send(page, `first MD-TWO-${key}`);
      await expect.poll(() => gate('a-' + key).entered, { timeout: 15_000 }).toBe(1);
      const turn = page.locator('#chat-log [data-chat-turn][data-chat-local]').last();
      const code = turn.locator('details').first();
      await expect(code, 'part one drew its code block').toBeVisible();
      await code.locator('summary').click();
      await expect(code).toHaveJSProperty('open', true);

      // Enter while the reply streams (the send button is disabled; Enter is not).
      await send(page, 'second _msg_');
      gate('a-' + key).release();
      await expect(turn, 'part two arrived').toContainText('tail two');
      const log = page.locator('#chat-log');
      expect((await log.innerText()).split('MARK-ONE').length - 1, 'the reply\'s first part is drawn once').toBe(1);
      await expect(page.locator('#chat-log [data-chat-turn][data-chat-local] details[open]'), 'the opened code block is still open').toHaveCount(1);
      gate('b-' + key).release();
      await expect(page.locator('#chat-log [data-chat-local]')).toHaveCount(0, { timeout: 20_000 });
    });

    // pendingSent (agentChatScript): an echo fills the OLDEST bubble still
    // waiting on THIS socket. A message the server never read (dropped here,
    // in flight) leaves its bubble waiting; after a reconnect, the next
    // message's echo must land in the next message's bubble, not that one —
    // and a second message on the same socket must get its own echo too.
    test('after a reconnect, an echo fills its own bubble, not one the old socket left waiting', async ({ page }) => {
      test.setTimeout(120_000);
      const key = 'rc' + vp.name + stamp;
      const name = `e2e-md3-${vp.name}-${stamp}`;
      fix('agent', '1', name, `e2e-md3-token-not-a-real-one-${vp.name}-${stamp}`);
      agentsMade.push(name);
      let sockets = 0;
      let current: { close(): Promise<void> } | null = null;
      await page.routeWebSocket((u) => u.pathname === `/agents/${name}/ws`, (ws) => {
        sockets++;
        current = ws;
        const srv = ws.connectToServer();
        ws.onMessage((m) => {
          if (String(m).includes('MD-DROP')) return; // never reaches the server: no echo
          srv.send(m);
        });
      });
      await signIn(page, origin);
      await page.goto(`${origin}/agents/${name}`);
      await expect(page.locator('#chat-form[data-cg-chat]')).toHaveCount(1);
      await expect.poll(() => sockets).toBe(1);

      const mine = page.locator('#chat-log [data-bubble="user"][data-chat-local]');
      await send(page, 'MD-DROP **one**');
      await expect(mine).toHaveCount(1);
      await current!.close();
      await expect.poll(() => sockets, { message: 'the page reconnected', timeout: 30_000 }).toBe(2);
      await expect.poll(() => page.evaluate(() => (window as any).__cgChat?.ws?.readyState), { timeout: 15_000 }).toBe(1);

      await send(page, `after _x_ MD-HOLD-${key}`);
      await expect(mine).toHaveCount(2);
      await expect.poll(() => mine.nth(1).evaluate((e) => e.innerHTML), { message: "the new message's echo, in its own bubble" })
        .toBe(`<p class="my-1">after <em>x</em> MD-HOLD-${key}</p>`);
      expect(await mine.nth(0).evaluate((e) => e.innerHTML), 'the dropped message keeps its plain text').toBe('MD-DROP **one**');
      gate(key).release();
      await expect(page.locator('#chat-log [data-chat-local]')).toHaveCount(0, { timeout: 20_000 });

      const k2 = key + 'b';
      await send(page, `again _y_ MD-HOLD-${k2}`);
      await expect.poll(() => mine.last().evaluate((e) => e.innerHTML), { message: 'a second echo on one socket fills its own bubble' })
        .toBe(`<p class="my-1">again <em>y</em> MD-HOLD-${k2}</p>`);
      gate(k2).release();
    });
  });
}
