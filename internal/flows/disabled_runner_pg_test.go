package flows

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// TestADisabledPersonsRunnerTokenStaysDeadAfterAReEnable: a disable revokes
// the person's flow runner tokens in its own transaction, so a re-enable
// brings none of them back (the rule above users.SetDisabled: an enable
// restores nothing). The token works before the disable (the control), is
// refused while disabled, is STILL refused once the person is enabled again,
// and a token the person mints after the enable works.
func TestADisabledPersonsRunnerTokenStaysDeadAfterAReEnable(t *testing.T) {
	s, pool := pgStore(t)
	sys := authz.System(context.Background())
	// An explicit id: this package's fixture plants users 2-4 by id
	// (OVERRIDING SYSTEM VALUE), so the identity sequence would collide.
	const uid int64 = 900681
	if _, err := pool.Exec(sys, `INSERT INTO users (id, role, display_name) OVERRIDING SYSTEM VALUE VALUES ($1, 'member', $2)`,
		uid, "runner-disabled-"+strconv.FormatInt(time.Now().UnixNano(), 36)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(sys, `DELETE FROM flow_runner_tokens WHERE owner_id = $1`, uid)
		_, _ = pool.Exec(sys, `DELETE FROM users WHERE id = $1`, uid)
	})
	tok, _, err := s.CreateRunnerToken(session(uid), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateRunner(sys, tok); err != nil {
		t.Fatalf("control: an enabled person's runner token: %v", err)
	}
	accounts := users.NewPG(pool)
	if _, err := accounts.SetDisabled(context.Background(), uid, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := s.AuthenticateRunner(sys, tok); !errors.Is(err, ErrBadRunnerToken) {
		t.Fatalf("a disabled person's runner token: %v, want ErrBadRunnerToken", err)
	}
	if _, err := accounts.SetDisabled(context.Background(), uid, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got, err := s.AuthenticateRunner(sys, tok); !errors.Is(err, ErrBadRunnerToken) {
		t.Fatalf("re-enable brought the OLD runner token back (%+v, %v): a disable must revoke it", got, err)
	}
	fresh, _, err := s.CreateRunnerToken(session(uid), "laptop")
	if err != nil {
		t.Fatalf("a re-enabled person could not mint a runner token: %v", err)
	}
	if _, err := s.AuthenticateRunner(sys, fresh); err != nil {
		t.Fatalf("a re-enabled person's NEW runner token: %v", err)
	}
}
