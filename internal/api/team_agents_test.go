package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http/httptest"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// Team sharing phase 4, slice 4.4 (team_agents.go): the unit legs. The
// database legs (the role walk, the turn's principal and secret, the removed
// member's queued message) are team_agents_pg_test.go.

const (
	tsTeam     int64 = 900
	tsOperator int64 = 901
	tsViewer   int64 = 902
	tsOutsider int64 = 903
)

// tsUsers is a users.Store over fixed accounts: tsTeam is a team, the rest
// persons with their memberships in it.
type tsUsers map[int64]users.Account

func (u tsUsers) Role(_ context.Context, id int64) (authz.Role, error) {
	if a, ok := u[id]; ok {
		return a.Role, nil
	}
	return "", users.ErrNotFound
}

func (u tsUsers) Account(_ context.Context, id int64) (users.Account, error) {
	if a, ok := u[id]; ok {
		return a, nil
	}
	return users.Account{}, users.ErrNotFound
}

func newTSUsers() tsUsers {
	return tsUsers{
		tsTeam:     {Role: authz.RoleMember, Team: true},
		tsOperator: {Role: authz.RoleMember, Teams: tsIn(authz.TeamOperator)},
		tsViewer:   {Role: authz.RoleMember, Teams: tsIn(authz.TeamViewer)},
		tsOutsider: {Role: authz.RoleMember},
	}
}

// tsIn is the memberships of a person holding role in tsTeam.
func tsIn(role authz.TeamRole) authz.Memberships {
	m, err := authz.NewMemberships([]authz.Membership{{Team: tsTeam, Role: role}})
	if err != nil {
		panic(err)
	}
	return m
}

// personCtx is uid's browser session as admit builds it.
func personCtx(u tsUsers, uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession, Teams: u[uid].Teams})
}

// tsAgents is an agents store whose List is exactly its rows (as a
// member's list reads under 0131's row-level security: their own agents AND
// their teams').
type tsAgents struct {
	stubAgentsStore
	mu   sync.Mutex
	rows []agents.Agent
	msgs []agents.ChatMessage
}

func (s *tsAgents) List(context.Context) ([]agents.Agent, error) {
	return append([]agents.Agent(nil), s.rows...), nil
}

func (s *tsAgents) LastMessageByAgentIDs(context.Context, []int64) (map[int64]time.Time, error) {
	return map[int64]time.Time{}, nil
}

func (s *tsAgents) Get(_ context.Context, id int64) (agents.Agent, error) {
	for _, a := range s.rows {
		if a.ID == id {
			return a, nil
		}
	}
	return agents.Agent{}, pgx.ErrNoRows
}

func (s *tsAgents) AddChatMessage(_ context.Context, m agents.ChatMessage) (agents.ChatMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.ID = int64(len(s.msgs) + 1)
	s.msgs = append(s.msgs, m)
	return m, nil
}

func teamChiefRow() agents.Agent {
	return agents.Agent{ID: 40, Name: agents.ChiefNameFor(tsTeam), DisplayName: ChiefDisplayName, OwnerID: tsTeam, Kind: agents.KindClaudeCode}
}

// chiefSwitchedOn is a chiefpref.Store in which every owner turned the chief on.
type chiefSwitchedOn struct{}

func (chiefSwitchedOn) Choice(context.Context, int64) (bool, bool, error) { return true, true, nil }
func (chiefSwitchedOn) SetEnabled(context.Context, int64, bool) error     { return nil }

