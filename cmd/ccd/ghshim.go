package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/podagent"
)

// ---------------------------------------------------------------------------
// `gh`, READ-ONLY, OVER muster's CONNECTOR PROXY (docs/officers.md "Reading
// pull requests with gh").
//
// The real `gh` speaks HTTPS to a fixed host, so it cannot be pointed at the
// plain-HTTP in-cluster proxy, and the image does not carry it. This is the
// subset of it a script that READS a pull request needs, answered by GETs to
// $MUSTER_CONNECT_URL/connect/github/api.github.com/… with the pod's own
// muster token (X-Muster-Token) — so every read is decided by the proxy
// exactly as the agent's curl would be (its grant, its gate, its read-only
// or officer App token):
//
//	gh pr view <n|url> [--repo|-R owner/name] --json <fields>
//	gh pr diff <n|url> [--repo|-R owner/name] [--name-only] [--patch] [--color never]
//	gh api <path> [--paginate] [-X|--method GET] [-H|--header 'Accept: …']
//	gh --version | gh version | gh auth status
//
// 🔴 IT WRITES NOTHING. Every other command — `gh pr comment`, `merge`,
// `close`, `edit`, `ready`, `create`, `review`, `checkout`, `gh issue …`,
// `gh repo …` — and every `gh api` with a method other than GET, a body
// (`-f`/`-F`/`--field`/`--raw-field`/`--input`) or the GraphQL endpoint is
// refused before any request is sent (exit 1). An officer's writes go only
// through muster's gated tools and push gate.
//
// It is installed as /usr/local/bin/gh, a link to ccd (run dispatches on the
// program name), in images/claude-code-agent.
// ---------------------------------------------------------------------------

const ghShimVersion = "gh version 0.0.0-muster (read-only shim over muster's connector proxy)"

// ghShim is one invocation's environment: where the proxy is, the token, and
// how to read the checkout's origin (for a command naming no repository).
type ghShim struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	origin         func() (string, error)
	http           *http.Client
}

// runGH is `gh` (argv after the program name). Exit 0 on success, 1 on any
// error or refusal (gh's own code for a failed command).
func runGH(args []string, stdout, stderr io.Writer) int {
	g := &ghShim{stdout: stdout, stderr: stderr, getenv: os.Getenv, origin: gitOrigin, http: &http.Client{Timeout: 60 * time.Second}}
	return g.run(args)
}

func gitOrigin() (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	return strings.TrimSpace(string(out)), err
}

func (g *ghShim) fail(format string, args ...any) int {
	fmt.Fprintf(g.stderr, format+"\n", args...)
	return 1
}

func (g *ghShim) refuse(what string) int {
	// STUB (red-first commit): writes are not refused yet.
	if what != "" {
		return 0
	}
	return g.fail("gh (muster's read-only shim): %s writes or is not supported — refused, nothing was sent. "+
		"Reads only: gh pr view --json, gh pr diff, gh api GET. Writes go through muster's gated tools.", what)
}

func (g *ghShim) run(args []string) int {
	if len(args) == 0 {
		return g.fail("gh (muster's read-only shim): pr view | pr diff | api — reads only")
	}
	switch args[0] {
	case "--version", "version":
		fmt.Fprintln(g.stdout, ghShimVersion)
		return 0
	case "auth":
		if len(args) > 1 && args[1] == "status" {
			fmt.Fprintln(g.stdout, "github.com: reads go through muster's connector proxy with this pod's own token (no gh login)")
			return 0
		}
		return g.refuse("gh " + strings.Join(args, " "))
	case "pr":
		if len(args) < 2 {
			return g.refuse("gh pr")
		}
		switch args[1] {
		case "view":
			return g.prView(args[2:])
		case "diff":
			return g.prDiff(args[2:])
		}
		return g.refuse("gh pr " + args[1])
	case "api":
		return g.api(args[1:])
	}
	return g.refuse("gh " + args[0])
}

