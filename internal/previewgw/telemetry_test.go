package previewgw

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	otelcodes "go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ZacxDev/muster/internal/preview/wire"
	"github.com/ZacxDev/muster/internal/telemetry"
)

// wantMetric asserts one /metrics sample's exact value.
func wantMetric(t *testing.T, e *gwEnv, series string, want float64) {
	t.Helper()
	if got := e.metric(t, series); got != want {
		t.Errorf("%s = %v; want %v", series, got, want)
	}
}

// 🔴 Every way a preview closes is counted under its own reason, and the
// open-previews gauge follows opens and closes: page (Close), host (its
// preview_close), idle (37m10s with no request), ttl (53m10s, while a
// WebSocket keeps it from idling), host_revoked (its host token gone, with
// the host online and with it offline) and
// owner_not_served (its owner's plugin token answers 401). Opens are counted
// by opener: agent (preview_open) and page (Open on an offer).
func TestPreviewOpensAndClosesAreCountedByReason(t *testing.T) {
	e := newGW(t, gwOpts{tokens: []string{"mpl_owner1", "mpl_owner2"}, idle: 37*time.Minute + 10*time.Second,
		ttl: 53*time.Minute + 10*time.Second})
	wsApp := wsEchoApp(t, nil)
	app := appServer(t)
	for p := 4331; p <= 4337; p++ {
		e.serve(p, app)
	}
	e.serve(4334, wsApp.port)
	alpha := e.hostFor(t, 1, "alpha", wire.Listener{Port: 4331, Process: "vite", Window: "@1"},
		wire.Listener{Port: 4333, Process: "vite", Window: "@3"}, wire.Listener{Port: 4334, Process: "vite", Window: "@4"})
	e.hostFor(t, 1, "beta", wire.Listener{Port: 4335, Process: "vite", Window: "@1"})
	e.hostFor(t, 2, "gamma", wire.Listener{Port: 4336, Process: "vite", Window: "@1"})
	delta := e.hostFor(t, 1, "delta", wire.Listener{Port: 4337, Process: "vite", Window: "@1"})
	wantMetric(t, e, `muster_previews_open{kind="host"}`, 0)

	pageClosed, _, tokA := e.admitIn(t, 1, "alpha", "@1", 4331)
	hostClosed := agentOpen(t, alpha, "r-agent", "@2", 4332)
	idle, _, _ := e.admitIn(t, 1, "alpha", "@3", 4333)
	ttl, ttlCookie, _ := e.admitIn(t, 1, "alpha", "@4", 4334)
	revoked, _, _ := e.admitIn(t, 1, "beta", "@1", 4335)
	ownerGone, _, _ := e.admitIn(t, 2, "gamma", "@1", 4336)
	offline, _, _ := e.admitIn(t, 1, "delta", "@1", 4337)
	wantMetric(t, e, `muster_previews_open{kind="host"}`, 7)
	wantMetric(t, e, `muster_preview_opens_total{opener="page"}`, 6)
	wantMetric(t, e, `muster_preview_opens_total{opener="agent"}`, 1)

	if a := e.do(t, "POST", webHost(), closePath, bearer(tokA), `{"preview":"`+pageClosed+`"}`); a.status != http.StatusOK {
		t.Fatalf("Close: HTTP %d %s", a.status, a.body)
	}
	alpha.offers <- wire.Offer{Type: wire.TypeClose, Request: &wire.Request{ID: "r-close", Window: "@2", Preview: hostClosed}}
	waitGone(t, e, hostClosed)
	toks, err := e.store.List()
	if err != nil {
		t.Fatal(err)
	}
	// delta is revoked while OFFLINE: its tunnel is gone, so only the
	// no-valid-token sweep of Recheck can close its preview.
	delta.cancel()
	waitFor(t, "delta's tunnel to end", func() bool { return e.tunnelFor(1, "delta") == nil })
	for _, tk := range toks {
		if tk.Host == "beta" || tk.Host == "delta" {
			if _, err := e.store.Revoke(tk.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.g.Recheck()
	waitGone(t, e, revoked)
	waitGone(t, e, offline)
	e.fm.set("mpl_owner2", http.StatusUnauthorized, Self{})
	e.g.CheckOwners(context.Background())
	waitGone(t, e, ownerGone)

	c, _, err := e.wsDial(t, ttl, "/", own(ttl, ttlCookie), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	e.clock.Advance(40 * time.Minute) // past idle: idle closes, the WebSocket's preview does not
	waitGone(t, e, idle)
	if !isOpen(e, ttl) {
		t.Fatal("the preview with an open WebSocket closed as idle")
	}
	e.clock.Advance(14 * time.Minute) // 54m: past the TTL
	waitGone(t, e, ttl)

	for reason, n := range map[string]float64{"page": 1, "host": 1, "idle": 1, "ttl": 1, "host_revoked": 2, "owner_not_served": 1} {
		wantMetric(t, e, `muster_preview_closes_total{reason="`+reason+`"}`, n)
	}
	wantMetric(t, e, `muster_previews_open{kind="host"}`, 0)
}

// 🔴 Admitted requests are counted by status class (the app's own 2xx and
// 4xx, a WebSocket's 101), their response time is observed, every upstream
// dial by outcome (an Open on a dead port is the host's "refused"), and the
// gateway's own refusals by code — cross_site, and max_open on an agent's
// open — with any code outside the vocabulary counted as "other".
func TestRequestsDialsAndRefusalsAreCounted(t *testing.T) {
	e := newGW(t, gwOpts{})
	wsApp := wsEchoApp(t, nil)
	app := (&recorder{}).start(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	dead := closedPort(t)
	e.serve(4341, app)
	e.serve(4342, wsApp.port)
	fh := e.hostFor(t, 1, "alpha", wire.Listener{Port: 4341, Process: "vite", Window: "@1"},
		wire.Listener{Port: 4342, Process: "vite", Window: "@2"}, wire.Listener{Port: dead, Process: "gone", Window: "@1"})
	id, cookie, tok := e.admitIn(t, 1, "alpha", "@1", 4341)
	for range 3 {
		if a := e.do(t, "GET", pHost(id), "/", own(id, cookie), ""); a.status != http.StatusOK {
			t.Fatalf("HTTP %d", a.status)
		}
	}
	if a := e.do(t, "GET", pHost(id), "/missing", own(id, cookie), ""); a.status != http.StatusNotFound {
		t.Fatalf("the app's 404: HTTP %d", a.status)
	}
	wsID, wsCookie, _ := e.admitIn(t, 1, "alpha", "@2", 4342)
	c, _, err := e.wsDial(t, wsID, "/", own(wsID, wsCookie), nil)
	if err != nil {
		t.Fatal(err)
	}
	wsEcho(t, c, "hi")
	c.Close(websocket.StatusNormalClosure, "")
	cross := own(id, cookie)
	cross.Set("Sec-Fetch-Site", "same-site")
	if a := e.do(t, "GET", pHost(id), "/", cross, ""); a.status != http.StatusForbidden {
		t.Fatalf("a sibling's fetch: HTTP %d", a.status)
	}
	if a := e.do(t, "POST", webHost(), openPath, bearer(tok), `{"port":`+itoa(int64(dead))+`}`); a.body != codeNotListening {
		t.Fatalf("Open on a dead port: HTTP %d %s", a.status, a.body)
	}
	for i, w := range []string{"@1", "@1", "@1"} {
		agentOpen(t, fh, "r"+itoa(int64(i)), w, 4350+i)
	}
	fh.offers <- wire.Offer{Type: wire.TypeOpen, Request: &wire.Request{ID: "r-over", Window: "@1", Port: 4359}}
	for over := false; !over; {
		select {
		case got := <-fh.answers:
			if got.a.RequestID == "r-over" {
				if got.a.Error != codeMaxOpen {
					t.Fatalf("the 5th open in @1: %+v; want max_open", got.a)
				}
				over = true
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no answer to the 5th open in @1")
		}
	}
	waitFor(t, "the WebSocket's end to be counted", func() bool {
		return e.metric(t, `muster_preview_requests_total{auth="owner",code_class="1xx",kind="host"}`) == 1
	})

	wantMetric(t, e, `muster_preview_requests_total{auth="owner",code_class="2xx",kind="host"}`, 3)
	wantMetric(t, e, `muster_preview_requests_total{auth="owner",code_class="4xx",kind="host"}`, 1)
	wantMetric(t, e, `muster_preview_response_seconds_count{kind="host"}`, 4)
	wantMetric(t, e, `muster_preview_refusals_total{reason="cross_site"}`, 1)
	wantMetric(t, e, `muster_preview_refusals_total{reason="not_listening"}`, 1)
	wantMetric(t, e, `muster_preview_refusals_total{reason="max_open"}`, 1)
	wantMetric(t, e, `muster_preview_dial_seconds_count{kind="host",outcome="refused"}`, 1)
	if n := e.metric(t, `muster_preview_dial_seconds_count{kind="host",outcome="ok"}`); n < 4 {
		t.Errorf("muster_preview_dial_seconds_count{ok} = %v; want at least 4: two Open probes, the first proxied request's dial and the WebSocket's", n)
	}
	for in, want := range map[string]string{"cross_site": "cross_site", "too_many_connections": "too_many_connections",
		"body_too_large": "body_too_large", "../../etc/passwd": "other", "p-abc": "other", "": "other"} {
		if got := refusalLabel(in); got != want {
			t.Errorf("refusalLabel(%q) = %q; want %q", in, got, want)
		}
	}
	for st, want := range map[int]string{101: "1xx", 204: "2xx", 302: "3xx", 413: "4xx", 503: "5xx", 0: "other", 600: "other"} {
		if got := codeClass(st); got != want {
			t.Errorf("codeClass(%d) = %q; want %q", st, got, want)
		}
	}
}

// 🔴 The gateway's spans: preview.request (kind, auth, the tunnel's path,
// status class, reason) with its preview.dial child (outcome), preview.ws
// for a relayed WebSocket, and a refused request's reason as its code. 🔴 NO
// span, attribute, event or status carries the request's path, query,
// cookie or header values (a sentinel in each is in none of them).
func TestPreviewSpansCarryNoPathQueryOrHeader(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	defer telemetry.Use(tp)()
	e := newGW(t, gwOpts{maxBody: 5000})
	wsApp := wsEchoApp(t, nil)
	e.serve(4362, wsApp.port)
	_, id, cookie, _ := e.admitted(t, 4361, appServer(t))
	const sentinel = "zqxsentinel"
	h := own(id, cookie)
	h.Set("Cookie", OwnerCookie+"="+cookie+"; app="+sentinel+"c")
	h.Set("Authorization", "Bearer "+sentinel+"a")
	h.Set("X-App", sentinel+"h")
	if a := e.do(t, "GET", pHost(id), "/"+sentinel+"p?q="+sentinel+"q", h, ""); a.status != http.StatusOK {
		t.Fatalf("HTTP %d", a.status)
	}
	if a := e.do(t, "POST", pHost(id), "/"+sentinel+"big", h, strings.Repeat("x", 5001)); a.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("an overlong body: HTTP %d", a.status)
	}
	e.hostFor(t, 1, "beta", wire.Listener{Port: 4362, Process: "vite", Window: "@2"})
	wsID, wsCookie, _ := e.admitIn(t, 1, "beta", "@2", 4362)
	c, _, err := e.wsDial(t, wsID, "/"+sentinel+"ws", own(wsID, wsCookie), nil)
	if err != nil {
		t.Fatal(err)
	}
	wsEcho(t, c, "hi")
	c.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "the preview.ws span", func() bool {
		for _, s := range exp.GetSpans() {
			if s.Name == "preview.ws" {
				return true
			}
		}
		return false
	})

	byName := map[string][]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		byName[s.Name] = append(byName[s.Name], s)
		blob := s.Name + s.Status.Description
		for _, kv := range s.Attributes {
			blob += string(kv.Key) + "=" + kv.Value.Emit()
		}
		for _, ev := range s.Events {
			blob += ev.Name
			for _, kv := range ev.Attributes {
				blob += string(kv.Key) + "=" + kv.Value.Emit()
			}
		}
		if strings.Contains(blob, sentinel) {
			t.Errorf("span %s carries the request's private content: %s", s.Name, blob)
		}
	}
	attrs := func(s tracetest.SpanStub) map[string]string {
		m := map[string]string{}
		for _, kv := range s.Attributes {
			m[string(kv.Key)] = kv.Value.Emit()
		}
		return m
	}
	reqs := byName["preview.request"]
	var ok, big *tracetest.SpanStub
	for i := range reqs {
		switch attrs(reqs[i])["muster.reason"] {
		case "ok":
			if attrs(reqs[i])["muster.preview.status_class"] == "2xx" && ok == nil {
				ok = &reqs[i]
			}
		case "body_too_large":
			big = &reqs[i]
		}
	}
	if ok == nil || big == nil {
		t.Fatalf("preview.request spans %d; want a 2xx ok one and a body_too_large one: %+v", len(reqs), reqs)
	}
	for k, v := range map[string]string{"muster.preview.kind": "host", "muster.preview.auth": "owner", "muster.preview.path": "public",
		"muster.preview.status_class": "2xx"} {
		if got := attrs(*ok)[k]; got != v {
			t.Errorf("preview.request %s = %q; want %q", k, got, v)
		}
	}
	if got := attrs(*big)["muster.preview.status_class"]; got != "4xx" {
		t.Errorf("the refused request's status class %q; want 4xx", got)
	}
	var child bool
	for _, d := range byName["preview.dial"] {
		if d.Parent.SpanID() == ok.SpanContext.SpanID() && attrs(d)["muster.reason"] == "ok" && attrs(d)["muster.preview.path"] == "public" {
			child = true
		}
	}
	if !child {
		t.Errorf("no preview.dial (outcome ok) under the 2xx preview.request; dials: %+v", byName["preview.dial"])
	}
	if ws := byName["preview.ws"]; len(ws) != 1 || attrs(ws[0])["muster.reason"] != "client_left" {
		t.Errorf("preview.ws spans %+v; want one, ended by the browser (client_left)", ws)
	}
}

// 🔴 The bare net/http answers on a p- host are refusals too, counted under
// their own codes: a request to a preview that is not open (never opened, or
// closed) is no_such_preview, a non-GET entry is method_not_allowed. The 404
// stays net/http's plain one (no code page, no ErrorCodeHeader).
func TestBareRefusalsOnAPreviewHostAreCounted(t *testing.T) {
	e := newGW(t, gwOpts{})
	_, id, cookie, _ := e.admitted(t, 4371, appServer(t))
	if a := e.do(t, "POST", pHost(id), enterPath+"?g=x", nil, "x"); a.status != http.StatusMethodNotAllowed {
		t.Fatalf("POST to the entry route: HTTP %d; want 405", a.status)
	}
	never, err := newPreviewID()
	if err != nil {
		t.Fatal(err)
	}
	if a := e.do(t, "GET", pHost(never), "/", nil, ""); a.status != http.StatusNotFound {
		t.Fatalf("a never-opened preview: HTTP %d; want 404", a.status)
	}
	if !e.g.closeFromPage(pageClaim{Owner: 1, Host: "alpha", Window: "@1"}, id) {
		t.Fatal("Close found no preview")
	}
	if a := e.do(t, "GET", pHost(id), "/", own(id, cookie), ""); a.status != http.StatusNotFound {
		t.Fatalf("a closed preview: HTTP %d; want 404", a.status)
	}
	wantMetric(t, e, `muster_preview_refusals_total{reason="method_not_allowed"}`, 1)
	wantMetric(t, e, `muster_preview_refusals_total{reason="no_such_preview"}`, 2)
	for _, c := range []string{"no_such_preview", "method_not_allowed", "internal"} {
		if got := refusalLabel(c); got != c {
			t.Errorf("refusalLabel(%q) = %q; want it in the vocabulary", c, got)
		}
	}
}

// 🔴 A preview.request span's reason is the gateway's own code, never a
// header the app wrote: an app sending X-Muster-Preview-Error:
// preview_closed — as a trailer, which net/http's ReverseProxy copies into
// the response's header map after the body, past the response rules — does
// not mark its span as an error with that reason. Control: a request the
// gateway refuses itself (body_too_large) carries its code.
func TestAnAppCannotSetItsSpansReason(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	defer telemetry.Use(tp)()
	e := newGW(t, gwOpts{maxBody: 5000})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", ErrorCodeHeader)
		w.Header().Set(ErrorCodeHeader, codePreviewClosed)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
		w.Header().Set(ErrorCodeHeader, codePreviewClosed)
	}))
	t.Cleanup(app.Close)
	_, id, cookie, _ := e.admitted(t, 4372, app.Listener.Addr().(*net.TCPAddr).Port)
	reqSpans := func() []tracetest.SpanStub {
		var out []tracetest.SpanStub
		for _, s := range exp.GetSpans() {
			if s.Name == "preview.request" {
				out = append(out, s)
			}
		}
		return out
	}
	reasonOf := func(s tracetest.SpanStub) string {
		for _, kv := range s.Attributes {
			if kv.Key == "muster.reason" {
				return kv.Value.Emit()
			}
		}
		return ""
	}
	if a := e.do(t, "GET", pHost(id), "/", own(id, cookie), ""); a.status != http.StatusOK {
		t.Fatalf("HTTP %d", a.status)
	}
	waitFor(t, "the preview.request span", func() bool { return len(reqSpans()) == 1 })
	s := reqSpans()[0]
	if r := reasonOf(s); r != "ok" || s.Status.Code == otelcodes.Error {
		t.Fatalf("the app's own 200 span: reason %q status %v %q; want ok, not an error", r, s.Status.Code, s.Status.Description)
	}
	if a := e.do(t, "POST", pHost(id), "/", own(id, cookie), strings.Repeat("x", 5001)); a.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("an overlong body: HTTP %d", a.status)
	}
	waitFor(t, "the refused request's span", func() bool { return len(reqSpans()) == 2 })
	if r := reasonOf(reqSpans()[1]); r != codeBodyTooLarge {
		t.Fatalf("the refused request's span reason %q; want %s", r, codeBodyTooLarge)
	}
}
