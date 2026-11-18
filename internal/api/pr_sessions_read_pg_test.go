package api

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/prroute"
)

// GET /api/pr-sessions (handlePRSessionList): the read over session_prs, through
// the real server against a real Postgres. Fixture values are invented: the
// repo and numbers are prFixture's (acme-labs/widget-api#4817), and every
// session id below is a made-up uuid, pairwise distinct.

const (
	listSessOld       = "5e55a1d0-0000-4000-8000-00000000a001"
	listSessNew       = "5e55a1d0-0000-4000-8000-00000000a002"
	listSessOtherPR   = "5e55a1d0-0000-4000-8000-00000000a003"
	listSessOtherRepo = "5e55a1d0-0000-4000-8000-00000000a004"
	listSessPush      = "5e55a1d0-0000-4000-8000-00000000a005"
	listSessOtherUser = "5e55a1d0-0000-4000-8000-00000000a006"
	listSessMachine   = "5e55a1d0-0000-4000-8000-00000000a007"
)

// list GETs the read route with query q and token tok (the owner's personal
// token when ""; no Authorization header at all when "-").
func (f *prFixture) list(t *testing.T, tok string, q url.Values) (int, string) {
	t.Helper()
	if tok == "" {
		tok = f.token
	}
	h := map[string]string{}
	if tok != "-" {
		h["Authorization"] = "Bearer " + tok
	}
	path := prSessionsPath
	if q != nil {
		path += "?" + q.Encode()
	}
	w := f.call(t, "GET", path, nil, h)
	return w.Code, w.Body.String()
}

func listQuery(repo, pr string) url.Values { return url.Values{"repo": {repo}, "pr": {pr}} }

type listedPRSessions struct {
	Repo     string         `json:"repo"`
	PR       int64          `json:"pr"`
	Sessions []prSessionRow `json:"sessions"`
}

func decodeListed(t *testing.T, body string) listedPRSessions {
	t.Helper()
	var out listedPRSessions
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("body is not the list shape: %v: %s", err, body)
	}
	return out
}

func TestPRSessionListRefusesAMissingOrWrongCredential(t *testing.T) {
	f := newPRFixture(t)
	f.authored(t, f.asOwner, listSessNew, prCreated())
	q := listQuery(prRepo, "4817")
	cases := map[string]string{
		"no credential":   "-",
		"a bad mst token": "mst_not-a-real-token-at-all-000000000000",
		"a plugin token":  f.mailTok,
		"a wrong hook":    "not-the-hook-token",
	}
	for name, tok := range cases {
		code, body := f.list(t, tok, q)
		// The fixture's hook tier is ARMED (connTestHookToken), so a missing or
		// wrong credential is a 401, not the unarmed 503.
		if code != 401 {
			t.Errorf("%s: %d, want 401", name, code)
		}
		if strings.Contains(body, listSessNew) {
			t.Errorf("%s: the refusal names the session: %s", name, body)
		}
	}
	// Positive control: the owner's own token reads the row.
	if code, body := f.list(t, "", q); code != 200 || !strings.Contains(body, listSessNew) {
		t.Fatalf("control: %d %s", code, body)
	}
}

func TestPRSessionListRefusesAMalformedQuery(t *testing.T) {
	f := newPRFixture(t)
	cases := map[string]url.Values{
		"no query":              nil,
		"no repo":               {"pr": {"4817"}},
		"no pr":                 {"repo": {prRepo}},
		"repo with no slash":    listQuery("acme-labs", "4817"),
		"repo with three parts": listQuery("acme-labs/widget-api/x", "4817"),
		"repo with a space":     listQuery("acme labs/widget-api", "4817"),
		"repo too long":         listQuery("a/"+strings.Repeat("b", 199), "4817"),
		"pr zero":               listQuery(prRepo, "0"),
		"pr negative":           listQuery(prRepo, "-4817"),
		"pr not a number":       listQuery(prRepo, "abc"),
		"pr fractional":         listQuery(prRepo, "4817.5"),
		"pr with a hash":        listQuery(prRepo, "#4817"),
	}
	for name, q := range cases {
		code, body := f.list(t, "", q)
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(body), &e)
		if code != 400 || e.Error != prSessionsQueryBadRequest {
			t.Errorf("%s: %d %s, want 400 naming the query's shape", name, code, body)
		}
	}
	// Control: the well-formed query is accepted, so every 400 above is the field's.
	if code, _ := f.list(t, "", listQuery(prRepo, "4817")); code != 200 {
		t.Fatalf("control: %d", code)
	}
}

