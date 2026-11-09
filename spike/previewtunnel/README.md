# previewtunnel: THROWAWAY spike

**This is not production code.** It is the phase-0 spike of
[docs/design/previews.md](../../docs/design/previews.md) (PR #544), kept only so
the measurements in [docs/design/previews-spike.md](../../docs/design/previews-spike.md)
can be reproduced. Nothing in muster imports it. It is a separate Go module, so
the repository's `go test ./...` does not build it, and its `go.mod` (which adds
`hashicorp/yamux`) does not touch muster's. Delete it once phase 1 has its
own tunnel.

## What it builds

One process, all on loopback:

```
client ──► EdgeSim ──► gateway preview listener ──► tunnel ──► host ──► upstream app
                                                     ▲
host ──(WebSocket, dialled out)──► EdgeSim ──► gateway /v1/host
```

- **Tunnel**: one WebSocket wrapped with `websocket.NetConn` (coder/websocket
  v1.8.14), carrying either **HTTP/2 `CONNECT`** streams (x/net/http2 v0.58.0,
  roles reversed: host runs `http2.Server.ServeConn`, gateway holds an
  `http2.ClientConn`) or **yamux** streams (hashicorp/yamux v0.1.2). Both use a
  4 MiB per-stream receive window. Both ends ping the tunnel WebSocket.
- **Gateway**: a reverse proxy whose dial opens a tunnel stream; WebSockets
  are relayed message by message with a browser-leg ping (as the design says);
  `/__bare_echo` is the baseline echo served by the gateway itself.
- **Upstream**: `/blob` (deterministic AES-CTR bytes, for byte-exact checks),
  `/sse`, `/ws` (echo, the HMR case).
- **EdgeSim**: a TCP relay that kills a connection after N seconds with no
  bytes either way. A local stand-in for Cloudflare's ≈100 s idle timeout and
  nothing more.
- **Faults**: deliberate breakages used by the negative controls only.

## Running

```
cd spike/previewtunnel
go test -count=1 -run 'TestC1$' -v .          # also TestC2$, TestC3$, TestC4Scaled$, TestC5$
go test -count=1 -run 'TestNC' -v .           # negative controls: each must PASS by seeing its check go red
go test -count=1 -run TestH2_Paused -v .      # the connection-window finding
SPIKE_LONG=1 go test -count=1 -run TestC4Long -timeout 15m -v .   # 10 min, 100 s edge idle, 30 s pings
```

`SPIKE_RUNS` (default 3) repeats each measured criterion. Each run stays under
about a minute and uses little CPU; run one criterion at a time.
