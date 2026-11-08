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
  });
}