// TestAMemberWithNoChiefIsNotHandedTheTeamsChief: since 0131 a member's list
// holds their team's chief. Two members must not be handed it:
//
//   - a member who switched the chief ON (a stored choice) and has no chief
//     of their own: chiefAgent answers ErrNoChief, not the team's chief;
//   - a member whose OWN chief has a higher id than the team's: chiefAgent
//     answers their own, not the lowest-id one labelled chief.
//
// CONTROL: under the TEAM ACTING PRINCIPAL the team's chief resolves (the
// seam phase 3's plugin delivery uses).
//
// 🔴 RED at origin/main 006bbe5c: both members were handed the team chief
// (lowest-id agent labelled chief among everything listed). A member with no
// chief AND no stored choice was already safe at main (chiefOn answered off:
// ErrChiefOff), which is why the stored choice is part of the fixture.
func TestAMemberWithNoChiefIsNotHandedTheTeamsChief(t *testing.T) {
	u := newTSUsers()
	store := &tsAgents{rows: []agents.Agent{teamChiefRow(),
		{ID: 41, Name: "keen-wren", DisplayName: "keen-wren", OwnerID: tsOperator, Kind: agents.KindClaudeCode}}}
	s := &Server{logger: log.New(io.Discard, "", 0), ext: Extensions{Agents: store, Users: u, ChiefPrefs: chiefSwitchedOn{}}}
	member := personCtx(u, tsOperator)
	if got, err := s.chiefAgent(member); err == nil || got.ID != 0 {
		t.Errorf("a member with the chief switched on and none of their own is handed %q (owner %d), %v; want ErrNoChief",
			got.Name, got.OwnerID, err)
	}
	teamCtx := authz.WithPrincipal(context.Background(), teamActingPrincipal(tsTeam, authz.ViaAgent))
	if got, err := s.chiefAgent(teamCtx); err != nil || got.ID != 40 {
		t.Errorf("control: under the team acting principal the chief is %q, %v; want the team's chief", got.Name, err)
	}
	store.rows = append(store.rows, agents.Agent{ID: 99, Name: agents.ChiefNameFor(tsOperator), DisplayName: ChiefDisplayName, OwnerID: tsOperator, Kind: agents.KindClaudeCode})
	if got, err := s.chiefAgent(member); err != nil || got.ID != 99 {
		t.Errorf("a member whose own chief (id 99) is newer than the team's (id 40) is handed %q, %v; want their own", got.Name, err)
	}
}

// TestHomeDoesNotOpenTheTeamsChief: `/` (homeTarget, primaryChief) opens the
// member's OWN chief — newer than the team's — never the team's, which their
// list holds since 0131.
//
// ⚠ With NO chief of their own, home falls through to the most recent
// session, which may be the team chief's page: that is a visible team agent
// opened as a session, not handed over as the member's chief, and is left
// as it is (plan §3.1's "home" fallback is a later slice).
//
// 🔴 RED at origin/main 006bbe5c: home opened the team chief (lowest id).
func TestHomeDoesNotOpenTheTeamsChief(t *testing.T) {
	u := newTSUsers()
	store := &tsAgents{rows: []agents.Agent{teamChiefRow()}}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: store, Users: u, ChiefPrefs: chiefSwitchedOn{}})
	r := httptest.NewRequest("GET", "/", nil).WithContext(personCtx(u, tsOperator))
	store.rows = append(store.rows, agents.Agent{ID: 99, Name: agents.ChiefNameFor(tsOperator), DisplayName: ChiefDisplayName, OwnerID: tsOperator, Kind: agents.KindClaudeCode})
	if href, err := s.homeTarget(r, homeExcept{}); err != nil || !strings.Contains(href, agents.ChiefNameFor(tsOperator)) {
		t.Errorf("home is %q (%v); want the member's own chief %s, not the team's", href, err, agents.ChiefNameFor(tsOperator))
	}
}

// tsOutcomes is an agentoutcome.Store that records Told.
type tsOutcomes struct {
	agentoutcome.Store
	mu   sync.Mutex
	told []int64
}

func (o *tsOutcomes) Told(_ context.Context, id int64, _ *time.Time, _ time.Time) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.told = append(o.told, id)
	return true, nil
}

// TestATeamOperatorOpeningATeamAgentSeesItsWait: a team agent waiting for its
// owner is "seen" (outcomeViewed: Told) when an operator of the team opens its
// page; a viewer's look (who cannot answer it) and an outsider's leave it owed.
// CONTROL: a person's own agent opened by its owner is seen.
//
// 🔴 RED at origin/main 006bbe5c: only requestOwner == owner counted, which no
// person ever is for a team.
func TestATeamOperatorOpeningATeamAgentSeesItsWait(t *testing.T) {
	u := newTSUsers()
	waiting := func(id, owner int64) agents.Agent {
		return agents.Agent{ID: id, Name: "w", OwnerID: owner, Outcome: agentoutcome.Waiting}
	}
	for who, c := range map[string]struct {
		uid   int64
		agent agents.Agent
		want  bool
	}{
		"the team's operator":       {tsOperator, waiting(50, tsTeam), true},
		"the team's viewer":         {tsViewer, waiting(50, tsTeam), false},
		"an outsider":               {tsOutsider, waiting(50, tsTeam), false},
		"control: a person, theirs": {tsOutsider, waiting(51, tsOutsider), true},
	} {
		rec := &tsOutcomes{}
		s := &Server{logger: log.New(io.Discard, "", 0), now: time.Now, ext: Extensions{Agents: &tsAgents{}, Users: u, Outcomes: rec}}
		s.outcomeViewed(personCtx(u, c.uid), c.agent)
		if got := len(rec.told) == 1; got != c.want {
			t.Errorf("%s opening the agent: seen %v; want %v", who, got, c.want)
		}
	}
}

