package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/prroute"
)

// 🔴 A LIVE SESSION IS FOUND BY THE PULL REQUEST IT CREATED OR PUSHED — the
// hint and focus_search, on both transports, against a real Postgres. The
// shape of the miss: a message titled "<owner>/<repo>#N: …", whose session
// works in a local clone named differently from the remote (so no repo match)
// and whose focus never spells the number (so no focus match); muster had
// recorded that session creating the pull request (session_prs). Invented
// names only: owner "acme", remote "infra-config", clone "cfg-local".

const (
	sidPRAuthor = "0b2c3d4e-pr-author"
	sidPRPusher = "0b2c3d4e-pr-pusher"
	sidPROther  = "0b2c3d4e-pr-other"
	sidPRGone   = "0b2c3d4e-pr-gone"
	prHintRepo  = "acme/infra-config"
	prHintN     = 2855
	prHintRef   = "feat/stage-c-job"
)

const prHintSubject = "acme/infra-config#2855: Stage C test and a completed one-shot Job"
const prHintBody = "The Stage C test ran; the one-shot Job completed. Follow-up on #2855 before the next stage."

// prHintReplay is the miss: the author's window, idle, in a clone named
// "cfg-local", focused on something that never spells the number; an
// unrelated window in "cache-quorum"; and, on an OFFLINE host, a window whose
// session also recorded the pull request.
func prHintReplay(t *testing.T) *gatedFixture {
	g := newGatedFixture(t)
	g.srv.ext.PRSessions = g.plugins
	t.Cleanup(func() {
		_, _ = g.pool.Exec(authz.System(context.Background()), `DELETE FROM session_prs WHERE claude_session LIKE '0b2c3d4e-pr-%'`)
	})
	idle := time.Now().Add(-72 * time.Hour).Unix()
	h1 := overviewHost("box-1", true,
		mirror.Window{Session: "work", ID: "@7", Index: 3, Active: true, Claude: true, ClaudeSession: sidPRAuthor, Label: "Mars:3", Activity: idle},
		mirror.Window{Session: "work", ID: "@8", Index: 4, Claude: true, ClaudeSession: sidPROther, Label: "Iris:4", Activity: time.Now().Unix()})
	h1.Repos = map[string]string{"@7": "cfg-local", "@8": "cache-quorum"}
	h2 := overviewHost("box-2", false,
		mirror.Window{Session: "work", ID: "@9", Index: 1, Claude: true, ClaudeSession: sidPRGone, Label: "Vega:1", Activity: time.Now().Unix()})
	g.setFocus(t, focus.MirrorSubject(sidPRAuthor), "Rolling the job controller to staging", "")
	g.setFocus(t, focus.MirrorSubject(sidPROther), "Reading the cache dashboards", "")
	g.srv.ext.Mirror = scopedMirror{overviewMirror{hosts: []mirror.HostView{h1, h2}}, g.owner}
	g.record(t, g.asOwner, sidPRAuthor, prroute.Signal{Kind: prroute.KindPRCreate, Repo: prHintRepo, PR: prHintN, HeadRef: prHintRef})
	g.record(t, g.asOwner, sidPRGone, prroute.Signal{Kind: prroute.KindPRCreate, Repo: prHintRepo, PR: prHintN})
	return g
}

func (g *gatedFixture) record(t *testing.T, ctx context.Context, sess string, sig prroute.Signal) {
	t.Helper()
	if _, err := g.plugins.RecordPRSession(ctx, sess, "", sig); err != nil {
		t.Fatal(err)
	}
}

func hintIDs(h plugins.Hints) []string {
	var ids []string
	for _, s := range h.Sessions {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestASessionIsHintedByThePullRequestItCreated(t *testing.T) {
	g := prHintReplay(t)
	h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 1, Subject: prHintSubject, Body: prHintBody})
	if ids := hintIDs(h); fmt.Sprint(ids) != "[mirror:"+sidPRAuthor+"]" {
		t.Fatalf("hint lists %v; want exactly mirror:%s", ids, sidPRAuthor)
	}
	if got := h.Sessions[0].PRs; fmt.Sprint(got) != "[acme/infra-config#2855]" || h.Sessions[0].LastActive.IsZero() {
		t.Fatalf("listed by %v (last active %v); want [acme/infra-config#2855] with a time", got, h.Sessions[0].LastActive)
	}
	// Every qualified spelling names it; the URL and the repo alone too.
	for _, text := range []string{"infra-config#2855 is green", "https://github.com/acme/infra-config/pull/2855",
		"ACME/Infra-Config#2855 merged"} {
		if ids := hintIDs(g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 2, Subject: "x", Body: text})); fmt.Sprint(ids) != "[mirror:"+sidPRAuthor+"]" {
			t.Errorf("%q: hint lists %v", text, ids)
		}
	}
}

