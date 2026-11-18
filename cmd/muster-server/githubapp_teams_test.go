package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/users"
)

// The variable's name, spelled out (not envTeamsOnGitHubApp) so these tests
// read the same against the code before slice 4.8.
const teamsOnGitHubAppVar = "MUSTER_TEAMS_ON_GITHUB_APP"

// appTeamsFixture is a database with a team T (the operator its manager), a
// person P, a team-owned agent and a person-owned agent, each with its own
// token; and the GitHub App's configuration, with the team list set to list.
type appTeamsFixture struct {
	cfg          config
	team, person int64
	teamTok      string
	personTok    string
}

func newAppTeamsFixture(t *testing.T, list func(team, person int64) string) appTeamsFixture {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`)) // the boot check: no installations
	}))
	t.Cleanup(fake.Close)
	github.UseAPIBaseURL(fake.URL)
	t.Cleanup(func() { github.UseAPIBaseURL("https://api.github.com") })

	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	sys := authz.System(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	f := appTeamsFixture{teamTok: "app-team-agent-token-" + suffix, personTok: "app-person-agent-token-" + suffix}
	team, err := users.NewPG(pool).CreateTeam(sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, "app team "+suffix, authz.OperatorUserID)
	if err != nil {
		t.Fatal(err)
	}
	f.team = team.ID
	if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "app person "+suffix).Scan(&f.person); err != nil {
		t.Fatal(err)
	}
	teamCtx := authz.WithPrincipal(ctx, authz.Principal{UserID: f.team, Team: true, Role: authz.RoleMember, Via: authz.ViaAgent})
	if _, err := agents.NewPG(pool).Create(teamCtx, agents.Agent{Name: "app-team-" + suffix, Namespace: "ns-app-team-" + suffix,
		HooksToken: f.teamTok, Repo: "example-org/widget"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(sys, `INSERT INTO agents (name, namespace, hooks_token, owner_id, repo) VALUES ($1, $2, $3, $4, 'example-org/widget')`,
		"app-person-"+suffix, "ns-app-person-"+suffix, f.personTok, f.person); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(func(n string) string {
		switch n {
		case envGitHubAppID:
			return "12345"
		case envGitHubAppKey:
			return testAppKeyPEM(t)
		case envGitHubAppAccounts:
			return "example-org:4200"
		case teamsOnGitHubAppVar:
			return list(f.team, f.person)
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	base := testConfig()
	base.Database = dsn
	base.ConnectorEncKey = []byte(strings.Repeat("k", 32))
	base.GitHubAppID, base.GitHubAppKey, base.GitHubAppAccounts = cfg.GitHubAppID, cfg.GitHubAppKey, cfg.GitHubAppAccounts
	f.cfg = cfg
	f.cfg.Port, f.cfg.UIPassword, f.cfg.Standalone, f.cfg.RouterActor = base.Port, base.UIPassword, base.Standalone, base.RouterActor
	f.cfg.Database, f.cfg.ConnectorEncKey = base.Database, base.ConnectorEncKey
	f.cfg.AgentConnectURL = "http://muster.example.test:18191" // the agent-facing store needs it
	return f
}

func appConnectorExists(t *testing.T, base, tok string) bool {
	t.Helper()
	code, body := relayCall(t, http.MethodGet, base+"/agent/connectors/github", "", tok)
	if code != http.StatusOK {
		t.Fatalf("GET /agent/connectors/github = %d %s", code, body)
	}
	var v struct{ Exists bool }
	_ = json.Unmarshal([]byte(body), &v)
	return v.Exists
}

// TestBuildAppLendsTheAppToAListedTeamsAgentsOnly is the WIRING seam of
// MUSTER_TEAMS_ON_GITHUB_APP: the binary, with the App configured and the
// team listed, presents the App-backed `github` connector to the TEAM's agent
// (its own token) and not to a person's — a member's — agent.
//
// 🔴 RED before slice 4.8: the variable was not read, and the connector's
// predicate was authz.IsAdmin, so the team's agent saw no github connector.
func TestBuildAppLendsTheAppToAListedTeamsAgentsOnly(t *testing.T) {
	f := newAppTeamsFixture(t, func(team, _ int64) string { return strconv.FormatInt(team, 10) })
	base := startServer(t, f.cfg)
	if !appConnectorExists(t, base, f.teamTok) {
		t.Error("the listed team's agent does not see the App-backed github connector")
	}
	if appConnectorExists(t, base, f.personTok) {
		t.Error("a member's agent sees the App-backed github connector")
	}
}

// TestBootRefusesAPersonOrNoAppInTheTeamGitHubAppList: a person's id in
// MUSTER_TEAMS_ON_GITHUB_APP refuses to boot, naming the variable and the id,
// as MUSTER_TEAMS_ON_SERVER_POOL does — a typo must not hand a member the
// admin's repositories; and a list with no App configured refuses too.
//
// 🔴 RED before slice 4.8: the variable was not read, and both booted.
func TestBootRefusesAPersonOrNoAppInTheTeamGitHubAppList(t *testing.T) {
	f := newAppTeamsFixture(t, func(team, person int64) string {
		return strconv.FormatInt(team, 10) + "," + strconv.FormatInt(person, 10)
	})
	app, err := buildApp(context.Background(), f.cfg, quietLogger())
	if err == nil {
		app.Close()
		t.Fatal("a person's id in the team GitHub App list booted")
	}
	if want := "lists " + strconv.FormatInt(f.person, 10) + ", which is not a team"; !strings.Contains(err.Error(), teamsOnGitHubAppVar) ||
		!strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal: %v; want it to name %s and %q", err, teamsOnGitHubAppVar, want)
	}
	if _, err := loadConfig(func(n string) string {
		if n == teamsOnGitHubAppVar {
			return "12"
		}
		return ""
	}); err == nil || !strings.Contains(err.Error(), teamsOnGitHubAppVar) || !strings.Contains(err.Error(), envGitHubAppID) {
		t.Fatalf("the list with no App: %v; want a boot refusal naming %s and %s", err, teamsOnGitHubAppVar, envGitHubAppID)
	}
}
