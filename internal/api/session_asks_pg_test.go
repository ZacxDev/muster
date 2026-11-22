package api

import (
	"html"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
)

// A SESSION'S ASKS IN ITS OPEN PANEL (session_asks.go), through the real
// routes, the real asks store and the real session views: the Open button
// counts the session's open asks — drawn with pages off and no viewer plugin
// — and its Asks tab lists the session's asks, open first, each linking to
// its own page. Every name, id and host is invented.

var listedAsk = regexp.MustCompile(`data-session-ask="(\d+)"`)

// listedAsks is the ask ids an Asks tab answer lists, in order.
func listedAsks(body string) []int64 {
	var out []int64
	for _, m := range listedAsk.FindAllStringSubmatch(body, -1) {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		out = append(out, n)
	}
	return out
}

// sessionAskOf files an open ask of owner's, raised by asker.
func sessionAskOf(t *testing.T, f *pageFixture, owner int64, asker attention.Asker, q string) attention.Ask {
	t.Helper()
	a, err := f.srv.ext.Attention.Create(f.as(owner, authz.ViaSession), attention.New{Asker: asker, Question: q})
	if err != nil {
		t.Fatalf("ask %q: %v", q, err)
	}
	return a
}

func windowAsker(sid string) attention.Asker {
	return attention.Asker{MirrorHost: "workstation", MirrorSession: sid, Label: "main:3 @ workstation"}
}

