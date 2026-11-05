package officership

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// PushGate is a shipping officer's connectors.Gate: the write filter every one
// of its instance's GitHub requests passes through in muster's connector
// proxy, and the source of the (write-capable) credential an admitted request
// carries.
//
// 🔴 IT IS AN ALLOWLIST. A read (GET, HEAD) is forwarded. A write is forwarded
// only when it is one of these, in the officer's own repository:
//
//   - git push (POST …/git-receive-pack) whose EVERY ref update targets
//     refs/heads/<prefix>… — so never the base branch, never a tag;
//   - git fetch (POST …/git-upload-pack), which writes nothing;
//   - POST /repos/{repo}/pulls with head <prefix>… and base = the base branch;
//   - PATCH /repos/{repo}/pulls/{n} of the officer's OWN pull request (its
//     App's bot authored it, its head is <prefix>…), title/body/state only —
//     never `draft`, which REST PATCH does not take: draft -> ready is the
//     officer_mark_ready tool (GraphQL, after the same own-PR gates);
//   - POST /repos/{repo}/issues/{n}/comments on its OWN pull request.
//
// Every other method, path and host shape is refused — the merge endpoint, the
// git/refs and git/tags endpoints, releases, the contents API (which commits to
// any branch), the merges API, GraphQL (which can do all of those), and every
// DELETE. Merges and tags are made by officer_merge_pr and officer_tag_release
// in muster-server, after their gates.
type PushGate struct {
	// Repo is the officer's repository (owner/name): the one a request naming
	// no repository is served for.
	Repo string
	// Ship is the officer's policy (Base, BranchPrefix; Repos for a draft
	// officer shipping to several repositories, each with its own base and
	// prefix: officers.Ship.RepoList).
	Ship *officers.Ship
	// OfficerID is the officer's id: a draft officer's branch shape, body
	// contract and notices name it.
	OfficerID string
	// GitHost and APIHost are the hosts served (github.com, api.github.com).
	GitHost, APIHost string
	// Cred mints the credential for an admitted request to a SHIP repository
	// (the officer App's connectors.Virtual Credential: its token for
	// caller.Repo, anonymous elsewhere).
	Cred func(ctx context.Context, caller connectors.Caller, host, rest string) (string, bool, error)
	// ReadCred, when set, mints the READ-ONLY credential for a request to one
	// of the caller's read repositories (caller.ReadRepos): the deployment
	// App's, never the officer's. nil: such a request goes anonymously. Admit
	// forwards no write to a repository outside the ship repositories, so
	// ReadCred only ever serves reads.
	ReadCred func(ctx context.Context, caller connectors.Caller, host, rest string) (string, bool, error)
	// OnRejected drops a cached credential after a 401.
	OnRejected func(caller connectors.Caller)
	// GH reads a pull request (PATCH's own-PR check) and the App's slug, for
	// Repo. GHFor, when set, is the GitHub of each ship repository, keyed by
	// its lower-cased owner/name (a draft officer with several); Repo's entry,
	// when missing, is GH.
	GH    GitHub
	GHFor map[string]GitHub
	// Announce posts a draft officer's content refusal (a forbidden path, a
	// removed manifest kind, a secret shape) to HQ. A failure is logged by
	// Logf; the refusal stands either way.
	Announce func(ctx context.Context, n officers.Notice) error
	Logf     func(format string, args ...any)
	// Now is the clock the daily limit reads (time.Now when nil).
	Now func() time.Time
}

// ghFor is the GitHub of ship repository repo.
func (g *PushGate) ghFor(repo string) GitHub {
	if gh, ok := g.GHFor[strings.ToLower(repo)]; ok {
		return gh
	}
	if strings.EqualFold(repo, g.Repo) {
		return g.GH
	}
	return nil
}

// shipRepo is the ship repository owner/name names (".git" stripped), ok
// false when the officer does not ship to it.
func (g *PushGate) shipRepo(owner, name string) (officers.ShipRepo, bool) {
	return g.Ship.RepoFor(g.Repo, owner+"/"+strings.TrimSuffix(name, ".git"))
}

var _ connectors.Gate = (*PushGate)(nil)

// Connector implements connectors.Gate.
func (g *PushGate) Connector() string { return officers.GitHubConnector }

