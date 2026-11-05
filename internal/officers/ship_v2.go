package officers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/githubapp"
)

// ---------------------------------------------------------------------------
// SHIP v2: SEVERAL REPOSITORIES AND THE `draft` STAGE (docs/officers.md
// "Shipping officers").
//
// A `draft` officer opens DRAFT pull requests in up to MaxShipRepos
// repositories and nothing more: it is never offered officer_mark_ready,
// officer_merge_pr, officer_tag_release or officer_rollout_status, its push
// gate refuses a pull request that is not a draft and every edit that would
// make one reviewable, and every pull request it opens or edits is checked
// against the comparison GitHub reports (forbidden paths, removed hunks of the
// kinds a merge must never delete, secret shapes), a body contract and rate
// limits (internal/officership's draft gate).
//
// 🔴 EVERY v2 RULE IS THE `draft` STAGE'S. A shadow, merge or release officer
// parses and behaves exactly as before; the keys only `draft` honours
// (`forbidden_paths`, several `repos`) are refused on the other stages rather
// than accepted and ignored.
// ---------------------------------------------------------------------------

// StageDraft: open DRAFT pull requests only (in one to MaxShipRepos
// repositories). No tool converts, merges or tags; required_checks and
// review_check are not required.
const StageDraft = "draft"

// Limits of a draft-stage officer.
const (
	// MaxShipRepos is how many repositories `ship.repos` may list.
	MaxShipRepos = 3
	// MaxOpenPRsPerRepo is how many of the officer's own pull requests may be
	// open in one repository at once.
	MaxOpenPRsPerRepo = 2
	// MaxNewPRsPerDay is how many pull requests the officer may open per UTC
	// day, across its repositories.
	MaxNewPRsPerDay = 3
	// MaxPRsPerItem is how many pull requests one HQ item may have, in any
	// state, across the officer's repositories.
	MaxPRsPerItem = 1
	// OrphanBranchAge is how old an officer branch with no pull request must be
	// before the daily report lists it.
	OrphanBranchAge = 7 * 24 * time.Hour
	// maxForbiddenPaths bounds one repository's forbidden_paths.
	maxForbiddenPaths = 64
)

// BodySections are the headings a draft-stage pull request body must carry,
// each as a line of its own.
var BodySections = []string{"## Root cause", "## Evidence", "## Test", "## Rollback"}

// BodyFirstLine is the first line a draft-stage officer's pull request body
// must be, exactly.
func BodyFirstLine(officerID string, item int) string {
	return fmt.Sprintf("Officer: %s · item %d · authored from untrusted inputs; review as such", officerID, item)
}

// ShipRepo is one repository a shipping officer writes to.
type ShipRepo struct {
	// Repo is owner/name.
	Repo string
	// Base and BranchPrefix are as Ship's, for this repository.
	Base         string
	BranchPrefix string
	// AppID and AppAccount are the officer App and the installation account
	// (the repository's owner) its token for Repo is minted from.
	AppID      int64
	AppAccount githubapp.Account
	// ForbiddenPaths are globs (MatchForbidden) no pull request of the officer
	// may change, add, remove or rename from. Draft stage only.
	ForbiddenPaths []string
	// ForbiddenPathsFile, when set, is a repository file listing more
	// forbidden globs, one per line; the draft gate reads it from the pull
	// request's TARGET branch at every check (draftgate.go).
	ForbiddenPathsFile string
}

// RepoList is the officer's ship repositories: Repos when parsed, else the one
// repository primary described by the Ship's own fields (a Ship built outside
// Parse, as tests do).
func (s *Ship) RepoList(primary string) []ShipRepo {
	if s == nil {
		return nil
	}
	if len(s.Repos) > 0 {
		return s.Repos
	}
	return []ShipRepo{{Repo: primary, Base: s.Base, BranchPrefix: s.BranchPrefix, AppID: s.AppID, AppAccount: s.AppAccount}}
}

// RepoFor is the ship repository named repo (compared without case), ok false
// when the officer does not ship to it.
func (s *Ship) RepoFor(primary, repo string) (ShipRepo, bool) {
	for _, r := range s.RepoList(primary) {
		if strings.EqualFold(r.Repo, repo) {
			return r, true
		}
	}
	return ShipRepo{}, false
}

// ForRepo is a copy of s whose Base, BranchPrefix and App are r's: the policy
// every single-repository rule (the own-pull-request rule, the ref rule) is
// asked with for r.
func (s *Ship) ForRepo(r ShipRepo) *Ship {
	c := *s
	c.Base, c.BranchPrefix, c.AppID, c.AppAccount = r.Base, r.BranchPrefix, r.AppID, r.AppAccount
	return &c
}

