package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/prroute"
)

// 🔴 A LIVE SESSION IS FOUND BY THE BRANCH ITS WINDOW HAS CHECKED OUT, as its
// host reports it (mirror.TypeBranches) — before it ever pushed, so with no
// session_prs row at all. Invented names only.

const (
	sidBranchOn   = "0b2c3d4e-br-on"
	sidBranchMain = "0b2c3d4e-br-main"
	sidBranchWord = "0b2c3d4e-br-word"
	sidBranchGone = "0b2c3d4e-br-gone"
	sidBranchOld  = "0b2c3d4e-br-old"
	branchHintRef = "feat/x-1"
)

// branchReplay: on an online host, a window that never pushed and sits on
// "feat/x-1" in a repo the message does not name, one on "main", one on
// "follow-up"; on an OFFLINE host a window on "feat/x-1" too; and on an older
// online host (no branches message at all) one more window. pr's fixture
// (prHintReplay) is under it, so its author and pusher rows exist.
func branchReplay(t *testing.T) *gatedFixture {
	g := prHintReplay(t)
	now := time.Now().Unix()
	h3 := overviewHost("box-3", true,
		mirror.Window{Session: "dev", ID: "@20", Index: 1, Claude: true, ClaudeSession: sidBranchOn, Label: "Rhea:1", Activity: now - 3600},
		mirror.Window{Session: "dev", ID: "@21", Index: 2, Claude: true, ClaudeSession: sidBranchMain, Label: "Leda:2", Activity: now},
		mirror.Window{Session: "dev", ID: "@22", Index: 3, Claude: true, ClaudeSession: sidBranchWord, Label: "Nyx:3", Activity: now})
	h3.Repos = map[string]string{"@20": "local-clone", "@21": "local-clone", "@22": "local-clone"}
	h3.Branches = map[string]string{"@20": branchHintRef, "@21": "main", "@22": "follow-up"}
	h4 := overviewHost("box-4", false,
		mirror.Window{Session: "dev", ID: "@30", Index: 1, Claude: true, ClaudeSession: sidBranchGone, Label: "Eos:1", Activity: now})
	h4.Branches = map[string]string{"@30": branchHintRef}
	h5 := overviewHost("box-5", true,
		mirror.Window{Session: "dev", ID: "@40", Index: 1, Claude: true, ClaudeSession: sidBranchOld, Label: "Io:1", Activity: now})
	m := g.srv.ext.Mirror.(scopedMirror)
	m.hosts = append(m.hosts, h3, h4, h5)
	g.srv.ext.Mirror = m
	return g
}

func TestASessionIsHintedByTheBranchItSitsOnBeforeItPushed(t *testing.T) {
	g := branchReplay(t)
	h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 21, Subject: "CI", Body: "CI failed on feat/x-1 after the rebase."})
	if ids := hintIDs(h); fmt.Sprint(ids) != "[mirror:"+sidBranchOn+"]" {
		t.Fatalf("hint lists %v; want exactly mirror:%s", ids, sidBranchOn)
	}
	if s := h.Sessions[0]; s.Branch != branchHintRef || len(s.PRs) != 0 || s.LastActive.IsZero() {
		t.Fatalf("listed by branch %q, PRs %v, last active %v; want %s, none, a time", s.Branch, s.PRs, s.LastActive, branchHintRef)
	}
	// Every whole-token spelling names it.
	for _, text := range []string{"refs/heads/feat/x-1 moved", "https://github.com/acme/widget-api/tree/feat/x-1",
		"compare main...feat/x-1", "Rebased feat/x-1."} {
		if ids := hintIDs(g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 22, Subject: "x", Body: text})); fmt.Sprint(ids) != "[mirror:"+sidBranchOn+"]" {
			t.Errorf("%q: hint lists %v", text, ids)
		}
	}
	// No store needed: without pull-request routing it is found all the same.
	g.srv.ext.PRSessions = nil
	if ids := hintIDs(g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 23, Subject: "x", Body: "feat/x-1"})); fmt.Sprint(ids) != "[mirror:"+sidBranchOn+"]" {
		t.Fatalf("without PRSessions: hint lists %v", ids)
	}
	// Nor focus: the hint without a focus store still lists it.
	g.srv.ext.Focus = nil
	if ids := hintIDs(g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 23, Subject: "x", Body: "feat/x-1"})); fmt.Sprint(ids) != "[mirror:"+sidBranchOn+"]" {
		t.Fatalf("without Focus: hint lists %v", ids)
	}
}

// 🔴 A generic branch name never names a session, though windows sit on it;
// nor does a longer or prefixed token; nor another owner's view.
func TestAGenericOrPartialBranchNamesNoSession(t *testing.T) {
	g := branchReplay(t)
	for _, text := range []string{"merged to main today", "a follow-up on this", "feat/x-10 is another branch",
		"origin/feat/x-1 is a remote ref", "feat/x-1-v2", "feat/x"} {
		if h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 24, Subject: "news", Body: text}); len(h.Sessions) != 0 {
			t.Errorf("%q named %v", text, hintIDs(h))
		}
	}
	other := g.newMember(t)
	if h := g.srv.pluginSessionHints(g.as(other), g.chief, plugins.Message{ID: 25, Subject: "x", Body: "feat/x-1"}); len(h.Sessions) != 0 {
		t.Errorf("another owner was hinted %v", hintIDs(h))
	}
}

