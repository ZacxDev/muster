# Telemetry: tracing an open

Opening an agent session (a card on **Agents**, or an entry of the session
switcher) is several requests on three machines: the browser, `muster-server` and
the host's `muster-mirror`. When one is slow, or the view says it disconnected,
this page is how to find which hop did it, from one trace, one log line or a
metric.

Everything here is **off unless configured**. With no OTLP endpoint set, neither
binary installs a tracer, wraps a handler, traces a query or stamps a frame
([configuration.md](configuration.md#tracing-muster-server-and-muster-mirror)).
Two things are always on, because they cost a clock read and answer the
question without any collector: the per-open log line and the Prometheus
histograms below, plus the server's phase timings in the document's
`Server-Timing` header.

## The open, hop by hop

### A rendered view (`/mirror/view`)

| # | where | what happens | measured as |
|---|---|---|---|
| 1 | browser | The tap navigates to `/mirror/view?host=…&session=…&window=…` (a full document load; a switcher entry or a Rendered/Terminal swap may load it in a frame). | the page's `open mirror_view` span starts at the navigation (`performance.timeOrigin`) |
| 2 | server | `GET /mirror/view`: the session, then `mirrorEntry` (the in-memory hub's host list), `recordMirrorVisit` (a database write), the composer's send queue (database), suggested replies, the host icon (database: mirror tokens), the window's attention asks (database), the navigation features, the session switcher (database and hub), then rendering. | span `GET /mirror/view` with one `open.<phase>` child per step and a `db …` span per query; log line `open view=mirror_view part=document`; `Server-Timing` entries per phase; `muster_open_phase_seconds{phase="document"}` |
| 3 | browser | Parse. The view script runs as it is parsed (the snapshot store, `mirror-snap.js`, is parser-blocking on this page and runs just before it): it begins the open and reads this window's snapshot from IndexedDB, waiting at most `readWaitMs`. | `snapshot_read_start`, `snapshot_read_end`; the `snapshot` word (`hit`, `miss`, `timeout`, or `off` without a store) |
| 4 | browser → server | As soon as the read is done — before the document has finished loading — the transcript socket `GET /mirror/view/ws` is created (resuming at the snapshot's `gen`/`offset` when there is one), carrying a trace context as `?tp=`. At DOMContentLoaded the snapshot is painted (one layout for all its batches), and the socket's events, queued until then, are applied in order. | `socket_created`, `dom_content_loaded`, `snapshot_paint_start`, `snapshot_painted` (with the `snapshot_batches` count), `stream_connected`, `load`; span `GET /mirror/view/ws` (in the page's trace; it ends when the open does, at the first batch) |
| 5 | server | The socket handler: `mirrorEntry` again, the WebSocket upgrade, the attention-ask subscription and the suggested-replies tracker, then the hub's watch. | `open.entry`, `open.accept`, `open.subscribe`, `open.watch` |
| 6 | server → host | The hub writes a `watch` frame on the host's one tunnel WebSocket (shared by every view and terminal on that host). | span `mirror.watch` (to the first batch, or why it never came) and its child `mirror.watch.first_batch` (frame written → first batch read), both carrying what that batch cost: `mirror.first_batch_bytes` (its JSON as read), `mirror.first_batch_events` and `mirror.host_rtt_ms` (the host's last keepalive round trip — pinged once the host's first message after the welcome is handled, then every 25 s; it includes the server read loop's delay in reading the pong, so it is an upper bound on the network's — so a slow open reads as latency or as bytes over bandwidth); `muster_mirror_hub_roundtrip_seconds{op="watch"}` |
| 7 | host | The host's read loop takes the frame; the watch runs in its own goroutine. It resolves the window's pane — ONE tmux listing of every window, then the pane's process tree, the pane map or the CLI's sessions file, shared by every runtime the open asks (opencode, Hermes, Claude Code) — opens and stats the transcript, reads the last 4 MiB and decodes it from the end until it has 300 events (`cctranscript.Backfill`), redacts them, cuts them into batches and writes them back on the tunnel. Then the prompt index (a scan of the whole file). An opencode window instead asks the local opencode server for its messages. | span `host.watch` (a child of `mirror.watch.first_batch`) with children `host.resolve`, `host.open`, `host.backfill`, `host.redact`, `host.frame`, `host.send` (the first batch's write), `host.prompt_index`, and `opencode GET` for opencode; `host.watch` carries `mirror.first_batch_bytes`, `mirror.first_batch_events`, and `mirror.first_batch_wire_bytes` (what the socket carried for it). Per-phase aggregates come from these spans (a span-metrics generator such as Tempo's), not from anything on the wire |
| 8 | server → browser | The hub hands the first batch to the socket handler, which renders its events to HTML (`MirrorTranscript.Ops`) and writes it to the browser. | `open.render`, `open.write`; log line `open view=mirror_view part=stream` (with the hub's round trip); `muster_open_phase_seconds{phase="first_batch"}` |
| 9 | browser | The batch is received, inserted into the log (the composer is already there), and painted. | `first_frame` (received), `batch_insert_start` / `batch_insert_end` (with the `first_batch_ops` count), then `first_paint` and `usable` at the first animation frame after the insertion (and a task after it, so the frame's paint is in); the page's open span ends; a Faro `muster_open` measurement |

Each mark is a span event of `open mirror_view` and, in the measurement, the
milliseconds from the open's start (the navigation) as `<mark>_ms`. Gap by gap:
`load_ms`→`socket_created_ms` is how long the socket waited for the page (it
is usually negative: the socket is created before load); `socket_created_ms`→
`first_frame_ms` is the server and host (step 4–8 spans); `first_frame_ms`→
`batch_insert_start_ms` is the page busy or still parsing when the batch came;
`batch_insert_start_ms`→`batch_insert_end_ms` is the DOM work, and
`batch_insert_end_ms`→`first_paint_ms` is style, layout and paint.
`warm_overlap` counts the snapshot store's warm passes that OPENED A WATCH (a
warm socket) — started, or with a warm socket open, between the open's start
and its end; a pass that found nothing to warm is not counted. Each is another
watch on a host's tunnel. On a view page warming waits for the view's open to be usable
(at most 15 s), so a non-zero value is an open that took longer than that, or a
reconnect during a pass.

⚠ `first_paint` and `usable` were, before this measurement was broken down,
marked the moment the batch was inserted; they are now after the browser
painted. A `usable_ms` series spanning that deploy grows by the paint because
its meaning changed, not because opens got slower.

In a hidden tab, which runs no animation frames until it is shown, the open
ends when its first batch is inserted (no `first_paint`) and its span carries
`muster.hidden=true`; a visible tab's open that gets no frame within 100 ms
ends then, without `first_paint`, too. Filter on `muster.hidden` to keep
background opens out of a latency series.

🔴 The Faro SDK is not loaded until the open is over. `faro-init.js` (in the
head) defines `musterOpen` at once and records each open in memory; once the
page has loaded and no open is running (at most 10 s), in idle time, it loads
the SDK and the tracing bundle, and the recorded opens become their spans and
measurements, at their recorded times. So the socket is created before the open
has a span: its `?tp=` is the DOCUMENT's traceparent, and the socket's server
span is a child of the document's server span, beside the page's open span in
the same trace rather than under it. An uncaught error before the SDK is up is
kept and sent once it is (at most 20).

What loading late costs, deliberately:

- **A page left before the SDK loaded reports nothing** — its opens, its
  disconnects and its errors were in memory only. That is a page left within
  about a second of its open ending (or, for an open still running, of the
  10 s cap).
- **Only `Error` objects thrown before the SDK loads are kept.** A throw of
  anything else (a string, an object) and a rejection with a non-Error reason
  are dropped.
- **Fetches and XHRs made before the SDK loads carry no `traceparent`**, so
  their server spans are roots of their own (sampled at the ratio), not the
  page's children.

### A terminal (`/mirror/terminal`)

Steps 1–2 as above (`GET /mirror/terminal`: entry, visit, the view, the
navigation features, render). Then the socket `GET /mirror/terminal/ws`: the
step-up check for an interactive attach, the upgrade, and the hub's attach —
an `open` frame to the host, which (on its tunnel's read loop) starts a
`muster-view-…` tmux session linking the window and attaches a client to it;
the first bytes that client prints are the first output. Spans:
`mirror.attach` → `mirror.attach.first_output` → `host.attach` →
`host.tmux_attach`; marks `stream_connected`, `first_output`, `usable`.

### An agent's page (`/agents/{name}`)

`GET /agents/{name}`: the agent row, its live status (which asks the
provisioner), the session list, the visit and read marks, the last 200
messages, the task, the asks, the switcher, the navigation features, render —
each an `open.<phase>`. The transcript is in the document, so `first_paint`
is when the script runs; `usable` is when the chat socket
(`GET /agents/{name}/ws`) opens.

## What "the session disconnected" is

No string says exactly that; it is what the person reads off one of these:

- **The rendered view's status line, "Disconnected — reconnecting…"**, on any
  close of the transcript socket the page did not ask for. It reconnects with
  backoff and jitter (0.5 s doubling to 15 s, each wait between half and all
  of that) while the page is shown and online, resuming from its offset.
- **The rendered view's error banner**, when the server ended the stream and
  said why (`{"type":"error","message":…,"reason":…}`). The usual one is
  *"mirror: the host's connection ended while the terminal was attached"*
  (`ErrHostDisconnected`, reason `host_disconnected`) — the text names a
  terminal for a rendered view too.
- **The terminal's "Disconnected · reconnecting…"**: it reconnects by itself
  like the rendered view, and offers Reconnect only after three failures in a
  row, or when the far side refused or ended the attach (its error text).
- An agent chat's socket closing (it reconnects silently, with the same
  backoff; the page reports it, the server does not count it).

Every one of them also reconnects **by itself when the page comes back** —
shown again after being hidden (a phone unlocked, a tab switched to), restored
by Back, the network back (`online`), the window focused — and replaces a
socket that still looks open but has heard nothing for about two heartbeats
(every live socket and the `/events` stream send one every
`MUSTER_LIVE_HEARTBEAT`, 15 s by default; see `internal/ui/live.go`). Each
such reconnect is an open of its own, and its span and measurement say so:
`muster.reconnect=true` and `muster.reconnect_reason` — `visibility`,
`pageshow`, `online`, `focus`, `stale` (the watchdog), `retry` (a closed
socket's backoff), `step_up` or `manual` — so a reconnect is never read as a
first open.

Every path that ends a view's stream, server side (each is a `reason` on the
`GET …/ws` span, on `mirror.watch`/`mirror.attach`, in
`muster_view_disconnects_total` and in the page's report):

| reason | what happened |
|---|---|
| `host_disconnected` | The host's tunnel ended, and every stream on it with it (`Hub.drop`): its read failed (the host went away, a network blip), its keepalive ping (every 25 s, 10 s to answer) failed, its token was revoked or could not be verified twice (re-checked every 30 s), a newer connection from the same host replaced it, or a **write** on the tunnel hit its context's end — `coder/websocket` closes the whole connection when a write's context ends, which before #277 included a browser leaving mid-write (one viewer leaving could drop every view on that host) and still includes any write that takes longer than its 10 s bound, on either side. |
| `host_ended` | The host ended the stream itself: the transcript could not be opened, stat'ed or read; the pane stopped running Claude Code (re-checked every third poll); the window went away. |
| `overrun` | The browser fell 256 batches (or terminal frames) behind. |
| `host_offline`, `session_gone`, `window_gone`, `not_agent`, `no_rendered_view`, `too_many` | Refused at open: no tunnel; not listed; no rendered view there; the host already has 8 views or 8 terminals open. |
| `step_up` | An interactive terminal without a fresh sign-in (a refused upgrade, which the page sees as a close before open). |
| `ended` | The far side ended cleanly (a terminal whose tmux client exited). |
| `shutdown` | muster ended it: a deploy or a restart (`EndTerminals` cancels every stream with `ErrShuttingDown`). The page is sent an error frame with this reason before the socket closes as going away. |
| `client_left` | The browser went first (navigation, a closed tab, Back). |
| `not_found`, `refused` | Refused before the upgrade: a host that is not the viewer's, or a failed upgrade. Counted like any other end. |

And one the server cannot see: **the rendered view's socket has no keepalive.**
The terminal's socket is pinged every 25 s; the transcript socket is not, and a
quiet conversation sends nothing at all, so a proxy with an idle timeout in
front of muster can close it. The page then reports `closed_1006` (or the
proxy's code) with no server reason. Telling that apart from a server-side end
is what the page's `reason` is for.

The page reports each disconnect as a Faro event `muster.session_disconnected`
`{view, reason, phase, code}` — `reason` is the server's closed word when it
sent one, else `closed_<WebSocket close code>`; `phase` is `opening` (before
the open ended: for the rendered view, before its first batch was painted — a
frame after it was drawn) or `live` — and, with tracing on, a `disconnected <view>` span
in the open's trace.

## The step-up dialog's passkey

The "Confirm it's you" dialog (`internal/ui/stepup_confirm.go`) sends two Faro
events. Both carry `open_id`, a random id the page makes for each dialog open
(16 hex digits from `crypto.getRandomValues`, derived from nothing about the
person or the session): every event of one open shares it, and a new open has
a new one.

| event | sent | fields |
|---|---|---|
| `stepup.passkey_options` | once per `POST /auth/confirm/passkey/options` the dialog makes, when it answers | `open_id`; `trigger`: `open` (the open's prefetch), `stale` (a tap found the challenge older than 4 minutes) or `retry` (a refused `/verify` spent it, or the last answer was unusable); `status`: the HTTP status, or `network`; `taps`: how many taps this open had made when it was fetched (0 for the open's own) |
| `stepup.passkey` | once per tap on "Confirm with passkey" | `open_id`; `attempt` (the tap's number in this open); `options_trigger` and `options_status` (the fetch whose challenge it used); `get`: how `navigator.credentials.get` ended (`ok`, the DOMException's name, or `not-called`); `verify`: `/verify`'s status, or empty when it was not sent |

A run of `stepup.passkey_options` with no `stepup.passkey` is options calls with
no tap: a burst within one open shows as several fetch events under one
`open_id`, a burst of opens as many `open_id`s. Neither event carries a
credential, challenge bytes or an account value. `e2e/tests/mirror-stepup.spec.ts`
pins both (against a recorder in place of `cgTrack`, not through Faro).

## Reading it

- **One trace per open, and it ENDS.** With browser tracing on, the page's
  `open <view>` span is a child of the document's server span; the socket's
  server span is the document span's child too for the page's first open
  (the open's span does not exist yet when the socket is created: see the
  table above), the open's own child for a reconnect once the SDK is up (none
  before); and the hub's and the
  host's spans follow. The socket's span is muster's own, and it ends when the open does
  (a view's first batch, a terminal's first output, a chat's accept), so a
  view left open for hours is not a trace growing for hours: what the socket
  does after that runs outside it. Its END is a span of its own,
  `<route> closed`, a root LINKED to the open, carrying the reason and how long
  the socket lived. Find the open by the `trace_id` in the log line, or the
  Faro measurement's context.
- **An open after a boosted swap** (htmx replaces the body, no navigation) is
  measured from the swap's request and parented on the swap response's
  `Server-Timing` traceparent — not on the first document's.
- **One log line per request of an open**, on the server:
  `open view=mirror_view part=stream outcome=ok trace_id=… total_ms=… entry_ms=… accept_ms=… subscribe_ms=… watch_ms=… render_ms=… write_ms=… hub_rtt_ms=… ops=… reset=… host=… session=… window=…`.
  `watch_ms` minus `hub_rtt_ms` is the server's own time before the frame;
  where the round trip went on the host is the trace's `host.*` spans.
- **Metrics:** `muster_open_phase_seconds{view,phase}`,
  `muster_mirror_hub_roundtrip_seconds{op,reason}`,
  `muster_view_disconnects_total{view,reason}`, beside
  `muster_http_request_duration_seconds{route}`. Every label is a closed
  vocabulary; no host, session, window or trace id is a label.

## Sampling

Every request that opens a session — the GET of `/mirror/view`,
`/mirror/terminal` or `/agents/{name}`, and each of their sockets — is always
traced, and so is everything under it (the hub's requests, the host's spans, the
queries) — in-process children follow their parent, and so do a host's spans,
whose parent is muster's own. Every other server request — the Agents list's
refreshes, the event stream, the badge and ask polls — is kept at
`MUSTER_TRACE_SAMPLE_RATIO` (default 5%), **even when it carries the browser's
sampled `traceparent`**: Faro samples every page, so following it would trace
every poll in full. Such a request, when kept, is still the browser span's
child. A socket's close span is kept exactly when its open was. The browser's
own spans are Faro's to sample.

## Clock skew

The host's spans carry the HOST's wall clock, so in a trace they sit where the
host's clock put them; a host whose clock is off by more than the round trip
draws its spans outside their parent. Durations are never affected: each span's
length is measured on one clock. muster does not estimate the offset; compare
`hub_rtt_ms` in the log line with the host span's duration when the placement
looks wrong.

## Privacy

muster's spans describe private sessions, so a span carries ids, sizes,
counts, durations and closed reason codes — never message or transcript text,
pane contents, a window title or label, a prompt, a token, a cookie, or an
error's prose (an error can quote a path). Server spans are named by route
pattern, and `url.path` IS the pattern (`/agents/{name}`, never the name; none
for a path no route matched); no query is recorded; the caller's address, port
and user agent are cut at export; and the paths that carry a credential
(`/auth/claim/`, `/invite/`, `/auth/reset/`) are not traced at all. SQL
statements are recorded, for kept spans only, with every literal replaced by
`?`, cut on a character boundary. The page cuts every URL it sends at its `?`.

`TestNoExportedSpanCarriesPrivateContent` (internal/api) opens a view whose
transcript, label, session name, terminal screen and a sent message all carry
a sentinel string (the view's open, the terminal's open and a hub send; not an
agent's page), checks the
sentinel reached the browser, and checks it is in no exported span, attribute,
event, status or resource.

The page's telemetry holds to the same rule:

- the console is not captured (and a console item that appears anyway is
  dropped before it is sent);
- the CSP-violation reports, which carry a sample of the violating text, are
  off;
- an exception is sent as its type (when that is a plain identifier) and its
  stack frames (file cut at the query, function name when it is an identifier,
  line and column) — its message is replaced with `[scrubbed]` and its context
  dropped.

`e2e/tests/telemetry.spec.ts` logs a sentinel at every console level, throws
it, and rejects with it on a live view, and checks it reaches the receiver in
no batch — with controls that the relay delivered the page's events and that
the thrown error did arrive, scrubbed.

## Setting it up

**muster-server** (the Deployment):

```
OTEL_EXPORTER_OTLP_ENDPOINT=http://<otlp-receiver>:4318   # Tempo's or Alloy's OTLP/HTTP
OTEL_SERVICE_NAME=muster-server                            # the default already
OTEL_RESOURCE_ATTRIBUTES=deployment.environment=<name>     # optional
MUSTER_FARO_UPSTREAM=http://<alloy>:<port>/collect         # the Faro receiver; turns the browser half on
MUSTER_FARO_TRACING=1                                      # browser spans + traceparent on same-origin requests
MUSTER_FARO_API_KEY=<key>                                  # only if the receiver checks one; it never reaches the page
```

**muster-mirror** (each host's service):

```
OTEL_EXPORTER_OTLP_ENDPOINT=http://<otlp-receiver>:4318   # reachable from the host
```

It names itself `service.name=muster-mirror` with a `host` attribute (its
mirror host name). A host with tracing off still reports its phases to the
server; it just exports no spans. Either binary can be upgraded first: the
trace context is an optional field on the tunnel that an older peer
ignores (`TestAnOlderHostIgnoresTheTraceContext`).

**The browser relay.** With `MUSTER_FARO_UPSTREAM` set, the page posts its
telemetry to `POST /telemetry/faro` on muster's own origin, and muster forwards
each batch: signed-in sessions only, same-origin only, 256 KiB per batch, 120
batches a minute per TAB (a user's Faro session) and 600 across a user's tabs,
then 429 with a `Retry-After` of 5 seconds; the receiver's key added
server-side, no browser header forwarded; a failing receiver logged at most
once a minute. The browser never addresses the receiver. The page batches every
5 s and does not capture its own polls (the ask poll, the send queue, the
badge, the relay), so one open view sends about 2 batches a minute when idle
(measured; 40 a minute before, which tripped the old per-user limit in three
minutes).

The page's initializer (`/static/js/faro-init.js`) is a static file like the
shell's other scripts: its configuration rides on its tag as `data-faro-*`.

⚠ Turning `MUSTER_FARO_TRACING` on loads the Faro tracing bundle (about 80 KiB)
on every page beside the SDK (about 90 KiB). Neither is in the head: the
initializer loads both after the page's open (above). Measured
(`e2e/tests/open-perf.spec.ts`, throttled): with them parser-blocking in the
head, DOMContentLoaded came about 150 ms later.
