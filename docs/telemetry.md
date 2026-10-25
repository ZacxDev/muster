# Telemetry: tracing an open

Opening an agent session (a card on **Agents**, a switcher entry, a Recent
entry) is several requests on three machines: the browser, `muster-server` and
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
| 3 | browser | Parse, the vendored scripts (htmx, the Faro SDK when on), then the view script: it reads this window's snapshot from IndexedDB (`mirror-snap.js`, waiting at most `readWaitMs`) and paints it if one is fresh. | `snapshot_painted` mark |
| 4 | browser → server | The transcript socket `GET /mirror/view/ws` (resuming at the snapshot's `gen`/`offset` when there is one), carrying the open span's trace context as `?tp=`. | `stream_connected` mark; span `GET /mirror/view/ws` (a child of the page's open) |
| 5 | server | The socket handler: `mirrorEntry` again, the WebSocket upgrade, the attention-ask subscription and the suggested-replies tracker, then the hub's watch. | `open.entry`, `open.accept`, `open.subscribe`, `open.watch` |
| 6 | server → host | The hub writes a `watch` frame on the host's one tunnel WebSocket (shared by every view and terminal on that host). | span `mirror.watch` (the stream's whole life and why it ended) and its child `mirror.watch.first_batch` (frame written → first batch read); `muster_mirror_hub_roundtrip_seconds{op="watch"}` |
| 7 | host | The host's read loop takes the frame; the watch runs in its own goroutine. It resolves the window's pane (a tmux listing of every window, then the pane's process tree, the pane map or the CLI's sessions file), opens and stats the transcript, reads and parses the last 4 MiB (`cctranscript.Backfill`, at most 300 events), redacts it, cuts it into batches and writes them back on the tunnel. Then the prompt index (a scan of the whole file). An opencode window instead asks the local opencode server for its messages. | span `host.watch` (a child of `mirror.watch.first_batch`) with children `host.resolve`, `host.open`, `host.backfill`, `host.redact`, `host.frame`, `host.prompt_index`, and `opencode GET` for opencode. Per-phase aggregates come from these spans (a span-metrics generator such as Tempo's), not from anything on the wire |
| 8 | server → browser | The hub hands the first batch to the socket handler, which renders its events to HTML (`MirrorTranscript.Ops`) and writes it to the browser. | `open.render`, `open.write`; log line `open view=mirror_view part=stream` (with the hub's round trip); `muster_open_phase_seconds{phase="first_batch"}` |
| 9 | browser | The batch is applied to the log; the composer is already there. | `first_paint`, `usable` marks; the page's open span ends; a Faro `muster_open` measurement |

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
  backoff (0.5 s doubling to 16 s), resuming from its offset.
- **The rendered view's error banner**, when the server ended the stream and
  said why (`{"type":"error","message":…,"reason":…}`). The usual one is
  *"mirror: the host's connection ended while the terminal was attached"*
  (`ErrHostDisconnected`, reason `host_disconnected`) — the text names a
  terminal for a rendered view too.
- **The terminal's "Disconnected"** with *"The terminal connection closed."*,
  or the server's error text.
- An agent chat's socket closing (it reconnects silently after 2 s; the page
  reports it, the server does not count it).

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
| `client_left` | The browser went first (navigation, a closed tab, Back), or muster shut down. |

And one the server cannot see: **the rendered view's socket has no keepalive.**
The terminal's socket is pinged every 25 s; the transcript socket is not, and a
quiet conversation sends nothing at all, so a proxy with an idle timeout in
front of muster can close it. The page then reports `closed_1006` (or the
proxy's code) with no server reason. Telling that apart from a server-side end
is what the page's `reason` is for.

The page reports each disconnect as a Faro event `muster.session_disconnected`
`{view, reason, phase, code}` — `reason` is the server's closed word when it
sent one, else `closed_<WebSocket close code>`; `phase` is `opening` (before
the first batch) or `live` — and, with tracing on, a `disconnected <view>` span
in the open's trace.

## Reading it

- **One trace per open.** With browser tracing on, the page's `open <view>`
  span is a child of the document's server span, the socket's server span is a
  child of the open, and the hub's and the host's spans follow. Find it by the
  `trace_id` in the log line, or the Faro measurement's context.
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
queries). A request that arrives with a sampled `traceparent` (a page fetch
inside its open's trace) follows it. Every other request — the Agents list's
refreshes, the event stream, the badge poll — starts a trace at
`MUSTER_TRACE_SAMPLE_RATIO` (default 5%), so a busy page does not bury the opens.
The browser's own spans are Faro's to sample.

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
pattern, never the path; `url.path` is recorded but the query is not, and the
paths that carry a credential (`/auth/claim/`, `/invite/`, `/auth/reset/`) are
not traced at all. SQL statements are recorded with every literal replaced by
`?`. The page cuts every URL it sends at its `?`.

`TestNoExportedSpanCarriesPrivateContent` (internal/api) opens a view whose
transcript, label and session name all carry a sentinel string, checks the
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
trace context and the phase timing are optional fields on the tunnel that an
older peer ignores (`TestAnOlderHostIgnoresTheTraceContext`).

**The browser relay.** With `MUSTER_FARO_UPSTREAM` set, the page posts its
telemetry to `POST /telemetry/faro` on muster's own origin, and muster forwards
each batch: signed-in sessions only, same-origin only, 256 KiB per batch, 120
batches a minute per user, the receiver's key added server-side, no browser
header forwarded. The browser never addresses the receiver.

⚠ Turning `MUSTER_FARO_TRACING` on loads the Faro tracing bundle (about 80 KiB)
on every page, synchronously in the head, beside the SDK.
