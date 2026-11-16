package api

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/notes"
)

// track_task (track_task_tool.go) through the real mirror door against a real
// Postgres: a mirrored session records its OWN tasks, nothing else's; the
// declaration is the owner's and shows on fleet_list for their chief only.

// trackTaskCall calls track_task on /mirror/mcp as Claude session sid of the
// host tok names: the tool's text and whether it is an error.
func (f *attnFixture) trackTaskCall(t *testing.T, tok, sid string, args any) (string, bool) {
	t.Helper()
	_, r, body := f.mirrorRPC(t, mirrorAs(tok, sid), mcpToolCall(trackTaskTool, args))
	if r == nil {
		t.Fatalf("track_task: %s", body)
	}
	if r.Error != nil {
		return "rpc-error " + r.Error.Message, true
	}
	text, _ := toolText(t, r)
	return text, strings.HasPrefix(text, `{"error"`)
}

// declared is user uid's stored declaration for session sid.
func (f *attnFixture) declared(t *testing.T, uid int64, sid string) focus.TaskLink {
	t.Helper()
	got, _, err := f.srv.ext.Focus.Get(asUser(uid), focus.MirrorSubject(sid))
	if err != nil {
		t.Fatal(err)
	}
	return got.Tasks
}

// TestASessionRecordsItsOwnTasksWithTrackTask: replace, add, none and clear
// through the door, each read back from the owner's focus row; a key muster
// holds a task for ("#<id>") also joins that task's session thread as
// worked (the existing link), and a plugin key in an enabled namespace with
// no task row is recorded without one. Refused, writing nothing: a namespace
// that is not enabled, another owner's task, keys beside none, and an
// unknown argument (there is no subject to name another session).
func TestASessionRecordsItsOwnTasksWithTrackTask(t *testing.T) {
	f, ws, _, _ := newMirrorMCPFixture(t)
	if err := notes.SetPluginTaskNamespaces([]string{"tracker"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = notes.SetPluginTaskNamespaces(nil) })
	mine, err := f.notes.Create(asUser(1), notes.Note{Title: "track-task mine", Body: "b", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := f.notes.Create(asUser(f.member), notes.Note{Title: "track-task theirs", Body: "b", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		_, _ = f.pool.Exec(sys, `DELETE FROM notes WHERE id IN ($1, $2)`, mine.ID, theirs.ID)
		_, _ = f.pool.Exec(sys, `DELETE FROM agent_focus WHERE subject_kind = 'mirror' AND subject_key IN ($1, $2)`, focusSID, secondSID)
	})
	mineKey := "#" + strconv.FormatInt(mine.ID, 10)

	text, isErr := f.trackTaskCall(t, ws, focusSID, map[string]any{"tasks": []string{"tracker:4711", mineKey}, "note": "the importer"})
	if isErr || !strings.Contains(text, `"subject":"mirror:`+focusSID+`"`) {
		t.Fatalf("replace: %s", text)
	}
	if got := f.declared(t, 1, focusSID); !slices.Equal(got.Keys, []string{"tracker:4711", mineKey}) || got.Note != "the importer" ||
		got.SetBy != "mirror:"+focusSID {
		t.Fatalf("stored after replace: %+v", got)
	}
	links, err := f.notes.SessionsForTask(asUser(1), mine.ID)
	if err != nil || !slices.ContainsFunc(links, func(l notes.SessionLink) bool { return l.SessionID == focusSID && l.Role == notes.RoleWorked }) {
		t.Fatalf("task %d's thread after the declaration: %+v %v; want %s as worked", mine.ID, links, err, focusSID)
	}

	if text, isErr := f.trackTaskCall(t, ws, focusSID, map[string]any{"tasks": []string{"tracker:4712"}, "mode": "add"}); isErr {
		t.Fatalf("add: %s", text)
	}
	if got := f.declared(t, 1, focusSID); !slices.Equal(got.Keys, []string{"tracker:4711", mineKey, "tracker:4712"}) || got.Note != "the importer" {
		t.Fatalf("stored after add: %+v", got)
	}

	before := f.declared(t, 1, focusSID)
	for name, args := range map[string]any{
		"a namespace not enabled": map[string]any{"tasks": []string{"elsewhere:1"}},
		"another owner's task":    map[string]any{"tasks": []string{"#" + strconv.FormatInt(theirs.ID, 10)}},
		"keys beside none":        map[string]any{"tasks": []string{"tracker:1"}, "none": "x"},
		"another session named":   map[string]any{"tasks": []string{"tracker:1"}, "subject": "mirror:" + secondSID},
		"nothing at all":          map[string]any{},
	} {
		if text, isErr := f.trackTaskCall(t, ws, focusSID, args); !isErr {
			t.Errorf("%s: not refused: %s", name, text)
		}
	}
	if got := f.declared(t, 1, focusSID); !slices.Equal(got.Keys, before.Keys) || !got.SetAt.Equal(before.SetAt) {
		t.Fatalf("a refused call wrote: %+v, was %+v", got, before)
	}
	if got := f.declared(t, 1, secondSID); got.Declared() {
		t.Fatalf("naming another session wrote its row: %+v", got)
	}

	if text, isErr := f.trackTaskCall(t, ws, focusSID, map[string]any{"none": "a scratch experiment"}); isErr {
		t.Fatalf("none: %s", text)
	}
	if got := f.declared(t, 1, focusSID); got.Keys != nil || got.None != "a scratch experiment" {
		t.Fatalf("stored after none: %+v", got)
	}
	if text, isErr := f.trackTaskCall(t, ws, focusSID, map[string]any{"mode": "clear"}); isErr {
		t.Fatalf("clear: %s", text)
	}
	if got := f.declared(t, 1, focusSID); got.Declared() {
		t.Fatalf("stored after clear: %+v", got)
	}
}

// TestOnlyTheSessionItselfWritesItsTaskLink: the link is written for the
// session the door BOUND the call to, never one the caller names: a host's
// other session writes its own row (the first one's is unchanged), a host
// that does not list the session is refused, and another owner's token for a
// host of the same name writes nothing of this owner's.
func TestOnlyTheSessionItselfWritesItsTaskLink(t *testing.T) {
	f, ws, laptop, members := newMirrorMCPFixture(t)
	if err := notes.SetPluginTaskNamespaces([]string{"tracker"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = notes.SetPluginTaskNamespaces(nil) })
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_kind = 'mirror' AND subject_key IN ($1, $2)`, focusSID, secondSID)
	})
	if text, isErr := f.trackTaskCall(t, ws, focusSID, map[string]any{"tasks": []string{"tracker:1"}}); isErr {
		t.Fatalf("the first session: %s", text)
	}
	if text, isErr := f.trackTaskCall(t, ws, secondSID, map[string]any{"tasks": []string{"tracker:2"}}); isErr {
		t.Fatalf("the second session: %s", text)
	}
	if a, b := f.declared(t, 1, focusSID), f.declared(t, 1, secondSID); !slices.Equal(a.Keys, []string{"tracker:1"}) || !slices.Equal(b.Keys, []string{"tracker:2"}) {
		t.Fatalf("each session's own row: %+v / %+v", a, b)
	}
	for name, tok := range map[string]string{"a host that lists nothing": laptop, "another owner's host": members} {
		if text, isErr := f.trackTaskCall(t, tok, focusSID, map[string]any{"tasks": []string{"tracker:9"}}); !isErr {
			t.Errorf("%s: not refused: %s", name, text)
		}
	}
	if a := f.declared(t, 1, focusSID); !slices.Equal(a.Keys, []string{"tracker:1"}) {
		t.Fatalf("a refused caller moved the first session's row: %+v", a)
	}
	if got := f.declared(t, f.member, focusSID); got.Declared() {
		t.Fatalf("another owner's token wrote a row of its own: %+v", got)
	}
}

// TestTheChiefReadsOnlyItsOwnersDeclarations: fleet_list shows user 1's
// session's declaration to user 1 (the control), and the member's chief —
// over its real /mcp door — reads none of it.
func TestTheChiefReadsOnlyItsOwnersDeclarations(t *testing.T) {
	f, ws, _, _ := newMirrorMCPFixture(t)
	if err := notes.SetPluginTaskNamespaces([]string{"tracker"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = notes.SetPluginTaskNamespaces(nil) })
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_kind = 'mirror' AND subject_key = $1`, focusSID)
	})
	if text, isErr := f.trackTaskCall(t, ws, focusSID, map[string]any{"tasks": []string{"tracker:4711"}}); isErr {
		t.Fatalf("declare: %s", text)
	}
	own := f.srv.toolFleetList(f.srv.attentionPrincipal(context.Background(), 1), "")
	if !strings.Contains(own, `"tasks":["tracker:4711"]`) {
		t.Fatalf("control: the owner's fleet_list lacks the declaration:\n%s", own)
	}
	r, code := f.mcp(t, f.chiefTok, mcpToolCall(fleetListToolName, map[string]any{}), nil)
	if r == nil || r.Error != nil {
		t.Fatalf("the member's chief fleet_list: HTTP %d %+v", code, r)
	}
	text, isErr := toolText(t, r)
	var got struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	if isErr || json.Unmarshal([]byte(text), &got) != nil {
		t.Fatalf("the member's chief fleet_list: %s", text)
	}
	if strings.Contains(text, "tracker:4711") || strings.Contains(text, focusSID) {
		t.Fatalf("another owner's chief reads the declaration:\n%s", text)
	}
}
