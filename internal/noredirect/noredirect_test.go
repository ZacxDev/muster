package noredirect

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// target counts every request it receives and remembers each Authorization
// header it saw.
type target struct {
	mu    sync.Mutex
	hits  int
	auths []string
}

func newTarget(t *testing.T) (*target, *httptest.Server) {
	t.Helper()
	tg := &target{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tg.mu.Lock()
		tg.hits++
		tg.auths = append(tg.auths, r.Header.Get("Authorization"))
		tg.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return tg, srv
}

func (tg *target) seen() (int, []string) {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return tg.hits, append([]string(nil), tg.auths...)
}

func redirector(t *testing.T, code int, to string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, to, code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAClientRefusesEveryRedirectBeforeTheTargetIsAsked: the redirect target
// (same hostname, another port — exactly where net/http would re-send the
// Authorization header) receives NO request, and the error names its host.
func TestAClientRefusesEveryRedirectBeforeTheTargetIsAsked(t *testing.T) {
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		tg, tsrv := newTarget(t)
		from := redirector(t, code, tsrv.URL+"/landing")
		req, _ := http.NewRequest(http.MethodGet, from.URL+"/x", nil)
		req.Header.Set("Authorization", "Bearer secret-for-test")
		resp, err := Client(nil).Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		var refused *Error
		if !errors.As(err, &refused) {
			t.Fatalf("%d: err = %v, want a *noredirect.Error", code, err)
		}
		want, _ := url.Parse(tsrv.URL)
		if refused.Host != want.Host {
			t.Fatalf("%d: refused host %q, want %q", code, refused.Host, want.Host)
		}
		if hits, auths := tg.seen(); hits != 0 || len(auths) != 0 {
			t.Fatalf("%d: the redirect target got %d requests (Authorization %q)", code, hits, auths)
		}
	}
}

// TestClientDoesNotChangeTheClientItCopies: the given client keeps its own
// CheckRedirect, and the copy keeps its Transport and Timeout.
func TestClientDoesNotChangeTheClientItCopies(t *testing.T) {
	tr := &http.Transport{}
	orig := &http.Client{Transport: tr, Timeout: 7}
	got := Client(orig)
	if orig.CheckRedirect != nil {
		t.Fatal("Client changed the client it was given")
	}
	if got == orig || got.Transport != tr || got.Timeout != 7 || got.CheckRedirect == nil {
		t.Fatalf("copy = %+v", got)
	}
}
