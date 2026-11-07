package connectors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// PINNED READS: a caller whose repository reads are pinned, each repository to
// ONE commit (Caller.ReadPin). Set for an instance of an officer with
// `pin_repo_reads_to_item` (api.Server.callerOf), whose pins come from the
// item it holds; nil for every other caller.
//
// 🔴 AN ALLOWLIST, NOT A DENYLIST. A pinned caller's request to the pinned
// connector is forwarded only when it is one of the shapes below, at exactly
// the pinned commit; everything else is refused before a credential exists:
//
//   - GET|HEAD <api host>/repos/<owner>/<name>/contents[/<path>]?ref=<sha>
//     (a file or a directory listing; `ref` is required and is the only
//     query key);
//   - GET|HEAD <api host>/repos/<owner>/<name>/git/trees/<sha>[?recursive=…]
//     (the whole tree at the pinned commit).
//
// So git over the git host (a clone sees every branch), commits, compare,
// pull requests, issues, branches, tags, search, GraphQL, the repository's
// own metadata, a contents read with no ref (the default branch) or any other
// ref, and a repository that is not pinned are all refused: none of them can
// show what came after the pinned commit.
// ---------------------------------------------------------------------------

// RepoPin is one repository a pinned caller may read and the one commit it may
// read it at.
type RepoPin struct {
	// Repo is owner/name, compared without case.
	Repo string
	// SHA is the full 40-hex commit id, compared without case.
	SHA string
}

// ReadPin is a caller's pinned-read policy (Caller.ReadPin).
type ReadPin struct {
	// Connector is the connector whose requests are pinned (`github`); a
	// request to any other connector is not this policy's.
	Connector string
	// APIHost is the host the two pinned shapes are served on (the GitHub
	// App's API host). A request to any other host — the git host above all —
	// is refused.
	APIHost string
	// Pins resolves the caller's pins for one request. An error refuses the
	// request (fail closed): no held item, an item that pins nothing, HQ
	// unreachable.
	Pins func(ctx context.Context) ([]RepoPin, error)
}

// ErrPinnedRead is a pinned-read refusal. Its text is shown to the agent and
// carries no secret.
var ErrPinnedRead = errors.New("refused by this agent's pinned-read policy")

var fullSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// ValidPinSHA reports whether s is a full 40-hex commit id.
func ValidPinSHA(s string) bool { return fullSHA.MatchString(s) }

// CheckPinnedRead decides one request of a pinned caller: nil when it is one
// of the allowed shapes at the pinned commit of a pinned repository, else an
// error wrapping ErrPinnedRead naming why and what is allowed. host is the
// request's host, rest its escaped path (no leading slash), rawQuery its
// query.
func CheckPinnedRead(method, apiHost, host, rest, rawQuery string, pins []RepoPin) error {
	// RED-FIRST SKELETON: the guard is inert in this commit, so the tests
	// that pin its refusals must fail; the next commit implements it.
	_, _ = url.PathUnescape(rest)
	_ = http.MethodGet
	_ = strings.EqualFold
	_ = fmt.Sprintf
	_ = pinnedAllowed
	return nil
}

// pinnedAllowed is the refusal's second sentence: what IS served, with the
// pins, so the agent can correct its request.
func pinnedAllowed(pins []RepoPin) string {
	if len(pins) == 0 {
		return "Nothing is served until you hold an item that pins its repositories."
	}
	list := make([]string, 0, len(pins))
	for _, p := range pins {
		list = append(list, strings.ToLower(p.Repo)+"@"+strings.ToLower(p.SHA))
	}
	sort.Strings(list)
	return "Served: GET /repos/<owner>/<name>/contents/<path>?ref=<sha> and GET /repos/<owner>/<name>/git/trees/<sha>?recursive=1, for exactly: " +
		strings.Join(list, ", ")
}