// 🔴 A bare "#2855", another owner's or repo's number, and a generic branch
// name nothing.
func TestABareNumberOrAnotherRepoNamesNoSession(t *testing.T) {
	g := prHintReplay(t)
	for _, text := range []string{
		"Follow-up on #2855 before the next stage.",
		"PR #2855 and pull request 2855",
		"other/infra-config#2855 is someone else's",
		"acme/widget-api#2855 is another repo's",
		"acme/infra-config#2856 is another number",
	} {
		if h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 3, Subject: "news", Body: text}); len(h.Sessions) != 0 {
			t.Errorf("%q named %v", text, hintIDs(h))
		}
	}
}

// 🔴 Only the owner's own record: another owner's row for the same pull
// request, for a session id an online window of THIS owner shows, names
// nothing — and that owner sees no window at all.
func TestAnotherOwnersPullRequestNamesNoSession(t *testing.T) {
	g := prHintReplay(t)
	other := g.newMember(t)
	t.Cleanup(func() {
		_, _ = g.pool.Exec(authz.System(context.Background()), `DELETE FROM session_prs WHERE owner_id = $1`, other)
	})
	g.record(t, g.as(other), sidPROther, prroute.Signal{Kind: prroute.KindPRCreate, Repo: prHintRepo, PR: prHintN})
	h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 4, Subject: prHintSubject, Body: prHintBody})
	if ids := hintIDs(h); fmt.Sprint(ids) != "[mirror:"+sidPRAuthor+"]" {
		t.Fatalf("hint lists %v; want only the owner's own author", ids)
	}
	if h := g.srv.pluginSessionHints(g.as(other), g.chief, plugins.Message{ID: 5, Subject: prHintSubject, Body: prHintBody}); len(h.Sessions) != 0 {
		t.Errorf("the other owner was hinted %v", hintIDs(h))
	}
}

// 🔴 A session no ONLINE window shows is never listed, even with a newer row.
func TestAnOfflineSessionIsNotHintedByItsPullRequest(t *testing.T) {
	g := prHintReplay(t)
	g.record(t, g.asOwner, sidPRGone, prroute.Signal{Kind: prroute.KindPRCreate, Repo: prHintRepo, PR: prHintN}) // now the newest
	h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 6, Subject: prHintSubject, Body: prHintBody})
	for _, id := range hintIDs(h) {
		if id == "mirror:"+sidPRGone {
			t.Fatalf("the offline session was hinted: %v", hintIDs(h))
		}
	}
	if len(h.Sessions) != 1 {
		t.Fatalf("hint lists %v; want the online author", hintIDs(h))
	}
}

// A row past PRSessionRetention names nothing, even before a report prunes it.
func TestAStalePullRequestRowNamesNoSession(t *testing.T) {
	g := prHintReplay(t)
	if _, err := g.pool.Exec(authz.System(context.Background()), `UPDATE session_prs SET last_seen = now() - interval '31 days'
		WHERE claude_session = $1`, sidPRAuthor); err != nil {
		t.Fatal(err)
	}
	if h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 11, Subject: prHintSubject, Body: prHintBody}); len(h.Sessions) != 0 {
		t.Fatalf("a stale row named %v", hintIDs(h))
	}
}

// matchPRHints lists only a session entries holds, and at most limit.
func TestMatchPRHintsListsOnlyShownSessionsWithinTheLimit(t *testing.T) {
	entries := map[string]mirror.SessionView{"s1": {Host: "box-1"}, "s2": {Host: "box-1"}, "s3": {Host: "box-1"}}
	var rows []plugins.PRSession
	for _, s := range []string{"ghost", "s1", "s2", "s3"} {
		rows = append(rows, plugins.PRSession{ClaudeSession: s, Repo: prHintRepo, PR: prHintN})
	}
	got := matchPRHints(prHintSubject, rows, entries, 2)
	var ids []string
	for _, h := range got {
		ids = append(ids, h.ID)
	}
	if fmt.Sprint(ids) != "[mirror:s1 mirror:s2]" {
		t.Fatalf("matchPRHints = %v; want [mirror:s1 mirror:s2]", ids)
	}
}