// Describe implements connectors.Gate: the GitHub preset's shape.
func (g *PushGate) Describe() connectors.Connector {
	return connectors.Connector{Name: officers.GitHubConnector, Platform: "github-app", Hosts: []string{g.APIHost, g.GitHost},
		Scheme: connectors.SchemeBasic, Username: "x-access-token", CreatedAt: connectors.VirtualCreatedAt, UpdatedAt: connectors.VirtualCreatedAt}
}

// Credential implements connectors.Gate.
//
// 🔴 THE OFFICER APP'S TOKEN IS MINTED FOR A SHIP REPOSITORY ONLY. A request
// that names a ship repository is served for that one (caller.Repo); a request
// naming none for g.Repo; a request naming one of the caller's read
// repositories by ReadCred (the deployment App, read-only); any other
// repository anonymously. The officer App never sees caller.ReadRepos.
func (g *PushGate) Credential(ctx context.Context, caller connectors.Caller, host, rest string) (string, bool, error) {
	target, named := g.targetRepo(host, rest)
	readRepos := caller.ReadRepos
	caller.Repo, caller.ReadOnly, caller.ReadRepos = g.Repo, false, nil
	if named {
		if r, ok := g.Ship.RepoFor(g.Repo, target); ok {
			caller.Repo = r.Repo
			return g.Cred(ctx, caller, host, rest)
		}
		for _, rr := range readRepos {
			if strings.EqualFold(rr, target) {
				if g.ReadCred == nil {
					return "", true, nil
				}
				caller.Repo, caller.ReadOnly = rr, true
				return g.ReadCred(ctx, caller, host, rest)
			}
		}
		return "", true, nil
	}
	return g.Cred(ctx, caller, host, rest)
}

// targetRepo is the owner/name a request names (git host: the first two
// segments; API host: /repos/{owner}/{name}/…), named false when it names
// none or does not decode (Admit has refused a write of that shape already).
func (g *PushGate) targetRepo(host, rest string) (string, bool) {
	segs, err := cleanSegments(rest)
	if err != nil {
		return "", false
	}
	switch {
	case host == g.GitHost && len(segs) >= 2:
		return segs[0] + "/" + strings.TrimSuffix(segs[1], ".git"), true
	case host == g.APIHost && len(segs) >= 3 && strings.EqualFold(segs[0], "repos"):
		return segs[1] + "/" + segs[2], true
	}
	return "", false
}

// Rejected implements connectors.Gate.
func (g *PushGate) Rejected(caller connectors.Caller) {
	if g.OnRejected != nil {
		g.OnRejected(caller)
	}
}

// conflict is a refusal answered 409 (connectors.ErrGateConflict): the
// write's content conflicts with the policy.
func conflict(format string, args ...any) error {
	return fmt.Errorf("%w: %s", connectors.ErrGateConflict, fmt.Sprintf(format, args...))
}

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s (merging and tagging go through the %s and %s tools; nothing else merges or tags)",
		connectors.ErrGateRefused, fmt.Sprintf(format, args...), officers.ToolMergePR, officers.ToolTagRelease)
}

// methodOverrideHeaders are refused on every request: an upstream that
// honoured one would turn an admitted read into a write.
var methodOverrideHeaders = []string{"X-HTTP-Method-Override", "X-HTTP-Method", "X-Method-Override"}

// maxPreamble bounds the receive-pack command section read before forwarding.
const maxPreamble = 1 << 20

// maxJSONBody bounds an admitted REST write's body.
const maxJSONBody = 1 << 20

// Admit implements connectors.Gate.
func (g *PushGate) Admit(r *http.Request, host, rest string) (*http.Request, error) {
	for _, h := range methodOverrideHeaders {
		if r.Header.Get(h) != "" {
			return nil, refused("the %s header is not forwarded", h)
		}
	}
	segs, err := cleanSegments(rest)
	if err != nil {
		return nil, refused("%v", err)
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
	if read {
		return r, nil
	}
	if r.URL.RawQuery != "" {
		return nil, refused("a write with a query string")
	}
	switch host {
	case g.GitHost:
		return g.admitGit(r, segs)
	case g.APIHost:
		return g.admitAPI(r, segs)
	}
	return nil, refused("host %s", host)
}

// cleanSegments decodes rest once and refuses every shape the upstream could
// resolve differently from this check: a second encoding, a backslash, an
// empty, `.` or `..` segment.
func cleanSegments(rest string) ([]string, error) {
	p, err := url.PathUnescape(rest)
	if err != nil {
		return nil, errors.New("the request path does not decode")
	}
	if strings.ContainsAny(p, "%\\") {
		return nil, errors.New("the request path is encoded more than once or holds a backslash")
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return nil, errors.New("the request path has an empty, '.' or '..' segment")
		}
	}
	return segs, nil
}

