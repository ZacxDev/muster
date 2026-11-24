package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/devices"
	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/users"
)

// recoveryWorld is a sole admin with a GoTrue login, for the recovery command.
type recoveryWorld struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	pg    *users.PGStore
	ds    *devices.PGStore
	fake  *gotruetest.Fake
	id    int64
	sub   string
	email string
	env   map[string]string
	audit func(action string) int
}

func newRecoveryWorld(t *testing.T) *recoveryWorld {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	fake := gotruetest.New(t, []byte(accountsSecret))
	fake.EnablePasskeys()
	email := "recover-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.com"
	sub := fake.AddUser(email, "a-long-enough-password")
	sys := authz.System(ctx)
	var id int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name, email, gotrue_sub) VALUES ('admin', 'sole admin', $1, $2) RETURNING id`, email, sub).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = pool.Exec(bg, `DELETE FROM audit_log WHERE object = $1`, users.UserObject(id))
		_, _ = pool.Exec(bg, `DELETE FROM devices WHERE owner_id = $1`, id)
		_, _ = pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
	})
	w := &recoveryWorld{t: t, ctx: ctx, pool: pool, pg: users.NewPG(pool), ds: devices.NewPG(pool), fake: fake, id: id, sub: sub, email: email,
		env: map[string]string{envDatabase: dsn, envGoTrueURL: fake.URL, envGoTrueSecret: accountsSecret}}
	w.audit = func(action string) int {
		var n int
		if err := pool.QueryRow(sys, `SELECT count(*) FROM audit_log WHERE action = $1 AND object = $2`, action, users.UserObject(id)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return w
}

func (w *recoveryWorld) run(args ...string) (int, string) {
	var out, errb bytes.Buffer
	rc := runAccountCommand(w.ctx, append([]string{"user", "reset-factors"}, args...), func(k string) string { return w.env[k] }, &out, &errb)
	return rc, out.String() + errb.String()
}

// pairPhone pairs a phone with a step-up key for the account.
func (w *recoveryWorld) pairPhone() {
	w.t.Helper()
	owner := authz.WithPrincipal(w.ctx, authz.Principal{UserID: w.id, Role: authz.RoleAdmin, Via: authz.ViaSession})
	code, _, err := w.ds.CreatePairingCode(owner, time.Now())
	if err != nil {
		w.t.Fatal(err)
	}
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if _, _, err := w.ds.Redeem(context.Background(), code, devices.PairIn{Name: "lost phone", AppVersion: "0.2.0",
		StepUpKey: base64.StdEncoding.EncodeToString(der), KeyType: devices.KeyBiometric, KeyLevel: devices.LevelStrongBox}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *recoveryWorld) phones() int {
	w.t.Helper()
	owner := authz.WithPrincipal(w.ctx, authz.Principal{UserID: w.id, Role: authz.RoleAdmin, Via: authz.ViaSession})
	l, err := w.ds.List(owner)
	if err != nil {
		w.t.Fatal(err)
	}
	return len(l)
}

func (w *recoveryWorld) marked() bool {
	w.t.Helper()
	had, err := w.pg.HadStrongFactor(w.ctx, w.id)
	if err != nil {
		w.t.Fatal(err)
	}
	return had
}

// TestTheRecoveryCommandResetsEveryFactor: `muster-server user reset-factors
// <id|email>` — the recovery for a sole admin who lost every passkey and phone
// — removes every passkey (GoTrue's admin API) and their records, unpairs every
// phone, clears the sticky record (migration 0188) and writes one audit row,
// printing what it removed; by id and by email. It is a server command only:
// no HTTP route names it.
func TestTheRecoveryCommandResetsEveryFactor(t *testing.T) {
	w := newRecoveryWorld(t)
	w.fake.AddPasskey(w.sub, "lost-cred-1")
	w.fake.AddPasskey(w.sub, "lost-cred-2")
	c := &gotrue.Client{BaseURL: w.fake.URL, Secret: []byte(accountsSecret), Issuer: gotrueIssuer}
	if err := c.AdminSetAppMetadata(w.ctx, "test", w.sub, "muster_passkeys", map[string]any{"x": map[string]any{"vouched": true}}); err != nil {
		t.Fatal(err)
	}
	w.pairPhone()
	if err := w.pg.MarkStrongFactor(w.ctx, w.id); err != nil {
		t.Fatal(err)
	}
	rc, out := w.run(strconv.FormatInt(w.id, 10))
	if rc != 0 {
		t.Fatalf("reset-factors by id: rc %d %s", rc, out)
	}
	if !strings.Contains(out, "removed 2 passkey(s) and 1 paired phone(s)") {
		t.Fatalf("the command did not say what it removed: %s", out)
	}
	if n := len(w.fake.Passkeys(w.sub)); n != 0 {
		t.Fatalf("%d passkey(s) left", n)
	}
	if fu, _ := w.fake.User(w.sub); fu.AppMetadata["muster_passkeys"] != nil {
		t.Fatalf("the passkey records were kept: %v", fu.AppMetadata["muster_passkeys"])
	}
	if n := w.phones(); n != 0 {
		t.Fatalf("%d phone(s) left paired", n)
	}
	if w.marked() {
		t.Fatal("the account is still marked")
	}
	if n := w.audit("user.factors_reset"); n != 1 {
		t.Fatalf("audit rows: %d, want 1", n)
	}
	// By email, on an account with nothing left: still a reset, still audited.
	_ = w.pg.MarkStrongFactor(w.ctx, w.id)
	if rc, out := w.run(w.email); rc != 0 || !strings.Contains(out, "removed 0 passkey(s) and 0 paired phone(s)") {
		t.Fatalf("reset-factors by email: rc %d %s", rc, out)
	}
	if w.marked() || w.audit("user.factors_reset") != 2 {
		t.Fatal("the reset by email did not clear and audit")
	}
	golden, err := os.ReadFile("../../internal/api/testdata/routes.golden")
	if err != nil || len(golden) < 100 {
		t.Fatalf("CONTROL: the route ledger could not be read: %v", err)
	}
	for _, word := range []string{"strong", "factors"} {
		if strings.Contains(string(golden), word) {
			t.Fatalf("an HTTP route names %q", word)
		}
	}
}

