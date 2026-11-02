package cctranscript

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The records below have the SHAPES measured from Claude Code 2.1.289 (a busy
// session in a private tmux server sent further messages; see queue.go):
// every key the CLI wrote, in its order, with the content, ids and times
// replaced.

const qSession = "00000000-0000-4000-8000-000000000000"

func qOp(op, content string, extra ...string) string {
	m := map[string]any{"type": "queue-operation", "operation": op, "timestamp": "2000-01-01T00:00:00.000Z", "sessionId": qSession}
	if content != "" || op != QueueDequeue {
		m["content"] = content
	}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i]] = extra[i+1]
	}
	b, _ := json.Marshal(m)
	return string(b) + "\n"
}

// qPrompt is the user record a dequeued message is written as (promptSource
// "queued", origin human).
func qPrompt(text string) string {
	b, _ := json.Marshal(map[string]any{"parentUuid": "p", "isSidechain": false, "promptId": "q", "type": "user",
		"message": map[string]any{"role": "user", "content": text}, "uuid": "u", "timestamp": "2000-01-01T00:00:01.000Z",
		"origin": map[string]any{"kind": "human"}, "promptSource": "queued"})
	return string(b) + "\n"
}

// qAbsorbed is the attachment a message absorbed mid-turn is written as.
func qAbsorbed(text string) string {
	att := map[string]any{"type": "queued_command", "prompt": text, "source_uuid": "s", "commandMode": "prompt"}
	b, _ := json.Marshal(map[string]any{"parentUuid": "p", "isSidechain": false, "type": "attachment",
		"attachment": att, "uuid": "u", "timestamp": "2000-01-01T00:00:01.000Z"})
	return string(b) + "\n"
}

// qNotice is a task notification delivered as its own user record.
func qNotice(body string) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "promptId": "q", "message": map[string]any{"role": "user", "content": body},
		"origin": map[string]any{"kind": "task-notification"}, "timestamp": "2000-01-01T00:00:01.000Z"})
	return string(b) + "\n"
}

const qTask = "<task-notification>\n<task-id>t1</task-id>\n<status>completed</status>\n<summary>Background command finished</summary>\n</task-notification>"

