package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// `pr-session` — which of your Claude Code sessions created a pull request, as
// the host's PostToolUse hook reported it (GET /api/pr-sessions, the read over
// session_prs):
//
//	muster pr-session acme/widget#12
//	muster pr-session https://github.com/acme/widget/pull/12
//
// With the ordinary muster credential (a personal token acts as its owner, the
// shared hook token as the machine user; the server answers that owner's rows
// only). stdout is the server's answer with a `resume` command added to each
// row; stderr has the same rows as lines a person reads, each followed by its
// `claude --resume <id>`. No row recorded is exit 4 (exitNotFound) with nothing
// on stdout — distinct from an error (1), a refused credential (3) and an
// unreachable server (6).

const prSessionsRoute = "/api/pr-sessions"

// prRefRepo is a repository as GitHub names one (the server's own pattern,
// prroute.RepoPattern, which this binary does not import).
var prRefRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// prURLPath is the path of a pull request's GitHub URL, anything after the
// number (/files, /commits, …) allowed.
var prURLPath = regexp.MustCompile(`^/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/pull/([0-9]+)(?:/.*)?$`)

// parsePRRef reads "<owner>/<repo>#<n>" or a github.com pull request URL into
// its repo and number.
func parsePRRef(s string) (string, int64, error) {
	s = strings.TrimSpace(s)
	repo, num := "", ""
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
		u, err := url.Parse(s)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
			return "", 0, fmt.Errorf("%q is not a github.com pull request URL", s)
		}
		m := prURLPath.FindStringSubmatch(u.Path)
		if m == nil {
			return "", 0, fmt.Errorf("%q is not a pull request URL (https://github.com/<owner>/<repo>/pull/<n>)", s)
		}
		repo, num = m[1], m[2]
	} else {
		i := strings.LastIndexByte(s, '#')
		if i < 0 {
			return "", 0, fmt.Errorf("%q is not <owner>/<repo>#<n> or a pull request URL", s)
		}
		repo, num = s[:i], s[i+1:]
	}
	if len(repo) < 3 || len(repo) > 200 || !prRefRepo.MatchString(repo) {
		return "", 0, fmt.Errorf("%q is not an <owner>/<repo>", repo)
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n <= 0 {
		return "", 0, fmt.Errorf("%q is not a positive pull request number", num)
	}
	return repo, n, nil
}

func newPRSessionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "pr-session <owner>/<repo>#<n> | <pull request URL>",
		Short: "GET " + prSessionsRoute + " — which of your sessions created a pull request, and how to resume it",
		Long: `Print the Claude Code sessions recorded as creating a pull request (the host's
PostToolUse hook reports every gh pr create), newest first: the session id, the
host whose window showed it, when it was first seen, and the claude --resume
command for it. Your own rows only.

stdout is JSON (each row with a "resume" command added); stderr repeats the
rows as lines. No session recorded for that pull request exits 4.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, n, err := parsePRRef(args[0])
			if err != nil {
				return failf(exitUsage, "%v", err)
			}
			q := url.Values{"repo": {repo}, "pr": {strconv.FormatInt(n, 10)}}
			raw, err := a.call(cmd.Context(), request{method: http.MethodGet, path: prSessionsRoute, query: q, cred: credHook})
			if err != nil {
				return err
			}
			var out struct {
				Repo     string           `json:"repo"`
				PR       int64            `json:"pr"`
				Sessions []map[string]any `json:"sessions"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				return wrapf(exitNonJSON, err, "server response was not the pr-sessions shape")
			}
			if len(out.Sessions) == 0 {
				return failf(exitNotFound, "no session of yours is recorded as creating %s#%d", repo, n)
			}
			for _, s := range out.Sessions {
				id, _ := s["claude_session"].(string)
				s["resume"] = "claude --resume " + id
				host, _ := s["host"].(string)
				if host == "" {
					host = "(no window showed it)"
				}
				first, _ := s["first_seen"].(string)
				kind, _ := s["kind"].(string)
				fmt.Fprintf(a.stderr, "%s  host %s  first_seen %s  kind %s\n  claude --resume %s\n", id, host, first, kind, id)
			}
			return a.emitValue(out)
		},
	}
}
