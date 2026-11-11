package previewgw

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// A host token registers only the host it was minted for: the same token
// naming another host is refused 403 and no tunnel appears; naming its own
// host, the tunnel is up.
func TestATokenRegistersOnlyItsOwnHost(t *testing.T) {
	e := newGW(t, gwOpts{})
	tok := e.mint(t, 1, "alpha")
	_, resp, err := e.dialHost(t, dialOpts{listener: "mesh", token: tok, hostName: "beta"})
	if err == nil || statusOf(resp) != http.StatusForbidden {
		t.Fatalf("token for alpha naming beta: err %v, HTTP %d; want a 403 refusal", err, statusOf(resp))
	}
	if n := len(e.g.Tunnels()); n != 0 {
		t.Fatalf("%d tunnel(s) after the refused dial; want 0", n)
	}
	e.mustDial(t, dialOpts{listener: "mesh", token: tok, hostName: "alpha"})
	ts := e.g.Tunnels()
	if len(ts) != 1 || ts[0].Host != "alpha" || ts[0].Owner != 1 {
		t.Fatalf("tunnels = %+v; want one, owner 1, host alpha", ts)
	}
}

// Revoking a host token ends its tunnel at the next 30 s recheck — not
// before the clock reaches it, and not later — and that host's previews close
// with it. Another host's tunnel is untouched.
func TestRevokeEndsTheTunnelWithin30s(t *testing.T) {
	e := newGW(t, gwOpts{})
	tokA := e.mint(t, 1, "alpha")
	tokB := e.mint(t, 1, "bravo")
	fa := e.mustDial(t, dialOpts{listener: "mesh", token: tokA, hostName: "alpha"})
	e.mustDial(t, dialOpts{listener: "mesh", token: tokB, hostName: "bravo"})
	ta, tb := e.tunnelFor(1, "alpha"), e.tunnelFor(1, "bravo")

	fa.offers <- wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: "r1", Window: "@1", Port: 5173}}
	got := <-fa.answers
	if got.a.Error != "" || got.a.Preview == "" {
		t.Fatalf("open answer %+v", got)
	}

	var id string
	for _, rec := range mustList(t, e.store) {
		if rec.Host == "alpha" {
			id = rec.ID
		}
	}
	if _, err := e.store.Revoke(id); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(29 * time.Second) // literals: the 30 s is the contract, not RecheckEvery's value
	stillOpen(t, "alpha's tunnel 1 s before the recheck", ta.Done())
	e.clock.Advance(time.Second)
	closedSoon(t, "alpha's tunnel at the 30 s recheck", ta.Done())
	stillOpen(t, "bravo's tunnel (not revoked)", tb.Done())
	waitFor(t, "alpha's previews to close", func() bool { return len(e.g.Previews(1)) == 0 })
}

