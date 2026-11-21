package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// Team sharing phase 3 (team officers), the parts that need no database.

// lendDoors is EVERY request door and whether lendHostSpawn admits it. A
// personal API token (ViaToken) above all NEVER lends: a personal token
// carries no team access (2a). The walk below drives the gate itself, so a
// door spawnLendDoors grew is caught by behaviour, and the literal fails when
// it names a door that is not one.
var lendDoors = map[authz.Via]bool{
	// The owner's session, and the owner's own chief: its MCP door, and a
	// gateway chief's machine-message and kickoff turns — the
	// operator-approved exception (a token that messages the chief may have
	// it lend the owner's own spawn). A personal token DIRECTLY never lends.
	authz.ViaSession: true, authz.ViaAgent: true, authz.ViaKickoff: true, authz.ViaAgentMessage: true,
	authz.ViaToken: false, authz.ViaDevice: false, authz.ViaHook: false, authz.ViaService: false, authz.ViaConnect: false,
	authz.ViaRelay: false, authz.ViaRunner: false, authz.ViaPlugin: false, authz.ViaQueue: false, authz.ViaFocus: false,
	authz.ViaRoute: false, authz.ViaAttention: false, authz.ViaAsk: false, authz.ViaReap: false, authz.ViaOfficer: false,
	authz.ViaMirrorHost: false, authz.ViaHostDispatch: false, authz.ViaJobQueue: false, authz.ViaPageLink: false,
}

// TestTheSpawnLendDoorsAreExactlyTheListedDoors: lendHostSpawn refuses every
// door outside spawnLendDoors with ErrLendDoor — the personal API token
// first among them — before it reads a membership or the store, and admits
// the listed ones past the door check (to the store's "off": no lender is
// wired here). The person is an OPERATOR of the team at every door, so a
// refusal is about the door alone.
func TestTheSpawnLendDoorsAreExactlyTheListedDoors(t *testing.T) {
	for v := range spawnLendDoors {
		if !lendDoors[v] {
			t.Errorf("spawnLendDoors admits %s, which lendDoors says never lends", v)
		}
	}
	if len(lendDoors) != 23 {
		t.Fatalf("lendDoors names %d doors; authz has 23 request doors", len(lendDoors))
	}
	ms, err := authz.NewMemberships([]authz.Membership{{Team: 12, Role: authz.TeamOperator}})
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	var names []string
	for v := range lendDoors {
		names = append(names, string(v))
	}
	sort.Strings(names)
	admitted := 0
	for _, n := range names {
		v := authz.Via(n)
		ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 7, Role: authz.RoleMember, Via: v, Teams: ms})
		for _, team := range []int64{12, 0} {
			_, err := s.lendHostSpawn(ctx, 1, team)
			switch {
			case lendDoors[v] && !errors.Is(err, ErrLendOff):
				t.Errorf("%s (team %d): %v; want past the door check (to ErrLendOff)", v, team, err)
			case !lendDoors[v] && !errors.Is(err, ErrLendDoor):
				t.Errorf("%s (team %d): %v; want ErrLendDoor", v, team, err)
			}
		}
		if lendDoors[v] {
			admitted++
		}
	}
	if admitted != 4 {
		t.Fatalf("%d doors lend; the ledger names 4", admitted)
	}
}

// TestTheLendGateNeedsAnOperatorAndAPerson: past the door, a lend needs the
// person to be an operator or manager of the team (a viewer and a non-member
// get ErrLendRole); the team acting principal (a team-owned chief's own
// token, ViaAgent) gets ErrLendTeamPrincipal; an UNLEND needs no role. Each refusal comes before the store (none is wired:
// an admitted call reaches ErrLendOff, the control).
func TestTheLendGateNeedsAnOperatorAndAPerson(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	ms, err := authz.NewMemberships([]authz.Membership{{Team: 12, Role: authz.TeamViewer}, {Team: 13, Role: authz.TeamOperator},
		{Team: 14, Role: authz.TeamManager}})
	if err != nil {
		t.Fatal(err)
	}
	person := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 7, Role: authz.RoleMember, Via: authz.ViaSession, Teams: ms})
	for team, want := range map[int64]error{12: ErrLendRole, 99: ErrLendRole, 13: ErrLendOff, 14: ErrLendOff, 0: ErrLendOff} {
		if _, err := s.lendHostSpawn(person, 1, team); !errors.Is(err, want) {
			t.Errorf("a person lending to team %d: %v; want %v", team, err, want)
		}
	}
	team := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 13, Team: true, Role: authz.RoleMember, Via: authz.ViaAgent})
	for _, to := range []int64{13, 0} {
		if _, err := s.lendHostSpawn(team, 1, to); !errors.Is(err, ErrLendTeamPrincipal) {
			t.Errorf("the team acting principal lending to %d: %v; want ErrLendTeamPrincipal", to, err)
		}
	}
}

// draftOnlyStore is a plugins.Store that holds one draft and records whether
// a decision reached it (every other method is the nil interface's: a call
// panics, so the test sees anything else the path reaches).
type draftOnlyStore struct {
	plugins.Store
	d       plugins.Draft
	decided bool
}

func (f *draftOnlyStore) Draft(_ context.Context, id int64) (plugins.Draft, error) {
	if id != f.d.ID {
		return plugins.Draft{}, plugins.ErrNotFound
	}
	return f.d, nil
}

func (f *draftOnlyStore) DecideDraftFrom(context.Context, int64, plugins.DraftDecision) (plugins.Draft, error) {
	f.decided = true
	return plugins.Draft{}, plugins.ErrNotFound
}

