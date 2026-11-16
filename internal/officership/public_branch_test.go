package officership

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// A PUBLIC REPOSITORY'S DRAFT FORM: the branch and the body name neither the
// officer nor its item; muster's record (PublicBranches) maps the branch to
// both, and every rule that read them from the name reads the record.
// publicRig marks example-infra (prefix bot/, base trunk) public.
// ---------------------------------------------------------------------------

// memRecord is a PublicBranchRecord over officers.MemPublicBranchStore, bound
// to one officer (as internal/api binds the database store).
type memRecord struct {
	st      *officers.MemPublicBranchStore
	officer string
	err     error // when set, every read fails
}

func newMemRecord(officer string) *memRecord {
	return &memRecord{st: officers.NewMemPublicBranchStore(), officer: officer}
}

func (m *memRecord) Item(ctx context.Context, repo, branch string) (int, bool, error) {
	if m.err != nil {
		return 0, false, m.err
	}
	return m.st.PublicBranchItem(ctx, m.officer, repo, branch)
}
func (m *memRecord) Branches(ctx context.Context, repo string) ([]string, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.st.PublicBranches(ctx, m.officer, repo)
}
func (m *memRecord) Mint(ctx context.Context, repo string, item int, branch string) (string, error) {
	return m.st.PublicBranchMint(ctx, m.officer, repo, item, branch)
}

// dPubBranch is the public rig's recorded branch for item 17 (publicRig):
// officers.NewPublicBranch's shape, naming neither the officer nor the item.
const dPubBranch = "bot/fix/clean-docs-qzvrwmtkpjhd"

// publicFirstLine is the public body's first line, spelled out (not read from
// the code under test).
const publicFirstLine = "Authored from untrusted inputs by an automated agent; review as such"

func goodPublicBody() string {
	return publicFirstLine + "\n\n## Root cause\nThe limit is low.\n\n## Evidence\naudit 51, audit 52\n\n" +
		"## Test\nkustomize build\n\n## Rollback\nrevert this commit\n"
}

func goodPublicCreate() map[string]any {
	m := goodCreate()
	m["head"] = dPubBranch
	m["body"] = goodPublicBody()
	return m
}

// mintFor names a public branch for item, making the rig's clean comparison
// and the README the docs-only gate reads at its head the minted branch's
// too, so a create from it is admitted.
func (r *draftRig) mintFor(t *testing.T, item int, slug string) string {
	t.Helper()
	br, err := r.g.MintPublicBranch(context.Background(), dInstance, dInfra, item, slug)
	if err != nil {
		t.Fatalf("minting a branch for item %d: %v", item, err)
	}
	r.infra.compares["trunk..."+br] = r.infra.compares["trunk..."+dBranch]
	if c, ok := r.infra.contents["README.md@"+dBranch]; ok {
		r.infra.contents["README.md@"+br] = c
	}
	return br
}

// pushRef admits a push of ref to repo (nil: admitted).
func (r *draftRig) pushRef(repo, ref string) error {
	body := pkt(oidA+" "+oidB+" "+ref+"\x00report-status\n") + "0000"
	_, err := r.g.Admit(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)), "github.com", repo+".git/git-receive-pack")
	return err
}