// flags parses args into positionals and --flag values. valued names the
// flags that take a value; bools those that do not. Anything else errs.
func parseFlags(args []string, valued, bools map[string]bool) (pos []string, vals map[string][]string, err error) {
	vals = map[string][]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		switch {
		case bools[name]:
			if hasVal {
				return nil, nil, fmt.Errorf("flag %s takes no value", name)
			}
			vals[name] = append(vals[name], "true")
		case valued[name]:
			if !hasVal {
				if i+1 >= len(args) {
					return nil, nil, fmt.Errorf("flag %s needs a value", name)
				}
				i++
				val = args[i]
			}
			vals[name] = append(vals[name], val)
		default:
			return nil, nil, fmt.Errorf("unknown flag %s", name)
		}
	}
	return pos, vals, nil
}

func last(vals map[string][]string, names ...string) string {
	v := ""
	for _, n := range names {
		if l := vals[n]; len(l) > 0 {
			v = l[len(l)-1]
		}
	}
	return v
}

var (
	ghRepoRE   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)
	ghPRURLRE  = regexp.MustCompile(`^https://github\.com/([^/]+/[^/]+)/pull/([1-9][0-9]{0,9})(?:[/?#].*)?$`)
	ghOriginRE = regexp.MustCompile(`github\.com[:/]([^/]+/[^/]+?)(?:\.git)?/?$`)
)

// repoAndPR resolves a pull request argument (a number, or its URL) and the
// repository (--repo/-R, else the URL's, else GH_REPO, else the checkout's
// origin).
func (g *ghShim) repoAndPR(pos []string, vals map[string][]string) (string, int, error) {
	if len(pos) != 1 {
		return "", 0, errors.New("name exactly one pull request (its number or URL); the shim does not infer one from the branch")
	}
	repo := last(vals, "--repo", "-R")
	arg := pos[0]
	if m := ghPRURLRE.FindStringSubmatch(arg); m != nil {
		if repo == "" {
			repo = m[1]
		}
		arg = m[2]
	}
	n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
	if err != nil || n <= 0 {
		return "", 0, fmt.Errorf("%q is not a pull request number or URL", pos[0])
	}
	if repo == "" {
		repo, err = g.defaultRepo()
		if err != nil {
			return "", 0, err
		}
	}
	if !ghRepoRE.MatchString(repo) {
		return "", 0, fmt.Errorf("%q is not owner/name", repo)
	}
	return repo, n, nil
}

func (g *ghShim) defaultRepo() (string, error) {
	if r := strings.TrimSpace(g.getenv("GH_REPO")); r != "" {
		return r, nil
	}
	if g.origin != nil {
		if u, err := g.origin(); err == nil {
			if m := ghOriginRE.FindStringSubmatch(u); m != nil {
				return m[1], nil
			}
		}
	}
	return "", errors.New("no repository: pass --repo owner/name (or set GH_REPO)")
}

// base is the proxy's prefix for api.github.com.
func (g *ghShim) base() (string, string, error) {
	u := strings.TrimRight(strings.TrimSpace(g.getenv(podagent.EnvConnectURL)), "/")
	tok := strings.TrimSpace(g.getenv(podagent.EnvAgentToken))
	if u == "" || tok == "" {
		return "", "", fmt.Errorf("%s and %s must be set (this pod reaches GitHub only through muster's proxy)", podagent.EnvConnectURL, podagent.EnvAgentToken)
	}
	return u + "/connect/" + podagent.GithubConnector + "/api.github.com", tok, nil
}

// ghHTTPError is a non-2xx answer, printed the way gh prints one.
type ghHTTPError struct {
	status int
	msg    string
	path   string
}

