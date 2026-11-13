package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The standing PUBLIC read (an officer's `read_grant: "standing"`).
//
// A caller with PublicReadRepos may READ those repositories through the
// `github` connector with NO grant at all: no request is filed, its owner is
// not prompted, and nothing is approved or shared. It is the narrowest thing
// that can be standing, because it carries NOTHING of the owner's:
//
//   - 🔴 NO CREDENTIAL. A standing read is forwarded anonymously — no App
//     token, no stored secret — so GitHub itself serves only what it serves
//     to anyone. A private repository answers 404 to it whatever muster
//     decides; muster's own public check below is the second, stated gate.
//   - 🔴 READS ONLY. GET and HEAD on github.com/<owner>/<name>[.git]/… and
//     api.github.com/repos/<owner>/<name>/…, plus git's fetch POST
//     (<owner>/<name>[.git]/git-upload-pack). Anything else that targets one
//     of the repositories — a push (git-receive-pack, in a POST or as the
//     info/refs service), an API write — is REFUSED 403, never passed on to
//     the ordinary path.
//   - 🔴 THE DECLARED REPOSITORIES ONLY. The repository is decided from the
//     path by GitHubTargetRepo, the GitHub App's own rule; a request for any
//     other repository, or a path that names none, is not a standing read and
//     takes the ordinary path (its grant, as before).
//   - 🔴 PUBLIC ONLY, VERIFIED. Before the first standing read of a repository,
//     and again once publicTTL has passed, muster asks GitHub's API
//     anonymously (GET /repos/<owner>/<name>, redirects NOT followed) and
//     serves the read only when it answers 200 with that exact full_name,
//     private false and visibility "public". A 404 (private, or absent) or
//     anything else is a 403 refusal naming the repository; a failed check is
//     a 502 and is not cached. A repository made private stops being served
//     within publicTTL (and at once by GitHub, since no credential is sent).
//
// The caller side (who gets PublicReadRepos) is internal/api's
// Server.agentPublicReadRepos: an instance of a BUILTIN officer that declares
// read_grant "standing" — never an HQ officer, never a shipping one.

const (
	// GitHubGitHost and GitHubAPIHost are the hosts a standing public read is
	// served on unless the Proxy names others (Proxy.GitHubGitHost).
	GitHubGitHost = "github.com"
	GitHubAPIHost = "api.github.com"
	// githubConnectorName is the connector a standing read is served under
	// (githubapp.ConnectorName; this package does not import the App).
	githubConnectorName = "github"
	// publicTTL is how long a verified-public verdict is reused, and
	// privateTTL a not-public one.
	publicTTL  = 10 * time.Minute
	privateTTL = time.Minute
)

// GitHubTargetRepo is the repository a request for host/rest (rest escaped, no
// leading slash) targets on GitHub, gitHost and apiHost being the platform's
// git and API hosts. scoped is false for an API path that names no repository
// (/graphql, /rate_limit, /user …).
//
// The path is FULLY percent-decoded first and compared without case, as GitHub
// routes it:
//
//   - gitHost: every path is <owner>/<repo>[.git]/… (git's smart-HTTP
//     protocol, and the web URLs). A path with fewer segments is refused.
//   - apiHost: /repos/<owner>/<repo>/… (any case) names a repository;
//     /repositories/<id>/… is REFUSED (an id muster cannot match without
//     another API call); any other path is not repository-scoped.
//
// Refused outright, because the upstream may resolve them to another
// repository after this check: a `.` or `..` segment, a leading empty segment
// (`%2Frepos/…` decodes to one), a `%` left after decoding (double encoding), a
// backslash, and an escape that does not decode.
//
// 🔴 ONE RULE, ONE PLACE: the GitHub App (githubapp.App.targetRepo) and the
// standing public read both decide a request's repository here.
func GitHubTargetRepo(gitHost, apiHost, host, rest string) (repo string, scoped bool, err error) {
	p, err := url.PathUnescape(rest)
	if err != nil {
		return "", false, errors.New("the request path does not decode")
	}
	if strings.ContainsAny(p, "%\\") {
		return "", false, errors.New("the request path is encoded more than once or holds a backslash")
	}
	segs := strings.Split(p, "/")
	if segs[0] == "" {
		return "", false, errors.New("the request path begins with an empty segment")
	}
	for _, s := range segs {
		if s == "." || s == ".." {
			return "", false, errors.New("the request path has a '.' or '..' segment")
		}
	}
	switch host {
	case gitHost:
		if len(segs) < 2 || segs[1] == "" {
			return "", false, fmt.Errorf("%s/%s names no repository", host, p)
		}
		return segs[0] + "/" + strings.TrimSuffix(segs[1], ".git"), true, nil
	case apiHost:
		switch strings.ToLower(segs[0]) {
		case "repos":
			if len(segs) < 3 || segs[1] == "" || segs[2] == "" {
				return "", false, fmt.Errorf("%s/%s names no repository", host, p)
			}
			return segs[1] + "/" + segs[2], true, nil
		case "repositories":
			return "", false, fmt.Errorf("%s/%s names a repository by id; use /repos/<owner>/<name>", host, p)
		}
		return "", false, nil
	}
	return "", false, fmt.Errorf("host %s is not served by the GitHub App connector", host)
}

// publicReadTarget is the entry of caller.PublicReadRepos a request for
// connector/host/rest targets, ok false when it is not a standing read at all
// (another connector or host, a path that names no repository or does not
// parse, or a repository the caller does not declare).
func (p *Proxy) publicReadTarget(caller Caller, connector, host, rest string) (string, bool) {
	if connector != githubConnectorName || len(caller.PublicReadRepos) == 0 {
		return "", false
	}
	gitHost, apiHost := p.githubHosts()
	target, scoped, err := GitHubTargetRepo(gitHost, apiHost, strings.ToLower(host), rest)
	if err != nil || !scoped {
		return "", false
	}
	for _, r := range caller.PublicReadRepos {
		if r = strings.TrimSpace(r); r != "" && strings.EqualFold(r, target) {
			return strings.ToLower(r), true
		}
	}
	return "", false
}

// githubHosts are the git and API hosts a standing public read is served on.
func (p *Proxy) githubHosts() (git, api string) {
	git, api = strings.ToLower(p.GitHubGitHost), strings.ToLower(p.GitHubAPIHost)
	if git == "" {
		git = GitHubGitHost
	}
	if api == "" {
		api = GitHubAPIHost
	}
	return git, api
}

// publicReadAllowed reports whether a request is a READ a standing public read
// may serve: GET or HEAD (a git info/refs only for the git-upload-pack
// service), or git's fetch POST to <owner>/<name>[.git]/git-upload-pack on the
// git host. Everything else — a push, an API write — is not.
func publicReadAllowed(gitHost, method, host, rest, rawQuery string) bool {
	h := strings.ToLower(host)
	p, err := url.PathUnescape(rest)
	if err != nil {
		return false
	}
	segs := strings.Split(p, "/")
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return false
	}
	for _, s := range q["service"] {
		if s != "git-upload-pack" {
			return false
		}
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		for _, s := range segs {
			if strings.EqualFold(s, "git-receive-pack") {
				return false
			}
		}
		return true
	case http.MethodPost:
		return h == gitHost && len(segs) == 3 && segs[2] == "git-upload-pack"
	}
	return false
}

