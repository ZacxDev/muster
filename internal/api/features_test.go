package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/suggest"
	"github.com/ZacxDev/muster/internal/ui"
)

// testFeatureSet is a registry with one feature in each state, plus the
// features the consumers under test look up. Variable names are synthetic.
func testFeatureSet(t *testing.T, states map[string]features.Status) features.Set {
	t.Helper()
	return testFeatureSetWith(t, states)
}

// testFeatureSetWith is testFeatureSet with more catalog entries (each needs
// its state in states).
func testFeatureSetWith(t *testing.T, states map[string]features.Status, more ...features.Feature) features.Set {
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
		{ID: features.Chief, Name: "Chief", Description: "A fleet agent.", Kubernetes: true, Enable: []features.Var{{Name: "EXAMPLE_PROVISIONER", Value: "kubernetes"}}},
	}
	catalog = append(catalog, more...)
	st := map[string]features.Status{
		features.Mirror: {State: features.On}, features.Dictation: {State: features.Off, Reason: "EXAMPLE_STT_TOKEN is unset"},
		features.Attachments: {State: features.Off, Reason: "none set"}, features.SuggestedReplies: {State: features.Misconfigured, Reason: "EXAMPLE_OPENROUTER_KEY is unset"},
		features.Connectors: {State: features.Off, Reason: "EXAMPLE_CONNECTOR_KEY is unset"}, features.Plugins: {State: features.Off, Reason: "EXAMPLE_PLUGINS is unset"},
		features.SessionRouting: {State: features.Off, Reason: "EXAMPLE_ROUTING is unset"}, features.Focus: {State: features.On},
		features.AgentAskTool: {State: features.On, Reason: "set up per host"},
		features.Chief:        {State: features.Off, Reason: "there is no agent provisioner"},
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

// TestTheDoctorServesTheRegistry: /api/doctor's `features` lists each feature
// with its state (on, off, misconfigured), reason and enable line, exactly as
// FeatureViews serialises it — the serialisation whose leak scan is
// cmd/muster-server's TestTheFeatureRegistryLeaksNoConfiguredValue. (GET
// GET /api/features was dropped, #441 round 0 D3: the doctor serves the list.)
func TestTheDoctorServesTheRegistry(t *testing.T) {
	set := testFeatureSet(t, nil)
	_, h := featureServer(t, Extensions{}, set)
	req := httptest.NewRequest(http.MethodGet, DoctorPath, nil)
	req.Header.Set("Authorization", "Bearer "+testHookToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", DoctorPath, w.Code, w.Body)
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
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for i, f := range got.Features {
		byID[f.ID] = i
	}
	for id, st := range map[string]string{features.Mirror: "on", features.Dictation: "off", features.SuggestedReplies: "misconfigured"} {
		i, ok := byID[id]
		if !ok || got.Features[i].State != st {
			t.Errorf("%s: %+v, want state %s", id, got.Features, st)
		}
	}
	d := got.Features[byID[features.Dictation]]
	if d.Reason != "EXAMPLE_STT_TOKEN is unset" || len(d.EnableLines) != 1 || d.EnableLines[0] != "EXAMPLE_STT_TOKEN=<value>" ||
		d.Docs != "docs/features.md#dictation" || d.EnableHint != "set EXAMPLE_STT_TOKEN in .env and restart (docs/features.md#dictation)" {
		t.Errorf("dictation row: %+v", d)
	}
	var raw struct {
		Features json.RawMessage `json:"features"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	views, _ := json.Marshal(FeatureViews(set))
	if string(raw.Features) != string(views) {
		t.Fatalf("served features differ from FeatureViews:\n%s\n%s", raw.Features, views)
	}
	// The removed JSON route (D3) answers 404.
	gone := httptest.NewRecorder()
	h.ServeHTTP(gone, httptest.NewRequest(http.MethodGet, "/api/features", nil))
	if gone.Code != http.StatusNotFound {
		t.Fatalf("GET /api/features: %d", gone.Code)
	}
}

// TestTheGatesReadTheRegistry: a feature the registry calls off or
// misconfigured is not drawn as working, even with its extension wired — the
// microphone, the paperclip, the Connectors tab — and with no registry the
// gate is the extension alone.
func TestTheGatesReadTheRegistry(t *testing.T) {
	ext := Extensions{Transcriber: fakeTranscriberForFeatures{}}
	s, _ := featureServer(t, ext, testFeatureSet(t, nil)) // dictation: off in the registry
	if s.dictatePath() != "" || !s.dictateOff() {
		t.Fatal("the registry says dictation is off, and a microphone is still drawn")
	}
	s, _ = featureServer(t, ext, testFeatureSet(t, map[string]features.Status{features.Dictation: {State: features.On}}))
	if s.dictatePath() == "" {
		t.Fatal("dictation on in the registry and wired: no microphone")
	}
	s, _ = featureServer(t, ext, features.Set{})
	if s.dictatePath() == "" {
		t.Fatal("no registry: the wired transcriber draws no microphone")
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
	// A Kubernetes feature shows no .env block, only what it needs.
	if !strings.Contains(body, "data-feature-kubernetes") {
		t.Error("the chief row does not say it needs Kubernetes")
	}
	// The Account page links here.
	acct := sessionGet(h, s, "/account", true).Body.String()
	if !strings.Contains(acct, `href="`+FeaturesPagePath+`" id="account-features"`) {
		t.Error("the Account page does not link to the Features page")
	}
}

// TestTheSuggestedRepliesSectionFollowsTheRegistry: with the registry saying
// suggested replies are OFF (a keyless server with no credential store), the
// Account page says Off with its reason and offers no switch; with them on,
// the section is as before. (cmd/muster-server's
// TestAKeylessAccountPageSaysSuggestedRepliesAreOff drives the same page with
// the real evaluator.)
func TestTheSuggestedRepliesSectionFollowsTheRegistry(t *testing.T) {
	for _, c := range []struct {
		name  string
		state features.Status
		off   bool
	}{
		{"off", features.Status{State: features.Off, Reason: "needs EXAMPLE_OPENROUTER_KEY"}, true},
		{"on", features.Status{State: features.On}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, h := featureServer(t, Extensions{Suggest: &suggest.Engine{}},
				testFeatureSet(t, map[string]features.Status{features.SuggestedReplies: c.state}))
			body := sessionGet(h, s, "/account", true).Body.String()
			if !strings.Contains(body, `data-testid="suggest-settings"`) {
				t.Fatalf("no suggested-replies section:\n%s", body)
			}
			got := strings.Contains(body, `data-state="off-needs-key"`) && strings.Contains(body, "Off: needs EXAMPLE_OPENROUTER_KEY") &&
				strings.Contains(body, `#suggested-replies`)
			if got != c.off {
				t.Fatalf("off note drawn = %v, want %v", got, c.off)
			}
			if c.off && strings.Contains(body, `id="suggest-toggle"`) {
				t.Fatal("off: a switch that would do nothing is drawn")
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

type fakeTranscriberForFeatures struct{}

func (fakeTranscriberForFeatures) Transcribe(context.Context, io.Reader, string) (string, error) {
	return "", nil
}

// TestTheAnswerBoxesShowTheMutedMicWhenDictationIsOff: the ask page's composer
// and every Asks answer box draw the muted mic when the server has no
// dictation, and the real one when it does (#441 round 1, R7).
func TestTheAnswerBoxesShowTheMutedMicWhenDictationIsOff(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/attention/asks/9", nil)
	off := askComposerHTML(r, attention.Ask{ID: 9}, "", true)
	if !strings.Contains(off, `id="ask-dictate-off" href="/account/features#dictation"`) || strings.Contains(off, `id="ask-dictate"`) {
		t.Errorf("ask page, dictation off: %s", off)
	}
	on := askComposerHTML(r, attention.Ask{ID: 9}, ui.TranscribePath, false)
	if !strings.Contains(on, `id="ask-dictate"`) || strings.Contains(on, "ask-dictate-off") {
		t.Errorf("ask page, dictation on: %s", on)
	}
	item := attentionItem{Kind: attentionKindAsk, AskID: 12, dictateOff: true}
	list := attentionItemsHTMLTo([]attentionItem{item}, time.Now(), "")
	if !strings.Contains(list, `id="answer-12-dictate-off" href="/account/features#dictation"`) {
		t.Errorf("Asks list, dictation off: no muted mic in the answer box")
	}
	item.dictate, item.dictateOff = ui.TranscribePath, false
	list = attentionItemsHTMLTo([]attentionItem{item}, time.Now(), "")
	if !strings.Contains(list, `id="answer-12-dictate"`) || strings.Contains(list, "answer-12-dictate-off") {
		t.Errorf("Asks list, dictation on: the real mic is missing or the hint is drawn")
	}
}
