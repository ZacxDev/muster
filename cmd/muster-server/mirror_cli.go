package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/users"
)

// mirrorCLIUsage is `muster-server mirror`'s help.
const mirrorCLIUsage = `usage:
  muster-server mirror token create <host>               mint a mirror token for host (printed ONCE)
  muster-server mirror token create <host> --team <id>   mint a TEAM host token: the host's sessions are team <id>'s
  muster-server mirror token ls                          list tokens (never the tokens themselves)
  muster-server mirror token revoke <id>                 revoke a token; a live connection using it ends within 30s

A team's managers mint, revoke and rotate its host tokens on /teams/<id>/hosts;
--team is the headless bootstrap of the same token, recorded in the team's audit
log (via cli).

Reads the server's own database (` + envDatabase + `). Run it where the server runs,
e.g. kubectl exec deploy/muster -c muster -- /muster-server mirror token create <host>.
`

// runMirrorCLI is `muster-server mirror …`: the operator's mirror-token admin.
//
// 🔴 IT IS ON THE SERVER BINARY, NOT THE `muster` CLI, DELIBERATELY. The muster
// CLI is a machine client of the HTTP API on the shared hook token; a token
// minted over that tier would let anything holding the hook token create a
// credential that reaches an operator's terminals. This path needs the
// database's own credential — i.e. access to wherever the server runs — and adds
// no HTTP route at all.
func runMirrorCLI(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) < 2 || args[0] != "token" {
		fmt.Fprint(stderr, mirrorCLIUsage)
		return 2
	}
	dsn := strings.TrimSpace(getenv(envDatabase))
	if dsn == "" {
		fmt.Fprintf(stderr, "muster-server mirror: %s is unset\n", envDatabase)
		return 2
	}
	// 🔴 AS THE OPERATOR (user 1). mirror_tokens is under row-level security
	// (migration 0008): a token minted here is owned by the operator, so its
	// host's sessions are visible to the operator's principal and nobody else's,
	// and `ls` / `revoke` see the operator's tokens only.
	ctx, cancel := context.WithTimeout(authz.Operator(ctx), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(stderr, "muster-server mirror: connect: %v\n", err)
		return 1
	}
	defer pool.Close()
	store := mirror.NewPG(pool)

	switch args[1] {
	case "create":
		if len(args) == 5 && args[3] == "--team" {
			return mirrorCreateTeamToken(ctx, store, args[2], args[4], stdout, stderr)
		}
		if len(args) != 3 {
			fmt.Fprint(stderr, mirrorCLIUsage)
			return 2
		}
		tok, rec, err := store.CreateToken(ctx, args[2])
		if err != nil {
			fmt.Fprintf(stderr, "muster-server mirror: create: %v\n", err)
			return 1
		}
		// The token goes to stdout and this note to stderr, so the note must
		// not point AT the next line: with stdout in a file (INSTALL.md mints
		// straight into one) nothing follows it on the screen.
		fmt.Fprintf(stderr, "minted mirror token %d for host %s (owner: user %d). The token is printed ONCE, on stdout; keep it in that host's muster-mirror token file.\n", rec.ID, rec.Host, rec.OwnerID)
		fmt.Fprintln(stdout, tok)
		return 0
	case "ls":
		list, err := store.ListTokens(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "muster-server mirror: ls: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "%-6s %-24s %-8s %-25s %s\n", "ID", "HOST", "STATE", "CREATED", "LAST USED")
		for _, t := range list {
			state, used := "live", "never"
			if t.RevokedAt != nil {
				state = "revoked"
			}
			if t.LastUsed != nil {
				used = t.LastUsed.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(stdout, "%-6d %-24s %-8s %-25s %s\n", t.ID, t.Host, state, t.CreatedAt.UTC().Format(time.RFC3339), used)
		}
		return 0
	case "revoke":
		if len(args) != 3 {
			fmt.Fprint(stderr, mirrorCLIUsage)
			return 2
		}
		id, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			fmt.Fprintf(stderr, "muster-server mirror: revoke: %q is not a token id (see `mirror token ls`)\n", args[2])
			return 2
		}
		ok, err := store.RevokeToken(ctx, id)
		if err != nil {
			fmt.Fprintf(stderr, "muster-server mirror: revoke: %v\n", err)
			return 1
		}
		if !ok {
			fmt.Fprintf(stderr, "muster-server mirror: token %d does not exist or is already revoked\n", id)
			return 1
		}
		fmt.Fprintf(stdout, "revoked mirror token %d\n", id)
		return 0
	}
	fmt.Fprint(stderr, mirrorCLIUsage)
	return 2
}

// mirrorCreateTeamToken is `muster-server mirror token create <host> --team
// <id>`: a TEAM host token (team sharing 2b.2), minted under authz.System —
// the server's shell can already do anything, so this grants nothing new — as
// team <id>'s, with the team's audit row (team.host.token.create, via cli) in
// the mint's own transaction (mirror.PGStore.CreateTeamToken). An id that
// names no team mints nothing.
func mirrorCreateTeamToken(ctx context.Context, store *mirror.PGStore, host, teamArg string, stdout, stderr io.Writer) int {
	team, err := strconv.ParseInt(teamArg, 10, 64)
	if err != nil || team <= 0 {
		fmt.Fprintf(stderr, "muster-server mirror: create: --team takes a team id (see /admin/teams), not %q\n", teamArg)
		return 2
	}
	tok, rec, err := store.CreateTeamToken(authz.System(ctx), users.Actor{Via: "cli"}, team, host)
	if errors.Is(err, mirror.ErrNotATeam) {
		fmt.Fprintf(stderr, "muster-server mirror: create: %d is not a team (see /admin/teams)\n", team)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "muster-server mirror: create: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "minted TEAM host token %d for host %s (owner: team %d). The token is printed ONCE, on stdout; keep it in that host's muster-mirror token file, as the Unix user the team shares.\n", rec.ID, rec.Host, rec.OwnerID)
	fmt.Fprintln(stdout, tok)
	return 0
}
