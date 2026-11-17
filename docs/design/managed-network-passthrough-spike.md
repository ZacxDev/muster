# Managed network: phase-0 Traefik SNI passthrough + ALPN spike, results

Status: **spike results**, for one part of the
[phase 0](managed-network.md#phase-0-spikes-no-product-change) "SNI passthrough
relay test" (listed under
[spikes not run here](managed-network-phase0.md#spikes-not-run-here)). Measured
on **2026-11-17**, design at `d1ff2dbe`.

**Scope.** This answers only the **Traefik routing** question, on a **test
cluster**, from one workstation on the same LAN. It is not the real edge: no
production node, no public name, no phone, no cellular path, no relay built on
`internal/preview/tunnel`. Read every result below at that scope. What is still
open is listed in [what this does NOT cover](#what-this-does-not-cover).

## The question

The design puts the relay behind the node's Traefik on a **shared `:443`**
([what runs where](managed-network.md#what-runs-where-on-one-node)): a TCP
router matches `HostSNI` for the product's names with TLS **passthrough**, next
to the HTTP routers already on that entrypoint, and the relay reads SNI and the
**offered** ALPN from the untouched ClientHello
([the relay](managed-network.md#the-relay)). So:

1. Does a `HostSNI` + `tls.passthrough` TCP router on the existing TLS
   entrypoint hand the backend the client's own TLS session (the backend's
   certificate, not Traefik's)?
2. Does the backend see the client's ALPN offer **exactly**, in order?
3. Can Traefik route by ALPN (`ALPN()` TCP matcher), so a ClientHello offering
   `muster-mirror/1` lands somewhere different from a browser's `h2,http/1.1`?
4. Do the existing HTTPS sites on the same entrypoint keep working?
5. Does a long-lived, idle passthrough connection survive, and what Traefik
   timeouts apply to it?

## Verdict

| # | question | answer |
|---|---|---|
| 1 | SNI passthrough on the shared entrypoint | **yes.** Matched SNI gets the backend's self-signed certificate; an unmatched SNI gets Traefik's default certificate and its HTTP routers (404). |
| 2 | ALPN offer reaches the backend intact | **yes**, exact list in client order, 4 of 4 shapes including an empty offer. |
| 3 | route by ALPN | **yes.** `HostSNI(x) && ALPN(muster-mirror/1)` beats `HostSNI(x)` for the same name. The matcher is "any offered protocol equals", **case-sensitive**. |
| 4 | coexistence | **yes**, an existing HTTPS site answered `200` before, during and after (3/3 each). |
| 5 | long-lived idle connection | **survived 200 s idle** (3 m 20 s), past both documented `respondingTimeouts` defaults. Traefik's source clears the connection deadline once the ClientHello is read, so entrypoint timeouts do not apply to a passthrough stream. |

**Two consequences for the design** (neither is a blocker):

- **Traefik cannot "refuse" a name; it falls through.** A ClientHello that
  matches no TCP router goes to the HTTP routers, which terminate TLS with
  Traefik's default certificate (measured below). So the design's "a name that
  is not a claimed `<id>` is refused by closing the connection" is the
  **relay's** job, and holds only for names the TCP router actually sends to the
  relay. That is the design's plan already (match the whole zone and let the
  relay decide); this spike confirms that a zone regex matches only the names
  it spells (see 1c) and everything else is served by Traefik, not refused.
- **ALPN routing at Traefik is available but not needed.** The design has the
  relay read ALPN itself and only select the caps. If an operator ever wanted
  machine and browser connections on different backends, `ALPN()` does it, but
  it is an exact, case-sensitive, any-of match, so a ClientHello that
  offers **both** `muster-mirror/1` and `h2` is routed as a machine connection,
  which agrees with the design's server-side preference order.

## Setup

- **Traefik v3.7.13**, one Deployment, kubernetescrd provider with **no
  namespace allowlist** (read from the live args: `--providers.kubernetescrd`
  with no `namespaces`), so the test objects sat in their own throwaway
  namespace. The TLS entrypoint is `websecure` (`http.tls=true`), behind a
  LoadBalancer on `:443`, already serving many HTTP routers. Traefik's
  configuration was not changed: no entrypoint added, no HelmRelease edit.
- **Backends:** two pods (`a`, `b`) running the same ~70-line Go program
  (`golang:1.26-bookworm`, `go run`) on `:8443`. Each makes a **self-signed**
  ECDSA certificate with `CN=backend-<a|b>.mn-spike.invalid`, sets
  `NextProtos` to `muster-mirror/1`, `h2`, `http/1.1` (the design's order),
  records `ClientHelloInfo.ServerName` and `ClientHelloInfo.SupportedProtos`
  in `GetConfigForClient`, and after the handshake writes one line
  `backend=<a|b> sni=<sni> offered=[<list>] negotiated="<proto>"` to the client,
  then echoes lines with the time since the handshake.
- **Routes** (`IngressRouteTCP`, entrypoint `websecure`, `tls.passthrough: true`):

  | route | match | service |
  |---|---|---|
  | `pass` | ``HostSNI(`pass.mn-spike.invalid`)`` | a |
  | `alpn` (two routes) | ``HostSNI(`alpn.mn-spike.invalid`) && ALPN(`muster-mirror/1`)`` | a |
  | | ``HostSNI(`alpn.mn-spike.invalid`)`` | b |
  | `only-mirror` | ``HostSNI(`only-mirror.mn-spike.invalid`) && ALPN(`muster-mirror/1`)`` | a |
  | `zone` | ``HostSNIRegexp(`^[a-z0-9-]+\.zone\.mn-spike\.invalid$`)`` | b |

  Priorities were left at the default (rule length), so the longer
  `HostSNI && ALPN` rule is tried first.
- **Client:** OpenSSL 3.5.8 `s_client` and curl 8.22.0 on a workstation on the
  same LAN. Names are under `.invalid` (RFC 6761), set explicitly as SNI; no
  DNS involved. Command shape:

  ```
  (printf 'hello\n'; sleep 2) | openssl s_client -connect <lb-ip>:443 \
      -servername <name> [-alpn <list>]
  ```

  and the lines read are `subject=`, `ALPN protocol:`, the backend's
  `backend=` line and its `echo` reply.

## Measurements

### 1. SNI routing with passthrough

**Negative control first, before any TCP route existed:**
`-servername pass.mn-spike.invalid -alpn muster-mirror/1,h2` returned
`subject=CN=TRAEFIK DEFAULT CERT`, `ALPN protocol: h2` (Traefik's own TLS,
which does not offer `muster-mirror/1`), and curl got `404`.

With the routes applied:

| SNI | ALPN offered | certificate | backend line |
|---|---|---|---|
| `pass.mn-spike.invalid` | `muster-mirror/1,h2` | `CN=backend-a.mn-spike.invalid` | `backend=a … negotiated="muster-mirror/1"` |
| `PASS.mn-spike.invalid` | `h2` | `CN=backend-a.mn-spike.invalid` | `backend=a sni=PASS.mn-spike.invalid …` (HostSNI is case-insensitive; the backend sees the client's spelling) |
| `nomatch.mn-spike.invalid` (**negative**) | `muster-mirror/1,h2` | `CN=TRAEFIK DEFAULT CERT` | none; Traefik negotiated `h2` itself |
| `nomatch.mn-spike.invalid` (**negative**) | `muster-mirror/1` only | none | Traefik refused the handshake: alert 120 `no application protocol` |
| no SNI (`-noservername`) | `muster-mirror/1` | none | same: Traefik's alert 120 |

The backend's certificate on the matched name, against Traefik's on the
unmatched one, is the passthrough evidence: Traefik never terminated TLS for a
matched name.

**1c. Zone regex** (the design matches "a regex on the zone"):

| SNI | result |
|---|---|
| `x1.zone.mn-spike.invalid` (`h2`) | `backend=b` |
| `abc-9.zone.mn-spike.invalid` (`muster-mirror/1`) | `backend=b`, negotiated `muster-mirror/1` |
| `a.b.zone.mn-spike.invalid` (**negative**, two labels) | Traefik default certificate |
| `x1.zonex.mn-spike.invalid` (**negative**) | Traefik default certificate |
| `zone.mn-spike.invalid` (**negative**, the apex) | Traefik default certificate |

So the apex and any name the regex does not spell stay with Traefik's
TLS-terminated routers, which is what the design needs for the apex site and
`tunnel.<zone>`.

### 2. The ALPN offer reaches the backend intact

All on `pass.mn-spike.invalid`, backend `a`:

| offered by client | backend's `SupportedProtos` | negotiated |
|---|---|---|
| `muster-mirror/1,h2` | `[muster-mirror/1,h2]` | `muster-mirror/1` |
| `h2,http/1.1` | `[h2,http/1.1]` | `h2` |
| `http/1.1,h2,muster-mirror/1` | `[http/1.1,h2,muster-mirror/1]` | `muster-mirror/1` |
| (none) | `[]` | `""` |

The list arrives exactly, in the client's order. The third row also confirms
the design's claim that Go's TLS server picks by **its own** `NextProtos`
order: the client listed `muster-mirror/1` last and still got it.

The negative control for "Traefik could be rewriting the offer" is the
unmatched name in 1: there Traefik's own TLS answered `h2` to the same
`muster-mirror/1,h2` offer, so a terminating Traefik is visibly different.

### 3. Routing by ALPN

All on `alpn.mn-spike.invalid` (route `a` = `HostSNI && ALPN(muster-mirror/1)`,
route `b` = `HostSNI` only):

| offered | landed on | negotiated |
|---|---|---|
| `muster-mirror/1` | **a** | `muster-mirror/1` |
| `muster-mirror/1,h2,http/1.1` | **a** | `muster-mirror/1` |
| `h2,muster-mirror/1` | **a** | `muster-mirror/1` |
| `h2,http/1.1` (**browser**) | **b** | `h2` |
| `http/1.1` | **b** | `http/1.1` |
| (none) | **b** | `""` |
| `muster-mirror/2` (**negative**) | **b**, which refused it (`tls: client requested unsupported application protocols`) |
| `MUSTER-MIRROR/1` (**negative**) | **b**, same refusal |

**ALPN-only route, no fallback** (`only-mirror.mn-spike.invalid`):
`muster-mirror/1` → backend `a`; `h2,http/1.1` → **Traefik default
certificate**, and curl to that name got `404` over HTTP/2. So a connection
that fails the ALPN condition is not refused by Traefik; it falls through to
the HTTP routers.

**Semantics, from the documentation and the source:**

- Docs, [TCP rules and priority](https://doc.traefik.io/traefik/reference/routing-configuration/tcp/routing/rules-and-priority/):
  "Checks if the connection's ALPN protocol equals `protocol`", and "the `ALPN`
  matcher is not allowed to match the `ACME-TLS/1` protocol, and Traefik returns
  an error if this is attempted". Priority is "directly equal to the length of
  the rule" by default.
- Docs, [TCP router](https://doc.traefik.io/traefik/reference/routing-configuration/tcp/routing/router/):
  "If both HTTP routers and TCP routers listen to the same EntryPoint, the TCP
  routers will apply before the HTTP routers. If no matching route is found for
  the TCP routers, then the HTTP routers will take over."
- The docs do not say whether "equals" means any offered protocol. The source
  does: `pkg/muxer/tcp/matcher.go` at `v3.7.13`, `alpn()` is
  `slices.Contains(meta.alpnProtos, proto)`, an exact, case-sensitive,
  any-of match over the offered list, which is what rows 3, 7 and 8 measured.
- The matcher reads the ClientHello Traefik peeks before choosing a router, so
  it is not specific to passthrough. Only passthrough was measured here.

### 4. Coexistence with the existing HTTPS sites

An existing HTTPS site on the same entrypoint, fetched with
`curl --resolve <site>:443:<lb-ip> https://<site>/`, 3 requests each:

| when | status | time |
|---|---|---|
| before the routes | `200` ×3 | 4.2–4.4 ms |
| with all four TCP routers live | `200` ×3 | 3.7–4.2 ms |
| after the namespace was deleted | `200` ×3 | 3.7–4.3 ms |

The `404` from an unmatched `.invalid` name in 1 is the control that a `200`
here is that site's router answering, not a catch-all.

### 5. A long-lived idle connection

Two connections opened together, each sending one line, then idle:

| connection | what happened | observed |
|---|---|---|
| `pass` → `a`, `muster-mirror/1` | idle **200 s**, then one more line | reply `echo after 3m20s: after-idle`; the backend logged the close at `3m23s` (the client's own exit) |
| `alpn` → `b`, `h2` (**negative control**) | backend pod deleted about 40 s in | still open 5 s after the delete; exited (stream closed) by the next check, 31 s after it, consistent with the pod's 30 s termination grace |

The control shows the client does see a closed passthrough stream, so the
first connection's survival is not an instrument that never notices a close.

**Which timeouts apply.** The live entrypoint sets `respondingTimeouts`
explicitly (and sets `readTimeout`/`writeTimeout` twice, `600s` then `0s`, with
different flag casing; which one wins was not determined). The documented v3
defaults are `readTimeout` **60s**, `writeTimeout` **0s**, `idleTimeout`
**180s** ([entrypoints](https://doc.traefik.io/traefik/reference/install-configuration/entrypoints/)),
and the 200 s hold passed both non-zero ones. That alone would not separate
"the timeouts do not apply" from "this deployment raised them", so the source
was read: `pkg/server/router/tcp/router.go` at `v3.7.13` clears the deadline
right after the ClientHello is read ("The deadline was set to avoid blocking
on the initial read of the ClientHello, but now that we have it, we can remove
it, and delegate this to underlying TCP server (for now only handled by HTTP
Server)"). So for a passthrough connection the entrypoint timeouts bound only
the ClientHello read. The TCP
[serversTransport](https://doc.traefik.io/traefik/reference/routing-configuration/tcp/serverstransport/)
has `dialTimeout` 30s, `dialKeepAlive` 15s and `terminationDelay` 100ms, and
**no idle timeout**. The 10-minute idle requirement therefore depends on
whatever sits in front of Traefik (NAT, the carrier, a cloud load balancer),
which this spike did not have.

WebSocket and SSE were not run as such: through passthrough they are bytes
inside the client's TLS session, which Traefik does not parse; the echo above
is the same path.

## Negative controls, collected

| control | expected | observed |
|---|---|---|
| matched-name probe before any route existed | Traefik's certificate | `CN=TRAEFIK DEFAULT CERT`, `404` |
| unmatched SNI | Traefik's certificate, not a backend | `CN=TRAEFIK DEFAULT CERT` |
| zone regex: apex, two labels, near-miss zone | not routed | Traefik's certificate, 3/3 |
| ALPN `muster-mirror/2`, `MUSTER-MIRROR/1` | not the ALPN route | landed on `b`, refused by it |
| ALPN-only route, browser offer | not the backend | Traefik's certificate, `404` |
| backend killed mid-connection | client sees the close | client exited within 31 s of the delete |
| coexistence `200` | not a catch-all | unmatched names return `404` |

## Cleanup

The throwaway namespace was deleted. Afterwards no `IngressRouteTCP` existed in
any namespace, the `pass` and `alpn` names fell back to Traefik's default
certificate (and to alert 120 for a `muster-mirror/1`-only offer), and the
coexistence site answered `200` ×3. Traefik logged `service not found` for the
four routes for about one second while the namespace's Services were deleted
before its routes, then nothing further.

## What this does NOT cover

Still open from the phase-0 "SNI passthrough relay test", all of which need the
real edge:

- **The production node's Traefik**, with the product's real zone, a public
  address and a certificate from a real CA. This was a test cluster on a LAN.
- **PROXY protocol** from Traefik to the relay (`serversTransport` with
  `proxyProtocol`), and the relay accepting it only from Traefik's addresses,
  plus the `NetworkPolicy`. Not configured here.
- **A relay built on `internal/preview/tunnel`** with the configurable cap and
  windows. The backends here were plain TLS servers, not a relay splicing into
  a tunnel.
- **A real phone on a cellular network:** WebSocket and SSE from a browser,
  through carrier NAT.
- **Idle survival past 10 minutes** on the real path. Measured here: 200 s on a
  LAN, and Traefik's own code imposes no idle limit on passthrough.
- **Added latency against Tailscale**, and **throughput at `W` = 128 KiB**.
- A ClientHello split over several TCP segments, and one over 16 KiB (the
  relay's buffering rule). Only ordinary single-segment ClientHellos were sent.
