package officers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// THE `shepherd` STAGE: AN OFFICER THAT SHEPHERDS OTHER OFFICERS' PULL
// REQUESTS (docs/officers.md "The shepherd stage").
//
// A shepherd opens no pull request of its own. It acts, with its OWN GitHub
// App, on pull requests other officers opened, and only on one that is
//
//   - open, into the configured base of one of its `ship.repos`;
//   - from a branch under that repository's branch_prefix (default
//     `officer/`), in that repository (not a fork);
//   - authored by a login in `ship.shepherd.authors` (never its own App's);
//   - the subject of an HQ item THIS instance holds under a live lease: the
//     item's subject_key is exactly ShepherdSubject(repo, n).
//
// On such a pull request it may push (fast-forward only, through the
// AUTHORING officer's forbidden paths — ShepherdRules), comment, bring the
// branch up to date with its base (GitHub's update-branch, a merge) and mark
// it ready for review (officer_shepherd_mark_ready). It never opens, edits,
// closes, reopens or merges a pull request, never creates or deletes a
// branch, never force-pushes and never tags (internal/officership's push gate
// and ShepherdGates).
// ---------------------------------------------------------------------------

// StageShepherd: push (fast-forward only), comment, update-branch and mark
// ready, on other officers' pull requests only. No merge, no tag.
const StageShepherd = "shepherd"

// ToolShepherdReady is the shepherd's mark-ready tool: draft -> ready for
// review on a pull request it shepherds (officership.ShepherdGates.MarkReady).
const ToolShepherdReady = "officer_shepherd_mark_ready"

// maxShepherdAuthors bounds ship.shepherd.authors.
const maxShepherdAuthors = 16

// Shepherd is a shepherd-stage officer's `ship.shepherd` block.
type Shepherd struct {
	// Authors are whose pull requests the shepherd may act on.
	Authors []ShepherdAuthor
	// Auditors (RED-FIRST STUB: read, never checked).
	Auditors []string
}

// Auditor (RED-FIRST STUB).
func (sh *Shepherd) Auditor(login string) bool {
	if sh == nil {
		return false
	}
	for _, a := range sh.Auditors {
		if strings.EqualFold(a, login) {
			return true
		}
	}
	return false
}

// ShepherdAuthor is one entry of ship.shepherd.authors.
type ShepherdAuthor struct {
	// Login is the pull request author's GitHub login (an App's bot is
	// `<slug>[bot]`), compared without case.
	Login string
	// Officer, when set, is the id of an officer declared on THIS muster whose
	// App the login is: its ship.repos entry for a repository is where the
	// forbidden paths a shepherd push there must pass are looked up
	// (ShepherdRules), and the gates check, at call time, that its App's bot
	// login is Login.
	Officer string
}

// IsShepherd reports whether the stage is StageShepherd.
func (s *Ship) IsShepherd() bool { return s != nil && s.Stage == StageShepherd }

// ShepherdSubject is the HQ item subject_key a shepherd must hold to act on
// pull request n of repo: `<owner>/<name>#<n>`, the repository lower-cased.
func ShepherdSubject(repo string, n int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(n)
}

// SameShepherdSubject reports whether an HQ item's subject_key names pull
// request n of repo (the repository compared without case, the number
// exactly; surrounding space ignored).
func SameShepherdSubject(subject, repo string, n int) bool {
	r, num, ok := strings.Cut(strings.TrimSpace(subject), "#")
	return ok && strings.EqualFold(r, strings.TrimSpace(repo)) && num == strconv.Itoa(n)
}