// fold reads jsonl with ReadLines (the live tail) and folds it, step by step:
// each element of steps is appended, then the queue's list is recorded.
func fold(t *testing.T, steps ...string) [][]string {
	t.Helper()
	var q Queue
	var file []byte
	var off int64
	var out [][]string
	for _, s := range steps {
		file = append(file, s...)
		lines, next, err := ReadLines(bytes.NewReader(file), off, int64(len(file)), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		off = next
		q.Observe(lines)
		out = append(out, q.List())
	}
	return out
}

func TestTheQueueFollowsClaudeCodesRecords(t *testing.T) {
	cases := []struct {
		name  string
		steps []string
		want  [][]string
	}{{
		// Sent while Claude was busy, taken when its turn ended.
		name:  "enqueue then dequeue",
		steps: []string{qOp(QueueEnqueue, "then say ok."), qOp(QueueDequeue, "") + qPrompt("then say ok.")},
		want:  [][]string{{"then say ok."}, nil},
	}, {
		// Two sent, both absorbed into the running turn at a tool boundary.
		name: "absorbed mid-turn",
		steps: []string{qOp(QueueEnqueue, "also mention a cat."), qOp(QueueEnqueue, "and a dog."),
			qOp(QueueRemove, "also mention a cat.", "reason", "absorbed_mid_turn", "commandUuid", "c1", "deliveryId", "d1"),
			qOp(QueueRemove, "and a dog.", "reason", "absorbed_mid_turn", "commandUuid", "c2", "deliveryId", "d2") +
				qAbsorbed("also mention a cat.") + qAbsorbed("and a dog.")},
		want: [][]string{{"also mention a cat."}, {"also mention a cat.", "and a dog."}, {"and a dog."}, nil},
	}, {
		// Up pulled the queue back into the input box.
		name:  "popAll",
		steps: []string{qOp(QueueEnqueue, "one"), qOp(QueueEnqueue, "two"), qOp(QueuePopAll, "one\ntwo")},
		want:  [][]string{{"one"}, {"one", "two"}, nil},
	}, {
		// The harness's own enqueues are tracked but never listed.
		name:  "a task notification is not the person's",
		steps: []string{qOp(QueueEnqueue, qTask), qOp(QueueDequeue, "") + qNotice(qTask)},
		want:  [][]string{nil, nil},
	}, {
		// NOT FIFO: a notification queued AFTER the person's message is taken
		// first. The record that follows the dequeue says which left.
		name: "a dequeue takes the one its record names",
		steps: []string{qOp(QueueEnqueue, "my message"), qOp(QueueEnqueue, qTask),
			qOp(QueueDequeue, "") + qNotice(qTask), qOp(QueueDequeue, "") + qPrompt("my message")},
		want: [][]string{{"my message"}, {"my message"}, {"my message"}, nil},
	}, {
		// The person's message is taken while an older envelope stays.
		name: "the person's message leaves past an older envelope",
		steps: []string{qOp(QueueEnqueue, qTask), qOp(QueueEnqueue, "mine"),
			qOp(QueueDequeue, "") + qPrompt("mine")},
		want: [][]string{nil, {"mine"}, nil},
	}, {
		// A dequeue's record and the dequeue split across two reads.
		name:  "the record after the dequeue arrives on a later poll",
		steps: []string{qOp(QueueEnqueue, "a"), qOp(QueueEnqueue, "b"), qOp(QueueDequeue, ""), qPrompt("a")},
		want:  [][]string{{"a"}, {"a", "b"}, {"a", "b"}, {"b"}},
	}, {
		// Another session's message is delivered inside the harness's sentence.
		name: "a message delivered inside other text",
		steps: []string{qOp(QueueEnqueue, "<agent-message from=\"x\">hi</agent-message>"), qOp(QueueEnqueue, "plain"),
			qOp(QueueDequeue, "") + qPrompt("Another session sent a message:\n<agent-message from=\"x\">hi</agent-message>")},
		want: [][]string{nil, {"plain"}, {"plain"}},
	}, {
		// A dequeue whose record matches nothing still takes one: the oldest
		// of the person's (a slash command is drawn as "/name args").
		name:  "an unmatched prompt takes the oldest of the person's",
		steps: []string{qOp(QueueEnqueue, qTask), qOp(QueueEnqueue, "x"), qOp(QueueEnqueue, "y"), qOp(QueueDequeue, "") + qPrompt("something else")},
		want:  [][]string{nil, {"x"}, {"x", "y"}, {"y"}},
	}, {
		// A remove of something never seen (it was queued before the window) is
		// nothing; a sidechain's record is not the session's queue.
		name: "unknown removes and sidechains change nothing",
		steps: []string{qOp(QueueRemove, "never queued", "reason", "absorbed_mid_turn"),
			strings.Replace(qOp(QueueEnqueue, "sub"), `"type"`, `"isSidechain":true,"type"`, 1), qOp(QueueEnqueue, "real")},
		want: [][]string{nil, nil, {"real"}},
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fold(t, c.steps...)
			if len(got) != len(c.want) {
				t.Fatalf("got %d steps, want %d", len(got), len(c.want))
			}
			for i := range got {
				if !slices.Equal(got[i], c.want[i]) {
					t.Fatalf("after step %d: %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// OBSERVE SAYS WHETHER THE LIST MOVED: an envelope's enqueue does not move it.
func TestObserveReportsAChangeOnlyWhenTheListMoved(t *testing.T) {
	var q Queue
	read := func(s string) []Line {
		lines, _, err := ReadLines(strings.NewReader(s), 0, int64(len(s)), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return lines
	}
	if !q.Observe(read(qOp(QueueEnqueue, "m"))) {
		t.Fatal("an enqueue of the person's message did not change the list")
	}
	if q.Observe(read(qOp(QueueEnqueue, qTask))) {
		t.Fatal("an envelope's enqueue changed the list")
	}
	if q.Observe(read(qOp(QueueDequeue, ""))) {
		t.Fatal("a dequeue before its record changed the list")
	}
	if !q.Observe(read(qPrompt("m"))) {
		t.Fatal("the dequeued message's record did not change the list")
	}
}

// THE READERS CARRY A QUEUE OPERATION AS A LINE WITHOUT EVENTS: it costs none
// of a backfill's events, Flatten never sees it, and Drawn drops it.
func TestQueueOperationsAreLinesWithoutEvents(t *testing.T) {
	file := qOp(QueueEnqueue, "waiting") + qPrompt("p1") + qOp(QueueDequeue, "") + qPrompt("waiting")
	lines, _, err := ReadLines(strings.NewReader(file), 0, int64(len(file)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, l := range lines {
		if l.Queue != nil {
			ops = append(ops, l.Queue.Op+":"+l.Queue.Content)
			if len(l.Events) != 0 {
				t.Fatalf("a queue line has events: %+v", l.Events)
			}
		}
	}
	if !slices.Equal(ops, []string{"enqueue:waiting", "dequeue:"}) {
		t.Fatalf("queue lines %q", ops)
	}
	if len(Flatten(lines)) != 2 || len(Drawn(lines)) != 2 {
		t.Fatalf("Flatten %d / Drawn %d, want the 2 prompts", len(Flatten(lines)), len(Drawn(lines)))
	}
	// Backfill, capped at ONE event, still returns the queue lines after it.
	back, _, err := Backfill(strings.NewReader(file), int64(len(file)), 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	var q Queue
	q.Observe(back)
	if n := len(Flatten(back)); n != 1 {
		t.Fatalf("backfill drew %d events, want 1", n)
	}
	if got := q.List(); got != nil {
		t.Fatalf("after the backfill the list is %q, want empty (the message was taken)", got)
	}
	// A queue operation's line is never on the wire.
	b, _ := json.Marshal(lines)
	if bytes.Contains(b, []byte(QueueEnqueue)) || bytes.Contains(b, []byte(QueueDequeue)) {
		t.Fatalf("a queue operation reached the JSON: %s", b)
	}
}

func TestIsQueueEnvelope(t *testing.T) {
	for s, want := range map[string]bool{
		qTask: true, "<agent-message from=\"a\">x</agent-message>": true, "  <x>": true,
		"plain": false, "< not a tag": false, "<3 you": false, "": false, "a <b>": false,
	} {
		if IsQueueEnvelope(s) != want {
			t.Errorf("IsQueueEnvelope(%q) = %v, want %v", s, !want, want)
		}
	}
}
