package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/suggest"
)

// testFeatureSet is a registry with one feature in each state, plus the
// features the consumers under test look up. Variable names are synthetic.
func testFeatureSet(t *testing.T, states map[string]features.Status) features.Set {
	t.Helper()
	v := func(name string) []features.Var { return []features.Var{{Name: name, Placeholder: "value"}} }
	catalog := []features.Feature{
		{ID: features.Mirror, Name: "Mirrored sessions", Description: "Sessions.", Core: true, Enable: []features.Var{{Name: "EXAMPLE_MIRROR", Value: "1"}}},
		{ID: features.Dictation, Name: "Dictation", Description: "A microphone.", Enable: v("EXAMPLE_STT_TOKEN")},
		{ID: features.Attachments, Name: "File attachments", Description: "Files.", Enable: v("EXAMPLE_S3_BUCKET")},
		{ID: features.SuggestedReplies, Name: "Suggested replies", Description: "Chips.", Enable: v("EXAMPLE_OPENROUTER_KEY")},
		{ID: features.Connectors, Name: "Connectors and credentials", Description: "Stored credentials.", Enable: v("EXAMPLE_CONNECTOR_KEY")},
		{ID: features.Plugins, Name: "Plugins", Description: "Bots.", Enable: []features.Var{{Name: "EXAMPLE_PLUGINS", Value: "1"}}},
		{ID: features.SessionRouting, Name: "Session routing", Description: "Routes.", Enable: []features.Var{{Name: "EXAMPLE_ROUTING", Value: "1"}}},
		{ID: features.Focus, Name: "Session focus", Description: "Focus.", Enable: v("EXAMPLE_OPENROUTER_KEY")},
		{ID: features.AgentAskTool, Name: "Muster tools in sessions", Description: "Tools.", HostSide: true, Note: "run muster-mirror with -mcp-listen"},
	}
	st := map[string]features.Status{
		features.Mirror: {State: features.On}, features.Dictation: {State: features.Off, Reason: "EXAMPLE_STT_TOKEN is unset"},
		features.Attachments: {State: features.Off, Reason: "none set"}, features.SuggestedReplies: {State: features.Misconfigured, Reason: "EXAMPLE_OPENROUTER_KEY is unset"},
		features.Connectors: {State: features.Off, Reason: "EXAMPLE_CONNECTOR_KEY is unset"}, features.Plugins: {State: features.Off, Reason: "EXAMPLE_PLUGINS is unset"},
		features.SessionRouting: {State: features.Off, Reason: "EXAMPLE_ROUTING is unset"}, features.Focus: {State: features.On},
		features.AgentAskTool: {State: features.On, Reason: "set up per host"},
	}
	for k, s := range states {
		st[k] = s
	}
	set, err := features.Evaluate(catalog, st)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func featureServer(t *testing.T, ext Extensions, set features.Set) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(ext)
	s.UseFeatures(set)
	return s, s.Handler()
}