// TestAPublicPullRequestBodyNamesNoOfficerOrItem: in a public repository the
// body's first line is the neutral one, exactly; the private line (which
// names the officer and the item) is refused, and so is a body or title that
// names the officer's id or slug anywhere. Control: the same private line in
// a repository NOT marked public is admitted, and the neutral one refused
// there (the private form is unchanged).
//
// Red at origin/main: a public repository took the private first line and
// refused the neutral one.
func TestAPublicPullRequestBodyNamesNoOfficerOrItem(t *testing.T) {
	r := publicRig(t)
	priv := goodCreate()
	priv["head"] = dPubBranch
	if err := r.create(priv); !isGateRefusal(err) || !strings.Contains(err.Error(), publicFirstLine) {
		t.Fatalf("public, the private first line: err = %v, want a 403 naming the neutral line", err)
	}
	r = publicRig(t)
	if err := r.create(goodPublicCreate()); err != nil {
		t.Fatalf("public, the neutral first line: %v", err)
	}
	for name, edit := range map[string]func(m map[string]any){
		"the officer id in the body":  func(m map[string]any) { m["body"] = goodPublicBody() + "\nfrom engineering:example-web\n" },
		"the slug in the body":        func(m map[string]any) { m["body"] = goodPublicBody() + "\nsee Engineering-Example-Web\n" },
		"the officer id in the title": func(m map[string]any) { m["title"] = "engineering:example-web raises the limit" },
	} {
		r := publicRig(t)
		m := goodPublicCreate()
		edit(m)
		err := r.create(m)
		if !isConflict(err) || !strings.Contains(err.Error(), "names this officer's internal id") || strings.Contains(err.Error(), "example-web raises") {
			t.Errorf("public, %s: err = %v, want a 409 that does not quote the text", name, err)
		}
	}
	// Controls: a repository NOT marked public keeps the private form.
	r = publicRig(t)
	r.g.Ship.Repos[1].Public = false
	if err := r.create(goodCreate()); err != nil {
		t.Fatalf("control, private: the private first line: %v", err)
	}
	r = publicRig(t)
	r.g.Ship.Repos[1].Public = false
	neutral := goodPublicCreate()
	neutral["head"] = dBranch
	if err := r.create(neutral); !isGateRefusal(err) || !strings.Contains(err.Error(), "Officer: "+dOfficer+" · item 17") {
		t.Fatalf("control, private: the neutral first line: err = %v, want the private line required", err)
	}
}

// TestAPublicBranchNotTheOfficersIsRefused: in a public repository a branch
// is the officer's only by muster's record. Refused there: the private shape
// (it names the officer — refused for that before ownership is asked —
// and the item),
// a neutral branch recorded for ANOTHER officer, a neutral branch muster
// never minted, and any branch when the record cannot be read (fail closed).
// Control: the officer's own recorded branch is admitted, and the private
// shape is still admitted in a repository not marked public.
//
// Red at origin/main: the private shape was admitted in a public repository
// (the name was the only test).
func TestAPublicBranchNotTheOfficersIsRefused(t *testing.T) {
	ctx := context.Background()
	privShape := "refs/heads/bot/engineering-example-web/18-fix-ui"
	r := publicRig(t)
	if err := r.pushRef(dInfra, privShape); !isGateRefusal(err) || !strings.Contains(err.Error(), "names this officer's internal id") {
		t.Fatalf("public: the private shape for an unrecorded item: err = %v, want a 403", err)
	}
	other := newMemRecord("engineering:example-other")
	theirs, err := officers.NewPublicBranch("bot/", "raise-limit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Mint(ctx, dInfra, 18, theirs); err != nil {
		t.Fatal(err)
	}
	r = publicRig(t)
	if err := r.pushRef(dInfra, "refs/heads/"+theirs); !isGateRefusal(err) {
		t.Fatalf("public: another officer's recorded branch: err = %v, want a 403", err)
	}
	m := goodPublicCreate()
	m["head"] = theirs
	r.infra.compares["trunk..."+theirs] = r.infra.compares["trunk..."+dBranch]
	if err := r.create(m); !isGateRefusal(err) {
		t.Fatalf("public: a pull request from another officer's branch: err = %v, want a 403", err)
	}
	never, _ := officers.NewPublicBranch("bot/", "raise-limit")
	if err := r.pushRef(dInfra, "refs/heads/"+never); !isGateRefusal(err) || !strings.Contains(err.Error(), "PUBLIC repository") {
		t.Fatalf("public: a neutral branch muster never minted: err = %v, want a 403", err)
	}
	r = publicRig(t)
	r.g.PublicBranches.(*memRecord).err = errors.New("fake: the database is down")
	if err := r.pushRef(dInfra, "refs/heads/"+dPubBranch); !isGateRefusal(err) || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("public: an unreadable record: err = %v, want a 403 (fail closed)", err)
	}
	r = publicRig(t)
	r.g.PublicBranches = nil
	if err := r.pushRef(dInfra, "refs/heads/"+dPubBranch); !isGateRefusal(err) {
		t.Fatalf("public: no record at all: err = %v, want a 403 (fail closed)", err)
	}
	// Controls.
	if err := publicRig(t).pushRef(dInfra, "refs/heads/"+dPubBranch); err != nil {
		t.Fatalf("control: the officer's own recorded branch: %v", err)
	}
	r = publicRig(t)
	r.g.Ship.Repos[1].Public = false
	if err := r.pushRef(dInfra, privShape); err != nil {
		t.Fatalf("control: the private shape in a repository not marked public: %v", err)
	}
}

