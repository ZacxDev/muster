package api

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// exposureProbeMetric is a series the default Prometheus registry always
// carries (the Go runtime collector): the positive control that the metrics
// listener's 200 is the registry, not an empty page.
const exposureProbeMetric = "go_goroutines"

// TestMetricsAreServedOnlyOnTheMetricsListener: GET /metrics is a 404 on every
// listener the main handler backs — the main port, the browser listener and the
// connect listener, in both auth modes, with no credential and with the hook
// token — and a 200 carrying the registry on MetricsHandler, which serves
// nothing else.
func TestMetricsAreServedOnlyOnTheMetricsListener(t *testing.T) {
	get := func(h http.Handler, path, bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, mode := range []AuthMode{AuthModePassword, AuthModeGoTrue} {
		s := New(nil, AuthConfig{Mode: mode, UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
		h := s.Handler()
		for name, l := range map[string]http.Handler{
			"main":    h,
			"browser": BrowserListener(h),
			"connect": OnlyConnectSurface(h),
		} {
			for _, bearer := range []string{"", testHookToken} {
				w := get(l, "/metrics", bearer)
				if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), exposureProbeMetric) {
					t.Errorf("%s mode, %s listener, credential %v: GET /metrics = %d (%d bytes); want 404",
						mode, name, bearer != "", w.Code, w.Body.Len())
				}
			}
		}
		// Control: the same handler still serves an open route, so the 404 above
		// is about /metrics and not a handler that answers nothing.
		if w := get(h, "/health", ""); w.Code != http.StatusOK {
			t.Fatalf("control: %s mode, GET /health on the main handler = %d", mode, w.Code)
		}
	}

	m := MetricsHandler()
	w := get(m, "/metrics", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), exposureProbeMetric) {
		t.Fatalf("metrics listener: GET /metrics = %d, body has the probe metric = %v; want 200 with it",
			w.Code, strings.Contains(w.Body.String(), exposureProbeMetric))
	}
	for _, path := range []string{"/health", "/readyz", "/api/whoami", "/"} {
		if w := get(m, path, ""); w.Code != http.StatusNotFound {
			t.Errorf("metrics listener: GET %s = %d; want 404 (it serves /metrics only)", path, w.Code)
		}
	}
}

// TestReadyzAnswersTheStatusOnly: /readyz answers anyone, so its body is the
// status and nothing else — no build version, no reason. The probe still
// works: 200 ready, 503 not ready.
func TestReadyzAnswersTheStatusOnly(t *testing.T) {
	old := BuildVersion
	BuildVersion = "9.9.9-exposure-test"
	t.Cleanup(func() { BuildVersion = old })

	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /readyz = %d; want 200", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"status":"ready"}` {
		t.Fatalf("GET /readyz body = %s; want exactly {\"status\":\"ready\"}", got)
	}

	s.UseExtensions(Extensions{Notes: stubNotesStore{}}) // a wiring defect
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz with a wiring defect = %d; want 503", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"status":"not ready"}` {
		t.Fatalf("GET /readyz body with a wiring defect = %s; want exactly {\"status\":\"not ready\"}", got)
	}
}

// TestHSTSIsSentWhenThePublicURLIsHTTPS: Strict-Transport-Security is on every
// response of a server whose public URL is https, and on none otherwise.
func TestHSTSIsSentWhenThePublicURLIsHTTPS(t *testing.T) {
	for _, tc := range []struct {
		publicURL, want string
	}{
		{"https://muster.example.com", hstsMaxAge},
		{"HTTPS://muster.example.com/", hstsMaxAge},
		{"http://muster.example.com", ""},
		{"http://127.0.0.1:8105", ""},
		{"", ""},
	} {
		s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken, PublicURL: tc.publicURL}, log.New(io.Discard, "", 0))
		for _, path := range []string{"/health", "/login", "/no-such-page"} {
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if got := w.Header().Get("Strict-Transport-Security"); got != tc.want {
				t.Errorf("public URL %q, GET %s: Strict-Transport-Security = %q; want %q", tc.publicURL, path, got, tc.want)
			}
		}
	}
	if hstsMaxAge != "max-age=31536000" {
		t.Fatalf("hstsMaxAge = %q; want max-age=31536000 (one year)", hstsMaxAge)
	}
}
