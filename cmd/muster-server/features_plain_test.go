package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/plainscan"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE REAL CATALOG'S TOP-LAYER WORDS (the operator rule: muster is for people
// who are not engineers). The Features page names each feature by its
// PlainName and says what it does in its OneLine; neither may hold what the
// plain-UI scans (internal/plainscan) find.

// TestEveryFeatureHasAPlainNameAndOneLine: every catalog entry has a plain name
// and a one-line plain sentence, neither holding jargon, the sentence one line
// ending in a full stop, and no two entries sharing a plain name.
//
// 🔴 POSITIVE CONTROL: the same scan run over the catalog's own Descriptions
// (written for the docs and the doctor, with variable and product names in
// them) flags many — so a clean pass over the plain fields is the scan seeing
// text it can flag and finding none, not a scan that sees nothing.
func TestEveryFeatureHasAPlainNameAndOneLine(t *testing.T) {
	cat := featureCatalog()
	if len(cat) != len(featureIDs) {
		t.Fatalf("the catalog has %d entries, featureIDs %d", len(cat), len(featureIDs))
	}
	flaggedDescriptions := 0
	names := map[string]string{}
	for _, f := range cat {
		if plainscan.Jargon(f.Description) != "" {
			flaggedDescriptions++
		}
		if f.PlainName == "" || f.OneLine == "" {
			t.Errorf("%s: plain name %q, one-liner %q — both are required", f.ID, f.PlainName, f.OneLine)
			continue
		}
		for field, s := range map[string]string{"plain name": f.PlainName, "one-liner": f.OneLine} {
			if m := plainscan.Jargon(s); m != "" {
				t.Errorf("%s: the %s %q holds %q", f.ID, field, s, m)
			}
		}
		if strings.ContainsAny(f.OneLine, "\n`") || !strings.HasSuffix(f.OneLine, ".") || len(f.OneLine) > 110 {
			t.Errorf("%s: the one-liner %q is not one plain sentence of at most 110 characters", f.ID, f.OneLine)
		}
		if other, dup := names[f.PlainName]; dup {
			t.Errorf("%s and %s share the plain name %q", other, f.ID, f.PlainName)
		}
		names[f.PlainName] = f.ID
	}
	if flaggedDescriptions < 5 {
		t.Fatalf("POSITIVE CONTROL FAILED: the scan flags only %d of the catalog's own descriptions", flaggedDescriptions)
	}
}

// operatorPage is path on a, as the signed-in password operator (an admin).
func operatorPage(t *testing.T, a *app, pw, path string) string {
	t.Helper()
	h := a.srv.Handler()
	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"password": {pw}, "next": {"/"}}.Encode()))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	lw := httptest.NewRecorder()
	h.ServeHTTP(lw, login)
	var cookie *http.Cookie
	for _, c := range lw.Result().Cookies() {
		if c.Value != "" && c.Name == "muster_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("no session from POST /login: %d %s", lw.Code, lw.Body)
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	req.Header.Set("Sec-Fetch-Dest", "document")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d", path, w.Code)
	}
	return w.Body.String()
}