// TestMintPublicBranchIsNeutralAndOpaque: officer_public_branch answers
// <prefix>fix/<slug>-<12 letters>: no digit anywhere after the prefix (so no
// item number), neither the officer id nor its slug, the same branch for the
// same item again, and a DIFFERENT token for the same item in another record
// (the token is random, not derived from the item). Refused: a repository not
// marked public, one that is not the officer's, an item HQ does not show the
// instance holding, a fix item, a slug with a digit, and a slug matching an
// internal-details pattern — none of them recorded.
func TestMintPublicBranchIsNeutralAndOpaque(t *testing.T) {
	ctx := context.Background()
	r := publicRig(t)
	br := r.mintFor(t, 18, "raise-limit")
	if !regexp.MustCompile(`^bot/fix/raise-limit-[a-z]{12}$`).MatchString(br) {
		t.Fatalf("branch %q, want bot/fix/raise-limit-<12 letters>", br)
	}
	lower := strings.ToLower(br)
	for _, leak := range []string{"18", "engineering", "example-web", dOfficer} {
		if strings.Contains(lower, leak) {
			t.Errorf("branch %q carries %q", br, leak)
		}
	}
	if again := r.mintFor(t, 18, "other-words"); again != br {
		t.Errorf("a second call for item 18: %q, want the recorded %q", again, br)
	}
	if n, ok, err := r.g.PublicBranches.Item(ctx, dInfra, br); err != nil || !ok || n != 18 {
		t.Errorf("the record maps %q to %d (ok %v, %v), want item 18", br, n, ok, err)
	}
	// Same officer, item and slug in a fresh record: another token.
	r2 := publicRig(t)
	if br2 := r2.mintFor(t, 18, "raise-limit"); br2 == br {
		t.Errorf("two records minted the same name %q for item 18: the token follows the item", br)
	}
	r.claims[19] = "officer-someone-else-1"
	r.claims[20] = dInstance
	r.kinds[20] = "engineering.pr_fix"
	for name, c := range map[string]struct {
		repo, slug string
		item       int
		want       string
	}{
		"a private repository":  {fxRepo, "raise-limit", 18, "not marked public"},
		"not its repository":    {"example-org/elsewhere", "raise-limit", 18, "not one of this officer's repositories"},
		"an item held by other": {dInfra, "raise-limit", 19, "does not show"},
		"a fix item":            {dInfra, "raise-limit", 20, "fix item"},
		"a digit in the slug":   {dInfra, "raise-limit-2", 21, "no digits"},
		"a leaky slug":          {dInfra, "corp-example", 22, "internal-details pattern line"},
	} {
		if c.item >= 21 {
			r.claims[c.item] = dInstance
		}
		if name == "a leaky slug" {
			// A pattern a lower-case word can spell (the rig's file has dots).
			r.g.Ship.InternalPatternsFile = writePatterns(t, "corp-example\n")
		}
		_, err := r.g.MintPublicBranch(ctx, dInstance, c.repo, c.item, c.slug)
		if !isGateRefusal(err) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want a refusal containing %q", name, err, c.want)
		}
		if got, _ := r.g.PublicBranches.Branches(ctx, dInfra); len(got) != 2 { // dPubBranch and br
			t.Errorf("%s: the record holds %v, want nothing new", name, got)
		}
	}
}

