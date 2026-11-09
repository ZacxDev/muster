package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/users"
)

const (
	hoZone  = "https://*.viewer.example.net"
	hoEntry = "https://w.viewer.example.net/__muster/enter"
)

// ownersMirrorHosts is a mirror hub whose listing is the PRINCIPAL's: each
// owner has one host, with one window (@3 in session main).
type ownersMirrorHosts struct {
	Mirror
	hosts map[int64]string
}

func (m ownersMirrorHosts) Hosts(ctx context.Context) []mirror.HostView {
	p, ok := authz.From(ctx)
	host, have := m.hosts[p.UserID]
	if !ok || !have {
		return nil
	}
	ws := mirror.NormalizeWindows([]mirror.Window{{Session: "main", ID: "@3", Index: 1, Active: true, Claude: true}}, 0)
	h := mirror.HostView{Host: host, Windows: ws, Online: true, Rendered: true}
	h.Sessions, h.Claude, h.Labels, h.Activity = mirror.SessionsOf(ws)
	return []mirror.HostView{h}
}

type handoffSeam struct {
	t    *testing.T
	srv  *Server
	h    http.Handler
	a, b int64
	now  time.Time
	hdr  string
}

func newHandoffSeam(t *testing.T, origins string) *handoffSeam {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &handoffSeam{t: t, hdr: "X-Handoff-Test-User", now: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	sys := authz.System(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	for _, id := range []*int64{&f.a, &f.b} {
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "hof"+suffix).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ids := []int64{f.a, f.b}
		for _, q := range []string{`DELETE FROM plugins WHERE owner_id = ANY($1)`, `DELETE FROM audit_log WHERE user_id = ANY($1)`,
			`DELETE FROM users WHERE id = ANY($1)`} {
			_, _ = pool.Exec(authz.System(context.Background()), q, ids)
		}
	})
	pats, err := plugins.ParseOrigins(origins)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "plugin-handoff-operator-pw"}, log.New(io.Discard, "", 0))
	f.srv.UseExtensions(Extensions{Users: users.NewPG(pool), Plugins: plugins.NewPG(pool), PluginOrigins: pats,
		Mirror: ownersMirrorHosts{hosts: map[int64]string{f.a: "ws-a", f.b: "ws-b"}}})
	f.srv.sessionUserOverride = func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.Header.Get(f.hdr), 10, 64)
		return v
	}
	f.srv.now = func() time.Time { return f.now }
	f.h = f.srv.Handler()
	return f
}

func (f *handoffSeam) do(method, path, body string, set func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "muster.test"
	if strings.HasPrefix(body, "{") {
		r.Header.Set("Content-Type", "application/json")
	} else if body != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	set(r)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	return rec
}

// as is uid's browser on muster's own origin: fresh unless stale.
func (f *handoffSeam) as(uid int64, stale bool) func(*http.Request) {
	return func(r *http.Request) {
		c := sessionCookieFor(f.srv)
		if stale {
			c = staleCookieFor(f.srv)
		}
		r.Header.Set("Cookie", c)
		r.Header.Set("Origin", "http://muster.test")
		r.Header.Set(f.hdr, strconv.FormatInt(uid, 10))
	}
}

var mintedTokenRE = regexp.MustCompile(`id="minted-plugin-token" readonly value="(mpl_[A-Za-z0-9_-]+)"`)

// create mints a plugin through the /plugins form as uid and returns its token
// and id ("" and the page's code when refused).
func (f *handoffSeam) create(uid int64, form url.Values) (string, int64, *httptest.ResponseRecorder) {
	f.t.Helper()
	rec := f.do("POST", "/plugins", form.Encode(), f.as(uid, false))
	m := mintedTokenRE.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		return "", 0, rec
	}
	r := httptest.NewRequest("GET", "/plugin/v1/self", nil)
	r.Header.Set("Authorization", "Bearer "+m[1])
	self := httptest.NewRecorder()
	f.h.ServeHTTP(self, r)
	var out struct {
		PluginID int64 `json:"plugin_id"`
	}
	if err := json.Unmarshal(self.Body.Bytes(), &out); err != nil || out.PluginID == 0 {
		f.t.Fatalf("GET /plugin/v1/self for a minted token: %d %s", self.Code, self.Body.String())
	}
	return m[1], out.PluginID, rec
}

