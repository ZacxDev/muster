package api

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/notes"
)

// Every task push that names one task carries the task's external reference
// and its creator in its BODY (taskRefFragment): "<ns> <id> · by <creator>",
// either half alone, nothing at all when neither is known. These pin the
// LITERAL title and body of each push kind for the four shapes (ref + creator,
// ref only, creator only, neither). Ids, names and namespaces are pairwise
// distinct across cases, so a fragment hardcoded to one case cannot satisfy
// another.

// refCases are the four fragment shapes on one task each, with their literal
// fragment.
type refCase struct {
	name string
	note notes.Note
	frag string
}

// withHQ configures "hq" as a plugin task namespace for the test.
func withHQ(t *testing.T) {
	t.Helper()
	if err := notes.SetPluginTaskNamespaces([]string{"hq"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = notes.SetPluginTaskNamespaces(nil) })
}

func fourShapes() []refCase {
	upstream := func(key, creator string) *notes.Reference {
		return &notes.Reference{Key: key, Snapshot: notes.RefSnapshot{Creator: creator}}
	}
	return []refCase{
		{"ref and upstream creator", notes.Note{ID: 501, OwnerID: 7, Title: "Rotate the key",
			Ref: upstream("hq:868ktt574", "Pat Example")}, "hq 868ktt574 · by Pat Example"},
		{"ref only", notes.Note{ID: 502, OwnerID: 7, Title: "Fix the build",
			Ref: upstream("hq:77aa", "")}, "hq 77aa"},
		{"creator only", notes.Note{ID: 503, OwnerID: 7, Title: "Tidy the docs",
			CreatedBy: notes.CreatedByAgent("worker-9")}, "by agent worker-9"},
		{"neither", notes.Note{ID: 504, OwnerID: 7, Title: "Plain task"}, ""},
	}
}

// TestTaskRefFragmentIsLiteral pins the fragment for every source of a
// reference and a creator.
func TestTaskRefFragmentIsLiteral(t *testing.T) {
	withHQ(t)
	upstream := func(key, creator string) *notes.Reference {
		return &notes.Reference{Key: key, Snapshot: notes.RefSnapshot{Creator: creator}}
	}
	cases := []struct {
		name string
		n    notes.Note
		want string
	}{
		{"reference with an upstream creator", notes.Note{Ref: upstream("hq:868ktt574", "Pat Example")},
			"hq 868ktt574 · by Pat Example"},
		{"reference without one", notes.Note{Ref: upstream("hq:abc1", "")}, "hq abc1"},
		// The snapshot is the ONLY creator a reference has: what muster recorded
		// for the row (a plugin filed it) is not who made the record upstream.
		{"reference ignores a recorded native creator", notes.Note{Ref: upstream("hq:abc2", ""),
			CreatedBy: notes.CreatedByPlugin("bridge")}, "hq abc2"},
		{"copy row: its plugin namespace tag, and its plugin", notes.Note{Tags: []string{"area:infra", "hq:q55"},
			CreatedBy: notes.CreatedByPlugin("bridge")}, "hq q55 · by plugin bridge"},
		{"clickup back-reference", notes.Note{Tags: []string{"clickup:cu9"}}, "clickup cu9"},
		{"an ordinary namespaced tag is not a reference", notes.Note{Tags: []string{"area:infra", "superseded-by:12"}}, ""},
		// The owner's own create names no one (the push goes to the owner), and
		// the producer fallback does not speak for it; a reference still shows.
		{"owner names no one", notes.Note{CreatedBy: strp(notes.CreatedByOwner)}, ""},
		{"owner is not replaced by the producer", notes.Note{SourceType: strp("drafter"),
			CreatedBy: strp(notes.CreatedByOwner)}, ""},
		{"owner keeps the reference", notes.Note{Tags: []string{"hq:q56"},
			CreatedBy: strp(notes.CreatedByOwner)}, "hq q56"},
		{"chief", notes.Note{CreatedBy: strp(notes.CreatedByChief)}, "by chief"},
		{"agent", notes.Note{CreatedBy: notes.CreatedByAgent("fixer-3")}, "by agent fixer-3"},
		{"plugin", notes.Note{CreatedBy: notes.CreatedByPlugin("inbox")}, "by plugin inbox"},
		{"producer only", notes.Note{SourceType: strp("drafter")}, "by Task drafter"},
		{"recorded creator wins over the producer", notes.Note{SourceType: strp("claude-code"),
			CreatedBy: notes.CreatedByAgent("lead-1")}, "by agent lead-1"},
		{"unknown recorded value names no one", notes.Note{CreatedBy: strp("robot")}, ""},
		{"empty agent name names no one", notes.Note{CreatedBy: strp("agent:  ")}, ""},
		{"nothing", notes.Note{}, ""},
		// Flattened and capped: one line, never the whole string.
		{"control characters flattened", notes.Note{Ref: upstream("hq:z1", "A\nB\tC")}, "hq z1 · by A B C"},
		{"long creator capped", notes.Note{Ref: upstream("hq:z2", strings.Repeat("x", 90))},
			"hq z2 · by " + strings.Repeat("x", taskCreatorMax) + "…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := taskRefFragment(c.n); got != c.want {
				t.Fatalf("taskRefFragment = %q, want %q", got, c.want)
			}
		})
	}
}

func refServer(t *testing.T) (*Server, *payloadPusher, *manualTimers) {
	t.Helper()
	store := &memPush{}
	claim(t, store, 7, ep("seven"))
	pusher := &payloadPusher{}
	s := eventServer(store, pusher)
	timers := &manualTimers{}
	s.taskCreateAfter, s.taskCommentAfter = timers.after, timers.after
	return s, pusher, timers
}

