package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE FOCUS AGE, EVERYWHERE THE CHIEF OR THE OWNER IS SHOWN A FOCUS TITLE, ON
// ONE CLOCK (round 1 of #476): the attention summary's why, attention_list,
// focus_search, the chief nudge's prompt, the Asks list, the switcher and the
// sidebar.
//
// 🔴 EVERY FIXTURE HERE SKEWS THE SERVER'S CLOCK FROM THE HOST'S: s.now is
// three days AHEAD of time.Now, and the focus was written two hours before
// time.Now. By the server's clock the title is 3 days old ("3d"); by the
// host's it is 2 hours old (no age). So an age worked out on time.Now instead
// of s.now reads as no age, and an age copied from anywhere but this title
// cannot be "3d".

const (
	r1Title  = "Pruning the fig orchard"
	r1Notice = 82
)

// r1Skew sets s's clock 3 days ahead of the host and gives the mirrored
// session ptOtherSID a focus titled title, written 2 hours before the host's
// now. It returns the focus's write time.
func r1Skew(s *Server, title string) time.Time {
	host := time.Now()
	s.now = func() time.Time { return host.Add(3 * 24 * time.Hour) }
	sub := focus.MirrorSubject(ptOtherSID)
	at := host.Add(-2 * time.Hour)
	s.ext.Focus = &memFocus{rows: map[focus.Subject]focus.Focus{
		sub: {Subject: sub, Owner: 1, Title: title, Source: focus.SourceSummarizer, UpdatedAt: at},
	}}
	return at
}

// r1NoticeServer is pushTargetServer with one open chief notice about
// ptOtherSID whose decision snapshotted r1Title, and the skewed focus.
func r1NoticeServer(t *testing.T) (*Server, time.Time) {
	t.Helper()
	s := pushTargetServer(t, nil, nil)
	s.ext.ChiefNudges = boxNudges{byAsk: map[int64]chiefnudge.Decision{
		r1Notice: {Owner: 1, AskID: r1Notice, Kind: chiefnudge.KindMirror, Key: ptOtherSID, Host: "ws", Title: r1Title},
	}}
	// whereAnswered: the Asks list also reads the recent answers (none here).
	s.ext.Attention = whereAnswered{openAsks: openAsks{asks: []attention.Ask{{ID: r1Notice, Owner: 1, Kind: attention.KindNotice,
		Asker: attention.Asker{System: true, Label: "ws · old:9"}, Question: "Waiting on you: merge or wait?",
		Options: []string{}, Status: attention.Open, CreatedAt: time.Now()}}}}
	return s, r1Skew(s, r1Title)
}

