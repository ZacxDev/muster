package users

import (
	"context"
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestResetFactorsRefusesAnUnknownAccountAndAuditsARealOne: the recovery's
// database half writes nothing — no audit row — for an id with no account
// (ErrNotFound), and for a real one deletes its phones, clears its mark and
// writes one audit row counting both halves.
func TestResetFactorsRefusesAnUnknownAccountAndAuditsARealOne(t *testing.T) {
	st, pool := storeFor(t)
	ctx := context.Background()
	sys := authz.System(ctx)
	audits := func() int {
		var n int
		if err := pool.QueryRow(sys, `SELECT count(*) FROM audit_log WHERE action = 'user.factors_reset'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := st.ResetFactors(ctx, Actor{Via: "cli"}, 9_999_999, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown account: %v, want ErrNotFound", err)
	}
	if n := audits(); n != 0 {
		t.Fatalf("an unknown account was audited %d time(s)", n)
	}
	var id int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', 'lost everything') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(sys, `INSERT INTO devices (owner_id, name, app_version, token_sha256, stepup_pubkey, stepup_key_type, stepup_key_level, paired_signin_at)
		VALUES ($1, 'p', '1', $2, '\x01', 'biometric', 'strongbox', now())`, id, []byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.HasStepUpPhone(ctx, id); err != nil || !ok {
		t.Fatalf("fixture: no step-up phone (%v %v)", ok, err)
	}
	_ = st.MarkStrongFactor(ctx, id)
	phones, err := st.ResetFactors(ctx, Actor{Via: "cli"}, id, map[string]any{"passkeys_removed": 3, "password_set": true})
	if err != nil || phones != 1 {
		t.Fatalf("a real reset: %d phones, %v", phones, err)
	}
	if ok, _ := st.HasStepUpPhone(ctx, id); ok {
		t.Fatal("the phone was kept")
	}
	if had, _ := st.HadStrongFactor(ctx, id); had {
		t.Fatal("the mark was kept")
	}
	var detail map[string]any
	if err := pool.QueryRow(sys, `SELECT detail FROM audit_log WHERE action = 'user.factors_reset' AND object = $1`, UserObject(id)).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail["phones_removed"] != float64(1) || detail["passkeys_removed"] != float64(3) || detail["password_set"] != true || detail["sessions_ended"] != true {
		t.Fatalf("audit detail %v", detail)
	}
}

// TestBothResetsRevokeFlowRunnerTokens: a password reset (ResetCredentials)
// and the server recovery (ResetFactors) revoke the account's flow-runner
// tokens alike, and leave another account's alone.
func TestBothResetsRevokeFlowRunnerTokens(t *testing.T) {
	st, pool := storeFor(t)
	ctx := context.Background()
	sys := authz.System(ctx)
	runner := func(owner int64, tag string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(sys, `INSERT INTO flow_runner_tokens (owner_id, host, token_hash) VALUES ($1, $2, $3) RETURNING id`, owner, "host-"+tag, "runnerhash-"+tag).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	revoked := func(id int64) bool {
		t.Helper()
		var r bool
		if err := pool.QueryRow(sys, `SELECT revoked_at IS NOT NULL FROM flow_runner_tokens WHERE id = $1`, id).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	user := func() int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(sys, `INSERT INTO users (role) VALUES ('member') RETURNING id`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b, other := user(), user(), user()
	ra, rb, ro := runner(a, "a"), runner(b, "b"), runner(other, "other")
	t.Cleanup(func() {
		_, _ = pool.Exec(sys, `DELETE FROM flow_runner_tokens WHERE id = ANY($1)`, []int64{ra, rb, ro})
	})
	if _, err := st.ResetCredentials(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResetFactors(ctx, Actor{Via: "cli"}, b, nil); err != nil {
		t.Fatal(err)
	}
	if !revoked(ra) || !revoked(rb) {
		t.Fatalf("a password reset revoked its runner token: %v; the recovery: %v (want both)", revoked(ra), revoked(rb))
	}
	if revoked(ro) {
		t.Fatal("another account's runner token was revoked")
	}
}
