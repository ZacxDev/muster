package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/devices"
)

// ROUND 6 (passkey_enrol.go accountHasStrongFactor, admin_proof.go): ONE rule
// decides the gated tier — the sticky record OR a live passkey OR a step-up
// phone — and every gate reads it; an admin page's proof must be VOUCHED.
// Red at the previous head.

// adminSub is the invite world's admin's GoTrue subject.
func adminSub(t *testing.T, w *invWorld) string {
	t.Helper()
	c, err := w.fake.Verifier().Verify(w.adminAT)
	if err != nil {
		t.Fatal(err)
	}
	return c.Subject
}

// promoteB invites B and returns a func that asks to promote B to admin with
// the world's admin's session.
func promoteB(w *invWorld, email string) (int64, func() int) {
	b, _, _, _ := w.joinMember(email)
	return b, func() int {
		return do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(b, 10)+"/role", url.Values{"role": {"admin"}}, w.asAdmin()).Code
	}
}

func mustBeUnmarked(t *testing.T, w *invWorld, uid int64) {
	t.Helper()
	if had, err := w.s.hadStrongFactor(context.Background(), uid); err != nil || had {
		t.Fatalf("fixture: user %d marked before the test (%v %v)", uid, had, err)
	}
}

// TestAnUnmarkedAdminWithALivePasskeyTakesTheProofToPromote: 🔴 the admin's
// passkey was made outside muster (no sign-in, no add) so nothing marked the
// account; the record alone read it as open and a promotion took no proof.
func TestAnUnmarkedAdminWithALivePasskeyTakesTheProofToPromote(t *testing.T) {
	w := newPasskeyWorld(t)
	a := adminSelf(t, w)
	w.fake.AddPasskey(adminSub(t, w), "admin-live-cred")
	mustBeUnmarked(t, w, a)
	b, promote := promoteB(w, "unmarked-passkey-puppet@invite.test")
	if c := promote(); c != http.StatusForbidden {
		t.Fatalf("promoting B by an admin with a live passkey, with no proof: %d, want 403", c)
	}
	if role := w.roleOf(t, b); role != "member" {
		t.Fatalf("B after a refused promotion: %s", role)
	}
	// The live passkey found marks the account: it stays gated once it goes.
	if had, err := w.s.hadStrongFactor(context.Background(), a); err != nil || !had {
		t.Fatalf("a live passkey found did not mark the account (%v %v)", had, err)
	}
	grantEnrolTicket(t, w.s, w.fake, a, w.adminAT)
	if c := promote(); c != http.StatusSeeOther {
		t.Fatalf("control: promoting B with the admin's vouched proof: %d", c)
	}
}

// pairStepUpPhone gives account uid a paired phone with a step-up key, through
// the devices store (no handler: nothing marks the account), and wires the
// store into w.
func pairStepUpPhone(t *testing.T, w *invWorld, uid int64) {
	t.Helper()
	ds := devices.NewPG(w.pool)
	w.s.ext.Devices = ds
	code, _, err := ds.CreatePairingCode(asUser(uid), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	_, d, err := ds.Redeem(context.Background(), code, devices.PairIn{Name: "admin phone", AppVersion: "0.2.0",
		StepUpKey: base64.StdEncoding.EncodeToString(der), KeyType: devices.KeyBiometric, KeyLevel: devices.LevelStrongBox})
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasStepUpKey {
		t.Fatal("fixture: the paired phone has no step-up key")
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(authz.System(context.Background()), `DELETE FROM devices WHERE id = $1`, d.ID)
	})
}

// TestAnUnmarkedAdminWithAStepUpPhoneTakesTheProofToPromote: the same, the
// factor a paired phone with a step-up key.
func TestAnUnmarkedAdminWithAStepUpPhoneTakesTheProofToPromote(t *testing.T) {
	w := newPasskeyWorld(t)
	a := adminSelf(t, w)
	pairStepUpPhone(t, w, a)
	// The pairing marked the account (migration 0189); a phone paired before
	// that, and never marked, is the case.
	w.s.clearStrongFactor(context.Background(), a)
	mustBeUnmarked(t, w, a)
	b, promote := promoteB(w, "unmarked-phone-puppet@invite.test")
	if c := promote(); c != http.StatusForbidden {
		t.Fatalf("promoting B by an admin with a step-up phone, with no proof: %d, want 403", c)
	}
	if role := w.roleOf(t, b); role != "member" {
		t.Fatalf("B after a refused promotion: %s", role)
	}
	grantEnrolTicket(t, w.s, w.fake, a, w.adminAT)
	if c := promote(); c != http.StatusSeeOther {
		t.Fatalf("control: promoting B with the admin's vouched proof: %d", c)
	}
}