func viewerForm(name, entry string, caps ...string) url.Values {
	v := url.Values{"name": {name}, "label": {"Example viewer"}, "data_scope": {"example"}, "origin": {hoZone}, "entry_url": {entry}}
	if len(caps) == 0 {
		caps = []string{"viewer"}
	}
	v["capability"] = caps
	return v
}

func mintBody(plugin int64, host, window string) string {
	return `{"plugin":` + strconv.FormatInt(plugin, 10) + `,"target":{"mirror":{"host":"` + host + `","window":"` + window + `"}}}`
}

func (f *handoffSeam) mint(uid int64, stale bool, body string) (*httptest.ResponseRecorder, string) {
	f.t.Helper()
	rec := f.do("POST", pluginHandoffsPath, body, f.as(uid, stale))
	var out struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out.URL
}

func (f *handoffSeam) redeem(tok, code string) *httptest.ResponseRecorder {
	return f.do("POST", pluginHandoffRedeemPath, `{"code":"`+code+`"}`, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) })
}

func codeOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("hand-off URL %q: %v", raw, err)
	}
	return u.Query().Get("muster_code")
}

// TestAHandoffCarriesTheOwnerTargetAndServerDerivedStepUp walks the hand-off end
// to end through the real store: the owner's browser mints a code for its own
// viewer plugin and its own window and is sent to the entry URL with the code
// and the plugin id; the plugin's token redeems it ONCE, learning the owner,
// the window as the hub lists it (session from the listing), purpose `view`
// and stepped_up exactly as the SESSION's step-up was — a stale session's
// body claiming `"stepped_up": true` still reads false. The metric counts the
// mint, the redeem and the refusal.
func TestAHandoffCarriesTheOwnerTargetAndServerDerivedStepUp(t *testing.T) {
	f := newHandoffSeam(t, hoZone)
	tok, id, rec := f.create(f.a, viewerForm("viewer-a", hoEntry))
	if tok == "" {
		t.Fatalf("creating a viewer plugin: %d %s", rec.Code, rec.Body.String())
	}
	minted := testutil.ToFloat64(metrics.PluginHandoffs.WithLabelValues("view", "minted"))
	redeemed := testutil.ToFloat64(metrics.PluginHandoffs.WithLabelValues("view", "redeemed"))
	notFound := testutil.ToFloat64(metrics.PluginHandoffs.WithLabelValues("unknown", "not_found"))

	rec, link := f.mint(f.a, false, mintBody(id, "ws-a", "@3"))
	if rec.Code != http.StatusOK || !strings.HasPrefix(link, hoEntry+"?") {
		t.Fatalf("mint: %d %s; want 200 with the entry URL", rec.Code, rec.Body.String())
	}
	u, _ := url.Parse(link)
	if u.Query().Get("muster_plugin") != strconv.FormatInt(id, 10) || len(u.Query().Get("muster_code")) != 43 {
		t.Fatalf("hand-off URL %s; want muster_code (43 chars) and muster_plugin=%d", link, id)
	}
	rec = f.redeem(tok, codeOf(t, link))
	want := `{"owner_id":` + strconv.FormatInt(f.a, 10) + `,"target":{"mirror":{"host":"ws-a","session":"main","window":"@3"}},"purpose":"view","stepped_up":true,"minted_at":"2030-01-02T03:04:05Z"}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("redeem: %d\n got %s\nwant %s", rec.Code, strings.TrimSpace(rec.Body.String()), want)
	}
	if rec := f.redeem(tok, codeOf(t, link)); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), PluginHandoffNotFound) {
		t.Fatalf("a second redeem: %d %s; want 404 (single use)", rec.Code, rec.Body.String())
	}

	// A stale session, whose body claims a fresh step-up: the claim is ignored.
	rec, link = f.mint(f.a, true, `{"plugin":`+strconv.FormatInt(id, 10)+`,"stepped_up":true,"target":{"mirror":{"host":"ws-a","window":"@3"}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a stale session's mint: %d %s; want 200", rec.Code, rec.Body.String())
	}
	if rec := f.redeem(tok, codeOf(t, link)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"stepped_up":false`) {
		t.Fatalf("redeeming a stale session's code: %d %s; want stepped_up false", rec.Code, rec.Body.String())
	}

	for name, got := range map[string]float64{
		"minted":    testutil.ToFloat64(metrics.PluginHandoffs.WithLabelValues("view", "minted")) - minted,
		"redeemed":  testutil.ToFloat64(metrics.PluginHandoffs.WithLabelValues("view", "redeemed")) - redeemed,
		"not_found": testutil.ToFloat64(metrics.PluginHandoffs.WithLabelValues("unknown", "not_found")) - notFound,
	} {
		if want := map[string]float64{"minted": 2, "redeemed": 2, "not_found": 1}[name]; got != want {
			t.Errorf("muster_plugin_handoffs_total %s moved by %v; want %v", name, got, want)
		}
	}
}

// TestAHandoffNamesOnlyTheOwnersPluginAndWindow: a mint naming another owner's
// host, a window the owner's host does not list, or another owner's plugin is
// refused 404 and mints nothing; a code redeemed with another of the owner's
// viewer plugins, or another owner's, is the same 404 as an unknown one; a
// token without `viewer` is refused 403 at the door; the code expires at 60 s
// on the server's clock (59 s is redeemed).
func TestAHandoffNamesOnlyTheOwnersPluginAndWindow(t *testing.T) {
	f := newHandoffSeam(t, hoZone)
	tokA, idA, _ := f.create(f.a, viewerForm("viewer-a", hoEntry))
	tokA2, _, _ := f.create(f.a, viewerForm("viewer-a2", hoEntry))
	tokB, idB, _ := f.create(f.b, viewerForm("viewer-b", hoEntry))
	msgTok, _, _ := f.create(f.a, url.Values{"name": {"messages-a"}, "label": {"Example inbox"}, "data_scope": {"example"}})
	if tokA == "" || tokA2 == "" || tokB == "" || msgTok == "" {
		t.Fatal("instrument: a plugin was not minted")
	}
	for name, body := range map[string]string{
		"another owner's host":   mintBody(idA, "ws-b", "@3"),
		"a window not listed":    mintBody(idA, "ws-a", "@9"),
		"no window":              mintBody(idA, "ws-a", ""),
		"another owner's plugin": mintBody(idB, "ws-a", "@3"),
	} {
		if rec, _ := f.mint(f.a, false, body); rec.Code != http.StatusNotFound {
			t.Errorf("mint with %s: %d %s; want 404", name, rec.Code, rec.Body.String())
		}
	}

	_, link := f.mint(f.a, false, mintBody(idA, "ws-a", "@3"))
	code := codeOf(t, link)
	for name, tok := range map[string]string{"another of the owner's viewer plugins": tokA2, "another owner's viewer plugin": tokB} {
		if rec := f.redeem(tok, code); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), PluginHandoffNotFound) {
			t.Errorf("redeem with %s: %d %s; want the one 404", name, rec.Code, rec.Body.String())
		}
	}
	if rec := f.redeem(msgTok, code); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"viewer\" capability`) {
		t.Errorf("redeem with a messages-only token: %d %s; want 403 naming viewer", rec.Code, rec.Body.String())
	}
	// CONTROL: those refusals did not spend it; the clock moves 59 s and it redeems.
	f.now = f.now.Add(plugins.HandoffTTL - time.Second)
	if rec := f.redeem(tokA, code); rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: its own plugin at 59 s: %d %s; want 200", rec.Code, rec.Body.String())
	}
	_, link = f.mint(f.a, false, mintBody(idA, "ws-a", "@3"))
	f.now = f.now.Add(plugins.HandoffTTL)
	if rec := f.redeem(tokA, codeOf(t, link)); rec.Code != http.StatusNotFound {
		t.Fatalf("a code at 60 s: %d %s; want 404 (expired)", rec.Code, rec.Body.String())
	}
}

