package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// pluginRouteCapabilities is THE LEDGER of what each /plugin/v1/* route needs
// (plugins.capabilities, migration 0102): "messages", or "any" for a route every
// live plugin token opens. It is written out here by hand, not read from the
// registration, so a route moving between capabilities is a diff a person reads.
var pluginRouteCapabilities = map[string]string{
	"GET /plugin/v1/self":                          "any",
	"POST /plugin/v1/messages":                     "messages",
	"POST /plugin/v1/drafts/claim":                 "messages",
	"POST /plugin/v1/drafts/{id}/result":           "messages",
	"POST /plugin/v1/tasks":                        "messages",
	"GET /plugin/v1/tasks":                         "messages",
	"POST /plugin/v1/task-writes/claim":            "messages",
	"POST /plugin/v1/task-writes/{id}/result":      "messages",
	"POST /plugin/v1/messages/{id}/owner-answered": "messages",
}

// goldenPluginRoutes is every /plugin/v1/* route in the checked-in route golden.
func goldenPluginRoutes(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "routes.golden"))
	if err != nil {
		t.Fatalf("reading the route golden: %v", err)
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		l = strings.TrimSpace(l)
		if _, path, ok := strings.Cut(l, " "); ok && strings.HasPrefix(path, pluginAPIPrefix+"/") {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// capabilityRefused serves one request and reports whether it was refused for
// a missing capability (403 naming it). A handler that panics on this fixture's
// unwired stores got past the door, so a panic counts as not refused.
func capabilityRefused(h http.Handler, method, path, tok string) (refused bool, code int) {
	r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	r.Host = "muster.test"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if recover() != nil {
				rec.Code = -1
			}
		}()
		h.ServeHTTP(rec, r)
	}()
	return rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), `capability this route needs`), rec.Code
}

// TestEveryPluginRouteNamesOneCapability: the plugin routes in the route golden
// are exactly the ledger's (red when a route is added or dropped without it),
// and each one, DRIVEN, refuses a live token that lacks the ledger's capability
// and admits one that has it — so the ledger is checked against what the door
// does, not against a list the registration keeps.
func TestEveryPluginRouteNamesOneCapability(t *testing.T) {
	golden := goldenPluginRoutes(t)
	if len(golden) < 8 {
		t.Fatalf("positive control: the golden lists %d plugin routes; want every one (at least 8)", len(golden))
	}
	var ledger []string
	for k := range pluginRouteCapabilities {
		ledger = append(ledger, k)
	}
	sort.Strings(ledger)
	if strings.Join(golden, "\n") != strings.Join(ledger, "\n") {
		t.Fatalf("the plugin routes and the capability ledger disagree:\n golden: %v\n ledger: %v", golden, ledger)
	}

	_, h, _ := newPluginsDoorServer(t, &countingPlugins{})
	for _, route := range golden {
		method, pattern, _ := strings.Cut(route, " ")
		path := strings.ReplaceAll(pattern, "{id}", "5")
		viewerRefused, viewerCode := capabilityRefused(h, method, path, pgnViewerTok)
		messagesRefused, messagesCode := capabilityRefused(h, method, path, pgnTok)
		switch want := pluginRouteCapabilities[route]; want {
		case "messages":
			if !viewerRefused {
				t.Errorf("%s with a viewer-only token: %d; want 403, the route needs messages", route, viewerCode)
			}
			if messagesRefused {
				t.Errorf("CONTROL %s with a messages token: refused (%d); want it past the capability check", route, messagesCode)
			}
		case "any":
			if viewerRefused || messagesRefused {
				t.Errorf("%s: viewer-only %d, messages %d; want both past the capability check", route, viewerCode, messagesCode)
			}
		default:
			t.Fatalf("ledger %s: unknown capability %q", route, want)
		}
	}
}

// TestAViewerOnlyTokenIsRefusedOnTheMessagesRoutes: a live token without
// `messages` cannot post to the chief. POST /plugin/v1/messages answers 403
// naming the capability, and the store sees only the token lookup — Submit is
// never reached. The control is the messages token reaching Submit.
func TestAViewerOnlyTokenIsRefusedOnTheMessagesRoutes(t *testing.T) {
	f := &countingPlugins{}
	_, h, _ := newPluginsDoorServer(t, f)
	rec := pServe(h, "POST", "/plugin/v1/messages", pgnMsg, pBearer(pgnViewerTok))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `does not have the \"messages\" capability`) {
		t.Fatalf("viewer-only token on POST /plugin/v1/messages: %d %q; want 403 naming messages", rec.Code, rec.Body.String())
	}
	if got := f.snapshot(); len(got) != 1 || got[0] != "Authenticate as system" {
		t.Fatalf("viewer-only token made store calls %v; want only the token lookup", got)
	}
	f.reset()
	if rec := pServe(h, "POST", "/plugin/v1/messages", pgnMsg, pBearer(pgnTok)); rec.Code != http.StatusAccepted {
		t.Fatalf("CONTROL: the messages token: %d %q; want 202", rec.Code, rec.Body.String())
	}
	if got := f.snapshot(); len(got) != 2 || got[1] != "Submit as plugin" {
		t.Fatalf("CONTROL: the messages token made store calls %v; want it to reach Submit", got)
	}
}

// TestPluginSelfNamesTheTokensPluginAndOwner: GET /plugin/v1/self answers the
// presented token's own plugin, owner and capabilities — each token its own —
// and refuses what the plugin door refuses (no credential, another kind of
// token, an unknown one) with the door's 401.
func TestPluginSelfNamesTheTokensPluginAndOwner(t *testing.T) {
	_, h, personal := newPluginsDoorServer(t, &countingPlugins{})
	for tok, want := range map[string]string{
		pgnTok:       `{"plugin_id":3,"plugin_name":"example-inbox","owner_id":1,"owner_login":"owner@example.invalid","capabilities":["messages"]}`,
		pgnViewerTok: `{"plugin_id":4,"plugin_name":"example-viewer","owner_id":1,"owner_login":"owner@example.invalid","capabilities":["viewer"]}`,
	} {
		rec := pServe(h, "GET", "/plugin/v1/self", "", pBearer(tok))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /plugin/v1/self (%s…): %d %q; want 200", tok[:6], rec.Code, rec.Body.String())
		}
		var got, exp any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("GET /plugin/v1/self: %v in %q", err, rec.Body.String())
		}
		_ = json.Unmarshal([]byte(want), &exp)
		gb, _ := json.Marshal(got)
		eb, _ := json.Marshal(exp)
		if string(gb) != string(eb) {
			t.Errorf("GET /plugin/v1/self (%s…):\n got %s\nwant %s", tok[:6], gb, eb)
		}
	}
	for name, c := range map[string]pluginCred{
		"no credential":  func(*http.Request) {},
		"personal token": pBearer(personal),
		"unknown plugin": pBearer("mpl_" + strings.Repeat("Z", len(pgnTok)-4)),
	} {
		if rec := pServe(h, "GET", "/plugin/v1/self", "", c); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET /plugin/v1/self with %s: %d; want 401", name, rec.Code)
		}
	}
}