// A session that PUSHED a branch is named by that branch (a push row names
// no pull request number); a generic branch it pushed ("main") names nothing.
func TestAPusherIsHintedByItsBranch(t *testing.T) {
	g := prHintReplay(t)
	h1 := g.srv.ext.Mirror.(scopedMirror)
	w := mirror.Window{Session: "work", ID: "@10", Index: 5, Claude: true, ClaudeSession: sidPRPusher, Label: "Juno:5", Activity: time.Now().Unix()}
	host := overviewHost("box-3", true, w)
	h1.hosts = append(h1.hosts, host)
	g.srv.ext.Mirror = h1
	g.record(t, g.asOwner, sidPRPusher, prroute.Signal{Kind: prroute.KindPush, Repo: prHintRepo, HeadRef: prHintRef})
	g.record(t, g.asOwner, sidPRPusher, prroute.Signal{Kind: prroute.KindPush, Repo: "acme/widget-api", HeadRef: "main"})
	h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 8, Subject: "x", Body: "CI is red on feat/stage-c-job."})
	if ids := hintIDs(h); fmt.Sprint(ids) != "[mirror:"+sidPRPusher+" mirror:"+sidPRAuthor+"]" ||
		fmt.Sprint(h.Sessions[0].PRs) != "[acme/infra-config branch feat/stage-c-job]" {
		t.Fatalf("by branch: %v %+v", ids, h.Sessions)
	}
	if h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 9, Subject: "x", Body: "merged to main today"}); len(h.Sessions) != 0 {
		t.Fatalf("a generic branch named %v", hintIDs(h))
	}
}

// At the hint's seam: a pull-request hit is listed before a focus hit — and
// so is not pushed out by MaxHintSessions focus hits.
func TestAPullRequestHitIsHintedBeforeFocusHits(t *testing.T) {
	g := prHintReplay(t)
	g.setFocus(t, focus.MirrorSubject(sidPROther), "Stage C one-shot Job test for infra-config", "")
	h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 10, Subject: prHintSubject, Body: prHintBody})
	if ids := hintIDs(h); fmt.Sprint(ids) != "[mirror:"+sidPRAuthor+" mirror:"+sidPROther+"]" {
		t.Fatalf("hint lists %v; want the pull-request hit, then the focus hit", ids)
	}
}

// mergeHints: earlier lists first; a session in several keeps its first place
// and gains every line it was listed by.
func TestMergeHintsKeepsEveryLineAtTheFirstPlace(t *testing.T) {
	pr := plugins.SessionHint{ID: "a", PRs: []string{"acme/x#1"}}
	fa := plugins.SessionHint{ID: "a", Titles: []plugins.HintTitle{{Title: "t"}}}
	fb := plugins.SessionHint{ID: "b", Titles: []plugins.HintTitle{{Title: "u"}}}
	ra := plugins.SessionHint{ID: "a", Repo: "x", ByRepo: true}
	got := mergeHints(3, []plugins.SessionHint{pr}, []plugins.SessionHint{fb, fa}, []plugins.SessionHint{ra})
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" || len(got[0].PRs) != 1 || len(got[0].Titles) != 1 || !got[0].ByRepo {
		t.Fatalf("mergeHints = %+v", got)
	}
}

func TestFocusSearchFindsASessionByItsPullRequest(t *testing.T) {
	g := prHintReplay(t)
	for _, args := range []string{`{"query":"infra-config#2855"}`, `{"query":"acme/infra-config#2855","since_hours":24}`,
		`{"query":"github.com/acme/infra-config/pull/2855"}`, `{"query":"feat/stage-c-job"}`} {
		for surface, text := range g.surfaces(t, g.chief, g.chiefTok, "focus_search", args) {
			if isToolErr(text) || !strings.Contains(text, `"results":[{"id":"mirror:`+sidPRAuthor+`","kind":"mirror","title":"Rolling the job controller to staging","matched":"pull request or branch","pull_requests":[`) {
				t.Errorf("%s %s: the author was not found first: %s", surface, args, text)
			}
			if strings.Contains(text, sidPROther) || strings.Contains(text, sidPRGone) {
				t.Errorf("%s %s: another session was found: %s", surface, args, text)
			}
		}
	}
	// Control: a bare number finds nothing.
	for surface, text := range g.surfaces(t, g.chief, g.chiefTok, "focus_search", `{"query":"#2855"}`) {
		if strings.Contains(text, sidPRAuthor) {
			t.Errorf("%s: #2855 found the author: %s", surface, text)
		}
	}
}

// focus_search lists a pull-request hit before a focus hit, as the hint does.
func TestFocusSearchListsAPullRequestHitBeforeAFocusHit(t *testing.T) {
	g := prHintReplay(t)
	g.setFocus(t, focus.MirrorSubject(sidPROther), "Reviewing acme/infra-config#2855 for the rollout", "")
	for surface, text := range g.surfaces(t, g.chief, g.chiefTok, "focus_search", `{"query":"acme/infra-config#2855"}`) {
		a, o := strings.Index(text, `"id":"mirror:`+sidPRAuthor+`"`), strings.Index(text, `"id":"mirror:`+sidPROther+`"`)
		if a < 0 || o < 0 || a > o {
			t.Errorf("%s: want the pull-request hit, then the focus hit: %s", surface, text)
		}
	}
}
