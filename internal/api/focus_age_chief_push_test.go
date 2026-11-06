package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/ui"
)

// A FOCUS TITLE'S AGE BEYOND THE WEB UI: the chief's fleet_list and
// sessions_overview rows, and a chief notice's push, carry how old the
// session's focus title is — by THE focus age rule (ui.FocusAgeOf), the one
// the dock, the palette and the Asks list draw (#462).

// focusAgeChiefCase is one fixture age of the labelled window's focus, with
// the two fields both chief tools must carry for it. Ages overshoot the
// rule's day and four-day boundaries (2h, 50h, 5d+3h) so no step rounds onto
// one, and are distinct from each other.
type focusAgeChiefCase struct {
	name           string
	age            time.Duration
	setAt, wantAge string
}

// The fixed clock: every expected focus_set_at is a literal.
var focusAgeChiefNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

var focusAgeChiefCases = []focusAgeChiefCase{
	{"two hours (set within the day: a time, no age)", 2 * time.Hour, "2026-11-05T10:00:00Z", ""},
	{"fifty hours", 50 * time.Hour, "2026-11-03T10:00:00Z", "2d"},
	{"five days three hours", 5*24*time.Hour + 3*time.Hour, "2026-10-31T09:00:00Z", "5d"},
}

// focusAgeRow is the part of a chief tool's row these tests read.
type focusAgeRow struct {
	ID    string `json:"id"`
	Focus string `json:"focus"`
	SetAt string `json:"focus_set_at"`
	Age   string `json:"focus_age"`
}

// TestTheChiefsFleetListAndOverviewSayHowOldAFocusTitleIs: fleet_list and
// sessions_overview give each session's focus title its focus_set_at, and its
// focus_age past a day — and NO focus_stale (round 0 of #476, D1: the age says
// it, and "stale" there already means focus_problem's failed refresh), not even
// for a five-day-old title. CONTROL in every case: the other window's title, a
// constant 30 h old ("1d", a value no labelled case produces), carries its own
// age, so an age copied from the wrong row reads wrong; and a session with no
// focus row carries neither field. Red at origin/main (no field existed).
func TestTheChiefsFleetListAndOverviewSayHowOldAFocusTitleIs(t *testing.T) {
	// The fixture's windows serve no transcript; the overview's reads are not
	// under test here, so they are not waited out.
	prevT, prevB := overviewTranscriptTimeout, overviewBudget
	overviewTranscriptTimeout, overviewBudget = 100*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { overviewTranscriptTimeout, overviewBudget = prevT, prevB })
	for _, c := range focusAgeChiefCases {
		t.Run(c.name, func(t *testing.T) {
			s := whereServer(t)
			s.now = func() time.Time { return focusAgeChiefNow }
			labelled, other := focus.MirrorSubject(whereLabelledSID), focus.MirrorSubject(whereUnlabelledSID)
			s.ext.Focus = &memFocus{rows: map[focus.Subject]focus.Focus{
				labelled: {Subject: labelled, Owner: 1, Title: "Pruning the fig orchard", Source: focus.SourceSummarizer, UpdatedAt: focusAgeChiefNow.Add(-c.age)},
				other:    {Subject: other, Owner: 1, Title: "Peeling the kiwi crate", Source: focus.SourceSummarizer, UpdatedAt: focusAgeChiefNow.Add(-30 * time.Hour)},
			}}
			ctx := s.attentionPrincipal(context.Background(), 1)
			for tool, text := range map[string]string{
				"fleet_list":        s.toolFleetList(ctx),
				"sessions_overview": s.toolSessionsOverview(ctx, agents.Agent{ID: 999}, `{}`),
			} {
				var got struct {
					Sessions []focusAgeRow `json:"sessions"`
				}
				if err := json.Unmarshal([]byte(text), &got); err != nil || len(got.Sessions) != 2 {
					t.Fatalf("%s: %d rows, %v:\n%s", tool, len(got.Sessions), err, text)
				}
				by := map[string]focusAgeRow{}
				for _, r := range got.Sessions {
					by[r.ID] = r
				}
				checkFocusAgeRow(t, tool+" labelled", by[labelled.String()], "Pruning the fig orchard", c.setAt, c.wantAge)
				checkFocusAgeRow(t, tool+" other", by[other.String()], "Peeling the kiwi crate", "2026-11-04T06:00:00Z", "1d")
				if strings.Contains(text, "focus_stale") {
					t.Errorf("%s carries focus_stale:\n%s", tool, text)
				}
			}
		})
	}
	// A dispatched agent's fleet_list row (its own branch of fleet_list) carries
	// its focus title's age too.
	s := whereServer(t)
	s.now = func() time.Time { return focusAgeChiefNow }
	ag := focus.AgentSubject(7)
	s.ext.Agents = listAgents{list: []agents.Agent{{ID: 7, OwnerID: 1, Name: "ag-wren", Status: agents.StatusRunning}}}
	s.ext.Focus = &memFocus{rows: map[focus.Subject]focus.Focus{
		ag: {Subject: ag, Owner: 1, Title: "Indexing the wren archive", Source: focus.SourceSummarizer, UpdatedAt: focusAgeChiefNow.Add(-50 * time.Hour)},
	}}
	var fl struct {
		Sessions []focusAgeRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(s.toolFleetList(s.attentionPrincipal(context.Background(), 1))), &fl); err != nil || len(fl.Sessions) != 3 {
		t.Fatalf("fleet_list with an agent: %+v %v", fl.Sessions, err)
	}
	checkFocusAgeRow(t, "fleet_list agent", fl.Sessions[0], "Indexing the wren archive", "2026-11-03T10:00:00Z", "2d")

	// No focus row at all: neither field is sent.
	s = whereServer(t)
	s.now = func() time.Time { return focusAgeChiefNow }
	s.ext.Focus = &memFocus{rows: map[focus.Subject]focus.Focus{}}
	text := s.toolFleetList(s.attentionPrincipal(context.Background(), 1))
	for _, k := range []string{"focus_set_at", "focus_age", "focus_stale"} {
		if strings.Contains(text, k) {
			t.Errorf("fleet_list with no focus carries %s:\n%s", k, text)
		}
	}
}