func sessionGet(h http.Handler, s *Server, target string, document bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Cookie", sessionCookieFor(s))
	if document {
		req.Header.Set("Sec-Fetch-Dest", "document")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestTheFeaturesEndpointServesEveryStateAndNeedsASession: GET /api/features
// lists each feature with its state (on, off, misconfigured), reason and enable
// line, exactly as FeatureViews serialises it — the serialisation whose leak
// scan is cmd/muster-server's TestTheFeatureRegistryLeaksNoConfiguredValue — and
// refuses a caller with no session.
func TestTheFeaturesEndpointServesEveryStateAndNeedsASession(t *testing.T) {
	set := testFeatureSet(t, nil)
	s, h := featureServer(t, Extensions{}, set)

	anon := httptest.NewRecorder()
	h.ServeHTTP(anon, httptest.NewRequest(http.MethodGet, FeaturesPath, nil))
	if anon.Code == http.StatusOK || strings.Contains(anon.Body.String(), "EXAMPLE_") {
		t.Fatalf("no session: %d %s", anon.Code, anon.Body)
	}

	w := sessionGet(h, s, FeaturesPath, false)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", FeaturesPath, w.Code, w.Body)
	}
	var got struct {
		Features []struct {
			ID          string   `json:"id"`
			State       string   `json:"state"`
			Reason      string   `json:"reason"`
			EnableHint  string   `json:"enable_hint"`
			EnableLines []string `json:"enable_lines"`
			Docs        string   `json:"docs"`
		} `json:"features"`
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for i, f := range got.Features {
		byID[f.ID] = i
	}
	want := map[string]string{features.Mirror: "on", features.Dictation: "off", features.SuggestedReplies: "misconfigured"}
	for id, st := range want {
		i, ok := byID[id]
		if !ok {
			t.Fatalf("%s missing from %s", id, w.Body)
		}
		if got.Features[i].State != st {
			t.Errorf("%s state %q, want %q", id, got.Features[i].State, st)
		}
	}
	d := got.Features[byID[features.Dictation]]
	if d.Reason != "EXAMPLE_STT_TOKEN is unset" || len(d.EnableLines) != 1 || d.EnableLines[0] != "EXAMPLE_STT_TOKEN=<value>" ||
		d.Docs != "docs/features.md#dictation" || d.EnableHint != "set EXAMPLE_STT_TOKEN in .env and restart (docs/features.md#dictation)" {
		t.Errorf("dictation row: %+v", d)
	}
	if got.Counts["on"] != 3 || got.Counts["off"] != 5 || got.Counts["misconfigured"] != 1 {
		t.Errorf("counts %v", got.Counts)
	}
	// The served list IS FeatureViews, byte for byte.
	var raw struct {
		Features json.RawMessage `json:"features"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	views, _ := json.Marshal(FeatureViews(set))
	if string(raw.Features) != string(views) {
		t.Fatalf("served features differ from FeatureViews:\n%s\n%s", raw.Features, views)
	}
}

// TestTheFeaturesPageRendersEachRowWithItsState: the page has one anchored row
// per feature, the state in each, and the exact .env line for one that is off.
func TestTheFeaturesPageRendersEachRowWithItsState(t *testing.T) {
	s, h := featureServer(t, Extensions{}, testFeatureSet(t, nil))
	w := sessionGet(h, s, FeaturesPagePath, true)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, c := range []struct{ id, state string }{
		{features.Mirror, "on"}, {features.Dictation, "off"}, {features.SuggestedReplies, "misconfigured"},
	} {
		if !strings.Contains(body, `<section id="`+c.id+`" data-feature="`+c.id+`" data-state="`+c.state+`"`) {
			t.Errorf("no %s row in state %s", c.id, c.state)
		}
	}
	if !strings.Contains(body, `<pre data-feature-enable`) || !strings.Contains(body, "EXAMPLE_STT_TOKEN=&lt;value&gt;") {
		t.Error("the dictation row does not show its .env line")
	}
	if !strings.Contains(body, "data-features-counts") {
		t.Error("no counts line")
	}
	// The Account page links here.
	acct := sessionGet(h, s, "/account", true).Body.String()
	if !strings.Contains(acct, `href="`+FeaturesPagePath+`" id="account-features"`) {
		t.Error("the Account page does not link to the Features page")
	}
}

// TestTheSuggestedRepliesSectionSaysMisconfigured: with the registry saying
// suggested replies are misconfigured (no OpenRouter key), the Account page no
// longer says only "On." — it says misconfigured, why, and links to the fix.
// The control: the same page with the feature on says no such thing.
func TestTheSuggestedRepliesSectionSaysMisconfigured(t *testing.T) {
	for _, c := range []struct {
		name  string
		state features.Status
		want  bool
	}{
		{"misconfigured", features.Status{State: features.Misconfigured, Reason: "EXAMPLE_OPENROUTER_KEY is unset"}, true},
		{"on", features.Status{State: features.On}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, h := featureServer(t, Extensions{Suggest: &suggest.Engine{}},
				testFeatureSet(t, map[string]features.Status{features.SuggestedReplies: c.state}))
			body := sessionGet(h, s, "/account", true).Body.String()
			if !strings.Contains(body, `data-testid="suggest-settings"`) {
				t.Fatalf("no suggested-replies section:\n%s", body)
			}
			got := strings.Contains(body, `id="suggest-misconfigured" data-state="misconfigured"`) &&
				strings.Contains(body, "EXAMPLE_OPENROUTER_KEY is unset") && strings.Contains(body, `#suggested-replies`)
			if got != c.want {
				t.Fatalf("misconfigured note drawn = %v, want %v", got, c.want)
			}
		})
	}
}