// IsDraft reports whether the stage is StageDraft.
func (s *Ship) IsDraft() bool { return s != nil && s.Stage == StageDraft }

// MayMarkReady reports whether officer_mark_ready is the officer's: every
// stage but draft.
func (s *Ship) MayMarkReady() bool { return s != nil && s.Stage != StageDraft }

// declaredShipRepo is one entry of `ship.repos`.
type declaredShipRepo struct {
	Repo         string `json:"repo"`
	Base         string `json:"base"`
	BranchPrefix string `json:"branch_prefix"`
	GitHubApp    *struct {
		ID      int64  `json:"id"`
		Account string `json:"account"`
	} `json:"github_app"`
	ForbiddenPaths     []string `json:"forbidden_paths"`
	ForbiddenPathsFile string   `json:"forbidden_paths_file"`
}

// parseShipRepo validates one repository's base, prefix, App and forbidden
// paths. what names it in errors.
func parseShipRepo(what string, d declaredShipRepo) (ShipRepo, error) {
	r := ShipRepo{Repo: strings.TrimSpace(d.Repo), Base: strings.TrimSpace(d.Base), BranchPrefix: strings.TrimSpace(d.BranchPrefix)}
	if !github.ValidRepoName(r.Repo) {
		return ShipRepo{}, fmt.Errorf("ship: %srepo %q is not a GitHub owner/name", what, d.Repo)
	}
	if r.Base == "" {
		r.Base = DefaultShipBase
	}
	if !validBranchName(r.Base) {
		return ShipRepo{}, fmt.Errorf("ship: %sbase %q is not a branch name", what, r.Base)
	}
	if r.BranchPrefix == "" {
		r.BranchPrefix = DefaultBranchPrefix
	}
	if !strings.HasSuffix(r.BranchPrefix, "/") || !validBranchName(strings.TrimSuffix(r.BranchPrefix, "/")) {
		return ShipRepo{}, fmt.Errorf("ship: %sbranch_prefix %q must be a branch-name prefix ending in '/' (e.g. %q)", what, r.BranchPrefix, DefaultBranchPrefix)
	}
	if strings.HasPrefix(r.Base+"/", r.BranchPrefix) || strings.HasPrefix(r.BranchPrefix, r.Base+"/") {
		return ShipRepo{}, fmt.Errorf("ship: %sbase %q and branch_prefix %q overlap: the officer could push to its own base", what, r.Base, r.BranchPrefix)
	}
	if d.GitHubApp == nil || d.GitHubApp.ID <= 0 {
		return ShipRepo{}, fmt.Errorf("ship: %sgithub_app.id is required: the officer's OWN GitHub App (never the deployment's)", what)
	}
	r.AppID = d.GitHubApp.ID
	login, idStr, ok := strings.Cut(strings.TrimSpace(d.GitHubApp.Account), ":")
	accID, err := strconv.ParseInt(idStr, 10, 64)
	if !ok || !loginRE.MatchString(login) || err != nil || accID <= 0 {
		return ShipRepo{}, fmt.Errorf("ship: %sgithub_app.account %q must be <login>:<numeric id> (the id is what is matched)", what, d.GitHubApp.Account)
	}
	owner, _, _ := strings.Cut(r.Repo, "/")
	if !strings.EqualFold(owner, login) {
		return ShipRepo{}, fmt.Errorf("ship: %sgithub_app.account %q is not the owner of repo %q", what, login, r.Repo)
	}
	r.AppAccount = githubapp.Account{Login: login, ID: accID}
	if len(d.ForbiddenPaths) > maxForbiddenPaths {
		return ShipRepo{}, fmt.Errorf("ship: %sat most %d forbidden_paths", what, maxForbiddenPaths)
	}
	seen := map[string]bool{}
	for _, p := range d.ForbiddenPaths {
		p = strings.TrimSpace(p)
		if _, err := compileGlob(p); err != nil {
			return ShipRepo{}, fmt.Errorf("ship: %sforbidden_paths entry %q: %v", what, p, err)
		}
		if !seen[p] {
			seen[p] = true
			r.ForbiddenPaths = append(r.ForbiddenPaths, p)
		}
	}
	if d.ForbiddenPathsFile != "" {
		f := strings.TrimSpace(d.ForbiddenPathsFile)
		if !ValidListFile(f) {
			return ShipRepo{}, fmt.Errorf("ship: %sforbidden_paths_file %q must be a repository-relative file path (no leading '/', no '.' or '..' segment, no glob characters)",
				what, d.ForbiddenPathsFile)
		}
		r.ForbiddenPathsFile = f
	}
	return r, nil
}

