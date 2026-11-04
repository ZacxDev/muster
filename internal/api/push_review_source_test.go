package api

import (
	"context"
	"go/ast"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
)

// TestReviewPushTitleNamesItsSource pins the ready-for-review push's LITERAL
// title for each writer class, driven through notifyTaskDone with the context
// setNoteStatus stamps (the writer label and who is writing). Every id, agent
// name and namespace is distinct, and none equals another case's, so a label
// hardcoded to one case's value cannot satisfy the others.
func TestReviewPushTitleNamesItsSource(t *testing.T) {
	ref := func(key string) *notes.Reference { return &notes.Reference{Key: key} }
	cases := []struct {
		name   string
		writer statusWriter
		origin refOrigin
		ref    *notes.Reference
		id     int64
		want   string
	}{
		{"upstream with its namespace", writerPluginReference, refOrigin{}, ref("hq:868ktt574"), 876,
			"Task #876 ready for review · upstream (hq)"},
		{"upstream, another namespace", writerPluginReference, refOrigin{}, ref("tracker:abc"), 877,
			"Task #877 ready for review · upstream (tracker)"},
		{"upstream without a reference key", writerPluginReference, refOrigin{}, nil, 878,
			"Task #878 ready for review · upstream"},
		{"agent tool, named", writerTaskTool, refOrigin{Author: "worker-7", Agent: "worker-7"}, nil, 879,
			"Task #879 ready for review · agent worker-7"},
		{"agent route, named", writerAgentRoute, refOrigin{Author: "repo-bot", Agent: "repo-bot"}, nil, 880,
			"Task #880 ready for review · agent repo-bot"},
		{"agent native tool, named", writerAgentTool, refOrigin{Author: "fixer", Agent: "fixer"}, nil, 881,
			"Task #881 ready for review · agent fixer"},
		{"machine route, no agent name", writerMachineAPI, refOrigin{Author: "api"}, nil, 882,
			"Task #882 ready for review · agent"},
		{"mirrored session via task_set_status, no agent", writerTaskTool, refOrigin{Author: "my-session"}, nil, 883,
			"Task #883 ready for review · agent"},
		{"owner's card control", writerHumanUI, refOrigin{Author: "user"}, nil, 884,
			"Task #884 ready for review"},
		// A reference-keyed task the OWNER moved is still the owner's: the
		// label follows the writer, never the note's shape.
		{"owner on a reference row", writerHumanUI, refOrigin{Author: "user"}, ref("hq:1"), 885,
			"Task #885 ready for review"},
		{"no writer stamped", "", refOrigin{}, nil, 886,
			"Task #886 ready for review"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &memPush{}
			claim(t, store, 7, ep("seven"))
			pusher := &payloadPusher{}
			s := eventServer(store, pusher)
			ctx := withRefOrigin(notes.WithWriter(context.Background(), string(c.writer)), c.origin)
			s.notifyTaskDone(ctx, notes.Note{ID: c.id, OwnerID: 7, Status: notes.StatusReadyForReview, Title: "t", Ref: c.ref})
			s.WaitForPushes()
			got := pusher.to(ep("seven"))
			if len(got) != 1 || got[0].Title != c.want {
				t.Fatalf("got %+v, want one push titled %q", got, c.want)
			}
		})
	}
}

// TestReviewPushIsSentOnlyFromTheStatusFunnel: notifyTaskDone has exactly ONE
// caller, setNoteStatus — the one place the writer is on the context its title
// is labelled from. A call from anywhere else would push with a context that
// carries no writer, and an upstream or agent move would arrive unlabelled.
func TestReviewPushIsSentOnlyFromTheStatusFunnel(t *testing.T) {
	fset := token.NewFileSet()
	files := parsePackageSources(t, fset)
	var callers []string
	for _, f := range files {
		forEachCallTo(f, "notifyTaskDone", func(enclosing string, _ *ast.CallExpr) {
			callers = append(callers, enclosing)
		})
	}
	sort.Strings(callers)
	if got := strings.Join(callers, ","); got != "setNoteStatus" {
		t.Errorf("notifyTaskDone is called from [%s], want exactly [setNoteStatus]: the funnel that stamps the "+
			"writer the push's title names", got)
	}
}