func onePush(t *testing.T, s *Server, p *payloadPusher) (string, string) {
	t.Helper()
	s.WaitForPushes()
	got := p.to(ep("seven"))
	if len(got) != 1 {
		t.Fatalf("got %d pushes, want 1: %+v", len(got), got)
	}
	return got[0].Title, got[0].Body
}

func expectPush(t *testing.T, gotTitle, gotBody, wantTitle, wantBody string) {
	t.Helper()
	if gotTitle != wantTitle || gotBody != wantBody {
		t.Fatalf("push = %q / %q\nwant   %q / %q", gotTitle, gotBody, wantTitle, wantBody)
	}
}

// paren is the literal body suffix for a fragment.
func paren(frag string) string {
	if frag == "" {
		return ""
	}
	return " (" + frag + ")"
}

func TestReviewPushCarriesTheRefAndCreator(t *testing.T) {
	withHQ(t)
	want := map[int64]string{
		501: "Rotate the key (hq 868ktt574 · by Pat Example)",
		502: "Fix the build (hq 77aa)",
		503: "Tidy the docs (by agent worker-9)",
		504: "Plain task",
	}
	for _, c := range fourShapes() {
		t.Run(c.name, func(t *testing.T) {
			s, p, _ := refServer(t)
			n := c.note
			n.Status = notes.StatusReadyForReview
			ctx := withRefOrigin(notes.WithWriter(context.Background(), string(writerAgentTool)), refOrigin{Agent: "w"})
			s.notifyTaskDone(ctx, n)
			title, body := onePush(t, s, p)
			// The title is unchanged: "Task #N …" and the source label own it.
			expectPush(t, title, body, "Task #"+itoa(n.ID)+" ready for review · agent w", want[n.ID])
		})
	}
}

func TestCreatedPushCarriesTheRefAndCreator(t *testing.T) {
	withHQ(t)
	want := map[int64]string{
		501: "Rotate the key (hq 868ktt574 · by Pat Example)",
		502: "Fix the build (hq 77aa)",
		503: "Tidy the docs (by agent worker-9)",
		504: "Plain task",
	}
	for _, c := range fourShapes() {
		t.Run(c.name, func(t *testing.T) {
			s, p, timers := refServer(t)
			s.notifyTaskCreated(c.note)
			timers.fireAll()
			title, body := onePush(t, s, p)
			expectPush(t, title, body, "New task", want[c.note.ID])
		})
	}
	// The batched push lists each task, each with its own fragment.
	t.Run("batched", func(t *testing.T) {
		s, p, timers := refServer(t)
		for _, c := range fourShapes() {
			s.notifyTaskCreated(c.note)
		}
		timers.fireAll()
		title, body := onePush(t, s, p)
		expectPush(t, title, body, "4 new tasks",
			"Rotate the key (hq 868ktt574 · by Pat Example), Fix the build (hq 77aa), Tidy the docs (by agent worker-9), Plain task")
	})
}

func TestCommentPushCarriesTheRefAndCreator(t *testing.T) {
	withHQ(t)
	for _, c := range fourShapes() {
		t.Run(c.name, func(t *testing.T) {
			s, p, timers := refServer(t)
			s.notifyTaskCommented(c.note, notes.Comment{Author: "bot", Body: "**done** here"})
			timers.fireAll()
			title, body := onePush(t, s, p)
			expectPush(t, title, body, "New comment on task #"+itoa(c.note.ID), "bot: done here"+paren(c.frag))
		})
	}
	t.Run("batched", func(t *testing.T) {
		s, p, timers := refServer(t)
		sh := fourShapes()
		s.notifyTaskCommented(sh[0].note, notes.Comment{Author: "bot", Body: "a"})
		s.notifyTaskCommented(sh[3].note, notes.Comment{Author: "ci", Body: "b"})
		timers.fireAll()
		title, body := onePush(t, s, p)
		expectPush(t, title, body, "2 new comments", "bot on #501 (hq 868ktt574 · by Pat Example), ci on #504")
	})
}

// runningAgentStore and oneTaskStore serve notifyAgentRunning: one task-linked
// running agent and its task.
type runningAgentStore struct {
	agents.Store
	a agents.Agent
}

func (o runningAgentStore) GetByName(_ context.Context, name string) (agents.Agent, error) {
	if name != o.a.Name {
		return agents.Agent{}, context.Canceled
	}
	return o.a, nil
}

type oneTaskStore struct {
	notes.Store
	n notes.Note
}

func (o oneTaskStore) Get(_ context.Context, id int64) (notes.Note, error) {
	if id != o.n.ID {
		return notes.Note{}, context.Canceled
	}
	return o.n, nil
}

func TestAgentReadyPushCarriesTheRefAndCreator(t *testing.T) {
	withHQ(t)
	for _, c := range fourShapes() {
		t.Run(c.name, func(t *testing.T) {
			s, p, _ := refServer(t)
			id := c.note.ID
			s.ext.Agents = runningAgentStore{a: agents.Agent{ID: 90 + id, Name: "runner", OwnerID: 7,
				Status: agents.StatusRunning, NoteID: &id}}
			s.ext.Notes = oneTaskStore{n: c.note}
			s.notifyAgentRunning("runner")
			title, body := onePush(t, s, p)
			expectPush(t, title, body, "Task #"+itoa(id)+": agent ready", "The agent is now working on it."+paren(c.frag))
		})
	}
}