func mustList(t *testing.T, s HostTokens) []HostToken {
	t.Helper()
	l, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// The path a tunnel is on is the listener it arrived on. Every header a
// client could use to claim the other path is set to the public answer on
// the mesh listener, and the mesh answer on the public one.
func TestThePathIsDecidedByTheListener(t *testing.T) {
	e := newGW(t, gwOpts{})
	tokM := e.mint(t, 1, "meshhost")
	tokP := e.mint(t, 1, "pubhost")
	lying := func(claim string) http.Header {
		return http.Header{
			"X-Forwarded-Host":      {"tunnel." + testDomain},
			"X-Forwarded-Proto":     {"https"},
			"Cf-Connecting-Ip":      {"192.0.2.9"},
			"X-Muster-Preview-Path": {claim},
			"Forwarded":             {"host=tunnel." + testDomain},
		}
	}
	// On the mesh listener, the Host itself is the public tunnel name.
	e.mustDial(t, dialOpts{listener: "mesh", token: tokM, hostName: "meshhost", header: lying("public")})
	e.mustDial(t, dialOpts{listener: "public", token: tokP, hostName: "pubhost", header: lying("mesh")})
	paths := map[string]Path{}
	for _, ti := range e.g.Tunnels() {
		paths[ti.Host] = ti.Path
	}
	if paths["meshhost"] != PathMesh || paths["pubhost"] != PathPublic {
		t.Fatalf("paths = %v; want meshhost=mesh, pubhost=public", paths)
	}
	if m, p := e.gauge(t, PathMesh), e.gauge(t, PathPublic); m != 1 || p != 1 {
		t.Fatalf("muster_preview_tunnels mesh=%v public=%v; want 1 and 1", m, p)
	}
}

// Ten bad tokens a minute from one peer get 401; the 11th gets 429. A valid
// token from that peer is still accepted, another peer is still 401, and a
// new minute starts afresh.
func TestThe11thBadTokenFromOnePeerIs429(t *testing.T) {
	e := newGW(t, gwOpts{trusted: loopbackPeers}) // the test's dials play the relay
	good := e.mint(t, 1, "alpha")
	bad := wire.TokenPrefix + strings.Repeat("A", hostTokenLen-len(wire.TokenPrefix))
	peer := func(ip string) http.Header { return http.Header{"X-Real-Ip": {ip}} }
	for i := 1; i <= 10; i++ { // literal: 10 a minute is the contract
		_, resp, _ := e.dialHost(t, dialOpts{listener: "mesh", token: bad, hostName: "alpha", header: peer("198.51.100.1")})
		if statusOf(resp) != http.StatusUnauthorized {
			t.Fatalf("bad token #%d: HTTP %d; want 401", i, statusOf(resp))
		}
	}
	_, resp, _ := e.dialHost(t, dialOpts{listener: "mesh", token: bad, hostName: "alpha", header: peer("198.51.100.1")})
	if statusOf(resp) != http.StatusTooManyRequests {
		t.Fatalf("bad token #11: HTTP %d; want 429", statusOf(resp))
	}
	// A malformed token counts the same.
	_, resp, _ = e.dialHost(t, dialOpts{listener: "mesh", token: "junk", hostName: "alpha", header: peer("198.51.100.1")})
	if statusOf(resp) != http.StatusTooManyRequests {
		t.Fatalf("malformed token past the limit: HTTP %d; want 429", statusOf(resp))
	}
	_, resp, _ = e.dialHost(t, dialOpts{listener: "mesh", token: bad, hostName: "alpha", header: peer("198.51.100.2")})
	if statusOf(resp) != http.StatusUnauthorized {
		t.Fatalf("another peer's first bad token: HTTP %d; want 401", statusOf(resp))
	}
	e.mustDial(t, dialOpts{listener: "mesh", token: good, hostName: "alpha", header: peer("198.51.100.1")})
	e.clock.Advance(time.Minute)
	_, resp, _ = e.dialHost(t, dialOpts{listener: "mesh", token: bad, hostName: "alpha", header: peer("198.51.100.1")})
	if statusOf(resp) != http.StatusUnauthorized {
		t.Fatalf("bad token in the next minute: HTTP %d; want 401", statusOf(resp))
	}
}

// The public listener keys the throttle on CF-Connecting-IP: ten bad tokens
// from one such peer, then 429 on the 11th; a fresh CF-Connecting-IP is still
// 401. There is no cap across peers (round 0, D1). The header is believed
// because the peer (the test, on loopback) is a trusted proxy.
func TestThePublicListenerThrottlesPerCFConnectingIP(t *testing.T) {
	e := newGW(t, gwOpts{trusted: loopbackPeers})
	bad := wire.TokenPrefix + strings.Repeat("B", hostTokenLen-len(wire.TokenPrefix))
	cf := func(ip string) http.Header { return http.Header{"Cf-Connecting-Ip": {ip}} }
	for i := 1; i <= 10; i++ {
		_, resp, _ := e.dialHost(t, dialOpts{listener: "public", token: bad, hostName: "alpha", header: cf("203.0.113.1")})
		if statusOf(resp) != http.StatusUnauthorized {
			t.Fatalf("public bad token #%d: HTTP %d; want 401", i, statusOf(resp))
		}
	}
	_, resp, _ := e.dialHost(t, dialOpts{listener: "public", token: bad, hostName: "alpha", header: cf("203.0.113.1")})
	if statusOf(resp) != http.StatusTooManyRequests {
		t.Fatalf("public bad token #11 from one CF-Connecting-IP: HTTP %d; want 429", statusOf(resp))
	}
	_, resp, _ = e.dialHost(t, dialOpts{listener: "public", token: bad, hostName: "alpha", header: cf("203.0.113.2")})
	if statusOf(resp) != http.StatusUnauthorized {
		t.Fatalf("public bad token from a fresh CF-Connecting-IP: HTTP %d; want 401", statusOf(resp))
	}
}

// /metrics and /healthz are on the mesh listener only: the public listener
// answers 404 for both, whatever the Host.
func TestMetricsAndHealthAreOnTheMeshListenerOnly(t *testing.T) {
	e := newGW(t, gwOpts{})
	for _, path := range []string{"/metrics", "/healthz"} {
		resp, err := http.Get(e.mesh.URL + path)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("mesh %s: %v, HTTP %d; want 200", path, err, statusOf(resp))
		}
		resp.Body.Close()
		for _, host := range []string{"", "tunnel." + testDomain, "w." + testDomain, "in-cluster.svc"} {
			req, _ := http.NewRequest(http.MethodGet, e.public.URL+path, nil)
			if host != "" {
				req.Host = host
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil || resp.StatusCode != http.StatusNotFound {
				t.Fatalf("public %s (Host %q): %v, HTTP %d; want 404", path, host, err, statusOf(resp))
			}
			resp.Body.Close()
		}
	}
}

// A version the gateway does not speak, or none, is refused 426 with the
// range it does speak, before the token is looked at.
func TestAnUnknownVersionIsRefusedWithTheRange(t *testing.T) {
	e := newGW(t, gwOpts{})
	tok := e.mint(t, 1, "alpha")
	for _, v := range []string{"2", "0", "-", "1.0", "01x"} {
		_, resp, err := e.dialHost(t, dialOpts{listener: "mesh", token: tok, hostName: "alpha", version: v})
		if err == nil || statusOf(resp) != http.StatusUpgradeRequired {
			t.Fatalf("version %q: err %v, HTTP %d; want 426", v, err, statusOf(resp))
		}
		if got := resp.Header.Get(wire.VersionRangeHeader); got != "1-1" {
			t.Fatalf("version %q: %s = %q; want \"1-1\"", v, wire.VersionRangeHeader, got)
		}
	}
	if n := len(e.g.Tunnels()); n != 0 {
		t.Fatalf("%d tunnels; want 0", n)
	}
	// Control: version 1 with the same token is accepted.
	e.mustDial(t, dialOpts{listener: "mesh", token: tok, hostName: "alpha"})
}

// /self answering 401, or 200 WITHOUT viewer, closes that owner's tunnels and
// previews at the next minute's check, and refuses its new tunnels; the
// positive control (viewer still held) keeps the tunnel, and another owner's
// tunnel is untouched either way.
func TestSelfWithoutViewerOr401ClosesTheOwnersTunnels(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		self     Self
		wantGone bool
	}{
		{"401", http.StatusUnauthorized, Self{}, true},
		{"200 without viewer", http.StatusOK, Self{PluginID: 10, OwnerID: 1, OwnerLogin: "one@example.invalid", Capabilities: []string{"messages"}}, true},
		{"positive control: 200 with viewer", http.StatusOK, viewerSelf(1, "one@example.invalid"), false},
		{"muster unreachable (500): no verdict", http.StatusInternalServerError, Self{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newGW(t, gwOpts{tokens: []string{"mpl_owner1", "mpl_owner2"}})
			f1 := e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha"), hostName: "alpha"})
			e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 2, "zulu"), hostName: "zulu"})
			t1, t2 := e.tunnelFor(1, "alpha"), e.tunnelFor(2, "zulu")
			f1.offers <- wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: "r1", Window: "@1", Port: 3000}}
			<-f1.answers

			e.fm.set("mpl_owner1", c.status, c.self)
			e.clock.Advance(59 * time.Second) // literals: the minute is the contract, not SelfEvery's value
			stillOpen(t, "owner 1's tunnel before the minute check", t1.Done())
			e.clock.Advance(time.Second)
			if c.wantGone {
				closedSoon(t, "owner 1's tunnel", t1.Done())
				if n := len(e.g.Previews(1)); n != 0 {
					t.Fatalf("owner 1 has %d preview(s); want 0", n)
				}
				_, resp, err := e.dialHost(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha2"), hostName: "alpha2"})
				if err == nil || statusOf(resp) != http.StatusForbidden {
					t.Fatalf("a new tunnel for the closed owner: err %v, HTTP %d; want 403", err, statusOf(resp))
				}
			} else {
				stillOpen(t, "owner 1's tunnel", t1.Done())
				if n := len(e.g.Previews(1)); n != 1 {
					t.Fatalf("owner 1 has %d preview(s); want 1", n)
				}
			}
			stillOpen(t, "owner 2's tunnel", t2.Done())
		})
	}
}

