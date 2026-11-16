package officers_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestNewPublicBranchIsNeutralAndRandom: a minted public branch is
// <prefix>fix/<slug>-<12 lower-case letters> — no digit after the prefix, so
// it can spell no item number — and 200 mints with the same inputs give 200
// different tokens (nothing about the inputs fixes the token). A slug with a
// digit, an upper-case letter, a slash or 41 characters is refused.
func TestNewPublicBranchIsNeutralAndRandom(t *testing.T) {
	shape := regexp.MustCompile(`^officer/fix/raise-limit-[a-z]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		b, err := officers.NewPublicBranch("officer/", "raise-limit")
		if err != nil {
			t.Fatal(err)
		}
		if !shape.MatchString(b) {
			t.Fatalf("minted %q, want officer/fix/raise-limit-<12 letters>", b)
		}
		if seen[b] {
			t.Fatalf("minted %q twice", b)
		}
		seen[b] = true
	}
	for _, slug := range []string{"", "fix-17", "Raise-limit", "raise/limit", "raise--limit", "-raise", strings.Repeat("a", 41)} {
		if _, err := officers.NewPublicBranch("officer/", slug); err == nil {
			t.Errorf("slug %q was accepted", slug)
		}
	}
	// Control: 40 letters is the bound, not over it.
	if _, err := officers.NewPublicBranch("officer/", strings.Repeat("a", 40)); err != nil {
		t.Errorf("control: a 40-letter slug: %v", err)
	}
}

// TestTheMemPublicBranchRecord: one branch per (officer, repository, item) —
// a second mint answers the first branch — read back by name and listed;
// another officer's or another repository's name is not this one's.
func TestTheMemPublicBranchRecord(t *testing.T) {
	ctx := context.Background()
	st := officers.NewMemPublicBranchStore()
	const a, b = "draft:example-a", "draft:example-b"
	got, err := st.PublicBranchMint(ctx, a, "Example-Org/Web", 7, "officer/fix/x-abcdefghijkl")
	if err != nil || got != "officer/fix/x-abcdefghijkl" {
		t.Fatalf("mint: %q, %v", got, err)
	}
	if got, _ := st.PublicBranchMint(ctx, a, "example-org/web", 7, "officer/fix/y-mnopqrstuvwx"); got != "officer/fix/x-abcdefghijkl" {
		t.Fatalf("a second mint for item 7: %q, want the first branch", got)
	}
	if n, ok, _ := st.PublicBranchItem(ctx, a, "example-org/web", "officer/fix/x-abcdefghijkl"); !ok || n != 7 {
		t.Fatalf("item: %d %v, want 7", n, ok)
	}
	for _, c := range []struct{ officer, repo string }{{b, "example-org/web"}, {a, "example-org/other"}} {
		if _, ok, _ := st.PublicBranchItem(ctx, c.officer, c.repo, "officer/fix/x-abcdefghijkl"); ok {
			t.Errorf("%s in %s sees officer %s's branch", c.officer, c.repo, a)
		}
	}
	if l, _ := st.PublicBranches(ctx, a, "example-org/web"); len(l) != 1 {
		t.Fatalf("branches: %v, want one", l)
	}
}

// TestThePublicBranchToolIsOfferedForAPublicDraftRepoOnly: officer_public_branch
// and the kickoff's public note go to a draft officer with a public
// repository, never to one without; the private kickoff keeps the item-form
// branch and first line.
func TestThePublicBranchToolIsOfferedForAPublicDraftRepoOnly(t *testing.T) {
	pub, err := parseDecl(t, draftDecl(docsShip(t)), shipEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := parseDecl(t, draftDecl(draftShip()), shipEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if !officers.ToolOffered(pub[0], officers.ToolPublicBranch) {
		t.Fatal("a draft officer with a public repository is not offered officer_public_branch")
	}
	if k := officers.KickoffText(pub[0], 1); !strings.Contains(k, "officer_public_branch{repo, item_id, slug}") ||
		!strings.Contains(k, "Authored from untrusted inputs by an automated agent; review as such") {
		t.Errorf("the public kickoff lacks the public branch and body:\n%s", k)
	}
	if officers.ToolOffered(priv[0], officers.ToolPublicBranch) {
		t.Fatal("a draft officer with no public repository is offered officer_public_branch")
	}
	k := officers.KickoffText(priv[0], 1)
	if strings.Contains(k, "officer_public_branch") || !strings.Contains(k, "/<item>-<slug>") ||
		!strings.Contains(k, "Officer: "+shipID+" · item <n> · authored from untrusted inputs; review as such") {
		t.Errorf("the private kickoff changed:\n%s", k)
	}
}