// admitGit: github.com/<owner>/<repo>[.git]/git-upload-pack (fetch) or
// /git-receive-pack (push, every ref under the prefix).
func (g *PushGate) admitGit(r *http.Request, segs []string) (*http.Request, error) {
	if r.Method != http.MethodPost || len(segs) != 3 {
		return nil, refused("%s on github.com/%s", r.Method, strings.Join(segs, "/"))
	}
	switch segs[2] {
	case "git-upload-pack":
		return r, nil
	case "git-receive-pack":
	default:
		return nil, refused("POST github.com/%s", strings.Join(segs, "/"))
	}
	sr, ok := g.shipRepo(segs[0], segs[1])
	if !ok {
		return nil, refused("a push to %s/%s, which is not one of this officer's repositories (%s)", segs[0], segs[1], g.repoNames())
	}
	return g.admitReceivePack(r, sr)
}

// admitReceivePack reads the pkt-line command section of a push (it ends at
// the first flush-pkt), refuses it unless every command updates a ref under
// refs/heads/<prefix>, and forwards the request with the bytes it read put
// back in front of the rest of the body.
//
// A gzip-encoded body is decoded and forwarded decoded (git accepts either);
// any other encoding is refused, since the commands could not be read.
func (g *PushGate) admitReceivePack(r *http.Request, sr officers.ShipRepo) (*http.Request, error) {
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	out := r.Clone(r.Context())
	var src io.Reader = body
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return nil, refused("a gzip push body that does not decode")
		}
		src = zr
		out.Header.Del("Content-Encoding")
		out.Header.Del("Content-Length")
		out.ContentLength = -1
	default:
		return nil, refused("a push body with Content-Encoding %q", enc)
	}
	br := bufio.NewReaderSize(src, 64<<10)
	var pre bytes.Buffer
	cmds, err := readPushCommands(br, &pre)
	if err != nil {
		return nil, refused("%v", err)
	}
	for _, c := range cmds {
		if err := g.allowedRef(sr, c.ref); err != nil {
			return nil, err
		}
		// 🔴 A DRAFT OFFICER DELETES NO BRANCH: an abandoned one is listed by
		// the daily orphan report, and a human removes it.
		if g.Ship.IsDraft() && strings.Trim(c.newOID, "0") == "" {
			return nil, refused("deleting %s: a draft officer never deletes a branch (the daily orphan report lists abandoned ones for a human)", c.ref)
		}
	}
	out.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(pre.Bytes()), br), body}
	return out, nil
}

// allowedRef is THE ref rule: refs/heads/<prefix><valid suffix> of ship
// repository sr — and, for a draft officer, exactly
// <prefix><officer slug>/<item>-<slug> (officers.BranchItem).
func (g *PushGate) allowedRef(sr officers.ShipRepo, ref string) error {
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok {
		return refused("a push to %s: only branches under refs/heads/%s may be pushed (no tags, no other refs)", ref, sr.BranchPrefix)
	}
	suffix, ok := strings.CutPrefix(branch, sr.BranchPrefix)
	if !ok || !officers.ValidRefSuffix(suffix) || !officers.ValidRefSuffix(branch) {
		return refused("a push to %s: only branches under refs/heads/%s may be pushed (never %s)", ref, sr.BranchPrefix, sr.Base)
	}
	if g.Ship.IsDraft() {
		if _, ok := officers.BranchItem(sr.BranchPrefix, g.OfficerID, branch); !ok {
			return refused("a push to %s: a draft officer's branches are %s%s/<item>-<slug> (the HQ item number, then a slug)", ref,
				sr.BranchPrefix, officers.Slug(g.OfficerID))
		}
	}
	return nil
}

// repoNames lists the ship repositories for a refusal.
func (g *PushGate) repoNames() string {
	var out []string
	for _, r := range g.Ship.RepoList(g.Repo) {
		out = append(out, r.Repo)
	}
	return strings.Join(out, ", ")
}

// pushCommand is one ref update of a push.
type pushCommand struct{ oldOID, newOID, ref string }