// A gateway whose muster has not said a token is live serves no owner: the
// tunnel is refused 403.
func TestAnOwnerIsNotServedBeforeMusterSaysSo(t *testing.T) {
	e := newGW(t, gwOpts{tokens: []string{"mpl_owner1", "mpl_nobody"}})
	_, resp, err := e.dialHost(t, dialOpts{listener: "mesh", token: e.mint(t, 3, "gamma"), hostName: "gamma"})
	if err == nil || statusOf(resp) != http.StatusForbidden {
		t.Fatalf("owner 3 (no plugin token): err %v, HTTP %d; want 403", err, statusOf(resp))
	}
}

// A newer tunnel for the same (owner, host) takes every new stream at once.
// The older drains: it stays open while a stream it carries is open, and
// closes when that stream ends — or DrainTimeout after it was replaced, if
// the stream never ends. With no stream, it closes at once.
func TestANewerTunnelReplacesTheOlderAndDrains(t *testing.T) {
	echo := echoServer(t)
	type ending int
	const (
		streamCloses ending = iota
		timeout
		noStream
	)
	for _, c := range []struct {
		name string
		end  ending
	}{{"the last stream ends", streamCloses}, {"60 s pass", timeout}, {"no stream at all", noStream}} {
		t.Run(c.name, func(t *testing.T) {
			e := newGW(t, gwOpts{})
			tok := e.mint(t, 1, "alpha")
			oldHost := e.mustDial(t, dialOpts{listener: "public", token: tok, hostName: "alpha"})
			old := e.tunnelFor(1, "alpha")
			ctx := context.Background()
			var conn interface{ Close() error }
			if c.end != noStream {
				cn, err := e.g.Dial(ctx, 1, "alpha", echo)
				if err != nil {
					t.Fatal(err)
				}
				roundTrip(t, cn, "before")
				conn = cn
			}
			newHost := e.mustDial(t, dialOpts{listener: "mesh", token: tok, hostName: "alpha"})
			cur := e.tunnelFor(1, "alpha")
			if cur == old || cur.path != PathMesh {
				t.Fatalf("the current tunnel is not the newer mesh one")
			}
			if c.end == noStream {
				closedSoon(t, "the older tunnel with no stream", old.Done())
				return
			}
			// New streams go to the newer tunnel only.
			before := oldHost.connects.Load()
			cn2, err := e.g.Dial(ctx, 1, "alpha", echo)
			if err != nil {
				t.Fatal(err)
			}
			roundTrip(t, cn2, "after")
			cn2.Close()
			if oldHost.connects.Load() != before || newHost.connects.Load() != 1 {
				t.Fatalf("CONNECTs old=%d (was %d) new=%d; want the new stream on the new tunnel only",
					oldHost.connects.Load(), before, newHost.connects.Load())
			}
			stillOpen(t, "the draining tunnel while its stream is open", old.Done())
			if m, p := e.gauge(t, PathMesh), e.gauge(t, PathPublic); m != 1 || p != 1 {
				t.Fatalf("while draining: tunnels mesh=%v public=%v; want 1 and 1", m, p)
			}
			switch c.end {
			case streamCloses:
				conn.Close()
				closedSoon(t, "the draining tunnel after its last stream", old.Done())
			case timeout:
				e.clock.Advance(59 * time.Second) // literals: the 60 s is the contract, not DrainTimeout's value
				stillOpen(t, "the draining tunnel 1 s before the drain timeout", old.Done())
				e.clock.Advance(time.Second)
				closedSoon(t, "the draining tunnel at the drain timeout", old.Done())
				conn.Close()
			}
			stillOpen(t, "the newer tunnel", cur.Done())
			waitFor(t, "the public gauge to drop", func() bool { return e.gauge(t, PathPublic) == 0 })
		})
	}
}

