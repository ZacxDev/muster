package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// TestTheDeclaredFlagIsOwnerAware (B4): GET /api/officers/defs says a def is
// declared only when the runtime runs THAT owner's officer of that id. User
// 1's env officer marketing:example must not make another owner's def of the
// same id read declared (nothing runs it). CONTROL: user 1's own def of it
// does read declared. Written against the API that predates DB-declared
// officers, so it is red on a server whose flag ignores the owner.
func TestTheDeclaredFlagIsOwnerAware(t *testing.T) {
	f := newConnFixture(t)
	const other = int64(987311)
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, kind, role, display_name) OVERRIDING SYSTEM VALUE VALUES (987311, 'person', 'member', 'flag-probe') ON CONFLICT DO NOTHING`)
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `DELETE FROM officer_defs WHERE owner_id = 987311`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM officer_types WHERE owner_id = 987311`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM users WHERE id = 987311`)
	})
	srv, store := builtinOfficerServer(t, f, `[{"id":"`+builtinOfficerID+`","backend":"builtin"}]`, &bytes.Buffer{})
	for _, owner := range []int64{1, other} {
		ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: owner, Role: authz.RoleMember, Via: authz.ViaToken})
		if err := store.PutType(ctx, owner, builtin.Type{Key: "marketing", Name: "Marketing"}); err != nil {
			t.Fatal(err)
		}
		if err := store.PutDef(ctx, owner, builtin.Def{Officer: builtinOfficerID, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	declared := func(owner int64) bool {
		req := httptest.NewRequest(http.MethodGet, "/api/officers/defs", nil)
		req = req.WithContext(authz.WithPrincipal(req.Context(), authz.Principal{UserID: owner, Role: authz.RoleMember, Via: authz.ViaToken}))
		rec := httptest.NewRecorder()
		srv.handleOfficerDefs(rec, req)
		var out struct {
			Defs []struct {
				Officer  string `json:"officer"`
				Declared bool   `json:"declared"`
			} `json:"defs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Defs) != 1 || out.Defs[0].Officer != builtinOfficerID {
			t.Fatalf("owner %d's defs: %d %s", owner, rec.Code, rec.Body.String())
		}
		return out.Defs[0].Declared
	}
	if !declared(1) {
		t.Fatal("control: user 1's def of its own env officer does not read declared")
	}
	if declared(other) {
		t.Fatalf("owner %d's def %s reads declared because USER 1's env officer has that id; nothing runs it", other, builtinOfficerID)
	}
}
