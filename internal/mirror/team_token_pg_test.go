package mirror

import (
	"context"
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestATeamOwnedMirrorTokenIsRefused: /mirror/connect and /mirror/ask attach
// no request principal, so internal/api's admit cannot refuse a team there;
// the token lookup does. A token whose owner is a team (users.kind = 'team',
// migration 0125) authenticates as if it did not exist — and the SAME token,
// owned by a person, authenticates (the control). Nothing mints a team token
// today; the row is planted as the login role.
func TestATeamOwnedMirrorTokenIsRefused(t *testing.T) {
	s, pool, dsn := pgStore(t)
	sys := authz.System(context.Background())
	tok, rec, err := s.CreateToken(authz.Operator(context.Background()), "ws-team")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Authenticate(sys, tok); err != nil || got.ID != rec.ID {
		t.Fatalf("control: a person's token = %+v, %v", got, err)
	}
	const team = 900125
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, kind, role, display_name) OVERRIDING SYSTEM VALUE
		VALUES (900125, 'team', 'member', 'mirror-team-probe') ON CONFLICT (id) DO NOTHING`)
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `UPDATE mirror_tokens SET owner_id = 1 WHERE owner_id IN (SELECT id FROM users WHERE kind = 'team')`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM users WHERE kind = 'team'`)
	})
	if _, err := pool.Exec(sys, `UPDATE mirror_tokens SET owner_id = $1 WHERE id = $2`, team, rec.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Authenticate(sys, tok); !errors.Is(err, ErrBadToken) {
		t.Fatalf("a team-owned token authenticated: %+v, %v; want ErrBadToken", got, err)
	}
}
