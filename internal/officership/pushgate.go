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
//     App's bot authored it, its head is <prefix>…), title/body/state only;
//   - POST /repos/{repo}/issues/{n}/comments on its OWN pull request.
//
// Every other method, path and host shape is refused — the merge endpoint, the
// git/refs and git/tags endpoints, releases, the contents API (which commits to
// any branch), the merges API, GraphQL (which can do all of those), and every
// DELETE. Merges and tags are made by officer_merge_pr and officer_tag_release
// in muster-server, after their gates.
type PushGate struct {
	// Repo is the officer's repository (owner/name).
	Repo string
	// Ship is the officer's policy (Base, BranchPrefix).
	Ship *officers.Ship
	// GitHost and APIHost are the hosts served (github.com, api.github.com).
	GitHost, APIHost string
	// Cred mints the credential for an admitted request (the officer App's
	// connectors.Virtual Credential: its token for Repo, anonymous elsewhere).
	Cred func(ctx context.Context, caller connectors.Caller, host, rest string) (string, bool, error)
	// OnRejected drops a cached credential after a 401.
	OnRejected func(caller connectors.Caller)
	// GH reads a pull request (PATCH's own-PR check) and the App's slug.
	GH GitHub
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
func (g *PushGate) Credential(ctx context.Context, caller connectors.Caller, host, rest string) (string, bool, error) {
	caller.Repo, caller.ReadOnly = g.Repo, false
	return g.Cred(ctx, caller, host, rest)
}

// Rejected implements connectors.Gate.
func (g *PushGate) Rejected(caller connectors.Caller) {
	if g.OnRejected != nil {
		g.OnRejected(caller)
	}
}

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s (merging and tagging go through the %s and %s tools; nothing else writes)",
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

func (g *PushGate) ownRepo(owner, name string) bool {
	return connectors.SameRepo(owner+"/"+strings.TrimSuffix(name, ".git"), g.Repo)
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
	if !g.ownRepo(segs[0], segs[1]) {
		return nil, refused("a push to %s/%s, which is not this officer's repository %s", segs[0], segs[1], g.Repo)
	}
	return g.admitReceivePack(r)
}

// admitReceivePack reads the pkt-line command section of a push (it ends at
// the first flush-pkt), refuses it unless every command updates a ref under
// refs/heads/<prefix>, and forwards the request with the bytes it read put
// back in front of the rest of the body.
//
// A gzip-encoded body is decoded and forwarded decoded (git accepts either);
// any other encoding is refused, since the commands could not be read.
func (g *PushGate) admitReceivePack(r *http.Request) (*http.Request, error) {
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
	refs, err := readPushCommands(br, &pre)
	if err != nil {
		return nil, refused("%v", err)
	}
	for _, ref := range refs {
		if err := g.allowedRef(ref); err != nil {
			return nil, err
		}
	}
	out.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(pre.Bytes()), br), body}
	return out, nil
}

// allowedRef is THE ref rule: refs/heads/<prefix><valid suffix>.
func (g *PushGate) allowedRef(ref string) error {
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok {
		return refused("a push to %s: only branches under refs/heads/%s may be pushed (no tags, no other refs)", ref, g.Ship.BranchPrefix)
	}
	suffix, ok := strings.CutPrefix(branch, g.Ship.BranchPrefix)
	if !ok || !officers.ValidRefSuffix(suffix) || !officers.ValidRefSuffix(branch) {
		return refused("a push to %s: only branches under refs/heads/%s may be pushed (never %s)", ref, g.Ship.BranchPrefix, g.Ship.Base)
	}
	return nil
}

// readPushCommands reads pkt-lines from br into pre up to and including the
// first flush-pkt, and returns the ref of every update command. Shallow lines
// are skipped; a push certificate (whose commands sit inside the certificate)
// is refused. Zero commands is allowed: git's probe request is a bare
// flush-pkt and updates nothing.
func readPushCommands(br *bufio.Reader, pre *bytes.Buffer) ([]string, error) {
	var refs []string
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
		refs = append(refs, f[2])
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
	if len(segs) < 4 || !strings.EqualFold(segs[0], "repos") || !g.ownRepo(segs[1], segs[2]) || strings.HasSuffix(segs[2], ".git") {
		return nil, refused("%s api.github.com/%s", r.Method, path)
	}
	tail := make([]string, len(segs)-3)
	for i, s := range segs[3:] {
		tail[i] = strings.ToLower(s)
	}
	switch {
	case r.Method == http.MethodPost && len(tail) == 1 && tail[0] == "pulls":
		return g.admitJSON(r, map[string]bool{"title": true, "body": true, "head": true, "base": true, "draft": true,
			"maintainer_can_modify": true}, func(m map[string]json.RawMessage) error {
			head, base := jsonString(m["head"]), jsonString(m["base"])
			owner, _, _ := strings.Cut(g.Repo, "/")
			if o, b, ok := strings.Cut(head, ":"); ok {
				if !strings.EqualFold(o, owner) {
					return refused("a pull request from another account's fork (%s)", head)
				}
				head = b
			}
			if err := g.allowedRef("refs/heads/" + head); err != nil {
				return err
			}
			if base != g.Ship.Base {
				return refused("a pull request into %q: the base must be %s", base, g.Ship.Base)
			}
			return nil
		})
	case r.Method == http.MethodPatch && len(tail) == 2 && tail[0] == "pulls" && isNumber(tail[1]):
		n, _ := strconv.Atoi(tail[1])
		return g.admitJSON(r, map[string]bool{"title": true, "body": true, "state": true}, func(map[string]json.RawMessage) error {
			return g.ownPR(r.Context(), n)
		})
	case r.Method == http.MethodPost && len(tail) == 3 && tail[0] == "issues" && isNumber(tail[1]) && tail[2] == "comments":
		n, _ := strconv.Atoi(tail[1])
		return g.admitJSON(r, map[string]bool{"body": true}, func(map[string]json.RawMessage) error {
			return g.ownPR(r.Context(), n)
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

// ownPR refuses unless pull request n is the officer's own: authored by its
// App's bot and from a branch under the prefix in its own repository.
func (g *PushGate) ownPR(ctx context.Context, n int) error {
	slug, err := g.GH.AppSlug(ctx)
	if err != nil {
		return fmt.Errorf("could not read the officer App's identity: %v", err)
	}
	pr, err := g.GH.PR(ctx, n)
	if err != nil {
		return fmt.Errorf("could not read pull request #%d: %v", n, err)
	}
	if !strings.EqualFold(pr.Author, BotLogin(slug)) || !strings.HasPrefix(pr.HeadRef, g.Ship.BranchPrefix) ||
		!connectors.SameRepo(pr.HeadRepo, g.Repo) {
		return refused("editing pull request #%d, which is not this officer's own (%s from %s)", n, pr.Author, pr.HeadRef)
	}
	return nil
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
