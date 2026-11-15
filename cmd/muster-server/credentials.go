package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/credentials"
)

// buildCredentialPlane builds the ONE decision of whose model-provider
// credentials an owner's agents run on (credentials.Plane): the environment's
// for an admin, the owner's own — and only those — for anyone else.
//
// personal and roles may be nil (no credential store / no database). With no
// personal store every member simply HAS NO credential: their kinds are
// unavailable, never served from the environment.
//
// ⚠ CALLERS PASS A CONCRETE POINTER ONLY WHEN IT IS NON-nil. A typed nil inside
// the interface would make the plane's nil checks false (buildGatewayWith's
// doc has the same trap).
func buildCredentialPlane(cfg config, personal credentials.Personal, roles credentials.RoleReader, logger *log.Logger) (*credentials.Plane, error) {
	p := &credentials.Plane{
		EnvOpenRouterKey: cfg.AgentOpenRouterKey,
		Personal:         personal,
		Roles:            roles,
		// Every id is a team: checkTeamsOnServerPool refused boot otherwise.
		TeamsOnServerPool: cfg.TeamsOnServerPool,
		Logger:            logger,
	}
	// The Compose chief with no MUSTER_CHIEF_CLAUDE_TOKEN has no account at all:
	// it runs on the OpenRouter key (composeChiefCredential), and an empty
	// static pool is refused by design (ccpool.NewStatic).
	if cfg.claudeCodeEnabled() && !(cfg.chiefCompose() && len(cfg.AgentCCTokens) == 0) {
		env, err := ccpool.NewStatic(cfg.AgentCCTokens)
		if err != nil {
			return nil, err
		}
		p.EnvClaude = env
	}
	return p, nil
}

// credResolver is agentprovision.CredentialResolver over the account pool and
// the plane: a pod's Claude token is the pool's token for the agent's OWNER and
// account; its OpenRouter key is the plane's answer for that owner.
type credResolver struct {
	pool  *ccpool.Pool
	plane *credentials.Plane
}

func (r credResolver) ClaudeToken(ctx context.Context, owner int64, account string) (string, error) {
	if r.pool == nil {
		return "", errors.New("the claude-code kind is not enabled on this deployment")
	}
	return r.pool.Token(ctx, owner, account)
}

func (r credResolver) OpenRouterKey(ctx context.Context, owner int64) (string, error) {
	return r.plane.OpenRouterKey(ctx, owner)
}

// credentialPage is api.CredentialStore: the per-owner store, with the plane's
// "does this user run on the environment's credentials" beside it.
type credentialPage struct {
	*credentials.PGStore
	plane *credentials.Plane
}

func (c credentialPage) UsesEnvironment(ctx context.Context, owner int64) bool {
	return c.plane.UsesEnvironment(ctx, owner)
}

// teamNamer is the users store's read of which ids are teams
// (users.PGStore.TeamNames: the system scope, teams only).
type teamNamer interface {
	TeamNames(ctx context.Context, ids []int64) (map[int64]string, error)
}

// checkTeamsOnServerPool is MUSTER_TEAMS_ON_SERVER_POOL's boot refusal: every
// listed id must be a TEAM (users.kind = 'team'). 🔴 A PERSON'S ID, OR ONE THAT
// NAMES NOBODY, REFUSES TO BOOT: the plane lends the server's Claude pool to
// every id in the list (credentials.Plane.TeamsOnServerPool), so a typo naming
// a member would build that member's pods on the admin's subscription. names
// nil (no database) with a non-empty list refuses too: nothing could check it.
// It returns the teams' names for the boot line.
func checkTeamsOnServerPool(ctx context.Context, names teamNamer, ids map[int64]bool) (map[int64]string, error) {
	return checkTeamList(ctx, names, ids, envTeamsOnServerPool, "the server's Claude pool")
}

// checkTeamsOnGitHubApp is MUSTER_TEAMS_ON_GITHUB_APP's boot refusal, the
// same rule as checkTeamsOnServerPool's (checkTeamList): 🔴 the list lends the
// deployment's GitHub App — its repositories and the `github` connector's
// tokens (githubapp.Admits) — to every id in it, so a person's id would hand
// that member the admin's repositories.
func checkTeamsOnGitHubApp(ctx context.Context, names teamNamer, ids map[int64]bool) (map[int64]string, error) {
	return checkTeamList(ctx, names, ids, envTeamsOnGitHubApp, "the deployment's GitHub App")
}

// checkTeamList is the one "every id is a team" boot check behind the team
// lists (checkTeamsOnServerPool, checkTeamsOnGitHubApp): env names the
// variable, lends what it lends, both for the refusal.
func checkTeamList(ctx context.Context, names teamNamer, ids map[int64]bool, env, lends string) (map[int64]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if names == nil {
		return nil, fmt.Errorf("%s is set but there is no database to check that each id is a team; unset it", env)
	}
	list := make([]int64, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
	got, err := names.TeamNames(ctx, list)
	if err != nil {
		return nil, fmt.Errorf("%s: reading which ids are teams: %w", env, err)
	}
	var bad []string
	for _, id := range list {
		if _, ok := got[id]; !ok {
			bad = append(bad, strconv.FormatInt(id, 10))
		}
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("%s lists %s, which %s not a team (a person, or no one): the list lends %s "+
			"to each id, so it must name teams only. Correct it or unset it", env,
			strings.Join(bad, ", "), map[bool]string{true: "is", false: "are"}[len(bad) == 1], lends)
	}
	return got, nil
}

// teamListLine is a team list's boot line body: "<id> (<name>), …", sorted.
func teamListLine(teams map[int64]string) string {
	ids := make([]string, 0, len(teams))
	for id, name := range teams {
		ids = append(ids, fmt.Sprintf("%d (%s)", id, name))
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}

// teamOwnerLookup is agentprovision.Adapter.UseTeamOwner's lookup over the
// users store: owner's team name, and whether it is a team at all.
func teamOwnerLookup(names teamNamer) func(ctx context.Context, owner int64) (string, bool, error) {
	return func(ctx context.Context, owner int64) (string, bool, error) {
		got, err := names.TeamNames(ctx, []int64{owner})
		if err != nil {
			return "", false, err
		}
		name, ok := got[owner]
		return name, ok, nil
	}
}