// TestTheCredentialsPageNamesItsVariable: with no credential store, the page
// names the variable that turns it on and links to its Features row.
func TestTheCredentialsPageNamesItsVariable(t *testing.T) {
	s, h := featureServer(t, Extensions{}, testFeatureSet(t, nil))
	body := sessionGet(h, s, "/account/credentials", true).Body.String()
	if !strings.Contains(body, "data-credentials-off") || !strings.Contains(body, "EXAMPLE_CONNECTOR_KEY") ||
		!strings.Contains(body, FeaturesPagePath+"#connectors") {
		t.Fatalf("the credentials page does not name its variable:\n%s", body)
	}
}

// TestAFeatureOffRouteAnswersATypedNotFound: a route whose feature is off
// answers 404 with {"error":"feature_disabled", feature, enable}, so the CLI can
// tell it from a route the server does not have; a person navigating gets the
// notice page saying the same. With no registry installed the body is still
// typed (feature and docs anchor).
func TestAFeatureOffRouteAnswersATypedNotFound(t *testing.T) {
	s, h := featureServer(t, Extensions{}, testFeatureSet(t, nil))
	cases := []struct {
		method, path, feature string
		hdr                   map[string]string
	}{
		{http.MethodPost, "/api/routes/abc/reply", features.SessionRouting, map[string]string{"Authorization": "Bearer " + testHookToken}},
		{http.MethodPost, "/transcribe", features.Dictation, map[string]string{"Cookie": sessionCookieFor(s), "Origin": "http://example.com"}},
		{http.MethodPost, "/uploads", features.Attachments, map[string]string{"Cookie": sessionCookieFor(s), "Origin": "http://example.com"}},
		{http.MethodPost, "/api/plugins/messages/7/retry", features.Plugins, map[string]string{"Authorization": "Bearer " + testHookToken}},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
			for k, v := range c.hdr {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			var b map[string]any
			if w.Code != http.StatusNotFound || json.Unmarshal(w.Body.Bytes(), &b) != nil {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if b["error"] != FeatureDisabledError || b["feature"] != c.feature || !strings.Contains(b["enable"].(string), "EXAMPLE_") {
				t.Fatalf("body %v", b)
			}
		})
	}
	// A document navigation gets the notice page.
	page := sessionGet(h, s, "/plugins", true)
	if page.Code != http.StatusNotFound || !strings.Contains(page.Body.String(), "Plugins is off on this server") ||
		!strings.Contains(page.Body.String(), "EXAMPLE_PLUGINS=1") {
		t.Fatalf("GET /plugins as a page: %d %s", page.Code, page.Body)
	}
	// No registry: still typed.
	_, bare := featureServer(t, Extensions{}, features.Set{})
	req := httptest.NewRequest(http.MethodPost, "/api/routes/abc/reply", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+testHookToken)
	w := httptest.NewRecorder()
	bare.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"error":"feature_disabled"`) ||
		!strings.Contains(w.Body.String(), `"docs":"docs/features.md#session-routing"`) {
		t.Fatalf("no registry: %d %s", w.Code, w.Body)
	}
}