// TestTheSummarysWhyIsThePushsFirstLineAgeIncluded (F1): GET
// /api/attention/summary's why for a notice is its push's first line —
// docs/attention.md: "the first line of the notification body" — so it
// carries the title's age, at the server's clock, as the push does.
func TestTheSummarysWhyIsThePushsFirstLineAgeIncluded(t *testing.T) {
	s, _ := r1NoticeServer(t)
	ctx := s.attentionPrincipal(context.Background(), 1)
	rec := httptest.NewRecorder()
	s.handleAttentionSummary(rec, httptest.NewRequest("GET", "/api/attention/summary", nil).WithContext(ctx))
	var sum struct {
		Items []struct {
			Key string `json:"key"`
			Why string `json:"why"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil || len(sum.Items) == 0 {
		t.Fatalf("summary %v:\n%s", err, rec.Body.String())
	}
	var why string
	for _, it := range sum.Items {
		if it.Key == attentionAskKey(r1Notice) {
			why = it.Why
		}
	}
	if want := r1Title + " · 3d"; why != want {
		t.Errorf("the notice's why %q, want %q", why, want)
	}
	// The contract itself: why is the push body's first line, as built for
	// the push (askPushItem).
	push := attentionPushBody(s.askPushItem(ctx, r1OpenNotice(t, s)))
	if first, _, _ := strings.Cut(push, "\n"); why != first {
		t.Errorf("why %q is not the push's first line %q", why, first)
	}
}

// r1OpenNotice is the server's one open notice (r1NoticeServer's).
func r1OpenNotice(t *testing.T, s *Server) attention.Ask {
	t.Helper()
	open, err := s.ext.Attention.Open(context.Background())
	if err != nil || len(open) != 1 {
		t.Fatalf("fixture: %d open asks, %v", len(open), err)
	}
	return open[0]
}

// TestAttentionListGivesTheChiefAFocusTitlesAge (F2): attention_list's item
// carries focus_age (and focus_set_at) beside its focus title, as fleet_list
// does.
func TestAttentionListGivesTheChiefAFocusTitlesAge(t *testing.T) {
	s, at := r1NoticeServer(t)
	var got struct {
		Items []struct {
			Key   string `json:"key"`
			Focus string `json:"focus"`
			SetAt string `json:"focus_set_at"`
			Age   string `json:"focus_age"`
		} `json:"items"`
	}
	text := s.toolAttentionList(s.attentionPrincipal(context.Background(), 1))
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range got.Items {
		if it.Key != attentionAskKey(r1Notice) {
			continue
		}
		found = true
		if it.Focus != r1Title || it.Age != "3d" || it.SetAt != at.UTC().Format(time.RFC3339) {
			t.Errorf("the notice: focus %q age %q set_at %q, want %q \"3d\" %q", it.Focus, it.Age, it.SetAt, r1Title, at.UTC().Format(time.RFC3339))
		}
	}
	if !found {
		t.Fatalf("no notice in attention_list:\n%s", text)
	}
}

// r1SearchFocus is memFocus whose current-focus search answers hits.
type r1SearchFocus struct {
	*memFocus
	hits []focus.Hit
}

func (r r1SearchFocus) Search(context.Context, string, []string, int) ([]focus.Hit, error) {
	return r.hits, nil
}

// TestFocusSearchGivesEachHitsTitleItsAge (F2): focus_search (current focus)
// gives a hit its title's focus_age and focus_set_at. CONTROL: a hit whose
// title the stored focus no longer has (a refresh moved it on) carries no age
// — the stored time is the newer title's.
func TestFocusSearchGivesEachHitsTitleItsAge(t *testing.T) {
	s := pushTargetServer(t, nil, nil)
	at := r1Skew(s, r1Title)
	mf := s.ext.Focus.(*memFocus)
	other := focus.MirrorSubject(focusSID)
	mf.rows[other] = focus.Focus{Subject: other, Owner: 1, Title: "A newer title", Source: focus.SourceSummarizer, UpdatedAt: at}
	s.ext.Focus = r1SearchFocus{memFocus: mf, hits: []focus.Hit{
		{Subject: focus.MirrorSubject(ptOtherSID), Title: r1Title, Snippet: "fig"},
		{Subject: other, Title: "Grafting the fig cuttings", Snippet: "fig"},
	}}
	var got struct {
		Results []struct {
			ID, Title string
			SetAt     string `json:"focus_set_at"`
			Age       string `json:"focus_age"`
		} `json:"results"`
	}
	text := s.toolFocusSearch(s.attentionPrincipal(context.Background(), 1), `{"query":"fig"}`)
	if err := json.Unmarshal([]byte(text), &got); err != nil || len(got.Results) != 2 {
		t.Fatalf("focus_search %v:\n%s", err, text)
	}
	if r := got.Results[0]; r.Age != "3d" || r.SetAt != at.UTC().Format(time.RFC3339) {
		t.Errorf("hit %s: age %q set_at %q, want \"3d\" %q", r.ID, r.Age, r.SetAt, at.UTC().Format(time.RFC3339))
	}
	if r := got.Results[1]; r.Age != "" || r.SetAt != "" {
		t.Errorf("a hit whose title moved on carries age %q set_at %q", r.Age, r.SetAt)
	}
}

// TestTheNudgePromptSaysHowOldTheFocusTitleIs (F2): the chief's stall check
// is given its session's focus title and, past a day, how old it is in words
// (ui.FocusAgeLabel, at the server's clock), on the line after it. CONTROL: a
// title the server's clock calls fresh (the host's 2h, the server not
// skewed) has no age line, and the prompt is as before.
func TestTheNudgePromptSaysHowOldTheFocusTitleIs(t *testing.T) {
	s := pushTargetServer(t, nil, nil)
	r1Skew(s, r1Title)
	c := chiefnudge.Session{Kind: chiefnudge.KindMirror, Key: ptOtherSID}
	title, age := s.nudgeTitle(context.Background(), c)
	if title != r1Title || age != "focus title set 3d ago" {
		t.Fatalf("nudgeTitle = %q, %q; want %q, \"focus title set 3d ago\"", title, age, r1Title)
	}
	c.Title, c.TitleAge = title, age
	last := chiefnudge.Msg{ID: "m1", Role: "assistant", Text: "done"}
	p := chiefnudge.Prompt(c, last, nil, time.Hour, chiefnudge.Hint{}, "0123456789abcdef")
	if !strings.Contains(p, "\n| focus title: "+r1Title+"\n| focus title set 3d ago\n") {
		t.Errorf("the prompt lacks the title's age line:\n%s", p)
	}
	s.now = time.Now
	title, age = s.nudgeTitle(context.Background(), c)
	if title != r1Title || age != "" {
		t.Fatalf("a fresh title: nudgeTitle = %q, %q; want %q, \"\"", title, age, r1Title)
	}
	c.TitleAge = age
	if p := chiefnudge.Prompt(c, last, nil, time.Hour, chiefnudge.Hint{}, "0123456789abcdef"); !strings.Contains(p, "\n| focus title: "+r1Title+"\n") || strings.Contains(p, "focus title set") {
		t.Errorf("a fresh title's prompt is not as before:\n%s", p)
	}
}

// TestTheAsksListAgesAFocusTitleOnTheServersClock (F4): the Asks list (GET
// /attention/items, the list the page polls) draws a notice's focus age at
// s.now, the clock its push and the chief are told it at.
func TestTheAsksListAgesAFocusTitleOnTheServersClock(t *testing.T) {
	s, _ := r1NoticeServer(t)
	rec := httptest.NewRecorder()
	s.handleAttentionItems(rec, httptest.NewRequest("GET", "/attention/items", nil).WithContext(s.attentionPrincipal(context.Background(), 1)))
	want := `<span data-attention-focus>` + r1Title + `</span><span class="font-normal" data-focus-age title="focus title set 3d ago" aria-label="focus title set 3d ago"> · 3d</span>`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("the Asks list lacks\n%s\nin:\n%s", want, rec.Body.String())
	}
	// The whole page (GET /attention) draws the same list on the same clock.
	rec = httptest.NewRecorder()
	s.handleAttentionPage(rec, httptest.NewRequest("GET", attentionPath, nil).WithContext(s.attentionPrincipal(context.Background(), 1)))
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("the Asks page lacks\n%s\nin:\n%s", want, rec.Body.String())
	}
}

// TestTheSwitcherAndSidebarAgeAFocusTitleOnTheServersClock (F4): the switcher
// (the dock and the palette draw from it) and the sidebar's rows carry an
// agent's focus age at s.now.
func TestTheSwitcherAndSidebarAgeAFocusTitleOnTheServersClock(t *testing.T) {
	s, _ := pinServer(t, nil, nil)
	host := time.Now()
	s.now = func() time.Time { return host.Add(3 * 24 * time.Hour) }
	sub := focus.AgentSubject(6)
	s.ext.Focus = &memFocus{rows: map[focus.Subject]focus.Focus{
		sub: {Subject: sub, Owner: 1, Title: "Indexing the wren archive", Source: focus.SourceSummarizer, UpdatedAt: host.Add(-2 * time.Hour)},
	}}
	sw := s.switcher(operatorCtx(), ui.SwitcherCurrent{})
	if sw == nil {
		t.Fatal("instrument check FAILED: no switcher was built")
	}
	found := false
	for _, it := range sw.All {
		if it.Href == "/agents/status-moved" {
			found = true
			if it.FocusAge.Text != "3d" {
				t.Errorf("the switcher's pill: focus age %+v, want 3d", it.FocusAge)
			}
		}
	}
	if !found {
		t.Fatalf("agent 6 is not in the switcher: %+v", sw.All)
	}
	rec := httptest.NewRecorder()
	s.handleNavAgents(rec, httptest.NewRequest("GET", ui.NavAgentsPath, nil).WithContext(operatorCtx()))
	if !strings.Contains(rec.Body.String(), `data-switcher-focus-age="3d"`) {
		t.Errorf("the sidebar lacks data-switcher-focus-age=\"3d\":\n%s", rec.Body.String())
	}
}
