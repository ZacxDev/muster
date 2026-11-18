package api

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/plugins"
)

// pluginOriginServer is a server whose MUSTER_PLUGIN_ORIGINS is origins.
func pluginOriginServer(t *testing.T, origins string) *Server {
	t.Helper()
	pats, err := plugins.ParseOrigins(origins)
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "plugin-origins-operator-pw"}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{PluginOrigins: pats})
	return s
}

// plugin-host requests: Host and Origin both a host under the plugin zone.
func pluginHostRequest(method, path, host string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader("x=1"))
	r.Host = host
	r.Header.Set("Origin", "https://"+host)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

// TestAPluginOriginHostIsRefusedOnEveryListener: with MUSTER_PLUGIN_ORIGINS
// set, a request whose Host is under it — Origin the same host, so muster's
// own same-origin check would take it as same-origin — is refused 421 before
// any handler, on each listener muster-server builds from Handler(): the main
// port, MUSTER_CONNECT_PORT's surface and MUSTER_BROWSER_PORT's listener; with
// a port on the Host, and in any case.
//
// The controls: the SAME request on a server with the variable unset is not
// refused and passes requireSameOrigin's check (the hole the refusal closes),
// and muster's own host is not refused with the variable set.
func TestAPluginOriginHostIsRefusedOnEveryListener(t *testing.T) {
	const zone = "https://*.preview.example.net"
	on := pluginOriginServer(t, zone).Handler()
	listeners := map[string]http.Handler{
		"main port":    on,
		"connect port": OnlyConnectSurface(on),
		"browser port": BrowserListener(on),
	}
	for name, h := range listeners {
		for _, rt := range []struct{ method, path, host string }{
			{"GET", "/health", "p-abc.preview.example.net"},
			{"POST", "/plugins", "p-abc.preview.example.net"},
			{"GET", "/mirror/view/ws", "P-ABC.Preview.Example.NET:443"},
			{"POST", "/mcp", "w.preview.example.net"},
		} {
			if name == "connect port" && rt.path != "/health" && rt.path != "/mcp" {
				continue // OnlyConnectSurface answers 404 for these before the handler, set or not
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, pluginHostRequest(rt.method, rt.path, rt.host))
			if rec.Code != http.StatusMisdirectedRequest || rec.Body.String() != PluginOriginHostRefusal {
				t.Errorf("%s: %s %s with Host %s: %d %q; want 421 with the refusal", name, rt.method, rt.path, rt.host,
					rec.Code, rec.Body.String())
			}
		}
	}

	// CONTROL: the hole. Unset, the same request is served, and muster's own
	// same-origin check takes it as same-origin because Host and Origin agree.
	off := pluginOriginServer(t, "")
	r := pluginHostRequest("POST", "/plugins", "p-abc.preview.example.net")
	if !off.isSameOrigin(r) {
		t.Fatal("CONTROL: with the variable unset, a plugin-host request is not same-origin; the hole this closes is not there")
	}
	rec := httptest.NewRecorder()
	off.Handler().ServeHTTP(rec, pluginHostRequest("GET", "/health", "p-abc.preview.example.net"))
	if rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: with the variable unset, GET /health on a plugin host: %d; want 200", rec.Code)
	}

	// CONTROL: muster's own host, and the zone's apex, are served with it set.
	for _, host := range []string{"muster.example.net", "preview.example.net"} {
		rec := httptest.NewRecorder()
		on.ServeHTTP(rec, pluginHostRequest("GET", "/health", host))
		if rec.Code != http.StatusOK {
			t.Errorf("CONTROL: GET /health on %s with the variable set: %d; want 200", host, rec.Code)
		}
	}
}

// TestAPluginOriginRefusalIsCountedAndLogged: the refusal is outermost, outside
// the access log, the span and the http metrics, so it must count and log
// itself. One refused request moves muster_plugin_origin_refusals_total by
// exactly one and writes one line naming its Host; the control, a served
// request on muster's own host, moves neither.
func TestAPluginOriginRefusalIsCountedAndLogged(t *testing.T) {
	pats, err := plugins.ParseOrigins("https://*.preview.example.net")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	s := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "plugin-origins-operator-pw"}, log.New(&logs, "", 0))
	s.UseExtensions(Extensions{PluginOrigins: pats})
	h := s.Handler()

	before := testutil.ToFloat64(metrics.PluginOriginRefusals)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, pluginHostRequest("GET", "/health", "muster.example.net"))
	if rec.Code != http.StatusOK || testutil.ToFloat64(metrics.PluginOriginRefusals) != before ||
		strings.Contains(logs.String(), "MUSTER_PLUGIN_ORIGINS (421)") {
		t.Fatalf("CONTROL: a served request: %d, counter moved %v, log %q; want 200 and neither",
			rec.Code, testutil.ToFloat64(metrics.PluginOriginRefusals)-before, logs.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, pluginHostRequest("POST", "/plugins", "p-abc.preview.example.net"))
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("a plugin host: %d; want 421", rec.Code)
	}
	if got := testutil.ToFloat64(metrics.PluginOriginRefusals) - before; got != 1 {
		t.Errorf("muster_plugin_origin_refusals_total moved by %v; want 1", got)
	}
	if want := `refused POST /plugins for Host "p-abc.preview.example.net": it is under MUSTER_PLUGIN_ORIGINS (421)`; strings.Count(logs.String(), want) != 1 {
		t.Errorf("log %q; want exactly one line %q", logs.String(), want)
	}
}