func checkFocusAgeRow(t *testing.T, name string, r focusAgeRow, title, setAt, age string) {
	t.Helper()
	if r.Focus != title || r.SetAt != setAt || r.Age != age {
		t.Errorf("%s: focus %q set_at %q age %q, want %q %q %q", name, r.Focus, r.SetAt, r.Age, title, setAt, age)
	}
}

// noticeAgeAsk is the notice the push tests file: a chief nudge about the
// mirrored session ptOtherSID, whose decision snapshotted its focus title.
const noticeAgeAsk = 81

// TestANoticesPushSaysHowOldItsFocusTitleIs: the push sent when a chief
// notice is filed (askFiled — the real path, through the push service to the
// endpoint) leads with the session's focus title and, past a day, its age
// (" · 2d"), as the Asks list shows it. No age for a title set within the day
// (just set: an age would say nothing), nor for a notice whose session has
// moved on to a newer title (the stored time is the newer title's — 3 days
// old here, so an age copied from it would show).
//
// ONE CLOCK: the age is the SERVER's (s.now), the clock the chief's tools age
// titles by — never the host's time.Now. Each case sets s.now to the host's
// time plus clock; the last two disagree with time.Now by more than a day in
// each direction, so an age taken from the host's clock reads wrong (a title 2h
// old by the host is 3d old by the server; one 50h old by the host is 10h by
// the server, no age). Red at origin/main for the aged cases; the 2h and
// moved-on cases are controls that pin the body unchanged.
func TestANoticesPushSaysHowOldItsFocusTitleIs(t *testing.T) {
	const snapshot = "Pruning the fig orchard"
	for _, c := range []struct {
		name, stored string
		age, clock   time.Duration
		want         string
	}{
		{"fifty hours", snapshot, 50 * time.Hour, 0, "Pruning the fig orchard · 2d\nWaiting on you: merge or wait?"},
		{"five days three hours", snapshot, 5*24*time.Hour + 3*time.Hour, 0, "Pruning the fig orchard · 5d\nWaiting on you: merge or wait?"},
		{"set two hours ago", snapshot, 2 * time.Hour, 0, "Pruning the fig orchard\nWaiting on you: merge or wait?"},
		{"the session moved on", "A newer title", 3*24*time.Hour + 5*time.Hour, 0, "Pruning the fig orchard\nWaiting on you: merge or wait?"},
		{"the server's clock is three days ahead", snapshot, 2 * time.Hour, 3 * 24 * time.Hour, "Pruning the fig orchard · 3d\nWaiting on you: merge or wait?"},
		{"the server's clock is forty hours behind", snapshot, 50 * time.Hour, -40 * time.Hour, "Pruning the fig orchard\nWaiting on you: merge or wait?"},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, pusher := &memPush{}, &rawPusher{}
			claim(t, store, 1, ep("notice-age"))
			s := pushTargetServer(t, store, pusher)
			host := time.Now()
			s.now = func() time.Time { return host.Add(c.clock) }
			s.ext.ChiefNudges = boxNudges{byAsk: map[int64]chiefnudge.Decision{
				noticeAgeAsk: {Owner: 1, AskID: noticeAgeAsk, Kind: chiefnudge.KindMirror, Key: ptOtherSID, Host: "ws", Title: snapshot},
			}}
			sub := focus.MirrorSubject(ptOtherSID)
			s.ext.Focus = &memFocus{rows: map[focus.Subject]focus.Focus{
				sub: {Subject: sub, Owner: 1, Title: c.stored, Source: focus.SourceSummarizer, UpdatedAt: host.Add(-c.age)},
			}}
			s.askFiled(attention.Ask{ID: noticeAgeAsk, Owner: 1, Kind: attention.KindNotice, Asker: attention.Asker{System: true, Label: "ws · old:9"},
				Question: "Waiting on you: merge or wait?", Options: []string{}, Status: attention.Open, CreatedAt: time.Now()})
			s.WaitForPushes()
			var b push.Payload
			pusher.one(t, ep("notice-age"), &b)
			if b.Body != c.want {
				t.Fatalf("body %q, want %q", b.Body, c.want)
			}
		})
	}
}