// The host's announced maxConns becomes the tunnel's Dial limit; a later
// config cannot raise it above the first. The fake host's own server admits
// 64, so only the gateway's limit can stop the third stream.
func TestTheHostsMaxConnsLimitsDialsAndIsNeverRaised(t *testing.T) {
	echo := echoServer(t)
	e := newGW(t, gwOpts{dialWait: 200 * time.Millisecond})
	fh := e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha"), hostName: "alpha", maxConns: 64})
	fh.offers <- wire.Offer{Type: wire.TypeConfig, Config: &wire.Config{Ports: []string{"3000-3999"}, Offer: true, MaxOpen: 8, MaxConns: 2}}
	waitFor(t, "the config to land", func() bool { return e.tunnelFor(1, "alpha").stateMaxConns() == 2 })
	ctx := context.Background()
	open := func() []interface{ Close() error } {
		var cs []interface{ Close() error }
		for range 2 {
			c, err := e.g.Dial(ctx, 1, "alpha", echo)
			if err != nil {
				t.Fatalf("a stream within maxConns: %v", err)
			}
			cs = append(cs, c)
		}
		return cs
	}
	cs := open()
	if _, err := e.g.Dial(ctx, 1, "alpha", echo); !isMaxConns(err) {
		t.Fatalf("third stream at maxConns 2: %v; want max_conns", err)
	}
	fh.offers <- wire.Offer{Type: wire.TypeConfig, Config: &wire.Config{Ports: []string{"3000-3999"}, Offer: true, MaxOpen: 8, MaxConns: 8}}
	waitFor(t, "the second config to land", func() bool {
		cfg := e.tunnelFor(1, "alpha").stateConfig()
		return cfg != nil && cfg.MaxConns == 8
	})
	if _, err := e.g.Dial(ctx, 1, "alpha", echo); !isMaxConns(err) {
		t.Fatalf("third stream after a config of 8: %v; want max_conns (the first config's 2 holds)", err)
	}
	for _, c := range cs {
		c.Close()
	}
}

