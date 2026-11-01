package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/apitokens"
)

// scopedTokens is fakeAPITokens with tokens of a chosen scope beside the full
// one, and a List and Revoke that COUNT, so a test can tell "refused at the
// door" from "the handler ran and refused".
type scopedTokens struct {
	*fakeAPITokens
	mu      sync.Mutex
	revokes int
	lists   int
}

func newScopedTokens() *scopedTokens { return &scopedTokens{fakeAPITokens: newFakeAPITokens()} }

// addScoped adds a token of scope for uid (add, the embedded one, adds a full
// token; addScoped names the scope either way, so each test says it).
func (s *scopedTokens) addScoped(t *testing.T, uid int64, scope string) string {
	t.Helper()
	tok := s.add(t, uid)
	s.fakeAPITokens.mu.Lock()
	defer s.fakeAPITokens.mu.Unlock()
	rec := s.byTok[tok]
	rec.Scope = scope
	s.byTok[tok] = rec
	return tok
}

func (s *scopedTokens) List(context.Context) ([]apitokens.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	return []apitokens.Token{}, nil
}

func (s *scopedTokens) Revoke(context.Context, int64) (bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokes++
	return true, true, nil
}

func (s *scopedTokens) counts() (lists, revokes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists, s.revokes
}

// scopeTestMux is the real route set (registerAll) over f.
func scopeTestMux(t *testing.T, f apitokens.Store) http.Handler {
	t.Helper()
	s := New(nil, AuthConfig{HookToken: tokTestHook}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: f, Users: stubUsers{}})
	mux := http.NewServeMux()
	s.registerAll(mux)
	return mux
}

func bearerDo(h http.Handler, method, path, tok string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// bearerReach is bearerDo for a request the door may ADMIT into a handler this
// fixture leaves unwired (no agents store, say): the handler's nil-store panic
// is reported as -1, "the handler ran" — admitted, which is what the full
// token's control wants and what a refused token must never get. Without it,
// a broken door panics the test binary and hides every later result.
func bearerReach(h http.Handler, method, path, tok string) (code int, body string) {
	defer func() {
		if recover() != nil {
			code, body = -1, "(the handler ran)"
		}
	}()
	rec := bearerDo(h, method, path, tok)
	return rec.Code, rec.Body.String()
}

// summaryTokenRefused is a spread of routes a personal token reaches that a
// bar-summary token must NOT: reads on the hook-tier door (an agent's
// messages, the task list, a task), the token API's own reads (the list,
// whoami), the flow API, and writes of every method.
var summaryTokenRefused = []struct{ method, path string }{
	{http.MethodGet, "/api/agents/chief/messages"},
	{http.MethodGet, "/api/agents"},
	{http.MethodGet, "/api/tasks"},
	{http.MethodGet, "/api/tasks/1"},
	{http.MethodGet, "/api/flows/jobs"},
	{http.MethodGet, "/api/whoami"},
	{http.MethodGet, apiTokensPath},
	{http.MethodDelete, apiTokensPath + "/1"},
	{http.MethodPost, "/api/tasks"},
	{http.MethodPatch, "/api/tasks/1/status"},
	{http.MethodPost, "/api/flows/jobs"},
	{http.MethodPost, "/api/agents/chief/messages"},
}

// TestABarSummaryTokenReachesTheSummaryAndNothingElse: a summary-scoped token
// gets GET (and HEAD) /api/attention/summary, and is refused 403 with the
// scope sentence on every other route in summaryTokenRefused — GETs on the
// hook-tier door included — BEFORE the handler runs (the store's List and
// Revoke are never called). The same owner's FULL token on the same requests
// is not refused (the 403 is the scope, not the route), and its list and
// revoke reach the store.
func TestABarSummaryTokenReachesTheSummaryAndNothingElse(t *testing.T) {
	f := newScopedTokens()
	full := f.addScoped(t, 7, apitokens.ScopeFull)
	bar := f.addScoped(t, 7, apitokens.ScopeSummary)
	h := scopeTestMux(t, f)
	if rec := bearerDo(h, http.MethodGet, attentionSummaryPath, bar); rec.Code != http.StatusOK {
		t.Fatalf("GET %s with a summary token: %d %s; want 200", attentionSummaryPath, rec.Code, rec.Body)
	}
	if rec := bearerDo(h, http.MethodHead, attentionSummaryPath, bar); rec.Code != http.StatusOK {
		t.Errorf("HEAD %s with a summary token: %d; want 200", attentionSummaryPath, rec.Code)
	}
	for _, c := range summaryTokenRefused {
		code, body := bearerReach(h, c.method, c.path, bar)
		var out map[string]any
		_ = json.Unmarshal([]byte(body), &out)
		if code != http.StatusForbidden || out["error"] != ScopedTokenRefusal {
			t.Errorf("%s %s with a summary token: %d %s; want 403 %q", c.method, c.path, code, truncate(body, 200), ScopedTokenRefusal)
		}
	}
	if lists, revokes := f.counts(); lists != 0 || revokes != 0 {
		t.Fatalf("a summary token reached the store's List %d / Revoke %d time(s); the door must refuse it first", lists, revokes)
	}
	for _, c := range summaryTokenRefused {
		if code, body := bearerReach(h, c.method, c.path, full); code == http.StatusForbidden || code == http.StatusUnauthorized {
			t.Errorf("CONTROL: %s %s with the FULL token: %d %s; a full token is unchanged", c.method, c.path, code, truncate(body, 200))
		}
	}
	if lists, revokes := f.counts(); lists != 1 || revokes != 1 {
		t.Fatalf("CONTROL: the full token reached List %d / Revoke %d time(s); want 1 and 1", lists, revokes)
	}
	if rec := bearerDo(h, http.MethodGet, attentionSummaryPath, full); rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: the full token on the summary: %d", rec.Code)
	}
}

