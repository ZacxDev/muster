package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/users"
)

// TestAServerCodeCarriesTheGoTrueSessionsSignInNotItsRefresh (audit round 1):
// in GOTRUE mode the minting session's sign-in time is GoTrue's amr timestamp
// (stepup.AuthTime), and it rides HandoffIn.SignedInAt into the code's
// signed_in_at and, at the redeem, the grant's signin_at. A REFRESH of the
// session mints new tokens but keeps the amr, so a code minted after it
// carries the same sign-in, never the refresh's time. A session whose token
// carries no amr is asked to sign in again (401) and mints nothing.
func TestAServerCodeCarriesTheGoTrueSessionsSignInNotItsRefresh(t *testing.T) {
	var pool *pgxpool.Pool
	var store *plugins.PGStore
	pats, _ := plugins.ParseOrigins(hoZone)
	r := newDeviceRig(t, func(e *Extensions, p *pgxpool.Pool) {
		pool, store = p, plugins.NewPG(p)
		e.Plugins, e.PluginOrigins = store, pats
	})
	sys := authz.System(context.Background())
	tok, app, err := store.InstallServerApp(sys, plugins.ServerAppIn{Name: "gt-meet-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		AppLabel: "Cobalt Hall", AppIcon: "video", Origin: hoZone, EntryURL: hoEntry, Origins: pats}, users.Actor{Via: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM plugin_server_users WHERE plugin_id = $1`, `DELETE FROM plugin_handoffs WHERE server_plugin_id = $1`,
			`DELETE FROM audit_log WHERE object = 'plugin:' || $1::text`, `DELETE FROM plugins WHERE id = $1`} {
			_, _ = pool.Exec(bg, q, app.ID)
		}
	})
	signedIn := time.Now().Add(-90 * time.Minute).Truncate(time.Second)
	session := func(amr gotrue.AMR) (string, string) {
		_, rt, sid := r.fake.Mint(r.subject())
		r.fake.SetSessionAMR(sid, amr)
		sess, err := r.fake.Client().Refresh(context.Background(), "203.0.113.9", rt)
		if err != nil {
			t.Fatal(err)
		}
		return sess.AccessToken, sess.RefreshToken
	}
	mint := func(at, rt string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", pluginHandoffsPath, strings.NewReader(`{"plugin":`+strconv.FormatInt(app.ID, 10)+`,"target":{"app":{}}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://"+req.Host)
		withCookies(at, rt)(req)
		w := httptest.NewRecorder()
		r.edge.ServeHTTP(w, req)
		return w
	}
	codeSignin := func(w *httptest.ResponseRecorder) (string, time.Time) {
		t.Helper()
		var out struct {
			URL string `json:"url"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("mint: %d %s", w.Code, w.Body.String())
		}
		u, _ := url.Parse(out.URL)
		code := u.Query().Get("muster_code")
		var at time.Time
		if err := pool.QueryRow(sys, `SELECT signed_in_at FROM plugin_handoffs WHERE code_hash = $1`, plugins.HashToken(code)).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return code, at
	}

	at, rt := session(gotrue.AMR{{Method: "password", Timestamp: signedIn.Unix()}})
	code, got := codeSignin(mint(at, rt))
	if !got.Equal(signedIn) {
		t.Fatalf("the code's signed_in_at = %v; want the session's amr sign-in %v", got, signedIn)
	}
	// A refresh: new tokens, the same sign-in.
	time.Sleep(1100 * time.Millisecond)
	sess, err := r.fake.Client().Refresh(context.Background(), "203.0.113.9", rt)
	if err != nil {
		t.Fatal(err)
	}
	if sess.AccessToken == at {
		t.Fatal("fixture: the refresh returned the same access token")
	}
	_, got2 := codeSignin(mint(sess.AccessToken, sess.RefreshToken))
	if !got2.Equal(signedIn) {
		t.Errorf("after a refresh the code's signed_in_at = %v; want the unchanged sign-in %v", got2, signedIn)
	}
	// The redeem copies it into the grant.
	req := httptest.NewRequest("POST", pluginHandoffRedeemPath, strings.NewReader(`{"code":"`+code+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.mach.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("redeem: %d %s", w.Code, w.Body.String())
	}
	var grant time.Time
	if err := pool.QueryRow(sys, `SELECT signin_at FROM plugin_server_users WHERE plugin_id = $1 AND owner_id = $2`, app.ID, r.uid).Scan(&grant); err != nil || !grant.Equal(signedIn) {
		t.Errorf("the grant's signin_at = %v (%v); want %v", grant, err, signedIn)
	}
	// No amr: sign in again, nothing minted.
	at, rt = session(nil)
	if w := mint(at, rt); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "records no sign-in time") {
		t.Errorf("a session with no amr: %d %s; want 401", w.Code, w.Body.String())
	}
}