// shipRepos reads the repository half of a `ship` block: either the
// single-repository form (base, branch_prefix, github_app and, on draft,
// forbidden_paths at the top, for the officer's `repo`) or `repos`, which
// must then hold every one of those and list `repo`. The returned list puts
// no entry first: the Ship's own fields are filled from the one named repo.
func shipRepos(stage, repo string, d declaredShip) ([]ShipRepo, error) {
	if len(d.Repos) == 0 || string(d.Repos) == "null" {
		if (len(d.ForbiddenPaths) > 0 || d.ForbiddenPathsFile != "") && stage != StageDraft {
			return nil, fmt.Errorf("ship: forbidden_paths is enforced by the %s stage only; stage %s would ignore it — remove it", StageDraft, stage)
		}
		r, err := parseShipRepo("", declaredShipRepo{Repo: repo, Base: d.Base, BranchPrefix: d.BranchPrefix, GitHubApp: d.GitHubApp,
			ForbiddenPaths: d.ForbiddenPaths, ForbiddenPathsFile: d.ForbiddenPathsFile})
		if err != nil {
			return nil, err
		}
		if err := draftPrefixPinned(stage, "", r); err != nil {
			return nil, err
		}
		return []ShipRepo{r}, nil
	}
	for name, set := range map[string]bool{"base": strings.TrimSpace(d.Base) != "", "branch_prefix": strings.TrimSpace(d.BranchPrefix) != "",
		"github_app": d.GitHubApp != nil, "forbidden_paths": d.ForbiddenPaths != nil, "forbidden_paths_file": d.ForbiddenPathsFile != ""} {
		if set {
			return nil, fmt.Errorf("ship: with repos, set %s on each entry; remove the top-level %s", name, name)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(d.Repos))
	dec.DisallowUnknownFields()
	var list []declaredShipRepo
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf(`ship: repos must be a list of {"repo", "base"?, "branch_prefix"?, "github_app", "forbidden_paths"?}: %v`, err)
	}
	if n := len(list); n < 1 || n > MaxShipRepos {
		return nil, fmt.Errorf("ship: repos lists %d repositories; 1..%d", n, MaxShipRepos)
	}
	if len(list) > 1 && stage != StageDraft {
		return nil, fmt.Errorf("ship: several repos are supported on the %s stage only (stage %s merges and tags in one repository)", StageDraft, stage)
	}
	seen := map[string]bool{}
	var out []ShipRepo
	listsRepo := false
	for i, e := range list {
		r, err := parseShipRepo(fmt.Sprintf("repos[%d].", i), e)
		if err != nil {
			return nil, err
		}
		if err := draftPrefixPinned(stage, fmt.Sprintf("repos[%d].", i), r); err != nil {
			return nil, err
		}
		if (len(r.ForbiddenPaths) > 0 || r.ForbiddenPathsFile != "") && stage != StageDraft {
			return nil, fmt.Errorf("ship: forbidden_paths is enforced by the %s stage only; stage %s would ignore it — remove it", StageDraft, stage)
		}
		lr := strings.ToLower(r.Repo)
		if seen[lr] {
			return nil, fmt.Errorf("ship: repos lists %q twice", r.Repo)
		}
		seen[lr] = true
		if strings.EqualFold(r.Repo, repo) {
			listsRepo = true
		}
		if len(out) > 0 && r.AppID != out[0].AppID {
			return nil, fmt.Errorf("ship: repos[%d].github_app.id %d differs from repos[0]'s %d: an officer has ONE App, whose key is %s<NAME>",
				i, r.AppID, out[0].AppID, AppKeyEnvPrefix)
		}
		out = append(out, r)
	}
	if !listsRepo {
		return nil, fmt.Errorf("ship: repos does not list the officer's `repo` %q: it is the repository a request naming none is served for", repo)
	}
	return out, nil
}

// draftPrefixPinned refuses a draft-stage repository whose branch prefix is
// not exactly DefaultBranchPrefix.
//
// 🔴 PINNED, NOT DEFAULTED: the target repositories' own walls (their CI's
// officer-pull-request checks) select the officer's pull requests by that
// literal prefix, so another prefix would make every one of them skip
// silently. Other stages keep a configurable prefix.
func draftPrefixPinned(stage, what string, r ShipRepo) error {
	if stage == StageDraft && r.BranchPrefix != DefaultBranchPrefix {
		return fmt.Errorf("ship: %sbranch_prefix %q must be exactly %q on the %s stage (the target repositories' checks select officer pull requests by that prefix); remove it or set it to %q",
			what, r.BranchPrefix, DefaultBranchPrefix, StageDraft, DefaultBranchPrefix)
	}
	return nil
}

// branchItemRE is the draft-stage branch shape after the prefix and the
// officer's slug: <item>-<slug>.
var branchItemRE = regexp.MustCompile(`^([1-9][0-9]{0,9})-[A-Za-z0-9][A-Za-z0-9._-]*$`)

