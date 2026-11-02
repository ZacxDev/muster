package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
)

// TestNewTaskPushNamesTheTaskByItsTitle pins the BODY of the "New task" push a
// machine-created task sends, end to end through notifyTaskCreated and the
// debounce flush. The push is named by the one task-naming rule: the producer's
// title when it says anything, then the body's first line that says anything
// once markdown is flattened, then the directory.
//
// Case "title wins" is the regression: the push used to read only the body, so a
// producer whose body opened with a routing header (`To: group.…`) put that raw
// header on the phone even though it had sent a title.
func TestNewTaskPushNamesTheTaskByItsTitle(t *testing.T) {
	cases := []struct {
		name string
		note notes.Note
		want string
	}{
		{"title wins over the body",
			notes.Note{Title: "Rotate the widget keys", Body: "To: group.example-one\nthe real ask", Directory: "/srv/alpha"},
			"Rotate the widget keys"},
		{"a markdown title is flattened",
			notes.Note{Title: "**Fix** the [login](https://example.com/l) page", Body: "body line"},
			"Fix the login page"},
		{"no title: the body's first line, flattened",
			notes.Note{Body: "\n**Goal:** ship the importer\nsecond line", Directory: "/srv/beta"},
			"Goal: ship the importer"},
		{"no title: a markup-only first line is skipped",
			notes.Note{Body: "---\nafter the rule"},
			"after the rule"},
		{"a whitespace-only title falls through to the body",
			notes.Note{Title: " \t ", Body: "body says this"},
			"body says this"},
		// A path is not markdown: it is sent verbatim, never flattened
		// ("*a*" would otherwise lose its asterisks).
		{"nothing but a directory, verbatim",
			notes.Note{Directory: "/srv/*a*/gamma"},
			"/srv/*a*/gamma"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &memPush{}
			claim(t, store, 7, ep("dev"))
			pusher := &payloadPusher{}
			s := eventServer(store, pusher)
			timers := &manualTimers{}
			s.taskCreateAfter = timers.after

			n := c.note
			n.ID, n.OwnerID = int64(100+i), 7
			s.notifyTaskCreated(n)
			if fired := timers.fireAll(); fired != 1 {
				t.Fatalf("%d debounce timers fired, want 1", fired)
			}
			s.WaitForPushes()
			got := pusher.to(ep("dev"))
			if len(got) != 1 || got[0].Title != "New task" {
				t.Fatalf("pushes = %+v, want one \"New task\"", got)
			}
			if got[0].Body != c.want {
				t.Errorf("push body = %q, want %q", got[0].Body, c.want)
			}
		})
	}
}

// TestTaskLabelsShareTheNamingRule: the attention action's display label and
// the stored label name a task the way the push does — title first — and the
// stored one keeps it as written.
func TestTaskLabelsShareTheNamingRule(t *testing.T) {
	n := notes.Note{ID: 5, Title: "Ship **it**", Body: "To: group.example-two"}
	if got, want := taskLabelPlain(n), "Ship it"; got != want {
		t.Errorf("taskLabelPlain = %q, want %q", got, want)
	}
	if got, want := taskLabel(n), "Ship **it**"; got != want {
		t.Errorf("taskLabel = %q, want %q", got, want)
	}
	// Untitled, markup-only first line: both labels skip it, as the push does.
	u := notes.Note{ID: 6, Body: "```\nreal first line"}
	if got, want := taskLabel(u), "real first line"; got != want {
		t.Errorf("taskLabel(untitled) = %q, want %q", got, want)
	}
	if got, want := taskLabelPlain(notes.Note{ID: 7, Directory: "/srv/delta"}), "/srv/delta"; got != want {
		t.Errorf("taskLabelPlain(directory only) = %q, want %q", got, want)
	}
}