// tsToolGateway records how many tools each turn carried, and whether it
// went out toolless.
type tsToolGateway struct {
	mu        sync.Mutex
	withTools []int
	toolless  int
}

func (g *tsToolGateway) Chat(context.Context, agents.Agent, string, string, func(string)) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.toolless++
	return "plain", nil
}

func (g *tsToolGateway) ChatWithTools(_ context.Context, _ agents.Agent, _, _, _ string, tools []agents.ToolDef, _ agents.ToolDispatch, _ agents.StreamEmit) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.withTools = append(g.withTools, len(tools))
	return "tooled", nil
}

// TestAnAnswerDeliveredToATeamGatewayAgentCarriesNoTools closes the gap carried
// from slice 4.1: answer delivery (deliverAnswer → runDeliveredTurn → chatTurn
// → turnTools) is a turn path turnPrincipal's team refusal never sees. A
// team-owned GATEWAY agent's delivered answer runs under the team acting
// principal (attentionPrincipal of a team) and must go out toolless. CONTROL:
// the same delivery to a person's gateway agent carries its tools.
//
// 🔴 RED at origin/main 006bbe5c: the team gateway agent's delivered turn
// carried the full worker tool set.
func TestAnAnswerDeliveredToATeamGatewayAgentCarriesNoTools(t *testing.T) {
	u := newTSUsers()
	for who, c := range map[string]struct {
		owner     int64
		wantTools bool
	}{
		"a team's gateway agent":            {tsTeam, false},
		"control: a person's gateway agent": {tsOperator, true},
	} {
		gw := &tsToolGateway{}
		s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
		s.UseExtensions(Extensions{Notes: stubNotesStore{}, Gateway: gw, Agents: &tsAgents{}, Users: u})
		a := agents.Agent{ID: 60, Name: "gw-agent", OwnerID: c.owner, Kind: agents.KindGateway, Status: agents.StatusRunning}
		ctx := s.attentionPrincipal(context.Background(), c.owner)
		if _, err := s.runDeliveredTurn(ctx, a, agents.ChatSession{ID: 1, SessionKey: "k"}, "the answer", nil); err != nil {
			t.Fatalf("%s: delivered turn: %v", who, err)
		}
		tooled := len(gw.withTools) == 1 && gw.withTools[0] > 0
		if tooled != c.wantTools {
			t.Errorf("%s: the delivered turn carried tools %v (toolless calls %d, tool counts %v); want tools %v",
				who, tooled, gw.toolless, gw.withTools, c.wantTools)
		}
	}
}

// TestEveryAgentRouteHasATeamRule is teamAgentRoutes' ledger: every route in
// routes.golden whose pattern names an agent ("agents/{") has a rule, and
// every rule is a registered route. A new agent route fails here until someone
// decides what a team member may do through it.
func TestEveryAgentRouteHasATeamRule(t *testing.T) {
	b, err := os.ReadFile("testdata/routes.golden")
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	var named []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		registered[l] = true
		if strings.Contains(l, "agents/{") {
			named = append(named, l)
		}
	}
	if len(named) < 20 {
		t.Fatalf("instrument check FAILED: %d agent routes found in routes.golden", len(named))
	}
	for _, r := range named {
		if _, ok := teamAgentRoutes[r]; !ok {
			t.Errorf("UNCLASSIFIED agent route %s: add it to teamAgentRoutes (team_agents.go) with what a team member may do through it", r)
		}
	}
	for r := range teamAgentRoutes {
		if !registered[r] {
			t.Errorf("teamAgentRoutes lists %s, which is not a registered route", r)
		}
	}
}

