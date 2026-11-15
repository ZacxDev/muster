package mirror_test

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/users"
)

// #681: A DISABLED PERSON'S MIRROR TOKENS STOP WORKING — at /mirror/connect,
// at the ask hook's door (Service.AuthenticateHost), and on a connection that
// was already open when the account was disabled.

// disabledOwnerPG is a real Postgres store plus one fresh person (a member, so
// a disable is never refused as the last admin), whose tokens and row the
// cleanup removes.
func disabledOwnerPG(t *testing.T) (*mirror.PGStore, *pgxpool.Pool, string, int64) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	var uid int64
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"mirror-disabled-"+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `DELETE FROM mirror_tokens WHERE owner_id = $1`, uid)
		dbtest.OwnerExec(t, dsn, `DELETE FROM users WHERE id = $1`, uid)
	})
	return mirror.NewPG(pool), pool, dsn, uid
}

// asMember is user uid's own principal: CreateToken mints a token it owns.
func asMember(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

// pgRig is a rig (a real Service behind a real HTTP server, a real
// muster-mirror client) over the REAL store, rechecking fast.
func pgRig(t *testing.T, store mirror.Store) *rig {
	t.Helper()
	svc := mirror.NewService(store, log.New(io.Discard, "", 0))
	svc.SetTokenRecheck(20 * time.Millisecond)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+mirror.ConnectPath, svc.ServeConnect)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &rig{svc: svc, srv: srv}
}

// askDoor is the ask hook's door answering token: the status it wrote, or
// 200 when it admitted the token.
func askDoor(svc *mirror.Service, token string) int {
	r := httptest.NewRequest(http.MethodPost, "/mirror/ask", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	if _, _, ok := svc.AuthenticateHost(w, r); ok {
		return http.StatusOK
	}
	return w.Code
}

// TestDisablingAPersonDropsTheirConnectedHostAndRefusesItsToken is #681's
// closing condition: a user whose mirror host is CONNECTED is disabled; the
// live connection is dropped, and a reconnect with the same token is
// refused, as is the ask hook's door. Then the account is re-enabled: the OLD
// token stays dead (a disable revokes it; an enable restores nothing), and a
// NEW token the person mints connects.
func TestDisablingAPersonDropsTheirConnectedHostAndRefusesItsToken(t *testing.T) {
	store, pool, _, uid := disabledOwnerPG(t)
	tok, _, err := store.CreateToken(asMember(uid), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	r := pgRig(t, store)
	b := &fakeBackend{}
	b.set("main")
	_, done := r.connect(t, "laptop", tok, b, false)
	r.online(t, uid, "laptop", "main")
	if code := askDoor(r.svc, tok); code != http.StatusOK {
		t.Fatalf("control: the ask door refused an enabled person's token: %d", code)
	}

	accounts := users.NewPG(pool)
	if _, err := accounts.SetDisabled(context.Background(), uid, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("#681: the disabled person's host connection is still open 5s after the disable")
	}
	eventually(t, "the disabled person's host leaves the hub", func() bool { return !r.svc.Hub().Online(uid, "laptop", "main") })
	if _, err := r.rawConnect(t, tok, hello("laptop", false, "main")); err == nil {
		t.Fatal("#681: a reconnect with the disabled person's token was admitted")
	}
	if code := askDoor(r.svc, tok); code != http.StatusUnauthorized {
		t.Fatalf("#681: the ask door answered the disabled person's token %d, want 401", code)
	}

	if _, err := accounts.SetDisabled(context.Background(), uid, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := r.rawConnect(t, tok, hello("laptop", false, "main")); err == nil {
		t.Fatal("re-enable brought the OLD mirror token back: a disable must revoke it (an enable restores nothing)")
	}
	fresh, _, err := store.CreateToken(asMember(uid), "laptop")
	if err != nil {
		t.Fatalf("a re-enabled person could not mint a new token: %v", err)
	}
	r.connect(t, "laptop", fresh, b, false)
	r.online(t, uid, "laptop", "main")
}

// TestADisabledOwnersTokenIsRefusedWithoutARevoke pins the READ, separately
// from the disable's revoke: disabled_at is set on the row directly, so no
// token is revoked. Authenticate refuses the token and TokenLive (the live
// connection's recheck) calls it dead; clearing disabled_at — the control —
// makes both answer live again, so the refusal is the owner rule, not the
// token's own state.
func TestADisabledOwnersTokenIsRefusedWithoutARevoke(t *testing.T) {
	store, _, dsn, uid := disabledOwnerPG(t)
	sys := authz.System(context.Background())
	tok, rec, err := store.CreateToken(asMember(uid), "ws")
	if err != nil {
		t.Fatal(err)
	}
	check := func(wantLive bool, when string) {
		t.Helper()
		got, err := store.Authenticate(sys, tok)
		if wantLive && (err != nil || got.ID != rec.ID) {
			t.Fatalf("%s: Authenticate = %+v, %v; want token %d", when, got, err, rec.ID)
		}
		if !wantLive && !errors.Is(err, mirror.ErrBadToken) {
			t.Fatalf("%s: Authenticate admitted a DISABLED owner's token: %+v, %v; want ErrBadToken", when, got, err)
		}
		live, err := store.TokenLive(sys, rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if live != wantLive {
			t.Fatalf("%s: TokenLive = %v, want %v (a live connection's recheck must apply the owner rule)", when, live, wantLive)
		}
	}
	check(true, "enabled")
	dbtest.OwnerExec(t, dsn, `UPDATE users SET disabled_at = now() WHERE id = $1`, uid)
	check(false, "disabled, no revoke")
	dbtest.OwnerExec(t, dsn, `UPDATE users SET disabled_at = NULL WHERE id = $1`, uid)
	check(true, "enabled again (control)")
}