// readPushCommands reads pkt-lines from br into pre up to and including the
// first flush-pkt, and returns the ref of every update command. Shallow lines
// are skipped; a push certificate (whose commands sit inside the certificate)
// is refused. Zero commands is allowed: git's probe request is a bare
// flush-pkt and updates nothing.
func readPushCommands(br *bufio.Reader, pre *bytes.Buffer) ([]pushCommand, error) {
	var refs []pushCommand
	for first := true; ; first = false {
		var lenHex [4]byte
		if _, err := io.ReadFull(br, lenHex[:]); err != nil {
			return nil, errors.New("a push whose command section ends early")
		}
		pre.Write(lenHex[:])
		n, err := strconv.ParseUint(string(lenHex[:]), 16, 16)
		if err != nil {
			return nil, errors.New("a push with a malformed pkt-line length")
		}
		if n == 0 {
			return refs, nil
		}
		if n < 4 {
			return nil, errors.New("a push with a delimiter or malformed pkt-line in its command section")
		}
		if pre.Len()+int(n) > maxPreamble {
			return nil, errors.New("a push command section over 1 MiB")
		}
		line := make([]byte, n-4)
		if _, err := io.ReadFull(br, line); err != nil {
			return nil, errors.New("a push whose command section ends early")
		}
		pre.Write(line)
		s := strings.TrimSuffix(string(line), "\n")
		if first && strings.HasPrefix(s, "push-cert") {
			return nil, errors.New("a signed push (push certificate) is not forwarded")
		}
		if strings.HasPrefix(s, "shallow ") {
			continue
		}
		if i := strings.IndexByte(s, 0); i >= 0 {
			s = s[:i] // capabilities follow the first command
		}
		f := strings.Split(s, " ")
		if len(f) != 3 || !isOID(f[0]) || !isOID(f[1]) || f[2] == "" {
			return nil, fmt.Errorf("a push command that is not `<old> <new> <ref>`")
		}
		refs = append(refs, pushCommand{oldOID: f[0], newOID: f[1], ref: f[2]})
	}
}

func isOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// admitAPI: the three REST writes of the allowlist.
func (g *PushGate) admitAPI(r *http.Request, segs []string) (*http.Request, error) {
	path := strings.Join(segs, "/")
	if len(segs) < 4 || !strings.EqualFold(segs[0], "repos") || strings.HasSuffix(segs[2], ".git") {
		return nil, refused("%s api.github.com/%s", r.Method, path)
	}
	sr, ok := g.shipRepo(segs[1], segs[2])
	if !ok {
		return nil, refused("%s api.github.com/%s", r.Method, path)
	}
	gh := g.ghFor(sr.Repo) // nil until a rule needs a read (ownPR, the draft gate)
	tail := make([]string, len(segs)-3)
	for i, s := range segs[3:] {
		tail[i] = strings.ToLower(s)
	}
	switch {
	case r.Method == http.MethodPost && len(tail) == 1 && tail[0] == "pulls":
		return g.admitJSON(r, map[string]bool{"title": true, "body": true, "head": true, "base": true, "draft": true,
			"maintainer_can_modify": true}, func(m map[string]json.RawMessage) error {
			head, base := jsonString(m["head"]), jsonString(m["base"])
			owner, _, _ := strings.Cut(sr.Repo, "/")
			if o, b, ok := strings.Cut(head, ":"); ok {
				if !strings.EqualFold(o, owner) {
					return refused("a pull request from another account's fork (%s)", head)
				}
				head = b
			}
			if err := g.allowedRef(sr, "refs/heads/"+head); err != nil {
				return err
			}
			if base != sr.Base {
				return refused("a pull request into %q: the base must be %s", base, sr.Base)
			}
			if g.Ship.IsDraft() {
				return g.draftCreate(r.Context(), sr, gh, head, m)
			}
			return nil
		})
	case r.Method == http.MethodPatch && len(tail) == 2 && tail[0] == "pulls" && isNumber(tail[1]):
		n, _ := strconv.Atoi(tail[1])
		return g.admitJSON(r, map[string]bool{"title": true, "body": true, "state": true, "draft": true}, func(m map[string]json.RawMessage) error {
			if _, ok := m["draft"]; ok && g.Ship.IsDraft() {
				return refused("the field \"draft\": a draft officer's pull requests stay drafts; a human marks one ready")
			}
			if _, ok := m["draft"]; ok {
				// Before the own-PR read: refused whatever the PR, value or
				// other fields, and the refusal says how to do it instead.
				return refused("the field \"draft\": GitHub's REST PATCH pulls does not convert a draft; mark your own draft ready "+
					"for review with the %s{pr} tool (there is no ready -> draft)", officers.ToolMarkReady)
			}
			if g.Ship.IsDraft() && strings.EqualFold(jsonString(m["state"]), "open") {
				// Before the own-PR read, as draft: reopening is a human's.
				return refused("the state \"open\": a draft officer never reopens a pull request (a human does)")
			}
			pr, err := g.ownPR(r.Context(), sr, gh, n)
			if err != nil || !g.Ship.IsDraft() {
				return err
			}
			return g.draftEdit(r.Context(), sr, gh, pr, m)
		})
	case r.Method == http.MethodPost && len(tail) == 3 && tail[0] == "issues" && isNumber(tail[1]) && tail[2] == "comments":
		n, _ := strconv.Atoi(tail[1])
		return g.admitJSON(r, map[string]bool{"body": true}, func(m map[string]json.RawMessage) error {
			pr, err := g.ownPR(r.Context(), sr, gh, n)
			if err != nil || !g.Ship.IsDraft() {
				return err
			}
			return g.draftComment(r.Context(), sr, pr, jsonString(m["body"]))
		})
	}
	return nil, refused("%s api.github.com/%s", r.Method, path)
}