// TestTheOpenButtonCountsAWindowsOpenAsksWithNoPagesAndNoPlugin: with pages
// off and no viewer plugin, a window whose session has asks draws the Open
// button, counting the OPEN ones (red before: no slot, no button, a count of
// 0); its answered one is named, not counted; another window's asks and
// another owner's from the same host and session are not its; a window with
// none draws no button. The Terminal view draws the slot and the panel, the
// panel with its Asks tab.
func TestTheOpenButtonCountsAWindowsOpenAsksWithNoPagesAndNoPlugin(t *testing.T) {
	f, m := newSessionPageFixture(t)
	f.srv.ext.Pages = nil
	ctx := f.as(f.owner, authz.ViaSession)
	sessionAskOf(t, f, f.owner, windowAsker(makerSID), "the first open one")
	sessionAskOf(t, f, f.owner, windowAsker(makerSID), "the second open one")
	done := sessionAskOf(t, f, f.owner, windowAsker(makerSID), "answered")
	if _, err := f.srv.ext.Attention.Answer(ctx, done.ID, "yes"); err != nil {
		t.Fatal(err)
	}
	sessionAskOf(t, f, f.owner, windowAsker(otherSID), "the other window's")
	m.set(f.other, sessionWindow("@3", 3, makerSID))
	sessionAskOf(t, f, f.other, windowAsker(makerSID), "another owner's")
	sessionAskOf(t, f, f.other, windowAsker(makerSID), "another owner's too")
	m.set(f.owner, sessionWindow("@3", 3, makerSID), sessionWindow("@4", 4, otherSID), sessionWindow("@5", 5, "0d1e2f3a-aaaa-4bbb-8ccc-0000000000cc"))

	button := func(win string) string {
		t.Helper()
		w := f.get(t, ui.PreviewButtonPath+"?"+windowQuery(win), f.ownerHdr())
		if w.Code != http.StatusOK {
			t.Fatalf("the button route for %s: %d %s", win, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	b := button("@3")
	for _, want := range []string{`id="preview-toggle"`, `data-preview-count="2"`, `data-open-asks="2"`, `data-open-asks-listed="3"`, `data-open-pages="0"`,
		`aria-label="Open: 2 open asks, 1 answered ask"`} {
		if !strings.Contains(b, want) {
			t.Errorf("@3's button lacks %s:\n%s", want, b)
		}
	}
	if b := button("@4"); !strings.Contains(b, `data-preview-count="1"`) || !strings.Contains(b, `aria-label="Open: 1 open ask"`) {
		t.Errorf("@4's button: %s", b)
	}
	if b := button("@5"); b != "" {
		t.Errorf("@5 asked nothing, its button route answered %q", b)
	}
	w := f.get(t, "/mirror/terminal?"+windowQuery("@3"), f.ownerHdr())
	if w.Code != http.StatusOK {
		t.Fatalf("the Terminal view: %d", w.Code)
	}
	doc := w.Body.String()
	for _, want := range []string{`id="preview-slot"`, `id="preview-toggle"`, `data-preview-count="2"`, `id="preview-panel"`,
		`data-asks-src="` + ui.SessionAsksPath + `?`, `data-tab="asks"`} {
		if !strings.Contains(doc, want) {
			t.Errorf("the Terminal view lacks %s", want)
		}
	}
}

// TestTheAsksTabListsTheWindowsOwnAsksOpenFirst: the Asks tab lists the
// window's session's asks — open ones newest first, then the answered one with
// its answer as text — each linking to its own ask page; none of another
// window's or another owner's from the same host and session; an unlisted
// window lists none.
func TestTheAsksTabListsTheWindowsOwnAsksOpenFirst(t *testing.T) {
	f, m := newSessionPageFixture(t)
	ctx := f.as(f.owner, authz.ViaSession)
	done := sessionAskOf(t, f, f.owner, windowAsker(makerSID), "Which branch?")
	if _, err := f.srv.ext.Attention.Answer(ctx, done.ID, `the <b>main</b> one`); err != nil {
		t.Fatal(err)
	}
	first := sessionAskOf(t, f, f.owner, windowAsker(makerSID), "Ship it?")
	second := sessionAskOf(t, f, f.owner, windowAsker(makerSID), "Tag it?")
	sessionAskOf(t, f, f.owner, windowAsker(otherSID), "Other window's")
	m.set(f.other, sessionWindow("@3", 3, makerSID))
	sessionAskOf(t, f, f.other, windowAsker(makerSID), "Another owner's")

	w := f.get(t, ui.SessionAsksPath+"?"+windowQuery("@3"), f.ownerHdr())
	if w.Code != http.StatusOK {
		t.Fatalf("the Asks tab: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if got, want := listedAsks(body), []int64{second.ID, first.ID, done.ID}; !slices.Equal(got, want) {
		t.Fatalf("the Asks tab lists %v, want %v (open newest first, then answered)", got, want)
	}
	for _, a := range []attention.Ask{second, first, done} {
		if want := `href="` + askPagePath(a.ID) + `" hx-boost="false" data-session-ask="` + strconv.FormatInt(a.ID, 10) + `"`; !strings.Contains(body, want) {
			t.Errorf("ask %d is not linked to its page (%s):\n%s", a.ID, want, body)
		}
	}
	if !strings.Contains(body, html.EscapeString(`the <b>main</b> one`)) || strings.Contains(body, "<b>main") {
		t.Errorf("the answer is not drawn as escaped text:\n%s", body)
	}
	for _, not := range []string{"Other window", "Another owner"} {
		if strings.Contains(body, not) {
			t.Errorf("the Asks tab lists %q", not)
		}
	}
	if b := f.get(t, ui.SessionAsksPath+"?host=workstation&session=main&window=%4099", f.ownerHdr()).Body.String(); !strings.Contains(b, ui.SessionAsksNone) || len(listedAsks(b)) != 0 {
		t.Errorf("an unlisted window's Asks tab = %q", b)
	}
}

// TestAnAgentSessionListsItsAgentsAsks: an agent's Open button counts its open
// asks and its Asks tab lists them; another agent's are not its; another
// owner's request for the agent is a 404 (row-level security: a missing one).
func TestAnAgentSessionListsItsAgentsAsks(t *testing.T) {
	f, _ := newSessionPageFixture(t)
	f.srv.ext.Pages = nil
	mine := sessionAskOf(t, f, f.owner, attention.Asker{AgentID: f.agent.ID, Label: f.agent.Name}, "May I rerun it?")
	// The other agent has TWO open asks: a count of 2 is its, never this
	// agent's 1.
	sessionAskOf(t, f, f.owner, attention.Asker{AgentID: f.agent2.ID, Label: f.agent2.Name}, "The other agent's")
	sessionAskOf(t, f, f.owner, attention.Asker{AgentID: f.agent2.ID, Label: f.agent2.Name}, "The other agent's second")
	sessionAskOf(t, f, f.owner, windowAsker(makerSID), "A window's")
	w := f.get(t, ui.AgentOpenButtonPath(f.agent.Name), f.ownerHdr())
	if b := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(b, `data-preview-count="1"`) || !strings.Contains(b, `aria-label="Open: 1 open ask"`) {
		t.Fatalf("the agent's button: %d %s", w.Code, b)
	}
	w = f.get(t, ui.AgentSessionAsksPath(f.agent.Name), f.ownerHdr())
	if got := listedAsks(w.Body.String()); w.Code != http.StatusOK || !slices.Equal(got, []int64{mine.ID}) {
		t.Fatalf("the agent's Asks tab: %d %v, want [%d]", w.Code, got, mine.ID)
	}
	f.session = f.other
	for _, p := range []string{ui.AgentOpenButtonPath(f.agent.Name), ui.AgentSessionAsksPath(f.agent.Name)} {
		if w := f.get(t, p, f.ownerHdr()); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "data-session-ask=") {
			t.Errorf("another owner's GET %s = %d %q, want 404", p, w.Code, w.Body.String())
		}
	}
	f.session = f.owner
}
