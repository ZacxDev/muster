# Previews

A **preview** serves a web server that is running on one of your mirrored
hosts (a Vite or Next dev server, a docs site, a Storybook) to you, inside
muster or in a new tab, through a tunnel. The server stays on the host's
loopback. Nothing about it is public, and nothing is exposed until you click
**Open** on it or the agent in that window asks for it.

This page describes what is built (phase 1: mirrored hosts, owner only). The
reasoning, the alternatives and the threat model are in the design,
[design/previews.md](design/previews.md); where the two differ, this page
and the code are right, and the design carries an "As built" note. Every
variable named here has its row in [configuration.md](configuration.md).

## The parts

| part | where | what it does |
|---|---|---|
| **the gateway**, `muster-preview` | its own process (the image ships `/muster-preview`; no deploy file runs it yet) | owns the preview zone: the session page on `w.<zone>`, each preview on `p-<id>.<zone>`, the hosts' tunnel endpoint on `tunnel.<zone>` and on an overlay-mesh listener. Holds the tunnels, the owner cookie and the reverse proxy. A [plugin](plugins.md) with an `mpl_` token |
| **the host side**, `muster-mirror -preview` | each mirrored host | keeps one tunnel to the gateway, reads `previews.json` (the allowlist), answers the tunnel's `CONNECT` streams by dialling loopback, reports this user's listeners in mirrored windows as **offers**, and tells muster how many offers and open previews each window has ([below](#what-muster-is-told)) |
| **the agent's tools**, `/preview-mcp` | the host's loopback MCP endpoint (`-mcp-listen`) | `preview_open`, `preview_list`, `preview_close`, answered by `muster-mirror` itself. Nothing on this path reaches muster ([attention.md](attention.md#the-preview-tools-preview-mcp)) |
| **muster's seams** | `muster-server` | the plugin [capabilities](plugins.md#capabilities) (`messages`, `viewer`), the one-time [viewer hand-off](plugins.md#viewer-hand-off) (mint and redeem), the session views' [Preview button](plugins.md#the-session-view-chip) with its panel and `frame-src`, and `MUSTER_PLUGIN_ORIGINS` with its `421` |

muster-server carries no preview byte and stores nothing about previews,
offers or the processes behind them. Its only previews state is the plugin
token (with its capabilities, origin and entry URL), the short-lived
hand-off codes, and, in memory with each connected host's window list, two
counts per window (below).

## What you do

1. **On a mirrored window's page** (`/mirror/view` or `/mirror/terminal`),
   while the window has a server to preview or a preview open, the header
   has a **Preview** button with that count, beside the Rendered | Terminal
   switch (on a phone, in the header's second row, under the title); the
   sidebar marks the window's row too. The button opens a panel
   (a side panel on a wide screen, the whole screen on a phone) that mints a
   hand-off code at the click and shows the gateway's **session page**,
   `https://w.<zone>/__muster/enter`, in its frame; the panel's **New tab**
   does the same in a new tab. With nothing to preview there is no button
   ([plugins.md](plugins.md#the-session-view-chip)). An agent's page
   (`/agents/{name}`) has none in this phase.
2. **The session page** lists, for that window only: its **offers**
   (`<process> · :<port>`, a server this user runs in one of the window's
   panes on an allowlisted port) and its **open previews** (`<label> · :<port>`).
   Each has **Open** (in this frame or tab) and **New tab**; an open preview
   also has **Close**. The list refreshes every 5 s while the page is visible.
   A host on the public path shows **slow path (via Cloudflare)**; a host with
   no tunnel says it is offline.
3. **Open** on an offer creates the preview: the gateway checks the open
   limits, probes the port with one `CONNECT` through the tunnel, records the
   preview and tells the host. **Open** on a preview mints a one-time entry
   code and sends the frame or tab to `https://p-<id>.<zone>/__muster/enter`,
   which sets the owner cookie and continues to the app (to the agent's
   `path`, if it gave one).
4. **Close** ends the preview at once: its requests, streams and WebSockets
   are cut, its address answers 404, and the agent's `preview_list` drops it.
   The port is offered again if its server still runs.

A reload of the session page, or a page token past its 15 minutes, says
"Open this from muster": open the Preview panel again. An agent's `preview_open`
appears on the page at its next refresh; no URL is ever given to the agent.

## Setting it up

The operator's steps, in order. The variables are in
[configuration.md](configuration.md#muster-preview-the-preview-gateway) (the
gateway), [configuration.md](configuration.md#muster-mirror-host-agent) (the
host) and [configuration.md](configuration.md#muster-server-optional-features)
(the server).

1. **muster-server:** `MUSTER_PLUGINS=1` and `MUSTER_PLUGIN_ORIGINS=https://*.<zone>`.
   A request whose `Host` is under a pattern is then refused `421` on every
   muster listener. At boot muster refuses a pattern that covers
   `MUSTER_PUBLIC_URL`'s host; it checks no other name muster is reached at.
2. **A viewer plugin token:** on `/plugins`, mint a token with `viewer` (and
   not `messages`), origin `https://*.<zone>`, entry URL
   `https://w.<zone>/__muster/enter`. Put it in the gateway's
   `PLUGIN_TOKENS_FILE`.
3. **The gateway:** `MUSTER_URL`, `MUSTER_PUBLIC_URL`, `PLUGIN_TOKENS_FILE`,
   `PREVIEW_DOMAIN`, `STATE_DIR`, `PREVIEW_SIGNING_KEY` (at least 32 bytes),
   `PREVIEW_LISTEN`, and `PREVIEW_MESH_LISTEN` for the mesh endpoint. The
   edge must send `*.<zone>` to the public listener with `Host` intact,
   WebSocket upgrades passed and responses unbuffered. 🔴 It must also
   **overwrite** `CF-Connecting-IP` (and the mesh relay `X-Real-IP`) with the
   real client address: the failed-token throttle counts per that header, so
   one a client could set itself would let a token guesser dodge the `429`
   (the token's 256 bits remain the real defence).
4. **A host token:** `muster-preview host-token create [<owner>] <host>`,
   where `STATE_DIR` is mounted. `<host>` must be the name the host's mirror
   token was minted for. The token is printed once. `<owner>` is a login as
   muster's `GET /plugin/v1/self` names it for a token in
   `PLUGIN_TOKENS_FILE`; the token is keyed on that owner's id, a login two
   owners answer to (or none) is refused, and with exactly one owner it may be
   left out. `host-token ls` lists the tokens and `host-token revoke <id>`
   revokes one; a running gateway re-reads the file on every tunnel and every
   30 s.
5. **The host:** save the token to a file only this user can read
   (`chmod 600`), write `previews.json`, and run `muster-mirror` with
   `-preview`, `-preview-urls ws://<mesh address>:<port>/v1/host,wss://tunnel.<zone>/v1/host`,
   `-preview-token-file` and `-mcp-listen` (without `-mcp-listen` there are
   no agent tools, but offers still work).
6. **The agent's session:** add the `preview` MCP server entry from
   [attention.md](attention.md#the-preview-tools-preview-mcp). A session that
   connected before this sees the tools only after it reconnects. The
   server's `initialize` carries instructions telling the agent when and how
   to use `preview_open`. Claude Code and opencode both show a server's
   instructions to the model, so neither needs more.
   **opencode** takes a different entry, in opencode's config under `mcp`:
   `"type": "remote"`, the same URL, `"oauth": false`, and two headers, the
   secret (`"Authorization": "Bearer {file:~/.config/muster-mirror/mcp-secret}"`)
   and the static `"X-Muster-Client": "opencode"`. It sends no pane headers.
   Each call is bound to the window showing the opencode session the call
   names, and fails with `no_window` when no window shows it
   ([attention.md](attention.md#opencode-sessions)). Restart opencode's
   service after adding it: it reads its MCP config once.

```json
{
  "ports": ["3000", "5173", "6006", "8000-8099"],
  "offer": true,
  "maxOpen": 8,
  "maxConns": 64
}
```

`previews.json`: `ports` are single ports or inclusive ranges within
1024–65535; `offer` (default `true`) turns the listener reports on;
`maxOpen` is 1–32 (default 8); `maxConns` is 1–64 (default 64). Keys must be
spelt exactly. A missing, empty or invalid file turns previews off with a
logged reason and is never fatal. 🔴 **At startup that is final:** a file (or
token file, or `-preview-urls`) that does not validate when `muster-mirror`
starts leaves previews off until `muster-mirror` restarts; the file is not
re-read, no tunnel is dialled, and `/preview-mcp` is not mounted, so the
agent has no preview tools at all (`cmd/muster-mirror/preview.go`). Once
previews have started, the file is re-read every 5 s, and a file that turns
invalid then turns previews off (`previews_off`) until it is fixed
(`internal/previewhost/config.go`, `host.go`). `-preview` runs on the default
tmux server's daemon only.

## How it is built

### The tunnel

- The host dials `-preview-urls` in order (at most 4, `ws://` or `wss://`)
  with `Authorization: Bearer mpt_…`, `X-Muster-Preview-Host` and
  `X-Muster-Preview-Version: 1`. Each endpoint gets **3 s** for the TCP
  connect, the WebSocket upgrade and the version answer; the first that
  answers is taken. All failing, the list is tried again after a jittered
  backoff from 1 s to 60 s, reset once a tunnel has stayed up a minute. On a
  later endpoint the host re-tries the earlier ones every **5 min**
  (`internal/previewhost/dial.go`). A `ws://` dial never uses an HTTP proxy
  (the token would cross it in the clear); a `wss://` one honours the proxy
  environment. Redirects are never followed.
- **The path is the listener.** A tunnel that arrived on `PREVIEW_MESH_LISTEN`
  is `mesh`, one on `PREVIEW_LISTEN` (`tunnel.<zone>/v1/host`) is `public`;
  nothing in the request changes it (`internal/previewgw/gateway.go`).
- **Roles reverse:** the host is the HTTP/2 server on the WebSocket and the
  gateway the client. The gateway's first stream is `GET /v1/offer`, a
  long-lived stream of newline-delimited JSON from the host: its config, its
  window listing and its listener reports (each sent only when it changed),
  and the agent's `open`/`close` requests. The gateway answers with
  `POST /v1/opened` and `POST /v1/closed`, within 5 s, through one worker per
  tunnel with a queue of 64; a host that overflows it has its tunnel closed
  (`internal/previewgw/offers.go`).
- **Data:** each upstream TCP connection is one HTTP/2 `CONNECT` stream. The
  host takes **only the port** from the authority, checks it, and dials
  `127.0.0.1`, then `[::1]` (`internal/previewhost/connect.go`).
- **One tunnel per (owner, host).** A newer one takes new streams at once; the
  older closes when its last stream ends, or 60 s after it was replaced
  (`internal/previewgw/tunnels.go`).
- **Pings** every 30 s at both ends; a ping unanswered within the interval
  ends that end of the tunnel.
- 🔴 **The flow-control invariant** (`internal/preview/tunnel/tunnel.go`,
  pinned by `TestFlowControlInvariantIsPinned`): `x/net/http2` returns
  connection credit only when a stream's body is read, so a paused reader
  keeps its stream window out of the connection window. The connection window
  must exceed every stream's window at once, or paused readers stall every
  other stream on the tunnel. As built: 4 MiB per stream and
  (64 + 4 + 1) × 4 MiB = **276 MiB** per connection towards the browser;
  1 MiB and (64 + 4 + 1) × 1 MiB = **69 MiB** for uploads. The 4 are the
  control streams the host's `MaxConcurrentStreams` (`maxConns + 4`) reserves
  for the offer stream and the answers. The worst case is that much gateway
  memory per host, held only while readers are paused.

### What the host checks

Every `CONNECT` and every new `preview_open` goes through the same check
(`Host.refusal`, `internal/previewhost/connect.go`):

1. previews on, and the port allowed. The host's own `-mcp-listen` port and
   the opencode server's port are refused first, even when listed
   (`port_not_allowed`);
2. on Linux, the listener: a loopback or wildcard listener on the port owned
   by another uid is `foreign_listener`, none is `not_listening`, and an
   unreadable `/proc` row fails closed as `foreign_listener`. Off Linux this
   check and the listener reports are **off**, and only the host's log says
   so (`internal/previewhost/host_other.go`);
3. then the loopback dial; a port that left the allowlist meanwhile is
   refused before it is spliced.

**Offers** are reported only for a listener that is allowlisted, bound to
loopback or the wildcard, owned by this uid, and run by a process that is a
pane's root or a descendant of one. A report carries exactly `port`,
`process` (its comm, at most 15 bytes), `window` and `first_seen`
(`internal/preview/wire/offer.go`). The host lists the tmux windows itself
every 5 s, so offers work whether or not muster is connected.
`muster-mirror -list -previews` prints what this host would offer and dials
nothing.

### What muster is told

On its own window-list connection to muster (not through the gateway), the
host sends a `previews` message: per tmux window id, `offered` (its offers
with no open preview, exactly the offers the session page lists) and `open`
(its open previews), counts only, after its window list and again whenever
they change, on the mirror's 5 s refresh. A window with nothing is left out;
an empty map clears them. The counts are reported **only while a gateway
tunnel serves the offer stream** and previews are on: with no tunnel the
session page could only say the host is offline. muster keeps them in memory
with the host's window list, drops them when the host disconnects, and draws
the Preview button and the sidebar mark from them
(`internal/previewhost/host.go`'s `Summary`, `internal/mirror/previews.go`). An
older `muster-mirror` never sends one, so its windows show no button; an
older muster drops the message ([mirroring.md](mirroring.md#rolling-upgrades)).

### The proxy

`httputil.ReverseProxy` with every write flushed, HTTP/1.1 over a `CONNECT`
stream on the host's current tunnel; bodies stream both ways, never buffered
whole (`internal/previewgw/proxy.go`).

- **To the app:** `Host: localhost:<port>`, `X-Forwarded-Host: p-<id>.<zone>`,
  `X-Forwarded-Proto: https`. Removed: the gateway's cookies (`__Host-mp`,
  `__Host-mps`) and every `muster_*` cookie, `Forwarded`, every inbound
  `X-Forwarded-*`, `X-Real-IP`, `CF-*`, `True-Client-IP`, `CDN-Loop`,
  `X-Muster-*`. **The viewer's address is not forwarded.** The app's own
  cookies and `Authorization` pass, and so does the browser's `Origin`
  (**only on a WebSocket upgrade** does an `Origin` equal to the preview's own
  become `http://localhost:<port>`).
- **To the browser:** `Set-Cookie` loses `Domain`, and a cookie named like a
  gateway cookie is dropped; a `Location` at `localhost`, `127.0.0.1` or
  `[::1]` on the preview's port is rewritten to the preview's origin;
  `Referrer-Policy: no-referrer` replaces the app's; the app's
  `Strict-Transport-Security`, every `X-Muster-*` header and `Clear-Site-Data`
  are dropped; `X-Content-Type-Options: nosniff` is set; and a second
  `Content-Security-Policy: frame-ancestors <MUSTER_PUBLIC_URL's origin>` is
  added, which the browser enforces beside the app's own.
- **App trailers:** a trailer named `X-Muster-*` (any case), or like any
  header the rules above drop or rewrite (`Clear-Site-Data`,
  `Strict-Transport-Security`, `Referrer-Policy`, `X-Content-Type-Options`,
  `Content-Security-Policy`, `Location`, `Set-Cookie`), is removed from the
  announced `Trailer:` line, and its value, declared or sent undeclared with
  net/http's `TrailerPrefix`, is stripped when the body ends. Other trailers
  pass (`dropAppNonFinal`, `internal/previewgw/proxy.go`).
- **App 1xx responses** (such as `103 Early Hints`) are forwarded, minus the
  same names as trailers: `X-Muster-*`, `Clear-Site-Data`,
  `Strict-Transport-Security`, `Referrer-Policy`, `X-Content-Type-Options`,
  `Content-Security-Policy`, `Location` and `Set-Cookie` (dropped whole, not
  stripped of its `Domain`) never reach the browser on one; a `Link` and other
  fields pass. The gateway's own `Referrer-Policy: no-referrer`,
  `X-Content-Type-Options: nosniff` and `frame-ancestors` CSP are then added
  to it, as on a final response. `httputil.ReverseProxy` forwards a 1xx from its own
  `httptrace` hook, outside `ModifyResponse`, so the gateway's transport adds
  a newer hook that filters the 1xx header in place before ReverseProxy's
  copies it (`filter1xx`). Nothing of a 1xx carries into the final
  response's headers.
- **WebSockets** are relayed message by message (coder/websocket on both
  legs, no read limit, compression off), the app's leg dialled first so an
  app's refusal reaches the browser as the app sent it. The browser leg is
  pinged every 30 s; a ping that fails or goes unanswered closes it. Any other
  `Upgrade` is `400 bad_upgrade` (`internal/previewgw/wsrelay.go`).
- **Server-sent events:** a `text/event-stream` body quiet for 30 s gets an
  SSE comment (`:\n\n`) only at an event boundary, and never in a body with a
  `Content-Encoding` other than identity (`internal/previewgw/sse.go`).
- The upstream pool is per tunnel: 16 idle connections, 4 per preview, a 30 s
  idle timeout and a **60 s** wait for the app's response headers. Idle pooled
  connections are closed when a dial finds every host slot taken and when a
  preview of the host closes, so they cannot hold slots another preview needs.
- An app's own error responses pass through untouched. The gateway's own are
  a fixed page naming a code ([below](#error-codes)).

## Security model

The full threat model is the design's
([threat model](design/previews.md#threat-model)); this is what the code
enforces.

- **Another site.** The zone is a different registrable domain from muster's,
  so preview content cannot read or ride muster's cookies, and a `Host` under
  `MUSTER_PLUGIN_ORIGINS` never reaches a muster handler (`421`).
- **Names are one label** under the zone: `p-<id>` with a 26-character
  lowercase base32 id (128 random bits), `w`, `tunnel`. Any other name is
  404, and so is the name of a preview that is not open, before anything is
  dialled. Before **Open**, an offer has no preview and its name serves
  nothing.
- **The session page holds no cookie.** Its credential is a page token,
  HMAC-SHA256-signed with `PREVIEW_SIGNING_KEY` (stateless, with its own MAC
  kind so it is never accepted as a cookie), 15 minutes, bound to owner, host
  and window. The script reads it from a boot element, removes the element,
  and sends the token as `Authorization: Bearer`. No `w.` response sets a
  cookie (`internal/previewgw/session.go`, `cookie.go`).
- **The owner cookie** `__Host-mp`: `Secure`, `HttpOnly`, `Path=/`,
  host-only, HMAC-signed `{preview, owner, expires}`, `Max-Age` **8 hours**,
  **never renewed** except by a fresh hand-off from the Preview panel. Its `SameSite`
  follows `Sec-Fetch-Dest` on the entry request: `iframe` gets
  `SameSite=None; Partitioned`, anything else (a new tab, or a browser
  without Fetch Metadata) `SameSite=Strict`. It admits only the preview and
  owner it names. It is minted only for a 60-second, single-use entry code
  (spent whatever the outcome; at most 64 unspent per owner and 4096 in all).
- **Fetch Metadata and Origin** on every `p-` request but the entry route
  (`internal/previewgw/samesite.go`): `Sec-Fetch-Site: same-origin` and
  `none` pass; otherwise only a `GET`/`HEAD` with `Sec-Fetch-Mode: navigate`
  and `Sec-Fetch-Dest: document` passes, or `Sec-Fetch-Dest: iframe` **on
  `/__muster/check` only** (the gateway answers that page itself, and its
  script navigates on to the app, so the app's request is same-origin).
  Everything else is `403 cross_site`. A WebSocket upgrade must carry its own
  preview's `Origin`, with or without Fetch Metadata; without
  `Sec-Fetch-Site`, so must every method but `GET` and `HEAD`.
- **Cookie bombing:** a `Cookie` header over 8 KiB gets a `431` page that
  expires every cookie it names (up to 128), at every path prefix of the
  request (up to 8), host-only and at `Domain=<zone>`, each plain and
  `Partitioned` (at most 512 `Set-Cookie` headers).
- **No `Referer`:** every gateway page and every proxied response carries
  `Referrer-Policy: no-referrer`. Gateway pages are framed only by
  `MUSTER_PUBLIC_URL`'s origin and use a nonce-only script and style CSP.
- **The panel's frame** (muster's side) is sandboxed without
  `allow-top-navigation`, with `allow=""` and `referrerpolicy="no-referrer"`;
  the session views' `frame-src` names only the origins of the owner's own
  live viewer plugins ([plugins.md](plugins.md#the-session-view-chip)).
- **The host bounds what is exposed** whatever the gateway asks: allowlisted
  ports, loopback only, this user's listener (Linux). The gateway believes the
  window a host names for an agent's open; muster does not check it.
- **The agent never sees gateway text.** A gateway refusal reaches the model
  only as a well-formed code (`^[a-z_]{1,32}$`, else `gateway_error`) with
  the host's own fixed message.

🔴 **A sign-out does not reach an open preview.** There is no liveness check:
signing out of muster, Sign out everywhere or a password rotation leave an
owner cookie working until it expires, up to 8 hours. **Close** is the
immediate cut-off. A revoked plugin token or a disabled owner is caught by
the gateway's minute-by-minute check ([lifecycle](#lifecycle)).

## Limits

| scope | limit | enforced by | past it |
|---|---|---|---|
| per window (calling session) | 4 open previews | gateway | `max_open` |
| per owner | 16 open previews | gateway | `max_open` |
| per host | `maxOpen` open previews (1–32, default 8; the gateway uses 8 until the host sends its config) | host and gateway | `max_open` |
| per host | `maxConns` upstream streams (1–64, default 64) | gateway, host as backstop | a new one waits up to **10 s** for a slot, then `429 max_conns` |
| per preview | 64 requests and WebSockets in flight | gateway | one more waits up to the dial wait (10 s) for a slot, then `429 too_many_connections`, before anything is dialled. An admitted request can then wait as long again for a host slot, so the worst case before a 429 is about twice the dial wait |
| request body | `PREVIEW_MAX_BODY`, default 32 MiB (1 KiB to 1 GiB) | gateway | `413 body_too_large`: at once for a declared length, as the body streams for a chunked one. WebSocket messages are not bodies |
| response | none (flow-controlled) | — | — |
| app response headers | 60 s | gateway | `504 upstream_timeout` |
| HTTP connection | 10 s for request headers, 5 min idle | gateway | closed |
| entry codes | 64 unspent per owner, 4096 in all | gateway | `429 too_many_opens` |
| hand-off codes | 60 a minute per owner | muster | `429` |
| failed host-token checks | 10 a minute per peer: `X-Real-IP` on the mesh listener, `CF-Connecting-IP` on the public one, else the TCP peer (the edge must overwrite that header) | gateway | `429` instead of `401` (a valid token is never refused) |

A repeat open of an existing (owner, host, window, port) returns the same id
and counts nothing.

## Lifecycle

Previews live in the gateway's memory only (there is no database); host
tokens are a file in `STATE_DIR`.

| event | what happens |
|---|---|
| `preview_open`, or **Open** on an offer | checked against the limits (and, for an offer, probed); recorded; the host is told |
| `preview_close`, or **Close** on the session page | closed at once: every request, stream and WebSocket of it is cut (WebSockets with close code 1008, reason `preview_closed`); its name answers 404 |
| the owner cookie expires (8 h) | the requests and WebSockets it admitted are cut at that moment (1008 `session_expired`); the next request gets `403 session_expired` |
| no request for `PREVIEW_IDLE` (default 2 h; an open WebSocket or stream counts as in use) | closed at the next 30 s sweep; the host is told |
| open longer than `PREVIEW_TTL` (default 24 h) | closed at the next 30 s sweep, in use or not; reopening mints a new id |
| its window absent from the host's window listing for 10 min | closed at the next sweep (so 10 to 10.5 min after); the clock starts when the listing arrives, a window absent at open counts from the open, and an offline host neither starts nor clears it |
| the host's tunnel drops | requests get `503 host_offline`; the preview stays open, and the page says the host is offline |
| a port leaves the host's `previews.json` | the host closes that port's previews itself (and tells the gateway), cuts their live connections and refuses new `CONNECT`s `port_not_allowed` |
| `previews.json` turns invalid while previews run | previews are off on the host: every preview closes, the tools answer `previews_off`, until the file is fixed. (Invalid at startup: previews never start and there are no tools until `muster-mirror` restarts.) |
| the host token is revoked | at the next 30 s recheck the tunnel ends and that host's previews close, even if it is offline then; a new token for the same host name starts with none |
| the owner's plugin tokens all answer `401`, or `200` without `viewer` | at the next minute's `GET /plugin/v1/self` the owner's tunnels and previews close and new tunnels are refused; while any one of the owner's tokens is live the owner is still served. An owner is not served at all before muster has answered for one of its tokens |
| muster gives no verdict (unreachable, `5xx`, `404`) | a token keeps its last verdict for 9 checks; at the 10th in a row (about ten minutes) it stops counting as live |
| the owner signs out of muster | nothing at once: the owner cookie works until it expires |
| the gateway restarts | every preview is forgotten; each host re-announces its acknowledged opens (not ones older than 24 h) and gets **new** ids, so old preview addresses and cookies stop working |
| the app stops answering | `502 not_answering`; the preview stays open, and `preview_open` of that port still returns its id |

## Error codes

### The agent's tools (`/preview-mcp`)

A refusal is a tool result with `isError: true` whose text is
`{"error": "<code>", "message": "…"}` (`internal/previewhost/mcp.go`). An
unknown tool or malformed `tools/call` params is JSON-RPC `-32602`, an
unknown method `-32601`, a request that is not JSON-RPC 2.0 `-32600`, a body
over 64 KiB HTTP 413.

| code | tools | meaning |
|---|---|---|
| `previews_off` | all | `previews.json` became missing or invalid after previews started |
| `invalid_arguments` | all | the arguments do not validate (port 1–65535; label ≤ 60 bytes, no control characters; path ≤ 512 bytes, one leading `/`, printable ASCII without spaces or backslashes; no unknown fields; `id` required) |
| `no_window` | all | the caller's pane is in no window this host mirrors |
| `port_not_allowed` | open | off the allowlist, or an always-refused port |
| `not_listening` | open | nothing accepted a connection on the port |
| `not_answering` | open | it accepted the connection but gave no HTTP answer to `HEAD /` within 5 s; retry once it is up |
| `foreign_listener` | open | another user listens on the port, or the listeners could not be read |
| `gateway_not_connected` | open, close | no tunnel to the gateway now |
| `gateway_timeout` | open, close | the gateway did not answer within 5 s (also: 64 requests already pending) |
| `max_open` | open | a limit is reached (window, owner or host) |
| `not_found` | close | no such preview, or another window's |
| `cancelled` | open, close | the caller's own request ended first |
| `gateway_error` | open, close | the gateway refused with something that is not a well-formed code |

A well-formed code the gateway answers is passed on as is. Five have a
fixed message of the host's own (`max_open`, `port_not_allowed`,
`previews_off`, `not_found`, `bad_answer`); every other gets "the preview
gateway refused", among them `host_offline` (the tunnel was replaced or the
owner is not served), `bad_port`, `bad_path`, `bad_window`, `bad_session`,
`bad_request` and `internal` (`internal/previewgw/offers.go`, `lifecycle.go`).
Either way no text the gateway wrote reaches the model
(`internal/previewhost/mcp.go`).

### The gateway's pages on `p-<id>.<zone>`

Each is a fixed page plus `X-Muster-Preview-Error: <code>`
(`internal/previewgw/codes.go`). A preview that is not open is a plain 404
with no code.

| status | code | when |
|---|---|---|
| 503 | `host_offline` | the host has no tunnel, or it ended mid-request |
| 502 | `not_answering` | nothing answered on the port (also any unexplained upstream failure) |
| 504 | `upstream_timeout` | no response headers within 60 s |
| 429 | `max_conns` | no host slot freed within the 10 s wait |
| 403 | `port_not_allowed`, `foreign_listener` | the host refused the `CONNECT` |
| 404 | `preview_closed` | the preview closed while the request waited or ran |
| 403 | `session_expired` | no valid owner cookie for this preview, or it expired mid-request |
| 403 | `cross_site` | the Fetch Metadata or `Origin` rule refused it |
| 431 | `too_many_cookies` | the cookie-bomb reset page |
| 400 | `bad_upgrade` | an `Upgrade` that is not a WebSocket |
| 403 | `entry_used` | an entry code that is unknown, spent, expired or another preview's |
| 403 | `frame_blocked` | `/__muster/check` received no cookie: the browser blocks embedded previews (use **New tab**) |
| 429 | `too_many_connections` | 64 in flight and none freed within the wait |
| 413 | `body_too_large` | past `PREVIEW_MAX_BODY` |

### The session page's API on `w.<zone>`

`/__muster/list`, `/__muster/open` and `/__muster/close` answer an error as
a bare code in a `text/plain` body (`internal/previewgw/session.go`):

| status | code |
|---|---|
| 401 | `page_expired` (missing, bad or expired page token, or the owner no longer served) |
| 400 | `bad_request` |
| 404 | `no_such_preview`, `no_such_offer` |
| 503 | `host_offline` |
| 409 | `max_open` |
| 502 | `not_listening`, `probe_failed` |
| 403 | `port_not_allowed`, `foreign_listener` |
| 429 | `max_conns`, `too_many_opens` |
| 500 | anything else |

`/__muster/enter` without a valid hand-off renders "Open this from muster"
(`400` for a missing or malformed code, `403` for one muster refused or a
token not live), and `502` "muster did not answer" when the redeem call
fails.

### The tunnel

| side | status | meaning |
|---|---|---|
| gateway, host dial | `426` + `X-Muster-Preview-Version-Range: 1-1` | an unknown protocol version |
| gateway, host dial | `401`, then `429` | a bad host token; the 11th failure a minute from one peer (keyed on `X-Real-IP` on the mesh listener, `CF-Connecting-IP` on the public one, else the TCP peer; `internal/previewgw/throttle.go`) |
| gateway, host dial | `403` | the token registers another host name, or its owner is not served |
| gateway, host dial | `503` | the host-token file cannot be read |
| host, `CONNECT` | `403 port_not_allowed`, `403 foreign_listener`, `502 refused`, `429 max_conns` | in `X-Muster-Preview-Reason` (`internal/preview/wire/wire.go`); `max_conns` from the host is a backstop the gateway retries within its wait |
| host, `POST /v1/opened` | `409` with `previews_off`, `bad_answer`, `port_not_allowed`, `max_open`, `late` or `unknown_request` | an open the host will not keep; it also asks the gateway to close it |

### muster's seams

| route | answers |
|---|---|
| any request with a `Host` under `MUSTER_PLUGIN_ORIGINS` | `421` |
| `POST /plugin-handoffs` | `404` (not your plugin or window), `403` (its origin left `MUSTER_PLUGIN_ORIGINS`), `429` (60 a minute) |
| `POST /plugin/v1/handoffs/redeem` | `404` for an unknown, used, expired or another plugin's code; `403` without `viewer` |
| both, while plugins are off | the typed `feature_disabled` 404, feature `plugins` |
| both, while plugins are on and `MUSTER_PLUGIN_ORIGINS` is unset | the typed `feature_disabled` 404, feature `plugin-viewers` |

## Telemetry

The gateway's metrics are on its own registry, served at `/metrics` on the
mesh listener only (with `/healthz`); every label is a closed vocabulary
(`internal/previewgw/metrics.go`):

- `muster_preview_tunnels{path}` (`mesh`, `public`; draining tunnels
  included) and `muster_previews_open{kind}`;
- `muster_preview_opens_total{opener}` (`agent`, `page`) and
  `muster_preview_closes_total{reason}` (`host`, `page`, `idle`, `ttl`,
  `window_gone`, `host_revoked`, `owner_not_served`);
- `muster_preview_refusals_total{reason}`: every page the gateway answers
  itself on a `p-` host, the plain `404` for a preview that is not open
  (`no_such_preview`), a non-`GET` entry (`method_not_allowed`), and refused
  opens; a code outside the known set counts as `other`;
- `muster_preview_requests_total{kind,auth,code_class}` for admitted requests;
- `muster_preview_response_seconds{kind}`: time to the app's response
  headers, measured from when the request took its per-preview slot (a wait
  for one is not counted);
- `muster_preview_dial_seconds{kind,outcome}`.

Spans (exported only when an OTLP endpoint is set; service
`muster-preview`): `preview.request` for an admitted request (kind, auth, the
tunnel's path, status class), `preview.dial` as its child, and `preview.ws`
(why the relay ended: `client_left`, `app_ended`, or the gateway's code). A
`preview.request`'s reason is only a code the gateway itself rendered, never
read from response headers, so an app cannot mark its own request as a
gateway error. No span or label carries a path, query, header or body.

muster exports `muster_plugin_handoffs_total{purpose,outcome}` and
`muster_plugin_origin_refusals_total`.

Not built: `muster_preview_bytes_total`, `muster_preview_offers{state}`, and
the host's `host.preview.dial` and `host.preview.tool` spans. The host logs
one line per refused stream.

## Browser behaviour, measured

From the end-to-end spec `e2e/tests/previews.spec.ts` (#573): the real
muster-server, gateway and `muster-mirror -preview` behind a TLS test edge
that passes every header through unchanged. Measured locally in Chromium 153
(Playwright 1.63); CI runs the same assertions on Playwright's bundled
Chromium.

| request | `Sec-Fetch-Site` / `-Mode` / `-Dest` | cookie sent | gateway |
|---|---|---|---|
| **The panel**: `p-/__muster/enter` from `w.` in muster's frame | `same-site` / `navigate` / `iframe` | none | 303 |
| **The panel**: `p-/__muster/check` (the redirect) | `same-site` / `navigate` / `iframe` | `__Host-mp` | 200 |
| **The panel**: the app's `/` (check's `location.replace`) | `same-origin` / `navigate` / `iframe` | `__Host-mp` | 200 |
| the app's WebSocket | **none sent** | `__Host-mp`, its own `Origin` | 101 |
| the app's `EventSource` | `same-origin` / `cors` / `empty` | `__Host-mp` | 200 |
| **New tab**: `p-/__muster/enter`, then the app | `same-site` / `navigate` / `document`, then `same-origin` | none, then `__Host-mp` | 303, then 200 |
| a sibling preview's hidden iframe to the victim's `/secret` | `same-site` / `navigate` / `iframe` | **the victim's** `__Host-mp` | **403 `cross_site`** |
| a sibling preview's WebSocket to the victim | **none sent** | **the victim's** `__Host-mp`, the sibling's `Origin` | **403 `cross_site`** |

- **The partitioned cookie works in the frame:** Chromium stored `__Host-mp`
  as `SameSite=None`, `Secure`, `HttpOnly`, partitioned under muster's
  top-level site, and sent it on every app request. The new tab's cookie is
  `SameSite=Strict`, not partitioned.
- **Chromium sends no `Sec-Fetch-*` on a WebSocket handshake**, so a
  WebSocket is defended by the `Origin` rule alone.
- **Partitioning does not separate two previews opened in the same frame:**
  their cookies share the partition, and Chromium attached the victim's
  cookie to the sibling's hidden-iframe request. The
  iframe-only-on-`/__muster/check` rule is what refuses it.

## What is not done, or not verified

- **Phase 2 (pods)** and **phase 3 (share links)** are not built: no `ccd`
  gate, no `agent_targets` capability, no `/plugin-handoffs/confirm`, no
  `__Host-mps` (the name is only reserved). The Preview button is on
  mirrored windows only.
- **Off Linux** (macOS) the listener reports and the `foreign_listener` check
  are off, and only the host's log says so: the offer stream does not.
- **The real edge in front of the gateway is untested** with the built
  gateway (wildcard certificate, pass-through of streaming and WebSockets,
  the ~125 s idle cut against the 30 s pings). The spike measured the
  transport through it, not this code.
- **Off-site mesh latency is unmeasured.** The spike's mesh numbers are a
  same-LAN best case ([the spike's go/no-go](design/previews.md#the-spikes-gono-go)).
- **Browsers other than Chromium**, Safari and the Android app's WebView,
  are unchecked; in a browser that refuses partitioned cookies in a frame,
  the Preview panel lands on `frame_blocked` and **New tab** works.
- **No deployment** runs the gateway yet; there is no compose service or
  manifest for it.
- A tunnel's ping timeout equals its 30 s interval, so a pong queued behind a
  large download on a slow uplink could end a healthy tunnel. Unmeasured.
