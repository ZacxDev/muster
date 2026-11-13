package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// `pr-session` (pr_session.go). Fixture values are invented: acme/widget#12 and
// made-up uuids, distinct from each other.

const (
	prSessNew = "00000000-0000-4000-8000-0000000000a1"
	prSessOld = "00000000-0000-4000-8000-0000000000a2"
)

func TestParsePRRef(t *testing.T) {
	ok := map[string]struct {
		repo string
		n    int64
	}{
		"acme/widget#12":                                {"acme/widget", 12},
		"  Acme-Labs/Widget.API#4817 ":                  {"Acme-Labs/Widget.API", 4817},
		"https://github.com/acme/widget/pull/12":        {"acme/widget", 12},
		"https://github.com/acme/widget/pull/12/files":  {"acme/widget", 12},
		"https://GitHub.com/acme/widget/pull/12?w=1#x":  {"acme/widget", 12},
		"http://github.com/acme/widget/pull/9000000001": {"acme/widget", 9000000001},
	}
	for in, want := range ok {
		repo, n, err := parsePRRef(in)
		if err != nil || repo != want.repo || n != want.n {
			t.Errorf("%q: %q %d %v; want %q %d", in, repo, n, err, want.repo, want.n)
		}
	}
	for _, in := range []string{
		"", "acme/widget", "acme/widget#", "acme/widget#0", "acme/widget#-3", "acme/widget#12a", "#12",
		"acme#12", "acme/widget/x#12", "acme wid/get#12",
		"https://github.com/acme/widget/issues/12", "https://github.com/acme/widget/pull/",
		"https://github.com/acme/widget/pull/0", "https://example.com/acme/widget/pull/12",
		"https://github.com/acme/pull/12",
	} {
		if repo, n, err := parsePRRef(in); err == nil {
			t.Errorf("%q: parsed as %q %d, want an error", in, repo, n)
		}
	}
}

func TestPRSessionPrintsEachRowAndItsResumeLine(t *testing.T) {
	for _, arg := range []string{"acme/widget#12", "https://github.com/acme/widget/pull/12/files"} {
		t.Run(arg, func(t *testing.T) {
			h := newHarness(t)
			h.json("GET /api/pr-sessions", http.StatusOK, `{"repo":"acme/widget","pr":12,"sessions":[`+
				`{"claude_session":"`+prSessNew+`","host":"alpha","kind":"gh-pr-create","repo":"acme/widget","pr":12,"head_ref":null,"first_seen":"2000-01-02T12:00:00Z","last_seen":"2000-01-03T12:00:00Z"},`+
				`{"claude_session":"`+prSessOld+`","host":null,"kind":"gh-pr-create","repo":"acme/widget","pr":12,"head_ref":"feat/x","first_seen":"2000-01-01T12:00:00Z","last_seen":"2000-01-01T13:00:00Z"}]}`)
			got := h.runCLI("pr-session", arg)
			if got.code != exitOK {
				t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
			}
			// The request: the parsed repo and number, as query parameters.
			reqs := h.nonHealthRequests()
			if len(reqs) != 1 || reqs[0].method != http.MethodGet || reqs[0].uri != "/api/pr-sessions?pr=12&repo=acme%2Fwidget" {
				t.Fatalf("requests %+v", reqs)
			}
			// stdout: JSON, the rows in the server's order, each with its resume command.
			var out struct {
				Sessions []struct {
					ClaudeSession string `json:"claude_session"`
					Resume        string `json:"resume"`
				} `json:"sessions"`
			}
			if err := json.Unmarshal([]byte(got.stdout), &out); err != nil {
				t.Fatalf("stdout is not JSON: %v %q", err, got.stdout)
			}
			if len(out.Sessions) != 2 || out.Sessions[0].ClaudeSession != prSessNew || out.Sessions[0].Resume != "claude --resume "+prSessNew ||
				out.Sessions[1].ClaudeSession != prSessOld || out.Sessions[1].Resume != "claude --resume "+prSessOld {
				t.Fatalf("stdout %s", got.stdout)
			}
			// stderr: the same rows as lines, each followed by its resume line.
			want := prSessNew + "  host alpha  first_seen 2000-01-02T12:00:00Z  kind gh-pr-create\n  claude --resume " + prSessNew + "\n" +
				prSessOld + "  host (no window showed it)  first_seen 2000-01-01T12:00:00Z  kind gh-pr-create\n  claude --resume " + prSessOld + "\n"
			if !strings.Contains(got.stderr, want) {
				t.Fatalf("stderr %q; want it to contain %q", got.stderr, want)
			}
		})
	}
}

func TestPRSessionWithNoRowIsNotFoundAndPrintsNothingOnStdout(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/pr-sessions", http.StatusOK, `{"repo":"acme/widget","pr":12,"sessions":[]}`)
	got := h.runCLI("pr-session", "acme/widget#12")
	if got.code != exitNotFound || got.stdout != "" || !strings.Contains(got.stderr, "no session of yours is recorded as creating acme/widget#12") {
		t.Fatalf("exit %d stdout %q stderr %q; want %d, empty stdout, the not-recorded message", got.code, got.stdout, got.stderr, exitNotFound)
	}
}

// TestPRSessionExitCodesAreDistinct: no row (4) is told apart from every way the
// lookup can fail — a bad argument (2, nothing sent), a refused credential (3),
// a server error (1), an unreachable server (6), a server without the route (7).
func TestPRSessionExitCodesAreDistinct(t *testing.T) {
	t.Run("bad argument", func(t *testing.T) {
		h := newHarness(t)
		got := h.runCLI("pr-session", "acme/widget")
		if got.code != exitUsage || len(h.nonHealthRequests()) != 0 {
			t.Fatalf("exit %d, %d requests; want %d and none", got.code, len(h.nonHealthRequests()), exitUsage)
		}
	})
	for _, c := range []struct {
		name   string
		status int
		want   int
	}{
		{"refused credential", http.StatusUnauthorized, exitAuth},
		{"server error", http.StatusInternalServerError, exitServerError},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.json("GET /api/pr-sessions", c.status, `{"error":"x"}`)
			if got := h.runCLI("pr-session", "acme/widget#12"); got.code != c.want || got.stdout != "" {
				t.Fatalf("exit %d stdout %q; want %d", got.code, got.stdout, c.want)
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		h := newHarness(t)
		if got := h.runCLI("--api-url", "http://127.0.0.1:1", "pr-session", "acme/widget#12"); got.code != exitNetwork {
			t.Fatalf("exit %d; want %d", got.code, exitNetwork)
		}
	})
	t.Run("route absent", func(t *testing.T) {
		h := newHarness(t)
		if got := h.runCLI("pr-session", "acme/widget#12"); got.code != exitRouteAbsent {
			t.Fatalf("exit %d; want %d (stderr %q)", got.code, exitRouteAbsent, got.stderr)
		}
	})
}