// publicVerdict is one cached answer of publicCheck.
type publicVerdict struct {
	public bool
	why    string
	at     time.Time
}

// publicCache holds publicCheck's verdicts, per lowercased repository.
type publicCache struct {
	mu   sync.Mutex
	seen map[string]publicVerdict
}

// errPublicCheck is a check that could not decide (network, rate limit, an
// answer it cannot read): a 502, never cached.
var errPublicCheck = errors.New("connectors: could not check with GitHub that the repository is public")

// publicCheck reports whether GitHub says repo is PUBLIC, asking its API
// anonymously, with redirects NOT followed (a renamed repository is not the
// one declared). why names the reason for a not-public verdict.
func (p *Proxy) publicCheck(ctx context.Context, repo string) (public bool, why string, err error) {
	key := strings.ToLower(repo)
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	p.public.mu.Lock()
	if v, ok := p.public.seen[key]; ok {
		ttl := privateTTL
		if v.public {
			ttl = publicTTL
		}
		if now.Sub(v.at) < ttl {
			p.public.mu.Unlock()
			return v.public, v.why, nil
		}
	}
	p.public.mu.Unlock()

	public, why, err = p.askGitHubPublic(ctx, repo)
	if err != nil {
		// A re-check GitHub could not answer (its anonymous API limit is 60 an
		// hour per address, shared by the deployment) keeps serving a verdict
		// that was PUBLIC: no credential is sent, so GitHub itself still
		// refuses a repository made private meanwhile. Anything else — no
		// verdict yet, or a not-public one — stays refused.
		p.public.mu.Lock()
		v, ok := p.public.seen[key]
		p.public.mu.Unlock()
		if ok && v.public {
			p.logf("connectors: standing read of %s: re-check failed, serving the last public verdict: %v", repo, err)
			return true, "", nil
		}
		return false, "", err
	}
	p.public.mu.Lock()
	if p.public.seen == nil {
		p.public.seen = map[string]publicVerdict{}
	}
	p.public.seen[key] = publicVerdict{public: public, why: why, at: now}
	p.public.mu.Unlock()
	return public, why, nil
}