// grantUnvouchedTicket grants the admin's session the ticket a proof with a
// passkey its password added makes: held, not vouched.
func grantUnvouchedTicket(t *testing.T, w *invWorld, uid int64) {
	t.Helper()
	c, err := w.fake.Verifier().Verify(w.adminAT)
	if err != nil {
		t.Fatal(err)
	}
	w.s.passkeyEnrol.grant(enrolKey(uid, gotrueCreds{access: w.adminAT, claims: c}), enrolTicket{vouched: false, proof: "passkey", granted: w.s.stepUpNow()})
}

// TestAnUnvouchedProofCannotMintAResetLinkOrPromote: 🟡 a fresh admin adds a
// first passkey with its password alone and proves with it: that ticket is
// not vouched, so it mints no reset link for a gated account and promotes no
// one.
func TestAnUnvouchedProofCannotMintAResetLinkOrPromote(t *testing.T) {
	w := newPasskeyWorld(t)
	a := adminSelf(t, w)
	_ = w.s.markStrongFactor(context.Background(), a)
	c, _, _, _ := w.joinMember("unvouched-target@invite.test")
	_ = w.s.markStrongFactor(context.Background(), c)
	mint := func() *http.Response {
		return do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(c, 10)+"/reset-link", url.Values{}, w.asAdmin()).Result()
	}
	grantUnvouchedTicket(t, w, a)
	res := mint()
	body := readAll(t, res)
	if res.StatusCode != http.StatusForbidden || !strings.Contains(body, "a passkey added with your password alone") {
		t.Fatalf("a reset link on an unvouched proof: %d, want 403 with the unvouched refusal", res.StatusCode)
	}
	if n := len(w.audit("user.reset_link")); n != 0 {
		t.Fatalf("a refused reset link was audited %d time(s)", n)
	}
	b, promote := promoteB(w, "unvouched-puppet@invite.test")
	if code := promote(); code != http.StatusForbidden {
		t.Fatalf("a promotion on an unvouched proof: %d, want 403", code)
	}
	if role := w.roleOf(t, b); role != "member" {
		t.Fatalf("B after a refused promotion: %s", role)
	}
	// The control: a vouched proof mints it.
	grantEnrolTicket(t, w.s, w.fake, a, w.adminAT)
	if res := mint(); res.StatusCode != http.StatusOK {
		t.Fatalf("control: a reset link on a vouched proof: %d", res.StatusCode)
	}
}

func readAll(t *testing.T, res *http.Response) string {
	t.Helper()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}

