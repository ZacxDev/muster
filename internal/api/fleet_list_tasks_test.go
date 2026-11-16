package api

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/focus"
)

// fleetTaskRow is the part of a fleet_list row these tests read.
type fleetTaskRow struct {
	ID        string   `json:"id"`
	Status    string   `json:"status"`
	ActiveAt  string   `json:"active_at"`
	Tasks     []string `json:"tasks"`
	Untracked string   `json:"untracked"`
	Note      string   `json:"task_note"`
	SetAt     string   `json:"task_set_at"`
}

// fleetTasksNow is the fixed clock: minute-aligned (mirror activity is floored
// to a minute), far from the wall clock, so an age taken on time.Now reads
// years and never one of the boundaries below.
var fleetTasksNow = time.Date(2021, 6, 2, 9, 0, 0, 0, time.UTC)

// fleetTasksServer is whereServer with the labelled window active 6 h ago and
// the unlabelled one 30 h ago (distinct, and neither a multiple of the
// other), an agent started 10 h ago, and the labelled session's declared
// tasks; the unlabelled one declared nothing.
func fleetTasksServer(t *testing.T) *Server {
	t.Helper()
	s := whereServer(t)
	s.now = func() time.Time { return fleetTasksNow }
	fm := s.ext.Mirror.(*fakeMirror)
	fm.windows[0].Activity = fleetTasksNow.Add(-6 * time.Hour).Unix()
	fm.windows[1].Activity = fleetTasksNow.Add(-30 * time.Hour).Unix()
	labelled := focus.MirrorSubject(whereLabelledSID)
	s.ext.Focus = &memFocus{rows: map[focus.Subject]focus.Focus{
		labelled: {Subject: labelled, Owner: 1, Tasks: focus.TaskLink{Keys: []string{"ex:4711", "ex:12"}, Note: "the importer",
			SetAt: fleetTasksNow.Add(-90 * time.Minute)}},
	}}
	s.ext.Agents = listAgents{list: []agents.Agent{{ID: 7, OwnerID: 1, Name: "ag-wren", Status: agents.StatusRunning,
		CreatedAt: fleetTasksNow.Add(-10 * time.Hour)}}}
	return s
}

func fleetListRows(t *testing.T, s *Server, args string) map[string]fleetTaskRow {
	t.Helper()
	text := s.toolFleetList(s.attentionPrincipal(context.Background(), 1), args)
	var got struct {
		Sessions []fleetTaskRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil || got.Sessions == nil {
		t.Fatalf("fleet_list %s: %v:\n%s", args, err, text)
	}
	out := map[string]fleetTaskRow{}
	for _, r := range got.Sessions {
		out[r.ID] = r
	}
	return out
}

func rowIDs(rows map[string]fleetTaskRow) []string {
	var ids []string
	for id := range rows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// TestFleetListShowsEachSessionsDeclaredTasks: a mirrored session's track_task
// declaration — its keys, note and when it was set — is on its fleet_list
// row with its window's activity; a session that declared nothing carries no
// task field at all (never a guess). Red before this change: the row had no
// such fields.
func TestFleetListShowsEachSessionsDeclaredTasks(t *testing.T) {
	s := fleetTasksServer(t)
	rows := fleetListRows(t, s, "")
	l := rows[focus.MirrorSubject(whereLabelledSID).String()]
	if !slices.Equal(l.Tasks, []string{"ex:4711", "ex:12"}) || l.Note != "the importer" || l.SetAt != "2021-06-02T07:30:00Z" ||
		l.ActiveAt != "2021-06-02T03:00:00Z" {
		t.Fatalf("the declaring session's row: %+v", l)
	}
	u := rows[focus.MirrorSubject(whereUnlabelledSID).String()]
	if u.ID == "" || u.Tasks != nil || u.Untracked != "" || u.SetAt != "" || u.ActiveAt != "2021-06-01T03:00:00Z" {
		t.Fatalf("the silent session's row: %+v", u)
	}
	text := s.toolFleetList(s.attentionPrincipal(context.Background(), 1), "")
	if strings.Count(text, `"tasks"`) != 1 {
		t.Fatalf("a session with no declaration carries a tasks field:\n%s", text)
	}
}

// TestFleetListFiltersByActivity: active_within_hours at each session's
// boundary (inclusive) and in the middle, for a mirrored session (its
// window's activity) and an agent (its start). Every case names its expected
// set, so a filter that keeps or drops too much is red.
func TestFleetListFiltersByActivity(t *testing.T) {
	s := fleetTasksServer(t)
	lab, unl, ag := "mirror:"+whereLabelledSID, "mirror:"+whereUnlabelledSID, "agent:7"
	for _, c := range []struct {
		args string
		want []string
	}{
		{``, []string{ag, lab, unl}},
		{`{}`, []string{ag, lab, unl}},
		{`{"active_within_hours":5}`, []string{}},              // below every boundary
		{`{"active_within_hours":6}`, []string{lab}},           // the labelled window's boundary: inclusive
		{`{"active_within_hours":9}`, []string{lab}},           // between it and the agent's
		{`{"active_within_hours":10}`, []string{ag, lab}},      // the agent's start: inclusive
		{`{"active_within_hours":29}`, []string{ag, lab}},      // just short of the unlabelled window
		{`{"active_within_hours":30}`, []string{ag, lab, unl}}, // its boundary
		{`{"active_within_hours":720}`, []string{ag, lab, unl}},
	} {
		got := rowIDs(fleetListRows(t, s, c.args))
		want := append([]string(nil), c.want...)
		slices.Sort(want)
		if !slices.Equal(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Errorf("fleet_list %s = %v, want %v", c.args, got, want)
		}
	}
	for _, bad := range []string{`{"active_within_hours":-1}`, `{"active_within_hours":721}`, `{"active_within_hours":"6"}`, `[`} {
		if text := s.toolFleetList(s.attentionPrincipal(context.Background(), 1), bad); !strings.Contains(text, `"error"`) {
			t.Errorf("fleet_list %s was not refused: %s", bad, text)
		}
	}
	// The bound in the refusal is the constant's.
	if text := s.toolFleetList(s.attentionPrincipal(context.Background(), 1), `{"active_within_hours":721}`); !strings.Contains(text, strconv.Itoa(fleetListMaxHours)) {
		t.Errorf("the refusal does not name the bound: %s", text)
	}
}
