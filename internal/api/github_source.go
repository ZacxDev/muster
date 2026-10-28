package api

import (
	"context"
	"errors"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/github"
)

// muster's GitHub IDENTITY, decided in ONE place.
//
// 🔴 EVERY READER OF "WHICH REPOSITORIES CAN muster SEE" GOES THROUGH HERE: the
// dispatch dialog's repo picker, the task-directory inference,
// the default-branch lookup, and connectedRepo — the check behind every
// claude-code dispatch door and task_dispatch's effective repository. They were
// five open-coded copies once; a source added at four of them and missed at the
// fifth would leave one door checking a different set of repositories.
//
// THE SOURCE: the configured GitHub App (Extensions.GitHubApp) — the identity
// the `github` connector's tokens come from, so a repository it cannot see is
// one no agent could clone. There is no other: the per-user OAuth connection
// that used to be the fallback was removed (its `github_connection` table is
// left in the schema, unread and unwritten, for a later migration to drop).
//
// 🔴 THE APP IS THE ADMIN'S (githubApp). For a member principal, and on a
// deployment with no App, there is NO GitHub identity: the repository list,
// dispatch checks and default branches all answer errNoGitHub — the same rule
// connectors.WithVirtual applies to the `github` connector, so a member can
// neither dispatch into an App repository nor have an agent reach one.

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
// admin), else nil. THE gate every App reader in this package goes through.
func (s *Server) githubApp(ctx context.Context) GitHubApp {
	if s.ext.GitHubApp == nil || !authz.IsAdmin(ctx) {
		return nil
	}
	return s.ext.GitHubApp
}

// githubAgentProblem is the GitHub App's operator-facing reason it cannot serve
// a's repository now ("" when no App is configured, a has no repository, or
// nothing is known to be wrong). In memory: no network call per card.
//
// Only for an agent whose OWNER is an admin (the App's users); the role is read
// only when there is a problem to show.
func (s *Server) githubAgentProblem(a agents.Agent) string {
	if s.ext.GitHubApp == nil || a.Repo == "" {
		return ""
	}
	msg := s.ext.GitHubApp.Problem(a.ID)
	if msg == "" || s.roleOf(context.Background(), agentOwnerID(a)) != authz.RoleAdmin {
		return ""
	}
	return msg
}
