package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/users"
)

// TestPluginCapabilitiesThroughTheRealStore is the seam the fake-store tests
// cannot see: a token whose ROW in Postgres holds {viewer} is refused 403 on
// POST /plugin/v1/messages, and stores nothing, while a token minted the
// ordinary way (0102's {messages} default) is accepted; and GET
// /plugin/v1/self answers each token's own row and owner as the database has
// them.
func TestPluginCapabilitiesThroughTheRealStore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	dsn := dbtest.DSN(t)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sys := authz.System(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	email := "pcap-" + suffix + "@example.invalid"
	var owner int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role, email, display_name) VALUES ('member', $1, 'Caps Owner') RETURNING id`,
		email).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM plugins WHERE owner_id = $1`, `DELETE FROM audit_log WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`} {
			_, _ = pool.Exec(authz.System(context.Background()), q, owner)
		}
	})
	store := plugins.NewPG(pool)
	asOwner := authz.WithPrincipal(ctx, authz.Principal{UserID: owner, Role: authz.RoleMember, Via: authz.ViaSession})
	msgTok, msgPlugin, err := store.Create(asOwner, "caps-messages", "Example inbox", "example")
	if err != nil {
		t.Fatal(err)
	}
	viewTok, viewPlugin, err := store.Create(asOwner, "caps-viewer", "Example viewer", "example")
	if err != nil {
		t.Fatal(err)
	}
	dbtest.OwnerExec(t, dsn, `UPDATE plugins SET capabilities = '{viewer}' WHERE id = $1`, viewPlugin.ID)

	srv := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "plugin-caps-operator-pw"}, log.New(io.Discard, "", 0))
	srv.UseExtensions(Extensions{Users: users.NewPG(pool), Plugins: store})
	mux := http.NewServeMux()
	srv.registerAll(mux)
	call := func(method, path, tok, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "muster.test"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}
	stored := func() int {
		var n int
		if err := pool.QueryRow(sys, `SELECT count(*) FROM plugin_messages WHERE owner_id = $1`, owner).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if rec := call("POST", "/plugin/v1/messages", viewTok, pgnMsg); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), `does not have the \"messages\" capability`) {
		t.Fatalf("a {viewer} row's token on POST /plugin/v1/messages: %d %q; want 403", rec.Code, rec.Body.String())
	}
	if n := stored(); n != 0 {
		t.Fatalf("the refused post stored %d messages; want 0", n)
	}
	if rec := call("POST", "/plugin/v1/messages", msgTok, pgnMsg); rec.Code != http.StatusAccepted {
		t.Fatalf("CONTROL: a default-minted token on POST /plugin/v1/messages: %d %q; want 202", rec.Code, rec.Body.String())
	}
	if n := stored(); n != 1 {
		t.Fatalf("CONTROL: the accepted post stored %d messages; want 1", n)
	}

	for tok, want := range map[string]string{
		msgTok: `{"plugin_id":` + strconv.FormatInt(msgPlugin.ID, 10) + `,"plugin_name":"caps-messages","owner_id":` +
			strconv.FormatInt(owner, 10) + `,"owner_login":"` + email + `","capabilities":["messages"]}`,
		viewTok: `{"plugin_id":` + strconv.FormatInt(viewPlugin.ID, 10) + `,"plugin_name":"caps-viewer","owner_id":` +
			strconv.FormatInt(owner, 10) + `,"owner_login":"` + email + `","capabilities":["viewer"]}`,
	} {
		rec := call("GET", "/plugin/v1/self", tok, "")
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("GET /plugin/v1/self: %d\n got %s\nwant %s", rec.Code, strings.TrimSpace(rec.Body.String()), want)
		}
	}
	// Revoked: the door's 401, which is how the gateway learns of it.
	if _, err := store.Revoke(asOwner, viewPlugin.ID); err != nil {
		t.Fatal(err)
	}
	if rec := call("GET", "/plugin/v1/self", viewTok, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /plugin/v1/self with a revoked token: %d; want 401", rec.Code)
	}
}
