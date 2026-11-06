package api

import (
	"io"
	"log"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// REGRESSION TESTS for "an opencode question or permission request is in the
// attention queue and notifies like a Claude Code one". They use nothing this
// change added, so they run — and fail — against the code before it: there a
// waiting opencode window is no item at all, and nothing is pushed.

const askOpenCodeSID = "ses_e5d55c179ffe6MEUqkUCZbHWA9"

// openCodeWaiting is fakeMirror's host "ws" with opencode windows in session
// "main": @1 waits on a question (two of them, so it names no options), @2 on
// a permission request, @3 is busy.
func openCodeWaiting() []mirror.Window {
	w := func(id, status string, p *mirror.Pending) mirror.Window {
		return mirror.Window{Session: "main", ID: id, Runtime: mirror.RuntimeOpenCode, AgentSession: askOpenCodeSID,
			Status: status, StatusSince: 1792000002222, Pending: p}
	}
	return []mirror.Window{
		w("@1", mirror.StatusWaiting, &mirror.Pending{Kind: "question", Tool: "question",
			Summary: "Which database should the fixture use? (and 1 more question)"}),
		w("@2", mirror.StatusWaiting, &mirror.Pending{Kind: "permission", Tool: "bash", Summary: "bash: rm -rf build",
			PromptID: "per_1a2aa6345001HPvxtQP9FD941m", Choices: "once reject"}),
		w("@3", mirror.StatusBusy, nil),
	}
}

// TestAWaitingOpenCodeWindowIsAnAttentionItemNotifiedLikeClaudes: a question
// is a "question" item pushed as "<window> is asking" with the question; a
// permission request a "session" item pushed as "A session is waiting on
// you", "Waiting: <tool> · <window>" — the tool's name only, never the
// command — exactly the shapes a Claude Code window's question and permission
// prompt get. A busy window is no item.
func TestAWaitingOpenCodeWindowIsAnAttentionItemNotifiedLikeClaudes(t *testing.T) {
	m := &fakeMirror{online: true, interactive: true, statusReport: true, windows: openCodeWaiting()}
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Mirror: m})
	by := itemsByWindow(s.waitingSessions(attentionUser()))
	if len(by) != 2 {
		t.Fatalf("%d items, want the two waiting opencode windows: %+v", len(by), by)
	}
	q := by["@1"]
	if q.Kind != attentionKindQuestion || q.Text != "Which database should the fixture use? (and 1 more question)" || q.host != "ws" {
		t.Fatalf("the question item: %+v", q)
	}
	if q.claudeSession != "opencode:"+askOpenCodeSID || q.subject.Key != "opencode:"+askOpenCodeSID {
		t.Errorf("the question's session key: %q %+v", q.claudeSession, q.subject)
	}
	n := attentionNotification(q, false)
	if n.Title != q.Source+" is asking" || n.Body != "Which database should the fixture use? (and 1 more question)" || n.Tag != "attention-"+q.Key {
		t.Fatalf("the question's notification: %+v", n)
	}
	p := by["@2"]
	if p.Kind != attentionKindSession || p.Text != "bash: rm -rf build" || p.tool != "bash" {
		t.Fatalf("the permission item: %+v", p)
	}
	n = attentionNotification(p, false)
	if n.Title != sessionPushTitle || n.Body != "Waiting: bash · "+p.Source || n.Tag != "attention-"+p.Key {
		t.Fatalf("the permission's notification: %+v", n)
	}
}