// TestATokenOfAnUnknownScopeReachesNothing: a scope the allowlist does not
// name — a row from a newer image, a blank — is refused everywhere, the
// summary included. The door fails closed; only ScopeFull is unlimited.
func TestATokenOfAnUnknownScopeReachesNothing(t *testing.T) {
	f := newScopedTokens()
	for _, scope := range []string{"", "admin", "Summary"} {
		tok := f.addScoped(t, 7, scope)
		h := scopeTestMux(t, f)
		for _, c := range append([]struct{ method, path string }{{http.MethodGet, attentionSummaryPath}}, summaryTokenRefused[:3]...) {
			if code, _ := bearerReach(h, c.method, c.path, tok); code != http.StatusForbidden {
				t.Errorf("scope %q: %s %s: %d; want 403", scope, c.method, c.path, code)
			}
		}
	}
}

// TestTheScopeAllowlistIsExactlyTheSummaryRoute pins the WHOLE allowlist: one
// narrower scope, one route, and that route registered under exactly that
// pattern (it is in routes.golden). A pattern that drifted from the route's
// registration would lock the bar out, and a second entry is a decision this
// line makes visible.
func TestTheScopeAllowlistIsExactlyTheSummaryRoute(t *testing.T) {
	want := map[string]map[string]bool{"summary": {"GET /api/attention/summary": true}}
	if !reflect.DeepEqual(tokenScopeRoutes, want) {
		t.Fatalf("tokenScopeRoutes = %v; want %v", tokenScopeRoutes, want)
	}
	golden := map[string]bool{}
	for _, r := range readGolden(t) {
		golden[r] = true
	}
	for scope, routes := range tokenScopeRoutes {
		for r := range routes {
			if !golden[r] {
				t.Errorf("scope %q allows %q, which no route is registered as", scope, r)
			}
		}
	}
}

// mintScopeTokens records the scope each mint asked for, and lists one token
// of each scope.
type mintScopeTokens struct {
	apitokens.Store
	got *[]string
}

func (m mintScopeTokens) Create(_ context.Context, _ string, _ int64, scope string) (string, apitokens.Token, error) {
	*m.got = append(*m.got, scope)
	tok, _ := apitokens.New()
	return tok, apitokens.Token{ID: 1, Scope: scope}, nil
}

func (mintScopeTokens) List(context.Context) ([]apitokens.Token, error) {
	return []apitokens.Token{{ID: 2, Name: "bar", Scope: apitokens.ScopeSummary}, {ID: 3, Name: "cli", Scope: apitokens.ScopeFull}}, nil
}

// tokenPagePost posts form to the token page as the operator's session.
func tokenPageDo(t *testing.T, h http.Handler, s *Server, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, accountTokensPath, strings.NewReader(body))
	r.Host = "muster.test"
	r.Header.Set("Cookie", sessionCookieFor(s))
	r.Header.Set("Origin", "http://muster.test")
	if body != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestTheMintPageOffersABarSummaryToken: the form's access choice is "Full
// access" (CHECKED: a mint that leaves it alone is full, as before) or "Bar
// summary only"; each choice reaches the store as its scope; and the list
// names each token's scope.
func TestTheMintPageOffersABarSummaryToken(t *testing.T) {
	var got []string
	s := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "mint-scope-operator-pw"}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: mintScopeTokens{got: &got}, Users: stubUsers{}})
	mux := http.NewServeMux()
	s.registerAll(mux)
	page := tokenPageDo(t, mux, s, http.MethodGet, "").Body.String()
	for _, want := range []string{
		`<input type="radio" name="scope" value="full" checked class="h-5 w-5">` + "\nFull access</label>",
		`<input type="radio" name="scope" value="summary" class="h-5 w-5">` + "\nBar summary only (can read the Attention summary and nothing else)</label>",
		`data-token-scope="summary">Bar summary only · created`,
		`data-token-scope="full">Full access · created`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the token page does not carry %q", want)
		}
	}
	for _, form := range []url.Values{{"name": {"bar"}, "scope": {"summary"}}, {"name": {"cli"}, "scope": {"full"}}, {"name": {"old"}}} {
		if rec := tokenPageDo(t, mux, s, http.MethodPost, form.Encode()); rec.Code != http.StatusOK {
			t.Fatalf("mint %v: %d", form, rec.Code)
		}
	}
	if strings.Join(got, ",") != "summary,full," {
		t.Fatalf("the store was asked for scopes %q; want summary, full, and none (the store's full)", got)
	}
}

// TestAMintNamingAnUnknownScopeIsABadRequest: the store's ErrBadScope reaches
// the person as a 400 and no token.
func TestAMintNamingAnUnknownScopeIsABadRequest(t *testing.T) {
	s := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "mint-scope-operator-pw"}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: refusingTokens{err: apitokens.ErrBadScope}, Users: stubUsers{}})
	mux := http.NewServeMux()
	s.registerAll(mux)
	rec := tokenPageDo(t, mux, s, http.MethodPost, url.Values{"name": {"x"}, "scope": {"admin"}}.Encode())
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "minted-token") {
		t.Fatalf("a mint with scope admin: %d; want 400 and no token", rec.Code)
	}
}