func isNumber(s string) bool {
	if s == "" || len(s) > 9 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s[0] != '0'
}

func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// ownPR refuses unless pull request n is the officer's own — THE rule the
// gated tools use ([owner.ownPR]: into the base, from a prefix branch of the
// repository, authored by the App's bot typed Bot). A read failure is not a
// refusal (the proxy answers 502: the policy could not decide). Pushes never
// come here: a branch is pushed before any pull request exists.
func (g *PushGate) ownPR(ctx context.Context, sr officers.ShipRepo, gh GitHub, n int) (PR, error) {
	if gh == nil {
		return PR{}, fmt.Errorf("no GitHub reader for %s", sr.Repo)
	}
	pr, err := gh.PR(ctx, n)
	if err != nil {
		return PR{}, fmt.Errorf("could not read pull request #%d: %v", n, err)
	}
	err = owner{repo: sr.Repo, ship: g.Ship.ForRepo(sr), gh: gh}.ownPR(ctx, pr, func(string) {})
	if r, ok := AsRefusal(err); ok {
		return PR{}, refused("editing pull request #%d, which is not this officer's own: %s", n, r.Reason)
	}
	return pr, err
}

// admitJSON reads a bounded JSON object body with no duplicate key and only
// allowed keys, runs check on it, and forwards the same bytes.
func (g *PushGate) admitJSON(r *http.Request, allowed map[string]bool, check func(map[string]json.RawMessage) error) (*http.Request, error) {
	if enc := strings.TrimSpace(r.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
		return nil, refused("a request body with Content-Encoding %q", enc)
	}
	var raw []byte
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
		if err != nil {
			return nil, fmt.Errorf("reading the request body: %v", err)
		}
		raw = b
	}
	if len(raw) > maxJSONBody {
		return nil, refused("a request body over 1 MiB")
	}
	m, err := strictObject(raw)
	if err != nil {
		return nil, refused("%v", err)
	}
	for k := range m {
		if !allowed[k] {
			return nil, refused("the field %q", k)
		}
	}
	if check != nil {
		if err := check(m); err != nil {
			return nil, err
		}
	}
	out := r.Clone(r.Context())
	out.Body = io.NopCloser(bytes.NewReader(raw))
	out.ContentLength = int64(len(raw))
	out.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	return out, nil
}

// strictObject parses a JSON object whose keys are unique. Two parsers can
// disagree on which of two equal keys wins; refusing the duplicate means the
// value checked is the value GitHub reads.
func strictObject(raw []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	t, err := dec.Token()
	if err != nil || t != json.Delim('{') {
		return nil, errors.New("a request body that is not a JSON object")
	}
	m := map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, errors.New("a request body that is not valid JSON")
		}
		k, _ := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, errors.New("a request body that is not valid JSON")
		}
		if _, dup := m[k]; dup {
			return nil, fmt.Errorf("a request body naming %q twice", k)
		}
		m[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, errors.New("a request body that is not valid JSON")
	}
	if dec.More() {
		return nil, errors.New("a request body with data after the object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("a request body with data after the object")
	}
	return m, nil
}

// BotLogin is a GitHub App's bot account login.
func BotLogin(slug string) string { return slug + "[bot]" }