func TestPRSessionListIsAnEmptyListWhenNoneIsRecorded(t *testing.T) {
	f := newPRFixture(t)
	code, body := f.list(t, "", listQuery(prRepo, "4817"))
	if code != 200 {
		t.Fatalf("%d %s, want 200", code, body)
	}
	// An empty JSON array, never null: a client iterates it.
	if !strings.Contains(body, `"sessions":[]`) {
		t.Fatalf("body %s; want \"sessions\":[]", body)
	}
	if got := decodeListed(t, body); got.Repo != prRepo || got.PR != prNumber || len(got.Sessions) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// TestPRSessionListAnswersOnlyThatPullRequestNewestFirst records, for the
// owner, two creates of the pull request under test plus a create of ANOTHER
// pull request of the same repo, a create of the same number in ANOTHER repo
// and a push of the same repo (no number) — and another owner's create of the
// same pull request. Only the two are answered, newest first, with every field.
func TestPRSessionListAnswersOnlyThatPullRequestNewestFirst(t *testing.T) {
	f := newPRFixture(t)
	other := f.newMember(t)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM session_prs WHERE owner_id = $1`, other)
	})
	withRef := prCreated()
	withRef.HeadRef = prBranch
	f.authored(t, f.asOwner, listSessOld, withRef)
	f.authored(t, f.asOwner, listSessNew, prCreated())
	f.authored(t, f.asOwner, listSessOtherPR, prroute.Signal{Kind: prroute.KindPRCreate, Repo: prRepo, PR: prNumber + 1})
	f.authored(t, f.asOwner, listSessOtherRepo, prroute.Signal{Kind: prroute.KindPRCreate, Repo: "acme-labs/gadget-api", PR: prNumber})
	f.authored(t, f.asOwner, listSessPush, prroute.Signal{Kind: prroute.KindPush, Repo: prRepo, HeadRef: prBranch})
	if _, err := f.plugins.RecordPRSession(f.as(other), listSessOtherUser, "tower", prCreated()); err != nil {
		t.Fatal(err)
	}
	// first_seen pinned so the order is the column's, not the insert's: the
	// row inserted FIRST is the newer one. The other rows are newer still, so a
	// filter that let one through would put it at the top.
	sys := authz.System(context.Background())
	for sess, at := range map[string]string{listSessOld: "2000-01-01T10:00:00Z", listSessNew: "2000-01-02T10:00:00Z",
		listSessOtherPR: "2000-01-03T10:00:00Z", listSessOtherRepo: "2000-01-03T10:00:00Z",
		listSessPush: "2000-01-03T10:00:00Z", listSessOtherUser: "2000-01-03T10:00:00Z"} {
		if _, err := f.pool.Exec(sys, `UPDATE session_prs SET first_seen = $2 WHERE claude_session = $1`, sess, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(sys, `UPDATE session_prs SET host = NULL WHERE claude_session = $1`, listSessNew); err != nil {
		t.Fatal(err)
	}

	// The repo as GitHub spells it, mixed case: stored lower-cased, matched so.
	code, body := f.list(t, "", listQuery("Acme-Labs/Widget-API", "4817"))
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	got := decodeListed(t, body)
	for _, s := range got.Sessions {
		switch s.ClaudeSession {
		case listSessOtherPR:
			t.Errorf("a row for ANOTHER pull request of the repo was returned: %+v", s)
		case listSessOtherRepo:
			t.Errorf("a row for the same number in ANOTHER repo was returned: %+v", s)
		case listSessPush:
			t.Errorf("a push row (no number) was returned: %+v", s)
		case listSessOtherUser:
			t.Errorf("ANOTHER owner's row was returned: %+v", s)
		}
	}
	if got.Repo != prRepo || got.PR != prNumber || len(got.Sessions) != 2 {
		t.Fatalf("got repo %q pr %d, %d sessions; want %s, %d, 2: %s", got.Repo, got.PR, len(got.Sessions), prRepo, prNumber, body)
	}
	n, o := got.Sessions[0], got.Sessions[1]
	if n.ClaudeSession != listSessNew || o.ClaudeSession != listSessOld {
		t.Fatalf("order %s, %s; want newest first (%s, %s)", n.ClaudeSession, o.ClaudeSession, listSessNew, listSessOld)
	}
	if n.Host != nil || n.HeadRef != nil || n.Kind != "gh-pr-create" || n.Repo != prRepo || n.PR != prNumber ||
		n.FirstSeen.Format("2006-01-02T15:04:05Z07:00") != "2000-01-02T10:00:00Z" {
		t.Fatalf("newest row %+v", n)
	}
	if o.Host == nil || *o.Host != "tower" || o.HeadRef == nil || *o.HeadRef != prBranch ||
		o.FirstSeen.Format("2006-01-02T15:04:05Z07:00") != "2000-01-01T10:00:00Z" || o.LastSeen.IsZero() {
		t.Fatalf("older row %+v", o)
	}
	// A null host is spelled null on the wire, not "".
	if !strings.Contains(body, `"claude_session":"`+listSessNew+`","host":null`) {
		t.Fatalf("the newest row's host is not null on the wire: %s", body)
	}
}

func TestPRSessionListIsOffWithNoAuthorshipStore(t *testing.T) {
	f := newPRFixture(t)
	f.srv.ext.PRSessions = nil
	if code, body := f.list(t, "", listQuery(prRepo, "4817")); code != 404 || !strings.Contains(body, FeatureDisabledError) {
		t.Fatalf("%d %s; want the feature_disabled 404", code, body)
	}
}

// TestPRSessionListReadsEachCredentialsOwnRows: the same pull request has a row
// of the machine user (where a report with the shared hook token lands) and a
// row of the owner (a personal token's). The hook token reads exactly the
// machine user's, the personal token exactly the owner's — neither empty, so
// "not the other's" is not passing on a read that returns nothing.
func TestPRSessionListReadsEachCredentialsOwnRows(t *testing.T) {
	f := newPRFixture(t)
	machine := machineUserID()
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM session_prs WHERE owner_id = $1 AND claude_session = $2`,
			machine, listSessMachine)
	})
	if _, err := f.plugins.RecordPRSession(f.as(machine), listSessMachine, "", prCreated()); err != nil {
		t.Fatal(err)
	}
	f.authored(t, f.asOwner, listSessNew, prCreated())
	for _, c := range []struct {
		name, tok, want, notWant string
	}{
		{"the shared hook token", connTestHookToken, listSessMachine, listSessNew},
		{"the owner's personal token", "", listSessNew, listSessMachine},
	} {
		code, body := f.list(t, c.tok, listQuery(prRepo, "4817"))
		if code != 200 {
			t.Fatalf("%s: %d %s", c.name, code, body)
		}
		got := decodeListed(t, body)
		if len(got.Sessions) != 1 || got.Sessions[0].ClaudeSession != c.want {
			t.Fatalf("%s: %+v; want exactly %s", c.name, got.Sessions, c.want)
		}
		if strings.Contains(body, c.notWant) {
			t.Fatalf("%s: the other principal's row %s is in %s", c.name, c.notWant, body)
		}
	}
}