// BranchItem is the HQ item a draft-stage officer branch is for: branch must
// be <prefix><officer slug>/<item>-<slug>. ok false for any other shape.
func BranchItem(prefix, officerID, branch string) (int, bool) {
	rest, ok := strings.CutPrefix(branch, prefix+Slug(officerID)+"/")
	if !ok || !ValidRefSuffix(branch) {
		return 0, false
	}
	m := branchItemRE.FindStringSubmatch(rest)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// MatchForbidden reports whether repository path p matches forbidden-path
// glob pat:
//
//   - `*` is any run of characters but '/', `?` one such character, and
//     `{a,b}` either alternative (no nesting);
//   - `**` as a whole segment is any number of segments, none included
//     (`**/secrets/**`, `clusters/x/**`);
//   - a pattern ending in '/' is a directory: anything under it;
//   - a pattern with no '/' is matched against EVERY segment of p, so
//     `.sops.yaml` and `*.enc.yaml` catch the file in any directory.
//
// A pattern that does not compile matches everything: a forbidden list is
// a wall, and a broken brick is not a gap (Parse refuses one anyway).
func MatchForbidden(pat, p string) bool {
	re, err := compileGlob(pat)
	if err != nil {
		return true
	}
	if !strings.Contains(pat, "/") {
		for _, seg := range strings.Split(p, "/") {
			if re.MatchString(seg) {
				return true
			}
		}
		return false
	}
	return re.MatchString(p)
}

// compileGlob is MatchForbidden's pattern as an anchored regexp.
func compileGlob(pat string) (*regexp.Regexp, error) {
	if pat == "" || strings.HasPrefix(pat, "/") {
		return nil, errors.New("must be a non-empty repository-relative pattern")
	}
	if strings.ContainsAny(pat, "[]\\") {
		return nil, errors.New("character classes and escapes are not supported")
	}
	if strings.HasSuffix(pat, "/") {
		pat += "**"
	}
	segs := strings.Split(pat, "/")
	var b strings.Builder
	b.WriteString("^")
	for i, seg := range segs {
		last := i == len(segs)-1
		if seg == "" || seg == "." || seg == ".." {
			return nil, errors.New("has an empty, '.' or '..' segment")
		}
		if seg == "**" {
			if last {
				b.WriteString(".+")
			} else {
				b.WriteString("(?:[^/]+/)*")
			}
			continue
		}
		if strings.Contains(seg, "**") {
			return nil, errors.New("`**` must be a whole segment")
		}
		s, err := segmentRegex(seg)
		if err != nil {
			return nil, err
		}
		b.WriteString(s)
		if !last {
			b.WriteString("/")
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// segmentRegex is one segment of a glob (no '/') as a regexp fragment.
func segmentRegex(seg string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		switch c := seg[i]; c {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '{':
			j := strings.IndexByte(seg[i:], '}')
			if j < 0 {
				return "", errors.New("has an unbalanced '{'")
			}
			body := seg[i+1 : i+j]
			if strings.ContainsAny(body, "{") || body == "" {
				return "", errors.New("has a nested or empty '{…}'")
			}
			var alts []string
			for _, a := range strings.Split(body, ",") {
				if a == "" {
					return "", errors.New("has an empty alternative in '{…}'")
				}
				s, err := segmentRegex(a)
				if err != nil {
					return "", err
				}
				alts = append(alts, s)
			}
			b.WriteString("(?:" + strings.Join(alts, "|") + ")")
			i += j
		case '}', ',':
			if c == '}' {
				return "", errors.New("has an unbalanced '}'")
			}
			b.WriteString(regexp.QuoteMeta(string(c)))
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String(), nil
}

// ValidListFile reports whether f may name a forbidden_paths_file: a
// repository-relative file path with no empty, '.' or '..' segment, no
// trailing '/', and no glob character (it is forbidden itself, by its exact
// path).
func ValidListFile(f string) bool {
	if f == "" || strings.HasPrefix(f, "/") || strings.HasSuffix(f, "/") || strings.ContainsAny(f, "*?{}[]\\") {
		return false
	}
	for _, seg := range strings.Split(f, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// ParseForbiddenList reads a forbidden_paths_file's content: one glob per
// line, surrounding space trimmed, blank lines and lines starting with '#'
// skipped. Any line that is not a valid glob (MatchForbidden) is an error
// naming its line number: the caller refuses rather than guesses.
func ParseForbiddenList(content string) ([]string, error) {
	var out []string
	for i, l := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if _, err := compileGlob(l); err != nil {
			return nil, fmt.Errorf("line %d %q: %v", i+1, l, err)
		}
		out = append(out, l)
	}
	return out, nil
}
