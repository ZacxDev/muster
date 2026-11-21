package plugins

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestRuleWriterRefusesBothPluginDoors: a routing rule is never written by a
// plugin — a person's own (ViaPlugin) or a server app acting for a person
// (ViaServerPlugin), which would otherwise write that person's rules. The
// person's own session writes as itself (the control).
// Red if ruleWriter checks only ViaPlugin.
func TestRuleWriterRefusesBothPluginDoors(t *testing.T) {
	for _, via := range []authz.Via{authz.ViaPlugin, authz.ViaServerPlugin} {
		ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 41, Role: authz.RoleMember, Via: via})
		if _, err := ruleWriter(ctx); !errors.Is(err, ErrRulePlugin) {
			t.Errorf("ruleWriter via %s: %v; want ErrRulePlugin", via, err)
		}
	}
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 41, Role: authz.RoleMember, Via: authz.ViaSession})
	if owner, err := ruleWriter(ctx); err != nil || owner != 41 {
		t.Errorf("CONTROL ruleWriter via session: %d %v; want 41", owner, err)
	}
}

// TestANewSubjectIs22Base64URLCharactersOfRandomBytes pins the format the
// meetings plugin is built against: exactly 22 base64url characters, no
// padding, decoding to 16 bytes, fresh each time; and ValidSubject accepts
// exactly that shape (the design's first example, 23 characters, is refused).
func TestANewSubjectIs22Base64URLCharactersOfRandomBytes(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		s, err := newSubject()
		if err != nil {
			t.Fatal(err)
		}
		raw, derr := base64.RawURLEncoding.DecodeString(s)
		if len(s) != 22 || derr != nil || len(raw) != 16 || !ValidSubject(s) {
			t.Fatalf("subject %q: want 22 base64url characters of 16 bytes", s)
		}
		if seen[s] {
			t.Fatalf("subject %q minted twice in 200", s)
		}
		seen[s] = true
	}
	for _, bad := range []string{"", "Q2hhbmdlTWUtMTIzNDU2Nzg", "AAECAwQFBgcICQoLDA0OD", "AAECAwQFBgcICQoLDA0OD+", "AAECAwQFBgcICQoLDA0OD/", "AAECAwQFBgcICQoLDA0ODw=="} {
		if ValidSubject(bad) {
			t.Errorf("ValidSubject(%q) = true; want false", bad)
		}
	}
	if !ValidSubject("AAECAwQFBgcICQoLDA0ODw") {
		t.Error("CONTROL ValidSubject refused a 22-character subject")
	}
}

// TestPersonalOwnerIsFalseForAServerApp: the accessor answers (0, false) for
// a server row, whatever its field holds, and the owner for a person's.
func TestPersonalOwnerIsFalseForAServerApp(t *testing.T) {
	if o, ok := (Plugin{Scope: ScopeServer, ownerID: 7}).PersonalOwner(); ok || o != 0 {
		t.Errorf("a server row: (%d, %v); want (0, false)", o, ok)
	}
	if o, ok := (Plugin{}).OwnedBy(7).PersonalOwner(); !ok || o != 7 {
		t.Errorf("a person's row: (%d, %v); want (7, true)", o, ok)
	}
}
