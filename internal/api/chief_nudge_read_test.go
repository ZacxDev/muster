package api

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// TestNudgeMirrorMessagesTimesEachPrompt: a prompt event becomes an owner-
// candidate message named by its place and timed by its own record; an
// assistant reply becomes an untimed assistant message; a time more than
// nudgeFutureSkew ahead is not believed (untimed), one within it is.
func TestNudgeMirrorMessagesTimesEachPrompt(t *testing.T) {
	now := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	got := nudgeMirrorMessages([]cctranscript.Event{
		{Kind: cctranscript.KindPrompt, Text: "keep going", Line: 579, PromptAt: now.Add(-time.Hour).UnixMilli()},
		{Kind: cctranscript.KindToolCall, Tool: "Bash"},
		{Kind: cctranscript.KindText, Text: "done?"},
		{Kind: cctranscript.KindPrompt, Text: "skewed", Line: 900, PromptAt: now.Add(4 * time.Minute).UnixMilli()},
		{Kind: cctranscript.KindPrompt, Text: "future", Line: 950, PromptAt: now.Add(6 * time.Minute).UnixMilli()},
		{Kind: cctranscript.KindPrompt, Text: "old host", Line: 990},
	}, now)
	if len(got) != 5 {
		t.Fatalf("%+v", got)
	}
	if got[0].Role != "user" || got[0].ID != "line:579" || !got[0].At.Equal(now.Add(-time.Hour)) {
		t.Fatalf("the prompt: %+v", got[0])
	}
	if got[1].Role != "assistant" || got[1].Text != "done?" || !got[1].At.IsZero() {
		t.Fatalf("the reply: %+v", got[1])
	}
	if !got[2].At.Equal(now.Add(4 * time.Minute)) {
		t.Fatalf("a time within the skew was not believed: %+v", got[2])
	}
	if !got[3].At.IsZero() || !got[4].At.IsZero() {
		t.Fatalf("a future (or missing) time was believed: %+v %+v", got[3], got[4])
	}
}

// TestNudgeMirrorCompleteVouchesOnlyForAWholeWindow: each way a mirrored read
// can fall short of the owner's last six hours.
func TestNudgeMirrorCompleteVouchesOnlyForAWholeWindow(t *testing.T) {
	now := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	p := func(line int64, ago time.Duration) cctranscript.Event {
		return cctranscript.Event{Kind: cctranscript.KindPrompt, Line: line, PromptAt: now.Add(-ago).UnixMilli()}
	}
	evs := []cctranscript.Event{p(100, 2*time.Hour), {Kind: cctranscript.KindText}, p(200, time.Hour)}
	cases := []struct {
		name string
		evs  []cctranscript.Event
		idx  mirrorIndex
		want bool
	}{
		{"complete", evs, mirrorIndex{Got: true, Lines: []int64{100, 200}}, true},
		{"no prompt index: ended early", evs, mirrorIndex{}, false},
		{"no prompt index and no prompts read", nil, mirrorIndex{}, false},
		{"a gap inside the backfill", evs, mirrorIndex{Got: true, Lines: []int64{100, 150, 200}}, false},
		{"the newest prompt missing", evs, mirrorIndex{Got: true, Lines: []int64{100, 200, 300}}, false},
		{"out of order", []cctranscript.Event{p(200, time.Hour), p(100, 2*time.Hour)}, mirrorIndex{Got: true, Lines: []int64{100, 200}}, false},
		{"cut by a cap inside six hours", evs, mirrorIndex{Got: true, Lines: []int64{50, 100, 200}}, false},
		{"cut by a cap, more dropped, inside six hours", evs, mirrorIndex{Got: true, Lines: []int64{100, 200}, More: 3}, false},
		{"cut by a cap beyond six hours", []cctranscript.Event{p(100, 7*time.Hour), p(200, time.Hour)}, mirrorIndex{Got: true, Lines: []int64{50, 100, 200}}, true},
		{"no prompts at all", nil, mirrorIndex{Got: true}, true},
		{"the index lists prompts the read has none of", nil, mirrorIndex{Got: true, Lines: []int64{100}}, false},
	}
	for _, c := range cases {
		if got := nudgeMirrorComplete(c.evs, c.idx, now); got != c.want {
			t.Errorf("%s: %t, want %t", c.name, got, c.want)
		}
	}
}