// botLoginRE is a GitHub login, optionally an App's `[bot]` account.
var botLoginRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})(?:\[bot\])?$`)

// declaredShepherdAuthor is an authors entry: a login string, or
// {"login", "officer"?}.
type declaredShepherdAuthor struct {
	Login   string `json:"login"`
	Officer string `json:"officer"`
}

func (d *declaredShepherdAuthor) UnmarshalJSON(b []byte) error {
	if s := bytes.TrimSpace(b); len(s) > 0 && s[0] == '"' {
		return json.Unmarshal(s, &d.Login)
	}
	type plain declaredShepherdAuthor
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p plain
	if err := dec.Decode(&p); err != nil {
		return err
	}
	*d = declaredShepherdAuthor(p)
	return nil
}

type declaredShepherd struct {
	Authors  []declaredShepherdAuthor `json:"authors"`
	Auditors []string                 `json:"auditors"`
}

// parseShepherd reads a `ship.shepherd` block. Cross-officer rules (an
// author's officer exists, ships, is not the shepherd) are validateShepherds'.
func parseShepherd(id string, raw json.RawMessage) (*Shepherd, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d declaredShepherd
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf(`ship: shepherd must be {"authors": [<login> | {"login", "officer"?}, …]}: %v`, err)
	}
	if len(d.Authors) == 0 {
		return nil, fmt.Errorf("ship: shepherd.authors is empty: a shepherd acts only on pull requests authored by a listed login, so with none it could act on nothing — there is no default")
	}
	if len(d.Authors) > maxShepherdAuthors {
		return nil, fmt.Errorf("ship: shepherd.authors lists %d logins, over the cap of %d", len(d.Authors), maxShepherdAuthors)
	}
	sh := &Shepherd{Auditors: d.Auditors}
	seen := map[string]bool{}
	for i, a := range d.Authors {
		login, off := strings.TrimSpace(a.Login), strings.TrimSpace(a.Officer)
		if !botLoginRE.MatchString(login) {
			return nil, fmt.Errorf("ship: shepherd.authors[%d] login %q is not a GitHub login (an App's bot is <slug>[bot])", i, a.Login)
		}
		if seen[strings.ToLower(login)] {
			return nil, fmt.Errorf("ship: shepherd.authors lists %q twice", login)
		}
		seen[strings.ToLower(login)] = true
		if off != "" && !idRE.MatchString(off) {
			return nil, fmt.Errorf("ship: shepherd.authors[%d] officer %q is not an officer id (<type>:<area>)", i, a.Officer)
		}
		if off == id {
			return nil, fmt.Errorf("ship: shepherd.authors[%d] names this officer itself (%s): a shepherd never acts on its own work", i, id)
		}
		sh.Authors = append(sh.Authors, ShepherdAuthor{Login: login, Officer: off})
	}
	return sh, nil
}

// Author is the authors entry for login (compared without case).
func (sh *Shepherd) Author(login string) (ShepherdAuthor, bool) {
	if sh == nil {
		return ShepherdAuthor{}, false
	}
	for _, a := range sh.Authors {
		if strings.EqualFold(a.Login, login) {
			return a, true
		}
	}
	return ShepherdAuthor{}, false
}

// ShepherdRule is what a shepherd may do on one author's pull requests in one
// repository: the wall a push there must pass.
type ShepherdRule struct {
	// Repo is the shepherd's ship repository; Login the author.
	Repo, Login string
	// AuthorOfficer is the authoring officer's id ("" for a login with no
	// officer on this muster).
	AuthorOfficer string
	// ForbiddenPaths are the globs (MatchForbidden) no shepherd push may touch;
	// ForbiddenPathsFiles the repository files listing more, each read from the
	// pull request's TARGET branch at every check (and each forbidden itself).
	ForbiddenPaths      []string
	ForbiddenPathsFiles []string
}

// Empty reports whether the rule's wall forbids nothing.
func (r ShepherdRule) Empty() bool {
	return len(r.ForbiddenPaths) == 0 && len(r.ForbiddenPathsFiles) == 0
}

// ShepherdRules are shepherd o's rules: one per (ship repository, author) the
// author can open pull requests in. 🔴 THE ONE PLACE A SHEPHERD'S WALL IS
// DECIDED: the union of the shepherd's own forbidden_paths for the repository
// (ship.repos[].forbidden_paths and forbidden_paths_file) and — for an author
// that names an officer — that officer's own forbidden paths for the same
// repository, LOOKED UP from its ship block, never copied. An author officer
// that does not ship to a repository has no rule there (its App opens no pull
// request there, so the shepherd acts on none). all is every declared officer.
// Boot refuses a rule whose wall is empty (validateShepherds).
func ShepherdRules(o Officer, all []Officer) []ShepherdRule {
	if !o.Ship.IsShepherd() || o.Ship.Shepherd == nil {
		return nil
	}
	byID := map[string]Officer{}
	for _, x := range all {
		byID[x.ID] = x
	}
	var out []ShepherdRule
	for _, r := range o.Ship.RepoList(o.Repo) {
		for _, a := range o.Ship.Shepherd.Authors {
			rule := ShepherdRule{Repo: r.Repo, Login: a.Login, AuthorOfficer: a.Officer}
			rule.ForbiddenPaths = append(rule.ForbiddenPaths, r.ForbiddenPaths...)
			if r.ForbiddenPathsFile != "" {
				rule.ForbiddenPathsFiles = append(rule.ForbiddenPathsFiles, r.ForbiddenPathsFile)
			}
			if a.Officer != "" {
				ao, ok := byID[a.Officer]
				if !ok || ao.Ship == nil {
					continue
				}
				ar, ok := ao.Ship.RepoFor(ao.Repo, r.Repo)
				if !ok {
					continue
				}
				rule.ForbiddenPaths = append(rule.ForbiddenPaths, ar.ForbiddenPaths...)
				if ar.ForbiddenPathsFile != "" {
					rule.ForbiddenPathsFiles = append(rule.ForbiddenPathsFiles, ar.ForbiddenPathsFile)
				}
			}
			rule.ForbiddenPaths = sortedUniqueStrings(rule.ForbiddenPaths)
			rule.ForbiddenPathsFiles = sortedUniqueStrings(rule.ForbiddenPathsFiles)
			out = append(out, rule)
		}
	}
	return out
}

func sortedUniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	set := map[string]bool{}
	for _, s := range in {
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// validateShepherds is Parse's cross-officer check of every shepherd in all.
// It refuses, naming the reason:
//
//   - an author officer that is not declared, has no ship block, is itself a
//     shepherd, or ships to none of the shepherd's repositories;
//   - an author officer whose App is the shepherd's own (github_app.id): the
//     shepherd's pushes would be the author's, so "never its own work" could
//     not hold;
//   - a (repository, author) whose wall (ShepherdRules) forbids nothing: a
//     shepherd push there would pass no forbidden-path check at all;
//   - two shepherds covering the same author in the same repository: two
//     writers racing on one branch.
func validateShepherds(all []Officer) error {
	byID := map[string]Officer{}
	for _, o := range all {
		byID[o.ID] = o
	}
	covered := map[string]string{}
	for _, o := range all {
		if !o.Ship.IsShepherd() {
			continue
		}
		for _, a := range o.Ship.Shepherd.Authors {
			if a.Officer == "" {
				continue
			}
			ao, ok := byID[a.Officer]
			switch {
			case !ok:
				return fmt.Errorf("officer %s: ship.shepherd.authors names officer %q, which is not declared in MUSTER_OFFICERS (its forbidden paths cannot be looked up)", o.ID, a.Officer)
			case ao.Ship == nil:
				return fmt.Errorf("officer %s: ship.shepherd.authors names officer %q, which has no ship block: it opens no pull request", o.ID, a.Officer)
			case ao.Ship.IsShepherd():
				return fmt.Errorf("officer %s: ship.shepherd.authors names officer %q, which is itself a shepherd: it opens no pull request", o.ID, a.Officer)
			case ao.Ship.AppID == o.Ship.AppID:
				return fmt.Errorf("officer %s: ship.shepherd.authors names officer %q, whose GitHub App (%d) is the shepherd's own: a shepherd fixes pull requests with its OWN App, never the author's", o.ID, a.Officer, o.Ship.AppID)
			}
			shared := false
			for _, r := range o.Ship.RepoList(o.Repo) {
				if _, ok := ao.Ship.RepoFor(ao.Repo, r.Repo); ok {
					shared = true
				}
			}
			if !shared {
				return fmt.Errorf("officer %s: ship.shepherd.authors names officer %q, which ships to none of the shepherd's repositories", o.ID, a.Officer)
			}
		}
		for _, rule := range ShepherdRules(o, all) {
			if rule.Empty() {
				return fmt.Errorf("officer %s: a push to %s on a pull request by %s would pass no forbidden-path check: set forbidden_paths (or forbidden_paths_file) on that ship.repos entry, or name the authoring officer whose own list applies", o.ID, rule.Repo, rule.Login)
			}
			key := strings.ToLower(rule.Repo) + " " + strings.ToLower(rule.Login)
			if other, dup := covered[key]; dup {
				return fmt.Errorf("officers %s and %s both shepherd %s's pull requests in %s: two writers would race on one branch", other, o.ID, rule.Login, rule.Repo)
			}
			covered[key] = o.ID
		}
	}
	return nil
}
