package credentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

func as(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

func pgStore(t *testing.T) (*PGStore, *pgxpool.Pool) {
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
	s, err := NewPG(pool, bytes.Repeat([]byte{7}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return s, pool
}

const (
	tokA  = "sk-ant-oat01-FAKE-store-A-0123456789"
	tokB  = "sk-ant-oat01-FAKE-store-B-9876543210"
	orA   = "sk-or-v1-FAKE-api-key-of-A-000000AAA1"
	orA2  = "sk-or-v1-FAKE-api-key-of-A-000000AAA2"
	orB   = "sk-or-v1-FAKE-api-key-of-B-000000BBB1"
)

// TestACiphertextMovedToAnotherOwnerDoesNotOpen: user A's sealed setup-token,
// copied byte for byte into user B's row (the shape of a bad UPDATE or a
// restore into the wrong place), does not open for B — the owner is in the
// associated data — while the same bytes still open in A's row.
func TestACiphertextMovedToAnotherOwnerDoesNotOpen(t *testing.T) {
	s, pool := pgStore(t)
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
	s, pool := pgStore(t)
	sys := authz.System(context.Background())
	if err := s.PutClaude(as(41), "work", tokA); err != nil {
		t.Fatal(err)
	}
	if err := s.PutOpenRouter(as(41), orA); err != nil {
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
	if dump := fmt.Sprintf("%+v", l); strings.Contains(dump, tokA) || strings.Contains(dump, orA) {
		t.Fatalf("List carries a secret: %s", dump)
	}
	var row string
	if err := pool.QueryRow(sys, `SELECT coalesce(json_agg(t)::text, '') FROM user_credentials t`).Scan(&row); err != nil {
		t.Fatal(err)
	}
	for _, sec := range []string{tokA, tokB, orA} {
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

// TestTheOpenRouterKeyIsUsedAsEnteredPerOwner: an ordinary API key is sealed
// and handed back AS ENTERED for its owner's agents; replacing it replaces it;
// another owner's key is a different row; a ciphertext moved to another owner's
// row does not open; a Claude-shaped value is refused; delete removes it.
func TestTheOpenRouterKeyIsUsedAsEnteredPerOwner(t *testing.T) {
	s, pool := pgStore(t)
	sys := authz.System(context.Background())
	if err := s.PutOpenRouter(as(41), orA); err != nil {
		t.Fatal(err)
	}
	if err := s.PutOpenRouter(as(42), orB); err != nil {
		t.Fatal(err)
	}
	if k, ok, err := s.OpenRouterKey(sys, 41); err != nil || !ok || k != orA {
		t.Fatalf("A's agents' key: right=%v ok=%v err=%v; want A's key as entered", k == orA, ok, err)
	}
	if err := s.PutOpenRouter(as(41), orA2); err != nil {
		t.Fatal(err)
	}
	if k, _, _ := s.OpenRouterKey(sys, 41); k != orA2 {
		t.Fatalf("after replace: A's key is not the new one")
	}
	if k, _, _ := s.OpenRouterKey(sys, 42); k != orB {
		t.Fatalf("A's replace touched B's key")
	}
	if has, err := s.HasOpenRouterKey(as(42), 41); err != nil || has {
		t.Fatalf("B's principal sees that A has a key: %v %v", has, err)
	}
	if _, err := pool.Exec(sys, `UPDATE user_credentials SET secret_ciphertext =
		(SELECT secret_ciphertext FROM user_credentials WHERE owner_id = 41 AND kind = 'openrouter')
		WHERE owner_id = 42 AND kind = 'openrouter'`); err != nil {
		t.Fatal(err)
	}
	if k, _, err := s.OpenRouterKey(sys, 42); !errors.Is(err, ErrDecrypt) || k != "" {
		t.Fatalf("B's row holding A's ciphertext opened (got A's key: %v), err %v", k == orA2, err)
	}
	if err := s.PutOpenRouter(as(41), "sk-ant-oat01-not-an-openrouter-key-0123"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a Claude-shaped value as an OpenRouter key: %v", err)
	}
	if ok, err := s.Delete(as(41), KindOpenRouter, ""); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, ok, _ := s.OpenRouterKey(sys, 41); ok {
		t.Fatal("a deleted key is still served")
	}
}
