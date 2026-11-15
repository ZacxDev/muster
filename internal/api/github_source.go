package api

import (
	"context"
	"errors"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/githubapp"
)

// muster's GitHub IDENTITY, decided in ONE place.
//
// 🔴 EVERY READER OF "WHICH REPOSITORIES CAN muster SEE" GOES THROUGH HERE: the
// dispatch dialog's repo picker, the task-directory inference,
// the default-branch lookup, and connectedRepo — the check behind every
// claude-code dispatch door and agent_dispatch's effective repository. They were
// five open-coded copies once; a source added at four of them and missed at the
// fifth would leave one door checking a different set of repositories.
//
// THE SOURCE: the configured GitHub App (Extensions.GitHubApp) — the identity
// the `github` connector's tokens come from, so a repository it cannot see is
// one no agent could clone. There is no other: the per-user OAuth connection
// that used to be the fallback was removed (migration 0059 drops its
// `github_connection` table).
//
// 🔴 THE APP IS THE ADMIN'S (githubApp), AND THE LISTED TEAMS' (team sharing
// phase 4, slice 4.8: MUSTER_TEAMS_ON_GITHUB_APP). For a member principal, a
// team that is not listed, and on a deployment with no App, there is NO GitHub
// identity: the repository list, dispatch checks and default branches all
// answer errNoGitHub — the same rule (githubapp.Admits) connectors.WithVirtual
// applies to the `github` connector, so a member can neither dispatch into an
// App repository nor have an agent reach one. A listed team's own principal
// (its chief's tool calls, its agents' tokens, its managers' door) can.
//
// 🔴 A BROWSER PAGE ABOUT AN OBJECT DECIDES BY THE OBJECT'S OWNER, NEVER BY
// THE VIEWER (githubAppForOwner): an operator editing a team task sees the
// repositories the TEAM's agents may use, and an admin editing a task of a
// team that is not listed sees none — the dispatch would refuse them.

// errNoGitHub is "no GitHub identity is configured or connected".
var errNoGitHub = errors.New("no GitHub connection")

// githubRepos lists the repositories muster's GitHub identity can see.
// errNoGitHub when there is none; any other error is the listing's.
func (s *Server) githubRepos(ctx context.Context) ([]github.Repo, error) {
	if app := s.githubApp(ctx); app != nil {
		return app.Repos(ctx)
	}
	return nil, errNoGitHub
}

// githubDefaultBranch is repo's default branch as muster's GitHub identity sees it.
func (s *Server) githubDefaultBranch(ctx context.Context, repo string) (string, error) {
	if app := s.githubApp(ctx); app != nil {
		return app.DefaultBranch(ctx, repo)
	}
	return "", errNoGitHub
}

// githubApp is the configured GitHub App when ctx's principal may use it (an
// admin, or a listed team's acting principal: githubapp.Admits), else nil. THE
// gate every App reader in this package goes through.
func (s *Server) githubApp(ctx context.Context) GitHubApp {
	if s.ext.GitHubApp == nil || !githubapp.Admits(s.ext.GitHubAppTeams)(ctx) {
		return nil
	}
	return s.ext.GitHubApp
}

// githubAppForOwner is the configured GitHub App when the OBJECT's owner may
// use it, for a page a person views about something owner owns (a task's
// repository picker): owner is a listed team (MUSTER_TEAMS_ON_GITHUB_APP), or
// owner is the viewer themself and the viewer's own principal is admitted
// (githubApp). Any other owner — a team that is not listed, or someone else —
// has none. The VIEWER'S role never lends it: an admin on a team's object of a
// team that is not listed gets nil.
func (s *Server) githubAppForOwner(ctx context.Context, owner int64) GitHubApp {
	if s.ext.GitHubApp == nil || owner <= 0 {
		return nil
	}
	if s.ext.GitHubAppTeams[owner] {
		return s.ext.GitHubApp
	}
	if p, ok := authz.From(ctx); ok && !p.All() && p.UserID == owner {
		return s.githubApp(ctx)
	}
	return nil
}

// githubReposForOwner is githubRepos decided by the object's owner
// (githubAppForOwner).
func (s *Server) githubReposForOwner(ctx context.Context, owner int64) ([]github.Repo, error) {
	if app := s.githubAppForOwner(ctx, owner); app != nil {
		return app.Repos(ctx)
	}
	return nil, errNoGitHub
}

// githubAgentProblem is the GitHub App's operator-facing reason it cannot serve
// a's repository now ("" when no App is configured, a has no repository, or
// nothing is known to be wrong). In memory: no network call per card.
//
// Only for an agent whose OWNER is an admin or a listed team (the App's
// users); the role is read only when there is a problem to show.
func (s *Server) githubAgentProblem(a agents.Agent) string {
	if s.ext.GitHubApp == nil || s.agentRepo(a) == "" {
		return ""
	}
	msg := s.ext.GitHubApp.Problem(a.ID)
	if msg == "" {
		return ""
	}
	if owner := agentOwnerID(a); !s.ext.GitHubAppTeams[owner] && s.roleOf(context.Background(), owner) != authz.RoleAdmin {
		return ""
	}
	return msg
}