// TestOnlyOneFunctionReadsTheStrongFactorRecord: 🔴 ONE RULE, ONE PLACE. The
// sticky record is read by hadStrongFactor alone, from accountStrongFactors
// alone; the tier is decided through accountHasStrongFactor (or the enrolment
// gate's passkeyEnrolNeed, which carries which factor). A new direct reader —
// in this package or anywhere in the module — fails this.
func TestOnlyOneFunctionReadsTheStrongFactorRecord(t *testing.T) {
	assertCallers(t, ".", "hadStrongFactor", []string{"passkey_enrol.go:accountStrongFactors"})
	assertCallers(t, ".", "accountStrongFactors", []string{"login_invite.go:handleResetSubmit", "passkey_enrol.go:accountHasStrongFactor", "passkey_enrol.go:passkeyEnrolNeed"})
	assertCallers(t, ".", "accountHasStrongFactor", []string{"admin_proof.go:adminProof", "admin_proof.go:targetMarked"})
	// THE open-tier decision, with its fresh session check (round 8): every
	// grant of the open tier goes through it.
	assertCallers(t, ".", "openTier", []string{"passkey_enrol.go:accountHasStrongFactor", "passkey_enrol.go:passkeyEnrolNeed"})

	root := filepath.Join("..", "..")
	// The store method, across the module.
	if got, want := moduleCallers(t, root, "HadStrongFactor"), []string{"internal/api/passkey_enrol.go:hadStrongFactor"}; !slices.Equal(got, want) {
		t.Errorf("HadStrongFactor is called from %v\nwant exactly %v", got, want)
	}
	// The column, across the module's non-test Go: the store's three methods
	// and the e2e seed's reset — nothing else writes SQL against it.
	got := moduleFilesMentioning(t, root, "had_strong_factor_at")
	want := []string{"e2e/seed/main.go", "internal/api/passkey_enrol.go", "internal/users/users.go"}
	if !slices.Equal(got, want) {
		t.Errorf("non-test Go naming had_strong_factor_at: %v\nwant exactly %v", got, want)
	}
	// The one in internal/api is comments only: no SQL there
	// (TestTheStrongFactorColumnIsALedger reads every literal, folded).
	for _, f := range []string{"passkey_enrol.go"} {
		if stringLiteralMentions(t, f, "had_strong_factor_at") {
			t.Errorf("%s has a string literal naming had_strong_factor_at: read it through accountHasStrongFactor", f)
		}
	}
	// The positive control: the scanner sees the store's own SQL.
	if !stringLiteralMentions(t, filepath.Join(root, "internal", "users", "users.go"), "had_strong_factor_at") {
		t.Fatal("control: the literal scan does not see users.go's SQL")
	}
}

// moduleGoFiles is every non-test .go file under root (module-relative).
func moduleGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "vendor", ".git", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 100 {
		t.Fatalf("control: only %d Go files under %s", len(out), root)
	}
	return out
}

// moduleCallers is every "<path>:<func>" in the module whose body calls a
// method or function named name.
func moduleCallers(t *testing.T, root, name string) []string {
	t.Helper()
	var out []string
	fset := token.NewFileSet()
	for _, rel := range moduleGoFiles(t, root) {
		af, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var got string
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					got = fn.Sel.Name
				case *ast.Ident:
					got = fn.Name
				}
				if got == name {
					out = append(out, rel+":"+fd.Name.Name)
				}
				return true
			})
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func moduleFilesMentioning(t *testing.T, root, word string) []string {
	t.Helper()
	var out []string
	for _, rel := range moduleGoFiles(t, root) {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), word) {
			out = append(out, rel)
		}
	}
	slices.Sort(out)
	return out
}

func stringLiteralMentions(t *testing.T, path, word string) bool {
	t.Helper()
	af, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(af, func(n ast.Node) bool {
		if l, ok := n.(*ast.BasicLit); ok && l.Kind == token.STRING && strings.Contains(l.Value, word) {
			found = true
		}
		return true
	})
	return found
}

// TestAResetLinkGoTrueRefusedGivesTheProofBack: 🟢 the admin's proof is
// spent only by a change that happened: a reset link GoTrue could not make
// leaves the ticket for the retry.
func TestAResetLinkGoTrueRefusedGivesTheProofBack(t *testing.T) {
	w := newPasskeyWorld(t)
	a := adminSelf(t, w)
	_ = w.s.markStrongFactor(context.Background(), a)
	c, _, _, _ := w.joinMember("giveback-target@invite.test")
	_ = w.s.markStrongFactor(context.Background(), c)
	setEmail := func(e string) {
		if _, err := w.pool.Exec(authz.System(context.Background()), `UPDATE users SET email = $2 WHERE id = $1`, c, e); err != nil {
			t.Fatal(err)
		}
	}
	mint := func() int {
		return do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(c, 10)+"/reset-link", url.Values{}, w.asAdmin()).Code
	}
	grantEnrolTicket(t, w.s, w.fake, a, w.adminAT)
	setEmail("unknown-to-gotrue@invite.test") // GoTrue makes no link for it
	if code := mint(); code != http.StatusBadGateway {
		t.Fatalf("a reset link GoTrue refused: %d, want 502", code)
	}
	setEmail("giveback-target@invite.test")
	if code := mint(); code != http.StatusOK {
		t.Fatalf("the retry on the same proof: %d, want 200 (the proof was spent by a failure)", code)
	}
	if code := mint(); code != http.StatusForbidden {
		t.Fatalf("control: a third mint with the proof spent: %d, want 403", code)
	}
}