// 🔴 An offline host's window on the branch is never listed (its entries carry
// no branch), and an older host that sends no branches is found by its push
// row exactly as before, with no branch line.
func TestAnOfflineOrOlderHostIsUnaffected(t *testing.T) {
	g := branchReplay(t)
	h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 26, Subject: "x", Body: "feat/x-1"})
	for _, id := range hintIDs(h) {
		if id == "mirror:"+sidBranchGone {
			t.Fatalf("the offline window was hinted: %v", hintIDs(h))
		}
	}
	g.record(t, g.asOwner, sidBranchOld, prroute.Signal{Kind: prroute.KindPush, Repo: prHintRepo, HeadRef: "feat/old-host-7"})
	h = g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 27, Subject: "x", Body: "feat/old-host-7 is red"})
	if ids := hintIDs(h); fmt.Sprint(ids) != "[mirror:"+sidBranchOld+"]" || h.Sessions[0].Branch != "" ||
		fmt.Sprint(h.Sessions[0].PRs) != "[acme/infra-config branch feat/old-host-7]" {
		t.Fatalf("the older host's pusher: %v %+v", ids, h.Sessions)
	}
}

// A session found BOTH by a push row and by its checkout is listed once, at
// the push row's place, and says the branch once (ListedBranch).
func TestABranchFoundByPushAndCheckoutIsListedOnce(t *testing.T) {
	g := branchReplay(t)
	g.record(t, g.asOwner, sidBranchOn, prroute.Signal{Kind: prroute.KindPush, Repo: prHintRepo, HeadRef: branchHintRef})
	h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 28, Subject: "x", Body: "feat/x-1"})
	if ids := hintIDs(h); fmt.Sprint(ids) != "[mirror:"+sidBranchOn+"]" {
		t.Fatalf("hint lists %v; want the session once", ids)
	}
	s := h.Sessions[0]
	if fmt.Sprint(s.PRs) != "[acme/infra-config branch feat/x-1]" || s.Branch != branchHintRef || s.ListedBranch() != "" {
		t.Fatalf("PRs %v, Branch %q, ListedBranch %q; want the push row, the branch, and no second branch line", s.PRs, s.Branch, s.ListedBranch())
	}
}

// 🔴 At the hint's seam: a checkout hit sits with the pull-request hits —
// after a push/create hit, before every focus hit — so MaxHintSessions focus
// hits cannot push it out.
func TestABranchHitIsHintedWithPullRequestHitsBeforeFocusHits(t *testing.T) {
	g := branchReplay(t)
	for _, sid := range []string{sidPROther, sidBranchMain, sidBranchWord} {
		g.setFocus(t, focus.MirrorSubject(sid), "Stage C one-shot Job test for infra-config", "")
	}
	h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 29, Subject: prHintSubject, Body: prHintBody + " Also feat/x-1."})
	ids := hintIDs(h)
	if len(ids) != plugins.MaxHintSessions || ids[0] != "mirror:"+sidPRAuthor || ids[1] != "mirror:"+sidBranchOn {
		t.Fatalf("hint lists %v; want the pull-request hit, then the branch hit, then a focus hit", ids)
	}
}

func TestFocusSearchFindsASessionByItsCheckedOutBranch(t *testing.T) {
	g := branchReplay(t)
	for surface, text := range g.surfaces(t, g.chief, g.chiefTok, "focus_search", `{"query":"feat/x-1"}`) {
		if isToolErr(text) || !strings.Contains(text, `"results":[{"id":"mirror:`+sidBranchOn+`","kind":"mirror","matched":"pull request or branch","pull_requests":[],"branch":"feat/x-1"`) {
			t.Errorf("%s: the session on the branch was not found first: %s", surface, text)
		}
		if strings.Contains(text, sidBranchGone) || strings.Contains(text, sidBranchMain) {
			t.Errorf("%s: another session was found: %s", surface, text)
		}
	}
	for surface, text := range g.surfaces(t, g.chief, g.chiefTok, "focus_search", `{"query":"main"}`) {
		if strings.Contains(text, sidBranchMain) {
			t.Errorf("%s: \"main\" found the session on main: %s", surface, text)
		}
	}
}

// matchBranchHints: only an online conversation's FIRST window (the one
// route_to_session takes), only a branch among the candidates, at most limit.
func TestMatchBranchHintsTakesEachConversationsFirstWindow(t *testing.T) {
	entries := []mirror.SessionView{
		{Host: "a", Online: true, Claude: true, ClaudeSession: "s1", Branch: "feat/q-1"},
		{Host: "b", Online: true, Claude: true, ClaudeSession: "s1", Branch: "feat/x-1"}, // s1's second window
		{Host: "a", Online: false, Claude: true, ClaudeSession: "s2", Branch: "feat/x-1"},
		{Host: "a", Online: true, Claude: true, ClaudeSession: "s3", Branch: "feat/x-1"},
		{Host: "a", Online: true, Claude: true, ClaudeSession: "s4", Branch: "feat/x-1"},
		{Host: "a", Online: true, Claude: true, ClaudeSession: "s5", Branch: "feat/x-1"},
	}
	got := matchBranchHints([]string{"feat/x-1"}, entries, 2)
	var ids []string
	for _, h := range got {
		ids = append(ids, h.ID+"="+h.Branch)
	}
	if fmt.Sprint(ids) != "[mirror:s3=feat/x-1 mirror:s4=feat/x-1]" {
		t.Fatalf("matchBranchHints = %v", ids)
	}
}
