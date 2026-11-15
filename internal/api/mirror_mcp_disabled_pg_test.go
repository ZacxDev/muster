package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/users"
)

// TestADisabledPersonsMirrorTokenIsRefusedAtTheMCPDoor (#681): the host's MCP
// door (POST /mirror/mcp) over the REAL token store. The step-up tools, the
// host-dispatch report tools, doctor and dispatch_hold_tools all run behind
// this one door, so it is where a disabled owner's host is refused.
//
//  1. The owner is disabled by setting disabled_at directly — NO revoke — and
//     the door refuses: the store's owner rule, not the revoke, is the guard.
//     Clearing it (the control) admits the token again.
//  2. The owner is disabled through users.SetDisabled and the door refuses;
//     after a re-enable the OLD token is still refused (the disable revoked
//     it) and a NEW one is admitted.
func TestADisabledPersonsMirrorTokenIsRefusedAtTheMCPDoor(t *testing.T) {
	f, _, _, _ := newMirrorMCPFixture(t)
	store := mirror.NewPG(f.pool)
	f.srv.ext.MirrorTokens = store
	sys := authz.System(context.Background())
	t.Cleanup(func() { _, _ = f.pool.Exec(sys, `DELETE FROM mirror_tokens WHERE owner_id = $1`, f.member) })
	member := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.member, Role: authz.RoleMember, Via: authz.ViaSession})
	tok, _, err := store.CreateToken(member, "ws")
	if err != nil {
		t.Fatal(err)
	}
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	door := func(tok string) (int, string) {
		code, _, body := f.mirrorRPC(t, mirrorAs(tok, focusSID), list)
		return code, mirror.OneLine(body, 160)
	}
	if code, body := door(tok); code != http.StatusOK {
		t.Fatalf("control: an enabled person's token: %d %s", code, body)
	}

	if _, err := f.pool.Exec(sys, `UPDATE users SET disabled_at = now() WHERE id = $1`, f.member); err != nil {
		t.Fatal(err)
	}
	if code, body := door(tok); code != http.StatusUnauthorized {
		t.Fatalf("#681: the MCP door answered a DISABLED owner's (unrevoked) token %d %s, want 401", code, body)
	}
	if _, err := f.pool.Exec(sys, `UPDATE users SET disabled_at = NULL WHERE id = $1`, f.member); err != nil {
		t.Fatal(err)
	}
	if code, body := door(tok); code != http.StatusOK {
		t.Fatalf("control: the token once its owner is enabled again: %d %s", code, body)
	}

	accounts := users.NewPG(f.pool)
	if _, err := accounts.SetDisabled(context.Background(), f.member, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if code, body := door(tok); code != http.StatusUnauthorized {
		t.Fatalf("#681: the MCP door answered a disabled person's token %d %s, want 401", code, body)
	}
	if _, err := accounts.SetDisabled(context.Background(), f.member, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if code, body := door(tok); code != http.StatusUnauthorized {
		t.Fatalf("re-enable brought the OLD mirror token back at the MCP door: %d %s, want 401", code, body)
	}
	fresh, _, err := store.CreateToken(member, "ws")
	if err != nil {
		t.Fatalf("a re-enabled person could not mint a new token: %v", err)
	}
	if code, body := door(fresh); code != http.StatusOK {
		t.Fatalf("a re-enabled person's NEW token: %d %s, want 200", code, body)
	}
}