// TestATeamsDraftIsDecidedByAnotherManager: decideDraftAs — the one decision
// the Asks page and plugin_draft_decide both make — refuses, BEFORE the
// store, the team itself deciding its own draft (a team officer's runtime or
// agent), a team member below manager, a manager approving a draft they
// caused, and a manager whose door carries no teams (their personal API
// token, their agent). CONTROLS reach the store (decided): another manager's
// approval, the requester's own REJECTION, and a person deciding their own
// personal draft as today.
func TestATeamsDraftIsDecidedByAnotherManager(t *testing.T) {
	const team, mgr, mgr2, op, person = 12, 7, 8, 9, 10
	mgrMS, _ := authz.NewMemberships([]authz.Membership{{Team: team, Role: authz.TeamManager}})
	opMS, _ := authz.NewMemberships([]authz.Membership{{Team: team, Role: authz.TeamOperator}})
	as := func(uid int64, via authz.Via, ms authz.Memberships) context.Context {
		return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: via, Teams: ms})
	}
	teamActing := authz.WithPrincipal(context.Background(), authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: authz.ViaAgent})
	byTeam, byMgr, byPerson := int64(team), int64(mgr), int64(person)
	officerDraft := plugins.Draft{ID: 1, OwnerID: team, Status: plugins.DraftPending, RequestedByUser: &byTeam}
	mgrDraft := plugins.Draft{ID: 1, OwnerID: team, Status: plugins.DraftPending, RequestedByUser: &byMgr}
	ownDraft := plugins.Draft{ID: 1, OwnerID: person, Status: plugins.DraftPending, RequestedByUser: &byPerson}
	for _, c := range []struct {
		what    string
		ctx     context.Context
		d       plugins.Draft
		approve bool
		refusal string // "" = reaches the store
	}{
		{"the team approving its own officer's draft", teamActing, officerDraft, true, DraftTeamSelfDecide},
		{"the team rejecting its own officer's draft", teamActing, officerDraft, false, DraftTeamSelfDecide},
		{"an operator approving", as(op, authz.ViaSession, opMS), officerDraft, true, DraftTeamNotManager},
		{"a manager through their personal API token", as(mgr2, authz.ViaToken, mgrMS), officerDraft, true, DraftTeamNotManager},
		{"a manager through their agent", as(mgr2, authz.ViaAgent, mgrMS), officerDraft, true, DraftTeamNotManager},
		{"a manager through their mirrored session", as(mgr2, authz.ViaMirrorHost, mgrMS), officerDraft, true, DraftTeamNotManager},
		{"the manager who caused it, approving", as(mgr, authz.ViaSession, mgrMS), mgrDraft, true, DraftTeamOwnRequest},
		{"control: another manager approving the officer's draft", as(mgr2, authz.ViaSession, mgrMS), officerDraft, true, ""},
		{"control: another manager approving the manager's draft", as(mgr2, authz.ViaSession, mgrMS), mgrDraft, true, ""},
		{"control: the requester rejecting it", as(mgr, authz.ViaSession, mgrMS), mgrDraft, false, ""},
		{"control: a person approving their own personal draft", as(person, authz.ViaSession, ""), ownDraft, true, ""},
		{"control: a person's own draft through their mirrored session", as(person, authz.ViaMirrorHost, ""), ownDraft, true, ""},
	} {
		st := &draftOnlyStore{d: c.d}
		s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
		s.ext.Plugins = st
		_, refused := s.decideDraftAs(c.ctx, 1, plugins.DraftDecision{Approve: c.approve})
		switch {
		case c.refusal == "" && !st.decided:
			t.Errorf("%s: refused (%v) before the store; want it decided", c.what, refused)
		case c.refusal != "" && (st.decided || refused == nil || refused.msg != c.refusal || refused.code != http.StatusForbidden):
			t.Errorf("%s: decided %v, refusal %+v; want 403 %q before the store", c.what, st.decided, refused, c.refusal)
		}
	}
}

// TestTheDatabaseApproverRefusalIsA403: a 42501 from 0130's draft_approver
// (its HINT) is the requester refusal, 403 — not a 500 — for a race past
// draftDeciderRefusal. CONTROL: another 42501 stays a 500.
func TestTheDatabaseApproverRefusalIsA403(t *testing.T) {
	if r := draftDecisionRefusal(plugins.Draft{}, pgErrWithHint("42501", draftApproverHint), true); r.code != http.StatusForbidden || r.msg != DraftTeamOwnRequest {
		t.Errorf("draft_approver's refusal answered %+v; want 403 %q", r, DraftTeamOwnRequest)
	}
	if r := draftDecisionRefusal(plugins.Draft{}, pgErrWithHint("42501", ""), true); r.code != http.StatusInternalServerError {
		t.Errorf("control: an unhinted 42501 answered %+v; want 500", r)
	}
	if !strings.Contains(DraftTeamSelfDecide, "manager") {
		t.Fatal("the self-decide refusal names nobody to ask")
	}
}

// pgErrWithHint is a Postgres error with code and hint, as pgx returns one.
func pgErrWithHint(code, hint string) error { return &pgconn.PgError{Code: code, Hint: hint} }

// VisibleServerApps: this fake has no server apps (every page's app tabs read
// it since core PR 1h.2).
func (draftOnlyStore) VisibleServerApps(context.Context) ([]plugins.Plugin, error) { return nil, nil }