// TestTheRecoveryCommandRefusesAnUnknownAccount: an unknown numeric id or
// email exits non-zero and writes nothing — no audit row anywhere.
func TestTheRecoveryCommandRefusesAnUnknownAccount(t *testing.T) {
	w := newRecoveryWorld(t)
	var before, after int
	sys := authz.System(w.ctx)
	count := func() int {
		var n int
		if err := w.pool.QueryRow(sys, `SELECT count(*) FROM audit_log WHERE action = 'user.factors_reset'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before = count()
	for _, who := range []string{strconv.FormatInt(w.id+1_000_000, 10), "nobody-" + w.email} {
		if rc, out := w.run(who); rc == 0 {
			t.Fatalf("an unknown account %s was reset: %s", who, out)
		}
	}
	after = count()
	if after != before {
		t.Fatalf("a refused reset wrote %d audit row(s)", after-before)
	}
	// The control: the same count moves on a real reset.
	if rc, out := w.run(strconv.FormatInt(w.id, 10)); rc != 0 || count() != before+1 {
		t.Fatalf("control: a real reset: rc %d %s", rc, out)
	}
}

// TestTheRecoveryCommandStopsWhilePasskeysMayRemain: GoTrue failing mid-way
// leaves the account gated — the mark and the phones untouched, nothing
// audited — and says to run it again.
func TestTheRecoveryCommandStopsWhilePasskeysMayRemain(t *testing.T) {
	w := newRecoveryWorld(t)
	w.fake.AddPasskey(w.sub, "kept-cred")
	w.pairPhone()
	_ = w.pg.MarkStrongFactor(w.ctx, w.id)
	w.fake.FailAdminPasskeys(10)
	rc, out := w.run(strconv.FormatInt(w.id, 10))
	w.fake.FailAdminPasskeys(0)
	if rc == 0 || !strings.Contains(out, "still gated") {
		t.Fatalf("a GoTrue failure: rc %d %s", rc, out)
	}
	if !w.marked() || w.phones() != 1 || w.audit("user.factors_reset") != 0 {
		t.Fatalf("a failed recovery changed the account (marked %v, phones %d)", w.marked(), w.phones())
	}
}