func (t *Tunnel) stateMaxConns() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state.MaxConns
}

func (t *Tunnel) stateConfig() *wire.Config {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state.Config
}

// The offer stream lands in memory: the boot id is sent, windows and
// listeners replace the whole set, an open is answered with a 26-character id
// (the same id for a repeat of the same window and port), and a close of it
// is answered and removes it.
func TestTheOfferStreamLandsInMemory(t *testing.T) {
	e := newGW(t, gwOpts{})
	fh, _, err := e.dialHost(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha"), hostName: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if b := <-fh.boots; b != e.g.Boot() || len(b) != 16 {
		t.Fatalf("boot header %q; want the gateway's %q", b, e.g.Boot())
	}
	fh.offers <- wire.Offer{Type: wire.TypeWindows, Windows: []string{"@1", "@2"}}
	fh.offers <- wire.Offer{Type: wire.TypeListeners, Listeners: []wire.Listener{{Port: 5173, Process: "node", Window: "@1", FirstSeen: "2000-01-02T03:04:05Z"}}}
	fh.offers <- wire.Offer{Type: "a-future-type"}
	fh.offers <- wire.Offer{Type: wire.TypeWindows, Windows: []string{"@2"}}
	waitFor(t, "the windows to be replaced", func() bool {
		ts := e.g.Tunnels()
		return len(ts) == 1 && fmt.Sprint(ts[0].State.Windows) == "[@2]" && len(ts[0].State.Listeners) == 1
	})
	open := wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: "r1", Window: "@2", Session: "s", Port: 5173, Label: "web", Path: "/"}}
	fh.offers <- open
	a1 := <-fh.answers
	open.Request.ID = "r2"
	fh.offers <- open
	a2 := <-fh.answers
	if a1.path != wire.OpenedPath || a1.a.RequestID != "r1" || len(a1.a.Preview) != 26 || a1.a.Error != "" {
		t.Fatalf("first open answered %+v", a1)
	}
	if a2.a.RequestID != "r2" || a2.a.Preview != a1.a.Preview {
		t.Fatalf("repeat open answered %+v; want the same preview %q", a2, a1.a.Preview)
	}
	fh.offers <- wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: "r3", Window: "@2", Port: 0}}
	if a := <-fh.answers; a.a.Error != "bad_port" {
		t.Fatalf("open of port 0 answered %+v", a)
	}
	fh.offers <- wire.Offer{Type: wire.TypeClose, Request: &wire.Request{ID: "r4", Window: "@2", Preview: a1.a.Preview}}
	if a := <-fh.answers; a.path != wire.ClosedPath || a.a.RequestID != "r4" || a.a.Error != "" || a.a.Port != 5173 {
		t.Fatalf("close answered %+v", a)
	}
	if n := len(e.g.Previews(1)); n != 0 {
		t.Fatalf("%d preview(s) after the close", n)
	}
	fh.offers <- wire.Offer{Type: wire.TypeClose, Request: &wire.Request{ID: "r5", Window: "@2", Preview: a1.a.Preview}}
	if a := <-fh.answers; a.a.Error != "not_found" {
		t.Fatalf("second close answered %+v", a)
	}
}