// askGitHubPublic is publicCheck's one call to GitHub, through the proxy's own
// upstream transport (the same address policy as every proxied request).
func (p *Proxy) askGitHubPublic(ctx context.Context, repo string) (bool, string, error) {
	scheme := "https"
	if p.Policy.InsecureHTTP {
		scheme = "http"
	}
	owner, name, _ := strings.Cut(repo, "/")
	_, apiHost := p.githubHosts()
	u := &url.URL{Scheme: scheme, Host: apiHost, Path: "/repos/" + owner + "/" + name}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, "", fmt.Errorf("%w: %v", errPublicCheck, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "muster")
	client := &http.Client{Transport: p.upstreamTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("%w: %v", errPublicCheck, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return false, "GitHub does not show it to an anonymous reader (it is private, or does not exist)", nil
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return false, "GitHub redirects it elsewhere (it was renamed or moved); declare its current name", nil
	default:
		return false, "", fmt.Errorf("%w: GitHub answered %d", errPublicCheck, resp.StatusCode)
	}
	var body struct {
		FullName   string `json:"full_name"`
		Private    *bool  `json:"private"`
		Visibility string `json:"visibility"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return false, "", fmt.Errorf("%w: its answer is not JSON: %v", errPublicCheck, err)
	}
	switch {
	case !strings.EqualFold(body.FullName, repo):
		return false, fmt.Sprintf("GitHub names it %q, not %q", body.FullName, repo), nil
	case body.Private == nil || *body.Private:
		return false, "GitHub says it is private", nil
	case body.Visibility != "public":
		return false, fmt.Sprintf("GitHub says its visibility is %q, not public", body.Visibility), nil
	}
	return true, "", nil
}

// servePublicRead serves one standing public read of repo (publicReadTarget
// decided it), or refuses it. It never files, reads or touches a grant.
func (p *Proxy) servePublicRead(w http.ResponseWriter, r *http.Request, caller Caller, connector, host, rest, repo string,
	status *int, detail *string, refuse func(int, RefusalBody, string)) {
	gitHost, apiHost := p.githubHosts()
	if !publicReadAllowed(gitHost, r.Method, host, rest, r.URL.RawQuery) {
		refuse(http.StatusForbidden, RefusalBody{Error: "the standing read of " + repo + " serves reads only (GET, HEAD, " +
			"and git fetch); " + r.Method + " /" + rest + " was refused — muster never writes to a repository through it"},
			"standing read: write refused")
		return
	}
	public, why, err := p.publicCheck(r.Context(), repo)
	if err != nil {
		p.logf("connectors: standing read of %s for agent %q: %v", repo, caller.AgentName, err)
		refuse(http.StatusBadGateway, RefusalBody{Error: err.Error()}, "standing read: public check failed")
		return
	}
	if !public {
		refuse(http.StatusForbidden, RefusalBody{Error: "the standing read serves PUBLIC repositories only, and " + repo +
			" is not one: " + why + ". A private repository needs the per-instance github grant (declare it without " +
			"read_grant \"standing\")"}, "standing read: not public")
		return
	}
	c := Connector{Name: connector, Platform: "github-public-read", Hosts: []string{apiHost, gitHost},
		Scheme: SchemeBasic, CreatedAt: VirtualCreatedAt, UpdatedAt: VirtualCreatedAt}
	*detail = "standing public read of " + repo
	p.forward(w, r, caller, Access{}, c, "", true, nil, connector, host, rest, status, detail, refuse)
}
