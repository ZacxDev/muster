package api

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAMirrorFocusCandidateIsActiveAtItsLastMessage: the focus pass decides
// whether a mirrored Claude window may have moved from the transcript's LAST
// MESSAGE (mirror.Window.CardActivity, the time the cards use), not tmux's
// window activity, which moves on any pane output. A window whose host reports
// no last message falls back to window activity.
//
// Invariant guard (already true on origin/main via SessionView.ActiveAt): it
// pins the signal PR "skip unchanged sessions" builds on, so a change to it
// shows up here and not as sessions silently re-read every pass.
func TestAMirrorFocusCandidateIsActiveAtItsLastMessage(t *testing.T) {
	const withMsg, noMsg = "0b6e2c1a-4d5f-4a7b-9c8d-1e2f3a4b5c6d", "7a8b9c0d-1e2f-4a3b-8c5d-6e7f8a9b0c1d"
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: []mirror.Window{
		{Session: "main", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: withMsg,
			Activity: 1_700_000_580, LastMessage: 1_699_999_980},
		{Session: "main", ID: "@2", Index: 2, Claude: true, ClaudeSession: noMsg, Activity: 1_700_000_280},
	}}
	s, _ := focusMirrorServer(t, fm, &memFocus{rows: map[focus.Subject]focus.Focus{}}, &recordingSummarizer{})
	cands, err := focusSessions{s}.Candidates(operatorCtx())
	if err != nil {
		t.Fatal(err)
	}
	got := map[focus.Subject]time.Time{}
	for _, c := range cands {
		got[c.Subject] = c.ActiveAt
	}
	if at := got[focus.MirrorSubject(withMsg)]; !at.Equal(time.Unix(1_699_999_980, 0)) {
		t.Errorf("a window with a last message is active at %v, want its last message %v", at, time.Unix(1_699_999_980, 0))
	}
	if at := got[focus.MirrorSubject(noMsg)]; !at.Equal(time.Unix(1_700_000_280, 0)) {
		t.Errorf("a window with no last message is active at %v, want its window activity %v", at, time.Unix(1_700_000_280, 0))
	}
}