// TestPluginCreateRefusesEntryURLTricksAndViewerWhenUnset: the /plugins form
// draws the viewer choice only while MUSTER_PLUGIN_ORIGINS is set, offering
// exactly its patterns; an entry URL outside the chosen origin (a look-alike
// host carrying the zone in its query, user info naming it) or an origin not
// in the list is refused 400 and mints nothing. With the variable unset, a
// viewer request is refused, the form has no viewer choice, and both hand-off
// routes answer the typed feature_disabled 404 naming plugin-viewers.
func TestPluginCreateRefusesEntryURLTricksAndViewerWhenUnset(t *testing.T) {
	f := newHandoffSeam(t, hoZone)
	page := f.do("GET", "/plugins", "", f.as(f.a, false)).Body.String()
	if !strings.Contains(page, `data-testid="plugin-capabilities"`) || !strings.Contains(page, `<option value="`+hoZone+`">`) {
		t.Fatalf("with MUSTER_PLUGIN_ORIGINS set, the form has no viewer choice offering %s", hoZone)
	}
	for name, form := range map[string]url.Values{
		"a look-alike host": viewerForm("trick-a", "https://evil.example.net/x?.viewer.example.net"),
		"user info":         viewerForm("trick-b", "https://w.viewer.example.net@evil.example.net/"),
		"an unlisted origin": func() url.Values {
			v := viewerForm("trick-c", hoEntry)
			v.Set("origin", "https://*.example.net")
			return v
		}(),
		"an unknown capability": func() url.Values {
			v := viewerForm("trick-d", hoEntry)
			v["capability"] = []string{"admin"}
			return v
		}(),
	} {
		if tok, _, rec := f.create(f.a, form); tok != "" || rec.Code != http.StatusBadRequest {
			t.Errorf("create with %s: %d (token %v); want 400 and no token", name, rec.Code, tok != "")
		}
	}
	// CONTROL: a viewer-only token stores data scope none; messages+viewer keeps it.
	if tok, _, rec := f.create(f.a, viewerForm("viewer-ok", hoEntry)); tok == "" || !strings.Contains(rec.Body.String(), "scope none") {
		t.Fatalf("CONTROL: a valid viewer-only create: %d; want a token and scope none", rec.Code)
	}

	off := newHandoffSeam(t, "")
	page = off.do("GET", "/plugins", "", off.as(off.a, false)).Body.String()
	if strings.Contains(page, `data-testid="plugin-capabilities"`) || strings.Contains(page, `name="capability"`) {
		t.Fatal("with MUSTER_PLUGIN_ORIGINS unset, the form offers a capability choice")
	}
	if tok, _, rec := off.create(off.a, viewerForm("viewer-off", hoEntry)); tok != "" || rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "does not offer viewer plugins") {
		t.Fatalf("a viewer create with the variable unset: %d; want 400 saying viewers are not offered, and no token", rec.Code)
	}
	if tok, _, _ := off.create(off.a, url.Values{"name": {"messages-off"}, "label": {"Example inbox"}, "data_scope": {"example"}}); tok == "" {
		t.Fatal("CONTROL: a messages create with the variable unset was refused")
	}
	for _, rec := range []*httptest.ResponseRecorder{
		off.do("POST", pluginHandoffsPath, mintBody(1, "ws-a", "@3"), off.as(off.a, false)),
		off.redeem("mpl_"+strings.Repeat("Z", 43), strings.Repeat("A", 43)),
	} {
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"feature":"plugin-viewers"`) {
			t.Errorf("a hand-off route with the variable unset: %d %s; want feature_disabled naming plugin-viewers", rec.Code, rec.Body.String())
		}
	}
}

// TestAHandoffIsRefusedOnceTheTokensOriginIsNoLongerAllowed: a viewer token
// minted under a MUSTER_PLUGIN_ORIGINS pattern cannot mint a code after the
// admin removes that pattern — the code would send the owner's browser to a
// host the server no longer trusts (and no longer refuses with 421). Refused
// 403 naming why, and nothing is minted. The control: the same token mints
// while its pattern is still listed, beside another one.
func TestAHandoffIsRefusedOnceTheTokensOriginIsNoLongerAllowed(t *testing.T) {
	f := newHandoffSeam(t, hoZone)
	_, id, rec := f.create(f.a, viewerForm("viewer-a", hoEntry))
	if id == 0 {
		t.Fatalf("creating a viewer plugin: %d", rec.Code)
	}
	minted := testutil.ToFloat64(metrics.PluginHandoffs.WithLabelValues("view", "minted"))
	still, err := plugins.ParseOrigins("https://*.other.example.net," + hoZone)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.ext.PluginOrigins = still
	if rec, _ := f.mint(f.a, false, mintBody(id, "ws-a", "@3")); rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: its pattern still listed (beside another): %d %s; want 200", rec.Code, rec.Body.String())
	}
	removed, err := plugins.ParseOrigins("https://*.other.example.net")
	if err != nil {
		t.Fatal(err)
	}
	f.srv.ext.PluginOrigins = removed
	if rec, link := f.mint(f.a, false, mintBody(id, "ws-a", "@3")); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), PluginHandoffOriginRetired) || link != "" {
		t.Fatalf("its pattern removed: %d %s; want 403 saying the origin is no longer allowed, and no URL", rec.Code, rec.Body.String())
	}
	if got := testutil.ToFloat64(metrics.PluginHandoffs.WithLabelValues("view", "minted")) - minted; got != 1 {
		t.Fatalf("minted moved by %v; want 1 (the control's only)", got)
	}
}