func (e *ghHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s (https://api.github.com/%s)", e.status, e.msg, e.path)
}

// get is ONE GET of path (relative to api.github.com, may carry a query)
// through the proxy. It returns the body and the next page's path (from the
// Link header), "" when none.
func (g *ghShim) get(path, accept string) ([]byte, string, error) {
	base, tok, err := g.base()
	if err != nil {
		return nil, "", err
	}
	path = strings.TrimPrefix(path, "/")
	req, err := http.NewRequest(http.MethodGet, base+"/"+path, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("X-Muster-Token", tok)
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("could not reach muster's proxy: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode/100 != 2 {
		var m struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		_ = json.Unmarshal(raw, &m)
		msg := m.Message
		if msg == "" {
			msg = m.Error
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return nil, "", &ghHTTPError{status: resp.StatusCode, msg: msg, path: path}
	}
	return raw, nextPage(resp.Header.Get("Link")), nil
}

var linkNextRE = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// nextPage is the path (after api.github.com/) of a Link header's rel="next".
func nextPage(link string) string {
	m := linkNextRE.FindStringSubmatch(link)
	if m == nil {
		return ""
	}
	u, err := url.Parse(m[1])
	if err != nil {
		return ""
	}
	p := strings.TrimPrefix(u.EscapedPath(), "/")
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

// maxShimPages bounds every paginated read.
const maxShimPages = 50

// getAll reads every page of a JSON-array path and merges the arrays.
func (g *ghShim) getAll(path string) ([]json.RawMessage, error) {
	var out []json.RawMessage
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	if !strings.Contains(path, "per_page=") {
		path += sep + "per_page=100"
	}
	for page := 0; path != ""; page++ {
		if page >= maxShimPages {
			return nil, fmt.Errorf("more than %d pages at %s", maxShimPages, path)
		}
		raw, next, err := g.get(path, "")
		if err != nil {
			return nil, err
		}
		var batch []json.RawMessage
		if err := json.Unmarshal(raw, &batch); err != nil {
			return nil, fmt.Errorf("GitHub's answer at %s is not a JSON array: %v", path, err)
		}
		out = append(out, batch...)
		path = next
	}
	return out, nil
}

func (g *ghShim) writeJSON(v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return g.fail("gh: %v", err)
	}
	fmt.Fprintln(g.stdout, string(b))
	return 0
}

// --- gh pr view --json ----------------------------------------------------

// restPRView is GET pulls/{n}, the part pr view reads.
type restPRView struct {
	Number    int        `json:"number"`
	NodeID    string     `json:"node_id"`
	HTMLURL   string     `json:"html_url"`
	Title     string     `json:"title"`
	Body      *string    `json:"body"`
	State     string     `json:"state"`
	Draft     bool       `json:"draft"`
	Merged    bool       `json:"merged"`
	MergedAt  *time.Time `json:"merged_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Mergeable *bool      `json:"mergeable"`
	Additions int        `json:"additions"`
	Deletions int        `json:"deletions"`
	Changed   int        `json:"changed_files"`
	User      restUser   `json:"user"`
	Labels    []struct {
		NodeID      string `json:"node_id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Color       string `json:"color"`
	} `json:"labels"`
	Head restRef `json:"head"`
	Base restRef `json:"base"`
}

type restUser struct {
	Login  string `json:"login"`
	NodeID string `json:"node_id"`
	Type   string `json:"type"`
}

type restRef struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		NodeID   string   `json:"node_id"`
		Name     string   `json:"name"`
		FullName string   `json:"full_name"`
		Owner    restUser `json:"owner"`
	} `json:"repo"`
}

func ghAuthor(u restUser) map[string]any {
	return map[string]any{"id": u.NodeID, "login": u.Login, "is_bot": u.Type == "Bot"}
}