// agentsListCallers is EVERY function in internal/api that calls
// Agents.List, with what its list means once 0131 shares team agents. It fails
// when the set grows OR shrinks: a new caller decides here whether it treats
// "every agent I can see" as "mine".
//
//   - own:     it filters to one owner itself (ownedBy, or OwnerID == owner).
//   - visible: it shows or matches every agent the viewer may see, team agents
//     included, on purpose — or uses the per-owner chiefPredicate.
//   - machine: it runs only under a principal that carries NO team
//     memberships (an agent's or a hook/API token's door, an officer, a
//     background loop, or System) — a team agent is in its list only when the
//     principal IS that team.
var agentsListCallers = map[string]string{
	"Server.chiefAgent":             "own: resolveChief(ownedBy(list, requestOwner(ctx)))",
	"Server.chiefOnFor":             "own: chiefOn reads ownedBy(list, owner)",
	"Server.accountChiefSection":    "own: chiefToggleAvailable / primaryChief over ownedBy(list, owner)",
	"Server.handleAccountChief":     "own: primaryChief(ownedBy(list, owner))",
	"Server.homeTarget":             "own: primaryChief(ownedBy(list, requestOwner(ctx)))",
	"Server.toolChiefRestart":       "own: resolveChief(ownedBy(list, owner))",
	"Server.reapplyComposeChief":    "own: ownedBy(list, owner)",
	"Server.liveOnTask":             "own: x.OwnerID == owner",
	"Server.mentionAsker":           "own: a.OwnerID == owner",
	"Server.replaceChief":           "own: by the owner's reserved slug ChiefNameFor(owner); hook-token door",
	"Server.agentsListEntries":      "visible: the sidebar lists team agents; primaryChief over ownedBy(list, viewer)",
	"Server.paletteTargets":         "visible: the palette searches team agents",
	"Server.mentionIndex":           "visible: a team agent's name is linked",
	"Server.actorIsAgent":           "visible: a team agent's name is an agent's",
	"Server.agentIsChief":           "visible: chiefPredicate is per owner",
	"Server.agentIsDeleteProtected": "visible: deleteProtection is per owner",
	"Server.agentPageCard":          "visible: chiefPredicate is per owner",
	"Server.outcomeIsChief":         "visible: chiefPredicate is per owner (outcome watchdog, System)",
	"Server.handleAPIAgentList":     "machine: GET /api/agents, hook and API tokens",
	"Server.AgentWatchdogPass":      "machine: System",
	"Server.stallFacts":             "machine: chief nudge sweep, per person, no teams",
	"Server.collectFleet":           "machine: the fleet report, an agent's tool",
	"Server.createPoolJobOnce":      "machine: worker pool jobs, agent and API tokens",
	"Server.liveSessions":           "machine: sessions_overview, an agent's tool",
	"Server.consumerUsageInstances": "machine: consumer usage principal",
	"Server.toolFleetList":          "machine: an agent's tool",
	"Server.focusSearchHistory":     "machine: an agent's tool",
	"Server.toolAgentsList":         "machine: an agent's tool",
	"officerFleet.byName":           "machine: an officer's door",
	"focusSessions.Candidates":      "machine: the focus refresher, ViaFocus",
}

func TestEveryAgentsListCallerIsClassified(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				rt := fn.Recv.List[0].Type
				if st, ok := rt.(*ast.StarExpr); ok {
					rt = st.X
				}
				if id, ok := rt.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "List" {
					return true
				}
				if x, ok := sel.X.(*ast.SelectorExpr); ok && x.Sel.Name == "Agents" {
					found[name]++
				}
				return true
			})
		}
	}
	if len(found) < 25 {
		t.Fatalf("instrument check FAILED: the walk found %d Agents.List callers: %v", len(found), found)
	}
	var unclassified, gone []string
	for fn := range found {
		if _, ok := agentsListCallers[fn]; !ok {
			unclassified = append(unclassified, fn)
		}
	}
	for fn := range agentsListCallers {
		if found[fn] == 0 {
			gone = append(gone, fn)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(gone)
	if len(unclassified) > 0 {
		t.Errorf("UNCLASSIFIED Agents.List callers %v: since 0131 a member's list holds their teams' agents; "+
			"classify each in agentsListCallers (own / visible / machine), filtering with ownedBy if it means \"mine\"", unclassified)
	}
	if len(gone) > 0 {
		t.Errorf("agentsListCallers lists %v, which no longer call Agents.List: remove them", gone)
	}
}
