package api

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAnOpenCodeAskIsPushedAndWithdrawnLikeAClaudeOne (REGRESSION; it uses
// nothing this change added, so it runs — and fails — against the code before
// it, where an opencode window that waits is pushed nothing): an opencode
// window that starts waiting on a question is pushed to the owner's device
// ONCE, titled "<window> is asking" with the question; settled in the
// terminal it stops waiting, and the notification is withdrawn by its tag
// ("resolved"); a permission request is pushed as a waiting session with
// the tool's name only — the same diff, title and body a Claude Code window
// gets.
func TestAnOpenCodeAskIsPushedAndWithdrawnLikeAClaudeOne(t *testing.T) {
	f := newAttnFixture(t)
	set := func(status string, p *mirror.Pending) {
		f.mirror.mu.Lock()
		f.mirror.windows = []mirror.Window{{Session: "main", ID: "@5", Index: 5, Runtime: mirror.RuntimeOpenCode, AgentSession: askOpenCodeSID,
			Status: status, StatusSince: attnSince, Pending: p, Activity: 1_700_000_000}}
		f.mirror.mu.Unlock()
	}
	nudge := func() {
		f.srv.BroadcastAgentsChangedFor(1)
		f.srv.WaitForPushes()
	}
	tag := "attention-" + sessionKey("ws", "main", "@5")
	set(mirror.StatusBusy, nil)
	nudge() // the host's first observation: nothing waiting
	if got := f.pushes(); len(got) != 0 {
		t.Fatalf("instrument check: the first observation pushed %v", got)
	}
	set(mirror.StatusWaiting, &mirror.Pending{Kind: "question", Tool: "question", Summary: "Which database should the fixture use? (and 1 more question)"})
	nudge()
	nudge()
	pastWaitingGrace(f.srv) // its push waits the grace (since #463's follow-up)
	if got := f.pushes(); len(got) != 1 || got[0] != "attention "+tag {
		t.Fatalf("the question: %v", got)
	}
	p := f.pusher.to(ep("attn-operator"))[0]
	if p.Title == sessionPushTitle || p.Title == "" || p.Title[len(p.Title)-len(" is asking"):] != " is asking" ||
		p.Body != "Which database should the fixture use? (and 1 more question)" {
		t.Fatalf("the question's push: %+v", p)
	}
	set(mirror.StatusBusy, nil) // answered in the terminal
	nudge()
	if got := f.pushes(); len(got) != 2 || got[1] != "resolved "+tag {
		t.Fatalf("the question settled: %v", got)
	}
	// A permission request in another window: a waiting session, the tool's
	// name only on the lock screen.
	f.mirror.mu.Lock()
	f.mirror.windows = append(f.mirror.windows, mirror.Window{Session: "main", ID: "@6", Index: 6, Runtime: mirror.RuntimeOpenCode,
		AgentSession: askOpenCodeSID, Status: mirror.StatusWaiting, StatusSince: attnSince,
		Pending: &mirror.Pending{Kind: "permission", Tool: "bash", Summary: "bash: rm -rf build", PromptID: "per_1a2aa6345001HPvxtQP9FD941m", Choices: "once reject"}})
	f.mirror.mu.Unlock()
	nudge()
	pastWaitingGrace(f.srv)
	got := f.pushes()
	if len(got) != 3 || got[2] != "attention attention-"+sessionKey("ws", "main", "@6") {
		t.Fatalf("the permission request: %v", got)
	}
	if p := f.pusher.to(ep("attn-operator"))[2]; p.Title != sessionPushTitle || p.Body[:len("Waiting: bash · ")] != "Waiting: bash · " ||
		len(p.Body) > 200 || strings.Contains(p.Body, "rm -rf") {
		t.Fatalf("the permission's push: %+v", p)
	}
}
