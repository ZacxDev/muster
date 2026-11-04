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
		via    *notes.RefWrite // the queued write a snapshot's move confirmed
		ref    *notes.Reference
		id     int64
		want   string
	}{
		{"upstream with its namespace", writerPluginReference, refOrigin{}, nil, ref("hq:868ktt574"), 876,
			"Task #876 ready for review · upstream (hq)"},
		{"upstream, another namespace", writerPluginReference, refOrigin{}, nil, ref("tracker:abc"), 877,
			"Task #877 ready for review · upstream (tracker)"},
		{"upstream without a reference key", writerPluginReference, refOrigin{}, nil, nil, 878,
			"Task #878 ready for review · upstream"},
		{"agent tool, named", writerTaskTool, refOrigin{Author: "worker-4", Agent: "worker-4"}, nil, nil, 879,
			"Task #879 ready for review · agent worker-4"},
		{"agent route, named", writerAgentRoute, refOrigin{Author: "repo-bot", Agent: "repo-bot"}, nil, nil, 880,
			"Task #880 ready for review · agent repo-bot"},
		{"agent native tool, named", writerAgentTool, refOrigin{Author: "fixer", Agent: "fixer"}, nil, nil, 881,
			"Task #881 ready for review · agent fixer"},
		// The machine route names an agent only when its caller is one (a
		// service actor that is one of the owner's agents); the shared hook
		// token and a personal token name no one, so they read "machine".
		{"machine route, no agent name", writerMachineAPI, refOrigin{Author: "api"}, nil, nil, 882,
			"Task #882 ready for review · machine"},
		{"machine route, a service caller that is an agent", writerMachineAPI, refOrigin{Author: "api", Agent: "chief-3"}, nil, nil, 887,
			"Task #887 ready for review · agent chief-3"},
		{"mirrored session via task_set_status, no agent", writerTaskTool, refOrigin{Author: "my-session"}, nil, nil, 883,
			"Task #883 ready for review · agent"},
		{"owner's card control", writerHumanUI, refOrigin{Author: "user"}, nil, nil, 884,
			"Task #884 ready for review"},
		// A reference-keyed task the OWNER moved is still the owner's: the
		// label follows the writer, never the note's shape.
		{"owner on a reference row", writerHumanUI, refOrigin{Author: "user"}, nil, ref("hq:1"), 885,
			"Task #885 ready for review"},
		{"no writer stamped", "", refOrigin{}, nil, nil, 886,
			"Task #886 ready for review"},
		// A snapshot's move that CONFIRMED a queued write names that write's
		// source, through the upstream; one that confirmed none is the
		// upstream's own.
		{"agent's queued write confirmed via hq, named", writerPluginReference, refOrigin{},
			&notes.RefWrite{Writer: string(writerAgentTool), Agent: "worker-7", Author: "worker-7"}, ref("hq:868ktt574"), 888,
			"Task #888 ready for review · agent worker-7 via hq"},
		{"task_set_status queued write confirmed via tracker, named", writerPluginReference, refOrigin{},
			&notes.RefWrite{Writer: string(writerTaskTool), Agent: "lead-2"}, ref("tracker:q1"), 889,
			"Task #889 ready for review · agent lead-2 via tracker"},
		{"agent's queued write confirmed via hq, unnamed", writerPluginReference, refOrigin{},
			&notes.RefWrite{Writer: string(writerTaskTool), Author: "my-session"}, ref("hq:5"), 890,
			"Task #890 ready for review · agent via hq"},
		{"machine route's queued write confirmed via hq", writerPluginReference, refOrigin{},
			&notes.RefWrite{Writer: string(writerMachineAPI), Author: "api"}, ref("hq:6"), 891,
			"Task #891 ready for review · machine via hq"},
		{"owner's queued write confirmed via hq stays the owner's", writerPluginReference, refOrigin{},
			&notes.RefWrite{Writer: string(writerHumanUI), Author: "user"}, ref("hq:7"), 892,
			"Task #892 ready for review"},
		{"queued write confirmed on a row without a key", writerPluginReference, refOrigin{},
			&notes.RefWrite{Writer: string(writerAgentRoute), Agent: "solo"}, nil, 893,
			"Task #893 ready for review · agent solo via upstream"},
		// The confirmed write is read only for a SNAPSHOT's move: on any other
		// writer it changes nothing.
		{"a confirmed write on the owner's own write is ignored", writerHumanUI, refOrigin{Author: "user"},
			&notes.RefWrite{Writer: string(writerAgentTool), Agent: "ghost"}, ref("hq:8"), 894,
			"Task #894 ready for review"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &memPush{}
			claim(t, store, 7, ep("seven"))
			pusher := &payloadPusher{}
			s := eventServer(store, pusher)
			ctx := withRefOrigin(notes.WithWriter(context.Background(), string(c.writer)), c.origin)
			if c.via != nil {
				ctx = withConfirmedRefWrite(ctx, *c.via)
			}
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