// TestAPublicBranchFlowsThroughTheGate: a minted branch is pushed, its draft
// opened with the neutral body, and the draft rules key off the record: the
// per-item limit counts the item's existing pull request (its body names no
// item), the per-repository open cap counts the officer's public pull
// requests, and a body edit is checked against the neutral line.
func TestAPublicBranchFlowsThroughTheGate(t *testing.T) {
	r := publicRig(t)
	br := r.mintFor(t, 18, "raise-limit")
	if err := r.pushRef(dInfra, "refs/heads/"+br); err != nil {
		t.Fatalf("a push of the minted branch: %v", err)
	}
	m := goodPublicCreate()
	m["head"] = br
	if err := r.create(m); err != nil {
		t.Fatalf("a draft from the minted branch: %v", err)
	}
	// The per-item limit: item 18 already has a pull request from br, whose
	// body names no item. A second create for item 18 is refused.
	yesterday := dNow.Add(-16 * time.Hour)
	mine := PR{Number: 40, State: "closed", Author: BotLogin(r.infra.slug), AuthorType: "Bot", HeadRef: br, CreatedAt: yesterday, Body: goodPublicBody()}
	r.infra.all = []PR{mine}
	r.infra.branches[br] = oidB
	if err := r.create(m); !isGateRefusal(err) || !strings.Contains(err.Error(), "item 18 already has 1") {
		t.Fatalf("a second pull request for item 18: err = %v, want the per-item refusal", err)
	}
	// The open cap: two of the officer's public pull requests open (items 17
	// and 18, by the record) refuse a third.
	r = publicRig(t)
	r.claims[19] = dInstance
	br18, br19 := r.mintFor(t, 18, "raise-limit"), r.mintFor(t, 19, "fix-probe")
	open := func(n int, head string) PR {
		return PR{Number: n, State: "open", Author: BotLogin(r.infra.slug), AuthorType: "Bot", HeadRef: head, CreatedAt: yesterday, Body: goodPublicBody()}
	}
	r.infra.all = []PR{open(41, dPubBranch), open(42, br18)}
	m = goodPublicCreate()
	m["head"] = br19
	if err := r.create(m); !isGateRefusal(err) || !strings.Contains(err.Error(), "are open in "+dInfra) {
		t.Fatalf("a third open pull request: err = %v, want the open cap", err)
	}
	// A body edit of #31 (from dPubBranch, item 17): the neutral line passes, the
	// private one is refused.
	r = publicRig(t)
	if err := r.patch(map[string]any{"body": goodPublicBody()}); err != nil {
		t.Fatalf("a body edit with the neutral line: %v", err)
	}
	if err := r.patch(map[string]any{"body": goodBody(17)}); !isGateRefusal(err) {
		t.Fatalf("a body edit with the private line: err = %v, want a 403", err)
	}
}

// TestAPrivateRepositoryKeepsTheItemForm (INVARIANT guard: the private form
// never changed): with no record at all, a repository not marked public takes
// <prefix><slug>/<item>-<slug> and the officer/item first line, and
// officer_public_branch is refused there.
func TestAPrivateRepositoryKeepsTheItemForm(t *testing.T) {
	r := newDraftRig()
	if r.g.PublicBranches != nil {
		t.Fatal("the private rig has a public-branch record")
	}
	if err := r.pushRef(dInfra, "refs/heads/"+dBranch); err != nil {
		t.Fatalf("a push of the item-form branch: %v", err)
	}
	if err := r.create(goodCreate()); err != nil {
		t.Fatalf("a draft with the officer/item first line: %v", err)
	}
	if _, err := r.g.MintPublicBranch(context.Background(), dInstance, dInfra, 17, "raise-limit"); !isGateRefusal(err) {
		t.Fatalf("officer_public_branch in a private repository: err = %v, want a refusal", err)
	}
}