// TestTheRealFeaturesPageIsPlainWithASheetPerFeature: the Features page of a
// server built by buildApp with the REAL catalog and evaluator, as its admin:
// every feature has its row and its ⓘ sheet (the anchor every "how to turn it
// on" link opens, ui.FeatureHref), the sheets hold the setup, and the top
// layer holds none of it.
func TestTheRealFeaturesPageIsPlainWithASheetPerFeature(t *testing.T) {
	const pw = "a-synthetic-operator-password"
	var buf bytes.Buffer
	a, err := buildApp(context.Background(), config{UIPassword: pw}, log.New(&buf, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	page := operatorPage(t, a, pw, api.FeaturesPagePath)
	i, j := strings.Index(page, `data-features-page>`), strings.Index(page, `Back to account</a></div>`)
	if i < 0 || j < i {
		t.Fatalf("no Features page content:\n%.2000s", page)
	}
	region := page[i:j]
	for _, id := range featureIDs {
		href := ui.FeatureHref(id)
		sheet := href[strings.Index(href, "#")+1:]
		if !strings.Contains(region, `<section id="`+id+`" data-feature="`+id+`"`) {
			t.Errorf("%s: no row", id)
		}
		if !strings.Contains(region, `id="`+sheet+`" popover="auto" role="dialog"`) {
			t.Errorf("%s: no ⓘ sheet %q — the anchor its hints link opens nothing", id, sheet)
		}
	}
	// The control: the setup is on the page — in the sheets.
	for _, want := range []string{envSTTToken, "COMPOSE_PROFILES=stt", "docker compose up -d --wait"} {
		if !strings.Contains(region, want) {
			t.Errorf("CONTROL: the admin's page does not carry %q anywhere", want)
		}
	}
	texts, err := plainscan.TopLayerTexts(region)
	if err != nil {
		t.Fatal(err)
	}
	if len(texts) < 3*len(featureIDs) {
		t.Fatalf("CONTROL: only %d top-layer text(s) for %d features", len(texts), len(featureIDs))
	}
	for _, s := range texts {
		if m := plainscan.Jargon(s); m != "" {
			t.Errorf("the real Features page's top layer says %q (%q)", s, m)
		}
	}
}

// TestTheSharedPasswordIsReplacedNotOffOnAnAccountsServer: with per-user
// accounts (MUSTER_AUTH_MODE=gotrue) the shared password is off because
// accounts replace it — the evaluator says so in a plain sentence, so the
// Features page tells nobody to ask for it to be turned on. The control: on
// a password server it is on and replaced by nothing.
func TestTheSharedPasswordIsReplacedNotOffOnAnAccountsServer(t *testing.T) {
	gt, _ := featureSet(t, config{AuthMode: "gotrue"}, api.Extensions{}, true).Get(features.PasswordSignIn)
	if gt.State != features.Off || gt.Replaced != "Not used: everyone signs in with their own account." {
		t.Fatalf("gotrue: shared password %s, replaced %q", gt.State, gt.Replaced)
	}
	if m := plainscan.Jargon(gt.Replaced); m != "" {
		t.Errorf("the replaced line %q holds %q", gt.Replaced, m)
	}
	pw, _ := featureSet(t, config{UIPassword: "a-synthetic-operator-password"}, api.Extensions{}, true).Get(features.PasswordSignIn)
	if pw.State != features.On || pw.Replaced != "" {
		t.Fatalf("CONTROL: password mode: shared password %s, replaced %q", pw.State, pw.Replaced)
	}
}

// TestEveryHintNamesItsFeatureAsTheFeaturesPageDoes is the seam between the
// hints that say a feature is off elsewhere in the app and the page they open:
// the hint calls the feature by the page's own plain name (the real catalog's
// PlainName), read from ONE constant. Structurally: each hinted feature's
// catalog PlainName is its constant, and the mic's and paperclip's labels
// begin with it. Behaviourally: the real Credentials page and its nav's muted
// entry, on a server with no credential store, as its admin, name the
// Features page's Connectors row by that name, and the Features page's row
// shows it.
func TestEveryHintNamesItsFeatureAsTheFeaturesPageDoes(t *testing.T) {
	plain := map[string]string{}
	for _, f := range featureCatalog() {
		plain[f.ID] = f.PlainName
	}
	for id, name := range map[string]string{features.Dictation: features.DictationName, features.Attachments: features.AttachmentsName,
		features.Connectors: features.ConnectorsName} {
		if plain[id] != name || name == "" {
			t.Errorf("%s: the catalog calls it %q, its hints %q", id, plain[id], name)
		}
	}
	for label, id := range map[string]string{ui.DictateOffLabel: features.Dictation, ui.AttachOffLabel: features.Attachments} {
		if !strings.HasPrefix(label, plain[id]+" ") {
			t.Errorf("the hint %q does not name %s as the Features page does (%q)", label, id, plain[id])
		}
	}
	const pw = "a-synthetic-operator-password"
	a, err := buildApp(context.Background(), config{UIPassword: pw}, log.New(&bytes.Buffer{}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cred := operatorPage(t, a, pw, "/account/credentials")
	for _, want := range []string{
		`>Features → ` + plain[features.Connectors] + `</a>`,
		`id="sidebar-connectors-off"`,
		`aria-label="` + plain[features.Connectors] + ` — off on this server; how to turn it on"`,
	} {
		if !strings.Contains(cred, want) {
			t.Errorf("the Credentials page (no credential store) lacks %s", want)
		}
	}
	page := operatorPage(t, a, pw, api.FeaturesPagePath)
	for _, id := range []string{features.Dictation, features.Attachments, features.Connectors} {
		if !strings.Contains(page, `<h2 data-feature-name class="min-w-0 flex-1 text-sm font-semibold text-fg">`+plain[id]) {
			t.Errorf("the Features page's %s row is not named %q", id, plain[id])
		}
	}
}
