package plugins

import (
	"context"
	"slices"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestAuthenticateCarriesCapabilitiesAndTheOwnersLogin: a token minted today has
// exactly {messages} (0102's default), a row set to {viewer} reads back as that
// and nothing more, and Authenticate names the owner as a person would: the
// account's email, or its display name when it has none. Every value is
// distinct, so a scan that read the wrong column cannot pass.
func TestAuthenticateCarriesCapabilitiesAndTheOwnersLogin(t *testing.T) {
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `UPDATE users SET email = 'member-two@example.invalid', display_name = 'Member Two' WHERE id = 2`)
	dbtest.OwnerExec(t, dsn, `UPDATE users SET email = NULL, display_name = 'Member Three' WHERE id = 3`)
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `UPDATE users SET email = NULL, display_name = '' WHERE id IN (2, 3)`)
	})
	tok2, p2 := mustCreate(t, s, 2, "example-inbox")
	tok3, p3 := mustCreate(t, s, 3, "example-viewer")
	if !slices.Equal(p2.Capabilities, []Capability{CapMessages}) {
		t.Fatalf("a newly minted token's capabilities = %v; want [messages]", p2.Capabilities)
	}
	dbtest.OwnerExec(t, dsn, `UPDATE plugins SET capabilities = '{viewer}' WHERE id = `+strconv.FormatInt(p3.ID, 10))

	sys := authz.System(context.Background())
	got2, err := s.Authenticate(sys, tok2)
	if err != nil || !slices.Equal(got2.Capabilities, []Capability{CapMessages}) || got2.OwnerLogin != "member-two@example.invalid" ||
		!got2.Has(CapMessages) || got2.Has(CapViewer) {
		t.Fatalf("Authenticate(user 2's token) = caps %v, login %q, %v; want [messages], the email", got2.Capabilities, got2.OwnerLogin, err)
	}
	got3, err := s.Authenticate(sys, tok3)
	if err != nil || !slices.Equal(got3.Capabilities, []Capability{CapViewer}) || got3.OwnerLogin != "Member Three" ||
		got3.Has(CapMessages) || !got3.Has(CapViewer) {
		t.Fatalf("Authenticate(user 3's token) = caps %v, login %q, %v; want [viewer], the display name", got3.Capabilities, got3.OwnerLogin, err)
	}
	if got, err := s.Plugin(session(3), p3.ID); err != nil || !slices.Equal(got.Capabilities, []Capability{CapViewer}) {
		t.Fatalf("Plugin(user 3's) = caps %v, %v; want [viewer]", got.Capabilities, err)
	}
}