// TestAPublicBranchNameNamingTheOfficerIsRefused: in a public repository a
// branch name never carries the officer's id or slug. officer_public_branch
// refuses a slug that IS the officer's slug (and records nothing), and a push
// to a hand-named branch holding the slug is refused even when muster's record
// holds it (so only the name rule can be what refuses). Controls: a neutral
// slug mints, and the recorded neutral branch is pushed.
//
// Red before the name rule: the mint answered (and recorded)
// bot/fix/engineering-example-web-<token>, and the push was admitted.
func TestAPublicBranchNameNamingTheOfficerIsRefused(t *testing.T) {
	ctx := context.Background()
	r := publicRig(t)
	if _, err := r.g.MintPublicBranch(ctx, dInstance, dInfra, 18, officers.Slug(dOfficer)); !isGateRefusal(err) ||
		!strings.Contains(err.Error(), "names this officer's internal id") {
		t.Errorf("mint with the officer's slug: err = %v, want a refusal naming the rule", err)
	}
	if got, _ := r.g.PublicBranches.Branches(ctx, dInfra); len(got) != 1 { // dPubBranch only
		t.Errorf("a refused mint recorded something: %v", got)
	}
	// A hand-named branch holding the slug, recorded (bypassing the mint), so
	// ownership alone would admit it.
	r = publicRig(t)
	handNamed := "bot/fix/engineering-example-web-qzvrwmtkpjhd"
	if _, err := r.g.PublicBranches.Mint(ctx, dInfra, 19, handNamed); err != nil {
		t.Fatal(err)
	}
	if err := r.pushRef(dInfra, "refs/heads/"+handNamed); !isGateRefusal(err) || !strings.Contains(err.Error(), "names this officer's internal id") {
		t.Errorf("a push to a recorded branch naming the officer: err = %v, want a 403", err)
	}
	// Controls.
	if br, err := r.g.MintPublicBranch(ctx, dInstance, dInfra, 18, "raise-limit"); err != nil || strings.Contains(br, officers.Slug(dOfficer)) {
		t.Errorf("control: a neutral slug: %q, %v", br, err)
	}
	if err := r.pushRef(dInfra, "refs/heads/"+dPubBranch); err != nil {
		t.Errorf("control: the recorded neutral branch: %v", err)
	}
}

// TestAnUnreadableRecordAfterAPublicHitLeavesTheBranchForAHuman: a content
// hit on a push to a public repository's branch, and muster's record of the
// officer's public branches cannot be read when the after-push check runs.
// Ownership is unknown, so the branch is NOT deleted — and HQ's notice says
// so (ownership could not be confirmed, a human must delete it) and still
// calls it a leak, never "not the officer's own branch shape". Both paths:
// no pull request from the branch (publicBranchCheck), and an open one
// (closeOnHit: closed, branch kept). Control: a readable record deletes.
func TestAnUnreadableRecordAfterAPublicHitLeavesTheBranchForAHuman(t *testing.T) {
	leak := File{Path: "README.md", Status: "modified", Changes: 2, Patch: "@@ -8,2 +8,2 @@\n \n-The end.\n+Ask db.corp.example"}
	for _, withPR := range []bool{false, true} {
		for _, broken := range []bool{true, false} {
			name := map[bool]string{false: "no pull request", true: "an open pull request"}[withPR]
			r := publicRig(t)
			r.infra.all = nil
			if withPR {
				r.infra.all = []PR{r.infra.prs[dPR]}
			}
			r.infra.listed = r.infra.all
			r.infra.branches[dPubBranch] = oidB
			r.content("README.md", readmeBase, strings.Replace(readmeBase, "The end.", "Ask db.corp.example", 1))
			r.docsFiles(leak)
			after := r.mustPush(t, dInfra, "refs/heads/"+dPubBranch)
			if broken {
				r.g.PublicBranches.(*memRecord).err = errors.New("fake: the database is down")
			}
			after(context.Background(), applied("refs/heads/"+dPubBranch))
			if withPR && len(r.infra.closed) != 1 {
				t.Errorf("%s, broken=%v: closed %v, want #31 closed", name, broken, r.infra.closed)
			}
			if len(r.notices) != 1 {
				t.Errorf("%s, broken=%v: notices %+v, want one", name, broken, r.notices)
				continue
			}
			sum := r.notices[0].Summary
			if !broken {
				if len(r.infra.deleted) != 1 {
					t.Errorf("control, %s: deleted %v, want the branch deleted", name, r.infra.deleted)
				}
				continue
			}
			if len(r.infra.deleted) != 0 || !strings.Contains(sum, "could NOT confirm") || !strings.Contains(sum, "a human must delete it") ||
				strings.Contains(sum, "not the officer's own branch shape") {
				t.Errorf("%s, record unreadable: deleted %v, notice %q; want kept and a human told to delete it", name, r.infra.deleted, sum)
			}
			if !withPR && !strings.Contains(sum, "treat it as a leak") {
				t.Errorf("%s, record unreadable: notice %q, want it still called a leak", name, sum)
			}
		}
	}
}

// writePatterns writes an internal-details pattern file and returns its path.
func writePatterns(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "internal-patterns.txt")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
