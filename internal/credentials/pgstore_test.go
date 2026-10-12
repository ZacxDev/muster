package credentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// fakeMinter records every mint and revoke. Keys it mints are distinct and
// carry the management key's last 4 so a test can tell whose key minted what.
type fakeMinter struct {
	mu      sync.Mutex
	n       int
	minted  []string // "mgmt-suffix|ref"
	revoked []string // "mgmt-suffix|ref"
	fail    error
}

func suffix(s string) string { return s[len(s)-4:] }

func (m *fakeMinter) Mint(_ context.Context, mgmt, _ string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return "", "", m.fail
	}
	m.n++
	ref := fmt.Sprintf("hash-%d", m.n)
	m.minted = append(m.minted, suffix(mgmt)+"|"+ref)
	return fmt.Sprintf("sk-or-v1-FAKE-minted-%d-from-%s", m.n, suffix(mgmt)), ref, nil
}

func (m *fakeMinter) Revoke(_ context.Context, mgmt, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revoked = append(m.revoked, suffix(mgmt)+"|"+ref)
	return nil
}

func as(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

func pgStore(t *testing.T) (*PGStore, *pgxpool.Pool, *fakeMinter, *bytes.Buffer) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{41, 42} {
		dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES ($1, 'member') ON CONFLICT (id) DO NOTHING`, id)
	}
	dbtest.OwnerExec(t, dsn, `DELETE FROM user_credentials`)
	m := &fakeMinter{}
	var logs bytes.Buffer
	s, err := NewPG(pool, bytes.Repeat([]byte{7}, KeySize), m, log.New(&logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return s, pool, m, &logs
}

const (
	tokA  = "sk-ant-oat01-FAKE-store-A-0123456789"
	tokB  = "sk-ant-oat01-FAKE-store-B-9876543210"
	mgmtA = "sk-or-v1-FAKE-management-A-key-AAA1"
	mgmtB = "sk-or-v1-FAKE-management-A-key-AAA2"
)

// TestACiphertextMovedToAnotherOwnerDoesNotOpen: user A's sealed setup-token,
// copied byte for byte into user B's row (the shape of a bad UPDATE or a
// restore into the wrong place), does not open for B — the owner is in the
// associated data — while the same bytes still open in A's row.
func TestACiphertextMovedToAnotherOwnerDoesNotOpen(t *testing.T) {
	s, pool, _, _ := pgStore(t)
	sys := authz.System(context.Background())
	if err := s.PutClaude(as(41), "work", tokA); err != nil {
		t.Fatal(err)
	}
	if err := s.PutClaude(as(42), "work", tokB); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(sys, `UPDATE user_credentials SET secret_ciphertext =
		(SELECT secret_ciphertext FROM user_credentials WHERE owner_id = 41 AND kind = 'claude' AND name = 'work')
		WHERE owner_id = 42 AND kind = 'claude' AND name = 'work'`); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaudeTokens(sys, 42)
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("B's row holding A's ciphertext opened: err %v, got A's token: %v", err, got["work"] == tokA)
	}
	if strings.Contains(err.Error(), tokA) || strings.Contains(err.Error(), tokB) {
		t.Fatalf("the refusal carries a token: %v", err)
	}
	// The control: the same bytes open in A's own row.
	if got, err := s.ClaudeTokens(sys, 41); err != nil || got["work"] != tokA {
		t.Fatalf("control: A's own row: %v", err)
	}
}

// TestTheStoreIsPerOwnerAndNeverReturnsASecret: B cannot list, open, replace or
// delete A's credentials (a same-named one is B's own); List carries no secret;
// writes need a user principal; the stored row holds no plaintext.
func TestTheStoreIsPerOwnerAndNeverReturnsASecret(t *testing.T) {
	s, pool, _, _ := pgStore(t)
	sys := authz.System(context.Background())
	if err := s.PutClaude(as(41), "work", tokA); err != nil {
		t.Fatal(err)
	}
	if err := s.PutOpenRouter(as(41), mgmtA); err != nil {
		t.Fatal(err)
	}
	// B's view: nothing.
	if l, err := s.List(as(42)); err != nil || len(l) != 0 {
		t.Fatalf("B lists %v, %v", l, err)
	}
	if ok, err := s.Delete(as(42), KindClaude, "work"); err != nil || ok {
		t.Fatalf("B deleted A's work: %v %v", ok, err)
	}
	if ok, err := s.Delete(as(42), KindOpenRouter, ""); err != nil || ok {
		t.Fatalf("B deleted A's OpenRouter key: %v %v", ok, err)
	}
	// Under B's principal, A's rows are invisible even by owner id (RLS).
	if got, err := s.ClaudeTokens(as(42), 41); err != nil || len(got) != 0 {
		t.Fatalf("B's principal opened A's tokens: %d, %v", len(got), err)
	}
	if k, ok, err := s.OpenRouterKey(as(42), 41); ok || k != "" || err != nil {
		t.Fatalf("B's principal opened A's OpenRouter key: %v %v", ok, err)
	}
	// B's own "work" is B's.
	if err := s.PutClaude(as(42), "work", tokB); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ClaudeTokens(sys, 41); got["work"] != tokA {
		t.Fatal("B's write of a same-named account replaced A's")
	}
	// A's view: both, and no secret anywhere in it.
	l, err := s.List(as(41))
	if err != nil || len(l) != 2 {
		t.Fatalf("A lists %+v, %v", l, err)
	}
	if dump := fmt.Sprintf("%+v", l); strings.Contains(dump, tokA) || strings.Contains(dump, mgmtA) || strings.Contains(dump, "minted") {
		t.Fatalf("List carries a secret: %s", dump)
	}
	var row string
	if err := pool.QueryRow(sys, `SELECT coalesce(json_agg(t)::text, '') FROM user_credentials t`).Scan(&row); err != nil {
		t.Fatal(err)
	}
	for _, sec := range []string{tokA, tokB, mgmtA, "FAKE-minted"} {
		if strings.Contains(row, sec) || strings.Contains(row, strings.TrimPrefix(sec, "sk-")) {
			t.Fatalf("a stored row holds plaintext %q", sec)
		}
	}
	// No principal / System cannot write.
	for name, ctx := range map[string]context.Context{"none": context.Background(), "system": sys} {
		if err := s.PutClaude(ctx, "x", tokA); !errors.Is(err, errNoOwner) {
			t.Errorf("%s wrote a credential: %v", name, err)
		}
	}
	if ok, err := s.Delete(as(41), KindClaude, "work"); err != nil || !ok {
		t.Fatalf("control: A deleting its own: %v %v", ok, err)
	}
}

// TestOpenRouterMintsTheAgentsKeyAndRevokesTheOldOne: saving a management key
// mints the inference key agents get (and it is THAT key OpenRouterKey
// returns, not the management key); replacing it revokes the old minted key
// with the OLD management key; deleting revokes the current one; a failed mint
// stores nothing.
func TestOpenRouterMintsTheAgentsKeyAndRevokesTheOldOne(t *testing.T) {
	s, _, m, logs := pgStore(t)
	sys := authz.System(context.Background())
	if err := s.PutOpenRouter(as(41), mgmtA); err != nil {
		t.Fatal(err)
	}
	k, ok, err := s.OpenRouterKey(sys, 41)
	if err != nil || !ok || k != "sk-or-v1-FAKE-minted-1-from-AAA1" {
		t.Fatalf("A's agents' key = %q %v %v; want the MINTED key, not the management key", k, ok, err)
	}
	if err := s.PutOpenRouter(as(41), mgmtB); err != nil {
		t.Fatal(err)
	}
	if k, _, _ := s.OpenRouterKey(sys, 41); k != "sk-or-v1-FAKE-minted-2-from-AAA2" {
		t.Fatalf("after replace: %q", k)
	}
	if strings.Join(m.revoked, ",") != "AAA1|hash-1" {
		t.Fatalf("revoked %v; want the first minted key, with the first management key", m.revoked)
	}
	if ok, err := s.Delete(as(41), KindOpenRouter, ""); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if strings.Join(m.revoked, ",") != "AAA1|hash-1,AAA2|hash-2" {
		t.Fatalf("revoked %v after delete", m.revoked)
	}
	m.fail = errors.New("OpenRouter refused")
	if err := s.PutOpenRouter(as(41), mgmtA); err == nil {
		t.Fatal("a failed mint was accepted")
	}
	if _, ok, _ := s.OpenRouterKey(sys, 41); ok {
		t.Fatal("a failed mint stored a key")
	}
	if err := s.PutOpenRouter(as(41), "sk-ant-oat01-not-an-openrouter-key"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a Claude-shaped value as an OpenRouter key: %v", err)
	}
	for _, sec := range []string{mgmtA, mgmtB, "minted"} {
		if strings.Contains(logs.String(), sec) {
			t.Fatalf("the log carries a secret: %s", logs)
		}
	}
}