// TestANoticesPushKeepsItsAgeWithinTheBodysBudget: a stored title far longer
// than any summarizer writes (focus.MaxTitleRunes) with an age, and a chief
// message that fills the rest: the body is within attentionPushBodyMax (the
// phone's own cut), the focus line is noticePushFocusMax runes and still ends
// in its age — the title is cut, never the age — and the message is what the
// body's cut shortens. CONTROL: the same title with no age is sent as before
// (the body's cut alone), so the bound applies only where an age needs room.
func TestANoticesPushKeepsItsAgeWithinTheBodysBudget(t *testing.T) {
	title := strings.Repeat("orchard ", focus.MaxTitleRunes/8)
	text := strings.Repeat("merge or wait? ", 20)
	it := attentionItem{Kind: attentionKindNotice, Focus: title, Text: text, pushFocusAge: ui.FocusAge{Text: "2d"}}
	body := attentionPushBody(it)
	if n := utf8.RuneCountInString(body); n > attentionPushBodyMax {
		t.Fatalf("the body is %d runes, over %d:\n%s", n, attentionPushBodyMax, body)
	}
	line, rest, _ := strings.Cut(body, "\n")
	wantLine := strings.Repeat("orchard ", 11) + "orchar…" + " · 2d"
	if line != wantLine || utf8.RuneCountInString(line) != noticePushFocusMax {
		t.Fatalf("the focus line (%d runes) is\n%q, want (%d runes)\n%q", utf8.RuneCountInString(line), line, noticePushFocusMax, wantLine)
	}
	if !strings.HasPrefix(rest, "merge or wait? merge") || !strings.HasSuffix(rest, "…") {
		t.Fatalf("the message is not what the cut shortens: %q", rest)
	}
	it.pushFocusAge = ui.FocusAge{}
	if got, want := attentionPushBody(it), push.CutClusters(pushLine(title)+"\n"+pushLine(text), attentionPushBodyMax); got != want {
		t.Fatalf("with no age the body is\n%q, want it as before\n%q", got, want)
	}
	if ui.FocusAgeSuffix(ui.FocusAge{}) != "" {
		t.Fatal("no age has a suffix")
	}
}
