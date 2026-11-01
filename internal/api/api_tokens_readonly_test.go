package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/apitokens"
)

// scopedTokens is fakeAPITokens with a read-only token beside the full one,
// and a List and Revoke that COUNT, so a test can tell "refused at the door"
// from "the handler ran and refused".
type scopedTokens struct {
	*fakeAPITokens
	mu      sync.Mutex
	revokes int
	lists   int
}

func (s *scopedTokens) addReadOnly(t *testing.T, uid int64) string {
	t.Helper()
	tok := s.add(t, uid)
	s.fakeAPITokens.mu.Lock()
	defer s.fakeAPITokens.mu.Unlock()
	rec := s.byTok[tok]
	rec.ReadOnly = true
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

func (s *scopedTokens) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists, s.revokes
}

// TestAReadOnlyTokenMayOnlyRead: a read-only token reaches GET routes (whoami,
// the token list, the attention summary) and is refused 403, with the
// read-only sentence, on every write — a token revoke (DELETE), a flow enqueue
// and cancel and a task create (POST), a task status (PATCH) — BEFORE the
// handler runs (the store's Revoke is never called). The same requests with
// the same owner's FULL token are not refused (the control: the 403 is the
// scope, not the route), and the full token's revoke reaches the store.
func TestAReadOnlyTokenMayOnlyRead(t *testing.T) {
	f := &scopedTokens{fakeAPITokens: newFakeAPITokens()}
	full := f.add(t, 7)
	ro := f.addReadOnly(t, 7)
	s := New(nil, AuthConfig{HookToken: tokTestHook}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: f, Users: stubUsers{}})
	mux := http.NewServeMux()
	s.registerAll(mux)
	do := func(method, path, tok string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}
	for _, path := range []string{"/api/whoami", apiTokensPath, attentionSummaryPath} {
		if rec := do(http.MethodGet, path, ro); rec.Code != http.StatusOK {
			t.Errorf("GET %s with a read-only token: %d %s; want 200", path, rec.Code, truncate(rec.Body.String(), 200))
		}
		if rec := do(http.MethodHead, path, ro); rec.Code == http.StatusForbidden {
			t.Errorf("HEAD %s with a read-only token: 403; HEAD is a read", path)
		}
	}
	_, body := whoami(t, mux, ro, "")
	if tk, _ := body["token"].(map[string]any); tk == nil || tk["readOnly"] != true {
		t.Errorf("whoami does not say the token is read-only: %v", body)
	}
	writes := []struct{ method, path string }{
		{http.MethodDelete, apiTokensPath + "/1"},
		{http.MethodPost, "/api/flows/jobs"},
		{http.MethodPost, "/api/flows/jobs/1/cancel"},
		{http.MethodPost, "/api/tasks"},
		{http.MethodPatch, "/api/tasks/1/status"},
	}
	for _, w := range writes {
		rec := do(w.method, w.path, ro)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusForbidden || out["error"] != ReadOnlyTokenRefusal {
			t.Errorf("%s %s with a read-only token: %d %s; want 403 %q", w.method, w.path, rec.Code, truncate(rec.Body.String(), 200), ReadOnlyTokenRefusal)
		}
	}
	if _, revokes := f.counts(); revokes != 0 {
		t.Fatalf("a read-only token's DELETE reached the store's Revoke %d time(s); the door must refuse it first", revokes)
	}
	for _, w := range writes {
		if rec := do(w.method, w.path, full); rec.Code == http.StatusForbidden {
			t.Errorf("CONTROL: %s %s with the FULL token: 403 %s; a full token is unchanged", w.method, w.path, truncate(rec.Body.String(), 200))
		}
	}
	if _, revokes := f.counts(); revokes != 1 {
		t.Fatalf("CONTROL: the full token's DELETE reached Revoke %d time(s); want 1", revokes)
	}
}

// mintScopeTokens records the scope each mint asked for, and lists one token
// of each scope.
type mintScopeTokens struct {
	apitokens.Store
	got *[]bool
}

func (m mintScopeTokens) Create(_ context.Context, _ string, _ int64, readOnly bool) (string, apitokens.Token, error) {
	*m.got = append(*m.got, readOnly)
	tok, _ := apitokens.New()
	return tok, apitokens.Token{ID: 1, ReadOnly: readOnly}, nil
}

func (mintScopeTokens) List(context.Context) ([]apitokens.Token, error) {
	return []apitokens.Token{{ID: 2, Name: "bar", ReadOnly: true}, {ID: 3, Name: "cli"}}, nil
}

// TestTheMintPageOffersAReadOnlyToken: the page's form has a read-only
// checkbox, UNCHECKED by default (a mint that leaves it alone is full access,
// as before); a mint with it ticked asks the store for a read-only token, one
// without for a full one; and the list says each token's scope.
func TestTheMintPageOffersAReadOnlyToken(t *testing.T) {
	var got []bool
	s := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "mint-scope-operator-pw"}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: mintScopeTokens{got: &got}, Users: stubUsers{}})
	mux := http.NewServeMux()
	s.registerAll(mux)
	do := func(method, body string) string {
		r := httptest.NewRequest(method, accountTokensPath, strings.NewReader(body))
		r.Host = "muster.test"
		r.Header.Set("Cookie", sessionCookieFor(s))
		r.Header.Set("Origin", "http://muster.test")
		if body != "" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d", method, accountTokensPath, rec.Code)
		}
		return rec.Body.String()
	}
	page := do(http.MethodGet, "")
	const box = `<input type="checkbox" id="token-read-only" name="read_only" value="1" class="h-5 w-5">`
	if !strings.Contains(page, box) {
		t.Fatalf("the token page has no unchecked read-only checkbox %q:\n%s", box, truncate(page, 1200))
	}
	for _, want := range []string{`data-token-scope="read-only">read-only · created`, `data-token-scope="full access">full access · created`} {
		if !strings.Contains(page, want) {
			t.Errorf("the token list does not say %q", want)
		}
	}
	do(http.MethodPost, url.Values{"name": {"bar"}, "read_only": {"1"}}.Encode())
	do(http.MethodPost, url.Values{"name": {"cli"}}.Encode())
	if len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("the store was asked for scopes %v; want [read-only, full]", got)
	}
}