// A re-announced open carrying the previous gateway's id gets a NEW id: the
// host cannot choose one (wire.Request.Preview is advisory on an open).
func TestAReannouncedOpenGetsANewID(t *testing.T) {
	e := newGW(t, gwOpts{})
	fh := e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha"), hostName: "alpha"})
	const old = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	fh.offers <- wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: "r1", Window: "@1", Port: 5173, Preview: old}}
	a := (<-fh.answers).a
	if a.Error != "" || a.Preview == old || len(a.Preview) != 26 {
		t.Fatalf("re-announced open answered %+v; want a new 26-character id, not %q", a, old)
	}
	if ps := e.g.Previews(1); len(ps) != 1 || ps[0].ID != a.Preview {
		t.Fatalf("previews %+v; want the one new id %q", ps, a.Preview)
	}
}

// A host can close only its own previews: another host of the same owner
// naming the id gets not_found and the preview stays.
func TestAHostClosesOnlyItsOwnPreviews(t *testing.T) {
	e := newGW(t, gwOpts{})
	fa := e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha"), hostName: "alpha"})
	fb := e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "bravo"), hostName: "bravo"})
	fa.offers <- wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: "r1", Window: "@1", Port: 5173}}
	id := (<-fa.answers).a.Preview
	fb.offers <- wire.Offer{Type: wire.TypeClose, Request: &wire.Request{ID: "r2", Window: "@1", Preview: id}}
	if a := <-fb.answers; a.a.Error != "not_found" {
		t.Fatalf("bravo closing alpha's preview: %+v; want not_found", a)
	}
	if n := len(e.g.Previews(1)); n != 1 {
		t.Fatalf("%d preview(s); want alpha's 1 still open", n)
	}
}

// An unreadable host-token file at one recheck keeps the tunnels (a torn
// read must not drop every host); at the second in a row it ends them all,
// as muster's mirror connect does when it cannot verify tokens twice.
func TestTwoUnreadableRechecksInARowEndEveryTunnel(t *testing.T) {
	e := newGW(t, gwOpts{})
	e.mustDial(t, dialOpts{listener: "mesh", token: e.mint(t, 1, "alpha"), hostName: "alpha"})
	ta := e.tunnelFor(1, "alpha")
	if err := os.WriteFile(filepath.Join(e.store.Dir, hostTokensFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(RecheckEvery)
	stillOpen(t, "the tunnel after one unreadable recheck", ta.Done())
	e.clock.Advance(RecheckEvery)
	closedSoon(t, "the tunnel after two unreadable rechecks", ta.Done())
}

// Another listener name in the zone is 404 on the public listener (the
// session page and previews arrive later); the tunnel is served only on
// tunnel.<domain>.
func TestThePublicListenerServesTheTunnelOnlyOnItsName(t *testing.T) {
	e := newGW(t, gwOpts{})
	tok := e.mint(t, 1, "alpha")
	for _, h := range []string{"w." + testDomain, testDomain, "p-abc." + testDomain, "other.example"} {
		_, resp, err := e.dialHost(t, dialOpts{listener: "public", token: tok, hostName: "alpha", hostHdr: h})
		if err == nil || statusOf(resp) != http.StatusNotFound {
			t.Fatalf("Host %s: err %v, HTTP %d; want 404", h, err, statusOf(resp))
		}
	}
	e.mustDial(t, dialOpts{listener: "public", token: tok, hostName: "alpha", hostHdr: "TUNNEL." + strings.ToUpper(testDomain) + "."})
}
