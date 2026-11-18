package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/githubapp"
)

// TestShipRESTWiresTheAppsCreation: the REST client buildShip gives a
// shipping officer's gates carries the App's Slug and Created readers, both
// answered from the App's own GET /app. Without Created, OfficerPRs reads the
// App's pull requests over the whole pulls list and refuses any repository
// with more than maxPages pages of them, so the bound would never apply live.
func TestShipRESTWiresTheAppsCreation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/app" {
			t.Errorf("asked %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"id":777,"slug":"fixture-ship-app-3","created_at":"2001-02-03T04:05:06Z"}`)
	}))
	defer srv.Close()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app, err := githubapp.New(githubapp.Config{AppID: 777, PrivateKey: k, BaseURL: srv.URL,
		Accounts: []githubapp.Account{{Login: "example-org", ID: 4200}}})
	if err != nil {
		t.Fatal(err)
	}
	rest := shipREST(app, shipRepo)
	if rest.Repo != shipRepo || rest.Slug == nil || rest.Created == nil {
		t.Fatalf("shipREST = repo %q, Slug set %v, Created set %v; want %q and both set", rest.Repo, rest.Slug != nil, rest.Created != nil, shipRepo)
	}
	if got, err := rest.Slug(context.Background()); err != nil || got != "fixture-ship-app-3" {
		t.Fatalf("Slug = %q, %v; want the App's", got, err)
	}
	got, err := rest.Created(context.Background())
	if want := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC); err != nil || !got.Equal(want) {
		t.Fatalf("Created = %v, %v; want the App's created_at %v", got, err, want)
	}
}
