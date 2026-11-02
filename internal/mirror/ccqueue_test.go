package mirror_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

func queueLine(op, content string) string {
	m := map[string]any{"type": "queue-operation", "operation": op, "timestamp": "2000-01-01T00:00:00.000Z", "sessionId": "s"}
	if op != cctranscript.QueueDequeue {
		m["content"] = content
	}
	b, _ := json.Marshal(m)
	return string(b) + "\n"
}

// lastOffset is the furthest offset rec's messages named.
func lastOffset(rec *watchRec) int64 {
	var at int64
	for _, m := range rec.all() {
		at = max(at, m.Offset)
	}
	return at
}

// latestQueued is the last queued list rec got, and whether it got any.
func latestQueued(rec *watchRec) ([]string, bool) {
	var l []string
	got := false
	for _, m := range rec.all() {
		if m.QueuedList {
			l, got = m.Queued, true
		}
	}
	return l, got
}

// END TO END through a real host, tunnel and hub: a message Claude Code
// queued is in the view's queued list — redacted — from the moment its
// enqueue is written until Claude takes it, and the list is sent on every
// start: a resume while one is queued is told it, and folds it once.
func TestClaudeCodesQueuedMessagesCrossTheWire(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	tr := newTranscript(t, promptLine("first"), textLine("working on it"))
	b := newClaudeBackend("main")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%3", PID: 42, Transcript: tr.path, Gen: "gen-q"})
	r.connect(t, "ws", tok, b, true)
	r.online(t, 1, "ws", "main")
	eventually(t, "main is a Claude pane", func() bool { h := r.svc.Hosts(asUser(1)); return len(h) == 1 && h[0].Rendered })

	rec, _, _ := watchAs(t, asUser(1), r.svc, "ws", "main", "", 0)
	// The start says the list, empty: a page that drew one before is cleared.
	eventually(t, "the start's empty list", func() bool { l, ok := latestQueued(rec); return ok && len(l) == 0 })
	before := lastOffset(rec)

	// Sent while Claude is busy: queued, with a secret in it.
	tr.write(queueLine(cctranscript.QueueEnqueue, "then deploy with TOKEN=ghp_QueuedSecret0001"))
	eventually(t, "the queued message", func() bool { l, _ := latestQueued(rec); return len(l) == 1 })
	l, _ := latestQueued(rec)
	if strings.Contains(l[0], "ghp_QueuedSecret0001") || !strings.HasPrefix(l[0], "then deploy with") {
		t.Fatalf("queued %q: want it redacted, its words kept", l[0])
	}

	// A RESUME AT THE END, with nothing new to read (a reconnect to an idle
	// session), is told the list at once — not only when a line next comes.
	rec2, _, _ := watchAs(t, asUser(1), r.svc, "ws", "main", "gen-q", lastOffset(rec))
	eventually(t, "the resume's list", func() bool { l, ok := latestQueued(rec2); return ok && len(l) == 1 })
	if all := rec2.all(); all[0].Reset {
		t.Fatalf("the resume started over (%+v): this test needs a resume", all[0])
	}
	// A resume from BEFORE the enqueue reads it once: the window it folds
	// stops at its offset, and the poll reads the rest.
	rec3, _, _ := watchAs(t, asUser(1), r.svc, "ws", "main", "gen-q", before)
	eventually(t, "the earlier resume's list", func() bool { l, ok := latestQueued(rec3); return ok && len(l) == 1 })
	time.Sleep(300 * time.Millisecond) // many polls
	for _, m := range rec3.all() {
		if m.Reset {
			t.Fatalf("the earlier resume started over: %+v", m)
		}
		if len(m.Queued) > 1 {
			t.Fatalf("the earlier resume listed %q: the enqueue was folded twice", m.Queued)
		}
	}

	// A harness envelope queued beside it is not listed.
	tr.write(queueLine(cctranscript.QueueEnqueue, "<task-notification><summary>done</summary></task-notification>"))

	// Claude takes it: its prompt is drawn, then it leaves the list.
	tr.write(queueLine(cctranscript.QueueDequeue, "") + promptLine("then deploy with TOKEN=ghp_QueuedSecret0001"))
	eventually(t, "the taken message leaves", func() bool { l, ok := latestQueued(rec); return ok && len(l) == 0 })
	eventually(t, "and on the resumed view", func() bool { l, ok := latestQueued(rec2); return ok && len(l) == 0 })
	// The prompt came BEFORE the emptied list on the stream: the message is
	// never off the page between the two.
	promptAt, emptiedAt, queuedSeen := -1, -1, false
	for i, m := range rec.all() {
		if promptAt < 0 && slices.ContainsFunc(m.Events, func(e cctranscript.Event) bool {
			return e.Kind == cctranscript.KindPrompt && strings.HasPrefix(e.Text, "then deploy")
		}) {
			promptAt = i
		}
		if m.QueuedList && len(m.Queued) > 0 {
			queuedSeen = true
		}
		if emptiedAt < 0 && queuedSeen && m.QueuedList && len(m.Queued) == 0 {
			emptiedAt = i
		}
	}
	if promptAt < 0 || emptiedAt < 0 || promptAt > emptiedAt {
		t.Fatalf("the prompt at message %d, the emptied list at %d: want the prompt first", promptAt, emptiedAt)
	}
	// No queue record ever reached the events.
	for _, m := range rec.all() {
		for _, e := range m.Events {
			if strings.Contains(e.Text, "task-notification") {
				t.Fatalf("a queue record leaked into the events: %+v", e)
			}
		}
	}
}

func TestNormalizeQueued(t *testing.T) {
	long := strings.Repeat("x", mirror.MaxQueuedText+50)
	in := []string{"  ", "a\x00b\nc", long}
	for range mirror.MaxQueued + 5 {
		in = append(in, "more")
	}
	got := mirror.NormalizeQueued(in)
	if len(got) != mirror.MaxQueued {
		t.Fatalf("%d messages, want %d", len(got), mirror.MaxQueued)
	}
	if got[0] != "a b c" {
		t.Fatalf("cleaned %q, want %q", got[0], "a b c")
	}
	if n := len([]rune(got[1])); n > mirror.MaxQueuedText {
		t.Fatalf("a message of %d runes, want at most %d", n, mirror.MaxQueuedText)
	}
	if mirror.NormalizeQueued(nil) != nil {
		t.Fatal("an empty list is not nil")
	}
}
