# Previews: phase-0 tunnel spike, results

Status: **spike results**, for [previews.md](previews.md) ("The spike's
go/no-go", design head `27c5b184`, PR #544). Code:
[`spike/previewtunnel/`](../../spike/previewtunnel/) (throwaway, separate Go
module). Measured 2026-11-08.

## Verdict

| | |
|---|---|
| **Transport** | **Go for HTTP/2 `CONNECT` over `websocket.NetConn`**, with one configuration rule (below). yamux also passed (1)–(3), so the fallback is not needed. |
| **Edge** | **Measured 2026-11-09** (see "Real-edge results" at the end): (4) **pass** (Cloudflare cuts idle WS at ≈125 s; 30 s pings keep it up), (5) **pass**, (1) **fail**. The extra edge leg (≈100 ms on that topology) dominates, not the transport. |
| **Phase 1** | Transport and edge behaviour are settled. **(1) needs a host↔gateway edge round trip under ≈50 ms**, which is a gateway-placement question, not a tunnel one. |

The doc asks for the measurement "from one real host, over the deployment's
real edge path to a gateway in the cluster". This spike ran **on one host, on
loopback**, with a local edge simulator. Read every number below at that scope:
it shows the transport's own cost and behaviour, not the edge's.

### The configuration rule HTTP/2 needs (new finding)

x/net/http2's client returns **connection-level** flow credit only when a
stream's body is read (`transport.go`, `cc.inflow.add` in the body `Read`). A
paused reader therefore keeps its stream window's worth of the connection
window until it resumes. Once **paused streams × stream window ≥ connection
window**, every other stream on the tunnel stalls. That is the head-of-line
failure the design rejected `ErrOverrun` for, coming back by configuration.

Measured (`TestH2_PausedReadersExhaustConnWindow`): connection window 8 MiB,
stream window 4 MiB. With **1** paused reader, 62/62 assets and the echo
complete. With **3** paused readers, **0/62** assets complete and the echo
WebSocket cannot even open.

With x/net's defaults (connection 1 GiB, stream 4 MiB) the limit is 256 paused
readers, and the design sets the host's `MaxConcurrentStreams` to `maxConns`.
**Rule for phase 1:** the gateway's
`MaxReceiveBufferPerConnection` must exceed `maxConns × MaxReceiveBufferPerStream`
(for example `maxConns` ≤ 200 with the defaults), and a test should pin it.
yamux has no connection-level window, so it cannot fail this way. Either way
the gateway holds up to one stream window (4 MiB) of memory per paused stream.

## Measurements

All runs: one Linux host (8 CPUs, kernel 7.2.3, Go 1.26.8), one process,
loopback TCP, x/net v0.58.0, coder/websocket v1.8.14, yamux v0.1.2, 4 MiB
stream window for both candidates. The edge simulator is in both paths
(browser → gateway and host → gateway). For (1), (2), (3) and (5) it never
times out. **3 runs per candidate** unless stated. Ranges are min–max across
runs.

| # | criterion (as the doc states it) | bound | HTTP/2 `CONNECT` | yamux | verdict |
|---|---|---|---|---|---|
| 1 | WS echo through a stream vs bare WS echo, same path | +50 ms p50, +150 ms p95 | overhead p50 **51–68 µs**, p95 **69–142 µs** (500 echoes/run) | p50 **72–77 µs**, p95 **95–126 µs** | **go on loopback.** The edge's share is unmeasured (see below). |
| 2 | 100 MB to a reader paused 10 s: byte-exact; host memory ≤ 2 × window | exact; ≤ 8 MiB | exact 3/3; heap growth **4.42–4.96 MiB**; host held **32 KiB** | exact 3/3; **3.12–4.46 MiB**; **32 KiB** | **go** |
| 3 | 64 concurrent streams, none stalls another; the paused reader doesn't push the echo past (1) | all complete; (1)'s bound | 62/62 assets each run (slowest 339–367 ms), all before the reader resumed; **peak 64 streams** at the host; echo under load: overhead p50 **46–68 µs**, p95 **147–200 µs** (200 echoes/run); download still exact | 62/62 (320–332 ms), peak 64; p50 **61–92 µs**, p95 **102–333 µs**; exact | **go** |
| 4 | idle tunnel and idle WS stream survive 10 min through the edge | survive | **local edge simulator only**, see below | same | **unverified on the edge.** The tunnel's side works locally. |
| 5 | an SSE event reaches the browser within 500 ms (edge doesn't buffer) | ≤ 500 ms | first-event latency p50 **137–147 µs**, max **180–265 µs** (20 streams/run) | p50 **124–131 µs**, max **149–223 µs** | **unverified on the edge.** The tunnel doesn't buffer. |

### Criterion 4, locally

| run | edge idle kill | pings (tunnel both ends, browser leg) | idle | HTTP/2 | yamux |
|---|---|---|---|---|---|
| scaled, 1 run | 3 s | 1 s | 20 s | echo after idle ok, new stream ok, tunnel alive, **0 kills** | same |
| doc's numbers, 1 run | 100 s | 30 s | 10 min | echo after idle ok, new stream ok, tunnel alive, **0 kills** | same (yamux also sends its own 30 s keepalive inside the tunnel, so its tunnel would survive without the WS pings; the HTTP/2 tunnel relies on them) |

The negative control (no pings) shows the simulator kills both legs once the
idle limit passes. So the zero kills above come from the pings, not from a
simulator that never fires. Whether the real edge treats ping frames the same
way is not known.

### How each criterion was read

1. "Bare WebSocket echo on the same path": the client's WebSocket goes
   through the edge to an echo the **gateway** serves itself (`/__bare_echo`).
   The tunnel case goes through the same edge to the gateway, which relays
   message by message, then through a `CONNECT` stream to the host and on to
   the upstream app's echo. Messages are 64 B, one in flight, 2 ms apart.
2. "The host's memory for that stream": host, gateway and client share one
   process, so the spike measures **whole-process live heap growth** (after
   `runtime.GC()`, sampled every second through the pause) against the heap
   before the request. That is an upper bound on the host's share. It also
   counts the bytes the host has read from upstream but not yet handed to the
   tunnel. "The HTTP/2 window" is the 4 MiB stream window, so the bound is
   8 MiB. Byte-exact means the sha256 and length of 100 MiB (104,857,600 B) of
   seeded AES-CTR bytes.
3. 64 streams = the paused 100 MiB download of (2) + 1 HMR WebSocket echo +
   62 asset GETs (64–512 KiB, the upstream holds each 300 ms so they overlap).
   "No stream stalling another" means every asset completes, byte-exact,
   **before the paused reader resumes**, and the echo, run during the pause,
   meets (1)'s bound against a bare baseline taken just before.
4. "Through the edge" could not be done (see below). Locally: both edges in
   the simulator kill a connection after the idle time with no bytes either
   way. After the idle period, the same idle WebSocket must echo, a **new**
   stream must open on the **same** tunnel (the host's `Run` must not have
   returned), and the simulator must have killed nothing.
5. Latency runs from the upstream's write+flush of the event (200 ms after
   the headers) to the client reading the `data:` line. One clock, one
   process.

## Negative controls (each watched red)

Each fault breaks one thing, and the test asserts the check fails **with its
own message**, so a red for the wrong reason also fails the control. Run with
`go test -run TestNC -v`. All of them went red on both candidates unless noted.

| check | fault | what was seen |
|---|---|---|
| (1) latency | host sleeps 100 ms before each write into the tunnel | `C1 FAIL: overhead p50 100.4 ms (max 50 ms) p95 100.6 ms` (h2 and yamux) |
| (2) byte-exact | host stops copying after 50 MiB (stream ends early) | `C2 FAIL: not byte-exact (52428677 of 104857600 bytes, sha match=false, read err=unexpected EOF)`. The first version reported this as a plain read error. The check was reordered so truncation reports as "not byte-exact". |
| (2) memory | host reads the upstream eagerly into an unbounded queue | `C2 FAIL: heap grew … > bound 8 MiB`: 85.5 and 88.9 MiB (h2), 79.2 and 81.3 MiB (yamux) over two control runs; host held 78–79 MiB |
| (3) head-of-line | HTTP/2 only: gateway connection window 64 KiB, below the 4 MiB stream window | `0/62` assets, `echo stalled: 0/50 before resume`. **The first attempt, connection window = stream window, stayed GREEN.** That led to the configuration rule above. |
| (4) idle survival | no pings anywhere; edge simulator kills after 3 s idle; 8 s idle | `C4 FAIL`, echo after idle false, new stream false, tunnel dead, **edge kills tunnel=1 preview=1** (so the simulator did the killing) |
| (5) no buffering | host flushes only its first write (the response head), so later small writes sit in its buffer | `C5 FAIL: event not delivered before the stream ended/timed out` (response headers did arrive, so the failure is the right one) |

The positive control for the connection-window finding is the 1-paused-reader
case of `TestH2_PausedReadersExhaustConnWindow`, which stays green.

## What remains UNVERIFIED

- **The real edge, for everything.** No `musterchief.com` records exist, so no
  host → Cloudflare → relay → gateway path exists to measure. The doc's go/no-go
  is defined on that path. Phase 1 stays gated until this harness (or its
  equivalent) runs over it.
- **(1)'s real cost is the extra edge leg, not the transport.** Through the
  edge, the tunnel adds a second traversal (browser → edge → gateway → edge
  → host, against browser → edge → gateway for the bare echo). The transport
  costs about 0.1 ms. The extra leg costs roughly one host↔gateway round trip
  through Cloudflare. If that round trip is over about 50 ms (a far host or PoP),
  (1) fails **whichever transport is used**.
- **Whether Cloudflare counts WebSocket ping/pong frames as activity** against
  its ≈100 s idle timeout. The simulator counts any byte. The 30 s interval is
  assumed sufficient, not measured.
- **Whether Cloudflare, or the relay between it and the gateway, buffers**
  `text/event-stream` or WebSocket frames (5). Locally nothing buffers. That
  is a fact about the spike's own path only.
- **The relay hop** (the deployment's relay into the cluster) is not modelled
  at all. Its buffering, idle timeouts and `Host`/upgrade pass-through matter
  for (4) and (5).
- **Real network conditions:** loss, bandwidth limits, and a real TLS
  WebSocket (this spike uses `ws://`). TCP-in-TCP behaviour under loss (both
  candidates share it) is untested.
- **Host memory in isolation:** (2)'s bound held for the whole process, which
  bounds the host's share from above. The host was not measured as a separate
  process.
- The design's other mechanisms (SSE comment injection, host port checks,
  listener reports, offers, version negotiation) are outside the spike.

## Recommendation

Build phase 1 on **HTTP/2 `CONNECT`**, as the design recommends. It met
(1)–(3) with margins of two to three orders of magnitude on loopback. It needs
no new dependency, and yamux bought nothing measurable. Two additions:

1. Pin the connection-window rule
   (`MaxReceiveBufferPerConnection > maxConns × MaxReceiveBufferPerStream`).
   Add a phase-1 test modelled on `TestH2_PausedReadersExhaustConnWindow`,
   red when the windows are misconfigured.
2. Before phase 1 starts, create the DNS records and a minimal gateway, then
   rerun (1), (4) and (5) from a real host through the real edge.
   The check functions read the client side through `Env.Base`. Pointing
   them at a remote gateway takes a small adaptor: the host process runs
   remotely, and the upstream's SSE send time has to come from the event
   payload rather than an in-process channel.

## Real-edge results (2026-11-09)

Measured through the real public path, then torn down. Code:
`cmd/edgespike` (`gateway` deployed; `run` and `idlews` on the client side).

**Path.** Client and host both ran in **one process on one workstation on the
homelab LAN**. The gateway was a pod on the homelab cluster. Every byte went
workstation → Cloudflare (PoP **ORD**, HTTP/2 + TLS 1.3 to the browser side)
→ prod Traefik (Hetzner, `5.161.19.116`) → prod nginx relay → nebula mesh →
homelab nginx relay → gateway pod. The host's tunnel made the **same trip**
in the other direction. Both relays used muster's streaming block: an empty-default
`Connection` map, `proxy_buffering off` and `proxy_read_timeout 3600s`. No
Authelia was in front; the gateway's own shared secret gated every path.
Segment round trips: workstation→ORD 28 ms, workstation→Hetzner 48 ms,
prod relay→homelab over nebula 49 ms. **Read every number at this scope.**
The gateway sits next to the host on the LAN, but the traffic goes out to
Cloudflare and Hetzner and comes back twice. A deployment whose gateway sits
near its edge would see different numbers.

| # | conditions | runs | result | bound | verdict |
|---|---|---|---|---|---|
| 1 | 150 echoes/side, 64 B, one in flight; bare = the gateway's own `/__bare_echo` over the same public path | 3 h2 + 3 yamux | bare p50 **98–114 ms**. Overhead p50 **90–106 ms** (h2), **102–118 ms** (yamux); p95 **95–124 ms** (h2), **111–154 ms** (yamux) | +50 ms p50 / +150 ms p95 | **FAIL on p50, every run, both transports.** p95 held for h2 (3/3) and for yamux 2/3 (one run 154 ms). |
| 4 | h2, 30 s WebSocket pings on the tunnel (both ends) and the gateway→client leg, 10 min with no data | 1 | echo after idle **ok**, new stream on the same tunnel **ok**, tunnel alive | survive 10 min | **pass** |
| 4 control | no pings anywhere (`-no-pings`) | 1 (+1 bare-only repeat) | host tunnel, relayed `/ws` and bare `/__bare_echo` all cut at **124.5 s, 124.6 s, 125.0 s** idle, clean EOF at both client and gateway. Repeat (bare only): **125 s** | — | the real edge cuts idle WS at **≈125 s** |
| 5 | first `data:` line, written 200 ms after headers; 15 streams/run | 3 h2, 3 h2 + `Accept-Encoding: gzip`, 1 yamux | p50 **96–106 ms**, max **105–415 ms**; gzip request answered uncompressed | ≤ 500 ms | **pass, 7/7**. The worst case was 415 ms, close to the bound. |
| 5 control | host flushes only the response head (`-host-no-flush`) | 1 | `C5: no response headers … Client.Timeout exceeded` after 7 s | — | **red**. The message differs from the loopback control ("event not delivered"): through this path, even the response headers did not arrive. |

**What the edge does, as measured:**

- **Idle WebSocket cut: about 125 s, by Cloudflare.** The same no-ping bare
  WebSocket sent **straight to the origin** (`5.161.19.116:443`, skipping
  Cloudflare) was **still open at 300 s**. Sent through Cloudflare it was cut
  at 125 s. The gateway log agrees (`bare_echo ended after 5m0.3s` vs
  `2m5.2s`). The relays' 3600 s read timeout and Traefik did not cut it within
  300 s. The cut is a clean close (EOF), seen at the client and the gateway at
  the same moment.
- **WebSocket ping/pong frames count as activity.** In the 10-minute pass, only
  ping/pong frames crossed the wire (30 s apart), and nothing was cut. Without
  them, the cut came at 125 s. So the 30 s ping interval is sufficient here,
  with a 4× margin.
- **No buffering of `text/event-stream` was observed** on this path, with the
  relays' `proxy_buffering off`. First-event latency (~100 ms) is about one
  edge round trip, since the event crosses the tunnel and the client leg.
  Not measured: what the relays do with buffering ON. The SSE recipe does not
  use it, and it was not changed for the test.
- **(1) is dominated by the extra edge leg, as predicted.** The tunnel case
  crosses the whole public path a second time. That costs one more
  workstation↔Cloudflare↔Hetzner↔homelab round trip, about 100 ms here. The
  transport itself costs about 0.1 ms (loopback numbers above), and h2 vs yamux
  is within noise. **With this topology, (1) fails whichever transport is
  used.** The bound can only be met if the host↔gateway round trip through the
  edge is under about 50 ms. That depends on placing the gateway near the edge,
  not on the tunnel.

**Not measured on the real edge:** (2) and (3), which passed on loopback and
were not re-run; the 10-minute run for yamux; loss or bandwidth limits; a
browser client. Brotli `Accept-Encoding` was not tried; Go asked only for gzip.
