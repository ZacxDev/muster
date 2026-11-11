package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
)

// TestClaimReleasedByPinnedCases is THE release rule's table
// (task_claims.go claimReleasedBy): which accepted status writes release a
// task's session claims.
func TestClaimReleasedByPinnedCases(t *testing.T) {
	for _, c := range []struct {
		name   string
		writer statusWriter
		status string
		queued bool
		want   bool
	}{
		{"the owner's card sets open", writerHumanUI, notes.StatusOpen, false, true},
		{"the owner's card sets open on a reference (queued)", writerHumanUI, notes.StatusOpen, true, true},
		{"the machine route sets open", writerMachineAPI, notes.StatusOpen, false, true},
		{"the task tool sets open", writerTaskTool, notes.StatusOpen, false, true},
		{"an agent's route sets open", writerAgentRoute, notes.StatusOpen, false, true},
		{"an agent's tool sets open", writerAgentTool, notes.StatusOpen, true, true},
		{"the upstream reopens (a stored snapshot)", writerPluginReference, notes.StatusOpen, false, true},
		{"the dispatch advance is no unclaim", writerDispatchAdvance, notes.StatusOpen, false, false},
		{"the reopen after a destroy is no unclaim", writerDestroyReopen, notes.StatusOpen, false, false},
		{"a merge is no unclaim", writerTaskMerge, notes.StatusOpen, false, false},
		{"complete by anyone, stored", writerDispatchAdvance, notes.StatusComplete, false, true},
		{"complete by the upstream", writerPluginReference, notes.StatusComplete, false, true},
		{"complete only queued is not done yet", writerHumanUI, notes.StatusComplete, true, false},
		{"in progress releases nothing", writerHumanUI, notes.StatusInProgress, false, false},
		{"ready for review releases nothing", writerMachineAPI, notes.StatusReadyForReview, false, false},
	} {
		if got := claimReleasedBy(c.writer, c.status, c.queued); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSessionEndedPinnedCases is THE "the claiming session ended" rule's
// table (task_claims.go sessionEnded): gone only from the host it claimed
// from, while that host is online, reports status and lists windows.
func TestSessionEndedPinnedCases(t *testing.T) {
	const sid, other = "5e55a1d0-0000-4000-8000-0000000e0d01", "5e55a1d0-0000-4000-8000-0000000e0d02"
	host := func(name string, online, report bool, ws ...mirror.Window) mirror.HostView {
		return mirror.HostView{Host: name, Online: online, StatusReport: report, Windows: ws}
	}
	runs := func(s string) mirror.Window { return mirror.Window{ID: "@1", Claude: true, ClaudeSession: s} }
	nested := mirror.Window{ID: "@2", Claude: true, ClaudeSession: other, OtherClaude: []string{sid}}
	for _, c := range []struct {
		name      string
		hs        []mirror.HostView
		host, sid string
		want      bool
	}{
		{"its window runs it", []mirror.HostView{host("bench", true, true, runs(sid))}, "bench", sid, false},
		{"another pane of a window runs it", []mirror.HostView{host("bench", true, true, nested)}, "bench", sid, false},
		{"its window is gone from a reporting host", []mirror.HostView{host("bench", true, true, runs(other))}, "bench", sid, true},
		{"the host is offline", []mirror.HostView{host("bench", false, true, runs(other))}, "bench", sid, false},
		{"the host reports no status", []mirror.HostView{host("bench", true, false, runs(other))}, "bench", sid, false},
		{"the host lists no windows (unread)", []mirror.HostView{host("bench", true, true)}, "bench", sid, false},
		{"another host is the one reporting", []mirror.HostView{host("lathe", true, true, runs(other))}, "bench", sid, false},
		{"the claim named no host", []mirror.HostView{host("bench", true, true, runs(other))}, "", sid, false},
		{"no session", []mirror.HostView{host("bench", true, true, runs(other))}, "bench", "", false},
	} {
		if got := sessionEnded(c.hs, c.host, c.sid); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
