package connectors_test

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
)

// TestRepoScopeIsOneCanonicalStringForTheSet pins RepoScope LITERALLY: with no
// read repositories it is the repository itself (trimmed — byte-identical to
// what every approval before read_repos recorded, so none re-opens on
// upgrade); with some it names the set with the read repositories lowercased,
// deduplicated and sorted, so the same set in another order or case is the
// same string, and any other set — or another own repository — is not.
func TestRepoScopeIsOneCanonicalStringForTheSet(t *testing.T) {
	for _, c := range []struct {
		repo string
		read []string
		want string
	}{
		{"example-org/widget", nil, "example-org/widget"},
		{" Example-Org/Widget ", []string{}, "Example-Org/Widget"},
		{"example-org/widget", []string{" ", ""}, "example-org/widget"},
		{"", nil, ""},
		{"example-org/widget", []string{"other-org/docs", "example-org/flags-state"},
			"example-org/widget (+ read: example-org/flags-state, other-org/docs)"},
		{"example-org/widget", []string{"Example-Org/Flags-State", " other-org/docs ", "example-org/flags-state"},
			"example-org/widget (+ read: example-org/flags-state, other-org/docs)"},
	} {
		if got := connectors.RepoScope(c.repo, c.read); got != c.want {
			t.Errorf("RepoScope(%q, %q) = %q, want %q", c.repo, c.read, got, c.want)
		}
	}
}

// grantStub is a Store holding one grant, recording every ReopenGrant. The
// methods CheckAccess does not call panic (the nil embedded interface).
type grantStub struct {
	connectors.Store
	g        connectors.Grant
	reopened []int64
}

func (s *grantStub) LatestGrant(context.Context, int64, string) (connectors.Grant, error) {
	return s.g, nil
}
func (s *grantStub) ReopenGrant(_ context.Context, id int64) (connectors.Grant, bool, error) {
	s.reopened = append(s.reopened, id)
	g := s.g
	g.Status = connectors.StatusPending
	return g, true, nil
}
func (*grantStub) Audit(context.Context, connectors.AuditEntry) error { return nil }

// TestTheProxyReopensAGrantWhenTheReadReposChangeAndNotOnAReorder is the
// grant's set at the proxy's own door (Proxy.CheckAccess): a grant approved
// for example-org/widget with read repositories {flags-state, docs} stays
// usable for a caller declaring that set in another order and case, and is
// put back to PENDING — with the reason naming both sets — when an entry is
// removed, one is added, one is swapped, or the own repository changes.
func TestTheProxyReopensAGrantWhenTheReadReposChangeAndNotOnAReorder(t *testing.T) {
	approvedFor := connectors.RepoScope("example-org/widget", []string{"example-org/flags-state", "other-org/docs"})
	for _, c := range []struct {
		name   string
		repo   string
		read   []string
		reopen bool
	}{
		{"the same set", "example-org/widget", []string{"example-org/flags-state", "other-org/docs"}, false},
		{"reordered", "example-org/widget", []string{"other-org/docs", "example-org/flags-state"}, false},
		{"reordered, other case", "Example-Org/Widget", []string{"Other-Org/Docs", "EXAMPLE-ORG/flags-state"}, false},
		{"one removed", "example-org/widget", []string{"example-org/flags-state"}, true},
		{"all removed", "example-org/widget", nil, true},
		{"one added", "example-org/widget", []string{"example-org/flags-state", "other-org/docs", "example-org/extra"}, true},
		{"one swapped", "example-org/widget", []string{"example-org/flags-state", "other-org/docs2"}, true},
		{"own repo changed", "example-org/gadget", []string{"example-org/flags-state", "other-org/docs"}, true},
	} {
		st := &grantStub{g: connectors.Grant{ID: 21, AgentID: 7, Connector: "github", Status: connectors.StatusApproved,
			Source: connectors.SourceStored, Repo: approvedFor}}
		p := &connectors.Proxy{Store: st, Policy: testPolicy()}
		acc, err := p.CheckAccess(context.Background(), connectors.Caller{AgentID: 7, AgentName: "officer-a-b-1",
			Repo: c.repo, ReadRepos: c.read}, "github", "")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if acc.Usable == c.reopen || (len(st.reopened) == 1) != c.reopen {
			t.Errorf("%s: usable=%v reopened=%v, want reopened=%v", c.name, acc.Usable, st.reopened, c.reopen)
		}
	}
	// The no-read-repositories control: a grant approved for the bare repository
	// (every approval before read_repos) stays usable for a caller with none.
	st := &grantStub{g: connectors.Grant{ID: 22, AgentID: 7, Connector: "github", Status: connectors.StatusApproved,
		Source: connectors.SourceStored, Repo: "example-org/widget"}}
	p := &connectors.Proxy{Store: st, Policy: testPolicy()}
	if acc, err := p.CheckAccess(context.Background(), connectors.Caller{AgentID: 7, Repo: "example-org/widget"}, "github", ""); err != nil ||
		!acc.Usable || len(st.reopened) != 0 {
		t.Fatalf("an approval made before read_repos, for a caller with none: usable=%v reopened=%v err=%v", acc.Usable, st.reopened, err)
	}
}

// TestReconcileNamesBothSetsWhenTheReadReposChange: the audit reason of a
// read_repos change names the set approved and the set now.
func TestReconcileNamesBothSetsWhenTheReadReposChange(t *testing.T) {
	base := &reopenStub{}
	was := connectors.RepoScope("example-org/widget", []string{"example-org/flags-state"})
	now := connectors.RepoScope("example-org/widget", []string{"example-org/flags-state", "other-org/docs"})
	g := connectors.Grant{ID: 11, Connector: "svc", Status: connectors.StatusApproved, Source: connectors.SourceStored, Repo: was}
	_, why, err := connectors.ReconcileGrant(context.Background(), base, g, now)
	const want = "re-opened: the agent's repository changed (approved for example-org/widget (+ read: example-org/flags-state), " +
		"now example-org/widget (+ read: example-org/flags-state, other-org/docs))"
	if err != nil || why != want {
		t.Fatalf("why = %q err = %v, want %q", why, err, want)
	}
}
