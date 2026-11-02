package cctranscript

import (
	"encoding/json"
	"strings"
)

// CLAUDE CODE'S OWN INPUT QUEUE: a message sent while Claude is busy (typed in
// the terminal, or pasted by muster) is held by the CLI — drawn above its input
// box with "Press up to edit queued messages" — until it takes it. The
// transcript records every change to that queue as a queue-operation record,
// so the rendered view can show what is waiting without reading the screen.
//
// The shapes relied on, MEASURED against CLI 2.1.289 in a private tmux server
// (a busy session sent further messages) and across a month of real
// transcripts (37,706 enqueues):
//
//   - {"type":"queue-operation","operation":"enqueue","content":"<text>"} —
//     a message joined the queue, as typed;
//   - {"operation":"dequeue"} — the CLI took ONE message to start a turn with.
//     It names none: the user record written just after it (promptSource
//     "queued") is the message. NOT strictly FIFO across kinds — a task
//     notification can be taken before an older message — so which one left
//     is read from that record (dequeueFrom);
//   - {"operation":"remove","content":"<text>","reason":"absorbed_mid_turn"} —
//     taken INTO the running turn at a tool boundary (a queued_command
//     attachment follows); reason "delivered_to_agent" or none for others;
//   - {"operation":"popAll","content":"<text>"} — the person pressed Up and
//     pulled every queued message back into the input box: the queue is empty.
//
// Most enqueues are the harness's own (a background task's
// <task-notification>, another session's <agent-message>): text that is one
// markup envelope. Those are tracked — a dequeue may take one — but are not
// the person's messages, so List leaves them out.

// The queue-operation operations (QueueOp.Op).
const (
	QueueEnqueue = "enqueue"
	QueueDequeue = "dequeue"
	QueueRemove  = "remove"
	QueuePopAll  = "popAll"
)

// QueueOp is one queue-operation record (Line.Queue).
type QueueOp struct {
	Op string
	// Content is the message an enqueue, remove or popAll names, capped at
	// MaxText like a prompt's text; "" on a dequeue.
	Content string
}

// queueOpOf is rec's queue operation, or nil when rec is not a queue-operation
// record this reader knows.
func queueOpOf(rec Record) *QueueOp {
	if rec.Type != "queue-operation" || rec.IsSidechain {
		return nil
	}
	switch rec.Operation {
	case QueueEnqueue, QueueDequeue, QueueRemove, QueuePopAll:
	default:
		return nil
	}
	var content string
	if len(rec.QueueContent) > 0 && json.Unmarshal(rec.QueueContent, &content) != nil {
		content = "" // not a string: names nothing this reader can match
	}
	if rec.Operation == QueueEnqueue && strings.TrimSpace(content) == "" {
		return nil
	}
	return &QueueOp{Op: rec.Operation, Content: Cap(content, MaxText)}
}

// Drawn is lines without the ones that carry no events (a queue operation's):
// what a reader that only draws events keeps.
func Drawn(lines []Line) []Line {
	out := lines[:0:0]
	for _, l := range lines {
		if len(l.Events) > 0 {
			out = append(out, l)
		}
	}
	return out
}

// MaxQueue bounds the messages a Queue holds; past it the oldest are dropped.
// The CLI's queue is a handful at most; this only bounds a transcript that
// never says what became of its enqueues.
const MaxQueue = 64

// Queue is Claude Code's input queue as a transcript's records describe it,
// folded from its Lines in order. The zero value is an empty queue.
type Queue struct {
	items []string
	// taking is how many dequeues are waiting for the user record that says
	// which message each took.
	taking int
}

// Observe folds lines into the queue and reports whether List changed.
func (q *Queue) Observe(lines []Line) bool {
	before := q.List()
	for _, l := range lines {
		if op := l.Queue; op != nil {
			switch op.Op {
			case QueueEnqueue:
				q.items = append(q.items, op.Content)
				if len(q.items) > MaxQueue {
					q.items = q.items[len(q.items)-MaxQueue:]
				}
			case QueueDequeue:
				q.taking++
			case QueueRemove:
				if i := indexOf(q.items, op.Content); i >= 0 {
					q.items = append(q.items[:i:i], q.items[i+1:]...)
				}
			case QueuePopAll:
				q.items, q.taking = nil, 0
			}
		}
		for _, ev := range l.Events {
			if q.taking > 0 && (ev.Kind == KindPrompt || ev.Kind == KindNotice) {
				q.taking--
				q.dequeueFrom(ev)
			}
		}
	}
	return !equalStrings(before, q.List())
}

// dequeueFrom removes the message a dequeue took, which ev — the user record
// that followed it — is: the queued message it IS (the CLI writes a queued
// message as typed), else the one it CONTAINS (another session's message is
// delivered inside a sentence of the harness's), else the oldest of ev's kind
// (a notice for a harness envelope, a prompt for the person's) — a dequeue
// took exactly one, so one goes whatever the text says.
func (q *Queue) dequeueFrom(ev Event) {
	if len(q.items) == 0 {
		return
	}
	i := indexOf(q.items, ev.Text)
	if i < 0 {
		t := strings.TrimSpace(ev.Text)
		for j, s := range q.items {
			if s := strings.TrimSpace(s); s != "" && (s == t || strings.Contains(ev.Text, s)) {
				i = j
				break
			}
		}
	}
	if i < 0 {
		envelope := ev.Kind == KindNotice
		for j, s := range q.items {
			if IsQueueEnvelope(s) == envelope {
				i = j
				break
			}
		}
	}
	if i < 0 {
		i = 0
	}
	q.items = append(q.items[:i:i], q.items[i+1:]...)
}

// List is the person's messages waiting in the queue, oldest first: every
// queued message but the harness's envelopes (IsQueueEnvelope). nil when none.
func (q *Queue) List() []string {
	var out []string
	for _, s := range q.items {
		if !IsQueueEnvelope(s) {
			out = append(out, s)
		}
	}
	return out
}

// IsQueueEnvelope reports whether a queued message is the harness's rather
// than the person's: text that opens with a markup tag (<task-notification>,
// <agent-message …>), the shape every harness enqueue measured has. A person's
// message that happens to open with a tag is left out too — it shows once
// Claude takes it.
func IsQueueEnvelope(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 3 || s[0] != '<' {
		return false
	}
	c := s[1]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func indexOf(items []string, s string) int {
	for i, it := range items {
		if it == s {
			return i
		}
	}
	return -1
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
