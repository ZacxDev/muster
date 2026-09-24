package github

import (
	"net/url"
	"testing"
)

func TestAuthorizeURL(t *testing.T) {
	raw := AuthorizeURL("client-abc", "https://muster.example/cb?x=1", "state-xyz")

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("AuthorizeURL produced an unparseable URL %q: %v", raw, err)
	}
	if u.Scheme != "https" || u.Host != "github.com" || u.Path != "/login/oauth/authorize" {
		t.Errorf("endpoint = %q, want https://github.com/login/oauth/authorize", u.Scheme+"://"+u.Host+u.Path)
	}
	q := u.Query()
	// Values must be present and correctly URL-encoded (the redirect carries a query
	// string of its own, which must survive encoding intact).
	for k, want := range map[string]string{
		"client_id":    "client-abc",
		"redirect_uri": "https://muster.example/cb?x=1",
		"scope":        "repo",
		"state":        "state-xyz",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("query %q = %q, want %q", k, got, want)
		}
	}
}