// prFields are the `--json` fields the shim answers, each from the pull
// request and, for the three list fields, its own paginated read.
var prFields = map[string]func(g *ghShim, repo string, pr restPRView) (any, error){
	"number":       func(_ *ghShim, _ string, p restPRView) (any, error) { return p.Number, nil },
	"id":           func(_ *ghShim, _ string, p restPRView) (any, error) { return p.NodeID, nil },
	"url":          func(_ *ghShim, _ string, p restPRView) (any, error) { return p.HTMLURL, nil },
	"title":        func(_ *ghShim, _ string, p restPRView) (any, error) { return p.Title, nil },
	"body":         func(_ *ghShim, _ string, p restPRView) (any, error) { return derefString(p.Body), nil },
	"isDraft":      func(_ *ghShim, _ string, p restPRView) (any, error) { return p.Draft, nil },
	"author":       func(_ *ghShim, _ string, p restPRView) (any, error) { return ghAuthor(p.User), nil },
	"baseRefName":  func(_ *ghShim, _ string, p restPRView) (any, error) { return p.Base.Ref, nil },
	"baseRefOid":   func(_ *ghShim, _ string, p restPRView) (any, error) { return p.Base.SHA, nil },
	"headRefName":  func(_ *ghShim, _ string, p restPRView) (any, error) { return p.Head.Ref, nil },
	"headRefOid":   func(_ *ghShim, _ string, p restPRView) (any, error) { return p.Head.SHA, nil },
	"additions":    func(_ *ghShim, _ string, p restPRView) (any, error) { return p.Additions, nil },
	"deletions":    func(_ *ghShim, _ string, p restPRView) (any, error) { return p.Deletions, nil },
	"changedFiles": func(_ *ghShim, _ string, p restPRView) (any, error) { return p.Changed, nil },
	"createdAt":    func(_ *ghShim, _ string, p restPRView) (any, error) { return p.CreatedAt, nil },
	"updatedAt":    func(_ *ghShim, _ string, p restPRView) (any, error) { return p.UpdatedAt, nil },
	"mergedAt":     func(_ *ghShim, _ string, p restPRView) (any, error) { return p.MergedAt, nil },
	"closedAt":     func(_ *ghShim, _ string, p restPRView) (any, error) { return p.ClosedAt, nil },
	"state": func(_ *ghShim, _ string, p restPRView) (any, error) {
		switch {
		case p.Merged || p.MergedAt != nil:
			return "MERGED", nil
		case p.State == "closed":
			return "CLOSED", nil
		}
		return "OPEN", nil
	},
	"mergeable": func(_ *ghShim, _ string, p restPRView) (any, error) {
		switch {
		case p.Mergeable == nil:
			return "UNKNOWN", nil
		case *p.Mergeable:
			return "MERGEABLE", nil
		}
		return "CONFLICTING", nil
	},
	"isCrossRepository": func(_ *ghShim, _ string, p restPRView) (any, error) {
		return p.Head.Repo == nil || p.Base.Repo == nil || !strings.EqualFold(p.Head.Repo.FullName, p.Base.Repo.FullName), nil
	},
	"headRepository": func(_ *ghShim, _ string, p restPRView) (any, error) {
		if p.Head.Repo == nil {
			return nil, nil
		}
		return map[string]any{"id": p.Head.Repo.NodeID, "name": p.Head.Repo.Name, "nameWithOwner": p.Head.Repo.FullName}, nil
	},
	"headRepositoryOwner": func(_ *ghShim, _ string, p restPRView) (any, error) {
		if p.Head.Repo == nil {
			return nil, nil
		}
		return map[string]any{"id": p.Head.Repo.Owner.NodeID, "login": p.Head.Repo.Owner.Login}, nil
	},
	"labels": func(_ *ghShim, _ string, p restPRView) (any, error) {
		out := []map[string]any{}
		for _, l := range p.Labels {
			out = append(out, map[string]any{"id": l.NodeID, "name": l.Name, "description": l.Description, "color": l.Color})
		}
		return out, nil
	},
	"comments": func(g *ghShim, repo string, p restPRView) (any, error) {
		raw, err := g.getAll(fmt.Sprintf("repos/%s/issues/%d/comments", repo, p.Number))
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, r := range raw {
			var c struct {
				NodeID      string    `json:"node_id"`
				Body        string    `json:"body"`
				HTMLURL     string    `json:"html_url"`
				CreatedAt   time.Time `json:"created_at"`
				Association string    `json:"author_association"`
				User        restUser  `json:"user"`
			}
			if err := json.Unmarshal(r, &c); err != nil {
				return nil, err
			}
			// 🔴 viewerDidAuthor IS ALWAYS false: the shim has no viewer — the
			// read is the proxy's, as this pod — so it never claims a comment
			// was "yours".
			out = append(out, map[string]any{"id": c.NodeID, "author": map[string]any{"login": c.User.Login}, "authorAssociation": c.Association,
				"body": c.Body, "createdAt": c.CreatedAt, "url": c.HTMLURL, "viewerDidAuthor": false, "includesCreatedEdit": false,
				"isMinimized": false, "minimizedReason": "", "reactionGroups": []any{}})
		}
		return out, nil
	},
	"commits": func(g *ghShim, repo string, p restPRView) (any, error) {
		raw, err := g.getAll(fmt.Sprintf("repos/%s/pulls/%d/commits", repo, p.Number))
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, r := range raw {
			var c struct {
				SHA    string    `json:"sha"`
				Author *restUser `json:"author"`
				Commit struct {
					Message string `json:"message"`
					Author  struct {
						Name  string    `json:"name"`
						Email string    `json:"email"`
						Date  time.Time `json:"date"`
					} `json:"author"`
					Committer struct {
						Date time.Time `json:"date"`
					} `json:"committer"`
				} `json:"commit"`
			}
			if err := json.Unmarshal(r, &c); err != nil {
				return nil, err
			}
			head, body, _ := strings.Cut(c.Commit.Message, "\n")
			author := map[string]any{"name": c.Commit.Author.Name, "email": c.Commit.Author.Email, "id": "", "login": ""}
			if c.Author != nil {
				author["id"], author["login"] = c.Author.NodeID, c.Author.Login
			}
			out = append(out, map[string]any{"oid": c.SHA, "messageHeadline": head, "messageBody": strings.TrimLeft(body, "\n"),
				"authoredDate": c.Commit.Author.Date, "committedDate": c.Commit.Committer.Date, "authors": []any{author}})
		}
		return out, nil
	},
	"files": func(g *ghShim, repo string, p restPRView) (any, error) {
		raw, err := g.getAll(fmt.Sprintf("repos/%s/pulls/%d/files", repo, p.Number))
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, r := range raw {
			var f struct {
				Filename  string `json:"filename"`
				Additions int    `json:"additions"`
				Deletions int    `json:"deletions"`
			}
			if err := json.Unmarshal(r, &f); err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"path": f.Filename, "additions": f.Additions, "deletions": f.Deletions})
		}
		return out, nil
	},
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func prFieldNames() []string {
	out := make([]string, 0, len(prFields))
	for k := range prFields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (g *ghShim) prView(args []string) int {
	pos, vals, err := parseFlags(args, map[string]bool{"--repo": true, "-R": true, "--json": true}, nil)
	if err != nil {
		return g.fail("gh pr view: %v (the shim takes --repo and --json only; --jq, --template and --web are not supported)", err)
	}
	repo, n, err := g.repoAndPR(pos, vals)
	if err != nil {
		return g.fail("gh pr view: %v", err)
	}
	fields := last(vals, "--json")
	if fields == "" {
		return g.fail("gh pr view: the shim prints JSON only: pass --json <fields> (one of: %s)", strings.Join(prFieldNames(), ", "))
	}
	var want []string
	for _, f := range strings.Split(fields, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if _, ok := prFields[f]; !ok {
			return g.fail("Unknown JSON field: %q\nAvailable fields:\n  %s", f, strings.Join(prFieldNames(), "\n  "))
		}
		want = append(want, f)
	}
	raw, _, err := g.get(fmt.Sprintf("repos/%s/pulls/%d", repo, n), "")
	if err != nil {
		return g.fail("gh pr view: %v", err)
	}
	var pr restPRView
	if err := json.Unmarshal(raw, &pr); err != nil {
		return g.fail("gh pr view: GitHub's answer is not a pull request: %v", err)
	}
	out := map[string]any{}
	for _, f := range want {
		v, err := prFields[f](g, repo, pr)
		if err != nil {
			return g.fail("gh pr view: reading %s: %v", f, err)
		}
		out[f] = v
	}
	return g.writeJSON(out)
}

// --- gh pr diff -----------------------------------------------------------

func (g *ghShim) prDiff(args []string) int {
	pos, vals, err := parseFlags(args, map[string]bool{"--repo": true, "-R": true, "--color": true},
		map[string]bool{"--name-only": true, "--patch": true})
	if err != nil {
		return g.fail("gh pr diff: %v", err)
	}
	if c := last(vals, "--color"); c != "" && c != "never" && c != "auto" {
		return g.fail("gh pr diff: the shim prints no color (--color %s)", c)
	}
	repo, n, err := g.repoAndPR(pos, vals)
	if err != nil {
		return g.fail("gh pr diff: %v", err)
	}
	if last(vals, "--name-only") != "" {
		raw, err := g.getAll(fmt.Sprintf("repos/%s/pulls/%d/files", repo, n))
		if err != nil {
			return g.fail("gh pr diff: %v", err)
		}
		for _, r := range raw {
			var f struct {
				Filename string `json:"filename"`
			}
			if json.Unmarshal(r, &f) == nil {
				fmt.Fprintln(g.stdout, f.Filename)
			}
		}
		return 0
	}
	accept := "application/vnd.github.diff"
	if last(vals, "--patch") != "" {
		accept = "application/vnd.github.patch"
	}
	raw, _, err := g.get(fmt.Sprintf("repos/%s/pulls/%d", repo, n), accept)
	if err != nil {
		return g.fail("gh pr diff: %v", err)
	}
	_, _ = g.stdout.Write(raw)
	return 0
}

// --- gh api ---------------------------------------------------------------

// apiWriteFlags are gh api's body flags: each makes gh send a POST.
var apiWriteFlags = map[string]bool{"-f": true, "-F": true, "--field": true, "--raw-field": true, "--input": true}

func (g *ghShim) api(args []string) int {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		if false && apiWriteFlags[name] {
			return g.refuse("gh api " + name + " (a request body)")
		}
	}
	pos, vals, err := parseFlags(args, map[string]bool{"-X": true, "--method": true, "-H": true, "--header": true, "--hostname": true},
		map[string]bool{"--paginate": true})
	if err != nil {
		return g.fail("gh api: %v (the shim takes --paginate, -X GET and -H only)", err)
	}
	if m := last(vals, "-X", "--method"); false && m != "" && !strings.EqualFold(m, http.MethodGet) {
		return g.refuse("gh api -X " + m)
	}
	if h := last(vals, "--hostname"); h != "" && h != "github.com" {
		return g.fail("gh api: only github.com is reachable (--hostname %s)", h)
	}
	if len(pos) != 1 {
		return g.fail("gh api: name exactly one path")
	}
	path := strings.TrimPrefix(pos[0], "/")
	if p, _, _ := strings.Cut(path, "?"); p == "graphql" || strings.HasPrefix(p, "graphql/") {
		return g.refuse("gh api graphql")
	}
	if strings.Contains(path, "{owner}") || strings.Contains(path, "{repo}") {
		repo, err := g.defaultRepo()
		if err != nil {
			return g.fail("gh api: %s names {owner}/{repo}: %v", pos[0], err)
		}
		o, r, _ := strings.Cut(repo, "/")
		path = strings.ReplaceAll(strings.ReplaceAll(path, "{owner}", o), "{repo}", r)
	}
	accept := ""
	for _, h := range append(vals["-H"], vals["--header"]...) {
		k, v, ok := strings.Cut(h, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "Accept") {
			return g.fail("gh api: the shim sends only an Accept header (-H %q)", h)
		}
		accept = strings.TrimSpace(v)
	}
	if last(vals, "--paginate") != "" {
		all, err := g.paginate(path, accept)
		if err != nil {
			return g.fail("gh: %v", err)
		}
		_, _ = g.stdout.Write(all)
		if len(all) > 0 && all[len(all)-1] != '\n' {
			fmt.Fprintln(g.stdout)
		}
		return 0
	}
	raw, _, err := g.get(path, accept)
	if err != nil {
		return g.fail("gh: %v", err)
	}
	_, _ = g.stdout.Write(raw)
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		fmt.Fprintln(g.stdout)
	}
	return 0
}

// paginate is gh api --paginate: every page, JSON arrays merged into one
// (as gh does); a page that is not an array is printed as it came.
func (g *ghShim) paginate(path, accept string) ([]byte, error) {
	var merged []json.RawMessage
	var other bytes.Buffer
	arrays := true
	for page := 0; path != ""; page++ {
		if page >= maxShimPages {
			return nil, fmt.Errorf("more than %d pages at %s", maxShimPages, path)
		}
		raw, next, err := g.get(path, accept)
		if err != nil {
			return nil, err
		}
		var batch []json.RawMessage
		switch {
		case arrays && json.Unmarshal(raw, &batch) == nil:
			merged = append(merged, batch...)
		case arrays:
			// Not an array after all: what was merged so far is printed as
			// it came, then this page and every later one.
			arrays = false
			if len(merged) > 0 {
				b, _ := json.Marshal(merged)
				other.Write(b)
			}
			other.Write(raw)
		default:
			other.Write(raw)
		}
		path = next
	}
	if !arrays {
		return other.Bytes(), nil
	}
	if merged == nil {
		merged = []json.RawMessage{}
	}
	return json.Marshal(merged)
}
